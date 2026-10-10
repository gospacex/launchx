# add-mesh-native-apps

> 本变更由 `/sddflow brainstorming` 产出，用户于 2026-10-07 17:23 对三段设计整体确认。
> 上游依赖：`add-launchx-bootstrap` 的后置项 2.5 / 3.8（mesh 清单）与 C4（业务零改动）论证。

## 背景与目标

`examples/mesh` 当前是**纯清单目录**：零 Go 代码，与 `examples/micro` 共用同一份二进制
`launchx-demo:dev`，三个 Deployment 只是换 `command` 启动。这条口径由根包
`examples_layout_test.go::TestMeshHasNoGoCode` 硬钉住，论证的是 C4「换形态不改代码」。

但这条口径回答不了另一个问题：**如果一开始就是为网格写的应用，代码该长什么样？**

网格形态下应用的定位是「只关心业务逻辑」——服务通信、服务发现、负载均衡、熔断降级、
链路追踪、流量控制原则上由 sidecar 承担。现有 mesh 示例无法体现这一点，因为它用的是
为宿主形态（Consul 发现 + 应用侧治理）写的二进制。

本变更的目标：

1. 在 `examples/mesh/apps/` 下规划并落地一套**网格原生应用**的分层目录骨架；
2. 规划 `examples/mesh/` 的整个目录树（代码 / 清单 / 脚本 / 证据四类分开）；
3. 以一张职责表明确**哪些服务治理由环境（minikube + Istio + K8s）承担、哪些必须留在业务代码**，
   并显式声明两处「交不出去」的能力，防止后来人误判。

同时**保留** micro 对照组的清单，形成两套证据链：micro 组证明「换形态不改代码」，
apps 组证明「网格原生应用可以有多瘦」。

## 用户场景

1. **网格形态的应用作者**：想知道在 Istio 环境下写业务应用，哪些装配代码可以删掉、
   删掉后由谁接管、哪些删不掉。打开 `examples/mesh/apps/` 就应看到一棵可直接照抄的分层树。
2. **架构评审者**：需要判定「服务治理是否已下沉网格」这一说法是否成立。需要一张把
   沉淀项、部分沉淀项、沉淀不了的项分开列出的表，而不是笼统的「已下沉」。
3. **回归验证者**：需要在不破坏既有 micro 对照证据的前提下，增加 apps 原生组的部署与
   trace 断言能力。

## 设计方向

### 一、`examples/mesh/apps/` 分层目录树（网格原生应用）

**五个进程**（bff + 4×srv），**独立 go.mod**，以 `launchx.Bootstrap` 为底座，
与 `examples/micro` 零 import 关系。**没有 gateway 进程** —— 入口转发整块交给
Istio `Gateway` + `VirtualService`（用户于 2026-10-07 18:4x 拍板，见 §五）。

```
examples/mesh/apps/
├── go.mod                       # module: github.com/gospacex/launchx/examples/mesh/apps
├── go.sum
├── Makefile                     # build / image / test —— 独立 module ⇒ 根包 go build ./... 覆盖不到
├── README.md                    # 应用侧职责声明 + 两处不可交付项 + 减法清单
├── cmd/                         # 五个入口，每个 ≤ 30 行：Bootstrap + OnSetup
│   ├── bff/main.go
│   └── srv-{order,points,product,stock}/main.go
├── conf/                        # 五份配置基线（集群内由 deploy/ 的 ConfigMap 覆盖）
│   ├── bff.yaml
│   └── srv-{order,points,product,stock}.yaml
├── internal/
│   ├── bff/                     # 聚合档 —— HTTP 入口 + 下游 RPC 客户端
│   │   ├── app/setup.go
│   │   ├── handler/
│   │   └── downstream/
│   └── srv/<域>/                # 服务档 —— 四域同构
│       ├── app/setup.go
│       ├── rpc/                 # 入向传输适配：实现 IDL 接口 + rpc.<域>.<Op> span + 走 admit 闸门
│       ├── route/               # 准入闸门：身份（Auth）+ 限流
│       ├── middleware/          # 应用侧兜底：RPC 跳的限流 / 重试
│       ├── handler/             # 入参出参转换
│       ├── service/             # ★ 唯一允许出现业务判断的层
│       ├── repository/
│       └── model/
└── pkg/                         # 跨五进程共享，只放非基础设施
    ├── codes/  events/  routing/
    └── kitex_gen/{order,points,product,stock}/
```

**与 `examples/micro` 的层差异**（网格原生性的落点）：

