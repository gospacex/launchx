package handler

import (
	"context"
	"errors"
	"testing"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/stock/model"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/stock/service"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	stockpb "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/stock"
)

// fakeSvc 结构性满足 handler 依赖的（未导出）窄接口。
type fakeSvc struct {
	view  *service.StockView
	err   error
	calls int
	gotID uint64
}

func (f *fakeSvc) GetStock(_ context.Context, productID uint64) (*service.StockView, error) {
	f.calls++
	f.gotID = productID
	if f.err != nil {
		return nil, f.err
	}
	return f.view, nil
}

func newHandler(svc *fakeSvc, traceOn bool) *StockHandler { return New(svc, traceOn) }

// GetStock：view → pb 的映射。StockInfo 字段集与 model 列一一对应，无派生字段。
func TestGetStockMapsViewToResp(t *testing.T) {
	svc := &fakeSvc{view: &service.StockView{Stock: model.Stock{
		ProductID: 5, Total: 100, Available: 42, Reserved: 8, Version: 3,
	}}}
	h := newHandler(svc, false)

	resp, err := h.GetStock(context.Background(), &stockpb.GetStockReq{ProductId: 5})
	if err != nil {
		t.Fatalf("GetStock: %v", err)
	}
	if svc.gotID != 5 {
		t.Fatalf("svc.gotID = %d, want 5", svc.gotID)
	}
	if resp.ProductId != 5 || resp.Stock == nil {
		t.Fatalf("resp = %+v", resp)
	}
	s := resp.Stock
	if s.ProductId != 5 || s.Total != 100 || s.Available != 42 || s.Reserved != 8 || s.Version != 3 {
		t.Fatalf("stock = %+v", s)
	}
}

// 零值请求（ProductId=0）不 panic，原样下传（由 service 决定语义，回 InvalidArgument）。
func TestGetStockZeroIDIsForwarded(t *testing.T) {
	svc := &fakeSvc{view: &service.StockView{}}
	h := newHandler(svc, false)
	if _, err := h.GetStock(context.Background(), &stockpb.GetStockReq{}); err != nil {
		t.Fatalf("GetStock: %v", err)
	}
	if svc.gotID != 0 || svc.calls != 1 {
		t.Fatalf("零值请求应原样下传：gotID=%d calls=%d", svc.gotID, svc.calls)
	}
}

// service 报错：原样上抛（不返回半成品响应）。
func TestGetStockPropagatesServiceError(t *testing.T) {
	h := newHandler(&fakeSvc{err: errors.New("db down")}, false)
	resp, err := h.GetStock(context.Background(), &stockpb.GetStockReq{ProductId: 5})
	if err == nil {
		t.Fatal("service 报错应上抛")
	}
	if resp != nil {
		t.Fatalf("出错不应返回响应体，实际 %+v", resp)
	}
}

// 未接线方法：显式 Unimplemented（不是 panic，也不是空 resp,nil）——
// **Unimplemented 的唯一落点是 handler**，rpc 层只开 span 后委派过来。
//
// 判据取具体类型指针为空（respNil）而非装进 any 判 nil：typed nil != nil。
func TestUnwiredMethodsReturnUnimplemented(t *testing.T) {
	h := newHandler(&fakeSvc{}, false)
	cases := []struct {
		name string
		call func() (respNil bool, err error)
	}{
		{"ListStocks", func() (bool, error) {
			resp, err := h.ListStocks(context.Background(), &stockpb.ListStocksReq{Limit: 10})
			return resp == nil, err
		}},
		{"ReserveStock", func() (bool, error) {
			resp, err := h.ReserveStock(context.Background(), &stockpb.ReserveStockReq{ProductId: 1, Quantity: 1})
			return resp == nil, err
		}},
		{"ReleaseStock", func() (bool, error) {
			resp, err := h.ReleaseStock(context.Background(), &stockpb.ReleaseStockReq{ProductId: 1, Quantity: 1})
			return resp == nil, err
		}},
		{"ConfirmStock", func() (bool, error) {
			resp, err := h.ConfirmStock(context.Background(), &stockpb.ConfirmStockReq{ProductId: 1, Quantity: 1})
			return resp == nil, err
		}},
		{"AdjustStock", func() (bool, error) {
			resp, err := h.AdjustStock(context.Background(), &stockpb.AdjustStockReq{OperatorId: 1, ProductId: 1, Delta: 5})
			return resp == nil, err
		}},
		{"Report", func() (bool, error) {
			resp, err := h.Report(context.Background(), &stockpb.ReportReq{Limit: 10})
			return resp == nil, err
		}},
	}
	for _, tc := range cases {
		respNil, err := tc.call()
		if err == nil {
			t.Fatalf("%s 必须显式报错，不得空实现", tc.name)
		}
		if !respNil {
			t.Fatalf("%s 不应返回响应体", tc.name)
		}
		if c, _ := codes.From(err); c != codes.Unimplemented {
			t.Fatalf("%s 应回 Unimplemented，实际 %v (err=%v)", tc.name, c, err)
		}
	}
}

// Unimplemented 文案里带上方法名，便于排障一眼看出是哪个方法没接线。
func TestUnwiredMethodsMessageNamesMethod(t *testing.T) {
	h := newHandler(&fakeSvc{}, false)
	_, err := h.ReserveStock(context.Background(), &stockpb.ReserveStockReq{})
	if err == nil || !contains(err.Error(), "stock.ReserveStock") {
		t.Fatalf("Unimplemented 文案应含方法名，实际 %v", err)
	}
}

func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// 埋点开关不改变行为。
func TestGetStockTraceFlagDoesNotChangeBehavior(t *testing.T) {
	for _, traceOn := range []bool{true, false} {
		svc := &fakeSvc{view: &service.StockView{Stock: model.Stock{ProductID: 5, Available: 7}}}
		h := newHandler(svc, traceOn)
		resp, err := h.GetStock(context.Background(), &stockpb.GetStockReq{ProductId: 5})
		if err != nil || resp.Stock == nil || resp.Stock.Available != 7 {
			t.Fatalf("traceOn=%v 行为异常: resp=%+v err=%v", traceOn, resp, err)
		}
	}
}
