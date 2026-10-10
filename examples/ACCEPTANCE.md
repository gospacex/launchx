# examples 验收标准（可执行版）

本文件是 `examples/` 的**执行与判定清单**：把 `openspec/changes/add-launchx-bootstrap/specs/*/spec.md` 里的验收案例（E1–E5、T1–T12）和 `tasks.md`（2.1–2.6、3.5–3.9、3.13、3.14、4.x）翻译成可复制粘贴的命令与可判定的断言。

**语义以 spec 为准。** 本文件只定义「怎么跑、怎么判、证据存哪」；若与 spec 冲突，改本文件，不改 spec。

判定状态三态：**PASS**（命令通过且断言成立）/ **FAIL**（任一条不成立）/ **SKIP**（前置环境缺失，必须写明缺什么，且不得记为 PASS）。

---

## 0. 环境口径（写死，不得各起炉灶）

| 项 | 值 | 来源 |
|---|---|---|
| 中间件供给 | `/Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml` | 用户给定，唯一供给源 |
| docker CLI | `/Applications/Docker.app/Contents/Resources/bin/docker` | PATH 里没有 |
| consul | `127.0.0.1:8500` | compose |
| mysql | `127.0.0.1:3306`，`order_rw` / `hubx123`，库 `order_db` | compose |
| kafka | `127.0.0.1:9092`（KRaft，auto-create topic，PLAINTEXT） | compose（**不**用 srv-order 旧端口 19093） |
| redis | `127.0.0.1:6379` | compose |
| jaeger | UI `127.0.0.1:16686`，OTLP/HTTP `127.0.0.1:4318` | compose |
| elasticsearch | `127.0.0.1:9200`（单节点，无鉴权） | compose（原为临时 `docker run` 的独立容器，已收编进 compose，数据目录 `/Users/hyx/work/dockerwork/data/elasticsearch/data`） |
| mongodb | `127.0.0.1:27018/27019/27020` | compose，需先 `docker compose --profile once up -d mongo-init` 初始化 rs0 |
| 网格 | 本机 minikube + Istio | 用户给定。**已装**：minikube `v1.36.0`、istioctl `1.30.1`（均在 `/usr/local/bin`）；**kubectl 未单独安装**，走 `minikube kubectl --`。集群**未启动**（无 `minikube` 容器），须先 `minikube start` |
| ⚠️ PATH | `/usr/local/bin` | WorkBuddy 的 shell PATH 不含 `/usr/local/bin`，`which minikube` 会**误报未安装**。所有脚本与手工命令必须先 `export PATH=/usr/local/bin:$PATH` |
| 宿主资源 | docker 29.7.2，总内存 **8 GB** | `minikube start` 须限 `--cpus=2 --memory=4096`，否则与 compose 中间件争内存导致 Pod 反复 OOM/Evicted |
| Go | `export PATH=/Users/hyx/sdk/go1.24.3/bin:$PATH` | GOTOOLCHAIN=auto 拉 go1.27.1 |

端口约定（避免同机互撞）：monolith `8080`；micro gateway `8080`、bff `8081`、srv 走 RPC 端口（沿用 srv-order 原端口，写进各自 config.yaml）。monolith 与 gateway 都是 8080 但**不同时运行**，跑之前确认端口空闲。

---

## 1. Gate 0：底座就绪（任何 demo 验收前必须先过）

```bash
bash examples/scripts/bring-up-trace.sh check        # 必须全绿
# 补两条本脚本未覆盖、但 T4 依赖的：
curl -sf http://127.0.0.1:9200/_cluster/health | grep -q '"status":"green"'   # ES
/Applications/Docker.app/Contents/Resources/bin/docker exec -it mongo1 \
  mongosh --quiet --eval 'rs.status().ok'            # mongo 副本集已初始化
```

判定：三条全过 → PASS；任一不过 → 该 demo 判 **SKIP**（写明缺哪个中间件），不许带病往下跑。

## 2. Gate 1：库侧门禁

```bash
export PATH=/Users/hyx/sdk/go1.24.3/bin:$PATH
cd /Users/hyx/work/gowork/src/hubx/launchx
gofmt -l .                       # 必须为空
go build ./... && go vet ./...   # 必须无错
go test ./...                    # 6 个包全 ok
grep -c "=>" go.mod              # ≥ 26（禁止 go mod tidy）
```

依赖面（3.14）——**判定边界以本段为准**：

```bash
# ① 库侧（根包 *.go + internal/**）：零 cloudwego / 零驱动直引
grep -rn "cloudwego" --include=*.go . | grep -v "^./examples/"    # 必须为空
grep -rn "go-redis\|segmentio/kafka-go\|hashicorp/consul/api\|gorm.io/gorm" \
     --include=*.go . | grep -v "^./examples/"                    # 必须为空
grep -n "cloudwego/kitex" go.mod                                  # Task 11 起已转 direct（见 ③）
```

**③ go.mod 直接依赖的既有偏差（已登记，不回购）**：demo 与库同属一个 module，
故 `github.com/cloudwego/hertz` 在 `go.mod` 中是**直接** require（唯一来源：
`examples/monolith/app/routes.go` 与 `internal/handler/` 要用 hertz 的
`route.Engine` / `app.RequestContext` 类型才能挂路由）。这是"demo 消费 webx 句柄"
的必然结果，不是库侧耦合——判据仍是 ① 的"库侧零 import"。

**【Task 11 触发，已达成】** `github.com/cloudwego/kitex`（及其生成桩依赖
`github.com/cloudwego/prutal`）现已同样变为**直接** require —— 唯一来源是
`examples/micro/srv/pkg/kitex_gen/**`（代码生成产物 import kitex 的
`server` / `client` 包）与 `examples/micro/srv/app/setup.go` 里对
`[]server.Option` 的类型断言。至此 ③ 预设的触发条件达成：**必须评估把
`examples/` 拆成独立 module**（让库侧 go.mod 回到零 cloudwego direct），
本轮不拆、不改判据，留作后续项并同步进 `plan-ready.md`。

**② demo 侧允许的特例（不算违规，但必须在下表内）**：demo 要在 go 层面消费 webx/rpcx 交出的句柄，
必然触及 hertz / gorm / kitex 的**类型**，这不是"业务直引驱动"。允许面仅限：

| 位置 | 允许引入 | 理由 |
|---|---|---|
| `examples/monolith/app/routes.go`、`internal/handler/` | `hertz/pkg/{app,route}` 类型 | `engine.GET` 与 handler 签名就是 hertz 的；业务分层内不出现 |
| `examples/monolith/app/setup.go`、`internal/repository/` | `gorm.io/gorm` **仅作类型断言与查询构造** | 句柄来自 `a.DB()`；**MUST NOT** 出现 `gorm.Open` |
| `examples/micro/{srv,bff}` 的 kitex_gen 构造处 | `kitex` | 代码生成产物 |

红线（越界即 FAIL）：demo **运行期**代码里出现 **`gorm.Open` / `redis.NewClient` / `kafka.NewWriter` / `consul.NewClient`**
任一自建客户端调用——那才是"绕过 launchx 直引驱动"。判定：

```bash
grep -rnE "gorm\.Open|redis\.NewClient|kafka\.(NewWriter|NewReader)|consul\.NewClient" --include=*.go .   # 必须为空
```

**Task 11 追加的豁免（不算越界，限定范围）**：随 srv-order 迁进来的单测
（`examples/micro/srv/internal/{repository,service}/*_test.go`）用
`gorm.Open(sqlite.Open(":memory:"))` 建**内存**库做查询语义断言——它不经
launchx、也不连任何中间件，是纯测试夹具，不属于"业务绕过 launchx 取句柄"。
故上式对该目录需排除 `_test.go`：

```bash
grep -rnE "gorm\.Open|redis\.NewClient|kafka\.(NewWriter|NewReader)|consul\.NewClient" \
     --include=*.go --exclude='*_test.go' .                                          # 必须为空
```

---

## 3. E1 — 单体 demo 跑通（tasks 2.1 / 3.5）

```bash
cd examples/monolith && export HUBX_DB_PW=hubx123
go build ./... && (go run ./cmd &>/tmp/monolith.log & echo $! >/tmp/monolith.pid)
sleep 3
curl -sf http://127.0.0.1:8080/orders                       # 200 + JSON 数组
curl -sf -X POST http://127.0.0.1:8080/orders \
  -H 'Content-Type: application/json' -d '{"item":"e2e","qty":1}'   # 2xx
mysql -h127.0.0.1 -uorder_rw -phubx123 order_db \
  -e "SELECT COUNT(*) FROM orders WHERE item='e2e';"        # ≥ 1，证明落真库不是内存
kill -TERM $(cat /tmp/monolith.pid); wait $(cat /tmp/monolith.pid) 2>/dev/null
```

判定：
- 三个请求 + mysql 行数断言全过 → **PASS**
- SIGTERM 后 **20s 内退出码 0** → PASS；超时或非零 → FAIL
- `main.go` 走 `Bootstrap(path).OnSetup(Setup).Run()` 形态，不出现裸装配代码

## 4. T2 / T3 / T4 — 数据层入链（tasks 4.3 / 4.4 / 4.5）

先取 trace（以 monolith 服务名为准）：

```bash
T=$(curl -s "http://127.0.0.1:16686/api/traces?service=demo-monolith&limit=20" | jq -r '.data[0].traceID')
curl -s "http://127.0.0.1:16686/api/traces/$T" | jq '.data[0]' > /tmp/trace.json
```

| 案例 | 断言（jq 判据） | 通过条件 |
|---|---|---|
| **T2** 单表 CRUD + SQL 入链 | `jq -r '.spans[] \| select(.operationName\|startswith("db.")) \| [.operationName,(.tags[]\|select(.key=="db.query.text").value)] \| @tsv' /tmp/trace.json` | 出现 `db.create/query/update/delete`；每条 `db.query.text` 为**完整 SQL**（非空、非 `?` 占位符残留）；`db.sql.table` 为表名 |
| **T3** 多表 + 事务 | 同上 + `jq '[.spans[] \| select(.operationName\|startswith("db.")) \| .tags[] \| select(.key=="db.query.text").value] \| map(select(test("(?i)join\|preload")))'` | 至少 1 条含 JOIN/关联文本；同一事务内多条 SQL 是**兄弟 span**（同一 parentSpanID）同 trace；回滚时相关 span 带 `error=true` 标签 |
| **T4** 四类组件同 trace | `jq -r '.spans[].operationName' /tmp/trace.json \| sort -u` | 同时出现 cache 类（`GET`/`SET`/…）、mq 类、`db.elasticsearch.*`、`db.mongodb.*`，且它们的 `references` 指向业务 span（非根） |

