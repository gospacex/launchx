# hubx x 系列 SDK：句柄与调用链封装审计

审计日期：2026-10-05 · 针对 `launchx` 当前 main（含 `dec3c62`）· 只读勘察 + 关键项逐条复核

回答两个问题：
1. launchx 初始化时是否把 hubx 下各 `x` 后缀 SDK 都初始化好、拿到**全局句柄**供项目直接用？
2. 这些 SDK 是否已封装调用链（OTel tracing）、开关怎么裁决？

---

## 0. 结论先行

1. **不是"全局句柄"，是"实例 + App 访问器"**。进程级全局只有两个：`otel` 的 TracerProvider + 传播器、以及 `logx` 全局 logger。其余 SDK 都是「构造实例 → 挂 `component.Env` → 业务用 `App.Xxx()` 取」。
2. **能用，但必须走 `App` 访问器**。`cachex` / `mongodbx` / `mqx` 自带包级全局单例（`cachex.Instance()`、`mongodbx.Instance()`、`mqx.GetSDK()`），而 launchx 调的是 `New(...)` 不是 `Init(...)`，**这些全局访问器在 launchx 应用里恒为 nil**。直接调会拿到 nil 且不报错。
3. **调用链都已封装，但开关形态不统一**，且存在 3 个"配了不生效也不报错"的点（下表标 ⚠）。

---

## 1. 句柄是怎么交到业务手里的

```
stepComponents  →  component.Mount(Env, componentOrder)  →  各 build* 构造实例写回 Env 字段
                →  业务在 OnSetup 里用 App.Xxx() 取
```

装配顺序（`app.go:34`，固定 8 个，无 objectx / vectorx）：

```
db → cache → mq → kv → lock → registry → search → mongodb
```

### App 访问器清单（`app.go`）

| 方法 | 返回类型 | 能否直接操作 | 边界 |
|---|---|---|---|
| `DB()` | `any`（实为 `*gorm.DB`） | ✅ | 必须断言：`a.DB().(*gorm.DB)`；失败要当装配失败处理 |
| `Cache()` | `cachex.Instances`（`map[string]Cache`） | ✅ | 只有接口方法，**拿不到底层 `*redis.Client`**（刻意收窄依赖面） |
| `MQ()` | `mqx.Producer` | ✅ | 只有生产者；关闭时 producer 仍会构造（开关不是构造开关） |
| `KV()` | `kvx.Stores`（`map[string]KVStore`） | ✅ | Get/Set/Delete/Exists/Keys/MGet/MSet/TTL/Incr/Decr |
| `Lock()` | `lockx.Locker` | ✅ | **必须判空**：为 nil 时分布式锁退化成无锁路径，超卖不报错 |
| `Search()` | `searchx.SearchClient` | ✅ | ES 客户端，可直接 Index/Get/Search/Delete/Bulk |
| `Mongo()` | `mongodbx.Instances`（`map[string]DB`） | ✅ | 集合句柄建议装配期取好，别拖到首次写 |
| `Registry()` | `coordx.Register` | ✅ | 合成句柄，**同时具备发现能力**（`dec3c62` 修的就是这个） |
| `Discover()` | `coordx.Discover` | ✅ | 按服务名解析上游实例 |
| `Engine()` / `RPCServerOptions()` / `RPCClientOptions(svc)` | `any` | ✅ | 按 mode 产出，形态不匹配时为 nil |
| `Tracer()` | `otelx.Provider` | ✅ | 业务自埋点用 `a.Tracer().TracerProvider().Tracer(name)` |
| `Config()` / `Logger()` / `Mode()` | — | ✅ | — |

**没有句柄的两类**：`objectx`（对象存储）、`vectorx`（向量库）—— 本轮不接（`openspec/.../design.md:57`），业务需自行 `New`。

---

## 2. 调用链封装矩阵

裁决链：`traceflag` 总闸（默认开） AND `trace.on.<组件名>`（**未配置即 false**） AND 实例级开关。
组件名取自 `componentOrder`：`db / cache / mq / kv / lock / registry / search / mongodb`。