| 层 | micro | apps | 原因 |
|---|---|---|---|
| `internal/gateway`（proxy + routing） | 562 行 | **整个进程消失** | 入口转发交 Istio Gateway + VirtualService |
| `internal/tracex` | 部分服务自建 | 整层消失 | 追踪由 sidecar + launchx telemetry 统一承担 |
| 服务注册装配 | 依赖 consul 地址配置 | 整层消失 | 改走 K8s Service / EDS |
| `upstream` 解析 | consul 发现上游实例 | K8s Service DNS | 配置差异，非目录差异 |
| `service` / `repository` | 有 | 原样保留 | 业务逻辑是唯一不许下沉的东西 |

`middleware/` 保留但**只为 RPC 跳服务**：`bff→srv` 走 Kitex 私有协议，Envoy 读不到请求，
该跳的熔断/重试/超时交不出去。

### 二、`examples/mesh/` 整体目录树

```
examples/mesh/
├── README.md                    # 形态导读与结论索引
├── apps/                        # 网格原生应用（独立 go.mod）
├── deploy/                      # 全部 k8s + Istio 清单
│   ├── shared/                  #   两套共用：namespace / consul / host-middleware
│   │                            #              / istio-operator / telemetry
│   ├── micro/                   #   对照组：3 Deployment（含 gateway），镜像 launchx-demo:dev
│   └── apps/                    #   原生组：5 Deployment（无 gateway），镜像 launchx-mesh-apps:dev
│                                #             + gateway.yaml（Istio Gateway + VirtualService，入口）
├── scripts/bring-up.sh          # check / image / install / deploy / verify / status / down
│                                # VARIANT={micro|apps} 选择部署哪套（默认 apps）
├── docs/verify.md               # T10–T12 证据 + 新增 T13「apps 原生组同 trace」
└── .gitignore
```

`kubectl apply -R -f examples/mesh/deploy/` 一把过（dry-run 不要求 namespace 先存在）；
真实 apply 由 bring-up.sh 分两步：先 `deploy/shared`，再 `deploy/$VARIANT`。

### 三、职责边界（本变更的判据）

| 能力 | 谁承担 | 落点 |
|---|---|---|
| 业务逻辑 | 应用 | `apps/internal/srv/<域>/service` |
| 业务分层埋点 | 应用 | 应用调 `otelx/layer` 产出 `rpc`/`handler`/`service`/`repository` 四层 span（`<layer>.<域>.<Op>`）；launchx 只负责装配全局 provider 与组件 span |
| 服务注册 / 发现 | **环境** | K8s Service + Endpoints → Istio EDS；应用侧零注册代码 |
| 负载均衡 | **环境** | Envoy 按 endpoint 轮询；Kitex 侧 `resolver=dns` 只解析 Service 名 |
| mTLS / 身份 | **环境** | Istio 自动注入证书，SPIFFE 身份，应用零感知 |
| 异常实例剔除 | **环境** | `outlier detection`（与应用侧重试二选一，别双开） |
| 连接级遥测 | **环境** | sidecar span，W3C `traceparent` 续链 |
| HTTP 跳 L7 治理（重试/超时/熔断/灰度/限流） | **环境** | `Istio Gateway → bff`，VirtualService + DestinationRule |
| **入口转发本身**（路由表 / 上游解析 / 逐跳首部） | **环境** | 不写 gateway 进程：Istio `Gateway` + `VirtualService` |
| RPC 跳熔断 / 重试 / 超时 | **应用** | `apps/internal/srv/*/middleware` |
| 业务级限流（按用户/租户） | **应用** | sidecar 看不到业务维度 |
| 幂等 / 事务 / outbox | **应用** | 一致性语义永远不下沉 |
| 配置分发 | **应用** | 网格不管配置；`coordx/k8s_configmap` 是配置源，不是网格能力 |

### 四、两处不可交付项（写进 `apps/README.md`）

1. `bff→srv` 这一跳**永远不会有 sidecar span** —— Kitex 走 TTHeader/TCP，Envoy 按
   `tcp_proxy` 处理。T13 断言认 `ingress→bff` 这一跳的 sidecar span，不认 `bff→srv`。
   这是协议事实，不是配置漏项。
2. **「治理已下沉网格」是虚假陈述。** 准确表述：网格本轮提供 mTLS + 连接级可观测 +
   HTTP 跳的 L7 治理；RPC 跳与企业级治理仍在应用侧。
