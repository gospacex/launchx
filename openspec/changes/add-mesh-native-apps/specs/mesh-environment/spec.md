# mesh-environment delta

## ADDED Requirements

### Requirement: 基础环境的三段式分工

网格形态的基础环境 SHALL 由三段互不重叠的能力拼成，且各自的边界 MUST 在
`examples/mesh/README.md` 与本 spec 中写明：

- **集群**：本机 `minikube`（docker driver）提供 K8s 控制面与 Pod 网络；
- **网格**：`istioctl` 安装的 Istio 提供 sidecar 注入、mTLS、连接级遥测与 HTTP 跳的 L7 治理；
- **中间件**：`hubx/dockers/docker-compose.yml` 提供的 docker 容器（mysql / redis / kafka /
  consul / jaeger / elasticsearch 等）跑在**宿主**，不进集群。

网格用例 MUST NOT 自行启动一套中间件容器；MUST NOT 把中间件部署进集群
（`deploy/shared/10-consul.yaml` 是为 micro 对照组的服务发现单独准备的例外，apps 组不使用）。

#### Scenario: 环境就绪时可被一次性确认

- **WHEN** 执行 `bash examples/mesh/scripts/bring-up.sh check`
- **THEN** 脚本分别报告 minikube / istioctl / docker 三项在位数、清单校验结果、
  变体目录存在性与地址假设自检，并给出总判定（`check 通过` / `check 失败`）

#### Scenario: 集群未启动时 check 不误报为清单错误

- **WHEN** minikube 集群处于 Stopped，`kubectl apply --dry-run=client` 连不上 API
- **THEN** check 降级为纯 YAML 解析并明示「集群未就绪」，**不得**返回非零码，
  也不得把「集群没起」渲染成「清单写错了」

#### Scenario: 缺工具时明确报缺并以非零码退出

- **WHEN** PATH 中缺少 `minikube` 或 `istioctl`
- **THEN** check 逐项打印 `✗ 缺 <tool>` 并以非零码退出（本机 `/usr/local/bin` 不在
  WorkBuddy shell 的默认 PATH 内，脚本 MUST 自行 `export PATH=/usr/local/bin:$PATH`）

### Requirement: 清单校验的范围必须等于实际 apply 的范围

清单校验 SHALL 按「实际会被 kubectl 应用的范围」逐目录列举，即
`deploy/shared` + `deploy/micro` + `deploy/apps`；MUST NOT 用 `-R -f deploy/` 整目录递归。

理由：`deploy/istio/operator.yaml` 的 `kind: IstioOperator` 是 `istioctl install -f` 的入参，
**不是** kubectl 对象。用整目录递归时，集群一旦健康，kubectl 能完成 discovery 就会报
`no matches for kind "IstioOperator"`；而集群没起时又会整体降级成 YAML 解析而看不见 ——
于是这条错误**只在集群刚装好时**冒出来，最容易被误判成「清单写错了」。

#### Scenario: 集群健康时 check 通过

- **WHEN** minikube 与 Istio 均已就绪，执行 `VARIANT={micro|apps} bring-up.sh check`
- **THEN** 清单 dry-run 通过（不因 `deploy/istio/operator.yaml` 报错），总判定为 `check 通过`

### Requirement: 宿主中间件在集群内的门面

宿主中间件 SHALL 经一个**无 selector 的 Service + 手工 Endpoints**（`host-middleware`）
暴露给集群内工作负载与 istiod，端点 IP MUST 按实际环境取
`minikube ssh -- "getent hosts host.minikube.internal"`。

其中面向 OTLP/gRPC 的端口名 MUST 以 `grpc-` 开头（`grpc-otlp`，并附
`appProtocol: grpc` 双保险）：Istio 靠**端口名前缀**判定上游协议，写成 `otlp-grpc`
会被判为 HTTP —— Envoy 自身的 OTLP 导出没有下游可跟随，gRPC 流被对端 reset，
sidecar span 全丢，且日志默认级别下不可见。

#### Scenario: 门面地址与实际宿主一致

- **WHEN** 集群重启后宿主 IP 变化
- **THEN** `host-middleware` 的 Endpoints IP 与实际 `host.minikube.internal` 解析结果一致，
  否则 Istio 的 tracing provider 指向一个空端点、sidecar span 静默消失

### Requirement: Istio 安装口径

