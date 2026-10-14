# add-micro-dtm-saga-tcc — tasks.md

> 上游：`proposal.md`（动机/范围）、`design.md`（HOW 决策）、`specs/`（5 能力 delta）。
> 顶层 checkbox 条目与 `plan-ready.md`、`docs/superpowers/plans/2026-10-13-add-micro-dtm-saga-tcc.md`
> 的 Task N 一一对应（N 从 1 递增）。

## 0. Prerequisites

- [x] 0.1 依赖接入与钉版：root `go.mod` 引入 `github.com/dtm-labs/client`（实现时以最新稳定版钉版号并登记），确认 workspace replace 体系与之无冲突（dtm client 不在 replace 列表，走 module cache）。验收：`go build ./...` 通过且 `go.mod`/`go.sum` 含该依赖 <!-- 已实现: 钉版 v1.18.7；go get 未跑 tidy（Task 4 前无人 import 会被剪掉）；Step 1.3 核实真实 API 并登记进 Task 4 执行记录 -->
- [x] 0.2 底座 compose 追加 dtm 服务：官方镜像 + 官方环境变量形态（`STORE_DRIVER=mysql`、`STORE_HOST/PORT/USER/PASSWORD/DB=dtm_db`、:36789/:36790 双端口，`PORT/HTTP_PORT/GRPC_PORT` 就位）；`dtm_db` 建库先于 dtm 启动；健康检查指向 gRPC 端口。验收：compose up 后 `dtm_db` 出现 `trans_global` / `trans_branch_op` 自建表，业务四库零 dtm 表 <!-- 已实现: 官方镜像在本网络不可达，改 go install 源码构建 dtm v1.19.0 宿主直跑；dtm 不自建表，须先灌官方 dtmsvr.storage.mysql.sql（库名 dtm→dtm_db）；账号口令按底座实况（rootpass / mall_rw+\$HUBX_DB_PW）从环境取；0 panic、:36789/:36790 在听 -->

## 1. 基础设施件

- [x] 1.1 schema 增量 DDL（登记进 `examples/mall/scripts/schema.sql` 系的四库 DDL 口径）：四域各建 dtm barrier 表；`product_db` 新增销量表（Saga 销量分支）；`stock_db` / `points_db` 增补冻结语义所需列（冻结量列或冻结辅助表，与现有可在读列并存）；`order_db` 订单状态机增列 `presale_locked` 语义。验收：DDL 幂等重放不报错、四库 schema 版本一致，`dtm_db` 表不在此口径内 <!-- 已实现: 落 examples/micro/scripts/schema-micro.sql（该文件自述政策：micro 增量不改 mall 共用 DDL）；barrier DDL 逐字取官方 sqls/dtmcli.barrier.mysql.sql（原草案缺 trans_type/gid 且唯一键错，会跨事务丢账）；按域隔离四库各一份；幂等两遍 exit=0 -->
- [x] 1.2 公共件与装配面：a) dtm 公共三件套落 `github.com/gospacex/dtmx` 组件仓（物理 `../dtmx`）：分支载荷公共结构（含 traceparent、`fail_inject` 注入字段）、`traceparent` 注入/Extract 成对工具、失败语义分流、官方 barrier 包装（经 `launchx.Inject` 同源）；b) 各域 dtm 分支 pb 定义与 `internal/dtm/proto/` 生成产物（`scripts/gen.sh` 扩展，产物不进 `pkg/kitex_gen`）；c) 各域 `internal/dtm` 分支面 grpc-go server 装配模板（:9041-9044，不注册 consul）+ 起 rpc.dtm 入口 span（`biz.dtm.gid`/`biz.dtm.branch` 属性约定）；d) 四域 `config.yaml` 新增 `dtm.branch.listen_addr`、bff 新增 `xmiddleware.dtm.grpc_addr`（缺省 `127.0.0.1:36790`）。验收：任一域服起服即多一个分支面端口监听且启动日志可见；`TestServiceSourceHasNoHardcodedLoopbackAddr` 仍绿

## 2. 四域分支（Saga + TCC 语义齐备）

