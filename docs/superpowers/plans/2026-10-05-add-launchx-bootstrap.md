# launchx Bootstrap 实现计划（本轮：内核 + monolith + micro）

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 实现 launchx 引导库（`Bootstrap(path)+OnSetup+Run` 一行启动、红线顺序代码化）+ `examples/monolith` 与 `examples/micro`（gateway/bff/srv，srv 由 srv-order 迁移）两个自包含 demo，并跑通本轮全部单测与集成验收（tasks.md 3.1–3.7、3.9）。

**架构：** 固定七步骨架流水线（configsrc → telemetry → logx → mode.Assemble → component 挂载 → Setup → Run 收尾反序退出）+ 注册表（配置源/组件可插拔）；四档 mode（monolith/bff/srv/gateway）由 yaml `service.mode` 声明；默认降级语义与 micro_apps 现状一致（strict 缺省 → 组件 nil + Warn 带病运行）。

**技术栈：** Go 1.27，hubx `*x` SDK（configx/logx/otelx/dbx/cachex/mqx/coordx/webx/traceflag，均经 go.mod `replace` 指向本地兄弟目录），hertz（webx 内嵌）、kitex+rpcx（srv 档）、consul/kafka/mysql/jaeger 经 `hubx/dockers` 共享 compose。

**Traceability (sddflow):**
- plan-ready: `openspec/changes/add-launchx-bootstrap/plan-ready.md`
- tasks: `openspec/changes/add-launchx-bootstrap/tasks.md`
- plan: `docs/superpowers/plans/2026-10-05-add-launchx-bootstrap.md`

**本轮范围（design.md 2026-10-04「本轮范围收敛」决策，以 tasks.md 条目号为准）：**
- 实现：1.1–1.7；Demo：2.1–2.4、2.6（仅 monolith 与 micro 两个 README）；验证：3.1–3.7、3.9
- **后置（本计划仅列占位任务，不实现）**：1.8（debug 伪组件）、2.5（examples/mesh）、3.8（E4/E5）、3.11–3.13（O1–O3）、3.10 中涉及后置案例的部分

**API 事实基线（2026-10-05 采集，写代码前必须遵守）：**
1. `configx` 不做 `${ENV}` 展开（字面量返回）→ DSN 占位展开由 launchx `internal/configsrc.ExpandEnv`（`os.ExpandEnv`）承担；cachex `InstanceConfig.Password` 的 `${VAR}` 由 cachex 包内私有展开（未解析变量会报错）
2. Composite 传播器（TraceContext+Baggage）在 `logx/zap/exporter.go` 与 `micro_apps/infra/telemetry/provider.go`，otelx 无传播器代码 → launchx `internal/telemetry` 自装 `otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(...))`
3. `Target(agentAddr, serviceID)` 是 `micro_apps/infra/consul/registry.go:148` 的应用层助手，coordx 无此符号 → registry 组件的 Target/Resolve 逻辑移植自该文件进 `internal/component/consul.go`，coordx 仅承担 Register/Deregister/Discovery 原语
4. otelx noop 开关字段 `Tracer.EnableTrace`（主键）/`Tracer.Enabled`（别名），经 `traceflag.Apply("otelx", ...)` 收敛；`Enabled=false` 时 `New` 返回 noop provider 且不设全局
5. 驱动注册均为 blank import：`_ "github.com/gospacex/logx/zap"`、`_ "github.com/gospacex/cachex/redis"`、`_ "github.com/gospacex/mqx/kafka"`、`_ "github.com/gospacex/coordx/consul"`（coordx/consul 存在重复 init()，幂等无害，不去重）
6. 底座事实（写死在 config.yaml 与 e2e 脚本）：consul `127.0.0.1:8500`；mysql `127.0.0.1:3306`（`order_rw/hubx123`，库 `order_db`）；kafka `127.0.0.1:9092`（KRaft，auto-create topic，PLAINTEXT；**不**用 srv-order 旧配置的 19093）；jaeger OTLP `127.0.0.1:4318` / UI `16686`；redis `127.0.0.1:6379`（无密码）

---
## 内容基线（2026-10-05 合并）

> 本节由 2026-10-05 的文档合并动作补写：把两份旧计划收敛为本文件（唯一驱动），并记录已落地实现、最小偏差与必须沿用的环境事实。执行后续 Task 前先读本节，避免重复探测环境或推翻已落地代码。

### 本文件的地位

- **唯一执行驱动**：本文件（`docs/superpowers/plans/2026-10-05-add-launchx-bootstrap.md`）是 `add-launchx-bootstrap` 的唯一驱动文件 —— 原生 21 个 Task（本次合并追加 Task 22 / 23 / 23b / 24 / 25）、每个 Task 带 `trace` / `sync` 锚点与 `- [ ]` 步骤复选框，sddflow build 机制直接消费本文件。
- **已归档 `archive/2026-10-04-add-launchx-bootstrap.md`**：初版全量计划，只有 5 个 Task、范围未收敛且内容过时 → 不再执行，仅留作回溯。
- **已归档 `archive/2026-10-04-launchx-kernel-monolith-micro.md`**：本轮实际执行并落地代码的那份（18 个「## 任务 N：」章节，含最新决策）→ 其有效内容已并入本节与新增 Task 22 / 23 / 23b / 24 / 25；因全篇无 `- [ ]` 复选框、无 trace/sync 锚点，无法驱动 sddflow，故归档不删除。

### 已完成清单（以真实提交为准）

- **Task 1**：launchx module + 单入口 API（`Bootstrap(path)` / `App` 句柄 / `OnSetup` / `Run` / 幂等 `Close`，非法 mode 拒绝启动）→ `e7bf5b1`（初始化骨架另见 `0c81939`）。
- **Task 2**：configsrc 注册表 + `local` 加载器 + `${ENV}` 展开 → `990d1a1`、`91565af`（缺失配置路径硬失败补齐）。
- **Task 3**：nacos 加载器三裁决（本地兜底 + `hubx.<service>` dataId key 级裁决 + `HUBX_NACOS_SERVER` env 覆盖，dataId 空纯本地、远端不可达即退）→ `cdbab21`。
- **Task 4**：组件注册表与组件构建器、strict/trace 双裁决、presence-based 装配 → `bdbbdbf`（**实际落地六类，偏差见下**）。
- **Task 6**：pipeline 固定启动骨架 + telemetry 全局注册（含显式传播器 Composite{TraceContext, Baggage}）+ 事件记录器 → `fc02f6c`。
- **组件挂载 + 反序退出链骨架**（对应本文件 Task 7）→ `37e5065`；**仅骨架**：缺 server 停止与注册实例跟踪，故 Task 7 在本文件中仍保持未勾选。
- **跨仓前置**：0.1 coordx/consul 复核 → 零改动通过（`../coordx`，复验非修复）；0.2 新建 `hubx/rpcx` 模块 → `a047afa` / `25e3e9b`。

### 最小偏差记录（必须写清）

- **偏差项**：本文件 Task 4 标题写的是「component 注册表与四个内置组件」（db / cache / mq / registry），**实际落地的是六类** —— db / cache / mq / registry / **search（Elasticsearch，经 `searchx/elasticsearch`）** / **mongodb（经 `mongodbx`）**。
- **原因**：用户验收标准明确要求「DB、缓存、队列、ES、MongoDB 都要入链」（tasks.md 1.4 与验收案例 T4 同此口径），且 2026-10-05 底座已提供 ES 与 mongo 副本集，具备验收条件。
- **性质**：**超集实现** —— 原四类组件的构造、`trace.on.<name>` 只控入链不控构造、`strict.<name>` 裁决、registry 失败带病运行、presence-based 装配等行为一条未削减，仅新增两类。
- **证据**：`bdbbdbf`；`go.mod` 的 `replace` 由 23 条增至 26 条（新增 `searchx`、`searchx/elasticsearch`、`mongodbx`）。
- **附带事实**：`github.com/gospacex/mongodbx` 的**包名是 `docx`**，import 必须显式 alias。

### 环境事实（2026-10-05 用户给定，写死不再探测）

1. **Go 工具链**：`export PATH=/Users/hyx/sdk/go1.24.3/bin:$PATH`，`GOTOOLCHAIN=auto`（自动拉取 go1.27.1；webx 声明 `go 1.27.1`、dbx `go 1.26.2`、mqx `go 1.27`），不需要手工装新版。
2. **中间件底座**：所有中间件一律由本机 `/Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml` 统一供给 —— consul `8500`、mysql `3306`、kafka `9092`（KRaft）、jaeger `16686`/`4317`/`4318`、redis `6379`、mongo 副本集 `27018`-`27020` 等；docker CLI 绝对路径 `/Applications/Docker.app/Contents/Resources/bin/docker`（不在 PATH 里）。**Elasticsearch 不在该 compose 内**，当前 `9200` 由独立容器提供（实测 `green`）；若整套环境重建必须自行补 ES，否则 `search` 组件无法验收。
3. **服务网格**：网格由本机 minikube + Istio 提供。**2026-10-05 实测更正**：minikube `v1.36.0`、istioctl `1.30.1` **已安装**于 `/usr/local/bin`（早先「均未安装」的结论是 PATH 误判——WorkBuddy shell 默认 PATH 不含 `/usr/local/bin`，`which` 全空）；kubectl 未单独安装，走 `minikube kubectl --`。集群**尚未启动**（`minikube status` 报 `No such container: minikube`），故网格验收 T10–T12 的真实跑分仍需先 `minikube start --driver=docker --cpus=2 --memory=4096`（宿主 docker 总内存仅 8GB）与 `istioctl install`，详见 Task 25。集群内访问宿主中间件用 **`host.minikube.internal`**（`host.docker.internal` 在 minikube 的 k8s 内不保证解析）。
4. **红线 —— otelx 只注册一半全局**：`otelx.New` 只注册 TracerProvider（`otelx/interface.go:207`，仅 `Tracer.Enabled` 为真时；关闭态返回 noop 且不碰全局），**从不注册传播器**。OTel 全局默认传播器是**空 Composite**（注入与提取皆空操作）→ `traceparent` 既不写出也不读取 → gateway→bff→srv 断成三段孤儿 trace，且**伪装成成功**（三服务各一根 trace、每条都能点开有 span），极难发现。故 launchx **必须**显式 `otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))`。同理 `webx.Config.FillDefaults` 的 Tracer 默认取 `otel.GetTracerProvider().Tracer("webx")` → **telemetry 必须先于 mode 装配**，否则 webx 拿到 noop tracer。

### 任务顺序提醒

- 本文件里 Task 5（mode 装配）排在 Task 6（pipeline/telemetry）之前，但上述红线要求 **telemetry 先于 mode 装配**。
- 本轮实际执行顺序已是**先 6 后 5**（`fc02f6c` 先于 `e7bf5b1`），代码已按此落地。
- **实现 Task 5 时不得回退顺序**，一律以已落地代码为准。

---

## 文件结构（锁定分解）

```
launchx/
  go.mod                      # module github.com/gospacex/launchx + replace 兄弟目录
  launchx.go                  # Bootstrap(path) (*App, error)；七步骨架调用
  app.go                      # App 结构体（句柄字段）+ Run() + Close()
  setup.go                    # OnSetup / OnStop 回调
  launchx_test.go             # A1 冒烟
  internal/configsrc/
    registry.go               # Register(name, Loader) 注册表
    local.go                  # local 加载器（configx 读 yaml）
    expand.go                 # ExpandEnv(s) string（os.ExpandEnv 包装，DSN 占位）
    nacos.go                  # nacos 加载器（本地兜底 + hubx.<svc> dataId key 级裁决 + env 覆盖）
  internal/component/
    registry.go               # Register(name, Builder) 注册表
    db.go cache.go mq.go      # 内置 db（dbx.OpenFromConfigx）/ cache（cachex.Init）/ mq（mqx.NewProducer）
    consul.go                 # registry 组件：coordx Register/Deregister/Discovery + Target/Resolve 移植
    strict.go                 # strict.<name> 裁决（降级 Warn / 失败即退）
  internal/mode/
    mode.go webx.go srv.go    # 四档分支 + 非法 mode 报错（含合法取值列表）
  internal/telemetry/provider.go   # otelx 封装（全局 TracerProvider + Composite 传播器 + 15s Shutdown）
  internal/pipeline/
    pipeline.go events.go     # 七步骨架 + 事件记录器（顺序可断言）
    shutdown.go               # 反序退出编排（显式序列，不依赖 defer）
  examples/
    monolith/{config.yaml,cmd/main.go,app/{setup.go,routes.go},internal/{handler,logic,service,repository,model}/*,e2e_test.go,README.md}
    micro/{gateway,bff,srv}/...、e2e 脚本、README.md
```

---

### Task 1: 创建 launchx module 与单入口 API

> **trace:** plan-ready.md → `### Task 1: 创建 launchx module 与单入口 API` | tasks.md → `- [ ] 1.1 创建 launchx module（\`go.mod\`）与单入口 API：\`Bootstrap(path) (*App, error)\`、\`App\` 句柄字段（Config/Logger/Tracer/DB/Cache/MQ/Registry/Server）、\`OnSetup(func(*App) error)\`、\`Run() error\`、幂等 \`Close()\`（对应验收案例 A1）`
> **sync:** tasks.md → `- [ ] 1.1 创建 launchx module（\`go.mod\`）与单入口 API：\`Bootstrap(path) (*App, error)\`、\`App\` 句柄字段（Config/Logger/Tracer/DB/Cache/MQ/Registry/Server）、\`OnSetup(func(*App) error)\`、\`Run() error\`、幂等 \`Close()\`（对应验收案例 A1）` | plan-ready.md → `### Task 1: 创建 launchx module 与单入口 API`

**文件：**
- 创建：`launchx/go.mod`、`launchx/launchx.go`、`launchx/app.go`、`launchx/setup.go`、`launchx/launchx_test.go`

- [x] **步骤 1.1.0：git init（plan-ready 2026-10-04 本地环境裁决）**

运行：
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx && git init && git add .sddflow .claude docs openspec && git commit -m "chore: 初始提交（openspec 制品 + sddflow 状态）"
```
预期：`git log --oneline` 1 条提交。

- [x] **步骤 1.1.1：写 go.mod（replace 指本地兄弟目录）**

创建 `launchx/go.mod`：
```go
module github.com/gospacex/launchx

go 1.27

require (
	github.com/gospacex/cachex v0.0.0
	github.com/gospacex/configx v0.0.0
	github.com/gospacex/coordx v0.0.0
	github.com/gospacex/dbx v0.0.0
	github.com/gospacex/logx v0.0.0
	github.com/gospacex/mqx v0.0.0
	github.com/gospacex/otelx v0.0.0
	github.com/gospacex/traceflag v0.0.0
	github.com/gospacex/webx v0.0.0
)

