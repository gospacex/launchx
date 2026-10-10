# mesh-app-handles delta

## ADDED Requirements

### Requirement: 中间件一律经 App 句柄取用，不直引驱动

apps 的全部中间件访问 SHALL 经 `launchx.App` 的句柄访问器取得，MUST NOT 直接
`import` 驱动或自行建连。可用句柄与其返回类型：

| 访问器 | 返回 | 说明 |
|---|---|---|
| `App.DB()` | `any`（断言为 `*gorm.DB`） | 关系库 |
| `App.Cache()` | `cachex.Instances` | 按 `service.name` 取缓存实例 |
| `App.KV()` | `kvx.Stores` | 原子计数 / 幂等键 |
| `App.Lock()` | `lockx.Locker` | 分布式锁 |
| `App.MQ()` | `mqx.Producer` | 消息生产 |
| `App.MQConsumer()` | `mqx.Consumer` | 消息消费（未装配为 nil） |
| `App.Search()` | `searchx.SearchClient` | 搜索引擎 |
| `App.Mongo()` | `mongodbx.Instances` | 文档库 |
| `App.Engine()` | `any`（断言为 `*webx.App`） | HTTP 引擎（bff 档非空） |
| `App.RPCServerOptions()` | `any`（断言为 `[]server.Option`） | srv 档 RPC 服务端选项 |
| `App.RPCClientOptions(svc)` | `any`（断言为 `[]client.Option`） | 下游 RPC 客户端选项；**只在 bff 档非空**（srv 档返回 error） |
| `App.Config()` / `Logger()` / `Tracer()` / `Graph()` | — | 配置 / 日志 / 追踪 / 注册面快照 |

apps 源码 MUST NOT **自行建连**：MUST NOT 出现 `gorm.Open(`、`gorm.io/driver/*`、
`redis/go-redis`、`segmentio/kafka-go`、`hashicorp/consul/api`。

`gorm.io/gorm` 这个包名本身是**允许**的，它有三处正当用法，判据是「有没有自己建连」
而不是「有没有 import 到这个包」：

1. `App.DB()` 的类型断言 `db, ok := a.DB().(*gorm.DB)`；
2. `repository/**` 的句柄字段类型（`db *gorm.DB`）—— 句柄是注入的，不是自建的；
3. 装配期自检探回调（`pkg/handles` 用 gorm 的 callback 注册表判断句柄是否带 dbx 埋点）。

单测可自建内存库（`gorm.io/driver/sqlite` + `gorm.Open`）：依赖面判据对 `_test.go` 豁免。

#### Scenario: 依赖面收敛

- **WHEN** 对 `examples/mesh/apps` 全量 grep import 与建连调用
- **THEN** 不存在 `go-redis` / `kafka-go` / `consul/api` 的直接引用，也不存在
  `gorm.Open(` 与 `gorm.io/driver/*`（`_test.go` 除外）；`gorm.io/gorm` 只出现在
  类型断言 / 句柄字段类型 / 自检探回调三处；`cloudwego/kitex` 只出现在
  `server.Option` / `client.Option` 断言（不在 `internal/srv/**`）与
  `kitex_gen`（生成代码，不可避免）处

### Requirement: 每个进程的句柄取用矩阵

apps 的五个进程 SHALL 按下表取用句柄，多取（取了不用）与少取（业务需要却绕开句柄
自建）都 MUST NOT 出现：

