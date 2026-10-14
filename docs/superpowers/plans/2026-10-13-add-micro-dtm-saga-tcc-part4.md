# add-micro-dtm-saga-tcc 实现计划（微服 DTM saga/tcc）— part4

**Traceability (sddflow):**
- plan-ready: `openspec/changes/add-micro-dtm-saga-tcc/plan-ready.md`
- tasks: `openspec/changes/add-micro-dtm-saga-tcc/tasks.md`
- plan: `docs/superpowers/plans/2026-10-13-add-micro-dtm-saga-tcc.md`（Task 1-4）+ `-part4.md`（本文件）

> 主计划头（目标/架构/技术栈/执行环境事实/文件结构）见 `docs/superpowers/plans/2026-10-13-add-micro-dtm-saga-tcc.md`。本文件是同一次变更的 Task 连续切片，Task N 在四份文件中连续不跳号。

### Task 10: bff Saga 编排接入（替换手动编排）

> **trace:** plan-ready.md → `### Task 10: bff Saga 编排接入（对应 tasks.md 3.1）` | tasks.md → `- [ ] 3.1 Saga 编排接入：bff 新增 \`internal/dtm\`（saga 编排器：分支序 \`stock.Deduct → order.CreateOrder → product.销+1 → points.Debit\`、补偿逆序、\`DtmAddr\` 读 config、traceparent 编入载荷、\`fail_inject\` 透传）；\`POST /api/order\` handler 改挂 saga 提交（预检 \`product.GetProduct\` 失败 4xx 不提交）；现有手动 \`ReleaseStock\` 补偿代码退役删除。验收：编排器单测断言分支序与载荷 traceparent；全仓 grep 手动补偿调用点为零`
> **sync:** tasks.md → `- [ ] 3.1 Saga 编排接入：bff 新增 \`internal/dtm\`（saga 编排器：分支序 \`stock.Deduct → order.CreateOrder → product.销+1 → points.Debit\`、补偿逆序、\`DtmAddr\` 读 config、traceparent 编入载荷、\`fail_inject\` 透传）；\`POST /api/order\` handler 改挂 saga 提交（预检 \`product.GetProduct\` 失败 4xx 不提交）；现有手动 \`ReleaseStock\` 补偿代码退役删除。验收：编排器单测断言分支序与载荷 traceparent；全仓 grep 手动补偿调用点为零` | plan-ready.md → `### Task 10: bff Saga 编排接入（对应 tasks.md 3.1）`

**Files:**
- 创建：`examples/micro/bff/internal/dtm/{saga.go,payload.go}`
- 修改：`examples/micro/bff/internal/handler/<下单 handler 文件>`（按现文件现状改挂）、`internal/route`（注册行落点保持）
- 删除：现有手动 `ReleaseStock` 补偿调用点（grep 定位，见 Step 10.4）
- 测试：`examples/micro/bff/internal/dtm/saga_test.go`

API 事实（Task 1 Step 1.3 grep 结果裁决；下文按 dtm-labs/client 常见形态写实，签名对不齐时以 grep 结果为准修正调用行，不改结构）：

- `gid := dtmgrpc.MustGenGid(dtmAddr)`（官方生成全局事务 ID）
- `saga := dtmgrpc.NewSaga(dtmAddr, gid)`；`saga.Add(actionURL, compURL, payload)`；`saga.WaitResult(timeout)` 或 `saga.Submit()`
- 分支 URL 形态（grpc 协议）：`"127.0.0.1:9043/dtmbusi.StockBusi/Deduct"` —— **地址来自 config**（bff `service.upstream.<域>` 已有这两个域服端口？904x 是新端口，所以 bff config 里**新增**上游「branch 面地址段」`dtm.branches.<域>`，不写代码硬编码）：

```yaml
# bff/config.yaml 追加（与 Task 4 Step 4.8 的 xmiddleware.dtm 同一处落）
dtm:
  branches:
    stock:   "127.0.0.1:9043"
    order:   "127.0.0.1:9042"
    product: "127.0.0.1:9041"
    points:  "127.0.0.1:9044"
```

- [ ] **Step 10.1：saga 编排器全量代码 `bff/internal/dtm/saga.go`**

