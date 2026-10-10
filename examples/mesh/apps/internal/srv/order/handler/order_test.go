package handler

import (
	"context"
	"errors"
	"testing"

	"github.com/gospacex/logx"
	"gorm.io/gorm"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/model"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/service"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	orderpb "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/order"
)

// nopLogger 内嵌接口 + 只覆写 Warn：service 在这些路径上只用 Warn。
type nopLogger struct{ logx.Logger }

func (nopLogger) Warn(string, ...logx.Field) {}

// fakeRepo 结构性满足 service 依赖的（未导出）仓储窄接口。
type fakeRepo struct {
	created    *model.Order
	createdIts []model.OrderItem
	items      []model.OrderItem
	findErr    error
}

func (f *fakeRepo) Create(_ context.Context, o *model.Order, items []model.OrderItem) error {
	o.ID = 777
	f.created, f.createdIts = o, items
	return nil
}
func (f *fakeRepo) FindByID(context.Context, uint64) (*model.Order, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	if f.created == nil {
		return &model.Order{ID: 1, OrderNo: "M-1", Status: model.StatusCreated}, nil
	}
	return f.created, nil
}
func (f *fakeRepo) ItemsByOrderID(context.Context, uint64) ([]model.OrderItem, error) {
	return f.items, nil
}
func (f *fakeRepo) UpdateStatus(context.Context, uint64, string, string) error { return nil }

func newHandler(repo *fakeRepo, traceOn bool) *OrderHandler {
	return New(service.New(repo, nil, nil, nil, nopLogger{}, traceOn), traceOn)
}

