// Package app bff 装配：`Engine()`（webx 路由 + 自带健康端点）+ `Cache()[name]`
// （边界凭据存储：`token → user_id` 反查）+ `RPCClientOptions(<域>)`（四域 kitex
// 客户端，resolver 落到 K8s Service 名）。
//
// 刻意**不取** DB() 与 MQ()：bff 是 HTTP 边界，不落业务数据、不发业务事件。
// 它唯一的数据入口是下游四域客户端与自己的凭据缓存（后者存的不是业务数据）。
//
// 也不取 Tracer()：全生态的 tracer 取法统一为「向 otel 全局现取」（otelx/layer
// 就是这么做的），全局 provider 由 launchx 的 telemetry 步骤**无条件**注册，
// 业务侧再交接一次只会让「本层与 CLIENT span 分家」变成取决于装配顺序。
package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gospacex/logx"
	"github.com/gospacex/traceflag"
	"github.com/gospacex/webx"

	"github.com/gospacex/launchx"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/bff/downstream"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/bff/handler"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/handles"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/routing"
)

// identitySkipPaths 豁免强制鉴权的路径。
//
// **只豁免健康探针**（K8s 发起、不带凭据）：凭据存储抖一下就让探针 401，会让
// 编排系统重启一批本来健康的实例，把局部故障放大成全局。
//
// 业务路径一条都不豁免，包括 `/ping`：它虽然不触达下游，但「无凭据必须 401」
// 是入口闸门的反向探针（见 docs/verify.md 的鉴权四条断言），豁免它就把这条
// 判据拆了。探针够用即可，少豁免一条就多一分保障。
var identitySkipPaths = map[string]bool{
	"/healthz/live":  true,
	"/healthz/ready": true,
}