| SDK | 是否内建 tracing | 真正生效的开关 | 默认 | 备注 |
|---|---|---|---|---|
| dbx | ✅ GORM 回调（`db.query` 等带完整 SQL） | `trace.on.db` → `orm.Open` vs `orm.OpenNoTrace` | 关 | ⚠ SDK 自己的 `enable_trace` 被 launchx 丢弃（`db.go:19` 丢弃第二个返回值） |
| cachex | ✅ redis / memcached 驱动 | 实例级 `EnableTrace` **或** 全局 `SetTracingConfig` | 关 | 2026-10-06 前实例级对 span **无效**（见下节第 ② 条）；现已修为三层裁决 |
| mongodbx | ✅ `db.mongodb.<op>` | **实例级** `EnableTrace` | 关 | ⚠ 全局 `SetTracingConfig` / `IsTracingEnabled()` **全库无调用者**（死开关） |
| coordx | ✅ consul 驱动有 `coord.register` / `coord.deregister` | 实例级 `cfg.EnableTrace` | 关 | 后台轮询已做孤儿 span 抑制（`consul.go:176-178`），但 `Deregister` 未见同样保护 ← 即我们见过的那条孤儿 span |
| kvx | ✅ 各自驱动 | 实例级 `EnableTrace` | 关 | ⚠ `Config` 仍无 mapstructure tag：业务自行 `Unmarshal` 时 `enable_trace` 静默失效（launchx 用逐字段 `Get` 绕开） |
| lockx / searchx | ✅ 各自驱动 | 实例级 `EnableTrace` | 关 | `Config` 的 yaml/json/mapstructure 三套 tag **2026-10-06 已补齐**（原状同上） |
| mqx | ✅ `messaging.kafka.send` 等 | `trace.on.mq` → `EnableTrace \|\| CallChainEnabled` | 关 | 2026-10-05 前恒产 span，已修（见下节第 ① 条） |
| rpcx | ✅ kitex obs-opentelemetry suite | 实例级 `TraceEnabled`（`trace.enabled`） | 关 | 只从 `otel.GetTracerProvider()` 取用，绝不自建 |
| webx | ✅ 中间件（**只有 Extract 无 Inject**） | `trace.enabled` | 关 | 出向注入要靠业务自己调 `trace.Inject` |

---

## 3. 三个会咬人的点（按危害排序）

### ① mqx：span 曾无条件产生（**2026-10-05 已修**）
原状：五个驱动（kafka / nsq / pulsar / rocketmq / rabbitmq）一律 `otel.Tracer(...)` 直接 `Start`，外层无 `EnableTrace` 包裹。`trace.on.mq=false` 只压住 `EnableTrace` / `CallChainEnabled`（它们只管日志与内存调用链条目），**span 照产照报** —— 违反「`trace.on.*` 只控入链」的红线。

修法（改在 SDK 内，launchx 拦不住）：

```go
tracer: traceflag.NewTracer(cfg.EnableTrace || cfg.CallChainEnabled,
                            "github.com/gospacex/mqx/kafka", "mqx"),
```

`traceflag.Tracer.Start` 在开关关闭时返回 `(ctx, noop.Span{})`，且仍受全局总闸与 `SetComponent("mqx", false)` 约束。字段类型同步从 `trace.Tracer` 改成 `*traceflag.Tracer`（otel 1.29 的 `trace.Tracer` 含未导出方法，外部类型实现不了 —— 这也是 `testTracer()` 这类测试 helper 要跟着改返回类型的原因）。

回归测试 `mqx/kafka/tracer_gate_test.go` 同时钉住两条：开关开则入链、开关关则不入链，且**两种情况下 producer 都建得出来**（开关不是构造开关）。

### ② 开关的"有效层级"各 SDK 不一样（cachex 已修，mongodbx 仍是坑）

- **cachex 曾只看全局**（`redis.go` 的 `startSpan` 查 `IsTracingEnabled()`），实例级 `EnableTrace` 存了从不读 —— 只配 `instances.<name>.enable_trace` 的调用方 span 恒为零，且不打任何日志。2026-10-06 已修：新增 `Client.traceEnabled()`，裁决顺序统一为
  `traceflag 总闸 AND 组件闸("cachex") AND (实例级 OR 全局)`，
  memcached 驱动同改。回归用例在 `cachex/redis/tracer_gate_test.go`（五条）。
- **mongodbx 正好相反**：只看实例级，全局 `SetTracingConfig` / `IsTracingEnabled()` **全库无调用者**（死开关）。

