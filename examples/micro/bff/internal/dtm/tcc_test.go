package dtm

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	bffbranch "github.com/gospacex/launchx/examples/micro/bff/internal/dtm/protos"
)

// ---------- 测试替身 ----------

// fakeTCC 记录 CallBranch 序列，让「Try 分支序 + 三段 URL」在没有 dtm server
// 的情况下可断言。
//
// 为什么必须打桩而不是直接用官方客户端：`dtmgrpc.TccGlobalTransaction` 一进去
// 就 Prepare、并在 defer 里 Submit/Abort，要一条真连接；桩把这两步收成一次
// 函数调用，于是「第一个分支是谁、confirm URL 填没填」全都暴露成可断言的值。
type fakeTCC struct {
	calls    []fakeCallBranch
	failAt   int // 第几次 CallBranch 起返回错误（-1 = 全绿）
	failWith error
}

type fakeCallBranch struct {
	busiMsg proto.Message
	tryURL  string
	confirm string
	cancel  string
	reply   any
	optsN   int
}

func (f *fakeTCC) CallBranch(busiMsg proto.Message, tryURL, confirmURL, cancelURL string, reply any, opts ...grpc.CallOption) error {
	if _, ok := busiMsg.(*bffbranch.BusiReq); !ok {
		panic(fmt.Sprintf("分支载荷类型是 %T，不是 *protos.BusiReq", busiMsg))
	}
	f.calls = append(f.calls, fakeCallBranch{
		busiMsg: busiMsg, tryURL: tryURL, confirm: confirmURL, cancel: cancelURL,
		reply: reply, optsN: len(opts),
	})
	if f.failAt >= 0 && len(f.calls)-1 >= f.failAt {
		if f.failWith != nil {
			return f.failWith
		}
		return errors.New("branch: FAILURE 库存不足")
	}
	return nil
}

// lastFakeTCC 让桩记住「自己被构造成了什么」，供断言提交序列/失败注入。
var lastFakeTCC *fakeTCC

// testLockCmd 一条各字段都合法的预售锁定命令。
func testLockCmd() LockCmd {
	return LockCmd{
		UserID:     1001,
		ProductID:  7,
		Quantity:   3,
		OrderNo:    "bfflock1",
		Title:      "机械键盘",
		UnitPrice:  19900,
		PointsCost: 597,
	}
}

