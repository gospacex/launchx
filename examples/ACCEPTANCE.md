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
| elasticsearch | `127.0.0.1:9200` | **不在 compose 内**，当前由独立容器 `elasticsearch:7.6.2` 提供，重建环境须自补 |
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

依赖面（3.14）：

```bash
grep -rn "cloudwego" --include=*.go .         # 库侧必须为空（仅 examples/micro/{srv,bff} 的 kitex_gen 构造处允许）
grep -n "cloudwego/kitex" go.mod              # 必须是 // indirect
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
curl -sf http://127.0.0.1:8080/api/ping      | grep -q '"service":"bff"'              # gateway → bff
curl -sf http://127.0.0.1:8080/api/order/1001 | grep -q 'order_id'                     # → bff → srv → mysql
```

链路断言：

```bash
curl -s "http://127.0.0.1:16686/api/traces?service=demo-gateway&limit=20" | jq -r '.data[0].traceID'
# 同一 trace 下的服务名集合必须含三个
jq -r '.processes | to_entries[] | .value.serviceName' /tmp/trace.json | sort -u
# 父子链：gateway 是 bff 的父，bff 是 srv 的父（不是三个并列根）
jq '[.spans[] | select((.references|length)==0)] | length' /tmp/trace.json            # = 1
```

判定：服务名集合含 `demo-gateway`/`demo-bff`/`demo-srv`；根 span = 1；父子链方向正确；srv 侧 db span 在同一 trace。**三段各成一根（trace 数 ≥ 3）→ FAIL**（典型症状：传播器没注册，见 §9）。

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
go test ./internal/telemetry/ -run TestInitDisabledStillRegistersPropagator -v   # 已存在：Fields 含三项
go test ./... -run 'TestInject|TestPropagator|TestNoTelemetry' -v                # Task 23 补齐后应命中
```

已覆盖：`TestInitDisabledStillRegistersPropagator`（`internal/telemetry/telemetry_test.go`）断言 `PropagatorFields()` == `baggage/traceparent/tracestate`，且 trace 关闭时仍注册。

**待补（Task 23，tasks 4.1）**：
- 向空 carrier 注入有 span 的 ctx，carrier 中存在格式合法的 `traceparent`（`00-<32hex>-<16hex>-01`）；
- 反向断言 T7：不跑 telemetry 步骤时提取不到上游 span context。

判定：两项都补齐且反向断言确实「提取失败」→ T1/T7 PASS。**反向断言若也能提取到，说明用例是空壳 → FAIL。** 当前只有 Fields 断言时，T1 记为「部分覆盖」，不得记 PASS。

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
istioctl install --set profile=demo -y                    # 含 tracing 所需组件
kubectl label namespace launchx-mesh istio-injection=enabled --overwrite
# 镜像：先在 hubx/ 仓库根构建（构建上下文必须是仓库根，见 §13.3 C-0），再
minikube image load launchx-examples/gateway:dev          # 三服务各一次
```

集群内访问宿主中间件用 **`host.minikube.internal`**（minikube 提供的宿主 DNS 名；`host.docker.internal` 是 Docker Desktop 容器内的写法，在 minikube 的 k8s 里**不保证解析**）——这一点由 `configs/*.yaml` 的地址副本体现，也是网格与微服配置的**唯一**差异来源。

- **T10**：同一 trace 下同时有应用 span（`gateway`/`bff`/`srv`）与 sidecar span（形如 `gateway.launchx-mesh`），且应用 span 父子链仍为 gateway → bff → srv。查询时**两侧都要查**：`service=demo-gateway` 与 `service=demo-gateway.<namespace>`。
- **T11**（反向断言）：只开 sidecar 追踪、应用侧不注册传播器 → 呈分裂形态：1 条 sidecar 链 + 应用侧各自孤儿 trace，**trace 数 ≥ 4**。这是网格特有的伪装——sidecar 自己串得很完整，但 app span 全脱落。
- **E5**：`hubx.trace` OFF/ON 各跑一次，OFF 无业务 span、ON 有完整链路，结果可复现。
- **T12**：证据落盘 `examples/mesh/docs/verify.md`，必须含 trace-id、span 总数、应用 span 数 vs sidecar span 数、sidecar 服务名、Jaeger API 原始响应片段、OFF/ON 对比、minikube/kubectl/istioctl/Istio 版本。

