# add-launchx-bootstrap 实现计划

> **面向 AI 代理的工作者：** 本计划按 superpowers 方式组织。执行时优先使用 `superpowers:executing-plans`（单会话顺序执行、检查点勾选）或 `superpowers:subagent-driven-development`（每 Task 独立子代理 + 评审检查点）。执行铁律：
> 1. 每个 Task 内步骤为 TDD 红绿节奏：先落测试/断言（`- [ ]`），再实现，再跑验证命令；命令输出贴回勾选。
> 2. 只有该 Task 全部步骤勾完并跑通其「验证方式」，才勾 `- [ ] **Task complete**` 并单独 commit（`<type>(task-N): <摘要>`，Conventional Commits）。
> 3. 允许对本计划做**最小偏差修正**（端口冲突、路径笔误、API 微差），但必须在 Task 注释中记录偏差；**规格语义（验收案例 A1–E5）不可偏离**。
> 4. 禁止 `TODO/TBD/实现待定`——一切含糊以本计划「全局设计约束」与 openspec `specs/*/spec.md` 为准。

## 目标

在 `launchx/` 内交付独立 Go module `github.com/gospacex/launchx`：

- **单入口三段式**：`Bootstrap(path) (*App, error)`（地基：配置→telemetry→logx，失败即退）→ `OnSetup(func(*App) error)`（业务装配回调）→ `Run() error`（mode 装配→组件挂载→Setup→注册→服务→反序退出）。
- **四档 mode**：`monolith | bff | srv | gateway`，按映射表装配 webx 引擎 / kitex ServerOptions / consul 注册 / 下游客户端工厂。
- **两套注册表**：`internal/configsrc`（配置源：local/nacos + `${ENV}` 展开）与 `internal/component`（组件构建器：db/cache/mq/registry，`trace.on.*` 只控入链、`strict.*` 裁决降级）。
- **反序退出**：显式序列不依赖 defer——停服务 → OnStop 逆序 → 组件 Stop → deregister → tracing.Shutdown(15s) → config 关闭 → logger 最后关闭；Setup 失败与运行期错误走同一链并返回首错；`Close()` 幂等。
- **三个 demo**：`examples/monolith`（真实 mysql 订单 CRUD）、`examples/micro`（gateway→bff→srv 三进程链路，srv 为 srv-order 零业务改动迁移）、`examples/mesh`（仓内自写 k8s+Istio 清单，两级验收）。
- **验收闭环**：A1–E5 单测/集成/降级/两级网格验收全绿，`openspec validate --strict` 持续通过。

## 架构

```
main（≤50 行）
  └─ launchx.Bootstrap ── internal/pipeline.Foundation
        configsrc.Load（local / nacos key 级裁决）→ mode.Validate → telemetry.Init（最先）→ logx.New+SetGlobal
  └─ app.OnSetup(fn)
  └─ app.Run
        pipeline.Assemble：mode.Assemble（webx / kitex ServerOptions）→ component.Mount(db/cache/mq/registry)
        → fn(app)（业务路由/下游客户端/OnStop 注册）
        → registry.Register → serve（webx engine 或 kitex svr 或用户 Serve(run,stop)）
        → 等待 signal / serveErr / Close
        → pipeline.Shutdown：反序链（见 D5），事件记录器留痕供断言
```

- **防环结构**：`internal/pipeline.Handle` 持有全部运行态（Config/Service/Mode/Logger/Tracer/Engine/Server/DB/Cache/MQ/Registry/Events/stopFns/serve 句柄），根包 `launchx.App` 以**字段嵌入** `*pipeline.Handle`——字段提升使 `app.Config`、`app.Engine` 可用；import 仅单向 `launchx → internal/pipeline`，`pipeline` 不回指 `launchx`，组件/模式包只面向 `component.Env`/`mode.Input` 接口，全图无 import 环。
- **事件记录器**：`pipeline.Events` 按序记录稳定事件名（见 D2），启动/退出顺序全部可断言，不靠日志文本。

## 技术栈

Go 1.27.1；kitex v0.16.3（`server`/`client` 选项经 `internal/rpcx` 封装）；hertz（经 webx v0.10.x 系）；gorm + mysql（dbx：`config.LoadDBConfigFromConfigx` + `orm.Open/OpenNoTrace`）；configx（viper 本地 + nacos remote，key 级裁决）；logx（zap，`logx.New` + `logx.SetGlobal`）；otel v1.46.0 + otelx（Composite 传播器，jaeger OTLP `127.0.0.1:4318`）+ traceflag；mqx + mqx/kafka（segmentio）；cachex + cachex/redis；coordx/consul（`RegisterCreator` 经 init 注册，需 blank import）；consul/api v1.34.5；openspec（`--strict` 持续校验）。

---

## 全局设计约束（所有 Task 必须遵守；与 openspec specs 冲突时以 specs 为准）

### D1 防环与职责边界

- `App` 嵌入 `*pipeline.Handle`；`internal/mode` 只收 `Input`/回 `Output`（不 import pipeline）；`internal/component` 构建器签名 `func(ctx context.Context, env *Env) (any, error)`，`Env = {Config, Logger, Service, Mode, TraceOn func(kind string) bool}`；pipeline 按注册名 type-switch 把 `any` 收进 Handle 字段。
- Task 1 先在根包定义**契约接口**（`CacheHandle`/`RegistryHandle`），Task 4 的具体实现必须满足之；Task 6 将状态字段迁入 `pipeline.Handle`（根包字段经嵌入保持同名可用，`launchx_test.go` 断言不改）。

### D2 事件记录器与稳定事件名（顺序断言只认这些名字）

启动序：`config.load` → `mode.validate` → `telemetry.init` → `logger.init` → `mode.assemble` → `component.db` / `component.cache` / `component.mq` / `component.registry`（仅实际挂载者）→ `setup` → `register`（失败且继续时 `register.degraded`）→ `serve`。
退出序：`server.stop` → `onstop.<n>`（n=注册序号，逆序出现）→ `component.stop.<name>`（逆序 mq→cache→db）→ `deregister` → `registry.close` → `tracing.shutdown` → `config.close` → `logger.close`（**最后**）。
约束：`telemetry.init` 下标必须小于任一 `component.*`；`logger.close` 必须是全序列最后一个；`deregister` 早于 `tracing.shutdown`；flush（`tracing.shutdown`）晚于全部可能标 span 的资源 stop。

### D3 配置 schema（demo config.yaml 一律按此写）

```yaml
service: { name: <svc>, mode: monolith|bff|srv|gateway, addr: 127.0.0.1:<port> }
trace:
  enabled: true
  endpoint: 127.0.0.1:4318          # jaeger OTLP http（裸 host:port 约定，scheme 由 endpointWithScheme 补齐）
  insecure: true
  exporter: otlp                    # telemetry.Options.Exporter
  sample_ratio: 1.0
  on: { db: true, cache: true, mq: true }   # 只控入链，不控构造
strict: { db: false, cache: false, mq: false, registry: false }
registry: { driver: consul, addr: 127.0.0.1:8500, enable_trace: false, name: <svc> }
configcenter: { type: local|nacos, data_id: "", group: HUBX, addr: "" }   # 缺省 type=local（spec 原文「缺省 local」）；data_id 空→纯本地不发远端；addr 由 HUBX_NACOS_SERVER 覆盖，缺省 127.0.0.1:8848
logx: { level: info, format: json }  # 键名 logx（logx SDK Unmarshal("logx") 约定）
webx:            # 可选；webx.ReadConfigFromConfigx 读取的段（sectionKey = "webx"）
  timeout: 5s
gateway:         # 仅 gateway demo
  routes: [{ prefix: /api, service: bff, strip_prefix: true }]
xmiddleware:
  sqlx:    { databases: { <svc>: { driver: mysql, dsn: "order_rw:${HUBX_DB_PW}@tcp(127.0.0.1:3306)/order_db", pool: { ... } } } }
  cachex:  { driver: redis, instances: { <svc>: { addr: 127.0.0.1:6379 } } }
  mqx:     { instances: { <svc>: { brokers: ["127.0.0.1:9092"], ... } } }
```

- 监听地址**单一事实源** `service.addr`；`webx.addr` 仅当 `service.addr` 缺省时兜底。
- 组件启用 = **presence-based**：db 看 `xmiddleware.sqlx.databases.<service>` 是否存在；cache 看 `instances.<service>.addr` 非空；mq 看 `instances.<service>.brokers` 非空；registry 由 D4 的 mode 表决定。
- `${ENV}` 展开点唯一：db 组件把 `DBConfig` 包一层 `expandDB{base}`，`DSN() = configsrc.ExpandEnv(base.DSN())`；configsrc 对外导出 `ExpandEnv`。

### D4 mode 映射表与组件裁决

| mode | Engine(webx) | Server(kitex opts) | registry 挂载+注册 | 下游客户端工厂 |
|---|---|---|---|---|
| monolith | ✅ | — | ❌ | — |
| bff | ✅ | — | ✅ | `RPCClientOptions(target)` |
| gateway | ✅ | — | ✅ | `Registry.Target(svc)` |
| srv | ❌ | ✅ | ✅ | `Registry.Target(svc)`（如 product） |

- 非法/缺失 mode → error，文案必须含合法取值 `monolith|bff|srv|gateway`；校验发生在 Bootstrap 地基段（`config.load` 之后、`telemetry.init` 之前），失败即退。
- `strict.<name>`（默认 false）：组件构建失败时 false → 字段保持 nil + `Warn`（文案含后果：某功能不可用）+ 事件不记 `component.<name>`；true → 返回 `fmt.Errorf("init <name>: %w", err)`，Run 走完整退出链。registry 分两段、**同受 `strict.registry` 裁决**（specs component-registry「registry 失败带病运行」原文：除 `strict.registry: true` 外 SHALL 记 Warn 并继续）：① Mount 段 `component.Mount` 里 `coordx.NewRegister` 构建失败按上句裁决；② Run 段 `Registry.Register()` 调用失败（consul 不可达的主形态）false → 记 `register.degraded` 事件 + Warn（未注册态、退出时跳过 deregister）+ 继续 serve；true → 返回 `fmt.Errorf("register: %w", err)` 走完整退出链。
- `trace.on.<kind>` 只决定 Open/OpenNoTrace、是否注入 trace 中间件；关闭时组件**仍构造**（D1 断言）。
- `Trace.On(kind)` 语义：`!trace.enabled → false`；显式 `trace.on.<kind>` 用其值；未写默认 true。

### D5 反序退出链（`pipeline.Shutdown(h, firstErr) error`，显式序列、无 defer）

1. 停 serve：`serveStop(ctx)`（webx：`Engine.Shutdown`；srv：`svr.Stop`；用户 `Serve(stop)` 的 stop fn）或 `serveRun` 已退出则跳过。
2. 项目 `OnStop` 钩子按注册**逆序**执行（`onstop.<n>` 事件）。
3. 内置组件 Stop **逆序挂载序**：`component.stop.mq` → `component.stop.cache` → `component.stop.db`（producer.Close / cache.Close / db 无 Close 则跳过，事件仍记）。
4. `deregister`（registry.Deregister）→ `registry.close`。
5. `tracing.shutdown`：`TraceSetup.Shutdown(ctx)`，15s 超时。
6. `config.close`：`Config.Close()`。
7. `logger.close`：`Logger.Close()`——**绝对最后**。

- 首错返回：地基/Setup/serve 期间的 error 作为返回值，链内各步错误仅记录不覆盖首错；链自身 error 仅当无首错时返回。
- 资源创建处**禁止 defer 关闭** span 相关资源（span 导出器最后关）。

### D6 端口、e2e 与进程约定

| 进程 | 端口 | 备注 |
|---|---|---|
| monolith 手动运行 | 127.0.0.1:8080 | demo config.yaml |
| monolith e2e | 127.0.0.1:18080 | **e2e 临时 config**，避免与 gateway 8080 并行冲突 |
| gateway | 127.0.0.1:8080 | plan-ready 验收命令 `curl localhost:8080/api/ping` |
| bff | 127.0.0.1:8091 | `curl localhost:8091/order/1001` |
| srv | 127.0.0.1:9013 | kitex |
| micro e2e 的 srv | 127.0.0.1:19013 | e2e 临时 config |

- `go run` 命令一律**从 demo 目录执行**（`cd examples/monolith && go run ./cmd`），保证相对路径 `config.yaml` 命中。
- e2e 底座前置（写进各脚本/测试，幂等可重跑）：
  ```bash
  docker compose -f /Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml up -d consul mysql kafka jaeger redis nacos
  docker exec -i mysql mysql -uroot -prootpass < /Users/hyx/work/gowork/src/hubx/micro_apps/docker/mysql/init.sql
  ```
  （dockers 的 mysql init 挂载目录为空，**不会**自动建 `order_db`/`order_rw`，必须手动灌脚本；脚本幂等。Task 3 的 B1 集成测试需 `nacos`，纯本地/其余 Task 不依赖它。）
- 底座端口：consul `127.0.0.1:8500`、mysql `127.0.0.1:3306`（root/rootpass；`order_db`、`order_rw/hubx123`）、kafka `127.0.0.1:9092`（KRaft、AUTO_CREATE、**禁止 19093**）、jaeger `4318/16686`、redis `127.0.0.1:6379`（无密码）、nacos `127.0.0.1:8848`/`9848`/`9849`（v2.4.2，`NACOS_AUTH_ENABLE=false` 无需登录，healthcheck 走 actuator，container `nacos`；Task 3 灌数用 OpenAPI `POST /nacos/v1/cs/configs`）。**`hubx/micro_apps` 自有 compose（`hubx-ma-*`）与 dockers 端口冲突，两套不可同跑；本计划一律用 dockers。**
- e2e 测试底座探测契约：helper `requireBase(t, addr)` 先 TCP dial（2s 超时，重试 15×1s）；`LAUNCHX_E2E=1` 时探测失败 `t.Fatal`（脚本强制），否则 `t.Skip("底座未启动：见 scripts/…")`——保证裸 `go test ./...` 无底座也全绿。
- kafka 消费断言用 `github.com/segmentio/kafka-go`（直连 `127.0.0.1:9092`，topic 自动创建）。

---

## Traceability 反查表

| 验收案例 | plan-ready Task | tasks.md | 本计划 Task |
|---|---|---|---|
| A1 单入口/句柄/幂等 | Task 1 | 1.1 | Task 1 |
| A2 启动固定顺序 | Task 6 | 1.6 | Task 6、15 |
| A3 反序退出 + 同链复用 | Task 7 | 1.7 | Task 7、15、22 |
| B1 配置三裁决/即退 | Task 3、14 | 1.3、3.1 | Task 3、14 |
| C1/C2/C3 四档装配与非法 mode | Task 5、17 | 1.5、3.4 | Task 5、17 |
| C4 零改动/自包含清单 | Task 11、12 | 2.4、2.5 | Task 11、12 |
| D1 trace.on 不改构造 | Task 4、16 | 1.4、3.3 | Task 4、16 |
| D2 strict 双分支 | Task 4、16 | 1.4、3.3 | Task 4、16 |
| E1 monolith 落 mysql | Task 8、18 | 2.1、3.5 | Task 8、18 |
| E2 micro 链路 | Task 9、10、19 | 2.2、2.3、3.6 | Task 9、10、19 |
| E3 srv 迁移 + outbox | Task 11、20 | 2.4、3.7 | Task 11、20 |
| E4/E5 网格同 trace / OFF-ON | Task 12、21 | 2.5、3.8 | Task 12、21 |
| （贯穿）配置 local/DSN 展开 | Task 2 | 1.2 | Task 2 |
| （贯穿）README 案例指引 | Task 13 | 2.6 | Task 13 |
| （贯穿）降级/registry 带病 | Task 22 | 3.9 | Task 22 |
| （贯穿）对齐核对 | Task 23 | 3.10 | Task 23 |

Scenario 编号以 `openspec/changes/add-launchx-bootstrap/specs/*/spec.md` 内 `#### Scenario: 验收案例 …` 标题为准；Task 23 做双向核对。

## 环境事实（写死，不得各起炉灶）