// newTestTCC 造一个装配齐备、只把 dtm 客户端换成桩的 TCC 编排器。
func newTestTCC(t *testing.T, mutate func(*TCCConfig)) *TCCOrchestrator {
	t.Helper()
	lastFakeTCC = nil
	cfg := TCCConfig{
		DTMAddr: "127.0.0.1:36790",
		BranchAddrs: map[string]string{
			DomainStock:   "127.0.0.1:9043",
			DomainOrder:   "127.0.0.1:9042",
			DomainProduct: "127.0.0.1:9041",
			DomainPoints:  "127.0.0.1:9044",
		},
		GenGID:      func(context.Context, string) (string, error) { return testGID, nil },
		Traceparent: func(context.Context) string { return testTraceparent },
		RunTCC: func(_ context.Context, _, _ string, fn func(TCC) error) error {
			lastFakeTCC = &fakeTCC{failAt: -1}
			return fn(lastFakeTCC)
		},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	o, err := NewTCC(cfg)
	if err != nil {
		t.Fatalf("构造 TCC 编排器：%v", err)
	}
	return o
}

// tccStepsOf 跑一次 buildSteps。
func tccStepsOf(t *testing.T, o *TCCOrchestrator, cmd LockCmd) []TCCStep {
	t.Helper()
	steps, err := o.buildSteps(context.Background(), cmd, testGID)
	if err != nil {
		t.Fatalf("构造分支：%v", err)
	}
	return steps
}

// ---------- Try 分支序 ----------

// TestTCCTryOrderIsStockPointsOrder 钉住 Try 分支序。
//
// 判据用**完整有序表**而不是「首是 TryFreeze、尾是 TryPlace」两条断言：后者对
// 中间一段是瞎的，而把 order 垫底这条恰恰是本链最值钱的设计（占位单只在库存
// 与积分都锁成之后才建，失败时只需回退两处冻结）。把 order 提到最前不只是
// 「顺序变了」，而是「每一笔失败都要先建一张单再废掉它」。
//
// 反向探针（实测过，见汇报）：把 tccBranchPlan 里 order 段挪到 stock 段之前，
// 本用例红，报 "第 0 个分支 = order/TryPlace，want stock/TryFreeze"。
func TestTCCTryOrderIsStockPointsOrder(t *testing.T) {
	steps := tccStepsOf(t, newTestTCC(t, nil), testLockCmd())
	want := [][2]string{
		{DomainStock, "TryFreeze"},
		{DomainPoints, "TryFreeze"},
		{DomainOrder, "TryPlace"},
	}
	if len(steps) != len(want) {
		t.Fatalf("分支数 = %d，want %d（少一条就是一个域不参与事务，多一条就是凭空多一次冻结）", len(steps), len(want))
	}
	for i, w := range want {
		gotDomain, gotMethod := splitURL(steps[i].Try)
		if gotDomain != w[0] || gotMethod != w[1] {
			t.Errorf("第 %d 个 Try 分支 = %s/%s，want %s/%s", i, gotDomain, gotMethod, w[0], w[1])
		}
	}
	// Methods 与 URL 的一致性：两者本该同源，分开断言是为了让「URL 对但
	// Methods 写错」也在测试期暴露（Methods 是响应体回显的唯一来源）。
	for i, s := range steps {
		if _, m := splitURL(s.Try); m != s.Methods.Try {
			t.Errorf("第 %d 个分支的 Methods.Try = %q，与 URL 里的方法名 %q 不一致", i, s.Methods.Try, m)
		}
	}
}

// TestTCCTryOrderHasNoProductBranch TCC 不碰商品域。
//
// 商品域只有 Saga 的 AddSales / ReduceSales（没有冻结语义），让它进 TCC 就是
// spec 明令禁止的「复用扣减接口充当冻结」。这条钉死的是「域集合」，不是序。
func TestTCCTryOrderHasNoProductBranch(t *testing.T) {
	steps := tccStepsOf(t, newTestTCC(t, nil), testLockCmd())
	for i, s := range steps {
		if s.Domain == DomainProduct {
			t.Errorf("第 %d 个分支是 product/%s —— 商品域没有冻结语义，不得进 TCC", i, s.Methods.Try)
		}
	}
	if got, want := strings.Join(domainsOf(steps), ","), "stock,points,order"; got != want {
		t.Errorf("TCC 的域集合 = %q，want %q", got, want)
	}
}

// TestTCCBranchMethodsDoNotOverlapSagaMethods 钉住两条链的方法名互不重叠。
//
// spec dtm-tcc-presale 要求「Saga 四分支与 TCC 三对分支方法名互不重叠、语义
// 互不借用」。这条守卫逐字比对两组常量：出现交集就是有人把 Saga 的扣减
// 方法当成了 TCC 的冻结方法 —— 那在真机上表现为「冻结走的是真扣减」，
// Cancel 解不回来。
func TestTCCBranchMethodsDoNotOverlapSagaMethods(t *testing.T) {
	sagaMethods := map[string]string{
		stockDeduct: "stock.Saga.Deduct", stockRelease: "stock.Saga.Release",
		orderCreateOrder: "order.Saga.CreateOrder", orderCancelOrder: "order.Saga.CancelOrder",
		productAddSales: "product.Saga.AddSales", productReduceSales: "product.Saga.ReduceSales",
		pointsDebit: "points.Saga.Debit", pointsRefund: "points.Saga.Refund",
	}
	for _, b := range tccBranchPlan() {
		for _, m := range []string{b.try, b.confirm, b.cancel} {
			if owner, dup := sagaMethods[m]; dup {
				t.Errorf("TCC 分支 %s.%s 与 Saga 的 %s 同名 —— 两条链的分支方法必须互不重叠", b.domain, m, owner)
			}
		}
	}
}

// ---------- 三段 URL ----------

// TestTCCEveryBranchFillsAllThreeURLs 钉住每个分支三段 URL 都非空且全名正确。
//
// 这是 TCC 相对 Saga 最容易漏的一处：Saga 只有 action / compensate 两个串，
// TCC 有三个，而 confirm / cancel 是**登记**给 dtm 的、提交时不拨 —— 少填一个
// 在提交时毫无异样，要等 dtm 走到二阶段回调到一个空 URL 才炸，而那已经是在
// 回滚资金的路上了。故此处对三个串都做「非空 + 逐字等于期望全名」的双判据。
//
// 反向探针（实测过，见汇报）：把 buildSteps 里 Confirm 那行删掉（改成空串），
// 本用例红，报 "第 0 个分支 confirm URL 为空"。
func TestTCCEveryBranchFillsAllThreeURLs(t *testing.T) {
	steps := tccStepsOf(t, newTestTCC(t, nil), testLockCmd())
	want := []struct {
		domain, addr, service   string
		tryM, confirmM, cancelM string
	}{
		{DomainStock, "127.0.0.1:9043", stockService, "TryFreeze", "ConfirmFreeze", "CancelFreeze"},
		{DomainPoints, "127.0.0.1:9044", pointsService, "TryFreeze", "ConfirmFreeze", "CancelFreeze"},
		{DomainOrder, "127.0.0.1:9042", orderService, "TryPlace", "ConfirmFreeze", "CancelFreeze"},
	}
	if len(steps) != len(want) {
		t.Fatalf("分支数 = %d，want %d", len(steps), len(want))
	}
	for i, w := range want {
		s := steps[i]
		if s.Domain != w.domain {
			t.Errorf("第 %d 个分支的域 = %q，want %q", i, s.Domain, w.domain)
		}
		for _, u := range []struct{ name, got, want string }{
			{"try", s.Try, w.addr + "/" + w.service + "/" + w.tryM},
			{"confirm", s.Confirm, w.addr + "/" + w.service + "/" + w.confirmM},
			{"cancel", s.Cancel, w.addr + "/" + w.service + "/" + w.cancelM},
		} {
			if u.got == "" {
				t.Errorf("第 %d 个分支（%s）的 %s URL 为空 —— dtm 会拿一个空地址去拨号，二阶段直接失败", i, w.domain, u.name)
				continue
			}
			if u.got != u.want {
				t.Errorf("第 %d 个分支（%s）的 %s URL = %q，want %q", i, w.domain, u.name, u.got, u.want)
			}
		}
		// 三段必须同域：跨域配对会让 dtm 拿库存载荷去调积分方法，
		// gRPC 直接 Unimplemented，而四域日志里都看不出是编排配错了。
		for _, u := range []string{s.Try, s.Confirm, s.Cancel} {
			if d := domainOfFullMethod(fullMethod(u)); d != w.domain {
				t.Errorf("第 %d 个分支的某段 URL 跨域：%s（want %s）", i, u, w.domain)
			}
		}
	}
}

// TestTCCBranchURLsCarryNoScheme 钉住三段 URL 都不带 scheme。
//
// 反向探针（实测过）：把注入的 BranchURL 改成 addr+"grpc://"+fullMethod，
// 本用例红。真机上则是 dtm server 每次回调都报 "call dtmdriver.Use() before
// you use custom scheme"，且只在 dtm server 日志里可见。
func TestTCCBranchURLsCarryNoScheme(t *testing.T) {
	steps := tccStepsOf(t, newTestTCC(t, nil), testLockCmd())
	for i, s := range steps {
		for name, u := range map[string]string{"try": s.Try, "confirm": s.Confirm, "cancel": s.Cancel} {
			if strings.Contains(u, "://") {
				t.Errorf("第 %d 个分支的 %s URL = %q，含 scheme：dtm 的默认 driver 会直接报错", i, name, u)
			}
			host, _, ok := strings.Cut(u, "/")
			if !ok {
				t.Fatalf("第 %d 个分支的 %s URL = %q，没有 '/'（dtm 按第一个 '/' 切分拨号目标与全方法名）", i, name, u)
			}
			if !strings.Contains(host, ":") {
				t.Errorf("第 %d 个分支的 %s URL 的拨号目标 = %q，缺 host:port", i, name, host)
			}
			// 分界处必须正好一个 '/'：gRPC 要求全方法名带**一个**前导斜杠。
			// 少一个就是 Unimplemented，多一个就是 Invoke 收到 "//dtmbranch…"
			// 同样 Unimplemented —— 而症状只在 dtm server 日志里可见。
			rest := strings.TrimPrefix(u, host)
			if !strings.HasPrefix(rest, "/") || strings.HasPrefix(rest, "//") {
				t.Errorf("第 %d 个分支的 %s URL = %q，拨号目标与全方法名之间必须正好一个 '/'", i, name, u)
			}
		}
	}
}

// TestTCCBranchMethodNamesMatchDomainFullMethodNames 把方法名对着三域 pb.go 核。
//
// 为什么要读源码而不自我声明：dtm server 只按 URL 回调，方法名错一个字符就是
// 分支 Unimplemented，而**本进程连日志都不会有** —— 「编排看起来提交成功了，
// 三个域一条写入都没有」是最难排查的一类症状。三份 busi_grpc.pb.go 才是权威。
//
// 顺带把 Saga 那条守卫也钉住「不重叠」：同一组常量被两条链共用时，只查
// 「存在」不够，还得查「本链取的确实是本域那一个」。
func TestTCCBranchMethodNamesMatchDomainFullMethodNames(t *testing.T) {
	cases := []struct {
		domain, srvDir, wantService string
		wantMethods                 []string
	}{
		{DomainStock, "srv-stock", stockService, []string{stockTryFreeze, stockConfirmFreeze, stockCancelFreeze}},
		{DomainPoints, "srv-points", pointsService, []string{pointsTryFreeze, pointsConfirmFreeze, pointsCancelFreeze}},
		{DomainOrder, "srv-order", orderService, []string{orderTryPlace, orderConfirmFreeze, orderCancelFreeze}},
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
	// order 的 Try 必须逐字是 TryPlace：写成 TryFreeze 会拼出一个不存在的全
	// 方法名（该服务的第一个 rpc 是 CreateOrder），症状是 Try 分支 Unimplemented。
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "srv-order", "internal", "dtm", "protos", "busi_grpc.pb.go"))
	if err != nil {
		t.Fatalf("读 srv-order pb.go：%v", err)
	}
	if hasFullMethodConst(string(src), orderService, "TryFreeze") {
		t.Errorf("srv-order 竟有 %s/TryFreeze —— 常量表与 busi.proto 已漂移，先修域再改编排", orderService)
	}
}