replace (
	github.com/gospacex/cachex => ../cachex
	github.com/gospacex/configx => ../configx
	github.com/gospacex/coordx => ../coordx
	github.com/gospacex/dbx => ../dbx
	github.com/gospacex/logx => ../logx
	github.com/gospacex/mqx => ../mqx
	github.com/gospacex/otelx => ../otelx
	github.com/gospacex/traceflag => ../traceflag
	github.com/gospacex/webx => ../webx
)
```

- [x] **步骤 1.1.2：写 App 句柄与单入口 API**

创建 `launchx/app.go`：
```go
package launchx

import (
	"context"
	"sync"

	"github.com/cloudwego/kitex/pkg/rpcx"
	"github.com/gospacex/cachex"
	"github.com/gospacex/coordx"
	"github.com/gospacex/dbx/dbsql"
	"github.com/gospacex/logx"
	"github.com/gospacex/mqx"
	"github.com/gospacex/webx"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/gorm"
)

// App 是 Bootstrap 产出的句柄；nil 字段 = 该组件降级/未装配（调用方判 nil 后 fail-open）。
type App struct {
	Config   *Config               // 已裁决生效的配置（configsrc 产物）
	Logger   logx.Logger
	Tracer   trace.Tracer
	DB       *gorm.DB              // 降级态 nil（strict.db 未置真）
	Cache    cachex.Cache          // 降级态 nil（cachex.Instance 返回 nil 时保持 nil）
	MQ       mqx.Producer          // 降级态 nil（仅 producer；consumer 场景走 GetSDK 自取）
	Registry *Registry             // consul 封装；降级态 *Registry 非 nil 但 Registered()==false
	Server   *webx.App             // srv 档为 nil；srv 档句柄在 App.RPCServer 字段
	RPCServer interface{ Run() error; Stop() error } // rpcx server（srv 档），类型留宽避免 kitex 泄漏

	setupMu sync.Mutex
	setup   []func(*App) error
	stop    []func()
	closed  bool
}

// Config 为 configsrc 裁决后的统一配置（configx.Config + 扩展的 service.mode 等段）。
type Config struct {
	configx configx.Config // 内嵌原始 configx.Config 供 Unmarshal 各段
}

func (a *App) Registered() bool { return a.Registry != nil && a.Registry.Registered() }

// 幂等 Close：重复调用直接返回 nil。
func (a *App) Close() error {
	a.setupMu.Lock()
	if a.closed {
		a.setupMu.Unlock()
		return nil
	}
	a.closed = true
	a.setupMu.Unlock()
	return shutdown(a) // Task 7 填充
}

// Run：等待 SIGINT/SIGTERM → 反序退出编排（Task 7 填充）。
func (a *App) Run() error { return shutdown(a) }
```
> 注：`rpcx` import 仅为 RPCServer 接口注释占位时删除；若无需 kitex 符号则删除该 import（Task 11 确认 srv 句柄实际类型后修正，允许在 Task 11 前微调 app.go import）。

创建 `launchx/launchx.go`：
```go
// Package launchx 固化 hubx 装配流水线：configsrc → telemetry → logx → mode → component → Setup → Run。
package launchx

import (
	"github.com/gospacex/launchx/internal/pipeline"
)

// Bootstrap 读取 path 指向的 yaml，完成七步骨架（telemetry 最先、logx.SetGlobal、mode 装配、
// 组件挂载、回调 Setup）。地基失败即返回 error；组件失败按 strict.<name> 裁决。
func Bootstrap(path string) (*App, error) { return pipeline.Run(path) } // Task 6 填充
```

创建 `launchx/setup.go`：
```go
package launchx

// OnSetup 注册业务装配回调（显式接口，不做 init() 自动发现）；Run 收尾前按注册顺序执行。
func (a *App) OnSetup(fn func(*App) error) {
	a.setupMu.Lock()
	defer a.setupMu.Unlock()
	a.setup = append(a.setup, fn)
}

// OnStop 注册退出钩子；退出链按注册逆序执行（先注册后注销）。
func (a *App) OnStop(fn func()) {
	a.setupMu.Lock()
	defer a.setupMu.Unlock()
	a.stop = append(a.stop, fn)
}
```

- [x] **步骤 1.1.3：写 A1 冒烟测试（RED）**

创建 `launchx/launchx_test.go`：
```go
package launchx

import "testing"

// 验收案例 A1：Bootstrap 返回 App、OnSetup 可注册、Close 幂等。
// 本步骤 pipeline.Run 尚未实现 → 先断言 API 形态可编译（TestBootstrap 允许 FAIL 到 Task 6）。
func TestBootstrapAPI(t *testing.T) {
	a := &App{}
	a.OnSetup(func(*App) error { return nil })
	if err := a.Close(); err != nil {
		t.Fatalf("Close first: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close must be idempotent, got: %v", err)
	}
}
```

- [x] **步骤 1.1.4：编译通过（骨架占位版）**

运行：
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx && go mod tidy && go build ./...
```
预期：`go build ./...` 通过（`pipeline.Run` 尚未存在 → 本步先以占位实现解锁编译：在 `internal/pipeline/pipeline.go` 写 `func Run(path string) (*launchx.App, error) { return nil, errors.New("pipeline not implemented") }`，import 循环用 `internal/pipeline` 定义 `Run(path) (*pipeline.Result, error)` 返回字段结构与 App 等价，Task 6 替换。若 import 循环不可解，将 `Bootstrap` 直接改在 `internal/pipeline` 实现、`launchx.go` 转调，保持 `launchx.Bootstrap` 对外签名不变）。

- [x] **步骤 1.1.5：Commit**

```bash
git add go.mod go.sum launchx.go app.go setup.go launchx_test.go internal/
git commit -m "feat: launchx module 骨架 + Bootstrap 单入口 API（A1）"
```

- [x] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 1.1 同步） <!-- 已实现: e7bf5b1 -->

---

### Task 2: configsrc 注册表与 local 加载器

> **trace:** plan-ready.md → `### Task 2: configsrc 注册表与 local 加载器` | tasks.md → `- [ ] 1.2 实现 \`internal/configsrc\` 注册表与 \`local\` 加载器：configx 读本地 yaml + \`\${ENV}\` 展开（DSN 占位）`
> **sync:** tasks.md → `- [ ] 1.2 实现 \`internal/configsrc\` 注册表与 \`local\` 加载器：configx 读本地 yaml + \`\${ENV}\` 展开（DSN 占位）` | plan-ready.md → `### Task 2: configsrc 注册表与 local 加载器`

**文件：**
- 创建：`internal/configsrc/registry.go`、`internal/configsrc/local.go`、`internal/configsrc/expand.go`、`internal/configsrc/local_test.go`

- [x] **步骤 2.1：写失败测试（local + ExpandEnv）**

创建 `internal/configsrc/local_test.go`：
```go
package configsrc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCfg(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	p := filepath.Join(d, "config.yaml")
	os.WriteFile(p, []byte("service:\n  name: demo\nsqlx:\n  dsn: \"order_rw:${HUBX_DB_PW}@tcp(127.0.0.1:3306)/order_db\"\n"), 0o644)
	return p
}

func TestLocalLoader(t *testing.T) {
	cfg, err := LoadLocal(writeCfg(t))
	if err != nil {
		t.Fatalf("LoadLocal: %v", err)
	}
	if got := cfg.GetString("service.name"); got != "demo" {
		t.Fatalf("service.name = %q", got)
	}
}

func TestExpandEnv(t *testing.T) {
	os.Setenv("HUBX_DB_PW", "hubx123")
	defer os.Unsetenv("HUBX_DB_PW")
	got := ExpandEnv("order_rw:${HUBX_DB_PW}@tcp(127.0.0.1:3306)/order_db")
	if !strings.Contains(got, "order_rw:hubx123@") {
		t.Fatalf("ExpandEnv = %q", got)
	}
	// 未解析变量保留字面量（与 configx 语义一致，不做错误）
	got = ExpandEnv("${HUBX_NOT_SET_X}")
	if got != "${HUBX_NOT_SET_X}" {
		t.Fatalf("unresolved = %q", got)
	}
}
```

- [x] **步骤 2.2：运行确认失败**

运行：`cd /Users/hyx/work/gowork/src/hubx/launchx && go test ./internal/configsrc/`
预期：FAIL（`undefined: LoadLocal / ExpandEnv`）。

- [x] **步骤 2.3：实现注册表 + local + ExpandEnv**

`internal/configsrc/expand.go`：
```go
package configsrc

import "os"

// ExpandEnv 展开字符串中 ${VAR} 占位（configx 不提供该能力，README 明言字面量返回）。
// 未解析变量保留原字面量（与 os.ExpandEnv 语义一致，不报错）。
func ExpandEnv(s string) string { return os.ExpandEnv(s) }
```

`internal/configsrc/registry.go`：
```go
package configsrc

import (
	"fmt"

	"github.com/gospacex/configx"
)

// Loader 产出一个已裁决生效的 configx.Config（local=本地 yaml；nacos=本地兜底+远端 key 级覆盖）。
type Loader func(path string) (configx.Config, error)

var (
	mu       sync.RWMutex
	registry = map[string]Loader{"local": LoadLocal}
)

// Register 注册/覆盖配置源加载器（可插拔；etcd 等后续源走此入口）。
func Register(name string, l Loader) {
	mu.Lock()
	defer mu.Unlock()
	registry[name] = l
}

// List 返回已注册源名（测试与排障用）。
func List() []string { ... }
```
（`import "sync"` 补齐；`List` 遍历 map 排序返回）

`internal/configsrc/local.go`：
```go
package configsrc

import (
	"fmt"

	"github.com/gospacex/configx"
)

// LoadLocal 读本地 yaml（configx.New 传 ConfigFile；ConfigType 缺省 yaml）。
func LoadLocal(path string) (configx.Config, error) {
	if path == "" {
		return nil, fmt.Errorf("configsrc: local 加载器要求非空 path（收到 %q）", path)
	}
	cfg, err := configx.New(&configx.ConfigOptions{ConfigFile: path, ConfigType: "yaml"})
	if err != nil {
		return nil, fmt.Errorf("configsrc: 读本地配置 %s: %w", path, err)
	}
	return cfg, nil
}
```

- [x] **步骤 2.4：运行确认通过 + Commit**

运行：`go test ./internal/configsrc/ -run 'TestLocalLoader|TestExpandEnv' -v`
预期：PASS。
```bash
git add internal/configsrc && git commit -m "feat: configsrc 注册表 + local 加载器 + ExpandEnv（1.2）"
```

- [x] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 1.2 同步） <!-- 已实现: 990d1a1 / 91565af -->

---

### Task 3: configsrc nacos 加载器（三裁决）

> **trace:** plan-ready.md → `### Task 3: configsrc nacos 加载器（三裁决）` | tasks.md → `- [ ] 1.3 实现 \`configsrc\` 的 \`nacos\` 加载器：本地兜底 + \`hubx.<service>\` dataId key 级裁决 + \`HUBX_NACOS_SERVER\` env 覆盖，dataId 空纯本地；地基失败即退（对应验收案例 B1）`
> **sync:** tasks.md → `- [ ] 1.3 实现 \`configsrc\` 的 \`nacos\` 加载器：本地兜底 + \`hubx.<service>\` dataId key 级裁决 + \`HUBX_NACOS_SERVER\` env 覆盖，dataId 空纯本地；地基失败即退（对应验收案例 B1）` | plan-ready.md → `### Task 3: configsrc nacos 加载器（三裁决）`

**文件：**
- 创建：`internal/configsrc/nacos.go`、`internal/configsrc/nacos_test.go`

- [x] **步骤 3.1：读参考实现，锁定语义**

运行：
```bash
sed -n '1,120p' /Users/hyx/work/gowork/src/hubx/micro_apps/infra/nacos/*.go
```
摘录：`LoadUnified`（该仓实际 API，历史文档 `LoadMerged` 即指此）的三裁决顺序——① 本地 yaml 兜底值；② nacos `hubx.<service>` dataId（group HUBX）按 key 覆盖（远端非空键优先）；③ `HUBX_NACOS_SERVER` env 覆盖 server 地址（地址级最高优先）。dataId 为空 → 纯本地直接返回兜底。

- [x] **步骤 3.2：写失败测试（B1：key 级裁决 + 纯本地 + 远端不可达即退）**

创建 `internal/configsrc/nacos_test.go`：
```go
package configsrc

import (
	"context"
	"errors"
	"testing"
)

// fakeNacosClient 走注入点替换真实 nacos HTTP 调用（nacos.go 内部只调 client.Fetch）。
type nacosClient struct {
	fetch func(ctx context.Context, dataID, group string) (map[string]string, error)
}

func TestNacosKeyLevelArbitration(t *testing.T) {
	// 本地兜底 a=1 b=2；远端 dataId 只覆盖 a=99 → 结果 a=99 b=2
	// 远端空 dataId → 纯本地 a=1 b=2
	// fetch 返回 error（不可达）→ LoadNacos 返回 error（地基即退，不静默降级）
}

func TestNacosUnreachableFailsFast(t *testing.T) {
	_, err := LoadNacos(writeCfg(t), "hubx.demo", &nacosClient{fetch: func(context.Context, string, string) (map[string]string, error) {
		return nil, errors.New("connection refused")
	}})
	if err == nil {
		t.Fatal("远端不可达必须返回 error（地基失败即退）")
	}
}
```
（测试体内补全第一个用例：构造本地 yaml + `&nacosClient{fetch: 返回 map{"a":"99"}}`，断言裁决结果；完整断言代码随实现一起落盘，不留占位——写实现前先把两条断言写死）

- [x] **步骤 3.3：实现 nacos 加载器**

创建 `internal/configsrc/nacos.go`：
```go
package configsrc

// LoadNacos 三裁决：本地兜底 → dataId(key:hubx.<service>, group:HUBX) key 级覆盖 → HUBX_NACOS_SERVER env 覆盖地址。
// client 由注入点提供（生产 = nacos HTTP client；测试 = nacosClient fake）。
// dataId 为空 → 纯本地；远端不可达 → 返回 error（地基失败即退，MUST NOT 静默降级）。
func LoadNacos(path, dataID string, client Fetcher) (configx.Config, error) { ... }

// Fetcher 是 nacos 拉取的最小抽象（server 地址经 HUBX_NACOS_SERVER 解析后调用）。
type Fetcher interface {
	Fetch(ctx context.Context, dataID, group string) (map[string]string, error)
}
```
实现要点：`base, err := LoadLocal(path)`；`if dataID == "" { return base, nil }`；`remote, err := client.Fetch(ctx, "hubx."+dataID, "HUBX")`（dataID 传服务名，完整 dataId 拼接在函数内）；`err != nil → return nil, fmt.Errorf("configsrc: nacos 远端不可达（dataId %s）: %w", ...)`；逐 key 覆盖：对 remote 每个非空 key，`base.Set(key, val)`（configx.Config 若无 Set 方法，改为将本地 + 远端两份 yaml 文本合并后重读——以 configx 实际接口为准，Step 3.1 探查 `micro_apps/infra/nacos` 的合并写法并复制其语义）。