- 仓库：`/Users/hyx/work/gowork/src/hubx/launchx`（当前**非 git 仓库**）；同级兄弟模块：`../{webx,configx,logx,traceflag,dbx,cachex,mqx,otelx,coordx,hubx}` 均 `module github.com/gospacex/<目录名>`；`../cachex/redis`、`../mqx/kafka` 为独立子 module；`coordx/consul` 亦是**独立子 module**（`head coordx/consul/go.mod` → `module github.com/gospacex/coordx/consul`，自带 go.mod/go.sum；`consul.go:158` 与 `register.go:6` 各有一个 `init()` 调 `RegisterCreator("consul",…)`，重复注册为幂等覆盖）——replace 必须同时写 `github.com/gospacex/coordx => ../coordx` 和 `github.com/gospacex/coordx/consul => ../coordx/consul`，import 用 `github.com/gospacex/coordx/consul`，代码里 blank import `_`（连同 coordx 本体 import）。
- `github.com/gospacex/hubx => ../hubx` 纳入 replace（dbx MVS 闭包依赖，micro_apps go.mod 先例）。
- 底座 compose：`/Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml`；init 脚本：`/Users/hyx/work/gowork/src/hubx/micro_apps/docker/mysql/init.sql`（幂等，e2e 前必灌，见 D6）。
- minikube 仅 context 存在、集群未运行；mesh 真实部署为**人工验证项**（close 不硬阻塞）。
- git 方案：Task 1 首步 `git init` + 初始提交；此后每 Task 一次 commit。
- **coordx/consul 上游注册修复（用户已裁决选项 2，修复代码已实测在盘）**：原 `../coordx/consul/consul.go` 的 `Check{TTL:"30s", Interval:"10s", Status:"passing"}` 被 Consul 1.22.4 拒绝（HTTP 400 `Invalid check: Interval and TTL cannot both be specified`，curl 复现），导致 coordx 注册恒失败。已在 `../coordx/consul/consul.go` 完成修复：import 增 `sync`/`time`；`const ttlBeatInterval = 10 * time.Second`；`registerImpl` 增 `mu sync.Mutex` + `beats map[string]chan struct{}`（`NewRegister` 内 `make`）；Check 删 `Interval` 只留 TTL 30s / Status `passing` / DeregisterCriticalServiceAfter 90s；Register 成功后 `startBeat(inst.ID)`（先 `stopBeat` 停旧防双写；goroutine 每 10s `Agent().UpdateTTL("service:"+id, "", "passing")`，失败 `WarnContext`）；Deregister 成功后 `stopBeat(inst.ID)`；`Close` 停全部 beats；无 beat 当 id==""。实测：注册 200、`passingOnly` 发现 1 实例、**35s 后 check 仍 passing（心跳存活）**、注销后 discovery 归零；`go build ./... && go vet ./...` 于 `../coordx/consul` 全绿。launchx 侧**不改**技术栈线（coordx 封装原样保留，`consul/api` 不直连）。Task 4 Step 4.1 为该修复的规范落地点（幂等重放 + build/vet 复验）；coordx 仓的 git commit 在 coordx 仓单独提交，不由本计划的 launchx commit 覆盖。
- **`webx` 段缺省不存在时 `configx.Unmarshal("webx", …)` 报 `key webx not found`（本会话实测）**：mode 装配必须先 `in.Config.IsSet("webx")` 守卫再调 `webx.ReadConfigFromConfigx`——这正是 D3「webx 可选」的落法（落点 Task 5 Step 5.3；缺段时用零值 `webx.Config{}`，`webx.New` 内部 `FillDefaults` 补齐）。
- **`go mod tidy` 实测（probe 复验）**：按 Step 1.2 的完整 replace 块（configx/coordx/coordx/consul/logx/traceflag 等全量）时 tidy 通过（GOPROXY=goproxy.cn）；缺 replace 会诱发 `genproto` 歧义与缺 go.sum 的假象——Step 1.2 的 replace 块**不得删减**，Task 4/5 的 Run 均须带 `go mod tidy`。

## 文件结构（本计划新建/改写的全部路径）

```
launchx/
├── go.mod / go.sum / .gitignore
├── launchx.go  app.go  setup.go  client.go  client_test.go  launchx_test.go   # Task 1/6/7（根包单入口）；client.go 见 Task 5 偏差注记
├── internal/
│   ├── configsrc/    registry.go local.go nacos.go expand.go trace.go *_test.go  # Task 2/3/14；trace.go（Trace.On 语义）见 Task 5 偏差注记
│   ├── component/    registry.go db.go cache.go mq.go consul.go strict.go *_test.go  # Task 4/16（Env/Builder 契约在 registry.go 内，无独立 env.go）
│   ├── mode/         mode.go webx.go srv.go registry.go rpcx.go mode_test.go    # Task 5/17
│   ├── pipeline/     pipeline.go events.go shutdown.go pipeline_test.go shutdown_test.go  # Task 6/7/15
│   ├── telemetry/    provider.go provider_test.go                               # Task 6
│   └── rpcx/         options.go middleware.go                                   # Task 5（对齐 micro_apps/pkg/rpcx）
├── examples/
│   ├── monolith/     config.yaml cmd/main.go app/{setup,routes}.go
│   │                 internal/{handler,logic,service,repository,model}/*.go
│   │                 e2e_test.go README.md                                      # Task 8/13/18/22
│   ├── micro/
│   │   ├── gateway/  config.yaml cmd/main.go app/setup.go internal/{routing,proxy}/*.go   # Task 9
│   │   ├── bff/      config.yaml cmd/main.go app/setup.go internal/{handler,downstream}/*.go  # Task 10
│   │   ├── srv/      config.yaml cmd/main.go app/setup.go
│   │   │             internal/{repository,service,rpc,middleware}/** pkg/kitex_gen/**   # Task 11（自 micro_apps 复制后改造）
│   │   │             e2e_test.go README.md                                      # Task 20/22
│   │   ├── scripts/e2e.sh  README.md                                            # Task 13/19
│   │   └── (bff/gateway degrade 用例并入 e2e 或 *_test.go)                      # Task 22
│   └── mesh/         k8s/** istio/** scripts/bring-up.sh configs/** README.md docs/verify.md  # Task 12/13/21
└── docs/superpowers/plans/2026-10-04-add-launchx-bootstrap.md                   # 本文件
```

> **同步对照**：每 Task 头部 `> **sync:**` 引用块逐字复制自 `openspec/changes/add-launchx-bootstrap/plan-ready.md` 与 `tasks.md`（勾选框前缀为引用格式）；若你对本计划做了偏差修正，**不得**修改 sync 引用块，偏差写在正文步骤里。

---

## 实现步骤

### Task 1: 创建 launchx module 与单入口 API

> **trace:** plan-ready.md Task 1 · tasks.md 1.1 · 验收案例 A1

> **sync:** plan-ready.md Task 1 · tasks.md 1.1

> - [ ] **任务完成**（与 superpowers plan `Task 1`、`tasks.md` 对应条目同步勾选）
> - 目标：`go.mod` + `launchx.go`/`app.go`/`setup.go`：`Bootstrap(path) (*App, error)`、App 句柄字段（Config/Logger/Tracer/DB/Cache/MQ/Registry/Engine）、`OnSetup(func(*App) error)`、`Run() error`、幂等 `Close()`；Run 先以最小骨架占位（后续 Task 填充），main.go 形态可编译（验收案例 A1）
> - 改动文件：`go.mod`、`launchx.go`、`app.go`、`setup.go`、`launchx_test.go`
> - 验证方式：`go build ./...` 通过；`go test ./... -run TestBootstrap` 单测通过（Bootstrap 返回 App、OnSetup 可注册、Close 幂等）
> - [ ] 1.1 创建 launchx module（`go.mod`）与单入口 API：`Bootstrap(path) (*App, error)`、`App` 句柄字段（Config/Logger/Tracer/DB/Cache/MQ/Registry/Server）、`OnSetup(func(*App) error)`、`Run() error`、幂等 `Close()`（对应验收案例 A1）

**Step 1.1 — git 仓库初始化**

- [ ] 执行：

```bash
cd /Users/hyx/work/gowork/src/hubx/launchx
git init
printf '%s\n' 'bin/' '*.test' '*.out' 'coverage*' '.DS_Store' > .gitignore
git add -A && git commit -m "chore: init launchx repository"
```

Run: `git log --oneline`；Expected: 一条 `chore: init launchx repository`（父级 hubx 若已是仓库，launchx 为独立嵌套仓库，不冲突）。

**Step 1.2 — go.mod（replace 块，requires 交给 tidy）**

- [ ] 新建 `go.mod`：

```go
module github.com/gospacex/launchx

go 1.27.1

replace (
	github.com/gospacex/cachex => ../cachex
	github.com/gospacex/cachex/redis => ../cachex/redis
	github.com/gospacex/configx => ../configx
	github.com/gospacex/coordx => ../coordx
	github.com/gospacex/coordx/consul => ../coordx/consul
	github.com/gospacex/dbx => ../dbx
	github.com/gospacex/hubx => ../hubx
	github.com/gospacex/logx => ../logx
	github.com/gospacex/mqx => ../mqx
	github.com/gospacex/mqx/kafka => ../mqx/kafka
	github.com/gospacex/otelx => ../otelx
	github.com/gospacex/traceflag => ../traceflag
	github.com/gospacex/webx => ../webx
)
```

Run（Step 1.6 末统一执行）: `go mod tidy`；Expected: 依赖全部解析（kitex 钉 `v0.16.3`、otel `v1.46.0` 已在本机 module cache，因 micro_apps 同版本使用）。

**Step 1.3 — `launchx.go`：Bootstrap（地基最小形态）**

- [ ] 新建 `launchx.go`：

```go
// Package launchx 提供 micro 服务统一单入口：
// Bootstrap（地基）→ OnSetup（业务回调）→ Run（装配/服务/反序退出）。
package launchx

import (
	"fmt"
	"os"

	"github.com/gospacex/configx"
)

// Bootstrap 装载地基：读取 path 指定的 yaml 配置并返回 App 句柄。
// 地基失败即退——任何错误都先关闭已打开的配置句柄再返回，不进入 Run。
// Task 6 起配置读取改经 internal/configsrc（其 Load 内含同款 os.Stat 守卫，
// 届时删除本处守卫，语义不变），Bootstrap 整体改经 pipeline.Foundation，
// 在 config.load 后追加 mode.validate / telemetry.init / logger.init 三步
// （失败同样即退，已打开资源显式反序关闭）。
func Bootstrap(path string) (*App, error) {
	if path == "" {
		path = "config.yaml"
	}
	// configx 对缺失文件不报错（provider.go: viper 失败→Warn+emptyConfig），
	// 地基失败即退要求显式守卫，否则「配置文件不存在」会静默降级为空配置。
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("bootstrap: load config %s: %w", path, err)
	}
	cfg, err := configx.ReadConfig(configx.ConfigOptions{
		ConfigFile: path,
		ConfigType: "yaml",
	})
	if err != nil {
		return nil, fmt.Errorf("bootstrap: load config %s: %w", path, err)
	}
	a := &App{
		Config:  cfg,
		Service: cfg.GetString("service.name"),
		Mode:    cfg.GetString("service.mode"),
		quit:    make(chan struct{}),
	}
	if a.Service == "" {
		a.Service = "launchx"
	}
	return a, nil
}
```

**Step 1.4 — `app.go`：App 句柄、契约接口、OnStop、幂等 Close、Run 占位**

- [ ] 新建 `app.go`：

```go
package launchx

import (
	"context"
	"sync"

	"github.com/cloudwego/kitex/server"
	"github.com/gospacex/cachex"
	"github.com/gospacex/configx"
	"github.com/gospacex/logx"
	"github.com/gospacex/mqx"
	"github.com/gospacex/webx"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/gorm"
)

// CacheHandle 是 cache 组件契约：Task 4 的 internal/component 具体类型
// 必须实现之；Task 1 先定契约保证根包可编译、后续不改调用方。
type CacheHandle interface {
	Instance(name string) cachex.Cache
	Close() error
}

// RegistryHandle 是 registry 组件契约（Task 4 实现）：
// Register/Deregister 供 Run 装配段与退出链调用，Target 供下游客户端解析
// （gateway 代理按请求 ctx 走发现，故带 ctx；实现内部按需用 ctx 发起查询）。
type RegistryHandle interface {
	Register(ctx context.Context) error
	Deregister(ctx context.Context) error
	Target(ctx context.Context, service string) (string, error)
	Close() error
}

// App 是单入口句柄。Task 6 起其状态字段迁入 *pipeline.Handle 并以嵌入
// 方式保持全部字段名不变（launchx_test.go 断言不需修改）。
type App struct {
	Config       configx.Config
	Service      string
	Mode         string
	Logger       logx.Logger
	Tracer       trace.Tracer
	TraceEnabled bool
	DB           *gorm.DB
	Cache        CacheHandle
	MQ           mqx.Producer
	Registry     RegistryHandle
	Server       []server.Option // kitex ServerOptions（mode=srv 产出）
	Engine       *webx.App       // webx 引擎（monolith/bff/gateway 产出）

	setupFn   func(*App) error
	stopFns   []func(context.Context) error
	serveRun  func() error
	serveStop func(context.Context) error
	quit      chan struct{}
	once      sync.Once
}

// OnStop 注册退出钩子：Run 收尾段按注册逆序执行（D5 第 2 步）。
func (a *App) OnStop(fn func(context.Context) error) {
	if fn != nil {
	a.stopFns = append(a.stopFns, fn)
	}
}

// Close 触发退出并幂等：首次调用关闭 quit 通道唤醒 Run 收尾，
// 后续调用复用同一次触发。Task 7 将扩展为「等待退出链完成后再返回」。
func (a *App) Close() error {
	a.once.Do(func() { close(a.quit) })
	return nil
}

// Run 最小骨架占位（对齐 Task 1 sync「Run 先以最小骨架占位」）：
// 等待 Close 触发后返回 nil。完整启动序与反序退出链
// （Assemble→Setup→register→serve→pipeline.Shutdown）Task 7 全量替换本体。
func (a *App) Run() error {
	<-a.quit
	return nil
}
```

**Step 1.5 — `setup.go`：OnSetup 与 Serve 契约**

- [ ] 新建 `setup.go`：

```go
package launchx

import "context"

// OnSetup 注册业务装配回调：Run 在 mode 装配与组件挂载完成后调用一次。
// 回调返回 error 视为 Setup 失败：走 D5 完整退出链并以该 error 返回。
func (a *App) OnSetup(fn func(*App) error) {
	a.setupFn = fn
}

// Serve 声明服务主循环，供 mode=srv 与用户自定义服务使用：
// run 启动（阻塞，如 kitex svr.Run），stop 优雅停止。
// Task 7 接入 Run 的等待与退出链；调用方可直接给 Engine 场景留空。
func (a *App) Serve(run func() error, stop func(context.Context) error) {
	a.serveRun = run
	a.serveStop = stop
}
```

**Step 1.6 — `launchx_test.go`（白盒，A1 断言）**

- [ ] 新建 `launchx_test.go`：

```go
package launchx

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBootstrap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := "service:\n  name: bootstrap-demo\n  mode: monolith\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	app, err := Bootstrap(path)
	if err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}
	if app == nil {
		t.Fatal("Bootstrap() = nil app, want *App")
	}
	if got := app.Config.GetString("service.name"); got != "bootstrap-demo" {
		t.Errorf("Config service.name = %q, want bootstrap-demo", got)
	}
	if app.Service != "bootstrap-demo" || app.Mode != "monolith" {
		t.Errorf("App.Service/Mode = %q/%q", app.Service, app.Mode)
	}
	if app.quit == nil {
		t.Error("Bootstrap() quit channel not initialized")
	}

	// OnSetup 可注册（白盒检查回调字段）
	app.OnSetup(func(*App) error { return nil })
	if app.setupFn == nil {
		t.Error("OnSetup() did not register callback")
	}

	// Close 幂等：两次调用均无错、不 panic
	if err := app.Close(); err != nil {
		t.Errorf("Close() #1 = %v", err)
	}
	if err := app.Close(); err != nil {
		t.Errorf("Close() #2 = %v", err)
	}

	// Run 占位（Task 1 语义）：quit 已关 → 立即返回 nil
	if err := app.Run(); err != nil {
		t.Errorf("Run() = %v, want nil", err)
	}

	// 缺失文件必须报错（地基失败即退）
	if _, err := Bootstrap(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("Bootstrap(missing) = nil error, want error")
	}
}
```

- [ ] Run（从 `launchx/` 根执行）：

```bash
go mod tidy
go build ./...
go test ./... -run TestBootstrap -count=1
```

Expected: `go build` 无输出无错；测试输出 `ok github.com/gospacex/launchx`（全 PASS，含缺失文件报错用例）。

**Step 1.7 — commit**

- [ ] `git add -A && git commit -m "feat(task-1): launchx module 与单入口 Bootstrap/OnSetup/Run/Close 骨架"`

- [ ] **Task complete**

---

### Task 2: configsrc 注册表与 local 加载器

> **trace:** plan-ready.md Task 2 · tasks.md 1.2 · specs/config-loading「缺省走 local」「DSN 密码占位展开」

> **sync:** plan-ready.md Task 2 · tasks.md 1.2

> - [ ] **任务完成**（与 superpowers plan `Task 2`、`tasks.md` 对应条目同步勾选）
> - 目标：`internal/configsrc`：`Register(name, Loader)` 注册表 + `local` 加载器（configx `ReadConfig` 读本地 yaml）+ `${ENV}` 展开辅助 `ExpandEnv(s string) string`（DSN 占位用）；yaml 未声明 `configcenter` 时默认 local
> - 改动文件：`internal/configsrc/{registry.go,local.go,expand.go}`、`internal/configsrc/local_test.go`
> - 验证方式：`go test ./internal/configsrc/` — 缺省走 local、env 展开、注册表覆盖三项断言通过
> - [ ] 1.2 实现 `internal/configsrc` 注册表与 `local` 加载器：configx 读本地 yaml + `${ENV}` 展开（DSN 占位）

**Step 2.1 — `internal/configsrc/registry.go`（注册表 + Load 分发 + os.Stat 守卫）**