// TestTCCBranchRespEnvelopeMatchesDomainRespMessages 把 bff 的响应信封对着三域核。
//
// `CallBranch` 的 reply 是 grpc-go 要反序列化的目标：字段编号/类型不对不至于
// 报错（proto 会把不匹配的字段当 unknown 丢掉），但它会让这份代码对「回包里
// 到底有什么」失去依据。故逐字段核。
func TestTCCBranchRespEnvelopeMatchesDomainRespMessages(t *testing.T) {
	fields := (&bffbranch.BusiResp{}).ProtoReflect().Descriptor().Fields()
	ours := map[string]string{}
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		ours[string(f.Name())] = fieldKey(f)
	}
	want := map[string]string{"code": "1:int64", "message": "2:string"}
	if !reflect.DeepEqual(ours, want) {
		t.Fatalf("bff 响应信封字段 = %v，want %v", ours, want)
	}
	for _, srv := range []string{"srv-stock", "srv-order", "srv-points"} {
		t.Run(srv, func(t *testing.T) {
			path := filepath.Join("..", "..", "..", srv, "internal", "dtm", "protos", "busi.pb.go")
			got, err := respStructFieldKeys(path)
			if err != nil {
				t.Fatalf("%s：%v", path, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s 的响应消息字段 = %v，want %v", srv, got, want)
			}
		})
	}
}

