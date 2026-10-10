# mesh-app-layering delta

## ADDED Requirements

### Requirement: apps 的分层目录树

`examples/mesh/apps` SHALL 按下列目录树组织，且**分层维度只表达一次**
（`internal/<档>/**` 表达分层，`internal/srv/<域>/**` 表达分域）：

```
apps/
├── go.mod  go.sum  Makefile  Dockerfile  .dockerignore  .gitignore  README.md
├── cmd/{bff,srv-order,srv-points,srv-product,srv-stock}/main.go
├── conf/{bff,srv-order,srv-points,srv-product,srv-stock}.yaml
├── internal/
│   ├── bff/{app,handler,downstream}/
│   └── srv/<域>/{app,handler,service,repository,model,rpc,middleware,route}/
└── pkg/{codes,events,routing}/ + pkg/kitex_gen/{order,points,product,stock}/
```

`internal/srv/<域>/` 八个目录的职责（四域同名同责）：

| 目录 | 职责 | MUST NOT |
|---|---|---|
| `app/` | 装配：取句柄 → 自检 → 自下而上接链 → `SetRPCServer` | 出现业务判断 |
| `rpc/` | **入向**传输适配：实现 `pkg/kitex_gen/<域>` 的接口全部方法、开 `rpc.<域>.<Op>` span、走 `route` 准入闸门、委派 `handler` | 建**出向**客户端（apps 的 srv 不跨域调用，见 `mesh-entry-chain`） |
| `route/` | 准入闸门：身份（Auth）+ 限流（`middleware`），在 handler 之前 | 出现业务判断 |
| `middleware/` | 应用侧兜底：RPC 跳的限流/重试（该跳 Envoy 只见连接） | 出现业务判断 |
| `handler/` | 入参出参转换与错误码映射 | 出现业务判断 |
| `service/` | ★ 唯一允许出现业务判断的层 | — |
| `repository/` | 只经注入的 `*gorm.DB` 持久化 | 自己 `gorm.Open`、直引驱动 |
| `model/` | 纯数据结构 | 依赖任何中间件句柄 |

四域 MUST 同构：上述八个目录的**名字与职责**四域一致，
差异只允许出现在表名、路由名、错误码与业务字段上。

#### Scenario: 树结构与设计文档一致

- **WHEN** 对照 `proposal.md` §一 的树与本目录实际结构
- **THEN** 逐目录一致；不存在未在树中声明的目录（如 `internal/tracex/`）

#### Scenario: 四域同构可机械核对

- **WHEN** 对比 `internal/srv/order/` 与 `internal/srv/points/`
- **THEN** 两边的子目录集合完全相同（各八个）；同名文件（`app/setup.go`、`service/*.go`）的
  结构（函数签名与分层调用顺序）一致，差异已逐条登记在差异表中

### Requirement: service 层是唯一允许出现业务判断的层

各域 MUST 维持 `rpc → route(Auth) → middleware(限流) → handler → service → repository → model`
的自下而上装配顺序，且：

- `handler` MUST 只做入参出参转换与错误码映射，MUST NOT 出现业务判断；
- `service` MUST 是唯一允许出现业务判断的层；
- `repository` MUST 只访问经 `App.DB()` 取得的句柄，MUST NOT 直引驱动；
- `model` MUST 是纯数据结构，MUST NOT 依赖任何中间件句柄。

链上的 `rpc` / `handler` / `service` / `repository` 四层 MUST **各自**开一个分层 span，
命名 `<layer>.<域>.<Op>`（如 `rpc.order.CreateOrder`、`handler.order.CreateOrder`、
`service.order.CreateOrder`、`repository.order.Create`），与 `examples/micro` 的 srv
逐层对应。

这条是入口链条验收的**前置**：`mesh-entry-chain` 的判据要求每条路由的 trace 内出现
目标域的 `service.<域>.<Op>` span。launchx / rpcx 只产出传输层 span（`client.<svc>.<method>`）
与组件 span（`db.query` / `cache.*` / `messaging.*`），**业务分层 span 由应用自己产出** ——
（`rpcx/middleware.go` 的包注释即写明「服务端 span 由各服务自己的 service 层产出」）。
故四个域若不写这四层 span，判据矩阵会在每一格 FAIL。

