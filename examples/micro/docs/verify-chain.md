# 全链路实测证据（gateway → bff → 四域服）

落盘时间：2026-10-06。所有 trace 都是**真机跑出来的**，不是设计图。

- 证据来源：Jaeger（`http://127.0.0.1:16686`），OTLP/HTTP `127.0.0.1:4318`
- 驱动方式：`bash examples/micro/scripts/chain.sh`（curl 打网关 HTTP 面）
- 判据脚本：`python3 examples/scripts/trace-audit.py --service demo-gateway,demo-bff,demo-product,demo-order,demo-stock,demo-points --lookback 30m` → **PASS**

## 1. 环境

| 项 | 值 |
|---|---|
| gateway | `demo-gateway` HTTP `127.0.0.1:8080`（`/api/*` 剥前缀转 bff） |
| bff | `demo-bff` HTTP `127.0.0.1:8081` |
| 商品/订单/库存/积分 | `demo-product :9021` / `demo-order :9022` / `demo-stock :9023` / `demo-points :9024` |
| 注册中心 | consul `127.0.0.1:8500`（六进程里五个注册，gateway 只发现不注册） |
| 存储 | mysql `3306`（product/order/stock/points 四库）、redis `6379`（cachex db0 / kvx db1 / lockx）、kafka `9092` |

## 2. 八条路径的证据索引

| # | 路径 | traceID（前 12 位） | span 数 | 涉及服务 | 数据层 span |
|---|---|---|---|---|---|
| ① | `GET /api/ping` | `2eff129e2c41` | 6 | gateway + bff | —（不触达下游） |
| ② | `GET /api/product/1` 缓存**未命中** | `74a77c796a52` | 24 | + product | `db.query`×1 + `db.redis.GET/SET` + `db.kvx.INCR/SET` |
| ③ | `GET /api/product/1` 缓存**命中** | `4a58d560981a` | 19 | + product | 只有 `db.redis.GET` + `db.kvx.INCR`，**无 db.query** |
| ④ | `GET /api/stock/1` | `0236fc0257f4` | 23 | + stock | `db.query`×1 + redis/kvx |
| ⑤ | `POST /api/order` **复合下单成功** | `3b49ebe80350` | 90 | + 四域（另见 §5） | `db.create`×9 / `db.query`×3 / `db.update`×1 / `lock.redis.*`×6 / `messaging.*`×6 |
| ⑥ | `GET /api/order/12` | `caff1f6973e0` | 21 | + order | `db.query`×1 + redis/kvx |
| ⑦ | `GET /api/points/1001` | `998899766821` | 21 | + points | `db.query`×1 + redis/kvx |
| ⑧ | `GET /api/product/99999999` 领域失败 | `e9ac58dd8f72` | 21 | + product | `db.query`×1（查不到，bff 侧标 domain error，HTTP 404） |
| ⑨ | `POST /api/order` **补偿路径** | `43983186dcb7` | 64 | product + stock | 见 §4 |

## 3. 主证据：复合下单 `3b49ebe80350`（90 span，单根，树深 16）

```
[demo-gateway] POST /api/order                      webx otel SERVER（根 span）
   [demo-gateway] coord.discover                    发现 demo-bff（consul）
   [demo-gateway] gateway.proxy.api                 转发 span（记 upstream/target/path/status）
      [demo-gateway] HTTP POST                      otelhttp CLIENT（出向语义：方法/URL/状态码）
         [demo-bff] POST /order                     webx otel SERVER（traceparent 续上）
            [demo-bff] handler.PlaceOrder           业务编排根
               [demo-bff] downstream.product.GetProduct
                  [demo-bff] product.Product/GetProduct        kitex CLIENT（rpcx suite）
                  [demo-bff] client.demo-product.GetProduct    rpcx ClientSpanMiddleware
                     [demo-product] product.Product/GetProduct kitex SERVER（suite）
                        [demo-product] rpc.product.GetProduct        ← 传输适配
                           [demo-product] route.product.GetProduct   ← 路由（读写语义）
                              [demo-product] middleware.product.audit    ← 审计（链首）
                                 [demo-product] middleware.product.rate_limit ← 限流（链尾）
                                    [demo-product] db.kvx.INCR            限流计数
                                    [demo-product] handler.product.GetProduct
                                       [demo-product] service.product.GetProduct
                                          [demo-product] db.kvx.INCR      热度计数
                                          [demo-product] db.redis.GET      缓存命中即到此为止
               [demo-bff] downstream.stock.ReserveStock
                  … [demo-stock] rpc → route → middleware.audit → middleware.rate_limit
                     → handler.stock.ReserveStock → service.stock.ReserveStock
                        → db.kvx.GET（幂等键）→ lock.redis.acquire
                        → repository.stock.ReserveTx → db.update + db.create×3 + db.query
                        → db.redis.DEL（失效）→ lock.redis.release
               [demo-bff] downstream.order.CreateOrder
                  … [demo-order] rpc → route → audit → rate_limit → handler
                     → service.order.CreateOrder → lock.redis.acquire
                     → repository.order.CreateOrderTx → db.create×3（orders+items+outbox）
                     → db.kvx.INCR → lock.redis.release
               [demo-bff] downstream.points.Earn
                  … [demo-points] rpc → route → audit → rate_limit → handler
                     → service.points.Earn → db.kvx.EXISTS（幂等键）→ lock.redis.acquire
                     → repository.points.ChangeTx → db.query×2 + db.create×3
                     → db.kvx.SET → db.redis.DEL → lock.redis.release
                     → messaging.outbox.send → messaging.kafka.send
```

