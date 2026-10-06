// Package wiring 是 launchx 的 SDK 注册面：15 个 hubx `*x` SDK 在此登记，
// 由 goforj/wire 在编译期求解依赖顺序，由 provider 在运行时裁决有无。
//
// 分工边界（改这个文件前先看懂，否则很容易把事做错）：
//
//   - wire 只接管「依赖求解与构造顺序」。它生成的是固定顺序、无分支的初始化
//     代码，表达不了运行时分支。
//   - presence-based 启用、`trace.on.<name>` 只控入链不控构造、`strict.<name>`
//     决定失败姿态 —— 这三条仍由 provider 内部读 Env 裁决。**尤其 presence
//     绝不能搬进图**：10 个可选组件若要靠图表达有无，需要 2^10 个注入器，
//     那是组合爆炸，不是设计。
//
// 为什么句柄要包一层命名类型：wire 按 type identity 匹配输入输出，且要求
// provider 返回具体类型。而 component.Env.DB 是刻意的 any（launchx 库不直引
// gorm，保持「业务经 launchx 取句柄、不直引驱动」的依赖面），这类字段无法直接
// 入图。具名句柄类型把弱类型包成具体类型即可求解；对外 App.DB() 仍返回 any，
// 调用方断言方式不变。
package wiring

// base 是所有具名句柄的公共部分。各 SDK 句柄用嵌入的方式继承这四个方法，
// 同时各自保有一个**互不相同的类型** —— 这正是 wire 求解所需的 type identity。
type base struct {
	// name 是 SDK 名（如 "cachex"），用于降级日志与错误文案，也便于排查时
	// 看清是哪个 SDK 没构造出来。
	name string
	// raw 底层句柄原值。未构造时为 nil。
	raw any
	// trace 该 SDK 是否入链（来自 trace.on.<name>）。它只影响要不要记 span，
	// 绝不影响 raw 是否为 nil —— 这是 D1 红线。
	trace bool
}

func (b base) Name() string       { return b.name }
func (b base) Raw() any           { return b.raw }
func (b base) Constructed() bool  { return b.raw != nil }
func (b base) TraceEnabled() bool { return b.trace }

// ===== 地基（恒构造，失败即退，不受 strict 影响）=====

// ConfigHandle configx 配置句柄。
type ConfigHandle struct{ base }

// LoggerHandle logx 日志器句柄。
type LoggerHandle struct{ base }

// TelemetryHandle otelx 调用链句柄。
type TelemetryHandle struct{ base }

// ===== 可选组件（presence-based：配了 yaml 段才构造）=====

// DBHandle dbx 句柄。Env.DB 是 any，这里包一层让 wire 可求解。
type DBHandle struct{ base }

// CacheHandle cachex 句柄。
type CacheHandle struct{ base }

// MQHandle mqx 句柄。
type MQHandle struct{ base }

// CoordHandle coordx 句柄。coordx 把「注册」与「发现」拆成两个接口，
// HTTP 网关与 RPC 客户端都要用发现侧，故两个都留。
type CoordHandle struct {
	base
	Registry any // coordx.Register
	Discover any // coordx.Discover
}

// SearchHandle searchx 句柄。
type SearchHandle struct{ base }

// MongoHandle mongodbx 句柄。
type MongoHandle struct{ base }

// KVHandle kvx 句柄。
type KVHandle struct{ base }

// LockHandle lockx 句柄。
type LockHandle struct{ base }

// ObjectHandle objectx 句柄。**登记在册但默认不构造**（见 Catalog 注释）。
type ObjectHandle struct{ base }

// VectorHandle vectorx 句柄。**登记在册但默认不构造**（见 Catalog 注释）。
type VectorHandle struct{ base }

// ===== 形态装配产物（由 service.mode 决定，非 presence-based）=====

// RPCHandle rpcx 句柄：kitex 服务端/客户端选项的装配产出。
type RPCHandle struct{ base }

// WebHandle webx 句柄：HTTP 引擎（*webx.App）。
type WebHandle struct{ base }

// Category SDK 在注册表中的类别，决定由谁负责构造。
type Category string

