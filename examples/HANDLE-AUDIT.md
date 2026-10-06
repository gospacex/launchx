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

`otel` 全局的取用只出现在**装配层的显式回落点**（tracer/provider 为 nil 时降级，
共 3 处，均有注释说明），业务分层里为零：

| 位置 | 回落条件 |
|---|---|
| `micro/srv/internal/tracex/tracex.go` | `UseProvider` 未注入（telemetry 未启用） |
| `mall/pkg/tracex/tracex.go` | 同上 |
| `micro/gateway/internal/proxy/proxy.go` | `App.Tracer()` 为 nil |

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
| 1 | `micro/srv/internal/tracex`、`mall/pkg/tracex` | `Tracer()` 直取 `otel.GetTracerProvider()` 全局单例 | 新增 `UseProvider(tp)`，由 `app.Setup` 调 `tracex.UseProvider(a.Tracer().TracerProvider())` 交接；未注入时回落全局（保住既有测试） |
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
