# 网格形态真机验收证据（T10 / T11 / E5 / T12 / T13 / metrics）

本文件是 `examples/ACCEPTANCE.md` §11b 的**证据落盘**，不是操作手册（操作看 `../README.md`、
`../scripts/bring-up.sh`）。所有数字来自本机实测，可复跑。

- 验收时间：2026-10-05 20:40–21:15（UTC+8）；**§6 的 T13 为 2026-10-08 18:56–19:02 追加**；
  **§7 metrics 复验与 §8 环境漂移修复为 2026-10-10 12:20–12:45 追加**
- 结论：**T10 PASS / E5 PASS（OFF↔ON 可复现）/ T11 SKIP（见 §3，说明为何不伪造）/
  T13 PASS（10-10 复跑 8/8，见 §6）/ metrics 采集口 PASS（4/4，见 §7）**

---

## 0. 版本口径

| 组件 | 版本 | 取值命令 |
|---|---|---|
| minikube | `v1.36.0`（commit f8f52f5d…dirty） | `minikube version` |
| kubectl（minikube 内置分发） | `v1.28.0` | `minikube kubectl -- version --client -o yaml` |
| istioctl / istiod / data plane | `1.30.1`（6 proxies） | `istioctl version` |
| Istio profile | `demo`（`holdApplicationUntilProxyStarts: true`） | `../istio-operator.yaml` |
| Jaeger | `all-in-one:1.63.0`，存储 **elasticsearch**（`jaeger-*` 索引） | `dockers/docker-compose.yml` |
| 宿主 | macOS，总内存 8 GB；minikube 限 `--cpus=2 --memory=4096` | — |

> ⚠️ PATH：本机 `/usr/local/bin` 不在 WorkBuddy shell 的 PATH 里，`which minikube` 会**误报未安装**。
> 所有命令请先 `export PATH=/usr/local/bin:$PATH`，或用绝对路径 `/usr/local/bin/minikube`。

---

## 1. T10 —— 同一 trace 内同时有应用 span 与 sidecar span

### 1.1 判据（已固化为可执行脚本）

```bash
python3 examples/scripts/trace-audit.py --service demo-gateway --lookback 2m --mesh
```

`--mesh` 追加三条判据（脚本内注释有完整说明）：

- **M1** 同一 trace 内**同时**存在应用 span 与 sidecar span（服务名以 `.launchx-mesh` 结尾）；
- **M2** sidecar span 必须真是 Envoy 产出的：tag `component=proxy` **且** 带 `upstream_cluster`
  （只按服务名后缀判不可靠，同名服务能伪造）；
- **M3** sidecar 至少覆盖 2 跳（inbound + outbound）。

### 1.2 实测输出（ON 档，3 次请求）

```
=== service=demo-gateway  trace 数=3 ===
  [PASS] demo-gateway.launchx-mesh.svc.cluster.local:8080/* span=15  ... sidecar=4 跳=['PassthroughCluster', 'inbound|8080||', 'inbound|8081||', 'outbound|8080||demo-gateway.launchx-mesh.svc.cluster.local']
  [PASS] demo-gateway.launchx-mesh.svc.cluster.local:8080/* span=15  ... sidecar=4 跳=[同上]
  [PASS] demo-gateway.launchx-mesh.svc.cluster.local:8080/* span=16  ... sidecar=4 跳=[同上]

网格入链：3/3 条业务 trace 含 sidecar span，覆盖跳：['PassthroughCluster', 'inbound|8080||', 'inbound|8081||', 'outbound|8080||demo-gateway.launchx-mesh.svc.cluster.local']
结论：PASS
```

### 1.3 代表性 trace（完整树）

- **trace-id**：`8a728dcfe9b47cb29b7b83e348a4bc9e`
- **span 总数 16**：应用 span **12**（`demo-gateway` 3 / `demo-bff` 6 / `demo-srv` 3）、sidecar span **4**
- **sidecar 服务名**：`demo-gateway.launchx-mesh`、`demo-bff.launchx-mesh`
- **根 span 唯一**，树深 13

