# 实现计划：add-micro-dtm-saga-tcc

## 来源

- 提案：`openspec/changes/add-micro-dtm-saga-tcc/proposal.md`
- 设计：`openspec/changes/add-micro-dtm-saga-tcc/design.md`
- 规格：`openspec/changes/add-micro-dtm-saga-tcc/specs/`（5 能力 delta）
- 任务：`openspec/changes/add-micro-dtm-saga-tcc/tasks.md`

## 实现步骤

### Task 1: 依赖接入与钉版（对应 tasks.md 0.1）
- [x] **任务完成**（与 superpowers plan `Task 1`、tasks.md `- [ ] 0.1` 同步勾选）
- 目标：root `go.mod` 引入 `github.com/dtm-labs/client`（登记录钉版本），不进 replace 列表
- 改动文件：`go.mod`、`go.sum`
- 验证方式：`cd /Users/hyx/work/gowork/src/hubx/launchx && go build ./...` 通过；`go list -m github.com/dtm-labs/client` 打出钉版号

### Task 2: 底座 compose 追加 dtm 服务（对应 0.2）
- [x] **任务完成**
- 目标：官方镜像形态 compose service（:36789/:36790、`STORE_DRIVER=mysql` + `STORE_*` 指向 `dtm_db`、healthcheck），`dtm_db` 先建库
- 改动文件：外部底座 compose（`~/work/gowork/src/hubx/dockers/docker-compose.yml` 或新增覆盖文件）、必要时建库脚本
- 验证方式：compose up 后 `mysql -h127.0.0.1 -utest -p${HUBX_DB_PW} dtm_db` 可连且 `trans_global` 存在、业务四库 dtm 表数为零

### Task 3: schema 增量 DDL（对应 1.1）
- [x] **任务完成**
- 目标：四域 barrier 表 ×4、`product_db` 销量表、stock/points 冻结语义列（或辅助表）、order 状态 `presale_locked` 语义列
- 改动文件：`examples/mall/scripts/schema.sql`（四库 DDL 口径，与 examples/micro 共用）
- 验证方式：DDL 幂等重放（连跑两遍不报错）；各库 DESCRIBE 对新列/新表逐一存在；`dtm_db` 表不混入业务库

### Task 4: 公共件与装配面（对应 1.2）
- [ ] **任务完成**
- 目标：dtm 分支载荷公共结构（含 trace carrier 与 `fail_inject`）、traceparent 注入/Extract 成对工具；各域 dtm 分支 pb + `internal/dtm/proto/` 生成（gen.sh 扩展）；各域 `internal/dtm` grpc-go 分支面装配（:9041-9044、不注册 consul、开 `rpc.dtm.<entity>.<Method>` span 带 `biz.dtm.gid`/`biz.dtm.branch`）；四域 config 新 `dtm.branch.listen_addr`、bff config 新 `xmiddleware.dtm.grpc_addr`
- 改动文件：`examples/micro/pkg/`（新增载荷/trace 工具）、`examples/micro/srv-*/internal/dtm/`（pb 与装配模板）、`examples/micro/srv-*/config.yaml`、`examples/micro/bff/config.yaml`、`examples/micro/scripts/gen.sh`
- 验证方式：任一域服起服多一个分支面端口监听且启动日志可见；`gofmt`/`go vet` 干净；`TestServiceSourceHasNoHardcodedLoopbackAddr` 仍绿

### Task 5: srv-stock 分支集（对应 2.1）
- [ ] **任务完成**
- 目标：service 层 Saga `Deduct`/`Release`（多表事务 + barrier）与 TCC `TryFreeze`/`Confirm`/`Cancel`；业务失败（库存不足/冻结不足）→ `ResultFailure` 语义，基础失败 → 普通 error；分支面 handler 载荷转 service
- 改动文件：`examples/micro/srv-stock/internal/{service,handler,dtm}/`、`internal/repository/`
- 验证方式：该域单测（两类失败建模、barrier 重放）全绿；反向探针（`ResultFailure` 改普通 error）FAIL 记录在案

### Task 6: srv-points 分支集（对应 2.2）
- [ ] **任务完成**
- 目标：service 层 Saga `Debit`/`Refund`（两表事务 + barrier）与 TCC `TryFreeze`/`Confirm`/`Cancel`（冻结量）；余额不足业务失败分流
- 改动文件：`examples/micro/srv-points/internal/{service,handler,dtm,repository}/`
- 验证方式：同 Task 5 口径单测全绿

### Task 7: srv-order 分支集（对应 2.3）
- [ ] **任务完成**
- 目标：service 层 Saga `CreateOrder`/作废补偿（orders+items+outbox 三表事务；作废标记 + outbox 必不扣回）与 TCC `TryPlace`（占位单 `presale_locked`、不写 outbox）/`Confirm`（转 `paid`，此刻才写 outbox）/`Cancel`（废占位）；gid/barrier 幂等
- 改动文件：`examples/micro/srv-order/internal/{service,handler,dtm,repository}/`
- 验证方式：单测断言「outbox 行仅在 Confirm 后出现」「作废不重复落 outbox」全绿

### Task 8: srv-product 分支集（对应 2.4）
- [ ] **任务完成**
- 目标：service 层销量 +1/-1（新销量表，Saga 第三分支，使四服全参与）；barrier 幂等
- 改动文件：`examples/micro/srv-product/internal/{service,handler,dtm,repository}/`
- 验证方式：单测断言补偿回退后销量回基线