- [x] **步骤 3.4：运行确认通过 + 注册默认 nacos 源 + Commit**

运行：`go test ./internal/configsrc/ -run Nacos -v`
预期：PASS。
补充：`registry.go` 默认注册 `"nacos": 经 HUBX_NACOS_SERVER 解析地址的 Fetcher 包装`（生产 Fetcher 在 Step 3.3 后追加）。
```bash
git add internal/configsrc && git commit -m "feat: configsrc nacos 加载器（三裁决，B1）（1.3）"
```

- [x] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 1.3 同步） <!-- 已实现: cdbab21 -->

---

### Task 4: component 注册表与四个内置组件

> **trace:** plan-ready.md → `### Task 4: component 注册表与四个内置组件` | tasks.md → `- [ ] 1.4 实现 \`internal/component\` 注册表与内置 **db / cache / mq / registry / search(ES) / mongodb** 六类构建器：\`trace.on.<name>\` 只控入链不控构造（对应验收案例 D1，判定经 \`traceflag\` 组件级注册，不自造布尔逻辑）、\`strict.<name>\` 裁决降级 Warn 或返回 error（对应验收案例 D2）、registry 失败带病运行；各组件经 \`dbx/orm\`、\`cachex/redis\`、\`mqx/kafka\`、\`coordx/consul\`、\`searchx/elasticsearch\`、\`mongodbx\` 构造，不直引 gorm / go-redis / kafka-go / consul-api / elastic / mongo-driver；\`search\` 与 \`mongodb\` 同 \`cachex/redis\`、\`mqx/kafka\`、\`coordx/consul\` 属独立 module，go.mod 补对应 \`replace\`；presence-based 启用（配了 \`xmiddleware.searchx\` / \`mongodbx\` 段才构造）（对应验收案例 T2/T3/T4）`
> **sync:** tasks.md → `- [ ] 1.4 实现 \`internal/component\` 注册表与内置 **db / cache / mq / registry / search(ES) / mongodb** 六类构建器：\`trace.on.<name>\` 只控入链不控构造（对应验收案例 D1，判定经 \`traceflag\` 组件级注册，不自造布尔逻辑）、\`strict.<name>\` 裁决降级 Warn 或返回 error（对应验收案例 D2）、registry 失败带病运行；各组件经 \`dbx/orm\`、\`cachex/redis\`、\`mqx/kafka\`、\`coordx/consul\`、\`searchx/elasticsearch\`、\`mongodbx\` 构造，不直引 gorm / go-redis / kafka-go / consul-api / elastic / mongo-driver；\`search\` 与 \`mongodb\` 同 \`cachex/redis\`、\`mqx/kafka\`、\`coordx/consul\` 属独立 module，go.mod 补对应 \`replace\`；presence-based 启用（配了 \`xmiddleware.searchx\` / \`mongodbx\` 段才构造）（对应验收案例 T2/T3/T4）` | plan-ready.md → `### Task 4: component 注册表与四个内置组件`

> **偏差说明（2026-10-05 合并时登记）**：tasks.md 现行 1.4 已是**六类**（含 search / mongodb），而 plan-ready.md 的 Task 标题仍写「四个内置组件」。本 Task 已按**六类超集**实现（`bdbbdbf`，replace 23→26 条），不削减原四类任何行为；plan-ready 标题为 build 阶段禁止修改的规格文档，故此处保留原文标题，以 tasks.md 现行条目为验收口径。

**文件：**
- 创建：`internal/component/{registry.go,db.go,cache.go,mq.go,consul.go,strict.go}`、`internal/component/*_test.go`

- [x] **步骤 4.1：前置——coordx/consul 修复确认（plan-ready Task 4 Step 4.1 落地记录）**

运行（已修复则跳过编辑；design.md 记载已改，此处复核）：
```bash
sed -n '70,100p' /Users/hyx/work/gowork/src/hubx/coordx/consul/consul.go
grep -n 'startBeat\|UpdateTTL' /Users/hyx/work/gowork/src/hubx/coordx/consul/consul.go
```
预期：Check 仅含 `TTL: "30s"` 无 `Interval`；`startBeat` 存在（10s `UpdateTTL`）。若仍含 `Interval` 则按 design.md 决策修复：删 `Interval` 保留 TTL，并确认 `startBeat`/`stopBeat` 在 Deregister/Close 时停止。
```bash
cd /Users/hyx/work/gowork/src/hubx/coordx/consul && go build ./... && go vet ./...
```
预期：全绿。

- [x] **步骤 4.2：写失败测试（D1/D2：strict 双分支 + trace.on 只控入链 + registry 带病运行）**

创建 `internal/component/component_test.go`（TDD 先行，测试内以 fake Builder 断言裁决语义；真实 db/cache/mq builder 在步骤 4.3 接线后由步骤 4.5 集成断言）：
```go
package component

import (
	"errors"
	"strings"
	"testing"
)

// D2：strict 缺省 → 组件 nil + Warn（日志含组件名与后果文案）；strict 置真 → 返回 init <name> 包装 error。
func TestStrictArbitration(t *testing.T) {
	// 断言 1：failBuilder + strict=false → App.DB==nil，Warn 记录含 "db" 与降级后果
	// 断言 2：failBuilder + strict=true → Bootstrap 链路返回 error，err.Error() 含 "init db"
}

// D1：trace.on.mq=false 时 mq 组件仍正常构造（只不入链）——断言 App.MQ 非 nil 且 Send 可用（fake broker 或 skip）
func TestTraceOnOnlyGatesChain(t *testing.T) {
	// 构造 MQ 时 Trace.On("mq")=false → producer 照常构造
}

// registry 失败带病运行：consul 不可达 + strict.registry 未置真 → 启动继续，Registered()==false
func TestRegistryDegradedRun(t *testing.T) {
	// 断言 App.Registry 非 nil 且 Registered()==false；无 error 返回
}
```
测试体内断言完整写死（不留占位）；依赖 `Register`/builder 签名——与步骤 4.3 实现同文件一次落盘，先跑 RED 确认 `undefined: Register`。

- [x] **步骤 4.3：实现注册表 + 四个 builder**

`internal/component/registry.go`：
```go
package component

// Builder 构造一个组件；返回 (val, traceOn bool, err)：val 为句柄（nil=降级），
// traceOn 表示是否挂 OTel 回调（只控入链、不控构造），err 非 nil 时按 strict.<name> 裁决。
type Builder func(ctx context.Context, c *Ctx) (any, bool, error)

// Ctx 携带 configsrc 裁决后的配置与 Trace.On/Strict 查询。
type Ctx struct {
	Config configx.Config
	TraceOn func(name string) bool
	Strict  func(name string) bool
	Logger  logx.Logger
}

func Register(name string, b Builder)
func MountAll(ctx context.Context, c *Ctx, a *AppHolders) error // 依次挂 db/cache/mq/registry 到 App 句柄字段
```
`strict.go`：`Arbitrate(name string, err error, c *Ctx, a *AppHolders) error` —— `strict.<name>` 缺省 → Warn(`init %s 失败，降级运行：%v`)+置 nil 返回 nil；置真 → `return fmt.Errorf("init %s: %w", name, err)`。

`db.go`：`dbx/dbsql.ReadConfigFromConfigx(cfg, name)` + `dbsql.OpenFromConfigx(cfg, name, logger)`（enable_trace 经 traceflag 收敛，false 时 SDK 自动走 `orm.OpenNoTrace`，本库无需分叉——API 事实 §4）；`cache.go`：`cachex.Init(cachex.WithConfig(cfg, "cachex"))` + `cachex.Instance(name)`（未 Init 返回 nil → 降级态保持 nil）；`mq.go`：`mqx.ReadConfigFromConfigx(cfg, instance)` + `mqx.NewProducer`（driver kafka，blank import `_ "github.com/gospacex/mqx/kafka"` 由入口 binary 承载——在组件包以 `_` import 保证库内测试也注册）；`consul.go`：coordx `NewRegister/NewDiscover` + 移植 `micro_apps/infra/consul/registry.go:148` 的 `Target(agentAddr, serviceID)` 与 `:124` 的 `Resolve`（读 Discovery 结果拼 kitex `ClientOptions.Target` 逗号分隔串；空表返回空串）。

- [x] **步骤 4.4：运行 RED→GREEN + 集成断言**

运行：`go test ./internal/component/ -v`
预期：全部 PASS（含 strict 双分支、trace.on 门控、registry 带病运行；降级断言中 db/cache/mq 用 fake/不可达地址触发，不依赖真底座）。

- [x] **步骤 4.5：Commit**

```bash
git add internal/component ../coordx 2>/dev/null; git add internal/component && git commit -m "feat: component 注册表 + db/cache/mq/registry 内置 builder（D1/D2，1.4）"
```

- [x] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 1.4 同步） <!-- 已实现: bdbbdbf -->

---

### Task 5: mode 四档装配分支

> **trace:** plan-ready.md → `### Task 5: mode 四档装配分支` | tasks.md → `- [ ] 1.5 实现 \`internal/mode\` 四档装配分支：monolith/bff/srv/gateway 按映射表装配服务端与 consul 发现，非法/缺失 mode 返回含合法取值的 error（对应验收案例 C1/C2/C3，C4 为零改动约束）`
> **sync:** tasks.md → `- [ ] 1.5 实现 \`internal/mode\` 四档装配分支：monolith/bff/srv/gateway 按映射表装配服务端与 consul 发现，非法/缺失 mode 返回含合法取值的 error（对应验收案例 C1/C2/C3，C4 为零改动约束）` | plan-ready.md → `### Task 5: mode 四档装配分支`

**文件：**
- 创建：`internal/mode/{mode.go,webx.go,srv.go}`、`internal/mode/mode_test.go`

- [x] **步骤 5.1：写失败测试（C1/C2/C3 + 非法/缺失 mode 报错 + 拒绝 mesh 取值）** <!-- 已实现: 0cb00c8 -->

创建 `internal/mode/mode_test.go`：
```go
package mode

import (
	"regexp"
	"strings"
	"testing"
)

var legalModeErr = regexp.MustCompile(`monolith|bff|srv|gateway`)

func TestIllegalMode(t *testing.T) {
	_, err := Assemble(&AppHolders{ /* config: service.mode=oops */ })
	if err == nil || !legalModeErr.MatchString(err.Error()) {
		t.Fatalf("非法 mode 须含合法取值列表，got: %v", err)
	}
}

func TestMissingMode(t *testing.T) {
	_, err := Assemble(&AppHolders{/* 无 service.mode */})
	if err == nil || !legalModeErr.MatchString(err.Error()) {
		t.Fatalf("缺失 mode 须报含合法取值的 error，got: %v", err)
	}
}

func TestMeshRejected(t *testing.T) {
	// C4 纯代码断言：mode=mesh 必须被拒绝（mesh 非独立档，与微服同代码）
	_, err := Assemble(&AppHolders{/* service.mode=mesh */})
	if err == nil || !strings.Contains(err.Error(), "mesh") {
		t.Fatalf("mode=mesh 须被拒绝并提示，got: %v", err)
	}
}

func TestModeAssemblyMatrix(t *testing.T) {
	// 四档映射断言：monolith→webx 构建+不注册；bff→webx+注册+RPCClientOptions；
	// srv→不建 webx+RPCServerOptions+注册；gateway→webx 转发+不注册（上游发现经 Registry.Target）
}
```

- [x] **步骤 5.2：实现四档分支** <!-- 已实现: 0cb00c8 -->

`internal/mode/mode.go`：
```go
package mode

// Assemble 按 service.mode 装配服务端与 consul 发现。合法取值：monolith|bff|srv|gateway。
func Assemble(h *AppHolders) error {
	m := h.Config.GetString("service.mode")
	switch m {
	case "monolith": return assembleWebx(h, false, false) // 无注册、无下游工厂
	case "bff":      return assembleWebx(h, true, true)     // consul 注册 + RPCClientOptions 工厂
	case "gateway":  return assembleWebx(h, false, false)   // webx 转发，上游发现经 Registry.Target
	case "srv":      return assembleSrv(h)                  // 不建 webx，rpcx ServerOptions + consul 注册
	case "":         return errMissingMode()
	case "mesh":     return errMeshRejected()
	default:         return errIllegalMode(m)
	}
}
```
`webx.go`：`webx.New(webx.WithConfig(webx.ReadConfigFromConfigx(cfg)), webx.WithLogger(h.Logger), webx.WithTracer(h.Tracer))`，PreRun 挂路由由 Setup 回调完成（Task 8 的 `app/routes.go` 经 `WithPreRun` 注入）；bff 档额外产出 `RPCClientOptions(service string)` 工厂（Target = `Registry.Target(agentAddr, service)`，挂 obs suite）；`srv.go`：`ServerOptions`（rpcx.NewServer 的 options 组装，kitex server 由业务 Setup 构建，句柄存 `App.RPCServer`）。
error 文案：`errMissingMode/errIllegalMode/errMeshRejected` 均含 `合法取值：monolith|bff|srv|gateway`，mesh 追加「mesh 非独立档，与微服同代码，请使用 srv/gateway/bff 并以部署清单体现网格」。

- [x] **步骤 5.3：运行确认通过 + Commit** <!-- 已实现: 0cb00c8 -->

运行：`go test ./internal/mode/ -v`
预期：PASS（C1/C2/C3 + 非法/缺失/mesh 拒绝）。
```bash
git add internal/mode && git commit -m "feat: mode 四档装配分支（C1-C3/C4 纯代码断言，1.5）"
```

- [x] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 1.5 同步） <!-- 已实现: 0cb00c8 -->

---

### Task 6: pipeline 固定启动骨架与 telemetry 封装

