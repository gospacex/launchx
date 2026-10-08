// Package mode 实现 launchx 的四档形态装配：monolith / bff / srv / gateway。
//
// 四档之间只差「服务端 + 服务发现」的装配方式，业务代码完全一致
// ——换形态不改业务，这是四档存在的全部理由。
//
// 网格（mesh）**不是**第五档：它与微服走完全相同的 Go 代码路径，
// 差异只在部署清单与地址副本，因此这里显式拒绝 mesh 并引导到 srv/gateway/bff。
//
// 依赖方向：本包只依赖 configx / logx / otelx / webx / rpcx 与调用方传入的
// AppHolders，**不** import launchx 根包，也**不** import internal/component
// （组件层挂载晚于本包装配，反向依赖会成环）。
package mode

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gospacex/configx"
	"github.com/gospacex/logx"
	"github.com/gospacex/otelx"
)

// Mode 服务形态。
type Mode string

// 合法形态取值。
const (
	ModeMonolith Mode = "monolith" // 单体：仅 HTTP 服务端
	ModeBFF      Mode = "bff"      // BFF：HTTP 入口 + 下游 RPC 客户端
	ModeSRV      Mode = "srv"      // 领域服务：RPC 服务端
	ModeGateway  Mode = "gateway"  // 网关：HTTP 入口 + 上游服务发现
)

// validValues 错误信息里的合法取值串（四档全列，缺一即视为不合格）。
const validValues = "monolith|bff|srv|gateway"

// 三类形态错误。共同点：文案必须带 validValues，否则调用方只能靠猜补档。
var (
	errMissingMode = fmt.Errorf("launchx: service.mode 缺失，合法取值：%s", validValues)
	errIllegalMode = fmt.Errorf("launchx: service.mode 非法，合法取值：%s", validValues)
	// errMeshRejected 网格不是独立档：与微服共用同一份代码，差异只在部署清单。
	errMeshRejected = fmt.Errorf("launchx: service.mode=mesh 不是独立档，合法取值：%s；"+
		"mesh 非独立档，与微服同代码，请使用 srv/gateway/bff 并以部署清单体现网格", validValues)
)

// Modes 返回合法形态取值（顺序固定，用于错误信息与文档）。
func Modes() []string {
	return []string{string(ModeMonolith), string(ModeBFF), string(ModeSRV), string(ModeGateway)}
}

// ParseMode 解析并校验服务形态；空值、非法值、mesh 均返回带合法取值列表的 error。
func ParseMode(s string) (Mode, error) {
	m := Mode(strings.TrimSpace(s))
	if m == "" {
		return "", errMissingMode
	}
	if strings.EqualFold(string(m), "mesh") {
		return "", errMeshRejected
	}
	for _, ok := range []Mode{ModeMonolith, ModeBFF, ModeSRV, ModeGateway} {
		if m == ok {
			return m, nil
		}
	}
	return "", fmt.Errorf("%w（当前值 %q）", errIllegalMode, s)
}

// MetricsEndpoint 指标采集口的最小契约（由 App 注入）。
//
// 只声明一个方法：mode 不需要知道指标后端是 Prometheus 还是别的、也不需要
// 拿到 MeterProvider（instrument 一律向 otel 全局取用）。nil 表示未启用指标。
type MetricsEndpoint interface {
	// Handler 返回采集口；未启用时返回 nil。
	Handler() http.Handler
}

// AppHolders 装配的输入句柄与输出产物，是 launchx App 与本包之间唯一的数据面。
//
// 装配发生在 component.mount 之前，故这里只传「此刻已就绪」的句柄（配置/日志/
// 调用链），组件产物由调用方按需要读取，不在本包内回取。
type AppHolders struct {
	// Cfg 已解析的配置句柄（service.mode / service.addr / webx 段等）。
	Cfg configx.Config
	// Logger 全局日志器（可为空，webx 侧回落到 logx.GetGlobal()）。
	Logger logx.Logger
	// Tracer telemetry provider（可为空；非空时注入 webx 与 rpcx 的埋点）。
	Tracer otelx.Provider
	// Metrics 指标采集口（可为空 = 未启用指标）。
	Metrics MetricsEndpoint
	// ServiceName 服务名，用于注册名与 tracer 名。
	ServiceName string

	// Out 装配产物，Assemble 成功后可读。
	Out Out
}