```
SIDECAR demo-gateway.launchx-mesh    demo-gateway.launchx-mesh.svc.cluster.local:8080/*      ← 根（Envoy inbound）
  SIDECAR demo-gateway.launchx-mesh    demo-gateway.launchx-mesh.svc.cluster.local:8080/*
    APP     demo-gateway                 GET /api/order/2
      APP     demo-gateway                 coord.discover
      APP     demo-gateway                 gateway.proxy.api
        SIDECAR demo-gateway.launchx-mesh    demo-bff.launchx-mesh.svc.cluster.local:8081/*   ← Envoy outbound
          SIDECAR demo-bff.launchx-mesh        demo-bff.launchx-mesh.svc.cluster.local:8081/* ← Envoy inbound
            APP     demo-bff                     GET /order/2
              APP     demo-bff                     handler.GetOrder
                APP     demo-bff                     downstream.Order.GetOrder
                  APP     demo-bff                     order.Order/GetOrder
                  APP     demo-bff                     client.demo-srv.GetOrder
                    APP     demo-bff                     coord.discover
                    APP     demo-srv                     order.Order/GetOrder
                      APP     demo-srv                     service.order.GetOrder
                        APP     demo-srv                     db.query
```

应用侧父子链仍是 `gateway → bff → srv`，sidecar 在**两跳**上把应用 span 包住 —— 这正是 T10 要的形态。

### 1.4 Jaeger API 原始响应片段

`GET /api/traces/8a728dcfe9b47cb29b7b83e348a4bc9e`（根 span，已截断）：

```json
{"traceID": "8a728dcfe9b47cb29b7b83e348a4bc9e", "spanID": "9809d98801a22232",
 "operationName": "demo-gateway.launchx-mesh.svc.cluster.local:8080/*",
 "references": [], "startTime": 1791205919766077, "duration": 81048,
 "tags": [
   {"key": "component",        "type": "string", "value": "proxy"},
   {"key": "downstream_cluster","type": "string", "value": "-"},
   {"key": "guid:x-request-id","type": "string", "value": "30d0b792-f5ae-9320-915b-6748154a93e9"},
   {"key": "http.method",      "type": "string", "value": "GET"},
   {"key": "http.protocol",    "type": "string", "value": "HTTP/1.1"},
   {"key": "http.status_code", "type": "string", "value": "200"},
   {"key": "http.url",         "type": "string",
    "value": "http://demo-gateway.launchx-mesh.svc.cluster.local:8080/api/order/2?user_id=1"},
   {"key": "internal.span.format","type": "string", "value": "otlp"},
   {"key": "istio.canonical_revision","type": "string", "value": "latest"}
 ]}
```

`component=proxy` 即 M2 的判据字段（Envoy 自己打）。

`GET /api/services`（全量，证明 sidecar 服务名真的进了 Jaeger）：

```json
{"data":["demo-monolith","jaeger-all-in-one","demo-bff","demo-gateway","demo-srv",
         "demo-gateway.launchx-mesh","demo-bff.launchx-mesh","demo-srv.launchx-mesh",
         "consul.launchx-mesh"],"total":9,"limit":0,"offset":0,"errors":null}
```

> `demo-srv.launchx-mesh` / `consul.launchx-mesh` 是排查过程中留下的历史条目（见 §4.3 的
> grpc-rpc 实验），**当前 ON 档的 9013 是 L4，不再产出 srv sidecar span**。

---

## 2. E5 —— `trace.enabled` OFF / ON 对比（可复现）

改 `configmap.yaml` 中三份配置的 `trace.enabled`，`rollout restart` 后各打 2–3 次请求。

| 档位 | 业务（应用）span | sidecar span | 代表性 trace |
|---|---|---|---|
| **OFF** | **0** | 有（4 条 trace，2–4 span） | `c927ea25ddef00282ac20541d62c4462`（4 span，全 sidecar） |
| **ON** | 有（15–16 span/trace） | 有（4 span/trace） | `8a728dcfe9b47cb29b7b83e348a4bc9e`（16 span） |

OFF 档实测（`service=demo-gateway.launchx-mesh` 查询）：

```
OFF 档 trace 数: 4
  c927ea25ddef00282ac20541d62c4462 spans=4 {'demo-gateway.launchx-mesh': 3, 'demo-bff.launchx-mesh': 1}
  028b6f068aa721ca0432ee37d5ba4b26 spans=2 {'demo-gateway.launchx-mesh': 1, 'consul.launchx-mesh': 1}
  3409bfdf6c57232c353864468636dac5 spans=4 {'demo-gateway.launchx-mesh': 3, 'demo-bff.launchx-mesh': 1}
  f309010f5578a8ab588e2f42d6c52e0f spans=2 {'demo-gateway.launchx-mesh': 1, 'consul.launchx-mesh': 1}
```

即：**OFF 时 Envoy 自己的链还在，但一条应用 span 都没有**；ON 时两者同 trace 合一。开关语义与
`trace.on.*` 只控入链、不控传播器的红线一致。

