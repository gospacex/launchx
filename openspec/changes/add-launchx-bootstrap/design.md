# add-launchx-bootstrap — design.md

## Context

hubx 生态三种形态（webx 单体、micro_apps 微服务、service_mesh 网格）各自手写装配流水线，顺序红线散落在注释里。launchx 要把它固化为引导库，并在 `examples/` 交付自包含验证 demo（本轮交付 monolith 与 micro，mesh 后置）。约束：不改 hubx 各 `*x` SDK 与参考仓；红线顺序必须由框架保证，不能依赖使用方自觉。

## Goals / Non-Goals

- Goals:
  - `Bootstrap(config) + OnSetup + Run` 单入口启动，main.go ≤ 50 行
  - 启动/退出顺序红线代码化（telemetry 最先；会标 span 资源先于 15s flush 关闭；退出不走 defer 跳过）
  - mode（monolith/bff/srv/gateway）由 yaml 声明，装配分支可扩展
  - 配置源、可选组件均为注册表（可插拔），默认行为与 micro_apps 现状语义一致（降级 + Warn）
  - examples/ 三 demo 自包含可复现，验收案例逐条标注
- Non-Goals:
  - 脚手架 / 代码生成、`launchx check` 目录校验
  - 直接 require hertz / go-redis / kafka-go / consul-api / gorm 等三方客户端 —— 一律经对应 `*x` SDK 间接引入（依赖选型 Decision）
  - 为 hubx 已有 SDK 覆盖的能力另建封装；`micro_apps/pkg/rpcx` 与 `gospacex/rpcx` 向新建的 `hubx/rpcx` 收敛不在本轮（回补时开新 change）
  - etcd 等除 nacos 外的远端配置源实现
  - micro_apps 其余 10 个服务**本期**迁移（形态通用、可接入；以 srv-order 迁移证明通用性）
  - 业务分层结构强约定
  - （可观测增强）metrics Prometheus remote-write 直连、自定义业务指标 DSL、healthz/readyz HTTP 端点
  - （本轮范围，2026-10-04 裁决）后置项：`examples/mesh/` 清单与两级验收、`debug` 伪组件与 O1–O3、trace 开关 OFF/ON 对比、涉及后置案例的对齐核对。本轮做 `tasks.md` 的 1.1–1.7 / 2.1–2.4 / 2.6 / 3.1–3.7 / 3.9，理由与编号陷阱说明见 Decisions「本轮范围收敛」

## Decisions

- **Decision: 固定骨架流水线 + 注册表（brainstorming 方案一）**
  - Alternatives: 全生命周期插件框架（退出/顺序红线退化为约定，首版过度设计）、纯硬编码流水线（组件增删改全动核心，与四 mode 目标不匹配）
  - 骨架步骤固定不可打乱；组件以 `Register(name, Builder)` 挂入，mode 分支只产出「服务端 + 发现」差异
- **Decision: mesh 不是独立 mode**
  - Alternatives: mode 加 mesh 档（与 service_mesh 仓「业务零改动」结论矛盾）
  - mesh demo = 在 `examples/mesh/` 独立编写最小 k8s + Istio 清单（原设计是 copy `service_mesh/` 仓清单，但该仓清单尚未交付，0/16 任务完成，copy 前提不成立；改为自写，业务零改动/同 trace/地址段转换三项结论仍作设计参考）
- **Decision: e2e 底座复用 `hubx/dockers/docker-compose.yml`，不随仓自带 compose**
  - Alternatives: launchx 仓自带精简 compose（重复维护、与本机既有环境端口冲突风险）
  - 本机已部署该共享环境（consul / mysql 8.4 / kafka KRaft / jaeger all-in-one），monolith 与 micro 两个 demo 的 e2e 验收直接依赖它；srv-order 迁移回归追加 outbox→kafka 投递断言（compose 配置 `KAFKA_CFG_AUTO_CREATE_TOPICS_ENABLE=true`，topic 自动创建）
