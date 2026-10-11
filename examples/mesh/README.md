# examples/mesh —— 服务网格形态

本目录同时承载**两条并存的证据链**：

| 组 | 代码 | 清单 | 镜像 | 论证 |
|---|---|---|---|---|
| **对照组** | 无（复用 `examples/micro`） | `deploy/micro/` | `launchx-demo:dev` | C4/T8「换形态不改代码」—— 与 micro 共用同一份二进制，差异全在部署面 |
| **原生组** | `apps/`（独立 module，**无 gateway 进程**） | `deploy/apps/` | `launchx-mesh-apps:dev` | 「为网格写的应用有多瘦」—— 入口转发整块交 Istio，减法清单见 `apps/README.md` §4 |

> **⚠️ 对照组现状：已失效，暂不可用（2026-10-10 实测）**。上表「与 micro 共用同一份二进制、
> 差异全在部署面」这条，**对 gateway/srv 成立、对 bff 不成立**：
> - `deploy/micro/10-configmap.yaml` 的 `bff` 块比 `examples/micro/bff/config.yaml` **少 18 个叶子键**
>   （缺整段 `auth`/`demo`、缺 `xmiddleware.cachex`、缺 `service.upstream.demo-{order,points,product,stock}`，
>   并残留已废弃的 `service.upstream.demo-srv`）；`gateway`/`srv` 两块与源 0 漂移。
> - 而 `bring-up.sh` 在 `VARIANT=micro` 下用**当前源码**重建二进制，当前 bff 在 `demo.auth_token`
>   为空时**启动即失败**（`examples/micro/bff/app/setup.go:179`）⇒ `VARIANT=micro` 起不来。
> - 拓扑也已落后：`deploy/micro/` 仍是**单 `srv`** 的 3 进程，微服现为 `bff` + 4×`srv-*` 五进程，
>   两者**不再同构**。
>
> **可跑通并满足验收的是原生组（`VARIANT=apps`，默认）**。对照组的取舍与跟进建议见
> `docs/code-review-2026-10-10.md` §9.10（要么升到五进程重新同构，要么整组连镜像一起冻结，
> 现状「清单冻结 + 二进制不冻结」是唯一不可持续的组合）。

**清单目录零 Go 代码** 仍是硬约束：`deploy/**` 下不许出现实现。Go 代码只允许在
`apps/` 子树，由根包 `examples_layout_test.go` 的 `TestMeshGoCodeOnlyUnderApps`
钉住（`TestMicroNotCoupledToMesh` 继续断言 `examples/micro` 不引用本目录）。

> 历史教训：曾有人把 `examples/micro` 的旧快照拷成 `examples/mesh/micro/**`，
> 那份快照反向 import `examples/micro/**/internal` —— 违反 Go internal 规则，
> 直接让 `go build ./...` 挂掉。已清除，别再拷回来。这也是 `apps/` 选择独立
> module 而不是复制 micro 层的直接原因。

## 1. 目录

```
examples/mesh/
├── README.md                 # 本文件
├── apps/                     # 网格原生应用（独立 go.mod，五个进程：bff + 4×srv）
│   ├── cmd/ conf/ internal/ pkg/
│   ├── go.mod  Makefile  Dockerfile
│   └── README.md             # ★ 职责边界表 + 两处不可交付项 + 四类减法清单（含反向项校正）
├── deploy/                   # 全部清单
│   ├── shared/               #   两套共用（kubectl apply 的对象）
│   │   ├── 00-namespace.yaml       # launchx-mesh + istio-injection=enabled
│   │   ├── 10-consul.yaml          # consul 进集群（对照组仍需要）
│   │   ├── 20-host-middleware.yaml # 宿主中间件门面（Service + 手工 Endpoints）
│   │   ├── 40-telemetry.yaml       # 采样率 100%（Istio 默认极低，T10 假阴性头号来源）
│   │   └── 50-peer-authn.yaml      # PeerAuthentication：mTLS STRICT
│   ├── istio/
│   │   └── operator.yaml           # istioctl install -f 的入参（**不是** kubectl 对象）
│   ├── micro/                #   对照组清单（含 gateway 进程）
│   │   └── 10-configmap / 20-srv / 30-bff / 40-gateway
│   ├── apps/                 #   原生组清单（10 个文件）
│   │                         #     05-secret（演示令牌）/ 10-configmap（五份配置的键值镜像）
│   │                         #     20/21/22/23-srv-{order,points,product,stock}（4 Deployment + Service）
│   │                         #     30-bff（Deployment + Service + ServiceAccount bff）
│   │                         #     40-gateway（Istio Gateway + VirtualService：入口转发）
│   │                         #     50-authz（AuthorizationPolicy ×4：四域只受 bff 调用）
│   │                         #     60-pdb（PodDisruptionBudget）
│   └── prod/                 #   生产加固底稿（**未 apply**）
│                             #     prometheusrule.yaml + podmonitor.yaml（告警/看板）
│                             #     hpa-bff.yaml + networkpolicy.yaml
├── scripts/bring-up.sh       # check / image / install / deploy / verify / status / down
├── scripts/route-matrix-{apps,micro}.txt  # 入口链条矩阵定义（判据与 PROBE_ROUTES 两处同源）
└── docs/verify.md            # T10–T13 证据落盘（T13 在 §6）
```

