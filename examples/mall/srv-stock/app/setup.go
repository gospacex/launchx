// Package app 业务装配层：把 launchx 装配好的中间件句柄（DB / Cache / MQ /
// KV / Lock 与 kitex 服务端选项）接成 repository → service → rpc 三层，
// 并把 kitex 服务端回存给 launchx 启动与停止。
//
// 这一层不含任何形态分支（不出现 monolith / bff / gateway 等取值）：
// 换形态只改 config.yaml 的 service.mode，与本装配无关。
package app

import (
	"errors"
	"time"

	"github.com/cloudwego/kitex/server"
	"gorm.io/gorm"

	"github.com/gospacex/launchx"
	stockrpc "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen/stock/stock"
	"github.com/gospacex/launchx/examples/mall/pkg/outbox"
	"github.com/gospacex/launchx/examples/mall/pkg/tracex"
	"github.com/gospacex/launchx/examples/mall/srv-stock/internal/middleware"
	"github.com/gospacex/launchx/examples/mall/srv-stock/internal/repository"
	srvrpc "github.com/gospacex/launchx/examples/mall/srv-stock/internal/rpc"
	"github.com/gospacex/launchx/examples/mall/srv-stock/internal/service"
)

// 自保护参数（固定窗口限流）。非地址类常量，故留在代码里：
// 会随环境变的只有地址，一律进 config.yaml。
const (
	rateLimitPerMinute = 600 // 库存是高频读+写热点，上限比商品域宽
	rateLimitWindow    = time.Minute
)

// Setup 是注册给 App.OnSetup 的装配回调：
// 取 DB / Cache / MQ / KV / Lock 句柄 → 装配 repository → outbox → service
// → rpc 桩 → 用 launchx 产出的 kitex 选项建服务端 → SetRPCServer 回存。
//
// 失败即返回 error：装配期报错好过带着半装配的状态把服务起出去 ——
// 库存域尤其如此：缓存/锁缺失只该降级，库连不上则必须起不来。
//
// 本函数不注册/注销 consul —— srv 档的注册由 launchx 按 mode 自己做
// （地址取 service.self.addr），业务侧只管装配。
func Setup(a *launchx.App) error {
	// DB 句柄只能经 App.DB() 取（业务不自建连接、不直引驱动）。
	db, ok := a.DB().(*gorm.DB)
	if !ok || db == nil {
		return errors.New("srv-stock: db component is unavailable, check xmiddleware.sqlx in config.yaml")
	}
	// kitex 服务端选项由 srv 档装配产出（监听地址、注册、trace suite 全在里面），
	// 业务只做类型断言，绝不自己拼地址。
	opts, ok := a.RPCServerOptions().([]server.Option)
	if !ok {
		return errors.New("srv-stock: rpc server options are unavailable, check service.mode=srv in config.yaml")
	}

	name := a.Config().GetString("service.name")
	logger := a.Logger()

	// 业务埋点的 provider 由装配层交接，业务分层因此不必去摸 otel 全局单例
	// （分层边界要求它不认识框架）。telemetry 未启用时 provider 为 nil，
	// tracex 会回落到 otel 全局（no-op），行为与之前一致。
	if tp := a.Tracer(); tp != nil {
		tracex.UseProvider(tp.TracerProvider())
	}

	// outbox 补发器：MQ 生产者经 App.MQ() 取。未装配（或降级成 nil）时
	// Runner 会显式 Error 而不静默跳过 —— 预占事件丢了，订单域永远
	// 等不到回执，症状是"订单卡在待确认"。
	runner := outbox.NewRunner(db, a.MQ(), logger, outbox.Config{
		TraceOn: a.Config().GetBool("trace.on.mq"),
	})
	runner.Start()
	// 退出链：业务回调先于组件停止（此时 DB / MQ 仍可用，本轮补发能收尾）。
	a.OnStop(func() error { runner.Stop(); return nil })

	repo := repository.NewStockRepository(db)

	// 缓存 / KV / 锁句柄：三者都允许为 nil，由 service 内部按 fail-open 处理
	// 并留日志。库存域的锁缺失必须留 Warn（那是唯一可能超卖的降级路径）。
	svc := service.NewStockService(
		repo,
		a.Cache()[name], // cachex 实例（库存快照缓存）
		a.KV()[name],    // kvx 实例（限流计数 + 预占失败熔断计数）
		a.Lock(),        // lockx 锁器（预占互斥，防超卖）
		a.Config().GetBool("trace.enabled"),
		logger,
	)

	// 限流器只在 KV 组件可用时启用（kv 为 nil 时 Allow 恒 true：fail-open）
	var limiter *middleware.RateLimiter
	if kv := a.KV()[name]; kv != nil {
		limiter = middleware.NewRateLimiter(kv, "rate:"+name, rateLimitPerMinute, rateLimitWindow)
	}

	a.SetRPCServer(stockrpc.NewServer(&srvrpc.Server{Svc: svc, Limiter: limiter}, opts...))
	return nil
}
