package handler

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// presaleSource 读 presale.go 源码。
//
// 为什么这三条判据都用源码而不是运行期断言：bff 是**唯一**知道 orderNo 与
// dtm_gid 从哪来、响应体该给什么的地方。TCC 又是异步的 —— 提交返回时三个分支
// 一个都没跑完，「有没有返回业务 id」在运行期除了读响应体没有别的观测点，而
// 响应体形状恰恰是要被钉住的东西本身。
func presaleSource(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("presale.go")
	if err != nil {
		t.Fatalf("读 presale.go: %v", err)
	}
	return string(src)
}

// lockHandlerBody 取 Lock 方法体（AST 口径：注释不在里面）。
func lockHandlerBody(t *testing.T) *ast.FuncDecl {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "presale.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 presale.go 失败：%v（守卫失效）", err)
	}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if ok && fd.Name.Name == "Lock" && fd.Recv != nil {
			return fd
		}
	}
	t.Fatal("presale.go 里找不到 (*PresaleHandler).Lock")
	return nil
}

// TestPresaleLockResponseCarriesNoBusinessIDs 钉住响应体只给 gid / 单号 / 分支。
//
// **不给 order_id / reservation_id** 是硬要求而不是偏好：TCC 的二阶段由 dtm
// 异步驱动，`Lock` 返回那一刻三个分支一个都没跑完，此刻报一个 id 就是编的 —
// 调用方拿它去查订单域只会得到「查不到」，而它在响应里出现过这件事无法事后
// 撤销（已发出去的响应）。Saga 侧 PlaceOrder 同一条口径。
//
// 判据用「响应字面量的顶层键集合 == 期望集合」而不是「不含某些键」：后者在有人
// 加一个新键时照样绿，而多出来的键同样可能是编出来的业务 id。
//
// 反向探针：在 c.JSON 的 map 里加一行 `"order_id": 123`，本用例立刻红。
func TestPresaleLockResponseCarriesNoBusinessIDs(t *testing.T) {
	body := lockHandlerBody(t)
	literal := responseMapLiteral(t, body)
	want := []string{"branches", "code", "dtm_gid", "order_no", "total_amount"}
	got := sortedMapKeys(literal)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("响应体顶层键 = %v，want %v（TCC 是异步的，除 gid/单号/分支清单外一律不能给业务 id）", got, want)
	}
	for _, banned := range []string{"order_id", "reservation_id", "orderId", "reservationId"} {
		for _, k := range got {
			if k == banned {
				t.Errorf("响应体里有 %q —— 提交返回时三个分支一个都没跑完，报 id 就是编的", k)
			}
		}
	}
}

// TestPresaleLockReturnsBranchesFromOrchestrator 分支清单必须来自编排器。
//
// 它是提交响应里**唯一**能确认「编排了什么」的地方：dtm 只按 URL 回调，配错了
// 在提交时毫无异样，要等二阶段才炸。让它硬编码在 handler 里就等于放一份会与
// 编排表漂移的副本 —— 漂了以后日志回显与实际回调对不上，排查方向直接被带偏。
func TestPresaleLockReturnsBranchesFromOrchestrator(t *testing.T) {
	text := presaleSource(t)
	idx := strings.Index(text, "c.JSON(http.StatusOK, map[string]any{")
	if idx < 0 {
		t.Fatal("Lock 里找不到成功响应（提交成功却没有返回体？）")
	}
	end := idx + 1200
	if end > len(text) {
		end = len(text)
	}
	seg := text[idx:end]
	if !strings.Contains(seg, `"branches":     presaleBranchView(res.Steps)`) &&
		!strings.Contains(seg, `"branches": presaleBranchView(res.Steps)`) {
		t.Error("branches 不是从编排器返回的 Steps 渲染的 —— 它必须是编排表的回显，而不是 handler 自己的一份副本")
	}
}

// TestPresaleLockStopsAtTCC handler 层不直连写型下游。
//
// 与 TestPlaceOrderStopsAtSaga 同一条纪律：TCC 接管之后，预售锁定的**写**只经
// 一条出口 —— dtm 的分支回调。handler 里再出现 ReserveStock / CreateOrder /
// Earn，意味着两条写路径并存，而它们的幂等键各自独立。
//
// 用 AST 而不是文本 search：这些字面量大量出现在**注释**里（说明为什么不这么
// 做），文本 search 必然误报。
func TestPresaleLockStopsAtTCC(t *testing.T) {
	banned := map[string]string{
		"ReserveStock": "手动锁库存：TCC 的冻结只由 dtm 的 stock.TryFreeze 分支发起",
		"ReleaseStock": "手动解冻：TCC 的 Cancel 只由 dtm 发起",
		"CreateOrder":  "手动建单：TCC 的占位单只由 dtm 的 order.TryPlace 分支发起",
		"Earn":         "手动积分入账：TCC 的冻结只由 dtm 的 points.TryFreeze 分支发起",
		"Consume":      "手动扣积分：同上",
	}
	ast.Inspect(lockHandlerBody(t), func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel == nil {
			return true
		}
		recv, ok := sel.X.(*ast.SelectorExpr)
		if !ok || recv.Sel == nil || recv.Sel.Name != "d" {
			return true
		}
		if why, ok := banned[sel.Sel.Name]; ok {
			t.Errorf("Lock 里仍有 h.d.%s —— %s", sel.Sel.Name, why)
		}
		return true
	})
}

