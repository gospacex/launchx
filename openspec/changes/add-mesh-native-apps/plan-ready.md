# 实现计划：add-mesh-native-apps

## 来源
- 提案：openspec/changes/add-mesh-native-apps/proposal.md
- 设计：无（OpenSpec 判定无需单独的 design.md —— 技术设计已在 proposal.md §一~§五 与六份 delta spec 中给全，无跨能力的技术权衡需要单独立档）
- 规格：openspec/changes/add-mesh-native-apps/specs/（7 个 capability：mesh-environment / mesh-app-bootstrap / mesh-app-handles / mesh-app-layering / mesh-deploy-manifests / mesh-trace-verification / mesh-entry-chain）
- 任务：openspec/changes/add-mesh-native-apps/tasks.md

## 前置已完成（不占 Task 编号）

`tasks.md` 0.1–0.6 已在本轮 spec 阶段复核并修复，故不进入本计划：
环境三段就绪复核（0.1）、`order_db` 数据面补齐（0.2）、
`bring-up.sh check` 校验范围修复（0.3）、`verify` 探针镜像依赖修复（0.4）、
`order_db.sh` 客户端选择修复（0.5）、注册竞态陷阱记录（0.6）。

## 任务与规格追溯

| 本计划 Task | tasks.md 条目 | 覆盖的 spec Requirement |
|---|---|---|
| 1 | 1.1 | mesh-app-bootstrap「独立 module 与构建入口」；mesh-app-layering「apps 与 micro 零 import 耦合」 |
| 2 | 1.2 | mesh-app-handles「句柄来源与入链必须可自检」；mesh-app-layering「service 层是唯一允许出现业务判断的层」 |
| 3 | 1.3 | mesh-app-handles「每个进程的句柄取用矩阵」「句柄缺失的姿态必须显式」；mesh-entry-chain「四域 RPC 实现体必须实现全部 IDL 方法」；mesh-app-layering「四层 span 逐层可见」 |
| 4 | 1.4 | 同上（points 行：`DB()` + `MQConsumer()`）+ mesh-entry-chain「四域 RPC 实现体必须实现全部 IDL 方法」+ mesh-app-layering「四层 span 逐层可见」 |
| 5 | 1.5 | 同上（product 行：`DB()` + `Cache()`）+ mesh-entry-chain「四域 RPC 实现体必须实现全部 IDL 方法」+ mesh-app-layering「四层 span 逐层可见」 |
| 6 | 1.6 | 同上（stock 行：`DB()` + `KV()` + `Lock()`）+ mesh-entry-chain「四域 RPC 实现体必须实现全部 IDL 方法」+ mesh-app-layering「四层 span 逐层可见」 |
| 7 | 1.7 | mesh-app-handles（bff 行：`Engine()` + `RPCClientOptions()` + `Cache()`）；mesh-entry-chain「bff 的路由表必须与四域一一对应」 |
| 8 | 1.8 | mesh-app-bootstrap「五个进程统一以 launchx.Bootstrap 装配」「apps 侧零服务注册」「退出链不在 apps 侧重写」 |
| 9 | 3.1 | mesh-app-layering「结构性守卫继续生效」；mesh-app-handles「中间件一律经 App 句柄取用」 |
| 10 | 3.2 | mesh-app-handles「每个进程的句柄取用矩阵」；mesh-app-layering「四域同构」 |
| 11 | 2.1 | mesh-deploy-manifests「配置段地址一律换算为集群内可达名」 |
| 12 | 2.2 | mesh-deploy-manifests「探针类型按跳的协议选定」 |
| 13 | 2.3 | mesh-deploy-manifests「入口由 Istio Gateway 声明」（含「入口 1 条 vs bff 6 条」分工） |
| 14 | 2.4 | mesh-deploy-manifests「镜像与变体互不污染」（含路由矩阵随变体切换） |
| 15 | 3.3 | mesh-trace-verification（三条 Requirement 全部，T13 收紧为路由矩阵）；mesh-entry-chain「每条路由的验收必须断言到目标域的业务层」「异步链必须单独断言」「验收证据以矩阵形式落盘」 |
| 16 | 3.4 | mesh-trace-verification「bff→srv 这一跳永远没有 sidecar span」；mesh-environment/探针结论修正 |
| 17（amend 追加） | 4.1 / 4.2 | mesh-entry-chain「每条路由的验收必须断言到目标域的业务层」「异步链必须单独断言」；mesh-deploy-manifests「变体不得把 apps 的验收退回单路径」 |
| 18（amend 追加） | 4.4 | mesh-app-layering「service 层是唯一允许出现业务判断的层」（四层 span 与八目录职责）；mesh-entry-chain「跨域编排只在 bff，srv 之间不互调」 |

## 实现步骤

### Task 1: apps module 公共件与 IDL 产物
- [x] **任务完成**（与 superpowers plan `Task 1`、`tasks.md` 1.1 同步勾选）
- 目标：让 `examples/mesh/apps` 成为可独立编译的 module，并把四域 IDL 生成产物与三个公共包摆好
- 改动文件：`apps/go.mod`、`apps/go.sum`、`apps/pkg/codes/codes.go`、`apps/pkg/events/events.go`、`apps/pkg/routing/routing.go`、`apps/pkg/kitex_gen/{order,points,product,stock}/**`（只读搬运生成产物）
- 验证方式：`cd examples/mesh/apps && go build ./...` 通过；`go list -m all | grep launchx` 指向本地路径