FAIL 判据（任一条）：存在 `db.query.text` 为空的 db span；四类缺任一；数据层 span 是根 span（孤儿）。

## 5. T5 — 单体调用树（tasks 4.2）

```bash
jq '[.spans[] | select((.references|length)==0)] | length' /tmp/trace.json      # 根 span 数，必须 = 1
jq -r '.spans[] | "\(.spanID) \(.operationName) \(.references[0].spanID // "-")"' /tmp/trace.json
```

判定：根 span 数 **= 1**（无孤儿）；span 树深度 **≥ 3**（入口 `GET /api/...` → 业务层 → 数据层）；数据层 span 的祖先链中含入口 span。深度不足 → FAIL（说明业务分层没自开 span，或 launchx 代开了业务 span——后者是违规）。

## 6. E2 + T6 — 微服三段同 trace（tasks 2.2 / 2.3 / 3.6 / 4.6）

启动顺序：srv → bff → gateway，每步等就绪再下一步。

```bash
curl -s 'http://127.0.0.1:8500/v1/health/service/demo-srv?passing=1' | jq 'length'   # ≥ 1，consul 发现生效
curl -sf http://127.0.0.1:8080/api/ping       | grep -q '"service":"bff"'             # gateway → bff
curl -sf http://127.0.0.1:8080/api/order/1                                            # → bff → srv → mysql
curl -s 'http://127.0.0.1:8500/v1/catalog/services' | jq -r 'keys[]'                  # 含 demo-bff/demo-srv，**不含** demo-gateway
```

> ⚠️ **本机端口坑（实测）**：`127.0.0.1:8081` 被 Docker Desktop 的 `com.docke` 以 `*:8081`
> （IPv6 通配，etcdkeeper 容器转发）占用，而 bff 只绑 IPv4 `127.0.0.1:8081`。两个监听并存，
> `curl localhost:8081` 会解析到 `::1` 从而打到 **etcdkeeper 返回 404**，
> 必须显式写 **`http://127.0.0.1:8081`**。同理 gateway 转发用的上游地址也必须写 `127.0.0.1` 而非 `localhost`。
> 另：本机 shell 常有 `http_proxy`，验收 curl 一律加 `--noproxy '*'`。

链路断言：

```bash
curl -s "http://127.0.0.1:16686/api/traces?service=demo-gateway&limit=20" > /tmp/gw.json
# 同一 trace 下的服务名集合必须含三个（服务名在 .processes 映射里，不在 span.process 上）
jq -r '.data[0].processes | to_entries[] | .value.serviceName' /tmp/gw.json | sort -u
# 父子链（按 spanID 排序后再看，避免 .spans[] 迭代顺序造成的错配）
jq -r '.data[0] as $t | [.data[0].spans[] | {id:.spanID, svc:$t.processes[.processID].serviceName, op:.operationName, p:((.references//[])[0].spanID // "-")}] | sort_by(.id)[] | "\(.svc)\t\(.op)\tparent=\(.p)"' /tmp/gw.json
# 根 span 必须唯一
jq '[.data[0].spans[] | select((.references//[])|length==0)] | length' /tmp/gw.json   # = 1
```

> ⚠️ **Jaeger 字段口径（实测，同 §9b.3）**：服务名在 **`.data[].processes[<processID>].serviceName`**，
> span 上只有 `.processID`。直接取 `.spans[].process.serviceName` 得到 `null`，
> 于是"服务名集合"判据恒为空 → 空壳。

判定：服务名集合含 `demo-gateway`/`demo-bff`/`demo-srv`；根 span = 1；父子链方向正确；srv 侧 db span 在同一 trace。**三段各成一根（trace 数 ≥ 3）→ FAIL**（典型症状：传播器没注册，见 §9）。

**golden 链（2026-10-05 真机提取，`/api/order/1`，单 trace 9 span）**：

```
demo-gateway   GET /api/order/1            parent=-            ← 根，webx otel 中间件
demo-gateway   gateway.proxy.api           parent=GET /api/...  ← 转发前 launchx.Inject
demo-bff       GET /order/1                parent=gateway.proxy.api   ← bff 入站 span 挂在网关下 = 续链成功
demo-bff       handler.GetOrder            parent=GET /order/1
demo-bff       downstream.Order.GetOrder   parent=handler.GetOrder
demo-bff       client.demo-srv.GetOrder    parent=downstream...  ← rpcx ClientSpanMiddleware
demo-bff       order.Order/GetOrder        parent=downstream...  ← kitex 客户端侧
demo-srv       order.Order/GetOrder        parent=client.demo-srv.GetOrder  ← 跨进程
demo-srv       service.order.GetOrder      parent=order.Order/GetOrder
```

关键观察：**bff 的入站 span 直接挂在 `gateway.proxy.api` 之下**，这是 `launchx.Inject` 生效的直接证据；
若注入缺失，bff 会另起一根 trace，此处会变成 `parent=-`。

## 7. E3 — srv 迁移回归（tasks 2.4 / 3.7）

```bash
wc -l examples/micro/srv/cmd/main.go            # ≤ 50
grep -rn "micro_apps/infra" examples/micro/srv/ # 必须为空（infra 复制件已剔除）
# 原 srv-order e2e：下单链路 + outbox 经 kafka 投递
```

判定：
- `main.go ≤ 50` 行 → 否则 FAIL
- 无 `micro_apps/infra` import（改走 `App.DB/Cache/MQ/Registry/Tracer`）→ 否则 FAIL
- 下单 e2e 全绿，且 outbox 对应 topic 在 kafka `9092` 可见消息（topic 自动创建）→ PASS

## 8. E1/E2 收尾：T9 尾部不缺（tasks 4.7）

退出冲刷唯一可观测证据是**尾部 span 不丢**：

```bash
# 触发请求后立刻 SIGTERM，再查 Jaeger：最后一条数据层 span 必须存在
jq -r '.spans[] | select(.operationName|startswith("db.")) | .operationName' /tmp/trace.json | tail -1
```

判定：请求中最后执行的那次数据层操作有对应 span → PASS；缺失 → FAIL（退出链没走 `tracing.shutdown`，或用了 defer 导致顺序反了）。

## 9. T1 / T7 — 传播器红线（tasks 4.1）

```bash
export PATH=/Users/hyx/sdk/go1.24.3/bin:$PATH
go test ./internal/telemetry/ -run TestInitDisabledStillRegistersPropagator -v   # Fields 含三项
go test . -run 'Inject|Propagator|Extract' -v                                    # Task 23 已落地
```

已覆盖（Task 23，`trace.go` / `trace_test.go`，commit `18a331d`）：

| 用例 | 断言 |
|---|---|
| `TestInjectWritesTraceParent` | 注入后 carrier 有 `traceparent`，且含上游 trace id（T1） |
| `TestInjectAndExtractRoundTrip` | 注入→提取闭环，下游取回同一个 trace id |
| `TestDefaultPropagatorIsNoop` | **反向断言**：noop 传播器下既写不出 traceparent、Extract 也取不到有效 context，Fields 为空（T7） |
| `TestPropagatorFieldsContainsW3C` | Fields 覆盖 `traceparent`/`tracestate`/`baggage` |
| `TestInjectNilCarrierSafe` | nil carrier 不 panic（转发场景易传空） |

另有 `internal/telemetry` 的 `TestInitDisabledStillRegistersPropagator`：trace 关闭时传播器仍注册（关闭 trace 不等于断链基础设施）。

判定：全部通过 → T1/T7 PASS。**`TestDefaultPropagatorIsNoop` 若也"通过地提取到了"，说明全局状态被其它用例污染 → FAIL。**

## 9b. T13 — 调用链完整性（分层 / 中间件矩阵 / CRUD 与连表逐条）

**为什么单列一条**：前面 T2–T6 判的是「有没有链路」，T13 判的是「链路里有没有空洞」——链路能画通但某层被跳过（比如 repository 层没埋点、连表那条 SQL 没成 span）是当前最容易蒙混过关的形态。

### 9b.1 分层完整性

```bash
TRACE=$(curl -s "http://127.0.0.1:16686/api/traces?service=monolith&limit=1" | jq -r '.data[0].traceID')
curl -s "http://127.0.0.1:16686/api/traces/$TRACE" > /tmp/t.json

# 根 span 必须唯一（路由入口），且总 span 数不低于预期下限
jq '[.data[0].spans[] | select((.references // []) | length == 0)] | length' /tmp/t.json   # == 1
jq '[.data[0].spans[]] | length' /tmp/t.json                                              # >= 预期下限
```

判据：根 span **恰好 1 个**且为 HTTP 入口（operationName 为 HTTP 方法/路径形态）；`handler / service / repository` 三层各至少 1 个 span；树深 ≥ 3。

**预期 span 清单（golden，2026-10-05 真机提取并固化）**——`service=demo-monolith`：

| 请求 | 根 span | 三层 span | 数据层 span | 总 span 数 |
|---|---|---|---|---|
| `POST /orders` | `POST /orders` | `handler.order.CreateOrder` → `service.order.CreateOrder` → `repository.order.CreateWithItems` | `db.create` × 2（orders + order_items，**同父兄弟**） | 6 |
| `GET /orders` | `GET /orders` | `handler.order.ListOrders` → `service.order.ListOrders` → `repository.order.List` | `db.query` × 1 | 5 |
| `GET /orders/:no` | 同名根 | `handler.order.GetOrder` → `service.order.GetOrder` → `repository.order.Get` | `db.query` × 1 | 5 |
| `GET /orders/:no/join` | 同名根 | `handler.order.GetOrderWithUser` → `service.order.GetOrderWithUser` → `repository.order.GetWithUser` | `db.query` × 1（含 `JOIN users`） | 5 |
| `PUT /orders/:no` | 同名根 | `handler.order.UpdateOrder` → `service.order.UpdateOrder` → `repository.order.UpdateStatus`、`repository.order.Get` | `db.update` × 1 + `db.query` × 1 | 7 |
| `DELETE /orders/:no` | 同名根 | `handler.order.DeleteOrder` → `service.order.DeleteOrder` → `repository.order.DeleteItems`、`repository.order.Delete` | `db.delete` × 2 | 7 |

实测 SQL 文本（8 条去重后）：`INSERT INTO orders/order_items`、`SELECT * FROM orders [ORDER BY id DESC LIMIT ?|WHERE order_no = ? LIMIT ?]`、
`SELECT o.id ... FROM orders o JOIN users u ON o.user_id = u.id WHERE o.order_no = ?`、`UPDATE orders SET status=?,updated_at=? WHERE order_no = ?`、
`DELETE FROM orders|order_items WHERE order_no = ?`。参数以 `?` 占位（gorm 预编译），**语句文本完整即可，不要求参数已代入**。