```go
// Package dtm bff 的事务编排面（saga / tcc 两套，本文件 saga）。
//
// 纪律：
//   - DtmAddr / 各域分支面地址一律来自 config（xmiddleware.dtm.grpc_addr 与
//     dtm.branches.<域>）；本包不得写死回环地址；
//   - 分支载荷公共信封是 pkg/dtmx.Payload（traceparent + fail_inject + biz）；
//   - 分支序与补偿由 dtm server 逆推（Saga.Add(action, comp) 双参语义），
//     bff 只负责构造顺序 —— 决策顺序即代码顺序，不复查不出错。
package dtm

import (
	"context"
	"encoding/json"
	"time"

	"github.com/dtm-labs/client/dtmgrpc"

	"github.com/gospacex/otelx/propagation"

	"github.com/gospacex/launchx/examples/micro/pkg/dtmx"
)

// Sac aOrderCmd bff 的 Saga 下单命令（handler 层转出的领域形态）
type Sac aOrderCmd struct {  // 落地 rename：Sac aOrderCmd → SagaOrderCmd（编译报错即照改）
	ProductID int64  `json:"product_id"`
	Quantity  int    `json:"quantity"`
	OrderNo   string `json:"order_no"`
	UserID    int64  `json:"user_id"`
	FailInject string `json:"fail_inject,omitempty"` // demo 注入口（验收脚本传）
}

// OrderSaga 编排器
type OrderSaga struct {
	dtm string
	branchesBranch func(domain string) string // config 转出的分支面地址
}

func NewOrderSaga(dtmAddr string, branches func(domain string) string) *OrderSaga {
	return &OrderSaga{dtm: dtmAddr, branchesBranch: branches}
}

// SubmitSagaPricesAgs 构造四分支 Saga 并提交（同步等结果）
func (o *OrderSaca) Submit(ctx context.Context, c SagaOrderCmd) (gid string, err error) {  // rename mail：OrderSaca→OrderSaga
	gid = dtmgrpc.MustGenGid(o.dtm)
	s := dtmgrpc.NewSaga(o.dtm, gid)
	tp := propagation.TraceparentIf(ctx, true)

	payload := func(domain, op string, biz any) (string, error) {
		raw, _ := json.Marshal(biz)
		return dtmx.Encode(dtmx.Payload{Biz: raw, Traceparent: tp, FailInject: c.FailInject})
	}

	// 分支序：stock.Deduct → order.Create → product.BumpSales → points.Debit（失败 dtm 逆序自动补偿）
	inner := [][2]string{ // [action, compensation]，域按序排
		{"stock", "Deduct"}, {"stock", "Release"},
		{"order", "Create"}, {"order", "CancelOrder"},
		{"product", "BumpSales"}, {"product", "UnbumpSales"},
		{"points", "Debit"}, {"points", "Refund"},
	}
	for i := 0; i < len(inner); i += 2 {
		da, ca := inner[i], inner[i+1]
		actionURL := o.branches(da[0]) + "/dtmbusi." + svcName(da[0]) + "/" + da[1]
		compURL := o.branches(ca[0]) + "/dtmbusi." + svcName(ca[0]) + "/" + ca[1]
		pl, pErr := payload(da[0], da[1], map[string]any{
			"product_id": c.ProductID, "quantity": c.Quantity, "order_no": c.OrderNo,
			"user_id": c.UserID, "total_amount": c.TotalAmount, "items": c.Items,
		})
		if pErr != nil {
			return "", pErr
		}
		s.Add(actionURL, compURL, pl)
	}
	s.WaitResult(time.Duration(60) * time.Second)  // 或 saga.Submit()：以 Task 1 grep 签名为准
	return gid, err
}

func svcName(d string) string {
	switch d {
	case "stock": return "StockBusi"
	case "order": return "OrderBusi"
	case "product": return "ProductBusi"
	case "points": return "PointsBusi"
	}
	return d
}
```

> 落地三处修订（执行者进文件第一事）：① `SagaOrderCmd` 增加 `TotalAmount int64, Items []OrderSciItem`（订单分支要落 items 列表；结构由 handler 从预检的 product 与请求合成）；② `s.WaitResult/Submit` 按签名；③ `inner` 二元表的 biz payload 随分支变（stock/points 分支要 user_id/product_id/quantity/order_no；order 分支要多 items+total；product 分支要 order_no/product_id/qty=quantity）——**Beta：落地下面的「按 op 分派 payload」终版**（把上面的统一 map 换成 op 分派）：