**待核（不凭记忆定稿）**：Istio 的 extensionProvider 必须选 `opentelemetry`（Envoy OTel tracer 走 W3C `traceparent`，与 launchx 的 `Composite{TraceContext, Baggage}` 对齐）；若用 `zipkin`/`jaeger` provider，Envoy 默认走 B3（`x-b3-*`）会直接断链。真机以 `istioctl proxy-config` + 实测请求头名确认。

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
| 健康入口 | `GET /healthz`（webx 默认） | gateway/bff `/healthz`；srv 靠 consul TTL | k8s `tcpSocket` 探针 + consul |

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
| 健康 | `curl -sf http://127.0.0.1:8080/healthz` | 200（webx `Config.HealthPrefix` 默认 `/healthz`，`webx/config.go:31`；`ReadyCheck` 未配时恒 200） |
| 数据 | `mysql order_db` 行数 | 落真库，非内存 |
| metrics | **无** | 见 13.4，`/metrics` 404 是预期，不判 FAIL |

#### A-6 判定清单

```bash
# 启动与业务
cd examples/monolith && export HUBX_DB_PW=hubx123
go build ./... && (go run ./cmd &>/tmp/monolith.log & echo $! >/tmp/monolith.pid)
sleep 3
curl -sf http://127.0.0.1:8080/healthz                                  # 200
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
| 健康 | gateway `8080/healthz`、bff `8081/healthz` | 200；srv 无 HTTP 端点，靠 consul TTL 心跳 |
| 限流/鉴权 | gateway `internal/ratelimit`、`auth`（若实现） | 404/429/502 原样透传（不改写为 200） |

#### B-5 判定清单

```bash
# 启动顺序：srv → bff → gateway，每步等 /healthz 或 consul 就绪
curl -s 'http://127.0.0.1:8500/v1/health/service/demo-srv?passing=1' | jq 'length'      # ≥1
curl -sf http://127.0.0.1:8081/healthz
curl -sf http://127.0.0.1:8080/healthz
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
| 日志 ↔ 链路关联（trace_id 落日志字段） | ⚠️ 待核 | ⚠️ 待核 | ⚠️ 待核 | logx 侧未检索到 trace_id 注入逻辑，需真机确认后再定；未确认前**不得**记 PASS |
| HTTP 健康端点 | ✅ `/healthz` | gateway/bff ✅；srv ❌ | 由 k8s 探针替代 | `webx/config.go:31`（`HealthPrefix` 默认 `/healthz`） |
| 注册与健康检查 | 不注册 | consul TTL + 心跳 | consul + k8s 双层 | coordx |
| metrics / Prometheus | ❌ | ❌ | ❌ | **实测缺口**：`webx.Config.MetricsEnabled`（`webx/config.go:35`）在全仓零引用；otelx 未导出 MeterProvider → `/metrics` 不存在是**预期**，判 SKIP 而非 FAIL，更不得伪造端点记 PASS |
| pprof | ❌ | ❌ | ❌ | 本轮不做 |
| 告警 / 看板 | ❌ | ❌ | ❌ | 本轮不做 |

> 上表是「有没有」的事实登记，不是验收项。缺口项统一记 **SKIP**，任何把 SKIP 写成 PASS 的行为按 §15 第 7 条处理。

## 14. 判定汇总表（跑一次填一次）

| 编号 | 案例 | 命令锚点 | 状态 | 证据 |
|---|---|---|---|---|
| E1 | 单体跑通 | §3 | | `/tmp/monolith.log` + mysql 行数 |
| E2 | 微服调用链 | §6 | | curl 输出 + consul 实例数 |
| E3 | srv 迁移回归 | §7 | | e2e 输出 + `wc -l` + kafka 消息 |
| E4 | 网格同 trace | §11b | | `examples/mesh/docs/verify.md` |
| E5 | trace OFF/ON | §11b | | 同上 |
| T1/T7 | 传播器 | §9 | | 单测输出 |
| T2/T3/T4 | 数据层入链 | §4 | | `/tmp/trace.json` |
| T5 | 单体调用树 | §5 | | `/tmp/trace.json` |
| T6 | 三段同 trace | §6 | | `/tmp/trace.json` |
| T8 | 网格无专属分支 | §12 | | grep/diff 输出 |
| T9 | 尾部 span 不缺 | §8 | | `/tmp/trace.json` |
| T10/T11/T12 | 网格链路 | §11b / §13.3 | | `examples/mesh/docs/verify.md` |
| 3.14 | 依赖面 | §2/§12 | | grep 输出 |
| A | 单体案例（目录树/链路/可观测） | §13.1 | | `/tmp/monolith.log` + `/tmp/trace.json` |
| B | 微服案例（三进程/三段链/ consul） | §13.2 | | consul 实例数 + `/tmp/trace.json` |
| C | 网格案例（清单/镜像/挂载/双链/探针） | §13.3 | | `docs/verify.md` + `kubectl` 输出 + `kubectl exec cat /app/config.yaml` |
| OBS | 可观测基线与缺口登记 | §13.4 | | 缺口项记 SKIP，不得记 PASS |

