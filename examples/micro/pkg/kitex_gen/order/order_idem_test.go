// 幂等键（CreateOrderReq.OrderNo）的序列化往返判据。
//
// 为什么单独钉这一条：OrderNo 是**手工追加**到生成码里的字段（本仓没有
// .proto/.thrift 源，见 CreateOrderReq 的注释）。prutal 按 struct tag 反射编解码，
// 字段号写错 / 类型写错不会编译报错，只会**静默丢字段** —— 表现为 bff 明明传了
// 号、订单域收到空串，然后各自生成一个新号，重试 = 重复下单。这正是本仓库最
// 难查的那类故障（两端代码看着都对），所以必须有可执行判据。
package order

import "testing"

// TestCreateOrderReqOrderNoRoundTrip 幂等键必须完整过一次编解码。
//
// 反向探针：把 CreateOrderReq.OrderNo 的 tag 字段号改成 4（与 Code 冲突之外的
// 任意错号）或删掉 tag，本用例立刻红。
func TestCreateOrderReqOrderNoRoundTrip(t *testing.T) {
	req := &CreateOrderReq{
		UserId:  7,
		OrderNo: "O-20261011123456-abcd1234",
		Items:   []*OrderItem{{ProductId: 1, Name: "p1", Quantity: 2, UnitPrice: 100}},
	}
	buf, err := req.Marshal(nil)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got CreateOrderReq
	if err := got.Unmarshal(buf); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.GetOrderNo() != req.GetOrderNo() {
		t.Fatalf("幂等键在编解码中丢失：got %q, want %q", got.GetOrderNo(), req.GetOrderNo())
	}
	if got.GetUserId() != 7 || len(got.GetItems()) != 1 {
		t.Fatalf("其它字段也被破坏了：user=%d items=%d", got.GetUserId(), len(got.GetItems()))
	}
}

// TestCreateOrderReqEmptyOrderNo 没带幂等键时必须是空串，而不是零值冒充的号。
// 订单域靠"空 = 没带"来决定要不要回落成自己生成 —— 收到一个假号比收到空串更糟。
func TestCreateOrderReqEmptyOrderNo(t *testing.T) {
	buf, err := (&CreateOrderReq{UserId: 7}).Marshal(nil)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got CreateOrderReq
	if err := got.Unmarshal(buf); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.GetOrderNo() != "" {
		t.Fatalf("没带幂等键时应为空串，got %q", got.GetOrderNo())
	}
}

// TestCreateOrderReqNilSafe getter 必须 nil 安全：kitex 反序列化失败或调用方
// 传 nil 时，`req.GetOrderNo()` 不得 panic（handler 里就是裸调的）。
func TestCreateOrderReqNilSafe(t *testing.T) {
	var req *CreateOrderReq
	if req.GetOrderNo() != "" {
		t.Fatal("nil 请求的 GetOrderNo 应返回空串")
	}
}
