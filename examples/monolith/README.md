# examples/monolith —— 单体形态 demo（验收案例 E1 / T5 / T13）

一个最小可跑的订单服务，用来证明 launchx 的**单体形态**是三段式成立的：

```go
a, err := launchx.Bootstrap("config.yaml")   // 1. 读配置 + 装配地基
a.OnSetup(app.Setup)                          // 2. 业务装配（取句柄、挂路由）
a.Run()                                       // 3. 起服务并托管退出
```

`cmd/main.go` **50 行**，只做这三件事：不开数据库连接、不装配 exporter、不注册节点。

## 1. 目录结构

```
examples/monolith/
├── cmd/main.go                  # 50 行：Bootstrap → OnSetup → Run
├── app/setup.go                 # 装配层：取各组件句柄 → 构造分层 → 挂中间件与路由
│   └── routes.go                # 唯一路由注册源
├── internal/middleware/         # 业务中间件：鉴权（经缓存查凭据）+ 限流（KV 计数）
├── internal/handler/order.go    # HTTP 层：绑参 + 状态码，自开 handler.* span
├── internal/service/order.go    # 业务层：校验 + 编排，自开 service.* span
├── internal/repository/order.go # 数据层：单表/连表/事务，自开 repository.* span
├── internal/cache/order.go      # 缓存适配：cache.* span 包住 cachex（下挂 db.redis.*）
│   └── token.go                 # 令牌存储（同一 cachex 实例，供鉴权中间件查凭据）
├── internal/ratelimit/counter.go # 限流计数：窗口自增交给 kvx 的原子 Incr
├── internal/messaging/order.go  # 消息适配：messaging.* span + traceparent 注入消息头
├── internal/search/order.go     # 检索适配：search.* span 包住 searchx（下挂 db.elasticsearch.*）
├── internal/docs/order.go       # 文档库适配：docs.* span 包住 mongodbx（下挂 db.mongodb.*）
├── internal/model/order.go      # orders / order_items / users 三表模型
├── config.yaml                  # 唯一配置源（无任何地址硬编码）
├── scripts/bring-up.sh          # 建表 + 授权 + 种子（幂等）
└── README.md                    # 本文件
```

分层边界是**硬**的，因为它直接决定 T13 的分层断言：

- `service` 不碰 `*gorm.DB`，`repository` 不做参数校验与业务决策；
- `handler` 是 hertz 唯一出现的地方，往下没有任何 HTTP 概念。

## 2. 运行

### 前置

```bash
# 1) 中间件底座（hubx/dockers 统一供给）
docker compose -f /Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml up -d mysql jaeger

# 2) 库密码（config.yaml 的 DSN 用 ${HUBX_DB_PW} 展开）
export HUBX_DB_PW=hubx123

# 3) 建表 + 授权 + 种子用户（幂等，可反复执行）
cd examples/monolith && ./scripts/bring-up.sh && ./scripts/bring-up.sh check
# 期望输出：OK tables:orders order_items users db:order_db users:2

# 建议：自动化用例走独立库 order_mono（MYSQL_DB 可覆盖），避开与 mall/micro 共库
# 时 orders 表缺 item/qty/amount 三列的串扰：
# MYSQL_DB=order_mono ./scripts/bring-up.sh
```

### 启动

```bash
cd examples/monolith            # config.yaml 按相对路径解析，必须在此目录下启动
export PATH=/Users/hyx/sdk/go1.24.3/bin:$PATH
export HUBX_DB_PW=hubx123
go run ./cmd
```

启动日志应出现 `service.mode=monolith`，且**不出现任何注册/deregister 日志**（单体不注册，验收陷阱第 11 条）。

## 3. 接口

每条链路都自带**两个业务中间件 span**（`middleware.RateLimit` 全局、
`middleware.AuthToken` 仅 `/secure` 分组）——它们都有外部依赖（缓存计数 / 缓存查
凭据），是验收矩阵里"必须入链"的项；webx 的 recovery / cors 属可豁免项，不埋点。

