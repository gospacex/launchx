# add-micro-dtm-saga-tcc — 设计

> 上游：`openspec/changes/add-micro-dtm-saga-tcc/proposal.md`（动机与范围）。
> 本文件记录「HOW」的架构级决策；行级实现归 `plan-ready.md` 与
> `docs/superpowers/plans/2026-10-13-add-micro-dtm-saga-tcc.md`。

## Context

`examples/micro` 现状：

- gateway(:8080) → bff(:8081) → 四域服（product/order/stock/points，kitex RPC :9021-9024）；
  域服是 `srv` 档、**无 HTTP 面**；六层分层链（rpc → route → middleware →
  handler → service → repository → model）与全套 trace 续链已就绪。
- 复合下单 `POST /api/order` 是 bff 手动编排 + 手动 `ReleaseStock` 单跳补偿，
  补偿覆盖不全且非引擎语义。
- 中间件底座（consul/jaeger/mysql/redis/kafka）由外部 docker-compose 提供；
  dtm server 当前不存在，仓内无任何 dtm 引用。

本变更新增两张事务面：**Saga 替换现有手动下单链**、**TCC 预售锁定链新增**。

## Goals / Non-Goals

**Goals:**

- bff 上两种事务（saga / tcc）经 `dtmgrpc` 提交，四域服各回一个 gRPC 分支面承接 DTM 回调；
- 分支数据操作直穿既有 `handler → service → repository` 六层入 trace（正向同 trace、
  补偿新根 + `biz.dtm.gid` 关联）；
- dtm server 官方镜像 + `STORE_DRIVER=mysql` 追加底座 compose，独立 `dtm_db`；
- 三层测试（单测 / dtm 真机集成 / `dtm_e2e.sh` 脚本三段）与文档口径同步。

**Non-Goals:**

- dtm 的 xa / msg / workflow 模式、Prometheus 接入、Dashboard、K8s 清单；
- gateway / mall / monolith / mesh / 冻结对照 `examples/micro/srv` 的改动；
- 配置中心远端源变更。

## Decisions

### D1. 分支面 = 六层复用（方案 A），不直达 repository

每域服新增轻量 gRPC 分支面（`internal/dtm/`，grpc-go，:9041-9044），分支 handler
与现有 handler 平级、转调统一走既有 `service` 层。理由：数据操作必须保留
`service.*` / `repository.*` / `db.*` 全套分层 span（用户验收第 2 条），
且「service 是唯一业务判断层」是仓纪律。备选「分支直达 repository」被否——
业务 span 缺失且打破分层；“状态机集中在 bff”被否——bff 档不建业务数据层。

### D2. 分支回调走 gRPC 而非 HTTP

域服保持「无 HTTP 面」档位纪律：分支面是**独立 grpc-go listener**，不引入 webx。
bff 侧经 `dtm-labs/client` 的 `dtmgrpc`（:36790）提交。HTTP 回调被否——
破坏 srv 档纪律且装配层不提供该面。

### D3. Saga=下单链（替换）；TCC=预售锁定（新增）

- Saga 分支序：`stock.Deduct → order.CreateOrder → product.销+1 → points.Debit`，
  失败 dtm 逆序补偿（product 销量分支使四服全参与 Saga）；
- TCC 分支序（Try 阶段）：`stock.TryFreeze、points.TryFreeze、order.TryPlace`
  （占位单 `presale_locked`，**不写 outbox**）；Confirm 才实扣/转正式单/写 outbox
  （事件下游感知到的只有「已支付」）；Cancel 全解冻。
  与 Saga 的「逐行动作 + 逆序补偿」形成语义对照。
- bff 提交前 `product.GetProduct` 预检（普通 RPC 读，非事务分支，失败 4xx）。

### D4. trace 传递：载荷携带 traceparent

traceparent 编入分支业务载荷（编号字段），正向分支在分支面 handler 入口
Extract → `rpc.dtm.<entity>.<Method>` span（属性 `biz.dtm.gid`/`biz.dtm.branch`）。
补偿/Cancel 由 dtm 异步发起 → 新根 trace + `biz.dtm.gid` 关联（与 outbox
补发同口径）。不依赖 dtm 透传自定义 metadata（不可控）。

公共件三件套（失败语义 / traceparent 续链 / 官方 barrier 包装）落
`github.com/gospacex/dtmx`（物理路径 `../dtmx`，与 dbx/mqx/kvx/lockx/otelx 平级），
launchx 以 `replace github.com/gospacex/dtmx => ../dtmx` 消费。

理由：这三样与业务无关，而 dtm 是跨形态能力（saga/tcc 的分支语义在
examples/{micro,mall,monolith,mesh} 都成立）。留在 `examples/micro/pkg/` 下会让
下一个接 dtm 的形态复制一份，而复制件之间的一致性只能靠人工 —— 这正是 D5 那个
风险项（自造失败哨兵）的成因。独立成组件仓后，复制变成 import。

边界：dtmx 是**纯库**，不 import launchx / configx / dbx，不起 listener、不读配置；
branch URL 的 `<host:port>` 段由调用方从自己的 config 拼入（dtm server 负责拆解与拨号）。

### D5. 幂等屏障用 dtm 官方 barrier

每个分支写操作经 `dtmgrpc.BarrierFromGrpc` 包裹；barrier 表进各域业务库
（DDL 入 `examples/mall/scripts/schema.sql` 系）。空补偿/防悬挂/重放由此守卫。

