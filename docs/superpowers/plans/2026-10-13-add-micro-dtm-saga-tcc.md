# add-micro-dtm-saga-tcc 实现计划（微服 DTM saga/tcc）

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 在 `examples/micro`（gateway → bff → 四域服）引入 dtm-labs/dtm：Saga 替换现有手动编排下单链，TCC 以新增「预售锁定」端点实现，四域服各挂轻量 gRPC 分支面，数据操作全部经既有六层链入 trace，官方 docker 形态部署 dtm server（独立 `dtm_db`），三层测试全落。

**架构：** bff 新增 `internal/dtm` 编排器（`dtmgrpc` 提交 :36790）；四域服各新增 grpc-go 分支面（:9041-9044，独立 proto，不进 kitex_gen、不注册 consul），分支 handler → 既有 `service` → `repository` 落库；traceparent 编入分支载荷（正向同 trace），补偿/Cancel 新根 + `biz.dtm.gid`；分支写经 dtm 官方 barrier（barrier 表入域业务库）；所有新地址落 `config.yaml`。

**技术栈：** Go 1.27（workspace replace 体系）、`github.com/dtm-labs/client`（dtmgrpc API，钉版）、grpc-go、otelx/layer 分层 span、gorm/dbx（既有句柄面）、docker compose 底座（官方 dtm 镜像 + mysql）。

**Traceability (sddflow):**
- plan-ready: `openspec/changes/add-micro-dtm-saga-tcc/plan-ready.md`
- tasks: `openspec/changes/add-micro-dtm-saga-tcc/tasks.md`
- plan: `docs/superpowers/plans/2026-10-13-add-micro-dtm-saga-tcc.md`

**执行环境事实（开工前必读）：**

- 工作区：`/Users/hyx/work/gowork/src/hubx/launchx`；`examples/micro` 属主模块（module `github.com/gospacex/launchx`），四域 import 路径形如 `github.com/gospacex/launchx/examples/micro/srv-stock/internal/...`。
- 门禁顺序：`bash examples/scripts/ci.sh micro`（gofmt → build → vet → test）。真机用例起底座必须 `export HUBX_DB_PW=hubx123`。
- 本机已知端口冲突：起服前 `docker stop etcdkeeper`（占 8081）；微服底座命令见 `examples/micro/README.md` §5。
- 六层纪律：分支面 handler 只做协议转换，service 是唯一业务判断层；`internal/handler` 不做业务校验；service 禁 import pb/kitex。

## 文件结构（锁定分解）

**新增：**

| 文件/目录 | 职责 |
|---|---|
| `examples/micro/pkg/dtmx/barrier.go` | barrier 包装：`BarrierFromCtx`（`dtmgrpc.BarrierFromGrpc`）+ `Call(ctx, sqlDB, cb)`（事务即官方 barrier.Call 语义，回滚/提交收敛一处） |
| `examples/micro/pkg/dtmx/payload.go` | 分支载荷结构 `Payload{Biz json.RawMessage, Traceparent, FailInject string}` → `Decode`/`MustEncode`；`ExtractParent(ctx, tp)`（otel MapCarrier，正向续链唯一入口） |
| `examples/micro/pkg/dtmx/result.go` | 失败语义分流：`ResultFailure(err)` / `IsResultFailure(err)` / `Normalize(err)`（业务失败 → dtm 不重试语义；基础失败原样上抛） |
| `srcn/srv-<域>/internal/dtm/protos/busi.proto` | 每域分支 proto（service `dtmbusi.<域>Busi`，方法名固定 `Deduct/Release/Try/Confirm/Cancel` 子集） |
| `srv-<域>/internal/dtm/{busi,setup,store}_*.go` | 分支面：`setup` 起 grpc-go listener（config `dtm.branch.listen_addr`）；`busi` handler（开 `rpc.dtm.<entity>.<op>` span、载荷 Decode、fail_inject 分流、调 service）；`store` 经 `RawDB()` + barrier 执行原始 SQL 守卫写 |
| `srv-<域>/internal/repository/{dtmstore,_test}.go` | `RawDB()`（gorm 官方访问器 `( *gorm.DB).DB()`，不自行建连）+ 各 barrier 型写的原始 SQL（liberate 官方 barrier 须 database/sql 层） |
| `srcv/srv-<域>/internal/service/dtm_*.go` | dtm 分支 service 方法（六层 span：`service.<entity>.<op>`，业务失败分流，转调既有 repo / dtmstore） |
| `bff/internal/dtm/{saga,tcc,trace}.go` | Saga/TCC 编排器 + traceparent 注入 + `fail_inject` 透传 |
| `bff/internal/dtm/*_test.go` | 编排单测（分支序/载荷/逆序） |
| `examples/micro/dtm_e2e_test.go` | 真机集成测试（缺底座 Skip） |
| `examples/micro/scripts/dtm_e2e.sh` | 三段脚本级验收 |
| `examples/micro/scripts/guard_dtmsvc.sh` | 「分支 handler 禁直连 dbx / 必经 service」静态守卫 |

**修改：**

| 文件 | 改动 |
|---|---|
| `go.mod` / `go.sum` | + `github.com/dtm-labs/client`（钉版） |
| 底座 compose（外部 `~/work/gowork/src/hubx/dockers/`） | + dtm 服务（官方形态）+ `dtm_db` 建库 |
| `examples/mall/scripts/schema.sql` | + 四域 `dtm_barrier.barrier`、`product_db` 销量表、stock/points 冻结列、order `presale_locked` 状态 |
| `srv-<域>/config.yaml` | + `dtm.branch.listen_addr` |
| `bff/config.yaml` | + `xmiddleware.dtm.grpc_addr` |
| `bff/internal/{handler,route}` | `POST /api/order` 改挂 Saga；+ `POST /api/presale/lock`；手动 `ReleaseStock` 补偿删除 |
| `examples/micro/trace_e2e_test.go` | + DTM trace 集成断言 |
| `examples/micro/README.md`、`examples/ACCEPTANCE.md`、`docs/verify-chain.md` | 文档口径同步 |

---

### Task 1: 依赖接入与钉版

