# add-mesh-native-apps — 实现计划

**Goal:** 把 `examples/mesh` 从「纯清单目录」推进到「网格原生应用 + 环境就绪判定 + 网格入链验证」：
在 `examples/mesh/apps` 落地 5 个以 `launchx.Bootstrap` 为底座、经 `App` 句柄访问中间件的进程
（`bff` + 4×`srv-*`，无 gateway），配齐 `deploy/apps` 的 k8s + Istio 清单，并拿到 T13 的
「应用 span 与 sidecar span 同 trace」证据 —— 证据形态为**路由矩阵**：
入口六条路由 × 各自目标域的业务层 span × sidecar 跳，外加 `order → MQ → points` 异步链一条。

> **Amend 追加（2026-10-07）**：本文档经 `/sddflow amend` 修订。新增内容为
> `Task 17`（判据矩阵化）与对 `Task 7` / `Task 15` 的就地扩写；来源见
> `plan-ready.md` 的 `## Amendments` 与 `proposal.md` 的 `## Amendments`。
> **执行顺序：Task 17 → Task 15**（Task 15 的矩阵判据依赖 Task 17 产出的脚本）。

**Architecture:**
- 入口：Istio `Gateway` + `VirtualService`（`match.uri.prefix: /api` → `host: bff`，`rewrite.uri: /`），
  不写应用侧 gateway 进程 —— micro 的 562 行入口代码整块被它取代。
  **入口路由数保持 1 条**，四域可达性由 bff 的 6 条应用路由承载（两个数量刻意分开）。
- 服务发现：不配 `xmiddleware.coordx`，完全交 K8s Service + Istio EDS；应用侧零注册代码。
- 中间件：仍来自宿主 `hubx/dockers` compose，经 `host-middleware.launchx-mesh.svc.cluster.local`
  门面接入（无 selector 的 Service + 手工 Endpoints）。
- 应用侧只保留网格接不住的部分：RPC 跳的限流（`internal/srv/*/middleware`）、业务级鉴权、
  幂等/事务/outbox。

**Tech Stack:** Go（`go` 指令与仓库根 `go.mod` 一致，当前为 `1.27.1`）、
`github.com/gospacex/launchx`（本地 replace）、`hubx/rpcx` + `kitex@v0.16.3`（RPC 与 IDL）、
`hubx/webx`（HTTP 引擎）、`gorm`（经 `App.DB()` 断言取用）、k8s v1.28.0（minikube）+ Istio 1.30.1。

**Traceability (sddflow):**
- plan-ready: `openspec/changes/add-mesh-native-apps/plan-ready.md`
- tasks: `openspec/changes/add-mesh-native-apps/tasks.md`
- plan: `docs/superpowers/plans/2026-10-07-add-mesh-native-apps.md`

## 计划粒度约定（先看清，再执行）

四域**结构完全同构**，所以本计划不把同一份代码抄四遍：

- **Task 1 / 2 / 3 / 7 / 11 / 12 / 13** 给出可直接落盘的**完整文件内容**；
- **Task 4 / 5 / 6** 是 Task 3 的机械移植，每个 Task 的 Step 1 给出**逐文件差异表**
  （文件 / 与 order 的差异 / 校验命令），差异项逐条列全，无 TBD；
- **Task 8 / 9 / 10 / 14 / 15 / 16 / 17** 是入口、配置与验证类 Task，给出**完整命令与期望输出**。

这条约定是刻意的：把 4 份 ≈300 行的同构代码全部抄进计划，只会让计划变成不可校对的噪声。
若执行时发现某域与 order 的差异超出差异表，**必须停下并回填差异表**，不得就地发挥。

**执行顺序的硬约束**：Task 1 → 2 → 3 是地基；3 完成后 4/5/6 可并行；7 依赖 1；
8 依赖 3/4/5/6/7；9/10 是本地可跑的判据；11 → 12 → 13（清单）→ 14（部署）→ 15（验证）→ 16（文案）。
**Task 17 是 amend 追加项，必须排在 Task 15 之前执行**（15 的矩阵判据用的就是 17 产出的脚本）。

---

### Task 1: apps module 公共件与 IDL 产物

> **trace:** plan-ready.md → `### Task 1: apps module 公共件与 IDL 产物` | tasks.md → `- [ ] 1.1 建 `examples/mesh/apps` 的 module 公共件：`go.mod`（`module github.com/gospacex/launchx/examples/mesh/apps` + `replace github.com/gospacex/launchx => ../../..`，go 指令与仓库根一致）、`pkg/codes`（业务错误码）、`pkg/events`（领域事件载荷，outbox 用）、`pkg/routing`（HTTP 边界中间件链骨架：audit / auth / rate_limit 三层 span 嵌套）、`pkg/kitex_gen/{order,points,product,stock}`（IDL 生成产物，从 `examples/micro/pkg/kitex_gen` 只读搬运生成代码，MUST NOT 搬运 `internal/**`）。验收：`cd examples/mesh/apps && go build ./...` 通过`
> **sync:** tasks.md → `- [ ] 1.1 建 `examples/mesh/apps` 的 module 公共件：`go.mod`（`module github.com/gospacex/launchx/examples/mesh/apps` + `replace github.com/gospacex/launchx => ../../..`，go 指令与仓库根一致）、`pkg/codes`（业务错误码）、`pkg/events`（领域事件载荷，outbox 用）、`pkg/routing`（HTTP 边界中间件链骨架：audit / auth / rate_limit 三层 span 嵌套）、`pkg/kitex_gen/{order,points,product,stock}`（IDL 生成产物，从 `examples/micro/pkg/kitex_gen` 只读搬运生成代码，MUST NOT 搬运 `internal/**`）。验收：`cd examples/mesh/apps && go build ./...` 通过` | plan-ready.md → `### Task 1: apps module 公共件与 IDL 产物`

**Files:**
- Create: `examples/mesh/apps/go.mod`
- Create: `examples/mesh/apps/pkg/codes/codes.go`
- Create: `examples/mesh/apps/pkg/events/events.go`
- Create: `examples/mesh/apps/pkg/routing/routing.go`
- Copy (read-only): `examples/micro/pkg/kitex_gen/{order,points,product,stock}/**` → `examples/mesh/apps/pkg/kitex_gen/`

- [x] **Step 1: 写 go.mod**

`replace` 段必须**整体**从仓库根 `go.mod` 搬过来（不能只写 `launchx` 一条）：`launchx` 依赖的
`cachex` / `coordx` / `mqx` / `dbx` / `rpcx` 等模块各自的 `replace` 会被主模块忽略，缺一条
MVS 图就闭不了环。路径前缀从根 go.mod 的 `../` 改为 `../../../../`（`apps` 在
`examples/mesh/apps`，到 `hubx/` 是四级）。

`examples/mesh/apps/go.mod`：

```go
module github.com/gospacex/launchx/examples/mesh/apps

go 1.27.1

require (
	github.com/cloudwego/kitex v0.16.3
	github.com/gospacex/launchx v0.0.0
	github.com/gospacex/logx v0.0.0
	github.com/gospacex/webx v0.0.0-00010101000000-000000000000
	gorm.io/gorm v1.25.12
)

// 以下 replace 段与仓库根 go.mod 一字不差，仅把 `../` 前缀换成 `../../../../`。
// 不要只手写 launchx 一条：依赖模块自身的 replace 会被主模块忽略。
replace (
	github.com/gospacex/cachex => ../../../../cachex
	github.com/gospacex/cachex/memcached => ../../../../cachex/memcached
	github.com/gospacex/cachex/redis => ../../../../cachex/redis
	github.com/gospacex/configx => ../../../../configx
	github.com/gospacex/coordx => ../../../../coordx
	github.com/gospacex/coordx/apollo => ../../../../coordx/apollo
	github.com/gospacex/coordx/consul => ../../../../coordx/consul
	github.com/gospacex/coordx/etcd => ../../../../coordx/etcd
	github.com/gospacex/coordx/k8s_configmap => ../../../../coordx/k8s_configmap
	github.com/gospacex/coordx/nacos => ../../../../coordx/nacos
	github.com/gospacex/dbx => ../../../../dbx
	github.com/gospacex/kvx => ../../../../kvx
	github.com/gospacex/kvx/redis => ../../../../kvx/redis
	github.com/gospacex/launchx => ../../..
	github.com/gospacex/lockx => ../../../../lockx
	github.com/gospacex/lockx/redis => ../../../../lockx/redis
	github.com/gospacex/logx => ../../../../logx
	github.com/gospacex/mqx => ../../../../mqx
	github.com/gospacex/mqx/kafka => ../../../../mqx/kafka
	github.com/gospacex/mqx/nsq => ../../../../mqx/nsq
	github.com/gospacex/mqx/pulsar => ../../../../mqx/pulsar
	github.com/gospacex/mqx/rabbitmq => ../../../../mqx/rabbitmq
	github.com/gospacex/mqx/rocketmq => ../../../../mqx/rocketmq
	github.com/gospacex/otelx => ../../../../otelx
	github.com/gospacex/rpcx => ../../../../rpcx
	github.com/gospacex/searchx => ../../../../searchx
	github.com/gospacex/searchx/elasticsearch => ../../../../searchx/elasticsearch
	github.com/gospacex/mongodbx => ../../../../mongodbx
	github.com/gospacex/hubx => ../../../../hubx
	github.com/gospacex/traceflag => ../../../../traceflag
	github.com/gospacex/webx => ../../../../webx
)
```

Run: `cd examples/mesh/apps && go mod tidy`
Expected: 退出码 0；`go.mod` 被补上 `require` / `// indirect` 若干行，`go.sum` 生成。
`go 1.27.1` 必须与仓库根一致 —— 写小了会在编译 `launchx` 时报 `requires go >= 1.27.1`。

- [x] **Step 2: 搬运四域 IDL 生成产物**

只搬 `kitex_gen`（生成代码），**不搬** `examples/micro/pkg/**/internal/**`（历史教训：拷贝
micro 快照反向 import 其 `internal` 会直接违反 Go internal 规则，让 `go build ./...` 挂掉）。

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx/examples
for d in order points product stock; do
  mkdir -p mesh/apps/pkg/kitex_gen
  cp -R micro/pkg/kitex_gen/$d mesh/apps/pkg/kitex_gen/$d
done
grep -rn "examples/micro" mesh/apps/pkg/kitex_gen/ | head
```
Expected: `grep` 零命中（生成产物里没有指向 micro 的 import）。
若有命中，逐条改成 `examples/mesh/apps` 的路径 —— 生成代码的 import path 取决于生成时的
module，不能靠 `sed` 批量改，必须逐条确认。

- [x] **Step 3: 写 pkg/codes/codes.go**

`examples/mesh/apps/pkg/codes/codes.go`：

```go
// Package codes 业务错误码：全局段 + 四域各自的段，避免跨域撞码。
//
// 与 examples/micro/pkg/codes 的差别：本包**不带** HTTP 状态映射（那是 bff 边界的事），
// 只提供码值与带 cause 的错误包装，让它在 srv 侧也能用。
package codes

import (
	"errors"
	"fmt"
)

// Code 业务错误码。四域段位：
//
//	1xxxx 通用 / 2xxxx order / 3xxxx points / 4xxxx product / 5xxxx stock
type Code int32

const (
	OK Code = 0

	InvalidArgument Code = 10001
	Internal        Code = 10500

	OrderNotFound   Code = 20404
	OrderCreateFail Code = 20500
	PointsNotEnough Code = 30400
	ProductNotFound Code = 40404
	StockNotEnough  Code = 50300
)

// String 便于日志与错误串里直接读值。
func (c Code) String() string { return fmt.Sprintf("code=%d", int32(c)) }

// Error 业务错误：码 + 人读信息 + 可选根因。
//
// cause 单独存放而不是拼进 msg：errors.Is/As 要能穿到根因（例如
// gorm.ErrRecordNotFound），拼成字符串就把这条链掐断了。
type Error struct {
	Code  Code
	Msg   string
	Cause error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("[%d] %s: %v", int32(e.Code), e.Msg, e.Cause)
	}
	return fmt.Sprintf("[%d] %s", int32(e.Code), e.Msg)
}

// Unwrap 让 errors.Is/As 能穿到 Cause。
func (e *Error) Unwrap() error { return e.Cause }

// New 构造业务错误。
func New(c Code, msg string) *Error { return &Error{Code: c, Msg: msg} }

// Wrap 在业务错误上挂根因。
func Wrap(c Code, msg string, cause error) *Error {
	return &Error{Code: c, Msg: msg, Cause: cause}
}

// From 从任意 error 提取业务码；非业务错误一律 Internal。
func From(err error) (Code, string) {
	if err == nil {
		return OK, ""
	}
	var be *Error
	if errors.As(err, &be) {
		return be.Code, be.Msg
	}
	return Internal, err.Error()
}
```

Run: `cd examples/mesh/apps && gofmt -l pkg/codes && go build ./pkg/...`
Expected: `gofmt` 无输出（干净），`go build` 退出码 0。

- [x] **Step 4: 写 pkg/events/events.go**

`examples/mesh/apps/pkg/events/events.go`：

```go
// Package events 领域事件载荷：跨进程只有这些结构，MQ 的 topic 常量也在这里。
//
// 为什么单独立包而不是塞进各域的 service：投递侧（order）与消费侧（points）
// 必须共用同一份结构与 topic 名，否则「生产者写 v1、消费者订阅 v2」这类
// 错配会以「消息发了没人收」的形式静默存在。
package events

import (
	"encoding/json"
	"errors"
	"time"
)

// TopicOrderCompleted 订单完成事件。生产方 srv-order，消费方 srv-points。
//
// 键位口径（mqx/kafka 现状，实测）：producer 的目标 topic 读的是
// `consumer.topic`，配 `producer.topic` 不生效（FillDefaults 只把该键填给 writer），
// 发送时会因 topic 为空报 Unknown Topic Or Partition。故配置侧对齐该键。
const TopicOrderCompleted = "order.completed.v1"

// OrderCompleted 订单完成。
type OrderCompleted struct {
	OrderID     uint64    `json:"order_id"`
	UserID      uint64    `json:"user_id"`
	TotalAmount int64     `json:"total_amount"`
	OccurredAt  time.Time `json:"occurred_at"`
}

// Marshal 序列化；失败即返回 error，由调用方决定重试还是落 outbox。
func (e OrderCompleted) Marshal() ([]byte, error) { return json.Marshal(e) }

// UnmarshalOrderCompleted 反序列化；OrderID 为零即报错，避免消费侧
// 拿一个 OrderID=0 的事件去加积分（那会写出一条永远对不上账的流水）。
func UnmarshalOrderCompleted(b []byte) (OrderCompleted, error) {
	var e OrderCompleted
	if err := json.Unmarshal(b, &e); err != nil {
		return OrderCompleted{}, err
	}
	if e.OrderID == 0 {
		return OrderCompleted{}, errors.New("events: order_id is zero")
	}
	return e, nil
}
```

Run: `cd examples/mesh/apps && gofmt -l pkg/events && go build ./pkg/...`
Expected: `gofmt` 无输出，`go build` 退出码 0。

- [x] **Step 5: 写 pkg/routing/routing.go**

`examples/mesh/apps/pkg/routing/routing.go`——**只保留链骨架与身份载体**（apps 相对 micro 的
明确减法：micro 的 `pkg/auth` 195 行 + `pkg/routing` 338 行里，只有「中间件链的顺序与
span 嵌套」是网格下仍需自持的部分，验签与票据校验交网格）：

```go
// Package routing HTTP 边界中间件链骨架：audit → identity → rate_limit 三层，
// 顺序固定且每层自带 span，便于在 Jaeger 里一眼看出请求停在哪一层。
//
// 与 examples/micro/pkg/routing 的差别（有意为之的减法）：
//   * 不承载 token 签发 / 缓存反验（那是 micro 的 pkg/auth 195 行）；
//   * 只做「从请求头取身份 → 放进 ctx → 打一条 span」，验签与票据时效
//     交网格的 RequestAuthentication + AuthorizationPolicy（mTLS 身份）。
package routing

import (
	"context"

	"go.opentelemetry.io/otel"

	"github.com/gospacex/webx"
)

// Identity 请求主体。取不到时为零值，由 handler 决定放行还是拒绝。
type Identity struct {
	UserID     uint64
	OperatorID uint64
}

type ctxKey struct{}

// WithIdentity 把身份放进 ctx。
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// IdentityFrom 从 ctx 取身份（取不到返回零值，不 panic）。
func IdentityFrom(ctx context.Context) Identity {
	v, _ := ctx.Value(ctxKey{}).(Identity)
	return v
}

// Chain 组装边界中间件链：audit → identity → rate_limit。
//
// 顺序不可调换：audit 最先（它要记录原始请求，晚于任何一层都会丢字段），
// identity 居中（rate_limit 需要按主体计数），rate_limit 最后
//（它是最便宜的一道拒绝，放在身份之后才能按人限流）。
func Chain() []webx.Middleware {
	return []webx.Middleware{
		passThrough("routing.audit"),
		passThrough("routing.identity"),
		passThrough("routing.rate_limit"),
	}
}

// passThrough 产出一个只记 span 不干预请求的中间件。
//
// 签名以 hubx/webx 的 Middleware 为准（见 Step 6 的核对命令，别凭记忆写 ——
// hertz 的 handler 签名与 webx 的中间件签名不是一回事）。
func passThrough(name string) webx.Middleware {
	return func(next webx.HandlerFunc) webx.HandlerFunc {
		return func(c context.Context, ctx *webx.RequestContext) {
			ctx2, span := otel.Tracer("launchx.mesh.apps").Start(c, name)
			defer span.End()
			next(ctx2, ctx)
		}
	}
}
```

Run: `cd examples/mesh/apps && gofmt -l pkg/routing`
Expected: 无输出（格式干净）。**此处只验格式，不验编译** —— 上面 `passThrough` 里的
`webx.Middleware` / `webx.HandlerFunc` 是否是本仓 webx 的真实签名，由下一步统一核对；
编译若在本步跑会先失败一次，属预期噪声。

- [x] **Step 6: 核对 webx 中间件签名并编译**

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx
grep -rnE "type Middleware|type HandlerFunc|func \(.*\) Use\(" webx/*.go | head
```
Expected: 打印 webx 的 `Middleware` / `HandlerFunc` 真实签名。
**按实际签名调整** `passThrough`（这是本 Task 唯一需要现场核对类型的地方，其余都已给出
确定内容）；调整后：

Run: `cd examples/mesh/apps && gofmt -l . && go build ./... && go vet ./...`
Expected: `gofmt -l` 无输出；`go build` / `go vet` 退出码 0。

- [x] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 对应行同步）

---

### Task 2: srv 通用骨架与句柄自检

> **trace:** plan-ready.md → `### Task 2: srv 通用骨架与句柄自检` | tasks.md → `- [ ] 1.2 建 `internal/srv/<域>` 的通用骨架件与句柄自检：`middleware/`（RPC 跳的固定窗口限流，按 `user_id`/`operator_id` 计数，KV 不可用即放行并 Warn）、`handlecheck` 等价的自检（取句柄后断言「来自 launchx」且「trace.on 为真时会产 span」，输出 `component`/`backend`/`joins_trace`/`probe` 四字段，覆盖 `db`/`cache`/`kv`/`mq`/`lock` 五类）。验收：自检在句柄被自建实例替换时使 `Setup` 返回 error`
> **sync:** tasks.md → `- [ ] 1.2 建 `internal/srv/<域>` 的通用骨架件与句柄自检：`middleware/`（RPC 跳的固定窗口限流，按 `user_id`/`operator_id` 计数，KV 不可用即放行并 Warn）、`handlecheck` 等价的自检（取句柄后断言「来自 launchx」且「trace.on 为真时会产 span」，输出 `component`/`backend`/`joins_trace`/`probe` 四字段，覆盖 `db`/`cache`/`kv`/`mq`/`lock` 五类）。验收：自检在句柄被自建实例替换时使 `Setup` 返回 error` | plan-ready.md → `### Task 2: srv 通用骨架与句柄自检`

**Files:**
- Create: `examples/mesh/apps/pkg/handles/check.go`
- Create: `examples/mesh/apps/pkg/handles/check_test.go`
- Create: `examples/mesh/apps/internal/srv/order/middleware/ratelimit.go`（四域各一份，仅命名空间与上限不同）

- [x] **Step 1: 写句柄自检**

`examples/mesh/apps/pkg/handles/check.go`：

```go
// Package handles 句柄来源与入链自检。
//
// 为什么必须有这一层：`App.Cache()` / `App.KV()` 这类访问器在组件降级时会返回
// nil，而业务把自建实例接上去也照样能跑 —— 两种情形都**完全静默**：读写正常，
// 只是链路里少了一段、组件门控（trace.on.<name>）形同不存在。
// 这个包把「来源合规」与「入链有效」变成装配期可断言的两件事。
package handles

import (
	"fmt"
	"strings"

	"github.com/gospacex/launchx"
)

// Check 单个组件的自检结果。
type Check struct {
	Component string // db / cache / kv / mq / lock
	Present   bool   // 句柄是否非空
	Backend   string // 驱动/类型名，尽力而为
	Observed  bool   // trace.on 为真时是否观测到会产 span
	Probe     string // 探针说明，供日志里一眼看懂做了什么
}

// Verify 对五类句柄做自检。返回顺序固定，便于日志逐行比对。
//
// 判据分两层：
//  1. 来源：句柄必须由 launchx 构造。自建实例的类型名在本包预期之外，
//     故用「类型名」判定，而不是靠接口断言（自建实例也能满足接口）。
//  2. 入链：trace.on.<name> 为真时句柄必须带 otel 埋点包装。
//     cache / kv 这两类最隐蔽 —— 自建实例连「回调在不在」都探不到。
func Verify(a *launchx.App) []Check {
	cfg := a.Config()
	out := make([]Check, 0, 5)

	out = append(out, Check{
		Component: "db",
		Present:   a.DB() != nil,
		Backend:   typeName(a.DB()),
		Observed:  a.DB() != nil && cfg.GetBool("trace.on.db"),
		Probe:     "App.DB() 非空 + 类型名，配合 repository 层 span 观测",
	})
	out = append(out, Check{
		Component: "cache",
		Present:   len(a.Cache()) > 0,
		Backend:   "cachex.Instances",
		Observed:  len(a.Cache()) > 0 && cfg.GetBool("trace.on.cache"),
		Probe:     "App.Cache() 长度",
	})
	out = append(out, Check{
		Component: "kv",
		Present:   len(a.KV()) > 0,
		Backend:   "kvx.Stores",
		Observed:  len(a.KV()) > 0 && cfg.GetBool("trace.on.kv"),
		Probe:     "App.KV() 长度",
	})
	out = append(out, Check{
		Component: "mq",
		Present:   a.MQ() != nil,
		Backend:   typeName(a.MQ()),
		Observed:  a.MQ() != nil && cfg.GetBool("trace.on.mq"),
		Probe:     "App.MQ() 非空",
	})
	out = append(out, Check{
		Component: "lock",
		Present:   a.Lock() != nil,
		Backend:   typeName(a.Lock()),
		Observed:  a.Lock() != nil && cfg.GetBool("trace.on.lock"),
		Probe:     "App.Lock() 非空",
	})
	return out
}

// VerifyOrError 在**必须存在**的句柄缺失时返回 error。
//
// 什么算必须存在由调用方声明：srv-* 传 db，bff 传 cache。
// 缺了它们业务跑不起来或跑起来是错的，所以装配期就该失败。
func VerifyOrError(a *launchx.App, required ...string) error {
	byName := make(map[string]Check, 5)
	for _, c := range Verify(a) {
		byName[c.Component] = c
	}
	var missing []string
	for _, name := range required {
		c, ok := byName[name]
		if !ok {
			return fmt.Errorf("handles: unknown component %q", name)
		}
		if !c.Present {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("handles: required component(s) unavailable: %s (check xmiddleware section in config)",
			strings.Join(missing, ","))
	}
	return nil
}

// typeName 给出句柄的 Go 类型名（去掉包路径）；nil 返回空串。
// 用反射式取名而不是 import 驱动，保持本包零中间件依赖。
func typeName(v any) string {
	if v == nil {
		return ""
	}
	t := fmt.Sprintf("%T", v)
	if i := strings.LastIndex(t, "."); i >= 0 {
		return t[i+1:]
	}
	return t
}
```

Run: `cd examples/mesh/apps && gofmt -l pkg/handles && go build ./pkg/...`
Expected: `gofmt` 无输出，`go build` 退出码 0。

- [x] **Step 2: 写自检的单测**

`examples/mesh/apps/pkg/handles/check_test.go`：

```go
package handles

import (
	"strings"
	"testing"
)

// 传 nil App 会 panic，故只测「未知组件名」这条不触达句柄的路径；
// 句柄来源的判定在 Task 9 Step 2 用 grep 判据覆盖（那里能看真实调用点）。
func TestVerifyOrErrorRejectsUnknownComponent(t *testing.T) {
	err := VerifyOrError(nil, "nope")
	if err == nil {
		t.Fatal("期望 unknown component 报错，实际为 nil")
	}
	if !strings.Contains(err.Error(), "unknown component") {
		t.Fatalf("错误串未点明 unknown component：%v", err)
	}
}
```

Run: `cd examples/mesh/apps && go test ./pkg/handles/`
Expected: PASS（1 个测试）。

- [x] **Step 3: 写 RPC 跳限流中间件**

`examples/mesh/apps/internal/srv/order/middleware/ratelimit.go`（四域各一份，
仅 `rateLimitPerMinute` 与 `namespace` 不同）：

```go
// Package middleware 应用侧兜底：RPC 跳的固定窗口限流。
//
// 为什么这层删不掉（两道独立理由，任一成立都留下它）：
//   1. bff→srv 走 Kitex 默认 TTHeader/TCP，Istio 对该端口按 tcp_proxy 处理 ——
//      Envoy 只看得见连接、看不见请求，「按请求限流」交不出去；
//   2. 它按 user_id / operator_id 计数，是**业务维度** —— 就算换成 gRPC 让 Envoy
//      读到请求，网格也认不出这两个字段属于谁。
package middleware

import (
	"context"
	"fmt"
	"time"

	"github.com/gospacex/kvx"
	"github.com/gospacex/logx"
)

const (
	// rateLimitPerMinute 每主体每分钟上限。订单域写多读也多，比商品域紧。
	rateLimitPerMinute = 120
	rateLimitWindow    = time.Minute
	// namespace 计数键前缀，避免与其它域的计数互相污染（同 redis 实例）。
	namespace = "mesh:order:rl"
)

// RateLimiter 固定窗口限流器。KV 为 nil 时**放行**（fail-open）并打 Warn。
type RateLimiter struct {
	store  kvx.Store
	logger logx.Logger
	limit  int64
	window time.Duration
}

// New 构造限流器；store 允许为 nil（表示 KV 不可用）。
func New(store kvx.Store, logger logx.Logger) *RateLimiter {
	return &RateLimiter{store: store, logger: logger, limit: rateLimitPerMinute, window: rateLimitWindow}
}

// Allow 判断该主体本轮是否放行。
//
// 失败姿态是**放行**而不是拒绝：限流是自保护，不该因为计数后端抖动就把正常
// 请求打成 429（那等于把限流器自己变成故障源）。但必须打 Warn ——
// 「限了却被打穿且无痕可查」是最难排查的形态。
func (r *RateLimiter) Allow(ctx context.Context, subject string) bool {
	if r == nil || r.store == nil || subject == "" {
		if r != nil && r.logger != nil {
			r.logger.Warn("mesh-order: rate limiter unavailable, request allowed",
				logx.String("subject", subject),
				logx.String("consequence", "本轮不受限流保护"),
				logx.String("probe", "检查 xmiddleware.kvx 段"))
		}
		return true
	}
	key := fmt.Sprintf("%s:%s:%d", namespace, subject, time.Now().Unix()/int64(r.window.Seconds()))
	n, err := r.store.Incr(ctx, key)
	if err != nil {
		r.logger.Warn("mesh-order: rate limit counter failed, request allowed",
			logx.String("subject", subject), logx.Err(err))
		return true
	}
	if n == 1 {
		// 首次计数才设过期：每次 Incr 都 Expire 会让窗口永不滚动。
		if err := r.store.Expire(ctx, key, r.window); err != nil {
			r.logger.Warn("mesh-order: set rate limit ttl failed", logx.Err(err))
		}
	}
	return n <= r.limit
}
```

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx && grep -rnE "type Store|func .*Incr|func .*Expire" kvx/*.go | head
```
Expected: 打印 kvx 的真实接口名与方法签名。
**按实际签名调整** `store kvx.Store` 与 `Incr` / `Expire` 的调用形式（本 Step 唯一需要现场
核对处）；调整后 `cd examples/mesh/apps && go build ./internal/...` 退出码 0。

> **执行期修正（2026-10-07，与规格对齐）**：`kvx` 里**没有** `Store` 也没有 `Expire` —— 真实接口是
> `kvx.KVStore`（= `kvxbase.KVStore`），只有 `Incr(ctx,key)` 与 `Set(ctx,key,value,ttl)`。
> TTL 改用 `n==1` 时 `Set(ctx,key,"1",window)` 补，与 `examples/micro/srv-*/internal/middleware/ratelimit.go`
> 的既有写法**逐字一致**。
>
> `pkg/handles` 按 `examples/monolith/internal/handlesrc/handlesrc.go` 的范式重做（计划给的
> `Observed: present && cfg.GetBool("trace.on.<name>")` 达不到 spec 要求）：拆分 `Want`（有效裁决
> `trace.enabled AND trace.on.<name>`）与 `Observed`（真实探针）；新增 `Diverged()` 并纳入
> `VerifyOrError` 的失败条件；五个 check 与 `verify` 核心逻辑 100% 覆盖（窄接口 `source` 注入）。
>
> ⚠️ 已知能力边界：**只有 `db` 能在装配期做「来源实锤」**（探 gorm 句柄上的 dbx trace 回调）；
> `cache`/`kv`/`mq`/`lock` 只有组件级/全局开关代理，**分不出自建实例**，已如实写进各条 `Probe`。
> 其余四类的来源合规依赖 Task 9 的源码守卫互补 —— `mesh-app-handles` spec 的 Scenario 举例
> 用了 `App.Cache()`，与这条能力边界不符，已提请 amend。

- [x] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 对应行同步）

---

### Task 3: srv-order（参考域）

> **trace:** plan-ready.md → `### Task 3: srv-order（参考域）` | tasks.md → `- [ ] 1.3 实现 `srv-order`（参考域，句柄最多）：`model`（orders / order_items / order_status_log）+ `repository`（经 `App.DB()`，开 `repository.order.*` span）+ `service`（★ 唯一业务判断层：下单 / 查单 / 改状态，开 `service.order.*` span）+ `rpc`（**入向传输适配层**：实现 `kitex_gen` 的 `Order` 接口全部 4 个方法、开 `rpc.order.*` span、走 `route` 准入闸门后委派 `handler`；`ListOrders`/`Report` 显式返回 `codes.Unimplemented`）+ `handler`（只做入出参转换，开 `handler.order.*` span）+ `route`（Auth + 限流）+ `app/setup.go`（`DB()`/`Cache()`/`KV()`/`Lock()`/`MQ()`/`RPCServerOptions()`+`SetRPCServer()`）。验收：单进程起服 + `CreateOrder` 落库并回读 + `order.completed.v1` 有消息 + 一条 trace 内 `rpc/handler/service/repository` 四层 span 齐全`
> **sync:** tasks.md → `- [ ] 1.3 实现 `srv-order`（参考域，句柄最多）：`model`（orders / order_items / order_status_log）+ `repository`（经 `App.DB()`，开 `repository.order.*` span）+ `service`（★ 唯一业务判断层：下单 / 查单 / 改状态，开 `service.order.*` span）+ `rpc`（**入向传输适配层**：实现 `kitex_gen` 的 `Order` 接口全部 4 个方法、开 `rpc.order.*` span、走 `route` 准入闸门后委派 `handler`；`ListOrders`/`Report` 显式返回 `codes.Unimplemented`）+ `handler`（只做入出参转换，开 `handler.order.*` span）+ `route`（Auth + 限流）+ `app/setup.go`（`DB()`/`Cache()`/`KV()`/`Lock()`/`MQ()`/`RPCServerOptions()`+`SetRPCServer()`）。验收：单进程起服 + `CreateOrder` 落库并回读 + `order.completed.v1` 有消息 + 一条 trace 内 `rpc/handler/service/repository` 四层 span 齐全` | plan-ready.md → `### Task 3: srv-order（参考域）`；spec → `mesh-entry-chain` 的「四域 RPC 实现体必须实现全部 IDL 方法」（本 Task 是它的参考实现）