// TestPresaleLockPrechecksBeforeSubmittingTCC 钉住「预检不进事务」的次序。
//
// 与 PlaceOrder 同一条判据：预检走现有 kitex RPC（product.GetProduct），必须在
// `h.tcc.Lock(...)` **之前**且必须存在。
//   - 提前到提交之后 = 商品查不到也已经建了占位单再 Cancel；
//   - 直接删掉 = 「商品不存在」变成一条必然失败、必然被 Cancel 的全局事务。
//
// 反向探针：把两段调换位置，或删掉 GetProduct 那段，本用例立刻红。
func TestPresaleLockPrechecksBeforeSubmittingTCC(t *testing.T) {
	text := presaleSource(t)
	pre := strings.Index(text, "h.d.GetProduct(ctx, &productpb.GetProductReq{UserId: req.UserID, ProductId: req.ProductID})")
	submit := strings.Index(text, "h.tcc.Lock(ctx, dtmorch.LockCmd{")
	if pre < 0 {
		t.Fatal("Lock 里找不到预检调用 h.d.GetProduct —— 预检不能删，否则商品不可得也会建立全局事务")
	}
	if submit < 0 {
		t.Fatal("Lock 里找不到 h.tcc.Lock —— 预售锁定没有提交 TCC")
	}
	if pre > submit {
		t.Errorf("预检(%d) 在提交(%d) 之后 —— 商品查不到也已经开了全局事务", pre, submit)
	}
}

// TestPresaleLockPassesIdempotencyKeyToTCC 钉住「bff 把幂等键交给了 dtm 分支」。
//
// dtm TCC 的三段共用同一份载荷（dtm 侧 svcRegisterBranch 从同一个 TransBranch
// 克隆出 confirm 与 cancel），order_no 就在那份 biz JSON 里；传不传，运行期没有
// 任何迹象 —— 订单域照建占位单，只是重放一次就是另一张单。
//
// 逐行比对而不是 strings.Contains(片段, "OrderNo:")：后者会命中**注释里**的同名
// 串（把字段整行注释掉后判据照样绿，等于空壳）。
func TestPresaleLockPassesIdempotencyKeyToTCC(t *testing.T) {
	text := presaleSource(t)
	idx := strings.Index(text, "LockCmd{")
	if idx < 0 {
		t.Fatal("presale.go 里找不到 LockCmd 的构造点（提交 TCC 的那段被改了？）")
	}
	end := idx + 1200
	if end > len(text) {
		end = len(text)
	}
	found := false
	for _, line := range strings.Split(text[idx:end], "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "OrderNo:") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("LockCmd 没带 OrderNo —— 幂等键没经 dtm 载荷交给三域，重试（上游重发 / dtm 重放）会重复冻结")
	}
}

// responseMapLiteral 取 Lock 里 c.JSON 的 map 字面量。
func responseMapLiteral(t *testing.T, body *ast.FuncDecl) *ast.CompositeLit {
	t.Helper()
	var found *ast.CompositeLit
	ast.Inspect(body, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		// 只认 "code" 这个最稳定的顶层键作为锚点：它在这条链上恒定存在，
		// 而分支清单里的键名会随排版改动。
		for _, elt := range cl.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if lit, ok := kv.Key.(*ast.BasicLit); ok && lit.Value == `"code"` {
				found = cl
				return false
			}
		}
		return true
	})
	if found == nil {
		t.Fatal("Lock 里找不到带 \"code\" 键的响应 map 字面量（成功响应被改了？）")
	}
	return found
}

// sortedMapLiteralKeys 取 map 字面量的键（按字面量值，排序后返回）。
func sortedMapKeys(cl *ast.CompositeLit) []string {
	out := make([]string, 0, len(cl.Elts))
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		lit, ok := kv.Key.(*ast.BasicLit)
		if !ok {
			continue
		}
		out = append(out, strings.Trim(lit.Value, `"`))
	}
	sort.Strings(out)
	return out
}