- **Decision: Setup 为显式回调接口**（`OnSetup(func(*App) error)`），不做 init() 自动发现（隐式、依赖 init 顺序，不可测）
- **Decision: 两级错误模型** — 地基（配置/telemetry/logx）失败即退；组件按 `strict.<name>` 裁决降级或退出；Setup 失败走完整反序退出后返回错误
- **Decision: 独立 go module** — launchx 不并入 hubx 根 module，避免依赖面扩大；通过 require 引用各 `*x` SDK
- **Decision: 修 coordx/consul 上游注册 payload，launchx 仍按原计划走 coordx 封装**
  - Alternatives: launchx 直连 `hashicorp/consul/api` 自封装（D4 措辞、技术栈、replace 块全要改）、原样保留 coordx（不可取）
  - 实测 Consul 1.22.4 拒绝 `Check{TTL:"30s", Interval:"10s"}`（HTTP 400 `Invalid check: Interval and TTL cannot both be specified`），原 `register.go` 路径注册恒失败 → strict 降级下永远带病未注册，E2 consul 发现链必挂
  - 修复落在兄弟仓 `../coordx/consul/consul.go`：删 `Interval` 只留 TTL，并补 `startBeat` 心跳（每 10s `UpdateTTL`，`Deregister` 成功/`Close` 停止，同 ID 重注册先停旧）；本 change 含该仓修复（replace 指本地），不发布 coordx 新版本
  - 实测通过：注册 200、passingOnly 发现 1 实例、35s 后 check 仍 passing（心跳存活）、注销后归零；`go build`/`go vet` 于 `../coordx/consul` 全绿。落地代码与复验步骤见计划文件 Task 4 Step 4.1（对应 `tasks.md` 0.1）
- **Decision: 可观测全走 otel 推送，零新增业务 HTTP 端点（2026-10-05 并入）**
  - Alternatives: 统一 ops sidecar 端口（多一个构件，srv 档被迫起 HTTP）、webx 系挂 handler + srv 走 TCP 探活（四档语义不一）
  - metrics 复用 otelx 通道（底座 jaeger/collector），四档统一；k8s 探针 liveness/readiness 用 `tcpSocket` 打业务端口（端口存活层），语义就绪由 consul 健康检查承载（业务层）；Istio sidecar 侧补充探测（README 注明）
- **Decision: Pprof 走 `debug` 伪组件（2026-10-05 并入）**
  - 组件注册表内置 `debug` 项：`debug.enabled`（yaml 或 `APP_DEBUG=1` 置位）时临时起仅监听 `127.0.0.1:6060` 的 debug server（标准 `net/http/pprof` handlers）；生产默认关；端口冲突 Warn 带病运行（与 registry 降级一致）；退出序列中 debug server 先于 `logger.Close` 关闭（插入反序退出链，事件记录器可断言）

- **Decision: 模块命名沿用 `launchx`，不改名 `infrax`（2026-10-04 裁决）**
  - Alternatives: 改名 `infrax`（对齐 `*x` SDK 命名家族，强调基建层）——代价是 6 份 spec 与计划全量改名，A1–E5 编号需逐条同步核对，纯机械返工；拆两层 `infrax`（可复用基建包）+ `launchx`（引导门面）——更灵活，但当前没有真正需要单独 import `infrax` 的调用方，属 YAGNI，且多一层 import 深度
  - `launchx` 符合 hubx 既有「能力 + x」家族语义（webx / logx / cachex / configx），`launch` 即"引导"；业务侧可见形态不变：`import "github.com/gospacex/launchx"` + 一份 yaml + 分层代码，**删除各 app 复制的 `infra/`**
