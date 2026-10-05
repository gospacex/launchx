package launchx

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gospacex/cachex"
	"github.com/gospacex/configx"
	"github.com/gospacex/coordx"
	"github.com/gospacex/launchx/internal/component"
	"github.com/gospacex/launchx/internal/configsrc"
	"github.com/gospacex/launchx/internal/pipeline"
	"github.com/gospacex/launchx/internal/telemetry"
	"github.com/gospacex/logx"
	_ "github.com/gospacex/logx/zap" // 注册 logx 的 zap provider（无此空白导入 logx.New 会失败）
	mongodbx "github.com/gospacex/mongodbx"
	"github.com/gospacex/mqx"
	"github.com/gospacex/otelx"
	"github.com/gospacex/searchx"
)

// componentOrder 组件挂载顺序。db/cache/mq 先（业务最先用到），
// registry 次之（服务端起来前要能注册），search/mongodb 最后。
var componentOrder = []string{"db", "cache", "mq", "registry", "search", "mongodb"}

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
	stops      []func() error
	closed     bool

	// 骨架产物（逐步填充）
	events      *pipeline.Events
	envv        *pipeline.Env
	cfg         configx.Config
	mode        Mode
	serviceName string
	logger      logx.Logger
	tracer      otelx.Provider
	comp        *component.Env
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

// Registry 返回服务注册句柄。
func (a *App) Registry() coordx.Register {
	v, _ := a.compProduct(func(c *component.Env) any { return c.Registry }).(coordx.Register)
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

// compProduct 在组件环境为空（未挂载/已降级）时返回 nil，调用方无需判空保护。
func (a *App) compProduct(fn func(*component.Env) any) any {
	if a.comp == nil {
		return nil
	}
	return fn(a.comp)
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

// OnSetup 注册装配回调，Bootstrap 后、Run 前调用。
func (a *App) OnSetup(fn func(*App) error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.setups = append(a.setups, fn)
}

// OnStop 注册退出回调，Run 收尾时按注册逆序执行（早于内置组件 Stop）。
func (a *App) OnStop(fn func() error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stops = append(a.stops, fn)
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

// stepMode 校验并解析服务形态（非法即退，错误信息含全部合法取值）。
// 实际的服务端装配在任务 9 补入此处。
func (a *App) stepMode(e *pipeline.Env) error {
	m, err := ParseMode(a.cfg.GetString("service.mode"))
	if err != nil {
		return err
	}
	a.mode = m
	return nil
}

// stepComponents 按固定顺序挂载六类内置组件。
//
// 组件的启用是 presence-based（配了段才构造），失败姿态由 strict.<name> 裁决：
// 降级不打断启动，strict=true 则返回 error（由 Bootstrap 走完整反序退出链）。
func (a *App) stepComponents(e *pipeline.Env) error {
	cenv := &component.Env{
		Cfg:         a.cfg,
		ServiceName: a.serviceName,
		Logger:      a.logger,
		TraceOn:     a.boolMap("trace.on"),
		Strict:      a.boolMap("strict"),
	}
	if err := component.Mount(cenv, componentOrder); err != nil {
		return err
	}
	a.comp = cenv
	return nil
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

// Run 启动服务并托管退出（服务端装配在 mode 层，任务 9 接入）。
func (a *App) Run() error { return nil }

// Close 幂等执行反序退出链。
//
// 显式序列，不用 defer：defer 会在手动 flush 之后才执行，span 已经丢了。
//
//	业务 OnStop（逆序）→ server.stop → component.stop（逆序）
//	→ registry.deregister → tracing.shutdown（15s 冲刷）→ logger.close
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
	stops := make([]func() error, len(a.stops))
	copy(stops, a.stops)
	a.mu.Unlock()

	var errs []error
	rec := func(name string) { a.events.Record(name) }

	// 1. 业务回调先退（此时基础设施仍可用）
	rec("setup.stop")
	for i := len(stops) - 1; i >= 0; i-- {
		if err := stops[i](); err != nil {
			errs = append(errs, err)
		}
	}

	// 2. 服务端停止（任务 9 接入 webx/rpcx 后填充）
	rec(pipeline.StepServerStop)

	// 3. 组件停止（反序：先起的后停）
	rec(pipeline.StepComponentStop)
	if a.comp != nil {
		cs := a.comp.Stops()
		for i := len(cs) - 1; i >= 0; i-- {
			if err := cs[i](); err != nil {
				errs = append(errs, err)
			}
		}
	}

	// 4. 注册中心注销（任务 9 记录注册实例后在此 Deregister）
	rec(pipeline.StepRegistryDeregister)

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

	// 6. 日志最后关（前面所有步骤都可能要打日志）
	rec(pipeline.StepLoggerClose)
	if a.logger != nil {
		_ = a.logger.Close()
	}

	return errors.Join(errs...)
}
