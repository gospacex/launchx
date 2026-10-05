# 实现计划：add-launchx-bootstrap

## 来源
- 提案：openspec/changes/add-launchx-bootstrap/proposal.md
- 设计：openspec/changes/add-launchx-bootstrap/design.md
- 规格：openspec/changes/add-launchx-bootstrap/specs/
- 任务：openspec/changes/add-launchx-bootstrap/tasks.md

## Amendments

### 2026-10-04: 补漏修订（brainstorming 续接确认 3 项）
- 原因：`service_mesh/` 仓清单未交付（0/16），copy 前提不成立；e2e 底座实际由 `hubx/dockers/docker-compose.yml` 提供；srv-order 回归需覆盖 outbox 链路
- 影响规格：`specs/example-demos/spec.md`（E1/E2/E3/E4/E5）
- 影响任务：Task 8（monolith 接 mysql）、Task 12（mesh 改自写清单 + dry-run 硬验收）、Task 19（micro e2e 底座 + kafka）、Task 20（srv 回归加 outbox→kafka）、Task 21（mesh 两级验收，close 不硬阻塞人工项）
- 底座约定：monolith/micro e2e 依赖 `hubx/dockers` compose（consul/mysql 8.4/kafka KRaft/jaeger all-in-one）；kafka topic 自动创建

### 2026-10-04: 本地环境适配修订（用户指令「环境适应本地 docker 里的容器」+ git 方案确认）
- 原因：本机 docker 实测容器全部 Exited；micro_apps 旧配置里的 kafka 端口（19093）与 `hubx/dockers` 现行端口（9092）不一致；launchx 非 git 仓库
- 影响任务：Task 1（`git init`）、Task 9/10/19/20（e2e 底座前置与地址）、Task 8/18（mysql 底座）
- **底座事实（写死在各 demo config.yaml 与 e2e 脚本，不得各起炉灶）**：
  - 启动前置：`docker compose -f /Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml up -d consul mysql kafka jaeger redis`（当前 Exited，e2e 前必须拉起）
  - 端口/凭据：consul `127.0.0.1:8500`；mysql `127.0.0.1:3306`（root/rootpass；demo 库 `order_db`、账号 `order_rw/hubx123`，由 `hubx/micro_apps/docker/mysql/init.sql` 幂等灌入）；kafka `127.0.0.1:9092`（KRaft、`AUTO_CREATE_TOPICS_ENABLE=true`、PLAINTEXT advertised 127.0.0.1:9092）；jaeger OTLP `127.0.0.1:4318` + UI `16686`；redis `127.0.0.1:6379`（无密码）
  - srv-order config.yaml 迁移时 DSN 写 `order_rw:hubx123@tcp(127.0.0.1:3306)/order_db`（或 `${HUBX_DB_PW}` 占位经 ExpandEnv），brokers 写 `127.0.0.1:9092`（**不**沿用原 `19093`）
- **git 方案（用户已确认）**：Task 1 首步在 `launchx/` 执行 `git init` + 初始提交；后续 Task 按 TDD 节奏逐任务 commit
- minikube 集群当前未运行（仅 context 存在）；mesh 真实部署为人工验证项，close 不硬阻塞（与 Task 21 两级验收一致）

### 2026-10-04: Task 10 下游调用方式对齐 OpenSpec（翻译层修正，不改 delta 规格）
- 原因：plan-ready Task 10 原文写「net/http 调 srv `/internal/order/:id` HTTP 适配层」，与三处 OpenSpec 制品矛盾：proposal.md「`bff`（webx 入口 + consul + rpcx 下游客户端）」、`specs/mode-assembly/spec.md`「下游 srv 的 rpcx 客户端工厂」、tasks.md 2.3「下游 srv rpcx 客户端」
- 修正：Task 10 改为 rpcx（kitex）客户端工厂路径；srv 不新增 HTTP 适配层；bff 复用 Task 11 迁入的 `examples/micro/srv/pkg/kitex_gen`（同 module import，kitex_gen 迁移位置不变）
- 影响任务：Task 10（改 rpcx 客户端工厂 + kitex GetOrder 调用）、Task 19（e2e 链路断言不变，走 gateway→bff→srv RPC）

### 2026-10-04: 依赖选型裁决（hubx `*x` SDK 优先）