3. **网格还带来一项「必须多写」**：micro 的 srv 用 `tcpSocket` 探针
   （`deploy/micro/20-srv.yaml:64-71`），而 Istio 的 `outlier detection` 需要 **HTTP**
   探针才能判 5xx 并剔除实例。减法清单里要单列，避免「全部下沉」的错觉。

### 五、相对 micro 的「减法清单」（哪些不用实现）

> 体量均为 `examples/micro` 实测行数（不含 `_test.go`）。判据：这一块**只是因为没有网格才存在**。

#### A. 整块不用实现

| 删掉的东西 | micro 里的体量 | 由谁接管 |
|---|---|---|
| **gateway 整个进程** | **562 行**（proxy 288 + routing 155 + setup 70 + main 49） | Istio `Gateway` + `VirtualService` |
| └ gateway 的注册中心发现（`proxy.Resolver` / `Finder` 断言 / 直连兜底） | 含在 proxy 288 内 | Envoy EDS + K8s Service |
| └ gateway 的逐跳首部处理（`hopHeaders` 9 项 + `skipRequestHeaders` 2 项） | 含在 proxy 288 内 | Envoy 天然正确，无需手写 |
| └ gateway 的路由表（前缀最长匹配 + `strip`） | routing 155 行，**实际只有 1 条**路由（`/api → demo-bff`） | VirtualService `match.uri.prefix` + `rewrite.uri` |
| bff 的下游客户端装配面 | `bff/app/setup.go` 的 `newClient` 闭包 + 4 次 JSON 式类型断言 | Kitex 默认 dns resolver 直连 Service 名 |
| 库侧的 `service.upstream.<svc>` 直连兜底 | `internal/mode/webx.go:111` + rpcx `coordxResolver` 适配（`rpcx/client.go:22-93`） | K8s Service DNS，目标即服务名 |
| 入口限流（按 client IP 令牌桶） | `webx.rate_limit_rps` / `rate_limit_burst` 两个配置项 + 中间件一层 | Envoy local / global rate limit（同为按源地址） |

`webx` 入口限流确认**是活接线**，不是死配置：`webx/webx.go:68-69` 把
`cfg.RateLimitRPS, cfg.RateLimitBurst` 传进 `middleware.DefaultChain`。

#### B. 部分不用实现

- **超时**：`webx.timeout: 5s`（`gateway/config.yaml`）与 `proxy.defaultTimeout`
  （`http.Client.Timeout`）属 `gateway→bff` 这一跳 → 交 VirtualService `timeout`。
  但 **bff 内四段下游 RPC 共享同一 deadline** 这件事必须留在应用侧
  （`webx/middleware/timeout.go` 的 `context.WithTimeout` 灌进请求 ctx）。
- **传输层身份**：「域服只接受来自 bff 的调用」→ Istio `AuthorizationPolicy`
  （`from.source.principals = .../sa/bff`）。`pkg/auth/auth.go:61` 自己就写着
  「它防不住内网里的恶意调用（**那需要 mTLS**）」——这条正是网格补上的那半。

#### C. 删不掉的，以及**要新补的**

删不掉（四类，别误删）：

1. `srv-*/internal/middleware/ratelimit.go` —— 按 `user_id` / `operator_id` 计数，
   sidecar 看不到业务维度；
2. `pkg/auth` 的 `Middleware`（bff 边界查缓存反验 `token → user_id`，`TokenStore.Resolve`）
   与 `Gate`（`Identity` 的 `UserID` / `OperatorID` 主体 + 签发方 + 时效判据）；
3. `pkg/routing` 的中间件链骨架（audit / auth / rate_limit 三层 span 嵌套）；
4. 幂等 / 事务 / outbox，以及业务分层埋点。

**要新补的（反向）**：srv 现在是 `tcpSocket` 探针（`deploy/micro/20-srv.yaml:64-71`），
而 Istio 的 `outlier detection` 需要 **HTTP** 探针才能判 5xx 并剔除实例。这是网格带来的
「必须多写」项，不是减法，应单列以免「全部下沉」的错觉。

#### 待决 → 已决（2026-10-07）：gateway 整块交给 Istio

用户裁决：「**micro 的 gateway 不能删除，mesh 里可以交给 Istio Gateway + VirtualService**」。
据此：

- `examples/micro/gateway/**` **保持不动** —— 它是「换形态不改代码」（C4/T8）的对照证据，
  必须可复现；删了就没法论证「同一份二进制换个清单就能跑网格」。
- `apps/` 里**不建 gateway**：不写 `cmd/gateway/`、`conf/gateway.yaml`、
  `internal/gateway/{app,proxy,routing}`。apps 因此是 **5 个进程**。
