// Package app 业务装配层：把 launchx 装配好的句柄（webx 引擎、下游 RPC 客户端
// 选项、dtm 编排器）接成 handler → downstream / dtm 三层，并注册路由。
//
// 这一层不含任何形态分支（不出现 monolith / bff / gateway 等取值）：
// 换形态只改 config.yaml 的 service.mode，与本装配无关。
package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/kitex/client"
	"github.com/gospacex/configx"
	"github.com/gospacex/logx"
	"github.com/gospacex/traceflag"
	"github.com/gospacex/webx"

	"github.com/gospacex/launchx"
	"github.com/gospacex/launchx/examples/micro/bff/internal/downstream"
	bffdtm "github.com/gospacex/launchx/examples/micro/bff/internal/dtm"
	"github.com/gospacex/launchx/examples/micro/bff/internal/handler"
	"github.com/gospacex/launchx/examples/micro/pkg/auth"
	orderrpc "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/order/order"
	pointsrpc "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/points/points"
	productrpc "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/product/product"
	stockrpc "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/stock/stock"
)

// downstreamServices 四个下游域服：既是 App.RPCClientOptions 的入参
// （须与各 srv 注册进 consul 的名字一致），也是 config.yaml 里
// service.upstream.<name> 直连兜底键的后缀。
//
// 顺序即全链路的调用次序：商品 → 库存 → 订单 → 积分（下单编排依此串联）。
var downstreamServices = []string{"demo-product", "demo-stock", "demo-order", "demo-points"}

// Setup 是注册给 App.OnSetup 的装配回调：
// 取 webx 引擎 + 四个下游 RPC 客户端选项 → 构造下游客户端 → 挂路由。
//
// 失败即返回 error：装配期报错好过带着半装配的状态把服务起出去。
//
// 本函数不注册/注销 consul —— bff 档的注册由 launchx 按 mode 自己做
// （地址取 service.self.addr → service.addr），micro 侧只管业务装配。
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

	productCliAny, err := newClient("demo-product", func(o []client.Option) (any, error) {
		return productrpc.NewClient("demo-product", o...)
	})
	if err != nil {
		return err
	}
	productCli, ok := productCliAny.(productrpc.Client)
	if !ok {
		return errors.New("bff: product client type assertion failed")
	}

	stockCliAny, err := newClient("demo-stock", func(o []client.Option) (any, error) {
		return stockrpc.NewClient("demo-stock", o...)
	})
	if err != nil {
		return err
	}
	stockCli, ok := stockCliAny.(stockrpc.Client)
	if !ok {
		return errors.New("bff: stock client type assertion failed")
	}

	orderCliAny, err := newClient("demo-order", func(o []client.Option) (any, error) {
		return orderrpc.NewClient("demo-order", o...)
	})
	if err != nil {
		return err
	}
	orderCli, ok := orderCliAny.(orderrpc.Client)
	if !ok {
		return errors.New("bff: order client type assertion failed")
	}

	pointsCliAny, err := newClient("demo-points", func(o []client.Option) (any, error) {
		return pointsrpc.NewClient("demo-points", o...)
	})
	if err != nil {
		return err
	}
	pointsCli, ok := pointsCliAny.(pointsrpc.Client)
	if !ok {
		return errors.New("bff: points client type assertion failed")
	}

	// 不再从 App.Tracer() 手里接 provider 往分层里塞：全生态的 tracer 取法统一为
	// 「向 otel 全局现取」（otelx/layer 就是这么做的）。全局 provider 由 launchx 的
	// telemetry 步骤注册，分层只要跟着全局走就不会与 CLIENT span（rpcx 产出）分家。
	cfg := a.Config()
	logger := a.Logger()
	traceOn := cfg.GetBool("trace.enabled")

	// ---------- 鉴权：本档是 HTTP 边界，全链路唯一验凭据的地方 ----------
	// 四个域服不再各验一次（只在内网侧校验透传下来的身份），见 pkg/auth 包注释。
	//
	// 装不上鉴权**不让服务起来**：一个没有鉴权的边界层， outbound 全通、
	// inbound 全收，出事时没有任何一道闸 —— 那比"起不来"危险得多。
	if err := installAuth(engine, a, cfg, logger, traceOn); err != nil {
		return err
	}

	saga, err := installDtmSaga(a, cfg, logger)
	if err != nil {
		return err
	}
	// TCC 编排器与 Saga 共用同一份 dtm 地址配置（同一个 dtm server、同一批分支
	// 监听口），但**不复用 saga 那个实例**：两链的分支方法、载荷、失败分档都是
	// 各自独立的，共用一个编排器等于让「改 Saga 顺带改了 TCC」。
	tcc, err := installDtmTCC(a, cfg, logger)
	if err != nil {
		return err
	}

	// 两个 handler 共用**同一个** downstream 客户端集：下游 RPC 客户端里带连接
	// 池，为两条编排各建一份等于凭空翻倍连接数，而它们调的是同样的四个域。
	down := downstream.New(downstream.Clients{
		Product: productCli,
		Stock:   stockCli,
		Order:   orderCli,
		Points:  pointsCli,
	}, traceOn)

	mallH := handler.New(down, saga, traceOn)
	presaleH := handler.NewPresale(down, tcc, traceOn)

	engine.Engine.GET("/ping", mallH.Ping)
	engine.Engine.GET("/product/:id", mallH.GetProduct)
	engine.Engine.GET("/stock/:product_id", mallH.GetStock)
	engine.Engine.GET("/order/:id", mallH.GetOrder)
	engine.Engine.GET("/points/:user_id", mallH.GetAccount)
	// 复合编排：预检商品 + 提交 dtm Saga（dtm 推进四分支并负责逆序补偿）——
	// 全链路验收就看这一条。
	engine.Engine.POST("/order", mallH.PlaceOrder)
	// 第二条复合编排：预检商品 + 提交 dtm TCC（bff 只发三个 Try，
	// Confirm / Cancel 由 dtm 在 Submit 之后驱动）。
	engine.Engine.POST("/api/presale/lock", presaleH.Lock)
	return nil
}

