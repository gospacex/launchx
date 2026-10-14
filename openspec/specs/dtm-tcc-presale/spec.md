# dtm-tcc-presale Specification

## Purpose
TBD - created by archiving change add-micro-dtm-saga-tcc. Update Purpose after archive.
## Requirements
### Requirement: 预售锁定 SHALL 以 DTM TCC 提交（新增端点）

`examples/micro/bff` SHALL 新增 `POST /api/presale/lock`，经 `dtmgrpc` 组
**TCC** 事务提交，二阶段三对分支：

| 阶段 | stock(:9043) | points(:9044) | order(:9042) |
|---|---|---|---|
| Try | `stock.TryFreeze`（可售↓、冻结↑，不实扣） | `points.TryFreeze`（可用↓、冻结↑） | `order.TryPlace`（占位单 `presale_locked`，**不写 outbox**） |
| Confirm | 冻结 → 实扣 | 冻结 → 消账+流水 | 占位单 → 正式单（**此刻才写 outbox**） |
| Cancel | 解冻 | 解冻 | 废占位 |

Try 阶段任一分支业务失败 SHALL 触发 dtm 逆序自动 Cancel；全 Try 成功后由
dtm 收到的 Confirm/Cancel 指令推进二阶段。三对分支均 SHALL 幂等。

#### Scenario: Try 全绿后 Confirm 收口

- **WHEN** `POST /api/presale/lock` 提交且三 Try 全部成功，随后走 Confirm
- **THEN** 三域冻结转实扣 / 占位单转正式单（`status=paid`）且 outbox 行
  **仅在 Confirm 后**出现

#### Scenario: 任一 Try 失败全量解冻

- **WHEN** `points.TryFreeze` 因余额不足返回业务失败
- **THEN** dtm 逆序 Cancel 已冻结的 stock（解冻）与已落占位单（废单）；
  三库无任何实扣或脏数据

#### Scenario: 支付超时走 Cancel

- **WHEN** 确认窗口结束未确认（验收脚本注入 Cancel 指令）
- **THEN** 三域全部解冻/废占位，重复 Cancel 亦幂等安全（barrier 空补偿守卫）

### Requirement: TCC 与 Saga SHALL 在同一 bff 与四域服上语义对照且互不复用分支

TCC 三对分支方法 SHALL 与 Saga 四对分支各自独立建模（不复用扣减接口充当冻结），
SHALL 在同一 bff 编排包内以同构的配置、提交、trace 口径实现，形成两种事务
模式的对照样板。

#### Scenario: 两链共用基础设施而不混用业务分支

- **WHEN** 检查 bff 编排代码与各域 service 分支方法
- **THEN** Saga 四分支与 TCC 三对分支方法名互不重叠、语义互不借用；
  两链共享同一 `dtmgrpc` 提交封装、同一 trace 编号约定与同一配置段