### 9b.2 中间件入链矩阵（逐行核对，未列出即缺失）

| 中间件 / 切面 | 位置 | 是否入链 | 依据或豁免理由 |
|---|---|---|---|
| otel（HTTP 入口） | `webx/middleware/otel.go` | ✅ 必须 | 根 span 来源，Extract 入站 traceparent |
| DB 访问 | `dbx/orm` gorm 插件 | ✅ 必须 | `db.query.text` 携带完整 SQL |
| 缓存 | `cachex` | ✅ 必须 | 有外部依赖，否则穿透不可见 |
| 消息 | `mqx` | ✅ 必须 | produce / consume 各一 span |
| 检索 | `searchx` | ✅ 必须 | 外部依赖 |
| 文档库 | `mongodbx` | ✅ 必须 | 外部依赖 |
| RPC 客户端 | `rpcx` ClientSpanMiddleware | ✅ 必须 | 跨进程续链，缺则断成两棵子树 |
| 鉴权 | 业务 middleware | ✅ 必须 | 有外部依赖（缓存 / DB 查凭据） |
| 限流 | 业务 middleware | ✅ 必须 | 依赖缓存计数 |
| recover 兜底 | `webx` | ⚠️ 可豁免 | 纯控制流、无外部调用——**必须在理由列写明** |
| CORS 响应头 | `webx` | ⚠️ 可豁免 | 仅设置响应头——**必须在理由列写明** |

判定：矩阵中「必须」项全部有 span；「可豁免」项**必须写明理由**，空白理由视为缺失。矩阵覆盖不全（实际启用却未列入）→ FAIL。

### 9b.3 CRUD 与连表逐条入链

> ⚠️ **Jaeger REST API 的字段口径（实测，2026-10-05）**：span 属性在 `.tags[]`，
> 结构是 `{"key","type","value"}`，**不是** `.attributes[]`。用 `.attributes[]`
> 取永远得到 0——判据会静默"通过"（0 条 SQL 也判定为空值数为 0），是典型空壳。
> 下面所有 jq 一律走 `.tags[]`。

```bash
SQLS='[.data[].spans[].tags[]? | select(.key=="db.query.text") | (.value|tostring)]'

# 1) 带完整 SQL 的 span 数（应等于该请求实际执行的 SQL 条数）
jq "$SQLS | length" /tmp/t.json
# 2) 空值 / 占位符必须为 0
jq "$SQLS | map(select(. == \"\" or length < 8 or (ascii_downcase|test(\"(?i)placeholder\")))) | length" /tmp/t.json   # == 0
# 3) 连表语句必须含 join，且每条 JOIN 各自成 span（不得合并）
jq "$SQLS | map(select(ascii_downcase|test(\"(?i)join\"))) | length" /tmp/t.json                                    # >= 1
# 4) 四类 CRUD 各至少一条
for op in insert select update delete; do
  printf "%-7s %s\n" "$op" "$(jq "$SQLS | map(select(ascii_downcase|test(\"(?i)^[^a-z]*$op\"))) | length" /tmp/t.json)"
done
```

判定：`db.query.text` 非空 span 数 **==** 实际 SQL 条数（不是 ≥，多了说明重复埋点，少了说明漏埋）；JOIN 语句的 text 含 `join`；INSERT / SELECT / UPDATE / DELETE 四类各 ≥ 1；事务中的多条 SQL **各自成 span**，合并成一条即 FAIL。

### 9b.4 反向断言（防空壳，必做）

```bash
# 关闭 db 埋点后重跑同一请求，完整性检查必须失败
sed -i '' 's/trace.on.db: true/trace.on.db: false/' examples/monolith/config.yaml
# ... 重跑并重新执行 9b.3 的判据 ...
```

判定：关闭后 `db.query.text` span 数必须为 0，即完整性检查**必须报错**。若关闭前后都"通过"，说明检查是空壳 → FAIL。测完必须还原配置。

## 10. 降级路径（tasks 3.9）

| 场景 | 步骤 | 判定 |
|---|---|---|
| `trace.enabled=false` | 改 yaml 后启动并请求 | 服务正常响应；Jaeger 中**无**该服务业务 span |
| 无 consul | 停 consul 后以 bff/srv 档启动 | 带病运行（Warn 日志 + registry 句柄 nil），进程**不退出**；接口仍可用 |
| `strict.<name>=true` 且组件不可达 | 造不可达地址启动 | **启动失败**并退出（与默认降级相反） |

## 11. 网格：两级验收（tasks 2.5 / 3.8 / 3.13 / 4.9–4.11）

### 11a. 硬验收（自动化，build 内必须通过）

```bash
kubectl apply --dry-run=client -f examples/mesh/ -R      # 清单全过
bash examples/mesh/scripts/bring-up.sh check             # 前置检查
```

```bash
export PATH=/usr/local/bin:$PATH          # 关键：否则 minikube/istioctl 不可见
command -v minikube istioctl              # 必须命中
minikube kubectl -- version --client      # kubectl 走 minikube 内置分发
minikube status                           # 未起集群时须报 "No such container: minikube"（而非静默通过）
kubectl apply --dry-run=client -f examples/mesh/ -R      # 清单全过
bash examples/mesh/scripts/bring-up.sh check             # 前置检查
```

**判据（重要）**：`check` 对每一项前置（PATH、minikube、istioctl、kubectl、集群 Running、istio-system 就绪、镜像已 load）都必须**逐项显式输出并以非零码退出**；**任一项缺失却仍返回 0 = FAIL**（静默通过是本类验收最高发的伪装点）。三 Deployment 探针字段须为 `tcpSocket` 且端口与业务一致（3.13/O3）。

### 11b. 人工验证项（close 不硬阻塞，缺底座时记 SKIP）

前置（工具已装，只需起集群）：

```bash
export PATH=/usr/local/bin:$PATH
minikube start --driver=docker --cpus=2 --memory=4096     # 宿主总内存仅 8GB，必须限额
# 必须用 operator 文件，不要用 --set：Istio 1.30 的 meshConfig.defaultConfig.tracing
# 是**数组**，--set ...tracing.sampling=100 会让 istioctl 在 values.Map.SetPath 里 panic
# （实测堆栈 operator/pkg/values/map.go:365）。采样率改由 Telemetry API 下发。
istioctl install -y -f examples/mesh/istio-operator.yaml
kubectl apply -f examples/mesh/namespace.yaml             # 带 istio-injection=enabled
# 镜像：先在 hubx/ 仓库根构建（构建上下文必须是仓库根，见 §13.3 C-0），再
minikube image load launchx-examples/gateway:dev          # 三服务各一次
```

集群内访问宿主中间件用 **`host.minikube.internal`**（minikube 提供的宿主 DNS 名；`host.docker.internal` 是 Docker Desktop 容器内的写法，在 minikube 的 k8s 里**不保证解析**）——这一点由 `configs/*.yaml` 的地址副本体现，也是网格与微服配置的**唯一**差异来源。

- **T10** ✅ **PASS（2026-10-05 真机实测，证据 `examples/mesh/docs/verify.md` §1）**：同一 trace 下同时有应用 span（`demo-gateway`/`demo-bff`/`demo-srv`）与 sidecar span（`demo-gateway.launchx-mesh`、`demo-bff.launchx-mesh`），应用 span 父子链仍为 gateway → bff → srv。代表性 trace `8a728dcfe9b47cb29b7b83e348a4bc9e`：16 span（应用 12 / sidecar 4），单根，树深 13。查询时**两侧都要查**：`service=demo-gateway` 与 `service=demo-gateway.launchx-mesh`。
  判据已可执行：`python3 scripts/trace-audit.py --service demo-gateway --lookback 2m --mesh`（追加 M1 共存 / M2 `component=proxy`+`upstream_cluster` 验真 / M3 覆盖 ≥2 跳）。
- **T11**（反向断言）⏸ **SKIP**：详见 `docs/verify.md` §3。要点：launchx 的传播器**恒注册**（`telemetry.Init` 注释明确「传播器与是否采样无关」），关 `trace.enabled` 只让应用不产出 span、仍会透传 `traceparent`，得到的是「纯 sidecar 链」而非 T11 描述的「sidecar 链 + 应用孤儿 trace 并存」。**不可用 OFF 档冒充 T11**——那属于偷换判据。真要构造需一个显式标注的非同构变体（`propagator=none`），不在本轮范围。
- **E5** ✅ **PASS**：`trace.enabled` OFF/ON 各跑一次。OFF 档 4 条 trace 全为 sidecar span、应用 span 数为 **0**；ON 档 15–16 span/trace（应用+sidecar 同 trace）。对比表见 `docs/verify.md` §2。
- **T12** ✅ **PASS**：证据落盘 `examples/mesh/docs/verify.md`，含 trace-id、span 总数、应用 span 数 vs sidecar span 数、sidecar 服务名、Jaeger API 原始响应片段、OFF/ON 对比、minikube/kubectl/istioctl/Istio 版本。

**已核实（不再是待核）**：Istio 的 extensionProvider 必须选 `opentelemetry`（Envoy OTel tracer 走 W3C `traceparent`，与 launchx 的 `Composite{TraceContext, Baggage}` 对齐）；`zipkin` provider 走 B3（`x-b3-*`）会与应用侧断成两条 trace。

**T10 假阴性的三个独立成因（全部修掉才入链，详见 `docs/verify.md` §4）**：

1. provider 目标必须是**集群内 Service**：直接写 `host.minikube.internal` 会被 istiod 拒（`could not find service ... in Istio service registry`）并静默不生成 OTLP cluster → 用无 selector Service + 手工 Endpoints 做宿主门面（`host-middleware.yaml`）。
2. 采样率必须 100（`Telemetry` API 的 `randomSamplingPercentage`），否则「十次看不到一条」会被误读成断链。
3. **最隐蔽的一条**：4317 端口名必须是 `grpc-` 前缀。写成 `otlp-grpc` 时 Istio 认不出协议 → cluster 只带 `use_downstream_protocol_config`，而 Envoy 自己的 OTLP 导出没有 downstream，回落 HTTP/1.1，gRPC 流被 reset。默认 warning 日志**看不见**，必须 `curl -X POST localhost:15000/logging?level=debug` 才报 `OTLP trace export failed ... protocol error`。