- [ ] 新建 `internal/configsrc/registry.go`：

```go
// Package configsrc 收敛 launchx 的配置加载：注册表 + local/nacos 两内置加载器。
//
// 所有权约定：Load 探读出的 probe 句柄**移交**被分发的 Loader——local 直接
// 复用为返回句柄；nacos 在构造出合并句柄后必须关闭 probe（成败皆关），
// 避免 viper/Nacos 长轮询句柄泄漏。configx 自身对缺失文件不报错
// （provider.go:61-66 viper 失败→Warn+emptyConfig），因此 Load 先 os.Stat
// 守卫——「地基失败即退」由本层保证（Task 1 在 launchx.Bootstrap 的同款
// 守卫在 Task 6 迁入此处后删除）。
//
// 远端开关唯一入口是 yaml 的 configcenter.type：本包不读 configx 自带的
// configx.remote.* 隐藏开关，demo 与文档不写该键（写法超出 spec 可插拔配置源）。
package configsrc

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/gospacex/configx"
)

// Input 传给 Loader 的上下文：Path 为本地 yaml 路径，Probe 为按该路径
// 探读出的 configx 句柄（所有权随本结构移交 Loader，见包注释）。
type Input struct {
	Path  string
	Probe configx.Config
}

// Loader 配置源加载器：local 返回 Probe 本身；nacos 可能返回合并后的新句柄。
type Loader interface {
	Load(ctx context.Context, in Input) (configx.Config, error)
}

var (
	loadersMu sync.RWMutex
	loaders   = map[string]Loader{}
)

// Register 注册（同名覆盖）加载器；local/nacos 经各自文件的 init() 注册，
// 测试可注入 fake 后用 unregister 清理。
func Register(name string, l Loader) {
	loadersMu.Lock()
	defer loadersMu.Unlock()
	loaders[name] = l
}

// unregister 仅供测试清理注册表（白盒）。
func unregister(name string) {
	loadersMu.Lock()
	defer loadersMu.Unlock()
	delete(loaders, name)
}

// Load 按 yaml configcenter.type 分发（缺省 "local"，spec「可插拔配置源」）。
// 探读走 configx.ReadConfig（ConfigFile/ConfigType 显式指定，避开 HUBX_CONFIG
// 环境变量的隐式路径切换）。
func Load(ctx context.Context, path string) (configx.Config, error) {
	if path == "" {
		path = "config.yaml"
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("configsrc: load %s: %w", path, err)
	}
	probe, err := configx.ReadConfig(configx.ConfigOptions{
		ConfigFile: path,
		ConfigType: "yaml",
	})
	if err != nil {
		return nil, fmt.Errorf("configsrc: read %s: %w", path, err)
	}

	name := probe.GetString("configcenter.type")
	if name == "" {
		name = "local"
	}
	loadersMu.RLock()
	l, ok := loaders[name]
	known := make([]string, 0, len(loaders))
	for k := range loaders {
		known = append(known, k)
	}
	loadersMu.RUnlock()
	if !ok {
		sort.Strings(known)
		_ = probe.Close()
		return nil, fmt.Errorf("configsrc: loader %q not registered (available: %v)", name, known)
	}
	// probe 所有权移交 Loader：后续关闭责任在 Loader，本函数不再碰它。
	h, err := l.Load(ctx, Input{Path: path, Probe: probe})
	if err != nil {
		return nil, err
	}
	return h, nil
}
```

**Step 2.2 — `internal/configsrc/local.go`**

- [ ] 新建 `internal/configsrc/local.go`：

```go
package configsrc

import (
	"context"

	"github.com/gospacex/configx"
)

// localLoader 纯本地加载器：直接复用探读句柄（configx.ReadConfig 已读
// path 指定文件），不发起任何远端请求（spec Scenario「缺省走 local」）。
type localLoader struct{}

// Load 返回 in.Probe——所有权已由 Load 移交，local 是句柄的最终持有者。
func (localLoader) Load(_ context.Context, in Input) (configx.Config, error) {
	return in.Probe, nil
}

func init() { Register("local", localLoader{}) }
```

**Step 2.3 — `internal/configsrc/expand.go`**

- [ ] 新建 `internal/configsrc/expand.go`：

```go
package configsrc

import "os"

// ExpandEnv 对 ${ENV} 占位做 os.ExpandEnv 展开。
//
// 展开点唯一（全局设计约束 D3）：configx 本身不做 env 展开（保持
// micro_apps 既有语义），dsn 等字段由 db 组件经 expandDB.DSN() 调用本函数；
// 组件拿到的已是展开后的真实值（spec Scenario「DSN 密码占位展开」）。
func ExpandEnv(s string) string { return os.ExpandEnv(s) }
```

**Step 2.4 — `internal/configsrc/local_test.go`（白盒四断言）**

- [ ] 新建 `internal/configsrc/local_test.go`：

```go
package configsrc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gospacex/configx"
)

// writeCfg 写入临时 config.yaml 并返回路径。
func writeCfg(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadDefaultLocal：yaml 未声明 configcenter → 缺省 local（specs/config-loading
// Scenario「缺省走 local」：仅本地加载，不发起任何远端请求）。
func TestLoadDefaultLocal(t *testing.T) {
	path := writeCfg(t, "service:\n  name: cfg-local\n  mode: monolith\n")

	h, err := Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	if got := h.GetString("service.name"); got != "cfg-local" {
		t.Errorf("service.name = %q, want cfg-local", got)
	}
	if h.IsSet("configcenter.type") {
		t.Error("configcenter.type unexpectedly set in fixture")
	}
}

// TestLoadMissingFile：缺失文件必须报错（configx 静默降级为空配置，守卫在本层）。
func TestLoadMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.yaml")
	_, err := Load(context.Background(), path)
	if err == nil {
		t.Fatal("Load(missing) = nil error, want error")
	}
	if !strings.Contains(err.Error(), "missing.yaml") {
		t.Errorf("error = %v, want contain missing.yaml", err)
	}
}

// fakeLoader 记录是否被分发到（注册表覆盖断言用）。
type fakeLoader struct{ called bool }

func (f *fakeLoader) Load(_ context.Context, in Input) (configx.Config, error) {
	f.called = true
	return in.Probe, nil
}

// TestLoadRegistryOverride：configcenter.type 声明后按名选择加载器
// （specs/config-loading Scenario「声明才启用远端」的注册表分发前提）。
func TestLoadRegistryOverride(t *testing.T) {
	fake := &fakeLoader{}
	Register("fake", fake)
	t.Cleanup(func() { unregister("fake") })

	path := writeCfg(t, "configcenter:\n  type: fake\nservice:\n  name: cfg-fake\n")
	h, err := Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	if !fake.called {
		t.Error("fake loader not dispatched")
	}
	if got := h.GetString("service.name"); got != "cfg-fake" {
		t.Errorf("service.name = %q, want cfg-fake", got)
	}
}

// TestLoadUnknownLoader：未注册类型必须报错并列出可用加载器（fail fast，
// 不静默回退 local——回退会把「以为用远端」的漂移藏进地基）。
func TestLoadUnknownLoader(t *testing.T) {
	path := writeCfg(t, "configcenter:\n  type: apollo\n")
	_, err := Load(context.Background(), path)
	if err == nil {
		t.Fatal("Load(apollo) = nil error, want error")
	}
	if !strings.Contains(err.Error(), `"apollo"`) || !strings.Contains(err.Error(), "not registered") {
		t.Errorf("error = %v, want contain %q and not registered", err, "apollo")
	}
}

// TestExpandEnv：DSN 占位展开（spec Scenario「DSN 密码占位展开」的函数级断言；
// 组件级断言见 Task 4/14 的 expandDB）。
func TestExpandEnv(t *testing.T) {
	t.Setenv("LAUNCHX_TEST_PW", "s3cret")
	got := ExpandEnv("order_rw:${LAUNCHX_TEST_PW}@tcp(127.0.0.1:3306)/order_db")
	want := "order_rw:s3cret@tcp(127.0.0.1:3306)/order_db"
	if got != want {
		t.Errorf("ExpandEnv() = %q, want %q", got, want)
	}
	if plain := ExpandEnv("no-placeholder"); plain != "no-placeholder" {
		t.Errorf("ExpandEnv(plain) = %q, want unchanged", plain)
	}
}
```

**Step 2.5 — Run**

- [ ] 执行（从 `launchx/` 根）：

```bash
go test ./internal/configsrc/ -count=1
go build ./...
```

Expected: `ok github.com/gospacex/launchx/internal/configsrc`（5 个测试全 PASS：缺省走 local、缺失文件报错、注册表覆盖分发、未知类型 fail fast、ExpandEnv 展开）；`go build` 无输出无错。

**Step 2.6 — commit**

- [ ] `git add -A && git commit -m "feat(task-2): configsrc 注册表与 local 加载器"`

- [ ] **Task complete**

---

### Task 3: configsrc nacos 加载器（三裁决）

> **trace:** plan-ready.md Task 3 · tasks.md 1.3 · 验收案例 B1

> **sync:** plan-ready.md Task 3 · tasks.md 1.3

> - [ ] **任务完成**（与 superpowers plan `Task 3`、`tasks.md` 对应条目同步勾选）
> - 目标：移植 micro_apps `infra/nacos.LoadUnified` 语义（该仓实际 API 为 `LoadUnified`，历史文档中的 `LoadMerged` 即指此）：本地兜底 + `hubx.<service>` dataId key 级覆盖 + `HUBX_NACOS_SERVER` env 覆盖地址；dataId 空纯本地；远端不可达返回地基 error（验收案例 B1）
> - 改动文件：`internal/configsrc/nacos.go`、`internal/configsrc/nacos_test.go`
> - 验证方式：`go test ./internal/configsrc/ -run Nacos` — key 级裁决/纯本地/不可达即退三断言通过
> - [ ] 1.3 实现 `configsrc` 的 `nacos` 加载器：本地兜底 + `hubx.<service>` dataId key 级裁决 + `HUBX_NACOS_SERVER` env 覆盖，dataId 空纯本地；地基失败即退（对应验收案例 B1）

**Step 3.1 — `internal/configsrc/nacos.go`（dataId 空纯本地 / 三裁决 / 错误串含 dataId 与地址）**

- [ ] 新建 `internal/configsrc/nacos.go`：

```go
package configsrc

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/gospacex/configx"
)

// 默认 Nacos 地址（与 micro_apps/infra/nacos 对齐；dockers compose 的 nacos 服务）。
const (
	EnvNacosServer   = "HUBX_NACOS_SERVER" // "127.0.0.1:8848" 形式
	DefaultNacosIP   = "127.0.0.1"
	DefaultNacosPort = 8848
)

// nacosLoader 远程加载器：移植 micro_apps/infra/nacos.Load 的三裁决语义——
//   1. 本地 config.yaml 逐 key 兜底（key 级合并由 configx Provider 完成：
//      Provider.Get 远程有的 key 用远程值，没有的逐 key 回本地 viper）；
//   2. 远程 dataId（hubx.<service> 等）key 级覆盖本地；
//   3. Nacos 地址由 env HUBX_NACOS_SERVER 覆盖 yaml configcenter.addr。
//
// dataId 为空 → 纯本地（返回 Probe，不发起任何远端请求，spec「dataId 为空
// MUST 纯本地运行」）；远程开启但 Nacos 不可达 → 返回地基 error（configx.New
// 内 nacosimpl.New 会主动 GetConfig，不降级），错误串含 dataId 与地址上下文
// （spec Scenario「远端不可达按地基失败」）。
type nacosLoader struct{}

func (nacosLoader) Load(ctx context.Context, in Input) (configx.Config, error) {
	if ctx.Err() != nil {
		_ = in.Probe.Close()
		return nil, ctx.Err()
	}
	dataID := strings.TrimSpace(in.Probe.GetString("configcenter.data_id"))
	if dataID == "" {
		// 纯本地：probe 所有权已移交，直接复用为最终句柄，不碰 configx.New。
		return in.Probe, nil
	}
	group := strings.TrimSpace(in.Probe.GetString("configcenter.group"))
	if group == "" {
		group = "HUBX"
	}
	addr := nacosAddr(in.Probe)

	cfg, err := configx.New(&configx.ConfigOptions{
		ConfigFile: in.Path,
		ConfigType: "yaml",
		Remote: configx.RemoteConfig{
			Enabled:  true,
			Provider: "nacos",
			Nacos: configx.NacosConfig{
				ServerConfigs: []configx.NacosServerConfig{parseNacosAddr(addr)},
				Group:         group,
				DataID:        dataID,
			},
		},
	})
	// 新句柄已接管同一份本地文件的读取，成败都关闭探读句柄（所有权契约）。
	_ = in.Probe.Close()
	if err != nil {
		return nil, fmt.Errorf("configsrc: remote config dataId=%s group=%s addr=%s: %w", dataID, group, addr, err)
	}
	return cfg, nil
}

// nacosAddr 解析地址：env 优先 → yaml configcenter.addr → 缺省
// 127.0.0.1:8848（三裁决第 3 条；micro_apps EnvNacosServer 同名同义）。
func nacosAddr(probe configx.Config) string {
	addr := os.Getenv(EnvNacosServer)
	if addr == "" {
		addr = strings.TrimSpace(probe.GetString("configcenter.addr"))
	}
	if addr == "" {
		addr = DefaultNacosIP + ":" + strconv.Itoa(DefaultNacosPort)
	}
	return addr
}

// parseNacosAddr 拆 "ip:port"；port 缺省/非法时回落 8848。
func parseNacosAddr(addr string) configx.NacosServerConfig {
	host, portStr, _ := strings.Cut(addr, ":")
	if host == "" {
		host = DefaultNacosIP
	}
	port := uint64(DefaultNacosPort)
	if p, err := strconv.ParseUint(portStr, 10, 64); err == nil && p > 0 {
		port = p
	}
	return configx.NacosServerConfig{IP: host, Port: port}
}

func init() { Register("nacos", nacosLoader{}) }
```

**Step 3.2 — `internal/configsrc/nacos_test.go`（三断言）**

- [ ] 新建 `internal/configsrc/nacos_test.go`：

```go
package configsrc

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// requireNacos 底座探测（对齐全局约定 D6 的 e2e helper 语义）：
// 可达即返回；不可达时 HUBX_NACOS_INTEGRATION=1 → Fatal，否则 Skip。
func requireNacos(t *testing.T) {
	t.Helper()
	try := func() bool {
		c, err := net.DialTimeout("tcp", "127.0.0.1:8848", time.Second)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}
	if try() {
		return
	}
	gate := os.Getenv("HUBX_NACOS_INTEGRATION") != ""
	if !gate {
		t.Skip("nacos 127.0.0.1:8848 未启动：docker compose -f /Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml up -d nacos（HUBX_NACOS_INTEGRATION=1 时改为 Fatal）")
	}
	for i := 0; i < 15; i++ {
		if try() {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatal("nacos 127.0.0.1:8848 不可达（HUBX_NACOS_INTEGRATION=1 要求底座就绪）")
}

// TestNacosPureLocalWithoutDataID：data_id 空 → 纯本地，死端口 env 下仍成功，
// 证明未发起任何远端请求（spec「dataId 为空时 MUST 纯本地运行」）。
func TestNacosPureLocalWithoutDataID(t *testing.T) {
	t.Setenv(EnvNacosServer, "127.0.0.1:1")
	path := writeCfg(t, "configcenter:\n  type: nacos\nservice:\n  name: local-only\n  mode: monolith\n")

	h, err := Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load() error = %v (data_id 空应纯本地，不触碰远端)", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	if got := h.GetString("service.name"); got != "local-only" {
		t.Errorf("service.name = %q, want local-only", got)
	}
}

// TestNacosUnreachableIsFoundationError：type=nacos 已声明且地址不可达 →
// 地基失败即退（不降级回本地），错误串含 dataId 与地址上下文。
// 注：nacos-sdk gRPC 拨号有超时重试，本用例可能耗时数秒。
func TestNacosUnreachableIsFoundationError(t *testing.T) {
	t.Setenv(EnvNacosServer, "127.0.0.1:1")
	path := writeCfg(t, "configcenter:\n  type: nacos\n  data_id: hubx.launchx.b1-test\n  group: HUBX\nservice:\n  name: local-base\n")

	_, err := Load(context.Background(), path)
	if err == nil {
		t.Fatal("Load(unreachable) = nil error, want foundation error")
	}
	if !strings.Contains(err.Error(), "hubx.launchx.b1-test") {
		t.Errorf("error = %v, want contain dataId hubx.launchx.b1-test", err)
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("error = %v, want contain addr 127.0.0.1:1", err)
	}
}

// TestNacosKeyArbitration：验收案例 B1——本地与远程同键不同值时远程值生效、
// 未在远程出现的键保留本地值、远程独有键可见。
// 需底座 nacos（up -d nacos）；不可达且未设 HUBX_NACOS_INTEGRATION 时 Skip，
// 裸 go test ./... 保持全绿（D6 契约）。
func TestNacosKeyArbitration(t *testing.T) {
	requireNacos(t)

	const dataID = "hubx.launchx.b1-test"
	const group = "HUBX"

	// 灌远程配置（nacos OpenAPI v1，dockers 里 NACOS_AUTH_ENABLE=false 无需登录）。
	form := url.Values{
		"dataId":  {dataID},
		"group":   {group},
		"content": {"service:\n  name: from-remote\nremoteflag: remote-only\n"},
	}
	resp, err := http.PostForm("http://127.0.0.1:8848/nacos/v1/cs/configs", form)
	if err != nil {
		t.Fatalf("publish remote config: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("publish status = %d, want 200", resp.StatusCode)
	}
	t.Cleanup(func() {
		req, _ := http.NewRequest(http.MethodDelete,
			"http://127.0.0.1:8848/nacos/v1/cs/configs?dataId="+url.QueryEscape(dataID)+"&group="+url.QueryEscape(group), nil)
		if r, err := http.DefaultClient.Do(req); err == nil {
			_ = r.Body.Close()
		}
	})

	t.Setenv(EnvNacosServer, "127.0.0.1:8848")
	path := writeCfg(t, "configcenter:\n  type: nacos\n  data_id: hubx.launchx.b1-test\n  group: HUBX\nservice:\n  name: local-base\nlocalflag: local-only\n")

	h, err := Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	// 三裁决断言（specs/config-loading Scenario「验收案例 B1 — key 级裁决」）：
	if got := h.GetString("service.name"); got != "from-remote" {
		t.Errorf("service.name = %q, want from-remote（远程覆盖本地）", got)
	}
	if got := h.GetString("localflag"); got != "local-only" {
		t.Errorf("localflag = %q, want local-only（远程没有的键保留本地）", got)
	}
	if got := h.GetString("remoteflag"); got != "remote-only" {
		t.Errorf("remoteflag = %q, want remote-only（远程独有键可见）", got)
	}
}
```