- 原第 1 段设计里的 `apps/internal/gateway/**` 三个目录与本文件同步删除（已执行）。
- 入口在 `deploy/apps/` 里由 **Istio `Gateway` + `VirtualService`** 声明：
  `match.uri.prefix: /api` → `route.destination.host: bff`，`rewrite.uri: /`
  即表达 micro 路由表里的 `strip: true`。
- 术语坑：应用侧的 gateway **进程**（micro 的 562 行）与 Istio 的 `Gateway` **资源**
  （apps 新增的 YAML）同名不同物。前者的活由后者接着干。

## 关键决策

1. **apps 与 micro 完全解耦**：新建网格原生应用，不拷贝 micro 快照（历史上拷贝快照曾因
   Go internal 规则直接让 `go build ./...` 挂掉）。
2. **服务发现改走 Istio/K8s 原生**：去掉 Consul，业务不注册也不发现。顺带消掉
   「集群内外两份注册表」的副作用。micro 对照组仍保留 Consul。
3. **守卫改写而非删除**：`TestMeshHasNoGoCode` 改为只断言 `examples/mesh` 下
   `apps/**` 之外无 `.go` 文件，继续挡住「随手往清单目录扔 main.go」。
4. **L7 治理混合口径**：HTTP 跳（Istio Gateway→bff）下沉 sidecar，RPC 跳（bff→srv）留应用侧。
   不为了「全下沉」而把 rpcx 换成 gRPC——那是另一个变更的量级。
5. **两套清单并存**：micro 组（零代码改动论证，含 gateway）+ apps 组（网格原生论证，无 gateway），
   各自独立镜像。
6. **apps 独立 go.mod**：隔离性最好，但需配套 Makefile 与 CI 入口，避免根包
   `go build ./...` 覆盖不到而漏编译。
7. **四域四进程**：与 micro 一对一，便于对照；代价是四份 main/setup 高度重复。
8. **gateway 不进 apps**：入口转发整块交给 Istio `Gateway` + `VirtualService`；
   micro 的 gateway 保留作对照。apps 因此是 5 个进程而非 6 个。

## 范围边界

**包含：**
- `openspec/changes/add-mesh-native-apps/proposal.md`（本文件）
- `examples/mesh/apps/` 的**目录骨架**：完整目录树 + 独立 `go.mod` + `README.md`（职责声明）+ `Makefile`
- `examples/mesh/deploy/{shared,micro,apps}/` 目录重组，现有 9 个 yaml 迁入 `shared/` 与 `micro/`
- `scripts/bring-up.sh` 的 `DEPLOY_DIR` / `VARIANT` / dry-run 目标调整
- `examples_layout_test.go::TestMeshHasNoGoCode` 改写为放行 `apps/**`
- `examples/mesh/README.md` 的目录段与「零 Go 代码」结论段重写

**不包含（本次）：**
- 任何业务实现代码（`cmd/**/*.go`、`internal/**/*.go` 的真实实现）—— 留给后续 `/sddflow build`
- apps 组的 k8s 清单落地（5 个 Deployment + `gateway.yaml` 的 Istio Gateway/VirtualService）
  —— 骨架阶段只建目录，清单随实现补齐
- `mode=mesh` 第五档装配（apps 复用现有 bff/srv 两档；gateway 档不再被 apps 使用）
- Kitex gRPC 改造（L7 全链下沉的方案 B，另开变更）
- `examples/micro` 的任何改动（对照组必须保持可复现，含其 gateway）

## 验收标准

- [ ] `examples/mesh/apps/` 目录树与 `deploy/{shared,micro,apps}` 结构与本文件一致
- [ ] `apps/` 下**不含** `cmd/gateway/`、`internal/gateway/**`、`conf/gateway.yaml`
- [ ] `examples/mesh/apps/go.mod` 独立存在，`module` 名为 `github.com/gospacex/launchx/examples/mesh/apps`
- [ ] `apps/README.md` 含职责表、三处不可交付项声明、四类减法清单（整块 / 部分 / 删不掉 / 要新补）
- [ ] 根包 `go test . -run 'TestMesh'` 通过：`apps/**` 之外的 `.go` 文件数必须为 0
- [ ] `go build ./...` / `go vet ./...` / `gofmt -l` 全绿
- [ ] `bash examples/mesh/scripts/bring-up.sh check` 在无集群环境下仍能完成清单校验
      （kubectl 可用时走 `--dry-run=client`，集群未启动时降级为 YAML 语法解析）