- 原因：用户指令「launchx 本身与三个 demo 优先采用 `hubx/` 目录下以 `x` 结尾的 SDK」；逐层盘点后 9 个能力层均有对应 SDK，RPC 为唯一真空
- 影响规格：`specs/mode-assembly/spec.md`（新增 Requirement「RPC 经 hubx/rpcx 装配」+ Scenario「RPC 依赖面收敛」）、`specs/component-registry/spec.md`（组件经 `*x` SDK 构造、`trace.on` 判定经 `traceflag`）
- 影响任务：新增 `tasks.md` 0.1（`../coordx/consul` 修复）/ 0.2（新建 `../rpcx`）/ 3.14（依赖面核对）；1.4 改 traceflag 判定、1.5 改经 `hubx/rpcx` 装配
- 影响本计划：新增前置 Task 26（coordx 修复）、Task 27（新建 `hubx/rpcx`）、Task 28（依赖面核对），**均须早于 Task 4/5/10/11 执行**；Task 4/5/10/11 的 sync 引用在下次 writing-plans 重排时一并同步
- 排除项：`gospacex/infrax`（仅空目录、零 go 文件）、`gospacex/rpcx`（模块名裸 `rpcx` + 依赖 `regdisx`，非 hubx 家族）不作为 SDK 采用

### 2026-10-05: 通用化定位 + 可观测增强（brainstorming 确认 4 项）
- 原因：launchx 定位为 hubx 通用基建库（业务侧 = 一份 yaml + 分层代码，删除各 app 复制的 `infra/`，11 服务可接入）；可观测能力（metrics/探针/pprof）原规格未覆盖
- 影响规格：新增 `specs/observability/spec.md`（第 6 capability）、`design.md` 决策与 Non-Goals 修订
- 影响任务：tasks.md 1.8 / 3.11 / 3.12 / 3.13（O1/O2/O3）
- 追加实现步骤：Task 24（debug 伪组件）、Task 25（可观测验证 O1–O3）
- 既有任务微调（不改行为契约）：Task 4 组件注册表内置清单追加 `debug` 伪组件（门控语义见 Task 24）；Task 12 mesh 清单探针字段固定为 `tcpSocket` 打业务端口（liveness + readiness，语义就绪由 consul 健康检查承载，README 注明 Istio sidecar 补充探测）

## 追加实现步骤（2026-10-05）

### Task 24: debug 伪组件（验收案例 O1/O2）
- [ ] **任务完成**（与 `tasks.md` 1.8 同步勾选）
- 目标：`internal/component` 内置 `debug` 伪组件——`debug.enabled`（yaml 或 `APP_DEBUG=1` 任一置位）时起仅监听 `127.0.0.1:6060` 的 debug server（标准 `net/http/pprof` handlers）；生产默认关；端口冲突 Warn 带病运行；退出序列中 debug server 先于 `logger.Close` 关闭（插入反序退出链，事件记录器可断言）
- 改动文件：`internal/component/debug.go`（builder + server + Stop）、`internal/pipeline` 退出链插入点、`App.Debug` 句柄字段
- 验证方式：`go test ./internal/component -run TestDebug` 门控单测通过（false 不起 server / true 仅回环可达 / 退出顺序断言）

### Task 25: 可观测验证 O1–O3
- [ ] **任务完成**（与 `tasks.md` 3.11/3.12/3.13 同步勾选）
- 目标：单测（debug 门控，O1）+ 集成（`APP_DEBUG=1` 启动 srv 档，`curl 127.0.0.1:6060/debug/pprof/` 返回索引页，yaml 置位等效，O2）+ 清单（mesh 三 Deployment 探针为 `tcpSocket` 且端口与业务一致，`kubectl apply --dry-run=client` + `bring-up.sh check` 全绿，O3）
- 验证方式：`go test ./...` 全绿；O2 在共享底座拉起后执行并记录输出；O3 dry-run 全绿（并入 Task 21 两级验收的硬验收段）

## 追加实现步骤（2026-10-04 依赖选型）

> 以下 Task 26/27 为**跨仓前置**，须早于 Task 4/5/10/11 执行；Task 28 为依赖面核对，随验证阶段执行。

