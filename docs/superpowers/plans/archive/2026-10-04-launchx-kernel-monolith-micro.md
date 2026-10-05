# launchx 内核 + monolith + micro 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 交付 `github.com/gospacex/launchx` 引导库内核与 monolith / micro 两个验证 demo，证明「同一套代码 + 不同 yaml = 单体 / 微服」，并把启动与退出顺序红线固化进框架。

**架构：** 三段式单入口 `Bootstrap(path) → OnSetup(fn) → Run()`。内部为「固定骨架流水线 + 注册表」：骨架步骤不可打乱（配置 → telemetry → 日志 → mode 装配 → 组件挂载 → Setup 回调），组件与配置源以 `Register(name, Builder)` 挂入，四档 `mode` 只产出「服务端 + 服务发现」的差异。退出走显式反序序列，不依赖 defer。

**技术栈：** Go 1.27.1（本机 `/Users/hyx/sdk/go1.24.3` + `GOTOOLCHAIN=auto` 自动拉取）；hubx `*x` SDK（configx / logx / otelx / dbx / cachex / mqx / coordx / webx / traceflag / 新建 rpcx）；kitex 仅经 rpcx 传递引入。

**范围（openspec `tasks.md` 条目号）：** 0.1 / 0.2 / 0.3 / 1.1–1.7 / 1.9 / 2.1–2.4 / 2.6 / 3.1–3.7 / 3.9 / 3.14 / 4.1–4.11。
**不在本轮：** 1.8（debug 伪组件）、2.5（examples/mesh 真实部署）、3.8（E4/E5）、3.10 后置部分、3.11–3.13（O1–O3）。

**2026-10-05 追加：网格链路验收（tasks 4.9–4.11，验收案例 T10–T12）** —— 用户给定「服务网格环境由本地 minikube 提供」，故把网格从"仅代码断言"升级为**有明确断言标准的链路验收**：T10（app span 与 sidecar span 同 trace）、T11（网格下"半断链"反向断言：只开 sidecar 追踪时出现 1 条 sidecar 链 + 应用侧孤儿 trace）、T12（证据落盘含环境版本）。
- **执行状态**：断言标准已写入 `specs/trace-verification/spec.md` 与 `tasks.md`；真实执行依赖 minikube/Istio 底座，而**实测本机 minikube/kubectl/istioctl 均未安装**，故 T10–T12 的真实跑分仍然后置（前置动作见本文件「环境事实」段）。
- **关键待核项**：Istio 侧追踪 provider 选型与传播头一致性（`opentelemetry` provider → Envoy OTel tracer → W3C `traceparent`，与 launchx 的 Composite{TraceContext,Baggage} 对齐；若用 `zipkin` provider 默认走 B3 会断链）。此点 MUST 在真机验证阶段以 `istioctl proxy-config` + 实测请求头名确认，不得凭记忆定稿。

**本轮新增（2026-10-04 用户给定端到端调用链验收标准后追加）：**
- **数据层组件 4 类 → 6 类**：新增 `search`（Elasticsearch，经 `searchx/elasticsearch`）与 `mongodb`（经 `mongodbx`）。两者 SDK 埋点已存在（`db.elasticsearch.*` / `db.mongodb.*`），本轮是装配契约不是新增埋点。
- **出向注入工具（1.9）**：`webx/middleware/otel.go` 只有 Extract 无 Inject，gateway→bff 的 HTTP 续链需调用方注入。
- **trace 验收 T1–T9（4.1–4.8）**：Jaeger 端到端调用链断言，见 `specs/trace-verification/spec.md`。
- **0.3 底座前置已完成**：`examples/scripts/bring-up-trace.sh` 已落地并实测 `check` 全绿（jaeger 16686/4318、mysql 3306、kafka 9092、elasticsearch 9200 `green`）。执行时无需重做，直接进入任务 1。

---

## 已核实的环境事实（写死，不要重新探测）

- **Go 工具链**：`export PATH=/Users/hyx/sdk/go1.24.3/bin:$PATH`，`GOTOOLCHAIN=auto` 会自动下载 go1.27.1（webx 声明 `go 1.27.1`，dbx `go 1.26.2`，mqx `go 1.27`）。不需要手工装新版。
- **hubx SDK 全部未发布**（模块缓存里只有 `mqx@v0.0.1`），因此 launchx 的 `go.mod` **必须**用 `replace` 指向本地兄弟目录。launchx 位于 `/Users/hyx/work/gowork/src/hubx/launchx`，故相对路径均为 `../<name>`。
- **独立 module（各有自己的 go.mod，需各自 replace）**：`mqx/kafka`、`cachex/redis`、`coordx/consul`、`coordx/nacos`。`dbx/orm`、`dbx/config`、`logx/zap` 无独立 go.mod，属父 module。
- **`coordx/consul` 修复已落地**（实测：Check 仅 `TTL:"30s"`、`startBeat`/`stopBeat` 在、`ttlBeatInterval=10s`）。任务 2 是**复验**不是修复。
- **`otelx.New` 只注册一半全局（2026-10-04 实测更正）**：它**会**调用 `otel.SetTracerProvider(tp)`（`otelx/interface.go:207`，仅 `Tracer.Enabled` 为真时；关闭态返回 noop 且不碰全局），但**从不**调用 `otel.SetTextMapPropagator`。我先前写成"源码零 `otel.Set*`、必须自己设两者"是错的，**不要照抄**。
  - 后果一（跨服务断链）：OTel 全局默认传播器是**空 Composite**（`otel@v1.46.0 internal/global/propagator.go`：`noop: propagation.NewCompositeTextMapPropagator()`），注入与提取皆空操作 → `traceparent` 既不写出也不读取 → gateway→bff→srv 断成三段孤儿 trace。它**伪装成成功**（三服务各一根 trace，每条都能点开有 span），极难发现。故 launchx MUST 显式 `otel.SetTextMapPropagator(Composite{TraceContext, Baggage})`（launchx 再调一次 `SetTracerProvider` 是幂等无害的，保留之）。
  - 后果二（静默不产 span）：`webx.Config.FillDefaults` 里 `Tracer` 默认取 `otel.GetTracerProvider().Tracer("webx")`。**所以 telemetry 必须先于 mode 装配，否则 webx 拿到 noop tracer** —— 这条不是形式主义，是实测出来的因果。
- **e2e 底座**：`docker compose -f /Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml up -d consul mysql kafka jaeger redis`。consul `127.0.0.1:8500`；mysql `127.0.0.1:3306`（demo 库 `order_db`，账号 `order_rw` / `hubx123`）；kafka `127.0.0.1:9092`；jaeger OTLP `127.0.0.1:4318`、UI `16686`；redis `127.0.0.1:6379`。
- **中间件底座（2026-10-05 用户给定，写死）**：**所有中间件一律由本机 docker compose 统一供给** —— `/Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml`（bridge 网络 `devnet`，25 个默认服务 + 3 个 profile 服务）。该文件已把历史上分散在 17 个 compose 项目的 54 个容器去重收敛，同类只留一个实例、端口统一到标准端口。相关地址：consul 8500、mysql 3306（root/rootpass，另有 testuser/testpass）、postgres 5432、redis 6379 / keydb 6380（密码 keydb123456）/ memcached 11211、kafka 9092（KRaft，无 zk）、rabbitmq 5672+15672、rocketmq 9876、nsq 4150/4161、nacos 8848+9848/9849（无鉴权）、etcd 2379、mongo 副本集 27018/27019/27020（**需先 `docker compose --profile once up -d mongo-init` 初始化 rs0**）、minio 9000/9001、jaeger 16686/4317/4318、apollo 18070/18085（宿主侧避让端口）。
  - **坑：Elasticsearch 不在该 compose 内**。当前 `9200` 由独立容器 `elasticsearch` 提供（实测 green）。若整套环境重建，必须自行补 ES，否则 `search` 组件（searchx）无法验收。
  - docker CLI 绝对路径：`/Applications/Docker.app/Contents/Resources/bin/docker`（PATH 里没有）。
- **服务网格环境（2026-10-05 用户给定）**：网格由本机 **minikube** 提供，中间件仍走上面的 docker compose（k8s 内服务经 `host.docker.internal` / 宿主 IP 访问宿主中间件）。
  - **实测：`minikube`、`kubectl`、`helm`、`istioctl`、`linkerd` 均未安装**（`which` 全空，brew 未装相关 formula）。
  - 影响面：本轮 mesh 已决定后置（仅代码断言 T8，不做真实验证），故**不阻塞任务 5–17**。真实网格验证留到后续迭代，前置动作是 `brew install minikube kubectl` + `minikube start --driver=docker` + 把 demo 镜像 `minikube image load` 进集群，并注意 k8s 内到宿主中间件的地址改写。

### 关键 API 签名（实现时直接照此调用，不要另找）

```go
// configx
configx.ReadConfig(configx.ConfigOptions{ConfigFile: "config.yaml", ConfigType: "yaml"}) (configx.Config, error)
// Config: GetString/GetBool/GetInt/Get/Unmarshal/IsSet/Close
// configx 构造时已自动把顶层 traceflag.enabled 映射到 traceflag.SetEnabled

// otelx
otelx.New(&otelx.Config{ServiceName, Tracer: otelx.TracerConfig{EnableTrace, Exported, Endpoint}, Sampler}) (otelx.Provider, error)
// Provider: TracerProvider() / MeterProvider() / Close()
// 注意：New 只注册 TracerProvider（otelx/interface.go:207），不注册传播器。
// launchx 必须自己 otel.SetTextMapPropagator(Composite{TraceContext, Baggage})，
// 否则跨服务链路断成孤儿 trace（详见「已核实的环境事实」）。

// logx
logx.New(cfg logx.Config) (logx.Logger, error)   // Logger 含 Close() error
logx.SetGlobal(l)                                // logx.GetGlobal()

// webx（App 内嵌 *server.Hertz，可直接 app.GET(...)）
webx.New(webx.WithConfig(cfg), webx.WithLogger(l), webx.WithTracer(t),
         webx.WithMeterProvider(mp), webx.WithPreRun(func(a *webx.App))) (*webx.App, error)
app.Run() error ; app.Shutdown(ctx) error

// coordx（blank import _ "github.com/gospacex/coordx/consul" 自注册 creator）
coordx.NewRegister(&coordx.Config{Driver: "consul", Addr: "127.0.0.1:8500", EnableTrace: b}) (coordx.Register, error)
coordx.NewDiscover(&coordx.Config{...}) (coordx.Discover, error)
// Register(ctx, service string, inst *coordx.ServiceInstance) error / Deregister / Close
// Discover.Discovery(ctx, service) ([]*coordx.ServiceInstance, error)
// ServiceInstance{ID, ServiceID, Addr string, Port int, Metadata, Healthy}

// dbx（yaml 键是 xmiddleware.sqlx，不是 dbx）
dbxconfig.LoadMySQLFromConfigx(cc configx.Config, name string) (*dbxconfig.MySQLConfig, *dbxconfig.TracingConfig, error)
orm.Open(cfg DialectorSource, opts ...gorm.Option) (*gorm.DB, error)      // 带 trace
orm.OpenNoTrace(cfg DialectorSource, opts ...gorm.Option) (*gorm.DB, error) // 不带 trace

// cachex（blank import _ "github.com/gospacex/cachex/redis"）
cachex.New([]*cachex.Config{{Name, Driver, Addr, DB, PoolSize, EnableTrace}}) (cachex.Instances, error)
// Instances = map[string]Cache，Cache 含 Close() error

// mqx（blank import _ "github.com/gospacex/mqx/kafka"）
mqx.NewProducer(&mqx.Config{Driver: "kafka", Brokers: []string{...}, EnableTrace: b, CallChainEnabled: b}) (mqx.Producer, error)
// Producer: Send(ctx, *mqx.Message) error / SendBatch / Close / Name / HealthCheck

// traceflag
traceflag.Apply(component string, enable bool) bool   // 实例级 AND 组件级 AND 全局总闸
traceflag.SetEnabled(false)                            // 全局总闸
```

