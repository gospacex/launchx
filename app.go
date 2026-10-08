package launchx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gospacex/cachex"
	"github.com/gospacex/configx"
	"github.com/gospacex/coordx"
	"github.com/gospacex/kvx"
	"github.com/gospacex/launchx/internal/component"
	"github.com/gospacex/launchx/internal/configsrc"
	"github.com/gospacex/launchx/internal/mode"
	"github.com/gospacex/launchx/internal/pipeline"
	"github.com/gospacex/launchx/internal/telemetry"
	"github.com/gospacex/launchx/internal/wiring"
	"github.com/gospacex/lockx"
	"github.com/gospacex/logx"
	_ "github.com/gospacex/logx/zap" // 注册 logx 的 zap provider（无此空白导入 logx.New 会失败）
	mongodbx "github.com/gospacex/mongodbx"
	"github.com/gospacex/mqx"
	"github.com/gospacex/otelx"
	"github.com/gospacex/searchx"
)

// serverShutdownTimeout 优雅停机排空 in-flight 请求的上限。
const serverShutdownTimeout = 5 * time.Second

// defaultStopTimeout 退出链**每一环**的等待上限。
//
// 为什么必须有：退出链上任何一环卡住，进程就永远收不到「SIGTERM 已优雅退出」，
// 于是 tracing 冲刷、logger 关闭、端口释放全部执行不到 —— 在 K8s 上表现为
// 优雅期耗尽后被 SIGKILL（等价于崩了），而进程自己还以为在正常收尾。
// 现实案例（2026-10-07 实测，monolith E1 用例挂 20s 不退）：kafka-go 的
// Reader 只要 GroupID 非空就会在构造时起 ConsumerGroup 协程，该协程阻塞在
// joinGroup 的网络读上且**不设读 deadline**（connDeadline.value 为零值时
// SetReadDeadline 等于不设）；退出时 Reader.Close → ConsumerGroup.Close →
// wg.Wait()，于是「一个从没消费过的消费者句柄」让整个进程退不出去。
//
// 故这里按「每一环都限时」处理，而不是只给个别驱动打补丁：驱动多到打不完，
// 而退出链是唯一一处能统一兜住的地方。
const defaultStopTimeout = 5 * time.Second

// stopTimeoutKey 退出链超时在 yaml 里的键（缺省走 defaultStopTimeout）。
const stopTimeoutKey = "service.stop_timeout"

// registryOpTimeout 单次注册/注销的上限。
//
// 注册中心不可达时不能让启动或退出无限挂住（退出挂住 = 后面的 tracing 冲刷
// 与 logger 关闭都执行不到，丢最后一批 span）。超时取消**不会影响已注册实例
// 的续期**：coordx 的 TTL 心跳由内部 ticker 维持，不继承这里的 ctx
// （实测 coordx/consul/consul.go:100）。
const registryOpTimeout = 5 * time.Second

// selfAddrKeys 自身注册地址的取值顺序（P3 纪律：地址一律来自配置，不硬编码）。
//
// 顺序按仓库现状实测：service.addr 是四档共用的监听地址键（webx 段未配 addr 时
// 回落到它，srv 档 kitex 的监听地址也取它）；webx.addr 是「只配了 webx 段」的
// 兜底；service.self.addr 排在最前，用于容器内外地址不一致时显式声明
// 「注册地址 ≠ 监听地址」。三次读取都经 configsrc 的 envConfig，${ENV} 生效。
var selfAddrKeys = []string{"service.self.addr", "service.addr", "webx.addr"}

// Mode 服务形态。四档之间只差「服务端 + 服务发现」的装配方式，
// 业务代码完全一致（C4：换形态不改业务）。
//
// 注意：网格（mesh）**不是**一个 mode。网格与微服走完全相同的 Go 代码路径，
// 差异只在部署清单与地址副本，故枚举里没有 mesh 取值。
type Mode string

// 合法形态取值。
const (
	ModeMonolith Mode = "monolith" // 单体：仅 HTTP 服务端
	ModeBFF      Mode = "bff"      // BFF：HTTP 入口 + 下游 RPC 客户端
	ModeSRV      Mode = "srv"      // 领域服务：RPC 服务端
	ModeGateway  Mode = "gateway"  // 网关：HTTP 入口 + 上游服务发现
)

// Modes 返回合法形态取值（顺序固定，用于错误信息与文档）。
func Modes() []string {
	return []string{string(ModeMonolith), string(ModeBFF), string(ModeSRV), string(ModeGateway)}
}

// ParseMode 解析并校验服务形态；空值或非法值返回带合法取值列表的 error。
func ParseMode(s string) (Mode, error) {
	m := Mode(strings.TrimSpace(s))
	for _, ok := range []Mode{ModeMonolith, ModeBFF, ModeSRV, ModeGateway} {
		if m == ok {
			return m, nil
		}
	}
	return "", fmt.Errorf("launchx: invalid mode %q, valid values: %s", s, strings.Join(Modes(), " / "))
}