### Task 26: 跨仓前置 —— coordx/consul 注册修复落地（对应 `tasks.md` 0.1）
- [x] **任务完成**（与 `tasks.md` 0.1 同步勾选） <!-- 已实现: coordx 零改动 -->
- 目标：`../coordx/consul/consul.go` 删 `Interval` 只留 `TTL: "30s"` + 补 `startBeat` 心跳（每 10s `UpdateTTL`，`Deregister` 成功 / `Close` 停止，同 ID 重注册先停旧）；与 superpowers plan `2026-10-05` Task 4 步骤 4.1 为同一件事，此处为落地确认
- 改动文件：`/Users/hyx/work/gowork/src/hubx/coordx/consul/consul.go`（兄弟仓，以 `replace` 指本地，不发布新版本）
- 验证方式：注册 200、passingOnly 发现 1 实例、35s 后 check 仍 passing、注销后归零；`cd ../coordx/consul && go build ./... && go vet ./...` 全绿

### Task 27: 跨仓前置 —— 新建 `hubx/rpcx` 模块（对应 `tasks.md` 0.2）
- [x] **任务完成**（与 `tasks.md` 0.2 同步勾选） <!-- 已实现: a047afa / 25e3e9b -->
- 目标：新建 `github.com/gospacex/rpcx` 独立 module（第 15 个 `x` SDK）：封装 kitex `server`/`client` Option 装配（`BuildServerOptions` / `BuildClientOptions`）——实现自 `micro_apps/pkg/rpcx/options.go` 移植，含缺省超时（RPC 3s / 建连 500ms / 读写 30s / 退出排空 5s）与 `kitex-contrib/obs-opentelemetry` Suite 挂载；**服务发现走 `coordx`，MUST NOT 引入 `regdisx`**；Suite 只从 `otel.GetTracerProvider()` 取用，不自行装配 provider
- 改动文件：`/Users/hyx/work/gowork/src/hubx/rpcx/{go.mod,options.go,server.go,client.go,README.md}`（新建仓）
- 验证方式：`go build ./... && go vet ./...` 全绿；README 注明与 `micro_apps/pkg/rpcx`、`gospacex/rpcx` 的关系与迁移路径（收敛不在本轮）

### Task 28: 依赖面核对（对应 `tasks.md` 3.14）
- [ ] **任务完成**（与 `tasks.md` 3.14 同步勾选）
- 目标：核对 launchx 与 `examples/*` 各模块 `go.mod` 与 import 中无 `cloudwego/kitex`、`cloudwego/hertz`、`redis/go-redis`、`segmentio/kafka-go`、`hashicorp/consul/api`、`gorm.io/gorm` 直接引用；RPC 相关 import 仅指向 `github.com/gospacex/rpcx`
- 改动文件：无（核对；不符合则修正 import 与 go.mod）
- 验证方式：`go list -deps` / grep 全仓 import 清单核对通过

## 实现步骤

### Task 1: 创建 launchx module 与单入口 API
- [x] **任务完成**（与 superpowers plan `Task 1`、`tasks.md` 对应条目同步勾选） <!-- 已实现: e7bf5b1 -->
- 目标：`go.mod` + `launchx.go`/`app.go`/`setup.go`：`Bootstrap(path) (*App, error)`、App 句柄字段（Config/Logger/Tracer/DB/Cache/MQ/Registry/Engine）、`OnSetup(func(*App) error)`、`Run() error`、幂等 `Close()`；Run 先以最小骨架占位（后续 Task 填充），main.go 形态可编译（验收案例 A1）
- 改动文件：`go.mod`、`launchx.go`、`app.go`、`setup.go`、`launchx_test.go`
- 验证方式：`go build ./...` 通过；`go test ./... -run TestBootstrap` 单测通过（Bootstrap 返回 App、OnSetup 可注册、Close 幂等）

### Task 2: configsrc 注册表与 local 加载器
- [x] **任务完成**（与 superpowers plan `Task 2`、`tasks.md` 对应条目同步勾选） <!-- 已实现: 990d1a1 / 91565af -->
- 目标：`internal/configsrc`：`Register(name, Loader)` 注册表 + `local` 加载器（configx `ReadConfig` 读本地 yaml）+ `${ENV}` 展开辅助 `ExpandEnv(s string) string`（DSN 占位用）；yaml 未声明 `configcenter` 时默认 local
- 改动文件：`internal/configsrc/{registry.go,local.go,expand.go}`、`internal/configsrc/local_test.go`
- 验证方式：`go test ./internal/configsrc/` — 缺省走 local、env 展开、注册表覆盖三项断言通过