> **trace:** plan-ready.md → `### Task 6: pipeline 固定启动骨架与 telemetry 封装` | tasks.md → `- [ ] 1.6 实现 \`internal/pipeline\` 固定启动骨架与 \`internal/telemetry\`（otelx 封装）：配置 → telemetry.Init（全局 TracerProvider + Composite 传播器，最先）→ logx.SetGlobal → mode 装配 → 组件挂载 → 回调 Setup；顺序用事件记录器可断言（对应验收案例 A2）`
> **sync:** tasks.md → `- [ ] 1.6 实现 \`internal/pipeline\` 固定启动骨架与 \`internal/telemetry\`（otelx 封装）：配置 → telemetry.Init（全局 TracerProvider + Composite 传播器，最先）→ logx.SetGlobal → mode 装配 → 组件挂载 → 回调 Setup；顺序用事件记录器可断言（对应验收案例 A2）` | plan-ready.md → `### Task 6: pipeline 固定启动骨架与 telemetry 封装`

**文件：**
- 创建：`internal/telemetry/provider.go`、`internal/pipeline/{pipeline.go,events.go}`（替换 Task 1 占位）、`internal/telemetry/provider_test.go`、`internal/pipeline/pipeline_test.go`

- [x] **步骤 6.1：写失败测试（A2 顺序断言：telemetry 早于 db/cache/mq 构建；exit 序 deregister 早于 flush、flush 最后）**

创建 `internal/pipeline/pipeline_test.go`（事件记录器断言）：
```go
func TestStartupOrder(t *testing.T) {
	// 事件记录器（events.go 的 Recorder）断言序列：
	// "configsrc.load" < "telemetry.init" < "logx.global" < "mode.assemble" < "component.db" < "component.cache" < "component.mq" < "component.registry" < "setup.callback"
}

func TestShutdownOrder(t *testing.T) {
	// 退出序列断言：server.stop < component.stop... < "registry.deregister" < "tracing.flush" < "logger.close"
	// deregister 早于 flush；flush 最后（logger.close 紧随其后）
}
```

- [x] **步骤 6.2：实现 telemetry 封装 + events + pipeline.Run**

`internal/telemetry/provider.go`：
```go
// Init 全局 TracerProvider（otelx.New，Enabled 经 traceflag 收敛；false → noop 不设全局）
// + Composite 传播器（API 事实 §2：otelx 无传播器，须在此自装）：
//   otel.SetTracerProvider(p.TracerProvider())
//   otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
//       propagation.TraceContext{}, propagation.Baggage{}))
func Init(cfg *otelx.Config) (Shutdown func(ctx context.Context), err error)

// Shutdown 15s flush（spec A3：会标 span 资源必须先于 15s flush 关闭 → 调用方保证组件 Stop 先完成）
```
`internal/pipeline/events.go`：`type Recorder struct { events []string; mu sync.Mutex }`，`Record(evt)` + `ContainsInOrder(e1, e2 string) bool`（测试用）。
`internal/pipeline/pipeline.go`：`Run(path string)` 按七步骨架调用 configsrc（按 yaml `configcenter:` 段裁决 local/nacos，缺省 local）→ telemetry.Init（**最先**，事件 `telemetry.init`）→ logx 装配（`_ "github.com/gospacex/logx/zap"` blank import 在此包声明，保证库内测试亦注册 zap provider；`logx.New + SetGlobal`）→ mode.Assemble → component.MountAll → 用户 Setup 回调；每步 `Recorder.Record`。地基失败（config/telemetry/logx）即返回 error 且走已装配部分的退出链。

- [x] **步骤 6.3：运行确认通过 + 解除 Task 1 占位 + Commit**

运行：`go test ./internal/... -v`
预期：`TestStartupOrder`/`TestShutdownOrder` PASS（ShutdownOrder 依赖 Task 7 的 shutdown.go，若尚未实现则以 `component.stop→deregister→tracing.flush→logger.close` 的桩事件先断言存在，Task 7 落实真实序列）。
```bash
git add internal/telemetry internal/pipeline launchx.go && git commit -m "feat: pipeline 七步骨架 + telemetry 封装（A2，1.6）"
```

- [x] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 1.6 同步） <!-- 已实现: fc02f6c -->

---

### Task 7: 反序退出编排

> **trace:** plan-ready.md → `### Task 7: 反序退出编排` | tasks.md → `- [ ] 1.7 实现反序退出编排（\`App.Run\` 收尾段）：server → 组件 Stop → deregister → tracing.Shutdown(15s flush) → logger.Close，显式序列不依赖 defer；Setup 失败与运行期错误走同一退出链（对应验收案例 A3）`
> **sync:** tasks.md → `- [ ] 1.7 实现反序退出编排（\`App.Run\` 收尾段）：server → 组件 Stop → deregister → tracing.Shutdown(15s flush) → logger.Close，显式序列不依赖 defer；Setup 失败与运行期错误走同一退出链（对应验收案例 A3）` | plan-ready.md → `### Task 7: 反序退出编排`

**文件：**
- 创建：`internal/pipeline/shutdown.go`；修改：`app.go`（Run/Close 接线）、`setup.go`（OnStop 逆序）

- [ ] **步骤 7.1：写失败测试（A3：退出顺序 + Setup 失败走完整退出链 + Close 幂等）**

追加至 `internal/pipeline/pipeline_test.go`（与 Task 6 的 `TestShutdownOrder` 合并断言，不重复）：
```go
func TestSetupFailureWalksFullShutdown(t *testing.T) {
	// Setup 回调返回 error → 仍执行 server.stop → component.stop → deregister → tracing.flush → logger.close 全链，
	// 并返回 Setup 首错（非退出链错误覆盖）
}

func TestCloseIdempotent(t *testing.T) {
	// Close() 两次：第二次直接返回 nil，退出事件序列只出现一次
}
```

- [ ] **步骤 7.2：实现 shutdown.go**

```go
// Shutdown 显式序列（MUST NOT 依赖 defer；A3 红线代码化）：
//  1. server stop（webx.App.Shutdown(ctx 10s) 或 rpcx server.Stop；按 OnStop 钩子逆序先执行业务钩子）
//  2. 组件 Stop（mq producer.Close → cache CloseAll → db（gorm Close + dbsql.ShutdownTracerProvider））
//  3. registry.deregister（coordx Deregister；未注册态跳过）
//  4. tracing.flush（telemetry.Shutdown，15s 超时——会标 span 资源必须先于本步关闭，序列保证）
//  5. logger.close（logx.CloseGlobal，最后）
// Setup 失败/运行期错误：先跑本链再返回首个原始错误。
func Shutdown(a *AppHolders) error
```
`app.go` 的 `Run()` 接线：`等待 SIGINT/SIGTERM → Shutdown(a)`；`Close()` 复用 `Shutdown` 且幂等（Task 1 已占位，此处落实）。

- [ ] **步骤 7.3：运行确认通过 + Commit**

运行：`go test ./... -v`
预期：A3 全组 PASS（退出顺序断言、Setup 失败全链、Close 幂等）。
```bash
git add internal/pipeline app.go setup.go && git commit -m "feat: 反序退出编排（A3，1.7）"
```

- [ ] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 1.7 同步）

---

### Task 8: examples/monolith 单体 demo

> **trace:** plan-ready.md → `### Task 8: examples/monolith 单体 demo` | tasks.md → `- [ ] 2.1 新建 \`examples/monolith/\`：参考 \`webx/examples\` 的 config.yaml 与分层（handler→logic→service→repository），\`Bootstrap + OnSetup\` 一行启动；订单存储经 launchx db 组件接真实 mysql（DSN 经 \`\${ENV}\` 展开，底座复用 \`hubx/dockers\` compose 的 mysql 8.4）（对应验收案例 E1）`
> **sync:** tasks.md → `- [ ] 2.1 新建 \`examples/monolith/\`：参考 \`webx/examples\` 的 config.yaml 与分层（handler→logic→service→repository），\`Bootstrap + OnSetup\` 一行启动；订单存储经 launchx db 组件接真实 mysql（DSN 经 \`\${ENV}\` 展开，底座复用 \`hubx/dockers\` compose 的 mysql 8.4）（对应验收案例 E1）` | plan-ready.md → `### Task 8: examples/monolith 单体 demo`

**文件：**
- 创建：`examples/monolith/config.yaml`、`examples/monolith/cmd/main.go`（≤50 行）、`examples/monolith/app/{setup.go,routes.go}`、`examples/monolith/internal/{handler,logic,service,repository,model}/*.go`

- [ ] **步骤 8.1：底座前置（plan-ready 2026-10-04 底座事实）**

运行：
```bash
docker compose -f /Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml up -d mysql && docker compose -f /Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml ps
```
预期：mysql `127.0.0.1:3306` 监听；账号 `order_rw/hubx123`、库 `order_db` 由 `hubx/micro_apps/docker/mysql/init.sql` 幂等灌入（`hubx/dockers` compose 挂载该 init 则自动完成，否则手动 `mysql -uroot -p$(docker inspect 提取) < .../init.sql` 一次）。

- [ ] **步骤 8.2：config.yaml（mode monolith + DSN 占位）**

创建 `examples/monolith/config.yaml`（对齐 srv-order 契约风格，DSN 用 `${HUBX_DB_PW}` 占位，运行前 `export HUBX_DB_PW=hubx123`；kafka 段本 demo 不用则不写）：
```yaml
service:
  name: demo-monolith
  mode: monolith
webx:
  addr: "127.0.0.1:8080"
xmiddleware:
  sqlx:
    driver: gorm
    databases:
      demo-monolith:
        dsn: "order_rw:${HUBX_DB_PW}@tcp(127.0.0.1:3306)/order_db?charset=utf8mb4&parseTime=True&loc=Local"
        max_open_conns: 20
        max_idle_conns: 5
logx:
  level: info
  otel:
    enable_trace: true
    exporter: otlp
    endpoint: "127.0.0.1:4318"
```

- [ ] **步骤 8.3：分层代码 + main.go（≤50 行）+ 路由**

`internal/model`（Order 表 GORM model）、`repository`（经 `*gorm.DB` 读写 order_db）、`service`（订单创建/查询逻辑）、`logic`（参数校验/错误映射）、`handler`（hertz handler）、`app/routes.go`（`func RegisterRoutes(e *app.Engine, h *handler.OrderH)`——经 `webx.WithPreRun` 挂入）、`app/setup.go`（`func Setup(lx *launchx.App) error`：装配 repo→service→logic→handler 并存闭包）。
`cmd/main.go` 核心形态（实际 ≤50 行，含注释）：
```go
func main() {
	app, err := launchx.Bootstrap("config.yaml")
	if err != nil { log.Fatal(err) }
	app.OnSetup(appsetup.Setup)
	if err := app.Run(); err != nil { log.Fatal(err) }
}
```

- [ ] **步骤 8.4：启动 + HTTP 断言（E1 前半）**

运行：
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx/examples/monolith && export HUBX_DB_PW=hubx123 && go build ./... && (go run ./cmd &>/tmp/monolith.log &) && sleep 3 && curl -s localhost:8080/orders && curl -s -X POST localhost:8080/orders -H 'Content-Type: application/json' -d '{"item":"demo","qty":1}'
```
预期：GET 返回 JSON 列表（初始空数组或既有数据）；POST 201/200 且再 GET 可见新增行（落 mysql）。

- [ ] **步骤 8.5：SIGTERM 优雅退出断言（E1 后半）+ Commit**

运行：
```bash
kill -TERM $(pgrep -f 'examples/monolith') && sleep 2 && grep -E 'deregister|flush|logger' /tmp/monolith.log
```
预期：日志退出序列完整（server → 组件 Stop → deregister → tracing.flush → logger.Close），进程退出码 0。
```bash
git add examples/monolith && git commit -m "feat: examples/monolith 单体 demo（E1，2.1）"
```

- [ ] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 2.1 同步）

---

### Task 9: examples/micro/gateway 网关 demo

> **trace:** plan-ready.md → `### Task 9: examples/micro/gateway 网关 demo` | tasks.md → `- [ ] 2.2 新建 \`examples/micro/gateway\`：webx 转发 + consul 上游发现，\`Bootstrap\` 一行启动`
> **sync:** tasks.md → `- [ ] 2.2 新建 \`examples/micro/gateway\`：webx 转发 + consul 上游发现，\`Bootstrap\` 一行启动` | plan-ready.md → `### Task 9: examples/micro/gateway 网关 demo`

**文件：**
- 创建：`examples/micro/gateway/{config.yaml,cmd/main.go,app/setup.go,internal/routing/routes.go,internal/proxy/proxy.go}`

- [ ] **步骤 9.1：config.yaml（mode gateway + gateway.routes /api→bff）**

```yaml
service:
  name: demo-gateway
  mode: gateway
webx:
  addr: "127.0.0.1:8080"
gateway:
  routes:
    - prefix: /api
      upstream: bff
coordx:
  driver: consul
  addr: "http://127.0.0.1:8500"
```
（yaml 段名以 Task 4/5 实现中实际读取的键为准，Step 9.1 先探查 `internal/component/consul.go` 与 `internal/mode` 读取的 config 段名，再定稿本文件——**不得**凭本步骤文本盲写）

- [ ] **步骤 9.2：转发实现（Registry.Target + hertz Any 反向代理 + 状态码映射）**

`internal/proxy/proxy.go`：经 `app.Registry.Target(agentAddr, "bff")` 取上游地址串（consul 不可达或空表 → 响应 502；参考 micro_apps gateway 行为但精简：路由匹配 + 上游 404/429/502 原样透传映射）；`internal/routing/routes.go`：`a.Any("/*p", handler)` 挂转发。`app/setup.go` 经 `WithPreRun` 注入。
`cmd/main.go` 同 Task 8 形态（≤50 行）。

- [ ] **步骤 9.3：构建 + 单进程可启动断言（E2 前半前置，完整链路在 Task 19）**

运行：
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx/examples/micro/gateway && go build ./... && grep -c 'launchx.Bootstrap' cmd/main.go
```
预期：编译通过；main.go 中 `launchx.Bootstrap` 出现 1 次（一行启动形态）。
```bash
git add examples/micro/gateway && git commit -m "feat: examples/micro/gateway 网关 demo（2.2，E2 前半）"
```

- [ ] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 2.2 同步）

---

### Task 10: examples/micro/bff 服务 demo

> **trace:** plan-ready.md → `### Task 10: examples/micro/bff 服务 demo` | tasks.md → `- [ ] 2.3 新建 \`examples/micro/bff\`：webx 入口 + consul 注册 + 下游 srv rpcx 客户端，\`Bootstrap\` 一行启动（与 2.2 共同对应验收案例 E2）`
> **sync:** tasks.md → `- [ ] 2.3 新建 \`examples/micro/bff\`：webx 入口 + consul 注册 + 下游 srv rpcx 客户端，\`Bootstrap\` 一行启动（与 2.2 共同对应验收案例 E2）` | plan-ready.md → `### Task 10: examples/micro/bff 服务 demo`

