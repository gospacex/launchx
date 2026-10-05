# bootstrap-pipeline delta

## ADDED Requirements

### Requirement: 单入口启动 API

系统 SHALL 提供 `launchx.Bootstrap(path string) (*App, error)` 作为唯一引导入口，`App` 暴露 `Config`、`Logger`、`Tracer`、`DB`、`Cache`、`MQ`、`Registry`、`Server` 只读句柄，并通过 `OnSetup(func(*App) error)` 注册项目装配回调、`Run() error` 启动与托管退出，使项目 main.go 可收敛为一行启动形态。

#### Scenario: 验收案例 A1 — 一行启动

- **WHEN** 项目提供 `config.yaml` 与 `OnSetup` 回调并调用 `Bootstrap(...).Run()`
- **THEN** 服务按 yaml `mode` 启动，`main.go` 不超过 50 行，且不直接操作 telemetry/配置合并/退出编排细节

#### Scenario: 地基失败即退

- **WHEN** 配置加载失败或 `configcenter` 已声明但远端不可达或 telemetry/logx 初始化失败
- **THEN** `Bootstrap` 返回带上下文的 error，进程可干净退出且无泄漏资源

### Requirement: 固定启动骨架顺序

引导骨架 SHALL 按固定顺序执行：配置加载 → telemetry 初始化（全局 TracerProvider + TraceContext/Baggage 传播器）→ logx 初始化并 SetGlobal → 按 mode 装配服务端与发现 → 挂载可选组件 → 回调项目 Setup；telemetry MUST 先于任何会取全局 tracer 的组件，且该顺序不可被项目代码打乱。

#### Scenario: 验收案例 A2 — 启动顺序红线

- **WHEN** 骨架启动完成（事件记录器记录各步骤调用序）
- **THEN** telemetry.Init 的调用序号早于 db/cache/mq 组件构建，且顺序断言单测通过

### Requirement: 反序退出编排

`App.Run()` SHALL 在收到 SIGINT/SIGTERM 或服务端退出时按逆序执行：停止服务端 → 停止组件（outbox/cache/db 等）→ 服务发现注销 → tracing.Shutdown（flush 超时 ≥10s，取 15s）→ 关闭 logger；会标 span 的资源 MUST 全部先于 flush 关闭，退出编排 MUST 以显式序列执行而不依赖可能被 `os.Exit` 跳过的 defer。

#### Scenario: 验收案例 A3 — SIGTERM 完整退出

- **WHEN** 运行中服务收到 SIGTERM
- **THEN** 服务端先停、consul 注销完成、随后 flush，jaeger 中该服务 span 无丢失，且 `trace.enabled=false` 时同样可正常退出

#### Scenario: Setup 失败走完整退出

- **WHEN** 项目 `Setup` 回调返回 error
- **THEN** `Run` 先执行完整反序退出编排（已初始化资源按红线全部关闭）再返回该 error

### Requirement: Setup 显式回调契约

系统 SHALL 通过 `OnSetup` 显式注册项目装配回调（或等价 Setup 接口），在基础设施就绪后调用；回调内项目可装配任意分层结构并向 `app.Server` 挂载路由/RPC，launchx MUST NOT 对业务分层目录或结构做强约定。

#### Scenario: 分层自由

- **WHEN** 不同 demo（monolith/micro）在 Setup 内采用不同分层结构
- **THEN** 均可正常启动运行，launchx 不因目录结构缺失而报错
