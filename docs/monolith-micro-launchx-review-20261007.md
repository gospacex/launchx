# 单体 / 微服 两例：launchx 使用一致性 + 企业上线标准核对

核对时间：2026-10-07；工作树含**未提交**改动（micro 四域切 `otelx/layer` 的迁移正在进行中），
本文结论基于工作树当前内容，非 HEAD。

核对基线：`gofmt -l` 空、`go build ./examples/monolith/... ./examples/micro/...` 通过、
`go vet` 同范围通过（`CGO_ENABLED=0`，go1.24.3）。

---

## 一、launchx 使用与风格一致性

### 1.1 入口 main.go：一致（无需改）

7 个入口逐字同构，只有包注释里的域/档名不同：

| 入口 | 形态 |
|---|---|
| `examples/monolith/cmd/main.go` | monolith |
| `examples/micro/srv-{order,product,stock,points}/cmd/main.go` | srv ×4 |
| `examples/micro/bff/cmd/main.go` | bff |
| `examples/micro/gateway/cmd/main.go` | gateway |

共同骨架（`configPath = "config.yaml"` 常量 → `launchx.Bootstrap` → `a.OnSetup(app.Setup)`
→ goroutine 跑 `a.Run()` → `signal.Notify(SIGINT, SIGTERM)` → `select` → `a.Close()`）：

```go
a, err := launchx.Bootstrap(configPath)
if err != nil { log.Fatalf("bootstrap: %v", err) }
a.OnSetup(app.Setup)

runErr := make(chan error, 1)
go func() { runErr <- a.Run() }()

stop := make(chan os.Signal, 1)
signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
select {
case sig := <-stop:  /* ... a.Close() */
case err := <-runErr: /* ... a.Close() */
}
```

三个纪律都被守住：入口不开连接、不装配 exporter、不注册节点；信号退出显式走 `App.Close`
（否则最后一批 span 来不及冲刷）；`select` 而非 `switch`（Go 不允许在 `case` 里带短变量声明）。

`examples/micro/srv/cmd/main.go`（冻结的拆分前对照）也是同构的，符合"只冻内容、不冻骨架"。

### 1.2 装配层 app/setup.go：骨架一致，两处需收敛

**一致的部分**（两边都做到）：

- 签名统一 `func Setup(a *launchx.App) error`，失败即返回 error（装配期报错好过半装配起出去）。
- 句柄一律经 `App` 取：`a.DB()` / `a.Cache()[name]` / `a.KV()[name]` / `a.Lock()` / `a.MQ()` /
  `a.Search()` / `a.Mongo()[name]` / `a.Engine()` / `a.RPCServerOptions()` /
  `a.RPCClientOptions(name)` / `a.Registry()`；DB 一律断言 `*gorm.DB`，拿不到即报错。
- 均不含形态分支（不出现 `monolith`/`bff`/`gateway` 取值），换形态只改 `service.mode`。
- 组件缺失一律 **显式 Warn + 降级**，不静默跳过。
- 均不自己注册/注销注册中心（注册由 launchx 按 mode 做）。

**差异 1（需收敛）：句柄来源校验的覆盖面差一档**

| | 包 | 覆盖 |
|---|---|---|
| monolith | `internal/handlesrc.VerifyOrError(a)` | db / cache / kv / mq / search / mongodb **六类**，逐类记 `present/want/observed/probe` |
| micro | `pkg/handlecheck.VerifyDBX(db)` | **仅 db** |

两份判据同源（`handlesrc.go` 头部注释已写明"examples 之间互不引用，故判据是复制而不是引入依赖"，
这是可接受的刻意重复）。但 micro 的 cachex / kvx / mqx / lockx 四类句柄**没有任何装配期校验** ——
"拿自考句柄冒充"在 micro 里是静默的。

**差异 2（需收敛）：tracer 取用口径残留旧形态**

- monolith：完全不碰 tracer，`app/setup.go:60` 只取 `traceOn := cfg.GetBool("trace.enabled")`，
  分层 span 一律由 `otelx/layer` 向 otel 全局要。✅