// CreateOrder：RPC 结构 → service 入参的转换（多明细 + 总额由 service 计算）。
func TestCreateOrderMapsReqToServiceInput(t *testing.T) {
	repo := &fakeRepo{}
	h := newHandler(repo, false)

	resp, err := h.CreateOrder(context.Background(), &orderpb.CreateOrderReq{
		UserId: 9,
		Items: []*orderpb.OrderItem{
			{ProductId: 11, Name: "a", Quantity: 2, UnitPrice: 100},
			{ProductId: 12, Name: "b", Quantity: 1, UnitPrice: 50},
		},
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if len(repo.createdIts) != 2 {
		t.Fatalf("明细转换丢失: %d", len(repo.createdIts))
	}
	if repo.createdIts[0].ProductID != 11 || repo.createdIts[0].ProductName != "a" ||
		repo.createdIts[0].Quantity != 2 || repo.createdIts[0].UnitPrice != 100 {
		t.Fatalf("item[0] = %+v", repo.createdIts[0])
	}
	if repo.created.TotalAmount != 2*100+1*50 {
		t.Fatalf("total = %d, want 250", repo.created.TotalAmount)
	}
	if resp.UserId != 9 || resp.OrderId != 777 || resp.TotalAmount != 250 || resp.OrderNo == "" {
		t.Fatalf("resp = %+v", resp)
	}
}

// GetOrder：model → RPC 响应，含明细与状态的映射。
func TestGetOrderMapsViewToResp(t *testing.T) {
	repo := &fakeRepo{
		created: &model.Order{ID: 5, UserID: 3, OrderNo: "M-5", Status: model.StatusPaid,
			TotalAmount: 300, PaidAmount: 300},
		items: []model.OrderItem{{OrderID: 5, ProductID: 11, ProductName: "a", Quantity: 2, UnitPrice: 150}},
	}
	h := newHandler(repo, false)

	resp, err := h.GetOrder(context.Background(), &orderpb.GetOrderReq{UserId: 3, OrderId: 5})
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if resp.OrderId != 5 || resp.OrderNo != "M-5" || resp.Status != "paid" ||
		resp.TotalAmount != 300 || resp.PaidAmount != 300 {
		t.Fatalf("resp = %+v", resp)
	}
	if len(resp.Items) != 1 || resp.Items[0].ProductId != 11 ||
		resp.Items[0].Quantity != 2 || resp.Items[0].Name != "a" {
		t.Fatalf("items = %+v", resp.Items)
	}
}

// CreateOrder 空请求（nil）→ 零明细 → service 回 InvalidArgument，不 panic。
func TestCreateOrderNilReqIsInvalidArgument(t *testing.T) {
	h := newHandler(&fakeRepo{}, false)
	resp, err := h.CreateOrder(context.Background(), nil)
	if resp != nil {
		t.Fatalf("出错时不应返回响应体，实际 %+v", resp)
	}
	if c, _ := codes.From(err); c != codes.InvalidArgument {
		t.Fatalf("nil 请求应回 InvalidArgument，实际 %v (err=%v)", c, err)
	}
}

// CreateOrder 落库成功但回读失败 → 错误上抛（不返回半成品响应）。
func TestCreateOrderPropagatesReadBackFailure(t *testing.T) {
	repo := &fakeRepo{findErr: errors.New("read back failed")}
	h := newHandler(repo, false)
	resp, err := h.CreateOrder(context.Background(), &orderpb.CreateOrderReq{
		UserId: 1, Items: []*orderpb.OrderItem{{ProductId: 1, Name: "a", Quantity: 1, UnitPrice: 1}},
	})
	if err == nil {
		t.Fatal("回读失败应报错")
	}
	if resp != nil {
		t.Fatalf("回读失败不应返回响应体，实际 %+v", resp)
	}
	if c, _ := codes.From(err); c != codes.Internal {
		t.Fatalf("回读失败应回 Internal，实际 %v (err=%v)", c, err)
	}
}

// GetOrder 零值请求（OrderId=0）→ 下游 ErrRecordNotFound → OrderNotFound。
func TestGetOrderZeroIDIsNotFound(t *testing.T) {
	h := newHandler(&fakeRepo{findErr: gorm.ErrRecordNotFound}, false)
	_, err := h.GetOrder(context.Background(), &orderpb.GetOrderReq{})
	if c, _ := codes.From(err); c != codes.OrderNotFound {
		t.Fatalf("零值请求应回 OrderNotFound，实际 %v (err=%v)", c, err)
	}
}

// ListOrders 未接线：显式 Unimplemented（不是 panic，也不是空 resp,nil）。
func TestListOrdersReturnsUnimplemented(t *testing.T) {
	h := newHandler(&fakeRepo{}, false)
	resp, err := h.ListOrders(context.Background(), &orderpb.ListOrdersReq{UserId: 1})
	if err == nil {
		t.Fatal("ListOrders 必须显式报错，不得空实现")
	}
	if resp != nil {
		t.Fatalf("ListOrders 不应返回响应体，实际 %+v", resp)
	}
	if c, _ := codes.From(err); c != codes.Unimplemented {
		t.Fatalf("ListOrders 应回 Unimplemented，实际 %v (err=%v)", c, err)
	}
}

// Report 未接线：同上。
func TestReportReturnsUnimplemented(t *testing.T) {
	h := newHandler(&fakeRepo{}, false)
	resp, err := h.Report(context.Background(), &orderpb.ReportReq{UserId: 1})
	if err == nil {
		t.Fatal("Report 必须显式报错，不得空实现")
	}
	if resp != nil {
		t.Fatalf("Report 不应返回响应体，实际 %+v", resp)
	}
	if c, _ := codes.From(err); c != codes.Unimplemented {
		t.Fatalf("Report 应回 Unimplemented，实际 %v (err=%v)", c, err)
	}
}

// 埋点开关不改变行为：traceOn=true / false 的转换结果必须一致。
func TestCreateOrderTraceFlagDoesNotChangeBehavior(t *testing.T) {
	for _, traceOn := range []bool{true, false} {
		repo := &fakeRepo{}
		h := newHandler(repo, traceOn)
		resp, err := h.CreateOrder(context.Background(), &orderpb.CreateOrderReq{
			UserId: 9,
			Items:  []*orderpb.OrderItem{{ProductId: 11, Name: "a", Quantity: 2, UnitPrice: 100}},
		})
		if err != nil {
			t.Fatalf("traceOn=%v CreateOrder: %v", traceOn, err)
		}
		if resp == nil || resp.OrderId != 777 || resp.TotalAmount != 200 {
			t.Fatalf("traceOn=%v 行为不一致：%+v", traceOn, resp)
		}
		if repo.created == nil || repo.created.TotalAmount != 200 {
			t.Fatalf("traceOn=%v 业务未走通", traceOn)
		}
	}
}