### Task 2: srv 通用骨架与句柄自检
- [x] **任务完成**（与 superpowers plan `Task 2`、`tasks.md` 1.2 同步勾选）
- 目标：把四域共用的两组件落下来 —— RPC 跳限流中间件、句柄来源与入链自检
- 改动文件：`apps/internal/srv/<域>/middleware/ratelimit.go`、`apps/pkg/handles/check.go`
- 验证方式：`go build ./...`；`go test ./pkg/handles/`（自建句柄被判不合规）

### Task 3: srv-order（参考域）
- [x] **任务完成**（与 superpowers plan `Task 3`、`tasks.md` 1.3 同步勾选）
- 目标：以 order 域把七层骨架与全部五类句柄（DB / Cache / KV / Lock / MQ）跑通，作为其余三域的模板
- 改动文件：`apps/internal/srv/order/{model,repository,service,handler,route,rpc,app}/**`
- 验证方式：本地起服（`go run ./cmd/srv-order`）→ `CreateOrder` 落库并回读 → `order.completed.v1` 有消息
- **执行期修正（2026-10-07 amend #2）**：本 Task 的 Step 已全部实现（model / repository /
  service / handler / route / app 全绿，order 域各包 96–100% 覆盖），但规格合规审查发现
  三处**计划与规格的错误**，其增补由 **Task 18** 承担，本 Task 的勾选不回退：
  `rpc/` 被判为「跨域调用」而实为入向传输适配层；`handler` 直接实现 IDL 接口（现应由 `rpc/` 实现）；
  四层分层 span 全缺（判据硬前提）。另：本 Task 的「起服 + 落库回读 + MQ 有消息」端到端验收
  依赖 `cmd/`（Task 8）与四域齐备，实际在 **Task 15** 一次性验证。

### Task 4: srv-points（消费侧）
- [x] **任务完成**（与 superpowers plan `Task 4`、`tasks.md` 1.4 同步勾选）
- 目标：验证 `MQConsumer()` 的取用与生命周期（在 `App.OnStop` 里取消消费循环），并按 Task 18 定下的模板铺四层埋点与 `rpc/` 入向适配层
- 改动文件：`apps/internal/srv/points/{model,repository,service,handler,route,rpc,app}/**`
- 前置依赖：**Task 18 必须先完成**（它定下 `rpc/` 入向适配层与四层 span 的模板）
- 验证方式：消费 order 域投递的消息后，积分流水表新增一行；`service.points.ConsumeOrderCompleted` 与下单请求同 traceID；退出时消费循环被取消（无 panic、无丢位点告警）
- **执行期修正（2026-10-07）**：
  - 本 Task 只做**单元级**验证（假 `mqx.Consumer` 驱动整条消费链）；真起服 + 查 MySQL + Jaeger 的
    **运行期校验归 Task 15**（`apps/cmd/**` 与 `apps/conf/**` 是 Task 8 的产物，本 Task 早于它执行，见 amend #5）
  - 句柄面比计划宽一项：`KV()[name]` 喂 `route` 的限流计数（amend #6 要求 `route` 的限流是真的，
    已同步进 `mesh-app-handles` 的句柄矩阵）
  - `route.Admit` 走过一轮修正：原为「只读 ⇒ 恒放行」，已改为**真实的按主体限流**
    （原写法让 `route/` 退化成空目录、`RateLimited` 成死码）
  - `Unimplemented` 的唯一落点是 `handler`（Task 18 模板口径）

### Task 5: srv-product（缓存侧）
- [x] **任务完成**（与 superpowers plan `Task 5`、`tasks.md` 1.5 同步勾选）
- 目标：验证 `Cache()[name]` 的读写与写后失效；同 Task 4 铺四层埋点与 `rpc/` 入向适配层
- 改动文件：`apps/internal/srv/product/{model,repository,service,handler,route,rpc,app}/**`
- 前置依赖：**Task 18 必须先完成**
- 验证方式：连续两次读同一商品，第二次 `cache.*` span 出现在同 trace 且不再产生第二条 `db.query`
- **执行期修正（2026-10-07/08）**：
  - 本 Task 只做**单元级**验证（假 `cachex.Cache` + sqlite 内存库驱动 `service.GetProduct` 两条路径）；
    真起服 + 真 redis + Jaeger 的**运行期校验归 Task 15**（`apps/cmd/**` 与 `apps/conf/**` 是 Task 8
    的产物，本 Task 早于它执行，见 amend #5 / #7）
  - 四条判据：第二次不回源（`countingRepo` 计数）/ 回填发生（键在 + TTL > 0）/
    `service.product.GetProduct` → `repository.product.FindByID` 父子链同 TraceID / 写后失效
  - **`cache.*` 组件 span 不在单测判据内**：`cachex` 只出 redis / memcached 两个实现、无内存驱动，
    单测起不了真缓存组件 ⇒ 该条只能由 Task 15 运行期承接（**不得**在单测里假装验过）
  - `kv` 不进 service，只在 `app/setup.go` 喂 `route` 的限流计数（与 order / points 模板一致）
  - `repository.UpdatePrice` **刻意不查 `RowsAffected`**（MySQL 对「值未变化」的 UPDATE 也报 0 行，
    0 行二义：不存在 / 值没变）—— 理由已写进代码注释，防止后来者「顺手补上」
  - `admit` 身份来源计数口径的四域措辞统一留给 **Task 10**（order 写「四处」含 `"svc"` 桶，
    points / product 写「三处」+ 在 RateLimited 段补述）
  - 句柄矩阵与 `Unimplemented` 唯一落点（`handler`）同 Task 4，无新增背离

