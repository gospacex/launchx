# 调用链入链核对：单体 / 微服 / 服务网格

> 核对日期：2026-10-05　修订：第二轮（G1 / G2 / G3 已修复，见 §8）
> 范围：`examples/{monolith,micro,mesh}` + `examples/mall` 四微服
> 判据一句话：**三态的数据操作是否只经由 launchx 初始化好的 hubx SDK 句柄，
> 并由这些句柄自带的埋点把 span 挂进同一条 trace。**

## 0. 结论速览

| 形态 | 走 launchx 句柄 | 调用链入链 | 结论 |
|---|---|---|---|
| **单体**（monolith） | ✅ DB/Cache/KV/MQ/Search/Mongo/Tracer | ✅ 六类组件 + 业务分层 span 全部入链 | 合格 |
| **微服**（micro: srv/bff/gateway） | ✅ DB/KV/MQ/Registry + kitex Server/Client 选项 + otelhttp | ✅ 入站 Extract、出站 Inject、RPC 双边 suite、HTTP 出向 CLIENT span | 合格 |
| **服务网格**（mesh） | ✅ 与 micro 同一份二进制（零 Go 代码） | ✅ 应用侧同 micro；sidecar 侧由 Envoy + Telemetry API | 合格，1 处结构性限制（非缺陷） |
| **mall 四微服**（新） | ✅ DB/Cache/MQ/KV/Lock/Registry 全句柄 | ✅ 六类组件 + 出向事件 traceparent 透传 | 合格 |

原 §6 的 G1 / G2 / G3 已在本轮修复：**网关出向补了标准 HTTP CLIENT span；
两处限流计数从「读-改-写」迁到 kvx 原子 Incr；单体顺带接上 kvx 句柄。**
唯一保留登记项 G4（网格下 bff→srv 无 sidecar span）是 Kitex 私有协议 × Envoy
的结构性限制，不是缺陷。

**没有发现任何直连驱动的代码**：全仓检索 `redis.NewClient` / `sql.Open` /
`kafka.NewWriter` / `sarama.` / `mongo.Connect` / `elastic.New` / `consul.` 均无命中，
连接管理 100% 在 launchx 组件层。

## 1. 句柄来源与埋点开关（三态共用同一套机制）

`launchx` 的组件层把「构造成立」与「是否入链」分成两件事：配置里出现了对应段才构造
（presence-based），`trace.on.<name>` 只裁决**入链**。逐组件的开关落点：

| 组件 | 构造 | 入链裁决点 | SDK 侧埋点方式 |
|---|---|---|---|
| sqlx | `internal/component/db.go` | `db.go:24` `traceOn(env,"db")` | dbx/orm 的 gorm 回调（每条 SQL 一个 span） |
| cachex | `cache.go` | `cache.go:34-45` | `cachex.SetTracingConfig` 全局 + 实例级 |
| mqx | `mq.go` | `mq.go:29-30` | `cfg.EnableTrace && wantTrace` |
| kvx | `kv.go` | `kv.go:40-48` | `kvx.New` 对 `EnableTrace` 实例套 traceStore |
| lockx | `lock.go` | `lock.go:34` | 实例级 `EnableTrace` |
| searchx | `search.go` | `search.go:46` | 段级开关 ∧ 组件开关 |
| mongodbx | `mongodb.go` | `mongodb.go:29-30` | `mongodbx.SetTracingConfig` |
| coordx | `consul.go` | `consul.go:33` | 段级开关 ∧ `trace.on.registry` |

`traceflag` 是更上层的总闸：全局关时任何组件级 `true` 都不能绕过
（`component_test.go:265` 有反向断言）。

## 2. 单体形态（examples/monolith）

**句柄取用**（`app/setup.go`）：`a.DB()` :47 → `*gorm.DB`；`a.Cache()[name]` :71；
`a.KV()[name]` :81；`a.MQ()` :86；`a.Search()` :95；`a.Mongo()[name]` :105；
`a.Tracer()` :57。业务分层各自开 span：`repository.*`、`cache.*`、`service.*`、
`handler.*`。

**入链链路**：webx 的 otel 中间件 Extract 入站 traceparent → 业务 span →
各 SDK 句柄自带 span（redis / SQL / kafka / ES / mongo）挂在同一棵树下。
`config.yaml:29-36` 的 `trace.on` 覆盖 db / cache / kv / mq / search / mongodb，
与该示例实际用到的组件**一一对应**（没配 coordx —— 单体不注册，正确）。
这一一对应本身就是一条判据：配了却没人用的段，等于给自己埋一段「为什么
这类 span 消失了」的假故障；用了却没配 `trace.on` 的组件，则会在 Jaeger 上
静默缺席。

