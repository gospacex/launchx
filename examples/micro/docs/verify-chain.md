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
| ⑨ | `POST /api/order` **补偿路径** | `43983186dcb7` | 64 | product + stock | 见 §4（**故意注入的失败**，不是链路故障） |

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

## 4. 补偿路径 `43983186dcb7` / `950f07cc2817180d4d`（停掉 srv-order 注入建单失败）

> ⚠️ **这一节是「故意让它失败」的证据，不是链路故障。** 网关在这里返回的
> **502 是正确行为**（下游真的不可达）。把本节与 §3 对照着看，不要单独拿去判
> 「链路通没通」—— 二者长得完全不一样：本节的 trace 里**只有 4 个服务**
> （无 `demo-order`）、带 `biz.compensation=true` 标记；§3 是 6 个服务、零错误。
> 判读口径见 §9。

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

## 9. 复验（2026-10-06 17:2x）：三类 trace 的形态对照

同一套进程，三种场景各取一条 trace，**形态互不相同**，这是判读的基准：

| 场景 | traceID | span | 参与了几个服务 | error span | 识别标记 |
|---|---|---|---|---|---|
| **主链路**（healthy `POST /api/order`） | `e91626f74db710d00f` | 88 | **6**（gateway+bff+product+order+stock+points） | **0** | — |
| 主链路（同上，另一条） | `1e72dea7b50fab1b7c` / `ef3319999709f9e27a` | 89 | 6 | 0 | — |
| **补偿路径**（停 srv-order 后下单） | `950f07cc2817180d4d` | 66 | **4**（gateway+bff+product+stock，**无 demo-order**） | 8 | `biz.compensation=true` |
| **客户端身份错误**（`GET /api/order/1` 不带 `user_id`） | `27ddbd76003240566e` | 19 | 3（gateway+bff+order） | 4 | HTTP **401**，非 502 |

判据：**「服务数 < 6」+「有 compensation 标记」= 我注入的补偿路径**；服务数 6 且
error 0 = 链路正常。仅凭「网关有 502」不足以判断链路故障 —— 见 §10。

复验用的八条路径全部通过（`chain.sh` 8/8，含新单 `order_id=14/15/16`），
`trace-audit.py --service demo-gateway,demo-bff,demo-product,demo-order,demo-stock,demo-points`
仍 PASS。

## 10. 修正：客户端错误曾被伪装成 502（误诊来源）

**症状**：`GET /api/order/1`（不带 `user_id`）经网关返回 **502** `{"error":"user required"}`，
看起来像"下游挂了 / srv 不通"，实际只是请求缺身份字段。

**根因**：`examples/micro/bff/internal/handler/mall.go` 的 `statusOf()` 只映射了
NotFound / RateLimited / 冲突 / 参数非法几类，**漏了 `CodeUnauthorized`(1004)**，
于是落进 `default: http.StatusBadGateway` —— 客户端错误被渲染成网关级故障。

**修法**：补齐语义确定的映射，并把「谁错映射谁」写进函数注释。

| 领域码 | 修前 | 修后 |
|---|---|---|
| `CodeUnauthorized` 1004 | 502 | **401** |
| `CodeInternal` 1003 / 三处 `WriteFailed` | 502 | 500 |
| `CodeOrderItemsEmpty` 3004 / `CodeProductTitleInvalid` 2006 / `CodePointsAmountInvalid` 5005 | 502 | 400 |
| `CodeOrderCancelled` 3002 / `CodeOrderStatus` 3003 / `CodeStockOverRelease` 4006 | 502 | 409 |
| `CodeStockLockFailed` 4004 / `CodeUpstream` 1006 | 502 | 429 |
| `CodeCategoryNotFound` 2005 | 502 | 404 |

**502 被收窄到只剩「下游真的不可达 / 未列出的未知码」**，于是 502 重新变得可诊断：

```bash
curl -s -w '\nHTTP %{http_code}\n' 'http://127.0.0.1:8080/api/order/1'          # 401 user required（修复后）
curl -s -w '\nHTTP %{http_code}\n' 'http://127.0.0.1:8080/api/product/99999999' # 404 product not found
# 真·502 只有这一种：停掉域服后打它
kill $(lsof -nP -iTCP:9022 -sTCP:LISTEN -t | head -1)
curl -s -w '\nHTTP %{http_code}\n' -X POST http://127.0.0.1:8080/api/order \
  -H 'Content-Type: application/json' -d '{"user_id":1001,"product_id":1,"quantity":1}'  # 502 connection closed
```