---

## 文件结构

```
launchx/
├── go.mod                              # replace 全部 hubx SDK 到本地
├── launchx.go                          # Bootstrap 唯一入口
├── app.go                              # App 句柄 / OnSetup / OnStop / Run / Close
├── internal/
│   ├── configsrc/
│   │   ├── registry.go                 # Register(name, Loader) + Resolve
│   │   ├── local.go                    # local 加载器（configx + ExpandEnv 包装）
│   │   ├── nacos.go                    # nacos 加载器（三裁决）
│   │   └── expand.go                   # envConfig 包装器：os.ExpandEnv
│   ├── telemetry/telemetry.go          # otelx 封装 + 全局注册 + Shutdown(15s)
│   ├── component/
│   │   ├── registry.go                 # Register(name, Builder) + Env
│   │   ├── db.go  cache.go  mq.go  consul.go
│   │   └── strict.go                   # strict.<name> 裁决
│   ├── mode/
│   │   ├── mode.go                     # 四档分支 + 非法 mode 报错
│   │   └── webx.go                     # webx 装配（monolith/bff/gateway）
│   └── pipeline/
│       ├── events.go                   # 事件记录器（顺序断言用）
│       ├── pipeline.go                 # 固定启动骨架
│       └── shutdown.go                 # 反序退出序列
└── examples/
    ├── monolith/
    └── micro/{gateway,bff,srv}
```

---

## 任务 1：git 初始化 + module 骨架 + replace 块

**文件：**
- 创建：`go.mod`、`launchx.go`、`app.go`
- 测试：`launchx_test.go`

launchx 当前**不是 git 仓库**（`git rev-parse` 报 fatal），第一步先建仓库。

- [ ] **步骤 1.1：初始化 git 并建 module**

```bash
cd /Users/hyx/work/gowork/src/hubx/launchx
git init
git config user.name >/dev/null 2>&1 || git config user.name "hyx"
printf '%s\n' '.workbuddy/' 'examples/**/bin/' > .gitignore
```

- [ ] **步骤 1.2：写 go.mod（replace 全部本地 SDK）**

```go
module github.com/gospacex/launchx

go 1.27.1

require (
	github.com/gospacex/cachex v0.0.0
	github.com/gospacex/cachex/redis v0.0.0
	github.com/gospacex/configx v0.0.0
	github.com/gospacex/coordx v0.0.0
	github.com/gospacex/coordx/consul v0.0.0
	github.com/gospacex/dbx v0.0.0
	github.com/gospacex/logx v0.0.0
	github.com/gospacex/mqx v0.0.0
	github.com/gospacex/mqx/kafka v0.0.0
	github.com/gospacex/otelx v0.0.0
	github.com/gospacex/rpcx v0.0.0
	github.com/gospacex/traceflag v0.0.0
	github.com/gospacex/webx v0.0.0
)

replace (
	github.com/gospacex/cachex => ../cachex
	github.com/gospacex/cachex/redis => ../cachex/redis
	github.com/gospacex/configx => ../configx
	github.com/gospacex/coordx => ../coordx
	github.com/gospacex/coordx/consul => ../coordx/consul
	github.com/gospacex/dbx => ../dbx
	github.com/gospacex/logx => ../logx
	github.com/gospacex/mqx => ../mqx
	github.com/gospacex/mqx/kafka => ../mqx/kafka
	github.com/gospacex/otelx => ../otelx
	github.com/gospacex/rpcx => ../rpcx
	github.com/gospacex/traceflag => ../traceflag
	github.com/gospacex/webx => ../webx
)
```

`../rpcx` 在任务 3 才创建。因此本步**先不要**跑 `go mod tidy`（会因 `../rpcx` 不存在而失败）；改为按以下顺序：把上面内容写入 go.mod → **暂时删掉 `require`/`replace` 两处 `rpcx` 行** → 跑 `go mod tidy` 拉齐其余依赖。任务 3 建好 `../rpcx` 后，再把这两行加回并重新 `go mod tidy`。

- [ ] **步骤 1.3：写失败测试（A1 一行启动骨架）**

```go
// launchx_test.go
package launchx

import "testing"

func TestBootstrapReturnsApp(t *testing.T) {
	app, err := Bootstrap("testdata/empty.yaml")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if app == nil {
		t.Fatal("app is nil")
	}
	if err := app.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Close 幂等
	if err := app.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestOnSetupRegistered(t *testing.T) {
	app, _ := Bootstrap("testdata/empty.yaml")
	called := false
	app.OnSetup(func(a *App) error { called = true; return nil })
	if len(app.setups) != 1 {
		t.Fatalf("setups = %d, want 1", len(app.setups))
	}
	_ = called
}
```

- [ ] **步骤 1.4：运行测试确认失败**

运行：`go test ./... -run 'TestBootstrap|TestOnSetup' -v`
预期：编译失败（`Bootstrap` / `App` 未定义）

- [ ] **步骤 1.5：最小实现**

```go
// launchx.go
package launchx

import "fmt"

// Bootstrap 是唯一引导入口：加载配置并构造 App。骨架步骤由 pipeline 执行，
// 失败即退（地基错误不降级）。
func Bootstrap(path string) (*App, error) {
	if path == "" {
		return nil, fmt.Errorf("launchx: config path is empty")
	}
	a := &App{configPath: path}
	return a, nil
}
```

```go
// app.go
package launchx

import "sync"

// App 暴露基础设施只读句柄。业务在 OnSetup 内装配自己的分层，
// launchx 不对目录结构做任何约定。
type App struct {
	configPath string
	mu         sync.Mutex
	setups     []func(*App) error
	stops      []func() error
	closed     bool
}

// OnSetup 注册装配回调，Bootstrap 后、Run 前调用。
func (a *App) OnSetup(fn func(*App) error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.setups = append(a.setups, fn)
}

// OnStop 注册退出回调，Run 收尾时按注册逆序执行（早于内置组件 Stop）。
func (a *App) OnStop(fn func() error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stops = append(a.stops, fn)
}

// Run 启动服务并托管退出。
func (a *App) Run() error { return nil }

// Close 幂等执行反序退出链。
func (a *App) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	return nil
}
```

- [ ] **步骤 1.6：建 testdata 并运行测试确认通过**

```bash
mkdir -p testdata && printf 'service:\n  name: test\n  mode: monolith\n' > testdata/empty.yaml
go test ./... -run 'TestBootstrap|TestOnSetup' -v
```

预期：PASS

- [ ] **步骤 1.7：Commit**

```bash
git add go.mod .gitignore launchx.go app.go launchx_test.go testdata/
git commit -m "feat(launchx): 初始化 module 与单入口骨架（A1）"
```

---

## 任务 2：复核 coordx/consul 修复（tasks 0.1）

**文件：** 只读复核 `/Users/hyx/work/gowork/src/hubx/coordx/consul/consul.go`

- [ ] **步骤 2.1：静态复核（应为已修复态）**

```bash
cd /Users/hyx/work/gowork/src/hubx/coordx/consul
grep -n 'TTL\|startBeat\|stopBeat\|ttlBeatInterval' consul.go
grep -n 'Interval' consul.go
```

预期：出现 `TTL: "30s"`、`startBeat`、`stopBeat`、`ttlBeatInterval = 10 * time.Second`；`Interval` 仅出现在注释里（说明 TTL 与 Interval 互斥），不出现在 `api.AgentServiceCheck{}` 字面量中。

- [ ] **步骤 2.2：build / vet**

```bash
cd /Users/hyx/work/gowork/src/hubx/coordx/consul && go build ./... && go vet ./...
```

预期：全绿

- [ ] **步骤 2.3：真机复验（底座 consul 必须已起）**

```bash
docker compose -f /Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml up -d consul
curl -s http://127.0.0.1:8500/v1/status/leader
```

预期：返回 leader 地址（非空）。随后在任务 9（component registry 组件）里用 launchx 的 registry 组件跑通「注册 → passingOnly 发现 1 实例 → 35s 后仍 passing → 注销归零」四项断言。本任务只确认底座与静态事实，**不写代码**。

- [ ] **步骤 2.4：若未修复则补齐**

若步骤 2.1 发现 `api.AgentServiceCheck{}` 里仍有 `Interval`，则删掉 `Interval` 只留 `TTL: "30s"`，并确认 `startBeat` 在 `Register` 末尾被调用、`stopBeat` 在 `Deregister` 与 `Close` 中被调用。改完重跑 2.2。

---

## 任务 3：新建 `hubx/rpcx` 模块（tasks 0.2）

**文件：**
- 创建：`/Users/hyx/work/gowork/src/hubx/rpcx/{go.mod,options.go,server.go,client.go,README.md}`
- 测试：`/Users/hyx/work/gowork/src/hubx/rpcx/options_test.go`

实现自 `micro_apps/pkg/rpcx/options.go` 移植（已知可用），**发现走 coordx，禁 regdisx**。

> **移植源绝对路径（2026-10-04 实测）**：`/Users/hyx/work/gowork/src/hubx/micro_apps/pkg/rpcx/`
> 含 `options.go`(6165B) / `middleware.go`(4207B) 与对应测试。
> **注意不要用** `/Users/hyx/orca/workspaces/micro_apps/micro_apps/...`——该路径已失效。

- [ ] **步骤 3.1：建 module**

```bash
mkdir -p /Users/hyx/work/gowork/src/hubx/rpcx
cd /Users/hyx/work/gowork/src/hubx/rpcx
cat > go.mod <<'EOF'
module github.com/gospacex/rpcx

go 1.27.1
EOF
```

- [ ] **步骤 3.2：写失败测试**

```go
// options_test.go
package rpcx

import "testing"

func TestBuildServerOptionsIncludesAddr(t *testing.T) {
	opts := BuildServerOptions(ServerOptions{
		ServiceName: "srv-order",
		Addr:        "127.0.0.1:9013",
		RegistryAddr: "127.0.0.1:8500",
	})
	if len(opts) == 0 {
		t.Fatal("no server options produced")
	}
}

func TestBuildClientOptionsIncludesTarget(t *testing.T) {
	opts := BuildClientOptions(ClientOptions{
		ServiceName:  "srv-order",
		RegistryAddr: "127.0.0.1:8500",
		Timeout:      3 * time.Second,
	})
	if len(opts) == 0 {
		t.Fatal("no client options produced")
	}
}
```

- [ ] **步骤 3.3：运行确认失败**

运行：`cd /Users/hyx/work/gowork/src/hubx/rpcx && go test ./... -v`
预期：编译失败（`BuildServerOptions` 未定义）

- [ ] **步骤 3.4：移植实现**

