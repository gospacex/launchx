// Package app 业务装配层：把 launchx 装配好的句柄（webx 引擎、下游 RPC 客户端
// 选项）接成 handler → downstream 两层，并注册路由。
//
// 这一层不含任何形态分支（不出现 monolith / bff / gateway 等取值）：
// 换形态只改 config.yaml 的 service.mode，与本装配无关。
package app

import (
	"errors"
	"fmt"

	"github.com/cloudwego/kitex/client"
	"github.com/gospacex/configx"
	"github.com/gospacex/logx"
	"github.com/gospacex/webx"
	"go.opentelemetry.io/otel"

	"github.com/gospacex/launchx"
	"github.com/gospacex/launchx/examples/mall/pkg/auth"
	orderrpc "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen/order/order"
	pointsrpc "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen/points/points"
	productrpc "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen/product/product"
	stockrpc "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen/stock/stock"
	"github.com/gospacex/launchx/examples/mall/pkg/routing"
	"github.com/gospacex/launchx/examples/mall/srv-bff/internal/downstream"
	"github.com/gospacex/launchx/examples/mall/srv-bff/internal/handler"
)

// downstreamServices 四个下游域服：既是 App.RPCClientOptions 的入参
// （须与各 srv 注册进 consul 的名字一致），也是 config.yaml 里
// service.upstream.<name> 直连兜底键的后缀。
//
// 顺序即全链路的调用次序：商品 → 库存 → 订单 → 积分（下单编排依此串联）。
var downstreamServices = []string{"srv-product", "srv-stock", "srv-order", "srv-points"}

// authSkipPaths 免鉴权路径前缀。
//
// 只有三类，每一类的共同点是"它们的存在意义就是在不依赖任何东西时仍能回答"：
//   - /ping：下游全挂时仍须 200，否则编排系统会把"下游故障"读成"bff 故障"；
//   - /healthz/*：存活/就绪探针，凭据存储抖一下就让探针失败，会引发一批健康
//     实例被无谓重启，把局部故障放大成全局；
//   - /metrics：Prometheus 拉取侧没有凭据也不该有（它是基础设施采集口）。
var authSkipPaths = map[string]bool{
	"/ping":          true,
	"/healthz":       true,
	"/healthz/live":  true,
	"/healthz/ready": true,
	"/metrics":       true,
}

// authSkip 判定某路径是否免鉴权（前缀匹配 /healthz/*，其余精确匹配）。
func authSkip(path string) bool {
	if authSkipPaths[path] {
		return true
	}
	if len(path) > len("/healthz") && path[:len("/healthz/")] == "/healthz/" {
		return true
	}
	return false
}

// bffRoutes 登记进 pkg/routing 的六条入口路由。
//
// 与四个 srv 的差别：srv 由 rpc 层在委派 handler 前 Enter 路由，bff 没有 rpc
// 层，故由 handler 自己 Enter（见 handler.MallHandler.enter）。中间件链与
// srv 同形（audit → rate_limit），这样 Jaeger 上 bff 与四个域服的入口段结构
// 一致，可直接按 middleware.<entity>.<name> 横向筛。
//
// rate_limit 目前是 **no-op**：bff 档不建数据层（无 kvx 段），
// routing.RateLimit(nil) 按包注释恒放行。把它挂在链上是刻意的 —— 哪天给 bff
// 接上 kvx，限流即刻生效，不用再改装配；且"这一层有没有挂"在链路上看得见，
// 不会出现"配了限流器却从没被调过"。
func bffRoutes(logger logx.Logger) []routing.Route {
	mw := func() []routing.Middleware {
		return []routing.Middleware{routing.Audit(logger), routing.RateLimit(nil)}
	}
	return []routing.Route{
		{Method: "Ping", Kind: routing.KindRead, Middlewares: mw()},
		{Method: "GetProduct", Kind: routing.KindRead, Middlewares: mw()},
		{Method: "GetStock", Kind: routing.KindRead, Middlewares: mw()},
		{Method: "GetOrder", Kind: routing.KindRead, Middlewares: mw()},
		{Method: "GetAccount", Kind: routing.KindRead, Middlewares: mw()},
		{Method: "PlaceOrder", Kind: routing.KindWrite, Middlewares: mw()},
	}
}