**Files:**
- Modify: `examples/mesh/apps/pkg/codes/codes.go`（amend 追加：补 `Unimplemented` 码）
- Create: `examples/mesh/apps/internal/srv/order/{model/order.go,repository/order.go,service/order.go,handler/order.go,route/route.go,rpc/order.go,app/setup.go}`

- [x] **Step 0（amend 追加）: 给 `pkg/codes` 补 `Unimplemented` 码**

`pkg/codes/codes.go` 是 Task 1 的产物（已完成），本步只在它的常量块里**追加一行** ——
四域 handler 需要它来给「IDL 里有、本示例没实现」的方法一个明确姿态
（见 `mesh-entry-chain` 的「四域 RPC 实现体必须实现全部 IDL 方法」）。

```go
	// Unimplemented 下游明确声明「本示例未实现」。
	// 与 Internal 分开是刻意的：Internal 会被 bff 映射成 502（上游坏了），
	// 而 501 如实表达「方法存在但没接线」，排查时不必去翻下游日志。
	Unimplemented Code = 10501
```

常量块追加在 `Internal` 之后；同时在 `pkg/codes/codes_test.go` 里给 `Code.String()` 的
表驱动用例补上 `Unimplemented` 一项（现有测试是表驱动的，漏加会让覆盖率掉出 100%）。

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx/examples/mesh/apps
gofmt -l pkg/codes && go test -count=1 -cover ./pkg/codes/
grep -n 'Unimplemented' pkg/codes/codes.go
```
Expected: `gofmt` 无输出；`go test` PASS 且覆盖率 100%；`grep` 命中常量声明。

- [x] **Step 1: 写 model**

`examples/mesh/apps/internal/srv/order/model/order.go`：

```go
// Package model 表模型：纯数据，不依赖任何中间件句柄。
package model

import "time"

