package dtm

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	otelprop "go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	bffbranch "github.com/gospacex/launchx/examples/micro/bff/internal/dtm/protos"
)

// ---------- 测试替身 ----------

// fakeSaga 记录 Add 序列，让「分支序」在没有 dtm server 的情况下可断言。
//
// 为什么必须打桩而不是直接用官方客户端：官方 *dtmgrpc.SagaGrpc 一 Add 就把
// 载荷编成字节、一 Submit 就去连 dtm server，单测既跑不起来、也看不到
// 「第几个分支、加了什么 URL」。
type fakeSaga struct {
	adds      []fakeAdd
	submitN   int
	submitErr error
}

type fakeAdd struct {
	action     string
	compensate string
	payload    *bffbranch.BusiReq
}

func (f *fakeSaga) Add(action, compensate string, payload proto.Message) Saga {
	req, ok := payload.(*bffbranch.BusiReq)
	if !ok {
		panic(fmt.Sprintf("分支载荷类型是 %T，不是 *protos.BusiReq", payload))
	}
	f.adds = append(f.adds, fakeAdd{action: action, compensate: compensate, payload: req})
	return f
}

func (f *fakeSaga) Submit() error {
	f.submitN++
	return f.submitErr
}

// fakeFactory 让桩记住「自己该返回什么」，供断言提交次数/失败注入。
var lastFake *fakeSaga

const testGID = "gid-test-0001"

const testTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