**Step 3.3 — Run**

- [ ] 执行（从 `launchx/` 根）：

```bash
go test ./internal/configsrc/ -run Nacos -count=1
# B1 完整三断言（需底座）：
docker compose -f /Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml up -d nacos
HUBX_NACOS_INTEGRATION=1 go test ./internal/configsrc/ -run Nacos -count=1
go build ./...
```

Expected: 第一条命令 `ok github.com/gospacex/launchx/internal/configsrc`——`TestNacosUnreachableIsFoundationError`（错误串含 `hubx.launchx.b1-test` 与 `127.0.0.1:1`）与 `TestNacosPureLocalWithoutDataID` PASS，`TestNacosKeyArbitration` 显示 `--- SKIP`（未设 gate 且底座未起）；底座起来后带 `HUBX_NACOS_INTEGRATION=1` 再跑，三个 `TestNacos*` 全 PASS（B1：远程覆盖/本地保留/远程独有），不带 gate 时 Skip 消息给出 up 命令。`go build ./...` 无输出无错。

**偏差注记**（不改上方 sync 引用块）：plan-ready 验证方式为单条 `go test ./internal/configsrc/ -run Nacos` 三断言全过——本计划把 B1 的 key 级裁决做成**依赖 nacos 底座的集成断言**（不可达即 Skip），保证裸 `go test ./...` 无底座也全绿（D6 契约）；完整三断言 = 底座 up 后带 `HUBX_NACOS_INTEGRATION=1` 的同一命令。

**Step 3.4 — commit**

- [ ] `git add -A && git commit -m "feat(task-3): configsrc nacos 加载器三裁决与 B1 集成断言"`

- [ ] **Task complete**

---

### Task 4: component 注册表与四个内置组件

> **trace:** plan-ready.md Task 4 · tasks.md 1.4 · 验收案例 D1/D2

> **sync:** plan-ready.md Task 4 · tasks.md 1.4

> - [ ] **任务完成**（与 superpowers plan `Task 4`、`tasks.md` 对应条目同步勾选）
> - 目标：`internal/component`：`Register(name, Builder)` + db(mysql/gorm 经 dbx config loader、DSN env 展开、`Trace.On("db")` 门控 Open/OpenNoTrace)、cache、mq(producer)、registry(coordx consul Register/Deregister/Target 封装)；`strict.<name>` 裁决降级 Warn/失败即退；`trace.on` 只控入链不控构造（验收案例 D1/D2）
> - 改动文件：`internal/component/{registry.go,db.go,cache.go,mq.go,consul.go,strict.go}`、`internal/component/*_test.go`
> - 验证方式：`go test ./internal/component/` — strict 双分支、Trace.On 不改变构造、registry 失败带病运行断言通过（验收案例 D1/D2）
> - [ ] 1.4 实现 `internal/component` 注册表与内置 db/cache/mq/registry 构建器：`trace.on.<name>` 只控入链不控构造（对应验收案例 D1）、`strict.<name>` 裁决降级 Warn 或返回 error（对应验收案例 D2）、registry 失败带病运行

**Step 4.1 — 跨仓前置：`../coordx/consul` TTL 心跳修复（幂等核对/重放）**

> 这是本计划**唯一跨仓改动**（用户裁决选项 2，证据与实测记录见「环境事实」首条）。launchx 侧保持原技术栈：coordx 封装 + blank import，不直连 `consul/api`。修复已在盘（本会话实测通过）；干净执行环境按下述 post-state 核对/重放。

- [ ] 核对 `../coordx/consul/consul.go` 含全部修复点（任一不过则按 post-state 块修复）：

Run: `grep -c 'Interval:' ../coordx/consul/consul.go; grep -c 'ttlBeatInterval' ../coordx/consul/consul.go; grep -c 'startBeat' ../coordx/consul/consul.go`

Expected: 三个输出为 `0`（Check 结构不再带 Interval 字段）、`≥2`（常量声明 + ticker 使用）、`≥2`（注册处调用 + 方法声明）。

post-state（修复后关键区域，逐字为准；仅列变更区域，未列区域保持原文件不动）：

```go
import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gospacex/coordx"
	"github.com/gospacex/logx"
	_ "github.com/gospacex/logx/zap"
	"github.com/gospacex/traceflag"
	"github.com/hashicorp/consul/api"
	"go.opentelemetry.io/otel/attribute"
)

// ttlBeatInterval TTL 心跳周期：须小于 Check.TTL（30s），取 1/3 留足三倍余量。
const ttlBeatInterval = 10 * time.Second

type registerImpl struct {
	client *api.Client
	cfg    *coordx.Config
	logger logx.Logger
	tracer *traceflag.Tracer

	mu    sync.Mutex
	beats map[string]chan struct{} // instanceID -> stop channel
}
```

```go
	r := &registerImpl{
		client: c,
		cfg:    cfg,
		logger: logx.GetGlobal(),
		tracer: traceflag.NewTracer(cfg != nil && cfg.EnableTrace, "github.com/gospacex/coordx/consul", "coordx"),
		beats:  make(map[string]chan struct{}),
	}
```

```go
	reg := &api.AgentServiceRegistration{
		ID:      inst.ID,
		Name:    service,
		Address: inst.Addr,
		Port:    inst.Port,
		// TTL 检查与 Interval 互斥（Consul 校验：Interval and TTL cannot both be
		// specified），只保留 TTL，由 startBeat 周期性 UpdateTTL 维持 passing。
		Check: &api.AgentServiceCheck{
			TTL:                            "30s",
			Status:                         "passing",
			DeregisterCriticalServiceAfter: "90s",
		},
	}
	if err := r.client.Agent().ServiceRegister(reg); err != nil {
		span.RecordError(err)
		r.logger.ErrorContext(ctx, "coordx/consul register failed", logx.Err(err), logx.String("service", service))
		return err
	}
	r.startBeat(inst.ID)
```

```go
// startBeat 启动实例 TTL 心跳；同 ID 重复注册先停旧心跳，防止双写。
func (r *registerImpl) startBeat(id string) {
	if id == "" {
		return
	}
	r.stopBeat(id)
	stop := make(chan struct{})
	r.mu.Lock()
	r.beats[id] = stop
	r.mu.Unlock()
	go func() {
		t := time.NewTicker(ttlBeatInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if err := r.client.Agent().UpdateTTL("service:"+id, "", "passing"); err != nil {
					r.logger.WarnContext(context.Background(), "coordx/consul ttl heartbeat failed",
						logx.Err(err), logx.String("inst_id", id))
				}
			}
		}
	}()
}

// stopBeat 停止并移除实例 TTL 心跳（不存在时为 no-op）。
func (r *registerImpl) stopBeat(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if stop, ok := r.beats[id]; ok {
		close(stop)
		delete(r.beats, id)
	}
}
```

```go
	if err := r.client.Agent().ServiceDeregister(inst.ID); err != nil {
		span.RecordError(err)
		r.logger.ErrorContext(ctx, "coordx/consul deregister failed", logx.Err(err), logx.String("service", service))
		return err
	}
	r.stopBeat(inst.ID)
```

```go
func (r *registerImpl) HealthCheck() error { return nil }
func (r *registerImpl) Close() error {
	r.mu.Lock()
	for id, stop := range r.beats {
		close(stop)
		delete(r.beats, id)
	}
	r.mu.Unlock()
	return nil
}
```

Run: `cd ../coordx/consul && go build ./... && go vet ./...`；Expected: 两命令均无输出无错。

- [ ] coordx 仓单独提交（**不在 launchx 的任何 commit 内**）：

Run: `cd ../coordx && git add consul/consul.go && git commit -m "fix(consul): drop check interval, ttl heartbeat"`；Expected: 一条新 commit（该仓原为单提交 `f3d2fda`；若执行环境已提交过，`git status` 干净即视为通过）。

**Step 4.2 — `internal/component/registry.go`（Env/Builder 契约 + Register/Mount 挂载语义）**

- [ ] 新建 `internal/component/registry.go`：

```go
// Package component 是 launchx 的组件注册表：内置 db/cache/mq/registry
// 四个构建器 + presence 裁决（D3）+ strict 裁决的挂载段（D4）。
//
// 防环（D1）：Builder 只面向 Env（configx 句柄 + logger + 服务元信息 +
// TraceOn），不 import pipeline/launchx；挂载结果是 any，由 pipeline 按
// 注册名 type-switch 收进 Handle 字段（Task 1 契约：Cache/Registry/MQ/DB）。
//
// 事件语义（D2/D4）：本包只发 Warn，不发射事件——component.<name> 事件由
// pipeline 在 inst != nil 时记录；strict=false 失败（带病）pipeline 不记该事件。
package component

import (
	"context"
	"fmt"
	"sync"

	"github.com/gospacex/configx"
	"github.com/gospacex/logx"
)

// Env 是传给构建器的运行环境（D1 防环契约；由 pipeline 构造注入）。
type Env struct {
	Config  configx.Config
	Logger  logx.Logger
	Service string
	Mode    string
	// TraceOn 调用链开关（kind ∈ db|cache|mq|rpc…）：只控入链、不控构造
	// （D4）。注入语义见 configsrc.TraceOn（D4「Trace.On」唯一实现，Task 5 落地）；
	// nil 容错为全开。
	TraceOn func(kind string) bool
}

// traceOn 容错读取：nil 视为开——构造路径不因注入缺失而 panic。
func (e *Env) traceOn(kind string) bool {
	if e == nil || e.TraceOn == nil {
		return true
	}
	return e.TraceOn(kind)
}

// Builder 组件构建器：成功返回值由 pipeline 收进 Handle 字段。
type Builder func(ctx context.Context, env *Env) (any, error)

// entry 注册表条目：presence 裁决 + 降级后果文案随构建器一起登记。
type entry struct {
	builder     Builder
	requested   func(*Env) bool // D3 presence-based 启用裁决
	consequence string          // strict=false 降级 Warn 的后果文案（D4）
}

var (
	regMu    sync.RWMutex
	registry = map[string]entry{}
)

// Register 登记组件构建器（同名覆盖）：各组件文件 init() 调用；
// 测试可用 unregister 清理。
func Register(name string, b Builder, requested func(*Env) bool, consequence string) {
	regMu.Lock()
	defer regMu.Unlock()
	registry[name] = entry{builder: b, requested: requested, consequence: consequence}
}

// unregister 仅测试用：移除登记。
func unregister(name string) {
	regMu.Lock()
	defer regMu.Unlock()
	delete(registry, name)
}

// Mount 按 presence 裁决挂载组件：
//   - 未 requested      → (nil, false, nil)：本档/本配置不启用，不记事件；
//   - 构建成功          → (inst, true, nil)；
//   - 失败 strict=false → (nil, true, nil)：已 Warn（含后果文案）带病继续；
//   - 失败 strict=true  → (nil, true, error)：`init <name>: …`，Run 走完整退出链。
//
// 未登记名属编程/装配错误，直接返回 error（fail fast，不走 strict 裁决）。
func Mount(ctx context.Context, env *Env, name string) (any, bool, error) {
	regMu.RLock()
	e, ok := registry[name]
	regMu.RUnlock()
	if !ok {
		return nil, false, fmt.Errorf("component: %q not registered", name)
	}
	if e.requested != nil && !e.requested(env) {
		return nil, false, nil
	}
	inst, err := e.builder(ctx, env)
	if err != nil {
		if isStrict(env, name) {
			return nil, true, fmt.Errorf("init %s: %w", name, err)
		}
		lg := env.Logger
		if lg == nil {
			lg = logx.GetGlobal()
		}
		lg.Warn("component degraded, continuing",
			logx.String("component", name),
			logx.String("consequence", e.consequence),
			logx.Err(err))
		return nil, true, nil
	}
	return inst, true, nil
}
```

**Step 4.3 — `internal/component/strict.go`（strict 裁决挂载段 + 后果文案）**

- [ ] 新建 `internal/component/strict.go`：

```go
package component

// 降级后果文案（D4：strict=false 的 Warn 必须说明某项功能不可用）。
const (
	consequenceDB       = "ORM 访问不可用（依赖数据库的功能不可用）"
	consequenceCache    = "限流 fail-open、缓存穿透"
	consequenceMQ       = "消息发送不可用（outbox 停滞、异步链路中断）"
	consequenceRegistry = "服务注册/发现不可用"
)

// isStrict 读 `strict.<name>`（缺省 false）——strict 两段裁决的挂载段；
// Run 段（registry.Register 失败）同样经本函数读同一开关（D4）。
func isStrict(env *Env, name string) bool {
	if env == nil || env.Config == nil {
		return false
	}
	return env.Config.GetBool("strict." + name)
}
```

**Step 4.4 — `internal/component/db.go`（presence + DSN 展开 + Trace.On 门控 Open/OpenNoTrace + Ping 即退）**

- [ ] 新建 `internal/component/db.go`：

```go
package component

import (
	"context"
	"fmt"
	"time"

	dbxconfig "github.com/gospacex/dbx/config"
	dbxorm "github.com/gospacex/dbx/orm"
	"github.com/gospacex/launchx/internal/configsrc"
	"github.com/gospacex/logx"
	"go.opentelemetry.io/otel"
	"gorm.io/gorm"
)

func init() {
	Register("db", buildDB, requestedDB, consequenceDB)
}

// requestedDB（D3 presence）：xmiddleware.sqlx.databases.<service> 存在。
func requestedDB(env *Env) bool {
	if env == nil || env.Config == nil {
		return false
	}
	return env.Config.IsSet("xmiddleware.sqlx.databases." + env.Service)
}

// expandDB 包装 dbxconfig.DBConfig：DSN 里的 ${HUBX_DB_PW} 占位经
// configsrc.ExpandEnv 展开（D3「${ENV} 展开点唯一」；configx 不做 env
// 展开，占位会原样进 DSN，必须在 Open 前展开——srv-order openDB 同款）。
type expandDB struct {
	base dbxconfig.DBConfig
}

func (e expandDB) DriverName() string { return e.base.DriverName() }

func (e expandDB) DSN() string { return configsrc.ExpandEnv(e.base.DSN()) }

// buildDB 打开 GORM。Trace.On("db") 只门控 Open/OpenNoTrace（入链），
// 不决定构造与否（D1/D4）。
func buildDB(ctx context.Context, env *Env) (any, error) {
	driver := env.Config.GetString("xmiddleware.sqlx.databases." + env.Service + ".driver")
	if driver == "" {
		driver = "mysql"
	}
	// loader 内部已过 validateInstance（含 PoolConfig.Validate 与零值补默认），
	// 与 srv-order openDB 一致不重复 Validate。
	base, _, err := dbxconfig.LoadDBConfigFromConfigx(env.Config, driver, env.Service)
	if err != nil {
		return nil, fmt.Errorf("db: load config: %w", err)
	}
	opts := []gorm.Option{
		dbxorm.WithLogxLogger(env.Logger, dbxorm.LogxLoggerConfig{
			Level:                "warn",
			SlowThreshold:        200 * time.Millisecond,
			IgnoreRecordNotFound: true,
		}),
	}
	// 【必须】dbx 的 traceOn 分支会自建 TracerProvider 顶掉全局（srv-order
	// openDB:313-316 同款理由），夺回：defer 恢复装配层的 provider。
	// Task 6 起 prev 即 telemetry.Init 装好的真 provider。
	prev := otel.GetTracerProvider()
	defer otel.SetTracerProvider(prev)
	src := expandDB{base: base}
	var db *gorm.DB
	if env.traceOn("db") {
		db, err = dbxorm.Open(src, opts...)
	} else {
		db, err = dbxorm.OpenNoTrace(src, opts...)
	}
	if err != nil {
		return nil, fmt.Errorf("db: open: %w", err)
	}
	// 连接池：PoolConfig 单位为秒（<=0 已被 loader 补默认），显式应用到 *sql.DB。
	if sqlDB, perr := db.DB(); perr == nil {
		if p := base.GetPool(); p != nil {
			if p.MaxOpenConns > 0 {
				sqlDB.SetMaxOpenConns(p.MaxOpenConns)
			}
			if p.MaxIdleConns > 0 {
				sqlDB.SetMaxIdleConns(p.MaxIdleConns)
			}
			if p.ConnMaxLifetime > 0 {
				sqlDB.SetConnMaxLifetime(time.Duration(p.ConnMaxLifetime) * time.Second)
			}
			if p.ConnMaxIdleTime > 0 {
				sqlDB.SetConnMaxIdleTime(time.Duration(p.ConnMaxIdleTime) * time.Second)
			}
		}
		// Ping 即退：数据库不可用 = 构建失败，交给 strict 裁决
		// （strict=false → 带病运行，strict=true → init db 即退；验收案例 D2）。
		if pingErr := sqlDB.PingContext(ctx); pingErr != nil {
			_ = sqlDB.Close()
			return nil, fmt.Errorf("db: ping: %w", pingErr)
		}
	}
	env.Logger.Info("db ready",
		logx.String("db.driver", driver),
		logx.Any("db.trace", env.traceOn("db")))
	return db, nil
}
```