// ---------- 载荷内容 ----------

// TestTCCEveryPayloadCarriesTraceparent 钉住三条分支载荷都带 traceparent。
//
// 反向探针（实测过）：把 buildSteps 里的 `Traceparent: tp` 删掉，本用例红。
// 少了它，正向链在 Jaeger 上断成「bff 一棵 + 三域各自一棵」，而每个服务都有
// span，最难看出是断链。
func TestTCCEveryPayloadCarriesTraceparent(t *testing.T) {
	steps := tccStepsOf(t, newTestTCC(t, nil), testLockCmd())
	for i, s := range steps {
		if s.Payload.GetTraceparent() != testTraceparent {
			t.Errorf("第 %d 个分支（%s）的载荷没有 traceparent：got %q，want %q",
				i, s.Methods.Try, s.Payload.GetTraceparent(), testTraceparent)
		}
	}
}

// TestTCCOrderBizCarriesGid 钉住 order 的 biz 带 dtm_gid。
//
// 这一条是 TCC 相对 Saga 最容易漏的：order 分支的 Confirm / Cancel 都按
// `order_no = ? and dtm_gid = ?` 定位占位单，dtm_gid 空则订单域
// validateDtmCmd 直接判业务失败（"dtm_gid required"），整条事务被 Cancel。
// 而它在提交时毫无异样 —— 占位单根本没建，看不出少了什么。
func TestTCCOrderBizCarriesGid(t *testing.T) {
	steps := tccStepsOf(t, newTestTCC(t, nil), testLockCmd())
	var order struct {
		DtmGID  string `json:"dtm_gid"`
		OrderNo string `json:"order_no"`
	}
	mustDecode(t, DomainOrder, stepByDomain(t, steps, DomainOrder).Payload.GetBiz(), &order)
	if order.DtmGID != testGID {
		t.Errorf("order biz 的 dtm_gid = %q，want %q（占位单与 Confirm/Cancel 都按它定位）", order.DtmGID, testGID)
	}
	if order.OrderNo != "bfflock1" {
		t.Errorf("order biz 的 order_no = %q，want bfflock1", order.OrderNo)
	}
}

// TestTCCBizBodiesMatchDomainDTOs 把三条业务体解回各域 DTO 的形状逐字段核对。
//
// 用镜像结构体而不是 map：map 只验键存在，验不出「键名对但类型错」—— quantity
// 编成字符串，JSON 解到 int32 会静默变 0，而 0 在库存域是「白写一条零流水」。
type (
	mirrorLockStock struct {
		OrderNo   string `json:"order_no"`
		ProductID uint64 `json:"product_id"`
		Quantity  int32  `json:"quantity"`
		UserID    uint64 `json:"user_id"`
	}
	mirrorLockPoints struct {
		UserID  int64  `json:"user_id"`
		Cost    int64  `json:"cost"`
		OrderNo string `json:"order_no"`
	}
	mirrorLockOrder struct {
		UserID      uint64       `json:"user_id"`
		OrderNo     string       `json:"order_no"`
		DtmGID      string       `json:"dtm_gid"`
		Items       []mirrorItem `json:"items"`
		Traceparent string       `json:"traceparent"`
	}
)

