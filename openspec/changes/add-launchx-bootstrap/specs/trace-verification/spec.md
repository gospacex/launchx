# trace-verification delta

本 capability 定义 launchx 的**端到端调用链验收标准**：Jaeger 里必须能看到完整调用树，而不是一堆孤儿 span。

## ADDED Requirements

### Requirement: 全局传播器注册（跨服务续链红线）

launchx 的 telemetry 步骤 SHALL 在 `otelx.New` 之后显式调用 `otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))`，且 MUST 早于任何组件的构造。

依据（已实测）：`otelx.New` 只调用 `otel.SetTracerProvider(tp)`（`otelx/interface.go:207`），**不注册传播器**；而 OTel 全局默认传播器是**空 Composite**（`otel@v1.46.0 internal/global/propagator.go`：`noop: propagation.NewCompositeTextMapPropagator()`，注入与提取均为空操作）。不注册则 `traceparent` 既不被写出也不被读取，跨进程链路必断成多段孤儿 trace——这是本 capability 最容易静默失败的一点，故立为红线而非约定。

#### Scenario: 验收案例 T1 — 传播器已注册

- **WHEN** launchx 完成 telemetry 步骤
- **THEN** `otel.GetTextMapPropagator()` 的 `Fields()` 含 `traceparent`、`tracestate`、`baggage` 三项
- **AND** 向空 carrier 注入一个有 span 的 ctx 后，carrier 中存在合法 `traceparent`（格式 `00-<32hex>-<16hex>-01`）

### Requirement: 数据层五类组件全部入链

`db` / `cache` / `mq` / `search`（Elasticsearch）/ `mongodb` 五类组件 SHALL 产出 span，且 span 的父节点为调用它的业务 span（同 trace，非孤儿）。

实测依据（各 SDK 埋点已存在，本要求是装配契约而非新增埋点）：

| 组件 | SDK 埋点位置 | span 名/属性 |
|---|---|---|
| db | `dbx/orm/gorm_tracing.go` | `db.create/query/update/delete`；属性 `db.system`、`db.sql.table`、`db.query.text` |
| cache | `cachex/redis/redis.go`、`cachex/memcached/memcached.go` | `GET/SET/DEL/EXISTS/KEYS/PING` |
| mq | `mqx/callchain.go` | `StartSpan` 产 span，经 traceflag 门控 |
| search | `searchx/elasticsearch/elasticsearch.go` | `db.elasticsearch.index/get/delete/search/bulk` |
| mongodb | `mongodbx/mongodb.go` | `db.mongodb.insert/find/update/delete/aggregate/...` |

#### Scenario: 验收案例 T2 — 数据库单表 CRUD 四类 span 且 SQL 入链

- **WHEN** 对单表执行 Create / Read / Update / Delete
- **THEN** Jaeger 中出现 4 个 `db.*` span，父节点为业务 span
- **AND** 每个 span 的 `db.query.text` 为**完整 SQL 语句**（非占位符、非空），`db.sql.table` 为表名
- **依据**：`dbx/orm/gorm_tracing_test.go:120` 已断言 `db.query.text` 非空，SQL 在 after 回调中仍可读

#### Scenario: 验收案例 T3 — 多表操作入链且 SQL 文本含 JOIN

- **WHEN** 执行跨表查询（`JOIN` / 预加载 `Preload`）与跨表事务（多表写入在同一 `Transaction` 内）
- **THEN** 每个 SQL 语句各自成 span，`db.query.text` 含 `JOIN`（或等价的关联查询文本），事务内的多条 SQL 为**兄弟 span**且同 trace
- **AND** 事务失败回滚时，相关 span 标 Error

#### Scenario: 验收案例 T4 — cache / mq / ES / mongodb span 同 trace

- **WHEN** 同一业务请求内依次操作缓存、投递消息、读写 ES、读写 MongoDB
- **THEN** Jaeger 中该 trace 下同时存在 cache、mq、`db.elasticsearch.*`、`db.mongodb.*` 四类 span，均为业务 span 的子节点

### Requirement: 单体形态全链路（路由 → 分层 → 数据层）

