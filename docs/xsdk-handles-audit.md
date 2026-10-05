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
| cachex | ✅ redis 驱动 | **全局** `SetTracingConfig` | 关 | ⚠ 实例级 `enable_trace` 对 span **无效**；launchx 已在 `cache.go:37` 以 `HostProvider:true` 补全局位 |
| mongodbx | ✅ `db.mongodb.<op>` | **实例级** `EnableTrace` | 关 | ⚠ 全局 `SetTracingConfig` / `IsTracingEnabled()` **全库无调用者**（死开关） |
| coordx | ✅ consul 驱动有 `coord.register` / `coord.deregister` | 实例级 `cfg.EnableTrace` | 关 | 后台轮询已做孤儿 span 抑制（`consul.go:176-178`），但 `Deregister` 未见同样保护 ← 即我们见过的那条孤儿 span |
| kvx / lockx / searchx | ✅ 各自驱动 | 实例级 `EnableTrace` | 关 | ⚠ 三者 `Config` 无 mapstructure tag：业务自行 `Unmarshal` 时 `enable_trace` 静默失效（launchx 用逐字段 `Get` 绕开） |
| mqx | ✅ `messaging.kafka.send` 等 | **管不住** | 恒产 span | ⚠ 见下节第 1 条 |
| rpcx | ✅ kitex obs-opentelemetry suite | 实例级 `TraceEnabled`（`trace.enabled`） | 关 | 只从 `otel.GetTracerProvider()` 取用，绝不自建 |
| webx | ✅ 中间件（**只有 Extract 无 Inject**） | `trace.enabled` | 关 | 出向注入要靠业务自己调 `trace.Inject` |

---

## 3. 三个会咬人的点（按危害排序）

### ① mqx：span 无条件产生，违反「`trace.on.*` 只控入链」红线
`mqx/kafka/kafka.go:259`：

```go
func (p *ProducerImpl) Send(ctx context.Context, msg *mqx.Message) error {
    ctx, span := p.tracer.Start(ctx, "messaging.kafka.send")   // 外层无 if EnableTrace
```

`callchain.go:231` 同理。后果：
- `trace.enabled: false`（全局）→ provider 是 noop，span 不导出，**符合预期**；
- 只关 `trace.on.mq: false`、全局仍开 → `EnableTrace` / `CallChainEnabled` 被压成 false（它们只管日志与内存调用链条目），**span 照产照报**。

属 mqx 侧缺陷，launchx 拦不住（span 在 SDK 内部产生）。需要彻底关只能关全局或用 `traceflag.SetComponent("mqx", false)`（该名字与 launchx 的 `trace.on.mq` 不是同一个键位，见 ③）。

### ② cachex / mongodbx：同一个 SDK 里"实例级"与"全局"两个开关，只有一个有效
- cachex 只看全局（`redis.go` 的 `startSpan` 查 `IsTracingEnabled()`），实例级 `EnableTrace` 存了从不读；
- mongodbx 正好相反：只看实例级，全局 `SetTracingConfig` 是死开关。

两边都得写，写少了不报错。

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
- `internal/component/mq.go`：把 mqx 无条件 span 的偏差写成注释（含触发条件与"只能关全局"的规避口径），避免下一个人再把它当 launchx 的 bug 查一遍。