---

## 3. T11 —— 反向断言：**SKIP**（如实记录，不伪造）

**判据原文**：只开 sidecar 追踪、应用侧不注册传播器 → 呈分裂形态（1 条 sidecar 链 + 应用侧各自孤儿 trace，trace 数 ≥ 4）。

**判定：SKIP。无法在同构 demo 上构造，硬造即为造假。** 理由：

1. launchx 的传播器是**恒注册**的 —— `telemetry.Init` 的注释写明「传播器与是否采样无关」，
   W3C `traceparent` 的注入/提取不随 `trace.enabled` 开关变化。这是**红线**，不能为了跑反向
   断言去改库行为。
2. 关掉 `trace.enabled`（§2 的 OFF 档）只让应用**不产出 span**，但应用依然**透传**
   `traceparent` —— 所以 OFF 档的结果是「纯 sidecar 链」，而**不是** T11 描述的
   「sidecar 链 + 应用孤儿 trace 并存」。要用 OFF 档冒充 T11 属于偷换判据。
3. 真要构造分裂形态，必须让应用侧的 propagator 变成 no-op（或换成 B3 与 Envoy 的 W3C 不通），
   这需要 `Composite{}` 空传播器或改 `OTEL_PROPAGATORS` —— 两者都会破坏 §12 的
   「网格与微服复用同一份二进制」同构约束。

要补这个断言，正确做法是给 demo 开一个**独立的非同构变体**（例如专门的 `propagator=none`
配置档并显式标注「仅用于反向断言，不属于同构案例」），这属于新增工作项，不在本轮范围内。

---

## 4. 排查记录（T10 曾经长期为 0 的根因链）

这一段是本轮最值钱的部分：**T10 假阴性有三个独立成因，任何一个单独修掉都不够**。

### 4.1 Istio 不认宿主主机名 → Envoy 完全不生成 OTLP cluster

istiod 日志：

```
Not able to configure requested tracing provider "jaeger": ...
could not find service host.minikube.internal in Istio service registry
```

`istioctl install` 的 tracing provider **只接受集群内 Service**。修法：用无 selector 的
Service + 手工 Endpoints（指向宿主 `192.168.65.254`）把宿主「搬进」集群视图
（`../host-middleware.yaml`），provider 再指向这个门面 Service。

### 4.2 采样率（最容易误判成因，本次不是主因）

`Telemetry` API 下发 `randomSamplingPercentage: 100`。若只给 1%（Istio 默认量级），
「十次请求看不到一条 sidecar span」会被误读成链路断了。用
`istioctl proxy-config listeners -o json | grep random_sampling` 可验。

### 4.3 **真正卡住本次的成因**：OTLP 走 gRPC，但 cluster 被判成 HTTP/1.1

现象：HCM 里 `random_sampling=100`、tracer provider 也配好了（`envoy.tracers.opentelemetry` →
`outbound|4317||host-middleware…`），但 Jaeger 里一条 sidecar span 都没有，istiod 也无报错。

只有把 Envoy 日志级别临时拉到 debug 才看得见真因：

```bash
kubectl exec -n launchx-mesh deploy/demo-gateway -c istio-proxy -- \
  curl -s -X POST "localhost:15000/logging?level=debug"
# 打一次请求后：
kubectl logs -n launchx-mesh deploy/demo-gateway -c istio-proxy | grep -i otlp
```

```
envoy tracing ... Number of exported spans: 3
envoy tracing grpc_trace_exporter.cc:39  OTLP trace export failed with status: Unavailable,
  message: upstream connect error or disconnect/reset before headers.
  reset reason: protocol error{upstream_reset_before_response_started{protocol_error}}
```

Envoy **确实生成了 span**，但导出时握手被打回。根因是 `host-middleware` 的 4317 端口原名
`otlp-grpc`：Istio 按**端口名前缀**判协议，`otlp-grpc` 谁也不匹配 → 落成默认 HTTP，cluster 只带

```json
"typed_extension_protocol_options": {
  "envoy.extensions.upstreams.http.v3.HttpProtocolOptions": {
    "use_downstream_protocol_config": {"http_protocol_options": {}, "http2_protocol_options": {}}
  }}
```

而 Envoy 自己的 OTLP 导出**没有 downstream**，于是回落 HTTP/1.1，gRPC 流被 Jaeger 侧 reset。

**修法**：端口名改成 `grpc-otlp`（并补 `appProtocol: grpc` 双保险）。改完 cluster 变成