**反向坑（别改）**：`demo-srv` 的 9013 端口名只能是 `tcp-rpc`。改成 `grpc-rpc` 会让 Istio 按 h2 解析 kitex 的 TTHeader → 网关直接 **502**（改回即 200）。代价是 9013 这一跳协议上就是 L4，没有 sidecar span、也交不出 L7 治理——这是协议事实，不是配置漏项。

## 12. 依赖面与同构（tasks 3.14 / 4.8）

```bash
grep -rn "cloudwego" --include=*.go .                    # 仅 micro/{srv,bff} 的 kitex 构造处允许命中
grep -rn "redis/go-redis\|segmentio/kafka-go\|hashicorp/consul/api\|gorm.io/gorm" --include=*.go examples/  # 必须为空

# 网格不复制业务代码：mesh 目录内不应有 .go 文件，只放清单与配置副本
find examples/mesh -name '*.go'                 # 必须为空
# 网格配置副本与微服配置的差异只在地址段（127.0.0.1 → host.minikube.internal）
for s in gateway bff srv; do
  diff <(sed -E 's#[0-9]+(\.[0-9]+){3}:[0-9]+#ADDR#g' examples/micro/$s/config.yaml) \
       <(sed -E 's#[0-9]+(\.[0-9]+){3}:[0-9]+#ADDR#g' examples/mesh/configs/$s.yaml)
done
```

判定：`mode` 枚举无 `mesh` 取值；`examples/` 与库内无 sidecar 相关 trace 分支；`examples/mesh` **不含 Go 业务代码副本**（复用微服 demo 镜像，只换配置与地址段）——出现 `.go` 文件即 FAIL，因为它意味着网格走了独立代码路径，违反 T8「与微服同构」。

---

## 13. 三类框架案例的验收细节（目录树 / 调用链 / 可观测）

三类案例指**三种部署与框架形态**，业务恒为「订单」——同一份业务在三类下走不同装配路径，用来证明 launchx 的「换形态不改业务代码」（验收案例 C4 / T8）。

### 13.0 总览对照

| 维度 | A 单体 `examples/monolith` | B 微服 `examples/micro` | C 网格 `examples/mesh` |
|---|---|---|---|
| `service.mode` | `monolith` | `gateway` / `bff` / `srv`（三个进程各一档） | 与 B **完全相同**（无 `mesh` 档） |
| 进程数 | 1 | 3 | 3（复用 B 的镜像） |
| 入口协议 | HTTP（webx/hertz） | HTTP → HTTP → Kitex RPC | 同 B，流量经 sidecar 接管 |
| 服务间调用 | 进程内函数调用 | RPC + consul 发现 | 同 B（+ k8s Service 一层） |
| 注册发现 | **不注册** | gateway 只发现不注册；bff/srv 注册 | 同 B |
| Go 代码 | 有 | 有 | **禁止有**（镜像由 micro 侧 Dockerfile 构建；mesh 只有部署面，见 §13.3 C-0） |
| 根 span 数 | 1 | 1 | 1（应用侧）；sidecar 侧另成并行链 |
| Jaeger 服务名 | `demo-monolith` | `demo-gateway` / `demo-bff` / `demo-srv` | 应用名 + `<workload>.<ns>` 两套 |
| 健康入口 | `GET /healthz/live`、`/healthz/ready`（webx 实际注册的两个子路径，**裸 `/healthz` 返回 404**） | gateway/bff 两个子路径；srv 靠 consul TTL | k8s `tcpSocket` 探针 + consul |

---

### 13.1 案例 A — 单体（`examples/monolith`）

#### A-1 目录树（约定即断言）

```
examples/monolith/
├── config.yaml                  # mode: monolith
├── cmd/main.go                  # ≤50 行，Bootstrap + OnSetup + Run
├── app/
│   ├── setup.go                 # func Setup(*launchx.App) error：装配 repo→service→logic→handler
│   └── routes.go                # func RegisterRoutes(...)：经 WithPreRun 挂入
└── internal/
    ├── handler/order.go         # 仅做参数绑定与响应封装
    ├── logic/order.go           # 校验 / 错误映射
    ├── service/order.go         # 业务编排（自开 span）
    ├── repository/order.go      # 经 App.DB（*gorm.DB）读写
    └── model/order.go           # GORM model
```

判定（结构不得漂移）：

```bash
cd examples/monolith
test -f config.yaml && test -f cmd/main.go && test -f app/setup.go && test -f app/routes.go
for d in handler logic service repository model; do test -d internal/$d || echo "MISSING internal/$d"; done
# 反向断言：业务分层不得出现 mode 分支（C4）
grep -rn "monolith\|bff\|gateway\|srv" internal/ | grep -v "_test.go"    # 必须为空
```

#### A-2 `main.go` 契约

```bash
wc -l cmd/main.go                                    # ≤ 50
grep -c "launchx.Bootstrap" cmd/main.go              # = 1
grep -nE "gorm\.Open|sql\.Open|consul|otel\." cmd/main.go   # 必须为空（裸装配全部下沉到 launchx）
```

形态必须是 `Bootstrap(path) → OnSetup(Setup) → Run()` 三段，且 `Setup` 只做业务装配。

#### A-3 `config.yaml` 契约（关键段）

| 键 | 值 | 说明 |
|---|---|---|
| `service.name` | `demo-monolith` | Jaeger 服务名，也是 dbx 实例名 |
| `service.mode` | `monolith` | 缺失/非法即退（C2） |
| `webx.addr` | `127.0.0.1:8080` | 与 gateway 同端口但不同时运行 |
| `xmiddleware.sqlx.driver` | `gorm` | |
| `xmiddleware.sqlx.databases.demo-monolith.dsn` | `order_rw:${HUBX_DB_PW}@tcp(127.0.0.1:3306)/order_db?...` | 运行前 `export HUBX_DB_PW=hubx123` |
| `logx.level` | `info` | |
| `logx.otel.enable_trace` / `exporter` / `endpoint` | `true` / `otlp` / `127.0.0.1:4318` | |

**不得出现**：`coordx` 段（单体不注册）、`gateway.routes`、`rpcx` 监听段。出现即为装配越界。

#### A-4 调用链拓扑（期望形态）

```
GET /orders  (demo-monolith, SERVER)                 ← 唯一根 span
└── service.ListOrders  (INTERNAL)                   ← 业务自开，非 launchx 代开
    ├── repository.Query  (INTERNAL)
    │   └── db.query  (CLIENT)  db.query.text="SELECT * FROM orders WHERE ..."
    └── cache.GET order:list  (CLIENT)               ← 若命中缓存路径
```

span 清单与判据（`/tmp/trace.json` 取自 `service=demo-monolith`）：

```bash
jq -r '.spans[] | "\(.spanID)  \(.operationName)  parent=\(.references[0].spanID // "-")"' /tmp/trace.json
jq '[.spans[] | select((.references|length)==0)] | length'                       # = 1
jq '[.spans[] | select(.operationName|startswith("db."))] | length'               # ≥ 1
jq -r '.spans[] | select(.operationName|startswith("db.")) | .tags[] | select(.key=="db.query.text").value'  # 完整 SQL
```

判定：根 span = 1；树深 ≥ 3；db span 的 `db.query.text` 为完整 SQL（非空、无 `?` 残留）；业务 span 由业务代码自开（若分层未自开 span 导致树深 = 2，属业务漏埋点 → 记 FAIL 并注明）。

#### A-5 可观测面

| 面 | 入口 | 判定 |
|---|---|---|
| 链路 | Jaeger `service=demo-monolith` | 见 A-4 |
| 日志 | 进程 stdout / `/tmp/monolith.log` | 启动序列与退出序列完整（见 A-6） |
| 健康 | `curl -sf http://127.0.0.1:8080/healthz/live` | 200（webx `Config.HealthPrefix` 默认 `/healthz`（`webx/config.go:51`），但**实际注册的是 `/healthz/live` 与 `/healthz/ready` 两条**（`webx/webx.go:130`），裸 `/healthz` 为 404；`ReadyCheck` 未配时 `/healthz/ready` 恒 200） |
| 数据 | `mysql order_db` 行数 | 落真库，非内存 |
| metrics | `curl -s http://127.0.0.1:8080/metrics` | **200 + Prometheus 文本**（2026-10-08 真机复验，见 §13.4）。判据不止 200：须含 `# HELP`、`webx_requests_total`、`webx_request_duration_seconds` 且分桶为秒级（`le="0.001"` 起，不是缺省的 `le="5"`） |

#### A-6 判定清单

```bash
# 启动与业务
cd examples/monolith && export HUBX_DB_PW=hubx123
go build ./... && (go run ./cmd &>/tmp/monolith.log & echo $! >/tmp/monolith.pid)
sleep 3
curl -sf http://127.0.0.1:8080/healthz/live                             # 200（裸 /healthz 是 404）
curl -sf http://127.0.0.1:8080/orders                                   # 200 + JSON 数组
curl -sf -X POST http://127.0.0.1:8080/orders -H 'Content-Type: application/json' -d '{"item":"e2e","qty":1}'
mysql -h127.0.0.1 -uorder_rw -phubx123 order_db -e "SELECT COUNT(*) FROM orders WHERE item='e2e';"   # ≥1

# 退出序列（反序，缺一即 FAIL）
kill -TERM $(cat /tmp/monolith.pid)
grep -E "server|component|deregister|tracing|logger|shutdown" /tmp/monolith.log
```

| 判据 | 通过条件 |
|---|---|
| 三个请求 + mysql 行数 | 全过 → PASS |
| SIGTERM 后 20s 内退出码 0 | 是 → PASS |
| 退出序列顺序 | `server.stop → component.stop → (无 deregister) → tracing.shutdown → logger.close`；单体不注册，故**不应**出现 deregister |
| 日志中无 `panic` / `span context missing` | 是 → PASS |

---

### 13.2 案例 B — 微服（`examples/micro`）

#### B-1 目录树

```
examples/micro/
├── gateway/
│   ├── config.yaml              # mode: gateway
│   ├── cmd/main.go              # ≤50 行
│   ├── app/setup.go
│   ├── internal/routing/routes.go
│   └── internal/proxy/proxy.go  # 反向代理 + 状态码映射
├── bff/
│   ├── config.yaml              # mode: bff
│   ├── cmd/main.go              # ≤50 行
│   ├── app/setup.go
│   ├── internal/handler/order.go
│   └── internal/downstream/client.go   # 经 App.RPCClientOptions("srv") 构造 kitex 客户端
└── srv/
    ├── config.yaml              # mode: srv
    ├── cmd/main.go              # ≤50 行（E3 硬断言）
    ├── app/setup.go
    ├── internal/{service,repository,rpc,middleware}/**   # 自 srv-order 迁移，业务零改动
    └── pkg/kitex_gen/**         # bff 通过同 module import 复用，不复制第二份
```