func TestTCCBizBodiesMatchDomainDTOs(t *testing.T) {
	steps := tccStepsOf(t, newTestTCC(t, nil), testLockCmd())

	var stock mirrorLockStock
	mustDecode(t, DomainStock, stepByDomain(t, steps, DomainStock).Payload.GetBiz(), &stock)
	if want := (mirrorLockStock{OrderNo: "bfflock1", ProductID: 7, Quantity: 3, UserID: 1001}); stock != want {
		t.Errorf("stock biz = %+v，want %+v", stock, want)
	}

	var points mirrorLockPoints
	mustDecode(t, DomainPoints, stepByDomain(t, steps, DomainPoints).Payload.GetBiz(), &points)
	if want := (mirrorLockPoints{UserID: 1001, Cost: 597, OrderNo: "bfflock1"}); points != want {
		t.Errorf("points biz = %+v，want %+v", points, want)
	}

	var order mirrorLockOrder
	mustDecode(t, DomainOrder, stepByDomain(t, steps, DomainOrder).Payload.GetBiz(), &order)
	if len(order.Items) != 1 {
		t.Fatalf("order biz 的 items 条数 = %d，want 1（空明细的订单总额恒为 0）", len(order.Items))
	}
	if want := (mirrorItem{ProductID: 7, Name: "机械键盘", Quantity: 3, UnitPrice: 19900}); order.Items[0] != want {
		t.Errorf("order biz 的明细 = %+v，want %+v", order.Items[0], want)
	}
	// traceparent 落在 biz 里而不只是 proto 字段：Confirm 写 outbox 时要靠它
	// 把 outbox 那一跳续回本条 trace（那一跳早已脱离本次 ctx）。
	if order.Traceparent != testTraceparent {
		t.Errorf("order biz 的 traceparent = %q（它要落 outbox.traceparent，供 outbox 补发续链）", order.Traceparent)
	}
}