~~待改进~~ → **已修**：限流计数原在 `internal/cache/counter.go`（cachex 的
`Get → +1 → Set`，非原子，并发下计数互相覆盖）。现已抽出 `internal/ratelimit/counter.go`，
改走 `kvx.KVStore.Incr`（Redis 原子自增），装配层从 `a.KV()[name]` 取句柄。
同一步顺带解决了 G3：单体形态从此也接上了 kvx，与 micro/mall 的口径对齐。

## 3. 微服形态（examples/micro）

| 角色 | 句柄取用 | 跨进程续链落点 |
|---|---|---|
| **srv** | `a.DB()` :41、`a.MQ()` :58、`a.KV()[name]` :82、`a.RPCServerOptions()` :47 | 入站由 kitex ServerSuite Extract（`rpcx/options.go:133-135`） |
| **bff** | `a.RPCClientOptions(...)` :43、`a.Tracer()` :56 | 出站由 kitex ClientSuite Inject（`rpcx/options.go:156-158`） |
| **gateway** | `a.Registry()` :50、`a.Tracer()` → provider | 手工 `launchx.Inject`（`proxy/proxy.go:187`）+ otelhttp CLIENT span + 转发 span :142 |

关键点：`BuildClientOptions` 里 **suite 必须先于 `ClientSpanMiddleware` 注册**
（`options.go:145-148` 有实测说明），否则注入给下游的不是本包建好的 span。
开关来源统一是 `trace.enabled`（`internal/mode/webx.go:112`、`mode/srv.go:28`）。

~~待改进 1~~ → **已修（G1）**：网关出向 `http.Client` 原本是裸客户端，
只有自造的 `gateway.proxy.<route>` span，没有标准 HTTP CLIENT span。现在改成
`otelhttp.NewTransport(http.DefaultTransport, WithTracerProvider(tp),
WithPropagators(launchx.Propagator()))`（`proxy.go:141-149`），provider 由
`app/setup.go` 从 `a.Tracer().TracerProvider()` 传入 —— **必须与自造 span 同出
一个 provider**，否则 CLIENT span 会掉进全局 noop。

~~待改进 2~~ → **已修（G2）**：`micro/srv` 的限流器已从 cachex 迁到 `a.KV()`
（`middleware/ratelimit.go`），与 mall 保持同一写法。由于 demo-srv 除限流外不再
使用缓存，`config.yaml` 里是**整段替换**（cachex 段删、kvx 段加）而非追加 ——
留一段配了却没人用的组件，会让「为什么看不到 cache span」变成一个猜谜题。

## 4. 服务网格形态（examples/mesh）

**本目录没有一行 Go 代码，这不是遗漏，是结论**（`mesh/README.md` §开头）：
网格与微服共用同一份二进制，差异只在部署清单。因此第 3 节的全部结论对网格同样成立，
应用侧入链的证据就是 micro 的证据 —— **本轮 G1 / G2 的修复对网格同样自动生效**
（网关的 otelhttp CLIENT span、srv 的 kvx 计数），不需要动任何清单侧配置，
唯一要跟的是上面那处 srv 段的 `trace.on`。

清单侧的调用链配置：

- `configmap.yaml` 三份配置副本各带 `trace.enabled: true` 与 OTLP endpoint
  （gateway :44-48 / bff :80-84 / srv :109-116）；srv 段的 `trace.on` 覆盖
  db / kv / mq / registry，与其实际用到的组件一致（随 micro/srv 的 cachex→kvx
  替换同步改过，两边必须保持一致 —— 网格复制配置时最容易漏的就是这一处）。
- `telemetry.yaml` 下发 `randomSamplingPercentage: 100` —— Istio 默认采样率极低，
  是「看不到 sidecar span」的头号假阴性来源。
- `istio-operator.yaml` 的 provider 必须是 `opentelemetry`（发 W3C `traceparent`），
  选 `zipkin` 会发 B3 头，sidecar span 与应用 span 必然分成两条 trace。

**结构性限制（已登记为结论，非缺陷）**：bff→srv 走 Kitex 私有协议
（TTHeader over TCP），Envoy 按 `tcp_proxy` 处理，只看见连接看不见请求，
所以这一跳**没有 sidecar span**。网格本轮提供的是 mTLS + 连接级可观测，
L7 治理仍在应用侧（`mesh/README.md` §5）。

## 5. mall 四微服（本轮新增，作为"句柄纪律"的正例）

四服各自 `app/setup.go` 一律从 `a.DB() / a.Cache()[name] / a.MQ() / a.KV()[name] /
a.Lock() / a.RPCServerOptions()` 取句柄，跨域异步事件用 outbox 落库，
`traceparent` 随 mqx 消息 header 透传（`pkg/outbox/outbox.go:248-258`），
消费侧 Extract 续链 —— 这是"跨进程 + 跨异步边界"两头都能续的完整闭环。

## 6. 缺口清单（按建议优先级）

