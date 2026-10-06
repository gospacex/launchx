# add-launchx-bootstrap

## Why

hubx 生态已有 14 个 `*x` SDK（不含 launchx 自身）与三种参考形态（`webx/examples` 单体、`micro_apps` 11 服务微服务、`service_mesh` 网格），但每个项目都在手工重复同一条装配流水线：配置加载 → telemetry → 驱动 → 注册 → 优雅退出。`micro_apps` 每个 app 各自复制一份 `infra/`（nacos/telemetry/drivers/coordx），装配顺序红线（telemetry 必须先于 db/cache/mq、退出时会标 span 的资源必须先于 15s flush 关闭）只写在注释里靠人遵守，`srv-order` 的 main.go 长达约 300 行。

launchx 目标：把基础设施初始化固化成引导库，各项目 `launchx.Bootstrap("config.yaml") + OnSetup + Run` 一行启动；单体/微服/BFF/网关形态由 yaml `mode` 段声明；红线顺序由框架代码保证。首版在本目录 `examples/` 内交付**单体、微服、服务网格三个自包含验证 demo**（验收案例逐条标注），微服 demo 中的 srv 由 `micro_apps/srv-order` 迁移而来并跑通原 e2e，证明「300 行 → 一行」可行。

## What Changes

- 新建 `launchx` 引导库（独立 module）：`Bootstrap(path) → *App → OnSetup → Run()` 一行启动入口，固定七步骨架流水线 + 反序退出编排（红线代码化）
- 四档 `mode` 装配分支：`monolith`（webx 单体）/ `bff`（webx 入口 + consul + rpcx 下游客户端）/ `srv`（rpcx + consul）/ `gateway`（webx 转发 + consul 上游发现）；**mesh 不是独立档**（与 micro 同代码，仅部署差异）
- 可插拔配置源 `configsrc`：默认 local（configx + `${ENV}` 展开）；yaml 声明 `configcenter:` 才启用 nacos 合并（本地兜底 + key 级裁决 + env 覆盖地址）
- 可选组件注册表 `component`：db/cache/mq/registry，受 `Trace.On` 门控（判定经 `traceflag`，只控入链、不控构造）+ `strict` 策略（默认降级 Warn，置 true 失败即退）
- **依赖选型：hubx `*x` SDK 优先，三方依赖白名单制**（见 2026-10-04 依赖选型裁决）：配置 `configx` / 日志 `logx` / 遥测 `otelx` / DB `dbx/orm` / 缓存 `cachex` / MQ `mqx` / 注册发现 `coordx` / HTTP `webx` / trace 开关 `traceflag` / RPC `rpcx`；launchx 与 `examples/` MUST NOT 直引 hertz / go-redis / kafka-go / consul-api / gorm
- **`examples/` 三个自包含验证 demo（验收案例标注；本轮收敛为 monolith 与 micro 两个，mesh 后置——见 2026-10-04 修订 2）：**
  - `examples/monolith/` — 单体 demo，参考 `webx/examples` 分层与路由；订单存储接真实 mysql（底座）
  - `examples/micro/` — 微服 demo 全套：`gateway → bff → srv` 调用链，参考 `micro_apps` 精简；其中 `srv` 为迁移的 `srv-order`，跑通原 e2e（含 outbox → kafka 投递断言）
  - `examples/mesh/` — 服务网格 demo：**launchx 自写 k8s/Istio 清单**（namespace + 3 Deployment/Service + Istio sidecar 注入 + bring-up 脚本，参考 `service_mesh/` 仓设计结论：业务零改动 / 同 trace / 地址段转换；该仓清单未交付，copy 前提不成立），验收两级：清单 + 脚本 dry-run 全绿为硬验收，真实 minikube + Istio 同 trace 执行为人工验证项（证据入 `examples/mesh/docs/verify.md`，close 不硬阻塞）
- 验收案例标注：每条验收案例在 `specs/*/spec.md` 的 `#### Scenario:` 与 `tasks.md` 验证项中一一对应标注
- **BREAKING**：无（全新仓库）

## Impact