### Task 6: srv-stock（KV + 锁）
- [x] **任务完成**（与 superpowers plan `Task 6`、`tasks.md` 1.6 同步勾选）
- 目标：验证 `KV()` 的原子增减与 `Lock()` 的互斥语义，以及锁缺失时的显式姿态；同 Task 4 铺四层埋点与 `rpc/` 入向适配层
- 改动文件：`apps/internal/srv/stock/{model,repository,service,handler,route,rpc,app}/**`
- 前置依赖：**Task 18 必须先完成**
- 验证方式：并发扣减不超卖；注释掉 `lockx` 段后启动不失败且日志出现「本次未走锁路径」
- **执行期修正（2026-10-08）**：
  - 本 Task 只做**单元级**验证；真起服 + 真 redis/lockx + Jaeger 的**运行期校验归 Task 15**
    （`apps/cmd/**` 与 `apps/conf/**` 是 Task 8 的产物，本 Task 早于它执行，见 amend #5 / #8）；
    `strict.lockx` 的**配置侧**落点归 Task 8
  - Step 2（锁降级）落为单元级三条：① 缺锁 `Decrease` 照常返回（**MUST NOT 因缺锁而失败**）；
    ② 打含 `lock unavailable` 的 Warn（**MUST NOT 静默当作已加锁**）；③ 注入真锁时**不出现**该 Warn
  - Step 3（不超卖）分两层且**如实标注强度**：repository 的**单线程**条件语义是强判据
    （无条件 UPDATE 会当场 FAIL）；service 的 20 并发计数是端到端判据，但 sqlite `:memory:` 必须
    `SetMaxOpenConns(1)`（否则第二条连接看到空库）⇒ 写被串行化，该层**不**证明「条件 UPDATE 是唯一护栏」
  - `admit` 签名是 `admit(ctx)` —— **本域没有**「回落请求 IDL 的 user_id」这一级：
    `GetStockReq` **只有 `product_id`、不带 user_id**，这是与 order / points / product 的**真实差异**
    （不是漏写）；无身份落 `"svc"` 桶
  - 读路径「无库存记录」→ **零值视图而非错误**（对齐 `points.GetPoints` 的「未开户=0」，
    与 `product.GetProduct` 回 `ProductNotFound` 是刻意差异）；`pkg/codes` 未新增码
  - `Decrease` 是**内部写路径、只由单测驱动**（IDL 的 `ReserveStock` 一族在 handler 显式
    `Unimplemented`）；它的存在让「互斥 + 条件扣减不超卖」有可执行判据，**不是**隐藏的生产入口
  - 登记给 **Task 7**：`StockNotEnough`(50300) 是本域真实生产者，但 bff 的错误码映射表没有
    stock 的 not-enough 档 ⇒ 需为它定映射（建议 409 或 400）
  - 登记给 **Task 8**：`stocks` 表 DDL 同 `products`，属 `order_db.sh` 的缺项

