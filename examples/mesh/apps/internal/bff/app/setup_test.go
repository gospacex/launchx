package app

// 入口闸门的**装配级**判据。
//
// handler 级（`pkg/routing` 的四向测试）证明「链条本身的裁决逻辑」是对的，
// 但它证明不了两件只有在这一层才看得见的事：
//
//  1. **中间件到底挂上了没有**：hertz 的 `Engine.Use` 是 `group.Handlers = append(...)`，
//     而路由在被注册的那一刻就把当时的 `group.Handlers` 快照进自己的链 ——
//     **注册之后再 Use，中间件对已注册的路由完全不起作用，且不报任何错**。
//     于是「闸门写好了、测试也绿了、入口却在裸奔」是一种可达状态。
//  2. **`auth.require=false` 是否真的不装闸门**：装了再放行会让配置变成谎话
//     （日志说全放行，实际全 401）—— 本仓最忌讳的「配置写着 A、行为是 B」。
//
// 因此这里两个测试一起用：
//   - TestAssemblyOrderPutsMiddlewareBeforeRoutes 静态钉住 setup.go 里的顺序；
//   - TestRequiredIdentityGateBlocksAnonymous 用真引擎把「无凭据 401」跑一遍，
//     并证明**顺序反过来时这道闸形同虚设**（反向探针，否则第 1 条只是句注释）。

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/gospacex/webx"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/bff/downstream"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/bff/handler"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/routing"
)

// identitySkipPathsForTest 与 Setup 的同名表保持一致（只豁免健康探针）。
//
// 刻意在这里再写一份而不是引用：测试要的是「按豁免表行为符合预期」，若直接
// 引用被测常量，表里少一条也不会红 —— 重复在这里是**有意的**，缺失由注释解释。
var identitySkipPathsForTest = map[string]bool{
	"/healthz/live":  true,
	"/healthz/ready": true,
}

// fakeResolver 廉价解析器：**只**认 `Bearer good-token`。
//
// 不依赖 cachex：`Resolver` 是函数类型，取证与裁决在实现里是分开的两件事
// （见 routing.NewTokenResolver 的包注释），裁决逻辑在 identityLayer。
// 用真 TokenResolver 反而会把「凭据存储」这个外部依赖拖进来 —— 它要是挂了，
// 入口闸门的所有用例会一起 401，而那不是这里要测的东西。
func fakeResolver(ctx context.Context, c *app.RequestContext) context.Context {
	raw := string(c.Request.Header.Peek(routing.AuthorizationHeader))
	if raw != "Bearer good-token" {
		return ctx
	}
	return routing.WithIdentity(ctx, routing.Identity{UserID: 1001, OperatorID: 1001})
}

// assemble 按 Setup 的形态装配一个引擎。reversed 为真时**先注册路由后挂链**
// （用来证明顺序是契约而不是风格）。
func assemble(t *testing.T, requireAuth, reversed bool) *webx.App {
	t.Helper()
	// 用 TestApp 而不是 New：它会真监听一个随机端口，把 hertz 的默认链
	// （recovery/otel/logging/ratelimit/cors/timeout）一并带上 —— New 出来的
	// 引擎不监听，手工 Serve 又要绕 hertz 的底层 Serve(ctx, conn) 接口。
	e, err := webx.TestApp()
	if err != nil {
		t.Fatalf("webx.TestApp: %v", err)
	}
	t.Cleanup(func() { _ = e.Shutdown(context.Background()) })

	opts := []routing.Option{routing.WithIdentityResolver(fakeResolver)}
	if requireAuth {
		opts = append(opts, routing.WithRequiredIdentity(func(p string) bool {
			return identitySkipPathsForTest[strings.TrimSuffix(p, "/")]
		}))
	}
	chain := routing.Chain(opts...)
	register := func() { handler.New(&downstream.Clients{}, false).Register(e) }

	if reversed {
		register()
		e.Use(chain...)
		return e
	}
	e.Use(chain...)
	register()
	return e
}