> **trace:** plan-ready.md → `### Task 1: 依赖接入与钉版（对应 tasks.md 0.1）` | tasks.md → `- [ ] 0.1 依赖接入与钉版：root \`go.mod\` 引入 \`github.com/dtm-labs/client\`（实现时以最新稳定版钉版号并登记），确认 workspace replace 体系与之无冲突（dtm client 不在 replace 列表，走 module cache）。验收：\`go build ./...\` 通过且 \`go.mod\`/\`go.sum\` 含该依赖`
> **sync:** tasks.md → `- [ ] 0.1 依赖接入与钉版：root \`go.mod\` 引入 \`github.com/dtm-labs/client\`（实现时以最新稳定版钉版号并登记），确认 workspace replace 体系与之无冲突（dtm client 不在 replace 列表，走 module cache）。验收：\`go build ./...\` 通过且 \`go.mod\`/\`go.sum\` 含该依赖` | plan-ready.md → `### Task 1: 依赖接入与钉版（对应 tasks.md 0.1）`

**Files:**
- 修改：`go.mod`、`go.sum`、`docs/superpowers/plans/2026-10-13-add-micro-dtm-saga-tcc.md`（Task 1 完成勾选）

- [x] **Step 1.1：拉取并钉版**

```bash
cd /Users/hyx/work/gowork/src/hubx/launchx
go get github.com/dtm-labs/client@latest
go mod tidy
# 把解析出的版本写进本文件（例如 v1.20.1——以 go list 输出为准）
go list -m github.com/dtm-labs/client
```

- [x] **Step 1.2：验证 build**

运行：`go build ./...`
预期：通过（PASS，无 replace 冲突；dtm client 走 module cache）。

- [x] **Step 1.3：核实 dtmgrpc API 形态（防版本漂移，钉版后立即做）**

```bash
DTM_CLIENT=$(go list -m -f '{{.Dir}}' github.com/dtm-labs/client)
grep -n "func NewSaga" "$DTM_CLIENT/dtmgrpc/saga.go"
grep -n "func .*Add\b" "$DTM_CLIENT/dtmgrpc/saga.go" | head -5
grep -n "func Commit\|func WaitResult" "$DTM_CLIENT/dtmgrpc/saga.go"
grep -n "func NewTcc\|func (.*) CallBranch\|func HandleGrpc? " "$DTM_CLIENT/dtmgrpc/tcc.go"
grep -n "func BarrierFromGrpc" "$DTM_CLIENT/dtmgrpc/dtmgrpc.go" || grep -rn "func BarrierFromGrpc" "$DTM_CLIENT/dtmgrpc/"
grep -n "func (.*) Call(" "$DTM_CLIENT/dtmcli/barrier.go"
grep -rn "ResultFailure\|CodeFailure" "$DTM_CLIENT/dtmgrpc/*.go" | head -8
```

预期：以上每个 grep 都有命中。把 `NewSaga`/`Saga.Add`/`NewTcc`/`CallBranch`/`BarrierFromGrpc`/`Barrier.Call`/失败语义常量的真实签名**登记到本计划 Task 4 的执行记录**；若与下文 Task 10/11/5-9 内嵌代码签名有差异，按真实签名**同步修正后续 Task 的代码**（这是钉版核实，不是占位——修正方式是编辑本计划对应代码块）。
预期：签名与内嵌代码一致或已修正。

- [ ] **Step 1.4：Commit**

```bash
git add go.mod go.sum docs/superpowers/plans/2026-10-13-add-micro-dtm-saga-tcc.md
git commit -m "chore(micro): pin dtm-labs/client for saga/tcc branches"
```

- [ ] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready `任务完成`、tasks.md `0.1` 同步）

---

### Task 2: 底座 compose 追加 dtm 服务

> **trace:** plan-ready.md → `### Task 2: 底座 compose 追加 dtm 服务（对应 tasks.md 0.2）` | tasks.md → `- [ ] 0.2 底座 compose 追加 dtm 服务：官方镜像 + 官方环境变量形态（\`STORE_DRIVER=mysql\`、\`STORE_HOST/PORT/USER/PASSWORD/DB=dtm_db\`、:36789/:36790 双端口，\`PORT/HTTP_PORT/GRPC_PORT\` 就位）；\`dtm_db\` 建库先于 dtm 启动；健康检查指向 gRPC 端口。验收：compose up 后 \`dtm_db\` 出现 \`trans_global\` / \`trans_branch_op\` 自建表，业务四库零 dtm 表`
> **sync:** tasks.md → `- [ ] 0.2 底座 compose 追加 dtm 服务：官方镜像 + 官方环境变量形态（\`STORE_DRIVER=mysql\`、\`STORE_HOST/PORT/USER/PASSWORD/DB=dtm_db\`、:36789/:36790 双端口，\`PORT/HTTP_PORT/GRPC_PORT\` 就位）；\`dtm_db\` 建库先于 dtm 启动；健康检查指向 gRPC 端口。验收：compose up 后 \`dtm_db\` 出现 \`trans_global\` / \`trans_branch_op\` 自建表，业务四库零 dtm 表` | plan-ready.md → `### Task 2: 底座 compose 追加 dtm 服务（对应 tasks.md 0.2）`

**Files:**
- 修改：`/Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml`（外部底座，与现 mysql/redis/kafka/consul/jaeger 同处）

- [ ] **Step 2.1：compose 增段**

在 `docker-compose.yml` 的 services 追加（官方 docker 方案的环境变量形态，等价 `docker run` 文档参数）：

```yaml
  dtm:
    image: dtrlabs/dtm:latest            # 官方镜像名以 hub.docker.com/r/dtmlabs 或官方 README 为准；执行时用 docker pull 实际可拉镜像替换此行
    container_name: dtm
    ports:
      - "36789:36789"
      - "36790:36790"
    environment:
      PORT: "36789"
      HTTP_PORT: "36789"
      GRPC_PORT: "36790"
      STORE_DRIVER: mysql
      STORE_HOST: mysql
      STORE_PORT: "3306"
      STORE_USER: root
      STORE_PASSWORD: hubx123
      STORE_DB: dtm_db
    depends_on:
      - mysql
    healthcheck:
      test: ["CMD", "sh", "-c", "nc -z 127.0.0.1 36790"]
      interval: 5s
      timeout: 3s
      retries: 10
    networks:
      - default
```

> 密码口径与底座 `HUBX_DB_PW=hubx123` 一致；镜像 tag 执行时以官方 README 的 docker run 为准（官方文档形态：`docker run --rm -p 36789:36789 -p 36790:36790 -e STORE_DRIVER=mysql ...`）。镜像名 / `PORT` 型环境变量三行按 docker pull 实测写入，云偏差不算脱发——写实际拉下来的 `REPO/TAG`。