```go
// options.go
// Package rpcx 收敛 kitex 服务端/客户端 Option 的装配，是 hubx 家族第 15 个 x SDK。
//
// 与 micro_apps/pkg/rpcx 的关系：本包由该包移植而来并作为其收敛目标；
// 与 gospacex/rpcx（模块名裸 rpcx、依赖 regdisx）无关，不得混用。
//
// 服务发现经 coordx：本包不引入 regdisx 或任何其它注册中心实现，
// 否则 hubx 家族会出现两套注册中心。
//
// 调用链：套件挂上 obs-opentelemetry 的 Server/Client Suite 后，kitex 的
// 入站/出站调用会经 propagator 注入与提取 traceparent —— 这是跨服务续链落点。
// 全局 TracerProvider 必须**先**由宿主（launchx telemetry）装配好，
// 本套件只从 otel.GetTracerProvider() 取用，绝不自行装配。
package rpcx

import (
	"time"

	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/server"
	kitextracing "github.com/kitex-contrib/obs-opentelemetry/tracing"
	"github.com/gospacex/coordx"
	_ "github.com/gospacex/coordx/consul"
)

// 缺省超时。零值会导致「立即超时」或「无限等待」，两者都不期望。
const (
	defaultRPCTimeout        = 3 * time.Second
	defaultConnectTimeout    = 500 * time.Millisecond
	defaultReadWriteTimeout  = 30 * time.Second
	defaultExitWait          = 5 * time.Second
)

// ServerOptions kitex 服务端装配参数
type ServerOptions struct {
	// ServiceName 服务名（与注册表一致）
	ServiceName string
	// Addr 监听地址，形如 "127.0.0.1:9013"
	Addr string
	// RegistryAddr 注册中心地址，形如 "127.0.0.1:8500"；空则不注册
	RegistryAddr string
	// TraceEnabled 是否挂 obs-opentelemetry 续链套件
	TraceEnabled bool
	// ExitWait 优雅退出等待 in-flight 排空上限
	ExitWait time.Duration
}

// BuildServerOptions 产出 kitex 服务端 Option 列表。
func BuildServerOptions(o ServerOptions) []server.Option {
	opts := []server.Option{
		server.WithServerBasicInfo(nil),
		server.WithExitWait(o.exitWait()),
	}
	if o.Addr != "" {
		opts = append(opts, server.WithServiceAddr(addrToNetAddr(o.Addr)))
	}
	if o.RegistryAddr != "" {
		if r, err := coordx.NewRegister(&coordx.Config{
			Driver: "consul", Addr: o.RegistryAddr, EnableTrace: o.TraceEnabled,
		}); err == nil {
			opts = append(opts, server.WithRegistry(newCoordxRegistry(r, o.ServiceName, o.Addr)))
		}
	}
	if o.TraceEnabled {
		opts = append(opts, server.WithSuite(kitextracing.NewServerSuite()))
	}
	return opts
}
```

> 说明：`addrToNetAddr` 与 `newCoordxRegistry`（把 `coordx.Register` 适配成 kitex `registry.Registry`）在同一包内实现，各约 40 行；移植来源是 `micro_apps/pkg/rpcx/options.go` 的同名逻辑与 `hubx/micro_apps/pkg/rpcx/middleware.go`。

```go
// client.go（要点）
// BuildClientOptions 产出 kitex 客户端 Option 列表；发现走 coordx resolver。
func BuildClientOptions(o ClientOptions) []client.Option {
	opts := []client.Option{
		client.WithRPCTimeout(o.timeout()),
		client.WithConnectTimeout(defaultConnectTimeout),
	}
	if o.RegistryAddr != "" {
		if d, err := coordx.NewDiscover(&coordx.Config{
			Driver: "consul", Addr: o.RegistryAddr, EnableTrace: o.TraceEnabled,
		}); err == nil {
			opts = append(opts, client.WithResolver(newCoordxResolver(d, o.ServiceName)))
		}
	}
	if o.TraceEnabled {
		opts = append(opts, client.WithSuite(kitextracing.NewClientSuite()))
	}
	return opts
}
```

- [ ] **步骤 3.5：运行测试确认通过**

```bash
cd /Users/hyx/work/gowork/src/hubx/rpcx && go mod tidy && go build ./... && go vet ./... && go test ./... -v
```

预期：全绿

- [ ] **步骤 3.6：写 README 并 commit**

README 必须写明三条：① 本包由 `micro_apps/pkg/rpcx` 移植并作为其收敛目标；② 与 `gospacex/rpcx`（裸模块名 + regdisx）无关，不得混用；③ 发现层走 coordx，新增注册中心必须经 coordx 扩展。

```bash
cd /Users/hyx/work/gowork/src/hubx/rpcx && git add -A && git commit -m "feat(rpcx): 新建 hubx/rpcx，kitex 服务端/客户端 Option 装配（coordx 发现）"
```

---

## 任务 4：configsrc 注册表 + local 加载器 + `${ENV}` 展开（tasks 1.2）

**文件：**
- 创建：`internal/configsrc/{registry.go,local.go,expand.go}`
- 测试：`internal/configsrc/{local_test.go,expand_test.go}`

- [ ] **步骤 4.1：写失败测试**

```go
// local_test.go
package configsrc

import "testing"

func TestLocalLoaderPresenceDefault(t *testing.T) {
	c, err := Resolve("local", Options{Path: "testdata/local.yaml"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := c.GetString("service.name"); got != "test-svc" {
		t.Fatalf("service.name = %q, want %q", got, "test-svc")
	}
}

func TestLocalExpandsEnvInDSN(t *testing.T) {
	t.Setenv("HUBX_DB_PW", "secret123")
	c, _ := Resolve("local", Options{Path: "testdata/local.yaml"})
	want := "order_rw:secret123@tcp(127.0.0.1:3306)/order_db"
	if got := c.GetString("xmiddleware.sqlx.databases.test-svc.dsn"); got != want {
		t.Fatalf("dsn = %q, want %q", got, want)
	}
}

func TestUnknownLoaderErrors(t *testing.T) {
	if _, err := Resolve("etcd", Options{Path: "testdata/local.yaml"}); err == nil {
		t.Fatal("expected error for unregistered loader")
	}
}
```

- [ ] **步骤 4.2：运行确认失败**

运行：`go test ./internal/configsrc/ -v`
预期：编译失败

- [ ] **步骤 4.3：实现**

```go
// registry.go
package configsrc

import (
	"fmt"
	"sort"

	"github.com/gospacex/configx"
)

// Options 配置源输入
type Options struct {
	Path string // 本地 yaml 路径
	// ConfigCenter 段：Type 空则走 local
	Type    string
	DataID  string
	Group   string
	AddrEnv string // 覆盖远端地址的 env 名，如 HUBX_NACOS_SERVER
}

// Loader 配置源加载器
type Loader func(o Options) (configx.Config, error)

var loaders = map[string]Loader{}

// Register 注册配置源加载器（同名覆盖）。
func Register(name string, l Loader) { loaders[name] = l }

// Names 返回已注册加载器名（用于错误信息）。
func Names() []string {
	out := make([]string, 0, len(loaders))
	for k := range loaders {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Resolve 按名选择加载器；Type 为空时用 local。
func Resolve(name string, o Options) (configx.Config, error) {
	if name == "" {
		name = "local"
	}
	l, ok := loaders[name]
	if !ok {
		return nil, fmt.Errorf("configsrc: unknown loader %q, available: %v", name, Names())
	}
	c, err := l(o)
	if err != nil {
		return nil, fmt.Errorf("configsrc: load %q: %w", name, err)
	}
	return c, nil
}
```

```go
// local.go
package configsrc

import (
	"fmt"

	"github.com/gospacex/configx"
)

func init() { Register("local", localLoader) }

// localLoader 经 configx 读本地 yaml，并以 envConfig 包装完成 ${ENV} 展开。
// configx 本身不做 env 展开（micro_apps 既有语义），展开点唯一在本包装层。
func localLoader(o Options) (configx.Config, error) {
	c, err := configx.ReadConfig(configx.ConfigOptions{
		ConfigFile: o.Path,
		ConfigType: "yaml",
	})
	if err != nil {
		return nil, fmt.Errorf("read local config %q: %w", o.Path, err)
	}
	return &envConfig{Config: c}, nil
}
```

```go
// expand.go
package configsrc

import (
	"os"

	"github.com/gospacex/configx"
)

// envConfig 包装 configx.Config，对字符串类读取施加 os.ExpandEnv。
// 展开点唯一在此，避免各处零散 ExpandEnv 导致口径漂移。
type envConfig struct{ configx.Config }

func (e *envConfig) GetString(key string) string {
	return os.ExpandEnv(e.Config.GetString(key))
}

func (e *envConfig) Get(key string) interface{} {
	if s, ok := e.Config.Get(key).(string); ok {
		return os.ExpandEnv(s)
	}
	return e.Config.Get(key)
}
```

- [ ] **步骤 4.4：建 testdata 并运行确认通过**

`internal/configsrc/testdata/local.yaml`：
```yaml
service:
  name: test-svc
  mode: monolith
xmiddleware:
  sqlx:
    driver: gorm
    databases:
      test-svc:
        dsn: "order_rw:${HUBX_DB_PW}@tcp(127.0.0.1:3306)/order_db"
```

运行：`go test ./internal/configsrc/ -v`
预期：3 项 PASS

- [ ] **步骤 4.5：Commit**

```bash
git add internal/configsrc/ && git commit -m "feat(configsrc): 注册表 + local 加载器 + ENV 展开（B1 部分）"
```

---

## 任务 5：nacos 加载器三裁决（tasks 1.3，验收案例 B1）

**文件：**
- 创建：`internal/configsrc/nacos.go`
- 测试：`internal/configsrc/nacos_test.go`

三裁决：① 本地 config.yaml 兜底；② 远端 dataId（`hubx.<service>`）key 级覆盖本地；③ Nacos 地址由 env（`HUBX_NACOS_SERVER`）覆盖 yaml；dataId 为空时纯本地运行。远端不可达按地基失败即退。

- [ ] **步骤 5.1：写失败测试**

```go
// nacos_test.go
package configsrc

import (
	"strings"
	"testing"
)

func TestNacosDataIDEmptyFallsBackToLocal(t *testing.T) {
	c, err := Resolve("nacos", Options{Path: "testdata/local.yaml", Type: "nacos", DataID: ""})
	if err != nil {
		t.Fatalf("empty dataId must run pure local: %v", err)
	}
	if c.GetString("service.name") != "test-svc" {
		t.Fatal("local fallback broken")
	}
}

func TestNacosUnreachableIsFatal(t *testing.T) {
	_, err := Resolve("nacos", Options{
		Path: "testdata/local.yaml", Type: "nacos",
		DataID: "hubx.test-svc", AddrEnv: "HUBX_NACOS_SERVER",
	})
	if err == nil {
		t.Fatal("unreachable nacos must be fatal (地基失败不降级)")
	}
	if !strings.Contains(err.Error(), "hubx.test-svc") {
		t.Fatalf("error must carry dataId context, got: %v", err)
	}
}
```

> 依赖真机 nacos 的 key 级裁决断言（本地与远端同键不同值 → 远端生效）以 `t.Skip` 形态保留：无 nacos 底座时跳过，不阻塞。

- [ ] **步骤 5.2：运行确认失败**

运行：`go test ./internal/configsrc/ -run Nacos -v`
预期：编译失败

- [ ] **步骤 5.3：实现**