```go
bizFor := func(domain, op string) any {
	switch domain {
	case "stock":
		return map[string]any{"product_id": c.ProductID, "quantity": c.Quantity, "order_no": c.OrderNo}
	case "points":
		return map[string]any{"user_id": c.UserID, "cost": c.PointsCost, "order_no": c.OrderNo}
	case "product":
		return map[string]any{"order_no": c.OrderNo, "product_id": c.ProductID, "qty": c.Quantity}
	case "order":
		return map[string]any{"user_id": c.UserID, "order_no": c.OrderNo,
			"total_amount": c.TotalAmount, "items": c.ItemDTOs(), "outbox_payload": c.OutboxPayload, "traceparent": ""}
	}
	return nil
}
```

（`PointsCost/OutboxPayload` 由 handler 组命令时带进来 —— Saga 命令结构里全字段：{ProductID, Quantity, PointsCost, TotalAmount, OrderNo, UserID, FailInject, Items, OutboxPayload}；ItemDTOs() = domain map。）

- [ ] **Step 10.2：handler 改挂（现下单 handler 的业务段替换为调编排器）**

以现文件为准的锚点（落地时打开 `bff/internal/handler/**.go`，找到 `POST /api/order` 的 handler 函数与「下单失败补偿」段）。替换的业务动词（全代码，参数形态按现文件变量名同位替换）：

```go
sagaOrch, err := dtm.NewOrderSaga(dtmAddr, branchAddrFrom(config))
if err != nil {
	// 编排器构造失败属装配期错误——Lift in setup，不该在 handler 里第二回构造
}
gid, err := sagaOrch.Submit(c, cmd)  // cmd 是 handler 解析出的 SagaOrderCmd
if err != nil {
	// dtm 不可达 / 提交失败 → 5xx + Warn（consequence：事务未建；hint：检查 xmiddleware.dtm）
	// 直接回到调用方；绝不 try 去手动补偿 —— 手动链已退役
}
// 成功响应带 gid（验收脚本用）
```

装配期绑定（`app/setup.go`，在 svc 组层构造后一次插入）：

```go
dtmAddr := a.Config().GetString("xmiddleware.dtm.grpc_addr")
branches := map[string]string{
	"stock": a.Config().GetString("dtm.branches.stock"),
	"order": a.Config().GetString("dtm.branches.order"),
	"product": a.Config().GetString("dtm.branches.product"),
	"points": a.Config().GetString("dtm.branches.points"),
}
sagaOrch := dtm.NewOrderSaga(dtmAddr, func(domain string) string { return branches[domain] })
handler 传入（NewHandler(svc, sagaOrch)——按现 handler 构造函数签名扩展参数）
```

- [ ] **Step 10.3：**撤手动补偿**（**删除证据**）**

```bash
grep -rn "ReleaseStock\|biz.compensation=true\|compensation" examples/micro/bff/ --include='*.go'
```
预期：**零命中**（bff 里不再手动调 stock release）。若有命中 → 删除该调用段与相关测试（上线前由 Task 15 文档把 E3 失败演示口径迁到 fail_inject）。

- [ ] **Step 10.4：编排器单测（不起 dtm：只截在「Add 语义序列正确 + 载荷带 traceparent」上，用 fake dtmgrpc 转接）**

> dtmgrpc 的构造体与 `Add` 难以单层 mock —— 单测改测**纯函数形态**：把「分支序表 + 地址 + 载荷」的构造函数抽出来（`buildEntries(cmd) [][2]string { url, payload }`），单测断言四分支的 action/comp 序对、payload JSON 各带 traceparent 与 fail_inject。dtm 真实调用语义归 Task 13 集成测试。**全代码（bff/internal/dtm/saga_test.go）**：

```go
package dtm_test

import (
	"testing"

	"github.com/gospacex/launchx/examples/micro/pkg/dtmx"
	"github.com/gospacex/launchx/examples/micro/bff/internal/dtm"
)

func TestBuildOrderSagaEntries(t *testing.T) {
	cmd := dtm.SagaOrderCmd{ProductID: 1, Quantity: 2, OrderNo: "o-1", UserID: 1001, FailInject: "order_Create"}
	entries := dtm.BuildOrderSagaEntries(cmd, "127.0.0.1:9041")  // branches 闭包由测试传桩
	if len(entries) != 4 {
		t.Fatalf("want 4 branch pairs, got %d", len(entries))
	}
	if entries[0][0] != "127.0.0.1:9043/dtmbusi.StockBusi/Deduct" || entries[0][1] != "127.0.0.1:9043/dtmbusi.StockBusi/Release" {
		t.Fatalf("first pair wrong: %v", entries[0])
	}
	if entries[3][0] != "127.0.0.1:9044/dtmbusi.PointsBusi/Debit" || entries[3][1] != "127.0.0.1:9044/dtmbusi.PointsBusi/Refund" {
		t.Fatalf("last pair wrong: %v", entries[3])
	}
	for i, e := range entries {
		p, err := dtmx.Decode(e[2])
		if err != nil {
			t.Fatalf("entry %d payload decode: %v", i, err)
		}
		if p.Traceparent == "" {
			t.Fatalf("entry %d missing traceparent", i)
		}
		if p.FailInject != "order_Create" {
			t.Fatalf("entry %d fail_inject lost", i)
		}
	}
}
```

