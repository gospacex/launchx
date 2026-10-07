// Package app 业务装配层：把 launchx 装配好的中间件句柄（DB / 缓存 / 消息 / 检索 /
// 文档库 / 调用链 / HTTP 引擎）接成 middleware → handler → service → repository 与组件
// 适配层，并注册路由。
//
// 这一层不含任何形态分支（不出现 monolith / bff / gateway 等取值）：
// 换形态只改 config.yaml 的 service.mode，与本装配无关。
//
// 组件的**降级姿态在这里定**：DB 缺失即 fail-fast（strict.db 已在库侧裁决），
// 其余组件缺失只 Warn 并把对应能力关掉 —— 但必须显式 Warn：静默跳过会把
// 「配了段却没接上」变成无声失败，Jaeger 上表现为 span 莫名消失且无人告警。
package app

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/gospacex/logx"
	"github.com/gospacex/traceflag"
	"github.com/gospacex/webx"
	"gorm.io/gorm"

	"github.com/gospacex/launchx"
	"github.com/gospacex/launchx/examples/monolith/internal/cache"
	"github.com/gospacex/launchx/examples/monolith/internal/docs"
	"github.com/gospacex/launchx/examples/monolith/internal/handler"
	"github.com/gospacex/launchx/examples/monolith/internal/handlesrc"
	"github.com/gospacex/launchx/examples/monolith/internal/messaging"
	"github.com/gospacex/launchx/examples/monolith/internal/middleware"
	"github.com/gospacex/launchx/examples/monolith/internal/ratelimit"
	"github.com/gospacex/launchx/examples/monolith/internal/repository"
	"github.com/gospacex/launchx/examples/monolith/internal/search"
	"github.com/gospacex/launchx/examples/monolith/internal/service"
)

// tracerName 业务侧 instrumentation 名（区别于 dbx / webx / 各组件 SDK 的 tracer 名）。
const tracerName = "order"

// demoUserID 演示令牌绑定的用户 ID（scripts/bring-up.sh 的种子用户之一）。
const demoUserID = 1