### Task 9: 四域失败语义统测（对应 2.5）
- [ ] **任务完成**
- 目标：四域「业务失败 → 不重试 / 基础失败 → dtm 重试」返回建模单测矩阵，每域至少一次注入反向探针
- 改动文件：四域 `internal/service/*_test.go`（或集中 `internal/dtm/resultsemantics_test.go`）
- 验证方式：`go test ./examples/micro/... -count=1` 全绿，反向探针结果记录在案

### Task 10: bff Saga 编排接入（对应 3.1）
- [ ] **任务完成**
- 目标：bff `internal/dtm` saga 编排器（四分支序 + 补偿逆序 + `DtmAddr` 读 config + traceparent/`fail_inject` 入载荷）；`POST /api/order` 改挂 saga 提交（预检失败 4xx）；手动 `ReleaseStock` 补偿代码退役删除
- 改动文件：`examples/micro/bff/internal/{dtm,handler,route}/`、相关测试
- 验证方式：编排器单测（分支序/载荷 traceparent）全绿；全仓 grep 手动补偿调用点为零

### Task 11: bff TCC 编排接入（对应 3.2）
- [ ] **任务完成**
- 目标：同包 tcc 编排器（Try 三分支 stock/points/order、Confirm/Cancel 二阶段、与 saga 同构配置/trace 口径）+ `POST /api/presale/lock` 路由
- 改动文件：`examples/micro/bff/internal/{dtm,handler,route}/`
- 验证方式：单测断言 Try 失败→Cancel 逆序、Try 全绿→Confirm；路由注册可见

### Task 12: 单测与静态守卫（对应 4.1）
- [ ] **任务完成**
- 目标：「分支 handler 禁直连 dbx / 必经 service」静态守卫（注入直连探针必红）；barrier 空补偿/悬挂/重放三条幂等单测（假 barrier 存储）；bff 两编排单测已随 Task 10/11 归位，此 Task 下补齐矩阵汇总
- 改动文件：`examples/micro/pkg/handlecheck/` 口径扩展或新增 `examples/micro/scripts/` 判据、各域测试文件
- 验证方式：`go test ./examples/micro/... -count=1` 全绿；守卫反向探针（分支 handler 直连 dbx）FAIL 由其报出

### Task 13: 真机集成测试（对应 4.2）
- [ ] **任务完成**
- 目标：Go test（无 dtm/底座即 Skip 且日志写明原因）：Saga 四库断言、fail 注入重建基线、TCC Confirm/Cancel、dtm kill 重启续推进、trace 集成断言（正向单 trace 四域 span 齐全；补偿新根带 `biz.dtm.gid`）
- 改动文件：`examples/micro/trace_e2e_test.go`（扩展）或新增 `examples/micro/dtm_e2e_test.go`
- 验证方式：起底座 + dtm + 六进程时全绿；无底座时 Skip 输出可见（不写成 PASS）

### Task 14: 脚本级验收 dtm_e2e.sh（对应 4.3）
- [ ] **任务完成**
- 目标：三段脚本——①全绿 Saga（单 traceID 四库正确）②fail 注入（逆序补偿、四库回基线、补偿 span 带 gid/compensation）③TCC Confirm/Cancel 两路径；`Want/Observed` 分离、退出码即结论、每段至少一次反向探针
- 改动文件：`examples/micro/scripts/dtm_e2e.sh`（新增）
- 验证方式：起全链跑全绿；人为注入失败判据时报红并指明失败段

### Task 15: 文档同步（对应 4.4）
- [ ] **任务完成**
- 目标：README（拓扑/端口/起停/dtm_e2e）、`examples/ACCEPTANCE.md` DTM 节、`docs/trace-chain-audit.md` §3 增补两链 trace 实测小节、E3 口径改 Saga（其余验收不回退）
- 改动文件：`examples/micro/README.md`、`examples/ACCEPTANCE.md`、`docs/trace-chain-audit.md`
- 验证方式：README/config 端口逐一核对成立；下单链章节零「手动补偿」措辞

## Amendments

### 2026-10-13：dtmx 独立为 hubx 平级组件仓
- 原因：dtm 公共三件套与业务无关，留在 `examples/micro/pkg/` 会让下一个接 dtm 的形态复制一份，复制件漂移即静默丢账。
- 影响规格：`specs/dtm-branch-facade`（分支面不得直连 dbx）、`specs/dtm-tcc-presale`（barrier 幂等）—— 均**未变**，仅归属改变。
- 影响任务：新增 5.1-5.4；原 Task 4 的公共件部分由 5.1/5.2 承接。
- 详细计划：`docs/superpowers/plans/2026-10-13-add-micro-dtm-saga-tcc-part6.md`

## 追加实现步骤

### Task 16: dtmx 组件仓骨架（对应 tasks.md 5.1）
- [ ] **任务完成**
- 目标：新建 `../dtmx` 独立 git 仓，`module github.com/gospacex/dtmx`，体例对齐 dbx/otelx
- 改动文件：`../dtmx/{go.mod,go.sum,interface.go,interface_test.go,README.md,.gitignore}`
- 验证方式：`cd ../dtmx && go build ./... && go test ./...` 全绿；`go list -m` 打出本仓 module 名

### Task 17: 三件套迁移与接线（对应 tasks.md 5.2 / 5.3 / 5.4）
- [ ] **任务完成**
- 目标：三件套移入组件仓（`ExtractParent` 改走 `otelx/propagation`）、launchx 加 replace、15 个导入方改路径、旧包删除、README 补齐
- 改动文件：`../dtmx/*.go`、`launchx/go.mod`、`examples/micro/pkg/dtmx/`（删）、srv-stock 与 srv-points 下的导入方
- 验证方式：`go build ./...` 通过；`grep -r "examples/micro/pkg/dtmx" --include="*.go" .` 为空；`go test ./examples/micro/... -count=1` 全绿