### Task 3: configsrc nacos 加载器（三裁决）
- [x] **任务完成**（与 superpowers plan `Task 3`、`tasks.md` 对应条目同步勾选） <!-- 已实现: cdbab21 -->
- 目标：移植 micro_apps `infra/nacos.LoadUnified` 语义（该仓实际 API 为 `LoadUnified`，历史文档中的 `LoadMerged` 即指此）：本地兜底 + `hubx.<service>` dataId key 级覆盖 + `HUBX_NACOS_SERVER` env 覆盖地址；dataId 空纯本地；远端不可达返回地基 error（验收案例 B1）
- 改动文件：`internal/configsrc/nacos.go`、`internal/configsrc/nacos_test.go`
- 验证方式：`go test ./internal/configsrc/ -run Nacos` — key 级裁决/纯本地/不可达即退三断言通过

### Task 4: component 注册表与四个内置组件
- [x] **任务完成**（与 superpowers plan `Task 4`、`tasks.md` 对应条目同步勾选） <!-- 已实现: bdbbdbf -->
- 目标：`internal/component`：`Register(name, Builder)` + db(mysql/gorm 经 dbx config loader、DSN env 展开、`Trace.On("db")` 门控 Open/OpenNoTrace)、cache、mq(producer)、registry(coordx consul Register/Deregister/Target 封装)；`strict.<name>` 裁决降级 Warn/失败即退；`trace.on` 只控入链不控构造（验收案例 D1/D2）
- 改动文件：`internal/component/{registry.go,db.go,cache.go,mq.go,consul.go,strict.go}`、`internal/component/*_test.go`
- 验证方式：`go test ./internal/component/` — strict 双分支、Trace.On 不改变构造、registry 失败带病运行断言通过（验收案例 D1/D2）
- **依赖选型（2026-10-04 裁决）**：`trace.on.<name>` 判定经 `github.com/gospacex/traceflag` 组件级注册（`traceflag.Apply(name, on)`），**不自造布尔判定**；组件构造经 `dbx/orm` / `cachex/redis` / `mqx/kafka` / `coordx/consul`，不直引 gorm / go-redis / kafka-go / consul-api
- **API 事实纠偏（2026-10-05 采集）**：`Target/Resolve` 不在 coordx（coordx 侧等价能力为 `Discover.Discovery(ctx, service) ([]*ServiceInstance, error)`）；`Target(agentAddr, serviceID)` 是 `micro_apps/infra/consul/registry.go:148` 的应用层助手 → registry 组件的 Target/Resolve 逻辑**移植自该文件**进 `internal/component/consul.go`，coordx 仅承担 Register/Deregister/Discovery 原语。另：`infra/` 目录不存在于 srv-order 仓（各 app 复制的是 `micro_apps/infra/{consul,nacos,telemetry,drivers}`），Task 11 迁移时删除的是对 `micro_apps/infra` 的 import，改为 launchx 等价物

### Task 5: mode 四档装配分支
- [x] **任务完成**（与 superpowers plan `Task 5`、`tasks.md` 对应条目同步勾选） <!-- 已实现: 0cb00c8 -->
- 目标：`internal/mode`：`Assemble(app) error` 按 `service.mode` 分支——monolith/bff/gateway 构造 `webx.App`（Addr 来自 yaml、Tracer/Logger 注入、PreRun 挂路由）并按档决定 consul 注册与下游客户端工厂；srv 不建 webx、提供 rpcx ServerOptions 与注册；非法/缺失 mode 返回含 `monolith|bff|srv|gateway` 合法取值的 error（验收案例 C1/C2/C3）
- 改动文件：`internal/mode/{mode.go,webx.go,srv.go}`、`internal/mode/mode_test.go`；`app.go` 接通 `mode.Assemble` + `Run` + `serverStop` 钩子（未建 `registry.go`：注册/发现一律经 rpcx 的 coordx 适配）
- **API 事实纠偏（2026-10-05 实测）**：`coordx.Register` 只有 `Register/Deregister/HealthCheck/Close`（`coordx/interface.go:23`），**没有** `Target(agentAddr, service)`；故「上游发现」不走 Registry.Target，改为经 `rpcx.BuildClientOptions(ClientOptions{RegistryAddr})` 内部的 coordx resolver（`rpcx/client.go:31`）
- **启动/停机 API 实测**：`webx.App.Run() error` / `Shutdown(ctx) error`（`webx/webx.go:235/240`，未运行时 Shutdown 返回 errStatusNotRunning，故停机钩子先判 `IsRunning()`）；kitex `server.Server` 为 `Run() error` / `Stop() error`（`kitex@v0.16.3/server/server.go:54`）
- 验证方式：`go test ./internal/mode/` — 四档装配断言 + 非法 mode 报错断言通过（验收案例 C1/C2/C3）
- **依赖选型（2026-10-04 裁决）**：srv 档的 ServerOptions 由新建的 `github.com/gospacex/rpcx` 提供（前置 Task 27），**不建 `internal/rpcx`**；HTTP 侧经 `webx`；本 Task 不 import kitex / hertz