// Out 装配产物。
//
// 服务端与客户端选项一律以 any 承载：launchx 库源码不直接引入 hertz/kitex
// （二者只经 webx/rpcx 引入，验收 3.14），调用方按需断言。
type Out struct {
	// Mode 生效的形态（四档之一）。
	Mode string
	// Webx HTTP 引擎（*webx.App）；monolith/bff/gateway 档非空，srv 档为 nil。
	Webx any
	// RPCServer kitex 服务端选项（[]server.Option）；仅 srv 档非空。
	// kitex server 本体由业务 Setup 用这些选项构造，句柄回存 App.RPCServer。
	RPCServer any
	// RPCClientOptions 下游 RPC 客户端选项工厂；bff/gateway 档非空。
	// 工厂按被调服务名产出 []client.Option，发现经 rpcx 的 coordx resolver。
	RPCClientOptions func(service string) (any, error)
	// RegistryAddr 注册中心地址（空 = 未配置注册中心，注册/发现降级为不接）。
	RegistryAddr string
	// Registered 本档是否需要向注册中心注册自身实例（bff/srv 为 true）。
	Registered bool
	// RPCGovernance 出向 RPC 治理参数（重试 / 熔断 / 负载均衡）。
	// **装配期解析并校验**，零值表示全关（见 rpc.go）。
	RPCGovernance RPCGovernance
	// MetricsEnabled 是否挂指标：RPC 侧中间件 + HTTP 档的 /metrics 端点。
	// otel MeterProvider 本身由 telemetry 步骤装配（与形态无关），本字段只表达
	// "这一档要不要把指标接出去"。
	MetricsEnabled bool
	// MetricsAddr 指标监听地址。**只对 srv 档有意义**：srv 没有 HTTP 面，
	// 需要独立监听才能被采集；HTTP 档的 /metrics 由 webx 引擎自带。
	MetricsAddr string
}

// Assemble 按 service.mode 分派装配：解析形态 → 装服务端 → 产出客户端/发现工厂。
//
// 形态非法/缺失/mesh 一律返回带合法取值列表的 error（地基失败不降级）。
func Assemble(h *AppHolders) error {
	if h == nil || h.Cfg == nil {
		return fmt.Errorf("launchx: mode assemble requires a config handle")
	}
	m, err := ParseMode(h.Cfg.GetString("service.mode"))
	if err != nil {
		return err
	}
	// 治理参数在分派之前解析：非法取值即退（不降级），见 readRPCGovernance。
	gov, err := readRPCGovernance(h.Cfg)
	if err != nil {
		return err
	}
	metricsOn := h.Cfg.GetBool(metricsEnabledKey)
	h.Out = Out{
		Mode:           string(m),
		RegistryAddr:   registryAddr(h),
		RPCGovernance:  gov,
		MetricsEnabled: metricsOn,
		MetricsAddr:    strings.TrimSpace(h.Cfg.GetString(metricsAddrKey)),
	}
	if h.Logger != nil {
		// 把装配期**真实的裁决结果**打出来：这三个开关"配了不生效"是一类
		// 静默病（段键写错、开关串位都长一个样），日志与 yaml 对照一眼可见。
		h.Logger.Info("launchx: " + rpcGovernanceSummary(gov, metricsOn))
	}
	// srv 档没有 HTTP 面：采集口只能自己起监听。不配地址的话指标会「只记不露」
	// —— 面板上永远是空的，而进程一切正常。这类配置错误在本仓一律即退
	//（与 load_balancer 越界同一口径），不降级、不静默。
	if metricsOn && m == ModeSRV && h.Out.MetricsAddr == "" {
		return fmt.Errorf("launchx: service.mode=srv 没有 HTTP 面，metrics.enabled=true 时必须配 metrics.addr" +
			"（否则指标会被采集但不暴露，且没有任何报错）")
	}
	switch m {
	case ModeMonolith:
		return assembleMonolith(h)
	case ModeBFF:
		return assembleBFF(h)
	case ModeSRV:
		return assembleSRV(h)
	case ModeGateway:
		return assembleGateway(h)
	}
	return errIllegalMode // 不可达：ParseMode 已收敛到四档
}

// registryAddr 读取注册中心地址。
//
// presence-based：配了段才接注册中心，未配即不接（与组件层同一裁决口径）。
// 刻意不给默认值——给默认会让「没配注册中心」变成「静默连 127.0.0.1:8500」，
// 症状是启动时多一次无意义的连接、失败还被降级吞掉。
func registryAddr(h *AppHolders) string {
	for _, key := range []string{"xmiddleware.coordx.addr", "xmiddleware.consul.addr"} {
		if v := strings.TrimSpace(h.Cfg.GetString(key)); v != "" {
			return v
		}
	}
	return ""
}
