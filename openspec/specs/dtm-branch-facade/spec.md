# dtm-branch-facade Specification

## Purpose
TBD - created by archiving change add-micro-dtm-saga-tcc. Update Purpose after archive.
## Requirements
### Requirement: 四域服 SHALL 各新增轻量 gRPC 分支面且业务仍走六层链

每个域服 SHALL 新增独立 gRPC 分支面（grpc-go listener，端口 :9041-9044，配置段
`dtm.branch.listen_addr`，不注册进 consul），承接 DTM 的 Saga Action/Compensate
与 TCC Try/Confirm/Cancel 回调（pb 落各域 `internal/dtm/proto/`）。分支 handler
MUST NOT 直连 dbx / 自行落库——数据操作 SHALL 统一经既有 `handler → service →
repository` 六层链完成。

#### Scenario: 分支数据操作经 service 层入链

- **WHEN** 分支面收到一次落库型回调（如 `stock.Deduct`）
- **THEN** 分支 handler 仅做协议转换后调用 service 分支方法，落库发生在
  repository；对分支面源码 grep 无 `gorm.Open(`/`dbx` 直连调用点（静态守卫 +
  动态 span 断言成对成立）

#### Scenario: 新增 proto 不进 kitex_gen

- **WHEN** 检查 `examples/micro/pkg/kitex_gen` 与各域 `internal/dtm/proto/`
- **THEN** dtm 分支 pb 生成产物只落在 `internal/dtm/proto/`，kitex_gen 保持
  原样（该面与 kitex 传输面互不渗透）

### Requirement: 分支调用链 SHALL 覆盖正向与补偿同 trace 且可按 gid 检索

正向分支 SHALL 在分支面 handler 入口从业务载荷中的 traceparent 字段 Extract，
开 `rpc.dtm.<entity>.<Method>` span（属性 `biz.dtm.gid`、`biz.dtm.branch`），
使各域 service/repository/db span 挂到与 bff 提交请求相同的 trace 下。

补偿/Cancel 分支 SHALL 与其正向分支**同处一条 trace**：dtm 按 step 共享载荷
（Saga 的 compensate 与 action 同取 `BinPayloads[i]`；TCC 的 cancel 与 confirm
是同一份 `TransBranch` 的副本），两侧拿到同一份 traceparent，故补偿段 Extract
后挂回正向那条链。补偿/Cancel 的 span SHALL 另置 `biz.compensation=true`，
并 SHALL 携带与正向链相同的 `biz.dtm.gid`。
链路内 MUST NOT 出现伪造同 ID 的孤儿 span —— 补偿段挂的是真实前序 span 的合法
子节点，不是另起一个同 ID 的假根。

#### Scenario: 正向链一条 trace 看完下单

- **WHEN** 无 fail inject 提交一次 Saga 下单
- **THEN** Jaeger 单条 trace 含：bff 提交 handler span、四域 `rpc.dtm.*`
  分支入口 span、各域 `service.*` / `repository.*` 与 `db.*` span，父子链正确

#### Scenario: 补偿段与正向同 trace 且按 gid 关联

- **WHEN** fail inject 触发补偿
- **THEN** 补偿各分支 span 与其正向分支落在**同一条 traceID** 上（不是新根），
  每个 span 携带与正向链相同的 `biz.dtm.gid` 与 `biz.compensation=true`；
  按该 gid 检索能同时召回正向与补偿两类 span

#### Scenario: 补偿段不是伪造的孤儿 span

- **WHEN** 检查补偿分支 span 的 parentSpanID
- **THEN** 它指向同 trace 内真实存在的前序 span，Jaeger 上无游离 span、
  无重复 traceID 的假根

### Requirement: 分支写操作 SHALL 经 dtm 官方 barrier 守护幂等

每域分支面落库操作 SHALL 使用 `dtmgrpc.BarrierFromGrpc` 官方屏障包裹；barrier
表 SHALL 落在各域业务库（DDL 进 `examples/mall/scripts/schema.sql` 系的域库增量）。
重复回调、补偿早于正向到达（空补偿）、Try 先崩溃再 Cancel（悬挂）SHALL 均安全，
不产生脏数据或重复账变。

#### Scenario: 重复回调幂等

- **WHEN** dtm 因重试对同一分支发起第二次回调（首次已提交成功）
- **THEN** barrier 判定重复，分支直接返回成功，不重复扣减

#### Scenario: 空补偿安全

- **WHEN** 补偿分支先于其正向分支到达（分支从未执行过）
- **THEN** barrier 判定空补偿，直接记录并返回成功，业务库无写入

