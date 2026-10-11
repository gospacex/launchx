# examples/mesh 服务网格案例 —— 完整 Code Review

- **审查时间**：2026-10-10 15:00–15:35（UTC+8）
- **审查对象**：`examples/mesh/**`（`apps/` 独立 module、`deploy/{shared,micro,apps,prod,istio}`、`scripts/`、`docs/`），参照 `examples/micro`（对照组）、`examples/monolith`、`examples/ACCEPTANCE.md`
- **审查方式**：全量读码 + 编译器/测试/脚本/配置实测（**不拿文档当结论**，文档只作为"待核验的断言"）
- **代码基线**：`HEAD = 866b30d`；工作区另有 11 个文件未提交改动（见 §5.1，判定与基线分开）
- **修复状态**：**已按 §8 顺序完成修复，见 §9 修复记录**（结论表为审查当时的判定，保留原样以便对照）

---

## 0. 结论速览

| 你的问题 | 结论 | 关键证据 |
|---|---|---|
| mesh 案例是否完整 | **结构完整、口径自洽；但验收基准文档落后于实现，且当前工作树不可提交**；且**micro 对照组已失效**（见 §9.10） | §1、§5.1、§5.7、§9.10 |
| 网关→各微服是否全线入链 | **应用侧全链连续（四层 span 齐全）；sidecar 只覆盖 `ingress→bff` 一跳**，RPC 跳无 sidecar span 是协议事实 | §2 |
| 存储中间件操作是否入链（含连表） | **机制上五类全入链**（含连表）；**但本案例无连表语句，连表覆盖为 0**，且消费侧无 driver span | §3 |
| 是否达到企业级可上线标准 | **没有达到**。3 条 P0 + 7 条 P1 | §5 |

**总评：Request changes（不通过）。**
前两问是对既有实现的确认——这块做得相当扎实（判据脚本、反向探针、证据落盘的纪律在本仓是第一档的）；
后两问是必须处理的：**门禁当前就是红的**，**入口鉴权缺失且文档声称它存在**，**关键判据被 `|| true` 吞掉退出码**。

> **复审结论（2026-10-10，修复后）**：上述 P0/P1/P2 已全部处理完毕，`bash examples/scripts/ci.sh mesh` 转绿。
> 逐条落点与证据见 **§9 修复记录**。原判定保留在上，供「修了什么、依据是什么」对照。
>
> **再复核（按「接收代码审查」纪律，同日晚）**：对 §9 逐条复验代码实体（而非复述文字），
> 发现并修正 §9.2 的一处实现缺口 —— `auth.require=false` 曾是「配置说关、行为仍 401」的谎话，见 **§9.11**。
> 其余条目均在代码中落到实处（可执行判据见 §9.8/§9.9/§9.11）。

---

## 1. 完整度

### 1.1 结构（对照设计意图，齐）

两条并存的证据链都在，且各自闭环：

| 组 | 代码 | 清单 | 镜像 | 进程数 |
|---|---|---|---|---|
| 对照组（证"换形态不改代码"） | 复用 `examples/micro` | `deploy/micro/` | `launchx-demo:dev` | 3（gateway/bff/srv） |
| 原生组（证"为网格写的应用有多瘦"） | `apps/`（独立 module） | `deploy/apps/`（8 文件） | `launchx-mesh-apps:dev` | 5（bff + 4×srv-域） |

- 五个进程各自 `cmd/main.go` + `conf/*.yaml` + `internal/{bff,srv-<域>}` 四层（`rpc/handler/service/repository`）+ `pkg/*`，四域同构（各 8 个子目录）。
- 部署面：`shared/`（namespace/consul/host-middleware/telemetry/peer-authn）+ `apps/`（configmap/四域 srv/bff/gateway/authz/secret/pdb）+ `prod/`（networkpolicy/podmonitor/prometheusrule/hpa，教学集群不 apply）+ `istio/operator.yaml`（`istioctl install -f` 入参，不是 kubectl 对象）。
- 脚本面：`bring-up.sh`（check/image/install/deploy/verify/status/down）+ `route-matrix-{apps,micro}.txt`（判据与 `PROBE_ROUTES` 两处同源）+ `trace-audit.py --route-matrix --mesh`（821 行，含 `+cache`/`+async` 两种行型）。
- 证据面：`docs/verify.md` 落盘 T10/E5/T11(SKIP 如实记录)/T13 矩阵/metrics 4 条判据/环境漂移三坑。

**结论：相对 `mesh-entry-chain` 规格（6 条路由 + 四域 + 异步链 1 条）是完整的**，且比多数示例多出"反向探针 + 反例自检"这一层。

### 1.2 四处**不完整**（详见 §5）

1. **入口鉴权**：整个 `examples/mesh` 里没有 `RequestAuthentication`，`bff` 自身也没有 `AuthorizationPolicy`；凭据缓存无人播种。
2. **连表查询**：mesh 组一条都没有（`ItemsByOrderID` 是独立 SELECT，不是 JOIN），所以"连表入链"在本案例**无证据**。
3. **两个一致性判据**：points 域的幂等（`points_log(order_id)` 唯一约束）与"扣分 + 记账同事务"都缺；outbox 在多副本下无行级认领。
4. **验收基准文档**：`examples/ACCEPTANCE.md` §13.3 仍按旧形态写（见 §5.7）。

---

## 2. 调用链：网关 → bff → 四域 srv

### 2.1 实测链路（连续、无断链）

