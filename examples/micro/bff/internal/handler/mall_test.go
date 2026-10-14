package handler

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// mallSource 读 mall.go 源码。
//
// 为什么这三条判据都用源码而不是运行期断言：bff 是**唯一**知道 orderNo 这个幂等
// 键的地方，它传没传，在运行期没有任何可观测的副作用 —— 不传也不会报错，订单域
// 只是自己另起一个号。Saga 提交成功也不代表四域都写成功（dtm 异步推进），所以
// 「载荷里有没有带幂等键」只能从编排代码上钉。
func mallSource(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("mall.go")
	if err != nil {
		t.Fatalf("读 mall.go: %v", err)
	}
	return string(src)
}

// TestPlaceOrderPassesIdempotencyKeyToSaga 钉住「bff 把幂等键交给了 dtm 分支」。
//
// dtm Saga 只有一个 payload 字段被四域共用，order_no 就在它的 biz JSON 里；传
// 不传，运行期同样没有任何迹象 —— 订单域照建单，只是重放一次就是一张新单。
//
// 反向探针：把 OrderCmd 字面量里的 `OrderNo: orderNo` 整行注释掉，本用例立刻红。
func TestPlaceOrderPassesIdempotencyKeyToSaga(t *testing.T) {
	text := mallSource(t)
	idx := strings.Index(text, "OrderCmd{")
	if idx < 0 {
		t.Fatal("mall.go 里找不到 OrderCmd 的构造点（提交 Saga 的那段被改了？）")
	}
	end := idx + 1200
	if end > len(text) {
		end = len(text)
	}
	// 逐行比对而不是 strings.Contains(片段, "OrderNo:")：后者会命中**注释里**
	// 的同名串（把字段整行注释掉后判据照样绿，等于空壳）。字段名只有在行首
	// （去掉缩进后）才是真的字段。
	found := false
	for _, line := range strings.Split(text[idx:end], "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "OrderNo:") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("OrderCmd 没带 OrderNo —— 幂等键没经 dtm 载荷交给订单域，" +
			"重试（上游重发 / dtm 重放）会重复下单")
	}
}

// TestPlaceOrderHasNoManualCompensation 钉住手动补偿已退役。
//
// 判据是**整个 handler 层**里不再出现对库存释放的直连调用：Saga 的补偿由 dtm
// 发起（stock.Release 分支），本层再调一次就是重复回补 —— 而「重复回补」的
// 症状是库存越补越多，当场没有任何一行日志会说「你调了两次」。
//
// 用 AST 而不是文本 search：本文件里这些字面量大量出现在**注释**里（说明为什么
// 不这么做），文本 search 必然误报；而 go/parser 的 AST 不含注释，正是本仓
// examples_addr_hygiene_test.go 选 AST 的同一条理由（判据一旦恒红就会被忽略）。
//
// 反向探针：把 `h.d.ReleaseStock(...)` 那一段抄回 PlaceOrder，本用例立刻红。
func TestPlaceOrderHasNoManualCompensation(t *testing.T) {
	bannedCalls := map[string]string{
		"ReleaseStock": "手动库存补偿：Saga 的补偿只由 dtm 的 stock.Release 分支发起",
		"Earn":         "手动积分入账：Saga 的扣积分只由 dtm 的 points.Debit 分支发起",
	}
	bannedFuncs := map[string]string{
		"compensate": "手动补偿方法：补偿段已整体退役",
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "mall.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 mall.go 失败：%v（守卫失效）", err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.FuncDecl:
			if why, ok := bannedFuncs[v.Name.Name]; ok {
				t.Errorf("mall.go 仍定义了 %s —— %s", v.Name.Name, why)
			}
		case *ast.SelectorExpr:
			if v.Sel == nil {
				return true
			}
			// 只判 `h.d.<Method>` 这一形态：downstream 门面是唯一的下游出口，
			// 别的调用点（构造、赋值）不构成「写路径复活」。
			recv, ok := v.X.(*ast.SelectorExpr)
			if !ok || recv.Sel == nil || recv.Sel.Name != "d" {
				return true
			}
			if why, ok := bannedCalls[v.Sel.Name]; ok {
				t.Errorf("mall.go 仍有 h.d.%s —— %s", v.Sel.Name, why)
			}
		}
		return true
	})
}

// TestPlaceOrderPrechecksBeforeSubmittingSaga 钉住「预检不进事务」的次序。
//
// 预检走现有 kitex RPC（product.GetProduct），必须在 Submit **之前**且必须存在：
//   - 提前到 Submit 之后 = 商品查不到也已经扣了库存再回滚；
//   - 直接删掉 = 「商品不存在」变成一条必然失败、必然被回滚的全局事务。
//
// 反向探针：把两段调换位置，或删掉 GetProduct 那段，本用例立刻红。
func TestPlaceOrderPrechecksBeforeSubmittingSaga(t *testing.T) {
	text := mallSource(t)
	pre := strings.Index(text, "h.d.GetProduct(ctx, &productpb.GetProductReq{UserId: req.UserId, ProductId: req.ProductID})")
	submit := strings.Index(text, "h.orch.Submit(ctx, dtmorch.OrderCmd{")
	if pre < 0 {
		t.Fatal("PlaceOrder 里找不到预检调用 h.d.GetProduct —— 预检不能删，" +
			"否则商品不可得也会建立全局事务")
	}
	if submit < 0 {
		t.Fatal("PlaceOrder 里找不到 h.orch.Submit —— 下单没有提交 Saga")
	}
	if pre > submit {
		t.Errorf("预检(%d) 在提交(%d) 之后 —— 商品查不到也已经开了全局事务", pre, submit)
	}
}

// TestPlaceOrderStopsAtSaga 钉住 handler 层不再直连写型下游方法。
//
// Saga 接管之后，bff 对四个域的**写**只经由一条出口：dtm 的分支回调。handler
// 里再出现 ReserveStock / CreateOrder / Earn，意味着同时存在两条写路径，而它们
// 的幂等键各自独立 —— 重放一次就是「一张 Saga 单 + 一条手动单」。
func TestPlaceOrderStopsAtSaga(t *testing.T) {
	text := mallSource(t)
	body := text[strings.Index(text, "func (h *MallHandler) PlaceOrder("):]
	end := strings.Index(body[strings.Index(body, "\n}\n"):], "\n}\n")
	if end > 0 {
		body = body[:end]
	}
	for _, banned := range []string{"h.d.ReserveStock", "h.d.ReleaseStock", "h.d.CreateOrder", "h.d.Earn"} {
		if strings.Contains(body, banned) {
			t.Errorf("PlaceOrder 里仍有 %q —— 下单的写只走 dtm Saga 分支", banned)
		}
	}
}
