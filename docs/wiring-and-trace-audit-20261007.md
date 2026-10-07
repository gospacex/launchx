# wire 注册面与 examples 入链核查（2026-10-07）

一次问答式核查，回答两件事：

1. `internal/wiring` 是否为 15 个 hubx `*x` SDK 真正建立了 goforj/wire 注册面？
2. examples 的单体 / 微服案例，操作中间件时是否**必须**经 launchx 取句柄？调用链开关打开后是否真的入链？

结论先给：**注册面已建成、验收全绿，但尚未挂进 App 装配主链路**；
**句柄取用是强制的，且默认 config 下 8 个 SDK 的操作已在同一条 trace 里实测可见**。

---

## 一、注册面：15 个 SDK 的现状

注册面位于 `internal/wiring/`，由 goforj/wire 在编译期求解依赖顺序，运行时决策仍留在 provider 内。

| SDK | 类别 | wire provider | 入命理据 | 构造现状 |
|---|---|---|---|---|
| configx | 地基 | `ProvideConfig` | `Env.Cfg` | 恒构造，失败即退 |
| logx | 地基 | `ProvideLogger` | `Env.Logger` | 恒构造，失败即退 |
| otelx | 地基 | `ProvideTelemetry` | 全局 TracerProvider | 恒构造；Provider 为空即报错，不静默 noop |
| dbx | 可选 | `ProvideDB` | builder `db` | presence `xmiddleware.sqlx` |
| cachex | 可选 | `ProvideCache` | builder `cache` | presence `xmiddleware.cachex` |
| mqx | 可选 | `ProvideMQ` | builder `mq` | presence `xmiddleware.mqx` |
| coordx | 可选 | `ProvideCoord` | builder `registry` | presence `xmiddleware.coordx` |
| searchx | 可选 | `ProvideSearch` | builder `search` | presence `xmiddleware.searchx` |
| mongodbx | 可选 | `ProvideMongo` | builder `mongodb` | presence `xmiddleware.mongodbx` |
| kvx | 可选 | `ProvideKV` | builder `kv` | presence `xmiddleware.kvx` |
| lockx | 可选 | `ProvideLock` | builder `lock` | presence `xmiddleware.lockx` |
| objectx | 可选 | `ProvideObject` | — | **登记在册，默认零值**（依赖面代价，见下） |
| vectorx | 可选 | `ProvideVector` | — | **登记在册，默认零值** |
| rpcx | 形态产物 | `ProvideRPC` | `mode.AppHolders.Out` | 由 `service.mode` 决定 |
| webx | 形态产物 | `ProvideWeb` | `mode.AppHolders.Out` | 由 `service.mode` 决定 |

`hubx` 不在表内 —— 按 2026-10-06 裁决移出注册面，`tasks.md` 5.6 保持未勾选。

### 边界：wire 接管什么、不接管什么

wire **只**接管依赖求解与构造顺序。presence-based 启用、`trace.on.<name>` 只控入链不控构造、
`strict.<name>` 失败姿态这三件事**仍在 provider 内读 `Env` 裁决**。这是刻意的：10 个可选组件
若把有无编进图，需要 2^10 个注入器。该边界由 `TestW4PresenceIsRuntimeAdjudication` 与
`TestW4StrictControlsFailurePosture` 锁死。

### 今日实测

```
go build ./internal/... ./examples/micro/... ./examples/monolith/...   → OK
go test ./internal/wiring/                                             → 5 PASS
PROBE_GO=~/sdk/go1.24.3/bin/go go test -run TestW1WireIsBuildTimeOnly  → PASS（5.42s）
```

`TestW1WireIsBuildTimeOnly` 默认会 Skip 第三段：链接级探针需要起一个真实二进制，
未设 `PROBE_GO` 时按设计跳过并说明原因（"跑不了就跳过"与"跑不了就假装通过"是两回事）。
设上之后源码级 / 产物级 / 链接级三层全绿。

### 缺口（当日已补齐，见文末补记）

**核查当时全仓零处 `import github.com/gospacex/launchx/internal/wiring`** —— 引用只出现在包自身与计划文档中。
当前 App 装配仍走 `internal/pipeline` 的固定七步骨架（`config.load → telemetry.init →
logger.init → mode.assemble → component.mount → registry.register → setup`）+ `component.Lookup`
builder 注册表，`wiring.InitializeGraph` 没有被任何生产路径调用。

即：注册能力已具备（`internal/wiring` 可编译、可调用、测试全绿），但装配尚未改走 wire。
要做的是把骨架的 `component.mount` 与 `mode.assemble` 两段换成 `InitializeGraph(env, holders)`，
并补一条集成级断言（当前 W6 只复用了 pipeline 既有的 A3 退出序断言）。

---

## 二、examples 的句柄取用：是强制的

### 三层强制

1. **收敛点**：取句柄的代码只出现在各案例的 `app/setup.go`
   （monolith 10 处、srv-* 各 14 处、bff 2 处、gateway 4 处），业务分层零取用。
2. **红线自检**（今日复跑，排除 `_test.go`）：
   - `gorm.Open|redis.NewClient|kafka.New*|consul.NewClient|mongo.Connect|elasticsearch.NewClient|sql.Open`
     —— 零命中（唯一出现处是注释与 handlecheck 守卫自身的文案）。
   - `127.0.0.1` / `localhost` 硬编码 —— 零命中（排除 kitex_gen / _test / mall）。
3. **运行时判据** `internal/handlecheck.VerifyDBX`：自己 `gorm.Open` 一个同名库照样通过类型断言、
   照样能查能写，唯独 SQL 不进链路 —— 所以判据取"dbx 注册的 4 个 gorm trace 回调在不在"，
   不在就让服务起不来。调用条件：**仅当 `trace.on.db` 为真**（dbx 走 `OpenNoTrace` 时没有回调
   是配置选择，不该让服务起不来）。

