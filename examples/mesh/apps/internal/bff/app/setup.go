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
	"errors"
	"strconv"

	"github.com/gospacex/logx"
	"github.com/gospacex/webx"

	"github.com/gospacex/launchx"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/bff/downstream"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/bff/handler"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/handles"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/routing"
)

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
	// identity 层装上真实解析器（凭据反查主体）：这一层不拒绝请求，反查失败即匿名，
	// 鉴权由网格的 RequestAuthentication + AuthorizationPolicy 承担。
	engine.Use(routing.Chain(
		routing.WithIdentityResolver(routing.NewTokenResolver(store, logger)),
	)...)

	handler.New(clients, traceOn).Register(engine)

	logger.Info("bff assembled",
		logx.String("service", name),
		logx.String("routes", "6 (ping/order/points/product/stock)"),
		logx.String("chain", "audit>identity>rate_limit>handler>downstream"),
		logx.String("identity", "best-effort：反查失败即匿名，传输层身份由网格承担"))
	return nil
}
