# 微服形态示例（examples/micro）

gateway → bff → **四个域服** 的完整链路样板：所有数据操作都走 launchx 初始化好的
hubx SDK 句柄，跨进程靠 RPC 双边的 trace suite 续链，Jaeger 上一条 trace 能看到全程。

## 1. 拓扑与端口

| 角色 | 目录 | 服务名 | 地址 | 说明 |
|---|---|---|---|---|
| 网关 | `srv-gateway`…（`gateway/`） | `demo-gateway` | `127.0.0.1:8080` | 流量入口，`/api/*` 剥前缀转给 bff |
| 聚合 | `bff/` | `demo-bff` | `127.0.0.1:8081` | HTTP 面 + 四个下游 kitex 客户端 |
| 商品域 | `srv-product/` | `demo-product` | `127.0.0.1:9021` | 商品 CRUD、上下架、四表 JOIN 报表 |
| 订单域 | `srv-order/` | `demo-order` | `127.0.0.1:9022` | 下单三表事务 + outbox、订单报表 |
| 库存域 | `srv-stock/` | `demo-stock` | `127.0.0.1:9023` | 预占/释放/确认（加锁 + 三表事务） |
| 积分域 | `srv-points/` | `demo-points` | `127.0.0.1:9024` | 入账/消费（幂等键 + 两表事务） |

端口段：`90xx` 是 RPC（kitex），`8080/8081` 是 HTTP。

## 2. 分层链（每个域服内部）

四个域服的内部结构完全一致，`srv-product / srv-order / srv-stock / srv-points`
只是换了业务实体。一次 RPC 进来要穿过六层：

```
route → middleware → handler → service → repository → model
  ↑
  rpc（kitex 传输适配，唯一认识协议的一层）
```

| 层 | 包 | 职责 | span 名 |
|---|---|---|---|
| 传输适配 | `internal/rpc` | 开 rpc span、走路由、唯一的 kitex 接触面 | `rpc.<entity>.<Method>` |
| 路由 | `internal/route` | 方法 → 读写语义 + 中间件链（漏登记会告警） | `route.<entity>.<Method>` |
| 中间件 | `internal/middleware` + `pkg/routing` | 审计属性 + kvx 固定窗口限流（fail-open） | `middleware.<entity>.audit` / `.rate_limit` |
| 协议适配 | `internal/handler` | pb ↔ model 转换、`codes.Error` → 响应体 Code | `handler.<entity>.<Method>` |
| 业务编排 | `internal/service` | 校验、锁、缓存、事务边界；**不 import pb** | `service.<entity>.<Op>` |
| 持久 | `internal/repository` | 多表 JOIN、多表事务，只用 dbx 句柄 | `repository.<entity>.<Op>` |
| 数据模型 | `internal/model` | 表结构与领域 DTO，零依赖（被上面所有层依赖） | — |

中间件 span 是**嵌套**的（`route → audit → rate_limit → handler`），不是平铺：
平铺时所有中间件的耗时都叠在 route 自身耗时里，看不出是限流慢还是审计慢。

层与层之间的依赖方向只有一条：`rpc → handler → service → repository → model`。
service 不认识 pb（pb 加字段不会渗进业务规则），handler 不做业务校验（规则
不会在两层各长一份然后漂移）。

## 3. 全链路形态

> 下面这棵树的**实测版**（含 traceID、span 数、各路径对照与补偿路径）在
> `docs/verify-chain.md`；一键复跑：`bash scripts/chain.sh`。

```
curl /api/order/:id
└── demo-gateway  GET /api/order/:id           (SERVER, webx otel 中间件开根 span)
    └── gateway.proxy.mall                     转发 span + HTTP CLIENT span（otelhttp）
        ↓ traceparent 经 HTTP 头注入
        demo-bff  handler.GetOrder
            └── downstream.order.GetOrder
                └── client.demo-order.GetOrder  (kitex ClientSpanMiddleware)
                    ↓ traceparent 经 kitex 元数据注入
                    demo-order
                    └── rpc.order.GetOrder
                        └── route.order.GetOrder
                            └── middleware.order.audit
                                └── middleware.order.rate_limit
                                    └── handler.order.GetOrder
                                        └── service.order.GetOrder
                                            └── repository.order.GetOrder
                                                └── db.query（dbx gorm 回调）
```

复合下单 `POST /api/order` 一条 trace 会串起四服：
`demo-product → demo-stock → demo-order → demo-points`；
下单失败时 bff 会补一次 `demo-stock.ReleaseStock`（span 属性
`biz.compensation=true`，排查时可整段回放）。

## 5. 起停

```bash
# 中间件底座（consul + jaeger + mysql/redis/kafka）
/Applications/Docker.app/Contents/Resources/bin/docker compose \
  -f /Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml up -d

export HUBX_DB_PW=hubx123
for d in product order stock points; do (cd srv-$d && go run ./cmd &); done
(cd bff     && go run ./cmd &)
(cd gateway && go run ./cmd &)
```

建表用 `examples/mall/scripts/schema.sql`（四库 DDL + outbox 同构表）。

## 6. 纪律

- 业务代码不出现任何硬编码地址：端口、DSN、注册中心、上游兜底全在各自
  `config.yaml`；换形态只改 `service.mode`。
- 不自建连接：`a.DB()` / `a.Cache()[name]` / `a.MQ()` / `a.KV()[name]` /
  `a.Lock()` / `a.RPCServerOptions()` / `a.RPCClientOptions(name)` 取句柄。
- **句柄来源有断言**：装配期 `pkg/handlecheck.VerifyDBX(db)` 会验 `*gorm.DB`
  确实是 launchx 的 dbx 交出来的（判据是 dbx 注册的 `dbx:trace_before_*` gorm
  回调在不在）。自己 `gorm.Open` 一个照样能查能写，但 SQL 不进链路 —— 这种
  退化是静默的，所以要在启动期直接失败而不是等排障时发现。
  仅在 `trace.on.db` 为真时验：`enable_trace=false` 时 dbx 走 `OpenNoTrace`，
  没有这些回调是配置选择，不是用错句柄。
- 入链开关：`trace.enabled` 是总闸，`trace.on.<组件>` 只裁决组件级 span 是否入链；
  RPC 双边的 suite 随选项注入，不经该开关裁决。
- **连表与事务**：每域除单表 CRUD 外都带多表操作，且都经分层链的第一条
  路径可达，不是"只写在 repository 里没人调"：

  | 域 | 多表联查 | 多表事务 |
  |---|---|---|
  | 商品 | `ListProducts` 2 表（⋈categories）、`GetProduct` 3 表、报表 4 表 | 建商品 4 表 + outbox、改商品 2 表、上下架 3 表（联动 SKU 可售） |
  | 订单 | `ListOrders` 2 表（⋈order_items 聚合）、`GetOrder` 2 表、报表 4 表 | 建单 3 表（orders + items + outbox）、改状态多表 |
  | 库存 | `GetStock` / `ListStocks` 2 表（⋈stock_reservations 聚合）、报表 3 表 | 预占/释放/确认/调整多表事务 |
  | 积分 | `GetAccount` 2 表（⋈point_ledgers 聚合）、`ListLedgers` 2 表（⋈point_accounts）、报表 2 表 | 入账/消费幂等多表事务 |