- micro 四域：已切到 `layer.Begin(..., traceOn, quiet)`。✅
- micro **bff / gateway 仍在传 tracer 对象**：
  - `bff/app/setup.go:111-124`：`tp := otel.GetTracerProvider()`，若 `a.Tracer() != nil` 则
    `tp = tr.TracerProvider()`，然后 `tp.Tracer("downstream")` / `tp.Tracer("bff")` 注入。
  - `bff/internal/handler/mall.go:35`、`bff/internal/downstream/clients.go:41`：`tracer trace.Tracer` 字段。
  - `gateway/internal/proxy/proxy.go:118/127`：`tracer trace.Tracer` 字段 + `New(..., tp trace.TracerProvider)`。

  这与既定的"全生态向 otel 全局要 tracer"口径冲突（`otelx/layer.Tracer()` 就是每次向全局现取、
  不缓存）。`a.Tracer()` 这条交接路径本该留给"业务自建 provider"的例外场景，不是常态。

### 1.3 内部分层：主体已统一，五处分叉

分层结构（两者骨架同形，micro 多一节 rpc/route）：

```
monolith : middleware → handler → service → repository  (+ cache/search/docs/messaging 适配层, + route)
micro/srv: rpc → route → middleware → handler → service → repository → model
```

**已统一的部分（✅）**：`handler` / `service` / `repository` 三层，两边写法完全一致——

```go
func (x *X) begin(ctx context.Context, op string) (context.Context, layer.End) {
    return layer.Begin(ctx, "<layer>", "<entity>", op, x.traceOn, quiet)
}
// 调用点：
ctx, end := x.begin(ctx, "<Op>")
defer func() { end(err) }()   // 前提：返回值命名
```

span 名三段式 `<layer>.<entity>.<op>`，属性走 `layer.Attrs(ctx, ...)`，
`quiet` 豁免非故障错误（持久层 `ErrNotFound`）。

**分叉 1（真缺陷，建议修）：micro 的 rpc 层未接 `traceOn` 门控**

- `srv-order/internal/rpc/order.go:34,48,62,76`（product / stock / points 同）：
  `ctx, span := layer.Start(ctx, "rpc", "order", "CreateOrder")` —— `layer.Start` 不接
  `traceOn` 参数，"有全局 provider 就开"。
- 后果：`trace.enabled=false` 时，kitex 的 server span 中间件被关
  （`internal/mode/srv.go:28` `TraceEnabled: h.Cfg.GetBool("trace.enabled")`），
  但 rpc span 照开 → 它成为**无父根 span** → 导出成一根孤儿 trace。
  这正是前几轮刚治理掉的孤儿 trace 问题的同一成因。
- 对照：`pkg/routing/routing.go:232,248` 的 route / middleware span **有** `if r.traceOn` 门控；
  monolith 全层走 `Begin` 门控。唯独 rpc 漏了。
- micro 也没有 monolith 那条 `TestDegradeTraceDisabledServesWithoutSpans` 的反向断言，所以测不出来。

**分叉 2**：bff / gateway 仍是旧形态（见 1.2 差异 2）。

**分叉 3（可接受，保留）**：`examples/micro/srv/` 是刻意冻结的拆分前对照，保留
`internal/tracex` 与 `internal/service/outbox.go`，不参与验收、默认不启动。**不要改成 layer。**

**分叉 4（两边都犯，建议修）：业务内部层直引 `github.com/gospacex/launchx`**

- `examples/monolith/internal/messaging/order.go:14`
- `examples/micro/srv-order/internal/service/order.go:36`（product/stock/points 同）

两处都只为用 `launchx.Inject(ctx, carrier)`。但 `micro/srv/internal/tracex/tracex.go:34`
自己写着"业务分层拿不到 `*launchx.App`（**分层边界要求它不认识框架**）"—— 自相矛盾。
传播入口应下沉到 `pkg/` 级的中立 helper，或由 SDK（mqx / 一个 propagation 包）承接。

**分叉 5（冗余，建议删）：monolith 手写 MQ header 注入**

`internal/messaging/order.go:71` 手工 `launchx.Inject(ctx, propagation.HeaderCarrier(headers))`
填进 `msg.Headers`。但 `mqx/kafka/kafka.go:274` 的 `Send` 内部已 `mqx.InjectTrace(ctx, msg)`（覆盖语义），
`pulsar` / `rabbitmq` / `rocketmq` 同。这段注入是重复的（同 ctx → 同 traceparent，结果无害但多余），
且违反"业务侧不要手写 header"的既定口径。

micro 侧不手写：`srv-order/internal/service/order.go:367` 的 `traceparentOf` 只把 traceparent
取出来**存进 outbox 行**（DB 列），那是另一回事，正确。