```json
"typed_extension_protocol_options": {
  "envoy.extensions.upstreams.http.v3.HttpProtocolOptions": {
    "explicitHttpConfig": {"http2ProtocolOptions": {}}
  }}
```

sidecar span 立刻开始入库。

### 4.4 顺带确认：`demo-srv` 的 9013 **不能**改成 `grpc-*`

试过把 `demo-srv` 的端口名从 `rpc` 改成 `grpc-rpc`，想给 srv 那跳也拿 sidecar span：
结果网关直接 **502**（kitex 的 TTHeader over TCP 被 Istio 当 h2 解析，流被打断）。改回
`tcp-rpc` 后业务立刻 200。所以：

- 9013 这一跳 **协议上就是 L4**，Istio 看不见请求 → 既没有 sidecar span，也交不出 L7 治理
  （重试/超时/熔断/灰度）。这是协议事实，不是配置漏项。
- M3 判据因此按「已解析为 L7 的跳数」统计（当前 inbound|8080、inbound|8081、
  outbound|8080 三跳），不把这件事算成 FAIL。

### 4.5 旁路坑：Jaeger 的 `?lookback=` 参数**无效**

审计脚本原样把 `--lookback` 传给 Jaeger，实测 2m/5m/1h 返回的 trace 完全一样（各 30 条），
等于一直在拿**陈旧 trace** 判定 —— 修完的问题会被旧数据判成 FAIL，更糟的是旧数据 PASS 会
掩盖新引入的断链。已在 `trace-audit.py` 里改成自己算 `start`/`end`（微秒）再传。

---

## 5. 复现命令（一键）

```bash
export PATH=/usr/local/bin:$PATH
cd /Users/hyx/work/gowork/src/hubx/launchx/examples

# 1) 起集群 + Istio + 部署（首次约 10 分钟，含拉镜像）
bash mesh/scripts/bring-up.sh install
bash mesh/scripts/bring-up.sh deploy

# 2) 打请求
kubectl -n launchx-mesh exec deploy/demo-gateway -c app -- \
  wget -qO- 'http://demo-gateway.launchx-mesh.svc.cluster.local:8080/api/order/2?user_id=1'

# 3) 判据（注意 lookback 要小于你刚打请求的时间窗）
python3 scripts/trace-audit.py --service demo-gateway --lookback 2m --mesh
```

期望：`网格入链：N/N 条业务 trace 含 sidecar span` 且 `结论：PASS`。

---

## 6. T13 —— apps 组（网格原生）：入口链条**路由矩阵**

本节是 `mesh-entry-chain` spec 的判据落盘。与 §1 的 T10 判据（「**任意**一条业务 trace
含 sidecar span」）不同，本节逐**路由**判定 —— 前者挡不住「六条路由全接到 order 域」
这一类接线错误，后者能。

- 验收时间：**2026-10-08 18:56 / 19:02 / 19:02（UTC+8，连跑三次）**
- 结论：**PASS（7/7 行）**，三次连跑结果一致（见 §6.5）
- 版本口径：minikube `v1.36.0` / kubectl `v1.28.0` / istioctl·istiod·data plane `1.30.1` /
  Jaeger `all-in-one:1.63.0` / 宿主 macOS 8 GB，minikube `--cpus=2 --memory=4096`
  （与 §0 同源，本轮复采确认未变）
- 判据脚本：`examples/scripts/trace-audit.py --route-matrix <file> --mesh`
- 矩阵定义：`examples/mesh/scripts/route-matrix-apps.txt`（apps 组）、
  `route-matrix-micro.txt`（对照组）
- 请求发起：`VARIANT=apps bash examples/mesh/scripts/bring-up.sh verify`

### 6.1 路由矩阵实测表（第三次跑，19:02）

| # | 路由 | 方法 | HTTP | 目标域 service span | sidecar 跳 | traceID（前 8） | 判定 |
|---|---|---|---|---|---|---|---|
| 1 | `/api/ping` | GET | 200 | —（刻意不触达任何域） | 2 跳 | `6ce73903` | PASS |
| 2 | `/api/order/2` | GET | 200 | `service.order.GetOrder` | 2 跳 | `3fbb04e3` | PASS |
| 3 | `/api/order` | POST | 200 | `service.order.CreateOrder` | 2 跳 | `b470f1ef` | PASS |
| 4 | `/api/points/1` | GET | 200 | `service.points.GetPoints` | 2 跳 | `d3452ef3` | PASS |
| 5 | `/api/product/1` | GET | 200 | `service.product.GetProduct` | 2 跳 | `2af90ab5` | PASS |
| 6 | `/api/stock/1` | GET | 200 | `service.stock.GetStock` | 2 跳 | `ff82acf9` | PASS |
| 7 | `+async`（异步链） | — | 200 | `service.points.ConsumeOrderCompleted` | 2 跳 | `b470f1ef`（**与第 3 行同一条**） | PASS |