判定：

```bash
for s in gateway bff srv; do
  test -f examples/micro/$s/config.yaml && test -f examples/micro/$s/cmd/main.go || echo "MISSING $s"
  wc -l examples/micro/$s/cmd/main.go                       # 每个 ≤50
  grep -c "launchx.Bootstrap" examples/micro/$s/cmd/main.go # 每个 =1
done
grep -rn "micro_apps/infra" examples/micro/                 # 必须为空（E3）
find examples/micro -name kitex_gen -type d                 # 只能 1 个（在 srv 下）
```

#### B-2 三个 `config.yaml` 的差异（差异即装配矩阵）

| 键 | gateway | bff | srv |
|---|---|---|---|
| `service.name` | `demo-gateway` | `demo-bff` | `demo-srv` |
| `service.mode` | `gateway` | `bff` | `srv` |
| `webx.addr` | `127.0.0.1:8080` | `127.0.0.1:8081` | **不配**（srv 不建 HTTP 引擎） |
| `coordx.driver/addr` | `consul` / `http://127.0.0.1:8500` | 同左 | 同左 |
| 注册 | **否**（只发现） | 是 | 是 |
| `gateway.routes` | `/api → bff` | 不配 | 不配 |
| RPC 监听 | 不配 | 不配（只有出向客户端） | `127.0.0.1:9013` |
| `xmiddleware.sqlx` | 不配 | 不配 | 配（order_db） |
| `xmiddleware.mqx`（kafka） | 不配 | 不配 | 配（`127.0.0.1:9092`，**不**用 19093） |

**端口定稿**：monolith `8080`、gateway `8080`、bff `8081`、srv RPC `127.0.0.1:9013`（沿用原 srv-order 监听地址）。
⚠️ 计划 Task 10 步骤 10.2 写的 bff `8091` 是笔误——实现按 **8081** 落，并回改计划与本文档保持单一口径。

#### B-3 调用链拓扑（跨进程，三段同 trace）

```
gateway  GET /api/order/1001  (SERVER)                       ← 唯一根 span
└── HTTP client → bff         (CLIENT)  traceparent 出向注入
    bff  GET /order/1001      (SERVER, 远端父 = gateway client span)
    └── RPC GetOrder → srv    (CLIENT)  traceparent 经 kitex 元数据注入
        srv  GetOrder         (SERVER, 远端父 = bff client span)
        └── db.query          (CLIENT)  db.query.text="SELECT ... FROM orders WHERE id=?"
```

出向注入（T1/T7 红线）：`webx/middleware/otel.go` **只有 Extract 没有 Inject**，因此 gateway 的反向代理在转发前**必须**显式调 `launchx.Inject(ctx, header)`；bff→srv 由 rpcx 的 `ClientSpanMiddleware` 注入。两处缺任一都断链。

判据：

```bash
curl -s "http://127.0.0.1:16686/api/traces?service=demo-gateway&limit=20" | jq -r '.data[0].traceID' | head -1
curl -s "http://127.0.0.1:16686/api/traces/$T" | jq '.data[0]' > /tmp/trace.json

jq -r '.processes | to_entries[] | .value.serviceName' /tmp/trace.json | sort -u   # 三个服务名齐
jq '[.spans[] | select((.references|length)==0)] | length'                          # = 1
jq -r '.spans[] | "\(.processID) \(.operationName)"' /tmp/trace.json                # 父子方向
```

判定：服务名集合 = {gateway, bff, srv}；根 span = 1；gateway span 是 bff span 的祖先、bff 是 srv 的祖先；srv 侧 db span 同 trace。三段各成一根（trace 数 ≥ 3）→ FAIL。

#### B-4 可观测面

| 面 | 入口 | 判定 |
|---|---|---|
| 服务发现 | `curl -s 'http://127.0.0.1:8500/v1/health/service/demo-srv?passing=1' \| jq length` | ≥ 1（bff 同理；**gateway 不应出现在注册表中**） |
| 链路 | Jaeger 三个服务名 | 见 B-3 |
| 消息 | kafka `127.0.0.1:9092`，outbox topic | 下单后有消息（E3） |
| 日志 | 三份进程日志 | 各自含启动/退出序列 |
| 健康 | gateway `8080/healthz/live`、bff `8081/healthz/live` | 200；srv 无 HTTP 端点，靠 consul TTL 心跳 |
| 限流/鉴权 | gateway `internal/ratelimit`、`auth`（若实现） | 404/429/502 原样透传（不改写为 200） |

#### B-5 判定清单

```bash
# 启动顺序：srv → bff → gateway，每步等 /healthz/live 或 consul 就绪
curl -s 'http://127.0.0.1:8500/v1/health/service/demo-srv?passing=1' | jq 'length'      # ≥1
curl -sf http://127.0.0.1:8081/healthz/live
curl -sf http://127.0.0.1:8080/healthz/live
curl -sf http://127.0.0.1:8080/api/ping        | grep -q '"service":"bff"'              # gateway→bff
curl -sf http://127.0.0.1:8080/api/order/1001  | grep -q 'order_id'                     # →bff→srv→mysql
# 上游不可达时 gateway 必须 502，不得 200 静默
```

| 判据 | 通过条件 |
|---|---|
| consul 实例数 | `demo-srv` ≥ 1、`demo-bff` ≥ 1、`demo-gateway` = 0 |
| 三个服务名同 trace | 是 |
| 根 span = 1 | 是 |
| 上游停掉后 gateway 返回 | 502（原样透传），非 200 |
| srv `main.go` ≤ 50 行、无 `micro_apps/infra` | 是（E3） |

---

### 13.3 案例 C — 网格（`examples/mesh`）

#### C-0 「业务代码与入口在哪」——代码面 vs 部署面的边界

`examples/mesh/` 里**故意没有**业务分层代码和 `main.go`。这不是漏写，而是 launchx 的核心主张（C4 / T8）：**换形态不改业务代码**。网格与微服跑的是**同一份二进制**，差异只存在于部署面。

三层的「入口」分别落在：

| 层级 | 入口 | 位置 |
|---|---|---|
| **源码 / 进程入口** | `func main()`：`Bootstrap → OnSetup → Run` | `examples/micro/{gateway,bff,srv}/cmd/main.go`（唯一业务代码所在地） |
| **镜像入口** | Dockerfile 的 `ENTRYPOINT`（即上表的二进制） | `examples/docker/Dockerfile`，`--build-arg SVC=gateway\|bff\|srv` |
| **部署入口** | `scripts/bring-up.sh start/check/down` | `examples/mesh/scripts/bring-up.sh` |
| **流量入口** | Istio IngressGateway（或 NodePort）→ `gateway` Service | `examples/mesh/workloads/gateway.yaml` + `mesh/gateway.yaml`（Gateway/VirtualService，若需要对外暴露） |

所以 `examples/mesh` 是**部署面**，它引用而非复制业务代码。判定这条边界的命令：

```bash
find examples/mesh -name '*.go'                     # 必须为空（出现即 FAIL：网格走了独立代码路径）
grep -rn "image:" examples/mesh/workloads/*.yaml    # 必须指向 launchx-examples/*，且 tag 与 micro 构建产物一致
```

#### C-0b 复用边界：代码零差异，但有三个前提

**结论：`examples/mesh` 直接复用 `examples/micro` 的三个二进制——不重写、不 fork、不重新构建。** 这不是省事，是 C4/T8 的硬约束：一旦网格出现自己的 `main.go`，"换形态不改业务代码"这条主张当场失效，T8 直接 FAIL。

但「直接复用」成立有三个前提，缺一个就跑不起来，而这三个**恰好都是代码面的纪律**：

| # | 前提 | 反面证据（`micro_apps` 现役代码就是反例） |
|---|---|---|
| **P1** | 监听地址来自配置，容器内须为 `0.0.0.0` | `apps/gateway/cmd/.../main.go` 写死 `const listenAddr = "127.0.0.1:8080"` —— `127.0.0.1` 在 Pod 内只有本容器可达，**sidecar 从 Pod 网络进不来** |
| **P2** | 注册中心地址来自配置 | `const consulAgent = "127.0.0.1:8500"` 写死，Pod 内指向自己 |
| **P3** | **注册进注册中心的「自身地址」来自配置** | 现役代码把 `listenAddr` 当自身地址注册；Pod 里会注册成 `127.0.0.1`，bff 拿到后连自己 → 断链 |

P3 的解法 launchx 侧已具备：Downward API 把 `status.podIP` 注入 env，配置里写 `${POD_IP}`，由 `configsrc` 的 `envConfig` 做 `${ENV}` 展开落地 —— **零代码改动**。

```yaml
# mesh/workloads/srv.yaml 片段
env:
  - name: POD_IP
    valueFrom: { fieldRef: { fieldPath: status.podIP } }
# mesh/configs/srv.yaml 片段
service:
  self:
    addr: ${POD_IP}      # ← 运行时展开为 Pod IP
```

```bash
# 网格复用前必须先过这两条（在代码面执行）
grep -rn "127\.0\.0\.1\|localhost" --include=*.go examples/micro/   # 必须为空：地址一律来自配置
grep -rln "POD_IP" examples/mesh/workloads/ examples/mesh/configs/  # 两侧都必须命中
```

#### C-0c 三个不能靠「复用」解决的部署面冲突（必须显式决策并落 `docs/verify.md`）

**① 服务发现载体冲突（最硬的一个）**：consul 在宿主，Pod IP 在 minikube 容器网络（`172.17.x`），**宿主到 Pod IP 不可路由**，且 consul 的主动健康检查会全失败 → 实例全 critical，bff 发现不到任何 srv。三选一：

- **A（推荐）consul 部署进集群**：`launchx-mesh` 内起 consul，配置副本只改 `coordx.addr → consul.launchx-mesh.svc:8500`。**代码零改动**，代价是多一个镜像与有状态负载。
- **B `hostNetwork: true`**：Pod 复用宿主网络命名空间、IP 可达，但 hostNetwork 下 Istio 的 iptables 拦截需特殊配置且易端口冲突 —— **破坏网格语义，不推荐**。
- **C 网格改走 k8s DNS 直连**（`srv.launchx-mesh.svc.cluster.local:9013`）：需 `rpcx.ClientOptions.Target` 支持直连地址（**待核实**）；且发现方式变更等于代码路径变更，**会削弱「与微服同构」的论证**，仅在 A 不可行时启用。

**② 双套注册表并存**：consul 与 k8s Service 各持一份实例表，Istio 只认后者。判据：consul 中健康实例数 **必须等于** `kubectl get endpoints` 的就绪地址数，差一个即判漂移。