| # | 缺口 | 位置 | 处理 | 状态 |
|---|---|---|---|---|
| G1 | 网关出向无标准 HTTP CLIENT span | `micro/gateway/internal/proxy/proxy.go:141` | `otelhttp.NewTransport` 包 `http.DefaultTransport`，provider 取自 `a.Tracer()` | ✅ 已修 |
| G2 | 限流计数非原子（Get→+1→Set） | `micro/srv/internal/middleware/ratelimit.go`<br>`monolith/internal/ratelimit/counter.go` | 改用 `a.KV()` 的 `Incr` | ✅ 已修 |
| G3 | 单体形态未接入 kvx / lockx | `monolith/config.yaml` | 限流迁 kvx 时一并接上；lockx 业务仍未用到，保持不配 | ✅ 已收口 |
| G4 | 网格下 bff→srv 无 sidecar span | Kitex TCP 协议 × Envoy | 结构性限制 | ➖ 登记不改 |

新增依赖只有一条：`go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp`
（§8 说明为什么它是「零成本」的）。

## 7. 怎么自证（三态通用的反向判据）

1. 关掉某个 `trace.on.<name>`，Jaeger 上对应组件的 span **必须消失**；
   关了还在，说明埋点没接进统一开关。
2. `trace.enabled: false` 时全链路只剩业务自造 span，且它们应各自成孤儿 trace。
3. 网关不通 Inject（`launchx.Inject` 那行删掉）时，上游的 trace id 与网关不同 ——
   这就是断链的标准形态。
   ⚠️ 本轮加了 otelhttp 之后，这条判据**不再成立**：transport 内部同样会用
   `launchx.Propagator()` 注入 traceparent。要验证 Inject 本身，改为断言请求头
   在发出前已含 `traceparent`（单测 / tcpdump），而不是指望删代码后必然断链。
4. `launchx.PropagatorFields()` 必须含 `traceparent`；为空即传播器没注册，
   跨服务链路必断。
5. 网关出向必须有且仅有一层 HTTP CLIENT span（`HTTP GET` 之类），且它是
   `gateway.proxy.<route>` 的**子** span —— 缺这一层说明 otelhttp 没接上或
   provider 传错了（掉进全局 noop 的 span 不会上报）。

## 8. 本轮修复记录（第二轮）

### G1 —— 网关出向 CLIENT span

- `proxy.New` 签名从接 `trace.Tracer` 改为接 `trace.TracerProvider`：CLIENT span
  与转发 span 必须同 provider，否则前者落进全局 noop。
- transport 包的是 `http.DefaultTransport`（而不是自建裸 TCP transport）：连接复用、
  空闲连接回收、代理环境变量都留给标准实现，这一层只叠加观测职责。
- 注入保持双写：显式的 `launchx.Inject` 加上 transport 内部的注入。两者写同一个
  header、**同一个 span context**，结果幂等。保留显式那行是为了让「续链发生在哪里」
  在代码里可读，不依赖 transport 的内部实现（也因此判据 3 需要修订，见 §7）。
- **依赖代价为零**：`otelhttp@v0.70.0` 早就挂在现有依赖图里 —— 它是
  `github.com/grpc-ecosystem/grpc-gateway/v2@v2.30.0` 的间接依赖（grpc-gateway
  本就随 grpc/protobuf 链路进来），且它要求的 otel 下限是 v1.45.0，低于当前图的
  v1.46.0。所以引入它**没有升级任何既有 otel 组件**，只是把已在图里的模块
  提升为直接依赖。

  > 教训：别急着 `go get <module>@<version>`。第一次用 `go get otelhttp@v0.69.0`
  > 指定了比图中更低的版本，结果连带降级了 otlptracegrpc/http（v1.46.0→v1.45.0）
  > 与 grpc-gateway（v2.30.0→v2.29.0），还顺手删掉了 cachex 的 require。
  > 正确顺序是：先 `go mod graph | grep <module>` 看图里有没有、谁引入的，
  > 再决定要不要 go get。

### G2 / G3 —— 限流计数入 kvx

- 两处计数统一走 `kvx.KVStore.Incr`。窗口 rollover 靠 key 里带窗口序号实现；
  `Incr` 首建 key 时带不上 TTL，故在自增结果为 1 时用 `Set` 把 TTL 补上
  （此刻值必然是 1，覆写不丢计数）。不补，冷 key 会永久堆在 redis 里。
- 装配件必须**显式留痕**：计数缺失不会报错，只会不限流。micro/srv 与 monolith
  都在门控未命中时打 Warn；monolith 另给计数器加了 `Ready()`，让「装了没装」
  是个可判定状态，而不是靠判 nil 猜。
- cachex 与 kvx 的分工因此写死在四个地方一致：examples/mall 四服、micro/srv、
  monolith、以及本报告 —— kvx 管可自增状态，cachex 管可失效缓存。

改完的验证：`go build ./...` 零错误、`go vet` 干净、`go test ./...` 无 FAIL、
`gofmt -l` 无输出。