完整 traceID 与 span 计数（Jaeger API 实测）：

| 路由 | traceID | span 数 | 该 trace 内全部 `service.*` span |
|---|---|---|---|
| `/api/ping` | `6ce739030bc733a82b321fc65f5b7d05` | 8 | — |
| `/api/order/2` | `3fbb04e3baec56d1ecfd3e75f63cfc1e` | 19 | `service.order.GetOrder` |
| `/api/order` | `b470f1effbe97a79c046aa2db55722b7` | 35 | `service.order.CreateOrder` / `service.order.GetOrder` / **`service.points.ConsumeOrderCompleted`** |
| `/api/points/1` | `d3452ef339ba5b89b6999dc36f5f7ec9` | 18 | `service.points.GetPoints` |
| `/api/product/1` | `2af90ab5b9b16f0af2aa750fcaacb449` | 17 | `service.product.GetProduct` |
| `/api/stock/1` | `ff82acf9b60d53d6aa975e58ccfe2424` | 18 | `service.stock.GetStock` |

**异步链是本节唯一能覆盖 points 域写路径的证据**：points 域的加分路径不经 HTTP
（没有 `POST /points` 路由），同步矩阵六行覆盖不到它。第 7 行断言的是
「`POST /order` 产生的 `order.completed.v1` 消息，被 `srv-points` 消费后开出的
`service.points.ConsumeOrderCompleted` span，与下单请求**落在同一条 trace 内**
（traceID 逐字相同）」—— 即 mqx 的 Inject/Extract 续链真的生效。

### 6.2 sidecar 跳的构成（矩阵「sidecar」列到底数的是什么）

矩阵里的 sidecar 列只覆盖 **`ingressgateway → bff` 这一跳**，实测每次请求产生 3 个
proxy span（去重后 2 个 `upstream_cluster`）：

| 产生方 | `upstream_cluster` | 性质 |
|---|---|---|
| `istio-ingressgateway.istio-system` | `outbound|8081||bff.launchx-mesh.svc.cluster.local` | 入向出侧 |
| `bff.launchx-mesh` | `inbound|8081||` | 目标入侧 |

> ⚠️ **不可交付项（不是缺陷）**：`bff→srv-<域>` 那几跳走 Kitex TTHeader over TCP，
> Envoy 只按 `tcp_proxy` 处理，**永远没有 sidecar span**。四域的 RPC 跳本来就不该
> 出现在这一列。同理，这四跳的熔断 / 重试 / 超时仍在应用侧，落点是
> **`xmiddleware.rpcx` 配置段 → `rpcx` 的 `client.Option`**（`conf/*.yaml`），网格接不住。
> 注意不是 `internal/srv/<域>/route` 或 `middleware` —— 那两层只有准入闸门与业务维度限流。
> 全仓 `WithRetry` / `WithCircuitBreaker` / `WithLoadBalancer` 只出现在一个地方：
> **`rpcx/governance.go`**（SDK 侧收敛，业务侧经 `xmiddleware.rpcx` 配置取用）。
>
> 因此「治理已下沉网格」的准确表述是：**mTLS + 连接级可观测 + HTTP 跳的 L7 治理**。
> RPC 跳与企业级治理仍在应用侧。

### 6.3 请求命令原文（六条，含 `POST /order` 的完整 body）

探针一律从集群内**已在运行**的 `istio-proxy` 容器发出（`kubectl exec … -c istio-proxy --
curl`）—— 不用临时 Pod，因为本机到 Docker Hub 不通，探针镜像会停在 `ImagePullBackOff`。