```
入口：Istio IngressGateway(80, hosts=["*"]) --VirtualService /api rewrite→/--> bff:8081
      [sidecar: istio-ingressgateway outbound|8081||bff... ]  ← M1/M3 的 2 跳之一
      [sidecar: bff inbound|8081||]                           ← 2 跳之二
 bff：routing.audit → routing.identity → routing.rate_limit      （三层各有 span，顺序由 TestChainOrder 钉住）
      → handler.bff.<Op>                                        （6 条路由各一个）
      → kitex client span + client.srv-<域>.<Op>                （rpcx 产出）
 bff→srv：Kitex TTHeader over TCP ⇒ Envoy 只做 tcp_proxy        ← 这一跳永远没有 sidecar span（协议事实）
 srv：  rpc.<域>.<Op> → handler.<域>.<Op> → service.<域>.<Op> → repository.<域>.<Op> → db.query
       （route.Guard 与 middleware 限流**无 span**，见 §5.9）
 异步： service.order.CreateOrder ─(同事务写 outbox)→ Runner 补发
       → messaging.outbox.send → messaging.kafka.send
       → [broker] → mqx.ExtractTrace → service.points.ConsumeOrderCompleted
```

- **判据强度**：不是"有 trace 就算"，而是 `trace-audit.py --route-matrix --mesh` 对**每一行**断言"入口 2xx + 同 trace 内目标域 `service.<域>.*` + 该 trace 含 sidecar span"。这条设计挡得住"六条路由全接到 order 域"这类接线错误，比单条 trace 的断言强一档。
- **可复跑**：`docs/verify.md` §6.5 连跑 7 次（含缓存已热时紧接连跑）均 8/8；两条反例各 `rc=1` 且失败原因精确命中（接错域 / stock 冒充缓存命中）。
- **异步链**：`+async` 行断言"`POST /order` 的 trace 内出现 `service.points.ConsumeOrderCompleted`，traceID 逐字相同"——这是 mqx Inject/Extract 续链真的生效的唯一证据，且它覆盖了 points 域的写路径（该路径不经 HTTP，同步矩阵六行覆盖不到）。

### 2.2 必须说清的两个"不是缺陷"

- **`bff→srv` 无 sidecar span**：Kitex 私有协议经 `tcp_proxy`，Envoy 看得见连接、看不见请求。`docs/verify.md` §4.4 有反证（把端口名从 `rpc` 改成 `grpc-rpc` 后入口直接 502）。这条已经在 README/apps README/verify.md 三处如实写明，**没有伪装成"治理已下沉网格"**——这一点做得对。
- **OFF 档（`trace.enabled=false`）Enovy 侧链仍在、应用 span 为 0**：这是开关语义，不是断链；T11（反向断言）因"传播器恒注册"无法在同构案例上构造，被记成 **SKIP 并说明理由**，没有伪造 PASS。

---

## 3. 存储中间件与连表

### 3.1 五类句柄的 span 落点（读 SDK 源码核对，非推测）

| 句柄 | 落点 | span 名 | 关键属性 |
|---|---|---|---|
| db（gorm via dbx） | `dbx/orm/gorm_tracing.go`，装配期由 `orm.Open` 注册 create/query/update/delete 四类回调 | `db.query` / `db.create` / `db.update` / `db.delete` | `db.query.text`（**完整 SQL**）、`db.sql.table`、`db.system` |
| cache（cachex/redis） | `cachex/redis/redis.go:330` | `db.redis.<COMMAND>` | `db.redis.key` / `db.redis.found` / `db.redis.ttl_ms` |
| kv（kvx） | `kvx/interface.go:194`（`traceflag` 门控） | `db.kvx.<op>` | — |
| lock（lockx/redis） | `lockx/redis/redis.go:80/152/164` | `lock.redis.acquire` / `.release` / `.renew` | `lock.key` / `lock.acquired` |
| mq（mqx/kafka） | `mqx/kafka/kafka.go:258`（生产侧） | `messaging.kafka.send`（+ batch） | `messaging.system/destination/operation` |
| 发件箱 | `examples/pkg/outbox/outbox.go:288` | `messaging.outbox.send` | `messaging.topic` |

### 3.2 连表

- **机制上入链**：dbx 的埋点挂在 gorm 的 create/query/update/delete **回调**上，任何 SQL 都过同一回调——`Joins()`、`Preload()`、`Raw("... JOIN ...")` 与单表 SELECT 走的是同一条路，`db.query.text` 里带完整 JOIN 语句。**不存在"连表因为不是简单查询所以漏埋"的问题。**
- **本案例没有连表**：`examples/mesh/apps` 全仓零 `Joins(` / `Preload(` / `Raw(`（实测）。order 的明细用 `ItemsByOrderID`（独立的 `WHERE order_id = ?`）取，product/stock/points 都是单表。
- **连表判据的覆盖范围**：`trace-audit.py` 里确有 JOIN 检测（非矩阵分支打印 `JOIN 出现：是/否`，并列入 CRUD 覆盖统计），但 **`--route-matrix` 分支不打印也不判定这段统计**；而全仓唯一的连表在 `examples/monolith/internal/repository/order.go:120 GetWithUser`（`orders JOIN users`）。
  ⇒ **"mesh 案例的连表入链"目前是空集**：既没有连表可验，矩阵判据也不检查连表。

### 3.3 两处真实缺口

- **消费侧无 driver span**：kafka 生产侧有 `messaging.kafka.send`，消费侧只有 `mqx.ExtractTrace(ctx, msg)`（`kafka.go:369`）**没有开 span**。所以异步链在消费端的入口只有应用自己的 `service.points.ConsumeOrderCompleted`，链路里**分不出"broker 排队慢"与"业务处理慢"**。
- **缓存与数据库共用 `db.` 前缀**：cachex 用 `db.redis.GET`、dbx 用 `db.query`，在 Jaeger 里从 span 名**分不出这一跳是缓存还是数据库**（只看 `db.*` 都是"db"），必须点开 attribute。这是 SDK 命名口径问题，不是本案例的锅，但会直接影响排障效率。

