# mode-assembly delta

## ADDED Requirements

### Requirement: 四档 mode 装配分支

系统 SHALL 按 yaml `service.mode` 的四档取值装配服务端与发现能力：`monolith` → webx 单体路由且不注册发现；`bff` → webx HTTP 入口 + consul 注册 + 下游 srv 的 rpcx 客户端工厂；`srv` → rpcx RPC 服务端 + consul 注册；`gateway` → webx 转发路由 + consul 发现上游。其中 HTTP 服务端 MUST 经 `webx`、RPC 服务端与客户端 MUST 经 `hubx/rpcx` 装配。

### Requirement: RPC 经 hubx/rpcx 装配

`srv` 档的 RPC 服务端与 `bff`/`gateway` 档的下游 RPC 客户端 SHALL 经 `github.com/gospacex/rpcx` 装配，该 SDK 封装 kitex 的 `server`/`client` Option（`BuildServerOptions` / `BuildClientOptions`）与 `kitex-contrib/obs-opentelemetry` 续链套件；其服务发现 MUST 走 `coordx`，MUST NOT 引入 `regdisx` 或其他注册中心实现；该套件 MUST 只从 `otel.GetTracerProvider()` 取用当前 provider，MUST NOT 自行装配 provider。

launchx 库源码（含 `internal/*`）MUST NOT 出现 `github.com/cloudwego/*` 的 import 语句 —— kitex 只能作为 `rpcx` 的传递依赖以 `// indirect` 形式出现在 go.mod 中，不得被直接引用或手装。`examples/` 侧仅 `examples/micro/srv` 与 `bff` 在构造 kitex server/client 处 import kitex（`kitex_gen` 生成代码所需，不可避免），其余模块与全部 demo MUST NOT 直接 import `cloudwego/*`、`redis/go-redis`、`segmentio/kafka-go`、`hashicorp/consul/api`、`gorm.io/gorm`。

#### Scenario: RPC 依赖面收敛

- **WHEN** 检查 launchx 库源码与 `examples/*` 各模块的 import 与 go.mod
- **THEN** launchx 源码（含 internal）零 `cloudwego/*` import，go.mod 中 kitex 仅 `// indirect`；`examples/` 中仅 `micro/srv` 与 `micro/bff` 的 kitex 构造处出现 kitex import，且无 hertz / go-redis / kafka-go / consul-api / gorm 的直接引用

#### Scenario: 验收案例 C1 — monolith 装配

- **WHEN** yaml `service.mode: monolith` 启动
- **THEN** 仅启动 webx HTTP 服务，不进行 consul 注册，Setup 可直接挂路由

#### Scenario: 验收案例 C2 — srv 装配

- **WHEN** yaml `service.mode: srv` 启动
- **THEN** 启动 rpcx（`hubx/rpcx`，kitex 封装）服务端并注册 consul，`app.Registry` 可用，Setup 可挂载 RPC 实现

#### Scenario: 验收案例 C3 — bff 与 gateway 装配

- **WHEN** yaml `service.mode: bff` / `gateway` 启动
- **THEN** bff 暴露 webx HTTP 入口并可经 `app.Registry` 构造下游 srv 客户端；gateway 通过 consul 发现上游并完成转发

### Requirement: mesh 不引入独立装配档

`mode` MUST NOT 定义 `mesh` 取值；网格形态 SHALL 复用与 `micro`（bff/srv/gateway）完全相同的装配代码路径，差异仅存在于部署清单与地址配置。

#### Scenario: 验收案例 C4 — 网格部署业务零改动

- **WHEN** 微服 demo 各服务经 k8s + Istio sidecar 部署运行
- **THEN** 三个 demo 服务的 Go 二进制与微服 demo 构建产物一致（业务代码零改动），仅配置副本地址段不同

### Requirement: 未知 mode 拒绝启动

`service.mode` 取值不在四档枚举内或缺失时，`Bootstrap` SHALL 返回包含合法取值列表的 error。

#### Scenario: 非法 mode 报错

- **WHEN** yaml `service.mode: mesh`（或其他非枚举值）启动
- **THEN** 返回 error 并列出 `monolith|bff|srv|gateway` 合法取值
