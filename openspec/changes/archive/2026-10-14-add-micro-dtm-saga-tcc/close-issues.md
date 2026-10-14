# close-issues — add-micro-dtm-saga-tcc

> 归档前的一致性核对记录（2026-10-13）。无 CRITICAL；有 2 条 WARNING 与 3 条 SUGGESTION。
> 判定口径：**必须有实际命令输出作证据**，「应该通过」不算。

## 阶段 1：实现验证

| 项 | 命令 | 结果 |
|---|---|---|
| 测试套件 | `CGO_ENABLED=0 go test ./examples/micro/... . -count=1` | 26 包 ok / 3 包 FAIL（见 WARNING-1） |
| 构建 | `CGO_ENABLED=0 go build ./...` | exit=0，零输出 |
| 组件仓构建 | `cd ../dtmx && go build ./...` | exit=0 |
| 组件仓测试 | `cd ../dtmx && go test ./...` | 10/10 PASS |
| gofmt | `gofmt -l examples/ internal/ *.go` | 空 |
| go vet | `CGO_ENABLED=0 go vet ./examples/micro/...` | exit=0 |
| OpenSpec | `openspec validate add-micro-dtm-saga-tcc --strict` | valid |

### 需求清单（19 条）逐条证据

全部通过文件存在性核对，关键几条：

- `0.1` `go.mod` 含 `dtm-labs/client v1.18.7`
- `0.2` dtm server 进程活；`dtm_db` 含 `kv` / `trans_branch_op` / `trans_global`；业务四库 `trans_*` 表数 = 0
- `1.1` `schema-micro.sql` 含 4 份 `CREATE TABLE IF NOT EXISTS barrier`（按域隔离）
- `1.2` dtmx 三件套 3 个文件 / 四域 `busi.proto` 4 个 / 分支面 handler+setup 8 个 / config `listen_addr` 4 处
- `2.1-2.4` 四域 `service/dtm_*.go` 与 `repository/dtmstore.go` 齐备
- `3.1` `bff/internal/dtm/saga.go` 存在；`func (h *MallHandler) compensate` 命中 **0** 处（手动补偿已退役）
- `3.2` `bff/internal/dtm/tcc.go` + `handler/presale.go` 存在
- `4.1-4.3` `dtm_surface_test.go` / `dtm_e2e_test.go` / `dtm_e2e.sh`（可执行位已置）
- `5.2-5.3` `examples/micro/pkg/dtmx` 已删；全仓 `examples/micro/pkg/dtmx` 引用 **0** 处

## WARNING（可归档，用户已知悉）

### WARNING-1：3 个包的 sqlite 用例在本机无法运行（**非本变更引入**）

`srv/internal/repository`、`srv/internal/service`、`srv-order/internal/repository`
的失败原因是：

```
order_test.go:121: open sqlite: Binary was compiled with 'CGO_ENABLED=0',
                            go-sqlite3 requires cgo to work. This is a stub
```

**已用基线对照证实与本变更无关**：`git worktree add /tmp/lxbase 319092b`（本变更的
第一个 commit 的父提交）跑同一批包，失败输出与 HEAD **逐字相同**（同文件、同行号）。
即这是工具链问题，非断言失败。

根因：本机任何 cgo 编译都会触发 `CLAUDEGATE` 交互式 profile 选择器并阻塞，
故全流程必须 `CGO_ENABLED=0`；而 `go-sqlite3` 在该模式下只剩 stub。

dtm 四个域的测试不受影响——它们用的是纯 Go 假驱动（`srv-stock/internal/dbmock`、
`srv-points|product/internal/dtmstub`），不依赖 cgo。

### WARNING-2：本地 demo 库 `mall_rw` 账号认证态不稳（环境问题）

复跑真机三段时受阻：`Access denied for user 'mall_rw'@'192.168.32.1'`。
已排除的范围：`root` / `testuser` 用同一 Go 驱动同一路径均通；`authentication_string`
hash 稳定不变；故不是口令错、也不是驱动问题，而是该账号的**认证态**在
`ALTER USER` 后进入不一致中间态（hash 对但认证态不对）。
恢复方式（已验证有效）：`DROP USER` + `CREATE USER` + `GRANT` + `FLUSH PRIVILEGES`。

对交付物的影响：无（纯环境）。但**本次 close 未由我本人重跑真机三段**，
证据来自三个执行代理的记录 + 我对其中关键事实的抽查（见下）。

## SUGGESTION（记录，不阻塞）

### SUGGESTION-1：Task 4.2 的「dtm kill → 重启续推进」未纳入真机用例