---

## 4. 企业级可上线评估

按可用性 / 可观测 / 安全 / 容量 / 发布 / 数据一致性六个面逐条对：

| 面 | 已具备 | 缺口 |
|---|---|---|
| 可用性 | bff 双副本 + PDB(minAvailable 1) + HPA 底稿(2→4)；liveness/readiness 分端口；`App.Close` 每环有界（`stop_timeout` 5s，超时只 Warn）、points 消费循环取消后有界 drain(3s) | 四域 srv 单副本（自证：单副本配 PDB 会死锁）；入口 Gateway 无 TLS（prod README 已登记为待办） |
| 可观测 | trace 全链 + metrics（HTTP 档复用主端口 / srv 档单开 9031-9034，4/4 真机复验）+ 秒级直方图桶 + 启动期句柄自检 + 降级必落日志 | 告警/看板仅底稿；kafka lag 无 exporter（已声明不写死规则）；消费侧无 span；pprof 无 |
| 安全 | Istio mTLS STRICT + 四域 ALLOW(sa/bff) + NetworkPolicy 底稿 + Pod/容器 securityContext（runAsNonRoot 65534 / readOnlyRootFilesystem / drop ALL / seccomp）+ DB 口令走 Secret | **入口鉴权完全缺失 + 越权**（§5.2）；Secret 是进仓占位口令；镜像 `:dev` tag |
| 容量 | 连接池显式（max_open 20 / redis pool 50·30）、限流两道（入口按 IP + 域内按主体 120/min）、熔断默认开、重试默认关（有超时预算论证） | outbox 轮询 3s/50 条无行级认领（多副本会重复投递，§5.6）；无限流压测数据 |
| 发布 | 滚动 + PDB + 双副本；配置走 ConfigMap（subPath），不 COPY 进镜像 | 无灰度/金丝雀（未用 VirtualService 权重）；镜像 tag 非不可变 |
| 数据一致性 | 下单三表 + outbox **同事务**；库存条件 UPDATE（`available >= ?`）从根上防超卖；缓存写后失效顺序正确（先落库再删缓存）；积分 upsert 原子累加（`ON DUPLICATE KEY UPDATE`） | **points 域无幂等键 + 加余额与记流水不同事务**（§5.5） |

**判定：教学/演示级已达标；企业级上线不达标**，卡点是 §5.2（安全）、§5.5/§5.6（一致性）、§5.1（门禁）。

---

## 5. 问题清单

### 5.1 [必须修复] 当前工作树门禁就是红的（测试编译不过）

**现象**（实测 `examples/mesh/apps`）：`go build ./...` 通过，但 `go vet ./...` / `go test ./...` 直接编译失败：

```
internal/srv/order/service/order_test.go:178: not enough arguments in call to New
    have (*fakeRepo, nil, *fakeLogger, bool)   want (orderRepo, cachex.Cache, lockx.Locker, logx.Logger, bool)
internal/srv/order/handler/order_test.go:50: too many arguments in call to service.New
internal/srv/order/repository/order_test.go:46: not enough arguments in call to repo.Create（缺 fill 参数）
internal/srv/order/rpc/order_test.go:102: too many arguments in call to service.New
```

**根因**：工作区正在做"去掉 service 层 MQ 直连、改走 outbox"的改造（`service.New` 由 6 参降为 5 参，`repo.Create` 增加 `fill` 回调），但 `_test.go` 只改了一半——同一个 `service/order_test.go` 里 4 参（L157/165/178/220…）、5 参（L238/255…）、6 参（L499/515）三种写法并存。

**影响**：`examples/scripts/ci.sh`（= 本地与 CI **同一份**门禁）对 `examples/mesh/apps` 跑 `build + vet + test`，vet 即失败 ⇒ **CI 必红**；同时 `openspec/changes/add-mesh-native-apps/tasks.md` 已 27/27 全勾选。

**已核**：HEAD 基线自洽（`git show HEAD:` 逐个核对：`service.New` 6 参与未改动的 `handler_test`/`rpc_test` 一致，`repo.Create` 3 参与 `repository_test` 一致），所以这是**未提交的在途改动**引入的，不是已提交缺陷。

**建议**：
1. 把 4 个测试文件的 `New` / `Create` 调用按新签名一次改齐（`go test ./...` 必须绿再提交）；
2. 提交前跑 `bash examples/scripts/ci.sh mesh`；
3. ⚠️ 该目录同一时间可能有另一会话在改（`git status` 显示这 5 个文件均为 `M`），**提交按路径显式 `add`，别用 `-A`**，并先 `git diff --cached --name-only` 扫一遍范围。

### 5.2 [必须修复] 入口鉴权缺失 + 越权（IDOR），且文档声称鉴权已下沉

**证据链**：