- [x] 2.1 <!-- 已实现: Deduct/Release + TryFreeze/ConfirmFreeze/CancelFreeze，24 用例/99 子用例，5 探针真红 -->
- [x] 2.1 `srv-stock` 分支集：service 层 `Deduct` / `Release`（Saga，多表事务：库存表 + 流水表，barrier 包裹）与 TCC 三兄弟 `TryFreeze` / `Confirm` / `Cancel`（可售↓冻结↑ → 冻结实扣 / 解冻）；业务失败（库存不足、冻结不足）与基础失败显式分流；rpc.dtm 入口 handler 从载荷提取 traceparent。验收：单测覆盖两类返回与 barrier 重放；注入 `ResultFailure` 误改普通 error 的反向探针使测试 FAIL
- [x] 2.2 <!-- 已实现: Debit/Refund + 三兄弟，72 用例，5 探针真红 -->
- [x] 2.2 `srv-points` 分支集：service 层 `Debit` / `Refund`（Saga，两表事务 + barrier）与 TCC `TryFreeze` / `Confirm` / `Cancel`（冻结量入账/消账/回退）；业务失败（余额不足）分流同上。验收：同 2.1 口径单测
- [x] 2.3 <!-- 已实现: CreateOrder/CancelOrder + TryPlace/ConfirmFreeze/CancelFreeze，38 用例/93 子用例，5 探针真红 -->
- [x] 2.3 `srv-order` 分支集：service 层 `CreateOrder` / 作废补偿（orders+items+outbox 三表事务 / 终态标记+outbox 必不扣回）与 TCC `TryPlace`（占位单 `presale_locked`，不写 outbox）/ `Confirm`（转 `paid`，**此刻才写 outbox**）/ `Cancel`（废占位）；幂等按 gid/barrier。验收：单测断言「outbox 行仅在 Confirm 后出现」与「作废补偿不重复落 outbox」
- [x] 2.4 <!-- 已实现: AddSales/ReduceSales，39 用例 + 真 gRPC 端到端，9 探针真红 -->
- [x] 2.4 `srv-product` 分支集：service 层销量 +1 / -1（新销量表，Saga 第三分支，使四服全参与 Saga）；幂等/barrier 同口径。验收：单测断言补偿回退后销量回到基线
- [x] 2.5 四域分支失败语义统测：对四域 service 分支方法跑「业务失败 → ResultFailure 不重试 / 基础失败 → dtm 重试」的分支返回建模单测矩阵（含每域至少一次反向探针）。验收：四域矩阵全绿且反向探针记录在案 <!-- 跨域真跑测试写不出来：internal 规则禁止 examples/micro import srv-*/internal，故改为 AST 结构守卫（四域失败出口须 IsBusinessFailure→Aborted、显式 Internal）；顺带统一 stock 基础失败由 Unknown 改为 Internal；2 条探针已实测报红 -->

## 3. bff 编排

- [x] 3.1 Saga 编排接入：bff 新增 `internal/dtm`（saga 编排器：分支序 `stock.Deduct → order.CreateOrder → product.销+1 → points.Debit`、补偿逆序、`DtmAddr` 读 config、traceparent 编入载荷、`fail_inject` 透传）；`POST /api/order` handler 改挂 saga 提交（预检 `product.GetProduct` 失败 4xx 不提交）；现有手动 `ReleaseStock` 补偿代码退役删除 <!-- 已实现: 真机全链跑通（trans_global succeed、四库对账、outbox traceparent 在 Jaeger 单树含五服务）；fail_inject 逆序补偿四库回基线；dtm 不可达 5xx+Warn 进程不退 -->验收：gin-less 单测断言分支序与载荷含 traceparent；全仓 grep 手动补偿调用点为零
- [x] 3.2 TCC 编排接入：bff 同包新增 tcc 编排器（Try 三分支：stock/points/order；Confirm/Cancel 二阶段；配置与 trace 口径与 saga 同构）与新增 `POST /api/presale/lock` 路由。验收：单测断言 Try 失败 → Cancel 逆序构造、Try 全绿 → Confirm 构造；路由注册可见 <!-- 已实现: 真机 Try 全绿→Confirm（outbox 停在 63、Confirm 后才 +1，占位单 presale_locked→paid 见于 order_status_log）与 fail_inject→Cancel（三域解冻、零脏数据、confirm/cancel 严格镜像）双路径验过 -->

## 4. 测试、验收与文档