**附带（小）：注释陈旧**
`monolith/internal/messaging/order.go:54` 仍写"沿用两段式 `messaging.<Op>` 命名"，
代码已是三段式 `messaging.order.PublishOrder`。改名时漏了注释。

### 1.4 结论

> 入口与装配层的 launchx 使用**风格一致**，骨架、纪律、降级姿态三边对齐。
> 内部分层的主体（handler/service/repository）**已统一到 `otelx/layer`**。
> 残留五处分叉：**1 处是真缺陷**（micro rpc 未接 `traceOn`，会产孤儿 trace），
> **1 处是覆盖面缺口**（micro 句柄校验只验 db），
> **2 处是待迁移的旧形态**（bff/gateway 传 tracer；两例内部层直引 launchx），
> **1 处是冗余 + 1 处注释陈旧**，另 1 处（冻结 srv）刻意保留。

---

## 二、企业上线标准核对

口径：`examples/ACCEPTANCE.md` §13.4 已登记的观测基线 + 通用 Go 服务上线清单。
**"企业上线"按"能被运维接管、出事能排障、能回滚"判**，不按"demo 能跑通"判。

### 2.1 逐项表

| # | 维度 | monolith | micro | 判定 |
|---|---|---|---|---|
| 1 | 配置外置（无硬编码地址/端口） | ✅ 全在 config.yaml | ✅ 六份 config.yaml | **PASS** |
| 2 | 密钥外置 | ✅ `${HUBX_DB_PW}` | ✅ 同 | **PASS**（见 §2.2-1） |
| 3 | 优雅退出（信号 + Close 链） | ✅ | ✅ 且 outbox `OnStop` 先于组件停止 | **PASS** |
| 4 | HTTP 健康探针 | ✅ `/healthz/live`、`/healthz/ready` | gateway/bff ✅；**四域 srv ❌ 无 HTTP 面**（靠 consul TTL） | **srv 档 FAIL** |
| 5 | 限流 | ✅ 全局 hertz 中间件 + webx rps/burst | ✅ 每方法固定窗口（kvx `Incr` 原子），fail-open | **PASS**（fail-open 需评审，见 §2.2-2） |
| 6 | 鉴权 | ✅ token 中间件（缓存校验 + 播种） | ❌ 只有 `user_id > 0` 的"身份存在性"校验，**不是鉴权** | **FAIL** |
| 7 | 幂等 / 并发互斥 | ✅ `uk_order_no` | ✅ 同 + `lockx` 同用户下单互斥（TTL 兜底） | **PASS** |
| 8 | 链路追踪 | ✅ 三段式、全层门控、孤儿 0 | ⚠️ 主体一致，rpc 层缺口（§1.3 分叉 1） | **修后 PASS** |
| 9 | 日志-链路关联 | ✅（webx 访问日志带 trace_id） | ✅ | **PASS** |
| 10 | metrics / Prometheus | ❌ | ❌ | **FAIL**（全仓缺口，已记 SKIP） |
| 11 | pprof | ❌ | ❌ | **FAIL**（规格已立项未实现） |
| 12 | 单测 | ✅ 19 个（降级 6 + 链路 5 + 句柄 4 + e2e 4） | ❌ **四域零单测**；只有 `trace_e2e_test.go` 3 个 + 冻结 srv 的 12 个 | **FAIL** |
| 13 | 容器镜像 | ❌ 无（`examples/docker/Dockerfile` 属 mesh/micro 旧档） | ⚠️ 镜像只装 `demo-gateway`/`demo-bff`/`demo-srv` 三个二进制，**不含四个域服**；`bin/` 被 gitignore，不可重现 | **FAIL** |
| 14 | CI | ❌ 无 `.github/workflows` | ❌ 同 | **FAIL** |
| 15 | schema 版本化 / 迁移 | ❌ schema.sql 在脚本里，无版本号 | ❌ 同 | **FAIL** |
| 16 | 传输加密 | ❌ 全明文（mysql/kafka/redis/consul/ES/mongo） | ❌ 同 | 内网可暂缓 |
| 17 | fail-fast vs 降级策略 | ✅ `strict.db` + 各组件 Warn | ✅ 同 | **PASS** |
| 18 | 运行手册 | ✅ README + ACCEPTANCE | ✅ README + `docs/verify-chain.md` | **PASS** |
| 19 | 告警阈值 / SLO | ❌ 无 | ❌ 无 | **FAIL** |
| 20 | 安全：SQL 注入 / 越权 | ✅ gorm 参数化 | ✅ 参数化；但**无鉴权即无越权边界**（与 #6 同源） | 见 #6 |