> 注：`10-configmap.yaml` 是五份 `apps/conf/*.yaml` 的**逐字镜像**（改 conf 后必须同步本文件；
> 判据：把每个 `  <svc>.yaml: |` 块脱去 4 空格缩进后与 `apps/conf/<svc>.yaml` **逐行相等**）。
> 本轮曾出现「键值一致但注释漂移」（4 份 srv 配置各少 2 行注释），已重新生成对齐。

## 2. 一条命令跑通

`VARIANT` 选部署哪套（默认 `apps`）；镜像 tag 随之切换，两条证据链互不污染。

```bash
export PATH=/usr/local/bin:$PATH          # 本机 minikube/istioctl 都在这里，默认 PATH 没有
cd /Users/hyx/work/gowork/src/hubx/launchx/examples

VARIANT=micro ./mesh/scripts/bring-up.sh check    # 前置 + 清单 dry-run + 地址自检
VARIANT=micro ./mesh/scripts/bring-up.sh image    # 交叉编译 → build 进 minikube daemon
               ./mesh/scripts/bring-up.sh install  # minikube start(2C4G) + istioctl install
VARIANT=apps  ./mesh/scripts/bring-up.sh deploy   # apply shared + 选中变体 → 等 rollout
               ./mesh/scripts/bring-up.sh verify   # 集群内发一次全链路请求 + 打印 Jaeger services
```

`install` 是变体无关的（集群与 Istio 只装一次）；`image` / `deploy` 按 `VARIANT` 走。

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
端口按 TCP 处理（`tcp_proxy`）—— **只看得见连接，看不见请求**。于是 **`bff→srv` 这一跳**：

| 能力 | 网格里可用？ | 说明 |
|---|---|---|
| mTLS / L4 授权 | ✅ | SPIFFE 身份，与本议题无关 |
| 连接级可观测 | ✅ | 但 bff→srv 这一跳**没有 sidecar span**（同根因） |
| 异常实例剔除 | ✅（需二选一） | 与应用侧熔断**阈值须对齐**再双开，见 ACCEPTANCE §13.5 硬规则 |
| 按请求重试 / 超时 / 熔断 | ❌ | 要按 HTTP 语义读请求；应用侧真接线在 `xmiddleware.rpcx`（`conf/*.yaml`） |
| 按 path/header 灰度、限流、JWT | ❌ | 同上 |
| 配置分发 | ❌ | 网格不管配置；ConfigMap 也没有长轮询热更新 |

**上表只对 RPC 跳成立。** 按跳分别判定（「入口」在 micro 组是 gateway **进程**，
在 apps 组是 Istio `Gateway` **资源** —— 同名不同物）：

| 跳 | 协议 | L7 治理 |
|---|---|---|
| `入口 → bff` | HTTP/1.1（webx） | ✅ **下沉 sidecar**：重试 / 超时 / 熔断 / 灰度 / 限流 / JWT 全部可用（VirtualService + DestinationRule） |
| `bff → srv` | Kitex TTHeader/TCP | ❌ **交不出去**：Envoy 只做 `tcp_proxy`，熔断 / 重试 / 超时仍在应用侧 —— 落点是 `xmiddleware.rpcx` 配置段 → `rpcx` 的 `client.Option`（应用代码零新增） |

