# mesh-deploy-manifests delta

## ADDED Requirements

### Requirement: apps 组的清单集合

`examples/mesh/deploy/apps/` SHALL 只包含下列清单，且 MUST 能被
`kubectl apply -R -f deploy/apps` 一次应用：

| 文件 | 内容 |
|---|---|
| `10-configmap.yaml` | 五份配置（`bff` + 四域 `srv-*`），由 `ConfigMap` 承载 |
| `20-srv-order.yaml` / `21-srv-points.yaml` / `22-srv-product.yaml` / `23-srv-stock.yaml` | 各域的 `Service` + `Deployment` |
| `30-bff.yaml` | `Service` + `Deployment` |
| `40-gateway.yaml` | Istio `Gateway` + `VirtualService`（入口）+ `DestinationRule`（HTTP 跳治理） |
| `50-authz.yaml` | `AuthorizationPolicy`：域服只接受来自 bff 的调用 |

命名 MUST 以两位数字前缀排序，保证 `-R` 的字典序与应用顺序一致。

#### Scenario: 清单可离线校验且可集群校验

- **WHEN** 分别执行 `VARIANT=apps bring-up.sh check`（离线降级路径）与
  `bash -c 'kubectl apply --dry-run=client -R -f examples/mesh/deploy/apps'`
- **THEN** 两条路径均通过；`deploy/istio/operator.yaml` **不**被扫入（见 mesh-environment）

### Requirement: 入口由 Istio Gateway 声明，不写应用侧 gateway

apps 的入口 SHALL 全部由 Istio 资源声明，MUST NOT 部署应用侧 gateway 进程：

- `Gateway` MUST 挂在 `istio-ingressgateway`（`selector: istio: ingressgateway`），
  至少开放 80 端口；
- `VirtualService` MUST 用 `match.uri.prefix: /api` 匹配入口，`route.destination.host`
  指向 `bff`，并用 `rewrite.uri: /` 表达 micro 路由表里的 `strip: true`（去掉 `/api` 前缀）；
- `hosts` MUST 用 `'*'`（本机 minikube 无真实域名，写死域名会让验收依赖 DNS）。

入口的 `VirtualService` MUST 保持**单条**路由，MUST NOT 按域展开成多条 ——
四域的可达性由 bff 的应用路由表承载（六条，见 `mesh-entry-chain`）。
两个数量被刻意分开：**入口 1 条**证明原 gateway 562 行的路由表可整块下沉，
**bff 6 条**证明四域业务逻辑都能被入口触达。验收 MUST 覆盖后者，MUST NOT
因为入口只有一条就把验收也收窄成一条路径。

#### Scenario: 入口路由表达力与被删掉的 155 行等价

- **WHEN** 请求 `http://<ingress>/api/order/2?user_id=1`
- **THEN** 到达 `bff` 时路径为 `/order/2`（前缀已剥离），返回 200；
  对照 `examples/micro/gateway` 的路由表（`/api → demo-bff` + `strip`）行为一致

#### Scenario: 术语不得混用

- **WHEN** 阅读 apps 的清单与 README
- **THEN** 应用侧 gateway **进程**（micro 的 562 行，apps 不建）与 Istio `Gateway`
  **资源**（apps 新增的 YAML）在文中被明确区分，不出现「apps 的 gateway 部署」

### Requirement: 探针类型按跳的协议选定

apps 的探针 SHALL 按「该跳能不能被 Envoy 按 L7 观察」来选，MUST NOT 一律沿用
`tcpSocket`：

- **`bff`**：MUST 用 `httpGet` 探针打在 `bff` 自身的 webx 健康端点
  （`/healthz/live` 与 `/healthz/ready`，端口为 webx 监听端口）。理由：`ingress→bff`
  是 HTTP 跳，Envoy 能读到状态码，`DestinationRule` 的 `outlier detection`
  才有 5xx 可判、才能剔除异常实例。
- **`srv-*`**：MUST 用 `tcpSocket` 探针打在 RPC 端口。理由：该跳走 Kitex 私有协议，
  端口名声明为 `tcp-*`，Envoy 按 `tcp_proxy` 处理，**看不到状态码**，
  `outlier detection` 在这一跳本就无从生效 —— 为它单独开一个 HTTP 健康端口属于
  建一个没有消费者的面，故不建。

> 结论修正：早期设计把「Istio 的 `outlier detection` 需要 HTTP 探针」记成
> 「apps 的 srv 必须新补 HTTP 探针」。准确表述是：**需要 HTTP 探针的是 HTTP 跳的
> 目标（bff），而 bff 的 webx 已自带健康端点，故 apps 没有因此新增任何进程或端口**。
> 这条 MUST 写进 `apps/README.md`，避免后来人按旧结论给 srv 加 HTTP 面。