// status 对已监听的引擎发一次请求。
//
// 走真 HTTP 而不是 hertz 的 ut 包：入口闸门的价值恰恰在于「整个 HTTP 栈
// （含路由匹配与默认链）都在链内」，跳过这一层就等于没测到顺序问题。
func status(t *testing.T, e *webx.App, method, path, token string) int {
	t.Helper()
	req, err := http.NewRequest(method, e.URL()+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set(routing.AuthorizationHeader, token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// pingPath / 业务路径：放行类断言一律用 /ping。
//
// 不能用 /order/:id 之类验证「放行」：它要触达下游，而这里的 Clients 是零值，
// 请求会在 nil 接口上炸成 500 —— 于是「没被闸门拦住」这一信息被 500 盖掉，
// 用例反而没法分辨是被拦还是通过。`/ping` 自响应、不触达下游，正是为这种判据
// 准备的（见 handler.ping 的注释）。
const (
	pingPath = "/ping"
	readPath = "/order/1"
)

// TestRequiredIdentityGateBlocksAnonymous 闸门打开时的四条装配级断言。
//
// 四条必须齐全，缺一条就可能放行一类不该放行的请求（这也是
// examples/micro/scripts/auth-probe.sh 那四条手工断言的来历）。
// 四条共用一个引擎：每次 TestApp 的优雅停机要等满 5s，一条一建会让这条判据
// 慢到没人愿意跑 —— 而慢本身就是判据被绕开的常见原因。
func TestRequiredIdentityGateBlocksAnonymous(t *testing.T) {
	e := assemble(t, true, false)
	cases := []struct {
		name   string
		path   string
		token  string
		want   int
		reason string
	}{
		{"无凭据业务路径被拒", readPath, "", http.StatusUnauthorized, "匿名不得读别人的订单"},
		{"错误凭据被拒", readPath, "Bearer wrong-token", http.StatusUnauthorized, "错令牌与匿名同样处理"},
		{"正确凭据放行", pingPath, "Bearer good-token", http.StatusOK, "反查到主体就该放行"},
		{"探针豁免", "/healthz/live", "", http.StatusOK, "探针必须豁免，否则存储抖一下会被编排系统重启一批健康实例"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := status(t, e, http.MethodGet, c.path, c.token); got != c.want {
				t.Fatalf("%s → %d，期望 %d（%s）", c.path, got, c.want, c.reason)
			}
		})
	}
}

// TestRequiredIdentityGateOpened: 装配顺序反过来时，闸门对已注册路由不起作用。
//
// 这不是「希望它这样」，而是把 hertz 的行为写成判据：**防的是有人调换顺序**。
// 若哪天 hertz 改了上面那条 append 语义，本用例会红 —— 那时候要改的是
// setup.go 里那条注释（它陈述的理由失效了），而不是把这条测试删掉。
func TestRequiredIdentityIsInertWhenRegisteredAfterRoutes(t *testing.T) {
	e := assemble(t, true, true)
	if got := status(t, e, http.MethodGet, pingPath, ""); got != http.StatusOK {
		t.Fatalf("反向顺序下拿到了 %d —— 若这里变成 401，说明 hertz 改了 Use 的语义，"+
			"setup.go 里「必须在注册路由之前挂上」的注释要跟着改", got)
	}
}

// TestAuthDisabledReallyDisablesGate 配置关掉鉴权 ⇒ 真的不装闸门。
//
// 反向断言的另一半：装了闸门再放行是谎话。History：曾经的写法让
// `auth.require=false` 在日志里宣称全放行、实际每个请求都 401。
func TestAuthDisabledReallyDisablesGate(t *testing.T) {
	e := assemble(t, false, false)
	if got := status(t, e, http.MethodGet, pingPath, ""); got != http.StatusOK {
		t.Fatalf("auth.require=false 时匿名请求应放行，实际 %d", got)
	}
	if got := status(t, e, http.MethodGet, readPath, ""); got == http.StatusUnauthorized {
		t.Fatal("auth.require=false 时不应出现 401 —— 那说明闸门装上了却宣称已关闭")
	}
}

// TestRequiredIdentityIsConditional 闸门必须挂在某个**条件**里，不能无条件装。
//
// 上面那个运行时用例（TestAuthDisabledReallyDisablesGate）用的是本文件的装配
// 复刻件，它测不到 `Setup` 里那句 `if requireAuth` —— 把条件删掉、直接 append，
// 运行时用例依旧全绿，而 `auth.require=false` 会变成「日志说全放行、实际全 401」。
// 这类「配置写着 A、行为是 B」正是本仓最忌讳的形态，故回到源码补一条。
//
// 只看 WithRequiredIdentity 是不是处在某个 if 分支里，而**不**校验那个条件具体
// 用了哪个变量：判条件表达式会把一次正常的重构（换个变量名）判成违规。
//
// 但条件**不能是常量**。`if true { opts = append(...) }` 在结构上同样"处在 if 里"，
// 于是第一版判据对它全绿 —— 而它恰恰是本用例要挡的形态：闸门无条件装上，
// `auth.require=false` 变成「日志说全放行、实际全 401」。故这里排除
// `true` / `false` 这类常量条件（含常量折叠不了的字面量条件）。
//
// 反向探针：`if requireAuth {` 改成 `if true {` ⇒ 本用例必须红。
func TestRequiredIdentityIsConditional(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "setup.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 setup.go 失败：%v（守卫失效）", err)
	}

	// condBlocks 收集"条件**非常量**"的分支块。
	type block struct{ stmt ast.Node }
	var condBlocks []block
	var constIfs int

	constant := func(e ast.Expr) bool {
		id, ok := e.(*ast.Ident)
		return ok && (id.Name == "true" || id.Name == "false")
	}

	ast.Inspect(f, func(n ast.Node) bool {
		stmt, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		if constant(stmt.Cond) {
			constIfs++
			return true
		}
		condBlocks = append(condBlocks, block{stmt.Body})
		if stmt.Else != nil {
			condBlocks = append(condBlocks, block{stmt.Else})
		}
		return true
	})

	var total, insideIf int
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel == nil || sel.Sel.Name != "WithRequiredIdentity" {
			return true
		}
		total++
		for _, blk := range condBlocks {
			b, ok := blk.stmt.(*ast.BlockStmt)
			if !ok {
				continue
			}
			if call.Pos() >= b.Pos() && call.End() <= b.End() {
				insideIf++
			}
		}
		return true
	})

	if total == 0 {
		t.Fatal("setup.go 里找不到 WithRequiredIdentity（装配形态变了，判据要跟着改）")
	}
	if insideIf != total {
		t.Fatalf("WithRequiredIdentity 有 %d 处，其中只有 %d 处挂在**非常量**的条件分支里"+
			"（本文件另有 %d 个常量条件分支）—— 常量条件的那一处等于无条件装闸门，"+
			"会让 auth.require 这个开关变成谎话（关了也照拦）", total, insideIf, constIfs)
	}
}