**文件：**
- 创建：`examples/micro/bff/{config.yaml,cmd/main.go,app/setup.go,internal/handler/order.go,internal/downstream/client.go}`

- [ ] **步骤 10.1：依赖前置检查（bff 调 srv 需要 Task 11 迁入的 kitex_gen 与下游 client 类型）**

本 Task 的 `internal/downstream` 依赖 `examples/micro/srv/pkg/kitex_gen`（同 module import）与 srv 已注册 consul → **执行顺序：先 Task 11 的复制部分（步骤 11.1–11.2），再本 Task；11.3（main.go 改写）与 11.4 可回移**。若 kitex_gen 尚未迁入，先跑 Task 11 的 11.1/11.2。

- [ ] **步骤 10.2：config.yaml + main.go + 下游调用**

`config.yaml`：`service.mode: bff`、`webx.addr: "127.0.0.1:8081"`（`examples/ACCEPTANCE.md` §13.2 端口定稿；原写的 `8091` 为笔误，已统一为 8081）、`coordx` 段同 gateway、`rpcx` 下游段（`downstream.srv.target` 缺省经 `Registry.Target` 解析）。
`internal/downstream/client.go`：经 `app.RPCClientOptions("srv")` 工厂（Task 5 产出）构造 kitex 客户端（import `examples/micro/srv/pkg/kitex_gen` 的订单服务类型），封装 `GetOrder(ctx, id)`；**不**为 srv 新增 HTTP 适配层（plan-ready 2026-10-04 翻译层修正）。
`internal/handler/order.go`：`GET /ping`（自响应 `{"service":"bff"}`）、`GET /order/:id`（调下游 GetOrder）。

- [ ] **步骤 10.3：构建 + 单进程可启动 + Commit**

运行：
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx/examples/micro/bff && go build ./...
```
预期：编译通过（下游调用断言在 Task 19 全链路执行）。
```bash
git add examples/micro/bff && git commit -m "feat: examples/micro/bff demo（rpcx 下游客户端，2.3，E2 中段）"
```

- [ ] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 2.3 同步）

---

### Task 11: examples/micro/srv 迁移 srv-order

> **trace:** plan-ready.md → `### Task 11: examples/micro/srv 迁移 srv-order` | tasks.md → `- [ ] 2.4 迁移 \`micro_apps/srv-order\` 为 \`examples/micro/srv\`：删除原 300 行 main.go 与 \`infra/\` 复制件，改为 \`Bootstrap\` 一行形态，业务分层保留（对应验收案例 E3，main.go ≤ 50 行）`
> **sync:** tasks.md → `- [ ] 2.4 迁移 \`micro_apps/srv-order\` 为 \`examples/micro/srv\`：删除原 300 行 main.go 与 \`infra/\` 复制件，改为 \`Bootstrap\` 一行形态，业务分层保留（对应验收案例 E3，main.go ≤ 50 行）` | plan-ready.md → `### Task 11: examples/micro/srv 迁移 srv-order`

**文件：**
- 创建：`examples/micro/srv/{config.yaml,cmd/main.go,app/setup.go,internal/**,pkg/kitex_gen/**}`（自 `hubx/micro_apps/apps/srv-order` 复制业务分层，**不**复制 `cmd/srv-order/main.go` 原 300+ 行装配与 `micro_apps/infra` import；原仓零改动）

- [ ] **步骤 11.1：复制业务代码（分层 + kitex_gen + 测试，排除旧装配）**

运行：
```bash
cd /Users/hyx/work/gowork/src/hubx && mkdir -p launchx/examples/micro/srv/{cmd,app,internal,pkg} && cp -r micro_apps/apps/srv-order/internal launchx/examples/micro/srv/ && find micro_apps/apps/srv-order -name 'kitex_gen' -o -name 'pkg' | head
```
随后把 srv-order 的 `pkg/kitex_gen` 与 `pkg/events`（如存在）复制进 `launchx/examples/micro/srv/pkg/`；**剔除**所有 `micro_apps/infra/*` 与 `micro_apps/pkg/*` import（替换为 launchx 句柄：infra/consul→`App.Registry`、infra/telemetry→`App.Tracer`+telemetry 内建、infra/drivers→`App.DB/Cache/MQ`、infra/nacos→`configsrc`）。

- [ ] **步骤 11.2：config.yaml（修正端口/DSN 占位，API 事实基线 6）**

以 srv-order 原 config.yaml 为底（已读：`xmiddleware.sqlx/cachex/mqx` 三段 + `enable_trace` 契约），改写：kafka `127.0.0.1:19093` → `127.0.0.1:9092`（本机 dockers 现行端口，**不**沿用 19093）；DSN 保留 `${HUBX_DB_PW}` 占位；追加 `service.name: demo-srv`、`service.mode: srv`、`coordx` 段、`rpcx` 监听段（端口与 micro_apps 原 srv-order 一致，供 bff Target 解析；探查原仓 rpc 段端口后定稿）。

- [ ] **步骤 11.3：main.go + app/setup.go（Bootstrap 一行形态，main.go ≤50 行）**

`cmd/main.go`：同 Task 8 形态（`Bootstrap("config.yaml") + OnSetup + Run`）。
`app/setup.go`：`Setup(*launchx.App)` 内装配 repo/outbox（经 `App.DB`）/service/rpc handler（经 `App.MQ` 的 outbox 补发 producer）、以 `App.RPCServerOptions` 构建 kitex rpcx server、`go server.Run()`、`OnStop` 注册反序关闭（rpc server Stop → outbox flush）。
运行：`wc -l examples/micro/srv/cmd/main.go` → **必须 ≤50 行**（E3 断言，Task 20 复核）。

- [ ] **步骤 11.4：构建 + 可接收 RPC 前置断言 + Commit**

运行：
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx/examples/micro/srv && export HUBX_DB_PW=hubx123 && go build ./... && go test ./... && wc -l cmd/main.go
```
预期：编译 + 单测 PASS（原仓 internal 测试随迁全绿）；main.go ≤50 行。完整 e2e（下单 + outbox→kafka）在 Task 20。
```bash
git add examples/micro/srv && git commit -m "feat: srv-order 迁移为 examples/micro/srv（Bootstrap 形态，E3 前置，2.4）"
```

- [ ] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 2.4 同步）

---

### Task 12: 3.1 configsrc 单测补全（B1）

> **trace:** plan-ready.md → `### Task 14: configsrc 单测套件（验收案例 B1）` | tasks.md → `- [ ] 3.1 单测：configsrc 三裁决规则 + local env 展开 + 远端不可达即退（验收案例 B1）`
> **sync:** tasks.md → `- [ ] 3.1 单测：configsrc 三裁决规则 + local env 展开 + 远端不可达即退（验收案例 B1）` | plan-ready.md → `### Task 14: configsrc 单测套件（验收案例 B1）`

**文件：**
- 修改：`internal/configsrc/local_test.go`、`internal/configsrc/nacos_test.go`

- [ ] **步骤 12.1：补全 B1 全分支（本地 + 远端不可达；真机 nacos 分支 Skip）**

追加至 `internal/configsrc/nacos_test.go`（key 级裁决与不可达即退已有，补两处）：
```go
// dataId 为空 → 纯本地（不触达 fetch；断言 fetch 未被调用）
func TestNacosEmptyDataIDPureLocal(t *testing.T) {
	calls := 0
	_, err := LoadNacos(writeCfg(t), "", &nacosClient{fetch: func(context.Context, string, string) (map[string]string, error) {
		calls++; return nil, nil
	}})
	if err != nil || calls != 0 {
		t.Fatalf("dataId 空必须纯本地（err=%v calls=%d）", err, calls)
	}
}

// HUBX_NACOS_SERVER env 覆盖 server 地址（地址级最高优先）
func TestNacosServerEnvOverride(t *testing.T) {
	// 断言：Fetch 收到的 server 地址 = $HUBX_NACOS_SERVER（Fetcher 实现须把解析后的地址传入/记录，
	// 测试以 fake Fetcher 记录入参或注入 addr 字段断言；若 Fetcher 签名不含地址，则断言解析函数
	// resolveNacosServer() 的返回值——本步随实现落点补断言，不留占位）
}
```
真机 nacos 底座裁决断言：以 `func TestNacosRealServer(t *testing.T) { t.Skip("无 nacos 底座；真机分支待回补") }` 形态保留（本轮裁决：不强制起 nacos 容器）。
运行：`go test ./internal/configsrc/ -v`
预期：全绿（Skip 项可见不阻塞）。

- [ ] **步骤 12.2：Commit**

```bash
git add internal/configsrc && git commit -m "test: configsrc B1 全分支单测（3.1，真机分支 Skip 保留）"
```

- [ ] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 3.1 同步）

---

### Task 13: 3.2 pipeline 顺序单测（A2/A3）

> **trace:** plan-ready.md → `### Task 15: pipeline 顺序单测（验收案例 A2/A3）` | tasks.md → `- [ ] 3.2 单测：pipeline 启动顺序断言（telemetry 先于组件）与退出顺序断言（deregister 先于 flush、flush 最后）（验收案例 A2/A3）`
> **sync:** tasks.md → `- [ ] 3.2 单测：pipeline 启动顺序断言（telemetry 先于组件）与退出顺序断言（deregister 先于 flush、flush 最后）（验收案例 A2/A3）` | plan-ready.md → `### Task 15: pipeline 顺序单测（验收案例 A2/A3）`

**文件：**
- 修改：`internal/pipeline/pipeline_test.go`、`internal/pipeline/shutdown_test.go`（Task 7 已建则补全断言）

- [ ] **步骤 13.1：启动序断言**

`pipeline_test.go` 补全事件记录器断言（Task 6 的 Recorder）：`telemetry.init` 严格早于 `component.db`/`component.cache`/`component.mq`；`configsrc.load` 最早；`logx.global` 在 `telemetry.init` 后、`mode.assemble` 前。
运行：`go test ./internal/pipeline/ -run TestStartupOrder -v`
预期：PASS。

- [ ] **步骤 13.2：退出序断言 + Commit**

`shutdown_test.go` 补全：`registry.deregister` 早于 `tracing.flush`；`tracing.flush` 早于 `logger.close` 且为最后一条 flush 类事件；会标 span 资源（db/mq 组件 Stop 事件）均早于 `tracing.flush`。
运行：`go test ./internal/pipeline/ -run 'TestShutdown|TestSetupFailure|TestCloseIdempotent' -v`
预期：全 PASS。
```bash
git add internal/pipeline && git commit -m "test: pipeline 启动/退出顺序断言（3.2，A2/A3）"
```

- [ ] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 3.2 同步）

---

### Task 14: 3.3 component 单测（D1/D2）

> **trace:** plan-ready.md → `### Task 16: component 单测（验收案例 D1/D2）` | tasks.md → `- [ ] 3.3 单测：component 双分支（strict 降级/失败）、\`trace.on\` 门控不改变组件构造（验收案例 D1/D2）`
> **sync:** tasks.md → `- [ ] 3.3 单测：component 双分支（strict 降级/失败）、\`trace.on\` 门控不改变组件构造（验收案例 D1/D2）` | plan-ready.md → `### Task 16: component 单测（验收案例 D1/D2）`

**文件：**
- 修改：`internal/component/component_test.go`（Task 4 已有骨架则补全；无则新建）

- [ ] **步骤 14.1：补全三组断言（TDD 风格，先写断言再核对实现）**

```go
// D2 降级：strict 缺省 → MountAll 返回 nil、App.DB==nil、Warn 记录含 "db" 与降级后果文案
// D2 即退：strict.db=true → 返回 error，err.Error() 含 "init db"（%w 包装链可 errors.Is 判定）
// D1 门控：TraceOn("mq")=false → App.MQ 非 nil（构造发生，仅入链关闭）
```
断言全部写死（无占位）；依赖 fake Builder 与 fake Fetcher。
运行：`go test ./internal/component/ -v`
预期：全 PASS。

- [ ] **步骤 14.2：Commit**

```bash
git add internal/component && git commit -m "test: component strict 双分支 + trace.on 门控单测（3.3，D1/D2）"
```

- [ ] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 3.3 同步）

---

### Task 15: 3.4 mode 单测（C1/C2/C3 + 非法/mesh 拒绝）

> **trace:** plan-ready.md → `### Task 17: mode 单测（验收案例 C1/C2/C3）` | tasks.md → `- [ ] 3.4 单测：mode 四档装配与非法 mode 报错（验收案例 C1/C2/C3）`
> **sync:** tasks.md → `- [ ] 3.4 单测：mode 四档装配与非法 mode 报错（验收案例 C1/C2/C3）` | plan-ready.md → `### Task 17: mode 单测（验收案例 C1/C2/C3）`

**文件：**
- 修改：`internal/mode/mode_test.go`（Task 5 已有 C 系列骨架则补全映射矩阵；无则新建）

- [ ] **步骤 15.1：补全四档映射矩阵断言**

```go
// monolith：webx 构建、不注册、无下游工厂
// bff：webx 构建 + 注册 + RPCClientOptions 工厂产出（Target 解析路径存在）
// srv：不建 webx、ServerOptions 产出（rpcx options 含服务名与端口）、注册
// gateway：webx 构建 + 转发路由注册 + 不注册（上游发现经 Registry.Target）
// 非法 mode / 缺失 mode / mesh：error 文案含合法取值列表 monolith|bff|srv|gateway（mesh 追加说明）
```
运行：`go test ./internal/mode/ -v`
预期：全 PASS。

- [ ] **步骤 15.2：Commit**

```bash
git add internal/mode && git commit -m "test: mode 四档装配 + 非法/缺失/mesh 拒绝单测（3.4，C1-C3）"
```

- [ ] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 3.4 同步）

---

### Task 16: 3.5 E1 monolith 集成验收

> **trace:** plan-ready.md → `### Task 18: monolith 集成验收（验收案例 E1）` | tasks.md → `- [ ] 3.5 集成验收：\`examples/monolith\` 在共享底座（\`hubx/dockers\` compose，mysql 就绪）下启动 + HTTP 请求（订单读写落 mysql）+ SIGTERM 正常退出（验收案例 E1）`
> **sync:** tasks.md → `- [ ] 3.5 集成验收：\`examples/monolith\` 在共享底座（\`hubx/dockers\` compose，mysql 就绪）下启动 + HTTP 请求（订单读写落 mysql）+ SIGTERM 正常退出（验收案例 E1）` | plan-ready.md → `### Task 18: monolith 集成验收（验收案例 E1）`