### 2.2 需要澄清的几个"灰度项"

1. **`demo.auth_token: "demo-token"` 明文写死在 config.yaml**（monolith:53）。
   demo 里是刻意的（注释写明"生产里这活属于签发服务"）。但**上线清单里必须显式替换**，
   否则等于一个永不过期、写在配置里的万能令牌。
2. **限流 fail-open**（micro `internal/middleware/ratelimit.go:51`；monolith 同姿态）。
   姿态本身有注释辩护（"把中间件的故障放大成整个域的不可用"更糟），技术上站得住。
   但**上线评审需要明确接受**：KV 挂掉的那一刻限流是失效的，且只有一条 Warn 日志、无告警。
3. **`trace.enabled` 与 `trace.on.*` 两套开关**：前者管业务分层 span，后者管组件 span。
   语义正确，但运维文档里必须写明"关 `trace.enabled` 不等于全链路关"，
   否则会被误当总闸用（micro 的 rpc 缺口正是这个误区的产物）。
4. **micro 无 `/metrics`、无 pprof**：按 ACCEPTANCE §13.4 记 SKIP 是诚实的，
   但 SKIP ≠ 可上线。这两项在上线清单里是硬缺口，不是"以后再说"。

   > **2026-10-08 状态更新**：`/metrics` 已补齐（`internal/telemetry.NewMetrics` +
   > `rpcx` 指标中间件 + `webx` 采集口注册，单体与微服均真机复验通过；网格列只备了
   > 清单、未起集群验）。**pprof 仍未做**，这一条对 pprof 依然成立。
   >
   > 顺带修掉一处会让人误判"已经好了"的隐患：两个耗时直方图原先用 OTel 缺省分桶
   > （`[0,5,10,...,10000]`，毫秒形状）却声明单位 `s`，结果所有请求挤进 `le="5"`
   > 一个桶 —— 端点 200、计数器也对，只有 p99 是废的。已改显式秒级分桶 + 防回归用例。

### 2.3 上线前阻塞项（按优先级）

| 优先级 | 项 | 位置 | 说明 |
|---|---|---|---|
| P0 | micro 补鉴权 | bff / gateway / 四域入口 | 现在任何能连到 9021-9024 / 8080 / 8081 的人都能下单、改库存、加积分 |
| P0 | micro 补单测 | `srv-*/internal/**` | 四域承载全部写路径（事务 + outbox + 锁），零单测不可接受；monolith 的 19 个可作模板 |
| P0 | metrics | 全生态 | 上线无指标 = 无法设告警、无法回滚判据 ~~（2026-10-08 已补：单体 + 微服真机复验，网格待验）~~ |
| P1 | rpc 层接 `traceOn` | `srv-*/internal/rpc/*.go` | 一行改动，但会产孤儿 trace |
| P1 | srv 档健康探针 | 四域 config / k8s 清单 | 至少给 `tcpSocket`；consul TTL 不是 k8s 探针 |
| P1 | CI + 镜像 + schema 版本化 | 仓库根 / `examples/docker` | 三个都要补；镜像须含四个域服二进制且 `bin/` 由 CI 产出 |
| P2 | 内部层去掉 launchx 直引 | 两例 internal | 传播入口下沉到 `pkg/` |
| P2 | 删 monolith 冗余注入 + 修陈旧注释 | `messaging/order.go` | 见 §1.3 分叉 5 |
| P2 | micro 句柄校验扩到 cache/kv/mq/lock | `pkg/handlecheck` | 对齐 monolith 的六类覆盖 |
| P2 | bff / gateway 切 `otelx/layer` | `bff/**`、`gateway/**` | 去掉 `trace.Tracer` / `TracerProvider` 字段 |

### 2.4 一句结论

> **作为 launchx 的示例：两边都是合格的**（入口/装配/分层/降级/链路口径一致且纪律硬）。
> **作为要上生产的服务：都不合格** —— monolith 缺 metrics/镜像/CI/测试覆盖外的全部工程件；
> micro 在此基础上还缺**鉴权**与**四域单测**两个 P0。
> 差距不在"用没用 launchx"（这块用得规整），而在"示例之外的工程配套一件都没有"。

