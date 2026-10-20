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
	"github.com/gospacex/launchx/examples/mall/pkg/handlecheck"
	stockrpc "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen/stock/stock"
	"github.com/gospacex/launchx/examples/mall/srv-stock/internal/handler"
	"github.com/gospacex/launchx/examples/mall/srv-stock/internal/middleware"
	"github.com/gospacex/launchx/examples/mall/srv-stock/internal/repository"
	"github.com/gospacex/launchx/examples/mall/srv-stock/internal/route"
	srvrpc "github.com/gospacex/launchx/examples/mall/srv-stock/internal/rpc"
	"github.com/gospacex/launchx/examples/mall/srv-stock/internal/service"
	"github.com/gospacex/outboxx"
)

// 自保护参数（固定窗口限流）。非地址类常量，故留在代码里：
// 会随环境变的只有地址，一律进 config.yaml。
const (
	rateLimitPerMinute = 600 // 库存是高频读+写热点，上限比商品域宽
	rateLimitWindow    = time.Minute
)

// Setup 是注册给 App.OnSetup 的装配回调：
// 取 DB / Cache / MQ / KV / Lock 句柄 → 断言句柄来源 → 装配
// repository → outbox → service → handler → route → rpc 桩 →
// 用 launchx 产出的 kitex 选项建服务端 → SetRPCServer 回存。
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
	traceOn := a.Config().GetBool("trace.enabled")

	// 句柄来源断言：确认这个 *gorm.DB 是 launchx 的 dbx 交出来的，而不是
	// 谁顺手 gorm.Open 了一个。用错句柄不报错、也能查能写，唯独 SQL 不进
	// 链路 —— 等排障时才发现整层 repository span 是空的就晚了。
	//
	// 只在 trace.on.db 为真时验：enable_trace=false 时 dbx 走 OpenNoTrace，
	// 句柄上没有 trace 回调，那是配置选择，不该让服务起不来。
	if a.Config().GetBool("trace.on.db") {
		if err := handlecheck.VerifyDBX(db); err != nil {
			return errors.New("srv-stock: " + err.Error())
		}
	}
	logger.Info("dbx handle in use",
		logx.String("service", name),
		logx.String("dialector", handlecheck.DialectorName(db)),
		logx.String("sql_traced", strconv.FormatBool(a.Config().GetBool("trace.on.db"))),
		logx.String("layer_chain", "rpc>route>middleware>handler>service>repository>model"))

	// outbox 补发器：MQ 生产者经 App.MQ() 取。未装配（或降级成 nil）时
	// Runner 会显式 Error 而不静默跳过 —— 预占事件丢了，订单域永远
	// 等不到回执，症状是"订单卡在待确认"。
	runner := outboxx.NewRunner(db, a.MQ(), logger, outboxx.Config{
		TraceOn: a.Config().GetBool("trace.on.mq"),
	})
	runner.Start()
	// 退出链：业务回调先于组件停止（此时 DB / MQ 仍可用，本轮补发能收尾）。
	a.OnStop(func() error { runner.Stop(); return nil })

	// ---- 分层链装配：自下而上 ----
	// model（纯数据，无装配） ← repository ← service ← handler ← route ← rpc
	repo := repository.NewStockRepository(db, traceOn)

	// 缓存 / KV / 锁句柄：三者都允许为 nil，由 service 内部按 fail-open 处理
	// 并留日志。库存域的锁缺失必须留 Warn（那是唯一可能超卖的降级路径）。
	svc := service.NewStockService(
		repo,
		a.Cache()[name], // cachex 实例（库存快照缓存）
		a.KV()[name],    // kvx 实例（限流计数 + 预占失败熔断计数）
		a.Lock(),        // lockx 锁器（预占互斥，防超卖）
		traceOn,
		logger,
	)

	// 限流器只在 KV 组件可用时启用（kv 为 nil 时 Allow 恒 true：fail-open）
	//
	// 0 号桶（无身份调用方）的配额单独给一个远小于 per-user 的值：rpc 层对拿
	// 不到 user_id 的方法（列表 / 报表 / 按商品维度的接口）一律传 0，全体无身份
	// 来源共用一个桶。不区分的话，一个匿名来源就能打满整个域的名额，把有身份
	// 的正常流量一起挤掉 —— 限流从"按主体配额"退化成"全域共享桶"。
	// 可在 config.yaml 用 rate_limit.anonymous_per_minute 覆盖；未配时按
	// rateLimitPerMinute/10（见 middleware.NewRateLimiter）。
	var limiter *middleware.RateLimiter
	if kv := a.KV()[name]; kv != nil {
		anonPerMinute := a.Config().GetInt("rate_limit.anonymous_per_minute")
		limiter = middleware.NewRateLimiter(kv, "rate:"+name, rateLimitPerMinute, rateLimitWindow, anonPerMinute, logger)
	}

	h := handler.NewStockHandler(svc, traceOn)
	rt := route.NewRouter(limiter, traceOn, logger)

	a.SetRPCServer(stockrpc.NewServer(&srvrpc.Server{Handler: h, Router: rt}, opts...))
	return nil
}