1. `deploy/apps/50-authz.yaml` 只有 **4 条** `AuthorizationPolicy`，全部是 `srv-<域>` 只收 `sa/bff`；**`bff` 自身没有任何 AuthorizationPolicy**；
2. 全仓（`examples/mesh/**`）**不存在 `RequestAuthentication`** 资源——但 5 处注释都写着"鉴权由网格的 RequestAuthentication + AuthorizationPolicy 承担"（`bff/app/setup.go:84`、`pkg/routing/routing.go:19`、`pkg/routing/identity.go:31`、`conf/bff.yaml:63`、`10-configmap.yaml` 副本）；
3. `pkg/routing/identity.go` 的凭据反查**永远 miss**：全仓无人播种 `mesh:bff:token:<token>`，`apps/conf/*.yaml` 里也没有 `demo.auth_token` 这个键（`examples/micro/bff/config.yaml:71` 与 `examples/monolith/config.yaml:60` 都有，并各自有启动期播种代码）。⇒ `identity` 层恒为匿名，只会在日志里刷 Warn；
4. `internal/bff/handler/mall.go:149-151`：匿名时**回落信任请求体的 `user_id`** ⇒ 任何人可替任意用户下单；
5. `internal/srv/order/service/order.go:145 GetOrder` 与服务端均**不校验订单归属** ⇒ 任何人可读任意订单（`GET /order/:id` 只校验 id 是正整数）；
6. 入口：`40-gateway.yaml` 的 Gateway 是 `hosts: ["*"]` + `port 80` 明文，无 JWT、无 RequestAuthentication。

**影响**：`micro` 对照组的口径是 `auth.require: true` + 令牌播种（bff 与四域都 require），**apps 组是安全姿态上的回退**，而文档读起来像"更好了（下沉给网格）"。

**建议**（任选其一，但不能维持现状）：
- **要做**：补 `RequestAuthentication`（JWT，provider 指向真实 issuer）+ 一条作用在 `bff` 上的 `AuthorizationPolicy`（`requestPrincipals` 或 `when` 校验 claim）；`createOrder` 在匿名时**拒绝**而不是回落 body；`GetOrder` 加"订单归属 == 主体"校验；若要保留演示用的静态令牌，就照 micro 那样播种 `mesh:bff:token:<token>` → `user_id`。
- **不做**：把 README / apps README / ACCEPTANCE / 源码注释里的"鉴权下沉给网格"统一改成"**本示例不含入口鉴权，仅供教学**"，并在 ACCEPTANCE §13.4 登记为已知缺口（现有 SKIP 登记表就有位置）。

### 5.3 [必须修复] 关键判据被 `|| true` 吞掉退出码

`examples/mesh/scripts/bring-up.sh:354`：

```bash
"${PYTHON:-python3}" "${EXAMPLES}/scripts/trace-audit.py" ... --route-matrix ... || true
```

`trace-audit.py` 的矩阵分支在 FAIL 时 `return 1`（`trace-audit.py:718-720`），但 `cmd_verify` 是脚本最后一条语句且带 `|| true` ⇒ **`bring-up.sh verify` 恒返回 0**，矩阵 0/8 也"成功"。

这与本仓自己的纪律直接冲突：`ci.sh` 第 8 行明写"写在 YAML 里会被人加一句 `|| true` 让某一步静默失效，而这类失效正是门禁存在的意义所在"。

**建议**：去掉 `|| true`（脚本是 `set -euo pipefail`，去掉后 FAIL 会正确地让脚本非零退出）。若确实想"继续打印后面内容"，改成捕获退出码、末尾 `exit "$rc"`。

### 5.4 [建议修改] 消费侧无 driver span，异步链缺一段可观测

`mqx/kafka/kafka.go:369` 消费循环只做 `mqx.ExtractTrace` 不开 span；生产侧 `kafka.go:258` 有 `messaging.kafka.send`。
后果：`POST /order` → 积分到账这段里，"消息在 broker 待了多久"没有 span 可算（outbox 是 3s 轮询，实际延迟大部分在这里）。

**建议**：在 `mqx` 消费侧补 `messaging.kafka.consume`（属性建议带 `messaging.destination` / `messaging.kafka.offset`）。这是 SDK 侧改动，落点不在本仓；退而求其次可在 `srv-points` 的 `consumeHandler` 里补一层 `layer.Begin(ctx, "messaging", "points", "Consume")`，覆盖面小但成本低。

### 5.5 [建议修改] points 域缺幂等与事务（注释已自认，但属上线阻断项）

- `internal/srv/points/repository/points.go`：`AddBalance` 与 `AddLog` 是两条独立语句，**不在同一事务**；`AddLog` 失败时余额已经加上，流水缺失 ⇒ 对不上账。
- `points_log.order_id` **无唯一约束** ⇒ 消息重投（kafka 至少一次 + mqx 的 `handleWithRetry`）会重复加分；`service/points.go:56-58` 注释里承认"本例不带幂等键"。

**建议**：`points_log(order_id)` 加唯一索引（`examples/scripts/order_db.sh` 的 `points_account.user_id` / `stocks.product_id` 已有"TABLES 之外单独幂等补建"的先例，照抄即可），并把"加余额 + 写流水"放进一个 `tx`；或先 `INSERT ... ON DUPLICATE KEY UPDATE` 流水作为幂等闸门，再累加余额。

### 5.6 [建议修改] outbox 补发器在多副本下会重复投递

`examples/pkg/outbox/outbox.go`：`pending()` 是裸 `SELECT ... WHERE status = 0 ORDER BY id LIMIT 50`，**没有行级认领**（`FOR UPDATE SKIP LOCKED` 或 `status=1 抢占`），`sendOne` 之后再 `markSent`。
⇒ 一旦 `srv-order` 提到 ≥2 副本（`60-pdb.yaml` 的注释里正是这个方向），两个 Runner 会同时捞到同一批行、各发一次；`markSent` 的 `status = pending` 守卫只能防"重复标记"，防不住"重复投递"。

