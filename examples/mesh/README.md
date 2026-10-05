# examples/mesh —— 服务网格形态（验收案例 E4 / E5、T10–T12）

**本目录没有一行 Go 代码，这不是遗漏，是结论。**

网格形态与微服形态共用同一份二进制（同一个镜像 `launchx-demo:dev`，三个 Deployment
只是用不同 `command` 启动同一个镜像里的三个二进制）。网格带来的差异全部落在
**部署面**：监听地址、注册中心位置、配置载体。一旦这里出现 `main.go`，
就说明「为网格 fork 了代码」，C4/T8 的论证当场失效。

## 1. 目录

```
examples/mesh/
├── namespace.yaml        # launchx-mesh + istio-injection=enabled
├── consul.yaml           # consul 部署进集群（见 §4，这是必须的）
├── configmap.yaml        # 三份配置副本（gateway/bff/srv）
├── gateway.yaml          # 3 × (Service + Deployment)，tcpSocket 探针
├── bff.yaml
├── srv.yaml
├── istio-operator.yaml   # istioctl install -f 的入参（OTel tracing provider）
├── telemetry.yaml        # 采样率 100%（Istio 默认极低，是 T10 假阴性头号来源）
├── scripts/bring-up.sh   # check / image / install / deploy / verify / status / down
├── docs/verify.md        # T12 证据落盘
└── README.md
```

## 2. 一条命令跑通

```bash
export PATH=/usr/local/bin:$PATH          # 本机 minikube/istioctl 都在这里，默认 PATH 没有
cd /Users/hyx/work/gowork/src/hubx/launchx/examples

./mesh/scripts/bring-up.sh check          # 前置 + 清单 dry-run + 地址自检
./mesh/scripts/bring-up.sh image          # 交叉编译 → build 进 minikube daemon
./mesh/scripts/bring-up.sh install        # minikube start(2C4G) + istioctl install + telemetry
./mesh/scripts/bring-up.sh deploy         # apply → 等 rollout
./mesh/scripts/bring-up.sh verify         # 集群内发一次全链路请求 + 打印 Jaeger services
```

**资源前提（本机实测，别忽略）**：宿主总内存 8 GB。要先把与 launchx 无关的中间件
停掉，否则 minikube 的 Pod 会反复 OOM/Evicted：

```bash
D=/Applications/Docker.app/Contents/Resources/bin/docker
$D stop apollo rocketmq-namesrv rocketmq-broker rabbitmq nsqlookupd nsqd nsqadmin \
        chroma rustfs keydb memcached etcdkeeper postgres
# 验收完再 $D start <同上> 恢复
```

## 3. 地址换算（网格形态最易错的一段）

| micro 形态（宿主） | mesh 形态（集群内） | 原因 |
|---|---|---|
| `127.0.0.1:8080` | `0.0.0.0:8080` | 绑回环 → Pod 外不可达，最经典的坑 |
| `127.0.0.1:8081` | `0.0.0.0:8081` | 同上 |
| `127.0.0.1:9013` | `0.0.0.0:9013` | 同上 |
| `127.0.0.1:3306` | `host.minikube.internal:3306` | mysql 在宿主 compose |
| `127.0.0.1:6379` | `host.minikube.internal:6379` | redis |
| `127.0.0.1:9092` | `host.minikube.internal:9092` | kafka（见 §6 的坑） |
| `127.0.0.1:4318` | `host.minikube.internal:4318` | jaeger OTLP/HTTP |
| `127.0.0.1:8500` | `consul.launchx-mesh.svc.cluster.local:8500` | consul **进集群**，见 §4 |
| 自身注册地址 = 监听地址 | `${POD_IP}:<port>` | Downward API 注入，configsrc 做 os.ExpandEnv |

`host.minikube.internal` 是 minikube 给 **Pod** 的「回宿主」名字。
`host.docker.internal` 是给**宿主容器**用的，两者不通用。

## 4. 为什么 consul 必须部署进集群