**③ Istio 对 Kitex 段的 tracing 覆盖（待真机核实，不得预先断言）**：Istio 的 tracing 由 Envoy 的 HTTP connection manager 产生；纯 TCP（Kitex TTHeader over TCP）经 `tcp_proxy` 层，**预期不生成 sidecar span**。因此 T10 分两段断言：

- **HTTP 段**（Ingress → gateway → bff）：预期**有** sidecar span；
- **Kitex 段**（bff → srv）：预期**只有应用 span**，无 sidecar span。若真机发现该段出现 sidecar span，回来修正本条说明。

**不得**以「bff→srv 缺 sidecar span」为由判 T10 FAIL —— 那是协议特性，不是断链。

#### C-1 目录树（部署面，**禁 Go 代码**）

```
examples/mesh/
├── namespace.yaml                     # launchx-mesh（带 istio-injection=enabled 标签）
├── workloads/
│   ├── gateway.yaml                   # Deployment + Service（gateway）
│   ├── bff.yaml                       # Deployment + Service（bff）
│   └── srv.yaml                       # Deployment + Service（srv，tcpSocket 探 9013）
├── configmap.yaml                     # ★ 三个 configs/*.yaml 的挂载载体（缺此则配置副本是死文件）
├── telemetry.yaml                     # Istio Telemetry + extensionProvider(opentelemetry)
├── configs/
│   ├── gateway.yaml                   # 微服配置的地址副本（宿主地址 → host.minikube.internal）
│   ├── bff.yaml
│   └── srv.yaml
├── scripts/bring-up.sh                # start / check / down（含镜像构建与 minikube image load）
└── docs/verify.md                     # T12 证据落盘
```

镜像构建面在**代码面一侧**（不进 mesh，以保住「网格无 Go 代码」）：

```
examples/docker/Dockerfile             # 单一 Dockerfile，--build-arg SVC=gateway|bff|srv
```

**构建上下文必须是 `hubx/` 仓库根**，不是 `launchx/`——因为 `go.mod` 的 26 条 replace 指向 `../{webx,configx,logx,dbx,cachex,mqx,otelx,rpcx,...}` 兄弟目录（沿用 `micro_apps/docker/Dockerfile` 已验证的约定）。构建命令：

```bash
cd /Users/hyx/work/gowork/src/hubx            # 仓库根，不是 launchx/
docker build -f launchx/examples/docker/Dockerfile \
  --build-arg SVC=gateway -t launchx-examples/gateway:dev .
minikube image load launchx-examples/gateway:dev
```

配置进容器靠 ConfigMap 挂载（**不 COPY 进镜像**，否则改配置要重建镜像，也破坏「配置副本是唯一差异」的可审计性）：

```yaml
# configmap.yaml 形态（要点）
volumes:        - name: cfg
                  configMap: { name: <svc>-config }
volumeMounts:   - name: cfg
                  mountPath: /app/config.yaml
                  subPath: config.yaml
```

```bash
kubectl create configmap gateway-config --from-file=config.yaml=examples/mesh/configs/gateway.yaml --dry-run=client -o yaml
```

#### C-2 与微服的同构断言（T8）

```bash
# 配置副本差异只在地址段
for s in gateway bff srv; do
  diff <(sed -E 's#[0-9]+(\.[0-9]+){3}:[0-9]+#ADDR#g' examples/micro/$s/config.yaml) \
       <(sed -E 's#[0-9]+(\.[0-9]+){3}:[0-9]+#ADDR#g' examples/mesh/configs/$s.yaml)
done
# 清单侧：镜像指向 launchx-examples/*，探针 tcpSocket 且端口与业务一致（3.13/O3）
grep -n "tcpSocket" -A2 examples/mesh/workloads/*.yaml
```

#### C-3 调用链拓扑（双链并存）

```
同一 trace 内：
  app:      demo-gateway ──► demo-bff ──► demo-srv ──► db.query
  sidecar:  gateway.<ns> ──► bff.<ns> ──► srv.<ns>       （Envoy 自产，与应用 span 并行插入）

反例（T11，应用侧未注册传播器）：
  trace#1  sidecar 完整链（看着严丝合缝）
  trace#2  demo-gateway 孤儿
  trace#3  demo-bff 孤儿
  trace#4  demo-srv 孤儿        → trace 数 ≥ 4
```

判据（**两侧都要查**）：

```bash
curl -s "http://127.0.0.1:16686/api/traces?service=demo-gateway&limit=20"          | jq '.data | length'
curl -s "http://127.0.0.1:16686/api/traces?service=demo-gateway.launchx-mesh&limit=20" | jq '.data | length'
# T10：同一 traceID 下两类 span 并存，且应用 span 父子链仍为 gateway→bff→srv
# T11：trace 数 ≥ 4 即判定为断链（不是"sidecar 链完整"就算过）
```

#### C-4 可观测面

| 面 | 入口 | 判定 |
|---|---|---|
| 注入 | `kubectl get pods -n launchx-mesh` | 每个 Pod READY `2/2`（业务 + sidecar） |
| 控制面 | `istioctl proxy-status` | 全部 `SYNCED` |
| 传播头 | 实测：bff 打印收到的 `traceparent`；或 `istioctl proxy-config log` 打开 `trace` 级 | 头名必须是 `traceparent`（W3C），出现 `x-b3-*` 即断链 |
| 链路 | Jaeger 两套服务名 | 见 C-3 |
| 探针 | `kubectl describe pod` 的 `tcpSocket` | 端口与业务一致；**不用 httpGet**（sidecar 未就绪时 httpGet 会误判） |
| OFF/ON | `hubx.trace` 各跑一次 | OFF 无业务 span，ON 有完整链路，可复现（E5） |

**待核（不凭记忆定稿）**：Istio 的 extensionProvider 选 `opentelemetry` 时 Envoy 走 W3C `traceparent`，与 launchx 的 `Composite{TraceContext, Baggage}` 对齐；选 `zipkin`/`jaeger` provider 会走 B3（`x-b3-*`）直接断链。真机以实测头名确认，结论写回 `docs/verify.md` 与 §11b。

#### C-5 判定清单

| 判据 | 通过条件 |
|---|---|
| `find examples/mesh -name '*.go'` | 空 |
| `kubectl apply --dry-run=client -R` | 全过 |
| `bring-up.sh check` | 底座齐全时 0；缺失时**非零**并显式报缺失 |
| Pod READY | 全部 `2/2` |
| T10 双链并存 | 同一 trace 有 app + sidecar span，app 父子链方向不变 |
| T11 反例 | 关掉应用传播器后 trace 数 ≥ 4（证明该断言非空壳） |
| E5 OFF/ON | 可复现，两侧各一次 |
| T12 证据 | `docs/verify.md` 含 trace-id / span 总数 / app span 数 vs sidecar 数 / 原始响应 / 版本清单 |

---

### 13.4 三类共用的可观测基线与已知缺口（**不得伪装**）

| 能力 | 单体 | 微服 | 网格 | 依据 |
|---|---|---|---|---|
| 分布式链路 | ✅ | ✅ | ✅ | otelx + OTLP 4318 → Jaeger |
| 结构化日志 | ✅ | ✅ | ✅ | logx |
| 日志 ↔ 链路关联（trace_id 落日志字段） | ✅ | ✅ | ✅ | **2026-10-05 真机确认**：webx 访问日志自带 `trace_id` / `span_id` / `request_id`（`webx/middleware/logging.go:34`），实测 `{"trace_id":"993a319f…","span_id":"f464c7d2…","method":"POST","path":"/orders","status":201}`，与 Jaeger 中该请求的 traceID 一致 |
| HTTP 健康端点 | ✅ `/healthz/live` + `/healthz/ready` | gateway/bff ✅；srv ❌ | 由 k8s 探针替代 | `webx/webx.go:130`（`HealthPrefix` 默认 `/healthz`，`webx/config.go:51`；**裸前缀路径无 handler**） |
| 注册与健康检查 | 不注册 | consul TTL + 心跳 | consul + k8s 双层 | coordx |
| metrics / Prometheus | ✅ | ✅ | ✅（**2026-10-10 真机复验**，4/4） | 落点：`internal/telemetry.NewMetrics` 建 MeterProvider（拉取式，`otel/exporters/prometheus`）→ HTTP 档挂 `webx.MetricsPrefix`、srv 档单起 `metrics.addr`；RPC 侧 `rpcx.{requests,request.duration,errors}`。**2026-10-08 实测**：单体 `:8080/metrics`、微服 `bff:8081/metrics`（含 `rpcx_requests_total{rpc_role="client"}`）与 `srv-order:9031/metrics`（`rpc_role="server"`）均返回 Prometheus 文本；网关/域服启动日志打出 `rpc governance: ... metrics=on`。**2026-10-10 网格列实测**（证据 `mesh/docs/verify.md` §7.3）：srv-order `9031/metrics` Pod 内 79 行、宿主 port-forward 抓取不被 sidecar 拦（对照 9022 被 ALLOW 策略拒）、bff `8081/metrics` 含 `rpcx_requests_total{rpc_role="client"}` 且计数与流量吻合、`rpcx_request_duration_seconds` 分桶 `le="0.001"` 起 |
| pprof | ❌ | ❌ | ❌ | 本轮不做 |
| 告警 / 看板 | ❌ | ❌ | ❌ | 本轮不做 |

> 上表是「有没有」的事实登记，不是验收项。仍为缺口的项统一记 **SKIP**，任何把 SKIP 写成
> PASS 的行为按 §15 第 7 条处理。
>
> **metrics 这一格本轮从 ❌ 翻成 ✅/⚠️，翻的依据是真机输出而非"代码里写了"**：
> 判据是 `curl /metrics` 拿到 `# HELP` + 带业务标签的计数器（`rpc_role="client"` /
> `rpc_role="server"`）、以及启动日志里的 `metrics=on`。复跑命令见 `micro/scripts/e2e.sh`
> 的断言 ③（已并入 E2，退出码即结论）。网格列曾记 ⚠️（2026-10-08 清单备好但无集群可验），
> **2026-10-10 起集群逐条复验 4/4 通过后翻 ✅**（命令与输出见 `mesh/docs/verify.md` §7.3，
> 复验过程中修掉的三个环境漂移坑见同文件 §8）。

## 13.5 服务治理分工：哪些交给网格，哪些必须留在应用侧

**结论（带前提）**：在 Kitex 私有协议下，网格能接管的治理**非常有限**。问题不是「应不应该交」，而是**大部分根本交不出去**。