const (
	// CategoryFoundation 地基，恒构造，失败即退且不受 strict 影响。
	CategoryFoundation Category = "foundation"
	// CategoryOptional 可选组件，presence-based。
	CategoryOptional Category = "optional"
	// CategoryModeArtifact 形态装配产物，由 service.mode 决定。
	CategoryModeArtifact Category = "mode-artifact"
)

// Entry 注册表中的一条。它回答的不是「怎么构造」，而是「这个 SDK 在仓里的
// 身份与归属」——构造逻辑各自在 provider / builder 里，避免这里变成第二份真相。
type Entry struct {
	// SDK hubx 模块名，如 "cachex"。
	SDK string
	// Category 类别，决定装配位。
	Category Category
	// PresenceKey yaml 配置里标识「有没有配这个 SDK」的段/键。
	// 地基与形态产物为空（它们不看 presence）。
	PresenceKey string
	// Builder component 层的 builder 名（Lookup 用的那个）；地基与形态产物为空。
	Builder string
	// HasProvider 是否已有 wire provider。false 表示**登记在册但未接线**。
	HasProvider bool
	// Note 为什么是这样，尤其是「为什么没接线」——留空会让人以为是遗漏。
	Note string
}

// Catalog 15 个 SDK 的注册面。这是本次交付的核心登记表。
//
// 顺序有意按「地基 → 可选组件 → 形态产物」排列，与 wire 的求解顺序一致。
var Catalog = []Entry{
	// 地基
	{SDK: "configx", Category: CategoryFoundation, HasProvider: true, Note: "pipeline 步骤 1；失败即退"},
	{SDK: "logx", Category: CategoryFoundation, HasProvider: true, Note: "pipeline 步骤 2；失败即退"},
	{SDK: "otelx", Category: CategoryFoundation, HasProvider: true, Note: "pipeline 步骤 3；失败即退"},

	// 可选组件：8 个由 component builder 承接
	{SDK: "dbx", Category: CategoryOptional, PresenceKey: "xmiddleware.sqlx", Builder: "db", HasProvider: true},
	{SDK: "cachex", Category: CategoryOptional, PresenceKey: "xmiddleware.cachex", Builder: "cache", HasProvider: true},
	{SDK: "mqx", Category: CategoryOptional, PresenceKey: "xmiddleware.mqx", Builder: "mq", HasProvider: true},
	{SDK: "coordx", Category: CategoryOptional, PresenceKey: "xmiddleware.coordx", Builder: "registry", HasProvider: true},
	{SDK: "searchx", Category: CategoryOptional, PresenceKey: "xmiddleware.searchx", Builder: "search", HasProvider: true},
	{SDK: "mongodbx", Category: CategoryOptional, PresenceKey: "xmiddleware.mongodbx", Builder: "mongodb", HasProvider: true},
	{SDK: "kvx", Category: CategoryOptional, PresenceKey: "xmiddleware.kvx", Builder: "kv", HasProvider: true},
	{SDK: "lockx", Category: CategoryOptional, PresenceKey: "xmiddleware.lockx", Builder: "lock", HasProvider: true},

	// 可选组件：登记在册但默认不构造
	{
		SDK: "objectx", Category: CategoryOptional, PresenceKey: "objectx", HasProvider: true,
		Note: "有 SDK 但默认不构造：把它接进 go.mod 会拖入阿里云/华为云/七牛/minio/腾讯 COS 全套依赖，与刚建立的依赖面收敛纪律冲突；provider 在 presence 未命中时返回零值，接进来只需补一个 builder",
	},
	{
		SDK: "vectorx", Category: CategoryOptional, PresenceKey: "vectorx", HasProvider: true,
		Note: "同 objectx：依赖面代价过高（elasticsearch v9 等），登记在册、默认零值",
	},

	// 形态装配产物
	{SDK: "rpcx", Category: CategoryModeArtifact, HasProvider: true, Note: "由 service.mode 决定；服务端选项仅 srv 档非空"},
	{SDK: "webx", Category: CategoryModeArtifact, HasProvider: true, Note: "由 service.mode 决定；monolith/bff/gateway 档非空，srv 档为 nil"},
}

// CatalogBySDK 按 SDK 名索引注册表，便于排查时快速查归属。
func CatalogBySDK() map[string]Entry {
	m := make(map[string]Entry, len(Catalog))
	for _, e := range Catalog {
		m[e.SDK] = e
	}
	return m
}