// newTestOrchestrator 造一个装配齐备、只把 dtm 客户端换成桩的编排器。
func newTestOrchestrator(t *testing.T, mutate func(*Config)) *Orchestrator {
	t.Helper()
	lastFake = nil
	cfg := Config{
		DTMAddr: "127.0.0.1:36790",
		BranchAddrs: map[string]string{
			DomainStock:   "127.0.0.1:9043",
			DomainOrder:   "127.0.0.1:9042",
			DomainProduct: "127.0.0.1:9041",
			DomainPoints:  "127.0.0.1:9044",
		},
		GenGID:      func(context.Context, string) (string, error) { return testGID, nil },
		Traceparent: func(context.Context) string { return testTraceparent },
		NewSaga: func(string, string) Saga {
			lastFake = &fakeSaga{}
			return lastFake
		},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	o, err := New(cfg)
	if err != nil {
		t.Fatalf("构造编排器：%v", err)
	}
	return o
}

// testCmd 一条各字段都合法的下单命令。
func testCmd() OrderCmd {
	return OrderCmd{
		UserID:     1001,
		ProductID:  7,
		Quantity:   3,
		OrderNo:    "bfforder1",
		Title:      "机械键盘",
		UnitPrice:  19900,
		PointsCost: 597,
	}
}

// stepsOf 跑一次 buildSteps。
func stepsOf(t *testing.T, o *Orchestrator, cmd OrderCmd) []Step {
	t.Helper()
	steps, err := o.buildSteps(context.Background(), cmd, testGID)
	if err != nil {
		t.Fatalf("构造分支：%v", err)
	}
	return steps
}

// ---------- 分支序 ----------

// TestBranchOrderIsStockOrderProductPoints 钉住正向分支序。
//
// 判据用**完整有序表**而不是「首是 Deduct、尾是 Debit」两条断言：后者对中间两
// 段是瞎的，而把 order 与 product 对调正是真实事故形态 —— 销量先加、订单后建，
// 订单失败时销量已经被逆序补偿回退过一次，表面一切正常，账上却少一单。
//
// 反向探针（实测过）：把 branchPlan 里 product 段挪到 order 段之前，本用例红，
// 报 "第 3 个分支 = product/AddSales，want order/CreateOrder"。
func TestBranchOrderIsStockOrderProductPoints(t *testing.T) {
	steps := stepsOf(t, newTestOrchestrator(t, nil), testCmd())
	want := [][3]string{
		{DomainStock, "Deduct"},
		{DomainOrder, "CreateOrder"},
		{DomainProduct, "AddSales"},
		{DomainPoints, "Debit"},
	}
	if len(steps) != len(want) {
		t.Fatalf("分支数 = %d，want %d（少一条就是一个域不参与事务，多一条就是凭空多一次写）", len(steps), len(want))
	}
	for i, w := range want {
		gotDomain, gotMethod := splitURL(steps[i].Action)
		if gotDomain != w[0] || gotMethod != w[1] {
			t.Errorf("第 %d 个分支 = %s/%s，want %s/%s", i, gotDomain, gotMethod, w[0], w[1])
		}
	}
}

// TestCompensationPairsFollowBranchOrder 钉住补偿分支与正向分支一一对应。
//
// dtm 按**已成功分支逆序**发起补偿，编排侧唯一能施加影响的地方就是
// Add(action, compensate) 的配对：配错一对，dtm 会拿库存载荷去调积分分支的
// URL —— 域错配时 gRPC 直接 Unimplemented，症状是「补偿全失败、事务卡在
// aborting」，而四域日志里都看不出是编排配错了。
//
// 反向探针（实测过）：把 stock 段的 compensate 改成 stockDeduct，本用例红。
func TestCompensationPairsFollowBranchOrder(t *testing.T) {
	steps := stepsOf(t, newTestOrchestrator(t, nil), testCmd())
	want := [][3]string{
		{"stock", "/dtmbranch.stock.StockBusi/Deduct", "/dtmbranch.stock.StockBusi/Release"},
		{"order", "/dtmbranch.order.OrderBusi/CreateOrder", "/dtmbranch.order.OrderBusi/CancelOrder"},
		{"product", "/dtmbranch.product.ProductBusi/AddSales", "/dtmbranch.product.ProductBusi/ReduceSales"},
		{"points", "/dtmbranch.points.PointsBusi/Debit", "/dtmbranch.points.PointsBusi/Refund"},
	}
	if len(steps) != len(want) {
		t.Fatalf("分支数 = %d，want %d", len(steps), len(want))
	}
	for i, w := range want {
		if steps[i].Domain != w[0] {
			t.Errorf("第 %d 个分支的域 = %q，want %q", i, steps[i].Domain, w[0])
		}
		if got := fullMethod(steps[i].Action); got != w[1] {
			t.Errorf("第 %d 个分支 action = %q，want %q", i, got, w[1])
		}
		if got := fullMethod(steps[i].Compensate); got != w[2] {
			t.Errorf("第 %d 个分支 compensate = %q，want %q", i, got, w[2])
		}
	}
	// 正反两段必须同域：跨域配对就是上面说的「域错配」事故。
	for i, s := range steps {
		if a, b := domainOfFullMethod(fullMethod(s.Action)), domainOfFullMethod(fullMethod(s.Compensate)); a != b {
			t.Errorf("第 %d 个分支正反两段跨域：%s / %s", i, a, b)
		}
	}
}

// TestBranchURLsCarryNoScheme 钉住分支 URL 不带 scheme。
//
// 反向探针（实测过）：把 JoinBranchURL 改成 addr + "grpc://" + fullMethod，
// 本用例红。真机上则是 dtm server 每次回调都报 "call dtmdriver.Use() before
// you use custom scheme"，且只在 dtm 日志里可见。
func TestBranchURLsCarryNoScheme(t *testing.T) {
	steps := stepsOf(t, newTestOrchestrator(t, nil), testCmd())
	for i, s := range steps {
		for _, u := range []string{s.Action, s.Compensate} {
			if strings.Contains(u, "://") {
				t.Errorf("第 %d 个分支 URL = %q，含 scheme：dtm 的默认 driver 会直接报错", i, u)
			}
			host, _, ok := strings.Cut(u, "/")
			if !ok {
				t.Fatalf("第 %d 个分支 URL = %q，没有 '/'（dtm 按第一个 '/' 切分拨号目标与全方法名）", i, u)
			}
			if !strings.Contains(host, ":") {
				t.Errorf("第 %d 个分支 URL 的拨号目标 = %q，缺 host:port", i, host)
			}
			// 分界处必须正好一个 '/'：dtm 按**第一个** '/' 切出拨号目标，
			// 余下整段原样交给 grpc.ClientConn.Invoke，而 gRPC 要求全方法名
			// 带前导 '/'。少一个就是 Unimplemented，多一个就是 Invoke 收到
			// "//dtmbranch…" 同样 Unimplemented。
			rest := strings.TrimPrefix(u, host)
			if !strings.HasPrefix(rest, "/") || strings.HasPrefix(rest, "//") {
				t.Errorf("第 %d 个分支 URL = %q，拨号目标与全方法名之间必须正好一个 '/'", i, u)
			}
		}
	}
	// 地址段与全方法名段逐字对齐（服务名本身另有对着四域 pb.go 的守卫）。
	if got, want := steps[0].Action, "127.0.0.1:9043/dtmbranch.stock.StockBusi/Deduct"; got != want {
		t.Errorf("首个分支 URL = %q，want %q", got, want)
	}
	if got, want := steps[3].Compensate, "127.0.0.1:9044/dtmbranch.points.PointsBusi/Refund"; got != want {
		t.Errorf("末个分支补偿 URL = %q，want %q", got, want)
	}
}

// TestBranchServiceNamesMatchDomainFullMethodNames 把服务全名对着四域 pb.go 核。
//
// 为什么要读源码而不自我声明：dtm server 只按 URL 回调，全名错一个字符就是分支
// Unimplemented，而**本进程连日志都不会有** —— 「编排看起来提交成功了，四个域
// 一条写入都没有」是最难排查的一类症状。四份 busi_grpc.pb.go 才是权威。
func TestBranchServiceNamesMatchDomainFullMethodNames(t *testing.T) {
	cases := []struct {
		domain, srvDir, wantService string
		wantMethods                 []string
	}{
		{DomainStock, "srv-stock", "dtmbranch.stock.StockBusi", []string{"Deduct", "Release"}},
		{DomainOrder, "srv-order", "dtmbranch.order.OrderBusi", []string{"CreateOrder", "CancelOrder"}},
		{DomainProduct, "srv-product", "dtmbranch.product.ProductBusi", []string{"AddSales", "ReduceSales"}},
		{DomainPoints, "srv-points", "dtmbranch.points.PointsBusi", []string{"Debit", "Refund"}},
	}
	for _, c := range cases {
		t.Run(c.domain, func(t *testing.T) {
			path := filepath.Join("..", "..", "..", c.srvDir, "internal", "dtm", "protos", "busi_grpc.pb.go")
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("读 %s 失败：%v（域目录结构变了？）", path, err)
			}
			if !strings.Contains(string(src), fmt.Sprintf("ServiceName: %q", c.wantService)) {
				t.Errorf("%s 的 ServiceName 不是 %q —— 编排侧拼出来的 URL 会 404", path, c.wantService)
			}
			// 生成物里这些常量是对齐排布的（名字与 = 之间有多余空白），
			// 故逐行匹配「含该方法名」且「= 右边是该全名」的行，而不是
			// 在整文件里搜一段拼死的空白。
			for _, m := range c.wantMethods {
				if !hasFullMethodConst(string(src), c.wantService, m) {
					t.Errorf("%s 里找不到 %s 的 %q FullMethodName 常量", path, c.wantService, m)
				}
			}
			if got := serviceOfDomain[c.domain]; got != c.wantService {
				t.Errorf("serviceOfDomain[%q] = %q，want %q", c.domain, got, c.wantService)
			}
		})
	}
}