Istio SHALL 以 operator 文件（`deploy/istio/operator.yaml`）经 `istioctl install -y -f` 安装，
MUST NOT 用 `--set meshConfig.defaultConfig.tracing.sampling=100`（1.30 的 `tracing` 是数组，
与 `--set` 的 map 语义冲突会让 istioctl 直接 panic）。

安装参数 MUST 满足三条：

1. `extensionProviders` 的 tracing provider MUST 为 `opentelemetry`（zipkin 走 B3 头
   `x-b3-traceid`，与 launchx 的 W3C `Composite{TraceContext,Baggage}` 不通，
   应用 span 与 sidecar span 必然分成两条 trace）；
2. provider 的 `service` MUST 指向**集群内 Service**（`host-middleware.<ns>.svc.cluster.local`），
   直接写 `host.minikube.internal` 会被 istiod 拒绝并**静默不生成** OTLP cluster；
3. `values.global.proxy.holdApplicationUntilProxyStarts` MUST 为 `true`
   （否则应用容器会抢在 Envoy 就绪前发请求，表现成极具误导性的
   `dial tcp <ClusterIP>:8500: connect: connection refused`）。

采样率 MUST 由 Telemetry API（`deploy/shared/40-telemetry.yaml`，`randomSamplingPercentage: 100`）
下发，而不是靠 istiod 默认值 —— Istio 默认采样率极低，是「Jaeger 里看不到 sidecar span」
这一假阴性的头号来源。

#### Scenario: 网格与数据面版本可核

- **WHEN** 执行 `istioctl version`
- **THEN** client / control plane / data plane 三行版本一致，data plane 的 proxy 数
  等于注入 sidecar 的 Pod 数，且 `deploy/shared/00-namespace.yaml` 带 `istio-injection: enabled`

### Requirement: 中间件数据面的就绪判据

宿主中间件的**数据面**（库表）SHALL 由 `examples/scripts/order_db.sh` 作为**唯一 DDL 真源**
幂等维护；用例启动前 MUST 通过 `order_db.sh check`。

该脚本的列定义 MUST 取 monolith 与 micro/srv 两套 DDL 的**并集**，追加列 MUST 带默认值，
对已存在的表 MUST 幂等补列（MySQL 8.0 无 `ADD COLUMN IF NOT EXISTS`，先查
`information_schema` 再决定是否 `ALTER`）。

#### Scenario: 缺列时表现为 502 而非连不上库

- **WHEN** `orders` 表缺少模型中的某列（例如 `pay_order_no`）而链路启动正常
- **THEN** 请求在数据层失败并回 502，错误信息为 `Unknown column 'orders.<col>' in 'field list'`；
  执行 `order_db.sh up` 补列后同一请求立即恢复 200。
  验收判据 MUST 区分「连不上库（DSN/网络）」与「库表结构漂移（缺列）」两类故障

#### Scenario: 宿主 mysql 客户端不可静默选中

- **WHEN** 宿主装有 mysql 客户端但其默认走 socket（`/tmp/mysql.sock`），而库在 docker 内
- **THEN** `order_db.sh` MUST 先以 TCP（`-h 127.0.0.1 -P 3306`）做一次可达性探测，
  不通则回落 `docker exec`；MUST NOT 因 `command -v mysql` 命中就默认选定宿主客户端

### Requirement: 环境态与镜像新鲜度自检

`bring-up.sh` 的 check MUST 报告出「工作负载在跑但处于降级态」这一情形，
不得只看 Pod 的 `Running` 计数。

具体：`launchx` 的注册是**启动时一次性**的，注册失败按 `strict.registry` 缺省降级为
「带病运行 + Warn」且**不再重试**。因此「consul 比应用晚就绪」会让 bff 永久未注册，
网关侧表现为 `proxy: no instance of upstream "demo-bff": discovery and static fallback both miss`
与 HTTP 502 ——而 Pod 状态是 `2/2 Running`。

#### Scenario: 注册竞态可被识别与恢复

- **WHEN** 集群重启后，应用 Pod 先于 consul 就绪启动
- **THEN** bff 日志出现 `coordx/consul register failed` + `launchx: register failed, running unregistered`，
  且 `consul catalog services` 中查不到 `demo-bff`；此时 MUST 能通过重启该 Deployment 恢复注册，
  恢复后同一请求返回 200