### Task 7: bff（HTTP 边界）
- [x] **任务完成**（与 superpowers plan `Task 7`、`tasks.md` 1.7 同步勾选）
- 目标：验证 `Engine()` + `RPCClientOptions()` + `Cache()` 三件句柄的组合，以及下游地址只来自 K8s Service 名；**路由表补齐到 6 条**，使四域业务逻辑都能从入口触达（不留死客户端）
- 改动文件：`apps/internal/bff/{app,handler,downstream}/**`
- 验证方式：`curl /healthz/live`、`/healthz/ready` 均 200；六条路由各返回 2xx；`grep -cE 'clients\.(Order|Points|Product|Stock)' apps/internal/bff/**` 四名各有命中；错误码映射含 `Unauthorized→401` / `RateLimited→429`（amend #2 追加，与 srv 侧 `rpc` 闸门返回的码对应）
- **执行期修正（2026-10-08）**：
  - **本 Task 只做单元级 + 静态验证**；`curl` 起服后的运行期校验归 **Task 15**
    （`apps/cmd/**` 与 `apps/conf/**` 是 Task 8 的产物，本 Task 早于它执行）
  - **闭合 Task 6 的登记项**：`StockNotEnough`(50300) → **409**；同档补 `PointsNotEnough`(30400) → 409、
    `OrderCreateFail`(20500) → **500**（下游写库失败 = 它自己坏了，不是上游）；
    **未登记的码一律 502 而不是 500** —— 能落到 default 的只有「跨进程回来的、码表里没有的码」，
    报成 500（「bff 自己崩了」）会把排查方向带到边界层。判据由 `TestStatusOfCoversEveryBizCode` 钉住
    （清单与该包常量块一一对应，漏一个码不会有编译期提示，只会静默落到 default）
  - **增开 `handler.bff.<Op>` 分层 span**（计划未列）：`mesh-app-handles` 的「句柄动作入链」
    Scenario 要求 trace 内可见「bff 的 handler span」，而 webx 的根 span 名是 `<METHOD> <path>`
    （不带 `<layer>.<entity>.<op>` 形态），故按分层口径补一层；`/ping` 的单跳形态由 `trace-audit.py`
    的 E2 豁免覆盖。span 名与 srv 侧对齐（points 用 RPC 方法名 `GetAccount`，与
    `handler.points.GetAccount` 同名）
  - **现场校正计划的两处不可编译写法**：① 计划「入参类型用 webx 的 RequestContext」——
    实测 webx **没有**该类型（`webx.go` 只内嵌 `*server.Hertz`，路由与中间件直接用 hertz 类型），
    按 `*app.RequestContext` 落地；② 计划示例的 `json.NewDecoder(c.Request().Body).Decode(...)`
    —— `c.Request().Body` 是方法不是 `io.Reader`，改用 `c.BindJSON`（同时去掉 `encoding/json` import）
  - **`engine.Use` 必须早于路由注册**：hertz 的 `RouterGroup.Use` 只是
    `Handlers = append(Handlers, mw...)`，路由在注册那一刻把 `Handlers` **快照**进自己的链 ——
    先注册再 `Use`，中间件对已注册路由**完全不起作用且不报错**。已写进 `app/setup.go` 注释
  - **bff 不取 `Tracer()`**（与 `mesh-app-handles` 矩阵 bff 行列的 `Tracer()` 不一致，登记见下）：
    `App.Tracer()` 返回的是 `otelx.Provider`（**provider 而非 tracer**），而 `mesh-app-layering`
    的埋点口径明确「全局 provider 由 telemetry 无条件注册、业务侧不再交接」—— 同口径已写在四域
    srv 行（「不用 `Tracer()`」）。故 bff 与四域一致：向 otel 全局现取（`otelx/layer`）
  - **登记给 Task 16（文档校正）三处**：
    - 计划 Task 7 路由表括注「`GET /order/:id` → `service.order.GetOrder`（**内部再调 product / stock**）」
      与 `mesh-entry-chain`「跨域编排只在 bff，srv 之间不互调」直接冲突；实测 order 域零出向客户端
      （`internal/srv/**` grep `NewClient(` / `client.Option` / `RPCClientOptions(` 零命中），
      已按**规格**落地（该路由 trace 内只应出现 order 一域的 service span）
    - 计划 Task 7 Step 4 的判据文本把「order 不存在」写成 `20001`、「product 不存在」写成 `40004`，
      实际 `pkg/codes` 是 `OrderNotFound 20404` / `ProductNotFound 40404`
    - `mesh-app-handles` 的句柄矩阵 bff 行列出 `Tracer()`，与 `mesh-app-layering` 的口径冲突（见上）
  - 不复用 `codes.From` 作为边界的码还原入口：MUST 用 `rpcwire.FromRPC`
    （跨进程回来的 biz error 是 `*kerrors.BizStatusError`，`codes.From` 认不出 ⇒ 整张表失效）。
    该退化由 `TestErrorMappingAtBoundary` 的「跨进程回来的业务码」一档钉住，并经反向探针验过

### Task 8: 入口与配置（五进程可本地起服）
- [x] **任务完成**（与 superpowers plan `Task 8`、`tasks.md` 1.8 同步勾选）
- 目标：五个 `main.go` 与五份 `conf/*.yaml` 落地，本地可完全起齐
- 改动文件：`apps/cmd/*/main.go`（5 个）、`apps/conf/*.yaml`（5 份），另修 `apps/Makefile`（`SERVICES` 去掉不存在的 gateway —— 原值会让整个 build 循环第一轮就 exit 1）、`apps/Dockerfile`（`WORKDIR /app` + ENTRYPOINT bff）、`examples/scripts/order_db.sh`（补 products/stocks/points_account/points_log 四张表的 DDL 与两张表的唯一索引 + 读路径种子）
- 验证方式：`gofmt -l` 干净、`go vet ./...` 通过；**本机起服实测**（`/tmp` 副本改地址与端口，避开 micro 对照进程）8 条入口全 200；conf 地址自检 0 命中裸本机地址、无注册中心段。行数口径：main 函数 31 行（有效语句 26）/文件 55 行，`wc -l` 那条判据数的是文件

### Task 9: 结构与依赖面核对
- [x] **任务完成**（与 superpowers plan `Task 9`、`tasks.md` 3.1 同步勾选）
- 目标：钉住两条结构约束与一条依赖面约束
- 改动文件：无（只跑判据；若守卫失败则修 apps 侧代码）
- 验证方式：`go test . -run 'TestMesh|TestMicro|TestLibrary'` 全绿；apps 内零 `go-redis` / `kafka-go` / `consul/api`，零 `gorm.Open(` / `gorm.io/driver/*`（`_test.go` 豁免）
- 判据校正：原命令 `grep -rnE ... .` 会命中 `go.mod` / `go.sum` 里的 `// indirect` 传递依赖，三条「应为空」**永远有命中**；须加 `--include='*.go'`（glob **必须加引号**，否则 zsh 报 no matches found）。另 gorm 正当用法原列「三处」，实际是**四类**（漏了 `service/**` 的 `gorm.ErrRecordNotFound` 哨兵）

