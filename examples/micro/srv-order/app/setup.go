// Package app 业务装配层：把 launchx 装配好的中间件句柄（DB / Cache / MQ /
// KV / Lock 与 kitex 服务端选项）接成
//
//	rpc → route → middleware → handler → service → repository → model
//
// 这条分层链，并把 kitex 服务端回存给 launchx 启动与停止。
//
// 这一层不含任何形态分支（不出现 monolith / bff / gateway 等取值）：
// 换形态只改 config.yaml 的 service.mode，与本装配无关。
package app

import (
	"errors"
	"strconv"
	"time"

	"github.com/cloudwego/kitex/server"
	"github.com/gospacex/logx"
	"gorm.io/gorm"

	"github.com/gospacex/launchx"
	"github.com/gospacex/launchx/examples/micro/pkg/handlecheck"
	orderrpc "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/order/order"
	"github.com/gospacex/launchx/examples/micro/pkg/outbox"
	"github.com/gospacex/launchx/examples/micro/pkg/tracex"
	"github.com/gospacex/launchx/examples/micro/srv-order/internal/handler"
	"github.com/gospacex/launchx/examples/micro/srv-order/internal/middleware"
	"github.com/gospacex/launchx/examples/micro/srv-order/internal/repository"
	"github.com/gospacex/launchx/examples/micro/srv-order/internal/route"
	srvrpc "github.com/gospacex/launchx/examples/micro/srv-order/internal/rpc"
	"github.com/gospacex/launchx/examples/micro/srv-order/internal/service"
)

// 自保护参数（固定窗口限流）。非地址类常量，故留在代码里：
// 会随环境变的只有地址，一律进 config.yaml。
const (
	rateLimitPerMinute = 120 // 写多读也多的域，上限比商品域紧
	rateLimitWindow    = time.Minute
)

// Setup 是注册给 App.OnSetup 的装配回调：
// 取 DB / Cache / MQ / KV / Lock 句柄 → 断言句柄来源 → 装配
// repository → outbox → service → handler → route → rpc 桩 →
// 用 launchx 产出的 kitex 选项建服务端 → SetRPCServer 回存。
//
// 失败即返回 error：装配期报错好过带着半装配的状态把服务起出去。
//
// 本函数不注册/注销 consul —— srv 档的注册由 launchx 按 mode 自己做
// （地址取 service.self.addr），业务侧只管装配。
func Setup(a *launchx.App) error {
	// DB 句柄只能经 App.DB() 取（业务不自建连接、不直引驱动）。
	db, ok := a.DB().(*gorm.DB)
	if !ok || db == nil {
		return errors.New("srv-order: db component is unavailable, check xmiddleware.sqlx in config.yaml")
	}
	// kitex 服务端选项由 srv 档装配产出（监听地址、注册、trace suite 全在里面），
	// 业务只做类型断言，绝不自己拼地址。
	opts, ok := a.RPCServerOptions().([]server.Option)
	if !ok {
		return errors.New("srv-order: rpc server options are unavailable, check service.mode=srv in config.yaml")
	}

	name := a.Config().GetString("service.name")
	logger := a.Logger()
	traceOn := a.Config().GetBool("trace.enabled")

	// 业务埋点的 provider 由装配层交接，业务分层因此不必去摸 otel 全局单例
	// （分层边界要求它不认识框架）。telemetry 未启用时 provider 为 nil，
	// tracex 会回落到 otel 全局（no-op），行为与之前一致。
	if tp := a.Tracer(); tp != nil {
		tracex.UseProvider(tp.TracerProvider())
	}

	// 句柄来源断言：确认这个 *gorm.DB 是 launchx 的 dbx 交出来的，而不是
	// 谁顺手 gorm.Open 了一个。用错句柄不报错、也能查能写，唯独 SQL 不进
	// 链路 —— 等排障时才发现整层 repository span 是空的就晚了。
	//
	// 只在 trace.on.db 为真时验：enable_trace=false 时 dbx 走 OpenNoTrace，
	// 句柄上没有 trace 回调，那是配置选择，不该让服务起不来。
	if a.Config().GetBool("trace.on.db") {
		if err := handlecheck.VerifyDBX(db); err != nil {
			return errors.New("srv-order: " + err.Error())
		}
	}
	logger.Info("dbx handle in use",
		logx.String("service", name),
		logx.String("dialector", handlecheck.DialectorName(db)),
		logx.String("sql_traced", strconv.FormatBool(a.Config().GetBool("trace.on.db"))),
		logx.String("layer_chain", "rpc>route>middleware>handler>service>repository>model"))

	// outbox 补发器：MQ 生产者经 App.MQ() 取。未装配（或降级成 nil）时
	// Runner 会显式 Error 而不静默跳过 —— 那种情况下下单仍在写 status=0
	// 的事件却无人投递，表会无声膨胀。
	runner := outbox.NewRunner(db, a.MQ(), logger, outbox.Config{
		TraceOn: a.Config().GetBool("trace.on.mq"),
	})
	runner.Start()
	// 退出链：业务回调先于组件停止（此时 DB / MQ 仍可用，本轮补发能收尾）。
	a.OnStop(func() error { runner.Stop(); return nil })

	// ---- 分层链装配：自下而上 ----
	// model（纯数据，无装配） ← repository ← service ← handler ← route ← rpc
	repo := repository.NewOrderRepository(db, traceOn)

	// 缓存 / KV / 锁句柄：三者都允许为 nil，由 service 内部按 fail-open 处理
	// 并留日志（装配期不因此失败 —— 没有缓存只该变慢，不该起不来）。
	svc := service.NewOrderService(
		repo,
		a.Cache()[name], // cachex 实例（订单详情缓存）
		a.KV()[name],    // kvx 实例（限流计数 + 下单笔数计数）
		a.Lock(),        // lockx 锁器（同用户下单互斥）
		traceOn,
		logger,
	)

	// 限流器只在 KV 组件可用时启用（kv 为 nil 时 Allow 恒 true：fail-open）
	var limiter *middleware.RateLimiter
	if kv := a.KV()[name]; kv != nil {
		limiter = middleware.NewRateLimiter(kv, "rate:"+name, rateLimitPerMinute, rateLimitWindow)
	}

	h := handler.NewOrderHandler(svc, traceOn)
	rt := route.NewRouter(limiter, traceOn, logger)

	a.SetRPCServer(orderrpc.NewServer(&srvrpc.Server{Handler: h, Router: rt}, opts...))
	return nil
}