- [ ] **Step 2.2：起服并验证**

```bash
cd /Users/hyx/work/gowork/src/hubx/dockers && (command -v docker-compose >/dev/null || true)
# dtm_db 建库（官方 dtm 自建表，但库要先行）：用既有 mysql 实器
docker exec -i mysql mysql -urot -phubx123 -e "CREATE DATABASE IF NOT EXISTS dtm_db DEFAULT CHARACTER SET utf8mb4;"
docker compose up -d dtm
sleep 6 && docker ps --filter name=dtm
docker exec -i mysql mysql -urot -phubx123 dtm_db -e "SHOW TABLES;" | head
```

预期：`trans_global` / `trans_branch_op` 在 `dtm_db` 中出现；`SHOW TABLES` 输出非空。随后核业务四库零 dtm 表：
```bash
for db in order_db stock_db product_db points_db; do
  docker exec -i mysql mysql -urot -phubx123 $db -e "SHOW TABLES;" | grep -E "dtm_barrier|trans_" && echo "FAIL: dtm tables leaked into $db" || echo "OK $db"
done
```
预期：四库全部 `OK`。

- [ ] **Step 2.3：Kill/重启续推进基础设施可达性预检**

```bash
docker stop dtm && docker start dtm && sleep 3 && docker ps --filter name=dtm --format '{{.Status}}'
```
预期：Status 显示 Up（重启即续推进的**行为**断言归 Task 13 的集成测试）。

- [ ] **Step 2.4：Commit（底座仓不在本仓，改动在此登记即可；本仓登记该任务完成）**

```bash
cd /Users/hyx/work/gowork/src/hubx/launchx
git add docs/superpowers/plans/2026-10-13-add-micro-dtm-saga-tcc.md
git commit -m "docs(plans): dtm compose service landed in external base compose"
```

- [ ] **Task complete**

---

### Task 3: schema 增量 DDL

> **trace:** plan-ready.md → `### Task 3: schema 增量 DDL（对应 tasks.md 1.1）` | tasks.md → `- [ ] 1.1 schema 增量 DDL（登记进 \`examples/mall/scripts/schema.sql\` 系的四库 DDL 口径）：四域各建 dtm barrier 表；\`product_db\` 新增销量表（Saga 销量分支）；\`stock_db\` / \`points_db\` 增补冻结语义所需列（冻结量列或冻结辅助表，与现有可在读列并存）；\`order_db\` 订单状态机增列 \`presale_locked\` 语义。验收：DDL 幂等重放不报错、四库 schema 版本一致，\`dtm_db\` 表不在此口径内`
> **sync:** tasks.md → `- [ ] 1.1 schema 增量 DDL（登记进 \`examples/mall/scripts/schema.sql\` 系的四库 DDL 口径）：四域各建 dtm barrier 表；\`product_db\` 新增销量表（Saga 销量分支）；\`stock_db\` / \`points_db\` 增补冻结语义所需列（冻结量列或冻结辅助表，与现有可在读列并存）；\`order_db\` 订单状态机增列 \`presale_locked\` 语义。验收：DDL 幂等重放不报错、四库 schema 版本一致，\`dtm_db\` 表不在此口径内` | plan-ready.md → `### Task 3: schema 增量 DDL（对应 tasks.md 1.1）`

**Files:**
- 修改：`examples/mall/scripts/schema.sql`（在末尾追加「── DTM 增量（add-micro-dtm-saga-tcc）──」段落）

- [ ] **Step 3.1：追加 DDL**

在 `examples/mall/scripts/schema.sql` 末尾追加（官方 barrier 表 DDL，来自 dtm 文档的 `dtm_barrier.barrier` 同构；官方 `Call` 会在事务内写这张表，列唯一键语义不可省）：

```sql
-- ============================================================
-- DTM 增量（add-micro-dtm-saga-tcc）
--   dtm_barrier.barrier = dtm 官方屏障表（每个业务库一份，归属各域业务库；
--   dtm server 自建事务表在 dtm_db，不落业务库）
-- ============================================================

-- 四域业务库各一份（stock_db / points_db / product_db / order_db）
CREATE TABLE IF NOT EXISTS dtm_barrier.barrier (
  id          BIGINT AUTO_INCREMENT NOT NULL,
  branch_id   VARCHAR(128)  NOT NULL,
  op          VARCHAR(128)  NOT NULL,
  barrier_id  VARCHAR(128)  NOT NULL,
  reason      VARCHAR(45)   NOT NULL DEFAULT '' COMMENT 'the comment of why this barrier exists',
  create_time DATETIME      NOT NULL,
  update_time DATETIME      NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY key_branch_id_barrier_id (branch_id, barrier_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

若迁移脚本按库分文件（以 `schema.sql` 现状为准：每库先 `CREATE DATABASE` + `USE`），上述 `CREATE TABLE` 段落应复制四份，前置各自的 `USE stock_db;` / `USE points_db;` / `USE product_db;` / `USE order_db;`，与文件既有写法同构。

冻结语义与销量表在域内 DTM store Task（Task 5-8）中先给到建表/加列 SQL，再统一回填此处——回填以 Task 5-8 实施后的 SQL 为准：

- `stock_db.stocks` 增列：`frozen INT NOT NULL DEFAULT 0`（`ALTER TABLE stocks ADD COLUMN frozen INT NOT NULL DEFAULT 0,`（幂等写法在 Step 3.2 重放验证中体现））
- `points_db.point_accounts` 增列：`frozen INT NOT NULL DEFAULT 0`
- `product_db` 新表：`sales_counters(product_id BIGINT PRIMARY KEY, order_gid VARCHAR(128) NOT NULL, qty INT NOT NULL, created_at DATETIME, index key (order_gid))`（按 order_gid 可回退）
- `order_db.orders` 增列：`status VARCHAR(32)` 已有列表中支持 `presale_locked` 字面值（不需 DDL 时登记为「无需 DDL，状态机内约定」）

- [ ] **Step 3.2：幂等重放与四库一致验证**

```bash
docker exec -i mysql mysql -urot -phubx123 < examples/mall/scripts/schema.sql
docker exec -i mysql mysql -urot -phubx123 < examples/mall/scripts/schema.sql   # 第二遍
for db in dtm_db order_db stock_db product_db points_db; do
  echo "== $db =="; docker exec -i mysql mysql -urot -phubx123 $db -e "SHOW TABLES;"