- [ ] `examples/mesh/README.md` 不再出现「本目录没有一行 Go 代码」这一全局结论

## Amendments

### 2026-10-07: 入口到各域业务逻辑的完整链条纳入验收范围

**原因**

`/sddflow build` 执行到 Task 2 时，用户提出一条验收要求：
**「服务网格验收时要验证从入口各种路由到 srv 各个业务逻辑完整的链条」**。

核对发现三处**规格与计划的共同缺口**（都不是代码没实现，而是规格与验收条件本身不完整）：

1. **bff 路由表只规划了 2 条**（`/order/:id`、`/ping`），但 `downstream.New` 建了**四域
   客户端** —— `Clients.Points` / `Clients.Product` / `Clients.Stock` 建了、零调用点。
   编译器与运行期都不报错，后果是那三个域的业务逻辑**永远无法从入口触达**
   （对照：`examples/micro/bff/app/setup.go:137-143` 有 6 条路由）。
2. **`srv-order` 的 `CreateOrder` / `MarkPaid` 不可达**：`handler/order.go` 只实现了
   `GetOrder` 一个 RPC，而 `publish`（事件投递）只在 `CreateOrder` 里被调用 ——
   连带的后果是 `order → MQ → points` 的异步链在入口侧根本没有触发点。
3. **T13 判据过弱**：初稿只打**一条** URL，且 `trace-audit.py --service bff --mesh`
   只断言「同一 trace 内有 sidecar span + 应用 span」。它**不看是哪个域的 span** ——
   六条路由全接到 order 域、或路由存在但 handler 空实现，判据照样 PASS。

附带发现一处**计划自身的错误**：Task 6 写「`handler/stock.go` 实现 `Decrease` /
`GetStock`」，但 kitex 生成的 `stock.Stock` 接口要求实现**全部 7 个方法**
（`pkg/kitex_gen/stock/stock.pb.go:type Stock interface`），且 `Decrease` 不是 IDL 方法
（对应的是 `ReserveStock` 一族）。按原写法该文件**编译期即失败**。四域同此。

另有一处规格瑕疵一并修正：`mesh-app-handles` 的 Scenario「自建句柄被拦下」以
`App.Cache()` 举例，但 `cachex.IsTracingEnabled()` 读的是**包级开关**，与句柄里
放的是哪个实例无关 —— 装配期拦不住自建 cache 实例。只有 `db` 有来源实锤
（dbx 注册在 gorm 句柄上的 trace 回调随句柄走）。

**影响规格**

| 规格 | 变更 |
|---|---|
| `specs/mesh-entry-chain/spec.md` | **新增** capability，5 条 Requirement（路由表对应、IDL 方法姿态、逐路由断言到业务层、异步链单列、矩阵证据落盘） |
| `specs/mesh-trace-verification/spec.md` | T13 断言范围由「sidecar 入链比例 ≥ 1 条」收紧为**路由矩阵每一行** |
| `specs/mesh-app-handles/spec.md` | 自检 Requirement 增加「有效裁决含总闸」与「证据强度分级表」；Scenario 由 `cache` 举例改为 `db`，并新增弱判据分级 Scenario |
| `specs/mesh-deploy-manifests/spec.md` | 入口 Requirement 补充「入口 1 条 vs bff 6 条」的分工；变体 Requirement 的 `PROBE_URL` 改为**路由矩阵**，新增不得退回单路径 Scenario |

**影响任务**

- `tasks.md` 1.7（bff）—— 未完成，就地扩写：路由由 2 条补到 6 条
- `tasks.md` 1.3 / 1.4 / 1.5 / 1.6（四域 handler）—— 未完成，就地校正：实现全接口 +
  未接线方法显式返回 `codes.Unimplemented`
- `tasks.md` 3.3（T13 验收）—— 未完成，就地扩写：单路径 → 路由矩阵
- `tasks.md` **新增 4.1 / 4.2 / 4.3**（`## 4. Amendment`）：判据脚本矩阵化、逐路由验收、矩阵证据落盘
- `plan-ready.md` 与 superpowers plan 同步：Task 7 / 15 就地扩写，**追加 Task 17**
  （入口链条判据矩阵化）；执行顺序为 **Task 17 → Task 15**

**非目标（本次 amend 明确不做）**

- 不把入口 `VirtualService` 展开成按域的多条路由（用户 2026-10-07 20:1x 选择：
  入口保持 1 条，链路覆盖由 bff 应用路由表承载）
- 不为四域补齐 IDL 里全部 22 个方法的业务实现 —— 未接线方法只要求**显式声明**
  （`codes.Unimplemented`），不要求实现
