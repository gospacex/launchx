# apps —— 网格原生应用（服务网格形态的应用代码）

**本目录是「为网格写的应用」长什么样**，与 `examples/micro` 零 import 耦合（独立 module 兜底）。

网格承诺承担的是：服务通信、服务发现、负载均衡、熔断降级、链路追踪、流量控制。
下面这张表说明**承诺的一部分兑现不了**，以及兑现不了的部分落在谁的头上。

## 1. 职责边界

| 能力 | 谁承担 | 落点 |
|---|---|---|
| 业务逻辑 | 应用 | `internal/srv/<域>/service` |
| 业务分层埋点 | 应用 | launchx 统一埋点，span 名 `<layer>.<entity>.<op>` |
| 服务注册 / 发现 | **环境** | K8s Service + Endpoints → Istio EDS；应用侧零注册代码 |
| 负载均衡 | **环境** | Envoy 按 endpoint 轮询；Kitex 侧 `resolver=dns` 只解析 Service 名 |
| mTLS / 身份 | **环境** | Istio 自动注入证书，SPIFFE 身份，应用零感知 |
| 异常实例剔除 | **环境** | `outlier detection`（与应用侧熔断**阈值须对齐**，别双开 —— 见 ACCEPTANCE §13.5 硬规则） |
| 连接级遥测 | **环境** | sidecar span，W3C `traceparent` 续链 |
| HTTP 跳 L7 治理（重试/超时/熔断/灰度/限流） | **环境** | `Istio Gateway → bff`，VirtualService + DestinationRule |
| **入口转发本身**（路由表 / 上游解析 / 逐跳首部） | **环境** | 不写 gateway 进程：Istio `Gateway` + `VirtualService`（见 §3） |
| RPC 跳熔断 / 重试 | **应用** | `xmiddleware.rpcx` 段 → `rpcx` 的 `client.Option`（熔断默认开、重试默认关，理由在 `conf/bff.yaml`） |
| RPC 跳超时 | **应用** | `rpcx` 单调用超时（缺省 3s）+ `webx.timeout`；入口那一跳交 VirtualService |
| 业务级限流（按用户/租户） | **应用** | sidecar 看不到业务维度 |
| 幂等 / 事务 / outbox | **应用** | 一致性语义永远不下沉 |
| 配置分发 | **应用** | 网格不管配置；`coordx/k8s_configmap` 是配置源，不是网格能力 |

## 2. 两处不可交付项（防后来人误判）

1. **`bff→srv` 这一跳永远不会有 sidecar span。**
   Kitex 走默认 TTHeader over TCP（私有协议），Istio 对该端口按 `tcp_proxy` 处理 ——
   Envoy 只看得见连接，看不见请求。T13 断言认 `ingress→bff` 这一跳的 sidecar span，
   不认 `bff→srv`。这是协议事实，不是清单漏配。

2. **「治理已下沉网格」是虚假陈述。**
   准确表述：网格本轮提供 **mTLS + 连接级可观测 + HTTP 跳的 L7 治理**；
   RPC 跳的熔断 / 重试 / 负载均衡与全部企业级治理（业务限流、幂等、事务、配置分发）仍在应用侧。

   > ⚠️ 这条曾经是**半句谎话**：文字说「留在应用侧」，但落点写的
   > `internal/srv/*/middleware` 里只有限流，而当时全仓零 `WithRetry` /
   > `WithCircuitBreaker` / `WithLoadBalancer` —— 等于宣称有的能力其实没有。
   > 现已真接线：落点是 **`xmiddleware.rpcx` 配置段 → `rpcx` 的 `client.Option`**，
   > 应用代码零新增（这正是「治理属装配不属业务」的体现）。
   > 这三个 kitex 选项现在只出现在 `rpcx/governance.go` 一处 —— 业务侧（含本目录）
   > 仍然零直引，依赖面守卫 `dependency_surface_test.go` 继续拦得住。

   推论：micro 形态里那份「入口自保护限流」（`examples/micro/srv-order/internal/middleware/ratelimit.go`）
   在本目录**必须保留**，只是服务对象从「HTTP 入口」收窄为「RPC 跳 + 业务维度」。

## 3. 目录

**五个进程**：`bff` + 四个 `srv-<域>`。**没有 gateway** —— 入口转发整块交给 Istio
（见 §4 的减法清单），这是本目录与 `examples/micro` 最直观的差别。