| 进程 | 必用句柄 | 用途 |
|---|---|---|
| `bff` | `Engine()` | 挂 HTTP 路由与 webx 健康端点 |
| `bff` | `RPCClientOptions(<域>)` | 建四域 kitex 客户端（resolver=dns → K8s Service 名） |
| `bff` | `Cache()[<name>]` | 边界凭据存储：`token → user_id` 反查 |
| `bff` | `Config()` / `Logger()` / `Tracer()` | 配置 / 日志 / 追踪 |
| 全部 `srv-*` | `DB()` | 各域自己的表读写（具体表见各域行） |
| 全部 `srv-*` | `KV()[<name>]` | **RPC 跳限流计数**（`route` 的 `middleware` 用原子 `Incr` 计数）。四域一视同仁 —— **不因「本域 RPC 面只读」而省略**：读路径同样打 DB，且该跳走 Kitex TTHeader/TCP，Envoy 只按 `tcp_proxy` 处理、拿不到 L7 限流，故 `route` 的限流是本跳唯一的治理手段 |
| 全部 `srv-*` | `RPCServerOptions()` + `SetRPCServer()` | kitex 服务端（监听地址 / 注册 / trace suite 都在这里面） |
| 全部 `srv-*` | `Config()` / `Logger()` | 配置读取与日志 |
| `srv-order` | `DB()` 的用途 | `orders` / `order_items` / `order_status_log` 读写 |
| `srv-order` | `Cache()[<name>]` | 订单详情缓存（写后失效） |
| `srv-order` | `Lock()` | 同用户下单互斥 |
| `srv-order` | `MQ()` | `order.completed.v1` 生产 |
| `srv-points` | `DB()` 的用途 | 积分余额与流水读写 |
| `srv-points` | `MQConsumer()` | 订阅 `order.completed.v1` 并加积分 |
| `srv-product` | `DB()` 的用途 | 商品读写 |
| `srv-product` | `Cache()[<name>]` | 商品详情缓存 |
| `srv-stock` | `DB()` 的用途 | 库存读写 |
| `srv-stock` | `KV()[<name>]` | 另用于库存计数（原子增减）—— 与限流计数共用同一实例，靠不同 `namespace` 前缀隔离 |
| `srv-stock` | `Lock()` | 同商品扣减互斥 |
| 全部 `srv-*` | **不用** `Registry()` / `Discover()` | 服务发现交 K8s Service + EDS |
| 全部 `srv-*` | **不用** `RPCClientOptions()` | 该工厂只在 `bff` / `gateway` 档产出；跨域编排只在 bff（见 `mesh-entry-chain`） |
| 全部 `srv-*` | **不用** `Engine()` | 域服不自己暴露 HTTP：入口转发整块交 Istio `Gateway` + `VirtualService` |
| 全部 `srv-*` | **不用** `Tracer()` | 全局 provider 由 `launchx/internal/telemetry` 无条件注册，业务侧不再交接（见 `mesh-app-layering` 的埋点口径） |
| `bff` | **不用** `DB()` / `MQ()` | 边界不落业务数据，业务数据只在下游域服 |

#### Scenario: 句柄取用与业务动作一一对应

- **WHEN** 逐进程核对 `internal/**/app/setup.go` 与各层对句柄的使用
- **THEN** 上表每一行都能在代码里指到具体调用点；表外的句柄（如 srv 取 `Engine()`）
  不出现

#### Scenario: bff 不直连数据库

- **WHEN** 检查 `apps/internal/bff/**` 与 `apps/conf/bff.yaml`
- **THEN** 无 `sqlx` 段、无 `App.DB()` 调用；bff 的唯一数据入口是四域 RPC 客户端
  与它自己的凭据缓存

### Requirement: 句柄缺失的姿态必须显式

业务 MUST 对每个句柄的缺失给出**明确姿态**，MUST NOT 静默当作可用：

- **fail-fast**：句柄是业务的必要前提时，`Setup` MUST 返回 error 并中止启动。
  至少覆盖：`srv-*` 的 `DB()`（配 `strict.db: true`）与 `RPCServerOptions()`。
- **fail-open + Warn**：句柄只影响性能或降级体验时，MUST 允许为 nil，
  由业务自行判空并按「放行 + 打日志说清本次未走该路径」处理。
  至少覆盖：`Cache()`（缓存不可用只应变慢）、`KV()`（限流计数不可用即放行）、
  `Lock()`（MUST NOT 静默当作已加锁继续跑）。

`Setup` 失败 MUST 走与运行期错误同一条反序退出链，MUST NOT 留下半装配进程。

#### Scenario: 库不可达即退而不是带病运行

- **WHEN** `srv-order` 的 DSN 指向一个不可达地址且 `strict.db: true`
- **THEN** 进程以非零码退出，日志含 `init db` 上下文；**不得**出现「起来但落不了库」

