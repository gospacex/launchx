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
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/cloudwego/kitex/server"
	"github.com/gospacex/logx"
	"gorm.io/gorm"

	"github.com/gospacex/launchx"
	"github.com/gospacex/launchx/examples/micro/pkg/auth"
	"github.com/gospacex/launchx/examples/micro/pkg/handlecheck"
	pointsrpc "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/points/points"
	"github.com/gospacex/launchx/examples/micro/pkg/routing"
	dtmsetup "github.com/gospacex/launchx/examples/micro/srv-points/internal/dtm"
	"github.com/gospacex/launchx/examples/micro/srv-points/internal/handler"
	"github.com/gospacex/launchx/examples/micro/srv-points/internal/middleware"
	"github.com/gospacex/launchx/examples/micro/srv-points/internal/repository"
	"github.com/gospacex/launchx/examples/micro/srv-points/internal/route"
	srvrpc "github.com/gospacex/launchx/examples/micro/srv-points/internal/rpc"
	"github.com/gospacex/launchx/examples/micro/srv-points/internal/service"
	"github.com/gospacex/launchx/examples/pkg/outbox"
)

// 自保护参数（固定窗口限流）。非地址类常量，故留在代码里：
// 会随环境变的只有地址，一律进 config.yaml。
const (
	rateLimitPerMinute = 120 // 积分是写多读少的域（入账/消费都要过），上限比商品域紧
	rateLimitWindow    = time.Minute
	// branchStopTimeout 分支面优雅停机的上限。
	//
	// 为什么要有上限：GracefulStop 会等在途的 barrier 事务做完，而 barrier 事务
	// 里含着一次真实落库 —— 若那条分支正在等锁（等另一个分支的账户行锁），
	// 无上限的停机会把整个进程挂住：进程挂着 → dtm 的补偿链也全部停滞。
	branchStopTimeout = 5 * time.Second
)

// Setup 是注册给 App.OnSetup 的装配回调：
// 取 DB / Cache / MQ / KV / Lock 句柄 → 断言句柄来源 → 装配
// repository → outbox → service → handler → route → rpc 桩 →
// 用 launchx 产出的 kitex 选项建服务端 → SetRPCServer 回存。
//
// 失败即返回 error：装配期报错好过带着半装配的状态把服务起出去 ——
// 积分域尤其如此：缓存/锁缺失只该降级，库连不上则必须起不来。
//
// 本函数不注册/注销 consul —— srv 档的注册由 launchx 按 mode 自己做
// （地址取 service.self.addr），业务侧只管装配。
func Setup(a *launchx.App) error {
	// DB 句柄只能经 App.DB() 取（业务不自建连接、不直引驱动）。
	db, ok := a.DB().(*gorm.DB)
	if !ok || db == nil {
		return errors.New("srv-points: db component is unavailable, check xmiddleware.sqlx in config.yaml")
	}
	// kitex 服务端选项由 srv 档装配产出（监听地址、注册、trace suite 全在里面），
	// 业务只做类型断言，绝不自己拼地址。
	opts, ok := a.RPCServerOptions().([]server.Option)
	if !ok {
		return errors.New("srv-points: rpc server options are unavailable, check service.mode=srv in config.yaml")
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
		return errors.New("srv-points: " + err.Error())
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
	// Runner 会显式 Error 而不静默跳过 —— 积分变动事件是链路终点，
	// 丢了就没有对账依据。
	runner := outbox.NewRunner(db, a.MQ(), logger, outbox.Config{
		TraceOn: cfg.GetBool("trace.on.mq"),
	})
	runner.Start()
	// 退出链：业务回调先于组件停止（此时 DB / MQ 仍可用，本轮补发能收尾）。
	a.OnStop(func() error { runner.Stop(); return nil })

	// ---- 分层链装配：自下而上 ----
	// model（纯数据，无装配） ← repository ← service ← handler ← route ← rpc
	repo := repository.NewPointsRepository(db, traceOn)

	// dtm 分支写通道：与 repo 同一个 gorm 句柄，但形态不同 —— 它取的是底层
	// *sql.DB，因为官方 barrier 要在自己的事务里插行（见 repository/dtmstore.go
	// 的包注释）。构造失败即启动失败：分支面不可用却照常起服务的话，dtm 会一直
	// 重试一个永远调不通的分支，那笔全局事务悬到超时。
	dtmStore, err := repository.NewDtmBranchStore(db)
	if err != nil {
		return errors.New("srv-points: " + err.Error())
	}

	// 缓存 / KV / 锁句柄：三者都允许为 nil，由 service 内部按 fail-open 处理
	// 并留日志。KV 缺失时幂等只剩数据库唯一键一层（仍然安全，只是慢一点）。
	svc := service.NewPointsService(
		repo,
		a.Cache()[name], // cachex 实例（账户缓存）
		a.KV()[name],    // kvx 实例（限流计数 + 幂等键）
		a.Lock(),        // lockx 锁器（入账/消费互斥）
		dtmStore,        // dtm 分支写通道（Saga/TCC 五个方法的落库通道）
		traceOn,
		logger,
	)

	// ---- dtm 分支面 listener（Saga Debit/Refund + TCC 三段）----
	// 地址只来自 config（dtm.branch.listen_addr，见 config.yaml）：Go 代码里
	// 不出现任何地址字面量，否则换个部署形态就得改代码再发一版。
	// **不注册 consul**（分支只被 dtm 回调，注册了反而让调用方调到它）。
	branch, err := dtmsetup.Start(cfg.GetString(dtmsetup.ListenAddrKey), svc, traceOn, logger)
	if err != nil {
		return errors.New("srv-points: dtm branch face: " + err.Error())
	}
	// 退出链：分支面先停（在途的 barrier 事务做完），再停组件。
	a.OnStop(func() error {
		ctx, cancel := context.WithTimeout(context.Background(), branchStopTimeout)
		defer cancel()
		return branch.Stop(ctx)
	})

	// 限流器只在 KV 组件可用时启用（kv 为 nil 时 Allow 恒 true：fail-open）
	var limiter *middleware.RateLimiter
	if kv := a.KV()[name]; kv != nil {
		limiter = middleware.NewRateLimiter(kv, "rate:"+name, rateLimitPerMinute, rateLimitWindow, logger)
	}

	h := handler.NewPointsHandler(svc, traceOn)
	// ---------- 鉴权闸门：只校验边界层透传下来的身份，不重复验签 ----------
	// 默认**启用**：鉴权是安全默认值，关掉它必须是显式配置的结果。
	// 这里刻意不写成 `cfg.GetBool("auth.require")` 一把梭 —— 键缺失时它返回 false，
	// 于是"忘了配"会等价于"主动关闭"，一个拼写错误就能让整个域裸奔。
	// 故键缺失时按启用处理并 Warn，让"我其实没配"这件事在启动日志里可见。
	var gate routing.Authenticator
	if cfg.IsSet("auth.require") && !cfg.GetBool("auth.require") {
		logger.Warn("srv-points: auth gate disabled, any inbound rpc is accepted",
			logx.String("hint", "auth.require=false is only valid inside a trusted network"))
	} else {
		gate = auth.Gate{Logger: logger, TTL: ticketTTL(cfg.GetDuration("auth.ticket_ttl"))}
	}

	rt := route.NewRouter(limiter, gate, traceOn, logger)

	a.SetRPCServer(pointsrpc.NewServer(&srvrpc.Server{Handler: h, Router: rt, TraceOn: traceOn}, opts...))
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
