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
	"github.com/gospacex/launchx/examples/micro/pkg/auth"
	"github.com/gospacex/launchx/examples/micro/pkg/handlecheck"
	orderrpc "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/order/order"
	"github.com/gospacex/launchx/examples/micro/pkg/routing"
	"github.com/gospacex/launchx/examples/micro/srv-order/internal/handler"
	"github.com/gospacex/launchx/examples/micro/srv-order/internal/middleware"
	"github.com/gospacex/launchx/examples/micro/srv-order/internal/repository"
	"github.com/gospacex/launchx/examples/micro/srv-order/internal/route"
	srvrpc "github.com/gospacex/launchx/examples/micro/srv-order/internal/rpc"
	"github.com/gospacex/launchx/examples/micro/srv-order/internal/service"
	"github.com/gospacex/launchx/examples/pkg/outbox"
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

	cfg := a.Config()
	name := cfg.GetString("service.name")
	logger := a.Logger()
	traceOn := cfg.GetBool("trace.enabled")

	// ---------- 存储/中间件句柄的来源与入链确认 ----------
	// 必须在取用句柄之后、装配分层之前做：这时句柄刚由 launchx 的 builder
	// 交出，还没被任何业务代码包一层（包过之后就不是同一个实例了，判据失效）。
	//
	// 验两件事：①句柄来源合规（不是自己开的）；②trace.on 要求入链时它真的
	// 会产 span。两者都不满足会照样能读写，只是走着走着链路就空了一块。
	//
	// 覆盖面是 db / cache / kv / mq / lock 五类（此前只验 db，cache 与 kv
	// 拿自考句柄冒充是**完全静默**的 —— 它们连"回调在不在"这种可探针的
	// 痕迹都没有）。
	checks, err := handlecheck.VerifyOrError(a)
	if err != nil {
		return errors.New("srv-order: " + err.Error())
	}
	for _, c := range checks {
		if !c.Present {
			continue
		}
		logger.Info("handle verified",
			logx.String("component", c.Component),
			logx.String("backend", c.Backend),
			logx.String("joins_trace", strconv.FormatBool(c.Observed)),
			logx.String("probe", c.Probe))
	}
	logger.Info("layer chain assembled",
		logx.String("service", name),
		logx.String("dialector", handlecheck.DialectorName(db)),
		logx.String("chain", "rpc>route>auth>ratelimit>handler>service>repository>model"))

	// outbox 补发器：MQ 生产者经 App.MQ() 取。未装配（或降级成 nil）时
	// Runner 会显式 Error 而不静默跳过 —— 那种情况下下单仍在写 status=0
	// 的事件却无人投递，表会无声膨胀。
	runner := outbox.NewRunner(db, a.MQ(), logger, outbox.Config{
		TraceOn: cfg.GetBool("trace.on.mq"),
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

	// ---------- 鉴权闸门：只校验边界层透传下来的身份，不重复验签 ----------
	// 默认**启用**：鉴权是安全默认值，关掉它必须是显式配置的结果。
	// 这里刻意不写成 `cfg.GetBool("auth.require")` 一把梭 —— 键缺失时它返回 false，
	// 于是"忘了配"会等价于"主动关闭"，一个拼写错误就能让整个域裸奔。
	// 故键缺失时按启用处理并 Warn，让"我其实没配"这件事在启动日志里可见。
	var gate routing.Authenticator
	if cfg.IsSet("auth.require") && !cfg.GetBool("auth.require") {
		logger.Warn("srv-order: auth gate disabled, any inbound rpc is accepted",
			logx.String("hint", "auth.require=false is only valid inside a trusted network"))
	} else {
		gate = auth.Gate{Logger: logger, TTL: ticketTTL(cfg.GetDuration("auth.ticket_ttl"))}
	}

	h := handler.NewOrderHandler(svc, traceOn)
	rt := route.NewRouter(limiter, gate, traceOn, logger)

	a.SetRPCServer(orderrpc.NewServer(&srvrpc.Server{Handler: h, Router: rt, TraceOn: traceOn}, opts...))
	return nil
}

// ticketTTL 身份票据有效期：未配或配了非正值都回落到 auth.DefaultTicketTTL。
//
// 不直接把 GetDuration 的返回值喂给 Gate（0 会被当成"永不过期"还是"立即过期"
// 全看实现怎么判，语义悬空）。显式回落让"配错了"的后果是"用默认值"，
// 而不是"鉴权行为变得无法预测"。
func ticketTTL(d time.Duration) time.Duration {
	if d <= 0 {
		return auth.DefaultTicketTTL
	}
	return d
}