- **Decision: 依赖选型 —— hubx `*x` SDK 优先，三方依赖白名单制（2026-10-04 裁决）**
  - 每个能力层 MUST 优先采用 `hubx/` 目录下以 `x` 结尾的 SDK；仅当 hubx 无对应 SDK 时才直引三方，且三方清单 MUST 显式写入白名单并注明理由
  - 能力层映射：配置 → `configx`；日志 → `logx` + `logx/zap`；遥测 → `otelx`；DB → `dbx/orm`（yaml 键沿用 `xmiddleware.sqlx`，实例名 = 可部署单元名）；缓存 → `cachex` + `cachex/redis`；MQ → `mqx` + `mqx/kafka`；注册发现 → `coordx` + `coordx/consul`；HTTP 服务端 → `webx`（内包 hertz）；trace 开关 → `traceflag`；RPC → `rpcx`（新建，见下条）
  - launchx 与 `examples/` 的 go.mod MUST NOT 直接 require `cloudwego/hertz`、`redis/go-redis`、`segmentio/kafka-go`、`hashicorp/consul/api`、`gorm.io/gorm` —— 这些能力一律经对应 `*x` SDK 间接引入，避免同一能力出现两套客户端
  - 有 SDK 但本轮不接：`lockx` / `kvx` / `objectx` / `vectorx`（YAGNI，无调用方）；组件注册表可扩展，后续按需 `Register` 接入，不预先占位。**修订（2026-10-04）**：`mongodbx` 与 `searchx` 原列此处，因端到端调用链验收标准要求 ES 与 MongoDB 入链，二者已改列本轮内置组件（见 Decisions「数据层组件从 4 类扩到 6 类」），此处表述以修订为准
  - Alternatives: 混合直引三方（同一能力出现两套客户端，版本与埋点口径分裂）；把三方全部再包一层（与 webx/cachex 已有封装重复）
- **Decision: RPC 补 SDK 真空 —— 新建 `hubx/rpcx`（第 15 个 `x` SDK），不在 launchx 内建 `internal/rpcx`（2026-10-04 裁决）**
  - 背景：`hubx/` 下 14 个 `x` SDK 无一封装 kitex（`grep -rl cloudwego/kitex *x/` 零命中），RPC 是唯一真空。`gospacex/infrax` 为只有空目录、零 go 文件的空壳；`gospacex/rpcx` 模块名是裸 `rpcx`（需 replace 才能 import）且依赖 `regdisx` 而非 hubx 家族 —— 两者均不可作为 hubx SDK 采用
  - Alternatives: launchx `internal/rpcx` 自封装（依赖面最小，但会产生第三份 rpcx 封装：`gospacex/rpcx` / `micro_apps/pkg/rpcx` / launchx，收敛成本转嫁给未来）；直引 `gospacex/rpcx`（裸模块名 + regdisx，与 coordx 形成双套注册中心）
  - 新建 `github.com/gospacex/rpcx` 独立 module：封装 kitex `server`/`client` Option 装配（`BuildServerOptions` / `BuildClientOptions`），实现思路复用 `micro_apps/pkg/rpcx` 已验证版本（缺省超时、优雅退出排空等待、obs-opentelemetry 套件挂载）
  - 跨服务续链挂 `kitex-contrib/obs-opentelemetry` Suite，该 Suite MUST 只从 `otel.GetTracerProvider()` 取用，MUST NOT 自行装配 provider —— 否则与「telemetry 最先」启动红线冲突
  - **发现层 MUST 走 `coordx`，MUST NOT 引入 `regdisx`** —— 否则与依赖选型中「注册发现统一 coordx」自相矛盾，hubx 家族将出现两套注册中心
  - 与 coordx/consul 修复同处理：本 change 含该仓新建，`launchx/go.mod` 以 `replace` 指本地 `../rpcx`，不发布新版本；`micro_apps/pkg/rpcx` 与 `gospacex/rpcx` 向 `hubx/rpcx` 的收敛不在本轮，回补时开新 change
- **Decision: `trace.on.<name>` 判定改接 `traceflag`（2026-10-04 裁决）**
  - Alternatives: launchx 自写布尔判定 —— 与 traceflag 已解决的问题重复造轮子（该包的存在正是为了收敛 `enable_trace` / `tracing.enabled` / `otel.enabled` / `tracer.enabled` 四套字段名分裂、部分组件配了 false 照样埋点、部分组件定义了字段却从不读取），且口径易与 hubx 各组件漂移
  - `traceflag` 虽非 `x` 结尾，但在 `hubx/` 下且是 hubx x 系列统一的官方开关实现（`micro_apps` 引用 33 次）；语义为「实例级 AND 组件级 AND 全局总闸」，组件侧接入点为 `traceflag.Apply(name, enableTrace)` / `traceflag.NewTracer(...)`
  - 约束不变：`trace.on.<name>` MUST 仍只控是否注册 OTel 回调（入链），MUST NOT 决定组件是否构造（验收案例 D1 的红线不受影响）
  - E5（trace OFF/ON 对比）语义直接对齐：全局总闸 `traceflag.SetEnabled(false)` 即整体熔断，无需另造开关