// TestTCCOrderItemJSONKeysAreDomainFieldNames 把「明细键名 = 域结构体字段名」
// 做实：把编排侧产出的 JSON 解进**无 tag** 的镜像结构体（与 srv-order 同形）。
func TestTCCOrderItemJSONKeysAreDomainFieldNames(t *testing.T) {
	steps := tccStepsOf(t, newTestTCC(t, nil), testLockCmd())
	var raw struct {
		Items []map[string]any `json:"items"`
	}
	mustDecode(t, DomainOrder, stepByDomain(t, steps, DomainOrder).Payload.GetBiz(), &raw)
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

// TestTCCOrderItemKeysMatchDomainStruct 钉住明细键名取自 srv-order 的字段名。
//
// 这不是风格问题而是硬约束：srv-order 的 model.OrderItemCmd **没有 json tag**，
// encoding/json 只能按字段名（或其大小写不敏感变体）匹配，而
// EqualFold("product_id", "ProductID") 为 false（多一个下划线）。载荷一旦改成
// 下划线风格，分支侧解出来就是零值明细，订单域以 "invalid item" 判业务失败。
//
// 守卫方式：直接看那份结构体现在有没有 tag。有人给它补了 tag，本用例立刻红，
// 迫使决策「新 tag 是什么、载荷要不要跟着改」。
func TestTCCOrderItemKeysMatchDomainStruct(t *testing.T) {
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

// TestTCCFailInjectPassesThroughToEveryBranch 钉住 fail_inject 逐分支透传。
//
// fail_inject 靠**分支方法名**命中（各域分支面 `req.GetFailInject() == branch`），
// 所以它必须是各域自己的方法名（TryPlace，不是 order.TryPlace）。串错的症状
// 是「点了火但正向全绿」—— 演示脚本以为 Cancel 被点着了。
func TestTCCFailInjectPassesThroughToEveryBranch(t *testing.T) {
	cmd := testLockCmd()
	cmd.FailInject = "TryPlace"
	steps := tccStepsOf(t, newTestTCC(t, nil), cmd)
	for i, s := range steps {
		if s.Payload.GetFailInject() != "TryPlace" {
			t.Errorf("分支 %d（%s）的 fail_inject = %q，want TryPlace", i, s.Methods.Try, s.Payload.GetFailInject())
		}
	}
}

// TestTCCFailInjectEmptyOnNormalLock 真机路径：fail_inject 必须为空。
func TestTCCFailInjectEmptyOnNormalLock(t *testing.T) {
	steps := tccStepsOf(t, newTestTCC(t, nil), testLockCmd())
	for i, s := range steps {
		if s.Payload.GetFailInject() != "" {
			t.Errorf("分支 %d 的 fail_inject = %q，正常锁定必须是空", i, s.Payload.GetFailInject())
		}
	}
}

// ---------- 提交流程 ----------

// TestLockSendsExactlyThreeTryCallsAndNothingElse 端到端钉住「bff 只发 Try」。
//
// 这是本文件最重要的一条断言，它把 spec 的口径「三对分支里 bff 只负责发三个
// Try」变成可执行的判据：
//
//   - CallBranch 恰好三次（多一次 = 多一个分支 = 多一次冻结）；
//   - 三次的顺序 = tccBranchPlan 的序；
//   - 三次都填了完整的 try/confirm/cancel 三串；
//   - 三次的 payload 都是 BusiReq 且带 traceparent；
//   - reply 都是非 nil 的 *protos.BusiResp（nil 会让 grpc codec 直接报
//     "failed to unmarshal, message is <nil>"）。
//
// 「没有第二次提交」这一面还由 TestLockNeverIssuesASecondTransaction 覆盖
// （桩里除了这一次 run 之外没有任何出口）。
func TestLockSendsExactlyThreeTryCallsAndNothingElse(t *testing.T) {
	o := newTestTCC(t, nil)
	res, err := o.Lock(context.Background(), testLockCmd())
	if err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if res.GID != testGID {
		t.Errorf("返回 gid = %q，want %q", res.GID, testGID)
	}
	if len(res.Steps) != 3 {
		t.Fatalf("返回的分支数 = %d，want 3", len(res.Steps))
	}
	if lastFakeTCC == nil {
		t.Fatal("RunTCC 没被调用")
	}
	if len(lastFakeTCC.calls) != 3 {
		t.Fatalf("CallBranch 调用次数 = %d，want 3（每多一次就多一个分支要冻结）", len(lastFakeTCC.calls))
	}
	for i, c := range lastFakeTCC.calls {
		step := res.Steps[i]
		if c.tryURL != step.Try || c.confirm != step.Confirm || c.cancel != step.Cancel {
			t.Errorf("第 %d 次 CallBranch 的 URL 三元组 = (%q, %q, %q)，与返回的 Step 不一致", i, c.tryURL, c.confirm, c.cancel)
		}
		for name, u := range map[string]string{"try": c.tryURL, "confirm": c.confirm, "cancel": c.cancel} {
			if u == "" {
				t.Errorf("第 %d 次 CallBranch 的 %s URL 为空", i, name)
			}
		}
		req, ok := c.busiMsg.(*bffbranch.BusiReq)
		if !ok {
			t.Fatalf("第 %d 次 CallBranch 的载荷类型 = %T", i, c.busiMsg)
		}
		if req.GetTraceparent() == "" {
			t.Errorf("第 %d 次 CallBranch 的载荷没有 traceparent", i)
		}
		reply, ok := c.reply.(*bffbranch.BusiResp)
		if !ok || reply == nil {
			t.Errorf("第 %d 次 CallBranch 的 reply = %#v，want 非 nil 的 *protos.BusiResp", i, c.reply)
		}
	}
	var orderBiz struct {
		DtmGID string `json:"dtm_gid"`
	}
	mustDecode(t, DomainOrder, lastFakeTCC.calls[2].busiMsg.(*bffbranch.BusiReq).GetBiz(), &orderBiz)
	if orderBiz.DtmGID != testGID {
		t.Errorf("order 分支载荷的 dtm_gid = %q，want %q", orderBiz.DtmGID, testGID)
	}
}

// TestLockNeverIssuesASecondTransaction 编排侧只有一次全局事务提交。
//
// 判据：RunTCC 被调用且**只**被调用一次。二次提交同一条全局事务在 dtm 侧的
// 表现是分支表里出现重复的 branch_id、被 barrier 判重后静默跳过 —— 表面
// 全绿，实际有一处冻结没人退。
func TestLockNeverIssuesASecondTransaction(t *testing.T) {
	runs := 0
	o := newTestTCC(t, func(c *TCCConfig) {
		c.RunTCC = func(_ context.Context, _, _ string, fn func(TCC) error) error {
			runs++
			lastFakeTCC = &fakeTCC{failAt: -1}
			return fn(lastFakeTCC)
		}
	})
	if _, err := o.Lock(context.Background(), testLockCmd()); err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if runs != 1 {
		t.Errorf("全局事务提交次数 = %d，want 1（TCC 只发三个 Try，不存在第二阶段提交）", runs)
	}
}

// TestLockStopsAtFirstTryFailure 某个 Try 业务失败后不再发后面的 Try。
//
// 判据是「CallBranch 次数 < 分支数」：dtm 拿到 tccFunc 的非 nil 返回就 Abort，
// 由它逆序 Cancel 已成功的那些。此处多发一次的后果是「已经决定回滚了还冻了
// 第三个域」，那一处要靠 dtm 的二次 Cancel 才退得掉。
func TestLockStopsAtFirstTryFailure(t *testing.T) {
	boom := errors.New("branch: FAILURE 冻结失败")
	o := newTestTCC(t, nil)
	o.run = func(_ context.Context, _, _ string, fn func(TCC) error) error {
		lastFakeTCC = &fakeTCC{failAt: 1, failWith: boom}
		return fn(lastFakeTCC)
	}
	_, err := o.Lock(context.Background(), testLockCmd())
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v，want 含 %v", err, boom)
	}
	if lastFakeTCC == nil {
		t.Fatal("RunTCC 没被调用")
	}
	if len(lastFakeTCC.calls) != 2 {
		t.Fatalf("CallBranch 次数 = %d，want 2（第二个 Try 失败后必须停手）", len(lastFakeTCC.calls))
	}
}

// TestLockFailureWordingDistinguishesTransactionExistence 三档失败后果必须不同。
//
// 三种失败在外部观察上都是 5xx，但调用方的下一步动作完全不同：只有「事务没
// 建立」可以原样重试，另两种必须先等 dtm 把冻结退干净。编排器把它们编成错误
// 文案，handler 只能原样上抛 —— 所以文案就是唯一的判据，必须逐档钉死。
//
// 判据取文案里的三个关键词串（本包 fail() 写入的固定措辞）。
func TestLockFailureWordingDistinguishesTransactionExistence(t *testing.T) {
	prepareErr := errors.New("Prepare failed")
	submitErr := errors.New("Submit failed")
	tryErr := errors.New("branch: FAILURE 余额不足")

	t.Run("Prepare 失败 = 事务未建立，可原样重试", func(t *testing.T) {
		o := newTestTCC(t, func(c *TCCConfig) {
			c.RunTCC = func(context.Context, string, string, func(TCC) error) error { return prepareErr }
		})
		_, err := o.Lock(context.Background(), testLockCmd())
		assertTCCErr(t, err, []string{"全局事务未建立", "可原样重试"}, prepareErr)
	})
	t.Run("Try 失败 = 已 Abort，正在解冻", func(t *testing.T) {
		o := newTestTCC(t, nil)
		o.run = func(_ context.Context, _, _ string, fn func(TCC) error) error {
			lastFakeTCC = &fakeTCC{failAt: 0, failWith: tryErr}
			return fn(lastFakeTCC)
		}
		_, err := o.Lock(context.Background(), testLockCmd())
		assertTCCErr(t, err, []string{"Abort", "请勿立刻重试"}, tryErr)
	})
	t.Run("Submit 失败 = 三个 Try 已冻结，超时后才 Abort", func(t *testing.T) {
		o := newTestTCC(t, nil)
		o.run = func(_ context.Context, _, _ string, fn func(TCC) error) error {
			lastFakeTCC = &fakeTCC{failAt: -1}
			if err := fn(lastFakeTCC); err != nil {
				return err
			}
			return submitErr
		}
		_, err := o.Lock(context.Background(), testLockCmd())
		assertTCCErr(t, err, []string{"尚未收到 Submit", "超时后 Abort"}, submitErr)
	})
}

// TestLockRejectsInvalidCommandBeforeOpeningTransaction 命令非法不得建立事务。
//
// 判据是「RunTCC 一个都没被调用」：命令若在事务里失败，dtm 会逆序 Cancel
// 掉已经冻成功的分支，对调用方表现为「提交成功、然后什么都没了」。
func TestLockRejectsInvalidCommandBeforeOpeningTransaction(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(c *LockCmd)
	}{
		{"user_id 为 0", func(c *LockCmd) { c.UserID = 0 }},
		{"product_id 为 0", func(c *LockCmd) { c.ProductID = 0 }},
		{"quantity 为 0", func(c *LockCmd) { c.Quantity = 0 }},
		{"quantity 为负", func(c *LockCmd) { c.Quantity = -1 }},
		{"order_no 为空", func(c *LockCmd) { c.OrderNo = "" }},
		{"unit_price 为负", func(c *LockCmd) { c.UnitPrice = -1 }},
		{"points_cost 为 0", func(c *LockCmd) { c.PointsCost = 0 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := newTestTCC(t, nil)
			cmd := testLockCmd()
			c.mutate(&cmd)
			if _, err := o.Lock(context.Background(), cmd); err == nil {
				t.Fatal("命令非法却提交成功了")
			}
			if lastFakeTCC != nil {
				t.Errorf("命令非法却建了事务：CallBranch %d 次", len(lastFakeTCC.calls))
			}
		})
	}
}

