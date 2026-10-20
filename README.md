# launchx

> 微服务启动骨架 SDK —— 一次装配，四档形态。业务只写业务。

[![examples-ci](https://github.com/gospacex/launchx/actions/workflows/ci.yml/badge.svg)](https://github.com/gospacex/launchx/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.27.1-00ADD8?logo=go&logoColor=white)](https://go.dev/dl/)
[![Module](https://img.shields.io/badge/module-github.com%2Fgospacex%2Flaunchx-blue)](https://github.com/gospacex/launchx)
[![License](https://img.shields.io/badge/license-internal-lightgrey)](#许可证)

`launchx` 不是一个框架，也不是一套目录规范。它只负责一件事：**把一个 Go 服务从进程启动到优雅退出的全过程标准化**——配置、可观测性、组件句柄、服务端装配、注册发现、退出顺序——然后把这些句柄交到你的 `OnSetup` 回调里。

你的业务代码只依赖 `launchx` 和语义层，**不直接引入 gorm / go-redis / kafka-go / hertz / kitex**。

---

## 目录

- [为什么需要它](#为什么需要它)
- [30 秒上手](#30-秒上手)
- [核心概念](#核心概念)
  - [生命周期](#生命周期)
  - [四档形态](#四档形态)
  - [句柄面](#句柄面)
- [配置参考](#配置参考)
- [可观测性](#可观测性)
- [仓库结构](#仓库结构)
- [示例与验收](#示例与验收)
- [本地开发与门禁](#本地开发与门禁)
- [设计红线](#设计红线)
- [故障排查](#故障排查)
- [文档索引](#文档索引)
- [许可证](#许可证)

---

## 为什么需要它

一个 Go 微服务的 `main.go` 里通常塞着这些东西：读配置、连数据库、建 Redis 池、起 HTTP 引擎、装配日志与链路、注册到 Consul、等信号、逐层关连接。这段代码**每个服务都写一遍、每次都写得不完全一样**，而且三类问题几乎必然出现：

| 常见故障 | 根因 |
|---|---|
| 换中间件要改业务代码 | 业务直接 `import` 了 `go-redis` / `gorm`，句柄类型散落各处 |
| 换部署形态要改业务代码 | 单体 / 微服 / 网关的装配分支渗进了业务层 |
| 优雅退出变成 SIGKILL | 退出链不完整或某一环无超时，进程卡死不退 |
| 「配了但没生效」查不出来 | 组件构造失败被静默吞掉，链路少一段而无人告警 |
| 埋点接不进去 | 业务自己造连接，绕过了 SDK 的 instrument 回调 |

`launchx` 把这五件事一次性收走：**句柄收敛、形态收敛、顺序收敛、失败姿态收敛、埋点入口收敛**。

---

## 30 秒上手

```go
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/gospacex/launchx"
	"your/module/app"
)

func main() {
	// 1. 引导：读配置 → telemetry → logx → mode 装配 → 组件挂载 → 注册
	a, err := launchx.Bootstrap("config.yaml")
	if err != nil {
		log.Fatalf("bootstrap: %v", err)
	}

	// 2. 业务装配：取句柄 → 组分层 → 挂路由（必须早于 Run）
	a.OnSetup(app.Setup)

	// 3. 起服（阻塞）
	runErr := make(chan error, 1)
	go func() { runErr <- a.Run() }()

	// 4. 等信号，走显式退出链（否则最后一批 span 来不及冲刷）
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-stop:
		log.Printf("shutdown: signal %s received", sig)
		if err := a.Close(); err != nil {
			log.Printf("close: %v", err)
			os.Exit(1)
		}
	case err := <-runErr:
		if err != nil {
			log.Printf("run: %v", err)
			_ = a.Close()
			os.Exit(1)
		}
		_ = a.Close()
	}
}
```

`app.Setup` 里只做两件事——**取句柄、接分层**：

```go
func Setup(a *launchx.App) error {
	db, ok := a.DB().(*gorm.DB)
	if !ok || db == nil {
		return errors.New("db component is unavailable, check xmiddleware.sqlx")
	}
	engine, ok := a.Engine().(*webx.App)
	if !ok || engine == nil {
		return errors.New("webx engine is unavailable in this mode")
	}

	repo := repository.New(db, a.Config().GetBool("trace.enabled"))
	svc := service.New(repo, a.Logger())

	// 路由必须挂完再 Run —— 路由树起服后只读
	return RegisterRoutes(engine.Engine, handler.New(svc))
}
```

注意这段 `Setup` 里**没有任何形态分支**：它不知道自己是单体还是微服。换形态只改 `config.yaml` 里的一行。

---

## 核心概念

### 生命周期

启动骨架是**固定 6 步**，顺序是框架契约，业务不能插队：

| # | 步骤 | 常量 | 职责 | 失败姿态 |
|---|---|---|---|---|
| 1 | 配置加载 | `StepConfigLoad` | 本地 YAML 兜底；`configcenter.type` 非 `local` 时叠加远端源，远端按 key 覆盖本地 | **即退** |
| 2 | 遥测初始化 | `StepTelemetryInit` | 建 `otelx` Provider 与 MeterProvider，注册为 otel 全局 | **即退** |
| 3 | 日志初始化 | `StepLoggerInit` | 构造 logx 并设为全局，业务可在任意包 `logx.GetGlobal()` | **即退** |
| 4 | 形态装配 | `StepModeAssemble` | 按 `service.mode` 装配服务端与客户端/发现工厂 | **即退**（含非法值与 `mesh`） |
| 5 | 组件挂载 | `StepComponentMount` | 按依赖图（wire 求解）构造各 SDK 组件 | `strict.<name>` 裁决 |
| 6 | 注册 | `StepRegistryRegister` | `bff` / `srv` 注册自身实例 | `strict.registry` 裁决 |

**任一步失败都会走完整反序退出链**再返回错误——`Bootstrap` 里连回收阶段的错误都用 `errors.Join` 带出来。理由：*地基失败不允许「带到运行时再炸」，而「回收了但不知道回收得干不干净」同样是有信息量的。*

退出链是**显式 8 环、反序、每环有界**：

| # | 环节 | 说明 |
|---|---|---|
| 1 | 业务 `OnStop`（逆序） | 此时基础设施仍可用 |
| 2 | `server.stop` | webx `Shutdown` / kitex `Stop`；**从未起服则为 no-op** |
| 3 | `metrics.listener` | 仅 srv 档自建的采集口；**必须晚于服务端停止**，否则停机过程自身的指标被提前掐断 |
| 4 | `component.stop`（逆序） | 先起的后停 |
| 5 | `registry.deregister` | 只注销本进程**注册成功**过的实例；必须早于 tracing 冲刷 |
| 6 | `tracing.shutdown` | **独立的 15s 上限**——批量导出器缓冲未冲刷会静默丢最后一批 span |
| 7 | `metrics.shutdown` | 先刷聚合再关日志 |
| 8 | `logger.close` | 刻意吞错：它是记录前面所有失败的唯一手段 |

每一环的等待上限取 `service.stop_timeout`（缺省 5s）。**超时只 Warn + 记 `stop.timeout.<name>` 事件，不进错误聚合**——退出链的契约是「在限定时间内收尾，然后必须让进程退出」，把超时算成 error 会把「某个驱动的 Close 慢了」升级成「进程退出失败」，在 Job / CI 里直接变 crashloop。

### 四档形态

`service.mode` 四档之间**只差「服务端 + 服务发现」的装配方式**：

| mode | HTTP 入口 | RPC 服务端 | 服务发现 | 注册自身 | 典型进程 |
|---|:---:|:---:|:---:|:---:|---|
| `monolith` | ✅ | — | — | — | 单体 API |
| `bff` | ✅ | — | ✅ 发现下游 | ✅ | 聚合层 |
| `srv` | — | ✅ | — | ✅ | 领域服务 |
| `gateway` | ✅ | — | ✅ 发现上游 | — | 自建反代网关 |

几点容易踩的：

- **`mesh` 不是第五档。** 服务网格与微服走**完全相同**的 Go 代码路径，差异只在部署清单与地址副本。`ParseMode` 显式拒绝 `mesh` 并引导到 `srv` / `gateway` / `bff`。
- `Run()` 按 `srv` 优先取服务端：`monolith`/`bff`/`gateway` 用 `App.Engine()`，`srv` 用业务经 `SetRPCServer` 回存的 kitex server。两者都没有则**返回 nil 并 Warn**（后台型服务，例如纯消费者）——「Run 立刻返回」不该被当成「起来了」。
- `gateway` 不注册自身：它是流量入口，注册只会让注册中心多一批无人调用的条目。
- `App.RPCClientOptions(service)` 只在 `bff` / `gateway` 可用，其余档返回带合法形态列表的 error，而不是一份没有目标的空选项。

### 句柄面

`App` 暴露的是**只读句柄**，业务不自己造连接：

| 配置段 | 底层 SDK | 访问器 | 定位 |
|---|---|---|---|
| `xmiddleware.sqlx` | dbx (gorm) | `App.DB()` | 关系库 |
| `xmiddleware.cachex` | cachex | `App.Cache()` | **可失效**缓存（TTL 对象缓存、读穿透） |
| `xmiddleware.kvx` | kvx | `App.KV()` | 键值**状态**（幂等键、原子计数） |
| `xmiddleware.mqx` | mqx | `App.MQ()` / `App.MQConsumer()` | 消息生产 / 消费 |
| `xmiddleware.lockx` | lockx | `App.Lock()` | 分布式锁 |
| `xmiddleware.searchx` | searchx | `App.Search()` | 全文检索 |
| `xmiddleware.mongodbx` | mongodbx | `App.Mongo()` | 文档库 |
| `xmiddleware.coordx` | coordx | `App.Registry()` / `App.Discover()` | 注册与发现 |
| — | webx | `App.Engine()` | HTTP 引擎（`srv` 档为 nil） |
| — | — | `App.RPCServerOptions()` / `App.RPCServer()` / `App.SetRPCServer()` | RPC 服务端 |
| — | — | `App.Config()` / `App.Logger()` / `App.Tracer()` / `App.Events()` / `App.Graph()` | 骨架产物 |

> **`Cache` 与 `KV` 为什么分开？** 两者常指向同一个 Redis，但语义不同：缓存被逐出是可接受的（回源即可），计数被逐出等于限流**静默失效**。混用一个句柄就分不清这两种后果。此外 `cachex` 没有 `Incr`，用它做计数只能读-改-写，并发下会**系统性低估**（lost update）。

> `App.Graph()` 不是给业务的，是给排查用的：返回 wire 求解产出的注册面快照，每个句柄带 `Name()` / `Constructed()` / `TraceEnabled()`，一次回答「构造了没有」「开没开入链」，不必翻日志也不必猜哪个 YAML 段没生效。

组件启用是 **presence-based**——配了段才构造。失败姿态由 `strict.<name>` 裁决：`true` 即退；缺省降级为带病运行，但**必须打一条带 `consequence` 与 `hint` 的 Warn**（静默降级会把「配了段却没接上」变成无声失败）。

---

## 配置参考

单一 YAML，`Bootstrap(path)` 的入参。完整样例见 [`examples/monolith/config.yaml`](examples/monolith/config.yaml)。

### 顶层

| 键 | 类型 | 说明 |
|---|---|---|
| `service.name` | string | **必填**。服务名 = Jaeger service 名 = 各组件实例名的取值键 |
| `service.mode` | string | **必填**。`monolith` / `bff` / `srv` / `gateway` |
| `service.addr` | string | 监听地址（`webx.addr` 未配时回落到它） |
| `service.self.addr` | string | 注册地址 ≠ 监听地址时显式声明（优先级最高） |
| `service.upstream.<svc>` | string | 注册中心不可用时**唯一的**直连兜底目标 |
| `service.stop_timeout` | duration | 退出链每环上限，缺省 `5s` |
| `configcenter.type` | string | 缺省 `local`；`nacos` 等则叠加远端源 |
| `configcenter.<src>.data_id` | string | 缺省 `hubx.<service.name>` |
| `configcenter.<src>.server_addr_env` | string | 缺省 `HUBX_NACOS_SERVER` |

> 自身地址的取值顺序是 `service.self.addr` → `service.addr` → `webx.addr`，三次读取都支持 `${ENV}` 展开。**空 host 会被拦下**：`:8080` 是合法的监听写法，但作为注册地址没有意义——注册出去之后上游解析到空主机，实例 ID 拼成 `svc--8080`，而日志里「service registered」照常打印，症状要等到调用侧 502 才暴露。

### `webx`（HTTP 档）

| 键 | 说明 |
|---|---|
| `addr` | 监听地址 |
| `timeout` | 请求超时 |
| `rate_limit_rps` / `rate_limit_burst` | 入口令牌桶限流 |
| `cors_origins` | CORS 白名单 |
| `health_prefix` | 健康检查前缀，缺省 `/healthz` |

> 默认中间件链：`recovery → otel → logging → ratelimit → cors → timeout`，业务中间件追加在链尾。⚠️ `Engine.Use` **必须早于**路由注册。

### `trace` / `metrics` / `logx`

| 键 | 说明 |
|---|---|
| `trace.enabled` | 业务**分层** span 总闸 |
| `trace.endpoint` | OTLP/HTTP 出口（Jaeger 等）。**span 出口只看这里** |
| `trace.on.<comp>` | 组件级 span 开关：`db` / `cache` / `kv` / `mq` / `search` / `mongodb` / `registry` |
| `trace.sampler` / `sampler_ratio` / `sampler_parent` | 采样策略，缺省 `always_on` |
| `metrics.enabled` | 指标总闸。关掉则 meter 退回 noop：不产生指标、不留采集口，但 Instrument 调用点仍在 |
| `metrics.addr` | **srv 档专有**：没有 HTTP 面，指标需独立采集口 |
| `logx.level` | 日志级别 |
| `logx.otel.enable_trace` | 日志侧的组件 span 开关 |

> `logx.otel.exporter` / `logx.otel.endpoint` 是**死配置**——装配路径从不读它们，span 出口由 `trace.endpoint` 决定。已从全仓 conf 与 ConfigMap 中删除，别再写回去。

### `strict`

`strict.<name>: true` 让某个组件构造失败时**启动即失败**而不是降级。典型用法是 `strict.db: true`——数据库连不上就启动失败，而不是降级成「起来了但落不了库」。

---

## 可观测性

### Span 命名

统一 `<layer>.<entity>.<op>`：

| 来源 | span 名 |
|---|---|
| dbx (gorm) | `db.query` / `db.create` / `db.update` / `db.delete`（带完整 SQL；连表走同一回调，无漏埋） |
| cachex (redis) | `db.redis.<CMD>` |
| kvx | `db.kvx.<op>` |
| lockx (redis) | `lock.redis.acquire` / `release` / `renew` |
| mqx (kafka) | `messaging.kafka.send`（消费侧无 driver span，只有 `ExtractTrace`） |
| outbox | `messaging.outbox.send` |
| 业务分层 | `<layer>.<entity>.<op>`（如 `route.order.CreateOrder`、`handler.bff.<Op>`） |

跨进程续链有两条路：HTTP 走 `webx` 的 otel 中间件自动 `Extract`；**出向注入**用 `launchx.Inject(ctx, carrier)`，MQ 的 header 注入由 mqx 自动完成，业务不手写 header。

### 双开关

`trace.enabled` 管**业务分层** span，`trace.on.<comp>` 管**组件** span——两者各管一段，不要互相顶替。关掉任一项，对应层的 span 必须消失；**关了还看得见，说明埋点没接进统一开关**。

### 指标落点

- HTTP 档（`monolith` / `bff` / `gateway`）挂到 webx 的 `MetricsPrefix`，**复用主端口**，并豁免入口限流。
- `srv` 档走独立的 `metrics.addr` 采集口。**开了指标却没配地址 → 装配期即退**，避免「指标被采集但永不暴露」这类面板空着而进程一切正常的静默故障。
- srv 的 RPC 口没有 HTTP 面。若你对它 `curl` 得到 `KITEX: invalid payload`，**那是反证**，说明端口是对的。

> ⚠️ **OTel 直方图默认桶是毫秒形状**：声明 `WithUnit("s")` 却沿用默认桶，会让所有观测值挤进 `le="5"`——端点 200、计数器正常，**只有分位数全废**。本项目一律显式 `metric.WithExplicitBucketBoundaries(...)`（回归测试 `TestDurationBucketBoundsAreSecondsScale`）。

### debug（观测侧伪组件）

`debug.enabled: true`（yaml）或 `APP_DEBUG=1`（环境变量）任一为真时，起一个仅监听 `127.0.0.1:<debug.port>`（缺省 6061）的 pprof 服务。三条纪律与组件层一致：presence-based、端口冲突降级为带病运行（Warn 不阻断）、退出钩子注册在组件停止相（先于 logger 关闭）。它不产出业务句柄，故不入 wire 图，由 `App.stepComponents` 在图求解后直接调用。

---

## 仓库结构

```
launchx/
├── launchx.go                      Bootstrap —— 唯一引导入口
├── app.go                          App 句柄面 / Run / Close（退出链编排）
├── trace.go                        Inject / Extract —— 跨进程续链
├── internal/
│   ├── pipeline/                   启动骨架与退出链（事件、幂等、错误聚合）
│   ├── configsrc/                  本地 YAML + 远端配置源（nacos 等）
│   ├── telemetry/                  otelx Provider 与 Prometheus MeterProvider
│   ├── mode/                       四档形态装配（webx 引擎 / kitex 选项 / 发现工厂）
│   ├── component/                  各 SDK 组件构造（presence + strict 裁决）
│   └── wiring/                     wire 依赖求解（组件顺序由入参依赖推导）
├── testdata/                       12 份配置夹具（各形态 / 各类失败姿态）
├── examples/                       四套可运行样板（见下节）
├── docs/                           评审与审计文档
└── openspec/                       规格变更制品（sddflow）
```

**库本体（不含 `examples/`）60 个 Go 文件、约 9.3k 行**，根包 49 条测试。`examples/` 另有 323 个文件、约 68k 行——它们的定位是**验收样板**，不是库的一部分。

---

## 示例与验收

| 目录 | 形态 | 进程 | 关键验收 |
|---|---|---|---|
| [`examples/monolith`](examples/monolith) | 单体 | 1（:8080） | E1 起服、T2–T5 数据层入链、T13 调用链完整性 |
| [`examples/micro`](examples/micro) | 微服 + 自建网关 + Consul | 6（gateway :8080 / bff :8081 / 四域 :9021-9024） | E2 三段同 trace、T6、E3 srv 迁移回归 |
| [`examples/mall`](examples/mall) | 微服（端口段 92xx / 91xx） | 6（srv-gateway :9200 / srv-bff :9201 / 四域 :9101-9104） | 与 micro 同口径 |
| [`examples/mesh`](examples/mesh) | 服务网格（Istio） | `apps` 五进程（bff + 四域）；入口交 Istio Gateway | 路由矩阵 8 行 |

`examples/micro/srv/` 是**冻结的拆分前对照**，不参与验收、不启动——它的存在是为了证明「换形态不改业务代码」可复现。

网格示例（`examples/mesh`）另有几条必须知道的**协议事实**：

- **`bff→srv` 这一跳永远不会有 sidecar span。** Kitex 走 TTHeader over TCP，Envoy 按 `tcp_proxy` 处理，只看得见连接看不见请求。这是协议事实，不是清单漏配。
- **「治理已下沉网格」是虚假陈述。** 准确表述：网格提供 mTLS + 连接级可观测 + HTTP 跳的 L7 治理；RPC 跳的熔断 / 重试 / 负载均衡与全部企业级治理（业务限流、幂等、事务、配置分发）**仍在应用侧**。
- **唯一的反向项**：Prometheus 采集口必须排除 sidecar 入向拦截（`traffic.sidecar.istio.io/excludeInboundPorts: "90xx"`）。因为四域各有一条 `action: ALLOW` 的 `AuthorizationPolicy` 只放行 `sa/bff`，ALLOW 语义下未匹配即拒绝，而 Prometheus 不在网格里、没有 SPIFFE 身份 ⇒ 一律 403。
- 端口名方向相反：**入口 Service 必须是 `http`**（写成纯数字会让 Envoy 退回 `tcp_proxy`，`outlier detection` 与 HTTP 重试一起**静默失效**）；**采集口必须是 `tcp-metrics`**（写 `http-metrics` 会被 L7 接管，正好撞上那条 ALLOW 策略）。

`examples/mesh` 另有更详细的两链形态说明：[`examples/mesh/apps/README.md`](examples/mesh/apps/README.md)。

---

## 本地开发与门禁

### 前置：必须在 hubx 工作区内

`go.mod` 的 `replace` 指向 `../<sdk>` 兄弟目录（`cachex` / `configx` / `coordx` / `dbx` / `kvx` / `lockx` / `logx` / `mqx` / `otelx` / `rpcx` / `searchx` / `traceflag` / `webx` / `mongodbx` / `hubx`）。**脱离工作区单独 clone 本仓无法构建。**

CI 用 [`examples/scripts/fetch-sdks.sh`](examples/scripts/fetch-sdks.sh) 把这些兄弟仓克隆到仓库目录之外。少了这一步，go 会去拉 GitHub 上的上游版本——**编译的是另一份代码，而 CI 依然是绿的**。

### 门禁

```bash
# 顺序有依赖：gofmt → build → vet → test
bash examples/scripts/ci.sh              # 全量
bash examples/scripts/ci.sh monolith     # 只跑一个示例
bash examples/scripts/ci.sh mesh         # 含独立 module examples/mesh/apps

# 库侧的硬守卫（任何环境都必须出结论）
go test . -count=1
```

`examples/mesh/apps` 是**独立 module**，主模块的 `./...` 覆盖不到它——脚本用 `extra_modules` 单独收，否则就是「门禁绿了，而那个模块根本没被编译过」。

真机用例（MySQL / Redis / Kafka / Jaeger / ES / Mongo）在没有底座时会自行 Skip。起底座前记得导环境变量：

```bash
export HUBX_DB_PW=hubx123
export PATH=$HOME/sdk/go1.24.3/bin:$PATH   # 本机 Go 工具链
```

> 已知的本机端口冲突（Docker Desktop 转发，`ps` 里看不到进程）：`etcdkeeper` 占 8081（撞 bff）、`elasticsearch` 占 9200（撞 mall 的 srv-gateway）。起服前 `docker stop`，跑完 `docker start` 回来。

---

## 设计红线

这四条是仓库的**不变量**，都有自动化守卫：

**1. 依赖面隔离。** 库源码（根包 + `internal/**`）不得直接 import 任何中间件驱动。
守卫：`TestLibrarySourceHasNoDirectMiddlewareImports`（逐个检查 gorm / cloudwego / redis / kafka-go / consul 等）。跨仓能力落 `*_x` 仓——RPC 治理在 `rpcx`，指标中间件在 `webx`。**新增顶层包后必须重跑这三条测试。**

**2. 地基失败不降级。** 配置路径错、`service.name` 缺失、形态非法——一律启动期即退。不允许「带到运行时再炸」。

**3. 降级必须留痕。** 降级不能只落 span（`trace.enabled=false` 时 span 不录，降级就彻底静默了），必须 Warn 且带 `consequence` 与 `hint`。

**4. 地址一律来自配置。** 示例的业务 Go 代码里不出现任何硬编码回环地址——同一份二进制与一份配置副本才能被网格直接复用。
守卫：`TestServiceSourceHasNoHardcodedLoopbackAddr`、`TestServiceSourceHasNoDirectMiddlewareSetup`、`TestEveryServiceDirIsGuarded`。

此外还有两条布局不变量：业务分层**不得出现 mode 分支**（反向断言见 [`examples/ACCEPTANCE.md`](examples/ACCEPTANCE.md) §13.1 的 C4），网格的 Go 代码只允许出现在 `apps/**`（`TestMeshGoCodeOnlyUnderApps`），微服与网格的 Go 代码不得互相 import（`TestMicroNotCoupledToMesh`）。

---

## 故障排查

| 症状 | 根因 | 处置 |
|---|---|---|
| 入口 502，日志 `discovery and static fallback both miss`，但 Pod 显示 `2/2 Running` | **注册是一次性的，失败不重试**。应用先于 Consul 就绪时按 `strict.registry` 降级为带病运行，Pod 永久未注册 | **先查 Consul catalog，再查 Pod 状态**；`kubectl rollout restart deploy/<svc>` 恢复。想避免就用 `strict.registry: true` 换「起不来」而不是「起来了没人发现」 |
| `Close()` 卡住不退，K8s 优雅期耗尽后被 SIGKILL | 某一环阻塞且无超时（典型：kafka-go 的 `joinGroup` 不带 read deadline） | 每环已有 5s 上限，超时会 Warn 并带**环节名**——看那条日志定位。可调 `service.stop_timeout` |
| 投出去的消息没人消费 | `mqx` 缺省消费组是 `"mqx-group"`，所有服务挤进同一组抢分区 | 显式配 `consumer.group_id: <服务名>`；用启动日志 `kafka consumer created {... "group_id": ...}` 核对 |
| Prometheus 抓到 `/metrics` 403 | `AuthorizationPolicy`（ALLOW）下 Prometheus 无 SPIFFE 身份 | 采集口写 `excludeInboundPorts`，端口名写 `tcp-metrics` |
| 面板分位数全是垃圾，端点却一直 200 | 直方图桶用了默认的毫秒形状 | 显式 `metric.WithExplicitBucketBoundaries(...)` |
| 配了指标但采集口 404 | srv 档没配 `metrics.addr`（本该装配期即退） | 补 `metrics.addr`；HTTP 档则检查是否被 `metrics.enabled: false` 关掉 |
| `curl /healthz` 404 | 实际注册的是带后缀的两条 | 用 `/healthz/live` 与 `/healthz/ready` |
| 业务拒绝变成 502 | 错误码还原用了 `codes.From` 而非 `rpcwire.FromRPC` | 统一走 `rpcwire.FromRPC` |
| 注册的地址连不上 | 注册地址配成了 `:8080` 这种空 host 形式 | 用 `0.0.0.0:<port>` 或显式设 `service.self.addr` |

---

## 文档索引

| 文档 | 内容 |
|---|---|
| [`examples/ACCEPTANCE.md`](examples/ACCEPTANCE.md) | **可执行验收标准**——三类框架案例的目录树 / 调用链 / 可观测细节，含反向断言 |
| [`examples/HANDLE-AUDIT.md`](examples/HANDLE-AUDIT.md) | 句柄审计：每个句柄的来源合规性与入链确认 |
| [`examples/mesh/apps/README.md`](examples/mesh/apps/README.md) | 为网格写的应用长什么样——职责边界表与减法清单 |
| [`examples/mesh/docs/verify.md`](examples/mesh/docs/verify.md) | 网格验收的执行口径 |
| [`docs/code-review-launchx-2026-10-10.md`](docs/code-review-launchx-2026-10-10.md) | 全仓代码评审（含 §5 分层与鉴权、§9 逐项勘误） |
| [`docs/trace-chain-audit.md`](docs/trace-chain-audit.md) | 调用链完整性审计 |
| [`docs/wiring-and-trace-audit-20261007.md`](docs/wiring-and-trace-audit-20261007.md) | 装配接线与埋点审计 |
| [`docs/xsdk-handles-audit.md`](docs/xsdk-handles-audit.md) | 跨仓 SDK 句柄审计 |
| [`openspec/changes/`](openspec/changes) | 规格变更制品（`add-launchx-bootstrap`、`add-mesh-native-apps`） |

### 写判据的纪律

仓库里的验收脚本和测试遵循几条硬规矩，改测试前请先读：

1. **`Want` 与 `Observed` 分离**。`Want` = 配置要求入链，`Observed` = 真实探针结果。`Diverged() = Present && Want && !Observed`。
2. **判据脚本的退出码就是结论**，调用处**不得**加 `|| true`——一道恒绿的闸门比没有闸门更危险。
3. **至少一次反向探针**：注入违规，确认判据真的会红。**不得把 SKIP 写成 PASS**。
4. **静态判据必须匹配「调用点独有的一段」**，不能匹配函数名本身——函数名必然也出现在定义处，那样的判据是空壳，恒绿。
5. **动态用例证明不了「有人调它」**：装配接线这类事实只能靠静态判据，动态（helper 可用）与静态（有人调用）**成对写**。
6. **鉴权必须有反向断言**：无凭据 401 / 错令牌 401 / 正确 200 / 探针免鉴权 200。少了反向断言，「中间件没挂上」也全绿。

---

## 许可证

本仓库为 `gospacex` 工作区内的内部仓库，**未附独立的 `LICENSE` 文件**。如需对外开源，请先补充许可证声明。

---

维护约定：新增顶层包后必须重跑根包三条守卫测试；改动退出链语义只改 `internal/pipeline` 一处；`deploy/**` 的 ConfigMap 由 `conf/*.yaml` 生成，不手写。