- **Decision: 本轮范围收敛为「内核 + monolith + micro」，以 `tasks.md` 条目编号表达（2026-10-04 裁决）**
  - Alternatives: 只做内核 + monolith demo（YAGNI 最激进，但无法端到端证明「同一套代码 + 不同 yaml = 不同形态」这个核心卖点）；全量交付（mesh 部分价值集中在部署层，对 launchx 自身代码能力增量有限，且本机 minikube 集群未运行，真实部署本就是人工验证项）
  - **编号陷阱（已修正）**：计划文件用 superpowers Task 1–23 编号，其 Task 1–11 只覆盖「实现 1.1–1.7 + demo 2.1–2.4」，若照此切会漏掉全部验证条目 3.1–3.7，A2/A3/D1/D2/E1/E2/E3 将无验收证据。故本轮范围一律以 openspec `tasks.md` 条目号表达
  - 本轮做：0.1–0.2（跨仓前置）、1.1–1.7、2.1–2.4、2.6（两 demo README）、3.1–3.7、3.9
  - 本轮不做：1.8（`debug` 伪组件）、2.5（`examples/mesh`）、3.8（E4/E5）、3.10 中涉及后置案例的部分、3.11–3.13（O1–O3）；另 `micro_apps/pkg/rpcx` 与 `gospacex/rpcx` 向 `hubx/rpcx` 的收敛亦不在本轮
  - 第一版交到「配置决定形态」被 monolith 与 micro 两个 demo 端到端证明为止；网格沿用与 micro 完全相同的代码路径，清单与两级验收后置
- **Decision: launchx 的 telemetry 步骤 MUST 显式注册全局传播器（2026-10-04 裁决，跨服务续链红线）**
  - 实测依据：`otelx.New` 只调用 `otel.SetTracerProvider(tp)`（`otelx/interface.go:207`），**不注册传播器**；OTel 全局默认传播器是**空 Composite**（`otel@v1.46.0 internal/global/propagator.go`：`noop: propagation.NewCompositeTextMapPropagator()`，注入与提取均为空操作）
  - 后果：不注册则 `traceparent` 既不写出也不读取，gateway → bff → srv 必然断成三段孤儿 trace——Jaeger 里每服务各一根，看着"都有链路"实则全断，属于静默故障
  - 实现：telemetry 步骤在 `otelx.New` 之后调用 `otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))`，且早于任何组件构造
  - Alternatives: 留给各 app 自行注册（正是 `micro_apps/infra/telemetry/provider.go:112` 被复制 11 遍的做法，抽象后仍会漏）；改 otelx 内部注册（跨仓改动，且 otelx 的语义是"按 Config 装配 Provider"，传播器属宿主决策，不该下沉）
  - 反向断言 T7 随单测落地，证明该红线是 T6 成立的必要条件而非形式主义
- **Decision: 数据层组件从 4 类扩到 6 类，新增 `search`（ES）与 `mongodb`（2026-10-04 裁决）**
  - 实测：两个 SDK 埋点均已存在——`searchx/elasticsearch` 产 `db.elasticsearch.index/get/delete/search/bulk`；`mongodbx` 产 `db.mongodb.insert/find/update/delete/aggregate/...`。本轮是**装配契约**，不是新增埋点
  - 沿用 presence-based 启用：配了 `xmiddleware.searchx` / `mongodbx` 段才构造；`strict.search` / `strict.mongodb` 沿用同一降级裁决
  - `searchx/elasticsearch` 与 `cachex/redis`、`mqx/kafka`、`coordx/consul` 一样是**独立 go module**，go.mod 需补对应 `replace`
  - Alternatives: 本轮只做 db/cache/mq（不满足验收标准；ES 与 MongoDB 入链是明确要求）