注册/注销一律由 launchx 按 `service.mode` 完成，任何 demo 都不调 `Register()` / `Deregister()`。

---

## 三、开关打开后：入链实测

### 接线落点（component builder 内）

| SDK | 入链开关 | 实际接法 |
|---|---|---|
| dbx | `trace.on.db` | `orm.Open`（带 OTel 回调，span 带 SQL 文本）vs `orm.OpenNoTrace` |
| cachex | `trace.on.cache` | `cachex.SetTracingConfig` + 实例 `EnableTrace` |
| mqx | `trace.on.mq` | `cfg.EnableTrace` / `CallChainEnabled` + `traceflag.NewTracer` |
| mongodbx | `trace.on.mongodb` | `SetTracingConfig` + 实例 `EnableTrace` |
| kvx | `trace.on.kv` | 实例 `EnableTrace` |
| lockx | `trace.on.lock` | 实例 `EnableTrace` |
| searchx | `trace.on.search` | 实例 `EnableTrace` |
| coordx | `trace.on.registry` | `EnableTrace` |
| rpcx / webx | 非组件开关 | 经形态装配的 kitex / hertz 中间件 |
| objectx / vectorx | 未接 | 未进入模块依赖图 |

D1 红线：`trace.on.X=false` **不**决定是否构造，只决定是否入链 —— `wiring.base.trace` 只记录
不参与判断，由 `TestW3TraceOnDoesNotGateConstruction` 守着。

### 实测证据（2026-10-07，六进程真机在跑）

trace-id `4bd70cf26461108333c1250b76d4e932`：**97 span / 树深 16 / 6 服务**。
组件级 span 在同一棵树下：

| SDK | 该 trace 内实测 span |
|---|---|
| coordx | `coord.discover`（bff ×4、gateway ×1） |
| dbx | `db.query` / `db.create`（×9）/ `db.update` |
| cachex | `db.redis.GET` / `SET` / `DEL` |
| kvx | `db.kvx.GET` / `SET` / `INCR` / `EXISTS` |
| lockx | `lock.redis.acquire` / `release`（四域服各一对） |
| mqx | `messaging.outbox.send` → `messaging.kafka.send` |
| rpcx | `rpc.*`（服务端）+ `client.demo-*`（客户端） |
| webx | `POST /api/order`、`POST /order` |

`go test ./examples/micro/` T6（三段同 trace 且父子链正确）/ T8（无网格专属分支）/ T9（尾部 span 不缺失）
三条 PASS。

### 未能复跑的部分

- **mongodbx / searchx** 只在 monolith 配了段，本次 monolith 未起 —— 它与 micro gateway 同占
  `127.0.0.1:8080`，同机互斥。这两个 SDK 的入链依据是已提交的 `TestT4ComponentSpansShareTrace`
  （断言 `db.elasticsearch.*`、`db.mongodb.*`、`messaging.kafka.send` 同为 `service.CreateOrder`
  的后代），**本次未重跑，属过往证据而非今日证据**。
- **objectx / vectorx**：登记在册但从未真正构造，谈不上入链。
- Task 16（monolith `e2e_test.go`）与 Task 19（monolith `degrade_test.go`）仍缺。

---

## 四、待办候选

1. 把 `wiring.InitializeGraph` 接进 pipeline，让注册面从"具备能力"变成"实际使用"。
2. Task 16 / 19 的 monolith 测试。
3. mongodbx / searchx 的今日重跑（需先停 micro gateway 或改端口）。
4. objectx / vectorx 是否接入 —— 接则先在 `go.mod` 解依赖面，不能 `go mod tidy` 的纪律下是硬成本。

---

## 五、补记：注册面已接进主链路（同日，commit `a466351`）

本节记录核查之后立刻执行的补齐动作，条目 1 至此关闭。

| 改动 | 内容 |
|---|---|
| `app.go` | `stepComponents` 由 `component.Mount(cenv, componentOrder)` 改为 `wiring.InitializeGraph(cenv, a.holders)`；删除手写顺序表 `componentOrder`；新增 `App.Graph()` 暴露注册面快照 |
| `internal/wiring/providers.go` | `ProvideModeReady` 判 `Out.Mode` 已装配即跳过；`ProvideMQ` 补 Consumer 侧 |
| `internal/wiring/wiring.go` | `base.Constructed()` 改反射判 nil；`MQHandle` 加 `Consumer` 字段 |
| 新增 `wiring_integration_test.go` | 根包 3 条集成断言 |
| `internal/wiring/wiring_test.go` | +4 条（跳过 / 必装 / typed nil / builder 全覆盖） |

接入后暴露两个此前看不见的缺陷 —— 都是"图第一次被真正使用"才问出来的：

1. **typed nil 被当成已构造**：`Env.Cache` / `KV` / `Mongo` 是 map、slice 类命名类型，
   零值装进 `any` 后 `raw != nil` 恒为真。yaml 没配段却报"已构造"，排查时会得到
   "Graph 说接上了、Jaeger 上一个 span 都没有"的矛盾结论。已改反射判 nil。
2. **形态装配被跑两次**：pipeline 是 `mode.assemble → component.mount`，而图里形态产物
   排在可选组件之后，于是图被求解时 mode 已装过一遍；再装一次会换掉 `Out.Webx`，
   而 `a.engine` 早在 mode 步骤交了出去 —— 症状是"引擎在听、路由一个都没有"且不报错。
   已改为判 `Out.Mode` 跳过。

验证：`go build ./...` 通过；`internal/...` + 根包 + `examples/micro`（T6/T8/T9 真机）
+ `examples/monolith` 全绿。