### Task 10: 句柄矩阵与分层纪律核对
- [x] **任务完成**（与 superpowers plan `Task 10`、`tasks.md` 3.2 同步勾选）
- 目标：逐进程核对句柄矩阵（多取/少取均违规）与四域同构
- 改动文件：无（只跑判据；违规则修 apps 侧代码）
- 验证方式：`srv-*` 无 `Registry()` / `Discover()` / `RPCClientOptions()`；`bff` 无 `DB()` / `MQ()`；四域子目录集合相同（各八个，含 `route/` 与 `rpc/`）；四域 `rpc`/`handler`/`service`/`repository` 四层各含 `layer.Begin`
- 判据校正：原矩阵命令循环里把 `internal/bff/` 加进了每一行 ⇒ 每行是 `bff ∪ <域>`，域侧多取 `Cache`/`Engine` 之类**永远查不出来**（注入探针实测：注入前后输出逐字相同）；且模式要求 `()` 紧邻，漏掉带参的 `RPCClientOptions(<域>)` / `SetRPCServer(srv)`。改为 `name:path` 逐进程独立 + 模式只匹配到 `(`
- 实测矩阵：`bff` → Cache/Config/Engine/Logger/RPCClientOptions；`order` → Cache/Config/DB/KV/Lock/Logger/MQ/RPCServerOptions/SetRPCServer；`points` → Config/DB/KV/Logger/MQConsumer/RPCServerOptions/SetRPCServer；`product` → Cache/Config/DB/KV/Logger/RPCServerOptions/SetRPCServer；`stock` → Config/DB/KV/Lock/Logger/RPCServerOptions/SetRPCServer。**四域都有 KV**（spec `mesh-app-handles`「全部 `srv-*` 用 `KV()`」是硬要求），本 Task 实测四域都把它传进了 `middleware.New(...)`

### Task 11: ConfigMap（五份配置）
- [x] **任务完成**（与 superpowers plan `Task 11`、`tasks.md` 2.1 同步勾选）
- 目标：把 apps 的配置搬进集群，地址段全部换成集群内可达名
- 改动文件：`examples/mesh/deploy/apps/10-configmap.yaml`（438 行，新建 `deploy/apps/` 目录）
- 验证方式：`VARIANT=apps bash examples/mesh/scripts/bring-up.sh check` 地址自检通过（实测 rc=0）
- 落地方式：由 `apps/conf/*.yaml` **程序化逐字镜像**（整体缩进 4 空格进块标量），不手抄 —— 手抄丢 `kvx` 这类段不报错。判据是往返比对：解析 ConfigMap 逐键与 `conf/` 原文 `==`，五份全一致
- 与计划模板的三处差异（模板已同步修正）：**srv-points / srv-product 保留 `kvx`**（spec「全部 srv-* 用 KV()」是硬要求，缺 kvx 是 fail-open）；**kvx 逻辑库四域统一 `db: 1`**（原模板 srv-stock 写 db 4）；`trace.on` 补 `kv`
- 挂载契约（Task 12/13 依赖）：各 Deployment 用 `subPath: <svc>.yaml` 挂到 `/app/conf/<svc>.yaml`（Dockerfile `WORKDIR /app`，入口按相对 CWD 读 `conf/<svc>.yaml`），并以 env 提供 `HUBX_DB_PW`

### Task 12: 四域 srv 清单
- [x] **任务完成**（与 superpowers plan `Task 12`、`tasks.md` 2.2 同步勾选）
- 目标：四份 `Service` + `Deployment` 落地，探针为 `tcpSocket`
- 改动文件：`deploy/apps/20-srv-order.yaml`、`21-srv-points.yaml`、`22-srv-product.yaml`、`23-srv-stock.yaml`
- 验证方式：`kubectl apply --dry-run=client -R -f examples/mesh/deploy/apps` 通过（实测 9 条 `created (dry run)`，rc=0）
- 实测补充：
  - 四路由同一模板程序化生成，判据 =「去注释行后抹除服务名/端口逐字相同」（已通过）；
    另做四路交叉核对：conf 监听端口 == Service port == targetPort == containerPort == 双探针端口 == bff `service.upstream.<域>` 端口，upstream 主机名 == Service 名 == conf `service.name`
  - **env 只有 `HUBX_DB_PW`**：与 micro 对照组不同，**没有 `POD_IP`** —— apps 零注册，
    `apps/conf/**` 与 `apps/internal/**` 内 `POD_IP` 零命中
  - ⚠️ `istioctl analyze <file>` **查不出端口命名问题**（对未 apply 的 Service 不跑 IST0118，
    已用「host-middleware 改名探针」证实）；端口命名要等 apply 后 `istioctl analyze -n launchx-mesh`
    才查得出 —— 即 Task 13 的判据，本 Task 的 dry-run 不能当该证据
  - `VARIANT=apps bring-up.sh check` 仍 rc=0（四段全绿）

