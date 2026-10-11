# examples/mall 验收取证（2026-10-06 真机）

四服真机跑起来 + Jaeger 入链的**可复核证据**。所有数字来自本机实测的
Jaeger REST 响应（`GET /api/traces/<traceID>`），不是估算。

## 1. 环境

| 项 | 值 | 来源 |
|---|---|---|
| mysql | 127.0.0.1:3306（compose `mysql`，8.4） | `docker compose ps` |
| redis | 127.0.0.1:6379（compose `redis`，8.4，无密码） | 同上 |
| kafka | 127.0.0.1:9092（compose `kafka`，KRaft） | 同上 |
| consul | 127.0.0.1:8500 | `GET /v1/agent/services` |
| jaeger | 127.0.0.1:16686 / OTLP 4318（ES 持久化） | `GET /api/services` |
| Go | go1.24.3 | `go version` |
| 驱动命令 | `go run ./cmd`（四服各一份 `config.yaml`，CWD 即服务目录） | README §2 |

四库账号 `mall_rw` 与建表由 `scripts/schema.sql` 一次建齐（含账号与种子：
`categories` 三条、`stocks` product_id=1 一行）。

## 2. 怎么跑

```bash
export HUBX_DB_PW=hubx123
docker exec -i mysql mysql -uroot -prootpass < examples/mall/scripts/schema.sql
bash examples/mall/scripts/topics.sh          # 建四个事件 topic，**别跳过**（见 §4-④）

# 四个终端（或用 run_in_background 的任务），CWD 必须是各自服务目录：
cd examples/mall/srv-product && go run ./cmd   # 9101
cd examples/mall/srv-order   && go run ./cmd   # 9102
cd examples/mall/srv-stock   && go run ./cmd   # 9103
cd examples/mall/srv-points  && go run ./cmd   # 9104

# 驱动一遍（两种模式都跑通）
go run ./examples/mall/e2e --mode=direct     # 直连四端口
go run ./examples/mall/e2e --mode=registry   # 经 consul 发现（默认）
```

> **坑**：`nohup go run ./cmd &` 起的进程会随发起它的 shell 一起被回收，
> 且被回收时会走 `coord.deregister` 把**同名实例 ID 从 consul 摘掉** ——
> 后起的实例如果用了同一个 ID，会被这个迟到的 deregister 连坐。表现是
> "进程活着、端口在听，但 consul 里查不到"。用长驻任务方式起，起之前先
> `lsof -nP -iTCP:910X -sTCP:LISTEN` 确认没有残留。

## 3. 取证：`--mode=registry` 那一次

**主证据 traceID `26ec7bb48914eec6fba32522008c1313`**（11:47，registry 模式，
四个 topic 已建好后的干净一轮）

| 判据 | 实测 | 结论 |
|---|---|---|
| 根 span 唯一 | roots = 1（`mall.e2e.session`） | ✅ |
| 进程数 | 5 个 serviceName：`mall-e2e` + 四服 | ✅ |
| span 总数 | 270 | — |
| 树深 | 7 | ✅ |
| 服务发现入链 | `coord.discover` × 4 | ✅ |
| 客户端 span | `client.srv-*.<Method>` × 25 | ✅ |

**对照证据 traceID `b11475c67c770afc178326e14b25c5f2`**（11:41:25，registry
模式，topic 还没建 —— 用于对比 outbox 重试的噪声量）

| 判据 | 主证据（topic 齐） | 对照（topic 缺） |
|---|---|---|
| span 总数 | 270 | 354 |
| `messaging.*` span | 12 | 约 120 |
| outbox 结果 | product/order/stock/points 均有 `status=1` | 除订单域外全部 `status=2`（abandon） |

差额全部来自 outbox 补发的重试 —— 见 §4-④。

```json
{"traceID":"b11475c67c770afc178326e14b25c5f2","spanID":"348eaa75cc53f56f",
 "operationName":"mall.e2e.session","startTime":1791258085638672,"duration":3244046}
```

| 判据 | 实测 | 结论 |
|---|---|---|
| 根 span 唯一 | roots = 1（`mall.e2e.session`） | ✅ 没断成多串 |
| 进程数 | 5 个 serviceName：`mall-e2e` + 四服 | ✅ |
| span 总数 | 354 | — |
| 树深 | 7 | ✅ 分层完整 |
| 服务发现入链 | `coord.discover` × 4（mall-e2e 侧） | ✅ 发现也埋点了 |
| 客户端 span | `client.srv-*.<Method>` × 25 | ✅ rpcx ClientSpanMiddleware 生效 |
| 分层 | 每服都有 `service.<域>.<op>` → `db.*` / `db.redis.*` / `db.kvx.*` / `lock.redis.*` / `messaging.outbox.send` | ✅ |

各服 span 构成（按 operationName 首段归类）：

| service | db | messaging | service | rpc 入口 | lock | 合计 |
|---|---|---|---|---|---|---|
| srv-product | 29 | 20 | 7 | 7 | 1 | 64 |
| srv-order | 13 | 2 | 4 | 4 | 1 | 24 |
| srv-stock | 45 | 40 | 8 | 8 | 2 | 103 |
| srv-points | 29 | 40 | 6 | 6 | 2 | 83 |

### 单表 CRUD 与多表的落点（trace 里逐条可见）