### D6. 部署：官方镜像 + 现有底座

dtm server 以官方形态（双端口 :36789/:36790，`STORE_DRIVER=mysql` +
`STORE_HOST/PORT/USER/PASSWORD/DB`）追加进现有底座 compose，存储库独立
`dtm_db`（密码复用 `HUBX_DB_PW`）。所有新地址进 `config.yaml`
（`xmiddleware.dtm` server 段、各域 `dtm.branch` 段）；`strict.dtm: false`
——dtm 不可达时 bff 提交 5xx + Warn 带病运行，进程不退。

### D7. 失败语义显式建模

分支返回区分**业务失败**（`dtmgrpc.ResultFailure`，不重试立即补偿/Cancel，
如积分不足、库存不足）与**基础失败**（普通 gRPC error，dtm 重试）。
失败注入走载荷 `fail_inject` 字段（`chain.sh --compensate` 演示口径的替代）。

### D7a. gRPC 分支的失败必须走 status code，不是响应体（2026-10-13 amend 新增）

D7 的「业务失败」在 gRPC 分支上**不能**靠响应体表达。dtm 回调用 **raw codec**：
dtm server v1.19.0 的 `dtmsvr/trans_status.go::getGrpcResult` 把响应字节塞进
`*[]byte`，**从不解析响应体**；客户端 `dtmgrpc/type.go::GrpcError2DtmError`
只看 gRPC status —— `Aborted` = 业务失败（不重试）、`FailedPrecondition` + ONGOING、
其余 = 基础失败（重试）。

故四域分支 handler 的失败返回口径固定为：
- 业务失败 → `status.Error(codes.Aborted, ...)`
- 基础失败 → `status.Error(codes.Internal, ...)`
- 响应体 `Code`/`Message` 只作人读与将来 HTTP 分支桥接，**不参与失败裁决**

这条搞反的后果与 D7 自造哨兵同级：业务失败被当基础失败**无限重试**。

### D7b. 冻结/实扣的三态不变式（2026-10-13 amend 新增）

`total` 是**累计入库量**，`AdjustTx` 的入库是 `total`/`available` 同量增减。
故 ConfirmFreeze 若只 `frozen -= n` 而不减 `total`，一次入库会被算两遍，
其后可卖总量虚高。

不变式：`available + frozen + reserved + sold <= total`，且
- `TryFreeze`：`available -= n, frozen += n`（total 不动，Cancel 才能原样退回）
- `ConfirmFreeze`：`frozen -= n, total -= n`（真正实扣，累计入库同步减少）
- `CancelFreeze`：`frozen -= n, available += n`（total 不动）
- Saga 的 `Deduct`/`Release` 同样做成 `total ∓ n, available ∓ n` 的严格逆运算

### D8. dtmx 独立为 hubx 平级组件仓（2026-10-13 amend 新增）

公共件三件套（失败语义 / traceparent 续链 / 官方 barrier 包装）落
`github.com/gospacex/dtmx`（物理路径 `../dtmx`，与 dbx/mqx/kvx/lockx/otelx 平级），
launchx 以 `replace github.com/gospacex/dtmx => ../dtmx` 消费。

理由：这三样与业务无关，而 dtm 是跨形态能力（saga/tcc 的分支语义在
examples/{micro,mall,monolith,mesh} 都成立）。留在 `examples/micro/pkg/` 下会让
下一个接 dtm 的形态复制一份，而复制件之间的一致性只能靠人工 —— 这正是 D7 那个
风险（自造失败哨兵）的成因。独立成组件仓后，复制变成 import。

边界：dtmx 是**纯库**，不 import launchx / configx / dbx，不起 listener、不读配置；
branch URL 的 `<host:port>` 段由调用方从自己的 config 拼入（dtm server 负责拆解与拨号）。
依赖方向上 `ExtractParent` 走 `otelx/propagation`（launchx 的 Inject/Extract 背后
即它），**不可**反向 import launchx —— 会被 replace 闭成环。

## Risks / Trade-offs

- [分支面两套 proto（dtm 分支 proto 与 kitex_gen）并存] → 生成产物隔离在
  各域 `internal/dtm/proto/` 与 `scripts/gen.sh`，不进 `pkg/kitex_gen`。
- [traceparent 编进载荷是自定义约定] → 正向链此前未被 dtm 透传保护；
  缓解：单测覆盖注入/Extract 成对，集成测试断言同 trace。
- [mtime：MySQL barrier 表 DDL 与 dtm 自建事务表分属业务库与 dtm_db]
  → schema.sql 注释标明归属；`dtm_db` 由 dtm 启动自建，不写 DDL。
- [Saga 替换现链会改 E2/E3 验收脚本口径] → 文档同步列入任务
  （ACCEPTANCE.md / verify-chain.md / micro README），不回退已绿的
  非下单链验收。
- [dtm-labs/client 版本漂移] → 计划阶段钉住版本号，go.mod replace 机制沿用。

## Migration Plan

底座 compose 追加 dtm 服务（先 `dtm_db` 建库，再起 dtm）→ 四域服/bff 新代码
+ config 新段 → schema 增量 DDL → 起 `dtm_e2e.sh` 三段验收。回滚：compose
去 dtm 服务、代码回退（此变更不影响库本体 `launchx/**`，只动 `examples/micro/**`）。

## Open Questions

- 无。dtm 客户端具体版本在实现计划首个任务中钉版并验证 API 形态。