### Task 6: pipeline 固定启动骨架与 telemetry 封装
- [x] **任务完成**（与 superpowers plan `Task 6`、`tasks.md` 对应条目同步勾选） <!-- 已实现: fc02f6c -->
- 目标：`internal/telemetry`（移植 micro_apps `infra/telemetry.Init/Setup`：otelx 全局 TracerProvider + Composite 传播器、Enabled=false 走 noop + traceflag 关闭）+ `internal/pipeline`：固定顺序 配置(configsrc) → telemetry.Init（最先）→ logx.New+SetGlobal → mode.Assemble → component 挂载 → 回调 Setup；每步写入事件记录器供断言（验收案例 A2）
- 改动文件：`internal/telemetry/{provider.go,provider_test.go}`、`internal/pipeline/{pipeline.go,events.go}`、`internal/pipeline/pipeline_test.go`
- 验证方式：`go test ./internal/...` — 启动顺序断言（telemetry 早于 db/cache/mq）通过（验收案例 A2）

### Task 7: 反序退出编排
- [x] **任务完成**（与 superpowers plan `Task 7`、`tasks.md` 对应条目同步勾选） <!-- 已实现: 21c2fb0 -->
- 目标：`App.Run` 收尾段：SIGINT/SIGTERM 等待 → 项目 `OnStop` 钩子逆序 → 内置组件 Stop → registry Deregister → tracing.Shutdown(15s) → logger.Close；显式序列不依赖 defer；Setup 失败/运行期错误走同一退出链并返回首错；`Close()` 幂等复用该链（验收案例 A3）
- 改动文件：`app.go`（Run/Close/OnStop/stepRegister）、`internal/pipeline/pipeline.go`（新增步骤名 `registry.register`）、`app_test.go`、`launchx_test.go`、`testdata/{bff,srv,gateway,strict-registry,selfaddr}.yaml`
- **落地偏差（未新建 `internal/pipeline/shutdown.go`）**：退出链已在 Task 5/6 落地于 `App.Close()`（显式序列、不依赖 defer），再抽一层 `Shutdown(a *AppHolders)` 会与 `App` 成环（pipeline 包不得回指根包），故本轮只在 `app.go` 内收口：补 serverStop 接线与注册实例跟踪 + 真注销，测试放 `app_test.go`（需 in-package 注入替身）
- **API 事实（2026-10-05 实测）**：webx `*App.Run() error`（`webx/webx.go:235`）/ `Shutdown(ctx) error`（`:240`，`IsRunning()` 来自内嵌 `*server.Hertz`）；kitex `server.Server` 为 `Run() error` / `Stop() error`（`kitex@v0.16.3/server/server.go:52-58`）；`coordx.Register` 只有 `Register/Deregister/HealthCheck/Close`；自身注册地址键定为 `service.self.addr` → `service.addr` → `webx.addr`（三次读取均经 `configsrc.envConfig`，`${ENV}` 生效）
- 验证方式：`go test ./... -count=1` 全绿 — 退出顺序断言（server.stop 早于 component.stop 与 tracing.shutdown、deregister 早于 flush）、bff/srv 注册一次且注销同一实例、monolith/gateway 零注册零注销日志、`strict.registry=true` 即退、缺省降级带后果 Warn（验收案例 A3）