// Setup 是注册给 App.OnSetup 的装配回调：
// 取 webx 引擎 + 四个下游 RPC 客户端选项 → 构造下游客户端 → 挂路由。
//
// 失败即返回 error：装配期报错好过带着半装配的状态把服务起出去。
//
// 本函数不注册/注销 consul —— bff 档的注册由 launchx 按 mode 自己做
// （地址取 service.self.addr → service.addr），mall 侧只管业务装配。
func Setup(a *launchx.App) error {
	// webx 引擎由 bff 档装配产出，路由须在 Run 之前挂完。
	engine, ok := a.Engine().(*webx.App)
	if !ok || engine == nil {
		return errors.New("bff: webx engine is unavailable in this mode")
	}

	// 下游客户端选项只能经 App.RPCClientOptions 取（发现经 rpcx 的 coordx
	// resolver，地址来自 xmiddleware.coordx 段），业务不自建注册中心客户端，
	// 也不拼任何目标地址。实测返回类型是 []client.Option（以 any 承载）。
	newClient := func(name string, build func(opts []client.Option) (any, error)) (any, error) {
		raw, err := a.RPCClientOptions(name)
		if err != nil {
			return nil, fmt.Errorf("bff: rpc client options for %s: %w", name, err)
		}
		opts, ok := raw.([]client.Option)
		if !ok {
			return nil, fmt.Errorf("bff: rpc client options for %s are unavailable, check service.mode=bff in config.yaml", name)
		}
		return build(opts)
	}

	productCliAny, err := newClient("srv-product", func(o []client.Option) (any, error) {
		return productrpc.NewClient("srv-product", o...)
	})
	if err != nil {
		return err
	}
	productCli, ok := productCliAny.(productrpc.Client)
	if !ok {
		return errors.New("bff: product client type assertion failed")
	}

	stockCliAny, err := newClient("srv-stock", func(o []client.Option) (any, error) {
		return stockrpc.NewClient("srv-stock", o...)
	})
	if err != nil {
		return err
	}
	stockCli, ok := stockCliAny.(stockrpc.Client)
	if !ok {
		return errors.New("bff: stock client type assertion failed")
	}

	orderCliAny, err := newClient("srv-order", func(o []client.Option) (any, error) {
		return orderrpc.NewClient("srv-order", o...)
	})
	if err != nil {
		return err
	}
	orderCli, ok := orderCliAny.(orderrpc.Client)
	if !ok {
		return errors.New("bff: order client type assertion failed")
	}

	pointsCliAny, err := newClient("srv-points", func(o []client.Option) (any, error) {
		return pointsrpc.NewClient("srv-points", o...)
	})
	if err != nil {
		return err
	}
	pointsCli, ok := pointsCliAny.(pointsrpc.Client)
	if !ok {
		return errors.New("bff: points client type assertion failed")
	}

	// tracer 同样经装配层交接：bff 的 provider 产三类 span —— handler.*（HTTP 入口）、
	// downstream.*（本档业务层）与 CLIENT span（rpcx 的 ClientSpanMiddleware 产出，
	// 选项里已挂，见 rpcx/options.go）。后两者必须同出一个 provider，否则 CLIENT
	// span 掉进全局 noop，Jaeger 上只见 bff 收了请求、见不到四个 srv 各自的跳。
	// 未启用 telemetry 时 App.Tracer() 为 nil，回落到 otel 全局（no-op）：
	// 埋点缺失可以降级，不能让聚合层起不来。
	tp := otel.GetTracerProvider()
	if tr := a.Tracer(); tr != nil {
		tp = tr.TracerProvider()
	}

	cfg := a.Config()
	logger := a.Logger()
	traceOn := cfg.GetBool("trace.enabled")

	// ---------- 鉴权：bff 是唯一的 HTTP 边界，全链路唯一验凭据的地方 ----------
	// 四个域服走 RPC，它们只能信上游送来的身份；这一层漏了，下游再怎么校验
	// 都是形式。故装不上鉴权就不让服务起来 —— 一个 inbound 全收、outbound
	// 全通的聚合层，比"起不来"危险得多。
	if err := installAuth(engine, cfg, logger, traceOn); err != nil {
		return err
	}

	// 入口路由表：audit + rate_limit 中间件链（见 bffRoutes 注释）。
	rt := routing.NewRouter("bff", traceOn, logger, bffRoutes(logger)...)
	logger.Info("bff: route table registered",
		logx.String("entity", "bff"),
		logx.String("methods", fmt.Sprint(rt.Methods())))

	h := handler.New(
		downstream.New(downstream.Clients{
			Product: productCli,
			Stock:   stockCli,
			Order:   orderCli,
			Points:  pointsCli,
		}, tp.Tracer("downstream")),
		tp.Tracer("bff"),
		rt,
		logger,
	)

	// 路由注册必须在 Engine.Use 之后：hertz 在注册那一刻快照中间件链，
	// 顺序反了会得到一个"看起来挂了、实际裸奔"的中间件 —— 静默失效，
	// 没有任何报错能提示这一点。
	engine.Engine.GET("/ping", h.Ping)
	engine.Engine.GET("/product/:id", h.GetProduct)
	engine.Engine.GET("/stock/:product_id", h.GetStock)
	engine.Engine.GET("/order/:id", h.GetOrder)
	engine.Engine.GET("/points/:user_id", h.GetAccount)
	// 复合编排：一次请求串起四个域服 —— 全链路验收就看这一条。
	engine.Engine.POST("/order", h.PlaceOrder)
	return nil
}