#### Scenario: 锁句柄缺失不得静默

- **WHEN** `lockx` 段缺失导致 `App.Lock()` 为 nil
- **THEN** 下单路径 MUST 在日志中显式声明本次未走锁路径（不得静默按已加锁处理），
  且 MUST NOT 因缺锁而启动失败

### Requirement: 句柄来源与入链必须可自检

apps SHALL 在装配期（取到句柄之后、包一层之前）对**句柄来源合规性**与
**入链有效性**做一次自检，并把结果打进启动日志；任一不满足 MUST 让 `Setup` 返回 error。

自检至少要回答两件事：

1. 该句柄是 launchx 构造的，不是业务自己开的（业务自建的实例会绕过组件门控与入链）；
2. `trace.on.<name>` 为真时，该句柄确实会产出 span（`cache` / `kv` 这类句柄
   拿自建实例冒充是**完全静默**的，连「回调在不在」都探不到）。

`trace.on.<name>` 的判定 MUST 取**有效裁决**
（`trace.enabled && trace.on.<name>`），MUST NOT 直接读 yaml 原话 —— 总闸关掉时
组件开关读出来仍是真，照原话当期望值会把「总闸关」这种正当配置判成背离。

自检 MUST 覆盖 `db` / `cache` / `kv` / `mq` / `lock` 五类，
并 MUST 输出 `component` / `backend` / `joins_trace` / `probe` 四个字段。

各组件能取到的证据强度**不同，MUST 如实分级并写进 `probe`**，MUST NOT 把代理人
证据写成实锤：

| 组件 | 最强判据 | 强度 |
|---|---|---|
| `db` | gorm 句柄上是否挂着 dbx 注册的 trace 回调 | **来源实测**（回调随句柄走，裸 `gorm.Open` 一个都没有） |
| `cache` | `cachex.IsTracingEnabled()` | 包级开关代理（分不出实例来源） |
| `kv` / `mq` / `lock` | `traceflag.EnabledFor("kvx"/"mqx"/"lockx")` | 开关链代理（最弱，挡不住「拿自造句柄冒充」） |

#### Scenario: 自建句柄被拦下（限 `db` —— 唯一能当场取证的组件）

- **WHEN** 把 `App.DB()` 换成业务自己 `gorm.Open` 的句柄
- **THEN** 装配期自检报出该句柄来源不合规并使启动失败（而不是安静地少一段链路）

#### Scenario: 弱判据组件如实分级，不假装取到实锤

- **WHEN** 把 `App.Cache()` 换成业务自建的 redis 实例
- **THEN** 自检 **不** 声称拦下了它：`cachex.IsTracingEnabled()` 读的是包级开关，
  与「`App.Cache()` 里放的是哪个实例」无关；`kv` / `mq` / `lock` 同理只有
  `traceflag` 组件位这一档证据
- **AND** 这几类的 `probe` 字段 MUST 标注其为「开关链代理」，
  其来源合规由源码守卫（`dependency_surface_test.go` 一类）互补承担，
  而不是由装配期自检单独宣称

### Requirement: 句柄承载的业务动作必须可观测

每个经句柄完成的关键业务动作 MUST 落到 Jaeger 的同一 trace 内，并与 launchx 的分层
命名口径一致：span 名形如 `<layer>.<entity>.<op>`（如 `service.order.CreateOrder`），
组件级 span 由 `trace.on.<name>` 门控。

apps MUST NOT 自建 `internal/tracex` 之类的手工埋点层（追踪由 sidecar + launchx
telemetry 统一承担）；MUST NOT 在业务代码里手写 `traceparent` 头。

#### Scenario: 句柄动作入链

- **WHEN** 触发一次下单（`POST /api/order`）
- **THEN** 同一 trace 内可见：bff 的 handler span、四域 RPC 客户端 span、
  `srv-order` 的 `rpc.order.*` / `handler.order.*` / `service.order.*` 与
  `repository.order.*` span、`db.query`
  （带 `db.query.text` 与 `db.sql.table`）、以及 `messaging.*` / `cache.*` 组件 span；
  且这些 span 的父子链正确（非并列多根）