- Affected specs: `bootstrap-pipeline`、`config-loading`、`mode-assembly`、`component-registry`、`example-demos`（全部为新增 capability，`openspec/specs/` 当前为空）
- Affected code:
  - 新增：`launchx/`（launchx.go、app.go、setup.go、internal/{pipeline,mode,configsrc,component}、go.mod）
  - 新增：`examples/monolith/`、`examples/micro/`、`examples/mesh/`
  - 参考（只读不改）：`webx/examples`、`micro_apps/apps/*`、hubx 各 `*x` SDK；`service_mesh/` **仅作设计结论参考（业务零改动 / sidecar 同 trace / 地址段转换），非 copy 源**（该仓清单未交付，见 2026-10-04 修订 1）
  - 迁移源：`micro_apps/srv-order`（改造为 launchx 形态进 `examples/micro/srv`，原仓不动）
- 运行时依赖：hubx `*x` SDK（configx/logx/webx/rpcx/dbx/cachex/mqx/coordx）、consul、**e2e 底座复用 `hubx/dockers/docker-compose.yml`（consul / mysql 8.4 / kafka KRaft / jaeger all-in-one，不随仓自带 compose）**、minikube + istioctl（mesh 真实部署验证，人工项；**本轮后置**）
- 明确不做：脚手架/代码生成、etcd 等其他远端配置源（仅留注册点）、业务分层目录强约定、`micro_apps` 其余 10 服务本期不迁移（**形态通用、可接入**，见 2026-10-05 修订）

## 可观测增强（2026-10-05 并入，O1–O3 验收案例）

- 定位升级：launchx 是 hubx 通用 bootstrap 库，业务侧 = 一份 yaml + 分层代码，删除各 app 复制的 `infra/` 目录；`micro_apps` 11 服务未来可同形态接入
- metrics 全走 otel 推送：复用 otelx 通道（底座 jaeger/collector），四档 mode 统一，`srv` 档无 HTTP 亦可观测，**零新增业务 HTTP 端点**
- Pprof 走 `debug` 伪组件：`debug.enabled`（yaml 或 `APP_DEBUG=1`）时临时起仅监听 `127.0.0.1:6060` 的 debug server（标准 `net/http/pprof` handlers）；生产默认关；端口冲突 Warn 带病运行；退出时先于 logger.Close 关闭
- k8s 探针 = TCP + consul 双层：`examples/mesh/` 清单 liveness/readiness 用 `tcpSocket` 打业务端口（端口存活层），语义就绪由 consul 健康检查承载；Istio sidecar 侧补充探测（README 注明）
## 验收案例（O 系列，可观测增强）

- **O1** 单测：`debug` 组件门控——`debug.enabled=false` 不起 server；`true` 时 `127.0.0.1:6060` 仅回环可达（外网不可达）；退出时 debug server 先于 `logger.Close` 关闭（反序退出链插入点可断言）
- **O2** 集成：`APP_DEBUG=1` 启动 `srv` 档，`curl 127.0.0.1:6060/debug/pprof/` 返回 pprof 索引页；`debug.enabled` 经 yaml 置位时等效
- **O3** 清单：`examples/mesh/` 三个 Deployment 的 liveness/readiness 均为 `tcpSocket`（端口与业务端口一致，无 HTTP 端点假设）；`kubectl apply --dry-run=client` + `bring-up.sh check` 全绿

## 明确不做（可观测增强，本期）

- metrics Prometheus remote-write 直连
- 自定义业务指标 DSL
- healthz/readyz HTTP 端点例外（统一 TCP + consul，不实现 HTTP 探针）

## Amendments

### 2026-10-04: 补漏修订（brainstorming 续接确认 3 项）