### Task 8: examples/monolith 单体 demo
- [x] **任务完成**（与 superpowers plan `Task 8`、`tasks.md` 对应条目同步勾选）
- 目标：参考 `webx/examples` 分层（handler→logic→service→repository）新建 `examples/monolith/`：`config.yaml`（mode monolith）+ `cmd/main.go`（Bootstrap+OnSetup+Run ≤50 行）+ 分层代码 + 路由（GET /orders、POST /orders）；repository 经 launchx db 组件接真实 mysql（底座复用 `hubx/dockers` compose mysql 8.4，DSN 经 `${ENV}` 展开），不再用内存替身
- 改动文件：`examples/monolith/{config.yaml,cmd/main.go,app/setup.go,app/routes.go,internal/{handler,logic,service,repository,model}/*.go}`
- 验证方式：`go build ./... && go run ./examples/monolith/cmd` 启动；`curl localhost:8080/orders` 返回 JSON（验收案例 E1 起）

### Task 9: examples/micro/gateway 网关 demo
- [ ] **任务完成**（与 superpowers plan `Task 9`、`tasks.md` 对应条目同步勾选）
- 目标：新建 `examples/micro/gateway/`：mode gateway，`config.yaml` 含 `gateway.routes`（/api→bff）；Setup 内实现极简转发（`app.Registry.Target` 解析上游 + hertz Any("/*p") 反向代理，参考 micro_apps gateway 行为但精简为转发+状态码映射 404/429/502 中路由与 502 部分）
- 改动文件：`examples/micro/gateway/{config.yaml,cmd/main.go,app/setup.go,internal/routing/*.go,internal/proxy/*.go}`
- 验证方式：`go build ./...`；起 consul 后 `curl localhost:8080/api/ping` 经转发命中 bff（验收案例 E2 前半）

### Task 10: examples/micro/bff 服务 demo
- [ ] **任务完成**（与 superpowers plan `Task 10`、`tasks.md` 对应条目同步勾选）
- 目标：新建 `examples/micro/bff/`：mode bff，webx 入口 + consul 注册；`GET /ping`（自响应）与 `GET /order/:id`（经 `app.RPCClientOptions("srv")` 工厂取 rpcx 客户端选项——Target 来自 `app.Registry.Target("srv")`、挂 obs-opentelemetry suite——构造 kitex 客户端调 srv `GetOrder`；客户端类型复用已迁入的 `examples/micro/srv/pkg/kitex_gen`，demo 内同 module 直接 import，**不**新增 srv HTTP 适配层。与 proposal/mode-assembly spec/tasks.md 2.3 的「rpcx 下游客户端」一致）
- 改动文件：`examples/micro/bff/{config.yaml,cmd/main.go,app/setup.go,internal/handler/*.go,internal/downstream/*.go}`
- 验证方式：`go build ./...`；起 consul+srv 后 `curl localhost:8091/order/1001` 返回 srv 数据（验收案例 E2 中段）
- **依赖选型（2026-10-04 裁决）**：下游客户端选项经 `github.com/gospacex/rpcx` 的 `BuildClientOptions`（前置 Task 27），bff 不 import kitex

### Task 11: examples/micro/srv 迁移 srv-order
- [x] **任务完成**（与 superpowers plan `Task 11`、`tasks.md` 对应条目同步勾选） <!-- 已实现: 3a22f89 -->
- 目标：从 `hubx/micro_apps/apps/srv-order` 复制业务代码（`internal/{repository,service,rpc,middleware}`、`pkg/kitex_gen`、`config.yaml`）到 `examples/micro/srv/`，删除其 `infra/` 与 336 行 main.go，改为 `Bootstrap+OnSetup+Run`：Setup 内装配 repo/outbox/service、用 launchx 提供的 rpcx ServerOptions 构建 kitex server 并 `go svr.Run()`、`OnStop` 注册反序关闭（main.go ≤50 行，验收案例 E3）
- 改动文件：`examples/micro/srv/{config.yaml,cmd/main.go,app/setup.go,internal/**,pkg/kitex_gen/**}`（新增），不改 `hubx/micro_apps` 原仓
- 验证方式：`go build ./... && wc -l examples/micro/srv/cmd/main.go`（≤50 行）；起 mysql/consul 后可接收 RPC（验收案例 E3 前置）
- **依赖选型（2026-10-04 裁决）**：kitex server 以 `github.com/gospacex/rpcx` 的 `BuildServerOptions` 构建（前置 Task 27）；db 经 `dbx/orm`、mq 经 `mqx/kafka`、cache 经 `cachex/redis`，均不直引三方客户端