done
```

预期：两遍执行均零错误退出；四业务库各自出现 `dtm_barrier.barrier`；`dtm_db` 表不混入业务库（`SHOW TABLES` 里业务四库无 `trans_*`）。
（若 `CREATE DATABASE IF NOT EXISTS` 已在前文出现，无需重复；执行时以文件现状修正前缀语句为宜。）

- [ ] **Step 3.3：Commit**

```bash
git add examples/mall/scripts/schema.sql
git commit -m "feat(schema): dtm barrier tables + presale/frozen/sales DDL"
```

- [ ] **Task complete**

---

### Task 4: 公共件与装配面
#### ⚠️ 执行记录：dtm-labs/client v1.18.7 真实 API（Task 1 Step 1.3 钉版核实，2026-10-13）

本 Task 及其后所有 Task 的 dtm 调用**一律以下表为准**。核实方式：
`go list -m -f '{{.Dir}}' github.com/dtm-labs/client` 后逐文件 grep。

| 用途 | **真实签名（v1.18.7）** | 本计划原假设（**已废弃**） |
|---|---|---|
| Saga 构造 | `dtmgrpc.NewSagaGrpc(server, gid string, opts ...TransBaseOption) *SagaGrpc` | ~~`dtmgrpc.NewSaga`~~ |
| Saga 加分支 | `(*SagaGrpc).Add(action, compensate string, payload proto.Message) *SagaGrpc` | 形状对，~~`payload string`~~ → **必须是 `proto.Message`** |
| Saga 提交 | `(*SagaGrpc).Submit() error` | ~~`Commit()`~~ / ~~`WaitResult(timeout)`~~（**均不存在**） |
| TCC 全局事务 | `dtmgrpc.TccGlobalTransaction(dtm, gid string, tccFunc TccGlobalFunc) error`，`TccGlobalFunc = func(tcc *TccGrpc) error` | ~~`dtmgrpc.NewTcc(dtmAddr, gid)`~~ |
| TCC 分支 | `(*TccGrpc).CallBranch(busiMsg proto.Message, tryURL, confirmURL, cancelURL string, reply interface{}, opts ...grpc.CallOption) error` | ~~`t.CallBranch(url, payload)`~~ |
| barrier 取 | `dtmgrpc.BarrierFromGrpc(ctx) (*dtmcli.BranchBarrier, error)` | 名字对，返回类型 → **`*dtmcli.BranchBarrier`** |
| barrier 执行 | `(*BranchBarrier).CallWithDB(db *sql.DB, busiCall dtmcli.BarrierBusiFunc) error`，`BarrierBusiFunc = func(tx *sql.Tx) error` | ~~`Call(ctx, sqlDB, cb)`~~ |
| 业务失败哨兵 | **官方** `dtmcli.ErrFailure`（`= dtmimp.ErrFailure = errors.New("FAILURE")`），判别用 `errors.Is(err, dtmcli.ErrFailure)` | ~~自造 `dtmx.ErrBizFailure` + `IsBusinessFailure`~~ |

**由此产生的三条强制修正：**

1. **`dtmx/result.go` 改包官方哨兵**：`BusinessFailure(err) = errors.Join(dtmcli.ErrFailure, err)`；
   `IsBusinessFailure(err) = errors.Is(err, dtmcli.ErrFailure)`。
   **不得自造 `ErrFailure`** —— 自造哨兵 dtm server 认不出，业务失败会被当基础失败无限重试。
2. **`dtmx/barrier.go` 改包 `*dtmcli.BranchBarrier`**：`BarrierFromCtx` 对外仍返回
   `(*Barrier, error)`，内部 `inner *dtmcli.BranchBarrier`；
   `Call(ctx, sqlDB, cb)` 内部转调 `inner.CallWithDB(sqlDB, func(tx *sql.Tx) error { return cb(ctx, tx) })`。
   对外签名不变（域服调用点与 Task 12 单测据此稳定）。
3. **Task 10/11 编排器按上表重写，且 TCC Confirm 由 dtm 自动驱动**：
   `TccGlobalTransaction` 的实现是 Prepare → `tccFunc`（Try）→ `defer` 中 Submit/Abort，
   即 **Try 全绿后 dtm 自己逐分支回调 Confirm**；任一 Try 失败则 dtm 回调 Cancel。
   本计划原设想的「bff 在 Try 成功后走 `t.Submit()` 再提交一次 Confirm 分支集」
   **是多余的，且 `WaitResult`/`Submit` 在 `TccGrpc` 上都不存在** —— 一律删掉。

**分支寻址（Saga / TCC 一致）**：action / compensate / try / confirm / cancel 均为
**URL 字符串**，形态 `<host:port>/<proto-service>/<method>`（无 scheme 前缀），
例如 `127.0.0.1:9043/dtmbranch.StockBusi/Deduct`。dtm server 用内置 grpc driver
把它拆成 server + method 再拨号。**因此各域 `busi.proto` 的 service 全名必须与 URL 里
的 `<proto-service>` 逐字一致**；对不上时 dtm 回调 404，且只在 dtm server 日志里可见。
Task 4 必须把该字符串来源收敛到单一常量。

**payload 形态**：Saga/TCC 的 payload 是 `proto.Message`，故 traceparent / fail_inject
必须是 **busi.proto 里的字段**（不是 JSON 字符串）。`dtmx/payload.go` 的
`Payload{Biz json.RawMessage, Traceparent, FailInject string}` 降级为
**分支 handler 侧的反序列化工具**（proto 字段 → 本包结构体，供 service 用），
不再是提交侧的包装类型。


> **trace:** plan-ready.md → `### Task 4: 公共件与装配面（对应 tasks.md 1.2）` | tasks.md → `- [ ] 1.2 公共件与装配面：a) micro 侧 dtm 分支载荷公共结构（含 traceparent、\`fail_inject\` 注入字段）与 \`traceparent\` 注入/Extract 成对工具（经 \`launchx.Inject\` 同源）；b) 各域 dtm 分支 pb 定义与 \`internal/dtm/proto/\` 生成产物（\`scripts/gen.sh\` 扩展，产物不进 \`pkg/kitex_gen\`）；c) 各域 \`internal/dtm\` 分支面 grpc-go server 装配模板（:9041-9044，不注册 consul）+ 起 rpc.dtm 入口 span（\`biz.dtm.gid\`/\`biz.dtm.branch\` 属性约定）；d) 四域 \`config.yaml\` 新增 \`dtm.branch.listen_addr\`、bff 新增 \`xmiddleware.dtm.grpc_addr\`（缺省 \`127.0.0.1:36790\`）。验收：任一域服起服即多一个分支面端口监听且启动日志可见；\`TestServiceSourceHasNoHardcodedLoopbackAddr\` 仍绿`
> **sync:** tasks.md → `- [ ] 1.2 公共件与装配面：a) micro 侧 dtm 分支载荷公共结构（含 traceparent、\`fail_inject\` 注入字段）与 \`traceparent\` 注入/Extract 成对工具（经 \`launchx.Inject\` 同源）；b) 各域 dtm 分支 pb 定义与 \`internal/dtm/proto/\` 生成产物（\`scripts/gen.sh\` 扩展，产物不进 \`pkg/kitex_gen\`）；c) 各域 \`internal/dtm\` 分支面 grpc-go server 装配模板（:9041-9044，不注册 consul）+ 起 rpc.dtm 入口 span（\`biz.dtm.gid\`/\`biz.dtm.branch\` 属性约定）；d) 四域 \`config.yaml\` 新增 \`dtm.branch.listen_addr\`、bff 新增 \`xmiddleware.dtm.grpc_addr\`（缺省 \`127.0.0.1:36790\`）。验收：任一域服起服即多一个分支面端口监听且启动日志可见；\`TestServiceSourceHasNoHardcodedLoopbackAddr\` 仍绿` | plan-ready.md → `### Task 4: 公共件与装配面（对应 tasks.md 1.2）`