// App 暴露基础设施只读句柄。业务在 OnSetup 内装配自己的分层，
// launchx 不对目录结构做任何约定。
type App struct {
	configPath string
	mu         sync.Mutex
	setups     []func(*App) error
	stops      []exitHook
	closed     bool

	// 骨架产物（逐步填充）
	events      *pipeline.Events
	envv        *pipeline.Env
	cfg         configx.Config
	mode        Mode
	serviceName string
	logger      logx.Logger
	tracer      otelx.Provider
	// metrics 指标门面（未启用时非 nil 但 Handler 为空，见 telemetry.NewMetrics）；
	// metricsAddr 是 srv 档自建采集口的监听地址（HTTP 档为空：采集口在引擎上）。
	metrics     *telemetry.Metrics
	metricsAddr string
	comp        *component.Env
	// graph 是 component.mount 那一步由 wire 求解产出的注册面快照。
	// 业务取句柄走 App.DB()/Cache()/... ，这里是为排查准备的（见 Graph 注释）。
	graph *wiring.Graph

	// 形态装配产物（stepMode 填充；服务端句柄以 any 承载，
	// launchx 库源码不直接依赖 hertz/kitex）
	holders          *mode.AppHolders
	engine           any
	rpcServer        any
	rpcClientOptions func(service string) (any, error)
	// serverStop 停止服务端的钩子，退出链在 server.stop 之后调用。
	// 只在 Run 真正起服时接上：从未起服（后台型服务）时保持 nil。
	serverStop func() error

	// registry / registered 记录「本进程注册进注册中心的那个实例」，
	// 退出链第 4 步按它注销（未注册态两者皆为零值，注销彻底静默）。
	registry   coordx.Register
	registered *coordx.ServiceInstance
}

// 组件句柄访问器。业务经此处取中间件，不直接构造驱动
//（保持"业务依赖 launchx + hubx 语义层，不引 go-redis / gorm / kafka-go"）。

// DB 返回 *gorm.DB（类型为 any：launchx 库自身不依赖 gorm，调用方按需断言）。
func (a *App) DB() any { return a.compProduct(func(c *component.Env) any { return c.DB }) }

// Cache 返回缓存命名实例表。
func (a *App) Cache() cachex.Instances {
	v, _ := a.compProduct(func(c *component.Env) any { return c.Cache }).(cachex.Instances)
	return v
}

// MQ 返回消息生产者。
func (a *App) MQ() mqx.Producer {
	v, _ := a.compProduct(func(c *component.Env) any { return c.MQ }).(mqx.Producer)
	return v
}

// MQConsumer 返回消息消费者（App.MQ 是它的发布侧配对句柄）。
//
// 未装配 / 构造降级时为 nil，业务必须显式判空 —— 消费不到消息不会立刻被
// 看见，静默跳过的后果是队列无声地一直涨。
//
// 使用姿势：拿到句柄后自行决定何时开始消费（一般是在一个可取消的 ctx 上调
// Consume，并把 cancel 挂到 App.OnStop）。本方法**不**替业务起消费循环：
// 什么时候开始消费、并发几路、失败要不要重投都是业务决策，框架替做会让
// 退出顺序与位点提交时机变成不可控项。
func (a *App) MQConsumer() mqx.Consumer {
	v, _ := a.compProduct(func(c *component.Env) any { return c.MQConsumer }).(mqx.Consumer)
	return v
}

// registryHandle 把「注册」与「发现」两个 coordx 句柄合成一个返回给业务。
//
// 为什么需要这层包装：coordx 把注册与发现拆成两个接口（Register / Discover），
// 而 HTTP 网关拿到的是 App.Registry()，它需要按服务名解析上游实例 —— 注册句柄
// 本身不含 Discovery。早先这里只返回注册句柄，网关的类型断言失败后静默落到
// 配置里的静态兜底地址，于是「网关经注册中心发现上游」这条路**从未真正生效**：
// micro 形态被 `service.upstream.<name>` 的 127.0.0.1 兜底掩盖了，网格形态把兜底
// 留空（上游地址只留一个来源）才暴露成 502。
// 注意：**不能**用内嵌 coordx.Register 来继承方法 —— 内嵌字段名恰好是
// `Register`，与接口方法 `Register` 同名，Go 会判定 `h.Register` 是字段而非
// 方法，方法提升被抑制，类型也就不再实现该接口。故这里逐方法显式转发。
type registryHandle struct {
	reg  coordx.Register
	disc coordx.Discover
}

func (h registryHandle) Register(ctx context.Context, service string, inst *coordx.ServiceInstance) error {
	return h.reg.Register(ctx, service, inst)
}

func (h registryHandle) Deregister(ctx context.Context, service string, inst *coordx.ServiceInstance) error {
	return h.reg.Deregister(ctx, service, inst)
}

func (h registryHandle) HealthCheck() error { return h.reg.HealthCheck() }

func (h registryHandle) Close() error { return h.reg.Close() }

