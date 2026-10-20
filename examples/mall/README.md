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
├── pkg/                      四服共享（跨进程的公共件）
│   ├── codes/                错误码（2xxx 商品 / 3xxx 订单 / 4xxx 库存 / 5xxx 积分）
│   │                         以及 BizError：分层之间用 error 带码上浮
│   ├── events/               跨域事件体（product.created / order.completed /
│   │                         stock.reserved / points.changed）
│   ├── handlecheck/          装配期断言句柄来源（确认 *gorm.DB 来自 dbx）
│   └── routing/              路由表 + 中间件链（Audit / RateLimit，span 嵌套）
├── pkg/kitex_gen/            四域 RPC 契约（由 scripts/gen_kitex_stub.py 生成）
├── scripts/
│   ├── gen_kitex_stub.py     契约桩生成器（本机没装 kitex CLI，脚本可复现）
│   └── schema.sql            四库建表 + 种子数据
├── srv-product/              商品域  127.0.0.1:9101  product_db
├── srv-order/                订单域  127.0.0.1:9102  order_db
├── srv-stock/                库存域  127.0.0.1:9103  stock_db
├── srv-points/               积分域  127.0.0.1:9104  points_db
├── srv-bff/                  聚合层  127.0.0.1:9201  复合下单
└── srv-gateway/              网关    127.0.0.1:9200  /api/* 剥前缀转发
```

**采集口（`/metrics`）落在哪，取决于该档有没有 HTTP 面** —— 这是同一套 `metrics`
开关的两种落法：

| 档 | 进程 | 业务口 | 采集口 |
|---|---|---|---|
| srv | `srv-product` / `srv-order` / `srv-stock` / `srv-points` | 9101 / 9102 / 9103 / 9104 | **9133 / 9131 / 9134 / 9132**（独立监听） |
| bff | `srv-bff` | 9201 | 复用 9201 |
| gateway | `srv-gateway` | 9200 | 复用 9200 |

- gateway / bff 有 webx → 直接挂在既有 HTTP 口上（`webx.MetricsPrefix`，缺省 `/metrics`），
  **不新开端口**；
- 四个域服是 `srv` 档、没有 HTTP 面 → 由 `metrics.addr` 单起一个只服务 `/metrics` 的监听
  （9131-9134）。漏配 `addr` 而 `enabled=true` 时**装配期即退**：否则指标会被采集却无人能读，
  且不报错。

RPC 口（9101-9104）**没有 HTTP 面**，拿 `curl` 打它只会得到
`KITEX: invalid payload` —— 那是预期行为，不是故障。

四服共用的另外两件不在 `pkg/` 里，它们已经是 SDK 的一部分（2026-10-07 上提）：

| 能力 | 位置 | 为什么在 SDK 而不是 demo |
|---|---|---|
| 事务发件箱 | `github.com/gospacex/outboxx` | 落库 + 3s 补发是对一个 gorm 表做的簿记，各服务语义必须逐字一致；抄多份的代价是语义漂移（重试上限 / 空 payload / 轮询 ctx 是否入链）。它**不能**进 launchx 库源码：库有「不得直引 gorm」的硬守卫（`dependency_surface_test.go`） |
| 业务分层埋点 | `github.com/gospacex/otelx/layer` | `layer.Begin(...)` 的 `layer.<entity>.<op>` 命名与 quiet 豁免是跨示例约定，且它只依赖 otel 核心三项，故做成 otelx 的子包而非根包（根包要链整条导出栈） |

两者的 tracer 都和 dbx / mqx 一样向 otel 全局取 —— 全局由 launchx 的 telemetry
步骤在装配最前注册（`internal/telemetry/telemetry.go`），业务侧不需要也不应该
再交接一次 provider。

每个 srv-\* 内部结构完全一致（六层 + model）：

```
srv-<domain>/
├── cmd/              进程入口（只做 launchx.Run + 装配回调注册）
├── app/              装配层：取 launchx 句柄 → 断言来源 → 串起六层
└── internal/
    ├── rpc/          传输适配：唯一的 kitex 接触面，开 rpc.<entity>.<Method> span
    ├── route/        路由表：方法 → 读写语义 + 中间件链
    ├── middleware/   域内中间件（kvx 固定窗口限流，fail-open）
    ├── handler/      协议适配：pb ↔ model，codes.Error → 响应体 Code
    ├── service/      业务编排：校验 / 锁 / 缓存 / 事务边界（不 import pb）
    ├── repository/   持久层：多表 JOIN 与多表事务（只用 dbx 句柄）
    └── model/        数据模型：表结构 + 领域 DTO，零依赖
```

## 2. 跑起来

```bash
export MYSQL_ROOT_PW=<底座 mysql root 口令>    # 必填，见 docker-compose.yml
export HUBX_DB_PW=hubx123                      # 应用账号 mall_rw 的口令

# 四库 DDL + 应用账号。⚠️ 别图省事写成 `mysql ... < scripts/schema.sql`：
# 那份 SQL 里口令是 ${HUBX_DB_PW:-...} 占位，重定向不经过 shell 展开，
# 占位会被当成口令灌进库 —— 账号照样建成功，应用却连不上（ERROR 1045，
# 且报错完全看不出是灌库姿势的问题）。schema.sh 负责渲染 + 灌 + 灌完用
# 应用账号真连一次验证。
bash examples/scripts/schema.sh apply

# 中间件：mysql:3306 / redis:6379 / kafka:9092 / consul:8500 / jaeger:4318

cd examples/mall/srv-product && go run ./cmd   # 另开终端依次跑其余三个
cd examples/mall/srv-bff     && go run ./cmd   # 聚合层（9201，HTTP 入口）
cd examples/mall/srv-gateway && go run ./cmd   # 网关（9200，/api/* 剥前缀转发）
```

自检采集口 —— srv 档是**独立口**，业务口上打不到（见 §1 采集口表）：

```bash
curl -s 127.0.0.1:9131/metrics | grep rpcx_requests_total   # srv-order（独立口）
curl -s 127.0.0.1:9201/metrics | grep webx_requests_total   # srv-bff（复用 HTTP 口）
```

⚠️ **跑 `srv-gateway` 前先腾出 9200**：宿主 `9200` 默认被 `elasticsearch` 容器占用
（ES 的默认端口，与网关选定的 9200 撞车）。占用者显示为 Docker 的端口转发进程
（`lsof -nP -iTCP:9200 -sTCP:LISTEN` 给出 `com.docke`），`ps` 里找不到对应进程 ——
只看报错会对不上号。处置：`docker stop elasticsearch`，跑完 `docker start elasticsearch` 复原。
（同类：`etcdkeeper` 占宿主 8081，与 `examples/micro` 的 bff 撞车。）

## 3. 四域能力与 SDK 句柄对照

| 域 | 单表 CRUD | 多表（JOIN / 事务） | 用到的 launchx 句柄 |
|---|---|---|---|
| **商品** | `categories` 存在性校验 | 建商品 4 表事务（products + skus + logs + outbox）；改商品 2 表事务；上下架 3 表事务（联动 SKU 可售）；列表 2 表 JOIN（⋈categories 带类目名）；详情 3 表 JOIN；报表 4 表 JOIN | DB / Cache（详情缓存）/ MQ（outbox）/ KV（限流 + 热度）/ Lock（上下架互斥）/ Registry |
| **订单** | — | 下单 3 表事务（orders + items + outbox）；状态流转多表事务（+status_log）；列表 2 表 JOIN（⋈order_items 聚合明细条数与金额）；详情 2 表 JOIN；报表 4 表 JOIN | DB / Cache / MQ / KV（限流）/ Lock（同用户下单互斥） |
| **库存** | — | 预占 / 释放 / 确认 / 调整多表事务；快照与列表 2 表 JOIN（⋈stock_reservations 聚合未释放预占量）；报表 3 表 JOIN | DB / Cache / MQ / KV（限流 + 失败熔断）/ Lock（**预占互斥，防超卖**） |
| **积分** | — | 入账 / 消费幂等多表事务（accounts + ledgers + outbox）；账户 2 表 JOIN（⋈point_ledgers 聚合笔数与最近变动）；流水 2 表 JOIN（⋈point_accounts 带余额）；报表 2 表 JOIN | DB / Cache / MQ / KV（限流 + **幂等键**）/ Lock（同用户互斥） |

多表操作全部挂在分层链的**第一条可达路径**上（`rpc → route → middleware →
handler → service → repository`），不是"只写在 repository 里没人调"的死代码。

## 3.5 分层链与 span 树

一次 RPC 进来的完整 span 树（以查询为例）：

```
rpc.<entity>.<Method>
└── route.<entity>.<Method>           漏登记的方法会 Warn 且不带中间件
    └── middleware.<entity>.audit      写 span 属性 + 入口日志，恒放行
        └── middleware.<entity>.rate_limit   kvx 固定窗口，拒绝短路
            └── handler.<entity>.<Method>    pb ↔ model；codes.Error → 响应体
                └── service.<entity>.<Op>    校验 / 锁 / 缓存 / 事务边界
                    └── repository.<entity>.<Op>
                        ├── db.query（dbx gorm 回调，每条 SQL 一个 span）
                        └── cache / kv / lock 各自的 SDK span
```

中间件 span 刻意嵌套而非平铺：平铺时所有中间件耗时都叠在 route 自身耗时里，
看不出是限流慢还是审计慢。

`service` 层**不 import pb / kitex_gen** —— pb 加字段不该顺着 import 渗进业务
规则；pb 与领域模型的转换只在 `handler` 一层发生。业务失败用 `codes.Error`
带码上浮，`handler` 统一翻译成响应体；非业务错误（超时 / 不可达）直接抛回
kitex 走错误体，不翻译成业务码。

## 3.6 句柄来源断言

`pkg/handlecheck.VerifyDBX(db)` 在装配期确认 `*gorm.DB` 确实是 launchx 的 dbx
交出来的，判据是 dbx 注册的 `dbx:trace_before_{create,query,update,delete}`
gorm 回调在不在。自己 `gorm.Open` 一个照样能查能写，唯独 SQL 不进链路 ——
这种退化是静默的，所以要让它**起不来**而不是等排障时发现。

仅在 `trace.on.db` 为真时验：`enable_trace=false` 时 dbx 走 `OpenNoTrace`，
句柄上没有这些回调，那是配置选择而不是用错句柄。

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