1. **mesh 清单改为 launchx 自写**：原设计「copy `service_mesh/` 仓 k8s/Istio/bring-up 清单」前提不成立——该仓 `add-service-mesh-demo` 变更 0/16 任务完成、清单未实现。改为 `examples/mesh/` 独立编写最小 k8s + Istio 清单（namespace / Deployment / Service / sidecar 注入 / bring-up 脚本），设计结论（业务零改动、同 trace、地址段转换）仍参考 `service_mesh/` 仓 proposal。验收降为两级：清单 + `kubectl apply --dry-run=client` 全绿为硬验收；真实 minikube + Istio 部署同 trace 验证为人工项（证据入 `examples/mesh/docs/verify.md`，close 不硬阻塞）。
2. **e2e 底座复用共享环境**：不再随仓自带 docker-compose，统一依赖 `hubx/dockers/docker-compose.yml`（consul / mysql 8.4 / kafka KRaft / jaeger all-in-one，本机已部署）。
3. **E3 回归加 kafka outbox 链路**：srv-order 迁移 e2e 除下单链路外，追加 outbox 经 kafka 投递断言（topic 不存在时由 compose `KAFKA_CFG_AUTO_CREATE_TOPICS_ENABLE` 自动建 topic）。

### 2026-10-05: 通用化定位 + 可观测增强（brainstorming 确认 4 项）

1. **定位升级为通用基建**：launchx 是 hubx 通用 bootstrap 库——`micro_apps` 全部 11 服务未来可同形态接入（业务侧 = 一份 yaml + 分层代码，**删除各 app 复制的 `infra/` 目录**）。「明确不做」相应修订为：其余 10 服务**本期不迁移**，但形态通用、可接入；本期仅以 `srv-order` 迁移证明通用性。
2. **可观测全走 otel 推送，零新增业务 HTTP 端点**：metrics 复用 otelx 通道（底座 jaeger/collector），四档 mode 统一；`srv` 档无 HTTP server 亦可被观测；探针语义交 k8s + consul，不挂 healthz/readyz HTTP 端点。
3. **Pprof 走 `debug` 伪组件**：组件注册表新增 `debug` 项，`debug.enabled`（yaml 或 `APP_DEBUG=1` 置位）时临时起仅监听 `127.0.0.1:6060` 的 debug server（标准 `net/http/pprof` handlers）；生产默认关；端口冲突 Warn 带病运行（与 registry 降级策略一致）；退出时先于 `logger.Close` 关闭（插入反序退出链）。
4. **k8s 探针 = TCP + consul 双层**：`examples/mesh/` 清单 liveness/readiness 用 `tcpSocket` 打业务端口（端口存活层），语义就绪由 consul 健康检查承载（业务层）；Istio sidecar 侧补充探测，README 注明。

新增验收案例 O1/O2/O3（见上），并进入 spec 阶段的第 6 个 capability（observability）与 tasks 3.x 验证项。

### 2026-10-04: 命名与交付范围裁决（本轮 brainstorming）

1. **模块命名沿用 `launchx`，不改名 `infrax`**：本轮曾提出「各项目只要 import `infrax`」的命名形态，裁决为不采纳。理由：① 本 change 的 6 份 spec 与计划已按 `github.com/gospacex/launchx` 写完（含 A1–E5 验收案例），改名是纯机械返工且要同步核对全部编号；② `launchx` 符合 hubx 既有的「能力 + x」家族语义（webx / logx / cachex / configx），`launch` 即"引导"，与"基建"定位不冲突；③ 备选方案「拆两层 `infrax`（基建包）+ `launchx`（引导门面）」当前没有真正需要单独 import `infrax` 的调用方，属 YAGNI，且多一层 import 深度。业务侧可见形态不变：`import "github.com/gospacex/launchx"` + 一份 yaml + 分层代码。
2. **本轮交付范围收敛：内核 + monolith + micro，以 `tasks.md` 条目编号为准**（计划文件的 superpowers Task 编号 1–11 只覆盖实现与 demo 代码，会漏掉全部验证条目，故本轮范围一律用 openspec 条目号表达）：
   - **跨仓前置**：0.1（`../coordx/consul` 注册修复）、0.2（新建 `../rpcx` 模块）
   - **实现**：1.1–1.7（单入口 API、configsrc、component、mode、pipeline、反序退出）
   - **Demo**：2.1–2.4（monolith、micro 的 gateway/bff/srv 三进程，srv 由 `srv-order` 迁移）、2.6（monolith 与 micro 两 demo 的 README）
   - **验证**：3.2（A2/A3 顺序断言）、3.3（D1/D2）、3.4（C1/C2/C3 + 非法 mode 报错）、3.5（E1）、3.6（E2）、3.7（E3，含 outbox→kafka 投递断言）、3.9（A3 降级路径）、3.1（B1；其中依赖真机 nacos 底座的 key 级裁决断言以 Skip 形态保留，无底座不阻塞，不强制起 nacos 容器）、3.14（依赖面核对：无 hertz/kitex/go-redis/kafka-go/consul-api/gorm 直引）
   - **后置**（不在本轮阻塞，回补时开新 change）：1.8（`debug` 伪组件）、2.5（`examples/mesh/` 清单）、3.8（E4/E5 网格两级验收）、3.11–3.13（O1–O3）、3.10 中涉及后置案例的对齐核对部分；另 `micro_apps/pkg/rpcx` 与 `gospacex/rpcx` 向 `hubx/rpcx` 的收敛亦不在本轮