**协议事实（决定了上面这句）**：

- `rpcx` 的 `options.go` / `client.go` 中**没有任何** `ConnType` / `Protocol` / HTTP 传输选项 → Kitex 走默认的 **TTHeader over TCP 私有二进制协议**。
- Istio 对未声明为 HTTP 的端口按 **TCP** 处理（端口名须 `http-` / `grpc-` 前缀，或 `appProtocol` 显式声明）。TCP 模式下 Envoy 走 `tcp_proxy`，**只看得见连接，看不见请求**。
- 后果：L7 治理（按请求重试、按 header/path 路由、速率限制、按错误率熔断）**全部不可用**，可用的只剩连接级能力。这与 §C-0c ③「Kitex 段无 sidecar span」是同一根因。

**分工表（本轮定稿）**：

| 治理能力 | 网格能接管？ | 处置 |
|---|---|---|
| mTLS / 服务身份认证 | ✅ TCP 层亦可 | **交给网格**，应用侧不再自造传输加密假设 |
| 连接级可观测（metrics / 访问日志） | ✅ 部分（span 见 §C-0c ③） | 网格补充，**不替代**应用侧 span |
| 异常实例剔除（outlier detection） | ✅ 主机 / 连接级 | 可交网格，但阈值须与**应用侧熔断**对齐（`xmiddleware.rpcx.circuit_breaker`） |
| 负载均衡 | ⚠️ 仅 L4 连接级；Kitex 自带实例选择 | **留在应用侧**（双侧决策会互相打架）：`xmiddleware.rpcx.load_balancer` |
| 请求级重试 / 超时 | ❌ TCP 层看不见请求 | **留在应用侧**，且严禁双侧同开：`xmiddleware.rpcx.retry`（缺省关） |
| 熔断（按错误率） | ❌ Envoy 读不到业务错误码 | **留在应用侧**：`xmiddleware.rpcx.circuit_breaker`（缺省开） |
| 限流（本地 / 全局速率） | ❌ L7 限流不可用 | **留在应用侧**（gateway 的 `ratelimit` 必须保留） |
| 鉴权（JWT / 用户态） | ❌ 网格只做服务身份，不做用户身份 | **留在应用侧**（`auth` 保留） |
| 灰度 / 流量切分 | ❌ 无 HTTP 属性无法按 path / header 切 | 留在应用侧，或先改协议（见下） |
| 幂等 / 业务降级 / outbox | ❌ 与业务语义强耦合 | **永远在应用侧** |

**三条硬规则（防双重治理，写死）**：

1. **重试不双侧开**：应用侧配了重试，网格侧就不得再开（重试放大在故障期会雪崩）。判据：
   `VirtualService` 的 `retries` 为空 **或** `xmiddleware.rpcx.retry.enabled` 为 false，二者只能居其一。
   本仓缺省取后者（重试关）—— 链路上有非幂等写，且三试 ≈ 9s 已超过入口的 `webx.timeout: 5s`。
2. **超时逐层收敛**：gateway 超时 > bff 超时 > srv 超时（越内层越短）。反之外层先断，内层 span 会成孤儿。
   开重试时还要多看一层：`(1 + max_attempts) × rpcx 的 RPCTimeout` 必须仍小于外层 timeout。
3. **限流只在入口做一次**：限流只在 gateway（应用侧），sidecar 不再叠加。
4. **治理开关必须真被读到**（本轮新增）：`xmiddleware.rpcx` 的三项若写了却没生效，症状是
   「按默认策略跑了很久却没人发现」。装配期会把**只含已生效项**的一行摘要打进启动日志
   （`rpc governance: retry(...) circuit_breaker(...) load_balancer=...`），对照 yaml 一眼可辨；
   非法取值（未知 LB、`max_attempts` 越界、`error_rate` 超范围）**装配期即退**，不静默退回默认值。

**若要真正吃到网格红利**：把 Kitex 传输改为 gRPC / HTTP2（Kitex 支持 gRPC 与 TTHeader over HTTP2），端口声明 `appProtocol: grpc` / `http2`，L7 治理才可用。这是**后续演进项，本轮不做**；不做就必须接受「网格本轮只提供 mTLS + 连接级可观测」这一现实，并在 `docs/verify.md` 写明——**不得宣称「治理已下沉网格」**。

```bash
# Kitex 段不得把端口误声明为 http（会让 Istio 误判协议并静默失败）
grep -rn "appProtocol\|name: http\|name: grpc" examples/mesh/deploy/*/*.yaml
# 双重重试检查：网格侧 retries 与应用侧 rpcx.retry 只能居其一。
# 注意必须锚在 xmiddleware.rpcx 段上：`retry` 在本仓还出现在 lockx（锁重试，无关）
# 与 outbox/mqx（消费端重试）—— 宽匹配会把它们一起捞进来，判据就失去意义。
grep -rn "retries" examples/mesh/deploy/ ; grep -rnA3 "^\s*rpcx:" examples/*/*/config.yaml examples/mesh/apps/conf/*.yaml | grep "enabled"
```

## 13.6 治理下沉决策：注册中心 / 配置中心 / 还有哪些

### 13.6.1 先给判断规则，再逐项裁决

「是否该把治理逻辑剥离到网格」不该一刀切，用**三问**判定，任一为否就不下沉：

1. **是否与业务语义耦合？** 耦合（按用户限流、JWT 用户态、幂等、outbox）→ 留应用侧。网格只认服务身份，不认用户身份。
2. **当前协议下网格能否真正接管？** Kitex 是私有 TCP（§13.5），L7 能力全废 → **不能接管就不要撤应用侧**，否则是裸奔。
3. **下沉是否破坏「同一份二进制」（C4/T8）？** 破坏 → **用换实现代替删代码**（注册中心正解，见 13.6.2）。

### 13.6.2 注册中心：**能下沉，但要用「换实现」而不是「删代码」**

k8s 的 `Service` + `endpoints` + DNS 本身就是注册中心，Istio 的服务发现完全基于它——**注册中心确实是网格能替代的**。但直接把 `coordx.Register` 从业务代码里删掉，就等于微服与网格跑两份代码，T8 当场失效。

**正解是换实现**：业务代码照旧调 `Registry.Register/Deregister`，网格形态下把 `Registry` 的具体实现换成 **k8s no-op registry**（平台已完成注册，应用侧注册退化为记录实例、注销退化为摘除本地状态）。这样：

- 业务代码零改动（还是那一行 `Register`）；
- 治理权真正下沉到平台（`kubectl get endpoints` 是唯一真相源）；
- 代价是 consul 退化为兼容层，**必须显式声明**：网格形态下 **k8s endpoints 为准**，consul 不得再作为流量依据（§C-0c ② 的双注册表由此收敛）。

若本轮不换实现（维持 consul 注册），则按 §C-0c 方案 A 把 consul 部署进集群，**仍属"应用侧注册"**，不得在 `docs/verify.md` 里表述为"注册中心已由网格接管"。

### 13.6.3 配置中心：**不下沉，网格也不该介入**

三条理由，任何一条都足够：

1. **职责不相干**：网格管运行时流量与身份，配置中心管应用配置。Envoy 不参与配置的读取与分发。
2. **ConfigMap 替代不了 Nacos 的动态推送**：Nacos 是长轮询热更新；ConfigMap 改完要么重启、要么应用自己 watch。用 ConfigMap 当配置中心等于**降级能力**。
3. **launchx 的 `configsrc` 已抽象 loader**：`local` / `nacos` 之外再加一个 `configmap` 只是在读挂载文件，语义等价于 `local`，收益为零。

**结论**：配置中心留在应用侧，Nacos（或纯本地）照旧。网格侧只承担**挂载载体**（`examples/mesh/configmap.yaml` + `volumeMounts`）——那是部署面，不是治理权转移。

### 13.6.4 还有哪些：能下沉 / 不能下沉清单

| 能力 | 能否下沉 | 说明 |
|---|---|---|
| 服务注册与发现 | ✅ 可（换实现） | 见 13.6.2；不换实现则维持 consul 且须声明 |
| mTLS / 服务身份认证 | ✅ 可 | TCP 层亦生效，应用侧不再自造加密假设 |
| 连接级可观测（metrics / 访问日志） | ✅ 可（部分） | 补充而非替代应用 span；Kitex 段无 span（§C-0c ③） |
| 异常实例剔除 | ⚠️ 可但会双份 | 应用侧 consul 健康检查与网格 outlier detection 都在做 → 须定其一为准，否则判定漂移 |
| L4 授权（基于服务身份的零信任） | ✅ 可 | `AuthorizationPolicy` 走 SPIFFE 身份，TCP 层可用 |
| **配置中心** | ❌ 不可 | 见 13.6.3 |
| 请求级重试 / 超时 / 熔断 | ❌ 不可 | L7 能力，TCP 下不可用（§13.5） |
| 限流 | ❌ 不可 | 同上；且限流键常含业务语义（用户 / 租户） |
| JWT / 用户态鉴权 | ❌ 不可 | 网格只做服务身份 |
| 灰度 / 流量切分 | ❌ 不可（本轮） | 需先改为 gRPC / HTTP2 传输 |
| 幂等 / 业务降级 / outbox / 分布式事务 | ❌ 永不可 | 业务语义强耦合 |

### 13.6.5 一句话口径

**本轮网格承担：mTLS + L4 授权 + 连接级可观测 + （可选）注册发现的平台化。应用侧保留：配置、限流、熔断、重试、鉴权、业务一致性。** 凡写入 `docs/verify.md` 的下沉结论，必须能指出"应用侧哪一处已撤除或已换实现"；指不出就是虚假陈述（陷阱第 15 条）。

## 14. 判定汇总表（跑一次填一次）