```go
// nacos.go
package configsrc

import (
	"fmt"
	"os"
	"strings"

	"github.com/gospacex/configx"
)

func init() { Register("nacos", nacosLoader) }

// nacosLoader 本地兜底 + 远端 dataId key 级覆盖 + env 覆盖地址。
//
// 三裁决（与 micro_apps infra/nacos.LoadUnified 同语义）：
//  1. 本地 config.yaml 兜底：远端没有的键保留本地值；
//  2. 远端 dataId（hubx.<service>）key 级覆盖本地同键值；
//  3. Nacos 地址由 env（HUBX_NACOS_SERVER）覆盖 yaml。
//
// dataId 为空 → 纯本地运行，不发起任何远端请求。
// 远端不可达 → 返回 error（地基失败不降级，不受 strict 影响）。
func nacosLoader(o Options) (configx.Config, error) {
	local, err := localLoader(o)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(o.DataID) == "" {
		return local, nil // 纯本地
	}
	addr := ""
	if o.AddrEnv != "" {
		addr = os.Getenv(o.AddrEnv)
	}
	if addr == "" {
		addr = local.GetString("configcenter.nacos.server_addr")
	}
	if addr == "" {
		return local, nil
	}
	remote, err := configx.ReadConfig(configx.ConfigOptions{
		ConfigFile: o.Path,
		ConfigType: "yaml",
		Remote: configx.RemoteConfig{
			Enabled:  true,
			Provider: "nacos",
			Nacos: configx.NacosConfig{
				DataID:        o.DataID,
				Group:         o.Group,
				ServerConfigs: parseServerAddrs(addr),
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("nacos dataId=%q addr=%q: %w", o.DataID, addr, err)
	}
	return &envConfig{Config: remote}, nil
}
```

`parseServerAddrs(addr string) []configx.NacosServerConfig`：按 `,` 切分，每项按 `host:port` 解析成 `{IP, Port}`，约 15 行，同文件内实现。

- [ ] **步骤 5.4：运行确认通过**

运行：`go test ./internal/configsrc/ -v`
预期：`TestNacosDataIDEmptyFallsBackToLocal` PASS；`TestNacosUnreachableIsFatal` PASS（无 nacos 时 configx 连接失败即报错）

- [ ] **步骤 5.5：Commit**

```bash
git add internal/configsrc/ && git commit -m "feat(configsrc): nacos 加载器三裁决，dataId 空纯本地、不可达即退（B1）"
```

---

## 任务 6：telemetry 封装 + 事件记录器 + 固定启动骨架（tasks 1.6，验收案例 A2）

**文件：**
- 创建：`internal/telemetry/telemetry.go`、`internal/pipeline/{events.go,pipeline.go}`
- 测试：`internal/pipeline/pipeline_test.go`

这是**顺序红线的落点**。`otelx.New` 不注册全局，所以 telemetry 必须显式 `otel.SetTracerProvider` + `otel.SetTextMapPropagator`，且必须早于 mode 装配（webx 的 `Tracer` 默认取全局）。

- [ ] **步骤 6.1：写失败测试（顺序断言）**

```go
// pipeline_test.go
package pipeline

import (
	"errors"
	"testing"

	"github.com/gospacex/logx"
)

// errBoom 步骤失败用哨兵错误
var errBoom = errors.New("boom")

// nopLogger 测试用空日志器。嵌入 logx.Logger 接口以获得全部方法，
// 只显式覆盖 Warn（退出链失败路径会调用）；其余方法在本用例中不会被触达。
type nopLogger struct{ logx.Logger }

func (nopLogger) Warn(msg string, fields ...logx.Field) {}
func (nopLogger) Error(msg string, fields ...logx.Field) {}

func TestStartupOrderTelemetryBeforeComponents(t *testing.T) {
	ev := NewEvents()
	// 用最小 stub 跑骨架：各步只记录事件，不真连外部
	err := Run(&Env{Events: ev, Logger: nopLogger{}}, []Step{
		{Name: "config.load", Fn: func(*Env) error { return nil }},
		{Name: "telemetry.init", Fn: func(*Env) error { return nil }},
		{Name: "logger.init", Fn: func(*Env) error { return nil }},
		{Name: "mode.assemble", Fn: func(*Env) error { return nil }},
		{Name: "component.mount", Fn: func(*Env) error { return nil }},
		{Name: "setup", Fn: func(*Env) error { return nil }},
	})
	if err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	tele := ev.IndexOf("telemetry.init")
	comp := ev.IndexOf("component.mount")
	if tele < 0 || comp < 0 {
		t.Fatalf("missing events: %v", ev.Order())
	}
	if tele >= comp {
		t.Fatalf("telemetry.init(%d) must precede component.mount(%d): %v", tele, comp, ev.Order())
	}
}

func TestStepFailureAborts(t *testing.T) {
	ev := NewEvents()
	err := Run(&Env{Events: ev, Logger: nopLogger{}}, []Step{
		{Name: "config.load", Fn: func(*Env) error { return errBoom }},
		{Name: "telemetry.init", Fn: func(*Env) error { return nil }},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if ev.IndexOf("telemetry.init") >= 0 {
		t.Fatal("later steps must not run after failure")
	}
}
```

- [ ] **步骤 6.2：运行确认失败**

运行：`go test ./internal/pipeline/ -v`
预期：编译失败

- [ ] **步骤 6.3：实现事件记录器与骨架**

```go
// events.go
package pipeline

import "sync"

// Events 事件记录器：顺序断言的唯一依据，不靠日志文本匹配。
type Events struct {
	mu   sync.Mutex
	seq  []string
	idx  map[string]int
}

func NewEvents() *Events { return &Events{idx: map[string]int{}} }

// Record 记录一步（同名只记首次出现）。
func (e *Events) Record(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.idx[name]; ok {
		return
	}
	e.idx[name] = len(e.seq)
	e.seq = append(e.seq, name)
}

// Order 返回调用序快照。
func (e *Events) Order() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.seq))
	copy(out, e.seq)
	return out
}

// IndexOf 返回该事件首次出现的下标，未出现为 -1。
func (e *Events) IndexOf(name string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if i, ok := e.idx[name]; ok {
		return i
	}
	return -1
}
```

```go
// pipeline.go
package pipeline

import (
	"context"
	"fmt"

	"github.com/gospacex/logx"
	"github.com/gospacex/otelx"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// Step 骨架步骤。骨架顺序固定，不允许业务打乱。
type Step struct {
	Name string
	Fn   func(*Env) error
}

// Env 骨架执行环境（组件包只面向 Env，不回指根包，避免 import 环）
type Env struct {
	Events  *Events
	Logger  logx.Logger
	TracerP otelx.Provider
	Ctx     context.Context
}

// Run 按序执行骨架；任一步失败立即返回，后续步不执行。
func Run(e *Env, steps []Step) error {
	for _, s := range steps {
		e.Events.Record(s.Name)
		if err := s.Fn(e); err != nil {
			return fmt.Errorf("pipeline step %q: %w", s.Name, err)
		}
	}
	return nil
}

// InitTelemetry 装配 otelx 并注册全局。
//
// 关键（2026-10-04 实测更正）：otelx.New **会**注册 TracerProvider
//（otelx/interface.go:207 的 otel.SetTracerProvider），但**不会**注册传播器。
// 而 webx.Config.FillDefaults 里 Tracer 默认取 otel.GetTracerProvider().Tracer("webx")。
//
// 因此本步必须做两件事，且必须早于 mode 装配：
//  1. otel.SetTextMapPropagator(Composite{TraceContext, Baggage}) —— 缺省传播器是
//     空 Composite（noop），不注册则 traceparent 既不注入也不提取，跨服务链路断成
//     孤儿 trace，且伪装成成功（各服务都能查到 trace，但 trace-id 不同）；
//  2. otel.SetTracerProvider(...) —— 显式再设一次（幂等），保证 webx 与组件在
//     mode 装配前就能取到真实 provider。否则拿到 noop tracer，症状是「配了
//     enable_trace 却不产 span」——静默故障，极难排查。
func InitTelemetry(serviceName, endpoint string, enabled bool) (otelx.Provider, error) {
	p, err := otelx.New(&otelx.Config{
		ServiceName: serviceName,
		Tracer: otelx.TracerConfig{
			EnableTrace: enabled,
			Exported:    "otlp",
			Endpoint:    endpoint,
		},
		Sampler: otelx.SamplerConfig{Type: "always_on"},
	})
	if err != nil {
		return nil, fmt.Errorf("telemetry init: %w", err)
	}
	otel.SetTracerProvider(p.TracerProvider())
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return p, nil
}
```

- [ ] **步骤 6.4：运行确认通过**

运行：`go test ./internal/pipeline/ -v`
预期：PASS

- [ ] **步骤 6.5：Commit**

```bash
git add internal/pipeline/ internal/telemetry/ && git commit -m "feat(pipeline): 固定启动骨架 + 事件记录器 + telemetry 全局注册（A2）"
```

---

## 任务 7：接通 Bootstrap 完整骨架（tasks 1.1 + 1.6）

**文件：**
- 修改：`launchx.go`、`app.go`
- 测试：`launchx_test.go`

把任务 4/5/6 的产物接到 `Bootstrap`：读配置 → telemetry → logx → mode（占位）→ 组件（占位）→ 记录 Setup。

- [ ] **步骤 7.1：扩展失败测试**

```go
// launchx_test.go 追加
func TestBootstrapStartupOrder(t *testing.T) {
	app, err := Bootstrap("testdata/monolith.yaml")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	defer app.Close()

	order := app.Events().Order()
	want := []string{"config.load", "telemetry.init", "logger.init", "mode.assemble", "component.mount"}
	for _, w := range want {
		if app.Events().IndexOf(w) < 0 {
			t.Fatalf("missing event %q in %v", w, order)
		}
	}
	if app.Events().IndexOf("telemetry.init") >= app.Events().IndexOf("component.mount") {
		t.Fatalf("telemetry must precede components: %v", order)
	}
}

func TestBootstrapBadModeRejected(t *testing.T) {
	_, err := Bootstrap("testdata/badmode.yaml")
	if err == nil {
		t.Fatal("bad mode must be rejected")
	}
	for _, w := range []string{"monolith", "bff", "srv", "gateway"} {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("error must list %q, got: %v", w, err)
		}
	}
}
```

- [ ] **步骤 7.2：运行确认失败**

运行：`go test ./... -run 'TestBootstrapStartupOrder|TestBootstrapBadMode' -v`
预期：失败（`app.Events()` 未定义；badmode 不报错）

- [ ] **步骤 7.3：实现**

`launchx.go` 重写 `Bootstrap`：

```go
func Bootstrap(path string) (*App, error) {
	if path == "" {
		return nil, fmt.Errorf("launchx: config path is empty")
	}
	a := &App{configPath: path, events: pipeline.NewEvents()}

	if err := pipeline.Run(a.env(), []pipeline.Step{
		{Name: "config.load", Fn: a.stepLoadConfig},
		{Name: "telemetry.init", Fn: a.stepTelemetry},
		{Name: "logger.init", Fn: a.stepLogger},
		{Name: "mode.assemble", Fn: a.stepMode},
		{Name: "component.mount", Fn: a.stepComponents},
	}); err != nil {
		// 地基或组件失败：已初始化资源按反序退出链关闭后返回
		_ = a.Close()
		return nil, err
	}
	return a, nil
}
```