**Step 4.5 — `internal/component/cache.go`（presence + 字段移植 drivers.NewCache + CacheHandle 实现）**

- [ ] 新建 `internal/component/cache.go`：

```go
package component

import (
	"context"
	"fmt"

	"github.com/gospacex/cachex"
	"github.com/gospacex/logx"
	"go.opentelemetry.io/otel"
	// 驱动自注册（blank import）：redis 驱动在包内 init() 注册 creator。
	_ "github.com/gospacex/cachex/redis"
)

func init() {
	Register("cache", buildCache, requestedCache, consequenceCache)
}

// requestedCache（D3 presence）：instances.<service>.addr 非空。
func requestedCache(env *Env) bool {
	if env == nil || env.Config == nil {
		return false
	}
	return env.Config.GetString("xmiddleware.cachex.instances."+env.Service+".addr") != ""
}

// Cache 是 launchx.CacheHandle 的具体实现（Task 1 契约；不 import launchx，
// 方法集自然满足）。
type Cache struct {
	Instances cachex.Instances
}

// Instance 按名取缓存实例。
func (c *Cache) Instance(name string) cachex.Cache {
	if c == nil {
		return nil
	}
	return c.Instances[name]
}

// Close 释放全部缓存实例（首错返回）。
func (c *Cache) Close() error {
	if c == nil {
		return nil
	}
	var firstErr error
	for name, inst := range c.Instances {
		if err := inst.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("component: close cache %q: %w", name, err)
		}
	}
	return firstErr
}

// buildCache 构造缓存实例：字段映射逐字段移植 micro_apps/drivers.NewCache。
// 注意 redis 驱动 NewClient 即建连（PING），失败 = 构建失败 → strict 裁决。
func buildCache(_ context.Context, env *Env) (any, error) {
	name := env.Service
	if name == "" {
		name = "default"
	}
	instKey := "xmiddleware.cachex.instances." + name
	cfg := &cachex.Config{
		Name:           name,
		Driver:         env.Config.GetString(instKey + ".driver"),
		Addr:           env.Config.GetString(instKey + ".addr"),
		Password:       env.Config.GetString(instKey + ".password"),
		DB:             env.Config.GetInt(instKey + ".db"),
		PoolSize:       env.Config.GetInt(instKey + ".pool_size"),
		MinIdleConn:    env.Config.GetInt(instKey + ".min_idle_conns"),
		Timeout:        env.Config.GetDuration(instKey + ".dial_timeout"),
		ReadTimeout:    env.Config.GetDuration(instKey + ".read_timeout"),
		WriteTimeout:   env.Config.GetDuration(instKey + ".write_timeout"),
		RetryCount:     env.Config.GetInt(instKey + ".max_retries"),
		SentinelMaster: env.Config.GetString(instKey + ".sentinel_master"),
		SentinelAddrs:  env.Config.GetString(instKey + ".sentinel_addrs"),
		Cluster:        env.Config.GetString(instKey + ".cluster"),
		TLSEnabled:     env.Config.GetBool(instKey + ".tls_enabled"),
	}
	if cfg.Driver == "" {
		cfg.Driver = env.Config.GetString("xmiddleware.cachex.driver")
	}
	if cfg.Addr == "" {
		return nil, fmt.Errorf("cache: instance %q missing addr", name)
	}
	// 调用链全局闸（D4：Trace.On 只控入链）：redis 驱动 startSpan 先查它。
	cachex.SetTracingConfig(cachex.TracingConfig{Enabled: env.traceOn("cache")})
	// 【必须】cachex 打开 trace 时会自建 TracerProvider 顶掉全局（drivers.go
	// 同款理由），夺回；Task 6 起 prev 即 telemetry 的真 provider。
	prev := otel.GetTracerProvider()
	defer otel.SetTracerProvider(prev)
	insts, err := cachex.New([]*cachex.Config{cfg})
	if err != nil {
		return nil, fmt.Errorf("cache: open %q (driver=%s): %w", name, cfg.Driver, err)
	}
	env.Logger.Info("cache ready",
		logx.String("cache.driver", cfg.Driver),
		logx.Any("cache.trace", env.traceOn("cache")))
	return &Cache{Instances: insts}, nil
}
```

**Step 4.6 — `internal/component/mq.go`（presence + readProducerConfig/applyTrace + topicRouter 全文内联）**

> `topicRouter` 不可从 micro_apps import（跨 module），按 drivers.go:149-359 全文内联；唯一裁剪：去掉 `fixed` 字段与 `NewProducerWithTopic`（launchx 无「装配期钉死 topic」需求），`producerFor` 的 key 即 topic。日志前缀 `drivers:` 改 `component:`。理由注释（kafka Writer 双 topic 坑）必须原样保留。

- [ ] 新建 `internal/component/mq.go`：

```go
package component

import (
	"context"
	"fmt"
	"sync"

	"github.com/gospacex/configx"
	"github.com/gospacex/logx"
	"github.com/gospacex/mqx"
	// 驱动自注册（blank import）：kafka producer 在包内 init() 注册 creator。
	_ "github.com/gospacex/mqx/kafka"
)

func init() {
	Register("mq", buildMQ, requestedMQ, consequenceMQ)
}

// requestedMQ（D3 presence）：instances.<service>.brokers 非空。
func requestedMQ(env *Env) bool {
	if env == nil || env.Config == nil {
		return false
	}
	return len(env.Config.GetSlice("xmiddleware.mqx.instances."+env.Service+".brokers")) > 0
}

// buildMQ 构造消息生产者。trace 开关只透传（D4），构造不依赖入链：
// kafka 建连是惰性的（首次 Send 才拨号），故本步几乎不失败。
func buildMQ(_ context.Context, env *Env) (any, error) {
	base, err := readProducerConfig(env.Config, env.Service)
	if err != nil {
		return nil, err
	}
	applyTrace(base, env.traceOn("mq"))
	env.Logger.Info("mq ready",
		logx.String("mq.driver", base.Driver),
		logx.Any("mq.trace", env.traceOn("mq")))
	return &topicRouter{cfg: base, traceOn: env.traceOn("mq")}, nil
}

// readProducerConfig 读 mqx 实例配置（移植 drivers.go:149-158）。
func readProducerConfig(h configx.Config, name string) (*mqx.Config, error) {
	cfg := mqx.ReadConfigFromConfigx(h, name)
	if cfg.Driver == "" {
		cfg.Driver = "kafka"
	}
	if len(cfg.Brokers) == 0 {
		return nil, fmt.Errorf("component: mq instance %q missing brokers", orDefault(name))
	}
	return cfg, nil
}

// applyTrace 调用链开关透传（移植 drivers.go:355-359；本层不读 trace 配置）。
func applyTrace(cfg *mqx.Config, on bool) {
	cfg.CallChainEnabled = on
	cfg.EnableTrace = on
}

// topicRouter 按 Message.Topic 懒装配底层 producer 的路由层。
//
// 【必须存在的理由（实测坑，勿回退，移植 drivers.go 注释原文）】mqx 的
// kafka producer 把 Writer.Topic 取成 cfg.Consumer.Topic，且 mqx 的配置
// 归一化会把空 topic 兜成 "default-topic"（interface.go:263）——也就是
// 说 Writer.Topic **永不为空**。而 kafka-go 的 Writer 一旦自带 Topic，
// Message.Topic 再非空就直接报
//
//	"kafka.(*Writer): Topic must not be specified for both Writer and Message"
//
// 症状极隐蔽：调用链日志照打、send span 照常产出，实际一条都没发出去
// —— outbox 行静默重试到 retry_count=10 后被放弃，Kafka 里空空如也。
// 路由层把约束收在一处：对外仍按 Message.Topic 语义工作，对内一 topic
// 一 Writer（send 前把 Message.Topic 清空）。
type topicRouter struct {
	cfg     *mqx.Config
	traceOn bool

	mu       sync.Mutex
	byTopic  map[string]mqx.Producer
	fallback mqx.Producer // Message.Topic 为空时的兜底（走配置默认 topic）
}

// producerFor 取该 topic 的底层 producer（懒装配 + 缓存）。
func (t *topicRouter) producerFor(topic string) (mqx.Producer, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if topic == "" {
		if t.fallback == nil {
			var err error
			if t.fallback, err = mqx.NewProducer(t.cfg); err != nil {
				return nil, err
			}
		}
		return t.fallback, nil
	}
	if p, ok := t.byTopic[topic]; ok {
		return p, nil
	}
	cfg := *t.cfg // 逐 topic 复制一份：Writer.Topic 是 per-producer 的
	cfg.Consumer.Topic = topic
	p, err := mqx.NewProducer(&cfg)
	if err != nil {
		return nil, err
	}
	if t.byTopic == nil {
		t.byTopic = map[string]mqx.Producer{}
	}
	t.byTopic[topic] = p
	return p, nil
}

// Send 单条发送：按 Message.Topic 选 producer，并把 Topic 清空后下发。
func (t *topicRouter) Send(ctx context.Context, msg *mqx.Message) error {
	if msg == nil {
		return fmt.Errorf("component: nil message")
	}
	p, err := t.producerFor(msg.Topic)
	if err != nil {
		return fmt.Errorf("component: producer for topic %q: %w", msg.Topic, err)
	}
	out := *msg
	out.Topic = "" // Writer 已绑定 topic，再带就报双 topic 错误
	return p.Send(ctx, &out)
}

// SendBatch 批量发送：先按 topic 分组，再逐组下发（组内同样清空 Topic）。
func (t *topicRouter) SendBatch(ctx context.Context, msgs []*mqx.Message) error {
	groups := map[string][]*mqx.Message{}
	for _, m := range msgs {
		if m == nil {
			continue
		}
		groups[m.Topic] = append(groups[m.Topic], m)
	}
	for topic, group := range groups {
		p, err := t.producerFor(topic)
		if err != nil {
			return fmt.Errorf("component: producer for topic %q: %w", topic, err)
		}
		outs := make([]*mqx.Message, 0, len(group))
		for _, m := range group {
			out := *m
			out.Topic = ""
			outs = append(outs, &out)
		}
		if err := p.SendBatch(ctx, outs); err != nil {
			return err
		}
	}
	return nil
}

// Close 关闭全部懒装配出来的底层 producer。
func (t *topicRouter) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	var firstErr error
	for topic, p := range t.byTopic {
		if err := p.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("component: close producer %q: %w", topic, err)
		}
	}
	if t.fallback != nil {
		if err := t.fallback.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	t.byTopic = nil
	t.fallback = nil
	return firstErr
}

// Name 驱动名（取底层配置，与 mqx.Producer 契约一致）。
func (t *topicRouter) Name() string { return t.cfg.Driver }

// HealthCheck 全部底层 producer 健康才健康（未装配时视为健康：尚未建连）。
func (t *topicRouter) HealthCheck() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, p := range t.byTopic {
		if !p.HealthCheck() {
			return false
		}
	}
	if t.fallback != nil && !t.fallback.HealthCheck() {
		return false
	}
	return true
}

// orDefault（移植 drivers.go:361-366）。
func orDefault(name string) string {
	if name == "" {
		return "default"
	}
	return name
}
```

**Step 4.7 — `internal/component/consul.go`（coordx 封装 + RegistryHandle 实现 + 惰性地址发现）**

- [ ] 新建 `internal/component/consul.go`：

```go
package component

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/gospacex/coordx"
	"github.com/gospacex/logx"
	// 驱动自注册（blank import）：consul 的 Register/DiscoverCreator 在包内
	// init() 登记，coordx.NewRegister/NewDiscover 按 cfg.Driver 分发（Step 4.1）。
	_ "github.com/gospacex/coordx/consul"
)

func init() {
	Register("registry", buildRegistry, requestedRegistry, consequenceRegistry)
}

// requestedRegistry（D4 mode 表）：仅 bff/gateway/srv 挂 registry，
// monolith 与非法/空 mode（mode.Validate 已在装配前兜底）不挂。
func requestedRegistry(env *Env) bool {
	if env == nil {
		return false
	}
	switch env.Mode {
	case "bff", "gateway", "srv":
		return true
	default:
		return false
	}
}

// consulHandle 是 launchx.RegistryHandle 的具体实现（Task 1 契约；
// 不 import launchx）。结构体字段即惰性形态：NewRegister/NewDiscover
// 只建 client 不拨号，Mount 段几乎不失败；注册失败的主形态（consul 不可达）
// 在 Run 段经 Register() 返回，由 strict.registry 两段裁决（D4）。
type consulHandle struct {
	reg  coordx.Register
	disc coordx.Discover
	svc  string
	inst *coordx.ServiceInstance
}

// buildRegistry 构造注册/发现句柄。
func buildRegistry(_ context.Context, env *Env) (any, error) {
	// 监听地址单一事实源 service.addr（D3），webx.addr 兜底。
	srvAddr := env.Config.GetString("service.addr")
	if srvAddr == "" {
		srvAddr = env.Config.GetString("webx.addr")
	}
	if srvAddr == "" {
		return nil, fmt.Errorf("registry: missing listen address (service.addr/webx.addr)")
	}
	host, portStr, err := net.SplitHostPort(srvAddr)
	if err != nil {
		return nil, fmt.Errorf("registry: parse addr %q: %w", srvAddr, err)
	}
	// 0.0.0.0/空/:: 是监听通配，不能进注册表——本地发现回环改写。
	switch host {
	case "0.0.0.0", "", "::":
		host = "127.0.0.1"
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("registry: parse port %q: %w", portStr, err)
	}
	cfg := &coordx.Config{
		Driver:      env.Config.GetString("registry.driver"),
		Name:        env.Config.GetString("registry.name"),
		Addr:        env.Config.GetString("registry.addr"),
		EnableTrace: env.Config.GetBool("registry.enable_trace"),
	}
	if cfg.Driver == "" {
		cfg.Driver = "consul" // D3 缺省 driver
	}
	if cfg.Name == "" {
		cfg.Name = env.Service // D3 `name: <svc>`
	}
	reg, err := coordx.NewRegister(cfg)
	if err != nil {
		return nil, fmt.Errorf("registry: register driver: %w", err)
	}
	disc, err := coordx.NewDiscover(cfg)
	if err != nil {
		_ = reg.Close()
		return nil, fmt.Errorf("registry: discover driver: %w", err)
	}
	inst := &coordx.ServiceInstance{
		ID:        fmt.Sprintf("%s@%s:%d", env.Service, host, port),
		ServiceID: env.Service,
		Addr:      host,
		Port:      port,
		Healthy:   true,
	}
	env.Logger.Info("registry ready",
		logx.String("registry.addr", cfg.Addr),
		logx.String("registry.service", env.Service))
	return &consulHandle{reg: reg, disc: disc, svc: env.Service, inst: inst}, nil
}

// Register 向注册中心登记本实例（Run 段两段裁决的第②段调用点，D4）。
func (h *consulHandle) Register(ctx context.Context) error {
	return h.reg.Register(ctx, h.svc, h.inst)
}

// Deregister 摘除本实例（退出链 deregister 事件对应调用点，D5）。
func (h *consulHandle) Deregister(ctx context.Context) error {
	return h.reg.Deregister(ctx, h.svc, h.inst)
}

// Target 解析下游服务地址：逗号拼接全部健康实例 "host:port"；
// 无健康实例返回 error（gateway/bff 下游解析的失败形态）。
func (h *consulHandle) Target(ctx context.Context, service string) (string, error) {
	insts, err := h.disc.Discovery(ctx, service)
	if err != nil {
		return "", fmt.Errorf("registry: discover %q: %w", service, err)
	}
	var addrs []string
	for _, in := range insts {
		if in == nil || !in.Healthy || in.Addr == "" {
			continue
		}
		addrs = append(addrs, fmt.Sprintf("%s:%d", in.Addr, in.Port))
	}
	if len(addrs) == 0 {
		return "", fmt.Errorf("registry: no healthy instance for %q", service)
	}
	return strings.Join(addrs, ","), nil
}

// Close 关闭注册与发现资源（首错返回）。
func (h *consulHandle) Close() error {
	var firstErr error
	if err := h.reg.Close(); err != nil {
		firstErr = err
	}
	if err := h.disc.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}
```