Pod 注册进 consul 的自身地址是 **Pod IP（10.244.x.x）**。如果 consul 还在宿主
compose 里，它做主动健康检查时打这个地址 —— 宿主根本路由不到 minikube 的内部
Pod 网络，于是**所有实例一律被判不健康**，发现侧永远拿不到实例，症状是
「三个 Pod 都 Running 2/2，一调链路就 502」。把 consul 放进集群，健康检查的
源和目标同处一张 Pod 网络，问题消失。

代码面零改动即可完成这次迁移：`xmiddleware.coordx.addr` 本来就是配置项。

**已登记的副作用**：注册表从此有两份 —— 集群内这份是网格形态的事实来源，
宿主那份只服务 micro 形态，不要让同一批服务同时写两边。

## 5. 交不出去的 L7 治理（结论，不是待办）

rpcx 走 Kitex 默认的 **TTHeader over TCP**（私有协议），Istio 对未声明 HTTP 的
端口按 TCP 处理（`tcp_proxy`）—— **只看得见连接，看不见请求**。于是：

| 能力 | 网格里可用？ | 说明 |
|---|---|---|
| mTLS / L4 授权 | ✅ | SPIFFE 身份，与本议题无关 |
| 连接级可观测 | ✅ | 但 bff→srv 这一跳**没有 sidecar span**（同根因） |
| 异常实例剔除 | ✅（需二选一） | 与应用侧重试/熔断别双开 |
| 按请求重试 / 超时 / 熔断 | ❌ | 要按 HTTP 语义读请求 |
| 按 path/header 灰度、限流、JWT | ❌ | 同上 |
| 配置分发 | ❌ | 网格不管配置；ConfigMap 也没有长轮询热更新 |

所以正确表述是：**网格本轮提供 mTLS + 连接级可观测，L7 治理仍在应用侧**。
写成「治理已下沉网格」就是虚假陈述（ACCEPTANCE 陷阱 15）。

## 6. 排查表（本机实测踩过的）

| 症状 | 根因 | 处理 |
|---|---|---|
| Pod `ImagePullBackOff` | minikube 有独立 daemon，看不到宿主镜像 | 用 `minikube docker-env` 直接 build，或 `minikube image load` |
| 容器 `exec format error` | 交叉编译 GOARCH 写错（宿主是 **x86_64**） | `GOOS=linux GOARCH=amd64` |
| 链路 502，Pod 全 Running | consul 在宿主 → Pod IP 健康检查不可达 | consul 进集群（本目录 `consul.yaml`） |
| Jaeger 里看不到 sidecar span | 采样率太低（Istio 默认非 100%） | `telemetry.yaml` 下发 100% |
| sidecar span 与应用 span 分两条 trace | 用了 `zipkin` provider（发 B3 头） | 必须 `opentelemetry` provider（发 W3C `traceparent`） |
| `istioctl install --set ...tracing.sampling` 直接 panic | 1.30 的 `tracing` 是数组，与 map 语义写路径冲突 | 用 `-f istio-operator.yaml` + Telemetry API |
| bff→srv 这一跳没有 sidecar span | Kitex 私有协议经 `tcp_proxy`，Envoy 看不见请求 | **不是 bug**，见 §5；T10 分两段断言 |

## 7. 验收落点

| 案例 | 判据 | 证据 |
|---|---|---|
| **T10** | 同一 trace 下同时有应用 span（demo-gateway/bff/srv）与 sidecar span（`<workload>.launchx-mesh`），应用父子链仍是 gateway→bff→srv | `docs/verify.md` §T10 |
| **T11** | 反向断言：只开 sidecar 追踪、应用侧不注册传播器时，出现「1 条 sidecar 链 + 应用侧孤儿 trace（≥4 条）」的分裂形态 | `docs/verify.md` §T11 |
| **T12** | trace-id / span 数（应用 vs sidecar）/ sidecar 服务名 / Jaeger API 原始片段 / 版本号 全部落盘 | `docs/verify.md` |
| **E4/E5** | 部署成功 + 全链路通；证据同上 | — |