### Task 13: bff / Istio 入口 / 授权清单
- [x] **任务完成**（与 superpowers plan `Task 13`、`tasks.md` 2.3 同步勾选）
- 目标：bff 用 `httpGet` 探针；入口由 Istio `Gateway` + `VirtualService` 声明；域服加授权
- 改动文件：`deploy/apps/30-bff.yaml`、`40-gateway.yaml`、`50-authz.yaml`
- 验证方式：`istioctl analyze -n launchx-mesh` 无 error（实测 Error 0 / Warning 0 / Info 3，三条 Info 均为已知基线 host-middleware）
- 实测补充：
  - **跨命名空间 Gateway selector 生效**：Gateway 在 `launchx-mesh`、`istio: ingressgateway` Pod 在 `istio-system`；apply 后 `istioctl proxy-config routes <gw> -n istio-system` 出现 `http.8080 /api* → bff.launchx-mesh`。绑不上则入口全 404 而 analyze 不报错
  - **Service 端口协议声明**决定入向滤波器（用对照组在跑 Pod 实测）：`http` → `http_connection_manager`，`tcp-rpc` → `tcp_proxy`；协议不在 cluster 里（`proxy-config cluster` 的 TYPE 是发现类型，拿它 A/B 是假阴性）
  - **补了 `ServiceAccount: bff` + `serviceAccountName: bff`**（原模板没有）：AuthorizationPolicy 的 `principals` 认调用方 SPIFFE 身份，缺它则四域对 bff 一律 403 且 analyze 报不出
  - 原模板四条 AuthorizationPolicy **同名**会互相覆盖，已改名 `srv-<域>-from-bff-only`
  - 计划里三条 grep 判据有两条是坏的，已更正（见 superpowers plan Task 13）

### Task 14: 镜像构建与 apps 组部署
- [x] **任务完成**（与 superpowers plan `Task 14`、`tasks.md` 2.4 同步勾选）
- 目标：产出 `launchx-mesh-apps:dev` 并把 apps 组部署进 minikube
- 改动文件：`apps/.dockerignore`（删掉 `bin/` 一行，否则镜像构建必然失败）；其余无代码改动
- 验证方式：五个 Pod `2/2 Running`；`kubectl get deploy -n launchx-mesh` 均可 `available`
- 实测：五个 apps Pod 全部 `2/2 Running`（bff + 四域 srv，另带 consul）；
  四个 srv Endpoints 均有地址；入口复核 `/api/order/2` → HTTP 200 且返回真实订单数据、
  `/api/ping` → 200。构建上下文 417MB / 19s
- 修掉的缺陷：`apps/.dockerignore` 含 `bin/`，与 Dockerfile 的 `COPY bin/ /app/bin/` 直接冲突，
  症状 `ERROR: ... "/bin": not found` 且**不提示 .dockerignore**（对照组 `docker/.dockerignore` 是空文件）

### Task 15: T13 网格入链验证（路由矩阵）
- [x] **任务完成**（与 superpowers plan `Task 15`、`tasks.md` 3.3 同步勾选）
- 目标：逐**路由矩阵**（6 条）拿到「入口 → bff → 目标域业务层」的完整证据，外加异步链一条，并落盘为矩阵
- 改动文件：`examples/mesh/docs/verify.md`（新增 §6 T13，以矩阵表形式）
- 前置依赖：**Task 17 必须先完成**（矩阵判据的脚本由它产出）
- 验证方式：`VARIANT=apps bring-up.sh verify` 输出矩阵表且每行 PASS；异步链 traceID 与 `POST /order` 一致；`verify.md` §T13 含 6 条路由的请求命令原文
- 实测：连跑四次均 `7/7 行通过 / 结论：PASS`（18:56 / 19:02 / 19:02 / 19:10），四次 traceID 各不相同、
  异步链 traceID 每次都与当次 `POST /order` 一致；`grep -cE '(GET|POST) /api/' docs/verify.md` = 10（≥ 6）；
  反例矩阵（把 `/product/:id` 接到 points 域）`rc=1`，判据方向为真
- 修掉的三个真因：① 宿主 Kafka 的 `PLAINTEXT` 监听器 advertised 为 `127.0.0.1:9092`，Pod 里元数据一返回就连自己
  ⇒ 加专供网格的 `MESH://:9094` 监听器（advertised `kafka:9094`）、conf 与 ConfigMap 的 brokers 改 9094、
  `20-host-middleware.yaml` 暴露 9094；② `srv-order` 与 `srv-points` 共用消费组 `mqx-group`，kafka-go Reader
  在**创建时**就占住分区而 order 侧从不读取 ⇒ 两域各配独立 `group_id`；③ 网格下根 span 是 Envoy 的 proxy span，
  路径只在 `http.url` 标签里 ⇒ 判据改按 `http.method` + `http.url`/`http.target` 绑定路由，不再按 span 名
- 修正：落盘小节编号按文件实际结构定为 **§6**（§0 版本口径 / §1 T10 / §2 E5 / §3 T11 / §4 排查记录 / §5 复现命令），
  不是计划里写的「§T13」；标题行与结论行已同步改写

