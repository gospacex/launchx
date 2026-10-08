# examples 句柄取用审计（2026-10-05）

一次全量核查：三个形态的案例（单体 / 微服 / 网格）以及 mall 四服，是否都
**经 launchx 装配产出的 SDK 句柄**操作中间件，而不是自建客户端或直取全局单例。

## 1. 判据

| 层级 | 要求 |
|---|---|
| 装配层 `app/setup.go` | 句柄只能来自 `a.DB()` / `a.Cache()` / `a.MQ()` / `a.Search()` / `a.Mongo()` / `a.KV()` / `a.Lock()` / `a.Registry()` / `a.Discover()` / `a.Engine()` / `a.Tracer()` / `a.RPCServerOptions()` / `a.RPCClientOptions()` |
| 业务分层 | 不出现 `gorm.Open` / `redis.NewClient` / `kafka.NewWriter` / `consul.NewClient` 等自建客户端；不出现 `otel.GetTracerProvider()` / `otel.GetTextMapPropagator()` 等直取全局单例（走 `launchx.Inject` / `launchx.Extract`，tracer 由装配层交接） |
| 地址 | 代码内不出现 `127.0.0.1` / `localhost`，一律来自配置 |

红线自检命令（排除 `_test.go`，测试里的 sqlite 内存库是已登记的豁免）：

```bash
cd examples
grep -rnE "gorm\.Open|redis\.NewClient|kafka\.(NewWriter|NewReader)|consul\.NewClient|mongo\.Connect|elasticsearch\.NewClient|sql\.Open" \
     --include="*.go" --exclude="*_test.go" .      # 必须为空
grep -rn "127\.0\.0\.1\|localhost" --include="*.go" . | grep -v kitex_gen | grep -v _test   # 必须为空
```

`otel` 全局的取用分两类，**业务分层里两类都不出现**：

- **SDK 内部取全局**（合规，与 dbx 的 `otel.Tracer(ormTracerName)` 同一条路）：
  `otelx/layer` 向 `otel.GetTracerProvider()` 要 tracer。全局由 launchx 的
  telemetry 步骤在装配最前注册好（`internal/telemetry/telemetry.go:55`），
  业务侧不需要、也不应该再交接一次 provider。
- **装配层的显式回落点**（仅剩 2 处）：

| 位置 | 回落条件 |
|---|---|
| `micro/srv/internal/tracex/tracex.go` | 拆分前形态的**冻结样例**，刻意保留原样，不参与验收 |
| `micro/gateway/internal/proxy/proxy.go` | `App.Tracer()` 为 nil |

> 2026-10-07 更正：本表原列 `mall/pkg/tracex/tracex.go` 的 `UseProvider` 回落点 ——
> 该包已并入 `otelx/layer`（见 §5），交接机制一并删除。

## 2. 配置段 ↔ 句柄取用矩阵

| 案例 | 配置的组件段 | 代码取的句柄 | 结论 |
|---|---|---|---|
| **monolith**（单体） | `sqlx` `cachex` `mqx` `searchx` `mongodbx` | `a.DB` `a.Cache` `a.MQ` `a.Search` `a.Mongo` `a.Engine` `a.Tracer` | ✅ 五个组件全覆盖，无遗漏无多余 |
| **micro/bff** | `coordx` | `a.Engine` `a.RPCClientOptions` `a.Tracer` `a.Mode` | ✅ 下游发现经 launchx 产出的 kitex 选项，业务不碰 consul |
| **micro/srv** | `sqlx` `cachex` `mqx` `coordx` | `a.DB` `a.MQ` `a.Cache` `a.RPCServerOptions` `a.Tracer` | ✅（本轮补 `a.Tracer` 交接，见 §3） |
| **micro/gateway** | `coordx` | `a.Engine` `a.Registry` `a.Tracer` `a.Config` | ✅（本轮订正过时注释，见 §3） |
| **mesh**（网格） | `configmap.yaml`：`coordx` / `sqlx` | 无 Go 代码 | ✅ 复用 micro 三档二进制，差异只在配置副本与部署清单 |
| **mall ×4** | `sqlx` `cachex` `mqx` `kvx` `lockx` `coordx` | `a.DB` `a.Cache` `a.KV` `a.Lock` `a.MQ` `a.RPCServerOptions` `a.Tracer` | ✅（本轮补 `a.Tracer` 交接，见 §3） |

注册/注销一律由 launchx 按 `service.mode` 自己完成，任何 demo 都不调
`Register()` / `Deregister()`；monolith 不配 `coordx` 段（单体不注册）。

## 3. 本轮修的问题

| # | 位置 | 问题 | 处理 |
|---|---|---|---|
| 1 | `micro/srv/internal/tracex`、`mall/pkg/tracex` | `Tracer()` 直取 `otel.GetTracerProvider()` 全局单例 | 新增 `UseProvider(tp)`，由 `app.Setup` 调 `tracex.UseProvider(a.Tracer().TracerProvider())` 交接；未注入时回落全局（保住既有测试）**— 已于 2026-10-07 撤销，见 §5** |
| 2 | `micro/srv/internal/service/{order,outbox}.go`、`mall` 四服 service、`mall/pkg/outbox` | 业务层裸调 `otel.GetTextMapPropagator().Inject/Extract` | 改走 `launchx.Inject` / `launchx.Extract` |
| 3 | `launchx/trace.go` | `Inject/Extract` 只收 `propagation.HeaderCarrier`，消息侧（MapCarrier）被迫裸调全局 | 入参放宽为 `propagation.TextMapCarrier`（`HeaderCarrier` 本身实现它，既有调用方兼容） |
| 4 | `micro/gateway/app/setup.go` | 注释称"`Registry()` 断言会失败走直连兜底"——实际 `App.Registry()` 返回的是合成了注册+发现的 `registryHandle`，断言会成功 | 订正注释，说明真正的降级条件（注册中心未装配） |
| 5 | `launchx/go.mod` | `cachex` / `cachex/redis` 的 `require` 被删（只剩 replace），根模块编译失败 | 补回两条 require（`go build -mod=mod` 顺带解析 indirect 版本） |