// installDtmSaga 构造下单 Saga 的编排器（装配期一次）。
//
// 失败姿态由 `strict.dtm` 裁决（config.yaml 显式写 false）：
//   - 构造失败（地址缺失 / 配错）+ strict.dtm=false → Warn 后装一个
//     dtm.Unavailable 编排器，**进程照常起来**，下单这一条返回 5xx，其余路由
//     （商品/库存/订单/积分查询、/ping）不受影响。理由：交易链路挂掉不该把
//     整个 bff 带走，而「起不来」在现场是比「下单不可用」更难定位的状态。
//   - strict.dtm=true → 装配期即退，把配置问题挡在上线之前。
//
// 注意这里区分了两种 dtm 故障：**装配期**的配置缺口由 strict 裁决，**运行期**
// 的 dtm 不可达一律 5xx + Warn（编排器内部已 Warn），两种情况进程都不退。
func installDtmSaga(a *launchx.App, cfg configx.Config, logger logx.Logger) (*bffdtm.Orchestrator, error) {
	dtmAddr := cfg.GetString("xmiddleware.dtm.grpc_addr")
	orch, err := bffdtm.New(bffdtm.Config{
		DTMAddr:     dtmAddr,
		BranchAddrs: bffdtm.BranchAddrsFrom(cfg),
		Logger:      logger,
	})
	if err == nil {
		logger.Info("bff: dtm saga orchestrator installed",
			logx.String("dtm_addr", dtmAddr),
			logx.String("branches", strings.Join(bffdtm.DescribeBranchAddrs(cfg), " ")),
			logx.String("branch_order", bffdtm.DescribeBranchOrder()))
		return orch, nil
	}
	// 为什么不把地址原样塞进 Unavailable：把「哪个键没配」与「这台机器上默认值
	// 是多少」一起打进 5xx 响应没有意义 —— 调用方要的是结论，运维要的是配置键。
	cause := fmt.Errorf("bff: dtm saga 编排器不可用: %w", err)
	if cfg.IsSet("strict.dtm") && cfg.GetBool("strict.dtm") {
		return nil, cause
	}
	logger.Warn("bff: dtm saga 编排器不可用，下单将 5xx（全局事务不会建立，四域无写入；其余路由正常）",
		logx.Err(cause),
		logx.String("hint", "补齐 xmiddleware.dtm.grpc_addr 与 dtm.branches.<domain>；"+
			"置 strict.dtm: true 可让这类配置缺失在装配期即退"))
	return bffdtm.Unavailable(cause), nil
}