**Files:**
- 创建：`examples/micro/pkg/dtmx/{barrier,payload,result}.go` + `_test.go`
- 创建：`examples/micro/srv-<域>/internal/dtm/protos/busi.proto`（×4 域）
- 创建：各域 `internal/dtm/setup.go` 模板（先以 一个域（srv-stock）落地，其余域在 Task 5-8 复用模板生成，模板本体在本 Task 已完整给出）
- 修改：`examples/micro/scripts/gen.sh`、`examples/micro/srv-*/config.yaml`、`examples/micro/bff/config.yaml`

- [ ] **Step 4.1：载荷公共件 `pkg/dtmx/payload.go`**

```go
// Package dtmx micro 示例的 DTM 公共件：分支载荷、traceparent 续链、官方 barrier 包装、
// 失败语义分流。四域分支面与 bff 编排共用，避免每域各写一份。
package dtmx

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// Payload DTM 分支载荷公共信封。
//
// Biz 是各域的具体业务字段 JSON；Traceparent 是 bff 提交事务那一刻的
// 当前 span 上下文（launchx.Inject 同源注入的结构），正向分支 Extract
// 后全部数据层 span 挂进同一 trace；FailInject 是验收演示注入字段——
// 分支命中该值时按业务失败返回（触发 dtm 补偿/Cancel）。
type Payload struct {
	Biz         json.RawMessage `json:"biz,omitempty"`
	Traceparent string          `json:"traceparent,omitempty"`
	FailInject  string          `json:"fail_inject,omitempty"`
}

// InjectKey 载荷里 traceparent 的字段名（bff 注入与域服 Extract 共用一份约定）
const InjectKey = "traceparent"

// Encode 序列化载荷（bff 编排用）
func Encode(p Payload) (string, error) {
	b, err := json.Marshal(p)
	return string(b), err
}

// Decode 反序列化载荷（域服分支面用）
func Decode(data string) (Payload, error) {
	var p Payload
	if err := json.Unmarshal([]byte(data), &p); err != nil {
		return Payload{}, errors.Join(errors.New("dtmx: bad payload"), err)
	}
	return p, nil
}

// ExtractParent 用载荷携带的 traceparent 重建上游 OTel 上下文。
//
// 与 launchx.Inject 同源（都是 W3C traceparent），故这边用 otel 全局传播器
// 直接 Extract 即可与 webx/kitex 双边 suite 的注入约定互通。
// tp 为空（dtm 定时补偿/重启续推等异步回调）原样返回——这些段落本就按
// 「新根 + biz.dtm.gid 关联」口径处理。
func ExtractParent(ctx context.Context, tp string) context.Context {
	tp = strings.TrimSpace(tp)
	if tp == "" {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier{InjectKey: tp})
}
```

- [ ] **Step 4.2：失败语义件 `pkg/dtmx/result.go`**

```go
package dtmx

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// FailCheck 业务失败哨兵：err.Inner==ErrBizFailure 的错误必须映射为
// 不重试的 dtm 失败语义（ResultFailure）；其余错误原样向上抛。
// （具体常量在 Step 4.3 后按 Step 1.3 核实的 glm 签名校准，见 result 的 goimpl 注释块。）

var ErrBizFailure = _errBizFailure{}

type _errBizFailure struct{}

func (_errBizFailure) Error() string { return "dtmx: business failure (dtm will not retry)" }

// BusinessFailure 把业务失败错误打包（service 层用）
func BusinessFailure(err error) error {
	return errors.Join(ErrBizFailure, err)
}

package dtmx

import (
	"context"
	"github.com/dtm-labs/client/dtmgrpc"
)

// Normalize 把 service 返回的 error 翻译成 dtm 分支返回值约定：
// 业务失败（errors.Is ErrBizFailure）→ dtmgrpc.ResultFailure 语义 / 或 err 语义化；
// 其他错误原样返回（dtm 按网络层失败重试）。
//
// 依 Step 1.3 核实的签名落地：本函数包住「业务错误 → dtm 失败标记」的仅有
// 翻译点，四域分支面共用；若 dtm 版本的真实导出常量名不同（dtmgrpc.ResultFailure
// := dtmgrpc.NewResultFailureErr 等），只改本文件这一处。
func Normalize(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), ErrBizFailure.Error()) {
		if span := trace.SpanFromContext(ctx); span != nil && span.IsRecording() {
			span.SetAttributes(attribute.Bool("biz.failure.dtmbiz", true))
		}
		return dtmgrpc.NewResultFailureErr(err)  // ← 按 Task 1 名单校准的二选：真实 API 可能是 dtmgrpc.ResultFailure(err)/dtmartpc.MayRetry
	}
	return err
}
```