| 方法 | 路径 | 说明 | 链路上可见的 span |
|---|---|---|---|
| POST | `/orders` | 建单：事务写 orders + order_items | `db.create` × 2 → `messaging.PublishOrder`+`messaging.kafka.send`、`search.IndexOrder`+`db.elasticsearch.index`、`docs.SaveSnapshot`+`db.mongodb.insert` |
| GET | `/orders?limit=20` | 单表列表 | `db.query` |
| GET | `/orders/search?q=` | **检索**（ES） | `search.SearchOrder` + `db.elasticsearch.search` |
| GET | `/orders/:order_no` | 单表单条，**缓存 read-through** | `cache.GetOrder`+`db.redis.GET`、未命中则 `repository.Get`+`db.query`、回写 `cache.SetOrder`+`db.redis.SET` |
| GET | `/orders/:order_no/join` | **连表**：orders JOIN users | JOIN 语句单独成 span（`db.query.text` 含 `join`） |
| GET | `/orders/:order_no/snapshot` | **文档库**：读该单快照 | `docs.FindSnapshot` + `db.mongodb.find_one` |
| GET | `/secure/orders`（需 `X-Token`） | **鉴权**入口 | `middleware.AuthToken` + `db.redis.GET` |
| PUT | `/orders/:order_no` | 改状态（会回读一次 + 失效缓存） | `db.update` + `db.query` + `db.redis.DEL` |
| DELETE | `/orders/:order_no` | 删主子数据（+ 失效缓存） | `db.delete` × 2 |

```bash
NO=$(curl -s -XPOST localhost:8080/orders -H 'Content-Type: application/json' \
     -d '{"user_id":1,"item":"e2e","qty":2,"unit_price":9.9}' | jq -r .order_no)

curl -s "localhost:8080/orders?limit=5"        | jq 'length'      # >= 1
curl -s "localhost:8080/orders/$NO"            | jq .status       # "created"
curl -s "localhost:8080/orders/$NO/join"       | jq .user_name    # "alice"（JOIN 生效）
curl -s -XPUT "localhost:8080/orders/$NO" -H 'Content-Type: application/json' \
     -d '{"status":"paid"}'                    | jq .status       # "paid"
curl -s -XDELETE "localhost:8080/orders/$NO"   | jq .deleted      # true

# 组件链（缓存 / 检索 / 文档库 / 鉴权）
curl -s "localhost:8080/orders/search?q=e2e"   | jq 'length'      # >= 1（ES 命中）
curl -s "localhost:8080/orders/$NO/snapshot"   | jq .item         # "e2e"（Mongo 快照）
curl -s -H 'X-Token: demo-token' "localhost:8080/secure/orders" | jq 'length'
curl -s "localhost:8080/secure/orders"         # 401（无令牌）
```

> 提示：本机有透明代理时，curl 本地服务要加 `--noproxy '*'`，否则拿到的是代理的
> `upstream connect failed`，看起来像服务挂了。

## 4. 验收指引

按 `examples/ACCEPTANCE.md` 跑，本 demo 对应三段：

| 验收 | 章节 | 一句话判据 |
|---|---|---|
| **E1** 单体跑通 | §3 | 下单后 mysql 真有行；SIGTERM 后 20s 内退出 |
| **T5** 树形态 | §5 | 根 span 恰好 1 个、树深 ≥ 3、无孤儿 |
| **T13** 调用链完整性 | §9b | 三层各 ≥1 span、JOIN 单独成 span、四类 CRUD 齐全 |

一条命令取出本 demo 的 trace：

```bash
curl -s 'http://127.0.0.1:16686/api/traces?service=demo-monolith&limit=1' > /tmp/monolith-trace.json
```

### T13 反向断言

```bash
# config.yaml 里把 trace.on.db 改成 false，重启后重跑 T13 检查 —— 必须失败。
# 改之前之后都通过，说明完整性检查是空壳。
```

### 自动化验收（用例自起进程，2026-10-07 起）