---

## 三、修复进度（2026-10-08）

§2.3 的表逐项落地后的实际状态，以及**本轮新发现的两条** —— 它们不在原表里，因为原表
是静态核对看不出来的（都是运行期行为）。

### 3.1 原 P0 / P1 / P2 的落地

| 项 | 状态 | 落点 |
|---|---|---|
| micro 补鉴权（边界验证 + RPC 身份透传） | ✅ | `examples/micro/pkg/auth/*`（Identity/TokenStore/Verifier/Gate） + 四域 `route.NewRouter(…, gate, …)`，链序 `audit → auth → ratelimit`；bff `installAuth` + `authSkipPaths`；`ordercli` / `chain.sh` / `trace_e2e_test.go` 带鉴权头 |
| micro 补单测 | ✅ | `pkg/auth`（10 例）+ `pkg/routing`（鉴权在限流前、审计留痕）+ 四域 `internal/rpc`（各 3 例：traceOn 零 span / 有 span / 鉴权拒 ≠ 限流码） |
| metrics | ⏸ **登记为 SDK 侧缺口**，本轮不硬凑端点 | 并行会话正在 `internal/mode` 做 `MetricsEndpoint`（截至本次核对处于编译不上的中间态） |
| rpc 层接 traceOn | ✅ | 四域 `rpc.Server{…, TraceOn}`，`begin` 接 `layer.Begin(…, traceOn, nil)`；`trace.enabled=false` 不再产孤儿 trace |
| 拒绝码区分 | ✅ | `rejected()` 按 `AbortedBy == routing.MiddlewareNameAuth` 给 `CodeUnauthorized(1004)`，否则 `CodeRateLimited(1005)` |
| srv 健康探针 | ✅ | `examples/micro/deploy/micro.yaml`：gateway/bff `httpGet /healthz/live`，四域 `tcpSocket`（9021-9024） |
| CI + 镜像 + schema 版本化 | ✅ | `.github/workflows/ci.yml` + `examples/scripts/ci.sh` + `fetch-sdks.sh`；`build-bin.sh` 交叉编译六个二进制；`schema.sql` 加版本头与 `schema_migrations` |
| 内部层去 launchx 直引 | ✅ | 收口到 `examples/micro/pkg/traceparent`，四域 `service` 不再 import launchx/propagation |
| 删冗余注入 + 修注释 | ✅ | `monolith/internal/messaging/order.go`（mqx 已自动 InjectTrace） |
| 句柄校验扩面 | ✅ | `pkg/handlecheck/verify.go` 五类（db/cache/kv/mq/lock），`want = trace.enabled && trace.on.<comp>` |
| bff / gateway 切 layer | ✅ | 去掉 `TracerProvider` 字段，改 `layer.Start` + `trace.enabled` |

### 3.2 本轮新发现（原表没有，都是运行期才暴露）

**① P0 —— 退出链无界：SIGTERM 后进程退不出来**

- 现象：monolith E1 用例全包跑时 `SIGTERM 后 20s 内没有退出`（单跑绿，极易被当成偶发放过）。
- 根因链：`component.buildMQ` 在**构造期**就建 consumer ⇒ `kafka.NewReader{GroupID}` 立刻起
  `ConsumerGroup` 协程；该协程阻塞在 `joinGroup` 的网络读上，而 kafka-go 的 `connDeadline.value`
  为零值时 `SetReadDeadline` **等于不设** ⇒ 退出时 `Reader.Close → ConsumerGroup.Close → wg.Wait()`
  永久等待。**一个从没消费过的消费者句柄让整个进程退不出去。**
- 修法：不给驱动逐个打补丁（打不完），改在退出链统一兜 —— `App.Close` 的每一环经 `runStop`
  有界执行（上限 `service.stop_timeout`，缺省 5s）；超时 Warn + 记事件，**不翻退出码**
  （翻了会把「某驱动 Close 慢」升级成 crashloop）。配套 `AddStop` 的 name 不再丢弃 +
  新增 `App.OnStopNamed`。
- 回归：`TestCloseNeverHangsOnBlockedStopHook`（卡死的回调不拖住 Close）
  / `TestStopHookErrorStillAggregated`（真实 error 照样进聚合）。

