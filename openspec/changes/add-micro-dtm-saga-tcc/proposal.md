# add-micro-dtm-saga-tcc

> 本变更由 `/sddflow brainstorming` 产出，经五段分项确认 + 整体确认
>（2026-10-13）。设计方向：**方案 A「分支面复用六层链」+ DTM（saga + tcc）
> 完整接入微服样板**。

## 背景与目标

`examples/micro` 当前的复合下单链是 bff 用普通 RPC 手动编排
（`product → stock → order → points`），失败时仅手工补一次
`demo-stock.ReleaseStock`（`biz.compensation=true` span）。问题：

1. 手动补偿只有一跳——订单/积分的逆向不存在，挂掉中间环节会留脏数据；
2. 没有任何「先冻结、二阶段收口」的事务形态；
3. 补偿逻辑散落在 bff fixture 里，不是编排引擎语义。

本变更引入 [dtm-labs/dtm](https://github.com/dtm-labs/dtm)：**Saga 替换
现有手动编排下单链**（`ReleaseStock` 手动补偿退役），**TCC 以独立的
「预售/抢购锁定」业务新增**，两者在同一 bff + 四域服上形成两种事务
语义的完整对照样板；全部数据操作直穿既有六层分层链入 Jaeger。

## 用户场景

- **场景 ①（Saga·下单链，替换现有）**：用户 `POST /api/order`，bff 组
  Saga 提交 DTM server，四个域服分支逐个扣减，任一业务失败 → dtm 自动
  **逆序补偿**（含新增的积分退款、订单作废、商品销量回退），最终一致。
- **场景 ②（TCC·预售锁定链，新增）**：大促预订，用户
  `POST /api/presale/lock`，bff 组 TCC 提交。Try 阶段全量冻结
  （库库存/积分/占位订单），Confirm 才实扣建正式单（此刻才写 outbox），
  Cancel 则全部解冻零脏数据。
- **场景 ③（可观测）**：两条链四域服全部数据操作进同一条 Jaeger trace
  （正向），补偿/Cancel 段以新根 trace + `biz.dtm.gid` 关联可整段回放。

## 设计方向

### 架构 / 整体结构

```
gateway :8080 → bff :8081（编排层）
                ├── POST /api/order        （Saga 链入口，替换现有手动编排）
                ├── POST /api/presale/lock （TCC 链入口，新增）
                └── dtmgrpc ──→ DTM server :36790（官方镜像 :36789/:36790 双端口）
                                   │  STORE_DRIVER=mysql → 底座 mysql 独立 dtm_db
                                   ↓ 分支回调（gRPC）
        ┌──────────┬──────────┬──────────┐
   product    order      stock      points
   :9041      :9042      :9043      :9044     ← 新增轻量 gRPC 分支面
```

- bff 新增 `internal/dtm/` 事务编排；现有 `ReleaseStock` 手动补偿逻辑退役删除。
- 四域服各新增独立轻量 gRPC 分支面（:9041-9044，kitex RPC 口不动），承接
  DTM 回调；**不注册进 consul**（DTM server 静态地址回调）。
- DTM server 官方镜像追加进现有底座 compose，独立 `dtm_db`。
- 全部新地址进 `config.yaml`，Go 代码零硬编码（仓纪律 4）。

### 核心组件 / 模块划分

| 组件 | 位置 | 职责 |
|---|---|---|
| bff 事务编排 | `bff/internal/dtm/`（新增） | `dtmgrpc.Saga` / `dtmgrpc.TCC` 构造与提交（`dtm-labs/client`），`DtmAddr` 读 config；两个 HTTP handler 只做请求 → 编排 → 返回全局事务 ID |
| 域服 gRPC 分支面 | `srv-*/internal/dtm/`（新增） | 起 grpc-go listener（:9041-9044），注册各域分支服务（pb 新增，`scripts/gen.sh` 沿用生成惯例），分支 handler 与现有 handler 平级 |
| service 层分支方法 | 各域 `internal/service` | Saga：stock `Deduct`/`Release`、order `CreateOrder`/作废、product 销量 ±1、points `Debit`/`Refund`；TCC：stock/points/order 各 Try/Confirm/Cancel 三兄弟分支 |
| 分支面 span | — | 入口 `rpc.dtm.<entity>.<Method>`，属性 `biz.dtm.gid` / `biz.dtm.branch`；补偿/Cancel 分支置 `biz.compensation=true` |
| 配置面 | 各 `config.yaml` | 新增 `xmiddleware.dtm`（server addr `127.0.0.1:36790`）与 `dtm.branch`（listen addr）段；`strict.dtm: false` → dtm 不可达时 Warn 带病运行 |

### 数据流 / 接口

**Saga 下单链**（`POST /api/order` 路径不变）：

```
bff 预检：product.GetProduct（普通 RPC 读，同 trace，失败 4xx 不提事务）
  ① stock.Deduct      :9043  库存表 + 库存流水多表事务    补偿：stock.Release
  ② order.CreateOrder :9042  orders + items + outbox 三表事务 补偿：订单作废
  ③ product.销量+1     :9041  域内新写分支（四服全参与 Saga）  补偿：销量 -1
  ④ points.Debit      :9044  积分两表事务                补偿：points.Refund
失败逆序补偿 ④→③→②→①，dtm 重试达成最终一致
```

**TCC 预售锁定链**（`POST /api/presale/lock`）：

```
Try（任一失败 → dtm 逆序自动 Cancel）
  ① stock.TryFreeze   库存可售↓、冻结↑（不实扣）
  ② points.TryFreeze  积分可用↓、冻结↑
  ③ order.TryPlace    占位单 status=presale_locked（不写 outbox）
Confirm
  stock 冻结→实扣；points 冻结→消账+流水；order 占位单→正式单（此刻写 outbox）
Cancel
  三域解冻/废占位，均幂等
```

**调用链传递**：traceparent 编入分支业务载荷，正向各分支 Extract 后挂在
同一 trace 下；补偿/Cancel 是 dtm 异步回调 → 域服新根 trace + `biz.dtm.gid`
关联（与现有 outbox 补发同口径，不伪造同 ID 孤儿链）。

**幂等屏障**：分支写操作用 dtm 官方 barrier（`dtmgrpc.BarrierFromGrpc`），
barrier 表进各域业务库。

### 错误处理

- 分支返回显式区分 **业务失败**（`dtmgrpc` 语义，不重试立即补偿，如积分不足）与
  **基础失败**（gRPC error/超时，dtm 自动重试）。
- 失败注入：分支载荷 `fail_inject` 字段指定命中某分支返回业务失败，用于点火
  补偿/Cancel 链（对应现有 `--compensate` 演示口径）。
- barrier 守卫空补偿/防悬挂/幂等重放。
- dtm server 不可达：bff 提交 5xx + Warn（`consequence` + `hint`），不回落手动链。
- dtm server kill → 重启：事务状态持久 `dtm_db`，自动续推进（验收含此实验）。

## 关键决策

1. **Saga 替换现有 bff 手动编排下单链**（`ReleaseStock` 手动补偿退役）；
   TCC 以新增的**预售/抢购锁定**业务实现，与 Saga 的「逐行动作 + 逆序补偿」
   形成语义对照。
2. **gRPC 分支面**（dtmgrpc 双端口 :36790 提交，域服分支面 :9041-9044 回调），
   分支内走既有 `handler → service → repository` 六层链，数据操作完整入 trace。
3. **DTM server 官方镜像 + 官方 `STORE_DRIVER=mysql` 环境变量形态**追加进现有
   底座 compose，独立 `dtm_db`（复用 HUBX_DB_PW）。
4. **三层测试全落**：单测（返回建模/barrier/编排 + 静态守卫）→ dtm 真机集成
   （Go test，缺底座 Skip）→ 脚本级验收 `scripts/dtm_e2e.sh`（三段 + 反向探针，
   退出码即结论）；E3 验收口径随替换同步改写。
5. 依赖 `github.com/dtm-labs/client`（dtmgrpc API），不直接引 dtm server 源码。

## 范围边界

**包含：**
- `examples/micro` 的 bff 与四域服：saga/tcc 编排、gRPC 分支面、service
  分支方法、barrier、DDL（销量字段/barrier 表）、config 新段
- DTM server 底座 compose 追加（官方镜像 + mysql 存储形态）
- `examples/micro/scripts/dtm_e2e.sh` + 三层 Go 测试 + 静态守卫
- 文档同步：`examples/ACCEPTANCE.md` DTM 验收节、`docs/verify-chain.md`
  两链 trace 实测、`examples/micro/README.md` 拓扑与端口

**不包含（本次）：**
- `examples/mall` / `examples/monolith` / `examples/mesh` 不动；
  `examples/micro/srv`（冻结对照）不动
- dtm 的 xa / msg / workflow 模式
- gateway 代码（`/api/*` 前缀透传自动覆盖新端点）
- dtm metrics 接入 Prometheus、Dashboard、K8s 部署清单
- 配置中心远端源变更

## 验收标准

- [ ] Saga 成功：`POST /api/order` 提交后四库落库正确，Jaeger 单 trace
      含正走四分支与各层 span（`rpc.dtm.*` / `service.*` / `db.*` 同链）
- [ ] Saga 失败注入：`fail_inject` 触发逆序补偿，四库回到基线，补偿段
      新根 trace 带 `biz.dtm.gid` 且可与正向链查关联
- [ ] TCC Try 全成功 → Confirm：冻结转实扣、占位单转正式单、outbox 行
      **仅在 Confirm 后**出现
- [ ] TCC 任一 Try 失败 → Cancel：三域解冻/废占位，零脏数据
- [ ] barrier 空补偿/重复回调安全（幂等断言）
- [ ] dtm server kill → 重启后未完成事务续推进（重启实验绿）
- [ ] 分支面数据操作经 `service → repository` 入链：静态守卫（分支 handler
      禁直连 dbx）+ 动态 span 断言成对
- [ ] `dtm_e2e.sh` 三段全绿，`Want/Observed` 分离 + 至少一次反向探针、退出码即结论
- [ ] 全部新地址落 `config.yaml`（复用 `TestServiceSourceHasNoHardcodedLoopbackAddr` 绿）
- [ ] E3 验收口径、`examples/ACCEPTANCE.md`、`docs/verify-chain.md` 同步改写

## Amendments

### 2026-10-13：dtmx 由仓内局部件提升为 hubx 平级组件仓

- **原因**：原设计把 dtm 公共件放在 `examples/micro/pkg/dtmx/`，是 micro 示例私有的包。
  但 dtm 的失败语义分流、traceparent 续链、官方 barrier 包装这三样与业务无关，
  `examples/{mall,monolith,mesh}` 任一形态接入 dtm 时都要复制一份 —— 复制即漂移，
  而这三样任何一处漂移的后果都是**静默丢账**（自造失败哨兵 → 业务失败被无限重试；
  续链入口不同源 → 每个服务都有 span 却不共链）。
  故按 hubx 既有体例（`dbx`/`mqx`/`kvx`/`lockx`/`otelx` 平级）独立成组件仓
  `github.com/gospacex/dtmx`，由 launchx 以 `replace => ../dtmx` 消费。
- **影响范围**：仅「公共件的归属与可复用性」。**不改变**任何业务行为、验收条件与
  D1-D7 决策 —— Saga/TCC 的分支语义、六层链纪律、barrier 按域隔离、Jaeger 入链要求全部不变。
- **不包含**：不把 dtm server 本身做成 launchx 组件（它仍是外部进程）；
  不给 dtmx 引入配置中心装配面（barrier 表名等经构造参数传入，保持纯库）。