3. **「网格」仍不是 mode 档（复述 C4）**：原始诉求把"单体 / 微服 / 网格"三者并列，本轮再次确认不成立——网格与微服的 Go 侧装配代码路径完全相同，差异 100% 在部署清单、配置地址副本与 Consul KV 重写（`service_mesh/design.md` 决策 2/6 已证）。`mode` 枚举恒为 `monolith | bff | srv | gateway`，`mode: mesh` 必须被拒绝并列出合法取值。
4. **本轮验收案例集**：A1–A3、B1（本地 + 远端不可达两分支；真机 nacos 分支以 Skip 形态保留）、C1–C3、D1–D2、E1–E3，**以及 T1–T9**（Jaeger 端到端调用链，随 tasks 第 4 节 4.1–4.8 验收）。**后置且保持未验收态**：C4（需 `examples/mesh` 真实部署）、E4–E5、O1–O3。C4 中「`mode` 必须拒绝 `mesh` 取值」这一纯代码断言随 3.4 一并验收，其「经 k8s+Istio 部署、二进制与微服 demo 一致」部分随后置（T8 为同构代码断言，随 4.8 验收）。

### 2026-10-04: 依赖选型裁决（hubx `*x` SDK 优先）

1. **原则：每个能力层优先采用 `hubx/` 目录下以 `x` 结尾的 SDK，三方依赖白名单制**。逐层盘点结果：配置 → `configx`；日志 → `logx` + `logx/zap`；遥测 → `otelx`；DB → `dbx/orm`（yaml 键沿用 `xmiddleware.sqlx`）；缓存 → `cachex` + `cachex/redis`；MQ → `mqx` + `mqx/kafka`；注册发现 → `coordx` + `coordx/consul`；HTTP → `webx`（内包 hertz）；trace 开关 → `traceflag`。九个能力层均有对应 SDK，launchx 与 `examples/` MUST NOT 直接 require `cloudwego/hertz`、`redis/go-redis`、`segmentio/kafka-go`、`hashicorp/consul/api`、`gorm.io/gorm`——这些一律经 `*x` SDK 间接引入，避免同一能力出现两套客户端。有 SDK 但本轮不接：`lockx` / `kvx` / `objectx` / `vectorx`（无调用方，YAGNI；组件注册表可扩展，后续按需 `Register`）。**修订（2026-10-04）**：`mongodbx` 与 `searchx` 原列此处，因端到端调用链验收标准要求 ES 与 MongoDB 入链，二者已改列本轮内置组件（见下条 amendment 第 2 点），此处表述以修订为准。
2. **RPC 是唯一 SDK 真空，补为新建 `hubx/rpcx`（第 15 个 `x` SDK）**：`hubx/` 下 14 个 `x` SDK 无一封装 kitex（`grep -rl cloudwego/kitex *x/` 零命中）。已排除两个候选：`gospacex/infrax` 是只有空目录、零 go 文件的空壳；`gospacex/rpcx` 模块名为裸 `rpcx`（需 replace 才能 import）且依赖 `regdisx` 而非 hubx 家族。新建的 `github.com/gospacex/rpcx` 封装 kitex `server`/`client` Option 装配（`BuildServerOptions` / `BuildClientOptions`，实现自 `micro_apps/pkg/rpcx` 已验证版本移植），**发现层 MUST 走 `coordx`、MUST NOT 引入 `regdisx`**（否则 hubx 家族出现两套注册中心）；续链套件只从 `otel.GetTracerProvider()` 取用、不自行装配 provider（与「telemetry 最先」红线一致）。与 coordx/consul 修复同处理：本 change 含该仓新建，`replace` 指本地，不发布新版本。`micro_apps/pkg/rpcx` 与 `gospacex/rpcx` 的收敛不在本轮。
3. **`trace.on.<name>` 判定改接 `traceflag`**：`traceflag` 虽非 `x` 结尾，但在 `hubx/` 下且是 hubx x 系列统一的官方开关实现（`micro_apps` 引用 33 次），其存在正是为解决 `enable_trace` / `tracing.enabled` / `otel.enabled` / `tracer.enabled` 四套字段名分裂、部分组件配了 false 照样埋点、部分组件定义了字段却从不读取。语义为「实例级 AND 组件级 AND 全局总闸」，launchx 只负责把 yaml `trace.on.<name>` 映射为组件级注册，不自造布尔判定。约束不变：开关仍只控入链、不控构造（D1 红线不受影响）。