// TestGIDFailureIsReportedBeforeLock gid 生成失败不得建事务。
//
// 覆盖的是「dtm 不可达」这条最常见的降级路径：gid 拿不到还往下走，order 的
// 载荷里 dtm_gid 就是空串，而空 dtm_gid 是订单域的业务失败 —— 一次配置故障
// 会被表现成「三个分支都报参数非法」。
func TestGIDFailureIsReportedBeforeLock(t *testing.T) {
	boom := errors.New("dtm server 不可达")
	o := newTestTCC(t, func(c *TCCConfig) {
		c.GenGID = func(context.Context, string) (string, error) { return "", boom }
	})
	if _, err := o.Lock(context.Background(), testLockCmd()); !errors.Is(err, boom) {
		t.Fatalf("错误 = %v，want %v", err, boom)
	}
	if lastFakeTCC != nil {
		t.Errorf("gid 失败却建了事务：CallBranch %d 次", len(lastFakeTCC.calls))
	}
}

// ---------- 配置与依赖 ----------

// TestNewTCCRejectsMissingConfig 配置缺失必须在装配期就说清缺哪一项。
func TestNewTCCRejectsMissingConfig(t *testing.T) {
	base := TCCConfig{
		DTMAddr: "127.0.0.1:36790",
		BranchAddrs: map[string]string{
			DomainStock: "127.0.0.1:9043", DomainOrder: "127.0.0.1:9042",
			DomainPoints: "127.0.0.1:9044",
		},
	}
	t.Run("缺 dtm server 地址", func(t *testing.T) {
		c := base
		c.DTMAddr = ""
		_, err := NewTCC(c)
		if err == nil || !strings.Contains(err.Error(), "xmiddleware.dtm.grpc_addr") {
			t.Fatalf("错误 = %v，want 含 xmiddleware.dtm.grpc_addr", err)
		}
	})
	t.Run("缺分支面地址", func(t *testing.T) {
		c := base
		c.BranchAddrs = map[string]string{DomainStock: "127.0.0.1:9043"}
		_, err := NewTCC(c)
		if err == nil {
			t.Fatal("缺两个域的分支地址却构造成功")
		}
		for _, d := range []string{DomainPoints, DomainOrder} {
			if !strings.Contains(err.Error(), d) {
				t.Errorf("错误文案 %q 没点名缺的 %s", err, d)
			}
		}
	})
	t.Run("缺 product 不影响 TCC", func(t *testing.T) {
		// 商品域不参与 TCC（没有冻结语义）。把它算进校验，会让一条与预售无关
		// 的配置缺口把整条锁单链挡在装配期之外。
		c := base
		c.BranchAddrs[DomainProduct] = ""
		if _, err := NewTCC(c); err != nil {
			t.Fatalf("只缺 product 却构造失败：%v", err)
		}
	})
	t.Run("配全了", func(t *testing.T) {
		if _, err := NewTCC(base); err != nil {
			t.Fatalf("配全却构造失败：%v", err)
		}
	})
}