| 编号 | 案例 | 命令锚点 | 状态 | 证据 |
|---|---|---|---|---|
| E1 | 单体跑通 | §3 | ✅ PASS (2026-10-05) | 六接口全通；orders 3→删除后 2；SIGTERM 退出 1s；无 deregister 日志 |
| E2 | 微服调用链 | §6 | ✅ PASS (2026-10-05) | `/api/ping`→bff、`/api/order/1`→srv 返回业务错误（跨进程到达）；consul 仅有 demo-bff/demo-srv，**gateway 未注册**符合装配矩阵 |
| E3 | srv 迁移回归 | §7 | | e2e 输出 + `wc -l` + kafka 消息 |
| E4 | 网格同 trace | §11b | | `examples/mesh/docs/verify.md` |
| E5 | trace OFF/ON | §11b | | 同上 |
| T1/T7 | 传播器 | §9 | ✅ PASS (2026-10-05) | 5 个用例全绿，含 `TestDefaultPropagatorIsNoop` 反向断言（commit `18a331d`） |
| T2/T3/T4 | 数据层入链 | §4 | ✅ PASS（T4 部分 SKIP） | SQL span 9 条、空值 0、JOIN 1、CRUD 四类齐全；T4 的 cache/mq/ES/mongo 四类同 trace 待微服 demo 起后补 |
| T5 | 单体调用树 | §5 | ✅ PASS (2026-10-05) | 6 条 trace 根 span 均恰好 1 个且为 HTTP 入口；树深 5（POST 链） |
| T6 | 三段同 trace | §6 | ✅ PASS (2026-10-05) | 单 trace 9 span、三进程、根唯一；bff 入站 span 挂在 `gateway.proxy.api` 下（Inject 生效） |
| T8 | 网格无专属分支 | §12 | | grep/diff 输出 |
| T9 | 尾部 span 不缺 | §8 | ✅ PASS (2026-10-05) | 最后一条 DELETE trace 7 span 完整（含 `db.delete` × 2），退出冲刷生效 |
| T10/T11/T12 | 网格链路 | §11b / §13.3 | | `examples/mesh/docs/verify.md` |
| T13 | 调用链完整性（分层/中间件矩阵/CRUD 连表） | §9b | ✅ PASS (2026-10-05) | 正向：SQL 9/空 0/JOIN 1/CRUD 2-4-1-2，事务两 INSERT 同父；**反向**：`trace.on.db=false` 后 SQL span 归 0（判据非空壳） |
| 3.14 | 依赖面 | §2/§12 | ✅ PASS（含登记偏差） | 库侧 cloudwego/gorm 零 import；kitex 因 demo 转 direct，已登记「examples 拆 module」后续项 |
| A | 单体案例（目录树/链路/可观测） | §13.1 | | `/tmp/monolith.log` + `/tmp/trace.json` |
| B | 微服案例（三进程/三段链/ consul） | §13.2 | | consul 实例数 + `/tmp/trace.json` |
| C | 网格案例（清单/镜像/挂载/双链/探针） | §13.3 | | `docs/verify.md` + `kubectl` 输出 + `kubectl exec cat /app/config.yaml` |
| C-复用 | 网格直接复用微服二进制（P1/P2/P3 + 决策 A/B/C） | §13.3 C-0b/C-0c | | `grep` 硬编码为空 + `docs/verify.md` 决策记录 |
| OBS | 可观测基线与缺口登记 | §13.4 | ✅（pprof / 告警 / 看板记 SKIP） | 日志-链路关联已由真机确认（webx 访问日志带 trace_id）；**metrics 于 2026-10-08 补齐并真机复验**（单体 + 微服，网格列记 ⚠️ 未复验） |
| GOV | 治理分工（应用侧 vs 网格侧） | §13.5 | | 无双重重试 / 无 L7 虚假陈述 |
| GOV2 | 下沉决策（注册中心 / 配置中心 / 其他） | §13.6 | | 下沉项须指出「应用侧已撤除或已换实现」 |

## 15. 「看起来过了其实没过」陷阱清单

1. **孤儿 trace**：三个服务各一根 trace，每条点开都有 span → 传播器没注册（空 Composite）。判据只有一条：根 span 数 = 1 / trace 数不暴增。
2. **db span 在但没有 SQL**：`db.query.text` 空或只剩占位符 → 埋点没取到语句。必须逐 span 查属性值，不能只看 span 名。
3. **尾部 span 缺失**：链路看着完整但少了最后一次落库 → 退出没冲刷（或用了 defer）。判据是"最后执行的那次操作有 span"。
4. **降级被当成通过**：consul 挂了照样起来 → 这是设计内行为（A3），但**必须**有 Warn 日志；`strict=true` 时必须反过来变成启动失败。只测一种不算过。
5. **网格 sidecar 假完整**：sidecar 之间靠 Envoy 自身透传，链路看着严丝合缝，但 app span 全脱落（trace 数 ≥ 4）。必须同时查应用服务名与 sidecar 服务名。
6. **dry-run 静默通过**：`bring-up.sh check` 对 PATH / minikube / istioctl / kubectl / 集群 Running / istio-system 就绪 / 镜像已 load 每一项都要逐条输出，缺失即非零退出；任一项缺失却返回 0 → FAIL。注意本机 `minikube`/`istioctl` 在 `/usr/local/bin` 而**不在默认 PATH**，`which` 找不到不等于没装——脚本里漏了 `export PATH=/usr/local/bin:$PATH` 会造成「工具齐全却被判缺失」和「真缺失却判通过」两种反向误判。
7. **把缺口记成通过**：pprof / 告警 / 看板三类本轮**不存在**（§13.4）；网格列的 metrics
   **只备了清单、没起集群验过**，故记 ⚠️ 而非 ✅。反过来同样成立 —— **不得伪造端点凑数**：
   一个返回空 JSON 或 `200 {}` 的 `/metrics` 一律 FAIL。本轮把 metrics 从 ❌ 翻成 ✅ 时
   判据是**三层**：有响应体、是 Prometheus 文本（含 `# HELP`）、且出现带业务标签的计数器
   （`rpcx_requests_total{rpc_role="client"|"server"}`）；复跑入口是 `micro/scripts/e2e.sh`
   的断言 ③，退出码即结论。
   > 同族的坑：**分桶与单位错配**也算"假通过"。缺省分桶 `[0,5,10,...,10000]` 是毫秒形状，
   > 而本仓两个直方图声明的单位是 `s` —— 不加显式分桶时所有请求挤进 `le="5"`，端点 200、
   > 计数器也对，只有 p99 是废的。已加显式分桶与防回归用例
   > （`rpcx/governance_test.go` / `webx/middleware/access_stats_test.go` 的
   > `TestDurationBucketBoundsAreSecondsScale`）。
8. **只跑一个进程就宣称微服链路通**：gateway 起来了、bff/srv 没起，`/api/ping` 也可能被 fallback 逻辑返回 200。判据必须同时看 consul 实例数（bff + srv 各 ≥1）与 Jaeger 中三个服务名同 trace，缺一不算过。
9. **bff 端口漂移**：计划 Task 10 曾写 `8091`，本文档定稿为 `8081`。实现与文档必须单一口径；出现两个端口说明有人各写各的，配置与验收已脱节。
10. **网格里混进 Go 代码**：`examples/mesh` 出现 `.go` 文件 = 网格走了独立代码路径，直接违反 T8。哪怕编译通过、链路也通，仍判 FAIL（因为"通"的是另一份代码，不能证明同构）。
11. **单体出现 deregister 日志**：单体不注册，退出序列里若打出 deregister，说明 `mode` 装配串档了（monolith 走了 bff/srv 分支），链路看似正常但装配矩阵已错。
12. **镜像构建上下文错**：在 `launchx/` 下 `docker build` 会因 `go.mod` replace 指向 `../webx` 等兄弟目录而失败（`../` 越出仓库根）。构建必须在 `hubx/` 根执行并 `-f launchx/examples/docker/Dockerfile`。看到 `../webx: no such file` 就是这个错。
13. **配置副本没挂进容器**：`configs/*.yaml` 若只写进 ConfigMap 却没有 `volumeMounts`，Pod 起来会用镜像内默认配置（指向 `127.0.0.1`），在 k8s 里必然连不上宿主中间件。判据：`kubectl exec ... -- cat /app/config.yaml` 必须与 `examples/mesh/configs/<svc>.yaml` 一致，且含 `host.minikube.internal`。
14. **地址硬编码导致网格被迫 fork 代码**：`examples/micro` 的 Go 源码里出现 `127.0.0.1` / `localhost` / 写死的端口常量 → 网格无法复用同一份二进制，只能复制一份改常量，T8 当场失效。判据：`grep -rn "127\.0\.0\.1\|localhost" --include=*.go examples/micro/` 必须为空。**这是本轮最容易复发的一条**：现役 `micro_apps` 三个服务全是这个写法（`const listenAddr = "127.0.0.1:8080"`），照抄即中招。
15. **把「网格装上了」当成「治理已下沉」**：Kitex 是 TTHeader over TCP 私有协议，Istio 按 TCP 处理，`tcp_proxy` 层**没有 L7 治理**（§13.5）。重试、熔断、限流、灰度、鉴权一项都交不出去。若验收报告写「治理已由网格接管」而应用侧配置并未撤除，等于双重治理 + 虚假陈述 → FAIL。正确表述是「本轮网格提供 mTLS 与连接级可观测，L7 治理仍在应用侧」。
16. **把 Kitex 段没有 sidecar span 当成断链**：Istio tracing 由 Envoy HTTP connection manager 产生，纯 TCP（Kitex TTHeader）经 `tcp_proxy` 预期**不生成** sidecar span（§C-0c ③）。判 T10 只看 HTTP 段的 sidecar span 与应用链的完整性；反过来，若 bff→srv 竟然出现 sidecar span，要回来修正该条预期说明。
17. **用 `.attributes[]` 取 Jaeger span 属性**（2026-10-05 实测踩到）：Jaeger REST API（`/api/traces`）返回的 span 属性在 **`.tags[]`**，结构是 `{"key","type","value"}`；`.attributes[]` **不存在**，取之恒为空数组。后果极隐蔽——`db.query.text | length == 0`、"空值数为 0"、`JOIN 数为 0` 全都"通过"，整套 T13 判据变成空壳。判据一律写 `.spans[].tags[]?`（见 §9b.3 顶部警示）。**自检方式**：先跑 `jq '[.data[].spans[].tags[]? | select(.key=="db.query.text")] | length'`，若得到 0 而 Jaeger UI 上明明有 SQL，就是命中本条。
18. **Jaeger 查询早于 batch flush**：OTLP BatchSpanProcessor 默认约 5s 批量导出，请求刚发完立刻查 `/api/traces` 会拿到**上一条** trace（本次还没入库），于是"关闭 db 埋点后 SQL 仍为 2 条"这种假失败就出现了。判据必须**按 traceID 精确定位**新请求的那条（不能只看 `limit=1`），并在请求后等待 ≥10s 再查。
19. **用 `.spans[].process.serviceName` 取服务名**（2026-10-05 实测）：Jaeger 的 span 上只有 `.processID`，服务名在 **`.data[].processes[<pid>].serviceName`。直接取前者得 `null`，"三服务名集合"判据恒为空 → 空壳（与第 17 条同源）。另：`.spans[]` 的迭代顺序不保证与 spanID 一致，核对父子链时须 `sort_by(.id)` 后再看，否则会把 span 名与父指针错配，看起来像"链断了"。
