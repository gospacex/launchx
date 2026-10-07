// Package app 业务装配层：把 launchx 装配好的中间件句柄（DB / Cache / MQ 与
// kitex 服务端选项）接成 repository → service → rpc 三层，并把 kitex 服务端
// 回存给 launchx 启动与停止。
//
// 这一层不含任何形态分支（不出现 monolith / bff / gateway 等取值）：
// 换形态只改 config.yaml 的 service.mode，与本装配无关。
package app

import (
	"errors"
	"time"

	"github.com/cloudwego/kitex/server"
	"github.com/gospacex/logx"
	"gorm.io/gorm"

	"github.com/gospacex/launchx"
	"github.com/gospacex/launchx/examples/micro/srv/internal/middleware"
	"github.com/gospacex/launchx/examples/micro/srv/internal/repository"
	srvrpc "github.com/gospacex/launchx/examples/micro/srv/internal/rpc"
	"github.com/gospacex/launchx/examples/micro/srv/internal/service"
	"github.com/gospacex/launchx/examples/micro/srv/internal/tracex"
	orderrpc "github.com/gospacex/launchx/examples/micro/srv/pkg/kitex_gen/order/order"
)

// 自保护参数（固定窗口限流）。非地址类常量，故留在代码里：
// 会随环境变的只有地址，一律进 config.yaml。
const (
	rateLimitPerMinute = 100
	rateLimitWindow    = time.Minute
)

// Setup 是注册给 App.OnSetup 的装配回调：
// 取 DB / Cache / MQ 句柄 → 装配 repository → outbox → service → rpc 桩
// → 用 launchx 产出的 kitex 选项建服务端 → SetRPCServer 回存。
//
// 失败即返回 error：装配期报错好过带着半装配的状态把服务起出去。
//
// 本函数不注册/注销 consul —— srv 档的注册由 launchx 按 mode 自己做
// （地址取 service.self.addr），demo 侧只管业务装配。
func Setup(a *launchx.App) error {
	// DB 句柄只能经 App.DB() 取（业务不自建连接、不直引驱动）。
	db, ok := a.DB().(*gorm.DB)
	if !ok || db == nil {
		return errors.New("srv: db component is unavailable, check xmiddleware.sqlx in config.yaml")
	}
	// kitex 服务端选项由 srv 档装配产出（监听地址、注册、trace suite 全在里面），
	// 业务只做类型断言，绝不自己拼地址。
	opts, ok := a.RPCServerOptions().([]server.Option)
	if !ok {
		return errors.New("srv: rpc server options are unavailable, check service.mode=srv in config.yaml")
	}

	name := a.Config().GetString("service.name")
	logger := a.Logger()

	// 业务埋点的 provider 由装配层交接，业务分层因此不必去摸 otel 全局单例
	// （分层边界要求它不认识框架）。telemetry 未启用时 provider 为 nil，
	// tracex 会回落到 otel 全局（no-op），行为与之前一致。
	if tp := a.Tracer(); tp != nil {
		tracex.UseProvider(tp.TracerProvider())
	}

	repo := repository.NewOrderRepository(db)

	// MQ 生产者经 App.MQ() 取。未装配（或降级成 nil）时 runner 会显式告警而不
	// 静默跳过：那种情况下下单仍在写 status=0 的事件却无人投递，表会无声膨胀。
	runner := service.NewOutboxRunner(repo, a.MQ(), logger, a.Config().GetBool("trace.on.mq"))
	runner.Start()
	// 退出链：业务回调先于组件停止（此时 DB / MQ 仍可用，本轮补发能收尾）。
	a.OnStop(func() error { runner.Stop(); return nil })

	// 商品域客户端：srv 档不产出下游客户端选项（那是 bff/gateway 档的事），
	// 故留空 —— service 层已有「无预占」分支，总额按明细计算。
	if logger != nil {
		logger.Warn("srv: product client is not wired in this mode, create-order runs without stock reservation")
	}
	svc := service.NewOrderService(repo, runner, nil, a.Config().GetBool("trace.enabled"), logger)

	// 限流器只在 KV 组件可用时启用（kv 为 nil 时 Allow 恒 true：fail-open）。
	// 计数走 kvx 而不是 cachex：自保护计数需要原子自增，cachex 只有 Get/Set，
	// 并发下 counting is lost-updated（详见 middleware 包注释）。
	var limiter *middleware.RateLimiter
	if inst := a.KV()[name]; inst != nil {
		limiter = middleware.NewRateLimiter(inst, "rate:"+name, rateLimitPerMinute, rateLimitWindow)
	} else if logger != nil {
		// 门控为 nil 不报错，但必须留痕：静默不限流会把「限流组件没接上」
		// 伪装成「流量没超」，事后无从分辨。
		logger.Warn("srv: kv unavailable, rate limiter is disabled (fail-open)",
			logx.String("hint", "check xmiddleware.kvx"))
	}

	a.SetRPCServer(orderrpc.NewServer(&srvrpc.Server{Svc: svc, Limiter: limiter}, opts...))
	return nil
}
