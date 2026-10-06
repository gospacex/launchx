# dependency-injection delta

## ADDED Requirements

### Requirement: DI 选型 goforj/wire（编译期代码生成，非运行时容器）

hubx 各 `*x` SDK 在 launchx 中的依赖注册与构造顺序求解 SHALL 使用
`github.com/goforj/wire`（`google/wire` 的维护分支，API 兼容）完成；
MUST NOT 引入基于反射的运行时 DI 容器（dig / facebookgo/inject 一类）。

具体约束：

- `wire.Build(...)` 只出现在带 `//go:build wireinject` 的文件中；
- 生成的 `wire_gen.go` 不 import wire，**运行时零依赖**；
- 要求 Go ≥ 1.25：launchx `go.mod` 为 `go 1.27.1`、`hubx` 为 `go 1.26.2`，均满足；
- 既有 `github.com/google/wire` 代码无需改动即可迁移（官方兼容声明），
  迁移手段为 `go mod edit -replace=github.com/google/wire=github.com/goforj/wire@<tag>`。

#### Scenario: 验收案例 W1 — wire 是 build-time 依赖，运行时不可见

- **WHEN** 不带 `wireinject` 标签对 launchx 执行 `go build ./...`
- **THEN** 构建成功，且依赖闭包（`go list -deps ./...`）中不含 `github.com/goforj/wire` 与 `github.com/google/wire`

### Requirement: 16 个 hubx SDK 全部纳入统一注册面

系统 SHALL 为 `hubx/` 下 16 个 SDK 各提供至少一个 wire provider，并聚合为
可组合的 ProviderSet：`cachex`、`configx`、`coordx`、`dbx`、`hubx`、`kvx`、
`lockx`、`logx`、`mongodbx`、`mqx`、`objectx`、`otelx`、`rpcx`、`searchx`、
`vectorx`、`webx`。

分类决定 provider 的归属包与启用语义，MUST NOT 混用：

| 类别 | SDK | 装配位 | 启用语义 |
|---|---|---|---|
| 地基（恒构造） | `configx`、`logx`、`otelx` | `internal/pipeline` 步骤 1–3 | 无条件；失败即退，不受 `strict` 影响 |
| 可选组件 | `dbx`、`cachex`、`mqx`、`coordx`、`searchx`、`mongodbx`、`kvx`、`lockx`、`objectx`、`vectorx` | `internal/component` | presence-based（配了 yaml 段才构造） |
| 形态装配产物 | `rpcx`、`webx` | `internal/mode` 四档分支 | 由 `service.mode` 决定，非 presence-based |
| 客户端注册表 | `hubx`（`package registry`） | 顶层 | 见「goforj/wire 替换」Requirement |

`objectx` 与 `vectorx` 在既往裁决中被列为「有 SDK 但本轮不接（YAGNI）」；
本次 SHALL 以 provider 形式登记在册但默认不构造（presence-based 未命中即零值），
使「注册面完整」与「不为无调用方组件付出构造成本」两者同时成立。

#### Scenario: 验收案例 W2 — 16 个 SDK 均可被图求解

- **WHEN** 执行 `wire gen ./...` 且注入器声明需要全部 16 个 SDK 的句柄
- **THEN** 生成成功（无 "no provider for" 报错），`wire_gen.go` 中含对每个 SDK 构造函数的显式调用

### Requirement: wire 图不承载运行时裁决

下列三条 launchx 既有语义 MUST 继续由运行时裁决（provider 内部或注入器外围），
MUST NOT 被搬进编译期 wire 图 —— wire 生成的是固定顺序、无分支的初始化代码，
表达不了运行时分支：

1. presence-based 启用（配了 yaml 段才构造）；
2. `trace.on.<name>` 只控入链、不控构造（D1 红线）；
3. `strict.<name>` 决定失败是降级还是返回 error（D2 红线）。

provider 允许返回零值 + `nil` error 表示「未构造 / 已降级」，wire 对此不作干预。

#### Scenario: 验收案例 W3 — 接入 wire 后 D1 语义不变

- **WHEN** `trace.on.db: false` 且 `xmiddleware.sqlx` 段已配置
- **THEN** `App.DB` 仍非 nil、业务读写正常，仅不产生 `db.query` span

#### Scenario: 验收案例 W4（反向断言）— 禁止把 presence 编进图

- **WHEN** 试图用多注入器变体表达 10 个可选组件的有无组合
- **THEN** 需要 2^10 个注入器，不可行；据此判定该方案 MUST NOT 采用，
  并以单测断言「组件层仍按 `Env` 运行时裁决」锁死该边界