`examples/monolith` SHALL 在 Jaeger 中呈现一条完整调用树：HTTP 入口 span（webx `middleware.Otel`，形如 `GET /api/xxx`）→ handler → service → repo → 数据层 span。层级 MUST 由调用链自然形成（业务各层自开 span），launchx MUST NOT 代开业务 span。

#### Scenario: 验收案例 T5 — 单体调用树层级完整

- **WHEN** 请求 `examples/monolith` 的一个会落库的业务接口
- **THEN** 该 trace 的 span 树深度 ≥ 3（入口 / 业务层 / 数据层），且数据层 span 的祖先链中包含入口 span
- **AND** trace 中不含孤儿 span（所有 span 有共同 trace-id）

### Requirement: 微服形态三段同 trace（gateway → bff → srv）

`examples/micro` SHALL 在 Jaeger 中呈现单个 trace 跨三个进程：`gateway` → `bff` → `srv`，三段 span 由同一 trace-id 串起，父子关系正确（`gateway` span 为 `bff` span 的父，`bff` 为 `srv` 的父）。

出向注入缺口（已实测）：`webx/middleware/otel.go` **只有 Extract，没有 Inject**（其 `extractCarrier` 仅读取入站 `traceparent`/`tracestate`/`baggage`）。因此 gateway 转发 HTTP 到 bff 时的出向注入 MUST 由调用方显式完成——`micro_apps/apps/gateway/internal/proxy/proxy.go:134` 的 `otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(outReq.Header))` 是已验证写法。launchx SHALL 导出极小注入工具（仅做 `Inject(ctx, carrier)`，不含转发逻辑），供 gateway 档调用，`examples/micro/gateway` MUST 使用它；bff → srv 的 RPC 续链由 `hubx/rpcx` 的 kitex obs-opentelemetry 套件承担。

#### Scenario: 验收案例 T6 — gateway → bff → srv 同一 trace-id

- **WHEN** 请求 `examples/micro` 的 gateway 接口，该接口转发到 bff 并由 bff 调用 srv
- **THEN** Jaeger 中三条服务（gateway / bff / srv）的 span 共享同一 trace-id
- **AND** 父子链为 gateway → bff → srv（非三个并列根）
- **AND** srv 侧的数据层 span 亦在同一 trace 下

#### Scenario: 验收案例 T7 — 传播器未注册时链路断裂（反向断言）

- **WHEN** 单元测试中不调用 launchx 的 telemetry 步骤，直接注入并提取
- **THEN** 提取不到上游 span context（结果为 invalid SpanContext）
- **说明**：此用例证明 T1 的红线不是形式主义，是 T6 成立的必要条件

### Requirement: 网格形态与微服同构（Go 侧零改动）

网格形态 SHALL 复用与微服**完全相同**的 Go 代码路径，差异仅在部署清单与地址配置副本。sidecar **不参与应用内** span 的生成——应用 span 树的层级与父子关系 MUST 与 T6 逐条一致；sidecar 只允许在应用 span 之外**追加**自己的 proxy span。

「同构」的准确含义（避免误读为「网格下只有三段 span」）：网格下同一 trace 里既有 otelx 产出的应用 span（service.name = `gateway` / `bff` / `srv`），也有 Envoy sidecar 产出的 proxy span（service.name 形如 `<workload>.<namespace>`，如 `gateway.launchx-mesh`）；后者是**新增节点**，不替代也不改变前者。

#### Scenario: 验收案例 T8 — 网格不引入新的链路语义

- **WHEN** 检查 launchx 与 `examples/` 中是否存在网格专属的 trace 分支
- **THEN** 不存在（`mode` 枚举无 `mesh` 取值；无 sidecar 相关 trace 代码）
- **AND** `examples/mesh` 与 `examples/micro` 的三服务 `main.go` 差异仅在配置路径与地址副本，业务与框架调用代码逐行同构

### Requirement: 网格形态端到端链路（minikube + sidecar 合流）

网格环境 SHALL 由本机 **minikube** 提供（中间件仍由 `hubx/dockers` compose 提供，集群内经 `host.docker.internal` / 宿主 IP 访问），并在 sidecar 追踪开启后，使 Envoy proxy span 与 otelx 应用 span 合流于**同一 trace-id**。