**一层不缺**：gateway 三跳（discover / proxy / HTTP CLIENT）+ bff 三跳（handler / downstream / kitex CLIENT）
+ 每个域服六跳（rpc / route / audit / rate_limit / handler / service / repository）+ 数据层，
全在同一根 trace 上。

## 4. 补偿路径 `43983186dcb7`（停掉 srv-order 注入建单失败）

```
[demo-bff] handler.PlaceOrder
   ├── downstream.product.GetProduct     → 成功（商品域六层齐全）
   ├── downstream.stock.ReserveStock     → 成功（lock + 3 表事务 + outbox）
   ├── downstream.order.CreateOrder      → 只有 kitex CLIENT span，**没有 SERVER span**（下游不可达）
   └── handler.ReleaseStock.compensate   ← biz.compensation=true
       └── downstream.stock.ReleaseStock → demo-stock 六层齐全
           └── service.stock.ReleaseStock → repository.stock.ReleaseTx（db.query+update×2+create）
               → db.redis.DEL → messaging.outbox.send → messaging.kafka.send
```

补偿**也在同一根 trace 上**，且带 `biz.compensation=true` 属性 —— 排查时可以从失败点
整段回放，不用去日志里拼。复现方式：

```bash
kill $(lsof -nP -iTCP:9022 -sTCP:LISTEN -t | head -1)   # 停 srv-order
bash examples/micro/scripts/chain.sh --compensate        # 期望 502 + 补偿 span
```

## 5. 异步边界：outbox 续链（含一处跨 demo 现象）

`3b49ebe80350` 这条 trace 里出现了 3 组 `messaging.outbox.send → messaging.kafka.send`，
父 span 分别是各域的 `service.*`（行内 `traceparent` 续链），说明**异步补发也挂在同一条 trace 上**。

其中两组的服务名是 `srv-order` / `srv-stock` —— 那是 **examples/mall 的进程**：

- micro 与 mall 共用了同一批库（`order_db` / `stock_db` / `points_db`）与同一张 outbox 表；
- mall 的轮询器扫到 micro 写的事件行，就替它发了（父 span 仍正确指向 micro 的 `service.*`）；
- 反向同理：micro 也会发 mall 的行。

**这是 demo 共库带来的串扰，不是 launchx 缺陷**（续链本身是对的）。要彻底分开，
两个 demo 必须各用一套库名或各自的 outbox 表名。跑单侧验收时把另一侧停掉即可。

## 6. 层内执行路径分层（同一层不同分支在 trace 上可区分）

| 层 | 分支 | 判读依据（span 差异） |
|---|---|---|
| service（读） | 缓存命中 | 只有 `db.redis.GET`，**无** `repository.*` / `db.query`（见 `4a58d560981a`） |
| service（读） | 缓存未命中 | `db.redis.GET` → `repository.*` → `db.query` → `db.redis.SET` 回填（`74a77c796a52`） |
| service（写） | 事务 + outbox | `repository.*Tx` → `db.create`×N（业务行 + outbox 行同事务） |
| service（写） | 加锁 | `lock.redis.acquire` … `lock.redis.release` 成对出现（库存/订单/积分） |
| service（幂等） | 重放命中 | `db.kvx.GET` / `db.kvx.EXISTS` 命中后直接返回，无 db 写入 |
| middleware | 限流触发 | 只有 `db.kvx.INCR` 就返回，下面**没有** `handler.*`（本轮未触发） |
| handler（bff） | 领域失败 | span 上记 `code != 0`，HTTP 侧映射 404/409，**不是** RPC error |
| handler（bff） | 补偿 | `handler.ReleaseStock.compensate` + `biz.compensation=true` |

## 7. 本轮顺带修的一处判据缺陷

`examples/scripts/trace-audit.py` 的 R4 原先把「SQL 文本非空」套在**所有** `db.*` span 上，
而 `db.redis.*` / `db.kvx.*` 是 redis 命令、压根没有 SQL 文本 —— 结果微服形态每条 trace
都被判 FAIL（限流必走 kvx、读路径几乎都先打 cachex）。已把 R4 收窄到 SQL 四类
（`db.query/create/update/delete`），R5（不得直接挂根）仍对全部 `db.*` 生效。改完 8 条
业务 trace 全 PASS。

**判据失真比漏判更糟**：一条永远 FAIL 的判据，两次运行都"通过"也证明不了任何事。

## 8. 已知缺口

- CRUD 覆盖缺 `delete`：bff 未暴露删除路径，域服侧有 `Delete` 方法但链路到不了。
  要补齐得在 bff 加一条 `DELETE /product/:id`。
- 限流拒绝分支未实测触发：四域阈值 120/min，压测成本高于收益；代码路径与 span 已就位。
