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
| 网格 | 本机 minikube | 用户给定；**实测 minikube/kubectl/istioctl 均未安装** |
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

**判据（重要）**：本机 minikube/kubectl/istioctl 均未安装 → `check` 必须**显式报出缺失并以非零码退出**；**静默返回 0 视为 FAIL**（这是伪装通过的高发点）。三 Deployment 探针字段须为 `tcpSocket` 且端口与业务一致（3.13/O3）。

### 11b. 人工验证项（close 不硬阻塞，缺底座时记 SKIP）

前置：`brew install minikube kubectl` → `minikube start --driver=driver` → `istioctl install` → `minikube image load` 三服务镜像 → 集群内经 `host.docker.internal` 访问宿主中间件。

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
# 网格配置副本与微服配置的差异只在地址段（*.mesh-external / host.docker.internal）
for s in gateway bff srv; do
  diff <(sed -E 's#[0-9]+(\.[0-9]+){3}:[0-9]+#ADDR#g' examples/micro/$s/config.yaml) \
       <(sed -E 's#[0-9]+(\.[0-9]+){3}:[0-9]+#ADDR#g' examples/mesh/configs/$s.yaml)
done
```

判定：`mode` 枚举无 `mesh` 取值；`examples/` 与库内无 sidecar 相关 trace 分支；`examples/mesh` **不含 Go 业务代码副本**（复用微服 demo 镜像，只换配置与地址段）——出现 `.go` 文件即 FAIL，因为它意味着网格走了独立代码路径，违反 T8「与微服同构」。

---

## 13. 判定汇总表（跑一次填一次）

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
| T10/T11/T12 | 网格链路 | §11b | | `examples/mesh/docs/verify.md` |
| 3.14 | 依赖面 | §2/§12 | | grep 输出 |

## 14. 「看起来过了其实没过」陷阱清单

1. **孤儿 trace**：三个服务各一根 trace，每条点开都有 span → 传播器没注册（空 Composite）。判据只有一条：根 span 数 = 1 / trace 数不暴增。
2. **db span 在但没有 SQL**：`db.query.text` 空或只剩占位符 → 埋点没取到语句。必须逐 span 查属性值，不能只看 span 名。
3. **尾部 span 缺失**：链路看着完整但少了最后一次落库 → 退出没冲刷（或用了 defer）。判据是"最后执行的那次操作有 span"。
4. **降级被当成通过**：consul 挂了照样起来 → 这是设计内行为（A3），但**必须**有 Warn 日志；`strict=true` 时必须反过来变成启动失败。只测一种不算过。
5. **网格 sidecar 假完整**：sidecar 之间靠 Envoy 自身透传，链路看着严丝合缝，但 app span 全脱落（trace 数 ≥ 4）。必须同时查应用服务名与 sidecar 服务名。
6. **dry-run 静默通过**：minikube/kubectl 没装时 `check` 返回 0 → 视为 FAIL，必须显式报缺失并退非零。