**文件：**
- 创建：`examples/monolith/e2e_test.go`

- [ ] **步骤 16.1：写 E1 e2e 测试（底座前置 + 读写断言 + SIGTERM 退出断言）**

```go
func TestMonolithE2E(t *testing.T) {
	// 前置：mysql 127.0.0.1:3306 可连（net.DialTimeout 1s）+ order_db 存在（gorm 直查 information_schema）
	//       底座拉起命令：docker compose -f /Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml up -d mysql
	// 1. 进程内启动：app, _ := launchx.Bootstrap("config.yaml")；app.OnSetup(appsetup.Setup)；go app.Run()
	//    （HUBX_DB_PW=hubx123 经 t.Setenv 注入，DSN 占位经 ExpandEnv 展开）
	// 2. GET localhost:8080/orders 断言 200 + JSON 数组；POST /orders {"item":"e2e","qty":1} 断言 2xx
	// 3. gorm 直连 order_db 断言新增行存在（订单落 mysql，非内存）
	// 4. app.Close() → 断言退出事件序列含 server.stop/component.stop/registry.deregister/tracing.flush/logger.close
	//    且进程不挂起（Close 返回 ≤5s）
}
```
运行：`cd examples/monolith && HUBX_DB_PW=hubx123 go test -run TestMonolithE2E -v`
预期：全绿。

- [ ] **步骤 16.2：Commit**

```bash
git add examples/monolith/e2e_test.go && git commit -m "test: monolith E1 集成验收（3.5）"
```

- [ ] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 3.5 同步）

---

### Task 17: 3.6 E2 微服调用链集成验收

> **trace:** plan-ready.md → `### Task 19: micro 调用链集成验收（验收案例 E2）` | tasks.md → `- [ ] 3.6 集成验收：共享底座（consul + kafka + jaeger，\`hubx/dockers\` compose）就绪下，\`gateway → bff → srv\` 调用链返回预期响应（验收案例 E2）`
> **sync:** tasks.md → `- [ ] 3.6 集成验收：共享底座（consul + kafka + jaeger，\`hubx/dockers\` compose）就绪下，\`gateway → bff → srv\` 调用链返回预期响应（验收案例 E2）` | plan-ready.md → `### Task 19: micro 调用链集成验收（验收案例 E2）`

**文件：**
- 创建：`examples/micro/scripts/e2e.sh`

- [ ] **步骤 17.1：写 E2 脚本（底座前置 + 三进程 + 双 curl 断言 + 清理）**

```bash
#!/usr/bin/env bash
set -euo pipefail
COMPOSE=/Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml
docker compose -f "$COMPOSE" up -d consul kafka jaeger redis   # 底座前置（幂等）
export HUBX_DB_PW=hubx123
cd "$(dirname "$0")/.."

# 1. srv：rpc 端口就绪 + consul passingOnly 查到 1 实例
(cd srv && go build -o /tmp/srv ./cmd) && /tmp/srv & SRV_PID=$!
#    轮询 consul KV/health API（curl http://127.0.0.1:8500/v1/health/service/demo-srv?passing 直到非空，超时 30s）
# 2. bff：/tmp/bff（8081）启动，同上轮询注册
# 3. gateway：/tmp/gateway（8080）启动
# 4. 断言：
#    curl -sf http://127.0.0.1:8080/api/ping | grep -q '"service":"bff"'
#    curl -sf http://127.0.0.1:8080/api/order/1001 | grep -q '<srv 侧订单数据标识，如 "order_id"'
# 5. 清理：kill 三进程（trap EXIT）
```
脚本正文按上述骨架写全（轮询函数、trap、超时退出码）；**种子数据**：步骤 4 的 `/order/1001` 断言依赖 order_db 存在 id=1001 的订单——e2e 前先经 monolith 或直连 mysql 灌一行（`INSERT` 语句写入脚本步骤 1 之前，幂等 `ON DUPLICATE KEY UPDATE`）。
运行：`bash examples/micro/scripts/e2e.sh`
预期：两条断言通过，退出码 0。

- [ ] **步骤 17.2：Commit**

```bash
git add examples/micro/scripts && git commit -m "test: micro E2 调用链集成验收（3.6，gateway→bff→srv）"
```

- [ ] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 3.6 同步）

---

### Task 18: 3.7 E3 srv 回归（原 e2e 下单 + outbox→kafka + main.go ≤50 行）

> **trace:** plan-ready.md → `### Task 20: srv-order e2e 回归（验收案例 E3）` | tasks.md → `- [ ] 3.7 回归验收：迁移后 srv 跑原 srv-order e2e 下单链路全绿，含 outbox 经 kafka 投递断言（topic 自动创建），\`examples/micro/srv/cmd/main.go\` ≤ 50 行（验收案例 E3）`
> **sync:** tasks.md → `- [ ] 3.7 回归验收：迁移后 srv 跑原 srv-order e2e 下单链路全绿，含 outbox 经 kafka 投递断言（topic 自动创建），\`examples/micro/srv/cmd/main.go\` ≤ 50 行（验收案例 E3）` | plan-ready.md → `### Task 20: srv-order e2e 回归（验收案例 E3）`

**文件：**
- 创建：`examples/micro/srv/scripts/run-e2e.sh`

- [ ] **步骤 18.1：写 E3 回归脚本（下单链路 + outbox 投递断言 + 行数断言）**

```bash
#!/usr/bin/env bash
set -euo pipefail
# 前置：mysql + kafka(9092, KRaft AUTO_CREATE_TOPICS_ENABLE=true) 就绪（hubx/dockers compose）
# 1. 启动迁移后 srv（/tmp/srv）+ 等 consul 注册
# 2. 原 srv-order e2e 下单场景：以 kitex 客户端（或原仓 e2e 的调用方式复刻为 rpc 调用脚本）
#    对 demo-srv 发下单请求 → 断言 200/成功码 + 订单落 order_db（gorm 直查）
# 3. outbox 投递断言：等 outbox 轮询窗口（3s 补发周期）后，用 kafka 消费端断言
#    topic order.completed.v1 收到该订单事件（compose AUTO_CREATE 自动建 topic；
#    工具：sarama 临时消费者小工具或 kafka-console-consumer（本机可用则用之，脚本内探测）
#    断言消息体含下单事件 ID
# 4. 行数断言：[ "$(wc -l < examples/micro/srv/cmd/main.go)" -le 50 ]
# 5. 清理
```
运行：`bash examples/micro/srv/scripts/run-e2e.sh`
预期：下单全绿 + kafka 消息断言 + 行数 ≤50 通过。

- [ ] **步骤 18.2：Commit**

```bash
git add examples/micro/srv && git commit -m "test: srv E3 回归（下单 + outbox→kafka + main.go ≤50 行，3.7）"
```

- [ ] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 3.7 同步）

---

### Task 19: 3.9 降级路径验收（A3 降级/registry 场景）

> **trace:** plan-ready.md → `### Task 22: 降级路径验收（验收案例 A3 降级/registry）` | tasks.md → `- [ ] 3.9 降级验收：\`trace.enabled=false\` 启动运行正常；无 consul 启动带病运行（验收案例 A3 降级路径、registry 场景）`
> **sync:** tasks.md → `- [ ] 3.9 降级验收：\`trace.enabled=false\` 启动运行正常；无 consul 启动带病运行（验收案例 A3 降级路径、registry 场景）` | plan-ready.md → `### Task 22: 降级路径验收（验收案例 A3 降级/registry）`

**文件：**
- 创建：`examples/monolith/degrade_test.go`

- [ ] **步骤 19.1：写三组降级断言（fake 不可达地址，不依赖真底座）**

```go
// TestTraceDisabledStillServes：yaml trace.enabled=false（logx.OTel.Enabled=false 且 traceflag.enabled=false）
//   → Bootstrap 成功、HTTP 自响应正常、Close 正常（noop provider 不设全局）
// TestNoConsulDegradedRun：coordx.addr=127.0.0.1:1（不可达）+ strict.registry 缺省
//   → 启动继续 + Warn（Registered()==false），本地链路可用
// TestStrictDBFailsBootstrap：strict.db=true + mysql 不可达地址（127.0.0.1:1）
//   → Bootstrap 返回含 "init db" 的 error（%w 链 errors.Is 可判定底层错误）
```
（三组用例各一个独立 yaml fixture 目录或 t.Setenv + 临时 config，避免相互污染。）
运行：`go test ./examples/monolith/ -run 'TestTraceDisabledStillServes|TestNoConsulDegradedRun|TestStrictDBFailsBootstrap' -v`
预期：全 PASS。

- [ ] **步骤 19.2：Commit**

```bash
git add examples/monolith/degrade_test.go && git commit -m "test: 降级路径验收（3.9，A3 降级/registry）"
```

- [ ] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 3.9 同步）

---

### Task 20: 两 demo README 与验收案例指引（2.6，本轮范围）

> **trace:** plan-ready.md → `### Task 13: 三 demo README 与验收案例指引` | tasks.md → `- [ ] 2.6 为三个 demo 各补 \`README\`（启动步骤、依赖、验收案例编号指引）`
> **sync:** tasks.md → `- [ ] 2.6 为三个 demo 各补 \`README\`（启动步骤、依赖、验收案例编号指引）` | plan-ready.md → `### Task 13: 三 demo README 与验收案例指引`
> **covers（deep-sync）:** 本轮完成 2.6 中 monolith + micro 两份 README（mesh 随 2.5 后置）；plan-ready Task 13 的「任务完成」待 mesh 回补后勾选，本 Task 勾选只代表本轮部分

**文件：**
- 创建：`examples/monolith/README.md`、`examples/micro/README.md`

- [ ] **步骤 20.1：写两份 README（启动步骤 + 依赖 + 验收案例编号 + 观测方式）**

每份含：前置底座命令（`docker compose -f /Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml up -d consul mysql kafka jaeger redis` + `export HUBX_DB_PW=hubx123`）、逐服务启动步骤（`go run ./cmd`）、依赖清单（端口事实：consul 8500 / mysql 3306 / kafka 9092 / jaeger 4318+16686 / redis 6379）、验收案例编号与预期结果（monolith→E1；micro→E2/E3）、观测方式（span 经 otelx 推 jaeger UI 16686；**无 HTTP 探针端点**，探针语义交 k8s TCP + consul——mesh 后置后 README 同步补 TCP+consul 探针说明，见 close-issues）。
人工核对：每个 README 含「验收案例」小节且编号与 `specs/example-demos/spec.md` Scenario 一致。

- [ ] **步骤 20.2：Commit**

```bash
git add examples/monolith/README.md examples/micro/README.md && git commit -m "docs: monolith/micro demo README（2.6 本轮范围）"
```

- [ ] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 2.6（本轮范围）同步）

---

### Task 21: 后置占位登记（1.8 / 2.5 / 3.8 / 3.11–3.13 / 3.10 后置部分）

> **trace:** plan-ready.md → `### Task 12: examples/mesh 网格清单自包含（launchx 仓内自写）` | tasks.md → `- [ ] 1.8 实现 \`debug\` 伪组件（observability capability）：\`debug.enabled\`（yaml 或 \`APP_DEBUG=1\`）时临时起仅监听 \`127.0.0.1:6060\` 的 debug server（标准 \`net/http/pprof\` handlers），生产默认关；端口冲突 Warn 带病运行；退出序列插入点先于 logger.Close（对应验收案例 O1/O2）`
> **sync:** tasks.md → `- [ ] 1.8 实现 \`debug\` 伪组件（observability capability）：\`debug.enabled\`（yaml 或 \`APP_DEBUG=1\`）时临时起仅监听 \`127.0.0.1:6060\` 的 debug server（标准 \`net/http/pprof\` handlers），生产默认关；端口冲突 Warn 带病运行；退出序列插入点先于 logger.Close（对应验收案例 O1/O2）` | plan-ready.md → `### Task 12: examples/mesh 网格清单自包含（launchx 仓内自写）`
> **covers（deep-sync）:** 占位 Task，完成即勾选 close-issues.md 条目落盘；tasks.md 1.8/2.5/3.8/3.11–3.13 与 plan-ready Task 12/24/25 的「任务完成」保持未勾选（后置项），本 Task 勾选仅表示占位登记完成

**文件：**
- 创建：`openspec/changes/add-launchx-bootstrap/close-issues.md`（sddflow close 阶段消费的文档，非实现文件）

- [ ] **步骤 21.1：写入回补清单（占位，不实现）**

```markdown
# close-issues — 后置项（本轮不验收，回补开新 change）

- [ ] 1.8 debug 伪组件（O1/O2）：spec 依据 specs/observability/spec.md（Requirement「Pprof 走 debug 伪组件」+ Scenario O1/O2）；插入点：internal/component/debug.go（builder+server+Stop，127.0.0.1:6060 仅回环，端口冲突 Warn 带病运行）、internal/pipeline 退出链（先于 logger.Close）、App.Debug 句柄字段
- [ ] 2.5 examples/mesh 清单（C4 部署部分/E4 清单基础）：自写 k8s+Istio 最小清单（namespace / 3 Deployment+Service / sidecar 注入 / bring-up.sh + check 前置检查），MUST NOT copy service_mesh/（清单未交付）；设计结论参考（业务零改动 / 同 trace / 地址段转换 *.mesh-external），README 注明来源
- [ ] 3.8 E4/E5 网格两级验收：(a) `kubectl apply --dry-run=client -f examples/mesh/ -R` + `bring-up.sh check` 全绿；(b) minikube+Istio 真实部署、应用 span 与 sidecar span 同 trace（E4）、`hubx.trace` OFF/ON 对比（E5），证据 `examples/mesh/docs/verify.md`，close 不硬阻塞
- [ ] 3.11 debug 门控单测（O1）：false 不起 server / true 仅回环可达 / 退出先于 logger.Close（事件记录器断言）
- [ ] 3.12 APP_DEBUG=1 srv pprof 可达（O2）：`curl 127.0.0.1:6060/debug/pprof/` 索引页；yaml 置位等效
- [ ] 3.13 mesh 三 Deployment tcpSocket 探针（O3）：liveness/readiness 为 tcpSocket 打业务端口（无 HTTP 端点假设，语义就绪由 consul 承载；Istio sidecar 补充探测），dry-run + bring-up check 全绿
- [ ] 3.10 中涉及后置案例（C4 部署部分 / E4–E5 / O1–O3）的对齐核对（A1–A3、B1、C1–C3 纯代码部分、D1–D2、E1–E3 已随本轮 Task 12–19 核对）
- [ ] B1 真机 nacos 分支：当前 t.Skip 形态，nacos 底座就绪后解除
- [ ] examples/mesh/README.md（随 2.5）
```