// TestBusiEnvelopeMatchesDomainReqMessages 把 bff 自己的信封对着四域请求消息核
// 字段编号与类型。
//
// 为什么必须核：proto 线上不带消息名，所以「一个类型编出的字节能被四个不同
// 名的请求类型解开」这件事的前提只有一条 —— 字段编号与类型逐字相同。编号漂了
// 不会编译报错、不会启动失败，只会在真机上表现为每个分支拿到全零载荷、JSON
// 解析失败 → 业务失败 → 逆序补偿，而日志文案是「分支载荷 biz 不是合法 JSON」，
// 几乎没人会想到是编排侧的编号问题。
func TestBusiEnvelopeMatchesDomainReqMessages(t *testing.T) {
	fields := (&bffbranch.BusiReq{}).ProtoReflect().Descriptor().Fields()
	ours := map[string]string{}
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		ours[string(f.Name())] = fieldKey(f)
	}
	want := map[string]string{
		"biz":         "1:string",
		"traceparent": "2:string",
		"fail_inject": "3:string",
	}
	if !reflect.DeepEqual(ours, want) {
		t.Fatalf("bff 信封字段 = %v，want %v", ours, want)
	}
	for _, srv := range []string{"srv-stock", "srv-order", "srv-product", "srv-points"} {
		t.Run(srv, func(t *testing.T) {
			path := filepath.Join("..", "..", "..", srv, "internal", "dtm", "protos", "busi.pb.go")
			got, err := reqStructFieldKeys(path)
			if err != nil {
				t.Fatalf("%s：%v", path, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s 的请求消息字段 = %v，want %v", srv, got, want)
			}
		})
	}
}

// TestBusiEnvelopeWireLayout 手按 wire 格式解一遍 marshal 出来的字节。
//
// 为什么不是「marshal 再 unmarshal 回自己」：那只能证明 proto 库自洽，证明不了
// 字节布局对得上**别的**类型。这里独立复核「字段 1/2/3 是这三个字符串」。
func TestBusiEnvelopeWireLayout(t *testing.T) {
	src := &bffbranch.BusiReq{Biz: `{"order_no":"o-1"}`, Traceparent: "00-trace-span-01", FailInject: "Debit"}
	raw, err := proto.Marshal(src)
	if err != nil {
		t.Fatalf("marshal：%v", err)
	}
	got := map[int]string{}
	for len(raw) > 0 {
		tag, n := consumeVarint(raw)
		raw = raw[n:]
		field, wire := int(tag>>3), int(tag&7)
		if wire != 2 {
			t.Fatalf("字段 %d 的 wire type = %d，本信封三个字段应全是 length-delimited", field, wire)
		}
		length, n := consumeVarint(raw)
		raw = raw[n:]
		ln := int(length)
		if len(raw) < ln {
			t.Fatalf("字段 %d 声明长度 %d，剩余 %d 字节", field, ln, len(raw))
		}
		got[field] = string(raw[:ln])
		raw = raw[ln:]
	}
	want := map[int]string{1: `{"order_no":"o-1"}`, 2: "00-trace-span-01", 3: "Debit"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wire 上解出 %v，want %v", got, want)
	}
}

// ---------- 载荷内容 ----------