**Step 4.8 — `internal/component/registry_test.go` + `internal/component/consul_test.go`（D1/D2/带病 + live 闭环）**

- [ ] 新建 `internal/component/registry_test.go`：

```go
package component

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gospacex/configx"
	"github.com/gospacex/logx"
)

// testEnv 从 yaml 构造 Env（TraceOn 默认全开，用例可覆盖）。
func testEnv(t *testing.T, yamlBody string) *Env {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yamlBody), 0o644); err != nil {
		t.Fatal(err)
	}
	cc, err := configx.ReadConfig(configx.ConfigOptions{ConfigFile: path, ConfigType: "yaml"})
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	return &Env{
		Config:  cc,
		Logger:  logx.GetGlobal(),
		Service: cc.GetString("service.name"),
		Mode:    cc.GetString("service.mode"),
		TraceOn: func(string) bool { return true },
	}
}

func TestMountStrictTwoBranches(t *testing.T) {
	failing := func(ctx context.Context, env *Env) (any, error) {
		return nil, errors.New("boom")
	}
	alwaysTrue := func(*Env) bool { return true }
	Register("fake1", failing, alwaysTrue, consequenceDB)
	t.Cleanup(func() { unregister("fake1") })

	t.Run("strict_true_returns_error", func(t *testing.T) {
		env := testEnv(t, "service: { name: t1, mode: monolith }\nstrict: { fake1: true }\n")
		inst, mounted, err := Mount(context.Background(), env, "fake1")
		if err == nil || !strings.Contains(err.Error(), "init fake1") {
			t.Fatalf("err = %v, want contain %q", err, "init fake1")
		}
		if mounted != true || inst != nil {
			t.Fatalf("got inst=%v mounted=%v, want nil/true", inst, mounted)
		}
	})

	t.Run("strict_false_degrades_to_warn", func(t *testing.T) {
		env := testEnv(t, "service: { name: t1, mode: monolith }\nstrict: { fake1: false }\n")
		inst, mounted, err := Mount(context.Background(), env, "fake1")
		if err != nil || inst != nil || mounted != true {
			t.Fatalf("got inst=%v mounted=%v err=%v, want nil/true/nil", inst, mounted, err)
		}
	})
}

func TestMountSkipsWhenNotRequested(t *testing.T) {
	calls := 0
	Register("fake2", func(ctx context.Context, env *Env) (any, error) {
		calls++
		return "x", nil
	}, func(*Env) bool { return false }, consequenceCache)
	t.Cleanup(func() { unregister("fake2") })

	inst, mounted, err := Mount(context.Background(), testEnv(t, "service: { name: t2, mode: monolith }\n"), "fake2")
	if inst != nil || mounted || err != nil || calls != 0 {
		t.Fatalf("got %v/%v/%v calls=%d, want nil/false/nil calls=0", inst, mounted, err, calls)
	}
}

func TestMountUnknownNameFailsFast(t *testing.T) {
	_, _, err := Mount(context.Background(), testEnv(t, "service: { name: t3, mode: monolith }\n"), "nope")
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("err = %v, want contain %q", err, "not registered")
	}
}

// TestMQConstructsWhenTraceOff（验收案例 D1）：trace.on 关闭时组件仍构造
// ——mq 惰性建连（首次 Send 才拨号），presence 满足即挂载成功、实例非 nil。
func TestMQConstructsWhenTraceOff(t *testing.T) {
	env := testEnv(t, `service: { name: demo, mode: monolith }
xmiddleware: { mqx: { instances: { demo: { brokers: ["127.0.0.1:1"] } } } }
`)
	env.TraceOn = func(string) bool { return false }
	inst, mounted, err := Mount(context.Background(), env, "mq")
	if err != nil || !mounted || inst == nil {
		t.Fatalf("got inst=%v mounted=%v err=%v, want constructed instance", inst, mounted, err)
	}
	r, ok := inst.(*topicRouter)
	if !ok || r.traceOn != false {
		t.Fatalf("topicRouter traceOn = %v, want false（开关只入链不改构造）", r.traceOn)
	}
}

// TestPresenceByModeAndYaml（D3 presence + D4 mode 表）。
func TestPresence(t *testing.T) {
	yamlDB := "service: { name: p, mode: monolith }\nxmiddleware: { sqlx: { databases: { p: { driver: mysql, dsn: x } } } }\n"
	yamlCache := "service: { name: p, mode: monolith }\nxmiddleware: { cachex: { instances: { p: { addr: 127.0.0.1:6379 } } } }\n"
	yamlMQ := "service: { name: p, mode: monolith }\nxmiddleware: { mqx: { instances: { p: { brokers: [\"127.0.0.1:9092\"] } } } }\n"

	if !requestedDB(testEnv(t, yamlDB)) {
		t.Error("requestedDB = false, want true")
	}
	if requestedDB(testEnv(t, "service: { name: p, mode: monolith }\n")) {
		t.Error("requestedDB(no sqlx) = true, want false")
	}
	if !requestedCache(testEnv(t, yamlCache)) {
		t.Error("requestedCache = false, want true")
	}
	if requestedCache(testEnv(t, "service: { name: p, mode: monolith }\n")) {
		t.Error("requestedCache(no addr) = true, want false")
	}
	if !requestedMQ(testEnv(t, yamlMQ)) {
		t.Error("requestedMQ = false, want true")
	}
	for mode, want := range map[string]bool{"monolith": false, "bff": true, "gateway": true, "srv": true} {
		env := testEnv(t, "service: { name: p, mode: "+mode+" }\n")
		env.Mode = mode
		if got := requestedRegistry(env); got != want {
			t.Errorf("requestedRegistry(mode=%s) = %v, want %v", mode, got, want)
		}
	}
}
```

- [ ] 新建 `internal/component/consul_test.go`：

```go
package component

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// TestConsulRunSegmentDegrades（验收案例 D2 / 「registry 失败带病运行」）：
// builder 段构造成功（client 不拨号），Run 段 Register() 对不可达注册中心
// 返回 error 而非 panic——pipeline 据此走 strict.registry 两段裁决（D4）。
func TestConsulRunSegmentDegrades(t *testing.T) {
	env := testEnv(t, `service: { name: t4-deg, mode: bff, addr: 127.0.0.1:19999 }