埋点 MUST 用 `github.com/gospacex/otelx/layer`（`layer.Begin(ctx, layer, entity, op, traceOn, quiet)`），
并 MUST 由 `trace.enabled`（总闸）门控 —— `traceOn=false` 时 `Begin` 返回空闭包，
调用方一律 `defer` 而不必写 `if`。MUST NOT 自建 `internal/tracex`，MUST NOT 手写 `traceparent`。

#### Scenario: 分层越界可被判据发现

- **WHEN** 在某域 `handler` 里写入业务分支（如「金额超过阈值则跳过」）
- **THEN** 该改动与 spec 冲突：判据为「handler 中不存在除参数校验与错误映射之外的
  条件分支」

#### Scenario: 四层 span 逐层可见

- **WHEN** 对任一域发起一次 RPC（经 bff 路由或直接调用），在 Jaeger 中取该 trace
- **THEN** 同一 trace 内按父子顺序出现 `rpc.<域>.<Op>` → `handler.<域>.<Op>` →
  `service.<域>.<Op>` → `repository.<域>.<Op>`（+ 组件 span `db.query` / `cache.*`），
  四层 span 一个都不缺；`trace.enabled=false` 时四层全不出现（不得只少一半）

#### Scenario: 分层 span 不是可选项

- **WHEN** 从某域的 `service` 层删掉 `layer.Begin` 调用
- **THEN** 该域的 `mesh-entry-chain` 路由判据 FAIL（trace 内无 `service.<域>.*`），
  即使 HTTP 返回 200 —— 证明这四层 span 是验收判据的一部分，而非「锦上添花」

### Requirement: apps 不建的三层，以及必须说明的理由

apps MUST NOT 存在下列 micro 里有、网格下不需要的层，`apps/README.md` MUST 逐条给出理由：

| 不建的层 | 理由 |
|---|---|
| `internal/gateway/**`（含 proxy / routing） | 入口转发整块交 Istio `Gateway` + `VirtualService` |
| `internal/tracex/**` | 追踪由 sidecar + launchx telemetry 统一承担 |
| 服务注册装配（coordx 段与注册调用） | 改走 K8s Service + Istio EDS，应用侧零注册 |

反向地，apps MUST 保留 `internal/srv/*/middleware`（RPC 跳的限流/重试兜底）
与 `pkg/routing`（HTTP 边界中间件链骨架）：`bff→srv` 走 Kitex 私有协议，
Envoy 只按 `tcp_proxy` 处理，该跳的治理交不出去。

#### Scenario: 减法不是无差别删除

- **WHEN** 检查 apps 的目录集合
- **THEN** 不含 gateway / tracex / 注册装配；但含 `internal/srv/*/middleware/` 与
  `pkg/routing/`，且 `middleware/` 的存在理由在 README 中被写明（RPC 跳无法下沉）

### Requirement: apps 与 micro 零 import 耦合

apps MUST NOT `import` `github.com/gospacex/launchx/examples/micro/**`（含其
`internal/**`），`examples/micro` MUST NOT 反向引用 apps。

历史教训：曾把 `examples/micro` 的旧快照拷进 `examples/mesh/micro/**`，那份快照
反向 import `examples/micro/**/internal` —— 违反 Go internal 规则，直接让
`go build ./...` 挂掉。故本次 MUST NOT 拷贝快照，MUST 从零编写。

#### Scenario: 双向都不耦合

- **WHEN** 对 apps 与 micro 互相 grep import 路径
- **THEN** 双向零命中；apps 的 `pkg/kitex_gen/*` 由 IDL 重新生成或从 micro 的
  `kitex_gen` **只读搬运生成产物**（不改其内容），不搬运任何 `internal/**`

### Requirement: 结构性守卫继续生效

仓库根 `examples_layout_test.go` 的守卫 SHALL 继续钉住两条结构约束：

1. `examples/mesh` 下的 `.go` 文件 MUST 只出现在 `apps/**`（`deploy/**` 是清单目录，
   禁实现）；
2. `examples/micro` MUST NOT 引用 `examples/mesh`。

#### Scenario: 往清单目录扔 main.go 会被拦下

- **WHEN** 在 `examples/mesh/deploy/**` 下新增任意 `.go` 文件
- **THEN** `go test . -run 'TestMeshGo'` 失败并列出违规文件路径

#### Scenario: apps 是唯一放行区

- **WHEN** 在 `examples/mesh/apps/**` 下新增 `.go` 文件
- **THEN** 同一守卫通过（apps 是独立 module，仍受 walk 覆盖，放行是精确的）