- **Decision: gateway → bff 的出向注入由 launchx 导出极小工具，不把转发逻辑收进框架（2026-10-04 裁决）**
  - 实测：`webx/middleware/otel.go` **只有 Extract，没有 Inject**（入站读取 `traceparent`/`tracestate`/`baggage` 三个头）；全 hubx 无出向注入 helper
  - 已验证写法：`micro_apps/apps/gateway/internal/proxy/proxy.go:134` 的 `otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(outReq.Header))`
  - launchx 只导出 `Inject(ctx, carrier)` 语义的薄封装（不代发 HTTP、不接管路由转发），`examples/micro/gateway` 调用它；bff → srv 的 RPC 续链由 `hubx/rpcx` 的 kitex obs-opentelemetry 套件承担
  - Alternatives: 给 webx 补 Inject 中间件（跨仓改动 webx，且 webx 是 HTTP 框架层，出向注入属客户端职责，放框架里会与 hertz client 耦合）；每个 gateway 复制 3 行（正是要消灭的复制件形态）
- **Decision: 验收以本机 Jaeger 容器为观测端，底座纳入本轮前置（2026-10-04 裁决）**
  - 实测环境：Docker Desktop 在跑（daemon v29.7.2，CLI 于 `/Applications/Docker.app/Contents/Resources/bin/docker`，**不在 PATH**）；`jaeger` 容器存在但已停止，镜像 `jaegertracing/all-in-one:1.63.0`，端口 `16686`(UI) / `4317`(OTLP gRPC) / `4318`(OTLP HTTP)，`COLLECTOR_OTLP_ENABLED=true`
  - otelx 走 OTLP/HTTP（`otlptracehttp`，默认端点 `localhost:4318`），与 Jaeger 原生 OTLP 接收对齐，无需 collector 中转
  - 底座状态：`consul`/`postgres`/`mongo1-3`/`keydb`/`memcached`/`rabbitmq`/`nsq`/`rocketmq`/`etcd` 在跑；`jaeger`/`mysql`/`kafka` **已停止需启动**；**无 Elasticsearch 容器**（镜像 `elasticsearch:7.6.2` 本地已有，需新建）
  - 所有镜像均在本地（镜像源为 `swr.cn-north-4.myhuaweicloud.com` 与 `hub.rat.dev` 的已拉取副本），无需联网拉取

## 定位（2026-10-05 修订）

launchx 是 hubx 通用 bootstrap 基建库：业务侧 = 一份 yaml + 分层代码，**删除各 app 复制的 `infra/` 目录**；`micro_apps` 11 服务未来可同形态接入。本期仅迁 `srv-order` 一个证明通用性。

## Risks / Trade-offs

- [srv-order 迁移暴露隐式耦合（全局 viper、包级 init）] → 迁移前用 impact 分析 srv-order main.go 依赖；保留原 e2e 作回归网（回归含 outbox→kafka 断言）
- [examples/mesh 需 minikube+Istio 环境，CI 不可复现] → mesh 验收分两级：清单 + `kubectl apply --dry-run=client` 全绿为硬验收（CI/自动化可复现）；真实 minikube + Istio 部署同 trace 验证为人工项，证据入 `examples/mesh/docs/verify.md`，close 不硬阻塞
- [新建 `hubx/rpcx` 属跨仓改动，本轮工作量前移] → 与 coordx/consul 修复同处理：`replace` 指本地 `../rpcx`、不发布新版本；`BuildServerOptions`/`BuildClientOptions` 从 `micro_apps/pkg/rpcx` 已验证实现移植（缺省超时 / 优雅退出排空 / obs-opentelemetry 套件挂载点均已跑通），不重写
- [`hubx/rpcx` 与 `micro_apps/pkg/rpcx` 短期两份实现共存] → 本轮 launchx 只依赖 `hubx/rpcx`，micro_apps 保持现状不动；收敛开新 change，`hubx/rpcx` README 注明两者关系与迁移路径
- [组件 nil 降级态被调用方忽略] → godoc 标注降级态 + strict 开关 + 运行期 Warn 带后果说明
- [configx/nacos 合并语义与 micro_apps LoadMerged 漂移] → 复用同一语义实现并以三裁决规则单测锁定
- [mesh 清单自写而非 copy，与 `service_mesh/` 仓未来交付版本可能漂移] → 清单按 service_mesh 设计结论（业务零改动/sidecar 同 trace/地址段转换）编写并留 `README` 注明来源；`service_mesh/add-service-mesh-demo` 交付后若语义变化，开新 change 对齐，不在本变更内阻塞
- [本轮范围收敛后 observability / example-demos 部分 Scenario 无验收证据] → 本轮 close 只勾 A1–A3、B1（本地 + 远端不可达分支）、C1–C3、D1–D2、E1–E3；C4（真实部署部分）、E4–E5、O1–O3 明确保持未验收态，回补时开新 change。已写入 proposal「本轮验收案例集」，避免 close 时误判为遗漏
- [范围以两套编号表达易错] → 计划文件用 superpowers Task 编号、`tasks.md` 用 openspec 条目编号，二者非一一对应；凡涉及本轮边界的表述一律以 `tasks.md` 条目号为准，并在 proposal / design 中显式写出映射，避免照 Task 号切范围时漏掉验证条目
- [Jaeger 里"链路看起来有但其实是断的"] → 默认传播器为 noop 时，三个服务各产一根 trace，UI 上每条都能点开、都有 span，极像成功。故 T6 断言**共享同一 trace-id 且父子链正确**，而非"各服务都能查到 trace"；并配 T7 反向断言锁死红线
- [尾部数据层 span 缺失] → 退出序列的 `tracing.Shutdown`（15s 上限）必须早于 `logger.Close`；批量导出器（Batcher 512/5s）缓冲未冲刷即退出会静默丢 span，Jaeger 侧表现为最后一个 span 不见但不报错。T9 显式断言尾部节点不缺失
- [ES 底座缺失导致 T4 无法验收] → 本机无 Elasticsearch 容器，需以 `elasticsearch:7.6.2` 本地镜像新建（9200/9300）；若该镜像起不来，`search` 组件的装配契约仍以单测覆盖，T4 的 ES 部分降级为人工验证项并在 close 时标注
- [docker CLI 不在 PATH] → 本机 CLI 位于 `/Applications/Docker.app/Contents/Resources/bin/docker`（daemon v29.7.2 运行中），验收脚本 MUST 用绝对路径或先导出 PATH，MUST NOT 假定 `docker` 可直接调用