registry: { driver: consul, addr: http://127.0.0.1:1, enable_trace: false, name: t4-deg }
`)
	inst, mounted, err := Mount(context.Background(), env, "registry")
	if err != nil || !mounted || inst == nil {
		t.Fatalf("Mount: inst=%v mounted=%v err=%v, want constructed", inst, mounted, err)
	}
	h := inst.(*consulHandle)
	defer func() { _ = h.Close() }()
	if err := h.Register(context.Background()); err == nil {
		t.Fatal("Register(unreachable) = nil, want error（带病运行：错误返回不 panic）")
	}
}

// TestConsulMountDegradesWhenAddrMissing（Mount 段 strict=false 带病）。
func TestConsulMountDegradesWhenAddrMissing(t *testing.T) {
	env := testEnv(t, "service: { name: t4-addr, mode: bff }\nregistry: { driver: consul, addr: http://127.0.0.1:1, enable_trace: false }\n")
	inst, mounted, err := Mount(context.Background(), env, "registry")
	if err != nil || inst != nil || mounted != true {
		t.Fatalf("got inst=%v mounted=%v err=%v, want nil/true/nil（strict 缺省 false 带病）", inst, mounted, err)
	}
}

// TestConsulLiveRegisterDiscovery：底座 up 时的真实注册-发现-注销闭环（D6）。
// 不带 gate 且底座不可达 → Skip（Skip 消息给 up 命令）；LAUNCHX_E2E=1 且
// 不可达 → Fatal（e2e 必须真跑）。
func TestConsulLiveRegisterDiscovery(t *testing.T) {
	addr := "127.0.0.1:8500"
	if conn, err := net.DialTimeout("tcp", addr, 2*time.Second); err != nil {
		if os.Getenv("LAUNCHX_E2E") != "" {
			t.Fatalf("consul unreachable at %s — run: docker compose -f /Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml up -d consul (%v)", addr, err)
		}
		t.Skipf("consul not up at %s — start with: docker compose -f /Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml up -d consul", addr)
	} else {
		_ = conn.Close()
	}

	env := testEnv(t, `service: { name: t4-live, mode: bff, addr: 127.0.0.1:19999 }
registry: { driver: consul, addr: 127.0.0.1:8500, enable_trace: false, name: t4-live }
`)
	inst, mounted, err := Mount(context.Background(), env, "registry")
	if err != nil || !mounted || inst == nil {
		t.Fatalf("Mount: inst=%v mounted=%v err=%v", inst, mounted, err)
	}
	h := inst.(*consulHandle)
	t.Cleanup(func() { _ = h.Close() })

	if err := h.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	target, err := h.Target(context.Background(), "t4-live")
	if err != nil {
		t.Fatalf("Target: %v", err)
	}
	if !strings.Contains(target, "19999") {
		t.Fatalf("Target = %q, want contain :19999", target)
	}
	if err := h.Deregister(context.Background()); err != nil {
		t.Fatalf("Deregister: %v", err)
	}
}
```

**Step 4.9 — Run**

- [ ] Run:

```
go mod tidy
go vet ./...
go test ./internal/component/ -count=1
go build ./...
cd ../coordx/consul && go build ./... && go vet ./...
```

Expected: 第三条命令输出 `ok github.com/gospacex/launchx/internal/component`——`TestMountStrictTwoBranches`（两子 PASS）、`TestMountSkipsWhenNotRequested`、`TestMountUnknownNameFailsFast`、`TestMQConstructsWhenTraceOff`、`TestPresence`、`TestConsulRunSegmentDegrades`、`TestConsulMountDegradesWhenAddrMissing` 全 PASS；`TestConsulLiveRegisterDiscovery` 在本机 consul（D6 底座）已起时 PASS（注册→Target 含 19999→注销），未起时 `--- SKIP` 且消息含 up 命令；`go mod tidy`/`go vet`/`go build` 无输出无错；coordx 子仓 build/vet 无输出无错（Step 4.1 复验）。

**Step 4.10 — commit**

- [ ] `git add -A && git commit -m "feat(task-4): component 注册表与 db/cache/mq/registry 内置构建器"`

- [ ] **Task complete**

---

### Task 5: mode 四档装配分支

> **trace:** plan-ready.md Task 5 · tasks.md 1.5 · specs/mode-assembly「四档 mode 装配分支」「未知 mode 拒绝启动」

> **sync:** plan-ready.md Task 5 · tasks.md 1.5

> - [ ] **任务完成**（与 superpowers plan `Task 5`、`tasks.md` 对应条目同步勾选）
> - 目标：`internal/mode`：`Assemble(app) error` 按 `service.mode` 分支——monolith/bff/gateway 构造 `webx.App`（Addr 来自 yaml、Tracer/Logger 注入、PreRun 挂路由）并按档决定 consul 注册与下游客户端工厂；srv 不建 webx、提供 rpcx ServerOptions 与注册；非法/缺失 mode 返回含 `monolith|bff|srv|gateway` 合法取值的 error（验收案例 C1/C2/C3）
> - 改动文件：`internal/mode/{mode.go,webx.go,srv.go,registry.go}`、`internal/mode/mode_test.go`
> - 验证方式：`go test ./internal/mode/` — 四档装配断言 + 非法 mode 报错断言通过（验收案例 C1/C2/C3）
> - [ ] 1.5 实现 `internal/mode` 四档装配分支：monolith/bff/srv/gateway 按映射表装配服务端与 consul 发现，非法/缺失 mode 返回含合法取值的 error（对应验收案例 C1/C2/C3，C4 为零改动约束）

**Step 5.1 — `internal/rpcx/options.go`（kitex server/client 选项封装，移植 micro_apps）**

- [ ] 新建 `internal/rpcx/options.go`（源：`micro_apps/pkg/rpcx/options.go` 155 行；仅重写包注释、改 3 处 micro 路径/存储注释，代码逐行不变）：

```go
// Package rpcx 收敛 kitex 服务端/客户端 Option 的装配（移植自
// micro_apps/pkg/rpcx：代码不变，仅包注释与 3 处 micro 路径注释适配）。
//
// 依赖面只有 kitex 官方 API + kitex-contrib/obs-opentelemetry 的 tracing
// suite，经 go mod tidy 随公共模块引入（kitex v0.16.3 /
// obs-opentelemetry v0.3.0，与 micro_apps 同版，无 replace）。
//
// 调用链：套件挂上 obs-opentelemetry 的 Server/Client Suite 后，kitex 的
// 入站/出站调用会经 propagator 注入与提取 traceparent —— 这是跨服务续链的
// 落点。全局 TracerProvider 必须**先**由 internal/telemetry（Task 6）装配好，
// 本套件只是从 otel.GetTracerProvider() 取用（见 ServerOptions.TraceEnabled 注释）。
package rpcx

import (
	"strings"
	"time"

	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/pkg/utils"
	"github.com/cloudwego/kitex/server"
	kitextracing "github.com/kitex-contrib/obs-opentelemetry/tracing"
)

// 缺省超时。不填 ClientOptions/ServerOptions 的时长字段时用这些值，
// 避免出现「零值 = 立即超时」或「零值 = 无限等待」两种都不期望的行为。
const (
	// defaultRPCTimeout 单 RPC 调用超时（含重试）
	defaultRPCTimeout = 3 * time.Second
	// defaultConnectTimeout 建连超时
	defaultConnectTimeout = 500 * time.Millisecond
	// defaultReadWriteTimeout 服务端读写超时
	defaultReadWriteTimeout = 30 * time.Second
	// defaultExitWait 优雅退出时等待 in-flight 请求排空的上限
	defaultExitWait = 5 * time.Second
)

// ServerOptions kitex 服务端装配参数
type ServerOptions struct {
	// ServiceName 服务名（与 consul 注册的 ServiceID 对齐）
	ServiceName string
	// Addr 监听地址，形如 "127.0.0.1:9013"
	Addr string
	// TraceEnabled 是否挂 obs-opentelemetry 服务端套件（跨服务续链）。
	// 关闭时不装任何中间件与 meta handler：非 recording span 的开销比
	// 中间件本身还小，没必要为一个 noop 链路付出 fixed cost。
	TraceEnabled bool
	// ExitWait 优雅退出排空等待时长，0 用 defaultExitWait
	ExitWait time.Duration
	// ReadWriteTimeout 连接读写超时，0 用 defaultReadWriteTimeout
	ReadWriteTimeout time.Duration
}

// BuildServerOptions 生成 kitex server.NewServer 的选项列表。
//
// 注意顺序：service name 之后必须先带 ServiceAddr，再叠加 tracing suite ——
// suite 内部也注册 option，后注册的 WithTransportProtocol 等会覆盖同位配置，
// 把地址放在前面可以保证它不会被 suite 的传输设置冲掉。
func BuildServerOptions(o ServerOptions) []server.Option {
	exitWait := o.ExitWait
	if exitWait <= 0 {
		exitWait = defaultExitWait
	}
	rwTimeout := o.ReadWriteTimeout
	if rwTimeout <= 0 {
		rwTimeout = defaultReadWriteTimeout
	}

	opts := []server.Option{
		server.WithServiceAddr(utils.NewNetAddr("tcp", o.Addr)),
		server.WithExitWaitTime(exitWait),
		server.WithReadWriteTimeout(rwTimeout),
	}
	if o.TraceEnabled {
		opts = append(opts, server.WithSuite(kitextracing.NewServerSuite()))
	}
	return opts
}

// ClientOptions kitex 客户端装配参数
type ClientOptions struct {
	// ServiceName 调用方自身的服务名（用于 span 的 source peer 属性）
	ServiceName string
	// Target 目标地址，逗号分隔的多实例 "host:port,host:port"。
	// 由 internal/component 的 RegistryHandle.Target 读出并拼接。
	Target string
	// ConnType 连接形态："short" 短连接 / "long" 长连接（默认长连接）。
	// 演示环境实例少且 Leader 会漂移，默认短连接更稳：连接断了下一轮新建。
	ConnType string
	// TraceEnabled 同 ServerOptions.TraceEnabled
	TraceEnabled bool
	// RPCTimeout 单调用超时，0 用 defaultRPCTimeout
	RPCTimeout time.Duration
	// ConnectTimeout 建连超时，0 用 defaultConnectTimeout
	ConnectTimeout time.Duration
}

// BuildClientOptions 生成 kitex client.NewClient 的选项列表。
//
// Target 为空时不产出 WithHostPorts：kitex 会回退到 DestService 的默认解析，
// 比给出一个空列表报错更易于排查「哪个环节没传目标」。
func BuildClientOptions(o ClientOptions) []client.Option {
	rpcTimeout := o.RPCTimeout
	if rpcTimeout <= 0 {
		rpcTimeout = defaultRPCTimeout
	}
	connTimeout := o.ConnectTimeout
	if connTimeout <= 0 {
		connTimeout = defaultConnectTimeout
	}

	opts := []client.Option{
		client.WithRPCTimeout(rpcTimeout),
		client.WithConnectTimeout(connTimeout),
	}
	// 顺序有讲究：suite 必须先注册，本包的 ClientSpanMiddleware 后注册。
	// kitex 的 endpoint chain 里「先注册的在内层、后执行」，而 suite 自带的
	// ClientMiddleware 负责把 ctx 里的 span 注入给下游——只有本包的 span
	// 先建好，被注入的才是它。
	if o.TraceEnabled {
		opts = append(opts, client.WithSuite(kitextracing.NewClientSuite()))
		opts = append(opts, client.WithMiddleware(ClientSpanMiddleware()))
	}
	if addrs := splitTarget(o.Target); len(addrs) > 0 {
		opts = append(opts, client.WithHostPorts(addrs...))
	}
	// 短连接才需要显式叠加（kitex 默认即长连接 + 默认 IdleConfig），
	// 因此非 short 分支刻意不加任何选项，避免硬编码一份 IdleConfig 后
	// 与框架默认值漂移。
	if strings.EqualFold(strings.TrimSpace(o.ConnType), "short") {
		opts = append(opts, client.WithShortConnection())
	}
	return opts
}

// splitTarget 拆分逗号分隔的目标串，丢弃空片段。
//
// RegistryHandle.Target 返回逗号 join 的目标串，ad-hoc 调用可能传入带
// 空格的 "a:1, b:2"，逐段 TrimSpace 比整体 strings.Split 更健壮。
func splitTarget(target string) []string {
	if strings.TrimSpace(target) == "" {
		return nil
	}
	parts := strings.Split(target, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
```

**Step 5.2 — `internal/rpcx/middleware.go`（客户端链路中间件，唯一改点 = instrumentation 名）**

- [ ] 新建 `internal/rpcx/middleware.go`（源：`micro_apps/pkg/rpcx/middleware.go` 102 行；仅改 `rpcxInstrumentationName` 值，其余逐行不变）：

```go
// Package rpcx 的客户端链路中间件：显式产出 RPC client span 并注入下游。
//
// 为什么不能只依赖 obs-opentelemetry 的 ClientSuite（2026-10-02 实测）：
//
//  1. suite 自带的 client span 由 kitex 的 stats.Tracer 产出，实测从不落地——
//     下游能收到正确的 traceparent（同 trace_id），父 span id 却指向一个
//     从不存在的 span，span 树在调用边界断成两棵子树。
//  2. suite 的 ClientMiddleware 只「转发」ctx 里已有的 span，且它的注入发生在
//     本包 middleware 建 span 之前，注入出去的是上面那个不落地的 span。
//
// 因此这里把两件事都收回本包：span 的生命周期由 defer span.End() 钉死，
// traceparent 由本包自己写进 kitex 的 metainfo（后写覆盖，确保下游续的是
// 本 span）。命名沿用案例的分层约定 handler.* / service.* / client.*。
package rpcx

import (
	"context"

	"github.com/bytedance/gopkg/cloud/metainfo"
	"github.com/cloudwego/kitex/pkg/endpoint"
	"github.com/cloudwego/kitex/pkg/rpcinfo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// rpcxInstrumentationName 本包创建的 span 所属的 instrumentation scope。
const rpcxInstrumentationName = "hubx/launchx/internal/rpcx"

// ClientSpanMiddleware 为每次出站 RPC 产出一个 client span 并完成续链注入。
//
// 命名 client.<目标服务>.<方法>，与服务端 span（由各服务自己的 service 层
// 产出）天然区分，Jaeger 上一眼能看出「谁调谁」。
func ClientSpanMiddleware() endpoint.Middleware {
	tracer := otel.GetTracerProvider().Tracer(rpcxInstrumentationName)
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, req, resp interface{}) (err error) {
			ri := rpcinfo.GetRPCInfo(ctx)
			name := "client.rpc"
			if ri != nil && ri.To() != nil {
				name = "client." + ri.To().ServiceName() + "." + ri.To().Method()
			}

			// 剥离 ctx 中 kitex 那个不落地的 span，改以调用方的业务 span 为父。
			base := detachKitexSpan(ctx)
			base, span := tracer.Start(base, name,
				oteltrace.WithSpanKind(oteltrace.SpanKindClient))
			defer span.End()

			if ri != nil && ri.To() != nil {
				span.SetAttributes(
					attribute.String("rpc.system", "kitex"),
					attribute.String("rpc.service", ri.To().ServiceName()),
					attribute.String("rpc.method", ri.To().Method()),
				)
			}

			// 自己注入：suite 的 ClientMiddleware 已在更早的位置注入过 kitex
			// 那个不落地的 span，这里后写覆盖，下游 extract 到的才是本 span。
			base = injectSpanContext(base)

			if err = next(base, req, resp); err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			}
			return err
		}
	}
}

// injectSpanContext 用全局传播器把 ctx 的 span context 写进 kitex metainfo。
//
// 只搬运传播器实际写出的键（traceparent / tracestate / baggage），不复制
// 业务头——metainfo 会随请求一并下发，塞无关键只会放大报文。
func injectSpanContext(ctx context.Context) context.Context {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	for _, k := range carrier.Keys() {
		ctx = metainfo.WithValue(ctx, k, carrier.Get(k))
	}
	return ctx
}

// detachKitexSpan 把 ctx 当前的 span 换成「只保留其 SpanContext」的形态，
// 使后续 Start 以调用方的业务 span 为父，而不是 kitex 那个不落地的 span。
//
// 若当前 span 不是 SDK span（拿不到 ReadOnlySpan.Parent），就沿用它自身的
// SpanContext——此时它是调用方放进 ctx 的业务 span，本来就是想要的父。
func detachKitexSpan(ctx context.Context) context.Context {
	cur := oteltrace.SpanFromContext(ctx)
	if cur == nil {
		return ctx
	}
	parent := cur.SpanContext()
	if ro, ok := cur.(sdktrace.ReadOnlySpan); ok && ro.Parent().IsValid() {
		parent = ro.Parent()
	}
	return oteltrace.ContextWithSpanContext(ctx, parent)
}
```

**Step 5.3 — `internal/mode/mode.go`（四档入口 + Validate + Input/Output）**

- [ ] 新建 `internal/mode/mode.go`：

```go
// Package mode 按 yaml service.mode 四档装配服务端与发现能力（D4 映射表）：
//
//	monolith → webx 引擎，不注册发现
//	bff      → webx 引擎 + consul 注册 + 下游 srv 客户端工厂
//	gateway  → webx 引擎 + consul 发现上游
//	srv      → kitex RPC 服务端选项 + consul 注册，不建 webx
//
// 「按档决定注册/工厂」以声明式形态给出：RequirementsFor（挂载需求，Task 6
// 的 pipeline 据此决定组件 mount）与 RPCClientOptions（下游客户端工厂）。
// 非法/缺失 mode 返回含合法取值列表的 error（验收案例 C1/C2/C3；spec
// 「未知 mode 拒绝启动」在 Bootstrap 地基段的接线点自 Task 6）。
//
// 防环（D1）：只收 Input、回 Output；不 import pipeline / launchx /
// component——装配产物由 pipeline 落地挂载。
package mode

import (
	"errors"
	"fmt"

	"github.com/cloudwego/kitex/server"
	"github.com/gospacex/configx"
	"github.com/gospacex/logx"
	"github.com/gospacex/webx"
	"go.opentelemetry.io/otel/trace"
)

// validModes 非法 mode 报错必须列出的合法取值（spec「未知 mode 拒绝启动」：
// 返回 error 并列出 monolith|bff|srv|gateway）。
const validModes = "monolith|bff|srv|gateway"

// Input 装配输入（Assemble 入参）。sync 文案 `Assemble(app) error` 为简写，
// 实际以显式 Input/Output 进出（D1 防环：mode 不持有 App/组件句柄）。
type Input struct {
	// Config 配置句柄（configsrc 装载后传入）
	Config configx.Config
	// Logger 可空；空则 webx 缺省（logx.GetGlobal）
	Logger logx.Logger
	// Tracer 可空；空则 webx 缺省（otel noop）
	Tracer trace.Tracer
	// TraceOn D4「Trace.On(kind)」门控注入（configsrc.TraceOn 的包装位）；
	// nil 容错为全开（与 component.Env 同款）。
	TraceOn func(kind string) bool
	// Service service.name
	Service string
	// Mode service.mode（四档之一，Assemble 首步 Validate）
	Mode string
	// Addr service.addr（D3 单一事实源）：srv 必填；webx 档覆盖段内地址
	Addr string
	// PreRun 路由注册回调（经 webx.WithPreRun 在 New 内执行）。
	// 装配期钩子位——项目 Setup 不走此位（webx preRun 在 New 内、组件挂载
	// 之前执行，与 A2「Setup 为骨架序最后一步」冲突），Task 6 经
	// pipeline.Assemble 的 setup 参数在组件挂载完成后调用。
	PreRun func(*webx.App)
}

// Output 装配产物（D1：mode 不持有组件，component/pipeline 的挂载在上游做）。
type Output struct {
	// Engine monolith/bff/gateway 的 HTTP 入口；srv 为 nil
	Engine *webx.App
	// Server srv 的 kitex 服务端选项；其余档为 nil
	Server []server.Option
}

// traceOn nil 容错：未注入视为全开（TestTraceOn 语义在 configsrc.TraceOn）。
func (in *Input) traceOn(kind string) bool {
	if in.TraceOn == nil {
		return true
	}
	return in.TraceOn(kind)
}

// Validate 校验 service.mode：四档枚举内方可启动；错误必须含合法取值列表。
// 调用点：Assemble 首步（本 Task）+ Bootstrap 地基段（Task 6 接线）。
func Validate(serviceMode string) error {
	switch serviceMode {
	case "monolith", "bff", "srv", "gateway":
		return nil
	default:
		return fmt.Errorf("mode: invalid service.mode %q, want one of %s",
			serviceMode, validModes)
	}
}

// Assemble 按 service.mode 四档装配（D4 表）：
//
//	monolith/bff/gateway → webx 引擎（三档装配产物同构，差异在组件挂载与
//	                       客户端工厂，见 RequirementsFor / RPCClientOptions）
//	srv                  → kitex ServerOptions，不建 webx
//
// 非法/缺失 mode 含合法取值报错；srv 缺 service.addr 即错（kitex 无默认
// 监听地址，fail fast 早于启动段）。偏差：sync 文案为 `Assemble(app) error`
// 简写——实现为 Input→Output 显式进出。
func Assemble(in *Input) (*Output, error) {
	if in == nil {
		return nil, errors.New("mode: nil input")
	}
	if err := Validate(in.Mode); err != nil {
		return nil, err
	}
	if in.Mode == "srv" {
		if in.Addr == "" {
			return nil, errors.New("mode: srv missing service.addr")
		}
		return &Output{Server: rpcServerOptions(in)}, nil
	}
	// monolith / bff / gateway（其余取值已被 Validate 拒绝）
	eng, err := assembleWeb(in)
	if err != nil {
		return nil, err
	}
	return &Output{Engine: eng}, nil
}
```

**Step 5.4 — `internal/mode/webx.go`（webx 三档装配 + service.addr 覆盖）**

- [ ] 新建 `internal/mode/webx.go`：

```go
package mode

import (
	"fmt"

	"github.com/gospacex/webx"
)

// assembleWeb 构造 webx.App（monolith/bff/gateway 共用）。
//
// 两个硬约束：
//  1. webx 段 presence 守卫：`configx.Unmarshal("webx", …)` 在缺段时报
//     "key webx not found"（环境事实实测），必须 IsSet 先判——否则所有
//     不写 webx 段的 yaml 一律装配失败。
//  2. service.addr 单一事实源（D3）：in.Addr 非空时覆盖段内/缺省地址。
func assembleWeb(in *Input) (*webx.App, error) {
	cfg := webx.Config{}
	if in.Config != nil && in.Config.IsSet("webx") {
		var err error
		cfg, err = webx.ReadConfigFromConfigx(in.Config)
		if err != nil {
			return nil, fmt.Errorf("mode: read webx section: %w", err)
		}
	}
	if in.Addr != "" {
		cfg.Addr = in.Addr
	}
	opts := []webx.Option{webx.WithConfig(cfg)}
	if in.Logger != nil {
		opts = append(opts, webx.WithLogger(in.Logger))
	}
	if in.Tracer != nil {
		opts = append(opts, webx.WithTracer(in.Tracer))
	}
	if in.PreRun != nil {
		opts = append(opts, webx.WithPreRun(in.PreRun))
	}
	engine, err := webx.New(opts...) // New = options → FillDefaults → Validate → 装配（不监听）
	if err != nil {
		return nil, fmt.Errorf("mode: assemble webx: %w", err)
	}
	return engine, nil
}
```

**Step 5.5 — `internal/mode/srv.go`（srv 档 kitex 服务端选项）**

- [ ] 新建 `internal/mode/srv.go`：

```go
package mode

import (
	"github.com/cloudwego/kitex/server"

	"github.com/gospacex/launchx/internal/rpcx"
)

// rpcServerOptions srv 档的 kitex 服务端选项（D4 表 srv 行）。
// TraceEnabled 走 D4「Trace.On」门控（只控入链、不控构造：关掉即不挂
// obs-opentelemetry 套件，选项骨架仍是同一份）。
func rpcServerOptions(in *Input) []server.Option {
	return rpcx.BuildServerOptions(rpcx.ServerOptions{
		ServiceName:  in.Service,
		Addr:         in.Addr,
		TraceEnabled: in.traceOn("rpc"),
	})
}
```

**Step 5.6 — `internal/mode/registry.go`（四档装配需求声明）**

- [ ] 新建 `internal/mode/registry.go`：

```go
package mode

import "fmt"

// Requirements 四档装配需求（D4 mode 映射表的声明式形态）。
// Task 6 的 pipeline 据此决定组件挂载（Engine 段 → webx.Run；Registry 段 →
// component.Mount("registry")；Server 段 → app.Engine 位换 kitex serve）。
type Requirements struct {
	// Engine 需要 webx HTTP 引擎（monolith/bff/gateway）
	Engine bool
	// Registry 需要 consul 发现（bff/srv/gateway；monolith 恒 false，
	// 即使 yaml 写了 coordx.consul 段也不注册——D4 表）
	Registry bool
	// Server 需要 kitex RPC 服务端选项（仅 srv）
	Server bool
}

// RequirementsFor 按 mode 返回装配需求；非法 mode 返回含合法取值的 error。
//
//	D4 表：monolith → Engine
//	       bff      → Engine + Registry（客户端工厂另见 RPCClientOptions）
//	       srv      → Registry + Server
//	       gateway  → Engine + Registry
func RequirementsFor(serviceMode string) (Requirements, error) {
	if err := Validate(serviceMode); err != nil {
		return Requirements{}, err
	}
	switch serviceMode {
	case "monolith":
		return Requirements{Engine: true}, nil
	case "bff", "gateway":
		return Requirements{Engine: true, Registry: true}, nil
	case "srv":
		return Requirements{Registry: true, Server: true}, nil
	default:
		// Validate 已按枚举兜底，理论不可达；防御性保留防 switch 漏分支。
		return Requirements{}, fmt.Errorf("mode: invalid service.mode %q, want one of %s",
			serviceMode, validModes)
	}
}
```

**Step 5.7 — `internal/mode/rpcx.go`（下游 srv 客户端工厂，D4 表 bff 行）**

- [ ] 新建 `internal/mode/rpcx.go`：

```go
package mode

import (
	"github.com/cloudwego/kitex/client"

	"github.com/gospacex/launchx/internal/rpcx"
)

// RPCClientOptions 下游 srv 的 kitex 客户端工厂（D4 表 bff 行；plan-ready
// Task 10 的 `app.RPCClientOptions("srv")` 调用形态的工厂本体——根包便捷
// 方法在 Step 5.9 就位）。
//   - target 由调用方经 Registry 惰性发现（RegistryHandle.Target）；
//   - ConnType 固定 "short"：演示实例少且 Leader 会漂移，短连接更稳
//     （micro demo 同款）；
//   - traceOn = D4「Trace.On」kind=rpc。
func RPCClientOptions(serviceName, target string, traceOn bool) []client.Option {
	return rpcx.BuildClientOptions(rpcx.ClientOptions{
		ServiceName:  serviceName,
		Target:       target,
		ConnType:     "short",
		TraceEnabled: traceOn,
	})
}
```

**Step 5.8 — `internal/configsrc/trace.go` + `trace_test.go`（D4「Trace.On」语义唯一实现）**

- [ ] 新建 `internal/configsrc/trace.go`：

```go
package configsrc

import "github.com/gospacex/configx"

// TraceOn 实现全局设计约束 D4「Trace.On(kind)」（plan 行 98 语义）：
//
//	!trace.enabled → false（总开关优先，kind 级 true 不能翻案）
//	显式 trace.on.<kind> → 用其值
//	未写 → 默认 true
//
// 判定全部走 IsSet：配置没写 trace 段时 viper 的零值 false 不能被误读成
// 「全关」（D3 schema 恒写 enabled: true；IsSet 守卫保底）。
//
// 为什么唯一实现落 configsrc：语义要被根包（App.RPCClientOptions，Step 5.9）
// 与 internal/pipeline（Task 6 注入 component.Env.TraceOn）共用，而 D1 防环
// 禁止 pipeline import 根包 launchx——两者共同的 leaf 包只有 configsrc。
// 组件/mode 的 TraceOn 回调即对本函数的包装（见 Task 4 Env 注释）。
func TraceOn(cfg configx.Config, kind string) bool {
	if cfg == nil {
		return true
	}
	if cfg.IsSet("trace.enabled") && !cfg.GetBool("trace.enabled") {
		return false // D4：!trace.enabled → false
	}
	key := "trace.on." + kind
	if cfg.IsSet(key) {
		return cfg.GetBool(key) // 显式 kind 覆盖
	}
	return true // 未写默认 true
}
```

- [ ] 新建 `internal/configsrc/trace_test.go`：

```go
package configsrc

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gospacex/configx"
)

// newTraceCfg 构造 TraceOn 用配置（helper 带 trace 前缀，避免与本包其他
// 测试文件的 helper 重名）。
func newTraceCfg(t *testing.T, yamlBody string) configx.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yamlBody), 0o644); err != nil {
		t.Fatal(err)
	}
	cc, err := configx.ReadConfig(configx.ConfigOptions{ConfigFile: path, ConfigType: "yaml"})
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	return cc
}

// TestTraceOn D4 行 98 语义表：总开关 / kind 覆盖 / 未写默认 / nil 容错。
func TestTraceOn(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		kind string
		want bool
	}{
		{"total_gate_off", "service: {name: t}\ntrace: {enabled: false}\n", "db", false},
		{"enabled_true_kind_default", "service: {name: t}\ntrace: {enabled: true}\n", "db", true},
		{"kind_override_false", "service: {name: t}\ntrace: {enabled: true, on: {db: false}}\n", "db", false},
		{"kind_override_scoped", "service: {name: t}\ntrace: {enabled: true, on: {db: false}}\n", "mq", true},
		{"no_trace_section_default_true", "service: {name: t}\n", "rpc", true},
		{"kind_write_without_enabled", "service: {name: t}\ntrace: {on: {db: false}}\n", "db", false},
		{"total_gate_beats_kind_true", "service: {name: t}\ntrace: {enabled: false, on: {db: true}}\n", "db", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TraceOn(newTraceCfg(t, tc.yaml), tc.kind); got != tc.want {
				t.Fatalf("TraceOn(%q) = %v, want %v", tc.kind, got, tc.want)
			}
		})
	}
}

// TestTraceOnNil nil 句柄容错为全开（未装载配置前的早调用不炸）。
func TestTraceOnNil(t *testing.T) {
	if !TraceOn(nil, "db") {
		t.Fatal("TraceOn(nil) = false, want true")
	}
}
```

**Step 5.9 — 根包 `client.go` + `client_test.go`（`App.RPCClientOptions` 便捷方法）**

- [ ] 新建根包 `client.go`（偏差注记第 4 条的落地）：

```go
package launchx

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/kitex/client"
	"github.com/gospacex/launchx/internal/configsrc"
	"github.com/gospacex/launchx/internal/mode"
)

// RPCClientOptions 按下游服务名产出 kitex 客户端选项（plan-ready Task 10
// 的调用形态：`opts, err := app.RPCClientOptions("srv")`）：
//   - target 经 Registry 惰性发现（consul，3s 超时）；
//   - trace 门控走 configsrc.TraceOn（D4「Trace.On」，kind=rpc）。
//
// Registry 未挂载（组件降级等）时即返错，不让错误拖到第一次 RPC 才暴露。
func (a *App) RPCClientOptions(svc string) ([]client.Option, error) {
	if a.Registry == nil {
		return nil, fmt.Errorf("rpcx: registry not mounted (mode=%q), cannot resolve downstream %q",
			a.Mode, svc)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	target, err := a.Registry.Target(ctx, svc)
	if err != nil {
		return nil, fmt.Errorf("rpcx: resolve %q: %w", svc, err)
	}
	return mode.RPCClientOptions(a.Service, target, configsrc.TraceOn(a.Config, "rpc")), nil
}
```

- [ ] 新建根包 `client_test.go`：

```go
package launchx

import "testing"

// TestRPCClientOptionsRequiresRegistry Registry 未挂载即报错（不能等到运行期
// 第一次 RPC 才炸；D4 表 bff 行的工厂依赖组件挂载段）。
func TestRPCClientOptionsRequiresRegistry(t *testing.T) {
	a := &App{Service: "bff-demo", Mode: "bff"}
	if _, err := a.RPCClientOptions("srv"); err == nil {
		t.Fatal("want error when Registry not mounted, got nil")
	}
}
```

**Step 5.10 — `internal/mode/mode_test.go`（四档装配断言，验收案例 C1/C2/C3）**

- [ ] 新建 `internal/mode/mode_test.go`：

```go
package mode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/gospacex/configx"
	"github.com/gospacex/webx"
)

// modeInput 从 yaml 构造 Input（TraceOn 全开；字段取法与 pipeline 一致）。
func modeInput(t *testing.T, yamlBody string) *Input {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yamlBody), 0o644); err != nil {
		t.Fatal(err)
	}
	cc, err := configx.ReadConfig(configx.ConfigOptions{ConfigFile: path, ConfigType: "yaml"})
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	return &Input{
		Config:  cc,
		Service: cc.GetString("service.name"),
		Mode:    cc.GetString("service.mode"),
		Addr:    cc.GetString("service.addr"),
		TraceOn: func(string) bool { return true },
	}
}

// TestAssembleMonolith 验收案例 C1：webx 单体 + 不注册发现 + 装配后可挂路由
// （PreRun 钩子在 New 内执行、路由注册早于监听；项目 Setup 不走此位，
// Task 6 经 pipeline.Assemble 在组件挂载完成后调用）。
func TestAssembleMonolith(t *testing.T) {
	preRunCalled := false
	in := modeInput(t, "service: {name: mono-demo, mode: monolith, addr: \"127.0.0.1:18081\"}\n")
	in.PreRun = func(a *webx.App) {
		preRunCalled = true
		a.GET("/ping", func(_ context.Context, c *app.RequestContext) {
			c.JSON(200, map[string]any{"pong": true})
		})
	}
	out, err := Assemble(in)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if out.Engine == nil {
		t.Fatal("Engine = nil, want webx app")
	}
	if len(out.Server) != 0 {
		t.Fatalf("Server = %d options, want 0", len(out.Server))
	}
	if !preRunCalled {
		t.Fatal("PreRun not executed during assemble")
	}
	if got := out.Engine.Config().Addr; got != "127.0.0.1:18081" {
		t.Fatalf("Addr = %q, want service.addr 127.0.0.1:18081", got)
	}
	req, err := RequirementsFor("monolith")
	if err != nil {
		t.Fatalf("RequirementsFor(monolith): %v", err)
	}
	if req != (Requirements{Engine: true}) {
		t.Fatalf("Requirements = %+v, want {Engine:true}", req)
	}
}

// TestAssembleSrv 验收案例 C2：不建 webx、产出 kitex 服务端选项 + 注册需求。
func TestAssembleSrv(t *testing.T) {
	in := modeInput(t, "service: {name: srv-demo, mode: srv, addr: \"127.0.0.1:19013\"}\n")
	out, err := Assemble(in)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if out.Engine != nil {
		t.Fatal("Engine = webx app, want nil for srv")
	}
	if len(out.Server) == 0 {
		t.Fatal("Server options empty, want kitex options")
	}
	req, err := RequirementsFor("srv")
	if err != nil {
		t.Fatalf("RequirementsFor(srv): %v", err)
	}
	if req != (Requirements{Registry: true, Server: true}) {
		t.Fatalf("Requirements = %+v, want {Registry:true, Server:true}", req)
	}
}

// TestAssembleBffAndGateway 验收案例 C3：两档均装配 webx 引擎 + 发现需求
// （bff 的下游工厂与 gateway 的上游发现均经 Registry，见 RPCClientOptions）。
func TestAssembleBffAndGateway(t *testing.T) {
	for _, m := range []string{"bff", "gateway"} {
		t.Run(m, func(t *testing.T) {
			in := modeInput(t, "service: {name: "+m+"-demo, mode: "+m+", addr: \"127.0.0.1:18091\"}\n")
			out, err := Assemble(in)
			if err != nil {
				t.Fatalf("Assemble: %v", err)
			}
			if out.Engine == nil {
				t.Fatal("Engine = nil, want webx app")
			}
			req, err := RequirementsFor(m)
			if err != nil {
				t.Fatalf("RequirementsFor(%s): %v", m, err)
			}
			if req != (Requirements{Engine: true, Registry: true}) {
				t.Fatalf("Requirements = %+v, want {Engine:true, Registry:true}", req)
			}
		})
	}
}

// TestAssembleWebxSection webx 段 presence 两分支：段内配置生效（缺段 yaml
// 不炸——TestAssembleMonolith 已覆盖缺段）+ service.addr 覆盖段内地址（D3）。
func TestAssembleWebxSection(t *testing.T) {
	in := modeInput(t,
		"service: {name: web-demo, mode: bff}\nwebx: {addr: \"127.0.0.1:18099\", timeout: 3s}\n")
	out, err := Assemble(in)
	if err != nil {
		t.Fatalf("Assemble(webx section): %v", err)
	}
	if got := out.Engine.Config().Addr; got != "127.0.0.1:18099" {
		t.Fatalf("Addr = %q, want webx 段内 127.0.0.1:18099", got)
	}
	if got := out.Engine.Config().Timeout; got != 3*time.Second {
		t.Fatalf("Timeout = %v, want 3s", got)
	}

	in2 := modeInput(t,
		"service: {name: web-demo, mode: bff, addr: \"127.0.0.1:18100\"}\nwebx: {addr: \"127.0.0.1:18099\"}\n")
	out2, err := Assemble(in2)
	if err != nil {
		t.Fatalf("Assemble(service.addr override): %v", err)
	}
	if got := out2.Engine.Config().Addr; got != "127.0.0.1:18100" {
		t.Fatalf("Addr = %q, want service.addr 覆盖后的 127.0.0.1:18100", got)
	}
}

// TestAssembleInvalidMode spec「未知 mode 拒绝启动」：非法与缺失（空串）都
// 返回含合法取值列表的 error。
func TestAssembleInvalidMode(t *testing.T) {
	for _, m := range []string{"mesh", ""} {
		_, err := Assemble(&Input{Mode: m})
		if err == nil {
			t.Fatalf("Assemble(mode=%q) = nil error, want error", m)
		}
		if !strings.Contains(err.Error(), "monolith|bff|srv|gateway") {
			t.Fatalf("Assemble(mode=%q) err = %q, want list monolith|bff|srv|gateway", m, err)
		}
		if _, rErr := RequirementsFor(m); rErr == nil ||
			!strings.Contains(rErr.Error(), "monolith|bff|srv|gateway") {
			t.Fatalf("RequirementsFor(mode=%q) err = %v, want list error", m, rErr)
		}
	}
}

// TestAssembleSrvMissingAddr srv 没有默认监听地址：缺 service.addr 即错。
func TestAssembleSrvMissingAddr(t *testing.T) {
	in := modeInput(t, "service: {name: srv-demo, mode: srv}\n")
	if _, err := Assemble(in); err == nil || !strings.Contains(err.Error(), "service.addr") {
		t.Fatalf("Assemble(srv without addr) err = %v, want contains service.addr", err)
	}
}

// TestRequirementsFor 四档需求表（D4 mode 映射表的声明式断言）。
func TestRequirementsFor(t *testing.T) {
	cases := []struct {
		mode string
		want Requirements
	}{
		{"monolith", Requirements{Engine: true}},
		{"bff", Requirements{Engine: true, Registry: true}},
		{"srv", Requirements{Registry: true, Server: true}},
		{"gateway", Requirements{Engine: true, Registry: true}},
	}
	for _, tc := range cases {
		got, err := RequirementsFor(tc.mode)
		if err != nil {
			t.Fatalf("RequirementsFor(%s): %v", tc.mode, err)
		}
		if got != tc.want {
			t.Fatalf("RequirementsFor(%s) = %+v, want %+v", tc.mode, got, tc.want)
		}
	}
}

// TestRPCClientOptionsFactory D4 表 bff 行的客户端工厂：总能产出非空选项
// （trace 门控两态都不影响构造——与 Trace.On「只控入链不控构造」一致）。
func TestRPCClientOptionsFactory(t *testing.T) {
	if opts := RPCClientOptions("bff-demo", "127.0.0.1:9013", false); len(opts) == 0 {
		t.Fatal("traceOn=false: options empty")
	}
	if opts := RPCClientOptions("bff-demo", "127.0.0.1:9013", true); len(opts) == 0 {
		t.Fatal("traceOn=true: options empty")
	}
}
```

**Step 5.11 — Run**

- [ ] Run:

```bash
go mod tidy
go vet ./...
go test ./internal/mode/ ./internal/configsrc/ -count=1
go test . -run TestRPCClientOptionsRequiresRegistry -count=1
go build ./...
```

Expected: 第三条命令输出两行 `ok`——`internal/mode`：TestAssembleMonolith（含 PreRun 回调已执行、Addr=service.addr、Requirements 断言）、TestAssembleSrv、TestAssembleBffAndGateway（两子 PASS）、TestAssembleWebxSection（段内地址/3s timeout + service.addr 覆盖）、TestAssembleInvalidMode（mode="mesh" 与 mode="" 双例，错误串含 `monolith|bff|srv|gateway`，RequirementsFor 同断言）、TestAssembleSrvMissingAddr（错误串含 `service.addr`）、TestRequirementsFor 四档表、TestRPCClientOptionsFactory（traceOn 两态）全 PASS；`internal/configsrc`：TestTraceOn 七子用例 + TestTraceOnNil PASS。第四条命令 `ok github.com/gospacex/launchx`——TestRPCClientOptionsRequiresRegistry PASS。`go mod tidy`/`go vet`/`go build` 无输出无错（kitex v0.16.3、obs-opentelemetry v0.3.0、bytedance/gopkg v0.1.4 经 tidy 落入 require，无 replace 新增）。

**偏差注记**（不改上方 sync 引用块）：
1. **签名**：sync 写 `Assemble(app) error`（简写）——实现为 `Assemble(in *Input) (*Output, error)`；Input/Output 显式进出是 D1 防环的硬要求（mode 不 import pipeline/launchx/component，Task 6 的 pipeline 消费 Output 并据 RequirementsFor 挂载）。
2. **改动文件扩三处 + 根包两文件**（文件结构树已同步标注）：`internal/rpcx/{options.go,middleware.go}`——D4 表头「kitex server/client 选项经 internal/rpcx 封装」与行 40 技术栈点名，plan-ready Task 5 改动文件未列，属 D4 必需；`internal/mode/rpcx.go`——D4 表 bff 行工厂本体；`internal/configsrc/trace.go`+`trace_test.go`——D4 行 98 `Trace.On(kind)` 语义的唯一实现（须被根包与 pipeline 共用，D1 禁 pipeline→launchx，唯一共同 leaf 包即 configsrc，Task 6 的 `Env.TraceOn` 由此注入）；根包 `client.go`+`client_test.go`——plan-ready Task 10 引用的 `app.RPCClientOptions("srv")` 调用形态，工厂须在 Task 10 前就位。
3. **rpcx 移植差异仅三类**（源 `micro_apps/pkg/rpcx`，Step 5.1/5.2 已逐行落定）：包注释（kitex-framework 背景改写为移植说明）、instrumentation 名 `hubx/micro_apps/pkg/rpcx` → `hubx/launchx/internal/rpcx`、3 处 micro 路径注释（`infra/consul.Target` → `RegistryHandle.Target`、`KV hubx/svc/<name>` → consul ServiceID、`infra/telemetry.Init` → `internal/telemetry`）。其余代码逐行不变。
4. **spec C1「Setup 可直接挂路由」**：Setup 回调在 Task 6 的 pipeline 才接线；本 Task 以 `Input.PreRun`（`webx.WithPreRun`，New 内执行）断言「装配后可立即注册路由」。Task 6 的项目 Setup 不经 PreRun（webx preRun 在 New 内、组件挂载前执行，A2 要求 Setup 为骨架序最后一步），改经 `pipeline.Assemble` 的 setup 参数在组件挂载完成后调用（组件注释同步指向）。
5. **C4（mesh 零改动）** 为纯部署约束，无代码断言：mode 无 `mesh` 枚举（TestAssembleInvalidMode 用 "mesh" 作非法例），网格形态复用 micro 档装配路径。

**Step 5.12 — commit**

- [ ] `git add -A && git commit -m "feat(task-5): mode 四档装配分支与 rpcx 选项封装"`

- [ ] **Task complete**