配置口径（Istio）：
- 追踪后端经 `meshConfig.extensionProviders[].opentelemetry` 指向 OTLP 端点（本机 Jaeger `4317` gRPC 或 `4318` HTTP），并以 `defaultProviders.tracing`（IstioOperator）或 `telemetry.istio.io/v1 Telemetry` 资源（`randomSamplingPercentage: 100.0`）在 namespace / workload 层绑定；
- **传播头必须与 launchx 注册的 Composite{TraceContext, Baggage} 对齐**：`opentelemetry` provider 走 Envoy OTel tracer，默认注入/提取 W3C `traceparent`，与本设计一致；若改用 `zipkin` / `jaeger` provider，Envoy 默认走 B3（`x-b3-traceid` 系列），与应用侧头名不一致会直接断链——（**待核**：以所选 Istio 版本官方文档 + 真机 `istioctl proxy-config` / 实测请求头名为准，不凭记忆定稿）。

#### Scenario: 验收案例 T10 — 网格下 app span 与 sidecar span 同 trace

- **WHEN** 经 Istio Ingress Gateway 或集群内服务间调用触发 `gateway → bff → srv` 全链路
- **THEN** Jaeger 中该 trace 下同时存在应用 span（服务名 `gateway`/`bff`/`srv`）与 sidecar span（服务名形如 `gateway.launchx-mesh`）
- **AND** 应用 span 的父子链仍为 gateway → bff → srv（sidecar span 只作为插入节点，不改变应用 span 间关系）
- **AND** srv 侧数据层 span 仍在同一 trace 下

#### Scenario: 验收案例 T11 — 网格形态下的「半断链」反向断言

- **WHEN** 仅开启 sidecar 追踪、应用侧不注册全局传播器（即 T7 场景搬到网格里）
- **THEN** Jaeger 中呈现**分裂形态**：一条由 sidecar span 串起的完整链路 + 应用侧各自独立的孤儿 trace（预期 trace 数 ≥ 4：1 条 sidecar 链 + gateway/bff/srv 各 1 根）
- **说明**：这是网格形态特有的伪装——sidecar 之间靠 Envoy 自身透传头串联，看起来"很完整"，而应用 span 全部脱落。它证明 T1/T6 的红线在网格下**同样必要**，并给出可观测的判别特征（trace 数暴增、app 服务名各自成根）

#### Scenario: 验收案例 T12 — 网格验收证据落盘

- **WHEN** 完成一次网格端到端请求
- **THEN** `examples/mesh/docs/verify.md` 记录：trace-id、span 总数、应用 span 数 / sidecar span 数、sidecar 服务名、`http://127.0.0.1:16686/api/traces?service=<name>` 的原始响应片段、以及 `hubx.trace` OFF/ON 两次对比结果
- **AND** 记录本机环境版本：minikube / kubectl / istioctl / Istio 版本（可复现性前提）

### Requirement: Jaeger 端可验证

验收 SHALL 以 Jaeger（本机容器 `jaeger`，all-in-one 1.63.0，UI `127.0.0.1:16686`，OTLP/HTTP `127.0.0.1:4318`，`COLLECTOR_OTLP_ENABLED=true`）为观测端。otelx 走 OTLP/HTTP 导出（`otlptracehttp`，默认端点 `localhost:4318`），与 Jaeger 原生 OTLP 接收对齐。

Span 导出 MUST 在退出序列中完成冲刷（反序退出链的 `tracing.Shutdown`，15s 上限），否则尾部 span 随进程退出丢失——Jaeger 里会表现为"最后一个数据层 span 缺失"。

#### Scenario: 验收案例 T9 — Jaeger API 可检索到完整 trace

- **WHEN** 触发一次业务请求后查询 `http://127.0.0.1:16686/api/traces?service=<service.name>`
- **THEN** 返回该 trace，且 span 数量与形态符合 T5 / T6 的断言
- **AND** span 树中无缺失的尾部节点（证明退出冲刷生效）

网格形态复用同一观测端（Jaeger `16686`）与同一 API 断言方式，但服务名按 T10 同时含应用名与 sidecar 名，查询时 MUST 两者都查（应用侧查 `service=gateway`，sidecar 侧查 `service=gateway.<namespace>`）。