// TestEveryBranchPayloadCarriesTraceparent 钉住四条分支载荷都带 traceparent。
//
// 反向探针（实测过）：把 buildSteps 里的 `Traceparent: tp` 删掉，本用例红，
// 报 "分支 0（/…/Deduct）的载荷没有 traceparent"。少了它，正向链在 Jaeger 上
// 断成「bff 一棵 + 四个域各自一棵」，而每个服务都有 span，最难看出是断链。
func TestEveryBranchPayloadCarriesTraceparent(t *testing.T) {
	steps := stepsOf(t, newTestOrchestrator(t, nil), testCmd())
	for i, s := range steps {
		if s.Payload.GetTraceparent() != testTraceparent {
			t.Errorf("分支 %d（%s）的载荷没有 traceparent：got %q，want %q",
				i, fullMethod(s.Action), s.Payload.GetTraceparent(), testTraceparent)
		}
	}
}

// TestFailInjectPassesThroughToEveryBranch 钉住 fail_inject 逐分支透传。
//
// fail_inject 靠**分支方法名**命中（各域分支面 `req.GetFailInject() == branch`），
// 所以它必须是各域自己的方法名（CreateOrder，不是 order.CreateOrder）。串错的
// 症状是「点了火但正向全绿」—— 演示脚本以为补偿被点着了。
func TestFailInjectPassesThroughToEveryBranch(t *testing.T) {
	cmd := testCmd()
	cmd.FailInject = "CreateOrder"
	steps := stepsOf(t, newTestOrchestrator(t, nil), cmd)
	for i, s := range steps {
		if s.Payload.GetFailInject() != "CreateOrder" {
			t.Errorf("分支 %d（%s）的 fail_inject = %q，want CreateOrder",
				i, fullMethod(s.Action), s.Payload.GetFailInject())
		}
	}
}

// TestFailInjectEmptyOnNormalOrder 真机路径：fail_inject 必须为空。
//
// 载荷里的 fail_inject 只要非空就等于「点名让某个分支失败」。把请求参数透传进
// 这里是四行代码的事故（提交即全局回滚），所以默认必须钉死为空。
func TestFailInjectEmptyOnNormalOrder(t *testing.T) {
	steps := stepsOf(t, newTestOrchestrator(t, nil), testCmd())
	for i, s := range steps {
		if s.Payload.GetFailInject() != "" {
			t.Errorf("分支 %d 的 fail_inject = %q，正常下单必须是空", i, s.Payload.GetFailInject())
		}
	}
}

// 四域业务体的同形镜像结构体。
//
// 字段/tag 逐字抄自各域 model 包里的 DTO（DtmStockCmd / DtmOrderCmd +
// OrderItemCmd / DtmSalesCmd / DtmBiz）。用镜像而不是 map：map 比对只验键存在，
// 验不出「键名对但类型错」—— quantity 编成字符串，JSON 解到 int32 会静默变 0，
// 而 0 在库存域是「白写一条零流水」。
type (
	mirrorStock struct {
		OrderNo   string `json:"order_no"`
		ProductID uint64 `json:"product_id"`
		Quantity  int32  `json:"quantity"`
		UserID    uint64 `json:"user_id"`
	}
	mirrorItem struct {
		ProductID uint64
		Name      string
		Quantity  int
		UnitPrice int64
	}
	mirrorOrder struct {
		UserID      uint64       `json:"user_id"`
		OrderNo     string       `json:"order_no"`
		DtmGID      string       `json:"dtm_gid"`
		Items       []mirrorItem `json:"items"`
		Traceparent string       `json:"traceparent"`
	}
	mirrorSales struct {
		ProductID uint64 `json:"product_id"`
		Quantity  int64  `json:"quantity"`
		OrderNo   string `json:"order_no"`
	}
	mirrorPoints struct {
		UserID  int64  `json:"user_id"`
		Cost    int64  `json:"cost"`
		OrderNo string `json:"order_no"`
	}
)

// TestBizBodiesMatchDomainDTOs 把四条业务体解回各域 DTO 的形状逐字段核对。
func TestBizBodiesMatchDomainDTOs(t *testing.T) {
	steps := stepsOf(t, newTestOrchestrator(t, nil), testCmd())
	byDomain := map[string]*bffbranch.BusiReq{}
	for _, s := range steps {
		byDomain[s.Domain] = s.Payload
	}
	for _, d := range []string{DomainStock, DomainOrder, DomainProduct, DomainPoints} {
		if byDomain[d] == nil {
			t.Fatalf("域 %s 没有分支", d)
		}
	}

	var stock mirrorStock
	mustDecode(t, DomainStock, byDomain[DomainStock].GetBiz(), &stock)
	if want := (mirrorStock{OrderNo: "bfforder1", ProductID: 7, Quantity: 3, UserID: 1001}); stock != want {
		t.Errorf("stock biz = %+v，want %+v", stock, want)
	}

	var order mirrorOrder
	mustDecode(t, DomainOrder, byDomain[DomainOrder].GetBiz(), &order)
	if order.DtmGID != testGID {
		t.Errorf("order biz 的 dtm_gid = %q，want %q（占位单与补偿都按它定位）", order.DtmGID, testGID)
	}
	if order.OrderNo != "bfforder1" || order.UserID != 1001 {
		t.Errorf("order biz 的 user/order_no = %d/%q", order.UserID, order.OrderNo)
	}
	if len(order.Items) != 1 {
		t.Fatalf("order biz 的 items 条数 = %d，want 1（空明细的订单总额恒为 0）", len(order.Items))
	}
	if want := (mirrorItem{ProductID: 7, Name: "机械键盘", Quantity: 3, UnitPrice: 19900}); order.Items[0] != want {
		t.Errorf("order biz 的明细 = %+v，want %+v", order.Items[0], want)
	}
	if order.Traceparent != testTraceparent {
		t.Errorf("order biz 的 traceparent = %q（它要落 outbox.traceparent，供 outbox 补发续链）", order.Traceparent)
	}

	var sales mirrorSales
	mustDecode(t, DomainProduct, byDomain[DomainProduct].GetBiz(), &sales)
	if want := (mirrorSales{ProductID: 7, Quantity: 3, OrderNo: "bfforder1"}); sales != want {
		t.Errorf("sales biz = %+v，want %+v", sales, want)
	}

	var points mirrorPoints
	mustDecode(t, DomainPoints, byDomain[DomainPoints].GetBiz(), &points)
	if want := (mirrorPoints{UserID: 1001, Cost: 597, OrderNo: "bfforder1"}); points != want {
		t.Errorf("points biz = %+v，want %+v", points, want)
	}
}