`app.go` 增加 `Events() *pipeline.Events`、`env() *pipeline.Env`，以及五个 step 方法（mode / component 在任务 8/9 填充，此处 `stepMode` 先做 mode 枚举校验并占位）。

`stepLoadConfig` 读 `service.name`、`service.mode`、`service.addr`、`trace.enabled`、`trace.endpoint`、`configcenter.*`，并调用 `configsrc.Resolve`（`configcenter.type` 缺省为 `local`）。`stepMode` / `stepComponents` 在本任务先做 mode 枚举校验与空挂载占位，任务 8/9 填充实际实现。五个 step 方法写在 `app.go`，每个约 20–30 行。

- [ ] **步骤 7.4：建 testdata 并运行确认通过**

```bash
printf 'service:\n  name: mono\n  mode: monolith\n  addr: 127.0.0.1:8080\ntrace:\n  enabled: false\n' > testdata/monolith.yaml
printf 'service:\n  name: m\n  mode: mesh\n' > testdata/badmode.yaml
go test ./... -run 'TestBootstrap' -v
```

预期：PASS

- [ ] **步骤 7.5：Commit**

```bash
git add launchx.go app.go launchx_test.go testdata/ && git commit -m "feat(launchx): Bootstrap 接通完整启动骨架，非法 mode 拒绝启动（A1/A2/C4）"
```

---

## 任务 8：组件注册表 + 六类组件（tasks 1.4，验收案例 D1/D2 + T2/T3/T4）

**文件：**
- 创建：`internal/component/{registry.go,db.go,cache.go,mq.go,consul.go,search.go,mongodb.go,strict.go}`
- 测试：`internal/component/component_test.go`

**六类组件：** db（`dbx/orm`）/ cache（`cachex/redis`）/ mq（`mqx/kafka`）/ registry（`coordx/consul`）/ **search（`searchx/elasticsearch`）** / **mongodb（`mongodbx`）**。
后两者经 presence-based 启用（配了 `xmiddleware.searchx` / `xmiddleware.mongodbx` 段才构造），`strict.search` / `strict.mongodb` 沿用同一裁决。三者（`searchx/elasticsearch`、`cachex/redis`、`mqx/kafka`、`coordx/consul`）均为独立 go module，go.mod 需各自 `replace`。
实测两个 SDK 埋点已存在，本任务只做装配契约：`searchx/elasticsearch` 产 `db.elasticsearch.index/get/delete/search/bulk`；`mongodbx` 产 `db.mongodb.insert/find/update/delete/aggregate/...`（`mongodbx/tracing.go` 只是全局开关，span 在 `mongodb.go`）。

**红线：** `trace.on.<name>` 只决定调 `orm.Open` 还是 `orm.OpenNoTrace`（是否入链），**不决定组件是否构造**。历史上把它当构造开关，导致 producer 变 nil、outbox 静默堆积到 retry_count=10 才放弃。判定经 `traceflag.Apply`。

- [ ] **步骤 8.1：写失败测试**

```go
// component_test.go
package component

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gospacex/configx"
	"github.com/gospacex/logx"
)

// warnLogger 捕获 Warn 文案供降级断言。嵌入 logx.Logger 获得全部方法，
// 只覆盖 Warn / Error（降级与失败路径会调用）。
type warnLogger struct {
	logx.Logger
	warns []string
}

func (w *warnLogger) Warn(msg string, fields ...logx.Field) {
	w.warns = append(w.warns, msg+" "+fmt.Sprint(fields))
}
func (w *warnLogger) Error(msg string, fields ...logx.Field) {}
func (w *warnLogger) joined() string                         { return strings.Join(w.warns, "\n") }

// newTestEnv 从内联 yaml 构造 Env（不依赖外部底座）。
func newTestEnv(t *testing.T, yaml string) (*Env, *warnLogger) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := configx.ReadConfig(configx.ConfigOptions{ConfigFile: path, ConfigType: "yaml"})
	if err != nil {
		t.Fatal(err)
	}
	w := &warnLogger{}
	return &Env{
		Cfg: c, ServiceName: "srv-order", Logger: w,
		TraceOn: map[string]bool{}, Strict: map[string]bool{},
	}, w
}

const mqYAML = `
service: {name: srv-order, mode: srv}
xmiddleware:
  mqx:
    driver: kafka
    instances:
      srv-order:
        brokers: ["127.0.0.1:9092"]
`

// D1：trace.on.mq=false 时 producer 仍被构造（只不产 span）。
// 这是历史 bug 的回归网：曾把该开关当构造开关，producer 变 nil，
// outbox 静默堆积到 retry_count=10 才放弃。
func TestTraceOffStillConstructsMQ(t *testing.T) {
	env, _ := newTestEnv(t, mqYAML)
	env.TraceOn["mq"] = false
	b := Lookup("mq")
	if b == nil {
		t.Fatal("mq builder not registered")
	}
	if err := b(env); err != nil {
		t.Fatalf("mq must be constructed even with trace off: %v", err)
	}
	if env.MQ == nil {
		t.Fatal("MQ must not be nil when trace.on.mq=false (D1)")
	}
}

const cacheYAML = `
service: {name: srv-order, mode: srv}
xmiddleware:
  cachex:
    driver: redis
    instances:
      srv-order: {addr: "127.0.0.1:1"}
`

// D2：cache 失败默认降级（字段 nil + Warn 含后果文案）
func TestCacheFailureDegradesByDefault(t *testing.T) {
	env, w := newTestEnv(t, cacheYAML)
	if err := Lookup("cache")(env); err != nil {
		t.Fatalf("default strict=false must not fail: %v", err)
	}
	if env.Cache != nil {
		t.Fatal("cache must be nil in degraded state")
	}
	if !strings.Contains(w.joined(), "缓存穿透") {
		t.Fatalf("warn must carry consequence, got: %q", w.joined())
	}
}

const dbYAML = `
service: {name: srv-order, mode: srv}
xmiddleware:
  sqlx:
    driver: gorm
    databases:
      srv-order: {dsn: "bad@tcp(127.0.0.1:1)/nodb"}
`

func TestStrictTrueFailsFast(t *testing.T) {
	env, _ := newTestEnv(t, dbYAML)
	env.Strict["db"] = true
	err := Lookup("db")(env)
	if err == nil || !strings.Contains(err.Error(), "init db") {
		t.Fatalf("strict.db=true must return 'init db' error, got: %v", err)
	}
}
```

- [ ] **步骤 8.2：运行确认失败**

运行：`go test ./internal/component/ -v`
预期：编译失败

- [ ] **步骤 8.3：实现注册表与裁决**

```go
// registry.go
package component

import (
	"github.com/gospacex/cachex"
	"github.com/gospacex/coordx"
	"github.com/gospacex/logx"
	"github.com/gospacex/mqx"
	"gorm.io/gorm"
)

// Env 组件构造环境。组件包只面向 Env，不回指 launchx 根包。
type Env struct {
	Cfg     configx.Config
	ServiceName string
	Logger  logx.Logger
	TraceOn map[string]bool
	Strict  map[string]bool
	DB      *gorm.DB
	Cache   cachex.Instances
	MQ      mqx.Producer
	// ...（测试用字段略）
}

// Builder 组件构造器
type Builder func(*Env) error

var builders = map[string]Builder{}

func Register(name string, b Builder) { builders[name] = b }
func Lookup(name string) Builder      { return builders[name] }
```

```go
// strict.go
// Strict 裁决组件失败：
//   - strict.<name> 缺省(false) → 字段置 nil 降级 + 带后果文案的 Warn，启动继续；
//   - true → 返回带 "init <name>" 上下文的 error（Bootstrap 会走完整退出链）。
//
// 地基（配置/telemetry/logx）失败不受 strict 影响，始终即退。
func degrade(env *Env, name, consequence string, err error) error {
	if env.Strict[name] {
		return fmt.Errorf("init %s: %w", name, err)
	}
	env.Logger.Warn("launchx: component degraded",
		logx.String("component", name),
		logx.Err(err),
		logx.String("consequence", consequence),
		logx.String("hint", "set strict."+name+": true to fail fast"),
	)
	return nil
}
```

```go
// db.go（要点，展示 trace 门控的正确形态）
func buildDB(env *Env) error {
	cfg, _, err := dbxconfig.LoadMySQLFromConfigx(env.Cfg, env.ServiceName)
	if err != nil {
		return degrade(env, "db", "订单写入不可用，请求将 5xx", err)
	}
	// 关键：trace.on.db 只决定入不入链，不决定构不构造。
	if traceOn(env, "db") {
		env.DB, err = orm.Open(cfg)
	} else {
		env.DB, err = orm.OpenNoTrace(cfg)
	}
	if err != nil {
		return degrade(env, "db", "订单写入不可用，请求将 5xx", err)
	}
	return nil
}

// traceOn 经 traceflag 判定：实例级 AND 组件级 AND 全局总闸。
// launchx 只把 yaml trace.on.<name> 映射为组件级注册，不自造布尔逻辑。
func traceOn(env *Env, name string) bool {
	return traceflag.Apply(name, env.TraceOn[name])
}
```

`cache.go` / `mq.go` / `consul.go` 同形：`cachex.New` + `_ "github.com/gospacex/cachex/redis"`；`mqx.NewProducer` + `_ "github.com/gospacex/mqx/kafka"`；`coordx.NewRegister` + `_ "github.com/gospacex/coordx/consul"`。降级后果文案分别为「缓存穿透 / 限流 fail-open」「事件投递中断，outbox 将堆积重试」「服务不被发现，上游调用 502」。

- [ ] **步骤 8.4：运行确认通过**

运行：`go test ./internal/component/ -v`
预期：PASS（D1/D2 + strict 分支）

- [ ] **步骤 8.5：Commit**

```bash
git add internal/component/ && git commit -m "feat(component): 四组件注册表 + traceflag 门控 + strict 降级（D1/D2）"
```

---

## 任务 9：mode 四档装配（tasks 1.5，验收案例 C1/C2/C3）

**文件：**
- 创建：`internal/mode/{mode.go,webx.go,srv.go}`
- 测试：`internal/mode/mode_test.go`

映射表：

| mode | webx | RPC 服务端 | consul 注册 | 下游客户端 |
|---|---|---|---|---|
| monolith | ✅ | — | ❌ | — |
| bff | ✅ | — | ✅ | ✅（rpcx） |
| gateway | ✅ | — | ✅ | 发现上游 |
| srv | ❌ | ✅（rpcx） | ✅ | — |

- [ ] **步骤 9.1：写失败测试**

```go
// mode_test.go
package mode

import "testing"

// testEnv 构造最小装配环境。Addr 用端口 0，Assemble 只装配不监听；
// Logger 留 nil（本装配路径不写日志），RegistryAddr 指向底座 consul。
func testEnv(mode string) *Env {
	return &Env{
		Mode: mode, ServiceName: "order-svc",
		Addr: "127.0.0.1:0", RegistryAddr: "127.0.0.1:8500", TraceEnabled: false,
	}
}

func TestFourModesAssemble(t *testing.T) {
	cases := []struct {
		mode        string
		wantWebx    bool
		wantRPC     bool
		wantReg     bool
	}{
		{"monolith", true, false, false},
		{"bff", true, false, true},
		{"gateway", true, false, true},
		{"srv", false, true, true},
	}
	for _, c := range cases {
		out, err := Assemble(testEnv(c.mode))
		if err != nil {
			t.Fatalf("%s: %v", c.mode, err)
		}
		if (out.Webx != nil) != c.wantWebx {
			t.Fatalf("%s: webx=%v want %v", c.mode, out.Webx != nil, c.wantWebx)
		}
		if (out.RPCServer != nil) != c.wantRPC {
			t.Fatalf("%s: rpc=%v want %v", c.mode, out.RPCServer != nil, c.wantRPC)
		}
		if (out.Registry != nil) != c.wantReg {
			t.Fatalf("%s: registry=%v want %v", c.mode, out.Registry != nil, c.wantReg)
		}
	}
}

func TestMeshModeRejected(t *testing.T) {
	if _, err := Assemble(testEnv("mesh")); err == nil {
		t.Fatal("mesh must be rejected: 网格不是 mode 档，差异只在部署清单")
	}
}
```

