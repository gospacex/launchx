# 网格形态真机验收证据（T10 / T11 / E5 / T12）

本文件是 `examples/ACCEPTANCE.md` §11b 的**证据落盘**，不是操作手册（操作看 `../README.md`、
`../scripts/bring-up.sh`）。所有数字来自本机实测，可复跑。

- 验收时间：2026-10-05 20:40–21:15（UTC+8）
- 结论：**T10 PASS / E5 PASS（OFF↔ON 可复现）/ T11 SKIP（见 §3，说明为何不伪造）**

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