**教训**：`default: 502` 是个"什么都兜、什么都说不清"的口袋。兜得越多，
一个 4xx 就越容易被读成"基础设施挂了"，排查方向被整体带偏。

## 11. 两个环境侧发现（会影响复验，不影响 launchx 本身）

**① 注册实例 ID 撞名连坐**：实例 ID 固定为 `<服务名>-<host>-<port>`（`app.go` 的
`stepRegister`），因此**同机同端口的两个进程共用一个 ID**。旧的 bff 进程退出时走
`coord.deregister`，会把新实例刚注册的条目一起摘掉 —— 现象是"进程在监听、consul 里
`[]`、日志里却明明打过 `launchx: service registered`"。

- 复现：连续重启两次 bff，中间用 `curl /v1/health/service/demo-bff` 观察；
- 单机 demo 的处置：确认 consul 里是 `[]` 时**重启一次**即可恢复（新起的会重新注册）；
- 多实例部署要注意：同一台机器上同端口的两个实例不会出现，但**滚动发布时旧实例的
  退出会摘掉同名条目**，需要实例 ID 带唯一后缀（如 PID / 随机串）才能根治。

**② 宿主 8081 被容器 `etcdkeeper` 映射**：`docker ps` 显示 `etcdkeeper 0.0.0.0:8081->8080/tcp`。
bff 绑的是 `127.0.0.1:8081`（loopback），容器绑的是 `*:8081`，在 macOS 上两者可共存，
所以链路照跑；但如果 bff 改成绑 `0.0.0.0:8081` 就会**直接起不来**（address already in use）。
复验环境里要么换端口，要么接受这个共存关系。


## 12. 复验（2026-10-07 12:0x）：Jaeger 里怎么认出这四个域服

**micro 四服的服务名是 `demo-` 前缀，不是 `srv-`** —— `srv-*` 是 `examples/mall` 那四个。
两个 demo 同机跑时，Jaeger 的 service 下拉里**两套名字都在**（实测 18 项），在搜索框里
敲 `srv` 只会筛出 mall 的四个，micro 的一个都不会出现。这是"明明跑通了却在 UI 里找不到
四个微服"最常见的原因，不是链路故障。

| 形态 | 目录 | Jaeger 服务名 | 端口 |
|---|---|---|---|
| micro | `examples/micro/srv-product` | **demo-product** | 9021 |
| micro | `examples/micro/srv-order` | **demo-order** | 9022 |
| micro | `examples/micro/srv-stock` | **demo-stock** | 9023 |
| micro | `examples/micro/srv-points` | **demo-points** | 9024 |
| micro | `examples/micro/bff` | **demo-bff** | 8081 |
| micro | `examples/micro/gateway` | **demo-gateway** | 8080 |
| mall | `examples/mall/srv-*` | `srv-order` / `srv-product` / `srv-stock` / `srv-points` | 9101-9104 |

名字之所以要分开：两套 demo 共用一个 consul，同名的两个实例会互相抢注册条目（§11 ①）。

**UI 直链**（服务名写死，不用在下拉里找）：

```
http://127.0.0.1:16686/search?service=demo-product
http://127.0.0.1:16686/search?service=demo-order
http://127.0.0.1:16686/search?service=demo-stock
http://127.0.0.1:16686/search?service=demo-points
```

**页面是早先打开的要硬刷新（Cmd+Shift+R）**：service 下拉只在页面加载时拉一次
`/api/services`，新服务首次上报后不刷新不会出现。实测核对方式：

```bash
curl -s --noproxy '*' 'http://127.0.0.1:16686/api/services'   # 应有 demo-{product,order,stock,points}
```

### 本轮复跑结果

- `bash examples/micro/scripts/chain.sh` 八条路径 8/8（新单 `order_id=1028`）
- `python3 examples/scripts/trace-audit.py --service demo-gateway,demo-bff,demo-product,demo-order,demo-stock,demo-points --lookback 30m` → **PASS**
- 六服务近 10 分钟孤儿 trace **全为 0**；主链路 `POST /api/order` 88-97 span、单根、树深 16
- 同时段 `examples/micro/srv`（旧单档 `demo-srv` :9013）**未启动**，故本轮 trace 里
  不应出现 `demo-srv`；若出现，说明有人把它起起来了（与四域共 order_db，会互相补发事件）