写之前先确认这一层，写少了不报错。

> 附带一条：`cachex.SetTracingConfig` 内部会 `traceflag.Apply("cachex", cfg.Enabled)`，
> **顺带把组件闸也写成同一个值**。想模拟「全局关、实例开」必须先
> `SetTracingConfig(false)` 再把组件闸单独拨回 `true`，顺序反了会被覆盖
> —— 写测试时极易踩。

### ③ 命名空间分裂：launchx 组件名 ≠ SDK traceflag 组件名
launchx 侧是 `cache / mq / mongodb / registry …`，SDK 侧是 `cachex / mqx / mongodbx / coordx …`。
yaml 里写 `trace.on.<launchx 名>`，代码里 `traceflag.SetComponent("<SDK 名>")` —— 两个键位互不通用，配错无提示。

---

## 4. 给四微服务（商品 / 订单 / 库存 / 积分）的落地检查清单

每个服务的 `config.yaml` 与装配层逐条对齐：

1. **`trace.on.*` 必须显式写全**（未配置 = false = 不上链）：
   ```yaml
   trace:
     enabled: true
     endpoint: "127.0.0.1:4318"     # 网格形态换成集群内 OTLP 地址
     on: { db: true, cache: true, mq: true, kv: true, lock: true, registry: true }
   ```
2. **取句柄一律走 `App` 访问器**，不要 `cachex.Instance()` / `mongodbx.Instance()` / `mqx.GetSDK()`（恒 nil）。
3. **库存扣减必须 `a.Lock()` 判空**：nil 时要么 fail-fast（`strict.lock: true`），要么显式 Warn——不能静默走无锁路径。
4. **多表 CRUD 用同一个 `App.DB()` 句柄**（事务在同一连接池里才有意义）；跨服务的一致性走 outbox + MQ。
5. **自埋点用 `a.Tracer().TracerProvider().Tracer("<业务名>")`**，不要自建 provider（会顶掉宿主的）。
6. **启动期播种/预热要临时关对应组件的埋点**（`traceflag.SetComponent` + 还原），否则留下孤儿单 span trace，Jaeger 里最容易被读成"链路断了"。

---

## 5. 本轮库侧改动

- `internal/component/mongodb.go`：注释校正 —— 原注释称"实例级 enable_trace 不存在、必须走 SetTracingConfig"，与代码不符（实例级才有效，全局是死开关）。
- `internal/component/mq.go`：记录 mqx 无条件 span 的历史偏差与修法（含"launchx 侧不要另加拦截"的结论）。
- `mqx`（五个驱动 + kafka 回归测试）：span 改由 `traceflag.NewTracer` 按开关产出，恢复「`trace.on.*` 只控入链」红线。

---

## 6. 版本与 tag（2026-10-06 现状）

行为改动必须落到 tag 上，否则外部依赖方拿到的仍是旧行为（本仓各模块之间
靠 `replace` 指向本地路径，所以**仓内**不受影响，坑在仓外）。

| 仓库 | 模块 | tag | 指向 | 含的改动 |
|---|---|---|---|---|
| mqx | `mqx` / `kafka` / `nsq` / `pulsar` / `rabbitmq` / `rocketmq` | `v0.1.0` 及 `<子模块>/v0.1.0` | `4e22bba` | span 改由 traceflag 裁决 |
| cachex | `cachex` / `redis` / `memcached` | `v0.1.0` 及 `redis/v0.1.0`、`memcached/v0.1.0` | `40bb0bd` | 实例级开关生效 |
| lockx | `lockx` / `redis` | `v0.1.0` 及 `redis/v0.1.0` | `b3a9f95` | 三套 tag 补齐 |
| searchx | `searchx` / `elasticsearch` | `v0.1.0` 及 `elasticsearch/v0.1.0` | `f62601c` | 三套 tag 补齐 + LegacyCluster |

子模块是**独立 go.mod**，必须各自打 `<子模块名>/v0.1.0`；只打根模块的
`v0.1.0` 对子模块无效（Go 的模块版本按模块路径解析）。

四个仓库的所有 `v0.1.0` tag 均已对齐到各自 `main` 的 HEAD（未推送，可安全
`git tag -f` 重指）。**均未 push** —— 推送属对外动作，须人工确认。