- [ ] **步骤 9.2：运行确认失败**

运行：`go test ./internal/mode/ -v`
预期：编译失败

- [ ] **步骤 9.3：实现**

```go
// mode.go
package mode

import "fmt"

// Values 合法取值。mesh 不在其中：网格与 micro 的 Go 侧装配路径完全相同，
// 差异 100% 在部署清单与地址配置副本，做成 mode 会让 launchx 背起 sidecar/k8s。
var Values = []string{"monolith", "bff", "srv", "gateway"}

// Out 装配产物。Webx 在 monolith/bff/gateway 档非 nil、srv 档为 nil；
// RPCServer 只在 srv 档非 nil；Registry 在 monolith 档为 nil。
type Out struct {
	Webx      *webx.App
	RPCServer *RPCServerHandle
	Registry  coordx.Register
}

// Assemble 按 mode 装配服务端与发现能力。
func Assemble(e *Env) (*Out, error) {
	switch e.Mode {
	case "monolith":
		return assembleWebx(e, false, false)
	case "bff":
		return assembleWebx(e, true, true)
	case "gateway":
		return assembleWebx(e, true, false) // gateway 只发现上游，不由 bff 发现它
	case "srv":
		return assembleSrv(e)
	default:
		return nil, fmt.Errorf("mode: invalid service.mode %q, valid values: %v", e.Mode, Values)
	}
}
```

`webx.go` 用 `webx.New(webx.WithConfig(webx.Config{Addr: e.Addr}), webx.WithLogger(e.Logger), webx.WithTracer(e.Tracer), webx.WithMeterProvider(e.MeterProvider), webx.WithPreRun(e.PreRun))`；`srv.go` 经 `rpcx.BuildServerOptions` 产出服务端 Option 工厂（不 import kitex，只调 rpcx）。

- [ ] **步骤 9.4：运行确认通过**

运行：`go test ./internal/mode/ -v`
预期：PASS

- [ ] **步骤 9.5：consul 真机四项断言（底座已起）**

```bash
curl -s 'http://127.0.0.1:8500/v1/health/service/order-srv?passing=1' | head -c 200
```

预期：注册后返回 1 实例；等 35s 再查仍 passing（心跳存活）；注销后归零。这四项并入任务 13 的集成验收执行，本步只确认手工 curl 可达。

- [ ] **步骤 9.6：Commit**

```bash
git add internal/mode/ && git commit -m "feat(mode): 四档装配分支，mesh 不是 mode 档（C1/C2/C3/C4）"
```

---

## 任务 10：反序退出编排（tasks 1.7，验收案例 A3）

**文件：**
- 创建：`internal/pipeline/shutdown.go`
- 修改：`app.go`
- 测试：`internal/pipeline/shutdown_test.go`

退出序列：`server.stop → component.stop → registry.deregister → tracing.shutdown(15s) → logger.close`。**显式序列，不用 defer** —— defer 在手动 flush 之后才执行，那批 span 会被永久丢弃。这是三个参考项目里被复制 11 遍的隐性知识。

- [ ] **步骤 10.1：写失败测试**

```go
// shutdown_test.go
package pipeline

import "testing"

func TestShutdownOrder(t *testing.T) {
	ev := NewEvents()
	Shutdown(&Env{Events: ev, Logger: nopLogger{}}, ShutdownSteps())
	o := ev.Order()
	idx := func(s string) int { return ev.IndexOf(s) }
	if idx("registry.deregister") >= idx("tracing.shutdown") {
		t.Fatalf("deregister must precede flush: %v", o)
	}
	if idx("tracing.shutdown") >= idx("logger.close") {
		t.Fatalf("flush must precede logger close: %v", o)
	}
	// 会标 span 的资源必须全部早于 flush
	for _, s := range []string{"server.stop", "component.stop", "registry.deregister"} {
		if idx(s) >= idx("tracing.shutdown") {
			t.Fatalf("%s must precede flush: %v", s, o)
		}
	}
}
```

- [ ] **步骤 10.2：运行确认失败**

运行：`go test ./internal/pipeline/ -run Shutdown -v`
预期：编译失败

- [ ] **步骤 10.3：实现**

```go
// shutdown.go
package pipeline

import (
	"time"

	"github.com/gospacex/logx"
)

// flushTimeout tracing.Shutdown 的冲刷上限（≥10s，取 15s）。
const flushTimeout = 15 * time.Second

// ShutdownSteps 返回反序退出序列。顺序是红线，不可调整：
//  1. server.stop —— 先停止收新请求；
//  2. component.stop —— 会标 span 的资源（outbox/cache/db）必须先关；
//  3. registry.deregister —— 摘流量后再注销；
//  4. tracing.shutdown —— 15s flush，必须在上述全部关闭**之后**；
//  5. logger.close —— 全程最后，否则后续日志丢失。
//
// 刻意不用 defer：defer 在手动 flush 之后才执行，那批 span 会被永久丢弃。
func ShutdownSteps() []Step {
	return []Step{
		{Name: "server.stop", Fn: stopServer},
		{Name: "component.stop", Fn: stopComponents},
		{Name: "registry.deregister", Fn: deregister},
		{Name: "tracing.shutdown", Fn: shutdownTracing},
		{Name: "logger.close", Fn: closeLogger},
	}
}

// Shutdown 执行退出序列；期间错误只记录不中断，保证后续步都跑完。
func Shutdown(e *Env, steps []Step) {
	for _, s := range steps {
		e.Events.Record(s.Name)
		if err := s.Fn(e); err != nil {
			e.Logger.Warn("launchx: shutdown step failed",
				logx.String("step", s.Name), logx.Err(err))
		}
	}
}

// shutdownTracing 冲刷 span。otelx Provider.Close 内部按自身 shutdownTimeout
// 冲刷；此处外层再套 flushTimeout，保证不会无限挂住退出流程。
func shutdownTracing(e *Env) error {
	if e.TracerP == nil {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- e.TracerP.Close() }()
	select {
	case err := <-done:
		return err
	case <-time.After(flushTimeout):
		return fmt.Errorf("tracing flush exceeded %s, spans may be dropped", flushTimeout)
	}
}
```

`app.go` 的 `Run()`：等 SIGINT/SIGTERM 或服务端退出 → 先逆序执行业务 `OnStop`（注册逆序）→ 再 `pipeline.Shutdown`。`Setup` 失败与运行期错误走同一条链后返回首错。`Close()` 幂等复用该链。

- [ ] **步骤 10.4：运行确认通过**

运行：`go test ./internal/pipeline/ -v`
预期：PASS

- [ ] **步骤 10.5：Commit**

```bash
git add internal/pipeline/ app.go && git commit -m "feat(pipeline): 反序退出编排，显式序列不依赖 defer（A3）"
```

---

## 任务 11：`examples/monolith` 单体 demo（tasks 2.1，验收案例 E1）

**文件：**
- 创建：`examples/monolith/{config.yaml,cmd/main.go,app/setup.go,app/routes.go,internal/{handler,logic,service,repository,model}/*.go}`
- 测试：`examples/monolith/e2e_test.go`

- [ ] **步骤 11.1：写 config.yaml**

```yaml
service:
  name: monolith
  mode: monolith
  addr: 127.0.0.1:8080
trace:
  enabled: true
  endpoint: 127.0.0.1:4318
  on: { db: true }
strict:
  db: true
xmiddleware:
  sqlx:
    driver: gorm
    databases:
      monolith:
        dsn: "order_rw:${HUBX_DB_PW}@tcp(127.0.0.1:3306)/order_db?charset=utf8mb4&parseTime=True&loc=Local"
        max_open_conns: 20
        max_idle_conns: 5
```

- [ ] **步骤 11.2：写 main.go（≤50 行的证明）**

```go
// examples/monolith/cmd/main.go
package main

import (
	"github.com/gospacex/launchx"
	"monolith/app"
)

func main() {
	a, err := launchx.Bootstrap("config.yaml")
	if err != nil {
		panic(err) // 地基失败即退，无资源泄漏
	}
	a.OnSetup(app.Setup)
	if err := a.Run(); err != nil {
		panic(err)
	}
}
```

- [ ] **步骤 11.3：写 app/setup.go（分层装配，launchx 不干涉目录）**

```go
// app/setup.go
package app

import "github.com/gospacex/launchx"

// Setup 装配分层并挂路由。launchx 不对目录结构做任何约定，
// 这里用 handler→logic→service→repository 是 demo 自己的选择。
func Setup(a *launchx.App) error {
	repo := repository.New(a.DB)          // a.DB 由 launchx db 组件注入
	svc := service.New(repo)
	lg := logic.New(svc)
	h := handler.New(lg)
	registerRoutes(a.Server, h)           // a.Server 是 *webx.App；monolith 档非 nil
	a.OnStop(func() error { return nil }) // 无自建资源；db 由 launchx 统一 Stop
	return nil
}
```

`routes.go` 挂 `GET /orders`、`POST /orders`（hertz 方法经内嵌 `*server.Hertz`）。`repository` 用 gorm 落 mysql。

- [ ] **步骤 11.4：起底座并跑通**

```bash
export HUBX_DB_PW=hubx123
docker compose -f /Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml up -d mysql
go run ./examples/monolith/cmd &
curl -s localhost:8080/orders
kill %1   # SIGTERM，观察反序退出日志
```

预期：`[]` 或订单列表 JSON；退出日志依序出现 `server.stop → component.stop → tracing.shutdown → logger.close`。

- [ ] **步骤 11.5：写 e2e 并 commit**

`e2e_test.go`：启动 App → POST/GET /orders 断言落库 → 发 SIGTERM 断言优雅退出（断言 `app.Events().Order()` 末四项为上述序列）。

```bash
git add examples/monolith/ && git commit -m "feat(examples/monolith): 单体 demo 接真实 mysql（E1）"
```

---

## 任务 12：`examples/micro` 三服务（tasks 2.2/2.3/2.4，验收案例 E2/E3）

**文件：**
- 创建：`examples/micro/gateway/{config.yaml,cmd/main.go,app/setup.go,internal/{routing,proxy}/*.go}`
- 创建：`examples/micro/bff/{config.yaml,cmd/main.go,app/setup.go,internal/{handler,downstream}/*.go}`
- 创建（迁移）：`examples/micro/srv/**`

顺序：**先 srv**（bff 要复用它的 `kitex_gen`）。

- [ ] **步骤 12.1：迁移 srv-order 业务代码（不改业务，只换装配）**

