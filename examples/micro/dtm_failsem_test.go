package micro

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// dtm 四域分支面失败语义的一致性守卫（tasks.md 2.5 / design.md D7a）。
//
// **为什么只能结构化判、不能真跨域压测**：四域分支面都在 `srv-<域>/internal/dtm/`
// 下，而 Go 的 internal 规则规定 `.../srv-order/internal/...` 只对
// `.../srv-order/` 内的包可见 —— `examples/micro` 根包**无权 import** 它们。
// 故一条真跑四域的测试在这层根本写不出来（写了也编译不过）。
// 各域自己的行为判据由各域测试负责（order/product/stock/points 各自都有
// 「业务失败 → Aborted、基础失败 → Internal」的用例）；本文件负责钉住
// **四域不许各写各的**，也就是那层跨进程测试覆盖不到的漂移面。
//
// 为什么这道判据必要：dtm 回调用 raw codec，server 端
// `dtmsvr/trans_status.go::getGrpcResult` 把响应字节塞进 `*[]byte` **从不解析**，
// 客户端 `dtmgrpc/type.go::GrpcError2DtmError` 只看 status：
// `Aborted` = 业务失败（不重试，转逆序补偿）、`FailedPrecondition` + ONGOING =
// 仍在进行中、其余 = 基础失败（退避重试）。实测就漂过一次：stock 直接
// `return err`（grpc-go 编成 Unknown），其余三域翻 Internal —— dtm 侧行为相同，
// 但日志里 Unknown 会被读成「回调根本没到」。没有这道守卫，它会继续漂。
func TestDtmBranchFacesShareOneFailureMapping(t *testing.T) {
	domains := []string{"product", "order", "stock", "points"}
	var bad []string
	scanned := 0

	for _, d := range domains {
		path := filepath.Join("srv-"+d, "internal", "dtm", "busi_server.go")
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			bad = append(bad, path+": 解析失败 "+err.Error())
			continue
		}
		scanned++

		// 找到失败映射出口，并在它的函数体里逐个查选择器。
		fn := findErrorMapper(fset, f)
		if fn == nil {
			bad = append(bad, path+": 找不到失败映射出口（branchError/reject 之类）—— 无法确认 status 是被有意设定的")
			continue
		}
		sel := collectSelectors(fset, fn)

		// ① 业务失败必须被识别且映射到 Aborted。
		//    注意这条是**结构性**判据：只确认「有这条分流且结果落在 Aborted」，
		//    不判条件写得对不对。条件的语义由各域自己的用例管（两者分工互补，
		//    否则这道守卫就得重实现一遍业务）。
		//    Aborted 可以由 status.Error(codes.Aborted,...) 直接给出，也可以由
		//    dtmgrpc.DtmError2GrpcError(err) 给出（它把 ErrFailure 哨兵译成 Aborted）。
		if !hasSelector(sel, "IsBusinessFailure") {
			bad = append(bad, path+": 失败出口未按 dtmx.IsBusinessFailure 分流业务失败 —— 业务失败会被当基础失败无限重试")
		}
		if !hasSelector(sel, "Aborted") && !hasSelector(sel, "DtmError2GrpcError") {
			bad = append(bad, path+": 业务失败未映射到 codes.Aborted（也无 DtmError2GrpcError 代译）—— dtm 会当基础失败重试")
		}
		// ② 基础失败必须显式翻成 Internal。直接 return err 会被 grpc-go 编成
		//    Unknown；Unknown 在 dtm 侧行为虽同 Internal，却会被读成「回调没到」。
		if !hasSelector(sel, "Internal") {
			bad = append(bad, path+": 失败出口未显式映射 codes.Internal —— 基础失败会落成 grpc Unknown，排障时误读为回调未达")
		}
	}

	if scanned != len(domains) {
		t.Fatalf("只解析到 %d/%d 个域的分支面，四域必须都在", scanned, len(domains))
	}
	if len(bad) > 0 {
		t.Fatalf("四域分支面失败语义不一致（%d 处）：\n  %s", len(bad), strings.Join(bad, "\n  "))
	}
	t.Logf("四域分支面失败语义一致：业务失败→Aborted、基础失败→Internal（扫过 %d 个域）", scanned)
	// 两条已实测会红的反向探针（改任一条，本用例必须报红）：
	//   ① 把 srv-stock 的 branchError 末行改成 `return err`
	//      → 「失败出口未显式映射 codes.Internal」
	//   ② 把 srv-points 的 `dtmx.IsBusinessFailure(err)` 换成自造判定
	//      → 「失败出口未按 dtmx.IsBusinessFailure 分流业务失败」
	// 注意：②**不能**用 `false && dtmx.IsBusinessFailure(err)` 注入 —— 调用仍在
	// AST 里，结构性判据本就该放它过去；用了它会得出「守卫没生效」的错误结论。
}

// findErrorMapper 找把 error 翻成 gRPC status 的那一处，形如
//
//	func branchError(err error) error
//	func (s *BusiServer) reject(...) (*Resp, error)
//
// 判据必须在**这个函数的函数体里**取，而不是全文件子串搜：全文件搜 "Internal"
// 会命中 codes.CodeInternal（响应体的业务码，与 gRPC status 无关），于是
// 「失败出口根本没翻 status」也会被判通过 —— 守卫变成空的。
func findErrorMapper(fset *token.FileSet, f *ast.File) *ast.FuncDecl {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if strings.Contains(fn.Name.Name, "branchError") || strings.Contains(fn.Name.Name, "reject") {
			return fn
		}
	}
	return nil
}

// collectSelectors 收函数体内出现过的所有「包.标识」选择器名（去重）。
func collectSelectors(fset *token.FileSet, fn *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(fn, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok {
			out[id.Name+"."+sel.Sel.Name] = true
			out[sel.Sel.Name] = true
		}
		return true
	})
	return out
}

func hasSelector(sel map[string]bool, name string) bool { return sel[name] }

// TestDtmBranchFacesHaveNoRawErrorPassthrough 反向判据：分支面里不该再有
// 「把 service 的 error 原样往上抛」的出口——那正是 stock 漂成 Unknown 的写法。
//
// 反向探针：把 srv-stock 的 branchError 改成 `return err`，本用例必须红。
// 已实测：会红，报「失败出口没有 status.Error / DtmError2GrpcError 调用」。
func TestDtmBranchFacesHaveNoRawErrorPassthrough(t *testing.T) {
	const stockPath = "srv-stock/internal/dtm/busi_server.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join("srv-stock", "internal", "dtm", "busi_server.go"), nil, 0)
	if err != nil {
		t.Fatalf("解析 %s 失败：%v", stockPath, err)
	}
	fn := findErrorMapper(fset, f)
	if fn == nil {
		t.Fatalf("srv-stock 找不到失败映射出口")
	}
	sel := collectSelectors(fset, fn)
	if !hasSelector(sel, "status.Error") && !hasSelector(sel, "DtmError2GrpcError") {
		t.Fatal("srv-stock 的失败出口没有 status.Error / DtmError2GrpcError 调用 —— 基础失败会落成 grpc Unknown")
	}
}