// installAuth 装配边界鉴权中间件。
//
// 三条路径对应三种失败姿态，刻意不合并：
//   - 显式关了（auth.require=false）→ Warn 后放行：这是**运维决定**，留痕即可；
//   - 未配凭据（demo.auth_token 为空）→ **报错不起**：中间件挂上了但永远 401，
//     那比"起不来"更难查（链路、日志全正常，只有业务全挂）。
//
// 安全默认值是「要求鉴权」：键缺失时按开启处理 —— 所以这里刻意不写
// `if !cfg.GetBool("auth.require")`，那会在键缺失时静默关掉鉴权。
//
// 凭据表按 micro 的同口径键读取（demo.auth_token → demo.seed_user_id）：
// 生产里这一张表属于签发服务（含过期轮换与吊销），demo 在装配期从配置读一条。
func installAuth(engine *webx.App, cfg configx.Config, logger logx.Logger, traceOn bool) error {
	if cfg.IsSet("auth.require") && !cfg.GetBool("auth.require") {
		logger.Warn("bff: auth disabled by config, every request is accepted",
			logx.String("hint", "auth.require=false exposes all four domains to anyone who can reach :9201"))
		return nil
	}
	if logger == nil {
		logger = logx.GetGlobal()
	}
	token := cfg.GetString("demo.auth_token")
	if token == "" {
		return errors.New("bff: demo.auth_token is empty, auth middleware would reject every request (set it or set auth.require=false)")
	}
	seedUserID := cfg.GetInt64("demo.seed_user_id")
	if seedUserID <= 0 {
		// 不静默回落成 1：0 号/1 号用户是"看起来能用"的假身份，
		// 出了问题没人能回答"当时这个令牌到底是谁"。
		return errors.New("bff: demo.seed_user_id must be a positive user id")
	}

	engine.Engine.Use(auth.Middleware(auth.Config{
		Tokens:  map[string]int64{token: seedUserID},
		Logger:  logger,
		TraceOn: traceOn,
		Skip:    authSkip,
	}))
	logger.Info("bff: auth middleware installed",
		logx.String("scheme", "Bearer"),
		logx.String("header", auth.AuthorizationHeader),
		logx.String("seed_user_id", fmt.Sprint(seedUserID)),
		logx.String("skip", "ping/healthz/metrics"))
	return nil
}