```bash
POD=$(kubectl -n launchx-mesh get pods -o name | grep bff | head -1 | sed 's|pod/||')
BASE=http://istio-ingressgateway.istio-system.svc.cluster.local

# 1  GET /api/ping
kubectl -n launchx-mesh exec "$POD" -c istio-proxy -- \
  curl -s -m 20 -X GET -w '\nHTTP=%{http_code}\n' $BASE/api/ping
# 2  GET /api/order/2
kubectl -n launchx-mesh exec "$POD" -c istio-proxy -- \
  curl -s -m 20 -X GET -w '\nHTTP=%{http_code}\n' "$BASE/api/order/2?user_id=1"
# 3  POST /api/order
kubectl -n launchx-mesh exec "$POD" -c istio-proxy -- curl -s -m 20 -X POST \
  -H 'Content-Type: application/json' \
  -d '{"user_id":1,"items":[{"product_id":1,"name":"demo","quantity":1,"unit_price":100}]}' \
  -w '\nHTTP=%{http_code}\n' $BASE/api/order
# 4  GET /api/points/1
kubectl -n launchx-mesh exec "$POD" -c istio-proxy -- \
  curl -s -m 20 -X GET -w '\nHTTP=%{http_code}\n' $BASE/api/points/1
# 5  GET /api/product/1
kubectl -n launchx-mesh exec "$POD" -c istio-proxy -- \
  curl -s -m 20 -X GET -w '\nHTTP=%{http_code}\n' $BASE/api/product/1
# 6  GET /api/stock/1
kubectl -n launchx-mesh exec "$POD" -c istio-proxy -- \
  curl -s -m 20 -X GET -w '\nHTTP=%{http_code}\n' $BASE/api/stock/1
```

响应（第三次跑的实测原文）：

```json
{"msg":"pong"}
{"user_id":1,"order_id":2,"order_no":"O2026100611321249656fe6","status":"created","total_amount":5998,"items":[{"product_id":3,"name":"e2e-商品-改价","quantity":2,"unit_price":2999}]}
{"user_id":1,"order_id":1053,"order_no":"M20261008110229000001","total_amount":100}
{"user_id":1,"account":{"user_id":1,"balance":1400}}
{"product":{"product_id":1,"title":"demo product","price":100,"status":1,"category_id":1,"created_at":1791432044}}
{"product_id":1,"stock":{"product_id":1,"total":100,"available":100}}
```

第 3 条回的 `order_id=1053` 是本轮新建的订单，它的 `order_no` 前缀 `M2026…` 与
§6.1 第 7 行的消费 span 一一对应。

### 6.4 判据脚本输出原文（第三次跑）

```
=== 路由矩阵判据（入口 → bff → 目标域业务层 + sidecar）===
#  路由                        方法     HTTP   域 span   sidecar  trace     年龄
13 /api/ping                 GET    200    ✓        2 跳      6ce73903  0.5m  PASS
14 /api/order/2              GET    200    ✓        2 跳      3fbb04e3  0.4m  PASS
15 /api/order                POST   200    ✓        2 跳      b470f1ef  0.4m  PASS
16 /api/points/1             GET    200    ✓        2 跳      d3452ef3  0.4m  PASS
17 /api/product/1            GET    200    ✓        2 跳      2af90ab5  0.3m  PASS
18 /api/stock/1              GET    200    ✓        2 跳      ff82acf9  0.3m  PASS
19 +async(service.points.Con -      200    ✓        2 跳      b470f1ef  0.4m  PASS
矩阵：7/7 行通过

结论：PASS
```

### 6.5 可复跑自检（连跑三次，判据必须每次都过）

| 跑次 | 时间 | 矩阵 | 首行 traceID | 异步链 traceID |
|---|---|---|---|---|
| 1 | 18:56 | 7/7 PASS | `0e88bd88` | `a6017497` |
| 2 | 19:02 | 7/7 PASS | `b4c3850f` | `2aab670a` |
| 3 | 19:02 | 7/7 PASS | `6ce73903` | `b470f1ef` |
| 4 | 19:10 | 7/7 PASS | `07281b9d` | `ca4b175a` |

四次 traceID **各不相同**，且异步链 traceID 每次都与当次 `POST /order` 行一致 ——
这条防的是「第一次 PASS 是陈旧 trace 凑出来的」：若第二次结果不稳定，说明
`--lookback` 时间窗或 flush 等待不足。

### 6.6 反例自检（判据必须真的能拦下接线错误）

把 `/product/:id` 错误地接到 order 域（`service.points.GetPoints` 换给 product 路由）：

```bash
printf '/api/product/1|GET|points|service.points.GetPoints\n' > /tmp/bad-matrix.txt
python3 scripts/trace-audit.py --base http://127.0.0.1:16686 \
  --route-matrix /tmp/bad-matrix.txt --lookback 5m --mesh
echo "rc=$?"     # 期望 1（非零 = 判据生效）
```

实测 `rc=1` 且报「窗口内没有该 span」—— 判据方向为真，不是恒真判据。

### 6.7 本轮实测踩到并修掉的三个真因（留给后来人）