> ⚠️ 本文件的 `/package` 语句块与校准点以 **Step 1.3 grep 出的真实 API** 为唯一裁决——落地时把这四小段合并成一个文件（只保留 `package dtmx` 一次），常量/构造函数名以 grep 结果写实。**落地完形如下（合并版全量代码，写入 `pkg/dtmx/result.go`，此为最终形态）：**

```go
package dtmx

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/dtm-labs/client/dtmgrpc"
)

// ErrBizFailure 业务失败哨兵，service 层所有「业务规则不满足」的 error 必须 errors.Wrap 进它
var ErrBizFailure = errors.New("dtmx: business failure (dtm must not retry)")

// BusinessFailure 包装业务失败（service 层用）
func BusinessFailure(err error) error { return errors.Join(ErrBizFailure, err) }

// IsBusinessFailure 判定是否业务失败
func IsBusinessFailure(err error) bool { return errors.Is(err, ErrBizFailure) }

// Normalize 业务失败 → dtm 立刻失败不重试；其他错误原样（dtm 自动重试）。
// API 名以 Task 1 Step 1.3 的 grep 结果为准（常见形态 dtmgrpc.ResultFailure(err)）。
func Normalize(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if IsBusinessFailure(err) {
		if span := trace.SpanFromContext(ctx); span != nil && span.IsRecording() {
			span.SetAttributes(attribute.Bool("biz.failure", true))
		}
		return dtmgrpc.ResultFailure("", "")  // ← 实名按 Step 1.3 校准（旧名 dtmgrpc.ResultFailure(op, failMsg)）
	}
	return err
}
```

- [ ] **Step 4.3：barrier 包装件 `pkg/dtmx/barrier.go`**

```go
package dtmx

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/dtm-labs/client/dtmgrpc"
)

// Barrier 官方屏障包装。
//
// 来自 dtmgrpc.BarrierFromGrpc(ctx)（分支 grpc server 的进入元数据里有
// X-DTM-Gid/branch_id/op/branch_type 等，官方实现从 ctx 里拎出来）。
// Call 在官方事务封装外再包一层： BeginTx(业务连接池) → barrier.Call(tx,
// cb) → commit/rollback。业务 SQL 全部走官方 barrier 传入的事务，屏障
// 行与业务行同事务落地，保证空补偿/悬挂/重放判定与业务写原子。
//
// 官方 Barrier.Call 签名以 Task 1 Step 1.3 的 grep 结果为准（常见形态：
// func (b *Barrier) Call(tx *sql.Tx, cb func() error) error；老版本是
// disitmcli.Barrier 同签名）。若真实签名不同，只改本文件。
//
// dB 自身应从仓库层的 RawDB() 传入（gorm 底层 *sql.DB，二号不建连）。
type Barrier struct {
	inner _barrierLike // 按真实 dtmgrpc.Barrier 类型对齐
}

type _barrierLike interface {
	Call(tx *sql.Tx, cb func() error) error
}

// BarrierFromCtx 从分支面 grpc 进程 ctx 取官方屏障
func BarrierFromCtx(ctx context.Context) (*Barrier, error) {
	b, err := dtmgrpc.BarrierFromGrpc(ctx)
	if err != nil {
		return nil, err
	}
	return &Barrier{inner: wrapBarrier(b)}, nil
}

func wrapBarrier(v interface{}) _barrierLike {
	if typeIface, ok := v.(interface{ Call(*sql.Tx, func() error) error }); ok {
		return typeIface
	}
	panic(fmt.Sprintf("dtmx: dtmgrpc.Barrier 类型未被官方 Call 识别: %T", v))
}

// Call 包装官方 barrier.Call 的事务生命周期。
// cb 收到的是已受屏障保护的同一事务——业务 SQL 应写在 cb 内。
func (b *Barrier) Call(ctx context.Context, sqlDB *sql.DB, cb func(ctx context.Context, tx *sql.Tx) error) error {
	if b == nil || b.inner == nil {
		return fmt.Errorf("dtmx: barrier unavailable (did the grpc request come from dtm server?)")
	}
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	callErr := func() (callErr error) {
		defer func() {
			if callErr != nil {
				_ = tx.Rollback()
			}
		}()
		err = b.inner.Call(tx, func() error { return cb(ctx, tx) })
		if err != nil {
			return err
		}
		return tx.Commit()
	}()
	if callErr != nil {
		if strings.Contains(callErr.Error(), "not found") { // 悬挂/空补偿类：finalize 已跳过本事务的提交意图——按官方语义位管理员粒度外抛
			return nil
		}
		return callErr
	}
	return nil
}
```

> 落地时 `wrapBarrier` 里 `panic` 改为返回 error（Barrier 构造报错），终版四人不要 panic 路径。终版形态（写入 `pkg/dtmx/barrier.go` 的全量代码）：

```go
package dtmx

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dtm-labs/client/dtmgrpc"
)

// dtmBarrier 官方 barrier 接口约束（以 Task 1 Step 1.3 grep 结果为准）
type dtmBarrier = interface {
	Call(tx *sql.Tx, cb func() error) error
}

// Barrier 官方屏障包装（Call 起事务并与业务写同事务提交/回滚）
type Barrier struct{ inner dtmBarrier }

// BarrierFromCtx 从分支面 grpc ctx 取官方 barrier
func BarrierFromCtx(ctx context.Context) (*Barrier, error) {
	inner, err := dtmgrpc.BarrierFromGrpc(ctx)
	if err != nil {
		return nil, fmt.Errorf("dtmx: barrier from grpc ctx: %w", err)
	}
	b, ok := any(inner).(dtmBarrier)
	if !ok {
		return nil, fmt.Errorf("dtmx: dtmgrpc.Barrier unexpected type %T", inner)
	}
	return &Barrier{inner: b}, nil
}

// Call 业务写全部走官方屏障事务（同事务提交/回滚）；cb 返回 nil 才提交。
func (b *Barrier) Call(ctx context.Context, sqlDB *sql.DB, cb func(ctx context.Context, tx *sql.Tx) error) error {
	if b == nil || b.inner == nil {
		return fmt.Errorf("dtmx: barrier unavailable (call must come from dtm server branch)")
	}
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	runErr := b.inner.Call(tx, func() error { return cb(ctx, tx) })
	if runErr == nil {
		runErr = tx.Commit()
	}
	if runErr != nil && tx.Rollback() != nil {
		runErr = fmt.Errorf("dtmx: rollback also failed after %v", runErr)
	}
	return runErr
}
```