判据要求真 kill dtm 进程，属侵入式且易毁现场，实现代理判定不纳入自动化，
改由 Task 2 的 Step 2.3 覆盖基础设施可达性（kill/start 后 `panic=0`、双端口在听）。
**事务续推本身（kill 发生在事务进行中）未被自动化判据覆盖**。

### SUGGESTION-2：~~分支落库不产 `db.*` span~~ **已修复（2026-10-13）**

**原症状**：四域 `rpc.dtm.*` → `service.*` → `repository.*` 齐全，但 repository 之下
的原始 SQL 走 dtm barrier 给的裸 `*sql.Tx`，绕开 dbx 的 gorm callback，故无 `db.*` span；
连带 `trace_e2e_test.go` 的 `TestT6ThreeHopsShareOneTrace` 第 ④ 步变红（域服最浅 span
已变成 `rpc.dtm.product.AddSales`，而 trace 里现存的 `db.*` 来自 redis/kvx 旁路、
不在其子树内），与 `dtm-acceptance` 的「现有其余验收 MUST NOT 回退」冲突。

**根因**：`dtmcli.BranchBarrier.CallWithDB` 要的是 `*sql.Tx`，而 gorm 不支持外接事务
——这是 barrier 机制本身的要求，不是实现取巧。

**修法（选「补最后一环」而非「放宽断言」）**：新增 `dtmx.ExecTx` / `dtmx.QueryRowTx`，
span 名 `db.<op>`、属性 `db.operation` / `db.system` / `db.sql.table` / `db.query.text`，
与 `dbx/orm/gorm_tracing.go` **逐字同形**，因此 T6 第 ④ 步与 `dtm-branch-facade`
的判据都无需为它开特例。四域 dtmstore 的 20 处裸 SQL 改走它。

**为什么不给 T6 开特例**：放宽断言（改成「trace 内任意位置有 db.* 就算」）会让这条
用例失去意义——它本来就在防「三棵树各查得到」的假通过。数据层入链是**本次变更的
原始要求**（「分布式事务的操作要入链，Jaeger 可查」），补 span 才是对齐要求，
改断言是对齐判据，方向反了。

**顺带的结构调整**：`srv-points` 的 `DtmBranchStore` 补 `traceOn` 字段（原先裸 SQL
完全没有埋点通道）；`srv-order` 的 `insertItems`/`insertStatusLog`/`insertOutbox`/
`selectOrderID` 与 `srv-product` 的 `classifyReduceMiss` 由包级函数挂到各自 repository
——它们原本拿不到 `traceOn`。

**验证**：`dtmx` 13 → 15 用例全绿（含一条反向探针：把 update 的修饰词组从
`(?:low_priority|ignore|only)` 放宽回 `(?:[a-z_]+)` 会让 `db.sql.table` 解析成 `set`
而报红——这条探针抓到过一个真 bug）；launchx 全量测试 FAIL 集合与改动前**完全一致**
（仍是那 3 个 cgo stub 包）。

### SUGGESTION-3：`examples/mall` 侧仍有同名 span 名

`examples/mall/srv-bff/internal/handler/mall.go:458` 仍有
`handler.bff.ReleaseStock.compensate`。属 mall 示例，本次变更范围明确不含
（proposal「不包含」已声明），未动。

## 我抽查核对过的真机关键事实

底座与 dtm 在跑期间，我独立查证（不是转述代理汇报）：

- Jaeger 按 `biz.dtm.gid` 反查：TCC `kNuEHDT788ThiHG8dQEaKe` 与 Saga
  `ZZHHh8BEDQwmJHc2Vix7ki` **各自只出现在 1 条 trace 上**，且正向与补偿分支 span
  同处该 trace —— 由此推翻了原 spec「补偿是新根」的写法（design.md D4 已按实测修正）
- MySQL：Saga 补偿后四库回基线；TCC 的 `order_status_log` 出现
  `presale_locked → paid`，outbox 行在 Confirm 后才 +1

## 阶段 3：规格一致性

- `openspec validate add-micro-dtm-saga-tcc --strict` → valid
- Completeness：`tasks.md` 19/19 `[x]`；plan `Task complete` 17/17；plan Step 92/92
- Correctness：spec 的 5 个 capability delta 与实现逐条对上；两处规格在实现期被
  实测推翻并已修正（补偿同链 D4、gRPC status D7a），修正过程与证据留痕在 design.md
- Coherence：`design.md` D1-D8 + D7a/D7b/D7c 九条决策全部与实现对应