// Setup 是注册给 App.OnSetup 的装配回调：
// 取各组件句柄 → 构造分层 → 把中间件与路由挂到 webx 引擎上。
//
// 失败即返回 error：装配期报错好过带着半装配的状态把服务起出去。
func Setup(a *launchx.App) error {
	// DB 句柄只能经 App.DB() 取（业务不自建连接、不直引驱动）。
	db, ok := a.DB().(*gorm.DB)
	if !ok || db == nil {
		return errors.New("monolith: db component is unavailable, check xmiddleware.sqlx in config.yaml")
	}
	// webx 引擎由 monolith 档装配产出，路由须在 Run 之前挂完。
	engine, ok := a.Engine().(*webx.App)
	if !ok || engine == nil {
		return errors.New("monolith: webx engine is unavailable in this mode")
	}

	tr := a.Tracer().TracerProvider().Tracer(tracerName)
	cfg := a.Config()
	logger := a.Logger()
	if logger == nil {
		logger = logx.GetGlobal()
	}
	name := cfg.GetString("service.name")

	// ---------- 存储句柄的来源与入链确认 ----------
	// 必须在取用句柄之后、装配分层之前做：这时句柄刚由 launchx 的 builder 交出，
	// 还没被任何业务代码包一层（包过之后就不是同一个实例了，判据失效）。
	//
	// 验两件事：①句柄来源合规（不是自己开的）；②trace.on 要求入链时它真的会产
	// span。两者都不满足会照样能读写，只是走着走着链路就空了一块。
	checks, err := handlesrc.VerifyOrError(a)
	if err != nil {
		return err
	}
	for _, c := range checks {
		if !c.Present {
			continue
		}
		logger.Info("storage handle verified",
			logx.String("component", c.Component),
			logx.String("backend", c.Backend),
			logx.String("joins_trace", strconv.FormatBool(c.Observed)),
			logx.String("probe", c.Probe))
	}

	repo := repository.New(db, tr)

	// ---------- 缓存（订单 read-through + 令牌查询）----------
	var orderCache *cache.Order
	var tokenStore *cache.TokenStore
	if inst := a.Cache()[name]; inst != nil {
		ttl := durationOrDefault(cfg.GetString("demo.cache_ttl"), time.Minute)
		orderCache = cache.NewOrder(inst, ttl, tr)
		store := cache.NewTokenStore(inst)
		tokenStore = &store
	} else {
		logger.Warn("monolith: cache unavailable, running without read-through cache",
			logx.String("hint", "check xmiddleware.cachex"))
	}

	// ---------- KV（限流计数，原子自增）----------
	// 计数不与缓存共用句柄：cachex 没有 Incr，计数只能读-改-写，并发下会系统性
	// 低估（详见 internal/ratelimit 包注释）。这里缺失的表现是**不限流**而非报错，
	// 属于"看不见的失效"，故必须在装配期显式留痕。
	var counter ratelimit.Counter
	if inst := a.KV()[name]; inst != nil {
		counter = ratelimit.NewCounter(inst)
	} else {
		logger.Warn("monolith: kv unavailable, rate limiter is disabled (fail-open)",
			logx.String("hint", "check xmiddleware.kvx"))
	}

	// ---------- 消息（下单事件投递）----------
	var orderEvents *messaging.Order
	// 注意键是 consumer.topic：producer 的目标 topic 在 mqx 里就是从这个键出来的，
	// producer.topic 看似更贴切但当前不生效（详见 config.yaml 该段的注释）。
	if p := a.MQ(); p != nil {
		orderEvents = messaging.NewOrder(p, cfg.GetString("xmiddleware.mqx.instances."+name+".consumer.topic"), tr)
	} else {
		logger.Warn("monolith: mq unavailable, order events are not published",
			logx.String("hint", "check xmiddleware.mqx"))
	}

	// ---------- 检索（ES 索引）----------
	var orderIndex *search.Order
	if cli := a.Search(); cli != nil {
		orderIndex = search.NewOrder(cli, cfg.GetString("demo.order_index"), tr)
	} else {
		logger.Warn("monolith: search unavailable, orders are not indexed",
			logx.String("hint", "check xmiddleware.searchx"))
	}

	// ---------- 文档库（订单快照）----------
	// 集合句柄在这层取好：缺库缺集合应当在装配期暴露，不该拖到第一次写数据时。
	var snapshots *docs.Order
	if inst := a.Mongo()[name]; inst != nil {
		snapshots = docs.NewOrder(inst.Collection(cfg.GetString("demo.snapshot_collection")), tr)
	} else {
		logger.Warn("monolith: mongo unavailable, order snapshots are not stored",
			logx.String("hint", "check xmiddleware.mongodbx"))
	}

	svc := service.New(repo, orderCache, orderEvents, orderIndex, snapshots, tr)

	// ---------- 业务中间件：鉴权 ----------
	// 凭据走缓存，故只在缓存可用时装配；令牌为空则不注册 /secure 路由，
	// 而不是注册一个恒 401 的空壳。
	var auth app.HandlerFunc
	switch {
	case tokenStore == nil:
		logger.Warn("monolith: auth middleware disabled, /secure routes are not registered",
			logx.String("reason", "cache unavailable"))
	case cfg.GetString("demo.auth_token") == "":
		logger.Warn("monolith: auth middleware disabled, /secure routes are not registered",
			logx.String("reason", "demo.auth_token is empty"))
	default:
		token := cfg.GetString("demo.auth_token")
		// 播种演示令牌：让鉴权链路上的那次 cache 查询真的有东西可查。
		// 生产里这活属于签发服务（含过期轮换），demo 里简化成启动即写一条。
		//
		// 播种期间临时关掉 cachex 的埋点：这里没有请求上下文，留下的 SET span 会
		// 成为一根孤儿单 span trace —— 正是 Jaeger 里最容易被读成"链路断了"的噪声。
		// 关的是组件级开关，播完立刻还原为原值（不是写死 true，避免把"全局关埋点"
		// 的部署悄悄打开）。
		before := traceflag.EnabledFor("cachex")
		traceflag.SetComponent("cachex", false)
		err := tokenStore.GrantToken(context.Background(), token, demoUserID, time.Hour)
		traceflag.SetComponent("cachex", before)
		if err != nil {
			logger.Warn("monolith: seed demo token failed", logx.Err(err))
		}
		tokMW := middleware.NewToken(tokenStore.Resolve, tr)
		auth = tokMW.Handle
	}

	// ---------- 业务中间件：限流（全局）----------
	// 有外部依赖的中间件必须在每条链上都出现 —— 挂全局是为了让每个 span 树
	// 都能看到它，而不是只在个别端点上"演示一次"。
	// 门控换成 KV 就绪与否（不再是缓存可用）：计数数字段的实际依赖变了，
	// 跟着旧门控走会出现"缓存挂了但计数其实还能用"的误判。
	if counter.Ready() {
		rl := middleware.NewRateLimit(counter, cfg.GetInt64("demo.rate_limit_per_min"), time.Minute, tr)
		engine.Engine.Use(rl.Handle)
	}

	RegisterRoutes(engine.Engine, handler.New(svc, tr), auth)
	return nil
}

// durationOrDefault 解析时长串，空串或非法都回落到 def。
//
// 配置里的 TTL / 窗口多是字符串（"60s"），写错一个字符就让 ttl 变成 0 ——
// 缓存会表现为「写了等于没写」，这种故障极难反查，故一律显式回落。
func durationOrDefault(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}