### Task 12: examples/mesh 网格清单自包含（launchx 仓内自写）
- [ ] **任务完成**（与 superpowers plan `Task 12`、`tasks.md` 对应条目同步勾选）
- 目标：launchx 仓内自写最小 k8s + Istio 清单到 `examples/mesh/`：namespace、gateway/bff/srv 三服务 Deployment+Service、Secret、sidecar 注入（istio label）、bring-up.sh + `check` 前置检查；裁剪为 3 服务并改写：镜像 tag 指向 `launchx-examples/*`、配置副本改本目录地址段（`*.mesh-external`）、workload 数量与端口对齐本 demo。**`service_mesh/` 仓清单未交付（0/16），MUST NOT 作为 copy 源**；其设计结论（业务零改动/sidecar 同 trace/地址段转换）作编写依据并在 README 注明来源
- 改动文件：`examples/mesh/{k8s/**,istio/**,scripts/**,configs/**,README.md}`（全部为本仓新写；`service_mesh/` 原仓零改动）
- 验证方式：`kubectl apply --dry-run=client -f examples/mesh/k8s/ -R` 全过 + `bash examples/mesh/scripts/bring-up.sh check`（前置检查通过）——此两级验收的第一级 MUST 在 build 内自动化通过

### Task 13: 三 demo README 与验收案例指引
- [ ] **任务完成**（与 superpowers plan `Task 13`、`tasks.md` 对应条目同步勾选）
- 目标：`examples/monolith/README.md`、`examples/micro/README.md`、`examples/mesh/README.md`：启动步骤、依赖清单（consul/mysql/minikube 等）、对应验收案例编号（E1/E2/E3/E4/E5）与预期结果
- 改动文件：三个 README.md
- 验证方式：人工核对每个 README 含「验收案例」小节且编号与 spec Scenario 一致

### Task 14: configsrc 单测套件（验收案例 B1）
- [ ] **任务完成**（与 superpowers plan `Task 14`、`tasks.md` 对应条目同步勾选）
- 目标：补齐 `internal/configsrc` 测试：三裁决规则（本地兜底/远程 key 覆盖/env 覆盖地址）、dataId 空纯本地、远端不可达即退、`${ENV}` DSN 展开
- 改动文件：`internal/configsrc/*_test.go`
- 验证方式：`go test ./internal/configsrc/ -v` 全绿，覆盖 B1 场景断言

### Task 15: pipeline 顺序单测（验收案例 A2/A3）
- [ ] **任务完成**（与 superpowers plan `Task 15`、`tasks.md` 对应条目同步勾选）
- 目标：事件记录器断言：telemetry.Init 调用序早于 db/cache/mq 构建；退出序 deregister 早于 flush、flush 晚于全部会标 span 资源、logger 最后关闭
- 改动文件：`internal/pipeline/{pipeline_test.go,shutdown_test.go}`
- 验证方式：`go test ./internal/pipeline/ -v` 全绿

### Task 16: component 单测（验收案例 D1/D2）
- [ ] **任务完成**（与 superpowers plan `Task 16`、`tasks.md` 对应条目同步勾选）
- 目标：strict 默认降级（字段 nil + Warn 含后果文案）/strict 置真返回 `init <name>` 包装 error；`trace.on.<name>=false` 时组件仍构造；registry 失败带病运行
- 改动文件：`internal/component/*_test.go`
- 验证方式：`go test ./internal/component/ -v` 全绿

### Task 17: mode 单测（验收案例 C1/C2/C3）
- [ ] **任务完成**（与 superpowers plan `Task 17`、`tasks.md` 对应条目同步勾选）
- 目标：四档装配分支断言（webx 构造与否、consul 注册与否、srv ServerOptions 产出）；非法 mode/缺失 mode 返回含合法取值列表的 error
- 改动文件：`internal/mode/mode_test.go`
- 验证方式：`go test ./internal/mode/ -v` 全绿