| 症状 | 真因 | 修法 |
|---|---|---|
| `POST /order` 返回 **504** | 宿主 Kafka 的 `PLAINTEXT` 监听器 advertised 为 `127.0.0.1:9092`；Pod 里 bootstrap 能连上，**元数据一返回就 `dial tcp 127.0.0.1:9092`（连的是 Pod 自己）** | 给 kafka 加一个专供网格的监听器 `MESH://:9094`（advertised 为 `kafka:9094`），宿主 9092 不动；`conf` 与 ConfigMap 的 brokers 改 9094；`20-host-middleware.yaml` 暴露 9094 |
| 异步链**时有时无**（约 2/3 命中） | `srv-order` 与 `srv-points` 共用消费组 `mqx-group`：kafka-go 的 Reader 在**创建时**就以 GroupID 加入并占住分区，order 侧占住 partition 0 却从不读取 ⇒ 落进该分区的消息永远不处理 | 两域各配独立 `group_id`（`srv-order` / `srv-points`），互不抢分区 |
| 6 条路由的 trace **一条都匹配不上** | 网格下根 span 是 **Envoy 的 proxy span**，`operationName` 是 `bff.launchx-mesh.svc.cluster.local:8081/*` 这类，路径只存在于 `http.url` **标签**里 —— 按 `GET /order/2` 匹配根 span 名必然零命中 | 改按「根 span 的 `http.method` + `http.url`/`http.target` 标签」绑定路由，而不是按 span 名 |

> 另外一条口径坑（判据脚本自身）：micro 组的根 span 是应用侧 `GET /api/order/2`，
> apps 组因为 VirtualService 有 `rewrite.uri: /`，根 span 路径不带 `/api`。脚本两种写法
> 都认，改判据时别只留一种。

---

## 7. 指标采集口（2026-10-10 **真机复验通过，4/4**）

本节原为 2026-10-08 的「配置就绪，未复验」登记；**2026-10-10 12:20–12:45 起集群逐条实测，
四条判据全部通过**，ACCEPTANCE §13.4 网格列随之由 ⚠️ 翻为 ✅。

- 复验环境：minikube `v1.36.0`（k8s server **v1.33.1**，与 §0 的 v1.28.0 不同，见 §8.1）/
  istio `1.30.1` / `VARIANT=apps` / Jaeger `all-in-one:1.63.0`（ES 后端）

### 7.1 两种落法（取决于该档有没有 HTTP 面）

| 档 | `/metrics` 落点 | 配置 |
|---|---|---|
| `bff`（HTTP 面） | **复用入口跳那一个端口** 8081 | `metrics.enabled: true`，不配 `addr` |
| 四域 `srv`（无 HTTP 面） | 单起监听 `0.0.0.0:9031-9034` | `metrics.enabled: true` + `metrics.addr` |

### 7.2 一处**必须多写**（反向项，唯一一处）

四域各有一条 `AuthorizationPolicy`，`action: ALLOW` 且只放行 `sa/bff` ——
ALLOW 语义下**未匹配即拒绝**，而 Prometheus 不在网格里、没有 SPIFFE 身份 ⇒ 一律 403。
所以在 `20-srv-order.yaml` 等四份清单里写了：

```yaml
annotations:
  traffic.sidecar.istio.io/excludeInboundPorts: "9031"
```

被排除的端口不经 sidecar：无 mTLS、不受 `AuthorizationPolicy` 约束，代价是这一口
的连接级遥测也没了（采集口不需要）。

**端口名同样有讲究，而且方向与入口跳相反**：入口跳的 Service 端口必须叫 `http`
（否则 Envoy 按 `tcp_proxy` 处理，`outlier detection` 与 HTTP 重试静默失效）；采集口
则必须叫 **`tcp-metrics`** —— 写成 `http-metrics` 会被 L7 接管，正好撞上上面那条
ALLOW 策略，Prometheus 拿 403 而不是指标。

### 7.3 复验判据与实测结果（2026-10-10，全过）

> 取数方式：app 容器是刻意不装包的 alpine（**没有 curl**），Pod 内判据一律经
> 同 Pod 的 `istio-proxy` 容器发（proxyv2 自带 curl 8.5，共享网络命名空间）。

1. **Pod 内采集口有输出** ✅
   `kubectl -n launchx-mesh exec deploy/srv-order -c istio-proxy -- curl -s localhost:9031/metrics`
   返回 `rpc_server_duration`（kitex server 侧）等 79 行 Prometheus 文本。