- [ ] **Step 4.4：公共件单测 `pkg/dtmx/dtmx_test.go`（不起 dtm，拦截语义可测）**

```go
package dtmx_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gospacex/launchx/examples/micro/pkg/dtmx"
)

func TestEncodeDecodeRoundtrip(t *testing.T) {
	en, err := dtmx.Encode(dtmx.Payload{Biz: []byte(`{"product_id":1}`), Traceparent: "00-abc-def-01", FailInject: "points_earn"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	p, err := dtmx.Decode(en)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.FailInject != "points_earn" || string(p.Biz) != `{"product_id":1}` {
		t.Fatalf("roundtrip mismatch: %+v", p)
	}
}

func TestDecodeBadJSONIsError(t *testing.T) {
	if _, err := dtmx.Decode("{bad"); err == nil {
		t.Fatal("want error on bad json")
	}
}

func TestBusinessFailureSentinelFlowsThroughNormalize(t *testing.T) {
	norm := dtmx.Normalize(context.Background(), dtmx.BusinessFailure(errors.New("insufficient")))
	// Normalize 之后错误身份可见（终版 API 校准后断言其 dtm 失败标记形态）
	if !dtmx.IsBusinessFailure(norm) {
		t.Fatalf("business failure lost via normalize: %v", norm)
	}
}
```

- [ ] **Step 4.5：各域分支 proto（busi.proto，×4 域）**

每域一份（落地 `examples/micro/srv-<域>/internal/dtm/protos/busi.proto`），`go_package` 按域指到本域生成目录；共用消息与「service 名 + 方法名」两个量按域落（proto 语言级约束：一文件一 service）：

```protobuf
// srv-stock 版（其余域：Stock→Points/Product/Order，service 名 Busi -> StockBusi/PointsBusi/ProductBusi/OrderBusi）
syntax = "proto3";

package dtmbusi;

option go_package = "github.com/gospacex/launchx/examples/micro/srv-stock/internal/dtm/protos;protos";

import "google/protobuf/empty?nope.proto";  // ← 不写这行；全文件只是给 worker 看到全字段

message DtmRequest {
  string biz         = 1;
  string traceparent = 2;
  string fail_inject = 3;
}

message DtmResponse {
  bool success  = 1;
  string msg    = 2;
}

service StockBusi {
  // Saga 下单链动作/补偿 与 TCC 三阶段同名方法共用一份 service
  rpc Deduct (DtmRequest) returns (DtmResponse);
  rpc Release (DtmRequest) returns (DtmResponse);
  rpc Try (DtmRequest) returns (DtmResponse);
  rpc Confirm (DtmRequest) returns (DtmResponse);
  rpc Cancel (DtmRequest) returns (DtmResponse);
}
```

> 落地时**删除** `import "google/protobuf/empty?nope.proto";` 注释行与「不对」两处标注——上面为让读者警觉；proto 终版 = `syntax + package + option + message DtmRequest + message DtmResponse + service <域>Busi（方法按域子集）`。各域方法子集：
> - srv-stock：`Deduct/Release/Try/Confirm/Cancel`（Saga 两段 + TCC 三段）
> - srv-points：`Deduct/Refund/Try/Confirm/Cancel`
> - srv-order：`Create/Cancel/Try/Confirm/Cancel2`（Saga 两段 + TCC 三段；svc端 TCC Cancel 与 Saga Cancel 都叫 Cancel 共用？——**不共享**：按 spec「TCC 三对分支方法 SHALL 与 Saga 四对分支各自独立建模」，TCC 段方法是 `TryPlace/ConfirmSale/CancelReserved`；Saga 是 `Create/CancelOrder`。**各域 proto 方法名定稿如下**：
>   - stock：Saga `Deduct`、`Release`；TCC `TryFreeze`、`ConfirmSale`、`CancelFreeze`
>   - points：Saga `Debit`、`Refund`；TCC `TryFreeze`、`ConfirmUse`、`CancelFreeze`
>   - order：Saga `Create`、`CancelOrder`；TCC `TryPlace`、`ConfirmSale`、`CancelReserved`
>   - product：Saga `BumpSales`、 Compensation `UnbumpSales`（纯两段）
> proto 文件按上表落方法名，Task 5-8 的 grpc 实现与之同构。

- [ ] **Step 4.6：gen.sh 扩展（产物落 `internal/dtm/protos`，显式不进 `pkg/kitex_gen`）**

在 `examples/micro/scripts/gen.sh` 追加段（置于既有 kitex 生成段之后）：

```bash
# ── DTM 分支 proto（grpc-go 生成；产物只落 internal/dtm/protos，禁入 pkg/kitex_gen）──
DTM_PROTO_ROOT="$REPO_ROOT/examples/micro"
declare -A DTM_DOMAINS=( [stock]=StockBusi [order]=OrderBusi [points]=PointsBusi [product]=ProductBusi )
for dom in stock order points product; do
  svc="${DTM_DOMAINS[$dom]}"
  out="${DTM_PROTO_ROOT}/srv-${dom}/internal/dtm"
  pkgpath="github.com/gospacex/launchx/examples/micro/srv-${dom}/internal/dtm/protos"
  protoc -I "${out}" \
    --go_out="${out}" --go_opt=paths=source_relative \
    --go-grpc_out="${out}" --go-grpc_opt=paths=source_relative \
    "${out}/protos/busi.proto" \
    || { echo "dtm proto gen failed for $dom" >&2; exit 1; }
done
# 防向护：kitex_gen 不得出现 dtmbusi
if rg -q "dtmbusi" "$DTM_PROTO_ROOT/pkg/kitex_gen/"; then
  echo "FAIL: dtm proto leaked into pkg/kitex_gen" >&2; exit 1
fi
echo "dtm proto gen: OK (4 domains)"
```

- [ ] **Step 4.7：分支面装配模板（`srv-stock/internal/dtm/setup.go`——其余域复用同一模板粘到各自 `internal/dtm/`）**