### Task 18: monolith 集成验收（验收案例 E1）
- [ ] **任务完成**（与 superpowers plan `Task 18`、`tasks.md` 对应条目同步勾选）
- 目标：e2e 测试（`examples/monolith/e2e_test.go`，参考 webx/examples/e2e_test.go）：启动 App → GET/POST /orders 断言分层响应 → 发送 SIGTERM/调用 Close 断言优雅退出
- 改动文件：`examples/monolith/e2e_test.go`
- 验证方式：`go test ./examples/monolith/ -v` 全绿

### Task 19: micro 调用链集成验收（验收案例 E2）
- [ ] **任务完成**（与 superpowers plan `Task 19`、`tasks.md` 对应条目同步勾选）
- 目标：`examples/micro/e2e_test.go` 或 `scripts/e2e.sh`：共享底座（`hubx/dockers` compose，consul + kafka + jaeger）就绪 → 启 srv/bff/gateway 三进程 → `curl gateway /api/ping` 与 `/api/order/1001` 断言 gateway→bff→srv 链路返回 → 清理
- 改动文件：`examples/micro/e2e_test.go` 或 `examples/micro/scripts/e2e.sh`
- 验证方式：执行脚本/测试全绿（本地 consul docker 可用前提）

### Task 20: srv-order e2e 回归（验收案例 E3）
- [ ] **任务完成**（与 superpowers plan `Task 20`、`tasks.md` 对应条目同步勾选）
- 目标：将 micro_apps 原下单 e2e 场景对接 `examples/micro/srv`：共享底座（`hubx/dockers` compose，mysql 8.4 + kafka）就绪 → 迁移服务启动 → 下单 RPC 链路（含 outbox 经 kafka 投递断言：topic 自动创建，消费端或直查 kafka 确认消息落地）→ 断言 `examples/micro/srv/cmd/main.go` ≤50 行
- 改动文件：`examples/micro/srv/e2e_test.go`（或 scripts/run-e2e.sh）
- 验证方式：e2e 全绿 + `wc -l` 断言通过

### Task 21: 网格部署验收（验收案例 E4/E5，两级）
- [ ] **任务完成**（与 superpowers plan `Task 21`、`tasks.md` 对应条目同步勾选）
- 目标：两级验收——(a) 硬验收（build 内自动化）：Task 12 的 dry-run + bring-up check 全绿；(b) 人工验证项（close 不硬阻塞）：构建 3 服务镜像 → `examples/mesh/scripts/bring-up.sh` 部署 minikube+Istio → 发起调用，jaeger 查询断言应用 span 与 sidecar span 同 trace（E4）→ `hubx.trace` OFF/ON 重启对比 0 span/全链路（E5），证据记录 `examples/mesh/docs/verify.md`
- 改动文件：`examples/mesh/scripts/*`（如需微调）、`examples/mesh/docs/verify.md`
- 验证方式：(a) 全绿为必达；(b) 若本地无 minikube+Istio 环境，记录「环境未执行」到 verify.md，不阻塞 close

### Task 22: 降级路径验收（验收案例 A3 降级/registry）
- [ ] **任务完成**（与 superpowers plan `Task 22`、`tasks.md` 对应条目同步勾选）
- 目标：`trace.enabled=false` 启动 monolith+micro 各一次断言正常服务与退出；停掉 consul 启动断言 Warn 带病运行可服务；strict.db=true + 无 mysql 断言 Bootstrap 返回 `init db` error
- 改动文件：`launchx_test.go` 或 `examples/*/degrade_test.go`
- 验证方式：`go test ./... -run Degrade` 全绿

### Task 23: 验收案例 A1–E5 对齐核对
- [ ] **任务完成**（与 superpowers plan `Task 23`、`tasks.md` 对应条目同步勾选）
- 目标：核对 A1–E5 每个编号在 `specs/*/spec.md` Scenario 名称与 `tasks.md` 验证项双向可追溯；`openspec validate add-launchx-bootstrap --strict` 通过；`go build ./... && go test ./...` 全绿作最终回归
- 改动文件：无（核对 + 可能的文档微调仅限 `openspec/changes/**`）
- 验证方式：核对清单打勾 + validate 通过 + 全量测试绿