- 不引入 outbox 表（事件投递失败仍只告警，如实保留为「删不掉的四类」之一）

### 2026-10-07: `rpc/` 层语义纠正 + 分层埋点补齐（Task 3 实现期发现）

**触发**：`srv-order`（Task 3）实现完毕、规格合规审查不通过。审查发现的问题不在实现，
而在规格与计划本身 —— 三处互相矛盾、一处整层漏写。

**问题与裁定**

| # | 问题 | 裁定 |
|---|---|---|
| 1 | `rpc/` 层被写成「跨域调用」：`proposal` §一 树注释、`tasks.md 1.3`、计划 Task 3 Step 4 都按出向客户端写，计划 Task 4/5/6 甚至要求「删除 `rpc/<域>.go`」 | **纠正为入向传输适配层**（实现 IDL 接口 + 开 `rpc.<域>.<Op>` span + 走 admit 闸门）。依据：`examples/micro` 四域的 `rpc/<域>.go` 全部是这个形态；launchx 的 `App.RPCClientOptions()` **只在 bff/gateway 档产出**（`internal/mode/webx.go`），srv 档直接返回 error，srv 无从经句柄取得出向选项 |
| 2 | 「order 域内部再调 product/stock」（`mesh-entry-chain` 路由表与 IDL 姿态表）与上一条同源 | **删除该表述**；新增 Requirement「跨域编排只在 bff，srv 之间不互调」并给出三条依据（库能力 / 句柄矩阵 / `srv→srv` 跳拿不到 sidecar span 与 L7 治理） |
| 3 | **四域的分层埋点整层漏写**：计划 Task 3–7 无任何 `otelx/layer` 步骤，而 `mesh-entry-chain` 的判据要求每条路由的 trace 内出现 `service.<域>.<Op>` span | **补齐**：`rpc` / `handler` / `service` / `repository` 四层各开一个 `<layer>.<域>.<Op>` span（与 micro 逐层对应）。`rpcx/middleware.go` 的包注释已写明「服务端 span 由各服务自己的 service 层产出」—— 这不是可选的锦上添花，是判据的前置 |
| 4 | `mesh-app-layering` 目录树写七个目录（漏 `route/`），而实际与计划 Task 10 都是八个 | 树补 `route/`，并逐目录写明职责与 MUST NOT |
| 5 | `mesh-app-handles` 写「apps 源码 MUST NOT 出现 `gorm.io/gorm`（`App.DB()` 断言除外）」，但 `repository` 必持 `*gorm.DB`、`pkg/handles` 还须 import 它探 trace 回调 | 措辞改为「禁**自行建连**（`gorm.Open` / `gorm.io/driver/*`）」；`gorm.io/gorm` 的三处正当用法写入 spec，并声明单测可自建内存库 |

**影响规格**

| 规格 | 变更 |
|---|---|
| `specs/mesh-app-layering/spec.md` | 目录树补 `route/` + 八目录职责表；分层链 Requirement 增「四层各开 `<layer>.<域>.<Op>` span」与两条 Scenario |
| `specs/mesh-entry-chain/spec.md` | 路由表与 IDL 姿态表去掉「内部再调 product/stock」；IDL Requirement 改名为「四域 **RPC 实现体**必须实现全部 IDL 方法」（实现方是 `rpc/`，不再是 `handler`）；**新增 Requirement**「跨域编排只在 bff，srv 之间不互调」 |
| `specs/mesh-app-handles/spec.md` | 依赖面 Requirement 与 Scenario 改为「禁建连」口径；矩阵新增「全部 `srv-*` 不用 `RPCClientOptions()`」；入链 Scenario 的 span 清单补 `rpc.order.*` / `handler.order.*` |

**影响任务**

- `tasks.md` 1.3 / 1.4 / 1.5 / 1.6 —— 均未勾选，就地改写：`rpc` 由「跨域调用 / 删除」
  改为「入向传输适配层」，并补四层分层埋点
- `tasks.md` **新增 4.4 / 4.5 / 4.6 / 4.7**（`## 4. Amendment`）
- `plan-ready.md` 与 superpowers plan 同步：Task 4/5/6 就地改写、Task 7 错误码映射补
  `Unauthorized`/`RateLimited`、Task 9/10 判据校正、**追加 Task 18**
  （srv 分层埋点与 `rpc/` 入向适配回填）；执行顺序为 **Task 18 → Task 4/5/6**