**建议**：`pending()` 改为"先 `UPDATE ... SET status = 1(claimed) WHERE status = 0 LIMIT n` 再 `SELECT` 认领结果"，或加 `claimed_at` + 租约；同时把这条写进 `60-pdb.yaml` 那段"扩副本前提"的说明里（现在只提了 kafka 消费组与 lockx）。

### 5.7 [建议修改] 文档与实现不同步（清单）

| 文件 | 现文 | 实际 |
|---|---|---|
| `examples/ACCEPTANCE.md` §13.3 | "`find examples/mesh -name '*.go'` **必须为空**（出现即 FAIL）"、目录树写 `workloads/`+`configs/`+`namespace.yaml`、镜像只复用 micro | 现状是 `apps/` 独立 module（Go 代码）+ `deploy/{shared,micro,apps}`；同文件 §13.5/§13.6 已按 apps 更新 ⇒ **一个文件里两套口径** |
| `examples/ACCEPTANCE.md` §13.4 | 告警/看板三列全 ❌ | 已有 `deploy/prod/{prometheusrule,podmonitor}.yaml` 底稿（未 apply，措辞可保留但需注明"底稿已备"） |
| `examples/mesh/README.md` §1 | 目录树缺 `05-secret.yaml` / `60-pdb.yaml` / `deploy/prod/` / `shared/50-peer-authn.yaml` | 均在仓中且会被 `apply -R` 扫到 |
| `examples/mesh/README.md` §1 | "10-configmap（五份配置的**逐字镜像**）" | 实测：**键值逐条一致 ✓，注释不同 ✗**（`srv-order.yaml` 注释 122→120 行、`bff.yaml` 93→106 行，含新增的"容器内必须监听全地址"一句） |
| `apps/README.md` §4.3、`internal/bff/handler/mall.go:129`、`internal/srv/order/handler/order.go:47` | "本示例不带 outbox，投递失败只告警"、"service 层在落库后调 publish" | 工作区已装配 outbox Runner + 同事务落库（`order/app/setup.go`） |
| `pkg/routing/identity.go:83` 的 Warn hint | "凭据播种见 conf 的 `demo.auth_token`" | 本变体没有这个配置键（排查信号把人引到不存在的键） |
| `scripts/route-matrix-apps.txt` 头注 | 只描述 4 段的普通行格式 | 还有 `+cache`(6 段) / `+async`(3 段)，格式定义在 `trace-audit.py` 里 |

### 5.8 [建议修改] `handler.CreateOrder` 创建后再查一次（读放大）

`internal/bff/handler/mall.go:165-172` → `srv-order` 的 `handler.CreateOrder`（`handler/order.go:71`）在 `CreateOrder` 成功后又调 `svc.GetOrder(id)` 来填响应：多 1 次 `FindByID` + 1 次 `ItemsByOrderID` + 1 次缓存回填，而这 3 个字段（user_id/order_id/order_no/total_amount）在创建时都已持有。

**建议**：让 `service.CreateOrder` 返回 `(id, orderNo, total, err)` 直接构造响应；`OrderView.Cached` 那条读路径留给 `GET /order/:id`。

### 5.9 [仅供参考] 若干命名/注释与实现不符

- `internal/srv/order/route/route.go` 包注释写"身份闸门（Auth）→ 业务维度限流"，但 `Guard.Admit` 只按 `id.UserID` 分桶，**不校验也不拒绝**（真正的拒绝只在 `rpc.admit` 对"格式坏的身份头"）。建议改名 `AdmitBySubject` 或补上拒绝分支。
- **route/middleware 两层没有 span**：`rpc → handler → service → repository` 四层有 `layer.Begin`，但 `route.Guard` 与 `middleware.RateLimiter` 一个 span 都没有。限流一旦触发（返回 429），Jaeger 上**看不到是"哪一层拒的"**（只能从 bff 的 429 反推）。建议给两者各加一个 span（成本极低，`routing.rate_limit` 在 bff 侧已有先例）。
- `internal/srv/order/service/order.go:174 MarkPaid` 只被单测调用（未在 `rpc`/`handler` 接线），但不像 `product.UpdatePrice` / `stock.Decrease` 那样在注释里声明"单测驱动、非隐藏入口"。补一句或删掉。
- `examples/mesh/apps/bin/` 下 5 个 linux 二进制与 `.omc/` 均已被忽略（`.gitignore: bin/`、根 `.gitignore:10 .omc/`），未进版本库 ✓。

---

## 6. 做得好的地方（这些别在重构里改坏）

1. **判据是"可执行的"，不是"文档里的一句话"**：`trace-audit.py` 逐路由矩阵 + 两条反例自检（接错域 rc=1 / stock 冒充缓存命中 rc=1，且失败原因精确命中真因），`docs/verify.md` §6.9 连"用来证明锁的判据自己曾有 check-then-act 竞态、单跑 PASS 全模块偶发 FAIL"都记下来了。
2. **装配期 fail-fast**：`pkg/handles` 把"句柄在不在"与"经它执行会不会入链"分开判（`Want` / `Observed` / `Diverged`），且每条 `Probe` 都注明是"实测"还是"代理"，db 用 gorm 回调做**来源实锤**——这是本仓最值钱的一段设计。
3. **端口/配置一致性实测通过**：9021-9024/9031-9034/8081 在"conf、Service port+targetPort、containerPort、探针、bff upstream"五处**全部对齐**；`conf/*.yaml` 与 `10-configmap.yaml` 的**键值逐条一致**（注释有差异，见 §5.7）。这类"错一处就是 Pod Running 但 502 且零报错"的地方做到零漂移，靠的是判据不是运气。
4. **协议边界讲清楚了**：`bff→srv` 无 sidecar span、JSON 治理交不出去、`xmiddleware.rpcx` 是唯一真接线（`governance.go` 三件套 + 启动日志 `rpcGovernanceSummary`），并且明确拒绝"治理已下沉网格"的说法。
5. **降级必留痕**：cache/kv/lock/mq 缺失一律 fail-open **但必打 Warn**（带 `consequence` / `probe` 两个字段），`outbox` 无 producer 时直接 `Error` 而不是静默跳过；`App.Close` 每环有界、超时只 Warn 不升级成 crashloop。