// TestAssemblyOrderPutsMiddlewareBeforeRoutes 静态钉住 setup.go 里的装配顺序。
//
// 上面的运行时用例证明「按这个顺序装 ⇒ 闸门生效」，但它管不到 setup.go 自己：
// 有人在 Setup 里把 `Register` 挪到 `Use` 前面，运行时用例依旧全绿（它用的是自己
// 的装配函数）。故这里再回到源码，断言真实装配的顺序。
//
// 只看**同一个文件内的行号**：本仓两处 HTTP 边界（monolith 的 Setup 与本文件）
// 都是「装配与注册写在一个函数里」的形态，跨函数的静态顺序要调用图才判得准，
// 那会把判据做成新的假红源 —— 宁愿窄一点也不引入不可靠的守卫。
func TestAssemblyOrderPutsMiddlewareBeforeRoutes(t *testing.T) {
	const path = "setup.go"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败（文件挪了？判据必须跟着改）：%v", path, err)
	}
	var useLine, registerLine int
	for i, line := range strings.Split(string(b), "\n") {
		n := i + 1
		switch {
		case strings.Contains(line, "engine.Use("):
			if useLine == 0 {
				useLine = n
			}
		case strings.Contains(line, ".Register(engine)"):
			if registerLine == 0 {
				registerLine = n
			}
		}
	}
	if useLine == 0 || registerLine == 0 {
		t.Fatalf("没找全装配语句（Use=%d, Register=%d）—— 装配形态变了，判据要跟着改", useLine, registerLine)
	}
	if useLine > registerLine {
		t.Fatalf("engine.Use（第 %d 行）晚于 Register（第 %d 行）：hertz 的路由在注册时就快照了当时的中间件链，"+
			"此刻挂的链对已注册路由完全不起作用，且不报错 —— 入口会静默裸奔", useLine, registerLine)
	}
}