// TestOrderItemKeysMatchDomainStruct 钉住明细键名取自 srv-order 的结构体字段名。
//
// 这不是风格问题而是硬约束：srv-order 的 model.OrderItemCmd **没有 json tag**，
// encoding/json 只能按字段名（或其大小写不敏感变体）匹配，而
// EqualFold("product_id", "ProductID") 为 false（多一个下划线）。载荷一旦改成
// 下划线风格，分支侧解出来就是零值明细，订单域以 "invalid item" 判业务失败 ——
// 一条永远修不好的单。
//
// 守卫方式：直接看那份结构体现在有没有 tag。有人给它补了 tag，本用例立刻红，
// 迫使决策「新 tag 是什么、载荷要不要跟着改」，而不是让两条链各自漂。
func TestOrderItemKeysMatchDomainStruct(t *testing.T) {
	path := filepath.Join("..", "..", "..", "srv-order", "internal", "model", "order.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败：%v", path, err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		t.Fatalf("解析 %s：%v", path, err)
	}
	var tags []string
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "OrderItemCmd" {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				t.Fatalf("OrderItemCmd 不是 struct")
			}
			for _, field := range st.Fields.List {
				if field.Tag != nil {
					tags = append(tags, strings.Trim(field.Tag.Value, "`"))
				}
			}
		}
	}
	if len(tags) > 0 {
		t.Fatalf("srv-order 的 OrderItemCmd 现在有 tag 了（%v）：bff 载荷的明细键名必须跟着改成 tag 的值，"+
			"否则分支侧解析出的仍是零值明细", tags)
	}
}

// TestOrderItemJSONKeysAreDomainFieldNames 把「明细键名 = 域结构体字段名」这条
// 断言做实：把编排侧产出的 JSON 解进**无 tag** 的镜像结构体（与 srv-order 同形）。
//
// 上一条是「源上还没有 tag」的反向判据，这一条是「有的话我们解得出来」的正向
// 判据。两者缺一不可：只有正向，域里换字段名时我们照样静默解出零值。
func TestOrderItemJSONKeysAreDomainFieldNames(t *testing.T) {
	steps := stepsOf(t, newTestOrchestrator(t, nil), testCmd())
	var raw struct {
		Items []map[string]any `json:"items"`
	}
	mustDecode(t, DomainOrder, steps[1].Payload.GetBiz(), &raw)
	if len(raw.Items) != 1 {
		t.Fatalf("items 条数 = %d", len(raw.Items))
	}
	wantKeys := map[string]any{
		"ProductID": float64(7),
		"Name":      "机械键盘",
		"Quantity":  float64(3),
		"UnitPrice": float64(19900),
	}
	if !reflect.DeepEqual(raw.Items[0], wantKeys) {
		t.Errorf("明细的键 = %v，want %v（键名必须与 srv-order 的无 tag 结构体字段名逐字相同）", raw.Items[0], wantKeys)
	}
}

// ---------- 提交流程 ----------