```
apps/
├── cmd/         五个入口（bff / srv-{order,points,product,stock}），每个 ≤ 30 行
│                只做 Bootstrap + OnSetup，不含任何形态分支
├── conf/        五份配置基线（集群内由 deploy/apps/10-configmap.yaml 覆盖）。
│                出向 RPC 治理（重试 / 熔断 / 负载均衡）与指标开关都在这里 ——
│                它们是**装配面**的能力，故不落任何 Go 代码
├── internal/
│   ├── bff/     聚合档：app(装配) / handler(HTTP 出入口) / downstream(RPC 客户端)
│   └── srv/<域>/       服务档，四域同构：
│       ├── app/       装配回调：取句柄 → 组链 → SetRPCServer
│       ├── handler/   入参出参转换，零业务判断
│       ├── service/   ★ 唯一允许出现业务判断的层
│       ├── repository/ 持久化（经 App.DB() 句柄，不直引驱动）
│       ├── model/     表模型
│       ├── rpc/       **入向**传输适配（实现生成的 IDL 接口、开 rpc span、走准入闸门）。
│       │              本层不做出向调用 —— apps 的 srv 之间不互调，出向客户端只有 bff 有
│       └── middleware/ 应用侧兜底：RPC 跳的**业务维度**限流（网格认不出 user_id）
└── pkg/         跨五进程共享，只放非基础设施：
    ├── codes/   业务错误码
    ├── events/  领域事件载荷（outbox 用）
    ├── routing/ 入口路由与中间件链骨架（auth / rate_limit / audit）
    └── kitex_gen/{order,points,product,stock}/   四域 IDL 产物
```

## 4. 减法清单：相对 `examples/micro` 哪些不用实现

判据：这一块**只是因为没有网格才存在**。体量为 micro 实测行数（不含 `_test.go`）。

### 4.1 整块不用实现

| 删掉的东西 | micro 里的体量 | 由谁接管 |
|---|---|---|
| **gateway 整个进程** | **562 行**（proxy 288 + routing 155 + setup 70 + main 49） | Istio `Gateway` + `VirtualService` |
| └ 注册中心发现（`proxy.Resolver` / `Finder` 断言 / 配置直连兜底） | 含在 proxy 288 内 | Envoy EDS + K8s Service |
| └ 逐跳首部处理（`hopHeaders` 9 项 + `skipRequestHeaders` 2 项） | 含在 proxy 288 内 | Envoy 天然正确 |
| └ 路由表（前缀最长匹配 + `strip`） | routing 155 行，**实际只有 1 条**路由（`/api → demo-bff`） | `match.uri.prefix` + `rewrite.uri` |
| bff 的下游客户端装配面 | `bff/app/setup.go` 的 `newClient` 闭包 + 4 次类型断言（≈60 行） | Kitex 默认 dns resolver 直连 Service 名 |
| 库侧的 `service.upstream.<svc>` 直连兜底 | `launchx/internal/mode/webx.go:111` + rpcx `coordxResolver` | K8s Service DNS，目标即服务名 |
| 入口限流（按 client IP 令牌桶） | `webx.rate_limit_rps` / `rate_limit_burst` + 中间件一层 | Envoy local / global rate limit（同按源地址） |

> 术语注意：`gateway` 这个词在本目录有**两个含义**。应用侧的 `gateway` 进程（micro 的
> 562 行，本目录删掉）与 Istio 的 `Gateway` 资源（本目录新增的 YAML）不是一回事。
> 前者的活由后者接着干。

### 4.2 部分不用实现

- **超时**：`webx.timeout: 5s` 与 `proxy.defaultTimeout` 属入口→bff 这一跳 → 交
  VirtualService `timeout`。但 **bff 内四段下游 RPC 共享同一 deadline** 必须留在应用侧。
- **传输层身份**：「域服只接受来自 bff 的调用」→ Istio `AuthorizationPolicy`。
  `examples/micro/pkg/auth/auth.go:61` 自己就写着「它防不住内网里的恶意调用（**那需要 mTLS**）」
  —— 这条正是网格补上的那半。

### 4.3 删不掉的，以及**要新补的**

删不掉（四类，别误删）：

1. `srv-*/middleware/ratelimit.go` —— 按 `user_id` / `operator_id` 计数，sidecar 看不到业务维度；
2. `pkg/auth` 的 HTTP 边界验凭据（查缓存反验 `token → user_id`）与 RPC 侧身份闸门
   （`Identity` 的 `UserID` / `OperatorID` 主体 + 签发方 + 时效判据）；