```go
// Package dtm srv-stock 的 DTM 分支面。
//
// 形态纪律：
//   - 这个 grpc-go listener 是**传输适配** dagen 独立面（:9043，配置
//     dtm.branch.listen_addr），**不注册进 consul**（DTM server 静态地址回调）；
//   - 分支 handler 只做协议转换，业务一律转调 internal/service——六层纪律
//     （rpc → route → handler → service → repository）在这个面上**不重复
//     实现**，只对齐为「dtm 面入口 span → service」；
//   - 入口 span 属性 biz.dtm.gid / biz.dtm.branch /（补偿时）biz.compensation，
//     与仓内 outbox 补发口径一致。
package dtm

import (
	"context"
	"errors"
	"fmt"
	"net"

	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/dtm/protos"
	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/service"
)

// BranchOptions 分支面装配参数（地址由 main 装配时从 config 注入）
type BranchOptions struct {
	ListenAddr string
	// Service 业务编排（nil 即装配失败）
	Stock *service.StockService
}

// Setup 起分支面并阻塞（main 里 go dtm.Setup(...) 常态后台跑）。
// 返回值：进程退出前必须 rtn 收净的具体列举（listener close + server stop）。
func Setup(ctx context.Context, opts BranchOptions) error {
	if opts.ListenAddr == "" {
		return errors.New("dtm.branch.listen_addr is required in config.yaml")
	}
	if opts.Stock == nil {
		return errors.New("dtm branch: nil service handle (check OnSetup wiring)")
	}

	h, err := protos.NewStockBusiServer(&busiHandler{svc: opts.Stock}, nil)
	if err != nil {
		return fmt.Errorf("dtm branch new server: %w", err)
	}
	if err := protos.RegisterStockBusiServer(...) {  // ← 具体注册调用以生成代码为准
		 ...
	}

	// grpc-go 标准装配：
	lis, err := net.Listen("tcp", opts.ListenAddr)
	if err != nil {
		return fmt.Errorf("dtm branch listen %s: %w", opts.ListenAddr, err)
	}
	gs := grpc.NewServer()
	protos.RegisterStockBusiServer(gs, &branchBoni{svc: opts.Stock})
	go func() {
		<-ctx.Done()
		gs.GracefulStop()
	}()
	if gErr := gs.Serve(lis); gErr != nil && !errors.Is(gErr, context.Canceled) {
		logger.WarnContext(ctx, "dtm branch serve exited", logx.Err(gErr))
		return gErr
	}
	return nil
}
```

> 落地纪律：`protos.NewXxxBusiServer` —— grpc-go 生成代码不提供这种签名；**标准写法**（终版代码，粘贴所有域共通化）：

```go
package dtm

import (
	"context"
	"errors"
	"net"

	"google.golang.org/grpc"

	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/dtm/protos"
	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/service"
)

// Setup 起 dtm 分支面并阻塞直至 listener 关闭。
// ctx 取消（进程退出链）则 GracefulStop。
func Setup(ctx context.Context, opts BranchOptions) error {
	if opts.ListenAddr == "" {
		return errors.New("dtm.branch.listen_addr is required in config.yaml")
	}
	if opts.Stock == nil {
		return errors.New("dtm branch: nil service handle")
	}
	lis, err := net.Listen("tcp", opts.ListenAddr)
	if err != nil {
		return fmt.Errorf("dtm branch listen %s: %w", opts.ListenAddr, err)
	}
	gs := grpc.NewServer()
	protos.RegisterStockBusiServer(gs, &StockBusi{svc: opts.Stock})
	go func() {
		<-ctx.Done()
		gs.GracefulStop()
	}()
	return gs.Serve(lis)
}
```

（`StockBusi` 类型（busi handler）在 Task 5 落域产物。装配点（`cmd/main.go` 的 OnSetup 尾部添行）：

```go
// cmd/main.go OnSetup 尾部（四域同构，端口随域）：
listenAddr := a.Config().GetString("dtm.branch.listen_addr")
go func() {
	if e := dtm.Setup(appCtx, dtm.BranchOptions{ListenAddr: listenAddr, Stock: stockSvc}); e != nil {
		// 分支面挂了不拖垮主进程，但必须告警：DTM 回调不进来，事务卡住
		logger.WarnContext(appCtx, "dtm branch face exited", logx.Err(e),
			logx.String("consequence", "dtm org callbacks unreachable; in-flight transactions stall"),
			logx.String("hint", "check dtm.branch.listen_addr in config.yaml"))
	}
}()
```

其中 `appCtx` 为 OnSetup可获得的生命周期 ctx（若无现成句柄，用 `context.Background()` 持一篇——GracefulStop 消息在 a.OnStop 里调 dtm.Shutdown 一并收；落地时用 launchx的 OnStop 挂 `gs.GracefulStop()`一等目标。）

- [ ] **Step 4.8：config 新段**

`examples/micro/srv-stock/config.yaml` 追加（四域同构，端口 9041(product) / 9042(order) / 9043(stock) / 9044(points)）：

```yaml

# ===== DTM 分支面（add-micro-dtm-saga-tcc）=====
# 轻量 gRPC 面，专承接 DTM server 的分支回调（Saga Action/Compensate、
# TCC Try/Confirm/Cancel）。不注册进 consul：DTM server 按本文件的
# listen_addr 以静态 {@addr} 回调，注册中心不可用与否与此面无关。
dtm:
  branch:
    listen_addr: "127.0.0.1:9043"
```

`examples/micro/bff/config.yaml` 追加：

```yaml

# ===== DTM 事务编排（add-micro-dtm-saga-tcc）=====
# Saga（替换现有手动编排下单链）与 TCC（预售锁定）均经 dtmgrpc 提交到
# 官方镜像 DTM server 的 gRPC 端口。strict.dtm: false——dtm 不可达时
# 编排器返回 5xx + Warn（consequence/hint 进日志），进程不退。
xmiddleware:
  dtm:
    grpc_addr: "127.0.0.1:36790"
```

- [ ] **Step 4.9：build/vet + 守卫回归**

运行：
```bash
bash examples/scripts/ci.sh micro
go test . -run 'TestServiceSourceHasNoHardcodedLoopbackAddr' -count=1
```
预期：PASS；无硬编码回环地址告警；`go vet ./...` 干净。

- [ ] **Step 4.10：Commit**

```bash
git add examples/micro pkg_goes_here../..
git add go.mod go.sum examples/micro pkg
git commit -m "feat(micro): dtm branch facade common pkg + proto gen + branch setup template + config"
```

- [ ] **Task complete**

---