- [ ] **Step 10.5：build + ci + Commit**

```bash
bash examples/scripts/ci.sh micro
git add examples/micro
git commit -m "feat(micro-bff): saga orchestration replaces manual order chain"
```

- [ ] **Task complete**

---

### Task 11: bff TCC 编排接入（预售锁定端点）

> **trace:** plan-ready.md → `### Task 11: bff TCC 编排接入（对应 tasks.md 3.2）` | tasks.md → `- [ ] 3.2 TCC 编排接入：bff 同包新增 tcc 编排器（Try 三分支：stock/points/order；Confirm/Cancel 二阶段；配置与 trace 口径与 saga 同构）与新增 \`POST /api/presale/lock\` 路由。验收：单测断言 Try 失败 → Cancel 逆序构造、Try 全绿 → Confirm 构造；路由注册可见`
> **sync:** tasks.md → `- [ ] 3.2 TCC 编排接入：bff 同包新增 tcc 编排器（Try 三分支：stock/points/order；Confirm/Cancel 二阶段；配置与 trace 口径与 saga 同构）与新增 \`POST /api/presale/lock\` 路由。验收：单测断言 Try 失败 → Cancel 逆序构造、Try 全绿 → Confirm 构造；路由注册可见` | plan-ready.md → `### Task 11: bff TCC 编排接入（对应 tasks.md 3.2）`

**Files:** 创建 `bff/internal/dtm/tcc.go` + `tcc_test.go`；修改 `bff/internal/handler`（新 handler）+ `internal/route`/路由注册点（新路由）

API 形态：dtm TCC 的 bff 侧是**两段函数**：先 Try 全部、finalize 分支确认/取消由 dtm 驱动（Confirm/Cancel 均 dtm 回调域服，bff 只需在 Try 段提交后按结果等待）。官方（grpc 形态）：

- `tcc := dtmgrpc.NewTcc(dtmAddr, gid)`；Try 编排在 `tcc.Call(func(t *dtmgrpc.Tcc){ t.CallBranch(url, payload) ... })`；Try 全绿 → 由 dtm「用户二次触发」——demo 里 bff 在 Try 成功后走 `t.Submit()` 路由到「立即 Confirm」语义（或验收脚本直接打 Confirm 分支入口——**demo 落法**：Try 成功后 bff 立即再提交一次 Confirm 分支集；Cancel 路径由验收脚本 fail_inject 驱动）。

- [ ] **Step 11.1：tcc 编排器全量代码 `bff/internal/dtm/tcc.go`**