---

## 7. 复跑记录（本次审查实际执行，可重放）

```bash
cd examples/mesh/apps && CGO_ENABLED=1 go build ./...        # ✅ 通过
cd examples/mesh/apps && CGO_ENABLED=1 go vet ./...          # ❌ 4 个测试包编译失败（§5.1）
cd examples/mesh/apps && CGO_ENABLED=1 go test ./...         # ❌ FAIL（order 四包 build failed，其余 ok）
cd examples/mesh/apps && gofmt -l .                          # ✅ 空
cd launchx && CGO_ENABLED=1 go test ./...                    # ❌ examples/monolith 的 TestT4ComponentSpansShareTrace
                                                             #    失败：mongodb span 缺失（本机 mongodb 未入链/未起，
                                                             #    与 mesh 无关，但会污染"门禁全绿"的判断）
# 端口五处一致性 / conf↔ConfigMap 键值比对：脚本实测，见 §5.7 与 §6.3
```

---

## 8. 建议的处理顺序

1. **P0-1** 补齐 4 个测试文件的签名 → `bash examples/scripts/ci.sh mesh` 绿（§5.1）；
2. **P0-2** 入口鉴权二选一：补 `RequestAuthentication` + `bff` 的 AuthorizationPolicy + 归属校验；或把"不含入口鉴权"写实到 5 处文档与 ACCEPTANCE 缺口表（§5.2）；
3. **P0-3** 去掉 `bring-up.sh` 的 `|| true`（§5.3）；
4. **P1** points 幂等 + 事务、outbox 行级认领（§5.5/§5.6）——这两条是"多副本 + 消息重投"下的数据正确性，跟容量规划绑在一起做；
5. **P1** 文档同步（`ACCEPTANCE.md` §13.3 尤其重要：它是验收基准，现在会**误判现状**）；
6. **P2** 消费侧 span、route/middleware span、`Handler.CreateOrder` 读放大、命名/注释（§5.4/§5.8/§5.9）。

---

## 9. 修复记录（2026-10-10，按 §8 顺序执行）

> 本节记录每一条的**落点与证据**，供复查。凡标「已修」的都以**可执行判据**背书（命令 + 结果），
> 不采用「代码里写了」这种主张。当前工作树门禁：`bash examples/scripts/ci.sh mesh` 见 §9.9。

### 9.1 P0-1 测试签名（§5.1）——已修
4 个测试文件的 `service.New` / `repo.Create` 调用按新签名一次改齐；`repo.Create` 增 `fill` 回调；`newRpcTestDB` 补 outbox 表迁移。证据：`go vet ./...` 与 `go test ./...`（apps 模块）转绿。

### 9.2 P0-2 入口鉴权 + 越权（§5.2）——已修（选择「要做」）
- **入口闸门**：`pkg/routing` 增 `WithRequiredIdentity(skip)`，`identityLayer` 在解析不出主体且不豁免时 `AbortWithStatusJSON(401)`；`bff/app/setup.go` 注入解析器 + 强制闸门，豁免仅 `/healthz/{live,ready}`。
- **凭据播种**：`bff/app/setup.go` 启动期写 `mesh:bff:token:<token> → user_id`（键位与 micro 一致）；配置新增 `auth.require: true` / `demo.auth_token` / `demo.seed_user_id`（`conf/bff.yaml` 与 `10-configmap.yaml` 同步）。
- **下单**：匿名或 body `user_id` 与主体不符 → 401 / 403（**不再回落信任 body**）；`codes.Forbidden=10403` → HTTP 403。
- **归属校验（IDOR）**：`rpc.resolveSubject` 返回 `authed` 布尔；`order.GetOrder` / `points.GetAccount` 在主体可信时校验归属，不符回 403。**只对可信来源校验**（回落值不参与，否则越权者可用入参喂全真）。
- **文档/注释口径**：`routing.go` / `identity.go` / `bff.yaml` / `setup.go` 五处「鉴权已下沉网格」的表述统一改为「入口鉴权由应用侧自己做，网格只保留 mTLS + 传输层身份约束」。
- 判据：见 §9.8 的 IDOR/401/403 反向断言用例。

### 9.3 P0-3 `|| true` 吞退出码（§5.3）——已修
`bring-up.sh` `cmd_verify` 尾部去掉 `|| true`；`set -e` 下该命令即子命令退出码。`bash -n` 语法通过。矩阵 FAIL 现在会让 `verify` 非零退出。

### 9.4 P1 points 幂等 + 事务（§5.5）——已修
- `points_log.order_id` 加唯一索引（`model.PointsLog` 的 `uniqueIndex`），是幂等的**正确性**闸门；快路径 `COUNT` 只是优化。
- 新增 `repository.CreditOnce`：`COUNT` 快路径 + 「写流水 → 加余额」在**同一事务**，任一步失败整体回滚。`addBalance` / `addLog` 收敛为收 `tx *gorm.DB` 的非导出方法。
- `service.ConsumeOrderCompleted` 改调 `CreditOnce`，`applied=false`（重复投递）只记 Warn 不报错。