// installDtmTCC 构造预售锁定 TCC 的编排器（装配期一次）。
//
// 与 installDtmSaga 同构、共用同一段配置，但**独立失败**：Saga 构造失败不
// 影响 TCC 可用，反之亦然。合成一个成败会把「预售不可用」表现成「下单也
// 挂了」，排查方向直接被带偏。
//
// 失败姿态同样由 `strict.dtm` 裁决（config.yaml 显式写 false）：dtm 配置缺失
// 时 Warn 后装一个恒失败的编排器，进程照常起来，`POST /api/presale/lock`
// 返回 5xx，其余路由不受影响。
func installDtmTCC(a *launchx.App, cfg configx.Config, logger logx.Logger) (*bffdtm.TCCOrchestrator, error) {
	dtmAddr := cfg.GetString(bffdtm.KeyDTMAddr)
	orch, err := bffdtm.NewTCC(bffdtm.TCCConfig{
		DTMAddr:     dtmAddr,
		BranchAddrs: bffdtm.BranchAddrsFrom(cfg),
		Logger:      logger,
	})
	if err == nil {
		logger.Info("bff: dtm tcc orchestrator installed",
			logx.String("dtm_addr", dtmAddr),
			logx.String("branches", strings.Join(bffdtm.DescribeTCCBranchAddrs(cfg), " ")),
			logx.String("tcc_order", bffdtm.DescribeTCCBranchOrder()))
		return orch, nil
	}
	// 为什么不把地址原样塞进 UnavailableTCC（同 installDtmSaga）：调用方要的是
	// 结论，运维要的是配置键。
	cause := fmt.Errorf("bff: dtm tcc 编排器不可用: %w", err)
	if cfg.IsSet("strict.dtm") && cfg.GetBool("strict.dtm") {
		return nil, cause
	}
	logger.Warn("bff: dtm tcc 编排器不可用，预售锁定将 5xx（全局事务不会建立，四域无写入；其余路由正常）",
		logx.Err(cause),
		logx.String("hint", "补齐 xmiddleware.dtm.grpc_addr 与 dtm.branches.{stock,points,order}；"+
			"置 strict.dtm: true 可让这类配置缺失在装配期即退"))
	return bffdtm.UnavailableTCC(cause), nil
}

// authSkipPrefixes 免鉴权路径前缀。
//
// 只有两类：存活探针与 /ping。它们存在的意义就是"在依赖全挂时仍能回答"，
// 要求凭据会让探针在凭据存储抖动的那一刻报失败 —— 于是编排系统会因为
// "鉴权用的 redis 抖了一下"而重启一批健康的实例，把局部故障放大成全局。
var authSkipPaths = map[string]bool{
	"/ping":          true,
	"/healthz/live":  true,
	"/healthz/ready": true,
}

// installAuth 装配边界鉴权中间件。
//
// 三条路径分别对应三种失败姿态，刻意不合并：
//   - 显式关了（auth.require=false）→ Warn 后放行：这是**运维决定**，留痕即可；
//   - 缓存不可用（拿不到凭据存储）→ **报错不起**：边界层没有凭据就是没有边界，
//     带病起来的后果是所有请求都能穿透，且看起来一切正常；
//   - 未配凭据（demo.auth_token 为空）→ **报错不起**：中间件会挂上但永远 401，
//     那比"起不来"更难查（链路、限流、日志全都正常，只有业务全挂）。
func installAuth(engine *webx.App, a *launchx.App, cfg configx.Config, logger logx.Logger, traceOn bool) error {
	if cfg.IsSet("auth.require") && !cfg.GetBool("auth.require") {
		logger.Warn("bff: auth disabled by config, every request is accepted",
			logx.String("hint", "auth.require=false exposes all four domains to anyone who can reach :8081"))
		return nil
	}

	inst := a.Cache()[cfg.GetString("service.name")]
	if inst == nil {
		return errors.New("bff: cache component is unavailable, auth has nowhere to look up credentials (check xmiddleware.cachex)")
	}
	token := cfg.GetString("demo.auth_token")
	if token == "" {
		return errors.New("bff: demo.auth_token is empty, auth middleware would reject every request (set it or set auth.require=false)")
	}

	store := auth.NewTokenStore(inst)
	// 播种演示令牌：生产里这活属于签发服务（含过期轮换与吊销），demo 在启动时
	// 写一条，让"校验时那次缓存查询"真的有东西可查。
	//
	// 播种期间临时关掉 cachex 的埋点：这里没有请求上下文，留下的 SET span 会
	// 成为一根孤儿单 span trace —— 正是 Jaeger 里最容易被读成"链路断了"的噪声。
	// 关的是组件级开关，播完立刻还原为原值（不是写死 true，避免把"全局关埋点"
	// 的部署悄悄打开）。
	before := traceflag.EnabledFor("cachex")
	traceflag.SetComponent("cachex", false)
	err := store.GrantToken(context.Background(), token, cfg.GetInt64("demo.seed_user_id"), time.Hour)
	traceflag.SetComponent("cachex", before)
	if err != nil {
		return fmt.Errorf("bff: seed demo token failed: %w", err)
	}

	if logger == nil {
		logger = logx.GetGlobal()
	}
	engine.Engine.Use(auth.Middleware(auth.Config{
		Verifier: store,
		Logger:   logger,
		TraceOn:  traceOn,
		TTL:      cfg.GetDuration("auth.ticket_ttl"),
		Skip:     func(path string) bool { return authSkipPaths[strings.TrimSuffix(path, "/")] },
	}))
	logger.Info("bff: auth middleware installed",
		logx.String("scheme", "Bearer"),
		logx.String("header", auth.AuthorizationHeader),
		logx.String("ticket_ttl", cfg.GetDuration("auth.ticket_ttl").String()))
	return nil
}