3. `pkg/routing` 的中间件链骨架（audit / auth / rate_limit 三层 span 嵌套）；
4. 幂等 / 事务 / outbox，以及业务分层埋点。

**要新补的（反向项）—— 勘误过的结论：只有一处，而且是本轮才出现的。**

早期版本这里写的是「srv 必须新补 HTTP 探针」。这个结论**不成立**，准确口径是：

- Istio 的 `outlier detection` 要判 5xx 并剔除实例，确实需要 **HTTP** 探针 —— 但需要它的是
  **HTTP 跳的目标**，也就是 `bff`，不是四域 `srv`。
- `bff` 的 `webx` 已自带 `/healthz/{live,ready}`（路由注册在 `webx.registerHealth`），清单里直接写
  `httpGet: {path: /healthz/live, port: 8081}` 即可（见 `../deploy/apps/30-bff.yaml`），
  **没有为这条判据新增任何进程或端口**。注意裸 `/healthz` 是 **404** —— 实际注册的是带后缀的两条。
- 四域 `srv` 是 RPC 跳的目标（Kitex TTHeader over TCP），Envoy 在这一跳只看连接不看请求，
  沿用 micro 的 `tcpSocket` 探针即可（对照证据见 `../deploy/micro/20-srv.yaml` 的
  `livenessProbe` / `readinessProbe`）。此处本就没有可判 5xx 的 HTTP 语义，补 HTTP 探针属无意义改造。

**唯一一处真反向项：Prometheus 采集口必须排除 sidecar 入向拦截。**

四域 `srv` 没有 HTTP 面，指标走独立采集口（`metrics.addr`，9031-9034）。而四域各有一条
`AuthorizationPolicy`，`action: ALLOW` 且只放行 `sa/bff` —— ALLOW 语义下**未匹配即拒绝**，
而 Prometheus 不在网格里、没有 SPIFFE 身份 ⇒ 一律 403。所以清单里必须写：

```yaml
annotations:
  traffic.sidecar.istio.io/excludeInboundPorts: "9031"
```

被排除的端口不经 sidecar：既无 mTLS，也不受 `AuthorizationPolicy` 约束，代价是这一口的
连接级遥测一并没了（采集口不需要）。**这一处是网格「强制多写」的实例**，记在这里免得
后人又把「反向项 = 零」当成通用结论。

> 顺带一条同族的坑（不是反向项，但同源于「端口名决定 Envoy 滤波器」）：入口跳的
> Service 端口名**必须**是 `http`（`30-bff.yaml` 同时写了 `appProtocol: http`）。写成纯数字
> 会让 Envoy 按 `tcp_proxy` 处理 ⇒ `outlier detection` 与 HTTP 重试一起**静默失效**（不报错）。
> 采集口则相反：显式写成 `tcp-metrics`（L4），别写 `http-metrics` —— 否则它会被 L7 接管，
> 正好撞上上面那条 ALLOW 策略。

## 5. 与 `examples/micro` 的层差异

| 层 | micro | 本目录 | 原因 |
|---|---|---|---|
| `internal/gateway`（proxy + routing） | 562 行 | **整个进程消失** | 入口转发交 Istio Gateway + VirtualService |
| `internal/tracex` | 部分服务自建 | **整层消失** | 追踪由 sidecar + launchx telemetry 统一承担 |
| 服务注册装配 | 依赖 consul 地址配置 | **整层消失** | 改走 K8s Service / EDS |
| `upstream` 解析 | consul 发现上游实例 | K8s Service DNS | 配置差异，非目录差异 |
| `service` / `repository` | 有 | **原样保留** | 业务逻辑是唯一不许下沉的东西 |

> `examples/micro` 的 gateway **保持不动** —— 它是「换形态不改代码」的对照证据，
> 必须可复现。上面删掉的是**为网格新写的这一份**里不需要的东西。

## 6. 构建

独立 module ⇒ 根包 `go build ./...` 覆盖不到本目录，构建入口在这里：

```bash
make build      # 交叉编译 GOOS=linux GOARCH=amd64 → bin/
make image      # 构建镜像并载入 minikube daemon
make test       # go test ./...
make tidy       # go mod tidy
```

部署侧入口见 `../README.md` 与 `../scripts/bring-up.sh`（`VARIANT=apps`）。