// Setup 注册给 App.OnSetup。
//
// 顺序即纪律：先取句柄 → 再自检 → 后装配。反过来（先装配再自检）判据就失效了：
// 句柄被业务包过一层之后，自检看到的已不是 launchx 交出的那个实例。
func Setup(a *launchx.App) error {
	// ---- 1. 取句柄 ----
	// webx 引擎由 bff 档装配产出；路由与中间件都必须在 Run 之前挂完
	// （路由树只读之后才开始并发 Serve）。
	engine, ok := a.Engine().(*webx.App)
	if !ok || engine == nil {
		return errors.New("bff: webx engine unavailable (check service.mode=bff)")
	}
	cfg := a.Config()
	logger := a.Logger()
	// traceOn 是分层埋点的总闸：一次取出交给 handler（bff 的 handler.<Op> span）。
	traceOn := cfg.GetBool("trace.enabled")
	// 实例名必须等于 service.name（spec mesh-app-handles）：它同时是 cache 实例的键。
	// 写成字面量会在改名后悄悄取到 nil，而 bff 的凭据反查全靠它。
	name := cfg.GetString("service.name")

	// ---- 2. 句柄自检：来源合规 + 入链有效 ----
	// 凭据缓存是 bff 的硬前提：缺了它边界反查不出主体，所有请求静默退化成匿名。
	if err := handles.VerifyOrError(a, "cache"); err != nil {
		return errors.New("bff: " + err.Error())
	}
	for _, c := range handles.Verify(a) {
		if !c.Present {
			continue
		}
		logger.Info("handle verified",
			logx.String("component", c.Component), logx.String("backend", c.Backend),
			logx.String("joins_trace", strconv.FormatBool(c.Observed)), logx.String("probe", c.Probe))
	}
	// VerifyOrError 判过「该实例在」（checkCache 的判据就是 a.Cache()[name] != nil），
	// 这里再判一次 nil 是为了挡住**静默**的那条路：store 为 nil 时
	// routing.NewTokenResolver 返回 nil resolver —— 身份层照常开 span、请求照常
	// 放行，只是永远认不出人。装配期能报出来的错，不要留到运行期靠日志发现。
	store := a.Cache()[name]
	if store == nil {
		return errors.New("bff: cache instance for service.name=" + name +
			" is unavailable (xmiddleware.cachex 的实例名须与 service.name 一致)")
	}

	// ---- 2.5 入口鉴权：播种演示令牌 + 定闸门姿态 ----
	//
	// 这是 2026-10-10 企业级加固的核心一处。改这之前，apps 组**没有任何入口
	// 鉴权**：整个 examples/mesh 里不存在 RequestAuthentication（Istio 需要真实
	// issuer + JWKS，本示例没有身份提供方），identity 层又是 fail-open 反查，
	// 于是匿名请求能下单、能读任意订单，而 5 处注释却写着「鉴权已下沉网格」。
	// 现在口径与 examples/micro 统一：**入口鉴权由应用侧自己做**。
	//
	// 三条失败姿态刻意不合并（照抄 micro 的判定，理由同源）：
	//   - 显式关了（auth.require=false）→ 放行但 Warn：这是运维决定，留痕即可；
	//   - 开了但没播种令牌 → **报错不起**：中间件会挂上而每个请求都 401，
	//     那比起不来更难查（链路/限流/日志全正常，只有业务全挂）；
	//   - seed_user_id 为 0 → **报错不起**：反查拿到 0 会被闸门当匿名拒掉，
	//     症状与「令牌没播种」一模一样。
	requireAuth := true // 缺省即要求（安全默认值；只有显式 false 才关）
	if cfg.IsSet("auth.require") {
		requireAuth = cfg.GetBool("auth.require")
	}
	token := cfg.GetString("demo.auth_token")
	switch {
	case !requireAuth:
		logger.Warn("bff: auth disabled by config, every request is accepted",
			logx.String("consequence", "六条路由对任何能到达 :8081 的调用方开放"),
			logx.String("probe", "把 auth.require 改回 true（或删掉该键，缺省即 true）"))
	case token == "":
		return errors.New("bff: demo.auth_token is empty while auth.require is on — " +
			"identity 层会挂上闸门而每个请求都 401（比起不来更难查）；" +
			"请播种令牌，或显式设 auth.require=false")
	default:
		uid := cfg.GetInt64("demo.seed_user_id")
		if uid == 0 {
			return errors.New("bff: demo.seed_user_id must be a positive user id " +
				"(反查拿到 0 会被入口闸门当匿名拒掉)")
		}
		// 播种期间临时关掉 cachex 埋点：这里没有请求上下文，留下的 SET span
		// 会是一根孤儿单 span trace —— 正是 Jaeger 里最容易被读成「链路断了」
		// 的噪声。关的是组件级开关，播完立刻还原为原值（不是写死 true，
		// 避免把「全局关埋点」的部署悄悄打开）。
		before := traceflag.EnabledFor("cachex")
		traceflag.SetComponent("cachex", false)
		serr := store.Set(context.Background(), routing.TokenKeyPrefix+token,
			strconv.FormatInt(uid, 10), time.Hour)
		traceflag.SetComponent("cachex", before)
		if serr != nil {
			return fmt.Errorf("bff: seed demo token failed: %w", serr)
		}
		logger.Info("bff: demo credential seeded",
			logx.String("key", routing.TokenKeyPrefix+token),
			logx.Int("user_id", int(uid)))
	}

	// ---- 3. 装配 ----
	clients, err := downstream.New(a)
	if err != nil {
		return err
	}

	// 边界中间件链：audit → identity → rate_limit（顺序是契约，见 pkg/routing）。
	//
	// **必须在注册路由之前**挂上：hertz 的 Engine.Use 是
	// `group.Handlers = append(group.Handlers, mw...)`，而路由在自己被注册的那一刻
	// 就把 `group.Handlers` 快照进自己的 handler 链 —— 先注册再 Use，中间件对已注册
	// 的路由**完全不起作用**，且不报错。
	//
	// identity 层装上真实解析器（凭据反查主体）；**闸门只在 auth.require 打开时装**。
	//
	// 「关掉鉴权」必须真的不装闸门——对齐 examples/micro 的 installAuth（它在该分支
	// 直接 return，不挂中间件）。若反过来「装了闸门再放行」，auth.require=false 这个
	// 开关就成了谎话：日志说「每个请求都放行」，实际每个无凭据请求都 401 —— 正是本仓
	// 最忌讳的「配置写着 A、行为是 B」。
	//
	// 打开时：解析不出主体且不在豁免表里的请求一律 401。豁免只有健康探针两条，
	// 理由见 identitySkipPaths。
	opts := []routing.Option{routing.WithIdentityResolver(routing.NewTokenResolver(store, logger))}
	if requireAuth {
		opts = append(opts, routing.WithRequiredIdentity(func(p string) bool {
			return identitySkipPaths[strings.TrimSuffix(p, "/")]
		}))
	}
	engine.Use(routing.Chain(opts...)...)

	handler.New(clients, traceOn).Register(engine)

	gate := "identity>handler>downstream（无凭据 401）"
	if !requireAuth {
		gate = "identity>handler>downstream（鉴权已按配置关闭）"
	}
	logger.Info("bff assembled",
		logx.String("service", name),
		logx.String("routes", "6 (ping/order/points/product/stock)"),
		logx.String("chain", "audit>identity>rate_limit>"+gate),
		logx.String("identity", "Bearer 令牌反查（cachex db2）+ 强制闸门，豁免仅 /healthz/{live,ready}"))
	return nil
}