```bash
export PATH=$HOME/sdk/go1.24.3/bin:$PATH CGO_ENABLED=0 HUBX_DB_PW=hubx123
cd examples/monolith && go test ./...            # 约 2.5 分钟
```

**不需要先手工起进程**：`TestMain` 会自己 build、挑一个空闲端口拉起实例，并把
地址写进 `MONOLITH_BASE`，之后 T2–T5 与 E1 用例都在真机上跑。

这样改的原因：本 demo 默认端口 8080 与微服 gateway 撞车，"先起进程再跑用例"
长期处在"这次能不能跑看谁先占端口"的状态，mongodbx / searchx 两段因此一直没
能实测。端口改由用例自挑后，这两条路径第一次被自动执行：

| 用例 | 判据 |
|---|---|
| `TestE1MonolithServesMySQLThenExitsGracefully` | 健康检查 → 下单 → 读回 → 改状态 → **docker exec 直查 mysql 复核**（绕开缓存证明真落库）→ SIGTERM 退出码 0 + 端口释放 |
| `TestE1MongoAndSearchBackendsReallyServe` | mongodbx 快照读回 + searchx 检索命中（ES 默认 1s refresh，给 15s 轮询窗口） |
| `TestDegrade*` / `TestStrict*` | 后端不可达时的带病运行（含"失败必须在链路上可见"）、`strict` 翻面即退、`trace.enabled=false` 零 span（带正向对照） |

环境不具备（无 go / mysql 不可达 / 无 docker）时用例 **Skip 并写明缺什么**，
不会假装通过；库名统一走 `order_mono`。

## 5. 本 demo 刻意不做的事

| 不做 | 原因 |
|---|---|
| 不注册（无 coordx/consul 段） | 单体形态不注册；出现 deregister 即串档（陷阱 11） |
| 不做 outbox 异步补发 | 异步投递会产生无父上下文的孤儿 trace（正是 T5 要抓的噪声）；demo 里同步投递，语义与 outbox 的"落库即事实、投递尽力"一致 |
| 缓存/检索失败不返回错误 | 它们是加速件，坏了降级到打库；但会在 service span 上留 `degrade.*` 事件，不留痕等于把降级伪装成没数据 |

### 已知环境约束（2026-10-05 实测）

| 组件 | 现状 | 说明 |
|---|---|---|
| MQ | 需预建 topic | mqx 的 producer 目标 topic 读的是 `consumer.topic`（`producer.topic` 当前不生效）；且 topic 不会自动创建，缺 topic 时报 `Unknown Topic Or Partition` |
| 检索 | 需 `legacy_cluster: true` | 本机 compose 的 `elasticsearch` 是 7.6.2，不返回 `X-Elastic-Product` 头，go-elasticsearch v8 会拒绝每一个响应。searchx 的 `legacy_cluster` 开关就是在响应上补齐该头（只补 2xx、不覆盖服务端已有的值）。升级到 ES ≥ 7.14 后应**删掉**这一行 |
| 代码内不出现 `127.0.0.1` / `localhost` | 网格要复用同一份二进制，地址必须全部配置化（陷阱 14） |
| 不做进程内 AutoMigrate | 表结构由 `scripts/bring-up.sh` 显式准备，避免隐式依赖 |
| 不自建 DB 连接 | DB 句柄只能经 `a.DB().(*gorm.DB)` 取 |

## 6. 与微服 / 网格的关系

本 demo 的业务三层（handler/service/repository）**不含任何形态分支**
（注意：模块路径本身含 `monolith`，故判据要排除 import 行）：

```bash
# 业务三层不得出现 mode 判定或形态取值字面量（import 路径含 monolith 不算）
grep -rnE '"monolith"|"bff"|"gateway"|Mode\(\)|service\.mode' --include=*.go internal/   # 必须为空
```

换形态只改 `config.yaml` 的 `service.mode`；网格形态复用**同一份二进制**，
差异只在配置副本与部署清单。