// Discovery 实现业务侧（网关反代）所需的发现能力。
func (h registryHandle) Discovery(ctx context.Context, service string) ([]*coordx.ServiceInstance, error) {
	if h.disc == nil {
		return nil, errors.New("launchx: discovery is unavailable (registry component built without a discover handle)")
	}
	return h.disc.Discovery(ctx, service)
}

// Registry 返回服务注册句柄；注册中心可用时，返回值同时具备发现能力
// （断言 Discovery(ctx, service) 即可，网关反代就是这么用的）。
func (a *App) Registry() coordx.Register {
	v, _ := a.compProduct(func(c *component.Env) any { return c.Registry }).(coordx.Register)
	if v == nil {
		return nil
	}
	disc, _ := a.compProduct(func(c *component.Env) any { return c.Discover }).(coordx.Discover)
	return registryHandle{reg: v, disc: disc}
}

// Discover 返回服务发现句柄（注册中心未装配或发现器构造失败时为 nil）。
func (a *App) Discover() coordx.Discover {
	v, _ := a.compProduct(func(c *component.Env) any { return c.Discover }).(coordx.Discover)
	return v
}

// Search 返回搜索引擎客户端。
func (a *App) Search() searchx.SearchClient {
	v, _ := a.compProduct(func(c *component.Env) any { return c.Search }).(searchx.SearchClient)
	return v
}

// Mongo 返回文档库命名实例表。
func (a *App) Mongo() mongodbx.Instances {
	v, _ := a.compProduct(func(c *component.Env) any { return c.Mongo }).(mongodbx.Instances)
	return v
}

// KV 返回键值存储命名实例表（kvx）。
//
// 与 Cache() 的分工：Cache 面向可失效的缓存语义（TTL 对象缓存、限流计数），
// KV 面向需要直接读写的键值状态（幂等键、跨请求状态位）。两者可能同指一个
// redis 实例，但句柄分开，业务不应拿缓存句柄存必须持久的状态。
func (a *App) KV() kvx.Stores {
	v, _ := a.compProduct(func(c *component.Env) any { return c.KV }).(kvx.Stores)
	return v
}

// Lock 返回分布式锁器（lockx，单实例）。
//
// 未装配（未配 xmiddleware.lockx 段或构造降级）时返回 nil，业务须显式判空并
// 决定姿态：要么 fail-fast，要么在日志里说清"本次未走锁路径"，**不可**静默
// 当作已加锁继续跑（超卖类故障最难排查的形态就是"锁没生效但没人报错"）。
func (a *App) Lock() lockx.Locker {
	v, _ := a.compProduct(func(c *component.Env) any { return c.Lock }).(lockx.Locker)
	return v
}

// compProduct 在组件环境为空（未挂载/已降级）时返回 nil，调用方无需判空保护。
func (a *App) compProduct(fn func(*component.Env) any) any {
	if a.comp == nil {
		return nil
	}
	return fn(a.comp)
}

// Graph 返回 wire 求解产出的注册面快照（component.mount 之后才有值，此前为 nil）。
//
// 业务取句柄照旧走 App.DB()/Cache()/... —— 这个访问器不是给业务用的，是给
// "为什么这个句柄是空的"这类排查用的：每个句柄带 Name() / Constructed() /
// TraceEnabled()，能一次回答「构造了没有」与「开没开入链」，不必去翻日志，
// 也不必去猜 yaml 里哪个段没生效。
func (a *App) Graph() *wiring.Graph { return a.graph }

// Engine 返回 webx 引擎（*webx.App）：monolith/bff/gateway 档非空，srv 档为 nil。
//
// 业务在 Setup 内断言后挂自己的路由（须早于 Run，路由树起服后只读）。
func (a *App) Engine() any { return a.engine }

// RPCServerOptions 返回 srv 档的 kitex 服务端选项（[]server.Option）。
//
// kitex server 本体由业务用这些选项构造（业务代码与形态无关），
// 构造出的句柄经 SetRPCServer 回存，由 Run 启动、Close 停止。
func (a *App) RPCServerOptions() any {
	if a.holders == nil {
		return nil
	}
	return a.holders.Out.RPCServer
}

// RPCServer 返回业务回存的 RPC 服务端句柄（kitex server），未回存为 nil。
func (a *App) RPCServer() any { return a.rpcServer }

// SetRPCServer 回存业务构造的 RPC 服务端句柄（kitex server）。
//
// 停机钩子不在此处接：它只在 Run 真正起服时接上（见 stopHook），
// 这样「回存了但从没起服」的进程退出链里不会有一个多余的停机动作。
func (a *App) SetRPCServer(srv any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rpcServer = srv
}

// RPCClientOptions 按被调服务名产出下游 RPC 客户端选项（[]client.Option）。
//
// 仅 bff/gateway 档可用（发现经 rpcx 的 coordx resolver）；
// 其余档返回带合法形态列表的 error，而不是返回一份没有任何目标的选项。
func (a *App) RPCClientOptions(service string) (any, error) {
	if a.rpcClientOptions == nil {
		return nil, fmt.Errorf("launchx: rpc client is not available in mode %q, valid modes: %s",
			a.mode, strings.Join(Modes(), "|"))
	}
	return a.rpcClientOptions(service)
}