> **路径更正（2026-10-04 实测）**：micro_apps 的真实位置是 `/Users/hyx/work/gowork/src/hubx/micro_apps`。
> 旧路径 `/Users/hyx/orca/workspaces/micro_apps/micro_apps` 已失效（该目录现只剩一个 `cachex` 软链和 `.DS_Store`），照抄会直接失败。
> 另：`srv-order` **没有 app 级 `pkg/` 目录**——`kitex_gen` 等共享包在 micro_apps 仓库根的 `pkg/`。故第二条的源路径是仓库根的 `pkg/`，不是 app 下的。

```bash
cd /Users/hyx/work/gowork/src/hubx/launchx
mkdir -p examples/micro/srv
cp -R /Users/hyx/work/gowork/src/hubx/micro_apps/apps/srv-order/internal examples/micro/srv/
cp -R /Users/hyx/work/gowork/src/hubx/micro_apps/pkg                      examples/micro/srv/
cp /Users/hyx/work/gowork/src/hubx/micro_apps/apps/srv-order/config.yaml  examples/micro/srv/config.yaml
```

**不复制** `infra/` 与原 336 行 `main.go` —— 那正是本轮要消灭的东西。
复制后请 `ls examples/micro/srv/` 确认 `internal/`、`pkg/`、`config.yaml` 三项都在再往下走。

- [ ] **步骤 12.2：写 srv 的 config.yaml / main.go / setup.go**

config.yaml：`service.mode: srv`、`service.addr: 127.0.0.1:9013`、`registry.driver: consul`、`registry.addr: 127.0.0.1:8500`、`xmiddleware.sqlx` + `mqx`（kafka `127.0.0.1:9092`，**不**沿用原 `19093`）。

`setup.go`：装配 repo/outbox/service；**用 `a.RPCServer` 提供的 rpcx Option 构造 kitex server**（此处是 demo 里唯一允许 import kitex 的地方，因为 `kitex_gen` 依赖它）；`a.OnStop` 注册 outbox 轮询器关闭。

`main.go` 同样 ≤50 行，形态与 monolith 一致。

- [ ] **步骤 12.3：写 gateway 与 bff**

gateway：`mode: gateway`，`config.yaml` 含 `gateway.routes`（`/api` → bff）；Setup 内 `hertz.Any("/*p")` 反向代理，上游地址经 `a.Registry.Discovery("bff")`。
bff：`mode: bff`；`GET /ping` 自响应；`GET /order/:id` 经 `a.RPCClientOptions("srv")` 拿 rpcx 客户端 Option 构造 kitex 客户端调 `GetOrder`（**不**新增 srv HTTP 适配层）。

- [ ] **步骤 12.4：起底座跑通链路**

```bash
export HUBX_DB_PW=hubx123
docker compose -f /Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml up -d consul mysql kafka jaeger jaeger
# 依次起 srv / bff / gateway
curl -s localhost:8080/api/ping
curl -s localhost:8080/api/order/1001
```

预期：`gateway → bff → srv` 链路返回数据；jaeger UI（`localhost:16686`）可见一条贯穿三服务的 trace。

- [ ] **步骤 12.5：srv e2e 回归（E3，含 outbox→kafka 断言）**

`examples/micro/srv/e2e_test.go`：下单 RPC → 断言 outbox 记录 → 轮询器投递 → 直查 kafka topic（compose `KAFKA_CFG_AUTO_CREATE_TOPICS_ENABLE=true` 自动建 topic）确认消息落地；并断言 `wc -l examples/micro/srv/cmd/main.go` ≤ 50。

```bash
git add examples/micro/ && git commit -m "feat(examples/micro): gateway→bff→srv 调用链 + srv-order 迁移（E2/E3）"
```

---

## 任务 13：demo README（tasks 2.6）

**文件：** 创建 `examples/monolith/README.md`、`examples/micro/README.md`

每个 README 必须含四节：① 启动前置（底座 compose 命令 + 需 export 的 env）；② 启动命令；③ 依赖清单（consul/mysql/kafka/jaeger 端口）；④ 对应验收案例编号与预期结果（E1 / E2 / E3）。mesh 的 README 不在本轮。

- [ ] **步骤 13.1：写两个 README**
- [ ] **步骤 13.2：Commit**

```bash
git add examples/monolith/README.md examples/micro/README.md && git commit -m "docs(examples): monolith 与 micro 两 demo README（2.6）"
```

---

## 任务 14：单测套件收口（tasks 3.1–3.4，验收案例 A2/A3/B1/C1–C3/D1/D2）

- [ ] **步骤 14.1：configsrc 补齐断言（3.1）**

`internal/configsrc/*_test.go` 覆盖：三裁决规则、dataId 空纯本地、远端不可达即退、`${ENV}` DSN 展开。
运行：`go test ./internal/configsrc/ -v`

- [ ] **步骤 14.2：pipeline 顺序断言（3.2）**

启动序（telemetry 早于组件）+ 退出序（deregister 早于 flush、flush 最后、`trace.enabled=false` 时同样可正常退出）。
运行：`go test ./internal/pipeline/ -v`

- [ ] **步骤 14.3：component 双分支（3.3）**

strict 降级 / 失败即退；`trace.on` 不改变构造（D1）。
运行：`go test ./internal/component/ -v`

- [ ] **步骤 14.4：mode 四档 + 非法 mode（3.4）**

含「`mode: mesh` 必须被拒绝」的代码断言（C4 中可自动化部分）。
运行：`go test ./internal/mode/ -v`

- [ ] **步骤 14.5：Commit**

```bash
git add internal/ && git commit -m "test: 补齐 configsrc/pipeline/component/mode 单测套件（3.1-3.4）"
```

---

## 任务 15：集成验收 + 降级验收 + 依赖面核对（tasks 3.5/3.6/3.7/3.9/3.14）

- [ ] **步骤 15.1：monolith 集成（3.5，E1）**

`go test ./examples/monolith/ -v` —— 底座 mysql 就绪下启动 + HTTP 订单读写落库 + SIGTERM 正常退出。

- [ ] **步骤 15.2：micro 链路集成（3.6，E2）**

`examples/micro/scripts/e2e.sh` 或 `e2e_test.go`：起 srv/bff/gateway → `curl /api/ping` 与 `/api/order/1001` 断言链路 → 清理。

- [ ] **步骤 15.3：srv-order 回归（3.7，E3）**

下单链路全绿 + outbox 经 kafka 投递断言 + `main.go` ≤50 行。

- [ ] **步骤 15.4：降级验收（3.9，A3 降级路径）**

- `trace.enabled=false` 启动 monolith 与 micro 各一次，断言正常服务与退出；
- 停掉 consul 启动，断言 Warn 带病运行可服务；
- `strict.db=true` + 无 mysql，断言 `Bootstrap` 返回 `init db` error。

运行：`go test ./... -run Degrade -v`

- [ ] **步骤 15.5：依赖面核对（3.14）**

```bash
cd /Users/hyx/work/gowork/src/hubx/launchx
# 库源码（含 internal）必须零 cloudwego 直接 import
grep -rn '"github.com/cloudwego/' --include='*.go' . | grep -v '^./examples/' || echo "OK: 库源码零 cloudwego import"
# go.mod 中 kitex 只能是 indirect
grep -n 'cloudwego' go.mod
# 全仓不得直引以下三方客户端
grep -rn '"github.com/cloudwego/hertz"\|"github.com/redis/go-redis\|"github.com/segmentio/kafka-go\|"github.com/hashicorp/consul/api"\|"gorm.io/gorm"' --include='*.go' . || echo "OK: 无禁用三方直引"
```

预期：三条分别为 OK / 仅 `// indirect` / OK；`examples/` 中仅 `micro/srv` 与 `micro/bff` 的 kitex 构造处出现 kitex import。

- [ ] **步骤 15.6：全量回归 + Commit**

```bash
go build ./... && go vet ./... && go test ./...
git add -A && git commit -m "test: 集成与降级验收 + 依赖面核对（3.5/3.6/3.7/3.9/3.14）"
```

---

## 任务 16：出向注入工具 + 传播器断言（tasks 1.9 + 4.1，验收案例 T1/T7）

**文件：**
- 创建：`trace.go`（导出薄封装）、`trace_test.go`

**背景（实测）：** `webx/middleware/otel.go` **只有 Extract 没有 Inject**，全 hubx 无出向注入 helper。gateway 转发 HTTP 到 bff 时若不在出向 Header 注入 `traceparent`，下游 Extract 拿不到上游 context，链路断开。已验证写法：`micro_apps/apps/gateway/internal/proxy/proxy.go:134` 的 `otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(outReq.Header))`。

- [ ] **步骤 16.1：写失败测试**

```go
// trace_test.go
package launchx

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// restoreGlobal 还原全局 provider/propagator，避免用例间污染
func restoreGlobal(t *testing.T) {
	t.Helper()
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTracerProvider(prevTP); otel.SetTextMapPropagator(prevProp) })
}

// T1：telemetry 注册后传播器必须含 W3C 三项字段，且注入产出合法 traceparent
func TestPropagatorRegisteredAfterTelemetry(t *testing.T) {
	restoreGlobal(t)
	// 直接调用注册动作（与 pipeline.InitTelemetry 内部同一段）
	registerPropagator()
	fields := otel.GetTextMapPropagator().Fields()
	for _, want := range []string{"traceparent", "tracestate", "baggage"} {
		if !contains(fields, want) {
			t.Fatalf("propagator fields %v missing %q", fields, want)
		}
	}
	rec := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	ctx, span := otel.Tracer("test").Start(context.Background(), "op")
	defer span.End()
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if got := carrier["traceparent"]; !validTraceparent(got) {
		t.Fatalf("traceparent = %q, want 00-<32hex>-<16hex>-<2hex>", got)
	}
}

// T7：反向断言 —— 不注册传播器时提取不到上游 context
func TestDefaultPropagatorIsNoop(t *testing.T) {
	restoreGlobal(t)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator()) // OTel 默认：空 Composite
	carrier := propagation.MapCarrier{"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}
	ctx := otel.GetTextMapPropagator().Extract(context.Background(), carrier)
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		t.Fatal("empty composite propagator must not extract a valid span context (T7)")
	}
}
```

- [ ] **步骤 16.2：运行确认失败**
- [ ] **步骤 16.3：实现**

```go
// trace.go
package launchx

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// registerPropagator 注册 W3C 传播器。otelx 只注册 TracerProvider，不注册传播器；
// 而 OTel 默认的全局传播器是空 Composite（noop），不注册则跨服务链路断成孤儿 trace。
// 必须在 telemetry 步骤内、早于任何组件构造调用。
func registerPropagator() {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
}

// Inject 把当前 span context 注入 carrier，供出向 HTTP 请求续链。
// 只做注入，不含任何转发逻辑 —— webx 只提供入站 Extract，出向注入是 SDK 真空。
func Inject(ctx context.Context, carrier propagation.TextMapCarrier) {
	otel.GetTextMapPropagator().Inject(ctx, carrier)
}
```

- [ ] **步骤 16.4：运行确认通过**
- [ ] **步骤 16.5：Commit**

```bash
git add trace.go trace_test.go && git commit -m "feat(launchx): 出向注入工具 + 传播器注册红线断言（1.9/T1/T7）"
```

---

## 任务 17：Jaeger 端到端调用链验收（tasks 4.2–4.8，验收案例 T2–T6/T8/T9）

**前置：** `examples/scripts/bring-up-trace.sh check` 全绿（jaeger / mysql / kafka / elasticsearch 均已启动，见任务 0.3）。