### 9.5 P1 outbox 行级认领（§5.6）——已修
`examples/pkg/outbox`：新增 `StatusClaimed=3` + `DefaultClaimLease`，`pending()` 选取「pending 或租约过期的 claimed」，`claim(id)` 用**条件 UPDATE + RowsAffected** 做 CAS 抢占；`markSent/markRetry/abandon` 的守卫统一为 `status=claimed`。多副本下两个 Runner 不再重复投递。单测 8 条（含重复投递/租约回收/失败回退/重试上限）。

### 9.6 P2 消费侧 span + route/middleware span + 读放大（§5.4/§5.8/§5.9）——已修
- **消费侧 span**：`srv-points` 的 `consumeHandler` 补 `messaging.points.Consume`（kafka 消费侧无 driver span）。
- **route/middleware span**：四域（order/points/product/stock）的 `route.Guard.Admit` 与 `middleware.RateLimiter.Allow` 各补 span（`route.<域>.Admit` / `middleware.<域>.RateLimit`），被拒（429）时能看出**拒在哪一层、按谁计数**。
- **读放大**：`service.CreateOrder` 返回 `CreateOrderResult`，`handler.CreateOrder` 直接拼响应、**不再回读**（去掉 1 次 FindByID + 1 次 ItemsByOrderID + 1 次缓存回填）。

### 9.7 P1/P2 文档与注释同步（§5.7/§5.9）——已修
- `examples/ACCEPTANCE.md`：§13.3 顶部加「形态修订」注（两链形态：micro 对照组 + apps 原生组），C-0/C-1/C-2/C-5 与 §13.0/§12/§11b 的口径就地修订（`deploy/**` 才禁 Go、路径迁 `deploy/{shared,micro,apps}`、apps 有独立 Dockerfile）；§13.4 告警/看板补「底稿已备」。
- `mesh/README.md` §1 目录树补 `05-secret/60-pdb/prod/50-peer-authn`；「逐字镜像」口径修正并给出可执行判据。
- `deploy/apps/10-configmap.yaml`：**实测 5/5 块与 `apps/conf/*.yaml` 逐行相等**（此前 4 份 srv 各漂 2 行注释），已重新生成对齐。
- `route` 包注释：改为「身份**主体分桶** + 限流」，并注明**身份裁决在 `rpc/`**（本包只分桶、不拒绝）；`MarkPaid` 补「单测驱动、非隐藏入口」；`identity.go` 的 Warn hint 改为指向真实存在的 `demo.auth_token` / `demo.seed_user_id`；`60-pdb.yaml` 的扩副本前提补上 outbox 行级认领；`route-matrix-apps.txt` 头注补齐三种行格式。

### 9.8 反向断言（新增/修订的用例）
| 断言 | 用例 |
|---|---|
| 匿名下单 → 401 且不触达下游 | `bff/handler.TestCreateOrderRejectsAnonymous` |
| body user_id 与主体不符 → 403 且不触达下游 | `bff/handler.TestCreateOrderRejectsBodySubjectMismatch` |
| 读他人订单 → 403；读自己订单 → 放行 | `order/rpc.TestGetOrderRejectsForeignOrder` / `TestGetOrderAdmitsOwnOrder` |
| 下单 body 与主体不符 → 403 | `order/rpc.TestCreateOrderRejectsSubjectMismatch` |
| 越权整户（积分）→ 403；匿名不校验归属 | `points/rpc.TestGetAccountRejectsForeignAccount` / `TestGetAccountAnonymousSkipsOwnership` |
| outbox 重复投递 / 租约回收 / 失败回退 / 重试上限 | `examples/pkg/outbox/*`（8 条） |
| 429 的状态码映射不漏码 | `bff/handler.TestStatusOfCoversEveryBizCode`（含 Forbidden→403） |
| 入口闸门四方向（含 `auth.require=false` 不装闸门） | `pkg/routing.TestRequiredIdentityGate`（见 §9.11） |

### 9.9 门禁结论
- `bash examples/scripts/ci.sh mesh`（= gofmt → build → vet → test，apps 独立模块走 `extra_modules`）：**全绿**。
- 集群侧 `bring-up.sh deploy` + `verify`（矩阵判据、退出码即结论）见 `docs/verify.md`；本机集群复验另记。

### 9.10 新发现（本轮修复过程中实测，超出原 §5 清单）——micro 对照组已失效

修复 §5.7 文档同步时，为写「配置副本只差地址段」这条判据，对 `deploy/micro/10-configmap.yaml` 做了一次
**键值往返比对**，发现它**不是**微服源配置的忠实副本：

| 比对对象 | 结果 |
|---|---|
| `gateway.yaml` 块 ↔ `examples/micro/gateway/config.yaml` | ✅ 0 漂移 |
| `srv.yaml` 块 ↔ `examples/micro/srv/config.yaml` | ✅ 0 漂移（`srv/` 是仓库里显式标注的**冻结对照**） |
| `bff.yaml` 块 ↔ `examples/micro/bff/config.yaml` | ❌ **源有/副本缺 18 个叶子键**；副本另有 1 个源已不存在的键 |

**缺的 18 个键**（`bff` 块整段缺失或不全）：
`auth`、`auth.require`、`auth.ticket_ttl`、`demo`、`demo.auth_token`、`demo.seed_user_id`、
`service.upstream.demo-{order,points,product,stock}`（4 个）、
`xmiddleware.cachex` 及其 6 个叶子键（`driver`/`enable_trace`/`instances.demo-bff.{addr,db,pool_size}`）。
**多出来的 1 个键**：`service.upstream.demo-srv`（单 srv 旧形态的残留）。