**② P1 —— 降级只在 span 留痕，trace 一关就静默**

`monolith/internal/service/order.go` 的 `mark()` 原只在 span 上写 event；span 不录时
（`trace.enabled=false`，为控成本的生产常态）直接 return ⇒ 「事件没投出去 / ES 没索引 /
快照没存」在线上**查无可查**。已改：Service 持 logger，`mark` 先 Warn 再写 span，日志带 `order_no`。

**③ P1 —— 消费组全挤在默认名 `mqx-group`**

`mqx.FillDefaults` 在 `group_id` 缺省时统一填 `"mqx-group"` ⇒ 订阅同一 topic 的服务互相抢分区，
表现为「消息投出去了但没人收到」。已给 monolith + micro 四域显式 `consumer.group_id: demo-<svc>`。

**④ P1 —— 门禁脚本自己跑不过（macOS bash 3.2 的多字节陷阱）**

`examples/scripts/ci.sh` 首跑在 `line 98: m?: unbound variable` 崩掉。根因不是变量没赋值：
**bash 3.2 不做多字节识别**，`echo "…（独立模块 $m）"` 里的 `（` 字节被并进变量名 ⇒
`set -u` 下报 unbound 且名字带乱码，**不带 `-u` 则静默展开成空串**。
同一坑 `examples/micro/scripts/e2e.sh:18` 早有记录，但只有那一个脚本守住了约定。
已修：`ci.sh`（2 处）+ `mesh/scripts/bring-up.sh`（1 处）改用 `${name}`；
并在门禁里**加了一步静态检查**（扫 `examples/**/*.sh` 中裸 `$var` 紧跟非 ASCII），
让这条约定由门禁守住而不是靠记性。

### 3.3 门禁结果

`bash examples/scripts/ci.sh`（全量：monolith + micro + mall + pkg + 独立模块）→ **门禁 PASS**。

| 范围 | 结果 |
|---|---|
| 根包 `go test . ./internal/...` | ✅（含新增两条退出链用例） |
| `examples/monolith`（全包真机，167s） | ✅ |
| `examples/micro/...` + `examples/pkg/...` | ✅（auth / routing / 四域 rpc） |
| `examples/mall/...` | ✅ build + vet |
| `examples/mesh/apps`（独立 module） | ✅ build + vet + test（补进 `ci.sh` 的 `extra_modules` 后才被扫到） |

mesh 这一项此前是 ⚠️（`internal/mode` 指标端点处于编译不上的中间态），并行会话落地后转绿。

### 3.4 真机验收（本轮已跑通）

| 验收 | 结果 |
|---|---|
| `bash examples/micro/scripts/e2e.sh`（六进程全链） | ✅ rc=0：consul 五实例 passing、`/api/ping` 通、`/api/order/1001` 返回真实订单（带 `Bearer`）、指标三断言全过 |
| `bash examples/micro/scripts/auth-probe.sh`（鉴权反向探针，本轮新增） | ✅ rc=0：无凭据 401 / 错令牌 401 / 正确令牌 200 / `/api/ping` 免鉴权 200 |

**为什么补 `auth-probe.sh`**：e2e.sh 只验「带对凭据能通」，那是**正向**判据 ——
一个把鉴权装上去却始终放行的实现照样能过（中间件没挂上、或 `Engine.Use` 晚于路由注册，
表现都是「谁都能访问」）。鉴权这种东西正向通过不算数，必须有一条「没凭据会被挡」的断言，
否则上线就是裸奔而门禁还是绿的。脚本自起最小三进程，且**借来的 8081 一定还回去**
（占用者是 etcdkeeper 的宿主映射，trap 里无条件 `docker start` 回来）。

### 3.5 仍挂着的（不属于本轮）

- **monolith 的 mongo 快照偶发未写入**（`docx: document not found`，全包跑出现过一次，
  单跑与后续全包均绿）。已在用例失败分支附进程日志尾部，下次复现可直接看 Warn 而非症状。
- 进程退出时偶见 `registry deregister failed 404 Unknown service ID`：注册是**启动时一次性**
  且失败降级为带病运行（MEMORY §十一），注销时实例本就不在 catalog 里。症状无害但要留意
  —— 它同时也是「这一轮压根没注册上」的提示，排查时先看 consul catalog 再看 Pod。
- 改动量大且混有并行会话的未提交迁移，**尚未提交**。