// TestSubmitFeedsBranchSequenceToDtm 端到端钉住「真正交给 dtm 的序列」。
//
// 为什么在 buildSteps 之外再测一遍桩：buildSteps 只证明编得出序列，桩证明真正
// 送进官方 Add 的是同一序列 —— 中间那层循环同样可能写错（只调一次 Add、或把
// compensate 传成 action）。
func TestSubmitFeedsBranchSequenceToDtm(t *testing.T) {
	o := newTestOrchestrator(t, nil)
	res, err := o.Submit(context.Background(), testCmd())
	if err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if res.GID != testGID {
		t.Errorf("返回 gid = %q，want %q", res.GID, testGID)
	}
	if lastFake == nil {
		t.Fatal("NewSaga 没被调用")
	}
	if lastFake.submitN != 1 {
		t.Errorf("Submit 调用次数 = %d，want 1", lastFake.submitN)
	}
	wantMethods := []string{"Deduct", "CreateOrder", "AddSales", "Debit"}
	wantServices := []string{stockService, orderService, productService, pointsService}
	if len(lastFake.adds) != len(wantMethods) {
		t.Fatalf("dtm 侧收到 %d 个分支，want %d", len(lastFake.adds), len(wantMethods))
	}
	for i, a := range lastFake.adds {
		if got, want := fullMethod(a.action), "/"+wantServices[i]+"/"+wantMethods[i]; got != want {
			t.Errorf("第 %d 个分支 action = %q，want %q", i, got, want)
		}
		if a.payload.GetTraceparent() == "" {
			t.Errorf("第 %d 个分支的载荷没有 traceparent", i)
		}
	}
	var orderBiz struct {
		DtmGID string `json:"dtm_gid"`
	}
	mustDecode(t, DomainOrder, lastFake.adds[1].payload.GetBiz(), &orderBiz)
	if orderBiz.DtmGID != testGID {
		t.Errorf("order 分支载荷的 dtm_gid = %q，want %q", orderBiz.DtmGID, testGID)
	}
}

// TestSubmitRejectsInvalidCommandBeforeOpeningTransaction 命令非法不得建立事务。
//
// 判据是「Add 一个都没发生」：命令若在事务里失败，dtm 会补偿掉已经跑成功的
// 分支，对调用方表现为「提交成功、然后什么都没了」—— 那是最难解释的一种结局。
func TestSubmitRejectsInvalidCommandBeforeOpeningTransaction(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(c *OrderCmd)
	}{
		{"user_id 为 0", func(c *OrderCmd) { c.UserID = 0 }},
		{"product_id 为 0", func(c *OrderCmd) { c.ProductID = 0 }},
		{"quantity 为 0", func(c *OrderCmd) { c.Quantity = 0 }},
		{"quantity 为负", func(c *OrderCmd) { c.Quantity = -1 }},
		{"order_no 为空", func(c *OrderCmd) { c.OrderNo = "" }},
		{"unit_price 为负", func(c *OrderCmd) { c.UnitPrice = -1 }},
		{"points_cost 为 0", func(c *OrderCmd) { c.PointsCost = 0 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := newTestOrchestrator(t, nil)
			cmd := testCmd()
			c.mutate(&cmd)
			if _, err := o.Submit(context.Background(), cmd); err == nil {
				t.Fatal("命令非法却提交成功了")
			}
			if lastFake != nil && (len(lastFake.adds) != 0 || lastFake.submitN != 0) {
				t.Errorf("命令非法却已经建了事务：Add %d 次、Submit %d 次", len(lastFake.adds), lastFake.submitN)
			}
		})
	}
}

// TestSubmitFailurePropagatesAndNeverFallsBack 提交失败原样上抛，且不回落到手动链。
//
// 「不回落到手动编排链」这条钉在这里：失败路径上不能出现任何对下游域服的直连
// 补偿调用。桩里除了那一次 Submit 之外没有别的出口 —— 手动链退役后，编排器唯一
// 的出向动作就是这一次 Submit。
func TestSubmitFailurePropagatesAndNeverFallsBack(t *testing.T) {
	boom := errors.New("dtm server 拒绝：全局事务不存在")
	o := newTestOrchestrator(t, nil)
	o.newSaga = func(string, string) Saga { lastFake = &fakeSaga{submitErr: boom}; return lastFake }
	if _, err := o.Submit(context.Background(), testCmd()); !errors.Is(err, boom) {
		t.Fatalf("错误 = %v，want 含 %v（handler 会把它渲成 5xx，调用方据此判断能不能原样重试）", err, boom)
	}
	if len(lastFake.adds) != 4 {
		t.Errorf("失败前应已构造 4 个分支，实际 %d", len(lastFake.adds))
	}
	if lastFake.submitN != 1 {
		t.Errorf("Submit 调用次数 = %d，want 1", lastFake.submitN)
	}
}

// TestGIDFailureIsReportedBeforeSubmit gid 生成失败不得建事务。
//
// 覆盖的是「dtm 不可达」这条最常见的降级路径：gid 拿不到还往下走，载荷里的
// dtm_gid 就是空串，而空 dtm_gid 是订单域的业务失败 —— 一次配置故障会被表现成
// 「每个分支都 4002」。
func TestGIDFailureIsReportedBeforeSubmit(t *testing.T) {
	boom := errors.New("dtm server 不可达")
	o := newTestOrchestrator(t, func(c *Config) {
		c.GenGID = func(context.Context, string) (string, error) { return "", boom }
	})
	if _, err := o.Submit(context.Background(), testCmd()); !errors.Is(err, boom) {
		t.Fatalf("错误 = %v，want %v", err, boom)
	}
	if lastFake != nil {
		t.Errorf("gid 失败却建了事务：Add %d 次、Submit %d 次", len(lastFake.adds), lastFake.submitN)
	}
}

// ---------- 配置与依赖 ----------