```go
package dtm

import (
	"context"
	"encoding/json"
	"time"

	"github.com/dtm-labs/client/dtmgrpc"

	"github.com/gospacex/otelx/propagation"

	"github.com/gospacex/launchx/examples/micro/pkg/dtmx"
)

// PresaleTccCmd 预售锁定命令
type PresaleTccCmd struct {
	ProductID  int64  `json:"product_id"`
	Quantity   int    `json:"quantity"`
	PointsCost int64  `json:"points_cost"`
	UserID     int64  `json:"user_id"`
	OrderNo    string `json:"order_no"`
	FailInject string `json:"fail_inject,omitempty"`
}

// PresaleTcc TCC 编排器（Try 三分支 + 二阶段）
type PresaleTcc struct {
	dtm      string
	branches func(domain string) string
}

func NewPresaleTcc(dtmAddr string, branches func(domain string) string) *PresaleTcc {
	return &PresaleTcc{dtm: dtmAddr, branches: branches}
}

// SubmitTry 提交 Try 三分支（stock/points/order 任一业务失败 → dtm 逆序自动 Cancel）。
// 全绿后返回 gid，由调用方择机提交 Confirm（демо 端点）或留给 Cancel 路径。
func (p *PresaleTcc) SubmitTry(ctx context.Context, c PresaleTccCmd) (gid string, err error) {
	gid = dtmgrpc.MustGenGid(p.dtm)
	t := dtmgrpc.NewTcc(p.dtm, gid)
	tp := propagation.TraceparentIf(ctx, true)

	url := func(domain, op string) string {
		return p.branches(domain) + "/dtmbusi." + svcName(domain) + "/" + op
	}
	trySo, _ := payloadEncode(tp, c.FailInject, map[string]any{"product_id": c.ProductID, "quantity": c.Quantity, "order_no": c.OrderNo})
	tryPo, _ := payloadEncode(tp, c.FailInject, map[string]any{"user_id": c.UserID, "cost": c.PointsCost, "order_no": c.OrderNo})
	tryOr, _ := payloadEncode(tp, c.FailInject, map[string]any{"user_id": c.UserID, "order_no": c.OrderNo, "total_amount": 0/* handler 合成 */, "items": c.ItemDTOs()})

	t.CallBranch(url("points", "Try"), tryPo)   // Try 序：业界惯例「先点便宜资源」。任一业务失败 → dtm 逆序 Cancel
	t.CallBranch(url("stock", "Try"), trySo)
	t.CallBranch(url("order", "TryPlace"), tryOr)
	t.WaitResult(time.Second * 45)              // 或 Submit()；以 Task 1 grep 为准
	return gid, err
}

// SubmitConfirm Try 全绿后收口（占位转正/实扣）
func (p *PresaleTcc) SubmitConfirm(ctx context.Context, gid string, c PresaleTccCmd) error {
	// 二阶段终态段：直接对三域分支持发 dtm 二阶段（四域各自分支在实现中把
	// Confirm/Cancel 两个 finalize endpoint 已登记）—— demo 的两条路径
	// （人工 Confirm/超时 Cancel）由 dt_e2e.sh 分段演示触发。
	// 落法：dtmgrpc.DtmGrpcCallBranceApi? 官方「转录二阶段」API 名按 Task 1 grep
	// 结果（常见为 dtmgrpc.DtmGrpc? 的 ConfirmAll 等）；签名不即时可确认时，
	// demo 直落「再开一次 TCC 分支提交只为二阶段」这套不必要 —— 以官方
	// finalize 调用 API 为准，本文件用真实签名落一行。
	return nil // ← 占位指令：执行者以 Task 1 核实的官方二阶段 API 落代码，禁止空实现出仓
}

func payloadEncode(tp, failInject string, biz any) (string, error) {
	raw, _ := json.Marshal(biz)
	return dtmx.Encode(dtmx.Payload{Biz: raw, Traceparent: tp, FailInject: failInject})
}
```

> ⚠️ **`SubmitConfirm` 里那个空 return 是本计划的登记占位 —— build 阶段禁止以此出仓。归还路径**：Task 1 Step 1.3 的 grep 清单里已经列出「官方确认二阶段 API 名」；执行按 grep 结果写一行真实调用（等价「二阶段 finalize 提交」）。若官方 dtm 形态根本没有独立二阶段调用 API（全部由 dtm server 自己推进），则该函数**删除**，SubmitTry 的「Try 全绿 → 二阶段」交由 dtm 推进（demo 脚本改用 wait 叙述）——**这两种形态必居其一，禁止半归半不归**。

- [ ] **Step 11.2：新 handler + 路由（锚点：现 bff handler/route 文件，同 Task 10.2 装配挂钩；路由：`POST /api/presale/lock`）**

handler 内的命令组装（全代码；变量名同现有文件另置）：

```go
// handler 处理预售锁定；预检与 saga 同一套（product 可读 && 用户存在）
gid, err := presaleOrch.SubmitTry(c, cmd)
if err != nil {
	// resource 冻结失败（任一 Try 业务失败）：dtm 已驱动 Cancel，响应 4xx（业务失败码）
	// 提交失败/基础失败：5xx + Warn
}
// Try 全绿：响应 202 带_gid（阶段账：验收脚本继续 Confirm 或等 Cancel）
```

路由注册（锚点照现文件 route 行形态）：

```go
r.POST("/api/presale/lock", authed(h.HandlePresaleLock))
```

- [ ] **Step 11.3：tcc 单测（同 Task 10.4 形态：抽取 `BuildPresaleTryEntries` 纯函数断言三对 Try 序与 payload）**

```bash
go test ./examples/micro/bff/... -count=1 && bash examples/scripts/ci.sh micro
```

- [ ] **Step 11.4：Commit**

```bash
git add examples/micro
git commit -m "feat(micro-bff): presale tcc orchestration + /api/presale/lock"
```

- [ ] **Task complete**

---
