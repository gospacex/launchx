# dtm-acceptance Specification

## Purpose
TBD - created by archiving change add-micro-dtm-saga-tcc. Update Purpose after archive.
## Requirements
### Requirement: DTM 场景 SHALL 三层测试全落

测试 SHALL 覆盖三层，全部遵循仓内判据纪律（Want/Observed 分离、退出码即结论、
至少一次反向探针、SKIP 不得写成 PASS）：

1. **单元测试**（不起 dtm/底座，CI 即跑）：service 分支方法的业务失败 vs
   基础失败返回建模、barrier 幂等重放逻辑、分支 handler pb→service 转换、
   bff 编排分支序与补偿逆序、traceparent 注入/Extract 成对；
2. **真机集成测试**（Go test，dtm 或底座缺任一即 Skip）：Saga 成功/补偿的
   四库断言、TCC Confirm/Cancel 路径断言、dtm kill 重启续推进、
   trace 集成断言（扩展现有 `examples/micro/trace_e2e_test.go` 口径）；
3. **脚本级验收**：新增 `examples/micro/scripts/dtm_e2e.sh` 三段——
   全绿 Saga（单 traceID 四库正确）→ fail 注入补偿（基线还原）→
   TCC Try/Confirm 与 Cancel 路径。

#### Scenario: 单测反向探针真判

- **WHEN** 故意把某分支的 `ResultFailure` 语义改成普通 error 再跑该域单测
- **THEN** 相关测试 FAIL（判据能区分两类错误），不是因为异常没抛而随机通过

#### Scenario: 脚本级验收三段全绿且退出码即结论

- **WHEN** 起全链（底座含 dtm + 六进程）跑 `bash scripts/dtm_e2e.sh`
- **THEN** 三段全部 PASS、末尾汇总一致、脚本以非零码反映任一失败；
  fail 注入段断言四库回到基线且补偿 span 带 `biz.dtm.gid`/`biz.compensation=true`

### Requirement: 验收文档口径 SHALL 与 Saga 替换同步改写

`examples/ACCEPTANCE.md` SHALL 新增 DTM 验收节（两条链的目录树/调用链/观测细节），
`examples/micro/docs/verify-chain.md` SHALL 增补「DTM Saga/TCC 两链 trace 实测」两节
（该文件是 micro 既有���「全链路实测证据」落点），
`examples/micro/README.md`
的拓扑/端口/起停段 SHALL 同步（:9041-9044 分支面、dtm 部署、`dtm_e2e.sh`）。
E3 下单链验收口径 SHALL 由手动编排改为 Saga 口径；现有其余验收（E1/E2 等）
MUST NOT 回退。

#### Scenario: 文档与事实一致

- **WHEN** 对照 README 拓扑表与实际 config/端口
- THEN 六进程 + 4 个分支面 + dtm 双端口逐一在文档有落点；旧「手动补偿」口径
  在下单链相关章节不复出现