// TestNewRejectsMissingConfig 配置缺失必须在装配期就说清缺哪一项。
func TestNewRejectsMissingConfig(t *testing.T) {
	base := Config{
		DTMAddr: "127.0.0.1:36790",
		BranchAddrs: map[string]string{
			DomainStock: "127.0.0.1:9043", DomainOrder: "127.0.0.1:9042",
			DomainProduct: "127.0.0.1:9041", DomainPoints: "127.0.0.1:9044",
		},
	}
	t.Run("缺 dtm server 地址", func(t *testing.T) {
		c := base
		c.DTMAddr = ""
		_, err := New(c)
		if err == nil || !strings.Contains(err.Error(), "xmiddleware.dtm.grpc_addr") {
			t.Fatalf("错误 = %v，want 含 xmiddleware.dtm.grpc_addr", err)
		}
	})
	t.Run("缺分支面地址", func(t *testing.T) {
		c := base
		c.BranchAddrs = map[string]string{DomainStock: "127.0.0.1:9043"}
		_, err := New(c)
		if err == nil {
			t.Fatal("缺三个域的分支地址却构造成功")
		}
		for _, d := range []string{DomainOrder, DomainProduct, DomainPoints} {
			if !strings.Contains(err.Error(), d) {
				t.Errorf("错误文案 %q 没点名缺的 %s", err, d)
			}
		}
	})
	t.Run("配全了", func(t *testing.T) {
		if _, err := New(base); err != nil {
			t.Fatalf("配全却构造失败：%v", err)
		}
	})
}

// TestUnavailableOrchestratorFailsSubmitButNotProcess strict.dtm=false 的失败姿态。
//
// 判据是「Submit 恒报错、且不 panic」：dtm 配错时进程必须活着（带病运行），
// 下单返回 5xx。这里若 panic，一次配置缺项会变成「每个请求都 500 且日志里是
// runtime error」。
func TestUnavailableOrchestratorFailsSubmitButNotProcess(t *testing.T) {
	o := Unavailable(errors.New("xmiddleware.dtm.grpc_addr 未配置"))
	_, err := o.Submit(context.Background(), testCmd())
	if err == nil {
		t.Fatal("不可用编排器竟然提交成功")
	}
	if !strings.Contains(err.Error(), "xmiddleware.dtm.grpc_addr") {
		t.Errorf("错误 = %v，want 含缺失的配置键", err)
	}
	if lastFake != nil {
		t.Errorf("不可用编排器仍然建了事务")
	}
}

// TestLocalGIDIsUniqueAndReadable 本地 gid：形态可读 + 不撞。
//
// 撞号的后果不是「重试一下」，而是「新事务顶掉旧事务」。前缀（毫秒 base36）让号
// 在日志/表里按时间可读，后缀（8 字节随机 hex）保证同毫秒内不撞。
func TestLocalGIDIsUniqueAndReadable(t *testing.T) {
	seen := make(map[string]bool, 2000)
	for i := 0; i < 2000; i++ {
		gid, err := LocalGID(context.Background(), "")
		if err != nil {
			t.Fatalf("LocalGID：%v", err)
		}
		head, tail, ok := strings.Cut(gid, "-")
		if !ok || head == "" || tail == "" {
			t.Fatalf("gid 形态 = %q，want <毫秒 base36>-<hex>", gid)
		}
		if _, err := strconv.ParseInt(head, 36, 64); err != nil {
			t.Fatalf("gid 的时间前缀 %q 不是 base36 毫秒：%v", head, err)
		}
		if _, err := hex.DecodeString(tail); err != nil {
			t.Fatalf("gid 的随机段 %q 不是 hex：%v", tail, err)
		}
		if seen[gid] {
			t.Fatalf("gid 撞号：%q（连续 2000 次生成）", gid)
		}
		seen[gid] = true
	}
}

// TestServerGIDTurnsPanicIntoError 官方 MustGenGid 的 panic 必须变 error。
//
// 官方用 dtmimp.E2P（panic）表达「dtm 不可达」，而本仓对 dtm 的失败姿态是
// 5xx + Warn 带病运行，进程不能退 —— 这层 recover 是口径不是防御。
func TestServerGIDTurnsPanicIntoError(t *testing.T) {
	// 端口 1 在本机没有任何监听，MustGenGid 内部 E2P → panic → 应被 recover。
	gid, err := ServerGID(context.Background(), "127.0.0.1:1")
	if err == nil {
		t.Skipf("该地址居然可达（拿到 gid=%q），无法验证 panic 路径", gid)
	}
	if !strings.Contains(err.Error(), "不可达") {
		t.Errorf("错误 = %v，want 含「不可达」", err)
	}
}

// ---------- 默认 traceparent 取值 ----------