- [ ] **步骤 21.2：Commit**

```bash
git add openspec/changes/add-launchx-bootstrap/close-issues.md && git commit -m "docs: 后置项回补清单（1.8/2.5/3.8/3.11-3.13，占位不实现）"
```

- [ ] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；占位登记完成，后置条目本身保持未勾选）

---

### Task 22: 跨仓前置复核（0.1 / 0.2）

> **trace:** plan-ready.md → `### Task 26: 跨仓前置 —— coordx/consul 注册修复落地（对应 \`tasks.md\` 0.1）` / `### Task 27: 跨仓前置 —— 新建 \`hubx/rpcx\` 模块（对应 \`tasks.md\` 0.2）` | tasks.md → `- [ ] 0.1 复核 \`../coordx/consul\` 注册修复**已落地**（实测该仓 \`consul.go\`：Check 仅 \`TTL: "30s"\` 无 \`Interval\`；\`startBeat\`/\`stopBeat\` 俱在，10s \`UpdateTTL\`，\`Deregister\` 与 \`Close\` 停心跳，同 ID 重注册先停旧）——故本条为**复验**而非重新修复；复验：注册 200、passingOnly 发现 1 实例、35s 后 check 仍 passing、注销后归零，\`go build\` / \`go vet\` 于该仓全绿（E2 发现链与 C2/C3 装配的前置）。若复核发现未修复，则按 design.md 决策补齐后再继续`<br>`- [ ] 0.2 新建 \`../rpcx\` 模块（\`github.com/gospacex/rpcx\`）：封装 kitex \`server\` / \`client\` Option 装配（\`BuildServerOptions\` / \`BuildClientOptions\`，实现自 \`micro_apps/pkg/rpcx\` 移植 —— 缺省超时 / 优雅退出排空等待 / obs-opentelemetry 套件挂载点）；服务发现走 \`coordx\`，MUST NOT 引入 \`regdisx\`；套件只从 \`otel.GetTracerProvider()\` 取用，MUST NOT 自行装配 provider；README 注明与 \`micro_apps/pkg/rpcx\`、\`gospacex/rpcx\` 的关系与后续迁移路径`
> **sync:** tasks.md → `- [ ] 0.1 复核 \`../coordx/consul\` 注册修复**已落地**（实测该仓 \`consul.go\`：Check 仅 \`TTL: "30s"\` 无 \`Interval\`；\`startBeat\`/\`stopBeat\` 俱在，10s \`UpdateTTL\`，\`Deregister\` 与 \`Close\` 停心跳，同 ID 重注册先停旧）——故本条为**复验**而非重新修复；复验：注册 200、passingOnly 发现 1 实例、35s 后 check 仍 passing、注销后归零，\`go build\` / \`go vet\` 于该仓全绿（E2 发现链与 C2/C3 装配的前置）。若复核发现未修复，则按 design.md 决策补齐后再继续`<br>`- [ ] 0.2 新建 \`../rpcx\` 模块（\`github.com/gospacex/rpcx\`）：封装 kitex \`server\` / \`client\` Option 装配（\`BuildServerOptions\` / \`BuildClientOptions\`，实现自 \`micro_apps/pkg/rpcx\` 移植 —— 缺省超时 / 优雅退出排空等待 / obs-opentelemetry 套件挂载点）；服务发现走 \`coordx\`，MUST NOT 引入 \`regdisx\`；套件只从 \`otel.GetTracerProvider()\` 取用，MUST NOT 自行装配 provider；README 注明与 \`micro_apps/pkg/rpcx\`、\`gospacex/rpcx\` 的关系与后续迁移路径` | plan-ready.md → `### Task 26: 跨仓前置 —— coordx/consul 注册修复落地（对应 \`tasks.md\` 0.1）` / `### Task 27: 跨仓前置 —— 新建 \`hubx/rpcx\` 模块（对应 \`tasks.md\` 0.2）`

**文件：**
- 跨仓（不在本仓改动）：`/Users/hyx/work/gowork/src/hubx/coordx/consul/consul.go`（0.1 复核）、`/Users/hyx/work/gowork/src/hubx/rpcx/{go.mod,options.go,server.go,client.go,README.md}`（0.2 新建）
- 本仓联动：`go.mod`（`replace github.com/gospacex/rpcx => ../rpcx`）

**说明：** 本 Task 已在 2026-10-05 执行完毕（详见「内容基线」已完成清单），此处登记状态与证据。

- [x] **步骤 22.1：0.1 coordx/consul 注册修复复验** <!-- 已实现: a047afa / 25e3e9b / coordx 零改动 -->

复核项（`Check` 仅 `TTL: "30s"` 无 `Interval`；`startBeat`/`stopBeat` 俱在，10s `UpdateTTL`；`Deregister` 与 `Close` 停心跳，同 ID 重注册先停旧）：
```bash
sed -n '70,100p' /Users/hyx/work/gowork/src/hubx/coordx/consul/consul.go
grep -n 'startBeat\|UpdateTTL' /Users/hyx/work/gowork/src/hubx/coordx/consul/consul.go
cd /Users/hyx/work/gowork/src/hubx/coordx/consul && go build ./... && go vet ./...
```
预期：注册 200、passingOnly 发现 1 实例、35s 后 check 仍 passing、注销后归零，`go build` / `go vet` 全绿。**实测：全部通过，`../coordx` 为零改动（复验非修复）。**

- [x] **步骤 22.2：0.2 新建 `hubx/rpcx` 模块** <!-- 已实现: a047afa / 25e3e9b / coordx 零改动 -->

形态：`BuildServerOptions` / `BuildClientOptions` 封装 kitex `server` / `client` Option（缺省超时：RPC 3s / 建连 500ms / 读写 30s / 退出排空 5s）+ `obs-opentelemetry` 套件挂载点；服务发现走 `coordx`，**MUST NOT** 引入 `regdisx`；套件只从 `otel.GetTracerProvider()` 取用，**MUST NOT** 自行装配 provider。
验证：`cd /Users/hyx/work/gowork/src/hubx/rpcx && go build ./... && go vet ./...` 全绿。**实测：`a047afa` / `25e3e9b`（覆盖率 94.6%）。**

- [x] **步骤 22.3：本仓 go.mod 以 replace 指向本地 `../rpcx`** <!-- 已实现: a047afa / 25e3e9b / coordx 零改动 -->

rpcx 未发布版本，`go.mod` 必须走本地 `replace`（与其余 `*x` SDK 同策略）；**实测已接回（见 `9eb64cd` / `9a43c2d`）**。

- [x] **步骤 22.4：跨仓 Commit 证据登记**

```bash
cd /Users/hyx/work/gowork/src/hubx/rpcx && git log --oneline   # a047afa / 25e3e9b
cd /Users/hyx/work/gowork/src/hubx/coordx && git status --short # 期望：clean（零改动）
```

- [x] **Task complete**（跨仓前置已完成；coordx 零改动，rpcx 落地于 `a047afa` / `25e3e9b`） <!-- 已实现: a047afa / 25e3e9b / coordx 零改动 -->

---

### Task 23: 出向注入工具（1.9，验收案例 T1/T7）

> **trace:** plan-ready.md 无对应 Task（该项为 2026-10-05 追加），跳过同步 | tasks.md → `- [ ] 1.9 导出极小出向注入工具（如 \`trace.Inject(ctx, carrier)\`，仅做 \`otel.GetTextMapPropagator().Inject\`，不含转发逻辑）：因 \`webx/middleware/otel.go\` 只有 Extract 无 Inject、全 hubx 无出向注入 helper；写法对齐已验证的 \`micro_apps/apps/gateway/internal/proxy/proxy.go:134\`；\`examples/micro/gateway\` MUST 调用它完成 gateway → bff 的 HTTP 续链（对应验收案例 T6）`
> **sync:** tasks.md → `- [ ] 1.9 导出极小出向注入工具（如 \`trace.Inject(ctx, carrier)\`，仅做 \`otel.GetTextMapPropagator().Inject\`，不含转发逻辑）：因 \`webx/middleware/otel.go\` 只有 Extract 无 Inject、全 hubx 无出向注入 helper；写法对齐已验证的 \`micro_apps/apps/gateway/internal/proxy/proxy.go:134\`；\`examples/micro/gateway\` MUST 调用它完成 gateway → bff 的 HTTP 续链（对应验收案例 T6）` | plan-ready.md 无对应 Task（该项为 2026-10-05 追加），跳过同步

**文件：**
- 创建：`trace.go`（导出薄封装）、`trace_test.go`

**背景（实测）：** `webx/middleware/otel.go` **只有 Extract 没有 Inject**，全 hubx 无出向注入 helper。gateway 转发 HTTP 到 bff 时若不在出向 Header 注入 `traceparent`，下游 Extract 拿不到上游 context，链路断开。已验证写法：`micro_apps/apps/gateway/internal/proxy/proxy.go:134` 的 `otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(outReq.Header))`。

- [ ] **步骤 23.1：写失败测试（T1 注入 + T7 反向断言）**

```go
// trace_test.go
func TestInjectWritesTraceParent(t *testing.T)      // 注入后 carrier 中存在合法 traceparent（T1）
func TestDefaultPropagatorIsNoop(t *testing.T)      // 不注册传播器时提取不到上游 context（T7）
```
注意：用例必须成对使用 `t.Cleanup` 还原全局 provider / propagator，避免污染其它用例。

- [ ] **步骤 23.2：运行确认失败** — `go test ./... -run 'Inject|DefaultPropagator'`

- [ ] **步骤 23.3：实现 `trace.Inject(ctx, carrier)`**

`trace.go` 只做一件事：`otel.GetTextMapPropagator().Inject(ctx, carrier)`，**不含任何转发逻辑**；carrier 入参类型对齐 `propagation.HeaderCarrier`。链路正确性依赖 Task 6 已注册的 Composite{TraceContext, Baggage}（见「内容基线」红线 4）。

- [ ] **步骤 23.4：`examples/micro/gateway` 调用注入完成 gateway → bff 续链**

转发前 MUST 调用 `trace.Inject(ctx, propagation.HeaderCarrier(outReq.Header))`；缺此步则 T6 的三段同 trace 断言无法通过。

- [ ] **步骤 23.5：Commit**

```bash
git add trace.go trace_test.go examples/micro/gateway && git commit -m "feat(launchx): 出向注入工具 trace.Inject（T1/T7，1.9）"
```

- [ ] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 tasks.md 1.9 同步；plan-ready.md 无对应 Task）

---

### Task 23b: Jaeger 端到端调用链验收（4.1–4.8，验收案例 T1–T9）

> **trace:** plan-ready.md 无对应 Task（该项为 2026-10-05 追加），跳过同步 | tasks.md → `- [ ] 4.1 单测：telemetry 步骤后 \`otel.GetTextMapPropagator().Fields()\` 含 \`traceparent\`/\`tracestate\`/\`baggage\`，且注入后 carrier 中存在合法 \`traceparent\`（验收案例 T1）；反向断言——不跑 telemetry 步骤时提取不到上游 span context（验收案例 T7）`<br>`- [ ] 4.2 集成：\`examples/monolith\` 落库接口在 Jaeger 中形成 span 树深度 ≥ 3（入口 → 业务层 → 数据层），数据层 span 祖先链含入口 span，无孤儿（验收案例 T5）`<br>`- [ ] 4.3 集成：数据库**单表** CRUD 四类 span 均带非空 \`db.query.text\`（完整 SQL）与 \`db.sql.table\`（验收案例 T2）`<br>`- [ ] 4.4 集成：数据库**多表**操作入链——\`JOIN\`（或 \`Preload\`）查询的 \`db.query.text\` 含关联文本；跨表事务内多条 SQL 为同 trace 的兄弟 span；回滚时相关 span 标 Error（验收案例 T3）`<br>`- [ ] 4.5 集成：同一请求内 cache / mq / \`db.elasticsearch.*\` / \`db.mongodb.*\` 四类 span 共存于同一 trace，均为业务 span 子节点（验收案例 T4）`<br>`- [ ] 4.6 集成：\`gateway → bff → srv\` 三段 span 共享同一 trace-id 且父子链正确（非三个并列根），srv 侧数据层 span 亦在同一 trace（验收案例 T6）`<br>`- [ ] 4.7 集成：Jaeger API \`http://127.0.0.1:16686/api/traces?service=<service.name>\` 可检索到上述 trace，且尾部 span 不缺失（证明退出冲刷生效）（验收案例 T9）`<br>`- [ ] 4.8 代码断言：全仓无网格专属 trace 分支，\`mode\` 枚举无 \`mesh\` 取值；\`examples/mesh\` 与 \`examples/micro\` 三服务 main.go 差异仅在配置路径与地址副本（验收案例 T8）`
> **sync:** tasks.md → `- [ ] 4.1 单测：telemetry 步骤后 \`otel.GetTextMapPropagator().Fields()\` 含 \`traceparent\`/\`tracestate\`/\`baggage\`，且注入后 carrier 中存在合法 \`traceparent\`（验收案例 T1）；反向断言——不跑 telemetry 步骤时提取不到上游 span context（验收案例 T7）`<br>`- [ ] 4.2 集成：\`examples/monolith\` 落库接口在 Jaeger 中形成 span 树深度 ≥ 3（入口 → 业务层 → 数据层），数据层 span 祖先链含入口 span，无孤儿（验收案例 T5）`<br>`- [ ] 4.3 集成：数据库**单表** CRUD 四类 span 均带非空 \`db.query.text\`（完整 SQL）与 \`db.sql.table\`（验收案例 T2）`<br>`- [ ] 4.4 集成：数据库**多表**操作入链——\`JOIN\`（或 \`Preload\`）查询的 \`db.query.text\` 含关联文本；跨表事务内多条 SQL 为同 trace 的兄弟 span；回滚时相关 span 标 Error（验收案例 T3）`<br>`- [ ] 4.5 集成：同一请求内 cache / mq / \`db.elasticsearch.*\` / \`db.mongodb.*\` 四类 span 共存于同一 trace，均为业务 span 子节点（验收案例 T4）`<br>`- [ ] 4.6 集成：\`gateway → bff → srv\` 三段 span 共享同一 trace-id 且父子链正确（非三个并列根），srv 侧数据层 span 亦在同一 trace（验收案例 T6）`<br>`- [ ] 4.7 集成：Jaeger API \`http://127.0.0.1:16686/api/traces?service=<service.name>\` 可检索到上述 trace，且尾部 span 不缺失（证明退出冲刷生效）（验收案例 T9）`<br>`- [ ] 4.8 代码断言：全仓无网格专属 trace 分支，\`mode\` 枚举无 \`mesh\` 取值；\`examples/mesh\` 与 \`examples/micro\` 三服务 main.go 差异仅在配置路径与地址副本（验收案例 T8）` | plan-ready.md 无对应 Task（该项为 2026-10-05 追加），跳过同步