### 2026-10-04: 端到端调用链验收标准（Jaeger，T1–T9）

用户给出的验收标准：**在 Jaeger 里完成全链路调用链**——单体从路由到各分层的完整链路；微服 gateway → bff → srv；网格同构；数据层操作 DB / 缓存 / 队列 / ES / MongoDB 均须入链；数据库操作要覆盖单表与多表 CRUD，且 **SQL 语句要入链**。据此新增 capability `trace-verification` 与 tasks 第 4 节。

1. **新增红线：launchx MUST 显式注册全局传播器**。`otelx.New` 只做 `otel.SetTracerProvider`（`otelx/interface.go:207`），**不注册传播器**；而 OTel 全局默认传播器是**空 Composite**（`otel@v1.46.0 internal/global/propagator.go`：`noop: propagation.NewCompositeTextMapPropagator()`），注入与提取均为空操作。不注册则 `traceparent` 既不写出也不读取，跨进程链路必断成多段孤儿 trace。危险之处在于**它看起来像成功**：Jaeger 里三个服务各有一根 trace，每条都能点开、都有 span，但彼此无关。故 T6 断言"共享同一 trace-id 且父子链正确"，而非"各服务都能查到 trace"，并配 T7 反向断言锁死。
2. **数据层组件 4 类 → 6 类，新增 `search`(ES) 与 `mongodb`**。实测两个 SDK 埋点已存在（`searchx/elasticsearch` 产 `db.elasticsearch.*`；`mongodbx` 产 `db.mongodb.*`），本轮是装配契约而非新增埋点。撤回上一轮「`mongodbx` / `searchx` 有 SDK 但本轮不接（YAGNI）」的表述——验收标准明确要求二者入链，调用方已存在。
3. **SQL 入链无需改 SDK**：`dbx/orm/gorm_tracing.go` 已在 create/query/update/delete 四个回调组上产 span，属性含 `db.system` / `db.sql.table` / `db.query.text`（完整 SQL），且 `gorm_tracing_test.go:120` 已断言 `db.query.text` 非空。本轮只需在 demo 里覆盖单表 CRUD 与多表（JOIN / 事务）两类场景并做 Jaeger 端断言。
4. **gateway → bff 的出向注入是 SDK 真空**：`webx/middleware/otel.go` **只有 Extract 没有 Inject**（入站读 `traceparent`/`tracestate`/`baggage` 三头），全 hubx 无出向注入 helper。已验证写法是 `micro_apps/apps/gateway/internal/proxy/proxy.go:134` 的 `otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(outReq.Header))`。launchx 只导出极小 `Inject` 薄封装（不含转发逻辑），由 `examples/micro/gateway` 调用；bff → srv 的 RPC 续链由 `hubx/rpcx` 的 kitex obs-opentelemetry 套件承担。
5. **观测端与底座（实测环境）**：Docker Desktop 在跑（daemon v29.7.2），CLI 在 `/Applications/Docker.app/Contents/Resources/bin/docker`、**不在 PATH**。`jaeger` 容器存在但已停止（镜像 `jaegertracing/all-in-one:1.63.0`，16686 / 4317 / 4318，`COLLECTOR_OTLP_ENABLED=true`）；otelx 走 OTLP/HTTP `otlptracehttp`（默认 `localhost:4318`），与 Jaeger 原生 OTLP 接收对齐，无需 collector 中转。需启动 `jaeger` / `mysql` / `kafka`，**新建 `elasticsearch`**（本机无该容器，镜像 `elasticsearch:7.6.2` 本地已有）；`consul` / `postgres` / `mongo1-3` / `keydb` / `memcached` / `rabbitmq` / `nsq` / `rocketmq` / `etcd` 已在跑。所有镜像均在本地，无需联网拉取。底座脚本 `examples/scripts/bring-up-trace.sh` 为第 4 节验收的前置（tasks 0.3）。
6. **网格按同构处理，不拉回本轮**：验收标准中「服务网格类似」按 C4 原裁决落地——网格与微服共用完全相同的 Go 侧路径，无专属 trace 分支（T8 代码断言）；其真实部署（`examples/mesh` 清单 + minikube/Istio）仍为本轮后置。