// Events 返回启动/退出步骤事件记录器（顺序断言的唯一依据）。
func (a *App) Events() *pipeline.Events { return a.events }

// Config 返回已解析的配置句柄（业务经它读自己的配置段）。
func (a *App) Config() configx.Config { return a.cfg }

// Mode 返回已装配的服务形态。
func (a *App) Mode() Mode { return a.mode }

// Logger 返回全局日志器（业务也可直接用 logx.GetGlobal()）。
func (a *App) Logger() logx.Logger { return a.logger }

// Tracer 返回 telemetry provider（退出链负责 Shutdown 冲刷）。
func (a *App) Tracer() otelx.Provider { return a.tracer }

// OnSetup 注册装配回调：在 Run 的 setup 步骤按注册顺序执行
// （基础设施就绪后、服务端监听前），业务在此装配自己的分层并挂路由/RPC。
func (a *App) OnSetup(fn func(*App) error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.setups = append(a.setups, fn)
}

// exitHook 一条业务退出回调（不叫 stopHook：那个名字已被「按服务端类型取停机
// 钩子」的函数占用。带名字是为了退出链超时时能报出「谁没退出来」（见
// defaultStopTimeout 的注释）：只打序号的话，一次 5s 的收尾卡顿没人查得下去。
type exitHook struct {
	name string
	fn   func() error
}

// OnStop 注册退出回调，Run 收尾时按注册逆序执行（早于内置组件 Stop）。
func (a *App) OnStop(fn func() error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stops = append(a.stops, exitHook{name: fmt.Sprintf("onstop#%d", len(a.stops)), fn: fn})
}

// OnStopNamed 同 OnStop，但显式给出名字。
//
// 业务自己注册的退出回调（关连接池、flush 缓冲区、等 goroutine 收尾）建议用它：
// 名字会原样出现在退出链超时的 Warn 里。
func (a *App) OnStopNamed(name string, fn func() error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if name == "" {
		name = fmt.Sprintf("onstop#%d", len(a.stops))
	}
	a.stops = append(a.stops, exitHook{name: name, fn: fn})
}

// env 返回骨架执行环境（同一实例贯穿全流程，步骤间经它传递句柄）。
func (a *App) env() *pipeline.Env {
	if a.envv == nil {
		a.envv = &pipeline.Env{Events: a.events, Ctx: context.Background()}
	}
	return a.envv
}

// steps 返回固定启动骨架。顺序是框架契约，业务不能插队。
func (a *App) steps() []pipeline.Step {
	return []pipeline.Step{
		{Name: pipeline.StepConfigLoad, Fn: a.stepLoadConfig},
		{Name: pipeline.StepTelemetryInit, Fn: a.stepTelemetry},
		{Name: pipeline.StepLoggerInit, Fn: a.stepLogger},
		{Name: pipeline.StepModeAssemble, Fn: a.stepMode},
		{Name: pipeline.StepComponentMount, Fn: a.stepComponents},
		{Name: pipeline.StepRegistryRegister, Fn: a.stepRegister},
	}
}

// stepLoadConfig 加载配置：本地 yaml 兜底，`configcenter.type` 非 local 时
// 再叠加远端源（远端 key 级覆盖本地）。
func (a *App) stepLoadConfig(e *pipeline.Env) error {
	local, err := configsrc.Resolve("local", configsrc.Options{Path: a.configPath})
	if err != nil {
		return err
	}
	src := strings.TrimSpace(local.GetString("configcenter.type"))
	if src == "" || src == "local" {
		a.cfg = local
		return nil
	}
	o := configsrc.Options{
		Path:    a.configPath,
		Type:    src,
		DataID:  local.GetString("configcenter." + src + ".data_id"),
		Group:   local.GetString("configcenter." + src + ".group"),
		AddrEnv: local.GetString("configcenter." + src + ".server_addr_env"),
	}
	if o.DataID == "" {
		if name := local.GetString("service.name"); name != "" {
			o.DataID = "hubx." + name
		}
	}
	if o.AddrEnv == "" {
		o.AddrEnv = "HUBX_NACOS_SERVER"
	}
	merged, err := configsrc.Resolve(src, o)
	if err != nil {
		return err
	}
	a.cfg = merged
	return nil
}

// stepTelemetry 装配 otelx 并注册全局 TracerProvider + 传播器。
// MUST 早于 mode 装配与组件挂载（见 internal/telemetry 包注释）。
func (a *App) stepTelemetry(e *pipeline.Env) error {
	name := strings.TrimSpace(a.cfg.GetString("service.name"))
	if name == "" {
		return fmt.Errorf("launchx: service.name is required")
	}
	a.serviceName = name
	p, err := telemetry.Init(name, a.cfg.GetString("trace.endpoint"), a.cfg.GetBool("trace.enabled"))
	if err != nil {
		return err
	}
	a.tracer = p
	e.Tracer = p

	// 指标与 tracer 同一步装配、同一条顺序红线：它设的是 otel 全局
	// MeterProvider，而 HTTP / RPC 的 instrument 都在各自的 SDK 构造时向全局取用。
	m, err := telemetry.NewMetrics(name, a.cfg.GetBool("metrics.enabled"))
	if err != nil {
		return err
	}
	a.metrics = m
	return nil
}