**前置（硬）：** 「内容基线」环境事实第 2 条的共享底座就绪 —— jaeger `16686`/`4317`/`4318`、mysql `3306`、kafka `9092`、elasticsearch `9200`（**注意：ES 不在 `hubx/dockers` compose 内，当前由独立容器提供**）。

**文件：**
- 创建：`examples/monolith/trace_e2e_test.go`、`examples/micro/trace_e2e_test.go`、`internal/traceutil/`（Jaeger API 检索助手）

- [ ] **步骤 23b.1：T1 传播器字段与注入断言 / T7 反向断言（4.1）**

断言 telemetry 步骤后 `otel.GetTextMapPropagator().Fields()` 含 `traceparent` / `tracestate` / `baggage`，且注入后 carrier 中存在合法 `traceparent`；反向断言 —— 不跑 telemetry 步骤时提取不到上游 span context。

- [ ] **步骤 23b.2：T5 monolith span 树深度 ≥ 3（4.2）**

落库接口的一条 trace 中，span 树须为「入口 → 业务层 → 数据层」，数据层 span 的祖先链含入口 span，**无孤儿**。

- [ ] **步骤 23b.3：T2 单表 CRUD 四类 span（4.3）**

`db.query.text`（完整 SQL）与 `db.sql.table` 均非空；单表四类操作（INSERT / SELECT / UPDATE / DELETE）逐一断言。

- [ ] **步骤 23b.4：T3 多表操作入链（4.4）**

`JOIN`（或 `Preload`）查询的 `db.query.text` 含关联文本；跨表事务内多条 SQL 为同 trace 的兄弟 span；回滚时相关 span 标 Error。

- [ ] **步骤 23b.5：T4 四类组件同 trace（4.5）**

同一请求内依次操作 cache / mq / `db.elasticsearch.*` / `db.mongodb.*`，断言四类 span 共存于同一 trace 且均为业务 span 的子节点（依托 Task 4 的六类组件超集实现）。

- [ ] **步骤 23b.6：T6 gateway → bff → srv 三段同 trace（4.6）**

**这是最容易假通过的断言**：不注册传播器时三服务各产一根 trace，每条都能点开、都有 span，看起来完全正常。必须断言 **traceID 相等 + 父子关系**（gateway → bff → srv，而不是三个并列根），而非「各服务都能查到 trace」。

- [ ] **步骤 23b.7：T9 尾部 span 不缺失（4.7）**

经 `traceutil` 检索 Jaeger API `http://127.0.0.1:16686/api/traces?service=<service.name>`，断言 span 数量符合预期且尾部节点不缺失 —— 证明退出链 `tracing.shutdown(15s)` 冲刷生效（批量导出器未冲刷会静默丢最后一个 span）。

- [ ] **步骤 23b.8：T8 网格同构代码断言（4.8）**

全仓无网格专属 trace 分支；`mode` 枚举无 `mesh` 取值；`examples/mesh` 与 `examples/micro` 三服务 main.go 差异仅在配置路径与地址副本。

- [ ] **步骤 23b.9：Commit**

```bash
git add examples/monolith/trace_e2e_test.go examples/micro/trace_e2e_test.go internal/traceutil/ trace_test.go && git commit -m "test: Jaeger 端到端调用链验收 T1-T9（4.1-4.8）"
```

- [ ] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 tasks.md 4.1–4.8 同步；plan-ready.md 无对应 Task）

---

### Task 24: 依赖面核对（3.14）

> **trace:** plan-ready.md → `### Task 28: 依赖面核对（对应 \`tasks.md\` 3.14）` | tasks.md → `- [ ] 3.14 依赖面核对（mode-assembly「RPC 依赖面收敛」）：launchx 库源码（含 \`internal/*\`）零 \`cloudwego/*\` import，go.mod 中 kitex 仅 \`// indirect\`；\`examples/\` 中仅 \`micro/srv\` 与 \`micro/bff\` 的 kitex 构造处出现 kitex import（\`kitex_gen\` 所需），且全仓无 \`cloudwego/hertz\`、\`redis/go-redis\`、\`segmentio/kafka-go\`、\`hashicorp/consul/api\`、\`gorm.io/gorm\` 的直接引用`
> **sync:** tasks.md → `- [ ] 3.14 依赖面核对（mode-assembly「RPC 依赖面收敛」）：launchx 库源码（含 \`internal/*\`）零 \`cloudwego/*\` import，go.mod 中 kitex 仅 \`// indirect\`；\`examples/\` 中仅 \`micro/srv\` 与 \`micro/bff\` 的 kitex 构造处出现 kitex import（\`kitex_gen\` 所需），且全仓无 \`cloudwego/hertz\`、\`redis/go-redis\`、\`segmentio/kafka-go\`、\`hashicorp/consul/api\`、\`gorm.io/gorm\` 的直接引用` | plan-ready.md → `### Task 28: 依赖面核对（对应 \`tasks.md\` 3.14）`

**说明：** 纯核对动作，不合格则修正 import 与 `go.mod`；用于坐实 mode-assembly 的「RPC 依赖面收敛」。

- [ ] **步骤 24.1：库源码零 cloudwego 直接 import**

```bash
grep -rn "cloudwego" --include="*.go" . | grep -v "^./examples/"    # 期望：空
```
含 `internal/*` 全量；期望无命中。

- [ ] **步骤 24.2：`go.mod` 中 kitex 仅 `// indirect`**

检查 `go.mod`：`github.com/cloudwego/kitex` 只能出现在 indirect 区；RPC 相关 import 只指向 `github.com/gospacex/rpcx`（前置 Task 22）。

- [ ] **步骤 24.3：`examples/` 白名单核对**

仅 `micro/srv` 与 `micro/bff` 的 kitex 构造处允许出现 kitex import（`kitex_gen` 所需）；全仓无 `cloudwego/hertz`、`redis/go-redis`、`segmentio/kafka-go`、`hashicorp/consul/api`、`gorm.io/gorm` 的直接引用。

- [ ] **步骤 24.4：Commit（仅在有修正时）**

```bash
go list -deps ./...    # 依赖面清单核对
git add -A && git commit -m "chore: 依赖面核对（mode-assembly RPC 依赖面收敛，3.14）"
```

- [ ] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 3.14 同步）

---

### Task 25: 网格形态端到端链路验收（4.9–4.11，验收案例 T10–T12）

> **trace:** plan-ready.md → `### Task 21: 网格部署验收（验收案例 E4/E5，两级）`（**关联项，非同名**：T10–T12 即其 (b) 级人工验证的链路断言部分，证据文件同为 `examples/mesh/docs/verify.md`） | tasks.md → `- [ ] 4.9 集成（网格）：minikube + Istio 就绪、sidecar 追踪 100% 采样并绑定 OTLP provider 后，触发 \`gateway → bff → srv\` 全链路，Jaeger 同一 trace 下同时存在应用 span（\`gateway\`/\`bff\`/\`srv\`）与 sidecar span（\`<workload>.<ns>\`），且应用 span 父子链仍为 gateway → bff → srv、srv 数据层 span 同 trace（验收案例 T10）`<br>`- [ ] 4.10 反向断言（网格）：仅开 sidecar 追踪、应用侧不注册传播器时，Jaeger 呈现「1 条 sidecar 链 + 应用侧各自孤儿 trace」的分裂形态（trace 数 ≥ 4）（验收案例 T11）`<br>`- [ ] 4.11 证据落盘（网格）：\`examples/mesh/docs/verify.md\` 记录 trace-id / span 数（应用 vs sidecar）/ sidecar 服务名 / Jaeger API 原始响应片段 / \`hubx.trace\` OFF-ON 对比 / minikube-kubectl-istioctl-Istio 版本（验收案例 T12）`
> **sync:** tasks.md → `- [ ] 4.9 集成（网格）：minikube + Istio 就绪、sidecar 追踪 100% 采样并绑定 OTLP provider 后，触发 \`gateway → bff → srv\` 全链路，Jaeger 同一 trace 下同时存在应用 span（\`gateway\`/\`bff\`/\`srv\`）与 sidecar span（\`<workload>.<ns>\`），且应用 span 父子链仍为 gateway → bff → srv、srv 数据层 span 同 trace（验收案例 T10）`<br>`- [ ] 4.10 反向断言（网格）：仅开 sidecar 追踪、应用侧不注册传播器时，Jaeger 呈现「1 条 sidecar 链 + 应用侧各自孤儿 trace」的分裂形态（trace 数 ≥ 4）（验收案例 T11）`<br>`- [ ] 4.11 证据落盘（网格）：\`examples/mesh/docs/verify.md\` 记录 trace-id / span 数（应用 vs sidecar）/ sidecar 服务名 / Jaeger API 原始响应片段 / \`hubx.trace\` OFF-ON 对比 / minikube-kubectl-istioctl-Istio 版本（验收案例 T12）` | plan-ready.md → `### Task 21: 网格部署验收（验收案例 E4/E5，两级）`（关联项，非同名）

**前置（2026-10-05 实测更正 —— 工具已装，只需起集群）：**

| 项 | 实测 | 说明 |
|---|---|---|
| minikube | ✅ `v1.36.0` | `/usr/local/bin/minikube` |
| istioctl | ✅ `1.30.1` | `/usr/local/bin/istioctl` |
| kubectl | ❌ 未单独安装 | 用 `minikube kubectl --`（`~/.minikube/bin` 下无 kubectl 分发） |
| 集群 | ❌ 未启动 | `minikube status` 报 `No such container: minikube` |
| ⚠️ PATH | `/usr/local/bin` **不在** WorkBuddy shell 的默认 PATH | `which minikube` 会**误报未安装**——本文件此前「均未安装」的结论即由此误判，已更正为「已装、未起集群」 |
```bash
export PATH=/usr/local/bin:$PATH           # 必须：否则 minikube / istioctl 不可见
minikube start --driver=docker --cpus=2 --memory=4096     # 宿主 docker 总内存仅 8GB
istioctl install --set profile=demo -y
kubectl label namespace launchx-mesh istio-injection=enabled --overwrite
minikube image load launchx-examples/gateway:dev          # 三服务镜像逐一 load
```
采样须 100%，Istio 侧 OTLP provider 指向本机 Jaeger（集群内经 **`host.minikube.internal:4318`**；`host.docker.internal` 在 minikube 的 k8s 内不保证解析）。

**待真机核实（不得凭记忆定稿）：** Istio 侧追踪 provider 必须选 **`opentelemetry`** —— Envoy OTel tracer 走 W3C `traceparent`，与 launchx 的 Composite{TraceContext, Baggage} 对齐；若选 `zipkin`，默认走 **B3**（`x-b3-traceid`）会与 W3C 头不互通而断链。落地前须以 `istioctl proxy-config bootstrap <pod> -n launchx-mesh -o json` 查 tracing 配置，并对 demo 发一次请求抓 sidecar 实际写出的头名确认。

**文件：**
- 依赖：`examples/mesh/`（2.5 清单后置，见 Task 21）、`examples/mesh/scripts/bring-up.sh`、`examples/mesh/docs/verify.md`

- [ ] **步骤 25.1：环境前置检查脚本（缺失即非零退出）**

`bring-up.sh check` 须显式检查 `minikube` / `kubectl` / `istioctl` 是否存在、`minikube status` 是否 Running、istio-system 是否就绪、宿主中间件端口（consul 8500 / mysql 3306 / kafka 9092 / jaeger 16686）是否可达；缺失时**打印缺失项并以非零码退出**，不得静默通过。

- [ ] **步骤 25.2：传播头对齐实测（决定 provider 选型）**

按上文「待真机核实」执行，结论写入 `examples/mesh/docs/verify.md` 的「环境与配置」段。

- [ ] **步骤 25.3：T10 — 应用 span 与 sidecar span 同 trace**

经 Ingress Gateway 触发 `gateway → bff → srv`；用 `http://127.0.0.1:16686/api/traces?service=gateway` 与 `?service=gateway.<ns>` 双向检索，断言：同一 trace-id；应用 span 父子链仍为 gateway → bff → srv；srv 数据层 span 同 trace。

- [ ] **步骤 25.4：T11 — 网格下「半断链」反向断言**

以不注册传播器的临时构建（仅实验，不入主干）跑一次请求，断言 Jaeger 出现「1 条 sidecar 完整链 + 应用侧各自孤儿 trace」（trace 数 ≥ 4），记录为反例证据。

- [ ] **步骤 25.5：T12 — 证据落盘**

`examples/mesh/docs/verify.md` 记录：trace-id、span 总数、应用 span 数 / sidecar span 数、sidecar 服务名、Jaeger API 原始响应片段、`hubx.trace` OFF/ON 对比、minikube / kubectl / istioctl / Istio 版本。

- [ ] **步骤 25.6：Commit**

```bash
git add examples/mesh/ && git commit -m "test(mesh): 网格形态端到端链路验收 T10-T12（4.9-4.11）与证据落盘"
```

- [ ] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 tasks.md 4.9–4.11 同步）

---

## 自检（writing-plans Self-Review）

1. **规格覆盖**：1.1→T1、1.2→T2、1.3→T3、1.4→T4、1.5→T5、1.6→T6、1.7→T7、2.1→T8、2.2→T9、2.3→T10、2.4→T11、3.1→T12、3.2→T13、3.3→T14、3.4→T15、3.5→T16、3.6→T17、3.7→T18、3.9→T19、2.6→T20；1.8/2.5/3.8/3.11–3.13/3.10(后置部分)→T21 占位。无遗漏。
2. **占位符扫描**：唯一有意占位为 T21（后置项，spec 明确本轮不做，正文仅锁定插入点与规格引用，无 TBD/TODO 型实现占位）；T1 步骤 1.1.4 给出 import 循环的两种解法，非占位。
3. **类型一致性**：`App` 字段名（DB/Cache/MQ/Registry/Server/RPCServer）在 T1/T4/T6 一致；`Registry.Target(agentAddr, serviceID)` 签名在 T4（移植自 micro_apps/infra/consul/registry.go:148）与 T5/T9/T10 一致；`RPCClientOptions(service)` 工厂 T5 产出、T10 消费；事件名（telemetry.init/component.*/registry.deregister/tracing.flush/logger.close）在 T6/T7/T13 一致。