### 2026-10-06: 用 goforj/wire 完成 16 个 hubx SDK 的统一注册（第 7 个 capability）

用户要求：launchx 使用 `https://github.com/goforj/wire` 完成下列 SDK 的注册 ——
`cachex / configx / coordx / dbx / hubx / kvx / lockx / logx / mongodbx / mqx /
objectx / otelx / rpcx / searchx / vectorx / webx`（16 个）。据此新增 capability
`dependency-injection` 与验收案例 W1–W8、`tasks.md` 第 5 节。

调研所得的三个事实（决定了本次的设计边界，均非推测）：

1. **goforj/wire 是 google/wire 的维护分支，编译期代码生成**，无运行时容器、无反射；
   要求 Go ≥ 1.25（launchx `go 1.27.1`、`hubx` `go 1.26.2`，均满足）；既有
   `google/wire` 代码**无需改动**即可迁移（官方兼容声明），手段是
   `go mod edit -replace=github.com/google/wire=github.com/goforj/wire@<tag>`。
2. **`hubx` 模块已经在用 wire，但只画了一半**：其 `wire.go` 有 `ProviderSet`，
   但 `InitializeApp` **未调用 `wire.Build(...)`**（不是合法 injector），且全仓
   **无 `wire_gen.go`**（从未生成过）。故本次对 hubx 侧是「换分支 + 真正接线」，
   不是从零引入。
3. **wire 按 type identity 匹配、provider 须返回具体类型**，而 launchx 的
   `component.Env.DB` 是刻意的 `any`（库不直引 gorm）——**弱类型字段无法直接入图**，
   必须引入具名句柄类型。这是本次最大的实现成本点。

因此本次修订的**核心边界**是：wire 只接管「依赖求解与构造顺序」，
**presence-based 启用 / `trace.on` 入链门控 / `strict` 失败姿态这三条运行时裁决
仍留在 provider 内部**，D1/D2/A3 既有红线与断言一律不回归。详见
`design.md`「Decision: wire 只接管依赖求解，不接管运行时裁决」。

同时修订两条既往约束（不改语义，只补边界）：

- **Non-Goals「脚手架 / 代码生成」**：该条针对的是「launchx 为业务项目生成脚手架」，
  仍成立；新增的是「launchx **自身构建期**用 wire 生成装配代码」，二者不同层，
  不冲突。已在 design.md Non-Goals 中显式区分。
- **依赖选型「有 SDK 但本轮不接：`objectx` / `vectorx`（YAGNI）」**：改为
  「登记在册但默认不构造」——provider 存在、presence-based 未命中即零值，
  使「注册面完整」与「不为无调用方付构造成本」同时成立。