> apps 组**没有 gateway 进程**：入口转发本身（路由表 / 上游解析 / 逐跳首部）也整块交给
> Istio `Gateway` + `VirtualService`，micro 的 gateway 562 行在 apps 里一行不用写。
> 详见 `apps/README.md` §4 减法清单。micro 组的 gateway 保留作对照，**不要删**。

所以正确表述是：**网格本轮提供 mTLS + 连接级可观测 + HTTP 跳的 L7 治理；RPC 跳与企业级
治理（业务限流、幂等、事务、配置分发）仍在应用侧**。写成「治理已下沉网格」就是虚假陈述
（ACCEPTANCE 陷阱 15）。逐项归属见 `apps/README.md` 的职责表。

## 6. 排查表（本机实测踩过的）

| 症状 | 根因 | 处理 |
|---|---|---|
| Pod `ImagePullBackOff` | minikube 有独立 daemon，看不到宿主镜像 | 用 `minikube docker-env` 直接 build，或 `minikube image load` |
| 容器 `exec format error` | 交叉编译 GOARCH 写错（宿主是 **x86_64**） | `GOOS=linux GOARCH=amd64` |
| 链路 502，Pod 全 Running | consul 在宿主 → Pod IP 健康检查不可达 | consul 进集群（`deploy/shared/10-consul.yaml`） |
| Jaeger 里看不到 sidecar span | 采样率太低（Istio 默认非 100%） | `deploy/shared/40-telemetry.yaml` 下发 100%（`bring-up.sh install` 会自动 apply） |
| sidecar span 与应用 span 分两条 trace | 用了 `zipkin` provider（发 B3 头） | 必须 `opentelemetry` provider（发 W3C `traceparent`） |
| `istioctl install --set ...tracing.sampling` 直接 panic | 1.30 的 `tracing` 是数组，与 map 语义写路径冲突 | 用 `-f deploy/istio/operator.yaml` + Telemetry API |
| bff→srv 这一跳没有 sidecar span | Kitex 私有协议经 `tcp_proxy`，Envoy 看不见请求 | **不是 bug**，见 §5；T10 分两段断言 |
| verify 每条路由 502，日志回退「宿主直连」 | k8s ≥1.29 原生 sidecar：istio-proxy 在 `initContainers`，旧版检测只查 `containers` | 已修进 `bring-up.sh`（两处都查）；详见 `docs/verify.md` §8.1 |
| 业务全通但 sidecar span 全灭（矩阵 0/8） | 共享集群的 meshConfig 被**别的案例**污染，Telemetry 引用的 provider 名不存在 | 重放 `istioctl install -y -f deploy/istio/operator.yaml`；核对 `cm/istio` 的 provider 名，详见 `docs/verify.md` §8.2 |
| `make image` 卡 `FROM alpine:3.20` timeout | buildkit 元数据解析联网且**不走本地回退** | 已固化进 `apps/Makefile`：`minikube image load` 预载 + `DOCKER_BUILDKIT=0`，详见 `docs/verify.md` §8.4 |

## 7. 验收落点

| 案例 | 判据 | 证据 |
|---|---|---|
| **T10** | 同一 trace 下同时有应用 span（demo-gateway/bff/srv）与 sidecar span（`<workload>.launchx-mesh`），应用父子链仍是 gateway→bff→srv | `docs/verify.md` §T10 |
| **T11** | 反向断言：只开 sidecar 追踪、应用侧不注册传播器时，出现「1 条 sidecar 链 + 应用侧孤儿 trace（≥4 条）」的分裂形态 | `docs/verify.md` §T11 |
| **T12** | trace-id / span 数（应用 vs sidecar）/ sidecar 服务名 / Jaeger API 原始片段 / 版本号 全部落盘 | `docs/verify.md` |
| **T13** | apps 原生组：**逐路由矩阵**（6 条 + 异步链 1 条）断言「入口 2xx + 目标域 `service.<域>.*` span 同 trace + 该 trace 含 sidecar span」；父子链是 `ingress→bff→srv`；`POST /order` 的 trace 内须同时出现 `service.points.ConsumeOrderCompleted` 且 traceID 与下单请求一致 | `docs/verify.md` **§6**（T13 路由矩阵） |
| **E4/E5** | 部署成功 + 全链路通；证据同上 | — |