## 4. 遗留

- **端到端复测未完成**：审计收尾时 Docker Desktop 已退出（8500 / 3306 / 6379 / 9200 /
  27018 / 16686 全部不可达），micro 三服与 mall 无法起链验证。已完成的是全模块
  `go build` 与单测（`internal/component`、`examples/micro/srv/internal/{rpc,service}`
  全绿）。恢复容器后需重跑一次链路验证。
- 期间 `TestTraceOffStillConstructsKV` / `TestLockConstructedWithRetryConfig` 曾报 FAIL，
  实为 Docker 退出途中 redis 半连通导致 kvx/lockx 构造失败；容器彻底停止后这两个用例
  按设计 `Skip`（`requireRedis`），非代码回归。
- `examples/mall` 尚未纳入 git，本次改动随它一起留在工作区。

## 5. 后续变更：tracex / outbox 合并为共享包（2026-10-07）

审计当时把「业务埋点封装」和「事务发件箱」留在 `examples/{micro,mall}/pkg/` 下各一份。
这次复查发现副本**已经漂移**，故合并 —— 但两者的落点不同，理由也不一样：

| 原位置 | 现位置 | 依据 |
|---|---|---|
| `examples/micro/pkg/tracex`、`examples/mall/pkg/tracex` | `github.com/gospacex/otelx/layer` | 只依赖 otel 核心三项，与 dbx / mqx 同域；落 otelx **子包**而非根包 —— 根包要链 configx + 三个 exporter + logx/zap，而只想要这个封装的服务不该被拖上 |
| `examples/micro/pkg/outbox`、`examples/mall/pkg/outbox` | `github.com/gospacex/launchx/examples/pkg/outbox` | **不能进 launchx 库源码**：本包整个就是对一个 gorm 表做簿记（`*gorm.DB`、`gorm.Expr` 自增），而库源码有硬约束 —— `dependency_surface_test.go` 的 `TestLibrarySourceHasNoDirectMiddlewareImports` 禁止直引 gorm（实测放进 `launchx/outbox` 后该守卫立即失败）。也不能进 mqx：mqx 不依赖 gorm，把 ORM 拖进消息层是同一个错误的反向版。而 dbx / mqx 谁都不单独拥有「事务发件箱」（一半是与业务行同事务的落库，一半是 MQ 的可靠投递）—— 这种跨域粘合是应用层的活，故合并成 examples 树内的一份 |

**副本账（合并前）**：tracex 三份（micro/pkg、mall/pkg、monolith/internal）+
micro/srv/internal 的冻结副本；outbox 两份 + micro/srv/internal/service 的冻结副本。
合并后除冻结样例 `micro/srv` 外只剩一份。

**漂移实测（合并的直接理由）**：`micro/pkg/outbox` 与 `mall/pkg/outbox` 当时只差 1 行
import 路径（暂时安全）；但 `mall/pkg/tracex` 缺 `Begin` / `End`（比 micro 少 41 行），
mall 四服 repository 仍是旧的 `defer end()`（product repo 8 处）——同一仓库里跑着两套
埋点语义。这正是 outbox 包自己第一段注释写的「抄多份的代价不是代码量，而是语义漂移」。

顺带做的三件事：

1. **删掉 `UseProvider` 交接机制**（撤销 §3 第 1 条）。它把 provider 存进包级变量，
   是 SDK 不该有的可变全局；而且在当前装配下**是冗余的** —— launchx 的 telemetry
   步骤已经无条件 `otel.SetTracerProvider(p.TracerProvider())`，回落全局拿到的就是
   同一个 provider（测试注入内存 exporter 也一直走的是全局这条路）。删掉后
   `otelx/layer` 与 dbx / mqx 的取用方式一致，`app.Setup` 里那 8 处交接一并移除
   （第 9 处在冻结样例 `micro/srv` 里，未动）。
2. **`tracerName` 改为模块路径约定**（原硬编码 `"mall"`，四个 micro 服务全报 mall）。
3. **mall 四服补齐错误标记**：12 个 `begin` 辅助函数改为调 `layer.Begin`，
   70 处收尾调用改成「命名返回值 + `defer func() { end(err) }()`」，与 micro 对齐。

刻意不动的两处：

- `examples/micro/srv/` —— 拆分前形态的冻结样例，自带 `internal/tracex` 与
  `internal/service/outbox.go` 两份旧实现，不参与验收，**未跟进**。
- `examples/monolith/internal/tracex` —— 该目录当时正由**另一个会话**改造
  （同期在加 `Attrs` / `BeginSpan` 两个 API，`cache` / `search` 两个适配层正在改调用点，
  期间 `go build ./examples/monolith/...` 处于在飞状态）。本次**主动放弃**对单体的改动，
  已回退唯一一处侵入（`internal/repository/order.go`），留给那个会话收口。
  若将来要收敛，`otelx/layer` 目前是「取全局 provider」形态，单体那种「tracer 显式注入」
  的变体需要再加一个入口。


