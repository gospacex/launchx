# mesh-entry-chain delta

## ADDED Requirements

### Requirement: bff 的路由表必须与四域一一对应

apps 的 `bff` SHALL 为 `srv-order` / `srv-points` / `srv-product` / `srv-stock`
四域各暴露至少一条可经入口触达的业务路由；`internal/bff/downstream.Clients`
的每个字段 MUST 至少有一个调用点。

MUST NOT 出现「客户端建好但零调用」：该形态既不会让 `go build` 失败，也不会在运行
期报错，只是让整个域的业务逻辑**永远无法从入口触达** —— 是本变更最容易漏掉的一类
静默失效（Task 7 初稿正是此形态：建了四域客户端，却只注册 `/order/:id` 与 `/ping`）。

路由基线（六条，入口侧 Istio `VirtualService` 保持单条 `/api` 前缀 + `rewrite.uri: /`）：

| 入口（Istio Gateway） | bff 应用路由 | 下游 RPC | 目标域 service 层 |
|---|---|---|---|
| `GET /api/ping` | `GET /ping` | — | —（单跳自响应，trace 判据豁免） |
| `GET /api/order/:id` | `GET /order/:id` | `order.GetOrder` | `service.order.GetOrder` |
| `POST /api/order` | `POST /order` | `order.CreateOrder` | `service.order.CreateOrder` → 事件投递 |
| `GET /api/points/:user_id` | `GET /points/:user_id` | `points.GetAccount` | `service.points.GetPoints` |
| `GET /api/product/:id` | `GET /product/:id` | `product.GetProduct` | `service.product.GetProduct` |
| `GET /api/stock/:product_id` | `GET /stock/:product_id` | `stock.GetStock` | `service.stock.GetStock` |

入口路由数与 bff 应用路由数是**两个不同的量**：前者保持 1 条（证明原 gateway 562 行
的路由表可整块下沉），后者是 6 条（证明四域业务逻辑都能被入口触达）。MUST NOT
把「入口 1 条」误读成「验收只需 1 条路径」。

#### Scenario: 路由覆盖矩阵完整

- **WHEN** 检查 `internal/bff/handler/mall.go` 的 `Register` 与
  `internal/bff/downstream/clients.go` 的调用点
- **THEN** 上表六条路由全部注册；`Clients` 的四个字段各至少一个调用点

#### Scenario: 不留死客户端

- **WHEN** 对 `internal/bff/**` 执行 `grep -cE 'clients\.(Order|Points|Product|Stock)'`
- **THEN** 四个名字各有 ≥ 1 命中

### Requirement: 四域 RPC 实现体必须实现全部 IDL 方法，未接线的必须显式声明

apps 的四个 `srv-*` 的 **`rpc/` 层**（入向传输适配层，见 `mesh-app-layering`）SHALL 实现
`pkg/kitex_gen/<域>` 里生成的**全部**接口方法。
这不是可选项：kitex 生成代码把接口类型断言写在每个分发函数里
（`svc.(stock.Stock).GetStock(...)`），且 `server.NewServer(svc stock.Stock)`
按接口签名约束入参 —— 少实现一个方法，**编译期**即失败，不是运行期。

`handler/` 不再直接满足该接口（它只做转换），接口的唯一实现方是 `rpc/<域>.go`；
「全接口实现」的编译期断言（`var _ <域>.<域> = (*Server)(nil)`）也 MUST 落在 `rpc/` 层。

每个方法 MUST 落到下表两档之一，MUST NOT 出现第三种形态（空实现、`panic`、
`TODO` 占位）：

| 姿态 | 要求 |
|---|---|
| **已接线** | 有 service 层实现，且能在本能力「bff 路由表」矩阵里指到入口（同步）或指到事件消费链（异步） |
| **未接线** | MUST 显式返回 `codes.Unimplemented` 业务码，并在方法注释里写明「本示例未实现，非漏项」 |

IDL 方法清点（`pkg/kitex_gen` 实测，四域共 22 个）：

| 域 | IDL 方法 | 本变更的姿态 |
|---|---|---|
| order | `CreateOrder` `GetOrder` | 已接线（`POST /order` / `GET /order/:id`） |
| order | `ListOrders` `Report` | 未接线（显式 `Unimplemented`） |
| points | `GetAccount` | 已接线（`GET /points/:user_id`） |
| points | `Earn` `Consume` `ListLedgers` `Report` | 未接线 |
| product | `GetProduct` | 已接线（`GET /product/:id`） |
| product | `CreateProduct` `UpdateProduct` `UpdateStatus` `ListProducts` `Report` | 未接线 |
| stock | `GetStock` | 已接线（`GET /stock/:product_id`） |
| stock | `ListStocks` `ReserveStock` `ReleaseStock` `ConfirmStock` `AdjustStock` `Report` | 未接线 |

> 这条同时修正了计划初稿的一处错误：Task 6 写「`handler/stock.go` 实现 `Decrease` /
> `GetStock`」——`Decrease` 不是 IDL 方法（IDL 里对应的是 `ReserveStock` 一族），
> 且按此写法该文件无法通过编译。四域按同一口径校正。

#### Scenario: 少实现一个方法即编译失败

- **WHEN** 从 `internal/srv/stock/rpc/stock.go` 删掉 `ListStocks` 方法
- **THEN** `cd examples/mesh/apps && go build ./...` 报错（接口未满足），
  证明「全接口实现」是编译期约束而非纪律约定

#### Scenario: 未接线方法不留 panic

- **WHEN** 对未接线方法发起一次 RPC（如 `stock.ListStocks`）
- **THEN** 返回业务码 `codes.Unimplemented` 与可读消息；
  MUST NOT 出现 panic、空响应或 `TODO` 实现