**为什么这是真的阻断项**（不是"注释不一致"那种）：

1. `bring-up.sh` 的 `cmd_image` 在 `VARIANT=micro` 下用**当前源码**编译（`scripts/bring-up.sh:216-218`：
   `for svc in gateway bff srv; do go build ./examples/micro/${svc}/cmd`），即跑的是**当前 bff 二进制**。
2. 当前 bff 在 `demo.auth_token` 为空时**启动即报错退出**：
   `examples/micro/bff/app/setup.go:177-179` —— `bff: demo.auth_token is empty, auth middleware would reject every request`。
   且它要用 `xmiddleware.cachex.instances.demo-bff` 播种令牌（同文件 :192 `store.GrantToken(...)`）。
3. 而 `deploy/micro/10-configmap.yaml` 的 `bff` 块**既没有** `demo.auth_token`，**也没有** `xmiddleware.cachex`
   ⇒ 该 Pod 起不来（CrashLoopBackOff）；即便起来了，四个 `service.upstream.demo-*` 也全缺，全部路由 502。
4. 拓扑也不匹配：`deploy/micro/` 只有 `20-srv.yaml`（**单个** `demo-srv`），而当前 micro 已是
   `bff` + `srv-{order,points,product,stock}` **五进程**。

**结论**：`VARIANT=micro`（网格的"对照组"）在现状下**不可用**；能跑通并满足验收的是 **apps 原生组**（`VARIANT=apps`）。
注意这与 T8「与微服同构」的诉求相冲突 —— 对照组的**拓扑**（3 进程）已经落后于微服（5 进程），
两边不再同构，因此它作为"对照"的证据力在本轮之后需要重新审视。

**本轮处置（有意保守）**：只做**文档纠偏 + 显式暴露**，**不**就地改建该组：
- 撤回/修正本文档与 `ACCEPTANCE.md` 里"配置副本只差地址段（键值往返比对）"的表述 → 收窄为
  **只对 `gateway`/`srv` 两块成立**，并写明 `bff` 块的滞后清单与后果（`ACCEPTANCE.md` §12、§13.3 C-2）。
- **不**重建 micro 组的原因：把它升到五进程（补 4 个 `srv-*` 清单 + 重生成 ConfigMap + 改 `PROBE_ROUTES`/
  `route-matrix-micro.txt` + 集群复验）属于**改变"冻结对照"语义**的改动，且 4GB minikube 跑不下两套并存，
  不宜在"按 review 修复"的任务里顺手做掉。
- **建议跟进（单列一条变更）**：要么把 micro 组升到五进程与微服重新同构，要么按 `srv/` 那样**整组冻结**
  （镜像改钉到某个历史 tag，而不是每次从当前源码重建），二者必须择一，现状（清单冻结 + 二进制不冻结）是**唯一不可持续的组合**。
- **建议补一条判据**：把上面这次比对固化成 `bring-up.sh check` 的一个步骤（每个 `deploy/*/10-configmap.yaml`
  的块 ↔ 对应源配置，地址归一后逐键比对），否则同类腐化还会再发生且**无声**。

### 9.11 自查修正（2026-10-10，按「接收代码审查」纪律复核）——`auth.require=false` 曾是谎话

对 §9 逐条复核（不拿本节的文字当结论，只认可执行判据）时，发现 §9.2 的实现缺口 —— 这是**本轮自己没做到**，不是新增需求：

- `internal/bff/app/setup.go` 读了 `auth.require`（L102-104），但**只把它用在日志与 Warn 上**（L108 / L164）；
  真正装闸门的 `engine.Use(routing.Chain(...))` **无条件**传了 `WithRequiredIdentity`（原 L154-159）。
- 后果：`auth.require: false` 时走 Warn 分支、打印「every request is accepted」，启动日志亦自称「鉴权已按配置关闭」，
  而闸门**仍装着** ⇒ 每个无凭据请求照样 401。**配置写着 A、行为却是 B** —— 正是本仓最忌讳的一类失效，
  且与 §5.2 当初批的是同一类（只是方向相反：一个是「说了有其实没有」，这里是「说了关其实没关」）。
- 参照实现 `examples/micro/bff/app/setup.go:167-171` 的 `installAuth` 在该分支**直接 return、不挂中间件**
  ⇒ 本组应对齐。

**修正**：闸门改为**按 `requireAuth` 条件挂载**（`opts` 按需 append）。对本变体**行为不变**
（`conf/bff.yaml` 与 `deploy/apps/10-configmap.yaml` 均为 `auth.require: true`，闸门照常生效）；
`auth.require=false` 现在真的不装闸门，注释/日志与行为一致。

**新增反向判据**：`pkg/routing/routing_test.go::TestRequiredIdentityGate`，四方向 ——
开闸门+匿名→401 且**不触达末端** / 开闸门+具名→放行 / 开闸门+豁免路径→放行 / **未开闸门+匿名→放行**。
已做**反向探针**：把第 1 条故意去掉闸门 → `status = 200, want 401` FAIL（判据非空壳），随后还原。

> 记一笔结构性缺口：入口闸门的**接线**此前只有 handler 级用例（各测自己的 401/403）与集群级 `verify`
> （`✓ 无凭据 → 401`）覆盖，**缺一层 bff-app 装配级单测** —— `internal/bff/app` 至今无 `_test.go`。
> 本轮补的是 `pkg/routing` 的闸门语义用例；装配级用例建议与 §9.10 的 micro 重构一并补。