// TestTraceparentFromCtxMatchesSpanContext 核默认取值真的取到本 span 的链路串。
//
// 判据不是「非空」：返回一段编造的字符串同样非空，但四域 Extract 回来会挂到一条
// 不存在的 trace 上。所以起真 span，比对 SpanContext 的 TraceID/SpanID。
func TestTraceparentFromCtxMatchesSpanContext(t *testing.T) {
	tp := otel.GetTextMapPropagator()
	tracerProvider := otel.GetTracerProvider()
	otel.SetTextMapPropagator(otelprop.TraceContext{})
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample())))
	t.Cleanup(func() {
		otel.SetTextMapPropagator(tp)
		otel.SetTracerProvider(tracerProvider)
	})

	ctx, span := otel.Tracer("bff-dtm-test").Start(context.Background(), "PlaceOrder")
	defer span.End()

	got := TraceparentFromCtx(ctx)
	sc := span.SpanContext()
	want := fmt.Sprintf("00-%s-%s-01", sc.TraceID(), sc.SpanID())
	if got != want {
		t.Errorf("traceparent = %q，want %q", got, want)
	}
}

// TestTraceparentEmptyWhenNoSpan 没有活跃 span 时返回空串而不是编造。
//
// 空串在分支面的语义是明确的（dtmx.ExtractParent 原样放行 → 新根 trace +
// biz.dtm.gid 关联）；编造一个同 ID 的孤儿 span 则是自欺，且在 Jaeger 上表现为
// 「有个 span 挂在一条不存在的 trace 下」，比没有更难查。
func TestTraceparentEmptyWhenNoSpan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := TraceparentFromCtx(ctx); got != "" {
		t.Errorf("无活跃 span 时 traceparent = %q，want 空串", got)
	}
}

// ---------- helper ----------

func mustDecode(t *testing.T, domain, biz string, out any) {
	t.Helper()
	if err := json.Unmarshal([]byte(biz), out); err != nil {
		t.Fatalf("解 %s 分支的 biz 失败：%v（biz=%s）", domain, err, biz)
	}
}

// hasFullMethodConst 生成物里是否存在 `<...>FullMethodName<空白>= "/svc/Method"`。
func hasFullMethodConst(src, svc, method string) bool {
	want := fmt.Sprintf("%s/%s\"", svc, method)
	for _, line := range strings.Split(src, "\n") {
		if !strings.Contains(line, "FullMethodName") || !strings.Contains(line, want) {
			continue
		}
		eq := strings.Index(line, "=")
		if eq >= 0 && strings.Contains(strings.TrimSpace(line[eq+1:]), want) {
			return true
		}
	}
	return false
}

// fullMethod 取 URL 的全方法名段（含前导 /）。
func fullMethod(url string) string {
	_, method, ok := strings.Cut(url, "/")
	if !ok {
		return url
	}
	return "/" + method
}

// splitURL 把分支 URL 拆成 (域, 方法名)：域由全方法名的服务前缀反推
// （URL 的 host 段是地址，不含域信息）。
func splitURL(url string) (domain, method string) {
	_, full, ok := strings.Cut(url, "/")
	if !ok {
		return "", url
	}
	full = "/" + full
	return domainOfFullMethod(full), full[strings.LastIndex(full, "/")+1:]
}

// domainOfFullMethod 全方法名 → 域键。
func domainOfFullMethod(full string) string {
	body := strings.TrimPrefix(full, "/")
	for domain, svc := range serviceOfDomain {
		if strings.HasPrefix(body, svc+"/") {
			return domain
		}
	}
	return body
}

// consumeVarint 解一个 protobuf varint。
func consumeVarint(b []byte) (uint64, int) {
	var v uint64
	var shift uint
	for i, c := range b {
		v |= uint64(c&0x7f) << shift
		if c < 0x80 {
			return v, i + 1
		}
		shift += 7
	}
	return 0, 0
}

// fieldKey protoreflect 字段 → "<编号>:<kind>"。
func fieldKey(f protoreflect.FieldDescriptor) string {
	return fmt.Sprintf("%d:%s", f.Number(), f.Kind())
}

var protobufTagRe = regexp.MustCompile(`protobuf:"(\w+),(\d+),(\w+),name=([A-Za-z0-9_]+),`)

// reqStructFieldKeys 从 pb.go 里抽出「*BusiReq」请求结构的 protobuf tag，
// 还原成 {字段名: "编号:类型"}。
func reqStructFieldKeys(path string) (map[string]string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || !strings.HasSuffix(ts.Name.Name, "BusiReq") {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			for _, field := range st.Fields.List {
				if field.Tag == nil {
					continue
				}
				m := protobufTagRe.FindStringSubmatch(strings.Trim(field.Tag.Value, "`"))
				if m == nil {
					continue
				}
				out[m[4]] = m[2] + ":" + kindOfWire(m[1])
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s 里没解析出 *BusiReq 的 protobuf tag（生成物形态变了？）", path)
	}
	return out, nil
}

// kindOfWire pb.go tag 里的 wire 形态 → protoreflect.Kind 名。
func kindOfWire(wire string) string {
	switch wire {
	case "bytes":
		return "string"
	case "varint":
		return "int64"
	case "fixed64":
		return "uint64"
	case "fixed32":
		return "uint32"
	case "zigzag32":
		return "sint32"
	case "zigzag64":
		return "sint64"
	default:
		return wire
	}
}