// stepLogger 构造 logx 并设为全局（业务在任意包内可 logx.GetGlobal()）。
func (a *App) stepLogger(e *pipeline.Env) error {
	lc := logx.DefaultConfig()
	if a.cfg.IsSet("logx") {
		// 注意：viper 的 Sub().Unmarshal 不零值化目标结构，
		// 故未在 yaml 中出现的字段会保留 DefaultConfig 的值。
		if err := a.cfg.Unmarshal("logx", &lc); err != nil {
			return fmt.Errorf("launchx: parse logx section: %w", err)
		}
	}
	if strings.TrimSpace(lc.ServiceName) == "" || lc.ServiceName == "logx" {
		lc.ServiceName = a.cfg.GetString("service.name")
	}
	l, err := logx.New(lc)
	if err != nil {
		return fmt.Errorf("launchx: init logger: %w", err)
	}
	a.logger = l
	e.Logger = l
	logx.SetGlobal(l)
	return nil
}

// stepMode 解析服务形态并装配本档的服务端与发现（非法/缺失/mesh 即退）。
//
// 装配只在此处发生一次：产物挂在 App 上，Run 负责启动、Close 负责停止。
// 组件层（含注册中心）挂载在其后，故注册/发现的地址在装配期从配置读取。
func (a *App) stepMode(e *pipeline.Env) error {
	a.holders = &mode.AppHolders{
		Cfg:         a.cfg,
		Logger:      a.logger,
		Tracer:      a.tracer,
		Metrics:     a.metrics,
		ServiceName: a.serviceName,
	}
	if err := mode.Assemble(a.holders); err != nil {
		return err
	}
	out := a.holders.Out
	a.mode = Mode(out.Mode)
	a.engine = out.Webx
	a.rpcClientOptions = out.RPCClientOptions
	a.metricsAddr = out.MetricsAddr
	return nil
}

// webxStop 产出 webx 引擎的停止闭包（接口断言，库源码不 import hertz）。
//
// 实测：webx/webx.go:235 Run() error、:240 Shutdown(ctx) error；
// IsRunning 来自内嵌的 *server.Hertz。
//
// 未启动的引擎直接跳过：hertz 对未运行的服务 Shutdown 会返回
// errStatusNotRunning，而「从没起来过」不是错误，不该冒进退出链的错误聚合。
func webxStop(engine any) func() error {
	w, ok := engine.(interface {
		Shutdown(context.Context) error
		IsRunning() bool
	})
	if !ok {
		return nil
	}
	return func() error {
		if !w.IsRunning() {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), serverShutdownTimeout)
		defer cancel()
		if err := w.Shutdown(ctx); err != nil {
			return fmt.Errorf("webx shutdown: %w", err)
		}
		return nil
	}
}

// rpcStop 产出 kitex 服务端的停止闭包（接口断言，库源码不 import kitex）。
//
// 实测：kitex@v0.16.3/server/server.go:52-58，Server 接口为
// Run() error / Stop() error。
//
// 不另加超时：kitex 的 Stop 自带优雅退出等待（rpcx.BuildServerOptions 的
// WithExitWaitTime，缺省 5s），再套一层只会把同一个上限写两遍。
func rpcStop(srv any) func() error {
	s, ok := srv.(interface{ Stop() error })
	if !ok {
		return nil
	}
	return func() error {
		if err := s.Stop(); err != nil {
			return fmt.Errorf("rpc server stop: %w", err)
		}
		return nil
	}
}

// stopHook 按服务端类型取停机钩子；两者都不匹配返回 nil（由调用方 Warn）。
//
// 断言失败**不 panic**：未知服务端类型只是没有停机钩子，起服照旧，
// 代价是退出时排空不了 in-flight 请求——必须说出来，但不能因此炸掉进程。
func stopHook(srv any) func() error {
	if stop := webxStop(srv); stop != nil {
		return stop
	}
	return rpcStop(srv)
}

// stepComponents 挂载内置组件：经 wire 求解出的依赖顺序执行各 SDK 的构造。
//
// 组件的启用是 presence-based（配了段才构造），失败姿态由 strict.<name> 裁决：
// 降级不打断启动，strict=true 则返回 error（由 Bootstrap 走完整反序退出链）。
//
// 为什么不再用一张手写的挂载顺序表：原先的 `componentOrder` 是八个字符串，
// 顺序全靠人记谁依赖谁，加一个组件就要回头想它该插在哪。现在交给
// `wiring.InitializeGraph` —— 顺序由 provider 的入参依赖推导，
// 改一个 provider 的入参，其余自动跟着走。
//
// 注意 wire **只**接管顺序：presence / trace.on / strict 三条裁决仍在 provider
// 内读 Env 完成（不进图，理由见 internal/wiring 包注释）。另外形态装配不在这里
// 重做 —— mode.assemble 已在它前面跑过，图里那一步会判"已装配"并跳过。
func (a *App) stepComponents(e *pipeline.Env) error {
	cenv := &component.Env{
		Cfg:         a.cfg,
		ServiceName: a.serviceName,
		Logger:      a.logger,
		TraceOn:     a.boolMap("trace.on"),
		Strict:      a.boolMap("strict"),
	}
	graph, err := wiring.InitializeGraph(cenv, a.holders)
	if err != nil {
		return err
	}
	a.comp = cenv
	a.graph = &graph
	return nil
}