- Task 3 的已勾选 Step **不回退**：其产出（model / repository / service / handler /
  route / app）在 Task 18 里按新口径增补，不改判据

**非目标（本次 amend 明确不做）**

- 不实现 srv→srv 的跨域调用（见上表 #1/#2）
- 不为 `pkg/codes` 之外新增错误码段位：`Unauthorized` / `RateLimited` 落在既有
  1xxxx 通用段
- 不改 bff 的六条路由与入口单条 `/api` 前缀的设计

---

### 2026-10-07: `route/` 的限流必须是真的（Task 4 实现期发现）

**问题**：计划初稿在 Task 4/5/6 写「因本域 RPC 只读，`Admit` 对只读方法直接 `return true`」。
按此落地后出现三处连锁后果：

| # | 问题 | 说明 |
|---|---|---|
| 1 | `srv-points` 的 `route/` **退化成空目录** | `mesh-app-layering` 的逐目录职责表第 27 行明确要求本目录承担「身份（Auth）+ 限流（`middleware`）」 |
| 2 | `codes.RateLimited` 在本域**没有真实生产者** | `rpc/points.go` 的 `if !guard.Admit(...)` 成为不可达分支 —— 与本变更在 Task 18 定下的裁决（「两个码各自的生产者必须是真的，不许『定义到位但无人使用』」）直接冲突 |
| 3 | `route` 包的 100% 覆盖率是**空函数的覆盖**；`rpc` 包因此掉到 94.3% | 覆盖率数字掩盖了「闸门其实是空的」 |

**裁定**：**四域的 `route/` 一视同仁 —— 身份闸门 + 按主体限流**，`Admit` MUST NOT 恒真。
上限取各域 Task 2 已定的 `middleware` 常量（order 120 / points 60 / product 300 / stock 180）。
统一比逐域特判更好（四域 `route/` 同构，只是上限不同，Task 10 的同构核对才有意义）；
且「只读所以不限流」**不是任何 spec 的要求** —— 它是计划初稿自己的减法，而这处减法把应用侧
**唯一**的 RPC 跳治理手段（该跳走 Kitex TTHeader/TCP，Envoy 只按 `tcp_proxy` 处理、拿不到 L7 限流）
给掏空了。

**影响规格**

| 规格 | 变更 |
|---|---|
| `specs/mesh-app-handles/spec.md` | **句柄矩阵重构**：`DB()` / `KV()[<name>]` / `RPCServerOptions()+SetRPCServer()` / `Config()+Logger()` 提为「全部 `srv-*`」共享行，其中 **`KV()[<name>]` 的用途写明为「RPC 跳限流计数」，并注明四域一视同仁**；`Engine()` / `Tracer()` 补进「全部 `srv-*` **不用**」；删除与共享行重复的 `srv-order` KV / RPCServerOptions 行 |

**影响任务**

- `tasks.md` —— **无文字变更**（1.4/1.5/1.6 的验收行不含限流细节；3.2 的「逐进程对照 `mesh-app-handles` 的句柄矩阵」自动吸收新矩阵）
- superpowers plan Task 4/5/6 差异表的 `route/` 行（三处）：判据从 `grep -n "return true"` 非空
  改为 `grep -cE "limiter\.Allow"` ≥ 1 **且** `grep -c "return true"` == 0
- superpowers plan 追加 `Amend 说明（2026-10-07 #6）`；Task 4 差异表 `model` 行的判据改成语义
  grep（`func \((PointAccount|PointsLog)\) TableName`）—— 裸 `grep -c "TableName"` 会连注释里的
  字样一起数，逼着人改注释去凑数（grep-as-oracle 反模式）
- `srv-points` 实现侧同步：`route.Admit` 改为真实限流、`app/setup.go` 三处与旧设计相悖的注释
  改正、`rpc.admit` 注释改正、补 `RateLimited` 生产者用例

**非目标**

- 不改 `middleware/ratelimit.go` 的上限常量与实现（仅同步其注释里与现状不符的一句）
- 不给 `srv-*` 新增其它治理件（重试 / 熔断仍不在应用侧，见本仓既有口径）


### 2026-10-08: 四项横向能力缺陷修复 + RPC 治理真接线 + 最小 metrics

**原因**

用户对 `examples/` 做了一次横向能力核查（业务逻辑 / 通信协议 / 服务发现 / 负载均衡 /
熔断降级 / 链路追踪 / 日志监控），查出**四项缺陷**并要求「修复」。用户同时钉了两个口径：
范围取「再加最小 metrics」；死配置取「删掉 `exporter` / `endpoint`」。

