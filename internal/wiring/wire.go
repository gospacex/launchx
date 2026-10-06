//go:build wireinject

package wiring

import (
	// goforj/wire 是 google/wire 的维护分支，API 兼容。
	// 本文件是**唯一**引用它的地方，且带 wireinject 标签，默认构建不编译它。
	"github.com/goforj/wire"

	"github.com/gospacex/launchx/internal/component"
	"github.com/gospacex/launchx/internal/mode"
)

// InitializeGraph 注入器。
//
// 这个文件里**只放注入器**，不放任何 ProviderSet 变量 —— 这是踩过坑之后的结论：
// 本 fork 的生成器会把 wireinject 文件的全部声明原样拷进 wire_gen.go，
// 包括 `var X = wire.NewSet(...)` 这类变量；一旦拷进去，wire 就成了运行时依赖，
// W1「build-time only」当场失效（实测该 fork v1.2.0 确实如此）。
//
// 因此 provider 直接列在 wire.Build 里。至于「地基 / 可选组件 / 形态产物」的
// 分组语义，改由 wiring.go 的 Catalog 承载 —— 那里按类别登记了每个 SDK 的归属
// 与 presence 键，排查时比在这里数 provider 更有用。
func InitializeGraph(env *component.Env, holders *mode.AppHolders) (Graph, error) {
	wire.Build(
		NewGraph,
		// 地基：恒构造，失败即退
		ProvideConfig, ProvideLogger, ProvideTelemetry,
		// 可选组件：presence-based，在 provider 内运行时裁决
		ProvideDB, ProvideCache, ProvideMQ, ProvideCoord,
		ProvideSearch, ProvideMongo, ProvideKV, ProvideLock,
		// 登记在册、默认不构造
		ProvideObject, ProvideVector,
		// 形态装配产物
		ProvideModeReady, ProvideRPC, ProvideWeb,
	)
	return Graph{}, nil
}