### Requirement: 跨域编排只在 bff，srv 之间不互调

apps 的**跨域编排 MUST 全部落在 `bff`**：bff 按路由表调用目标域的 `rpc/` 层，
每个域只处理自己那一跳。`internal/srv/**` MUST NOT 建出向 RPC 客户端
（`kitex_gen/*/NewClient`、`[]client.Option`、`App.RPCClientOptions`）。

理由有三条，缺一条都不足以成为纪律，三条同时成立：

1. **库能力**：launchx 的 `App.RPCClientOptions()` 只在 `bff` / `gateway` 档产出
   （`internal/mode/webx.go`），srv 档 `App.RPCClientOptions()` 直接返回
   `rpc client is not available in mode "srv"` 的 error —— 没有经句柄取得出向选项的路径；
2. **句柄矩阵**：`mesh-app-handles` 的矩阵里 `RPCClientOptions` 只出现在 `bff` 行，
   srv 侧自建客户端等于把「一律经 `App` 取用」这条约束破一个口子；
3. **收益为零**：`srv→srv` 同样走 Kitex TTHeader/TCP，Envoy 按 `tcp_proxy` 处理，
   该跳**永远拿不到 sidecar span 与 L7 治理** —— 多一跳只多一次序列化与一个故障点。

`internal/srv/<域>/rpc/` 因此是**入向**适配层（`mesh-app-layering` 已定义其职责），
不是出向客户端。这与 `examples/micro` 的 srv 形态一致：micro 的 `srv-order/rpc/order.go`
是「实现 `order.Order` 接口 + 走 Router + 开 `rpc.order.<Op>` span」的入向适配层，
四域的 srv 都不调兄弟域。

#### Scenario: srv 里找不到出向客户端

- **WHEN** 对 `examples/mesh/apps/internal/srv/**` grep `NewClient(` / `client.Option` /
  `RPCClientOptions(`
- **THEN** 零命中；同一 grep 在 `internal/bff/**` 有命中（`RPCClientOptions` 四域各一次）

#### Scenario: 跨域串在 bff，不串在 srv

- **WHEN** 取一次 `GET /api/order/:id` 的 trace
- **THEN** trace 内是 `bff → client.srv-order.GetOrder → rpc.order.GetOrder →
  handler.order.GetOrder → service.order.GetOrder → repository.order.Find`；
  MUST NOT 出现 `srv-order → srv-product` 这一跳

### Requirement: 每条路由的验收必须断言到目标域的业务层

验收 SHALL 逐路由发起一次真实请求（经 Istio Gateway），并对每条路由断言三件事：

1. 入口返回 2xx；
2. 该请求的 trace 内出现**目标域的 `service.<域>.<Op>` span**（业务判断层确实执行了）；
3. 该 trace 内出现 sidecar span（M1/M2/M3 判据不变）。

MUST NOT 只用「trace 内存在任意 `service.*` span」判定 —— 那会让「六条路由其实全打到
order 域」这类接线错误逃过验收；这正是初稿 `trace-audit.py --service bff` 判据的盲区。

#### Scenario: 路由 × 域 矩阵

- **WHEN** 执行 `VARIANT=apps bring-up.sh verify`
- **THEN** 输出一张「路由 × 目标域 service span × HTTP 状态 × sidecar 跳」矩阵；
  任一格缺失即 FAIL 且非零退出

#### Scenario: 接线错误被拦下（反例）

- **WHEN** 把 `GET /product/:id` 的处理函数误接到 `clients.Order`
- **THEN** 验收 FAIL，原因指向「`/product/:id` 的 trace 内没有 `service.product.*` span」；
  MUST NOT 因为 HTTP 200 而判通过

### Requirement: 异步链（order → MQ → points）必须单独断言

`POST /api/order` 触发的 `order.completed.v1` 由 `srv-points` 消费；消费侧 span MUST 与
下单请求落在**同一 trace**（mqx 的注入/提取是覆盖语义：ctx 站在哪，谁就是下游的父节点）。

这条 MUST 单列，因为 `srv-points` 的主业务路径 `ConsumeOrderCompleted` **不经 HTTP** ——
同步路由矩阵覆盖不到它，而它恰是该域唯一写入积分流水的路径。

#### Scenario: 消费 span 与下单请求同 trace

- **WHEN** `POST /api/order` 返回 2xx，并等待消费链路完成
- **THEN** 能取到含 `service.points.ConsumeOrderCompleted` 的 trace，且其 traceID
  与下单 HTTP trace 一致；`points_log` 新增一行且 `order_id` 等于新建订单 ID

#### Scenario: 消费 span 缺失不得被当作通过

- **WHEN** `srv-points` 的 `MQConsumer()` 为 nil（消费循环未起）
- **THEN** 异步链断言 FAIL 并给出「消费侧无 span」的原因；
  MUST NOT 因为同步路由全绿而跳过该条

### Requirement: 验收证据以矩阵形式落盘

`examples/mesh/docs/verify.md` 的 T13 段 SHALL 含：

- 路由矩阵表（六条路由 × HTTP 状态 × 目标域 service span × sidecar 跳）；
- 每条路由的请求命令原文（含 `POST /order` 的 body）；
- 异步链的 trace-id 与消费侧 span 名；
- 版本口径（minikube / kubectl / istioctl / Jaeger）与结论行（PASS/FAIL）。

MUST NOT 只贴「一条 URL 的原始输出」—— 单条路径的证据无法支撑「各域业务逻辑都被触达」
这一结论。

#### Scenario: 矩阵可被第三方复跑

- **WHEN** 另一人按 `verify.md` 的命令原文在本机执行
- **THEN** 得到同形的矩阵（trace-id 可不同，各格 PASS/FAIL 一致）