- [ ] 4.1 单元测试与静态守卫：a)「分支 handler 禁直连 dbx / 必经 service」静态守卫（反向断言：注入直连探针必红）；b) barrier 空补偿/悬挂/重放三条幂等单测（以假 barrier 存储为主口径）；c) bff 两编排器的分支序/补偿序/载荷单测。验收：`go test ./examples/... -count=1` 全绿
- [ ] 4.2 真机集成测试：Go test（dtm 或底座缺任一即 Skip，不可写成 PASS）：Saga 成功四库断言、fail 注入补偿基线还原断言、TCC Confirm/Cancel 路径断言、dtm kill → 重启续推进、Jaeger trace 集成断言（正向单 trace 含四域 rpc.dtm/service/repository/db span；补偿段与正向**同一条 traceID**、带 `biz.compensation=true`，真机已实测 dtm 按 step 共享载荷）——扩展现有 `examples/micro/trace_e2e_test.go` 口径。验收：起底座 + dtm 时全绿，无底座时 Skip 且日志写明跳过原因
- [ ] 4.3 脚本级验收 `examples/micro/scripts/dtm_e2e.sh`：三段——①全绿 Saga（单 traceID、四库正确）；②fail 注入 Saga（自动逆序补偿、四库回基线、补偿 span 带 `biz.dtm.gid`/`biz.compensation=true`）；③TCC Try 全绿→Confirm 与 Try 失败→Cancel 路径。`Want/Observed` 分离、退出码即结论（无 `|| true`）、每段至少一次反向探针。验收：起全链跑三段全绿；人为破坏某判据（如注入失败分支）脚本报红且指明失败段
- [ ] 4.4 文档同步：`examples/micro/README.md`（拓扑/端口 9041-9044、dtm 部署、起停、`dtm_e2e.sh`）、`examples/ACCEPTANCE.md` 新增 DTM 验收节、`examples/micro/docs/verify-chain.md` 增补「DTM Saga/TCC 两链 trace 实测」两节（含正向单 trace span 清单与补偿段同链查法）、E3 下单链口径改为 Saga 验收（其余验收不回退）。验收：对照 README 与 config 端口逐一核对成立；下单链章节不再出现手动补偿口径

## 5. dtmx 组件仓（2026-10-13 amend 追加，见 design.md D8）

- [x] 5.1 组件仓骨架：新建 `../dtmx`（`module github.com/gospacex/dtmx`）+ `git init`；体例对齐 `dbx`/`otelx`（`interface.go` / `interface_test.go` / `README.md` / `.gitignore` / `go.mod`+`go.sum`）；`go.mod` 只依赖 `github.com/dtm-labs/client` 与 otel，**不 import launchx/configx/dbx**（纯库）。验收：`cd ../dtmx && go build ./... && go test ./...` 全绿 <!-- 已实现: ../dtmx 已 git init；go.mod module github.com/gospacex/dtmx；interface_test.go 依赖面守卫已跑红（空仓实现文件 0 < 3）-->
- [x] 5.2 迁移三件套：把 `examples/micro/pkg/dtmx/{result,barrier,payload}.go` 及其 `_test.go` 移入 `../dtmx`，包名保持 `dtmx`；`ExtractParent` 原依赖 `launchx.Extract` 需改为 `otelx/propagation`（与 launchx 的 Inject/Extract 同源实现，语义不变）。验收：`../dtmx` 内测试全绿且与迁移前用例数一致 <!-- 10/10 PASS；go mod tidy 需显式钉 genproto 分裂模块三行（照抄 launchx go.mod:172-174）-->
- [x] 5.3 接线：launchx `go.mod` 增 `replace github.com/gospacex/dtmx => ../dtmx` + `require`；全仓 15 个导入方（srv-stock 5 个 / srv-points 6 个文件）改为 `github.com/gospacex/dtmx`，删 `examples/micro/pkg/dtmx`。验收：`go build ./...` 通过、仓内无 `examples/micro/pkg/dtmx` 残留引用 <!-- 15 文件改路径，launchx commit 0e1eef6 -->
- [x] 5.4 文档：`../dtmx/README.md` 写清三件套各自解决什么、与 dtm 官方 API 的对应关系、按域隔离 barrier 表的接法、以及「不自造失败哨兵」的硬约束