// stepRegister 按形态把自身实例注册进注册中心，并记下实例供退出链注销。
//
// 只有 bff / srv 注册（monolith 没有下游、gateway 是流量入口，注册它们只会让
// 注册中心多一批无人调用的条目）；未注册态必须**彻底静默**——验收陷阱第 11 条
// 就是把「单体退出时打出 deregister」当作 mode 装配串档的判据。
//
// 注册中心未装配（presence-based：没配 xmiddleware.coordx 段）同样不注册：
// 无注册中心是合法部署形态，不是故障。
func (a *App) stepRegister(e *pipeline.Env) error {
	if a.holders == nil || !a.holders.Out.Registered {
		return nil
	}
	reg := a.Registry()
	if reg == nil {
		return nil
	}
	host, port, err := selfAddr(a.cfg)
	if err != nil {
		return a.registerFail(err)
	}
	inst := &coordx.ServiceInstance{
		// ID 与 rpcx 的实例标识同构（service-host-port）：注册与注销必须落在
		// 同一个 ID 上，否则注销作用在另一个实例，注册中心会残留脏条目。
		ID:        fmt.Sprintf("%s-%s-%d", a.serviceName, host, port),
		ServiceID: fmt.Sprintf("%s-%s-%d", a.serviceName, host, port),
		Addr:      host,
		Port:      port,
		Metadata:  map[string]string{"mode": string(a.mode)},
		Healthy:   true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), registryOpTimeout)
	defer cancel()
	if err := reg.Register(ctx, a.serviceName, inst); err != nil {
		return a.registerFail(err)
	}
	a.registry = reg
	a.registered = inst
	if a.logger != nil {
		a.logger.Info("launchx: service registered",
			logx.String("service", a.serviceName), logx.String("instance", inst.ID))
	}
	return nil
}

// registerFail 按 strict.registry 裁决注册失败：
// true 即退（起了但不被发现比启动失败更难排查）；缺省降级——打一条带后果的
// Warn 并**不记实例**，退出链因此也不会去注销一个没注册成功的实例。
func (a *App) registerFail(err error) error {
	if a.strictOf("registry") {
		return fmt.Errorf("launchx: register %s: %w", a.serviceName, err)
	}
	if a.logger != nil {
		a.logger.Warn("launchx: register failed, running unregistered",
			logx.String("service", a.serviceName),
			logx.Err(err),
			logx.String("consequence", component.ConsequenceRegistry),
			logx.String("hint", "set strict.registry: true to fail fast"),
		)
	}
	return nil
}

// strictOf 读取 strict.<name> 裁决（组件环境未挂载时按缺省 false = 降级）。
func (a *App) strictOf(name string) bool {
	if a.comp == nil {
		return false
	}
	return a.comp.StrictOf(name)
}