## Migration Plan

0. 跨仓前置：落地 `../coordx/consul` 修复（tasks 0.1）→ 新建 `../rpcx` 模块（tasks 0.2），二者均以 `replace` 指本地
0'. 底座前置（tasks 0.3）：启动/新建 Jaeger、MySQL、Kafka、Elasticsearch 容器（CLI 用绝对路径），并写入验收脚本 `examples/scripts/bring-up-trace.sh`
1. 库骨架 + 单测（无外部依赖）
2. `examples/monolith` 跑通 → `examples/micro`（bff/gateway 新建、srv 迁移）→ `examples/mesh`（**本轮后置**；届时仓内自写清单，MUST NOT copy `service_mesh/`）
3. 回退：全新仓库，无存量破坏；任一 demo 不达标可单独回退该目录

## Open Questions

- 无阻塞项（brainstorming 已收敛；mesh/micro demo 范围、模块命名、本轮交付边界、依赖选型、验收标准均经用户确认）
- 数据库选型：验收要求「单表与多表 CRUD、SQL 语句入链」。`mysql:8.4` 镜像本地有但容器已停止（3306），`postgres:16-alpine` 容器在跑（5432）——`dbx/orm` 两者都支持。当前按 **MySQL** 编写 demo（与 `micro_apps` 口径一致），若改为 Postgres 只需换 yaml 的 driver 与 dsn，组件契约不变
- Elasticsearch 版本：本地镜像为 `7.6.2`，`searchx/elasticsearch` 的客户端版本兼容性需实跑确认；不兼容时开新 change 处理 SDK 侧，不在本轮阻塞
- 后置项（`examples/mesh` 清单、`debug` 伪组件与 O1–O3、nacos 真机集成重验、trace 开关 OFF/ON 对比、`micro_apps/pkg/rpcx` 与 `gospacex/rpcx` 向 `hubx/rpcx` 收敛）的回补时机：待本轮 `tasks.md` 0.1–3.9 条目验收通过后另开 change 决定，不在本轮排期
- `hubx/rpcx` 是否需要承载 gRPC / 直连（direct）等非 kitex 传输：`micro_apps/pkg/rpcx` 只做 kitex，`gospacex/rpcx` 另有 `grpc`/`direct_client`；本轮新建版本只实现 kitex，其余按需开新 change