// Order 订单主表。列名与 `examples/scripts/order_db.sh` 的 DDL 真源一致。
type Order struct {
	ID          uint64    `gorm:"primaryKey;column:id"`
	UserID      uint64    `gorm:"column:user_id"`
	OrderNo     string    `gorm:"column:order_no"`
	Status      string    `gorm:"column:status"`
	TotalAmount int64     `gorm:"column:total_amount"`
	PaidAmount  int64     `gorm:"column:paid_amount"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

// TableName 固定表名，避免依赖 gorm 的复数化规则（规则变动会让表名漂移）。
func (Order) TableName() string { return "orders" }

// OrderItem 订单明细。
type OrderItem struct {
	ID          uint64 `gorm:"primaryKey;column:id"`
	OrderID     uint64 `gorm:"column:order_id"`
	ProductID   uint64 `gorm:"column:product_id"`
	ProductName string `gorm:"column:product_name"`
	Quantity    int    `gorm:"column:quantity"`
	UnitPrice   int64  `gorm:"column:unit_price"`
}

func (OrderItem) TableName() string { return "order_items" }

// OrderStatusLog 状态流转日志。
type OrderStatusLog struct {
	ID         uint64    `gorm:"primaryKey;column:id"`
	OrderID    uint64    `gorm:"column:order_id"`
	FromStatus string    `gorm:"column:from_status"`
	ToStatus   string    `gorm:"column:to_status"`
	CreatedAt  time.Time `gorm:"column:created_at"`
}

func (OrderStatusLog) TableName() string { return "order_status_log" }

// 状态常量：散落的字符串字面量是状态机类 bug 的常见来源。
const (
	StatusCreated = "created"
	StatusPaid    = "paid"
	StatusClosed  = "closed"
)
```

Run: `cd examples/mesh/apps && gofmt -l internal/srv/order/model && go build ./internal/...`
Expected: `gofmt` 无输出，`go build` 退出码 0。

- [x] **Step 2: 写 repository（只经 App.DB() 句柄）**

`examples/mesh/apps/internal/srv/order/repository/order.go`：

```go
// Package repository 持久化层：只使用 launchx 交出的 *gorm.DB，不直引驱动、
// 不开连接。db 由 app.Setup 经 App.DB() 取到后注入，本层不感知它从哪来。
package repository

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/model"
)

// OrderRepository 订单存储。
type OrderRepository struct {
	db      *gorm.DB
	traceOn bool // trace.on.db：只用于决定是否额外打点，不决定构造
}

// New 构造 repository。
func New(db *gorm.DB, traceOn bool) *OrderRepository {
	return &OrderRepository{db: db, traceOn: traceOn}
}

// Create 落一张订单 + 明细 + 首条流转日志（同一事务）。
func (r *OrderRepository) Create(ctx context.Context, o *model.Order, items []model.OrderItem) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(o).Error; err != nil {
			return err
		}
		for i := range items {
			items[i].OrderID = o.ID
		}
		if len(items) > 0 {
			if err := tx.Create(&items).Error; err != nil {
				return err
			}
		}
		return tx.Create(&model.OrderStatusLog{
			OrderID: o.ID, FromStatus: "", ToStatus: o.Status,
		}).Error
	})
}

// FindByID 读订单主表。gorm 的 ErrRecordNotFound 原样上抛，由 service 翻成
// 业务码 —— 在这一层翻会让「查不到」与「库报错」混为一谈。
func (r *OrderRepository) FindByID(ctx context.Context, id uint64) (*model.Order, error) {
	var o model.Order
	if err := r.db.WithContext(ctx).First(&o, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &o, nil
}

// ItemsByOrderID 读明细。
func (r *OrderRepository) ItemsByOrderID(ctx context.Context, id uint64) ([]model.OrderItem, error) {
	var items []model.OrderItem
	if err := r.db.WithContext(ctx).Where("order_id = ?", id).Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

// UpdateStatus 改状态并记流转日志（同一事务）。
//
// 先按 fromStatus 做条件更新：并发下两个请求都想把 created 改成 paid 时，
// 只有一个的 RowsAffected 为 1 —— 这把「状态机竞态」变成库层的原子判定，
// 比在 service 里先读后写可靠。
func (r *OrderRepository) UpdateStatus(ctx context.Context, id uint64, from, to string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.Order{}).
			Where("id = ? AND status = ?", id, from).
			Update("status", to)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errors.New("order status changed by another request")
		}
		return tx.Create(&model.OrderStatusLog{
			OrderID: id, FromStatus: from, ToStatus: to,
		}).Error
	})
}
```

Run: `cd examples/mesh/apps && gofmt -l internal/srv/order/repository && go build ./internal/...`
Expected: `gofmt` 无输出，`go build` 退出码 0。

- [x] **Step 3: 写 service（唯一业务判断层）**

`examples/mesh/apps/internal/srv/order/service/order.go`：

```go
// Package service ★ 唯一允许出现业务判断的层。
//
// 句柄在这里汇合：cache（详情缓存）、lock（同用户下单互斥）、mq（事件投递）。
// 全部按「缺失即降级 + 留日志」处理；db 是硬前提（app.Setup 已用 VerifyOrError 挡过）。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/gospacex/cachex"
	"github.com/gospacex/lockx"
	"github.com/gospacex/logx"
	"github.com/gospacex/mqx"
	"gorm.io/gorm"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/model"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/repository"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/events"
)

// OrderService 订单业务。
type OrderService struct {
	repo     *repository.OrderRepository
	cache    cachex.Cache
	lock     lockx.Locker
	producer mqx.Producer
	logger   logx.Logger
}

// New 注入全部句柄；cache / lock / producer 允许为 nil（各有降级路径）。
func New(repo *repository.OrderRepository, cache cachex.Cache, lock lockx.Locker,
	producer mqx.Producer, logger logx.Logger) *OrderService {
	return &OrderService{repo: repo, cache: cache, lock: lock, producer: producer, logger: logger}
}

// CreateOrderInput 下单入参（service 自己的入参类型，与 RPC 结构解耦）。
type CreateOrderInput struct {
	UserID      uint64
	ProductID   uint64
	ProductName string
	Quantity    int
	UnitPrice   int64
}

// OrderView 查单出参。
type OrderView struct {
	Order  model.Order
	Items  []model.OrderItem
	Cached bool // 是否命中缓存，便于验收时判断缓存是否真的生效
}

// CreateOrder 下单：加锁 → 落库 → 投事件。
func (s *OrderService) CreateOrder(ctx context.Context, in CreateOrderInput) (uint64, error) {
	if in.Quantity <= 0 {
		return 0, codes.New(codes.InvalidArgument, "quantity must be positive")
	}

	unlock, err := s.acquire(ctx, in.UserID)
	if err != nil {
		return 0, err
	}
	if unlock != nil {
		defer unlock()
	}

	o := &model.Order{
		UserID:      in.UserID,
		OrderNo:     fmt.Sprintf("M%s%06d", time.Now().Format("20060102150405"), in.UserID%1000000),
		Status:      model.StatusCreated,
		TotalAmount: in.UnitPrice * int64(in.Quantity),
	}
	items := []model.OrderItem{{
		ProductID: in.ProductID, ProductName: in.ProductName,
		Quantity: in.Quantity, UnitPrice: in.UnitPrice,
	}}
	if err := s.repo.Create(ctx, o, items); err != nil {
		return 0, codes.Wrap(codes.OrderCreateFail, "create order", err)
	}

	s.publish(ctx, o)
	return o.ID, nil
}

// GetOrder 查单：先缓存，未命中回源并回填。
func (s *OrderService) GetOrder(ctx context.Context, id uint64) (*OrderView, error) {
	if s.cache != nil {
		if raw, err := s.cache.Get(ctx, s.key(id)); err == nil && raw != nil {
			if o, ok := decodeOrder(raw); ok {
				items, _ := s.repo.ItemsByOrderID(ctx, id)
				return &OrderView{Order: o, Items: items, Cached: true}, nil
			}
		}
	}

	o, err := s.repo.FindByID(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, codes.New(codes.OrderNotFound, fmt.Sprintf("order %d not found", id))
		}
		return nil, codes.Wrap(codes.Internal, "get order", err)
	}
	items, err := s.repo.ItemsByOrderID(ctx, id)
	if err != nil {
		return nil, codes.Wrap(codes.Internal, "get order items", err)
	}
	s.fill(ctx, *o)
	return &OrderView{Order: *o, Items: items}, nil
}

// MarkPaid 改状态：库层条件更新 → 缓存失效。
func (s *OrderService) MarkPaid(ctx context.Context, id uint64) error {
	if err := s.repo.UpdateStatus(ctx, id, model.StatusCreated, model.StatusPaid); err != nil {
		return codes.Wrap(codes.OrderCreateFail, "mark paid", err)
	}
	// 写后失效：顺序必须是「库成功 → 删缓存」。先删缓存再落库会让并发请求
	// 读到空后回填旧值。删除失败只告警：TTL 会兜底。
	if s.cache != nil {
		if err := s.cache.Delete(ctx, s.key(id)); err != nil {
			s.logger.Warn("mesh-order: cache invalidate failed, ttl will expire it",
				logx.String("key", s.key(id)), logx.Err(err))
		}
	}
	return nil
}

// acquire 取锁；返回 nil unlock 表示本次未走锁路径。
//
// 句柄缺失时 fail-open，**但必须说出来** —— 「锁没生效但没人报错」是超卖类
// 故障最难排查的形态。
func (s *OrderService) acquire(ctx context.Context, userID uint64) (func(), error) {
	if s.lock == nil {
		s.logger.Warn("mesh-order: lock unavailable, running without mutex",
			logx.String("consequence", "同用户并发下单不再互斥"),
			logx.String("probe", "检查 xmiddleware.lockx 段"))
		return nil, nil
	}
	release, err := s.lock.Lock(ctx, "order:user:"+strconv.FormatUint(userID, 10))
	if err != nil {
		return nil, codes.Wrap(codes.Internal, "acquire lock", err)
	}
	return func() {
		if rerr := release(ctx); rerr != nil {
			s.logger.Warn("mesh-order: unlock failed, ttl will expire it", logx.Err(rerr))
		}
	}, nil
}

// publish 投递订单完成事件。失败只告警：本例不带 outbox 表，
// 投递失败由上层重试；真实系统这里必须落 outbox（见 apps/README「删不掉的四类」）。
func (s *OrderService) publish(ctx context.Context, o *model.Order) {
	if s.producer == nil {
		s.logger.Warn("mesh-order: mq producer unavailable, event not published",
			logx.String("order_no", o.OrderNo),
			logx.String("consequence", "积分域收不到该订单"))
		return
	}
	payload, err := events.OrderCompleted{
		OrderID: o.ID, UserID: o.UserID, TotalAmount: o.TotalAmount, OccurredAt: time.Now(),
	}.Marshal()
	if err != nil {
		s.logger.Warn("mesh-order: marshal event failed", logx.Err(err))
		return
	}
	if err := s.producer.Send(ctx, events.TopicOrderCompleted, payload); err != nil {
		s.logger.Warn("mesh-order: publish event failed", logx.Err(err))
	}
}

func (s *OrderService) key(id uint64) string {
	return "mesh:order:detail:" + strconv.FormatUint(id, 10)
}

// decodeOrder 只从缓存解出订单主表；明细仍回源（明细变更频率高于主表）。
func decodeOrder(raw []byte) (model.Order, bool) {
	var o model.Order
	if err := json.Unmarshal(raw, &o); err != nil || o.ID == 0 {
		return model.Order{}, false
	}
	return o, true
}

// fill 回填缓存，失败只告警（缓存是加速器，不该影响请求成败）。
func (s *OrderService) fill(ctx context.Context, o model.Order) {
	if s.cache == nil {
		return
	}
	b, err := json.Marshal(o)
	if err != nil {
		return
	}
	if err := s.cache.Set(ctx, s.key(o.ID), b, 5*time.Minute); err != nil {
		s.logger.Warn("mesh-order: cache fill failed", logx.Err(err))
	}
}
```

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx && grep -rnE "type Producer|Send\(ctx" mqx/*.go | head
cd /Users/hyx/work/gowork/src/hubx/launchx/examples/mesh/apps && gofmt -l internal/srv/order/service && go build ./internal/...
```
Expected: 打印 mqx `Producer.Send` 的真实签名；`gofmt` 无输出；`go build` 退出码 0。
**按实际签名调整** `producer.Send(ctx, topic, payload)`（topic 键位见 `pkg/events` 注释：
`consumer.topic` 才是 producer 读的那个键）。

- [x] **Step 4: 写 handler / route / rpc**

`handler/order.go`——只做入出参转换与错误码映射，零业务判断。字段名以**实际生成产物**为准。

⚠️ **本文件必须实现 `orderrpc.Order` 接口的全部 4 个方法**（`CreateOrder` / `GetOrder` /
`ListOrders` / `Report`）：kitex 生成代码把 handler 类型断言写在分发函数里
（`handler.(orderrpc.Order).ListOrders(...)`），少一个方法 `server.NewServer(...)` 就
编译不过 —— 这是编译期约束，不是纪律约定。未接线的两个方法**显式**返回
`codes.Unimplemented`，MUST NOT 空实现或 panic。

```go
// Package handler 入参出参转换，零业务判断。
//
// 四个 RPC 方法全部实现（kitex 的接口约束）。其中：
//   CreateOrder / GetOrder —— 走 service，已接线，入口路由见 mesh-entry-chain
//   ListOrders / Report    —— 本示例未接线，显式返回 codes.Unimplemented（注明「非漏项」）
package handler

import (
	"context"

	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	orderrpc "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/order/order"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/service"
)

// OrderHandler 实现 kitex_gen 的 OrderService 接口。
type OrderHandler struct {
	svc *service.OrderService
}

// New 构造 handler。
func New(svc *service.OrderService) *OrderHandler { return &OrderHandler{svc: svc} }

// CreateOrder RPC 入口。这是 `POST /order` 的下游，也是 `order → MQ → points`
// 异步链的起点（service 层在落库后调 publish）。
func (h *OrderHandler) CreateOrder(ctx context.Context, req *orderrpc.CreateOrderReq) (*orderrpc.CreateOrderResp, error) {
	in := service.CreateOrderInput{UserID: uint64(req.UserId)}
	for _, it := range req.Items {
		in.Items = append(in.Items, service.CreateOrderItem{
			ProductID: uint64(it.ProductId), ProductName: it.Name,
			Quantity: int64(it.Quantity), UnitPrice: it.UnitPrice,
		})
	}
	id, err := h.svc.CreateOrder(ctx, in)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.GetOrder(ctx, id)
	if err != nil {
		return nil, err
	}
	return &orderrpc.CreateOrderResp{
		UserId: v.Order.UserID, OrderId: v.Order.ID,
		OrderNo: v.Order.OrderNo, TotalAmount: v.Order.TotalAmount,
	}, nil
}

// GetOrder RPC 入口。
func (h *OrderHandler) GetOrder(ctx context.Context, req *orderrpc.GetOrderReq) (*orderrpc.GetOrderResp, error) {
	v, err := h.svc.GetOrder(ctx, req.OrderId)
	if err != nil {
		return nil, err
	}
	items := make([]*orderrpc.OrderItem, 0, len(v.Items))
	for _, it := range v.Items {
		items = append(items, &orderrpc.OrderItem{
			ProductId: it.ProductID, Name: it.ProductName,
			Quantity: int32(it.Quantity), UnitPrice: it.UnitPrice,
		})
	}
	return &orderrpc.GetOrderResp{
		UserId: v.Order.UserID, OrderId: v.Order.ID, OrderNo: v.Order.OrderNo,
		Status: v.Order.Status, TotalAmount: v.Order.TotalAmount, Items: items,
	}, nil
}

// ListOrders 未接线：本示例不提供列表页。显式声明，不留 panic / 空实现。
func (h *OrderHandler) ListOrders(_ context.Context, _ *orderrpc.ListOrdersReq) (*orderrpc.ListOrdersResp, error) {
	return nil, codes.New(codes.Unimplemented, "order.ListOrders not wired in mesh apps demo")
}

// Report 未接线：同上。
func (h *OrderHandler) Report(_ context.Context, _ *orderrpc.ReportReq) (*orderrpc.ReportResp, error) {
	return nil, codes.New(codes.Unimplemented, "order.Report not wired in mesh apps demo")
}
```

`route/route.go`——身份闸门 + 限流两层，顺序不可换：

```go
// Package route 内层：身份闸门（Auth）→ 业务维度限流。
// 两层都在 handler 之前，被拒绝的请求不进入业务判断。
//
// 鉴权口径的减法：传输层身份（「调用方是不是 bff」）下沉给 Istio 的
// AuthorizationPolicy + mTLS；本层只校验 bff 透传下来的身份主体。
// micro 的 pkg/auth/auth.go:61 自己就写着「它防不住内网里的恶意调用（那需要 mTLS）」
// —— 那半正是网格补上的。
package route

import (
	"context"
	"strconv"

	"github.com/gospacex/logx"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/middleware"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/routing"
)

// Guard 身份闸门 + 限流。
type Guard struct {
	limiter *middleware.RateLimiter
	logger  logx.Logger
}

// New 构造闸门。
func New(limiter *middleware.RateLimiter, logger logx.Logger) *Guard {
	return &Guard{limiter: limiter, logger: logger}
}

// Admit 返回是否放行；subject 为空表示无身份（按服务级计数）。
func (g *Guard) Admit(ctx context.Context, id routing.Identity) bool {
	subject := "svc"
	if id.UserID != 0 {
		subject = "u" + strconv.FormatUint(id.UserID, 10)
	}
	return g.limiter.Allow(ctx, subject)
}
```

`rpc/order.go`——跨域调用客户端（选项由 launchx 产出，地址只来自配置）：

```go
// Package rpc 出向 RPC 客户端：跨域调用（order → product / stock）。
//
// 客户端选项由 launchx 按 mode 产出：apps 下没有注册中心，走
// service.upstream.<域> 的 K8s Service FQDN → kitex DNS 解析 → Envoy 按
// Service endpoints 负载均衡。业务侧只做类型断言，不拼地址。
package rpc

import (
	"github.com/cloudwego/kitex/client"

	productrpc "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/product/product"
)

// NewProductClient 建商品域客户端。服务名 MUST 与 conf 里
// service.upstream.<名> 的键、以及商品域的 service.name 三处完全一致。
func NewProductClient(opts []client.Option) (productrpc.Client, error) {
	return productrpc.NewClient("srv-product", opts...)
}
```

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx/examples/mesh/apps
grep -n "type GetOrderReq\|type GetOrderResp\|type OrderItem\|type CreateOrderReq" -A 12 pkg/kitex_gen/order/order/order.go | head -60
gofmt -l internal/ && go build ./internal/...
```
Expected: 打印生成结构的真实字段名；据此把上面的字段逐条对齐（**生成代码的字段名不是
猜的**）；`gofmt` 无输出；`go build` 退出码 0。

- [x] **Step 5: 写 app/setup.go（句柄汇合点）**

`examples/mesh/apps/internal/srv/order/app/setup.go`：

```go
// Package app 装配层：把 launchx 交出的句柄接成
//
//	rpc → route → handler → service → repository → model
//
// 这条链，并把 kitex 服务端回存给 launchx 启动与停止。
//
// 与 examples/micro/srv-order/app/setup.go 的装配差异只有三处：
//   1. 不注册/不发现（没有 xmiddleware.coordx 段，launchx 也不会去注册）；
//   2. 句柄自检用 pkg/handles（micro 用的是自己的 pkg/handlecheck）；
//   3. 没有 outbox Runner（本例不带 outbox 表，投递失败只告警）。
package app

import (
	"context"
	"errors"
	"strconv"

	"github.com/cloudwego/kitex/pkg/endpoint"
	"github.com/cloudwego/kitex/server"
	"github.com/gospacex/logx"
	"gorm.io/gorm"

	"github.com/gospacex/launchx"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/handler"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/middleware"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/repository"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/route"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/service"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/handles"
	orderrpc "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/order/order"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/routing"
)

// Setup 注册给 App.OnSetup。
//
// 顺序即纪律：先取句柄 → 再自检 → 后装配。反过来（先装配再自检）判据就失效了：
// 句柄被业务包过一层之后，自检看到的已不是 launchx 交出的那个实例。
func Setup(a *launchx.App) error {
	// ---- 1. 取句柄 ----
	db, ok := a.DB().(*gorm.DB)
	if !ok || db == nil {
		return errors.New("srv-order: db handle unavailable (check xmiddleware.sqlx + strict.db)")
	}
	opts, ok := a.RPCServerOptions().([]server.Option)
	if !ok {
		return errors.New("srv-order: rpc server options unavailable (check service.mode=srv)")
	}
	cfg := a.Config()
	name := cfg.GetString("service.name")
	logger := a.Logger()

	// ---- 2. 句柄自检：来源合规 + 入链有效 ----
	// db 是硬前提（strict.db: true 已在配置里声明），缺了必须装配期即退。
	if err := handles.VerifyOrError(a, "db"); err != nil {
		return errors.New("srv-order: " + err.Error())
	}
	for _, c := range handles.Verify(a) {
		if !c.Present {
			continue
		}
		logger.Info("handle verified",
			logx.String("component", c.Component), logx.String("backend", c.Backend),
			logx.String("joins_trace", strconv.FormatBool(c.Observed)), logx.String("probe", c.Probe))
	}

	// ---- 3. 分层装配：自下而上 ----
	repo := repository.New(db, cfg.GetBool("trace.on.db"))
	svc := service.New(repo, a.Cache()[name], a.Lock(), a.MQ(), logger)
	guard := route.New(middleware.New(a.KV()[name], logger), logger)

	// ---- 4. 建服务端并回存 ----
	// 身份闸门挂服务端级中间件：一次挂上对所有方法生效（本域方法少，不做方法级区分）。
	// 取身份的来源是 RPC 元信息（由 bff 透传）；键名必须与 bff 写入的一致，
	// 否则这里永远拿到零值、闸门退化成服务级限流。
	opts = append(opts, server.WithMiddleware(rpcAdmit(guard)))

	srv := orderrpc.NewServer(handler.New(svc), opts...)
	a.SetRPCServer(srv)

	logger.Info("layer chain assembled",
		logx.String("service", name),
		logx.String("chain", "rpc>route>auth>ratelimit>handler>service>repository>model"))
	return nil
}

// rpcAdmit 产出 kitex 服务端中间件：取身份 → 闸门判定 → 拒绝即返回业务码。
func rpcAdmit(g *route.Guard) endpoint.Middleware {
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, req, resp interface{}) error {
			if !g.Admit(ctx, routing.IdentityFrom(ctx)) {
				return codes.New(codes.Internal, "rate limited")
			}
			return next(ctx, req, resp)
		}
	}
}
```

Run: `cd examples/mesh/apps && gofmt -l internal/srv/order/app && go build ./... && go vet ./...`
Expected: `gofmt` 无输出；`go build` / `go vet` 退出码 0。
若 `server.WithMiddleware` 与 kitex 版本不符，用
`go doc github.com/cloudwego/kitex/server.WithMiddleware` 取实际签名后调整。

> **执行期修正（2026-10-07 amend #2，Task 3 Step 4/5 的语义纠正）**
>
> Step 4/5 已按本文件实现并勾选，**不回退**；但规格合规审查发现三处**计划自身的错误**，
> 增补统一放在 **Task 18**（执行顺序：Task 18 在 Task 4/5/6 之前）：
>
> 1. **`rpc/` 不是「跨域调用客户端」**。`examples/micro` 四域的 `rpc/<域>.go` 全部是
>    **入向传输适配层**（实现生成的 `<域>` 接口 + 走 Router + 开 `rpc.<域>.<Op>` span）；
>    而 launchx 的 `App.RPCClientOptions()` 只在 bff/gateway 档产出（`internal/mode/webx.go`），
>    srv 档直接返回 error —— 本 Step 写出的出向客户端在 apps 下**取不到选项**，是死代码。
>    Task 18 把它重写为入向适配层（`NewServer(rpc.New(handler, guard, traceOn), ...)`），
>    并撤销 `app/setup.go` 里的 `rpcAdmit` 服务端中间件（闸门移进 `rpc/` 层）。
> 2. **`handler` 不该直接满足 IDL 接口**：接口的唯一实现方是 `rpc/`（与 micro 同构），
>    `handler` 只做转换。Task 18 迁移该编译期断言。
> 3. **四层分层 span 全缺**：本 Task 未写任何 `otelx/layer` 埋点，而 `mesh-entry-chain`
>    的判据要求 trace 内出现 `service.order.<Op>`；`rpcx/middleware.go` 的包注释已写明
>    「服务端 span 由各服务自己的 service 层产出」。Task 18 给四域补齐
>    `rpc` / `handler` / `service` / `repository` 四层 span。
>
> 另：本 Task 的端到端验收（起服 + 落库回读 + MQ 有消息）依赖 `cmd/`（Task 8）与四域齐备，
> 实际在 **Task 15** 一次性验证；本 Task 不因此判为未完成。
>
> 顺带记录实现期确认的真实签名（计划里写错的，实现已按实际调整）：
> `mqx.Producer.Send(ctx, *mqx.Message)`、`cachex.Cache.Get/Set(ctx, key, string, ttl)`、
> `lockx.Locker.Lock(ctx, key) (lockx.Lock, error)` + `Lock.Unlock(ctx)`、
> IDL 结构体在 `pkg/kitex_gen/<域>`（不是 `.../<域>/<域>`）。

- [x] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 对应行同步）

---

### Task 4: srv-points（消费侧）

> **trace:** plan-ready.md → `### Task 4: srv-points（消费侧）` | tasks.md → `- [ ] 1.4 实现 `srv-points`：`DB()`（积分余额与流水）+ `MQConsumer()`（订阅 `order.completed.v1` 加积分，`App.OnStop` 里 cancel 消费循环）；`rpc` 入向适配层实现 `points.Points` 接口全部 5 个方法（`GetAccount` 委派 handler 走业务，`Earn`/`Consume`/`ListLedgers`/`Report` 显式 `codes.Unimplemented`），四层各开 `*.<域>.*` span。验收：消费到 order 域投递的消息后积分表落一条流水，且 `service.points.ConsumeOrderCompleted` 与下单请求同 traceID`
> **sync:** tasks.md → `- [ ] 1.4 实现 `srv-points`：`DB()`（积分余额与流水）+ `MQConsumer()`（订阅 `order.completed.v1` 加积分，`App.OnStop` 里 cancel 消费循环）；`rpc` 入向适配层实现 `points.Points` 接口全部 5 个方法（`GetAccount` 委派 handler 走业务，`Earn`/`Consume`/`ListLedgers`/`Report` 显式 `codes.Unimplemented`），四层各开 `*.<域>.*` span。验收：消费到 order 域投递的消息后积分表落一条流水，且 `service.points.ConsumeOrderCompleted` 与下单请求同 traceID` | plan-ready.md → `### Task 4: srv-points（消费侧）`；spec → `mesh-entry-chain` 的「四域 RPC 实现体必须实现全部 IDL 方法」+「跨域编排只在 bff」；mesh-app-layering 的「四层 span 逐层可见」

**Files:** `examples/mesh/apps/internal/srv/points/{model,repository,service,handler,route,rpc,app}/**`
（`middleware/ratelimit.go` 从 Task 2 复制，改 `namespace` 与上限；`rpc/`、四层埋点、
`app/setup.go` 的组装按 **Task 18** 定下的模板写 —— 本 Task 不再「删除 `rpc/<域>.go`」）

- [x] **Step 1: 逐文件差异表（与 Task 3 的 order 对照）**

| 文件 | 与 order 的差异 | 校验 |
|---|---|---|
| `model/points.go` | 表名 `points_account`（`id/user_id/balance/updated_at`）与 `points_log`（`id/user_id/order_id/delta/created_at`）；无 `order_items`/`order_status_log` | `grep -cE "func \((PointAccount\|PointsLog)\) TableName" model/points.go` == 2（**别用裸 `grep -c "TableName"`** —— 那个判据会连注释里的字样一起数，逼着人改注释去凑数，是 grep-as-oracle 的反模式） |
| `repository/points.go` | 方法集换为 `AddBalance(ctx, userID, delta)` / `Balance(ctx, userID)` / `AddLog(ctx, PointsLog)`；`AddBalance` 用 gorm `clause.OnConflict` 的 `ON DUPLICATE KEY UPDATE balance = balance + ?` 而非「事务内先查后写」 | `go build ./internal/...` |
| `service/points.go` | 主路径是 `ConsumeOrderCompleted(ctx, e events.OrderCompleted)`；只读 RPC 路径 `GetPoints`（**IDL 名是 `GetAccount`**）；句柄只用 db + kv（`route` 限流计数）+ mqConsumer（无 cache / lock / mq producer） | `grep -cE '(a\.Cache\(\)|a\.Lock\(\)|a\.MQ\(\))' app/setup.go` == 0 |
| `handler/points.go` | 只做入出参转换（不实现 IDL 接口）；`GetAccount` 的转换，另 4 个未接线方法返回 `codes.New(codes.Unimplemented, "points.<方法> not wired in mesh apps demo")` —— **`Unimplemented` 的唯一落点是 `handler`**（Task 18 的模板口径，别写进 `rpc/`） | `go build ./...` |
| `handler/points.go` / `service/points.go` / `repository/points.go` | 三层各补 `layer.Begin(ctx, "<layer>", "points", "<Op>", traceOn, quiet)`；`repository` 的 `quiet` 对 `gorm.ErrRecordNotFound` 豁免（未命中不是故障） | `grep -c "layer.Begin" handler/points.go service/points.go repository/points.go` 各 ≥ 1 |
| `route/route.go` | **与 order 同款**：身份闸门 + 按主体限流（`subject = "u"+userID`，无身份落 `"svc"`），上限取本域 `middleware` 的 **60/分钟**。**`Admit` MUST NOT 恒返回 `true`** —— 那会让 `RateLimited` 变成死码、`route/` 退化成空目录（spec `mesh-app-layering` 明确要求本目录承担「身份 + 限流」） | `grep -cE "limiter\.Allow" route/route.go` ≥ 1 **且** `grep -c "return true" route/route.go` == 0 |
| `rpc/points.go` | **入向传输适配层**（与 Task 18 的 order 模板同构）：实现 `points.Points` 全部 5 个方法（含编译期断言 `var _ points.Points = (*Server)(nil)`）、开 `rpc.points.<Op>` span、`GetAccount` 经 `route` 闸门后委派 handler；未接线的 4 个方法**只开 span 后原样委派 handler**（不过 `admit` 闸门 —— 理由同 Task 18 的「按方法分档」），`codes.Unimplemented` 由 handler 产出 | `grep -n "var _ points.Points" rpc/points.go` 非空 |
| `app/setup.go` | 多出消费循环的启动与取消：`ctx, cancel := context.WithCancel` → `go consume(ctx)` → `a.OnStop(func() error { cancel(); return nil })`；服务端组装改为 `pointsrpc.NewServer(rpc.New(handler.New(svc), guard, traceOn), opts...)` | `grep -n "OnStop" app/setup.go` 非空 |

差异表逐行落地（Step 2/3 完成后回验）：

Run:
```bash
cd examples/mesh/apps/internal/srv/points
grep -cE "func \((PointAccount|PointsLog)\) TableName" model/points.go   # 期望 2（语义判据，别数裸字样）
grep -cE '(a\.Cache\(\)|a\.Lock\(\)|a\.MQ\(\))' app/setup.go   # 期望 0（句柄越界）
grep -n "OnStop" app/setup.go                             # 期望非空
grep -nE "clause\.OnConflict" repository/points.go            # 期望非空（不是先查后写）
grep -cE "layer\.Begin" handler/points.go service/points.go repository/points.go rpc/points.go   # 期望各 ≥ 1
```

Expected: 依次打印 `2`、`0`，其余 `grep` 有命中；四层各含 `layer.Begin`；
`gofmt -l` 无输出。**不在此步跑 `go build ./...`** —— 差异表尚未落全时编译必然失败，
整体编译归 Step 3 的端到端校验。

- [x] **Step 2: 写消费循环（本域唯一的非机械部分）**

在 `examples/mesh/apps/internal/srv/points/app/setup.go` 的装配段之后加入：

```go
	// 消费循环的起停归业务决策（launchx 不替业务起循环，见 App.MQConsumer 注释：
	// 何时开始消费、并发几路、失败要不要重投都是业务的事）。
	// 这里取最小可用形态：单路消费 + 退出时取消 + 单条坏载荷跳过。
	consumer := a.MQConsumer()
	if consumer == nil {
		// 消费不到消息不会立刻被看见，静默跳过的后果是队列无声地一直涨，故必须 Error。
		logger.Error("srv-points: mq consumer unavailable, points will never grow",
			logx.String("consequence", "积分流水表不会新增"),
			logx.String("probe", "检查 xmiddleware.mqx 段与 consumer.topic"))
	} else {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			// 签名按 mqx 实测：Consume(ctx, handler) 只有两个参数 —— **topic 不是入参**，
			// 它在构造消费端时从 `consumer.topic` 取（见 apps/pkg/events 的键位口径注释）。
			// msg 是 *mqx.Message（指针），载荷在 msg.Value。
			err := consumer.Consume(ctx,
				func(c context.Context, msg *mqx.Message) error {
					e, err := events.UnmarshalOrderCompleted(msg.Value)
					if err != nil {
						logger.Warn("srv-points: bad event payload, skipped", logx.Err(err))
						return nil // 坏载荷不重投：重投只会无限循环
					}
					if err := svc.ConsumeOrderCompleted(c, e); err != nil {
						return err // 业务失败返回 error 让驱动按策略重投
					}
					return nil
				})
			if err != nil && ctx.Err() == nil {
				logger.Error("srv-points: consume loop exited", logx.Err(err))
			}
		}()
		// 退出链：业务回调先于组件停止（此时 DB / MQ 仍可用，本轮处理能收尾）。
		a.OnStop(func() error { cancel(); return nil })
	}
```

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx && grep -nE "type Consumer interface" -A 12 mqx/interface.go
```
Expected（**已实测，直接照此写，不必再猜**）：
`Consume(ctx context.Context, handler func(ctx context.Context, msg *Message) error) error`
—— 两个参数、`*Message` 指针、**无 topic 入参**；`Message` 字段为
`Topic/Key/Value/Headers/Timestamp`。

> **跨 Task 契约（与 Task 8 / Task 11 对齐）**：`srv-points` 的配置里
> **`consumer.topic` MUST 等于 `order.completed.v1`**（即 `events.TopicOrderCompleted`）。
> 键名是 `consumer.topic` 而**不是** `producer.topic` —— mqx/kafka 现状是**两侧都读这一个键**
> （`FillDefaults` 只把该键填给 writer；见 `apps/pkg/events/events.go` 的键位口径注释，
> 2026-10-05 实测）。写错的症状是「消息发了没人收」，而且**两侧日志都不报错**。
> 本地 conf（Task 8）与 K8s ConfigMap（Task 11）都要落这一条。

- [x] **Step 3: 消费链的单元级端到端（运行期归 Task 15）**

本 Task 只做**单元级等价物**：用假 `mqx.Consumer` 驱动整条链，证明「消费到一条
`order.completed.v1` → 积分流水落一行 → 四层 span 齐全且父子相接」。
真起 `srv-order` + `srv-points` 查 MySQL / Jaeger 的运行期校验**归 Task 15** ——
`apps/cmd/**` 与 `apps/conf/**` 是 Task 8 的产物，本 Task 早于它执行（见 amend #5）。

单元级要覆盖的三条：

1. **落库**：喂一条合法 `events.OrderCompleted{OrderID: 42, UserID: 9, TotalAmount: 100}` →
   `points_log` 新增一行且 `order_id = 42`；`points_account.balance` 增加对应分值。
2. **坏载荷不重投**：喂 `{}`（`order_id` 为 0，`UnmarshalOrderCompleted` 会报错）→
   handler 返回 `nil`（**不重投**，否则无限循环），且库内无新行。
3. **span 父子链**：与 Task 18 的 `TestLayerSpanChain` 同构，断言
   `service.points.ConsumeOrderCompleted → repository.points.<Op>` 的
   `child.Parent().SpanID() == parent.SpanContext().SpanID()`。
   **必须做反向自检**（先看它 FAIL 再看它 PASS）。

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx/examples/mesh/apps
CGO_ENABLED=1 go test -count=1 -v ./internal/srv/points/...
CGO_ENABLED=1 go test -count=1 -cover ./internal/srv/points/... ./pkg/codes/
go build ./... && go vet ./... && gofmt -l internal/ pkg/codes/
```
Expected: 三条断言全绿；`model`/`repository`/`service`/`handler`/`route`/`rpc` 各 ≥ 90%
（`app` 装配层以 build/vet 兜底，与 Task 18 口径一致）；build / vet 退出码 0；`gofmt` 无输出。

> **执行期修正（2026-10-07 amend #6 + 质量修复轮 的实际落地记录）**
>
> 已实现的落点：`internal/srv/points/{model,repository,service,handler,route,rpc,app}/**`
> `+ 各包 _test.go`；六个目录的 `.gitkeep` 已删。实测：gofmt / build / vet 全绿；
> `model`/`repository`/`service`/`handler`/`route`/`rpc` 与 `pkg/codes` **全部 100%**，
> `app` 16.0%（装配层，口径内）；`-race` 全绿。
>
> | # | 计划怎么写 | 实际怎么落地 | 为什么 |
> |---|---|---|---|
> | 1 | `grep -n "OnDuplicateKeyUpdate"` 判「不是先查后写」 | 用 `clause.OnConflict{Columns, DoUpdates}` | **gorm 里没有 `OnDuplicateKeyUpdate` 这个标识符**（v1.25/v1.30/v1.31 均无）；`ON DUPLICATE KEY UPDATE balance = balance + ?` 是 **MySQL driver** 对 `clause.OnConflict` 的渲染结果。配套给 `model.PointAccount.UserID` 加了 `uniqueIndex` —— 没有唯一约束时 `ON CONFLICT` 无从命中，会静默退化成每次插新行 |
> | 2 | 句柄只用 db + mqConsumer | 加了 `KV()[name]`（喂 `route` 的限流计数） | amend #6 要求 `route` 的限流是真的 ⇒ 必然要 KV 计数器；句柄矩阵已同步补这一行 |
> | 3 | service 侧方法名 | service 叫 `GetPoints`，public 面是 IDL 名 `GetAccount` | `mesh-entry-chain` 的路由表逐字要求 `service.points.GetPoints`；读路径四层 span 因此是 `rpc.points.GetAccount → handler.points.GetAccount → service.points.GetPoints → repository.points.Balance` |
> | 4 | service 构造 | `service.New(repo, traceOn)`，**不注入 logger** | 本域 service 不触 cache/kv/lock，没有「缺句柄需 Warn」的场景；注入一个不用的 logger 是死依赖 |
> | 5 | 消费闭包内联在 `go func` 里 | 抽成包级 `consumeHandler(svc, logger)` | `Setup` 依赖 `*launchx.App`，外部包构造不出 ⇒ 内联写法无法单测驱动整条链 |
> | 6 | Step 3「起服 + 查 MySQL」 | 改为**单元级**等价物（假 `mqx.Consumer` 驱动） | `apps/cmd/**` 与 `apps/conf/**` 是 Task 8 的产物，本 Task 早于它执行（见 amend #5）；运行期归 Task 15 |
>
> **质量修复轮补的两处**（审查报出、已修）：
> - **毒丸**：`service` 对 `total_amount <= 0` 回 `codes.InvalidArgument`（确定性错误），
>   原先会被 `consumeHandler` 当「可重试的业务失败」外抛 ⇒ 朴素重投即无限循环。
>   现按 `nonRetryable(err)`（**范围刻意收窄**，目前只含 `InvalidArgument`）分类：确定性失败
>   记 Warn 后吞掉（等价死信），可恢复失败才外抛。
> - **退出确定**：`OnStop` 原先只 `cancel()` 就返回，注释「本轮处理能收尾」是过度承诺。
>   现改为 `cancel()` + **有界**等待消费 goroutine 退出（`consumeDrainTimeout = 3s`），
>   超时才 Warn 并继续；注释同步改写。（这条**没有 RED 证据** —— drain 逻辑内联在依赖
>   `*launchx.App` 的 `Setup` 里，不重构结构就测不到，属已知取舍。）

- [x] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 对应行同步）

---

### Task 5: srv-product（缓存侧）

> **trace:** plan-ready.md → `### Task 5: srv-product（缓存侧）` | tasks.md → `- [ ] 1.5 实现 `srv-product`：`DB()`（商品读写）+ `Cache()[name]`（详情缓存，写后失效）；`rpc` 入向适配层实现 `product.Product` 接口全部 6 个方法（`GetProduct` 委派 handler 走业务，`CreateProduct`/`UpdateProduct`/`UpdateStatus`/`ListProducts`/`Report` 显式 `codes.Unimplemented`），四层各开 `*.<域>.*` span。验收：连续两次读同一商品，第二次命中缓存且 `cache.*` span 在同 trace 内`
> **sync:** tasks.md → `- [ ] 1.5 实现 `srv-product`：`DB()`（商品读写）+ `Cache()[name]`（详情缓存，写后失效）；`rpc` 入向适配层实现 `product.Product` 接口全部 6 个方法（`GetProduct` 委派 handler 走业务，`CreateProduct`/`UpdateProduct`/`UpdateStatus`/`ListProducts`/`Report` 显式 `codes.Unimplemented`），四层各开 `*.<域>.*` span。验收：连续两次读同一商品，第二次命中缓存且 `cache.*` span 在同 trace 内` | plan-ready.md → `### Task 5: srv-product（缓存侧）`；spec → `mesh-entry-chain` 的「四域 RPC 实现体必须实现全部 IDL 方法」+「跨域编排只在 bff」；mesh-app-layering 的「四层 span 逐层可见」

**Files:** `examples/mesh/apps/internal/srv/product/{model,repository,service,handler,route,rpc,app}/**`
（`rpc/` 与四层埋点按 **Task 18** 的模板；不再「删除 `rpc/product.go`」）

- [x] **Step 1: 逐文件差异表**

| 文件 | 与 order 的差异 | 校验 |
|---|---|---|
| `model/product.go` | 单表 `products`（`id/title/price/status/category_id/created_at/updated_at`）；无 `order_items`/`order_status_log` | `grep -cE "func \(Product\) TableName" model/product.go` == 1（**语义 grep**：裸 `grep -c "TableName"` 会把注释里的字样一起数，逼人为了凑数改注释 —— Task 4 踩过） |
| `repository/product.go` | 方法集 `FindByID` / `UpdatePrice`；开 `repository.product.*` span（`quiet` 对未命中豁免）。**修正**：原写的 `List` 删掉 —— `mesh-entry-chain` 的 product 行把 `ListProducts` 判为**未接线**（handler 直接回 `Unimplemented`，不过 service），留一个无人调用的仓储方法既是死码、又会把本包覆盖率拖下去 | `go build ./internal/...` |
| `service/product.go` | 句柄 db + cache + kv（`route` 限流计数）；**无 lock / mq**；`GetProduct` 走「读缓存 → 回源 → 回填」，`UpdatePrice`（service 内部名）走「落库 → 删缓存」；开 `service.product.*` span | `grep -cE '(a\.Lock\(\)|a\.MQ\(\))' app/setup.go` == 0 |
| `handler/product.go` | 只做入出参转换（不实现 IDL 接口），开 `handler.product.*` span；5 个未接线方法返回 `codes.New(codes.Unimplemented, "product.<方法> not wired in mesh apps demo")`（**唯一落点在 `handler`**） | `go build ./...` |
| `route/route.go` | 同 order / points：身份闸门 + 按主体限流（本域 **300/分钟**）；**`Admit` MUST NOT 恒真**（理由同 Task 4） | `grep -cE "limiter\.Allow" route/route.go` ≥ 1 **且** `grep -c "return true" route/route.go` == 0 |
| `rpc/product.go` | **入向传输适配层**（同 Task 18 模板）：实现 `product.Product` **全部 6 个方法**（含编译期断言）；`GetProduct` 经闸门后委派 handler，`CreateProduct`/`UpdateProduct`/`UpdateStatus`/`ListProducts`/`Report` **只开 span 后委派 handler**（`Unimplemented` 由 handler 产出，理由见 Task 18 模板口径）（**修正**：早期写的 `UpdatePrice` 不是 IDL 方法，价格更新归 `UpdateProduct`） | `grep -cE 'var _ .*\.Product = \(\*Server\)\(nil\)' rpc/product.go` == 1（别名随实现者定，判据不绑死别名） |
| `app/setup.go` | 无消费循环；`a.Cache()[name]` 传给 service。`name` 必须是 `service.name`（= `srv-product`），与 `cachex.instances` 的键一致，否则拿到 nil；组装 `productrpc.NewServer(rpc.New(handler.New(svc), guard, traceOn), opts...)` | `grep -n "a.Cache()\[name\]" app/setup.go` 非空 |

差异表逐行落地（Step 2 完成后回验）：

Run:
```bash
cd examples/mesh/apps/internal/srv/product
grep -cE "func \(Product\) TableName" model/product.go       # 期望 1（语义 grep，不是裸 "TableName"）
grep -cE '(a\.Lock\(\)|a\.MQ\(\))' app/setup.go              # 期望 0（句柄越界）
grep -n 'a\.Cache()\[name\]' app/setup.go                    # 期望非空
grep -cE "layer\.Begin" handler/product.go service/product.go repository/product.go rpc/product.go   # 期望各 ≥ 1
```

Expected: 依次打印 `1`、`0`，其余两条有命中；`gofmt -l` 无输出。
**不在此步跑 `go build ./...`** —— 整体编译归 Step 2 的编译/测试验证。
（zsh 下 `--include=*.go` 之类的通配会被当成 glob 而报 `no matches found`；
需要按文件名过滤时用 `grep -E` 的多文件列法，或用内置 Grep 工具。）

- [x] **Step 2: 校验缓存真的生效（单元级）**

> **定界（amend #5 对 Task 4–7 一体适用）**：本 Task 早于 Task 8 执行，而 `apps/cmd/**` 与
> `apps/conf/**` 是 **Task 8** 的产物 ⇒ 「真起 `srv-product`」在本位置结构上做不到。
> 故本节只做**单元级等价物**；真起服 + 真看 Jaeger 的**运行期校验归 Task 15**。
> **不得**为了让 Step 打勾而伪造运行期证据。

用假 `cachex.Cache`（内存 map 即可，`cachex.Cache` 是 7 方法的接口）+ sqlite 内存库驱动
`service.GetProduct` 两次，断言：

1. **第二次不回源**：第一次调 repo 1 次、第二次仍为 1 次（假 repo 计调用次数），
   且第二次 `View.Cached == true`；
2. **回填真的发生了**：第一次返回后假 cache 里已有该键（TTL > 0）；
3. **span 在同 trace**：注入内存 span recorder，断言
   `service.product.GetProduct` 与其子 `repository.product.FindByID` 的
   `child.Parent().SpanID() == parent.SpanContext().SpanID()` 且 TraceID 相同；
4. **写后失效**：`UpdatePrice` 成功后假 cache 里该键已被删（详情缓存必须失效，
   否则读路径会拿到旧价）。

**必须做反向自检**（先看它 FAIL 再看它 PASS）：例如把回填那行注释掉 → 第 2 条必须 FAIL；
把 `Delete` 换成 `Set` → 第 4 条必须 FAIL。`go test -overlay` 可做到不改仓库文件。

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx/examples/mesh/apps
CGO_ENABLED=1 go test -count=1 -v ./internal/srv/product/...
CGO_ENABLED=1 go test -count=1 -cover ./internal/srv/product/... ./pkg/codes/
```
Expected: 四条断言全绿；`model`/`repository`/`service`/`handler`/`route`/`rpc` 各 ≥ 90%。
（第 4 条依赖 service 暴露一个**已接线的**写路径；IDL 的 `UpdateProduct` 在本示例未接线，
故 service 内部的 `UpdatePrice` 只由单测驱动，不进 `rpc` 面 —— 这正是「写后失效」在
「对外只读、写未接线」的域里仍能被验到的原因。）

> **执行期修正（2026-10-07/08，Task 5 实际落地记录）**
>
> 已实现的落点：`internal/srv/product/{model,repository,service,handler,route,rpc,app}/**`
> `+ 各包 _test.go`（13 个新文件 + 删 `model/ repository/ service/ handler/ rpc/ app/`
> 的 6 个 `.gitkeep`；`middleware/ratelimit.go` 是 Task 2 产物，mtime 未变、未改）。
> 实测：`gofmt` / `build` / `vet` 全绿；`model`/`repository`/`service`/`handler`/`route`/`rpc`
> 与 `pkg/codes` **六个包 100%**、`service` 97.3%、`app` 0%（装配层，口径内）；`-race` 全绿；
> 47 条单测；根守卫 `go test .` 三条全 PASS。
>
> | # | 计划怎么写 | 实际怎么落地 | 为什么 |
> |---|---|---|---|
> | 1 | `service/product.go` 行写「句柄 db + cache + **kv**（route 限流计数）」 | kv **不进** service，只在 `app/setup.go` 喂 `route.New(middleware.New(a.KV()[name], logger), logger)` | 该行描述的是**域**的句柄面；kv 是限流计数后端，进 service 会让业务层依赖限流组件，与 order / points 的模板不一致（points 的 service 也不拿 kv）。同表 `app/setup.go` 行已写明 kv 的去处 |
> | 2 | 第 2 条断言读「假 repo 的 `FindByID` 调用次数」 | 用 `countingRepo` **包一层真仓储** | 第 3 条要观测 `repository.product.*` span，只有走真仓储才会在链上出现它；包一层同时满足「计数」与「真 span」两个判据 |
> | 3 | 验收原文「`cache.*` span 在同 trace 内」 | 单测只断言 `service` → `repository` 两层的父子链与同 TraceID | `cache.*` 是 **cachex 组件级** span（由 redis 驱动产出）；apps 档没有内存 cachex 驱动（`cachex` 只出 redis / memcached 两个实现），单测起不了真缓存组件 ⇒ 该判据由 **Task 15** 的运行期校验承接 |
> | 4 | 未指定缓存 TTL | `detailTTL = 5 * time.Minute`（带理由注释：写后失效是尽力而为，5 分钟是「失效没删干净」时最坏情况的兜底上界） | 与 order 域详情缓存同口径；不设更长是因为 price 属资金敏感字段 |
> | 5 | 未指定 `UpdatePrice` 落库后是否查 `RowsAffected` | **刻意不查**，并把理由写进注释 | MySQL 默认对「值未变化」的 UPDATE 也报 0 行 ⇒ 0 行二义（不存在 / 值没变），据此报错会把幂等的重复改价变成 404/500。order 的 `UpdateStatus` 查 RowsAffected 是因为它带 `status = ?` 条件、0 行不二义 —— 两者不同款，注释已点明防止后来者「顺手补上」 |
>
> **质量修复轮（审查报出、已修）**：`repository.UpdatePrice` 的「不查 RowsAffected」原先只是**没写**，
> 读代码的人无法区分「刻意」与「漏了」⇒ 补注释写明取舍与反例（上表第 5 行）。修复后
> gofmt / build / vet / 覆盖全部复跑，`repository` 仍 100%。
>
> **登记项（不属本 Task）**：
> - `admit` 的身份来源计数口径四域措辞不一（order 的 Task 18 模板写「四处」含最后的 `"svc"` 桶，
>   points / product 写「三处」+ 在 RateLimited 段里补 `"svc"`）。两者都不算错，但 Task 10 的
>   同构核对会 diff 四个域 ⇒ 留给 **Task 10** 统一措辞。
> - `service` 包 import `gorm` 仅为 `gorm.ErrRecordNotFound` 哨兵值 = Task 18 已登记的 **M3**
>   （order / points 同款），三域一致，不新增背离。
> - `app/setup.go` 无单测（0%）⇒ 按 Task 4/18 口径以 build/vet 兜底（装配层依赖
>   `*launchx.App`，外部包构造不出）。

- [x] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 对应行同步）

---

### Task 6: srv-stock（KV + 锁）

> **trace:** plan-ready.md → `### Task 6: srv-stock（KV + 锁）` | tasks.md → `- [ ] 1.6 实现 `srv-stock`：`DB()`（库存读写）+ `KV()[name]`（原子增减）+ `Lock()`（同商品扣减互斥，未装配时显式声明未走锁路径）；`rpc` 入向适配层实现 `stock.Stock` 接口全部 7 个方法（`GetStock` 委派 handler 走业务，`ListStocks`/`ReserveStock`/`ReleaseStock`/`ConfirmStock`/`AdjustStock`/`Report` 显式 `codes.Unimplemented`），四层各开 `*.<域>.*` span。验收：并发扣减不超卖；缺 `lockx` 段时日志出现「本次未走锁路径」且不启动失败`
> **sync:** tasks.md → `- [ ] 1.6 实现 `srv-stock`：`DB()`（库存读写）+ `KV()[name]`（原子增减）+ `Lock()`（同商品扣减互斥，未装配时显式声明未走锁路径）；`rpc` 入向适配层实现 `stock.Stock` 接口全部 7 个方法（`GetStock` 委派 handler 走业务，`ListStocks`/`ReserveStock`/`ReleaseStock`/`ConfirmStock`/`AdjustStock`/`Report` 显式 `codes.Unimplemented`），四层各开 `*.<域>.*` span。验收：并发扣减不超卖；缺 `lockx` 段时日志出现「本次未走锁路径」且不启动失败` | plan-ready.md → `### Task 6: srv-stock（KV + 锁）`；spec → `mesh-entry-chain` 的「四域 RPC 实现体必须实现全部 IDL 方法」+「跨域编排只在 bff」；mesh-app-layering 的「四层 span 逐层可见」

**Files:** `examples/mesh/apps/internal/srv/stock/{model,repository,service,handler,route,rpc,app}/**`
（`rpc/` 与四层埋点按 **Task 18** 的模板；不再「删除 `rpc/stock.go`」）

- [x] **Step 1: 逐文件差异表**

| 文件 | 与 order 的差异 | 校验 |
|---|---|---|
| `model/stock.go` | 单表 `stocks`（`id/product_id/total/available/reserved/version/updated_at`，**与 IDL 的 `StockInfo` 字段集一一对应**；原写的 `locked` 与 IDL 的 `reserved` 同名不同物，且漏了 IDL 暴露的 `total`/`version`） | `grep -cE "func \(Stock\) TableName" model/stock.go` == 1 |
| `repository/stock.go` | 方法集 `FindByProductID` / `TryDecrease(ctx, productID, n)`：`UPDATE stocks SET available = available - ?, version = version + 1 WHERE product_id = ? AND available >= ?`，以 `RowsAffected` 判定成功；开 `repository.stock.*` span | `grep -n "RowsAffected" repository/stock.go` 非空 |
| `service/stock.go` | 句柄 db + kv + lock；**无 cache / mq**；`GetStock` 读（已接线路径）；`Decrease` 流程＝取锁 → 库层条件扣减 → `kv.Incr` 记当日扣减量。**扣减的真相在库层的条件 UPDATE**，KV 只是旁路计数（失败不影响扣减结果）；开 `service.stock.*` span | `grep -cE '(a\.Cache\(\)|a\.MQ\(\))' app/setup.go` == 0 |
| `handler/stock.go` | 只做入出参转换（不实现 IDL 接口），开 `handler.stock.*` span；6 个未接线方法返回 `codes.New(codes.Unimplemented, "stock.<方法> not wired in mesh apps demo")`（**唯一落点在 `handler`**） | `go build ./...` |
| `route/route.go` | 同 order / points / product：身份闸门 + 按主体限流（本域 **180/分钟**）；**`Admit` MUST NOT 恒真**（理由同 Task 4） | `grep -cE "limiter\.Allow" route/route.go` ≥ 1 **且** `grep -c "return true" route/route.go` == 0 |
| `rpc/stock.go` | **入向传输适配层**（同 Task 18 模板）：实现 `stock.Stock` **全部 7 个方法**（含编译期断言）；`GetStock` 经闸门后委派 handler；`ListStocks`/`ReserveStock`/`ReleaseStock`/`ConfirmStock`/`AdjustStock`/`Report` **只开 span 后委派 handler**（`Unimplemented` 由 handler 产出）（**修正**：早期写的 `Decrease` 不是 IDL 方法 —— 对应的是 `ReserveStock` 一族；且按原写法只实现两个方法，该文件**编译期即失败**） | `grep -nE 'var _ .*\.Stock = \(\*Server\)\(nil\)' rpc/stock.go` 非空 |
| `app/setup.go` | `a.KV()[name]` + `a.Lock()` 进 service；**不**配 `strict.lockx`（锁缺失必须降级而非即退 —— 配置侧要求，落地与校验归 Task 8）；组装 `stockrpc.NewServer(rpc.New(handler.New(svc, traceOn), guard, traceOn), opts...)` | `grep -nE 'a\.(KV|Lock)\(\)' app/setup.go` 非空 |

差异表逐行落地（Step 2/3 完成后回验）：

Run:
```bash
cd examples/mesh/apps/internal/srv/stock
grep -cE "func \(Stock\) TableName" model/stock.go               # 期望 1
grep -cE '(a\.Cache\(\)|a\.MQ\(\))' app/setup.go                  # 期望 0（句柄越界）
grep -nE 'a\.(KV|Lock)\(\)' app/setup.go                          # 期望非空
grep -n "RowsAffected" repository/stock.go                         # 期望非空（以影响行数判成功）
grep -cE "layer\.Begin" handler/stock.go service/stock.go repository/stock.go rpc/stock.go   # 期望各 ≥ 1
```

Expected: 依次打印 `1`、`0`，后三条有命中；四层各含 `layer.Begin`；`gofmt -l` 无输出。
**配置侧**的「`conf/srv-stock.yaml` 只写 `strict.db: true`、不写 `strict.lockx`」是
Task 8 的产物，本 Task 落不到（该文件此刻不存在）—— 归 Task 8 落、Task 15 验。
**不在此步跑 `go build ./...`** —— 整体译验由 Step 2/3 覆盖。

- [x] **Step 2: 锁缺失的降级校验（单元级）**

本 Task 早于 Task 8（`apps/cmd/**` 与 `apps/conf/**` 是 Task 8 产物）⇒ **不得**起进程、
不得用改 yaml 的方式伪造运行期证据；运行期校验归 Task 15。
单测里以 `service.New(repo, kv, lock = nil, logger, traceOn)` 直接驱动：
1. `Decrease` **正常返回**（缺锁不使调用失败）——对应「MUST NOT 因缺锁而启动失败」的执行期内核；
2. logger 收到含 `lock unavailable` 的 Warn（用内存 logger 断言文案与字段，
   文案沿用 order 模板前缀改成 `mesh-stock:`）——对应「MUST NOT 静默当作已加锁」；
3. 对照组：注入真锁（内存假 `lockx.Locker`）时**不出现**该 Warn。

Expected: 三条全绿。第 2 条在 `service/stock_test.go` 里断言，不看运行日志。

- [x] **Step 3: 并发不超卖校验（单元级，两层判据）**

运行期的「20 并发打同一 `product_id`」需要进程与真库（Task 8/15）；本 Task 用单元级的两层判据，
**并且如实标注每层的强度**（不把弱判据说成强判据）：
1. **repository 层的条件语义（强判据，单线程即可验）**：
   `available = 10` 时 `TryDecrease(pid, 11)` 返回「未命中」且行值不变；
   `TryDecrease(pid, 10)` 成功、`available` 归 0、`version` +1；再扣 1 仍「未命中」。
   —— 若把 UPDATE 写成无条件的 `available = available - ?`，这条会 FAIL，故它是**真判据**。
2. **service 层的并发计数（端到端判据，强度受限）**：
   `available = 10`，起 20 个 goroutine 各调 `Decrease(pid, 1)`，断言成功数为 **10**、
   `available` 终值为 **0** 且不为负。⚠️ **如实说明**：sqlite 内存库会把写连接串行化，
   这层的通过**部分**来自串行化而非条件 UPDATE；它证明的是「并发下终值正确、无负数」，
   不是「条件 UPDATE 是唯一护栏」—— 后者由第 1 层承担。`-race` 下必须无数据竞争。

Expected: 两层全绿；`go test -race` 干净。

**执行期修正（2026-10-07 amend #8 + 实现期）**

| # | 计划怎么写 | 实际怎么落地 | 为什么 |
|---|---|---|---|
| 1 | `TryDecrease` 返回值「形态自定但注释写清」 | `(ok bool, err error)`：`ok=false,err=nil` = 未命中（库存不足/无该商品）；`err!=nil` = 库错 | 必须让上层区分「库存不足(4xx)」与「库坏了(5xx)」，两者映射不同 |
| 2 | `Decrease` 返回 `remaining int32` | 扣减成功后再 `FindByProductID` **观测**剩余量；观测失败只 Warn 回 0，**不回退已提交的扣减** | 否则一次观测读的抖动会让调用方以为没扣成功而重试，反而多扣 |
| 3 | `UpdatedAt` 维护方式「自己定」 | **显式写** `updated_at`（`Updates(map)` + `gorm.Expr` 表达原子表达式） | gorm 的自动维护只对**结构体**形式的 Updates 生效，map 形式不生效 |
| 4 | `rpc/stock.go` 的 `admit(ctx, userID)` | `admit(ctx)` —— **去掉** userID 参数 | `GetStockReq` 只有 `product_id`、**不带 user_id**，本域没有「回落请求 IDL 的 user_id」这一级。硬造一个只会把匿名流量错记到商品维度 |
| 5 | `codes` 里没有 `StockNotFound` | 读路径「无库存记录」→ **零值视图而非错误**（对齐 `points.GetPoints` 的「未开户=0」） | 不改 `pkg/codes`；`available=0` 在业务上就是扣不动，读接口不该因「还没建库存行」失败 |

**sqlite 并发的实测记录（影响第 2 层判据的强度，必须说清）**：`SetMaxOpenConns(1)` 是必需的 ——
不加它，`:memory:` 的**每连接独立内存库**会让池扩出的第二条连接看到空库（实测报
`no such table: stocks`，成功数 1/3）。加了它也意味着**写被串行化**：故
`TestDecreaseConcurrentNoOversell` 的通过**部分是**由串行化贡献的，它证明的是「并发下终值正确、
无负数、无 race」，**不是**「条件 UPDATE 是唯一护栏」。后者由 repository 层的**单线程**条件语义
（`TestTryDecreaseConditionalBlocksOversell` + 反向探针 1）承担 —— 无条件 UPDATE 在单线程下即暴露。

**登记项（顺带，属 Task 7 / Task 8）**：`StockNotEnough`(50300) 是本域 `Decrease` 的真实生产者，
但 bff 的错误码映射（`{Order,Product}NotFound→404` 那张表）**没有 stock 的 not-enough 档**
⇒ Task 7 需为它定一个映射（建议 409 或 400），否则该码在 HTTP 边界没有出口。
`stocks` 表 DDL 同 `products`，属 `order_db.sh` 的缺项（见 amend #7/#8 末条）。

- [x] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 对应行同步）

---

### Task 7: bff（HTTP 边界）

> **trace:** plan-ready.md → `### Task 7: bff（HTTP 边界）` | tasks.md → `- [ ] 1.7 实现 `bff`：`Engine()`（webx 路由 + 健康端点）+ `Cache()[name]`（边界凭据存储 `token → user_id`）+ `RPCClientOptions(<域>)`（四域 kitex 客户端，resolver 落到 K8s Service 名）+ `pkg/routing` 三层中间件链。**不取** `DB()` / `MQ()`。路由表 **6 条**（见 `mesh-entry-chain`）：`GET /ping`、`GET /order/:id`、`POST /order`、`GET /points/:user_id`、`GET /product/:id`、`GET /stock/:product_id` —— `Clients` 的四个字段**每个至少一个调用点，不留死客户端**。验收：`/healthz/live`、`/healthz/ready` 返回 200；六条路由各返回 2xx；`grep -cE 'clients\.(Order|Points|Product|Stock)' internal/bff/**` 四名各有命中；错误码映射表含 `InvalidArgument→400` / `Unauthorized→401` / `{Order,Product}NotFound→404` / `RateLimited→429` / `Unimplemented→501` / `Internal→502``
> **sync:** tasks.md → `- [ ] 1.7 实现 `bff`：`Engine()`（webx 路由 + 健康端点）+ `Cache()[name]`（边界凭据存储 `token → user_id`）+ `RPCClientOptions(<域>)`（四域 kitex 客户端，resolver 落到 K8s Service 名）+ `pkg/routing` 三层中间件链。**不取** `DB()` / `MQ()`。路由表 **6 条**（见 `mesh-entry-chain`）：`GET /ping`、`GET /order/:id`、`POST /order`、`GET /points/:user_id`、`GET /product/:id`、`GET /stock/:product_id` —— `Clients` 的四个字段**每个至少一个调用点，不留死客户端**。验收：`/healthz/live`、`/healthz/ready` 返回 200；六条路由各返回 2xx；`grep -cE 'clients\.(Order|Points|Product|Stock)' internal/bff/**` 四名各有命中；错误码映射表含 `InvalidArgument→400` / `Unauthorized→401` / `{Order,Product}NotFound→404` / `RateLimited→429` / `Unimplemented→501` / `Internal→502`` | plan-ready.md → `### Task 7: bff（HTTP 边界）`；spec → `mesh-entry-chain` 的「bff 的路由表必须与四域一一对应」（本 Task 是它的落地动作）

**Files:** `examples/mesh/apps/internal/bff/{downstream/clients.go,handler/mall.go,app/setup.go}`

> **amend 提示**：本 Task 的 handler 由 2 条路由扩写为 **6 条**，使四域业务逻辑都能从
> 入口触达。六条路由与下游的对应关系是 Task 15 验收矩阵的**唯一来源**，两处 MUST 保持一致：

| 路由 | 下游 | 目标域 service 层 |
|---|---|---|
| `GET /ping` | — | —（单跳自响应，trace 判据豁免） |
| `GET /order/:id` | `clients.Order.GetOrder` | `service.order.GetOrder`（内部再调 product / stock） |
| `POST /order` | `clients.Order.CreateOrder` | `service.order.CreateOrder` → 事件投递 |
| `GET /points/:user_id` | `clients.Points.GetAccount` | `service.points.GetPoints` |
| `GET /product/:id` | `clients.Product.GetProduct` | `service.product.GetProduct` |
| `GET /stock/:product_id` | `clients.Stock.GetStock` | `service.stock.GetStock` |

- [x] **Step 1: 写 downstream（客户端工厂）**

> 落地口径与示例代码的两处差异（都不改设计，只对齐实际形态）：
> ① `Clients` 的四个字段声明成**窄接口**（`OrderClient` / `PointsClient` / `ProductClient` /
> `StockClient`，只含 bff 真正调用的方法），不是 kitex 生成的宽 `Client` —— 生成的 `Client`
> 天然满足窄接口，赋值无需适配层，收益是「客户端建了没用」在编译期就能发现，且单测假件
> 只需 1~2 个方法而不是 22 个空实现；
> ② 每个客户端的选项叠加 `rpcwire.ClientOptions()`（TTHeader handler）—— 没有它，下游回的
> 业务码会被 kitex 降级成一段字符串，bff 的错误码映射整张失效（见 `pkg/rpcwire` 包注释）。

`examples/mesh/apps/internal/bff/downstream/clients.go`：

```go
// Package downstream 四域 RPC 客户端。
//
// 与 micro 的 bff/internal/downstream 的差别：**没有 newClient 闭包里的
// 「注册中心地址 + 类型断言 + 静默兜底」三件套（约 60 行）**。
// apps 下 launchx 的 clientOptionsFactory 读到 RegistryAddr 为空，直接走
// service.upstream.<域> 的 K8s Service FQDN（kitex DNS 解析 + Envoy 负载均衡），
// 业务侧只需要把 opts 传进去。
package downstream

import (
	"fmt"

	"github.com/cloudwego/kitex/client"

	"github.com/gospacex/launchx"
	orderrpc "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/order/order"
	pointsrpc "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/points/points"
	productrpc "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/product/product"
	stockrpc "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/stock/stock"
)

// Clients 四域客户端集合。
type Clients struct {
	Order   orderrpc.Client
	Points  pointsrpc.Client
	Product productrpc.Client
	Stock   stockrpc.Client
}

// New 按域名建四个客户端。
//
// 域名常量 MUST 与 conf/bff.yaml 的 service.upstream.<域> 键、以及各域
// service.name 三处完全一致 —— 任一处不一致的后果都不是报错，而是
// 「客户端建好了但没有目标」，直到第一次调用才炸。
func New(a *launchx.App) (*Clients, error) {
	optsOf := func(domain string) ([]client.Option, error) {
		v, err := a.RPCClientOptions(domain)
		if err != nil {
			return nil, fmt.Errorf("bff: rpc client options for %s: %w", domain, err)
		}
		opts, ok := v.([]client.Option)
		if !ok {
			return nil, fmt.Errorf("bff: rpc client options for %s is %T, want []client.Option", domain, v)
		}
		return opts, nil
	}

	c := &Clients{}
	var err error

	oOpts, err := optsOf("srv-order")
	if err != nil {
		return nil, err
	}
	if c.Order, err = orderrpc.NewClient("srv-order", oOpts...); err != nil {
		return nil, err
	}

	pOpts, err := optsOf("srv-points")
	if err != nil {
		return nil, err
	}
	if c.Points, err = pointsrpc.NewClient("srv-points", pOpts...); err != nil {
		return nil, err
	}

	prOpts, err := optsOf("srv-product")
	if err != nil {
		return nil, err
	}
	if c.Product, err = productrpc.NewClient("srv-product", prOpts...); err != nil {
		return nil, err
	}

	sOpts, err := optsOf("srv-stock")
	if err != nil {
		return nil, err
	}
	if c.Stock, err = stockrpc.NewClient("srv-stock", sOpts...); err != nil {
		return nil, err
	}

	return c, nil
}
```

Run: `cd examples/mesh/apps && gofmt -l internal/bff && go build ./internal/...`
Expected: `gofmt` 无输出；`go build` 退出码 0。

- [x] **Step 2: 写 handler（HTTP 出入口，零业务判断）**

> 落地口径与示例代码的四处差异（前两处是本 Step 要求的「按实际签名调整」）：
> ① 回调签名用 hertz 的 `*app.RequestContext` —— **webx 没有**自己的 `RequestContext` 类型
> （`webx.go` 只内嵌 `*server.Hertz`，路由与中间件直接用 hertz 类型）；
> ② 请求体用 `c.BindJSON(&in)` —— 示例的 `json.NewDecoder(c.Request().Body)` 不可编译
> （`Body` 是方法，不是 `io.Reader`）；
> ③ 出向调用前经 `withIdentity(ctx)` 把主体写进 metainfo（跨进程身份的唯一载体），
> 且 `createOrder` 的归属用户**身份压过请求体**（否则改个 user_id 就能以别人名义下单）；
> ④ 每条路由开 `handler.bff.<Op>` 分层 span（OpenSpec `mesh-app-handles` 的「句柄动作入链」
> 要求 trace 内可见「bff 的 handler span」，webx 的根 span 名是 `<METHOD> <path>`，不带分层形态）。
> 错误码还原 MUST 用 `rpcwire.FromRPC` 而不是 `codes.From`（理由同 Step 1 ②）。

`examples/mesh/apps/internal/bff/handler/mall.go`：

```go
// Package handler HTTP 出入口：解析参数 → 调下游 → 序列化。
// 零业务判断（不出现「金额大于多少就…」这类分支），与 srv 侧的 handler 同纪律。
//
// 六条路由 MUST 覆盖四域（见 mesh-entry-chain）：任何一个 Clients 字段没有调用点，
// 就意味着那个域的业务逻辑永远无法从入口触达 —— 编译器不会提醒这件事。
package handler

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/gospacex/webx"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/bff/downstream"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	orderrpc "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/order/order"
	pointsrpc "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/points/points"
	productrpc "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/product/product"
	stockrpc "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/stock/stock"
)

// MallHandler 聚合入口。
type MallHandler struct {
	clients *downstream.Clients
}

// New 构造 handler。
func New(c *downstream.Clients) *MallHandler { return &MallHandler{clients: c} }

// Register 把六条路由挂到 webx 引擎上。
//
// 入参类型用 webx 的 RequestContext（不是 hertz 的 app.RequestContext）：
// 引擎经 App.Engine() 以 any 交出来，业务断言成 *webx.App —— 与 launchx
// 库源码不 import hertz 是同一纪律。webx 已自带 /healthz/{live,ready}，
// 无需在此重复注册（探针用的就是它）。
func (h *MallHandler) Register(engine *webx.App) {
	engine.GET("/ping", h.ping)
	engine.GET("/order/:id", h.getOrder)
	engine.POST("/order", h.createOrder)
	engine.GET("/points/:user_id", h.getPoints)
	engine.GET("/product/:id", h.getProduct)
	engine.GET("/stock/:product_id", h.getStock)
}

func (h *MallHandler) ping(_ context.Context, c *webx.RequestContext) {
	c.JSON(200, map[string]string{"msg": "pong"})
}

// getOrder 聚合查单：入参解析 → 调 order 域 → 序列化。
// 订单明细由 order 的 service 内部经 rpc/order.go 向 product / stock 补全 ——
// 所以这一条路由的 trace 里应当同时出现 order / product / stock 三个域的 service span。
func (h *MallHandler) getOrder(ctx context.Context, c *webx.RequestContext) {
	id, err := parseUint(c.Param("id"))
	if err != nil {
		writeErr(c, codes.New(codes.InvalidArgument, "bad order id"))
		return
	}
	resp, err := h.clients.Order.GetOrder(ctx, &orderrpc.GetOrderReq{OrderId: id})
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(200, resp)
}

// createOrder 下单：**`order → MQ → points` 异步链的唯一入口**，
// 也是 Task 15 验收矩阵里「异步链」那一行的触发点。
// 本示例不带 outbox，投递失败只告警（见 apps/README「删不掉的四类」）。
func (h *MallHandler) createOrder(ctx context.Context, c *webx.RequestContext) {
	var in struct {
		UserID int64 `json:"user_id"`
		Items  []struct {
			ProductID int64  `json:"product_id"`
			Name      string `json:"name"`
			Quantity  int32  `json:"quantity"`
			UnitPrice int64  `json:"unit_price"`
		} `json:"items"`
	}
	if err := json.NewDecoder(c.Request().Body).Decode(&in); err != nil {
		writeErr(c, codes.Wrap(codes.InvalidArgument, "bad body", err))
		return
	}
	if in.UserID == 0 || len(in.Items) == 0 {
		writeErr(c, codes.New(codes.InvalidArgument, "user_id and items are required"))
		return
	}
	items := make([]*orderrpc.OrderItem, 0, len(in.Items))
	for _, it := range in.Items {
		items = append(items, &orderrpc.OrderItem{
			ProductId: it.ProductID, Name: it.Name,
			Quantity: it.Quantity, UnitPrice: it.UnitPrice,
		})
	}
	resp, err := h.clients.Order.CreateOrder(ctx, &orderrpc.CreateOrderReq{
		UserId: in.UserID, Items: items,
	})
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(200, resp)
}

// getPoints 积分只读路径（points 域的写路径在 MQ 消费侧，不经 HTTP）。
func (h *MallHandler) getPoints(ctx context.Context, c *webx.RequestContext) {
	uid, err := parseUint(c.Param("user_id"))
	if err != nil {
		writeErr(c, codes.New(codes.InvalidArgument, "bad user_id"))
		return
	}
	resp, err := h.clients.Points.GetAccount(ctx, &pointsrpc.GetAccountReq{UserId: uid})
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(200, resp)
}

// getProduct 商品详情（srv-product 侧走「读缓存 → 回源 → 回填」）。
func (h *MallHandler) getProduct(ctx context.Context, c *webx.RequestContext) {
	id, err := parseUint(c.Param("id"))
	if err != nil {
		writeErr(c, codes.New(codes.InvalidArgument, "bad product id"))
		return
	}
	resp, err := h.clients.Product.GetProduct(ctx, &productrpc.GetProductReq{ProductId: id})
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(200, resp)
}

// getStock 库存查询。
func (h *MallHandler) getStock(ctx context.Context, c *webx.RequestContext) {
	pid, err := parseUint(c.Param("product_id"))
	if err != nil {
		writeErr(c, codes.New(codes.InvalidArgument, "bad product_id"))
		return
	}
	resp, err := h.clients.Stock.GetStock(ctx, &stockrpc.GetStockReq{ProductId: pid})
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(200, resp)
}

// parseUint 路径参数解析。抽出来只因为六条路由里有四条做同一件事 ——
// 其余一律不抽（业务判断一条都不抽，这是分层纪律）。
func parseUint(s string) (int64, error) {
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, err
	}
	return int64(v), nil
}

// writeErr 把业务码映射为 HTTP 状态。映射表只在 bff 有 —— srv 侧不关心 HTTP。
func writeErr(c *webx.RequestContext, err error) {
	code, msg := codes.From(err)
	status := 500
	switch code {
	case codes.InvalidArgument:
		status = 400
	case codes.Unauthorized:
		status = 401 // 身份缺失/非法：重试一万次也是同一个结果，别把鉴权故障伪装成容量问题
	case codes.OrderNotFound, codes.ProductNotFound:
		status = 404
	case codes.RateLimited:
		status = 429 // 被限流：值得退避重试（与 401 分开正是为让客户端重试策略能区分）
	case codes.Unimplemented:
		status = 501 // 下游明确声明「本示例未实现」：如实透出，不要伪装成 404/502
	case codes.Internal:
		status = 502 // 下游失败：对客户端是「上游坏了」，不是「服务端崩了」
	}
	c.JSON(status, map[string]any{"code": int32(code), "error": msg})
}
```

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx && grep -rnE "func \(.*RequestContext\) (Param|JSON|Bind|Body|Request)\(" webx/*.go | head
```
Expected: 打印 webx 的取参 / 绑定 / 取原始请求的方法真实签名。**按实际签名调整**
`c.Param(...)` 与 `json.NewDecoder(c.Request().Body)`（本 Step 唯一需要现场核对处；
若 webx 不暴露原始 `*http.Request`，改用它的绑定方法 `c.BindJSON(&in)` 并去掉
`encoding/json` 的 import），之后

Run: `cd examples/mesh/apps && gofmt -l internal/bff && go build ./internal/...`
Expected: `gofmt` 无输出；`go build` 退出码 0。

- [x] **Step 2b: 确认没有死客户端**

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx/examples/mesh/apps
grep -c 'clients\.Order'   internal/bff/handler/mall.go
grep -c 'clients\.Points'  internal/bff/handler/mall.go
grep -c 'clients\.Product' internal/bff/handler/mall.go
grep -c 'clients\.Stock'   internal/bff/handler/mall.go
```
Expected: 四个计数**均 ≥ 1**（`Order` 应 ≥ 2）。任一为 0 即说明该域的客户端建了没用，
对应的路径在 Task 15 的路由矩阵里必然缺失 —— 这是本 Step 存在的全部理由。

- [x] **Step 3: 写 app/setup.go**

> 落地口径与示例代码的三处差异：
> ① **`engine.Use(...)` 必须在 `Register(engine)` 之前** —— hertz 的 `RouterGroup.Use` 只是
> `Handlers = append(Handlers, mw...)`，而路由在被注册的那一刻就把 `Handlers` 快照进自己的链，
> 先注册再 `Use` 会让中间件对已注册路由完全不起作用（且不报错）。示例的顺序恰好是对的，
> 这条注释是为了防止后来者调换；
> ② identity 层装上**真实解析器**：`routing.Chain(routing.WithIdentityResolver(
> routing.NewTokenResolver(store, logger)))`，store 取 `a.Cache()[service.name]`；
> ③ 增一道 `store == nil` 的显式判断 —— `VerifyOrError(a, "cache")` 判的是「实例在」，
> 而 `store` 为 nil 时 `NewTokenResolver` 返回 nil resolver，是**静默**退化（身份层照常开 span、
> 请求照常放行，只是永远认不出人）。

`examples/mesh/apps/internal/bff/app/setup.go`：

```go
// Package app bff 装配：Engine（路由 + webx 自带健康端点）+ Cache（凭据存储）
// + 四域 RPC 客户端。
//
// 刻意不取 DB() 与 MQ()：bff 是 HTTP 边界，不落业务数据、不发业务事件。
// 它唯一的数据入口是下游四域客户端与自己的凭据缓存（后者存的不是业务数据）。
package app

import (
	"errors"

	"github.com/gospacex/logx"
	"github.com/gospacex/webx"

	"github.com/gospacex/launchx"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/bff/downstream"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/bff/handler"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/handles"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/routing"
)

// Setup 注册给 App.OnSetup。
func Setup(a *launchx.App) error {
	engine, ok := a.Engine().(*webx.App)
	if !ok || engine == nil {
		return errors.New("bff: webx engine unavailable (check service.mode=bff)")
	}
	// bff 的硬前提是凭据缓存：缺了它所有请求都会 401，比启动失败更难查。
	if err := handles.VerifyOrError(a, "cache"); err != nil {
		return errors.New("bff: " + err.Error())
	}
	for _, c := range handles.Verify(a) {
		if c.Present {
			a.Logger().Info("handle verified",
				logx.String("component", c.Component), logx.String("backend", c.Backend),
				logx.String("probe", c.Probe))
		}
	}

	clients, err := downstream.New(a)
	if err != nil {
		return err
	}

	// 中间件链：audit → identity → rate_limit（顺序见 pkg/routing 注释）。
	// 这一层不是「网关的替代品」—— 入口转发已交给 Istio；这里只保留收敛在 bff
	// 的边界职责（身份透传、审计、按主体的粗粒度限流）。
	engine.Use(routing.Chain()...)

	handler.New(clients).Register(engine)

	a.Logger().Info("bff assembled",
		logx.String("service", a.Config().GetString("service.name")),
		logx.String("chain", "audit>identity>rate_limit>handler>downstream"))
	return nil
}
```

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx && grep -rnE "func \(.*\*App\) Use\(|Middleware" webx/webx.go | head
```
Expected: 打印 webx 的中间件挂载方法真实签名。
**按实际签名调整** `engine.Use(...)`（若 webx 没有 `Use`，改为在 `webx.New` 的 options
里传链或直接在 handler 内包一层），之后
`cd examples/mesh/apps && go build ./... && go vet ./...` 退出码 0。

- [x] **Step 4: 本地校验健康端点与六条路由**

> **本 Step 未跑（不能跑）**：`apps/cmd/**` 与 `apps/conf/**` 是 **Task 8** 的产物，
> 本 Task 执行时没有可起的二进制；真起服 + 真四域下游的 curl 矩阵归 **Task 15**。
> **不得**为了让 Step 打勾而伪造运行期证据。
>
> 本 Step 在单元级有**等价物**（已全绿，见 `internal/bff/handler/mall_test.go`）：
> `TestRegisterCoversSixRoutes` 对着真 webx 引擎的 `Routes()` 断言六条路由模板
> （`GET /ping` / `GET /order/:id` / `POST /order` / `GET /points/:user_id` /
> `GET /product/:id` / `GET /stock/:product_id`）逐条在册；
> `TestRoutesReachTheirDomain` 逐路由真跑一遍 handler，断言「对应域的客户端调用数 +1」
> 且 HTTP 200 —— 它比 curl 判据更强的一点是能抓住「HTTP 200 但打错了域」；
> `/healthz/{live,ready}` 由 webx 自带（`webx.registerHealth`，无需在本 Task 注册）。
> 唯一留下的是「真数据库里有没有数据」（`order_db.sh` 种子）与真 MQ/缓存的运行期表现。

Run: 起四域 `srv-*` 与 `bff`（本地 `conf/`）后（`bff` 的 webx 端口按 `conf/bff.yaml`，
下面以 8081 为例）：
```bash
curl -s -o /dev/null -w 'live=%{http_code}\n'  localhost:8081/healthz/live
curl -s -o /dev/null -w 'ready=%{http_code}\n' localhost:8081/healthz/ready
curl -s -o /dev/null -w 'ping=%{http_code}\n'  localhost:8081/ping
curl -s -o /dev/null -w 'order=%{http_code}\n' localhost:8081/order/2
curl -s -o /dev/null -w 'points=%{http_code}\n' localhost:8081/points/1
curl -s -o /dev/null -w 'product=%{http_code}\n' localhost:8081/product/1
curl -s -o /dev/null -w 'stock=%{http_code}\n' localhost:8081/stock/1
curl -s -o /dev/null -w 'create=%{http_code}\n' -X POST localhost:8081/order \
  -H 'Content-Type: application/json' \
  -d '{"user_id":1,"items":[{"product_id":1,"name":"demo","quantity":1,"unit_price":100}]}'
```
Expected: `live=200`、`ready=200`（webx 自带，无需自建）；`ping=200`；
**六条业务路由全部 2xx**（`order` / `points` / `product` / `stock` / `create`）。
任一条非 2xx，先看它的响应体里的 `code` 字段：
`20001`（order 不存在）/ `40004`（product 不存在）是**数据未初始化**而非接线错误 ——
用 `bash examples/scripts/order_db.sh up` 补种子数据即可；
若是连接错误，说明该域的 `service.upstream.<域>` 在本地 `conf/` 里没配。

- [x] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 对应行同步）

---

### Task 8: 入口与配置（五进程可本地起服）

> **trace:** plan-ready.md → `### Task 8: 入口与配置（五进程可本地起服）` | tasks.md → `- [ ] 1.8 补齐五个 `cmd/<svc>/main.go` 与五份 `conf/<svc>.yaml`：main ≤ 30 行（Bootstrap + OnSetup + Run 协程 + 信号 → `App.Close()`）；配置的地址段一律写集群内可达名，无 `xmiddleware.coordx` 段，`srv-*` 配 `strict.db: true`。验收：`gofmt -l` 干净、`go vet ./...` 通过、五个二进制均能以本地 `conf/` 起服`
> **sync:** tasks.md → `- [ ] 1.8 补齐五个 `cmd/<svc>/main.go` 与五份 `conf/<svc>.yaml`：main ≤ 30 行（Bootstrap + OnSetup + Run 协程 + 信号 → `App.Close()`）；配置的地址段一律写集群内可达名，无 `xmiddleware.coordx` 段，`srv-*` 配 `strict.db: true`。验收：`gofmt -l` 干净、`go vet ./...` 通过、五个二进制均能以本地 `conf/` 起服` | plan-ready.md → `### Task 8: 入口与配置（五进程可本地起服）`

**Files:** `examples/mesh/apps/cmd/{bff,srv-order,srv-points,srv-product,srv-stock}/main.go`、
`examples/mesh/apps/conf/{bff,srv-order,srv-points,srv-product,srv-stock}.yaml`

- [x] **Step 1: 写五个 main.go（同一份，仅 import 与 configPath 不同）**

`examples/mesh/apps/cmd/srv-order/main.go`：

```go
// Package main 订单域（srv 档）入口：Bootstrap → OnSetup → Run。
//
// 本文件不开连接、不装配 exporter、不注册节点 —— 那些都在 launchx 与
// app.Setup 里。它与 examples/micro/srv-order/cmd/main.go 逐字相同（除 import
// 路径与 configPath），这正是「网格形态不需要为形态改代码」的直接证据。
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/gospacex/launchx"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/app"
)

const configPath = "conf/srv-order.yaml"

func main() {
	a, err := launchx.Bootstrap(configPath)
	if err != nil {
		log.Fatalf("bootstrap: %v", err)
	}
	a.OnSetup(app.Setup)

	// Run 阻塞监听故放协程；信号到来时显式走 App.Close 退出链
	// （server.stop → … → logger.close），否则最后一批 span 来不及冲刷。
	runErr := make(chan error, 1)
	go func() { runErr <- a.Run() }()

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

其余四份：替换 `configPath`（`conf/srv-points.yaml` / `conf/srv-product.yaml` /
`conf/srv-stock.yaml` / `conf/bff.yaml`）与 `app` 的 import 路径
（`internal/srv/points/app` / `internal/srv/product/app` / `internal/srv/stock/app` /
`internal/bff/app`），包注释里的域名同步改。

Run:
```bash
cd examples/mesh/apps && gofmt -l cmd && go build ./... && wc -l cmd/*/main.go
# main **函数**的行数（本 Task 真正要守的那条）：
awk '/^func main\(\)/{f=1} f{c++} f&&/^}/{print FILENAME": main="c; exit}' cmd/*/main.go
```
Expected: `gofmt` 无输出；`go build` 退出码 0；五份 `main` 函数均 **31 行**
（扣掉 2 行注释与 3 行空行后 **有效语句 26 行**）。**口径更正**：`wc -l` 得 55/文件 ——
「≤ 30 行」指的是 `main` **函数**而不是文件（本处给出的模板本身就 31 行；
按文件数会把这份模板自己判成不合格）。

- [x] **Step 2: 写 conf/bff.yaml**

`examples/mesh/apps/conf/bff.yaml`（本地起服可用 `127.0.0.1` 覆盖地址；下列值面向集群）：

```yaml
# bff（apps 组）配置。纪律：地址一律来自本文件，Go 代码零硬编码。
# 与 micro 的 bff/config.yaml 的差异只有三处：
#   1. 无 xmiddleware.coordx（apps 不注册、不发现）；
#   2. service.upstream.<域> 从「直连兜底」变成唯一地址来源，值为 K8s Service FQDN；
#   3. 中间件地址走宿主门面名 host-middleware（本机起服时改成 127.0.0.1）。
service:
  name: bff
  mode: bff
  addr: "0.0.0.0:8081"
  upstream:
    srv-order:   "srv-order.launchx-mesh.svc.cluster.local:9022"
    srv-points:  "srv-points.launchx-mesh.svc.cluster.local:9024"
    srv-product: "srv-product.launchx-mesh.svc.cluster.local:9021"
    srv-stock:   "srv-stock.launchx-mesh.svc.cluster.local:9023"

webx:
  addr: "0.0.0.0:8081"
  timeout: 5s
  rate_limit_rps: 100
  rate_limit_burst: 200
  cors_origins: []
  health_prefix: /healthz

trace:
  enabled: true
  endpoint: "host-middleware.launchx-mesh.svc.cluster.local:4318"
  on:
    cache: true

logx:
  level: info
  otel:
    enable_trace: true
    exporter: otlp
    endpoint: "host-middleware.launchx-mesh.svc.cluster.local:4318"

xmiddleware:
  cachex:
    driver: redis
    enable_trace: true
    instances:
      bff:
        addr: "host-middleware.launchx-mesh.svc.cluster.local:6379"
        db: 2
        pool_size: 30
```

Run:
```bash
cd examples/mesh/apps
grep -nE '^[[:space:]]*(mode|service|name):' conf/bff.yaml     # 期望 mode: bff / name: bff
grep -cE '(127\.0\.0\.1|localhost)' conf/bff.yaml             # 期望 0（无裸本机地址）
grep -n "coordx" conf/bff.yaml                                # 期望零命中（apps 侧零注册）
```

Expected: 三条都符合右侧注释；`gofmt` 无关（yaml）。地址段在本地起服时用不上，
但必须以集群内可达名落盘 —— 本地联调改用 `127.0.0.1` 属临时改动，不得落回文件。

> **实测补充**：这条自检要**连注释一起守**。`bring-up.sh check` 的判据是
> `grep -rn "127\.0\.0\.1" … --include="*configmap*.yaml"`（只排除纯注释行），
> 而 Task 11 把 `conf/*.yaml` 的内容原样搬进 ConfigMap —— 注释里出现字面地址同样会被
> 拦下。故本文本的注释一律写作「回环地址」而不写那个字面量；`coordx` 同理
> （写成「注册中心配置段」）。

- [x] **Step 3: 写 conf/srv-order.yaml，并给出另三域的差异**

`examples/mesh/apps/conf/srv-order.yaml`：

```yaml
# 订单域（apps 组）。与 micro 同名配置的差异：无 coordx 段、无 service.self.addr、
# 中间件地址走宿主门面名、strict.db 保持 true（库连不上即退，
# 别降级成「起来但落不了库」）。
service:
  name: srv-order
  mode: srv
  addr: "0.0.0.0:9022"

trace:
  enabled: true
  endpoint: "host-middleware.launchx-mesh.svc.cluster.local:4318"
  on:
    db: true
    cache: true
    kv: true
    mq: true
    lock: true

logx:
  level: info
  otel:
    enable_trace: true
    exporter: otlp
    endpoint: "host-middleware.launchx-mesh.svc.cluster.local:4318"

strict:
  db: true

xmiddleware:
  sqlx:
    driver: gorm
    databases:
      srv-order:
        dsn: "order_rw:${HUBX_DB_PW}@tcp(host-middleware.launchx-mesh.svc.cluster.local:3306)/order_db?charset=utf8mb4&parseTime=True&loc=Local"
        max_open_conns: 20
        max_idle_conns: 5
        tracing:
          enabled: true
  cachex:
    driver: redis
    enable_trace: true
    instances:
      srv-order:
        addr: "host-middleware.launchx-mesh.svc.cluster.local:6379"
        db: 0
  kvx:
    driver: redis
    enable_trace: true
    instances:
      srv-order:
        addr: "host-middleware.launchx-mesh.svc.cluster.local:6379"
        db: 1
  lockx:
    driver: redis
    addr: "host-middleware.launchx-mesh.svc.cluster.local:6379"
    namespace: srv-order
    ttl: 10s
    enable_trace: true
  mqx:
    driver: kafka
    instances:
      srv-order:
        brokers:
          - "host-middleware.launchx-mesh.svc.cluster.local:9092"
        enable_trace: true
        # topic 键位口径：producer 读的是 consumer.topic（见 pkg/events 注释）。
        consumer:
          topic: order.completed.v1
```

另三域的差异（逐项列全，不含省略）：

| 文件 | 与 `srv-order.yaml` 的差异 |
|---|---|
| `conf/srv-points.yaml` | `service.name: srv-points`；`addr: "0.0.0.0:9024"`；`databases.<名>` 与 `instances.<名>` 的键改为 `srv-points`；**删除** `cachex` / `lockx`（**保留 `kvx`** —— 见下方「差异表更正」）；保留 `mqx` 且 `consumer.topic: order.completed.v1`；`trace.on` 去掉 `cache` / `lock`，保留 `db` / **`kv`** / `mq` |
| `conf/srv-product.yaml` | `service.name: srv-product`；`addr: "0.0.0.0:9021"`；键名改 `srv-product`；**删除** `lockx` / `mqx`（**保留 `kvx`** —— 见下方「差异表更正」）；保留 `cachex`；`trace.on` 保留 `db` / `cache` / **`kv`** |
| `conf/srv-stock.yaml` | `service.name: srv-stock`；`addr: "0.0.0.0:9023"`；键名改 `srv-stock`；**删除** `cachex` / `mqx` 三段；保留 `kvx` / `lockx`；`trace.on` 保留 `db` / `kv` / `lock` |

**差异表更正（2026-10-08 实测）**：上表初稿让 `srv-points` / `srv-product` 也删掉 `kvx`，
这与 `mesh-app-handles` 的矩阵**冲突** —— 规格原文是「全部 `srv-*` 都用 `KV()[<name>]`，
四域一视同仁，**不因「本域 RPC 面只读」而省略**：该跳走 Kitex TTHeader/TCP，Envoy 只按
`tcp_proxy` 处理拿不到 L7 限流，`route` 的 `middleware` 是本跳唯一的治理手段」。

实测过删掉它的后果（`/tmp` 副本起服，仓库文件零改动）：进程**照常启动、日志零错误零警告**，
只有装配期自检里少一行 `component=kv` —— 即限流**静默失效**。故保留 `kvx`，
并把 `kv` 加进 `trace.on`（`Diverged = Present && Want && !Observed`：句柄在册但开关关时
自检**不会**核对它，那是另一条静默路径）。

同理更正一条先前写错的注记：`sqlx.databases.<name>.tracing.enabled` **不是** db 入链的开关 ——
`component.buildDB` 只读 `trace.on.db` 在 `orm.Open`（装四类回调）与 `orm.OpenNoTrace`（不装）
之间选，`LoadMySQLFromConfigx` 的第二个返回值被丢弃。保留该子段只为与 `examples/micro`
的配置形状一致。实测：`trace.on.db=false` 时进程照常起，自检如实报 `joins_trace=false`，不报背离。

Run:
```bash
cd examples/mesh/apps
gofmt -l . && go build ./... && go vet ./...
grep -rnE 'coordx|self:' conf/ || echo "OK: 无 coordx / self 段"
```
Expected: `gofmt` 无输出；`go build` / `go vet` 退出码 0；打印 `OK: 无 coordx / self 段`
（apps 侧零注册是 spec 的硬要求，这条 grep 就是它的判据）。

> **判据写法更正**：初稿写的是 `grep -rn "coordx\|self:"`（BRE 交替）。本机 grep 是
> toybox 0.8.13，BRE 的 `\|` **静默失效**（在有命中的文件上返回 0 命中），这条判据于是
> 永远打印 OK —— 是假阴性而不是真通过。已改为 `-E`。

- [x] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 对应行同步）

> **实现记录（2026-10-08）**：五进程本机起服实测 —— 8 条入口全 200
> （`/healthz/{live,ready}` + `/ping` + 六条路由，`POST /order` 亦 200）；
> 句柄自检逐域与 `mesh-app-handles` 矩阵一致（order=db/cache/kv/mq/lock、
> points=db/kv/mq、product=db/cache/kv、stock=db/kv/lock、bff=cache）；
> 业务码跨进程还原三连（`order 999999`→404/20404、`product 999`→404/40404、
> `points abc`→400/10001 —— 没有 `rpcwire` 时这三条会全是 502/10500）；
> MQ 异步链实证落库（`points_log` 增行 + 消费端 `mqx call chain entry status=success`）。
>
> **本 Task 另修三处实现缺陷**（都不在原 Files 清单里，但会直接阻塞后续 Task）：
>
>   1. `apps/Makefile` 的 `SERVICES` 含 `gateway` —— 而 apps 没有 `cmd/gateway`
>      （README §3：五个进程），`make build` / `make image` 在第一轮就 `exit 1`，
>      Task 14 无法进行；
>   2. `apps/Dockerfile` 的 `ENTRYPOINT` 同样是 `/app/bin/gateway`，且 `WORKDIR`
>      是 `/app/conf` —— 而本目录的 `main.go` 用相对路径 `conf/<svc>.yaml`
>      （CWD 契约 = apps 根），照原值会解析成 `/app/conf/conf/<svc>.yaml` 而读不到配置；
>   3. `examples/scripts/order_db.sh` 缺 `products` / `stocks` / `points_account` /
>      `points_log` 四张表（`/product/1`、`/stock/1`、`/points/1` 会以「表不存在」
>      返回 502，而不是业务码 40404）；`stocks.product_id` 与 `points_account.user_id`
>      的唯一索引也缺 —— 后者少了它，`AddBalance` 的 `ON DUPLICATE KEY UPDATE`
>      无从命中，会**静默**退化成「每加一次分插一行新账户」。

---

### Task 9: 结构与依赖面核对

> **trace:** plan-ready.md → `### Task 9: 结构与依赖面核对` | tasks.md → `- [ ] 3.1 结构与依赖面核对：根包 `go test . -run 'TestMesh|TestMicro|TestLibrary'` 全绿（`apps/**` 之外零 `.go`；micro 不引用 mesh）；`examples/mesh/apps` 内零 `go-redis` / `kafka-go` / `consul/api` 直接引用，且零 `gorm.Open(` / `gorm.io/driver/*`（`_test.go` 豁免）—— 判据是「不自行建连」，`gorm.io/gorm` 只允许作句柄类型与自检用`
> **sync:** tasks.md → `- [ ] 3.1 结构与依赖面核对：根包 `go test . -run 'TestMesh|TestMicro|TestLibrary'` 全绿（`apps/**` 之外零 `.go`；micro 不引用 mesh）；`examples/mesh/apps` 内零 `go-redis` / `kafka-go` / `consul/api` 直接引用，且零 `gorm.Open(` / `gorm.io/driver/*`（`_test.go` 豁免）—— 判据是「不自行建连」，`gorm.io/gorm` 只允许作句柄类型与自检用` | plan-ready.md → `### Task 9: 结构与依赖面核对`

**Files:** 无（判据任务；失败时回改 apps 侧代码）

- [x] **Step 1: 跑根包守卫**

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx && export PATH=$HOME/sdk/go1.24.3/bin:$PATH
go test . -run 'TestMesh|TestMicro|TestLibrary' -v 2>&1 | tail -20
```
Expected: 三个测试全 PASS。若 `TestMeshGoCodeOnlyUnderApps` 失败并列出 `deploy/**` 下的
文件，说明清单目录里混进了 `.go`，删掉即可。

- [x] **Step 2: 依赖面核对**

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx/examples/mesh/apps
echo "== 驱动直引（应为空）=="
grep -rnE --include='*.go' "redis/go-redis|segmentio/kafka-go|hashicorp/consul/api" . ; echo "--- 上一行应为空 ---"
echo "== 自行建连（应为空；_test.go 豁免）=="
grep -rnE --include='*.go' "gorm\.Open\(|gorm\.io/driver/" . | grep -v "_test.go" ; echo "--- 上一行应为空 ---"
echo "== gorm 的正当用法（四类：断言 / 句柄字段 / 错误哨兵 / 自检探回调）=="
grep -rn --include='*.go' "gorm.io/gorm" . | grep -v "_test.go"
echo "== srv 不得有出向 RPC 客户端（应为空）=="
grep -rnE "NewClient\(|client\.Option|RPCClientOptions\(" internal/srv/ ; echo "--- 上一行应为空 ---"
```
Expected: 三条「应为空」的 grep 零命中。gorm 的命中只出现在**四类**只读用法 ——
`app/setup.go` 的 `a.DB().(*gorm.DB)` 断言、`repository/**` 的结构体字段类型（`db *gorm.DB`）、
`service/**` 的 `errors.Is(err, gorm.ErrRecordNotFound)` 哨兵判定、以及
`pkg/handles/check.go` 的 trace 回调探针。出现 `gorm.Open(` 或 `gorm.io/driver/*`
（非 `_test.go`）即违规，必须改回经 `App.DB()` 取用。

> **判据写法更正（2026-10-08 实测）**：三处改动，两处是原命令会产出假结果。
> 1. 原命令用 `grep -rnE ... .` 扫当前目录 —— `go.mod` / `go.sum` 里必然出现
>    `hashicorp/consul/api`、`segmentio/kafka-go`、`gorm.io/driver/*`（都是 `// indirect`
>    传递依赖），于是「应为空」的三条**永远有命中**，判据形同虚设。
>    加 `--include='*.go'` 只看源码。
> 2. `--include` 的 glob **必须加引号**：原注说「zsh 下会因 glob 无匹配报错」指的是
>    **不引号**的 `--include=*.go`；写成 `--include='*.go'` 则完全可用（已实测）。
> 3. gorm 正当用法原列「三处」，漏了 `service/**` 的 `gorm.ErrRecordNotFound` ——
>    它也是句柄类型用法（错误哨兵），不是建连，实际是**四类**。

**实测（2026-10-08）**：三个守卫全 PASS（`TestLibrarySourceHasNoDirectMiddlewareImports` /
`TestMeshGoCodeOnlyUnderApps` / `TestMicroNotCoupledToMesh`）；`find examples/mesh -name "*.go"
-not -path "examples/mesh/apps/*"` 零命中（含 `deploy/**`）。驱动直引与自行建连在 `.go` 上
**零命中** —— 11 处 `gorm.io/driver/sqlite` 与 13 处 `gorm.Open(` 全在 `_test.go`（豁免：
内存 sqlite 搭测试库）。`gorm.io/gorm` 非测试命中 14 行 / 13 文件，落在四类：
`app/setup.go`×4（断言）、`repository/**`×5（字段类型 + `clause`）、`service/**`×4
（`ErrRecordNotFound` 哨兵）、`pkg/handles/check.go`×1（trace 回调探针）。
`internal/srv/` 内出向 RPC 客户端零命中。

- [x] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 对应行同步）

---

### Task 10: 句柄矩阵与分层纪律核对

> **trace:** plan-ready.md → `### Task 10: 句柄矩阵与分层纪律核对` | tasks.md → `- [ ] 3.2 句柄矩阵与分层纪律核对：逐进程对照 `mesh-app-handles` 的句柄矩阵（多取/少取都为违规）；`srv-*` 不出现 `Registry()` / `Discover()` / `RPCClientOptions()`；`bff` 不出现 `DB()` / `MQ()`；四域 `internal/srv/<域>/` 子目录集合完全相同（各八个）；四域 `rpc`/`handler`/`service`/`repository` 四层各含 `layer.Begin` 调用`
> **sync:** tasks.md → `- [ ] 3.2 句柄矩阵与分层纪律核对：逐进程对照 `mesh-app-handles` 的句柄矩阵（多取/少取都为违规）；`srv-*` 不出现 `Registry()` / `Discover()` / `RPCClientOptions()`；`bff` 不出现 `DB()` / `MQ()`；四域 `internal/srv/<域>/` 子目录集合完全相同（各八个）；四域 `rpc`/`handler`/`service`/`repository` 四层各含 `layer.Begin` 调用` | plan-ready.md → `### Task 10: 句柄矩阵与分层纪律核对`

**Files:** 无（判据任务）

- [x] **Step 1: 句柄取用矩阵核对**

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx/examples/mesh/apps
echo "== srv-* 不得出现注册/发现/出向客户端 =="; grep -rnE "a\.Registry\(\)|a\.Discover\(\)|a\.RPCClientOptions\(" internal/srv/ || echo OK
echo "== bff 不得出现 DB/MQ =="; grep -rnE "a\.DB\(\)|a\.MQ\(\)|a\.MQConsumer\(\)" internal/bff/ || echo OK
echo "== 各进程取用的句柄清单（逐进程独立，禁止并集）=="
for kv in bff:internal/bff order:internal/srv/order points:internal/srv/points \
          product:internal/srv/product stock:internal/srv/stock; do
  printf "%-8s " "${kv%%:*}"
  grep -rhoE "a\.(DB|Cache|KV|Lock|MQ|MQConsumer|Search|Mongo|Engine|RPCClientOptions|RPCServerOptions|SetRPCServer|Config|Logger|Tracer|Graph)\(" \
    "${kv##*:}" 2>/dev/null | sort -u | sed 's/^a\.//;s/($//' | tr '\n' ' '
  echo
done
echo "== srv-* 不得取 Tracer / Engine（应为空）=="
grep -rnE "a\.(Tracer|Registry|Discover|Engine)\(" internal/srv/ ; echo "--- 上一行应为空 ---"
```
Expected: 前两条打印 `OK`；第三条与 spec 的句柄矩阵逐行对齐：
`bff` → Cache/Config/Engine/Logger/RPCClientOptions；`order` → Cache/Config/DB/KV/Lock/Logger/MQ/RPCServerOptions/SetRPCServer；
`points` → Config/DB/**KV**/Logger/MQConsumer/RPCServerOptions/SetRPCServer；
`product` → Cache/Config/DB/**KV**/Logger/RPCServerOptions/SetRPCServer；
`stock` → Config/DB/KV/Lock/Logger/RPCServerOptions/SetRPCServer。
**四域都有 `KV`** —— spec `mesh-app-handles` 的「全部 `srv-*` 用 `KV()[<name>]`」一行是硬要求
（理由：读路径同样打 DB，且该跳走 TTHeader/TCP，Envoy 只按 `tcp_proxy` 处理、拿不到 L7 限流，
`route` 的限流是本跳唯一的治理手段），**不因「本域 RPC 面只读」而省略**。
多出的项即违规（多取），少掉也是违规（少取）。

> **判据写法更正（2026-10-08 实测）**：原命令有三处会让判据失真，必须按上面改。
> 1. **原循环把 `internal/bff/` 加进了每一行**（`grep ... internal/bff/ internal/srv/$d/`）
>    ⇒ 每行输出的是 `bff ∪ <域>`，四域行全都带着 bff 的 `Engine` / `Cache`。
>    后果是「某域多取了 Cache」这类违规**永远查不出来**（bff 本来就有 Cache，并集里看不出）。
>    改成 `name:path` 对，每行只扫自己的目录。
> 2. **原模式写 `...\)\(\)` 要求紧跟空括号**，而实际调用是带参的
>    `a.RPCClientOptions(<域>)` / `a.SetRPCServer(srv)` ⇒ 这两个**关键句柄一条都匹配不到**，
>    bff 行会少掉 `RPCClientOptions`、四域行会少掉 `SetRPCServer`，判据直接漏掉最该看的项。
>    改成只匹配到 `(`，再由 `sed` 去掉尾部。
> 3. **上一条 Expected 里 points / product 漏写了 `KV`**（与 spec 冲突，也与实际代码不符）——
>    本 Task 实测四域 setup 都把 `a.KV()[name]` 传进了 `middleware.New(...)`，是真用不是取而不用。
> 4. 追加一条 `a.(Tracer|Registry|Discover|Engine)(` 的反向判据：spec 明确「全部 `srv-*`
>    **不用** `Tracer()` / `Engine()` / `Registry()` / `Discover()`」，原命令只验证了后三个。
>
> **反向探针实测（`/tmp` 副本注入，仓库文件零改动）**：在 `srv-points/app/setup.go` 注入
> `_, _ = a.Engine()`（spec 明令 srv 不得取 HTTP 引擎）。
> - 原命令输出：`a.Cache() a.Config() a.DB() a.Engine() a.KV() a.Logger() a.MQConsumer() a.RPCServerOptions()`
>   —— 与**注入前逐字相同**（`a.Engine()` 本来就来自并集里的 bff），违规**完全不可见**；
>   同时 `SetRPCServer` 缺席（被 `\(\)` 模式漏掉）。
> - 修正后命令输出：多出 `Engine` ⇒ 违规可见；`SetRPCServer` 正常出现。
> - 追加的反向判据：直接给出 `points/app/setup.go:98: _, _ = a.Engine()` 的文件行号。

**实测（2026-10-08）**：矩阵与 spec 逐行对齐（见上）；`srv-*` 内
`a.(Registry|Discover|RPCClientOptions|Tracer|Engine)(` **零命中**；
四域 `app/setup.go` 都把 `a.KV()[name]` 传进了 `middleware.New(...)`（真用，非取而不用）。

- [x] **Step 2: 四域同构与分层埋点核对**

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx/examples/mesh/apps
for d in order points product stock; do printf "%-8s " "$d"; ls internal/srv/$d | tr '\n' ' '; echo; done
echo "== 四层埋点（每域应各 4 处 layer.Begin）=="
grep -rc "layer.Begin" internal/srv/*/rpc/*.go internal/srv/*/handler/*.go internal/srv/*/service/*.go internal/srv/*/repository/*.go | grep -v ":0$"
```
Expected: 四行目录集合一致（各八个：`app handler middleware model repository route rpc service`）；
`layer.Begin` 在**四个层各至少 1 处**（`rpc` / `handler` / `service` / `repository`）。
不一致即同构或埋点被破坏 —— 要么补齐，要么回填差异表（**不许**默默接受差异）。

> **判据写法更正（2026-10-08 实测）**：原 Expected 写「每域 4 处（各层各 1）」。
> 实际是**每域四个层各有命中，但每层的处数不等**（`rpc` 层 order=4 / points=5 /
> product=6 / stock=7 —— 一个方法一个 span，`rpc` 层方法数各不相同；`handler` 层
> order=3 其余=2）。「各层各 1」的表述会让人误以为多出来的是违规，故改为「各至少 1 处」，
> 真正的判据是**层不被漏**，不是数量相等。
> 另外这条 `grep -rc` 会把 `_test.go` 也算进去（`internal/srv/order/rpc/order_test.go:1`），
> 属于可接受的噪声 —— 测试里也有 `layer.Begin` 不影响「层有没有被埋」的判断。

**实测（2026-10-08）**：四域目录集合逐字一致（各八个）；`layer.Begin` 覆盖
`rpc` / `handler` / `service` / `repository` 四层 × 四域 = 16 个文件全部命中，无零命中项。

- [x] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 对应行同步）

---

### Task 11: ConfigMap（五份配置）

> **trace:** plan-ready.md → `### Task 11: ConfigMap（五份配置）` | tasks.md → `- [ ] 2.1 写 `deploy/apps/10-configmap.yaml`：五份配置（bff + 四域 srv），宿主中间件地址写 `host-middleware.launchx-mesh.svc.cluster.local`，域服互访写 K8s Service 名；无裸 `127.0.0.1`。验收：`VARIANT=apps bring-up.sh check` 的地址自检通过`
> **sync:** tasks.md → `- [ ] 2.1 写 `deploy/apps/10-configmap.yaml`：五份配置（bff + 四域 srv），宿主中间件地址写 `host-middleware.launchx-mesh.svc.cluster.local`，域服互访写 K8s Service 名；无裸 `127.0.0.1`。验收：`VARIANT=apps bring-up.sh check` 的地址自检通过` | plan-ready.md → `### Task 11: ConfigMap（五份配置）`

**Files:** `examples/mesh/deploy/apps/10-configmap.yaml`

- [x] **Step 1: 写 ConfigMap**

结构固定（一个 ConfigMap 装五份配置，各 Deployment 用 `subPath` 挂自己那份）；
内容由 Task 8 的 `conf/*.yaml` 得来，把地址段换成集群内可达名：

> **本模板已按实测修正三处（2026-10-08）**，与最初的写法不同，照抄旧版会埋雷：
> 1. **srv-points / srv-product 保留 `kvx`**（原写「删 kvx」）—— spec `mesh-app-handles`
>    「全部 srv-* 用 `KV()[<name>]`，四域一视同仁」是硬要求；缺 kvx 是 **fail-open**，
>    进程照常起、日志零错误，只是该跳没有限流（Task 8 探针实测）。
> 2. **kvx 逻辑库四域统一 `db: 1`**（原写 srv-stock 用 `db: 4`）—— 计数键都带服务名前缀，
>    分开库只会让「同一份计数散在四个库里」更难排查。
> 3. `trace.on` 补上 `kv`（points/product）—— 在册句柄逐个要有对应开关，
>    否则 handles 自检不核对它（`Want=false` 时 `Diverged` 恒为假，又是一条静默路径）。
>
> **落地方式是「逐字镜像」而不是手抄**：ConfigMap 由 `apps/conf/*.yaml` 程序化生成
> （每份整体缩进 4 空格放进块标量），理由是手抄必然丢字段，而丢 `kvx` 这类段
> 恰恰不报错。判据也相应从「看着像」升级为**往返比对**：解析 ConfigMap，
> 逐键与 `conf/` 原文 `==` 比较，五份全部逐字一致才算过。

```yaml
# apps 组配置：五份配置塞进一个 ConfigMap，各 Deployment 用 subPath 挂各自那一份。
#
# 与 micro 组 configmap 的系统性差异只有一处：地址段。
# micro 组指向 127.0.0.1 / host.minikube.internal 的项，这里全部换成
# host-middleware.launchx-mesh.svc.cluster.local —— 集群内到宿主中间件只留一个
# 名字，换宿主地址只需改 host-middleware 的 Endpoints
#（见 deploy/shared/20-host-middleware.yaml）。
apiVersion: v1
kind: ConfigMap
metadata:
  name: mesh-apps-configs
  namespace: launchx-mesh
data:
  bff.yaml: |
    service:
      name: bff
      mode: bff
      addr: "0.0.0.0:8081"
      upstream:
        srv-order:   "srv-order.launchx-mesh.svc.cluster.local:9022"
        srv-points:  "srv-points.launchx-mesh.svc.cluster.local:9024"
        srv-product: "srv-product.launchx-mesh.svc.cluster.local:9021"
        srv-stock:   "srv-stock.launchx-mesh.svc.cluster.local:9023"
    webx:
      addr: "0.0.0.0:8081"
      timeout: 5s
      rate_limit_rps: 100
      rate_limit_burst: 200
      cors_origins: []
      health_prefix: /healthz
    trace:
      enabled: true
      endpoint: "host-middleware.launchx-mesh.svc.cluster.local:4318"
      on:
        cache: true
    logx:
      level: info
      otel:
        enable_trace: true
        exporter: otlp
        endpoint: "host-middleware.launchx-mesh.svc.cluster.local:4318"
    xmiddleware:
      cachex:
        driver: redis
        enable_trace: true
        instances:
          bff:
            addr: "host-middleware.launchx-mesh.svc.cluster.local:6379"
            db: 2
  srv-order.yaml: |
    service: {name: srv-order, mode: srv, addr: "0.0.0.0:9022"}
    strict: {db: true}
    trace:
      enabled: true
      endpoint: "host-middleware.launchx-mesh.svc.cluster.local:4318"
      on: {db: true, cache: true, kv: true, mq: true, lock: true}
    logx:
      level: info
      otel:
        enable_trace: true
        exporter: otlp
        endpoint: "host-middleware.launchx-mesh.svc.cluster.local:4318"
    xmiddleware:
      sqlx:
        driver: gorm
        databases:
          srv-order:
            dsn: "order_rw:${HUBX_DB_PW}@tcp(host-middleware.launchx-mesh.svc.cluster.local:3306)/order_db?charset=utf8mb4&parseTime=True&loc=Local"
            tracing: {enabled: true}
      cachex:
        driver: redis
        enable_trace: true
        instances:
          srv-order: {addr: "host-middleware.launchx-mesh.svc.cluster.local:6379", db: 0}
      kvx:
        driver: redis
        enable_trace: true
        instances:
          srv-order: {addr: "host-middleware.launchx-mesh.svc.cluster.local:6379", db: 1}
      lockx:
        driver: redis
        addr: "host-middleware.launchx-mesh.svc.cluster.local:6379"
        namespace: srv-order
        ttl: 10s
      mqx:
        driver: kafka
        instances:
          srv-order:
            brokers: ["host-middleware.launchx-mesh.svc.cluster.local:9092"]
            enable_trace: true
            consumer: {topic: order.completed.v1}
  srv-points.yaml: |
    # 由 Task 8 Step 3 的差异表逐项改写 srv-order.yaml 得来：
    #   name=srv-points / addr=0.0.0.0:9024 / 键名改 srv-points /
    #   删 cachex+lockx / **保留 kvx**（限流计数，四域一视同仁） /
    #   保留 mqx(consumer.topic=order.completed.v1) / trace.on 保留 db+kv+mq
    service: {name: srv-points, mode: srv, addr: "0.0.0.0:9024"}
    strict: {db: true}
    trace: {enabled: true, endpoint: "host-middleware.launchx-mesh.svc.cluster.local:4318", on: {db: true, kv: true, mq: true}}
    logx: {level: info, otel: {enable_trace: true, exporter: otlp, endpoint: "host-middleware.launchx-mesh.svc.cluster.local:4318"}}
    xmiddleware:
      sqlx:
        driver: gorm
        databases:
          srv-points:
            dsn: "order_rw:${HUBX_DB_PW}@tcp(host-middleware.launchx-mesh.svc.cluster.local:3306)/order_db?charset=utf8mb4&parseTime=True&loc=Local"
      kvx:
        driver: redis
        enable_trace: true
        instances:
          srv-points: {addr: "host-middleware.launchx-mesh.svc.cluster.local:6379", db: 1}
      mqx:
        driver: kafka
        instances:
          srv-points:
            brokers: ["host-middleware.launchx-mesh.svc.cluster.local:9092"]
            consumer: {topic: order.completed.v1}
  srv-product.yaml: |
    # 差异：name=srv-product / addr=0.0.0.0:9021 / 键名改 srv-product /
    #       删 lockx+mqx / **保留 kvx**（限流计数，四域一视同仁） /
    #       保留 cachex / trace.on 保留 db+cache+kv
    service: {name: srv-product, mode: srv, addr: "0.0.0.0:9021"}
    strict: {db: true}
    trace: {enabled: true, endpoint: "host-middleware.launchx-mesh.svc.cluster.local:4318", on: {db: true, cache: true, kv: true}}
    logx: {level: info, otel: {enable_trace: true, exporter: otlp, endpoint: "host-middleware.launchx-mesh.svc.cluster.local:4318"}}
    xmiddleware:
      sqlx:
        driver: gorm
        databases:
          srv-product:
            dsn: "order_rw:${HUBX_DB_PW}@tcp(host-middleware.launchx-mesh.svc.cluster.local:3306)/order_db?charset=utf8mb4&parseTime=True&loc=Local"
      kvx:
        driver: redis
        enable_trace: true
        instances:
          srv-product: {addr: "host-middleware.launchx-mesh.svc.cluster.local:6379", db: 1}
      cachex:
        driver: redis
        enable_trace: true
        instances:
          srv-product: {addr: "host-middleware.launchx-mesh.svc.cluster.local:6379", db: 3}
  srv-stock.yaml: |
    # 差异：name=srv-stock / addr=0.0.0.0:9023 / 键名改 srv-stock /
    #       删 cachex+mqx / 保留 kvx+lockx / trace.on 保留 db+kv+lock
    service: {name: srv-stock, mode: srv, addr: "0.0.0.0:9023"}
    strict: {db: true}
    trace: {enabled: true, endpoint: "host-middleware.launchx-mesh.svc.cluster.local:4318", on: {db: true, kv: true, lock: true}}
    logx: {level: info, otel: {enable_trace: true, exporter: otlp, endpoint: "host-middleware.launchx-mesh.svc.cluster.local:4318"}}
    xmiddleware:
      sqlx:
        driver: gorm
        databases:
          srv-stock:
            dsn: "order_rw:${HUBX_DB_PW}@tcp(host-middleware.launchx-mesh.svc.cluster.local:3306)/order_db?charset=utf8mb4&parseTime=True&loc=Local"
      kvx:
        driver: redis
        enable_trace: true
        instances:
          srv-stock: {addr: "host-middleware.launchx-mesh.svc.cluster.local:6379", db: 1}
      lockx:
        driver: redis
        addr: "host-middleware.launchx-mesh.svc.cluster.local:6379"
        namespace: srv-stock
        ttl: 10s
```

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx/examples
PYTHON=/Users/hyx/.workbuddy/binaries/python/envs/default/bin/python3 VARIANT=apps bash mesh/scripts/bring-up.sh check
```
Expected: `✓ 地址全部换算为 host.minikube.internal / 集群内 Service`，总判定 `== check 通过 ==`。
若报「configmap 里仍有裸 127.0.0.1」，按输出的行号改掉。

**实测（2026-10-08）**：`VARIANT=apps bring-up.sh check` → rc=0，四段全绿
（minikube / istioctl / docker CLI 均在；`kubectl apply --dry-run=client` 通过；
`VARIANT=apps 的清单目录存在`；地址自检通过）。
产物 `deploy/apps/10-configmap.yaml` 438 行，五份配置（bff 71 / order 103 / points 77 /
product 71 / stock 68 行）与 `apps/conf/*.yaml` **逐字一致**（往返比对）。
句柄段与 Task 10 矩阵一致：`order`=sqlx+cachex+kvx+lockx+mqx、`points`=sqlx+kvx+mqx、
`product`=sqlx+cachex+kvx、`stock`=sqlx+kvx+lockx、`bff`=cachex；
`trace.on` 恰好覆盖各自在册句柄；所有实例名 = `service.name`；kvx 四域统一 db 1，
cachex `order 0 / bff 2 / product 3`。

> **反向探针（改真实地址值，非注释；仓库已还原）**：把一个 `addr:` 改成裸回环地址
> ⇒ `check` rc=1 并精确报 `10-configmap.yaml:105`。证明地址自检对本文件有效。
>
> **一个虚惊（值得记）**：用 PyYAML 校验时 `trace.on` 读出来是空的 —— 因为
> PyYAML 走 YAML 1.1，把 `on` 解析成布尔 `True`。但 Go 侧是 `gopkg.in/yaml.v3`
> （YAML 1.2），实测 **`on` 仍是字符串键**，配置没问题。
> 教训：**用 Python 校验本仓配置时**，`on` / `off` / `yes` / `no` 这四个键会被静默
> 换成布尔，读到的「开关缺失」是校验脚本的错觉，不是配置的缺陷。

- [x] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 对应行同步）

---

### Task 12: 四域 srv 清单

> **trace:** plan-ready.md → `### Task 12: 四域 srv 清单` | tasks.md → `- [ ] 2.2 写四份 srv 清单（`20-srv-order.yaml` ~ `23-srv-stock.yaml`）：`Service`（端口名 `tcp-rpc`）+ `Deployment`（探针 `tcpSocket`）。验收：`kubectl apply --dry-run=client -R -f deploy/apps` 通过`
> **sync:** tasks.md → `- [ ] 2.2 写四份 srv 清单（`20-srv-order.yaml` ~ `23-srv-stock.yaml`）：`Service`（端口名 `tcp-rpc`）+ `Deployment`（探针 `tcpSocket`）。验收：`kubectl apply --dry-run=client -R -f deploy/apps` 通过` | plan-ready.md → `### Task 12: 四域 srv 清单`

**Files:** `deploy/apps/20-srv-order.yaml`、`21-srv-points.yaml`、`22-srv-product.yaml`、`23-srv-stock.yaml`

- [x] **Step 1: 写 `20-srv-order.yaml`**

```yaml
# 订单域（apps 组）。
#
# 端口名写成 `tcp-rpc` 是实测结论，别改成 grpc-*：改 grpc-rpc 后 Istio 会为该端口
# 建 HTTP/2 listener，把 kitex 的 TTHeader 当 h2 解析 → 入口直接 502。
# 代价是这一跳拿不到 sidecar span，T13 只认 ingress→bff 跳 —— 协议事实，不是漏配。
#
# 探针用 tcpSocket 而不是 httpGet：srv 档没有 HTTP 面，且这一跳 Envoy 按 tcp_proxy
# 处理、读不到状态码，outlier detection 在本跳无从生效。需要 HTTP 探针的是 bff
#（见 30-bff.yaml），那里 webx 自带 /healthz。
apiVersion: v1
kind: Service
metadata:
  name: srv-order
  namespace: launchx-mesh
spec:
  selector:
    app: srv-order
  ports:
    - name: tcp-rpc
      port: 9022
      targetPort: 9022
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: srv-order
  namespace: launchx-mesh
spec:
  replicas: 1
  selector:
    matchLabels:
      app: srv-order
  template:
    metadata:
      labels:
        app: srv-order
    spec:
      containers:
        - name: app
          image: launchx-mesh-apps:dev
          imagePullPolicy: IfNotPresent
          command: ["/app/bin/srv-order"]
          ports:
            - name: rpc
              containerPort: 9022
          env:
            - name: HUBX_DB_PW
              value: hubx123
          volumeMounts:
            - name: conf
              mountPath: /app/conf/srv-order.yaml
              subPath: srv-order.yaml
          livenessProbe:
            tcpSocket:
              port: 9022
            initialDelaySeconds: 15
            periodSeconds: 10
          readinessProbe:
            tcpSocket:
              port: 9022
            initialDelaySeconds: 5
            periodSeconds: 5
          resources:
            requests: {cpu: 100m, memory: 128Mi}
            limits: {cpu: 500m, memory: 384Mi}
      volumes:
        - name: conf
          configMap:
            name: mesh-apps-configs
```

另三份的差异（其余逐字相同）：

| 文件 | name / app 标签 | command | 端口（Service 与探针同步改） | subPath |
|---|---|---|---|---|
| `21-srv-points.yaml` | `srv-points` | `/app/bin/srv-points` | 9024 | `srv-points.yaml` |
| `22-srv-product.yaml` | `srv-product` | `/app/bin/srv-product` | 9021 | `srv-product.yaml` |
| `23-srv-stock.yaml` | `srv-stock` | `/app/bin/srv-stock` | 9023 | `srv-stock.yaml` |

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx
export PATH=/usr/local/bin:$PATH
minikube kubectl -- apply --dry-run=client -R -f examples/mesh/deploy/apps
```
Expected: 九条 `... created (dry run)`（1 ConfigMap + 4 Service + 4 Deployment），退出码 0。

> **实测更正 1（端口命名自查不了）**：`istioctl analyze <file>` 对**尚未 apply** 的
> Service **不跑 IST0118**。实测：把 `shared/20-host-middleware.yaml` 的 Service 整份改名
> 成新服务做探针，端口名照抄 `kafka` / `otlp-http`（这两个在集群里稳定各报一条 IST0118），
> 结果**零消息**；换成 `rpc` / `foo` / `grpc-rpc` / `tcp-rpc` 也都是零消息。
> 同一调用下 VirtualService 的 IST0101 却能报出来（对照探针已验证），说明不是「文件没被
> 加载」，而是 IST0118 这个分析器不作用于模拟资源。
> ⇒ **端口命名只能等 apply 之后 `istioctl analyze -n launchx-mesh` 才查得出**（Task 13 判据）。
> 本 Task 的 dry-run **查不出**端口名写错，别拿它当端口命名的证据。
>
> **实测更正 2（与对照组的一处差异，模板里没写）**：micro 的 `20-srv.yaml` 有 `POD_IP`
> env（consul 注册要用 `service.self.addr: ${POD_IP}:9013`）；**apps 零注册，不需要它**——
> 已核对 `apps/conf/**` 与 `apps/internal/**` 内 `POD_IP` 零命中。四份清单的 env 只有
> `HUBX_DB_PW` 一项（conf 里 sqlx 的 dsn 是 `order_rw:${HUBX_DB_PW}@tcp(...)`）。
>
> **落盘方式**：四份由同一模板程序化生成（`{name}` / `{port}` 两个占位），
> 判据是**去掉注释行后抹除服务名与端口逐字相同** —— 已实测通过。
> 另做了四路交叉核对：conf 监听端口 == Service port == targetPort == containerPort ==
> 两个探针端口 == bff `service.upstream.<域>` 的端口，且 upstream 主机名 == Service 名
> == conf `service.name`（四域全对）。这四个数散在四个文件里，手抄错一个就是
> 「Pod Running 但入口 502」且没有任何报错。

- [x] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 对应行同步）

---

### Task 13: bff / Istio 入口 / 授权清单

> **trace:** plan-ready.md → `### Task 13: bff / Istio 入口 / 授权清单` | tasks.md → `- [ ] 2.3 写 `30-bff.yaml`（`Service` + `Deployment`，探针 `httpGet` 打 `/healthz/{live,ready}`）与 `40-gateway.yaml`（Istio `Gateway` + `VirtualService`：`match.uri.prefix: /api` → `host: bff` + `rewrite.uri: /`；`DestinationRule` 的 `outlier detection` 只作用在 bff）+ `50-authz.yaml`（`AuthorizationPolicy`：域服只接受来自 bff 的调用）。验收：`istioctl analyze -n launchx-mesh` 无 error`
> **sync:** tasks.md → `- [ ] 2.3 写 `30-bff.yaml`（`Service` + `Deployment`，探针 `httpGet` 打 `/healthz/{live,ready}`）与 `40-gateway.yaml`（Istio `Gateway` + `VirtualService`：`match.uri.prefix: /api` → `host: bff` + `rewrite.uri: /`；`DestinationRule` 的 `outlier detection` 只作用在 bff）+ `50-authz.yaml`（`AuthorizationPolicy`：域服只接受来自 bff 的调用）。验收：`istioctl analyze -n launchx-mesh` 无 error` | plan-ready.md → `### Task 13: bff / Istio 入口 / 授权清单`

**Files:** `deploy/apps/30-bff.yaml`、`deploy/apps/40-gateway.yaml`、`deploy/apps/50-authz.yaml`

- [x] **Step 1: 写 `30-bff.yaml`（唯一用 httpGet 探针的 Deployment）**

```yaml
# bff（apps 组）：HTTP 边界。探针用 httpGet 打 webx 自带的健康端点 ——
# 这是唯一一条 Envoy 能按 L7 观察的跳（ingress→bff），DestinationRule 的
# outlier detection 只有在这一跳才有 5xx 可判。
apiVersion: v1
kind: Service
metadata:
  name: bff
  namespace: launchx-mesh
spec:
  selector:
    app: bff
  ports:
    - name: http-web
      port: 8081
      targetPort: 8081
      appProtocol: http
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: bff
  namespace: launchx-mesh
spec:
  replicas: 1
  selector:
    matchLabels:
      app: bff
  template:
    metadata:
      labels:
        app: bff
    spec:
      containers:
        - name: app
          image: launchx-mesh-apps:dev
          imagePullPolicy: IfNotPresent
          command: ["/app/bin/bff"]
          ports:
            - name: http
              containerPort: 8081
          volumeMounts:
            - name: conf
              mountPath: /app/conf/bff.yaml
              subPath: bff.yaml
          livenessProbe:
            httpGet: {path: /healthz/live, port: 8081}
            initialDelaySeconds: 10
            periodSeconds: 10
          readinessProbe:
            httpGet: {path: /healthz/ready, port: 8081}
            initialDelaySeconds: 5
            periodSeconds: 5
          resources:
            requests: {cpu: 100m, memory: 128Mi}
            limits: {cpu: 500m, memory: 384Mi}
      volumes:
        - name: conf
          configMap:
            name: mesh-apps-configs
```

Run:
```bash
cd examples/mesh/deploy/apps
grep -cnE '^kind: (Service|Deployment)$' 30-bff.yaml      # 期望 2
grep -cnE 'path: /healthz/(live|ready)' 30-bff.yaml       # 期望 2（HTTP 探针）
grep -nE 'name: http|appProtocol: http' 30-bff.yaml       # 期望非空（端口名决定 Envoy 按 L7 处理）
```

Expected: 三条都有预期的命中数。**端口协议声明缺失或写成纯数字端口 ⇒ Envoy 按
`tcp_proxy` 处理入口跳，`outlier detection` 与 HTTP 重试一起失效** ——
这是本 Task 唯一一个「不报错但静默降级」的坑，必须靠这条 grep 拦住。

> **判据写法更正（原三条 grep 有两条是坏的，实测）**：
> 1. `grep 'kind: (Service|Deployment)'`（无锚点）会把 **`kind: ServiceAccount`**
>    当子串命中（`kind: Service` + `Account`）⇒ 计数 3 而非 2，且加了 ServiceAccount
>    之后永远「通过」。已改为 `^kind: ...$` 锚定行尾。
> 2. `grep -n 'port: http'` **永远零命中** —— 协议不是写在 `port:` 上的。实际形态是
>    Service port 的 `name: http`（Istio 按 `http-` 前缀识别）或 `appProtocol: http`。
>    已改为 `grep -nE 'name: http|appProtocol: http'`（本仓两样都写了，互为兜底）。
>
> **协议声明的实测证据**（用对照组的在跑 Pod，不必等 apps 部署）：
>   `istioctl proxy-config listeners <pod> --port <p> -o json` 看入向 filter：
>   - `demo-bff`（Service 端口名 `http`） → `envoy.filters.network.http_connection_manager`；
>   - `demo-srv`（Service 端口名 `tcp-rpc`） → `envoy.filters.network.tcp_proxy`。
>   同一个字段、两种下场，这就是「静默降级」的机制。
>   ⚠️ 另：协议**不在** cluster 里（`proxy-config cluster` 的 `TYPE` 列是发现类型 EDS/STATIC，
>   不是协议），拿它做 A/B 会得到「改前改后一模一样」的假阴性。

- [x] **Step 2: 写 `40-gateway.yaml`（入口 + HTTP 跳治理）**

```yaml
# apps 组的入口：Istio Gateway + VirtualService。
#
# 这一份 YAML 取代了 examples/micro/gateway 的 562 行代码
#（proxy 288 + routing 155 + setup 70 + main 49）：
#   * 路由表（1 条 /api → bff）      → match.uri.prefix
#   * strip: true                    → rewrite.uri: /
#   * 上游解析（consul 发现/兜底）    → destination.host（由 EDS 解析）
#   * 逐跳首部处理（hopHeaders 9 项） → Envoy 天然正确
#
# 术语注意：这里的 Gateway 是 Istio **资源**，不是应用侧 gateway **进程**。
apiVersion: networking.istio.io/v1
kind: Gateway
metadata:
  name: mesh-apps
  namespace: launchx-mesh
spec:
  selector:
    istio: ingressgateway
  servers:
    - port: {number: 80, name: http, protocol: HTTP}
      hosts: ["*"]
---
apiVersion: networking.istio.io/v1
kind: VirtualService
metadata:
  name: bff
  namespace: launchx-mesh
spec:
  hosts: ["*"]
  gateways: [mesh-apps]
  http:
    - match:
        - uri: {prefix: /api}
      rewrite:
        uri: /
      route:
        - destination:
            host: bff
            port: {number: 8081}
      timeout: 5s
      retries:
        attempts: 2
        perTryTimeout: 2s
        retryOn: 5xx,connect-failure,reset
---
# outlier detection 只挂在 bff 上：它是唯一被 Envoy 按 L7 观察的目标。
# 不要给 srv-* 加 DestinationRule —— 那一跳是 tcp_proxy，没有状态码可判，
# 挂上去只会让人以为「RPC 跳的异常剔除也生效了」。
apiVersion: networking.istio.io/v1
kind: DestinationRule
metadata:
  name: bff
  namespace: launchx-mesh
spec:
  host: bff
  trafficPolicy:
    outlierDetection:
      consecutive5xxErrors: 3
      interval: 30s
      baseEjectionTime: 30s
```

Run:
```bash
cd examples/mesh/deploy/apps
grep -cnE '^kind: (Gateway|VirtualService|DestinationRule)$' 40-gateway.yaml  # 期望 3
grep -c 'uri: {prefix: /api}' 40-gateway.yaml                                # 期望 1
grep -cE '^\s+rewrite:' 40-gateway.yaml                                      # 期望 1（表达 strip）
grep -cE 'host: srv-' 40-gateway.yaml                                        # 期望 0
```

Expected: 前三条命中数符合；最后一条**零命中** —— 四域 `srv-*` 一律不得出现在本文件里
（包括别给它们加 `DestinationRule`，理由见文件内注释：tcp_proxy 没有状态码可判）。

> **判据写法更正**：`grep 'rewrite'` 会连头注里的散文（「→ `rewrite.uri: /`」）一起数，
> 得 2 而不是 1 —— 与 Task 8 在 conf 里踩的是同一类（注释污染 grep）。已改为
> `^\s+rewrite:` 只匹配 YAML 键行。第一条同样要 `$` 锚定。
>
> **跨命名空间 Gateway selector 已实测生效**（这是本份清单唯一「不看就不知道」
> 的正确性问题）：Gateway 资源在 `launchx-mesh`、ingressgateway Pod 在 `istio-system`，
> `spec.selector: {istio: ingressgateway}` **确实能跨命名空间选中**。
> 证据：apply 后 `istioctl proxy-config routes <ingressgateway> -n istio-system` 出现
> ```
> http.8080   *:80   *   /api*   bff.launchx-mesh
> ```
> 若没绑上，入口会全 404 而 `istioctl analyze` **不报错**。

- [x] **Step 3: 写 `50-authz.yaml`**

```yaml
# 传输层身份：域服只接受来自 bff 的调用。
#
# 这正是 micro 的 pkg/auth/auth.go:61 自己承认「防不住内网里的恶意调用
#（那需要 mTLS）」的那一半 —— 由 Istio 的身份（SPIFFE）补上。
# 应用侧仍需保留按业务字段的鉴权（谁能退谁的款），那是 AuthorizationPolicy 看不到的。
apiVersion: security.istio.io/v1
kind: AuthorizationPolicy
metadata:
  name: srv-from-bff-only
  namespace: launchx-mesh
spec:
  selector:
    matchLabels:
      app: srv-order
  action: ALLOW
  rules:
    - from:
        - source:
            principals: ["cluster.local/ns/launchx-mesh/sa/bff"]
---
# srv-points / srv-product / srv-stock 同构：复制上面这一段，替换 selector 的 app 标签。
# 四份写在同一文件的 --- 分隔下。
```

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx
export PATH=/usr/local/bin:$PATH
minikube kubectl -- apply --dry-run=client -R -f examples/mesh/deploy/apps && \
istioctl analyze -n launchx-mesh
```
Expected: dry-run 全过；`istioctl analyze` 输出 `No validation issues found`
或仅 Info 级提示（Warning / Error 必须为 0）。

实测（apply 后）：`Error 0 / Warning 0 / Info 3`，三条 Info 全是已知基线
（`host-middleware` 的 `consul-host`/`kafka`/`otlp-http`，见 Task 12 的交接第 2 条）。

> **Step 3 落地时的两处修正（照原模板抄会踩）**：
> 1. **补 `ServiceAccount: bff` + `serviceAccountName: bff`**（原模板没有）。
>    AuthorizationPolicy 的 `principals` 认的是**调用方**的 SPIFFE 身份；bff Pod 若用
>    default SA，实际 principal 是 `.../sa/default` ⇒ 四条 ALLOW 全部匹配不上、
>    **四域对 bff 一律 403**，而 `analyze` 报不出（纯运行期行为）。已在 `30-bff.yaml`
>    里显式建了 SA 并绑定。
> 2. 原模板四条策略**同名**（都叫 `srv-from-bff-only`）⇒ 同一 namespace 下后三条会
>    覆盖第一条，实际只有一条生效。已改名为 `srv-<域>-from-bff-only`。
>    （AuthorizationPolicy 的 `selector` 一个资源只能选一个 workload，四域必须四份。）

> **Task 12 实测交接（两条，都会让这一步白跑或误判）**：
> 1. `istioctl analyze` **必须在 apply 之后**跑。文件模式（`istioctl analyze -f ...`，
>    1.30 里 `-f` 已移除，直接传文件路径）对未 apply 的 Service **不跑 IST0118**：
>    把 `shared/20-host-middleware.yaml` 的 Service 整份改名做探针，
>    端口名照抄 `kafka` / `otlp-http`（集群里稳定报 IST0118 的那两个）也**零消息**；
>    而同一次调用里 VirtualService 的 IST0101 能报出来 —— 所以是分析器作用域问题，
>    不是文件没被加载。**端口命名只有 apply 后这一步才查得出。**
> 2. 基线本来就有 **3 条 IST0118**（Info 级，来自 `host-middleware` 的
>    `kafka`(9092) / `otlp-http`(4318) / `consul-host`(8500)）。它们不是本变更引入的，
>    也不是 Error ⇒ 判「Warning / Error 为 0」时不要把它们算进去。
>    （真要清掉得改 `shared/20-host-middleware.yaml` 的端口名，属另一件事。）
>
> **另：AuthorizationPolicy 的 principal 依赖 ServiceAccount**。上面写的
> `principals: ["cluster.local/ns/launchx-mesh/sa/bff"]` 要求 bff 跑在名为 `bff` 的
> ServiceAccount 下；而 Task 12 落地的四份 srv 清单**没有指定 `serviceAccountName`**
> （用 default）。若 Task 13 不给 bff 显式建 `ServiceAccount: bff`，
> 实际 principal 会是 `.../sa/default` ⇒ 该 ALLOW 规则匹配不上、域服**全部 403**，
> 且 analyze 报不出（这是运行期行为）。Task 13 落 `30-bff.yaml` 时一并处理。

- [x] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 对应行同步）

---

### Task 14: 镜像构建与 apps 组部署

> **trace:** plan-ready.md → `### Task 14: 镜像构建与 apps 组部署` | tasks.md → `- [ ] 2.4 构建 apps 镜像并部署：`VARIANT=apps bring-up.sh image`（委托 `apps/Makefile`，交叉编译 5 个二进制 → `launchx-mesh-apps:dev`）→ `bring-up.sh deploy`。验收：五个 Pod 均 `2/2 Running`（app + istio-proxy），`bff` 的 readiness 为真`
> **sync:** tasks.md → `- [ ] 2.4 构建 apps 镜像并部署：`VARIANT=apps bring-up.sh image`（委托 `apps/Makefile`，交叉编译 5 个二进制 → `launchx-mesh-apps:dev`）→ `bring-up.sh deploy`。验收：五个 Pod 均 `2/2 Running`（app + istio-proxy），`bff` 的 readiness 为真` | plan-ready.md → `### Task 14: 镜像构建与 apps 组部署`

**Files:** 无代码改动（执行部署）

- [x] **Step 1: 确认镜像构建入口可用**

Run: `cd /Users/hyx/work/gowork/src/hubx/launchx/examples && VARIANT=apps bash mesh/scripts/bring-up.sh image`
Expected: `make -C .../apps image` 被调用；五个二进制交叉编译成功；镜像
`launchx-mesh-apps:dev` 构建进 minikube daemon
（`minikube image ls | grep launchx-mesh-apps` 有输出）。
失败时先看 `apps/Makefile` 的 `SERVICES` 变量是否已从六个改成五个（删掉 `gateway`，
否则 `go build ./cmd/gateway` 会报不存在）。

> **实测踩到的另一个坑（比上面那条更隐蔽）**：`apps/.dockerignore` 里写了 `bin/`
> —— 而 Dockerfile 唯一一条 `COPY` 正是 `COPY bin/ /app/bin/`。症状：
> ```
> ERROR: failed to calculate checksum of ref ...:: "/bin": not found
> ```
> **错误信息完全不提 .dockerignore**，很容易被引到 Dockerfile 路径或构建上下文上。
> 根因是有人照抄 `.gitignore`（`bin/` 是构建产物）写了这一行 —— 但两个文件语义相反：
> .gitignore 排除产物，这里的 .dockerignore 恰恰要**只送**产物。已删掉该行。
> 对照 `examples/docker/.dockerignore` 是**空文件**，同样是因为要 `COPY bin/`。
> 修复后实测：上下文 417MB（5 个静态二进制，各约 83MB），构建 19s 通过。

- [x] **Step 2: 部署并等就绪**

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx/examples
VARIANT=micro bash mesh/scripts/bring-up.sh down   # 先清对照组，避免两套同命名空间
VARIANT=apps  bash mesh/scripts/bring-up.sh deploy
```
Expected: `kubectl -n launchx-mesh get pods` 五行为 `2/2 Running`；
`kubectl -n launchx-mesh get deploy` 五个均 available。

- [x] **Step 3: 入口连通性独立复核（脱离部署脚本再确认一次）**

部署脚本跑完不等于入口通。用集群内已在跑的 `istio-proxy`（proxyv2 自带 curl）复核一次
—— 这条命令与 `bring-up.sh verify` 的探针机理相同，但**不依赖脚本状态**，
是「502 到底是清单问题还是脚本问题」的分水岭。

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx/examples
KUBECTL_NS=launchx-mesh
kubectl -n $KUBECTL_NS get pods -l app=bff -o name
kubectl -n $KUBECTL_NS exec "$(kubectl -n $KUBECTL_NS get pods -l app=bff -o name | head -1)" -c istio-proxy -- \
  curl -s -o /dev/null -w '%{http_code}\n' \
  'http://istio-ingressgateway.istio-system.svc.cluster.local/api/order/2?user_id=1'
kubectl -n $KUBECTL_NS get endpoints srv-order srv-points srv-product srv-stock
```
Expected: 第二条打印 `200`；第三条四个 Endpoints 均有非空 `ENDPOINTS` 列。

若打印 `502`：apps 组不注册 consul，原因只可能是 `service.upstream.<域>` 的 FQDN 写错、
或目标 Service 的 selector 没匹配上，**不会**是注册竞态（那一类只发生在 micro 对照组）。
按序核对：`kubectl -n launchx-mesh get endpoints srv-order` 是否有地址 →
`kubectl -n launchx-mesh exec deploy/bff -c app -- cat /app/conf/bff.yaml` 是否与 ConfigMap 一致。

- [x] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 对应行同步）

---

### Task 15: T13 网格入链验证（路由矩阵）

> **trace:** plan-ready.md → `### Task 15: T13 网格入链验证（路由矩阵）` | tasks.md → `- [ ] 3.3 T13 网格入链验证并落盘证据：`VARIANT=apps bring-up.sh verify` 逐**路由矩阵**（6 条）打请求 → `trace-audit.py --mesh` 对每行断言「入口 2xx + 目标域 `service.<域>.*` span 在同一 trace + 该 trace 含 sidecar span（M1/M2/M3）」，另加**异步链**一条（`POST /order` 的 trace 内出现 `service.points.ConsumeOrderCompleted`，traceID 与下单请求一致）；输出连同版本口径写入 `examples/mesh/docs/verify.md` 的 §T13，**以矩阵表形式**`
> **sync:** tasks.md → `- [ ] 3.3 T13 网格入链验证并落盘证据：`VARIANT=apps bring-up.sh verify` 逐**路由矩阵**（6 条）打请求 → `trace-audit.py --mesh` 对每行断言「入口 2xx + 目标域 `service.<域>.*` span 在同一 trace + 该 trace 含 sidecar span（M1/M2/M3）」，另加**异步链**一条（`POST /order` 的 trace 内出现 `service.points.ConsumeOrderCompleted`，traceID 与下单请求一致）；输出连同版本口径写入 `examples/mesh/docs/verify.md` 的 §T13，**以矩阵表形式**` | plan-ready.md → `### Task 15: T13 网格入链验证（路由矩阵）`；spec → `mesh-entry-chain` 的三条验收 Requirement

> **⚠️ 前置依赖：Task 17 必须先完成。** 本 Task 用的 `--route-matrix` 判据与
> `PROBE_ROUTES` 循环打请求都是 Task 17 的产物。前置未完成时跑本 Task 只会得到
> 「未知参数」或仍只打一条 URL 的旧行为。

**Files:** `examples/mesh/docs/verify.md`（新增 §T13）

- [x] **Step 1: 逐路由打请求并跑矩阵判据**

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx/examples
PYTHON=/Users/hyx/.workbuddy/binaries/python/envs/default/bin/python3 VARIANT=apps bash mesh/scripts/bring-up.sh verify
```
Expected: 六条路由各打印一行 `HTTP=<2xx>`（其中 `POST /order` 打印新建的 `order_id`）；
`trace-audit.py` 输出**矩阵表**，每一行含四列：路由 / HTTP / 目标域 service span / sidecar 跳；
异步链单独一行显示 `service.points.ConsumeOrderCompleted` 与「traceID 与下单一致 ✓」；
末行 `结论：PASS`。

若矩阵里有行 FAIL，按缺的列分流排查：

| 缺的列 | 含义 | 先查什么 |
|---|---|---|
| HTTP 非 2xx | 路由没接上 / 下游不可达 | `kubectl -n launchx-mesh get endpoints <域>` 是否有地址；`conf` 的 `service.upstream.<域>` 与 Service 名是否逐字一致 |
| 目标域 service span 缺失 | 路由接到了**别的**域，或 handler 是空实现 | 对着 Task 7 的路由表核对 `Register` 里那一行的 handler 名；再看该域 `handler` 里那个方法是否真的调了 service |
| sidecar 跳缺失 | 网格没入链 | ① `deploy/shared/40-telemetry.yaml` 采样率是否为 100（Istio 默认极低，这条判据的头号假阴性来源）；② `kubectl -n launchx-mesh get endpoints host-middleware` 的 IP 是否等于 `minikube ssh -- "getent hosts host.minikube.internal" \| awk '{print $1}'`；③ 命名空间是否带 `istio-injection: enabled` |
| 异步链行缺失 | `POST /order` 没触发到消费侧 | ① `srv-order` 的 `MQ()` 是否为 nil（日志找 `mq producer unavailable`）；② `srv-points` 的 `MQConsumer()` 是否为 nil（找 `mq consumer unavailable`）；③ 等得不够久 —— 消费是异步的，Jaeger flush 前多等一轮 |

> 已知边界（不要当缺陷查）：`bff→srv` 那几跳走 Kitex TTHeader/TCP，Envoy 按
> `tcp_proxy` 处理，**永远没有 sidecar span**。矩阵里的 sidecar 列指的是
> `ingress→bff` 那一跳，四域 RPC 跳本来就不该有。

- [x] **Step 2: 落盘矩阵证据**

在 `examples/mesh/docs/verify.md` 新增一节 `## T13 —— apps 组（网格原生）`，
**以矩阵表形式**写入：

1. 版本口径（minikube / kubectl / istioctl / Jaeger / 宿主资源限制）；
2. 路由矩阵表（六行 × 四列：路由 / HTTP / 目标域 service span / sidecar 跳）；
3. **每条路由的请求命令原文**（含 `POST /order` 的完整 body）；
4. 异步链一行的 trace-id 与消费侧 span 名；
5. 结论行（PASS/FAIL）与 FAIL 时的原因分析。

并显式写入两条「不可交付项」：`bff→srv` 永远没有 sidecar span；
「治理已下沉网格」的准确表述是「mTLS + 连接级可观测 + HTTP 跳的 L7 治理」。

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx
grep -n 'T13' examples/mesh/docs/verify.md
grep -cE '(GET|POST) /api/' examples/mesh/docs/verify.md    # 期望 >= 6
```
Expected: 命中新增小节标题与判据段落；请求命令原文计数 ≥ 6（六条路由各一条）。

- [x] **Step 3: 矩阵可复跑自检**

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx/examples
VARIANT=apps bash mesh/scripts/bring-up.sh verify 2>&1 | tail -3
```
Expected: **再跑一次**仍得到 `结论：PASS`，且矩阵行数与上次一致。
这一条防的是「第一次 PASS 是陈旧 trace 凑出来的」：判据按 `--lookback` 自己算时间窗，
若第二次结果不稳定，说明时间窗或 flush 等待不足，回 Task 17 调 `--lookback` 与 `sleep`。

- [x] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 对应行同步）

---

### Task 16: 文档校正

> **trace:** plan-ready.md → `### Task 16: 文档校正` | tasks.md → `- [ ] 3.4 文案校正：`apps/README.md` 把「srv 必须新补 HTTP 探针」修正为「需要 HTTP 探针的是 HTTP 跳的目标 bff，bff 的 webx 已自带健康端点，apps 未因此新增进程或端口」；`examples/mesh/README.md` 的目录段与结论索引同步 apps 组清单`
> **sync:** tasks.md → `- [ ] 3.4 文案校正`；plan-ready.md → `### Task 16: 文档校正`；spec → `mesh-deploy-manifests` 的「探针类型按跳的协议选定」Requirement（本 Task 是它的落地动作）

**Files:** `examples/mesh/apps/README.md`（§4.3）、`examples/mesh/README.md`（§1 目录、§7 验收落点）

本 Task 不产出功能，只把前 15 个 Task 过程中被推翻的两处结论从文档里清掉。之所以单独立题，
是因为这两处都属于「早期推导错误但写得像结论」——留着会被后来人当成既定事实照抄。

- [x] **Step 1: 校正 `apps/README.md` §4.3 的探针口径**

`examples/mesh/apps/README.md:105-108` 现为：

```markdown
**要新补的（反向项）**：micro 的 srv 用的是 `tcpSocket` 探针
（`../deploy/micro/20-srv.yaml:64-71`），而 Istio 的 `outlier detection` 需要 **HTTP**
探针才能判 5xx 并剔除实例。这是网格带来的「必须多写」，不是减法 —— 列在这里是为了
避免「全部下沉」的错觉。
```

整段替换为：

```markdown
**要新补的（反向项）—— 校正后结论：一处也没补。**

早期版本这里写的是「srv 必须新补 HTTP 探针」。这个结论**不成立**，准确口径是：

- Istio 的 `outlier detection` 要判 5xx 并剔除实例，确实需要 **HTTP** 探针 —— 但需要它的是
  **HTTP 跳的目标**，也就是 `bff`，不是四域 `srv`。
- `bff` 的 `webx` 已自带 `/healthz`（挂载在 `internal/bff/app/setup.go`），清单里直接写
  `httpGet: {path: /healthz, port: http}` 即可，**没有为这条判据新增任何进程或端口**。
- 四域 `srv` 是 RPC 跳的目标（Kitex TTHeader over TCP），Envoy 在这一跳只看连接不看请求，
  沿用 micro 的 `tcpSocket` 探针即可（对照组证据：`../deploy/micro/20-srv.yaml:64-71`）。
  此处本就没有可判 5xx 的 HTTP 语义，补 HTTP 探针属于无意义改造。

所以「反向项」一栏在本目录的实际取值是：**零**。保留这段是为了留下推导痕迹，
防止后来人重新踩一遍这个错误结论。
```

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx
grep -n "要新补的（反向项）" examples/mesh/apps/README.md                        # 期望命中
grep -nE '必须多写|避免「全部下沉」的错觉' examples/mesh/apps/README.md            # 期望零命中
```

Expected: 第一条命中 §4.3 下校正后的段落；第二条**零命中**（旧结论的措辞已被清除）。

注意：`grep -n "srv 必须新补 HTTP 探针"` 在新版本里**仍会命中一处** —— 校正后的段落刻意
以「早期版本这里写的是『srv 必须新补 HTTP 探针』」引用旧结论，为的是留下推导痕迹。
这是有意保留的，不作为失败判据；判旧结论是否已清除，认的是「必须多写」那一条断言。
同理，本机 grep 是 toybox，多模式一律用 `-E`，不要用 BRE 的 `\|`。

- [x] **Step 2: 同步 `examples/mesh/README.md` 的目录段与结论索引**

四处定点修改（均在 §1 的代码块内 + §7 核对）。

**2.1 原生组清单说明去掉「随实现补齐」**（`examples/mesh/README.md:37`）：

```diff
-│   └── apps/                 #   原生组清单（5 Deployment，随实现补齐）
+│   └── apps/                 #   原生组清单（5 Deployment + 入口 + 授权）
```

**2.2 证据落盘范围补 T13**（`examples/mesh/README.md:40`）：

```diff
-└── docs/verify.md            # T10–T12 证据落盘
+└── docs/verify.md            # T10–T13 证据落盘
```

**2.3 §1 目录树里 `apps/README.md` 的说明也要一起改**（`examples/mesh/README.md:27`）：
该行现写「职责边界表 + **三处**不可交付项 + 四类减法清单」，而 `apps/README.md` §2 的实际
标题是「**两处**不可交付项」（探针那条在 Step 1 校正后已并入 §4.3 的反向项段落，不再单列）。

```diff
-│   └── README.md             # ★ 职责边界表 + 三处不可交付项 + 四类减法清单
+│   └── README.md             # ★ 职责边界表 + 两处不可交付项 + 四类减法清单（含反向项校正）
```

**2.4 §7 验收落点核对**：该表在 Task 15 前已预留 T13 行
（`examples/mesh/README.md:149`，判据「apps 原生组：五进程应用 span 与 sidecar span 同 trace」），
本步只需确认其证据列指向 `docs/verify.md` §T13 —— 与 Task 15 Step 2 实际写入的小节标题
逐字一致。不一致就改这里（不改证据文件）。

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx
grep -n "随实现补齐" examples/mesh/README.md          # 期望零命中
grep -n "T10–T12" examples/mesh/README.md             # 期望零命中
grep -n "三处不可交付项" examples/mesh/README.md        # 期望零命中
grep -n "两处不可交付项" examples/mesh/README.md        # 期望命中（与 apps/README.md §2 标题一致）
grep -n "docs/verify.md §T13" examples/mesh/README.md  # 期望命中 §7 表格的 T13 行
```

Expected: 前三条零命中；后两条各命中一次。若「两处不可交付项」零命中，说明 §1 没改到位。

- [x] **Step 3: 结构守卫与制品校验**

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx
export PATH=$HOME/sdk/go1.24.3/bin:$PATH
gofmt -l . | grep -v '^examples/monolith' ; echo "gofmt rc=$?"
go test -run 'TestMeshGoCodeOnlyUnderApps|TestMicroNotCoupledToMesh|TestLibrarySourceHasNoDirectMiddlewareImports' -v .
openspec validate add-mesh-native-apps --strict
```

Expected:
- `gofmt -l` 对 apps 目录零输出（本 Task 只动 markdown，属回归确认）；
- 三个守卫用例全部 `PASS`（文档改动不得影响目录守卫）；
- `openspec validate` 输出 `Change 'add-mesh-native-apps' is valid`，退出码 0。

若 `openspec validate` 报 Requirement 与 spec 不一致，说明 Step 1 的改写幅度超过了
`mesh-deploy-manifests` spec 里「探针类型按跳的协议选定」那条的措辞 —— 以 spec 为准回调文档，
不要反向改 spec（spec 是 Task 12/13 的验收基线）。

- [x] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 对应行同步）

---

### Task 17: 入口链条判据矩阵化（trace-audit.py + bring-up.sh）

> **trace:** plan-ready.md → `### Task 17: 入口链条判据矩阵化（trace-audit.py + bring-up.sh）` | tasks.md → `- [ ] 4.1 判据脚本矩阵化：…` 与 `- [ ] 4.2 `bring-up.sh verify` 逐路由打请求：…`
> **sync:** tasks.md → `- [ ] 4.1 判据脚本矩阵化` + `- [ ] 4.2 bring-up.sh verify 逐路由打请求` | plan-ready.md → `### Task 17: 入口链条判据矩阵化（trace-audit.py + bring-up.sh）`；spec → `mesh-entry-chain` 的「每条路由的验收必须断言到目标域的业务层」「异步链必须单独断言」；`mesh-deploy-manifests` 的「变体不得把 apps 的验收退回单路径」

> **⚠️ 本 Task MUST 早于 Task 15 执行。** Task 15 的 `--route-matrix` 与 `PROBE_ROUTES`
> 都是本 Task 的产物。

**Files:** `examples/scripts/trace-audit.py`（新增 `--route-matrix`）、
`examples/mesh/scripts/bring-up.sh`（`PROBE_URL` → `PROBE_ROUTES` + 循环打请求）

**这是本 Task 存在的全部理由**：初稿的判据是「打一条 URL，trace 里有 sidecar span 就 PASS」。
它能证明网格入链了，**证明不了入口的六条路由各自通到了对应域的业务逻辑** —— 六条路由全接到
order 域、或路由存在但 handler 空实现，判据都会绿。所以要把「存在性」升级为「逐路由的
目标域业务 span 存在性」。

- [x] **Step 1: 给 `trace-audit.py` 增加 `--route-matrix`**

矩阵文件格式（一行一条，`#` 开头为注释；字段用 `|` 分隔）：

```
# 路径|方法|期望域名|期望 service span 前缀|请求体(可选)
/api/ping|GET|-|-
/api/order/2|GET|order|service.order.GetOrder
/api/order|POST|order|service.order.CreateOrder|{"user_id":1,"items":[{"product_id":1,"name":"demo","quantity":1,"unit_price":100}]}
/api/points/1|GET|points|service.points.GetPoints
/api/product/1|GET|product|service.product.GetProduct
/api/stock/1|GET|stock|service.stock.GetStock
# 异步链：不以 HTTP 路由形态出现，单独一档
+async|order|service.points.ConsumeOrderCompleted
```

判据（对每一行）：

1. 在 `--lookback` 窗口内，从**全域服务并集**（`bff` + 四个 `srv-*`，按 traceID 去重）里找到
   一条 trace，满足「含目标域 `service.<域>.*` span」**且**「含 `handler.*` span（bff 侧 HTTP）」
   **且**「含 sidecar span（`component=proxy` + `upstream_cluster`，即复用既有 M2）」；
2. 该 trace 的根 span HTTP 状态可读时（`http.status_code` / `http.response.status_code`）
   MUST 为 2xx；标签不存在时记为 `n/a` 并**不**判失败（webx 的根 span 不一定打这个 tag，
   实测口径见 `trace-audit.py` 顶部注释）；
3. `+async` 行：MUST 找到一条 trace 同时含 `service.order.CreateOrder` 与
   `service.points.ConsumeOrderCompleted`（同一 trace = mqx 的注入/提取生效）。

输出为矩阵（列：行号 / 路由 / HTTP / 目标域 span / sidecar 跳）。任一行为 FAIL 即 `return 1`。
**MUST NOT** 把判据降级成「任一行通过即 PASS」或「去掉 M2 让它变绿」。

新增 CLI 参数：

```
--route-matrix FILE   路由矩阵文件；给出后启用矩阵判据（与 --mesh 组合使用）
--mesh                （既有）启用 M1/M2/M3
--service             矩阵模式下自动扩展为 bff + 四域（不再要求手填）
```

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx
/Users/hyx/.workbuddy/binaries/python/envs/default/bin/python3 -c "import ast,sys; ast.parse(open('examples/scripts/trace-audit.py').read())" && echo "语法 OK"
grep -nE 'route.matrix|route_matrix' examples/scripts/trace-audit.py | head
```
Expected: 打印 `语法 OK`；`grep` 命中 argparse 的参数声明与判据实现。
另需 `--help` 自检：`python3 examples/scripts/trace-audit.py --help` 里出现 `--route-matrix`。

- [x] **Step 2: 反例自检（判据必须真的能拦下接线错误）**

**这一步不能省。** 一个「永远 PASS」的判据比没有判据更糟：它会让人相信接线是对的。

写一个**故意写错**的矩阵文件 —— 把 `/api/product/1` 那一行的期望域写成 `points`
（模拟「路由接到了别的域」）：

```bash
cat > /tmp/bad-matrix.txt <<'EOF'
/api/product/1|GET|points|service.points.GetPoints
EOF
```

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx/examples
PYTHON=/Users/hyx/.workbuddy/binaries/python/envs/default/bin/python3 \
JAEGER=http://127.0.0.1:16686 \
"${PYTHON}" ../scripts/trace-audit.py --base http://127.0.0.1:16686 \
  --route-matrix /tmp/bad-matrix.txt --lookback 5m --mesh ; echo "rc=$?"
```
Expected: `rc=1`，且 FAIL 原因指名「`/api/product/1` 的 trace 内没有 `service.points.*` span」。
**若 `rc=0`，说明判据没做事 —— 回 Step 1 重写，不得进入 Step 3。**

> 前置：本步需要集群里已有跑过一轮请求的 trace（即 Task 14 已完成）。
> 若此时还没部署，可先把 Step 1/3 做完、把本步留到 Task 15 之前补跑，
> 但 **Task 15 MUST NOT 在没跑过本步的情况下开始**。

- [x] **Step 3: `bring-up.sh` 改为逐路由打请求**

把 `VARIANT` 分支里的 `PROBE_URL`（单值）替换为 `PROBE_ROUTES`（六条），
并把 `cmd_verify` 的单次 `mesh_curl` 换成循环：

```bash
# PROBE_ROUTES —— 路由矩阵，每行：路径|方法|body(可空)
# 与 mesh-entry-chain「bff 的路由表」逐条对应；改这里 MUST 同步改矩阵文件。
apps)
  IMAGE="${IMAGE:-launchx-mesh-apps:dev}"
  PROBE_BASE="${PROBE_BASE:-http://istio-ingressgateway.istio-system.svc.cluster.local}"
  PROBE_ROUTES=(
    '/api/ping|GET|'
    '/api/order/2|GET|'
    '/api/order|POST|{"user_id":1,"items":[{"product_id":1,"name":"demo","quantity":1,"unit_price":100}]}'
    '/api/points/1|GET|'
    '/api/product/1|GET|'
    '/api/stock/1|GET|'
  )
  TRACE_SERVICE="${TRACE_SERVICE:-bff}" ;;
micro)
  IMAGE="${IMAGE:-launchx-demo:dev}"
  PROBE_BASE="${PROBE_BASE:-http://demo-gateway.${NS}.svc.cluster.local:8080}"
  # 对照组保持单路径：它的入口本来就只有 1 条路由，矩阵在它身上没有意义。
  PROBE_ROUTES=('/api/order/2?user_id=1|GET|')
  TRACE_SERVICE="${TRACE_SERVICE:-demo-gateway}" ;;
```

`mesh_curl` 增加方法/body 入参；`cmd_verify` 改为：

```bash
cmd_verify() {
  echo "== 逐路由打请求（${VARIANT} 组，共 ${#PROBE_ROUTES[@]} 条）=="
  local line path method body
  for line in "${PROBE_ROUTES[@]}"; do
    IFS='|' read -r path method body <<<"$line"
    mesh_curl "${PROBE_BASE}${path}" "$method" "$body"
  done
  echo
  echo "== 等 Jaeger flush（batch 约 5s）+ 等异步链消费完成 =="
  sleep 15        # 比初稿的 12s 多 3s：POST /order 的消费 span 是异步落盘的
  echo
  echo "== T13 判据：路由矩阵（入口 → bff → 目标域业务层 + sidecar）=="
  "${PYTHON:-python3}" "${LAUNCHX_ROOT}/examples/scripts/trace-audit.py" \
    --base "$JAEGER" --service "$TRACE_SERVICE" --lookback 3m --mesh \
    --route-matrix "${LAUNCHX_ROOT}/examples/mesh/scripts/route-matrix-${VARIANT}.txt" || true
}
```

矩阵文件按变体落盘（`examples/mesh/scripts/route-matrix-apps.txt` /
`route-matrix-micro.txt`），内容与上面的 `PROBE_ROUTES` 逐条对应 —— 两处 MUST 一致。

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx
bash -n examples/mesh/scripts/bring-up.sh && echo "语法 OK"
VARIANT=micro bash examples/mesh/scripts/bring-up.sh check
```
Expected: `语法 OK`；`micro` 的 check 仍通过（本步骤不得破坏对照组的离线校验路径）。

- [x] **Step 4: 回归 —— micro 变体的行为必须不变**

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx/examples
VARIANT=micro bash mesh/scripts/bring-up.sh verify 2>&1 | tail -6
```
Expected: 仍只打**一条** URL；`trace-audit.py` 仍输出 `结论：PASS`；
覆盖跳仍是 `['PassthroughCluster', 'inbound|8080||', 'inbound|8081||']` 那一组。

这条防的是「为了让 apps 过而改了公用路径，把 micro 的对照组证据链弄坏」——
`add-mesh-native-apps` 的立项前提之一就是 micro 组必须保持可复现。

- [x] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 对应行同步）

---

### Task 18: srv 分层埋点与 `rpc/` 入向适配层回填（四域，order 先做）

> **trace:** plan-ready.md → `### Task 18: srv 分层埋点与 rpc/ 入向适配层回填（四域，order 先做）` | tasks.md → `- [ ] 4.4 srv 分层埋点与 `rpc/` 入向适配层回填：四域各自的 `rpc/<域>.go` 实现生成的 `<域>` 接口**全部方法**（含编译期断言 `var _ <域>.<域> = (*Server)(nil)`）、开 `rpc.<域>.<Op>` span、经 `route` 闸门后委派 `handler`，未接线方法显式 `codes.Unimplemented`；`handler`/`service`/`repository` 三层各补 `layer.Begin(ctx, "<layer>", "<域>", "<Op>", traceOn, quiet)`（`repository` 的 `quiet` 对 `ErrRecordNotFound` 豁免），由 `trace.enabled` 门控；组装改为 `NewServer(rpc.New(handler.New(svc), guard, traceOn), opts...)`，撤销 `app/setup.go` 里的 `rpcAdmit` 服务端中间件；`pkg/codes` 补 `Unauthorized`(10401) 与 `RateLimited`(10429) 供闸门拒绝用（拒绝码语义与 micro 的 1004/1005 对应）。验收：`apps/internal/srv/**` 内零出向客户端（`NewClient(` / `client.Option` / `RPCClientOptions(` 在非测试源码零命中）；`rpc`/`handler`/`service`/`repository` 四个**分层包**单测覆盖 ≥ 90%（`app` 装配层以 build/vet 兜底）；一条 trace 内四层 span 按 `rpc→handler→service→repository` 父子链齐全（由 `TestLayerSpanChain` 锁定；`trace.enabled=false` 时四层全不出现）；`Unauthorized` 与 `RateLimited` 各有真实生产者`
> **sync:** tasks.md → `- [ ] 4.4 srv 分层埋点与 `rpc/` 入向适配层回填：四域各自的 `rpc/<域>.go` 实现生成的 `<域>` 接口**全部方法**（含编译期断言 `var _ <域>.<域> = (*Server)(nil)`）、开 `rpc.<域>.<Op>` span、经 `route` 闸门后委派 `handler`，未接线方法显式 `codes.Unimplemented`；`handler`/`service`/`repository` 三层各补 `layer.Begin(ctx, "<layer>", "<域>", "<Op>", traceOn, quiet)`（`repository` 的 `quiet` 对 `ErrRecordNotFound` 豁免），由 `trace.enabled` 门控；组装改为 `NewServer(rpc.New(handler.New(svc), guard, traceOn), opts...)`，撤销 `app/setup.go` 里的 `rpcAdmit` 服务端中间件；`pkg/codes` 补 `Unauthorized`(10401) 与 `RateLimited`(10429) 供闸门拒绝用（拒绝码语义与 micro 的 1004/1005 对应）。验收：`apps/internal/srv/**` 内零出向客户端（`NewClient(` / `client.Option` / `RPCClientOptions(` 在非测试源码零命中）；`rpc`/`handler`/`service`/`repository` 四个**分层包**单测覆盖 ≥ 90%（`app` 装配层以 build/vet 兜底）；一条 trace 内四层 span 按 `rpc→handler→service→repository` 父子链齐全（由 `TestLayerSpanChain` 锁定；`trace.enabled=false` 时四层全不出现）；`Unauthorized` 与 `RateLimited` 各有真实生产者` | plan-ready.md → `### Task 18: srv 分层埋点与 rpc/ 入向适配层回填（四域，order 先做）`；spec → mesh-entry-chain「跨域编排只在 bff，srv 之间不互调」「四域 RPC 实现体必须实现全部 IDL 方法」；mesh-app-layering「四层 span 逐层可见」

**本 Task 是 Task 4 / 5 / 6 的模板**：先只改 `order` 域把形态定死，再由 4/5/6 按同一模板
复制到 points / product / stock。**MUST 先于 Task 4 执行。**

**Files:**
- Modify: `examples/mesh/apps/pkg/codes/{codes.go,codes_test.go}`（补两个码）
- Rewrite: `examples/mesh/apps/internal/srv/order/rpc/order.go`（出向客户端 → 入向适配层）
- Modify: `examples/mesh/apps/internal/srv/order/{handler/order.go,service/order.go,repository/order.go}`（三层埋点）
- Modify: `examples/mesh/apps/internal/srv/order/app/setup.go`（组装改 `rpc.New` + 撤销 `rpcAdmit`）
- Modify: `examples/mesh/apps/go.mod`（`otelx` 由 indirect 升为 direct，`go mod tidy` 后）

- [x] **Step 0: `pkg/codes` 补 `Unauthorized` / `RateLimited`**

`rpc/` 层的闸门要在「身份**非法**」与「被限流」两种拒绝上回**不同的码**：限流值得退避重试，
身份头坏了重试一万次也是同一个结果 —— 混成一个码等于把身份故障伪装成容量问题
（`examples/micro/pkg/codes` 的 1004/1005 就是为这个区分而拆的）。

**两个码各自的生产者必须是真的**（不许「定义到位但无人使用」——那是本次 amend 要消灭的那类
错觉）。apps 里它们的落点：

- `Unauthorized`：`rpc.admit` 里**metainfo 的 `mesh-user-id` 解析失败**（bff 写坏了身份头）。
  MUST NOT 因为「请求没带身份」就回 401 —— apps 的鉴权口径是**传输层身份下沉给 Istio**
  （Task 3 的 `route/route.go` 注释已写明这条减法），应用侧对匿名请求是 fail-open 按服务级
  计数，不是拒绝。
- `RateLimited`：`route.Guard.Admit` 返回 false（超过该主体的窗口上限）。

在 `pkg/codes/codes.go` 的常量块里、`Unimplemented` 之后追加：

```go
	// Unauthorized 身份非法：metainfo 里的 identity 解析不出来（bff 侧写坏），
	// 重试无意义；bff 映射 401。
	Unauthorized Code = 10401

	// RateLimited 被限流：值得退避重试（bff 映射 429）。
	// 与 Unauthorized 分开是刻意的：客户端对两者的重试策略相反。
	RateLimited Code = 10429
```

`codes_test.go` 的表驱动用例同步补两项（漏加会让覆盖率掉出 100%）。

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx/examples/mesh/apps
gofmt -l pkg/codes && go test -count=1 -cover ./pkg/codes/
grep -nE "Unauthorized|RateLimited" pkg/codes/codes.go
```
Expected: `gofmt` 无输出；覆盖率 100%；`grep` 命中两处声明。

- [x] **Step 1: 把 `internal/srv/order/rpc/order.go` 从「出向客户端」重写为「入向适配层」**

参照 `examples/micro/srv-order/internal/rpc/order.go` 的形态（**只读**，不 import）：

```go
// Package rpc 入向传输适配层：链的最外一节，也是唯一认识 kitex 的一层。
//
// 职责边界（写窄，别让业务渗进来）：
//   - 实现生成的 order.Order 接口**全部方法**（kitex 按接口签名约束 NewServer 入参，
//     少一个方法编译期即失败）；
//   - 开 rpc.order.<Op> 分层 span，把 kitex 带进来的 ctx 接进链路；
//   - 经 route.Guard 走准入闸门，被拒时回**对应的**拒绝码（身份非法 401 / 被限流 429）；
//   - 把请求原样交给 handler（不做任何字段加工）。
//
// 本层不做出向调用：apps 的 srv 之间不互调（见 mesh-entry-chain 的同名 Requirement），
// 且 srv 档没有出向客户端的装配来源，故这里只负责入向。
package rpc

import (
	"context"
	"strconv"

	"github.com/bytedance/gopkg/cloud/metainfo"
	"github.com/gospacex/otelx/layer"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/handler"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/route"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	orderpb "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/order"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/routing"
)

// Server 实现生成的 order.Order 接口。
type Server struct {
	handler *handler.OrderHandler
	guard   *route.Guard
	traceOn bool // trace.enabled（总闸），由装配层注入
}

// New 构造入向适配层。
func New(h *handler.OrderHandler, g *route.Guard, traceOn bool) *Server {
	return &Server{handler: h, guard: g, traceOn: traceOn}
}

// 编译期断言：接口必须全实现（放在 rpc 层，不放 handler）。
var _ orderpb.Order = (*Server)(nil)

// CreateOrder 下单（3 表事务 + 事件投递）。
//
// 结果列表命名：与 ListOrders / Report 同形，让 defer 里的 end(err) 覆盖所有
// return 路径（Go 要求「全命名或全不命名」）。`return nil, aerr` 已经给命名的
// err 赋了值，**不要再写 `err = aerr`**（那是死赋值，会让人误以为它承担语义）。
func (s *Server) CreateOrder(ctx context.Context, req *orderpb.CreateOrderReq) (resp *orderpb.CreateOrderResp, err error) {
	ctx, end := layer.Begin(ctx, "rpc", "order", "CreateOrder", s.traceOn, nil)
	defer func() { end(err) }()

	if aerr := s.admit(ctx, req.GetUserId()); aerr != nil {
		return nil, aerr
	}
	return s.handler.CreateOrder(ctx, req)
}

// GetOrder 查单。
func (s *Server) GetOrder(ctx context.Context, req *orderpb.GetOrderReq) (resp *orderpb.GetOrderResp, err error) {
	ctx, end := layer.Begin(ctx, "rpc", "order", "GetOrder", s.traceOn, nil)
	defer func() { end(err) }()

	if aerr := s.admit(ctx, req.GetUserId()); aerr != nil {
		return nil, aerr
	}
	return s.handler.GetOrder(ctx, req)
}

// ListOrders 未接线：本示例不提供列表页。与 CreateOrder / GetOrder 同形 ——
// 只开 rpc.order.ListOrders span 后原样委派 handler（错误码映射归 handler）；
// **不过 admit 闸门**：未接线、无业务主体可限，开了只会误伤限流桶。
// （这是对 mesh-app-layering 责任表「rpc 层走准入闸门」的有意例外，按方法分档。）
func (s *Server) ListOrders(ctx context.Context, req *orderpb.ListOrdersReq) (resp *orderpb.ListOrdersResp, err error) {
	ctx, end := layer.Begin(ctx, "rpc", "order", "ListOrders", s.traceOn, nil)
	defer func() { end(err) }()
	return s.handler.ListOrders(ctx, req)
}

// Report 未接线：同上，只开 span 后委派 handler，不过 admit 闸门。
func (s *Server) Report(ctx context.Context, req *orderpb.ReportReq) (resp *orderpb.ReportResp, err error) {
	ctx, end := layer.Begin(ctx, "rpc", "order", "Report", s.traceOn, nil)
	defer func() { end(err) }()
	return s.handler.Report(ctx, req)
}

// admit 走准入闸门。
//
// 限流主体的来源按优先级取**四处**（前者非零即短路，不再往下取）：
//   1. 同进程 ctx 里的 routing identity（`routing.IdentityFrom`）—— 单测 / 直连时有效；
//      它的值过不了进程边界，所以线上这条通常是空的；
//   2. kitex metainfo 的 mesh-user-id（由 bff 在出向调用前写入 —— 跨进程的身份载体）。
//      **解析失败即 Unauthorized**：身份头坏了要出声，不许静默降级成匿名；
//   3. 回落到本次请求 IDL 里的 user_id（订单域的请求本就带 user_id，够用作限流主体）；
//   4. 都没有时按 "svc" 分桶（服务级计数）—— 这才是「没带身份」的正当处理：放行 + 服务级计数。
//
// 注意 Unauthorized 的语义是「身份**非法**」，不是「没带身份」：没带身份（metainfo
// 键不存在）走第 3/4 条回落，fail-open。apps 的鉴权口径把传输层身份下沉给 Istio，
// 故应用侧不因匿名而拒绝（见 Task 3 的 route/route.go 注释）。
func (s *Server) admit(ctx context.Context, userID int64) error {
	id := routing.IdentityFrom(ctx) // 同进程内调用时有效（单测 / 直连）
	if id.UserID == 0 {
		if v, ok := metainfo.GetValue(ctx, identityMetaKey); ok {
			n, perr := strconv.ParseUint(v, 10, 64)
			if perr != nil || n == 0 {
				return codes.New(codes.Unauthorized,
					"malformed identity in metainfo key "+identityMetaKey)
			}
			id.UserID = n
		}
	}
	if id.UserID == 0 && userID > 0 {
		id.UserID = uint64(userID)
	}
	if s.guard.Admit(ctx, id) {
		return nil
	}
	return codes.New(codes.RateLimited, "rate limited")
}

// identityMetaKey bff 透传身份的 kitex metainfo 键名；与 Task 7 的写入侧 MUST 一致。
const identityMetaKey = "mesh-user-id"
```

> **跨 Task 契约（与 Task 7 对齐）**：srv 侧从 kitex `metainfo` 读身份键
> `mesh-user-id`（`metainfo.GetValue(ctx, "mesh-user-id")`），bff 在出向 RPC 前
> `metainfo.WithValue(ctx, "mesh-user-id", ...)` 写入 —— 两处的**键名 MUST 完全一致**，
> 否则 srv 永远读到零值，闸门退化成服务级限流（这正是 Task 3 的 setup 注释里
> 「键名必须与 bff 写入的一致」所指）。本 Task 只落地 srv 侧的读取与回落；
> bff 侧的写入归 Task 7。

同时把 `handler/order.go` 的编译期断言 `var _ orderpb.Order = (*OrderHandler)(nil)` 删掉
（接口实现方已迁到 `rpc/`），并在文件头注释里写明「本层不实现 IDL 接口，唯一的实现方是
`rpc/`」。`handler` 的四个方法保持原签名（`rpc/` 直接调）。

> **模板口径（Task 4/5/6 照抄这条，别再分叉）**：
>
> - **`rpc/` 的四个方法一律委派 `handler`，不在本层做方法级特判**（不留「已接线走 handler、
>   未接线就地返回」的分叉）—— 否则接口实现方会出现不可达分支，等于把死代码从旧的出向
>   客户端挪到了新位置；
> - **`Unimplemented` 只有一个落点：`handler`**（「本示例未接线」是错误码映射，属 handler 的
>   职责，不是业务判断）。`handler.ListOrders` / `handler.Report` 返回
>   `codes.New(codes.Unimplemented, "<域>.<方法> not wired in mesh apps demo")`；
> - 「四域 RPC 实现体必须实现全部 IDL 方法」因此落在 `rpc/`（编译期断言在这里），
>   而「未接线显式声明」落在 `handler` —— 两者各只有一处，`grep` 就能验收。

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx/examples/mesh/apps
gofmt -l internal/srv/order/rpc internal/srv/order/handler
go build ./internal/... && go vet ./internal/...
grep -n "var _ orderpb.Order" internal/srv/order/rpc/order.go   # 期望命中
grep -n "var _ orderpb.Order" internal/srv/order/handler/order.go && echo "不应命中" || echo "handler 断言已移除 ✓"
```
Expected: `gofmt` 无输出；build / vet 退出码 0；两处 `grep` 分别命中 / 不命中。

- [x] **Step 2: 给 handler / service / repository 三层补分层埋点**

三处各加一行，`traceOn` 一律取 `trace.enabled`（由装配层注入构造函数），
`repository` 的 `quiet` 传 `gorm.ErrRecordNotFound` 的判定（未命中不是故障，
不该污染错误率）：

```go
// handler（entity=order，op=方法名）
return layer.Begin(ctx, "handler", "order", "CreateOrder", h.traceOn, nil)

// service
return layer.Begin(ctx, "service", "order", "CreateOrder", s.traceOn, nil)

// repository（quiet：ErrRecordNotFound 是正常 miss）
func quietNotFound(err error) bool { return errors.Is(err, gorm.ErrRecordNotFound) }
return layer.Begin(ctx, "repository", "order", op, r.traceOn, quietNotFound)
```

调用形态与 micro 逐字一致：`ctx, end := layer.Begin(...)` → `var err error` →
`defer func() { end(err) }()` → 结果列表**全部命名**（Go 要求「全命名或全不命名」，
否则 defer 里的 `err` 覆盖不到所有 return 路径）。

> 注意：`handler` / `service` / `repository` 的构造签名要各加一个 `traceOn bool`
> 参数（`repository.New(db, traceOn)` 已有；`handler.New(svc)` → `handler.New(svc, traceOn)`；
> `service.New(...)` 末尾加）。调用点只有 `app/setup.go` 与各自的 `_test.go`。

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx/examples/mesh/apps
gofmt -l internal/ && go build ./... && go vet ./...
grep -c "layer.Begin" internal/srv/order/handler/order.go internal/srv/order/service/order.go internal/srv/order/repository/order.go internal/srv/order/rpc/order.go
CGO_ENABLED=1 go test -count=1 -cover ./internal/srv/order/...
```
Expected: `gofmt` 无输出；build / vet 退出码 0；四处 `grep -c` 各 ≥ 1；四个包覆盖率 ≥ 90%。

- [x] **Step 3: `app/setup.go` 改为经 `rpc.New` 组装，撤销 `rpcAdmit`**

- 删掉 `rpcAdmit` 函数与 `server.WithMiddleware(rpcAdmit(guard))`（闸门已移进 `rpc/` 层）；
- `traceOn := cfg.GetBool("trace.enabled")` 一次取出，分别交给 `handler.New` / `service.New` / `rpc.New`；
- 组装行改为 `srv := orderrpc.NewServer(rpc.New(handler.New(svc, traceOn), guard, traceOn), opts...)`；
- 启动日志的 chain 文案改为 `rpc>route(auth>ratelimit)>handler>service>repository>model` —— 在
  改了 `rpc/` 语义之后，这条文案才与代码一致（旧文案 `rpc>route>...` 指向的是一个死客户端）。

Run: `cd examples/mesh/apps && gofmt -l internal/srv/order/app && go build ./... && go vet ./...`
Expected: `gofmt` 无输出；build / vet 退出码 0；`grep -n "rpcAdmit" internal/srv/order/app/setup.go` 零命中。

- [x] **Step 4: 四层 span 的开关语义验证**

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx/examples/mesh/apps
echo "== 四层各至少一处埋点（只看非测试源码）=="
grep -rlE "layer\.(Begin|Start)" internal/srv/order/ --include='*.go' | grep -v "_test.go" | sort
echo "== 应恰好这四层（rpc/handler/service/repository），共 4 个文件 =="
grep -rlE "layer\.(Begin|Start)" internal/srv/order/ --include='*.go' | grep -v "_test.go" | wc -l
echo "== 不自建 tracer、不手写传播头（应为空）=="
grep -rnE "otel\.Tracer\(|traceparent" internal/srv/ | grep -v "_test.go" ; echo "--- 上一行应为空 ---"
```
Expected: 非测试源码里恰好四个文件命中（`rpc/order.go`、`handler/order.go`、`service/order.go`、
`repository/order.go`）；`internal/srv/**` 内不出现自建 tracer，也不手写 `traceparent`。
（判据只数**非测试源码**：测试里的字符串字面量/注释出现 `layer.Begin` 字样不算命中，
否则判据会被文案左右 —— 这正是本仓 `grep` 口径踩过的坑。）
`trace.enabled=false` 时四层 span 全不出现的运行时验证归 Task 15 的矩阵；代码层的证据由
Step 5 的父子链单测给出。

- [x] **Step 5: 四层 span 的可执行判据（父子链 + 总闸关闭 + 闸门身份来源）**

`mesh-app-layering` 的 Scenario「四层 span 逐层可见」要求同一 trace 内按
`rpc → handler → service → repository` 的**父子顺序**出现四层 span。`grep -c layer.Begin`
只能证明「调了」，证明不了「接上了」—— 补一条用 `tracetest.SpanRecorder` 的单测
（`rpc/order_test.go` 里已有 recorder 基建，扩一条即可）：

```go
// TestLayerSpanChain 断言 rpc → handler → service → repository 四层 span 的父子链。
//
// 判据不是「四个名字都出现」，而是「每一个的 Parent() 是上一层」—— 少了这条，
// 四层各自成根（或挂在 kitex 那个不落地的 span 上）同样能通过「名字都在」的检查，
// 而那正是 rpcx 包注释里记着的「span 树在调用边界断成两棵子树」那类故障。
```

断言步骤：起一次 `traceOn=true` 的 `CreateOrder`（sqlite 内存库 + fake producer）→
从 recorder 取全部 span → 按 span 名取 `rpc.order.CreateOrder` / `handler.order.CreateOrder`
/ `service.order.CreateOrder` / `repository.order.Create` → 逐对断言
`child.Parent().SpanID() == parent.SpanContext().SpanID()`，且四者的 `TraceID()` 相同。

同批再落两条判据（都属本 Step 的可执行证据，别拆成「以后再说」）：

- **总闸关闭 → 四层 span 全不出现**（`TestTraceOffEmitsNoLayerSpans`）：用同一个 recorder
  起一次 `traceOn=false` 的 `CreateOrder`，断言 recorder 里**没有任何**以 `rpc.` /
  `handler.` / `service.` / `repository.` 开头的 span。注意**不要**断言 recorder 为空 ——
  组件级 span（`db.query` / `messaging.*`）由 `trace.on.*` 那套开关单独管，不属本判据
  （两套开关的分工见 `mesh-app-layering`）。
- **准入闸门的同进程身份来源**（`TestAdmitUsesInProcessIdentity`）：
  `routing.WithIdentity(ctx, routing.Identity{UserID: 42})` 起一次 `GetOrder`，
  断言限流键落 `:u42:` 桶。这条来源过不了进程边界、线上通常为空，因而最容易在重构里
  被误删而无人察觉。

三条都 MUST 做反向自检（先看 FAIL 再看 PASS）。已验证可用的两个探针：
① `rpc.New` 里把 `traceOn` 写死 `true` → 总闸用例 FAIL（报出 `rpc.order.CreateOrder`）；
② `admit` 里把 `routing.IdentityFrom(ctx)` 换成 `routing.Identity{}` → 身份用例 FAIL
（限流键退化成 `:svc:` 桶）。

Run:
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx/examples/mesh/apps
CGO_ENABLED=1 go test -count=1 -run 'TestLayerSpanChain|TestTraceOffEmitsNoLayerSpans|TestAdmitUsesInProcessIdentity' -v ./internal/srv/order/rpc/
CGO_ENABLED=1 go test -count=1 -cover ./internal/srv/order/...
```
Expected: 三条均 PASS；`internal/srv/order/rpc` 仍 100% 覆盖。
**先看它 FAIL 再看它 PASS**（把断言里的顺序故意写反一次以确认它真的在判父子链，不是恒真）。

- [x] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 对应行同步）

---

## 计划完成判据（全部为真才允许进入 build）

| # | 判据 | 校验命令 |
|---|---|---|
| 1 | 18 个 `### Task N` 连续无缺号，每个含 `> **trace:**` 与 `> **sync:**` | `grep -c '^### Task ' docs/superpowers/plans/2026-10-07-add-mesh-native-apps.md` → 18 |
| 2 | 每个 Task 末尾有 `- [ ] **Task complete**` | `grep -c '^- \[ \] \*\*Task complete\*\*' …` → 18 |
| 3 | 无 TBD / TODO / 「待补」占位符 | `grep -nE 'TBD|TODO|待补' …` → 零命中（注意用 `-E`，本机 grep 是 toybox，BRE 的 `\|` 静默返回 0 命中） |
| 4 | `tasks.md` 待办条数与 Task 数一致 | 见 tasks.md 的 `## 1/2/3/4` 四章合计 20 条（1/2/3 章 16 条 + 第 4 章 amend 追加 4 条，其中 4.1/4.2 合为 Task 17、4.4 为 Task 18） |
| 5 | `openspec validate add-mesh-native-apps --strict` 通过 | rc=0 |

计数口径：判据 1 的 `grep -c` 数的是 Task 标题行；判据 2 数的是勾选行。两者相差 1 是正常的
—— 本文件末尾的「计划完成判据」表用的是表格而非勾选行，不参与计数。

**Amend 说明（2026-10-07 #1）**：本计划原为 16 个 Task。经 `/sddflow amend` **追加 Task 17**
（入口链条判据矩阵化），并对 Task 3 / 4 / 5 / 6（handler 全接口 + `codes.Unimplemented`）、
Task 7（路由 2 → 6 条）、Task 15（单路径 → 路由矩阵）就地进行扩写。
Task 17 虽编号最大，但 **MUST 早于 Task 15 执行**。

**Amend 说明（2026-10-07 #2，Task 3 实现期）**：再追加 **Task 18**（srv 分层埋点与 `rpc/`
入向适配层回填），并对 Task 4 / 5 / 6（`rpc/` 层语义 + 四层埋点）、Task 7（错误码映射补
401/429）、Task 9 / Task 10（依赖面与同构判据口径）就地进行扩写；Task 3 的已勾选 Step
不回退，其增补由 Task 18 承担。
**执行顺序约束**：`Task 18 → Task 4 → Task 5 → Task 6`（Task 18 为其余三域定模板）；
`Task 17 → Task 15`（矩阵判据依赖 Task 17 的脚本）。

**Amend 说明（2026-10-07 #3，计划文本对齐 —— 不改规格、不加任务）**：Task 18 的规格复审
（第二轮）判定 ✅ 通过，但报出 5 条 Minor，其中 3 条会**被 Task 4/5/6 抄走**，故就地修正：

| # | 缺陷 | 修正 |
|---|---|---|
| N-3 | Task 18 Step 1 的示例代码块仍是**非命名结果**形态（`(*T, error)` + `var err error`），与同 Task Step 2「结果列表全部命名」自相矛盾，也与已落地的 `rpc/order.go` 不一致 | 示例块四个方法改为 `(resp *T, err error)` + `defer func(){ end(err) }()` 直返，与落地代码逐字对齐 |
| N-5 | `ListOrders`/`Report` **不过 `admit` 闸门**，而 `mesh-app-layering` 责任表只说「rpc 层走准入闸门」，未按方法分档 | 在 Step 1 的 `ListOrders` 注释里点明这是**有意例外**（未接线、无业务主体可限），并注明「按方法分档」 |
| N-1/N-2/N-4 | `pkg/codes/codes.go` 注释把 `Unauthorized` 写成「身份**缺失**或非法」（与 fail-open 行为相反）、`RateLimited` 引用了一条不相干的 Requirement、`handler` 未接线注释未用 spec 字面「非漏项」 | **代码侧**修正（属 build 职责）：`codes.go` 两处注释改写、`handler/order.go` 补「（非漏项）」 |

本段**不修改任何规格文件**，`openspec validate --strict` 结论不变；`tasks.md` 的 `4.4` 与
`plan-ready.md` 的 `Task 18` 验证口径在 amend #2 时已同步，本轮无需再动。

**Amend 说明（2026-10-07 #4，Task 4/5/6 文本纠正 —— 不改规格、不加任务）**：开始 Task 4 前
按「实现前先核实事实」的口径复查 Task 4/5/6 的代码样例，发现三类**计划文本与实测不符**，
逐条就地修正（不改任何 spec 文件）：

| # | 缺陷 | 实测事实 | 修正 |
|---|---|---|---|
| P-1 | Task 4 Step 2 的消费循环写成 `consumer.Consume(ctx, events.TopicOrderCompleted, func(c, msg mqx.Message) error {...})` —— **两个都错**（多了一个 topic 位置参数、`msg` 少了一个指针） | `mqx.Consumer.Consume(ctx context.Context, handler func(ctx context.Context, msg *Message) error) error`（`mqx/interface.go:42`）；**topic 不是入参**，构造消费端时从 `consumer.topic` 取（`mqx/kafka/kafka.go:138/157/196`） | 代码块改为 `consumer.Consume(ctx, func(c context.Context, msg *mqx.Message) error {...})`，并把实测签名直接写进 Expected（不再让实现者现场猜） |
| P-2 | `consumer.topic` 这条**跨 Task 契约**没人写：Task 8 的本地 conf 与 Task 11 的 ConfigMap 都可能漏配，症状是「消息发了没人收」而**两侧都不报错** | `apps/pkg/events/events.go:16-19` 已记录键位口径：mqx/kafka 现状是 **producer 与 consumer 都读 `consumer.topic`**（`FillDefaults` 只把该键填给 writer），配 `producer.topic` 不生效 | Task 4 Step 2 增加「跨 Task 契约（与 Task 8 / Task 11 对齐）」引块：`srv-points` 的 `consumer.topic` MUST = `order.completed.v1` |
| P-3 | Task 4/5/6 的差异表仍写「未接线方法由 `rpc/` 层返回 `codes.Unimplemented`」，与 Task 18 的**模板口径**（`Unimplemented` 唯一落点是 `handler`）矛盾 —— Task 4/5/6 照抄会把接口实现方搞出不可达分支 | Task 18 Step 1 已裁决并写进「模板口径」块；order 域已按此落地（`handler/order.go:106,111`） | 三张差异表的 `handler` 行补「**唯一落点在 `handler`**」并注明理由归属 Task 18 模板口径；`rpc` 行改为「只开 span 后委派 handler」 |

**附带记录（不是缺陷，但会影响 Task 15 的措辞）**：`examples/micro/**` 全仓 **零 `MQConsumer()` 用法**
（`grep -rn "MQConsumer" examples/micro/` 无命中），micro 的 `srv-points` 也**没有**
`ConsumeOrderCompleted`。即 **「order → MQ → points」这条异步链是 apps 组独有的能力，不是形态对照项**：
micro 对照组（`demo-*`）只有同步 HTTP 链。`mesh-entry-chain` 的异步链 Requirement 只绑
`srv-points`（apps），未声称 micro 也有，故 spec 无需改动；但 Task 15 的矩阵里**异步链只能对
`VARIANT=apps` 断言**，不得写成「两套形态都验」。

**Amend 说明（2026-10-07 #5，Task 4–7 的「运行期步骤」重定界 —— 不改规格、不加任务）**

**问题**：Task 4 / 5 / 6 / 7 都含「起服 → 观察日志 / 查 MySQL / 看 Jaeger」这类**运行期**步骤，
但让进程能起来的两件东西 —— `apps/cmd/**` 与 `apps/conf/**` —— 是 **Task 8** 的产物，
而计划定的执行顺序是 `… 1.4 → 1.5 → 1.6 → 1.7 → 1.8 …`，即 **Task 4–7 一律早于 Task 8**。
实测确认：现在 `apps/cmd/**` 与 `apps/conf/**` 下**只有 `.gitkeep`**。故这些步骤在此位置
**结构上不可能执行**（Task 3 的端到端亦是同一个原因，已在 Task 3 的注记里登记）。

**定界（对 Task 4 / 5 / 6 / 7 一体适用，不再逐 Task 扯）**：

| 步骤类型 | 何时做 | 怎么算过 |
|---|---|---|
| **单元级**（用假件驱动整条链） | 本 Task 当场做 | 单测跑通 + 断言到位（落库行、span 父子链、降级分支）。这是本 Task 的**必做**部分 |
| **运行期**（真起进程、真查库、真看 Jaeger） | **Task 15**（矩阵）与 **Task 8**（能起服之后） | 见 Task 15 的判据；Task 4–7 的对应 Step 只保留单元级部分，并在 `Expected` 里写明「运行期归 Task 15」 |

**因此**：Task 4 Step 3 / Task 5 Step 2 / Task 6 Step 2 的**运行期**内容一律改为「本 Task 只做
单元级等价物；运行期归 Task 15」。**不得**为了让 Step 打勾而伪造运行期证据（例如贴一段
没有真实执行的 SQL 输出）—— 那正是本变更一路在消灭的「看起来验过了」。

**Amend 说明（2026-10-07 #6，`route/` 的限流必须是真的 —— 改规格不新增任务、不加 Task）**

**问题**：Task 4/5/6 的差异表原写「因本域 RPC 只读，`Admit` 对只读方法直接 `return true`」。
Task 4 照此落地后出现两个连锁后果：

1. `internal/srv/points/route/route.go` 的 `Admit` 恒 `true` ⇒ **`route/` 退化成空目录**，
   而 spec `mesh-app-layering` 第 27 行明确要求本目录承担「**身份（Auth）+ 限流（`middleware`）**」；
2. `rpc/points.go` 里 `if !s.guard.Admit(...) { return codes.RateLimited }` 成为**不可达分支**
   ⇒ `RateLimited` 在本域**没有任何真实生产者**，与 Task 18 的裁决（「两个码各自的生产者必须
   是真的，不许『定义到位但无人使用』」）直接冲突。顺带后果：`route` 包 100% 覆盖率是**空函数的
   覆盖**，`rpc` 包因此掉到 94.3%。

**裁定**：**四域的 `route/` 一视同仁 —— 身份闸门 + 按主体限流**，`Admit` MUST NOT 恒真。
上限取各域 Task 2 已定的 `middleware` 常量（order 120 / points 60 / product 300 / stock 180）。
统一比逐域特判更好：`route/` 在四域同构（只是上限不同），Task 10 的同构核对才有意义；
且「只读所以不限流」并不是任何 spec 的要求 —— 它是计划初稿自己的减法，而这处减法把
应用侧**唯一**的 RPC 跳治理手段（`middleware` 限流；Envoy 对 RPC 跳只见连接）给掏空了。

**同步范围**：
- **规格**：`specs/mesh-app-handles/spec.md` 的句柄矩阵重构 —— `DB()` / `KV()[<name>]` /
  `RPCServerOptions()+SetRPCServer()` / `Config()+Logger()` 提为「全部 `srv-*`」共享行，
  **`KV()[<name>]` 的用途写明「RPC 跳限流计数」且四域一视同仁**（原表只给 `srv-order` 与
  `srv-stock` 列了 KV，points / product 限流后必然也要取 KV，否则严格读表会误判「多取句柄」）；
  `Engine()` / `Tracer()` 补进「全部 `srv-*` **不用**」。改动记入 `proposal.md` 的 `## Amendments`。
- **计划**：Task 4/5/6 差异表的 `route/` 行（三处）+ Task 4 Step 1 的 `OnDuplicateKeyUpdate`
  grep（该标识符在 gorm 里不存在，实测 gorm 的 upsert 原语是 `clause.OnConflict{Columns, DoUpdates}`，
  由 **MySQL driver** 渲染成 `ON DUPLICATE KEY UPDATE balance = balance + ?`）+ `model` 行的
  表名判据改成语义 grep（裸 `grep -c "TableName"` 会连注释里的字样一起数，逼人改注释凑数）。
- **tasks.md 无文字变更**（1.4/1.5/1.6 的验收行不含限流细节；3.2 自动吸收新矩阵）。

**Amend 说明（2026-10-07 #7，Task 5/6 的 Step 文本对齐 —— 不改规格、不加任务）**

amend #5 已定界「Task 4–7 的运行期内容改单元级」，但当时只把 Task 4 Step 3 就地改写，
Task 5 Step 2 / Task 6 Step 2 的正文仍是运行期写法。开始 Task 5 前按同一口径逐条对齐
（本次只动 Task 5 的两处，Task 6 在做 Task 6 前同样处理）：

| # | 原写法 | 问题 | 改为 |
|---|---|---|---|
| Q-1 | Task 5 Step 1 判据 `grep -c "TableName" model/product.go == 1` | 与 Task 4 同款**grep-as-oracle**：注释里出现字样就被计数，逼实现者改注释凑数 | `grep -cE "func \(Product\) TableName" model/product.go`（语义 grep） |
| Q-2 | Task 5 Step 2「起 `srv-product` → 看 Jaeger 取两次请求的 trace」 | 结构上做不到（见 amend #5）；照抄只能靠伪造证据打勾 | 改为**单元级**四条断言（第二次不回源 / 回填发生 / span 同 trace 父子链 / 写后失效），并要求 `go test -overlay` 反向自检；运行期归 Task 15 |
| Q-3 | Task 5 `model/product.go` 列表写成 `id/name/price/stock_qty/updated_at` | 三处不一致：① IDL 的 `ProductInfo` 字段是 `Title`/`Status`/`CategoryId`/`CreatedAt`，用 `name` 会让 handler 里出现无来由的改名；② `stock_qty` 把**库存**塞进商品域的表，而库存是 `srv-stock` 的域（Task 6 的 `DB()` + `KV()` 原子增减）—— 这与本变更「域边界即句柄边界」的主张自相矛盾；③ 计划别处的 DDL 讨论（Task 8 Step 期望里的 `40004`）也按「商品不存在」而非「库存不足」措辞 | `id/title/price/status/category_id/created_at/updated_at`，与 IDL 字段集一一对应 |
| Q-4 | Task 5 `repository/product.go` 方法集写 `FindByID` / `List` / `UpdatePrice` | `mesh-entry-chain` 把 `ListProducts` 判为未接线（handler 直接回 `Unimplemented`）⇒ `List` 没有任何调用方：既是死码，又会把 repository 的覆盖率拖到 90% 线以下，逼实现者写「为了覆盖而覆盖」的测试 | 删掉 `List`，方法集收敛为 `FindByID` / `UpdatePrice`（只有真被调用的才留） |
| Q-5 | Task 5 `rpc/product.go` 判据 `grep -n "var _ product.Product"` | 把**导入别名**写死进判据：实现者用 `productpb` 之类的别名就是等价实现，却会误判 FAIL（判据该盯「有没有编译期断言」，不是「别名叫什么」） | `grep -cE 'var _ .*\.Product = \(\*Server\)\(nil\)' rpc/product.go == 1` |

**登记项（不属本 Task，留给相应 Task）**：`examples/scripts/order_db.sh` 的 `TABLES`
只建 `orders`/`order_items`/`order_status_log`/`refund_records`/`outbox`/`users`，
**没有 `products` / `stock`**；而 Task 8 的期望（`curl /product/1` 返回 2xx）与 Task 15 的
路由矩阵都要求这两张表存在。故 Task 8 必须补这两张表的 DDL（同一真源文件、同一个库
`order_db`）或把种子脚本拆一份 apps 专用，否则 `/product/1` 会以「表不存在」失败而
**不是**计划里写的 `40004`（商品不存在）。

**Amend 说明（2026-10-07 #8，Task 6 的 Step 文本对齐 —— 不改规格、不加任务）**

承接 amend #7 的同款处理（「Task 5 先做先改，Task 6 在做 Task 6 前同样处理」）。开始 Task 6
之前按「实现前先核实事实」逐条复查，五处**计划文本与实测不符**，就地修正：

| # | 原写法 | 问题 | 改为 |
|---|---|---|---|
| R-1 | Task 6 `model/stock.go` 列表写 `id/product_id/available/locked/updated_at` | 与 IDL 的 `StockInfo` 三处不一致：① IDL 字段是 `Total`/`Available`/`Reserved`/`Version`，`locked` 与 `reserved` **同名不同物**；② 漏了 `total`（而 `ConfirmStockResp`/`AdjustStockResp` 都回 `Total`）；③ 漏了 `version`（IDL 对外暴露的乐观锁版本），而条件 UPDATE 恰好是最该递增它的地方 | `id/product_id/total/available/reserved/version/updated_at`，与 `StockInfo` 字段集一一对应 |
| R-2 | Task 6 `repository/stock.go` 的 UPDATE 只动 `available` | 同 R-1③：`version` 已由 IDL 暴露，扣减成功后 `version` 不前进会让「同一商品两次扣减不可区分」，也让并发不超卖少了一条独立判据 | `SET available = available - ?, version = version + 1 WHERE product_id = ? AND available >= ?` |
| R-3 | Task 6 `service/stock.go` 只列了 `Decrease` | `GetStock` 是**已接线**路径（`GET /stock/:product_id` → `stock.GetStock`），它必须在 `service` 有承接方法 —— 照原写法「一行实现都不写」，已接线路径的 service 层是空的 | 补 `GetStock` 读路径；并写明**扣减的真相在库层条件 UPDATE**，KV 只做当日量旁路计数（失败不影响扣减结果）—— 与 `mesh-app-handles` 的 `KV()[<name>]` 用途（「另用于库存计数」+ 限流计数）一致 |
| R-4 | Task 6 Step 1 判据 `grep -n "strict" ../../conf/srv-stock.yaml`（期望只命中 `db: true`）；`grep -c "TableName"`；`grep -n "var _ stock.Stock"` | 三条同款问题：`apps/conf/**` 是 **Task 8 产物**，此刻不存在，判据结构上跑不了；`TableName` 是 grep-as-oracle（同 Q-1）；`var _ stock.Stock` 把导入别名写死（同 Q-5） | 配置侧要求保留但标注「归 Task 8 落、Task 15 验」；表名改语义 grep；断言改 `var _ .*\.Stock = \(\*Server\)\(nil\)` |
| R-5 | Task 6 Step 2「注释掉 `lockx` 段 → 起服 → 看日志」/ Step 3「起进程 20 并发」 | 与 amend #5 的定界冲突（Task 4–7 一律早于 Task 8，`apps/cmd/**` 与 `apps/conf/**` 尚不存在），照抄只能靠伪造证据打勾 | Step 2 改**单元级**（nil 锁 → `Decrease` 正常返回 + 内存 logger 断言 `lock unavailable` Warn + 真锁对照组不出现该 Warn）；Step 3 改**单元级两层判据**并**如实标注强度**（repository 条件语义＝强判据，单线程即可验；service 并发计数＝端到端判据，但受 sqlite 写连接串行化影响，不得说成「条件 UPDATE 是唯一护栏」） |

**登记项（顺带，属 Task 8）**：`stocks` 表与 `products` 表同属 `order_db.sh` 的缺项（见 amend #7 末条），
Task 8 补 DDL 时**一并补 `stocks`**（列集按 R-1 的七列），否则 Task 15 的
`GET /stock/1` 会以「表不存在」失败。