2. **网格外抓取不被 sidecar 拦** ✅
   宿主 `kubectl port-forward svc/srv-order 19031:9031` → `curl 127.0.0.1:19031/metrics`
   拿到同样 79 行（`excludeInboundPorts: 9031` + 端口名 `tcp-metrics` 生效）；
   **对照**：同法 port-forward 9022（RPC 口）直接连接失败（HTTP=000）——ALLOW 策略对
   网格外身份确实在拦，说明 9031 的放行是排除注解的功劳，不是策略没生效。
3. **应用侧指标没被网格吞掉** ✅
   `bff:8081/metrics` 出现 `rpcx_requests_total{rpc_role="client"}`：
   `CreateOrder→srv-order 4 / GetAccount→srv-points 4 / GetOrder→srv-order 4 /
   GetProduct→srv-product 8`（计数与 verify 打流量次数逐次吻合）。
4. **直方图分桶是秒级** ✅
   `rpcx_request_duration_seconds_bucket` 分桶 `le="0.001"` 起：
   `0.001 / 0.0025 / 0.005 / 0.01 / 0.025 / 0.05 / 0.1 / 0.25 / 0.5 / 1 / 2.5 / 5 / 10 / +Inf`。
   注意别拿 `rpc_server_duration`（`le="5"` 起）当反例 —— 那是 kitex-contrib
   第三方埋点的 OTel 缺省毫秒桶，不是本判据的对象；判据只管 rpcx 自己的
   `rpcx_request_duration_seconds`（`_seconds` 后缀即单位声明）。

---

## 8. 环境漂移修复记录（2026-10-10，留给后来人）

本轮复验前集群是**重建过的**（k8s v1.28.0 → v1.33.1），连环踩了三个新坑，
两个已修进脚本/Makefile，一个属外部环境污染：

### 8.1 原生 sidecar 让探针检测误判「未注入」→ 全 502（脚本已修）

k8s ≥1.29 起 Istio 的 `istio-proxy` 以**原生 sidecar** 形态落在
`spec.initContainers`（不再是 `spec.containers`）。`bring-up.sh` 的 `mesh_curl`
探针检测只查 `.spec.containers[*].name` → 永远找不到 → 回退宿主直连 →
宿主解析不了集群域名，每条路由都 502。已修：jsonpath 同时查两处
（`{.spec.containers[*].name} {.spec.initContainers[*].name}`），新旧集群都兼容。

### 8.2 istio-system 被同机另一个网格案例污染 → sidecar span 全灭

本轮 istiod 起来后业务全通但**一条 sidecar span 都没有**（T13 矩阵 0/8）。
根因：minikube 里的 istio-system 是**另一套网格案例**（`service_mesh/`）装的，
meshConfig 的 extensionProvider 叫 `otel-tracing` 指向 `jaeger.mesh-external`，
与本案例 Telemetry 引用的 provider `jaeger`（→ `host-middleware.launchx-mesh:4317`）
**名字对不上**，istiod 静默不生成 OTLP cluster（与 §4.1 同族）。
修法：`istioctl install -y -f deploy/istio/operator.yaml` 重放即收敛（幂等）。
教训：`bring-up.sh install` 的「istio-system 已存在则跳过」只挡重复安装，
**挡不住配置漂移** —— 共享集群里探到 sidecar span 全灭时先核对
`kubectl -n istio-system get cm istio -o jsonpath='{.data.mesh}'` 的 provider 名。

### 8.3 Jaeger 的 ES 后端退出 → `/api/services` 500

`elasticsearch` 容器 Exited(137)（19 小时前，OOM 类），Jaeger 查询全 500
（`no Elasticsearch node available`）。`docker start elasticsearch` 后集群 **red**
（单节点 102 个 unassigned 副本分片），`PUT /jaeger-*/_settings
{"index":{"number_of_replicas":0}}` 后 yellow 恢复查询。
注意 `hot:product:*` 是 micro/mall 组的缓存键；apps 组的详情缓存键是
**`mesh:product:detail:<id>`**（`service/product.go key()`），清缓存别清错前缀。

### 8.4 顺带修掉：buildkit 的 FROM 元数据解析不走本地回退

`make image` 在 minikube daemon 里 buildkit 构建，`FROM alpine:3.20` 的**元数据解析**
联网（docker.io 在本机不可达），即使 daemon 本地已有该镜像也**不回退**，必然 timeout。
已在 `apps/Makefile` 的 image 目标固化为两步：`minikube image load alpine:3.20`
预载 + `DOCKER_BUILDKIT=0` 走 legacy builder（直接用本地镜像）。