### Task 16: 文档校正
- [x] **任务完成**（与 superpowers plan `Task 16`、`tasks.md` 3.4 同步勾选）
- 目标：把「srv 必须新补 HTTP 探针」这条旧结论改成准确表述，并同步 mesh 顶层 README
- 改动文件：`examples/mesh/apps/README.md`（§4.3）、`examples/mesh/README.md`（§1 目录四处 + §7 核对）
- 验证方式：`grep -nE '必须多写|避免「全部下沉」的错觉' examples/mesh/apps/README.md` 零命中（旧结论措辞已清除）；`examples/mesh/README.md` 目录段的 `deploy/apps` 行不再含「随实现补齐」、`apps/README.md` 说明行改为「两处不可交付项」（与 apps §2 标题一致）、证据落盘范围含 T13；`deploy/apps` 清单集合为 8 个文件（`10-configmap` + `20/21/22/23-srv-*` + `30-bff` + `40-gateway` + `50-authz`），与 spec 表格一致
- 实测：三条「零命中」判据全部零命中（`随实现补齐` / `T10–T12` / `三处不可交付项`），
  两条「命中」判据各命中一次（`两处不可交付项` / `docs/verify.md`）；`grep -nE '必须多写|避免「全部下沉」的错觉'`
  rc=1（旧措辞已清），而「srv 必须新补 HTTP 探针」在校正段落里保留一处作为**推导痕迹**
  （计划已声明这是有意保留，不作失败判据）。另在 §4.3 追加一条同族坑：入口 Service 端口名必须写 `http`，
  写成纯数字会让 Envoy 按 `tcp_proxy` 处理 ⇒ `outlier detection` 与 HTTP 重试静默失效
- 现场校正两处（与计划模板不同，以实际文件为准）：① §7 T13 行的证据列改为 `docs/verify.md **§6**（T13 路由矩阵）`
  —— 计划写的是「§T13」，但 verify.md 既有结构只到 §5，Task 15 实际写入的是 §6；② `deploy/apps` 目录行
  按实测 8 个文件逐条列出（计划只要求去掉「随实现补齐」），并补 `scripts/route-matrix-{apps,micro}.txt` 两行

## Amendments

### 2026-10-07: 入口到各域业务逻辑的完整链条纳入验收范围

- 原因：build 执行到 Task 2 时用户提出「服务网格验收时要验证从入口各种路由到 srv 各个业务逻辑完整的链条」。核对发现三处规格与计划的共同缺口：① bff 只规划了 2 条路由，但建了四域客户端 —— 三个域的客户端零调用点，业务逻辑永远无法从入口触达；② `srv-order` 的 `CreateOrder` 无 RPC 接线，导致 `order → MQ → points` 异步链在入口侧没有触发点；③ T13 判据只打一条 URL，`trace-audit.py --service bff --mesh` 不看是哪个域的 span，接线错误可逃过验收。附带修正计划 Task 6 的一处编译期错误（kitex 生成的 `stock.Stock` 接口要求实现全部 7 个方法，而计划写的是「实现 `Decrease` / `GetStock`」），以及 `mesh-app-handles` 里「自建 cache 句柄被拦下」这条装配期做不到的 Scenario。
- 影响规格：新增 `specs/mesh-entry-chain/spec.md`（5 Req / 9 Scenario）；`specs/mesh-trace-verification/spec.md`（T13 收紧为路由矩阵）；`specs/mesh-app-handles/spec.md`（自检增加有效裁决与证据强度分级；Scenario 由 cache 改为 db）；`specs/mesh-deploy-manifests/spec.md`（入口 1 条 vs bff 6 条的分工；`PROBE_URL` → 路由矩阵）
- 影响任务：`tasks.md` 1.3–1.7 就地扩写（handler 全接口 + bff 六条路由）、3.3 就地扩写（路由矩阵验收）；**追加 `## 4. Amendment` 4.1 / 4.2 / 4.3**
- 就地扩写的既有 Task：`Task 7`（路由 2 → 6 条）、`Task 15`（单路径 → 路由矩阵 + 异步链 + 矩阵证据落盘）
- 执行顺序：**Task 17 → Task 15**（Task 15 的矩阵判据依赖 Task 17 产出的脚本）

### 2026-10-07: `rpc/` 层语义纠正 + 四域分层埋点补齐（Task 3 实现期发现）

- 原因：`srv-order`（Task 3）实现完毕、规格合规审查不通过。问题不在实现，而在规格与计划本身：
  ① `rpc/` 层被写成「跨域调用」（`proposal` §一 树注释、`tasks.md 1.3`、计划 Task 3 Step 4），
  而 `examples/micro` 四域的 `rpc/<域>.go` 全部是**入向传输适配层**，且 launchx 的
  `App.RPCClientOptions()` 只在 bff/gateway 档产出（`internal/mode/webx.go`），srv 档直接返回 error
  —— 按计划写根本取不到出向选项；计划 Task 4/5/6 甚至要求「删除 `rpc/<域>.go`」，属连锁误读。
  ② 四域的**分层埋点整层漏写**：计划 Task 3–7 无任何 `otelx/layer` 步骤，而
  `mesh-entry-chain` 的判据要求每条路由的 trace 内出现目标域 `service.<域>.<Op>` span
  （`rpcx/middleware.go` 注释已写明「服务端 span 由各服务自己的 service 层产出」）——
  照现状实现，Task 15 的矩阵会在每一格 FAIL。
  ③ `mesh-app-layering` 目录树写七个目录（漏 `route/`），实际与计划 Task 10 都是八个。
  ④ `mesh-app-handles` 的「MUST NOT 出现 `gorm.io/gorm`」与 repository 必持 `*gorm.DB`、
  `pkg/handles` 必 import 它探回调打架。