// selfAddr 从配置解析自身注册地址，拆成 host + port。
//
// 配置缺失或不是 host:port 形式都返回 error：注册地址错了比不注册更糟——
// 上游按注册的地址连过来，连的是个不存在的实例。
func selfAddr(cfg configx.Config) (string, int, error) {
	raw := ""
	for _, key := range selfAddrKeys {
		if v := strings.TrimSpace(cfg.GetString(key)); v != "" {
			raw = v
			break
		}
	}
	if raw == "" {
		return "", 0, fmt.Errorf("no self address configured (tried %s)", strings.Join(selfAddrKeys, " / "))
	}
	host, portStr, err := net.SplitHostPort(raw)
	if err != nil {
		return "", 0, fmt.Errorf("self address %q is not host:port: %w", raw, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("self address %q has a non-numeric port %q: %w", raw, portStr, err)
	}
	return host, port, nil
}

// boolMap 读取 `key.<name>: true|false` 形态的开关表（缺省空表）。
func (a *App) boolMap(key string) map[string]bool {
	out := map[string]bool{}
	if !a.cfg.IsSet(key) {
		return out
	}
	if err := a.cfg.Unmarshal(key, &out); err != nil {
		// 开关表读不动不应阻断启动（它只影响埋点与失败姿态），交由组件按缺省值处理。
		if a.logger != nil {
			a.logger.Warn("launchx: config section unreadable, falling back to defaults",
				logx.String("section", key), logx.Err(err))
		}
		return map[string]bool{}
	}
	return out
}

// Run 执行业务 Setup 回调，然后启动本档的服务端（阻塞）。
//
//	monolith/bff/gateway → webx 引擎；srv → 业务构造的 RPC server；
//	两者都没有 → 返回 nil（后台型服务，例如纯消费者）。
//
// 服务端句柄以 any 承载并断言 Run() error：hertz/kitex 只经 webx/rpcx 引入，
// launchx 库源码不直接引入它们（验收 3.14 的依赖面红线）。
func (a *App) Run() error {
	if a.events != nil {
		a.events.Record(pipeline.StepSetup)
	}
	// Setup 必须早于监听：路由与处理器注册只能在起服前完成。
	for _, fn := range a.setupSnapshot() {
		if err := fn(a); err != nil {
			// Setup 失败与运行期错误走同一条退出链（spec "Setup 失败走完整退出"）：
			// 先收干净已初始化的资源（tracer 冲刷 / logger 关闭）再返回。
			// 退出链自己的错误不吞，附在 setup 错误之后（首错仍是 setup error）。
			setupErr := fmt.Errorf("launchx: setup: %w", err)
			if exitErr := a.Close(); exitErr != nil {
				return errors.Join(setupErr, exitErr)
			}
			return setupErr
		}
	}
	a.mu.Lock()
	rpc, engine := a.rpcServer, a.engine
	a.mu.Unlock()
	// srv 档没有 HTTP 面 ⇒ 采集口得自己起一个监听。放在起服之前：
	// 端口被占这类问题必须在"看起来起来了"之前就暴露。HTTP 档不在此列
	//（采集口挂在 webx 引擎上，多起一个端口只会多一处要维护的暴露面）。
	if err := a.startMetricsListener(); err != nil {
		return err
	}
	// srv 档优先：业务构造的 RPC server 是该档唯一入口。
	for _, srv := range []any{rpc, engine} {
		if srv == nil {
			continue
		}
		r, ok := srv.(interface{ Run() error })
		if !ok {
			return fmt.Errorf("launchx: server %T does not implement Run() error", srv)
		}
		// 停机钩子只在**确定要起服**时接上：从未起服的进程退出链里
		// 不该出现对一个没监听过的句柄的停机动作。
		stop := stopHook(srv)
		a.mu.Lock()
		a.serverStop = stop
		a.mu.Unlock()
		if stop == nil && a.logger != nil {
			a.logger.Warn("launchx: server has no stop hook, in-flight requests will not be drained",
				logx.String("server", fmt.Sprintf("%T", srv)))
		}
		return r.Run()
	}
	// 一个服务端都没有（后台型服务，或 srv 档业务忘了回存 RPC server）：
	// 返回 nil 是刻意的，但必须说出来——否则"Run 立刻返回"会被当成"起来了"。
	if a.logger != nil {
		a.logger.Warn("launchx: no webx engine and no rpc server, Run returns immediately",
			logx.String("mode", string(a.mode)))
	}
	return nil
}

// startMetricsListener 为「没有 HTTP 面」的档起一个只服务采集口的监听。
//
// 三种情况直接返回 nil（都不是错误）：
//   - 指标未启用（handler 为空）；
//   - 本档有 HTTP 引擎（采集口挂在引擎的 MetricsPrefix 上，见 webx.registerMetrics）；
//   - 没配 metrics.addr（srv 档且开了指标却没配地址，已在 mode.Assemble 阶段即退，
//     走到这里说明本来就没开指标）。
//
// 监听挂在业务退出链上（OnStop）而不是 server.stop：它先于服务端启动，
// 反序退出时也就晚于服务端关闭 —— 停机过程本身的指标不会被提前掐断。
func (a *App) startMetricsListener() error {
	a.mu.Lock()
	m, engine, addr := a.metrics, a.engine, a.metricsAddr
	a.mu.Unlock()
	if m == nil || engine != nil || addr == "" {
		return nil
	}
	if err := m.Serve(addr); err != nil {
		return err
	}
	a.OnStopNamed("metrics", func() error {
		ctx, cancel := context.WithTimeout(context.Background(), serverShutdownTimeout)
		defer cancel()
		return m.Shutdown(ctx)
	})
	return nil
}

// setupSnapshot 返回 Setup 回调快照（启动期只读，避免持锁执行业务代码）。
func (a *App) setupSnapshot() []func(*App) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]func(*App) error, len(a.setups))
	copy(out, a.setups)
	return out
}