**文件：**
- 创建：`examples/monolith/trace_e2e_test.go`、`examples/micro/trace_e2e_test.go`、`internal/traceutil/jaeger.go`（Jaeger API 检索客户端，仅测试用）

- [ ] **步骤 17.1：Jaeger 检索客户端**

`internal/traceutil/jaeger.go`：封装 `GET http://127.0.0.1:16686/api/traces?service=<name>&start=<us>&end=<us>`，返回 span 列表（含 `traceID`、`spanID`、`references`、`operationName`、`tags`）。提供 `WaitForTrace(service, minSpans, timeout)` 轮询重试——批量导出器有 5s 缓冲，查询需重试。

- [ ] **步骤 17.2：T5 单体调用树（4.2）**

请求 monolith 的落库接口 → 从 Jaeger 取该 trace → 断言：span 树深度 ≥ 3；数据层 span 的祖先链包含入口 span；所有 span 共享同一 traceID（无孤儿）。

- [ ] **步骤 17.3：T2 单表 CRUD + SQL 入链（4.3）**

对单表执行 Create/Read/Update/Delete → 断言 4 个 `db.*` span，每个 `db.query.text` 非空且含对应关键字（`INSERT`/`SELECT`/`UPDATE`/`DELETE`），`db.sql.table` 为表名。
`db.query.text` 由 `dbx/orm/gorm_tracing.go` 产出（`gorm_tracing_test.go:120` 已断言其非空），本步只做 Jaeger 端验证。

- [ ] **步骤 17.4：T3 多表操作入链（4.4）**

- `JOIN`（或 `Preload`）查询：`db.query.text` 含关联文本，同为业务 span 的子节点；
- 跨表事务：多条 SQL 为同 trace 的**兄弟 span**；事务回滚时相关 span 标 `error=true`。

- [ ] **步骤 17.5：T4 cache/mq/ES/mongodb 同 trace（4.5）**

同一请求内依次操作四类 → 断言 Jaeger 中该 trace 同时存在 cache span、mq span、`db.elasticsearch.*`、`db.mongodb.*`，且均为业务 span 的子节点。

- [ ] **步骤 17.6：T6 微服三段同 trace（4.6）**

`curl gateway /api/order/1001` → 从 Jaeger 取 trace → 断言 gateway / bff / srv 三服务的 span **共享同一 traceID**，且父子链为 gateway → bff → srv（**不是三个并列根**）；srv 侧数据层 span 亦在同一 trace。
**这是最容易假通过的断言**：不注册传播器时三服务各产一根 trace，每条都能点开、都有 span，看起来完全正常。必须断言 traceID 相等 + 父子关系，而非"各服务都能查到 trace"。

- [ ] **步骤 17.7：T9 尾部 span 不缺失 + T8 网格同构（4.7/4.8）**

- T9：触发请求后经 traceutil 检索，断言 span 数量符合预期且**尾部节点不缺失**——证明退出链的 `tracing.shutdown(15s)` 冲刷生效（批量导出器缓冲未冲刷会静默丢最后一个 span）。
- T8：代码断言——全仓无网格专属 trace 分支；`mode.Values` 不含 `mesh`。

- [ ] **步骤 17.8：Commit**

```bash
git add examples/monolith/trace_e2e_test.go examples/micro/trace_e2e_test.go internal/traceutil/ && git commit -m "test: Jaeger 端到端调用链验收 T2-T6/T8/T9（4.2-4.8）"
```

---

## 任务 18：网格形态端到端链路验收（tasks 4.9–4.11，验收案例 T10–T12，**2026-10-05 新增**）

**前置（硬）**：minikube + kubectl + istioctl 就绪；`minikube start --driver=docker`；`istioctl install --set profile=demo`；Istio 侧追踪 provider 指向本机 Jaeger（`host.docker.internal:4317` 或集群内 Jaeger），采样 100%。
**前置（待核）**：Istio provider 类型与传播头对齐（见本文件「2026-10-05 追加」段）。

- [ ] **步骤 18.1：环境前置检查脚本**
  在 `examples/mesh/scripts/bring-up.sh` 的 `check` 子命令中显式检查 `minikube` / `kubectl` / `istioctl` 是否存在、`minikube status` 是否 Running、istio-system 是否就绪、宿主中间件端口（consul 8500 / mysql 3306 / kafka 9092 / jaeger 16686）是否可达。缺失时**打印缺失项并以非零码退出**，不得静默跳过。
- [ ] **步骤 18.2：传播头对齐实测（决定 provider 选型）**
  `istioctl proxy-config bootstrap <pod> -n launchx-mesh -o json` 查 tracing 配置；再对 demo 发一次请求，从 sidecar 侧抓实际写出的头名（`traceparent` vs `x-b3-traceid`），确认与 launchx 的 Composite{TraceContext,Baggage} 一致。结论写入 `examples/mesh/docs/verify.md` 的「环境与配置」段。
- [ ] **步骤 18.3：T10 — app span 与 sidecar span 同 trace**
  经 Ingress Gateway 触发 `gateway → bff → srv`。用 `http://127.0.0.1:16686/api/traces?service=gateway` 与 `?service=gateway.<ns>` 双向检索，断言：同一 trace-id；应用 span 父子链仍为 gateway → bff → srv；srv 数据层 span 同 trace。
- [ ] **步骤 18.4：T11 — 网格下「半断链」反向断言**
  临时以不注册传播器的构建（或置 `trace.enabled=true` 但跳过 `InitTelemetry` 的传播器注册，仅用于实验，不入主干）跑一次请求，断言 Jaeger 出现「1 条 sidecar 完整链 + 应用侧 ≥3 根孤儿 trace」。记录为反例证据。
- [ ] **步骤 18.5：T12 — 证据落盘**
  `examples/mesh/docs/verify.md` 记录：trace-id、span 总数、应用 span 数 / sidecar span 数、sidecar 服务名、Jaeger API 原始响应片段、`hubx.trace` OFF/ON 对比、minikube/kubectl/istioctl/Istio 版本。
- [ ] **步骤 18.6：Commit**
  ```bash
  git add examples/mesh/ && git commit -m "test(mesh): 网格端到端链路验收 T10-T12 与证据落盘"
  ```

---

## 执行进度（2026-10-05 更新，真实提交为准）

| 任务 | 状态 | 证据 |
|---|---|---|
| 1 module 骨架 + 23 条 replace | ✅ | `0c81939` |
| 2 复核 coordx/consul | ✅ | 复验通过，零改动 |
| 3 新建 hubx/rpcx（含 ClientSpanMiddleware 断链修复） | ✅ | `a047afa` / `25e3e9b`（覆盖 94.6%） |
| 4 configsrc 注册表 + local + `${ENV}` | ✅ | `990d1a1` / `91565af` |
| 5 nacos 三裁决（B1） | ✅ | `cdbab21` |
| 6 telemetry + 事件记录器 + 固定骨架（A2） | ✅ | `fc02f6c` |
| 7 Bootstrap 接通骨架 + 非法 mode 即退（A1/A2/C4） | ✅ | `e7bf5b1` |
| 8 六类组件 + strict/trace 双裁决（D1/D2/T2–T4） | ✅ | `bdbbdbf`（26 条 replace） |
| 7+8 收口：组件挂载 + 反序退出链（A3 部分） | ✅ | `37e5065` |
| 9 mode 四档装配（C1–C3） | ⏳ 待做 | — |
| 10 反序退出链收口（A3 全量：server 停止 + deregister） | ⏳ 待做（现已实现 setup.stop→server.stop→component.stop→deregister→tracing.shutdown→logger.close 骨架，待接 server 与注册实例跟踪） | `37e5065` |
| 11 monolith demo（E1） | ⏳ 待做 | — |
| 12 micro 三服务（E2/E3） | ⏳ 待做 | — |
| 13 demo README（2.6） | ⏳ 待做 | — |
| 14–17 单测收口 / 集成与降级验收 / Inject+传播器 / Jaeger e2e（T1–T9） | ⏳ 待做 | — |
| 18 网格链路验收（T10–T12） | ⏳ 待做（标准已定稿，缺 minikube/Istio 底座） | 见 `specs/trace-verification` |

**本轮（2026-10-05）额外完成的事实修正与新增：**
1. **网格链路验收标准定稿**：`specs/trace-verification/spec.md` 新增「网格形态端到端链路（minikube + sidecar 合流）」Requirement 与验收案例 **T10 / T11 / T12**；`tasks.md` §4 扩为 T1–T12 并新增 4.9–4.11；本文件新增任务 18。核心断言：T10（app span 与 sidecar span 同 trace，且**应用 span 父子链不变**）、T11（网格特有的「半断链」反向断言：只开 sidecar 追踪时呈 1 条 sidecar 链 + 应用侧孤儿 trace）、T12（证据落盘含环境版本）。
2. **minikube 实测未安装**（minikube/kubectl/helm/istioctl/linkerd 全部 `command not found`），故 T10–T12 真实跑分仍后置；环境约定与前置动作已写入本文件「环境事实」段。
3. **Elasticsearch 不在 `hubx/dockers` compose 内**（当前 9200 由独立容器提供）——重建环境时须自行补 ES，否则 `search` 组件无法验收。此坑已写入环境事实段。
4. **实测踩坑（已修）**：`mqx/kafka.NewProducer` 会调用 `cfg.Logger.InfoContext`，若注入的 logx 句柄是**嵌入了 nil 接口的测试替身**会 panic；测试必须注入真实日志器（`internal/component/component_test.go`）。同时 `github.com/gospacex/mongodbx` 的**包名是 `docx`**，import 必须显式 alias。

---

## 自检结论

**规格覆盖度：** 6 份 spec 的 Requirement 逐一对应 —— bootstrap-pipeline（单入口/固定顺序/反序退出/Setup 契约）→ 任务 1/7/6/10；config-loading（可插拔源/local+ENV/nacos 三裁决）→ 任务 4/5；mode-assembly（四档/mesh 不入档/未知 mode 拒绝/RPC 经 rpcx）→ 任务 9/3；component-registry（注册表/trace.on 门控/strict 裁决/registry 带病运行）→ 任务 8；example-demos（E1/E2/E3）→ 任务 11/12/13。O1–O3 与 E4/E5 属本轮后置，按要求未排期。

**占位符扫描：** 无「待定/TODO/后续实现/类似任务 N」。`addrToNetAddr`、`newCoordxRegistry`、`parseServerAddrs` 三处均标注了移植来源与大致行数，实现时必须照来源写全，不得留空函数。

**类型一致性：** `pipeline.Events`（Record/Order/IndexOf）、`pipeline.Step`、`pipeline.Env`、`component.Env`（含 `TraceOn`/`Strict`/`DB`/`Cache`/`MQ`）、`mode.Out`（`Webx`/`RPCServer`/`Registry`）在任务 6/7/8/9 中同名同签名；`launchx.App` 的 `Events()`/`OnSetup`/`OnStop`/`Run`/`Close` 在任务 1 定义后未在后续任务改名。

---

## 执行交接

计划已完成并保存到 `docs/superpowers/plans/2026-10-04-launchx-kernel-monolith-micro.md`。两种执行方式：

**1. 子代理驱动（推荐）** —— 每个任务调度一个新的子代理，任务间进行审查，快速迭代
**2. 内联执行** —— 在当前会话中批量执行并设有检查点

选哪种方式？
