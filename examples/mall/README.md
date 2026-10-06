# examples/mall —— 电商四微服示例（商品 / 订单 / 库存 / 积分）

四个**独立进程**（srv 档，kitex RPC），各自持有自己的私有库，跨域只存 ID。
本示例的存在目的是钉住两件事：

1. **数据操作一律走 launchx 初始化好的 hubx SDK 句柄**（`a.DB() / a.Cache() /
   a.MQ() / a.KV() / a.Lock() / a.Registry()`），业务代码不自建连接、
   不直引驱动、不读地址 —— 全仓库 `grep` 不到 `redis.NewClient` / `sql.Open` /
   `kafka.NewWriter` 之类的调用。
2. **单表 CRUD 与多表 JOIN/事务都在示例里有落点**，且每处注释写清"为什么
   这么做"与"降级时的故障形态"。

## 1. 目录

```
examples/mall/
├── pkg/                      四服共享（纯业务无关的分层工具）
│   ├── codes/                错误码（2xxx 商品 / 3xxx 订单 / 4xxx 库存 / 5xxx 积分）
│   ├── events/               跨域事件体（product.created / order.completed /
│   │                         stock.reserved / points.changed）
│   ├── outbox/               事务发件箱（落库 + 3s 补发，四服共用一份语义）
│   └── tracex/               业务分层埋点（service.<entity>.<op>）
├── pkg/kitex_gen/            四域 RPC 契约（由 scripts/gen_kitex_stub.py 生成）
├── scripts/
│   ├── gen_kitex_stub.py     契约桩生成器（本机没装 kitex CLI，脚本可复现）
│   └── schema.sql            四库建表 + 种子数据
├── srv-product/              商品域  127.0.0.1:9101  product_db
├── srv-order/                订单域  127.0.0.1:9102  order_db
├── srv-stock/                库存域  127.0.0.1:9103  stock_db
└── srv-points/               积分域  127.0.0.1:9104  points_db
```

## 2. 跑起来

```bash
export HUBX_DB_PW=hubx123
mysql -h127.0.0.1 -P3306 -uroot -p < examples/mall/scripts/schema.sql
# 中间件：mysql:3306 / redis:6379 / kafka:9092 / consul:8500 / jaeger:4318

cd examples/mall/srv-product && go run ./cmd   # 另开终端依次跑其余三个
```

## 3. 四域能力与 SDK 句柄对照

| 域 | 单表 CRUD | 多表（JOIN / 事务） | 用到的 launchx 句柄 |
|---|---|---|---|
| **商品** | `products` 分页列表、`categories` 存在性校验 | 建商品 3 表事务（products + skus + logs + outbox）；改商品 / 上下架 2 表事务；详情 3 表 JOIN；报表 4 表 JOIN | DB / Cache（详情缓存）/ MQ（outbox）/ KV（限流 + 热度）/ Lock（上下架互斥）/ Registry |
| **订单** | `orders` 流水只读 | 下单 3 表事务（orders + items + outbox）；状态流转 2 表事务；详情 2 表 JOIN；报表 4 表 JOIN | DB / Cache / MQ / KV（限流）/ Lock（同用户下单互斥） |
| **库存** | `stocks` 快照、分页 | 预占 3 表事务（+outbox）；释放 / 确认 3 表事务；调整 2 表事务；报表 3 表 JOIN | DB / Cache / MQ / KV（限流 + 失败熔断）/ Lock（**预占互斥，防超卖**） |
| **积分** | 账户查询、流水分页 | 入账 / 消费 2 表事务（+outbox）；报表 2 表 JOIN | DB / Cache / MQ / KV（限流 + **幂等键**）/ Lock（同用户互斥） |

## 4. 三条刻画意的取舍（改之前先看这里）

**kvx 与 cachex 分工。** `cachex.Cache` 只有 `Get/Set/Delete`，没有原子自增；
`kvx.KVStore` 有 `Incr/Decr/Exists`。所以：

- 需要原子自增的状态（限流计数、热度计数、预占失败熔断计数）→ **kvx**
- 可显式失效的派生缓存（订单/商品/库存/账户快照）→ **cachex**

两者不是重复建设。用 cachex 做计数会在并发下被读-改-写覆盖成低估，
表现为"限流器拦不住"。

**防超卖的两道闸。** 库存预占同时用 lockx（商品维度互斥）与 SQL 条件更新
（`WHERE available >= ?` + `RowsAffected`）。只留一道都会在并发下超卖：
只有锁 → 锁组件降级即超卖；只有条件更新 → 高并发下失败面从"拒绝"退化成
"抖动"。

**积分的两层幂等。** 数据库唯一键 `uk_order_no_type` 是**事实来源**；
kvx 幂等键是**快速失败**，挡在加锁与事务之前省开销。只有 kvx 层不安全
（key 丢了就重复加钱），所以 kvx 层只用于提前返回，从不作为允许写入的依据；
反之每次都查库太慢，故两层并存。

## 5. 降级姿态（四服统一）

| 句柄 | 缺失时的行为 |
|---|---|
| `a.DB()` | **fail-fast**（`strict.db: true`）—— 库连不上就起不来 |
| `a.Cache()` | 降级为直查 DB，装配期 `Warn` |
| `a.MQ()` | outbox Runner **不启动并 `Error`**（事件会无声膨胀，不能静默跳过） |
| `a.KV()` | 限流 fail-open、热度/熔断计数变 no-op |
| `a.Lock()` | fail-open + `Warn`，日志里写明后果（库存域会写"可能超卖"） |

一律不允许静默跳过：配了段却没接上，Jaeger 上的表现是 span 莫名消失且无人告警。