// TestUnavailableTCCFailsLockButNotProcess strict.dtm=false 的失败姿态。
//
// 判据是「Lock 恒报错、且不 panic」。这里若 panic，一次配置缺项会变成
// 「每个请求都 500 且日志里是 runtime error」。
func TestUnavailableTCCFailsLockButNotProcess(t *testing.T) {
	o := UnavailableTCC(errors.New("xmiddleware.dtm.grpc_addr 未配置"))
	_, err := o.Lock(context.Background(), testLockCmd())
	if err == nil {
		t.Fatal("不可用编排器竟然提交成功")
	}
	if !strings.Contains(err.Error(), "xmiddleware.dtm.grpc_addr") {
		t.Errorf("错误 = %v，want 含缺失的配置键", err)
	}
	if lastFakeTCC != nil {
		t.Errorf("不可用编排器仍然建了事务")
	}
}

// TestServerGIDRecoversPanicForTCC TCC 复用 saga 的 ServerGID 也要 panic→error。
//
// TCC 与 Saga 共用同一个 gid 生成器（同一个 dtm server、同一套 recover 口径）。
// 这里单独钉一条是为了让「共用」这件事有测试兜底：将来有人给 Saga 换掉
// ServerGID，本用例会红而不是让 TCC 悄悄跟着 panic。
func TestServerGIDRecoversPanicForTCC(t *testing.T) {
	gid, err := ServerGID(context.Background(), "127.0.0.1:1")
	if err == nil {
		t.Skipf("该地址居然可达（拿到 gid=%q），无法验证 panic 路径", gid)
	}
	if !strings.Contains(err.Error(), "不可达") {
		t.Errorf("错误 = %v，want 含「不可达」", err)
	}
}

// TestDescribeTCCBranchOrderMatchesPlan 启动日志回显的序 = 真实编排序。
//
// 真机上「dtm 到底按什么次序回调」是最常被反复确认的一件事，故把它打进日志；
// 但回显一旦与 tccBranchPlan 漂移，它就成了误导。
func TestDescribeTCCBranchOrderMatchesPlan(t *testing.T) {
	want := "stock.TryFreeze/ConfirmFreeze/CancelFreeze → " +
		"points.TryFreeze/ConfirmFreeze/CancelFreeze → " +
		"order.TryPlace/ConfirmFreeze/CancelFreeze"
	if got := DescribeTCCBranchOrder(); got != want {
		t.Errorf("DescribeTCCBranchOrder() = %q，want %q", got, want)
	}
}

// ---------- helper ----------

// stepByDomain 按域键取分支。
func stepByDomain(t *testing.T, steps []TCCStep, domain string) TCCStep {
	t.Helper()
	for _, s := range steps {
		if s.Domain == domain {
			return s
		}
	}
	t.Fatalf("域 %s 没有分支", domain)
	return TCCStep{}
}

// domainsOf 分支序列的域键（按序）。
func domainsOf(steps []TCCStep) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.Domain)
	}
	return out
}

// assertTCCErr 断言错误文案含给定的关键词串，且底层错误透传。
func assertTCCErr(t *testing.T, err error, keywords []string, want error) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望出错却成功了")
	}
	for _, k := range keywords {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("错误文案 %q 缺关键词 %q（调用方靠它判断能不能重试）", err.Error(), k)
		}
	}
	if !errors.Is(err, want) {
		t.Errorf("错误 = %v，want 透传 %v", err, want)
	}
}

// respStructFieldKeys 从 pb.go 里抽出「*BusiResp」响应结构的 protobuf tag。
func respStructFieldKeys(path string) (map[string]string, error) {
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
			if !ok || !strings.HasSuffix(ts.Name.Name, "BusiResp") {
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
		return nil, fmt.Errorf("%s 里没解析出 *BusiResp 的 protobuf tag（生成物形态变了？）", path)
	}
	return out, nil
}