// Close 幂等执行反序退出链。
//
// 显式序列，不用 defer：defer 会在手动 flush 之后才执行，span 已经丢了。
//
//	业务 OnStop（逆序）→ server.stop → component.stop（逆序）
//	→ registry.deregister → tracing.shutdown（15s 冲刷）
//	→ metrics.shutdown → logger.close
//
// 每一步都记录事件，供测试断言顺序（不靠日志文本匹配）。
// 中间步骤失败不中断后续回收（否则会漏掉 flush），错误聚合返回。
func (a *App) Close() error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	stops := make([]exitHook, len(a.stops))
	copy(stops, a.stops)
	a.mu.Unlock()

	var errs []error
	rec := func(name string) { a.events.Record(name) }
	limit := a.stopTimeout()

	// 1. 业务回调先退（此时基础设施仍可用）
	rec("setup.stop")
	for i := len(stops) - 1; i >= 0; i-- {
		if err := a.runStop(stops[i].name, stops[i].fn, limit); err != nil {
			errs = append(errs, err)
		}
	}

	// 2. 服务端停止：钩子由 Run 起服时按服务端类型接上（webx Shutdown /
	//    kitex Stop）。从未起服则为 nil（no-op），不往错误聚合里塞噪声。
	rec(pipeline.StepServerStop)
	if a.serverStop != nil {
		if err := a.runStop("server.stop", a.serverStop, limit); err != nil {
			errs = append(errs, fmt.Errorf("server stop: %w", err))
		}
	}

	// 3. 组件停止（反序：先起的后停）
	rec(pipeline.StepComponentStop)
	if a.comp != nil {
		cs := a.comp.NamedStops()
		for i := len(cs) - 1; i >= 0; i-- {
			if err := a.runStop(cs[i].Name, cs[i].Fn, limit); err != nil {
				errs = append(errs, err)
			}
		}
	}

	// 4. 注册中心注销：只注销本进程**注册成功**的那个实例（未注册态彻底静默，
	//    见 stepRegister）。必须早于 tracing.shutdown：注销本身会标 span，
	//    冲刷之后再注销等于把最后一批 span 丢在缓冲里。
	rec(pipeline.StepRegistryDeregister)
	if inst := a.registered; inst != nil && a.registry != nil {
		ctx, cancel := context.WithTimeout(context.Background(), registryOpTimeout)
		err := a.registry.Deregister(ctx, a.serviceName, inst)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("registry deregister: %w", err))
		}
		a.registered = nil
	}

	// 5. 调用链冲刷：必须在所有组件停止之后、logger 关闭之前。
	//    15s 上限：批量导出器缓冲未冲刷会静默丢最后一批 span。
	rec(pipeline.StepTracingShutdown)
	if a.tracer != nil {
		done := make(chan error, 1)
		go func() { done <- a.tracer.Close() }()
		select {
		case err := <-done:
			if err != nil {
				errs = append(errs, fmt.Errorf("tracing shutdown: %w", err))
			}
		case <-time.After(15 * time.Second):
			errs = append(errs, errors.New("tracing shutdown: 15s timeout"))
		}
	}

	// 6. 指标收口：与 tracing 同理 —— 先把 MeterProvider 的聚合刷出去再关 logger。
	//    srv 档的采集口监听在前面 OnStop("metrics") 里已关过一次，这里重复调用是
	//    幂等的（http.Server.Shutdown 与 MeterProvider.Shutdown 都做了幂等保护）。
	rec(pipeline.StepMetricsShutdown)
	if a.metrics != nil {
		ctx, cancel := context.WithTimeout(context.Background(), serverShutdownTimeout)
		if err := a.metrics.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("metrics shutdown: %w", err))
		}
		cancel()
	}

	// 7. 日志最后关（前面所有步骤都可能要打日志）
	rec(pipeline.StepLoggerClose)
	if a.logger != nil {
		_ = a.logger.Close()
	}

	return errors.Join(errs...)
}

// stopTimeout 退出链每一环的等待上限：yaml 的 service.stop_timeout（>0 才生效）。
func (a *App) stopTimeout() time.Duration {
	if a.cfg != nil {
		if d := a.cfg.GetDuration(stopTimeoutKey); d > 0 {
			return d
		}
	}
	return defaultStopTimeout
}

// runStop 有界执行一条退出回调。
//
// 超时**不**进错误聚合，只 Warn + 记事件，理由：退出链的契约是「在限定时间内
// 收尾，然后必须让进程退出」。把超时算成 error 会翻退出码（main 里 Close 出错
// 即 os.Exit(1)），于是「一个驱动的 Close 慢了」被升级成「进程退出失败」——
// 在 Job / CI 里直接变 crashloop，而真正的故障形态（退不出去 → 优雅期耗尽被
// SIGKILL）已经被这里挡掉了。超时是已知且已处理的降级，所以按「放弃等待 +
// 记账」处理：Warn 里带名字与后果，事件流里留 stop.timeout.<name>。
//
// 反过来，回调自己返回的 error 照旧进聚合：那是它明确报出来的失败，
// 不是我们替它猜的。
//
// 被放弃的回调会留下一个 goroutine（它自己卡住了，没法取消），进程退出时由
// 运行时一并收走 —— 这正是「宁可漏一次显式 Close，也不要漏一次退出」的取舍。
func (a *App) runStop(name string, fn func() error, limit time.Duration) error {
	if fn == nil {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(limit):
		a.events.Record("stop.timeout." + name)
		if a.logger != nil {
			a.logger.Warn("退出链一环超时，放弃等待继续收尾",
				logx.String("stop", name),
				logx.String("timeout", limit.String()),
				logx.String("consequence", "该句柄未显式释放，进程退出时由运行时回收；请查这一环的 Close 是否阻塞"))
		}
		return nil
	}
}