- 影响规格：`specs/mesh-app-layering/spec.md`（树补 `route/` + 八目录职责表 + 四层 span Requirement）；
  `specs/mesh-entry-chain/spec.md`（去掉「内部再调 product/stock」；IDL Requirement 改名为
  「四域 **RPC 实现体**必须实现全部 IDL 方法」；**新增**「跨域编排只在 bff，srv 之间不互调」）；
  `specs/mesh-app-handles/spec.md`（依赖面改「禁建连」口径；矩阵补 srv 不用 `RPCClientOptions()`）
- 影响任务：`tasks.md` 1.3–1.6 就地改写（`rpc` 语义 + 四层 span）、1.7 就地扩写（错误码映射
  补 401/429）、3.1 / 3.2 就地校正判据；**追加 4.4**（`rpc/` 入向适配与分层埋点回填）
- 就地扩写的既有 Task：`Task 7`（错误码映射）、`Task 9` / `Task 10`（判据口径）
- 不改判据的既有 Task：`Task 3` —— 已勾选的 Step 不回退，增补由 Task 18 承担
- 执行顺序：**Task 18 → Task 4 / 5 / 6**（Task 18 为其余三域定下模板）；Task 17 → Task 15 不变

## 追加实现步骤

### Task 17: 入口链条判据矩阵化（trace-audit.py + bring-up.sh）
- [x] **任务完成**（与 superpowers plan `Task 17`、`tasks.md` 4.1 / 4.2 / 4.3 同步勾选）
- 目标：把「入口 1 条 URL → 有 sidecar span 即 PASS」升级为「路由矩阵 × 目标域业务 span」判据，并让 `bring-up.sh verify` 逐路由打请求
- 改动文件：`examples/scripts/trace-audit.py`（新增 `--route-matrix`）、`examples/mesh/scripts/bring-up.sh`（`PROBE_URL` → `PROBE_ROUTES` 六条 + 循环 `mesh_curl`）
- 验证方式：`VARIANT=apps bring-up.sh verify` 输出矩阵表且打请求次数 = 6；构造「`/product/:id` 接到 order 域」的假矩阵时脚本必须 FAIL；`VARIANT=micro` 路径行为不变
- 不做的事：不改动 T10 既有的 M1/M2/M3 三条规则；不给 `micro` 变体加矩阵（它只有 1 条入口路由，对照组的证据链不动）
- 实测：apps 侧连跑五次均 `7/7 PASS`；反例矩阵（`service.points.GetPoints` 挂到 `/api/product/1`）`rc=1`；
  micro 回归（Step 4）仍只打**一条** URL、`1/1 行通过 / 结论：PASS`、
  覆盖跳 `['PassthroughCluster', 'inbound|8080||', 'inbound|8081||']` 与计划期望逐字一致、
  域 span 仍是 `service.order.GetOrder`
- 回归的操作代价（留给后来人）：宿主仅剩 ~1.2 GB 可用，无法 apps 与 micro **并存**跑满。
  做法是先把 apps 五个 Deployment `scale --replicas=0`，再 `VARIANT=micro deploy`，
  verify 完 `delete -R -f deploy/micro` 并把 apps `scale --replicas=1`；
  恢复后重跑一次 apps 矩阵（第五次）仍 7/7，证明确实恢复如初。
  `cmd_deploy` **不会**自动 `down`（两变体清单可并存），所以这一步必须手工腾资源

### Task 18: srv 分层埋点与 `rpc/` 入向适配层回填（四域，order 先做）
- [x] **任务完成**（与 superpowers plan `Task 18`、`tasks.md` 4.4 同步勾选）
- 目标：把 `rpc/` 层按真实语义（入向传输适配）落地，并给四域补齐 `rpc`/`handler`/`service`/`repository` 四层分层 span —— 这是入口链条验收判据（`service.<域>.<Op>` 必须出现在 trace 内）的前置
- 改动文件：`apps/internal/srv/<域>/rpc/<域>.go`（重写为入向适配层）、四域的 `handler`/`service`/`repository`（各补 `layer.Begin`）、四域 `app/setup.go`（`NewServer(rpc.New(...))`，撤销 `rpcAdmit`）、`apps/pkg/codes/codes.go`（补 `Unauthorized` / `RateLimited`）
- 验证方式：`apps/internal/srv/**` 内零出向客户端（`NewClient(` / `client.Option` / `RPCClientOptions(` 在非测试源码零命中）；`rpc`/`handler`/`service`/`repository` 四个**分层包**单测覆盖 ≥ 90%（`app` 装配层以 build/vet 兜底）；一条 trace 内四层 span 按 `rpc→handler→service→repository` 父子链齐全（由 `TestLayerSpanChain` 锁定；`trace.enabled=false` 时四层全不出现）；`Unauthorized` 与 `RateLimited` 各有真实生产者
- 不做的事：不实现 srv→srv 的跨域调用；不改 bff 的六条路由；不改四域的业务判断逻辑（只加埋点与适配层）