四项缺陷与处置：

| # | 缺陷 | 处置 |
|---|---|---|
| 1 | **重试注释撒谎**：文档写「RPC 重试在应用侧」，全仓却零 `WithRetry` | 真接线（见下），并把落点从假路径 `internal/srv/*/middleware` 改成 `xmiddleware.rpcx` |
| 2 | **RPC 无熔断 / 无重试 / 无显式负载均衡** | `rpcx/governance.go` 收口三项，`launchx/internal/mode/rpc.go` 读配置透传 |
| 3 | **`logx.otel.exporter` / `endpoint` 是死配置** | 删（19 份源配置 + 9 份内嵌副本，共 56 行）；保留 `enable_trace` |
| 4 | **服务发现失败静默**：绕过 `strict.registry` 只看注册、不看发现 | `component/consul.go` 改走 `degrade(..., ConsequenceDiscovery)` + 回归网 + 反向探针 |

**判据为何是「实测」而不是「代码里写了」**

第 3 项是先做了**全仓消费点核查**才敢删的：`OTel.Exporter` / `OTel.Endpoint` 的读取点
**只存在于 `logx/example/**`** 这些独立示例（它们自己手工建 TracerProvider）；库侧
`zap.go` 只读 `OTel.Enabled`，而 launchx 的 span 出口走
`telemetry.Init(name, trace.endpoint, trace.enabled)` —— 所以配置里写 `exporter: otlp`
只会让人误以为「日志走 otlp」。

第 4 项的正向与**反向**探针都跑了：把 `degrade` 换回旧行为后新增用例两条子用例均 FAIL，
还原后 PASS（判据不是恒真）。

**影响规格**

**无 Requirement 正文改动。** 逐条核对过：七份 `spec.md` 均未出现 metrics / 反向项 /
`WithRetry` 等字样；`mesh-deploy-manifests` 的「清单集合」Requirement 枚举的是**文件**，
本轮只往既有文件里加端口与注解，未增删文件 ⇒ 两条校验路径
（`bring-up.sh check` 离线降级 + `kubectl apply --dry-run=client -R`）均通过。

**影响任务**

本变更的 Task 1–17 已全部勾完，本次 amend **不重开任何 Task**，改动落在三个位置：

1. **SDK 侧（跨仓）**：`rpcx` 新增 `governance.go` / `metrics.go`（治理三件套 + RPC 指标），
   `webx` 新增 metrics 采集口注册与限流豁免；
2. **装配侧**：`launchx/internal/telemetry/metrics.go`（拉取式 MeterProvider）、
   `internal/mode/rpc.go`（治理配置解析 + 校验即退）、`app.go`（srv 档独立采集口 + 退出链收口）；
3. **examples 侧**：13 份源配置 + 3 份派生复制 + 5 份清单同步（采集口、治理段、删死键），
   三处文档改为真实表述，`micro/scripts/e2e.sh` 增断言 ③。

**一并记录的两条口径修正（防止后人按旧结论办事）**

1. **「反向项 = 零」被推翻**：早期结论是网格不带来任何「必须多写」。本轮出现**真反向项**
   ——四域 `AuthorizationPolicy` 是 `action: ALLOW`（未匹配即拒绝），Prometheus 没有 SPIFFE
   身份 ⇒ 采集口必须 `traffic.sidecar.istio.io/excludeInboundPorts` 排除 sidecar 拦截。
   端口名方向也和入口跳**相反**：入口必须 `http`，采集口必须 `tcp-metrics`。
2. **上一条 amend 的「重试 / 熔断仍不在应用侧」作废**：那说的是当时状态；现在三项治理
   都在 SDK 侧真接线，业务侧经 `xmiddleware.rpcx` 取用（业务代码零新增，
   `dependency_surface_test.go` 继续拦得住直引）。

**非目标（本次 amend 明确不做）**

- 不在 examples 里打开 `retry`：链路上有非幂等写（下单 / 扣减 / 入账），且
  `(1 + max_attempts) × RPCTimeout(3s)` 会盖过入口 `webx.timeout(5s)`。
  配置里写了 `enabled: false` 并注明上述三条理由，属**刻意的显式关闭**而非遗漏
- 不补 pprof / 告警 / 看板（ACCEPTANCE §13.4 继续记 SKIP）
- 不起 minikube 复验网格的采集口 ⇒ ACCEPTANCE §13.4 网格列记 **⚠️**，**不记 ✅**
- 不改 `examples/mall/**`（并发会话正在重做，本轮只留说明）