| 步骤 | 落点 span |
|---|---|
| 建商品 | `service.product.CreateProduct` → `db.query`(类目) + `db.create`×4（products/skus/logs/outbox） |
| 查商品（miss） | `db.redis.GET` → `db.query` → `db.redis.SET` |
| 查商品（hit） | 只有 `db.redis.GET`，无 `db.query` —— 缓存命中可肉眼确认 |
| 改商品 | `db.update` + `db.create`(logs) + `db.redis.DEL` |
| 上下架 | `lock.redis.acquire` → `db.update` + `db.create` + `db.redis.DEL` |
| 商品报表（4 表 JOIN） | `service.product.Report` → `db.query` ×1 |
| 预占库存 | `lock.redis.acquire` → `db.update`(stocks) + `db.create`×2（reservations/logs）+ `db.create`(outbox) |
| 确认 / 释放 | `db.update`×2 + `db.create`(logs) |
| 下单 | `lock.redis.acquire` → `db.create`×3（orders/items/outbox） |
| 查订单（2 表 JOIN） | `db.redis.GET` → `db.query` → `db.redis.SET` |
| 积分入账 | `db.kvx.EXISTS`(幂等) → `lock.redis.acquire` → `db.query`×2 → `db.update` + `db.create`×2 |
| 积分重复入账 | 只有 `db.kvx.INCR` + `db.kvx.EXISTS`，**无 lock、无 db.update** —— 幂等快路径生效 |

驱动输出全绿（26 步全 `[ OK ]`，退出码 0）。

## 4. 顺带查实的三件事

**① 库存域没有"建库存"入口。** `AdjustTx` 是纯 UPDATE，
`RowsAffected==0` 时按 `classifyMiss` 区分"没记录"(4001) 与"不够扣"(4002)。
所以新建商品不会自动有库存行 —— e2e 里库存域固定打 `schema.sql` 种好的
`product_id=1`。跨域只存 ID，两域用不同 ID 不影响链路；但**示例确实缺一个
"商品创建 → 库存初始化"的落地路径**（product.created.v1 仍只有生产端）。

> 补记（2026-10-11）：四个事件里 `order.completed.v1` 已补上消费端
> （srv-points 消费 → 下单送积分，见 `srv-points/internal/consumer`）。
> 其余三个（product.created / stock.reserved / points.changed）仍只有生产端。

**② 一张预占单只能结算一次。** `ReleaseTx` 与 `ConfirmTx` 都只认
`status=ReserveReserved` 的流水，且各自把整行翻成终态（3/2）。对同一
`order_no` 先释放再确认，第二步必报 `4005 reservation not found` ——
这是设计而非 bug，e2e 已改成两条独立终态路径（各用一个 order_no）。

**③ outbox 补发会往"老 trace"上持续追加 span。** 补发器用行里存的
`traceparent` 续链，于是历史上每次下单/建商品的 trace 会随时间不断长出新
的 `messaging.outbox.send` → `messaging.kafka.send`。看单条 trace 时先按
`startTime` 排序，别把补发 span 误当成同步调用链的一部分。

**④ 事件 topic 必须先建，否则事件静默 abandon（本次实测最严重的一条）。**

compose 的 kafka 虽然开了 `KAFKA_CFG_AUTO_CREATE_TOPICS_ENABLE=true`，但
自动建 topic 与首次 Send 是竞态的 —— sarama 第一次投递就撞：

```
ERROR kafka/kafka.go:284 kafka send failed
  {"error":"[3] Unknown Topic Or Partition: the request is for a topic
    or partition that does not exist on this broker","topic":""}
WARN  outbox/outbox.go:193 outbox: send failed, will retry {"id":4}
...
WARN  outbox/outbox.go:219 outbox: retry cap reached, abandoned for manual reconcile {"count":1}
```

outbox Runner 上限 10 次 × 3s，攒够就把行置 `status=2`。**业务侧完全无感**：
建商品/下单/预占全部返回 OK，事件却永远没出域。实测补建 topic 前后对比：

| 域 | topic 补建前 | topic 补建后 |
|---|---|---|
| product | 0 sent / 4 abandon | 1 sent |
| stock | 0 sent / 5 abandon | 2 sent |
| points | 0 sent / 12 abandon | 2 sent |
| order | 7 sent（topic 已存在）/ 1 abandon | 8 sent |

故 `scripts/topics.sh` 是跑通示例的**前置步骤**，不是可选项。

顺带一条判读提醒：`sendOne` 刻意不把 `ev.Topic` 写进消息（producer 由
`xmiddleware.mqx.instances.*.producer.topic` 绑定，kafka-go 会拒绝
Writer 与 Message 同时带 topic），所以错误日志里的 `"topic":""` 是消息体的
字段，**不等于**没有目标 topic —— 排查时别被它带偏。

## 5. 两种模式的差别

| 模式 | 客户端目标来源 | 实测 |
|---|---|---|
| `direct` | `client.WithHostPorts` | 26 步全绿，trace `47e5dd71e883049a2da9c2b0f2d14e1f`（350 span） |
| `registry` | consul resolver，每服 `discovery done instances=1` | 26 步全绿，trace `b11475c67c770afc178326e14b25c5f2`（354 span） |

registry 模式额外产出 `coord.discover` span，可用来确认"发现真的走了
consul"而不是悄悄回落到直连。