#### Scenario: srv 端口名不得写成 grpc-*

- **WHEN** 检查 `deploy/apps/20~23-*.yaml` 的端口名
- **THEN** RPC 端口名为 `tcp-` 前缀（如 `tcp-rpc`）。实测把 9013 之类的 Kitex 端口
  写成 `grpc-*` 会让 Istio 建 HTTP/2 listener，把 TTHeader 当 h2 解析，入口直接 502

#### Scenario: 探针语义被核对

- **WHEN** 检查四份 srv Deployment 与 bff Deployment 的 `livenessProbe` / `readinessProbe`
- **THEN** srv 为 `tcpSocket`，bff 为 `httpGet`；两者都带 `initialDelaySeconds`
  与 `periodSeconds`，且容器端口名与 Service 端口名一致

### Requirement: 配置段地址一律换算为集群内可达名

`deploy/apps/10-configmap.yaml` 里的地址 MUST 全部是可从 Pod 内解析的名字，
MUST NOT 出现裸 `127.0.0.1`：

- 宿主中间件 MUST 经 `host-middleware.<ns>.svc.cluster.local`（或其简写）访问，
  `bring-up.sh check` 的「地址假设自检」MUST 对 `deploy/**/*configmap*.yaml` 保持这条判据；
- 域服之间 MUST 用 K8s Service 的 FQDN 经 `service.upstream.<域>` 声明，例如
  `srv-order: "srv-order.launchx-mesh.svc.cluster.local:9022"`；
- 配置 MUST NOT 含 `xmiddleware.coordx` 段（见 mesh-app-bootstrap）。

> 关于 `service.upstream.<域>` 的语义变化（易误读，必须写明）：
> 在 micro 组里它是**注册中心不可用时的直连兜底**（配了 coordx 时该键不生效）；
> 在 apps 组里没有注册中心，`launchx/internal/mode/webx.go` 的 `clientOptionsFactory`
> 读到 `RegistryAddr` 为空 → `BuildClientOptions` 走 `Target != ""` 分支 → 产出
> kitex `client.WithHostPorts(...)`。所以它**不是兜底，而是唯一地址来源**，
> 其值必须写 K8s Service FQDN（kitex 对这些 host 做 DNS 解析，kube-dns 给出
> ClusterIP，再由 Envoy 按 Service endpoints 做负载均衡）。
> 换个说法：apps 组「上游地址只有一个来源」，而不是「有兜底所以能容错」。

#### Scenario: 地址自检拦住回环地址

- **WHEN** 在 `deploy/apps/10-configmap.yaml` 写入 `addr: "127.0.0.1:6379"`
- **THEN** `bring-up.sh check` 的地址自检报错并以非零码退出

#### Scenario: 上游地址是唯一来源

- **WHEN** 从 apps 的配置中删掉 `service.upstream.srv-order`
- **THEN** bff 对该域的客户端**没有目标**（kitex 选项里既无 resolver 也无 hostPorts），
  调用必然失败；这证明该键是唯一来源而非兜底 —— 与 micro 组「删了还有 consul 发现」
  的行为相反

### Requirement: 镜像与变体互不污染

apps 组 MUST 使用独立镜像 tag（`launchx-mesh-apps:dev`），MUST NOT 与
micro 对照组的 `launchx-demo:dev` 混用；两套清单 MUST NOT 同时部署进同一命名空间。

`bring-up.sh` 的 `VARIANT` MUST 决定三件事：镜像 tag、**路由矩阵**
（入口基址 + 各域路径清单，取代初稿的单一 `PROBE_URL`）、`TRACE_SERVICE`
（trace 判据的服务名）。路由矩阵 MUST 与 `mesh-entry-chain` 的六条路由逐条对应。

#### Scenario: 变体切换不残留

- **WHEN** 先 `VARIANT=micro bring-up.sh deploy`，再 `VARIANT=apps bring-up.sh deploy`
- **THEN** 切换前 MUST 先 `bring-up.sh down` 清空命名空间；路由矩阵与 `TRACE_SERVICE`
  随变体自动切换（micro → 单一 `demo-gateway` 入口；apps → `istio-ingressgateway`
  入口 × 六条路由）

#### Scenario: 变体不得把 apps 的验收退回单路径

- **WHEN** `VARIANT=apps` 下执行 `bring-up.sh verify`
- **THEN** 打请求次数 MUST 等于路由矩阵行数（六条）；MUST NOT 只打一条入口 URL
  就宣告通过