## 15. 「看起来过了其实没过」陷阱清单

1. **孤儿 trace**：三个服务各一根 trace，每条点开都有 span → 传播器没注册（空 Composite）。判据只有一条：根 span 数 = 1 / trace 数不暴增。
2. **db span 在但没有 SQL**：`db.query.text` 空或只剩占位符 → 埋点没取到语句。必须逐 span 查属性值，不能只看 span 名。
3. **尾部 span 缺失**：链路看着完整但少了最后一次落库 → 退出没冲刷（或用了 defer）。判据是"最后执行的那次操作有 span"。
4. **降级被当成通过**：consul 挂了照样起来 → 这是设计内行为（A3），但**必须**有 Warn 日志；`strict=true` 时必须反过来变成启动失败。只测一种不算过。
5. **网格 sidecar 假完整**：sidecar 之间靠 Envoy 自身透传，链路看着严丝合缝，但 app span 全脱落（trace 数 ≥ 4）。必须同时查应用服务名与 sidecar 服务名。
6. **dry-run 静默通过**：`bring-up.sh check` 对 PATH / minikube / istioctl / kubectl / 集群 Running / istio-system 就绪 / 镜像已 load 每一项都要逐条输出，缺失即非零退出；任一项缺失却返回 0 → FAIL。注意本机 `minikube`/`istioctl` 在 `/usr/local/bin` 而**不在默认 PATH**，`which` 找不到不等于没装——脚本里漏了 `export PATH=/usr/local/bin:$PATH` 会造成「工具齐全却被判缺失」和「真缺失却判通过」两种反向误判。
12. **镜像构建上下文错**：在 `launchx/` 下 `docker build` 会因 `go.mod` replace 指向 `../webx` 等兄弟目录而失败（`../` 越出仓库根）。构建必须在 `hubx/` 根执行并 `-f launchx/examples/docker/Dockerfile`。看到 `../webx: no such file` 就是这个错。
13. **配置副本没挂进容器**：`configs/*.yaml` 若只写进 ConfigMap 却没有 `volumeMounts`，Pod 起来会用镜像内默认配置（指向 `127.0.0.1`），在 k8s 里必然连不上宿主中间件。判据：`kubectl exec ... -- cat /app/config.yaml` 必须与 `examples/mesh/configs/<svc>.yaml` 一致，且含 `host.minikube.internal`。
7. **把缺口记成通过**：metrics / pprof / 日志 trace_id 关联这三类本轮**不存在**（§13.4）。`/metrics` 返回 404 是预期，记 SKIP；若为使表格好看而写 PASS，或临时加个返回空 JSON 的假端点凑数 → 一律 FAIL。
8. **只跑一个进程就宣称微服链路通**：gateway 起来了、bff/srv 没起，`/api/ping` 也可能被 fallback 逻辑返回 200。判据必须同时看 consul 实例数（bff + srv 各 ≥1）与 Jaeger 中三个服务名同 trace，缺一不算过。
9. **bff 端口漂移**：计划 Task 10 曾写 `8091`，本文档定稿为 `8081`。实现与文档必须单一口径；出现两个端口说明有人各写各的，配置与验收已脱节。
10. **网格里混进 Go 代码**：`examples/mesh` 出现 `.go` 文件 = 网格走了独立代码路径，直接违反 T8。哪怕编译通过、链路也通，仍判 FAIL（因为"通"的是另一份代码，不能证明同构）。
11. **单体出现 deregister 日志**：单体不注册，退出序列里若打出 deregister，说明 `mode` 装配串档了（monolith 走了 bff/srv 分支），链路看似正常但装配矩阵已错。