### Requirement: 具名句柄类型消解 any 与 wire 的 type identity 冲突

系统 SHALL 为每个弱类型句柄引入具名句柄类型（如 `type DBHandle struct{ ... }`，
内部持有原值并暴露取值方法），使 wire 可求解；对外暴露形态 MUST 保持向后兼容
（`App.DB()` 的调用方断言方式不变，或与变更说明一并给出迁移路径）。

冲突来源：wire 按 **type identity** 匹配输入与输出，且要求 provider 返回**具体类型**
（接口须经 `wire.Bind` 显式绑定）。launchx `component.Env` 的 `DB` 字段现为 `any`
（刻意：库不直引 gorm，保持「业务经 launchx 取句柄、不直引驱动」的依赖面），
此类字段无法直接进入 wire 图。

#### Scenario: 验收案例 W5 — 弱类型句柄可入图且不引入直引

- **WHEN** 组件层 db provider 经具名句柄类型进图
- **THEN** wire 求解成功；且 `launchx/go.mod` 仍不直接 require `gorm.io/gorm`
  （维持 3.14 依赖面核对结论）

### Requirement: 退出语义由 launchx 显式退出链独占

launchx 既有的反序退出编排 MUST 保持为退出语义的唯一权威：
`Env.AddStop` + 事件记录器是 A3 断言的唯一落点，退出序列 MUST 仍可被单测逐点断言，
MUST NOT 交由 wire 的 cleanup 机制承载。

背景：wire provider 可返回 `func()` cleanup，且其调用顺序保证先于自身依赖的 cleanup
（LIFO，与反序退出链同向）。但该机制 SHALL 仅用于「构造中途失败时回滚本次已建资源」
这一窄场景。

#### Scenario: 验收案例 W6 — A3 退出顺序断言不回归

- **WHEN** 接入 wire 后启动并在 Setup 失败路径退出
- **THEN** 事件记录器仍记录完整反序序列（server → 组件 Stop → deregister →
  tracing.Shutdown → logger.Close），A3 既有断言全绿

### Requirement: 生成产物入库且可复现

`wire_gen.go` MUST 提交入库，且 MUST 可被 `wire gen ./...` 复现。
`wire` CLI 的版本 MUST 固定（写入 Makefile 或 `docs/` 的构建说明），避免不同版本
产出不同格式导致 diff 噪声。`wire_gen.go` MUST NOT 被手工编辑。

#### Scenario: 验收案例 W7 — 生成物可复现

- **WHEN** 在干净工作区按固定版本 CLI 执行 `wire gen ./...`
- **THEN** `git diff --exit-code` 为空（生成物与入库版本逐字节一致）

### Requirement: 用 goforj/wire 替换已停维护的 google/wire 并接线 hubx 侧半成品

系统 SHALL 以 `go mod edit -replace` 将 `github.com/google/wire` 切到
`github.com/goforj/wire`，并 SHALL 为 `hubx` 模块的 injector 补上 `wire.Build(...)`
使其成为合法 injector 并生成 `wire_gen.go`；MUST NOT 执行 `go mod tidy`
（既有纪律：本仓手工维护 go.mod 与 replace 块）。

`hubx` 模块（`package registry`）当前 require `github.com/google/wire v0.7.0`
（原项目已停止维护），其 `wire.go` 存在两处缺陷：

1. `InitializeApp` 未调用 `wire.Build(...)` —— 它不是合法 injector，Wire 不会为它生成任何代码；
2. 全仓无 `wire_gen.go`（`find hubx -name wire_gen.go` 零命中）—— 从未生成过。

#### Scenario: 验收案例 W8 — hubx 侧 injector 真正接线

- **WHEN** 在 `hubx/` 执行 `wire gen ./...`
- **THEN** 产出 `wire_gen.go`，且其中含对 `Registry` 与 `configx.ViperLoader` 的
  显式构造调用（证明不再是"画了一半"的占位）

### Requirement: 三方依赖白名单新增 goforj/wire

`github.com/goforj/wire` SHALL 显式加入依赖白名单，并 MUST 标注为 build-time only：
它仅被 `//go:build wireinject` 文件与 `wire` CLI 需要，运行时产物不依赖它，
MUST NOT 成为 launchx 的运行时传递依赖。

#### Scenario: 白名单条目可核对

- **WHEN** 核对依赖白名单（与 3.14 依赖面核对同一次执行）
- **THEN** 白名单中含 `github.com/goforj/wire` 且标注为 build-time only；
  同时 `go list -deps ./...`（不带 `wireinject`）不含该模块
