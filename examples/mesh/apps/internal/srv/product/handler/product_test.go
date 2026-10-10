package handler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/product/model"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/product/service"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	productpb "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/product"
)

// fakeSvc 结构性满足 handler 依赖的（未导出）窄接口。
type fakeSvc struct {
	view  *service.ProductView
	err   error
	calls int
	gotID uint64
}

func (f *fakeSvc) GetProduct(_ context.Context, id uint64) (*service.ProductView, error) {
	f.calls++
	f.gotID = id
	if f.err != nil {
		return nil, f.err
	}
	return f.view, nil
}

func newHandler(svc *fakeSvc, traceOn bool) *ProductHandler { return New(svc, traceOn) }

// GetProduct：view → pb 的映射（含派生字段留空）。
func TestGetProductMapsViewToResp(t *testing.T) {
	created := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	svc := &fakeSvc{view: &service.ProductView{Product: model.Product{
		ID: 5, Title: "demo", Price: 1999, Status: 1, CategoryID: 7, CreatedAt: created,
	}}}
	h := newHandler(svc, false)

	resp, err := h.GetProduct(context.Background(), &productpb.GetProductReq{UserId: 9, ProductId: 5})
	if err != nil {
		t.Fatalf("GetProduct: %v", err)
	}
	if svc.gotID != 5 {
		t.Fatalf("svc.gotID = %d, want 5", svc.gotID)
	}
	if resp.UserId != 9 || resp.Product == nil {
		t.Fatalf("resp = %+v", resp)
	}
	p := resp.Product
	if p.ProductId != 5 || p.Title != "demo" || p.Price != 1999 || p.Status != 1 || p.CategoryId != 7 {
		t.Fatalf("product = %+v", p)
	}
	if p.CreatedAt != created.Unix() {
		t.Fatalf("created_at = %d, want %d", p.CreatedAt, created.Unix())
	}
	// 派生字段主表没有，应为零值（不编造）。
	if p.CategoryName != "" || p.SkuCount != 0 {
		t.Fatalf("派生字段应留空：name=%q sku=%d", p.CategoryName, p.SkuCount)
	}
}

// 零值请求（ProductId=0）不 panic，原样下传（由 service 决定语义）。
func TestGetProductZeroIDIsForwarded(t *testing.T) {
	svc := &fakeSvc{view: &service.ProductView{Product: model.Product{ID: 0}}}
	h := newHandler(svc, false)
	if _, err := h.GetProduct(context.Background(), &productpb.GetProductReq{}); err != nil {
		t.Fatalf("GetProduct: %v", err)
	}
	if svc.gotID != 0 || svc.calls != 1 {
		t.Fatalf("零值请求应原样下传：gotID=%d calls=%d", svc.gotID, svc.calls)
	}
}

// 零值时间戳（CreatedAt 为零）不得输出 -62135596800 这种怪值。
func TestGetProductZeroCreatedAt(t *testing.T) {
	svc := &fakeSvc{view: &service.ProductView{Product: model.Product{ID: 1, Title: "p"}}}
	h := newHandler(svc, false)
	resp, err := h.GetProduct(context.Background(), &productpb.GetProductReq{ProductId: 1})
	if err != nil {
		t.Fatalf("GetProduct: %v", err)
	}
	if resp.Product.CreatedAt != 0 {
		t.Fatalf("零值 CreatedAt 应输出 0，实际 %d", resp.Product.CreatedAt)
	}
}

// service 报错：原样上抛（不返回半成品响应）。
func TestGetProductPropagatesServiceError(t *testing.T) {
	h := newHandler(&fakeSvc{err: errors.New("db down")}, false)
	resp, err := h.GetProduct(context.Background(), &productpb.GetProductReq{ProductId: 5})
	if err == nil {
		t.Fatal("service 报错应上抛")
	}
	if resp != nil {
		t.Fatalf("出错不应返回响应体，实际 %+v", resp)
	}
}

// 未接线方法：显式 Unimplemented（不是 panic，也不是空 resp,nil）——
// **Unimplemented 的唯一落点是 handler**。
//
// 判据是 respNil（具体类型的指针为空）而不是把 resp 装进 `any` 判 nil：
// 装进 any 的 typed nil 不等于 nil，会把「返回了类型化空指针」误判成有响应体。
func TestUnwiredMethodsReturnUnimplemented(t *testing.T) {
	h := newHandler(&fakeSvc{}, false)
	cases := []struct {
		name string
		call func() (respNil bool, err error)
	}{
		{"CreateProduct", func() (bool, error) {
			resp, err := h.CreateProduct(context.Background(), &productpb.CreateProductReq{OperatorId: 1})
			return resp == nil, err
		}},
		{"UpdateProduct", func() (bool, error) {
			resp, err := h.UpdateProduct(context.Background(), &productpb.UpdateProductReq{OperatorId: 1})
			return resp == nil, err
		}},
		{"UpdateStatus", func() (bool, error) {
			resp, err := h.UpdateStatus(context.Background(), &productpb.UpdateStatusReq{OperatorId: 1})
			return resp == nil, err
		}},
		{"ListProducts", func() (bool, error) {
			resp, err := h.ListProducts(context.Background(), &productpb.ListProductsReq{UserId: 1})
			return resp == nil, err
		}},
		{"Report", func() (bool, error) {
			resp, err := h.Report(context.Background(), &productpb.ReportReq{UserId: 1})
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

// 埋点开关不改变行为。
func TestGetProductTraceFlagDoesNotChangeBehavior(t *testing.T) {
	for _, traceOn := range []bool{true, false} {
		svc := &fakeSvc{view: &service.ProductView{Product: model.Product{ID: 5, Title: "p", Price: 7}}}
		h := newHandler(svc, traceOn)
		resp, err := h.GetProduct(context.Background(), &productpb.GetProductReq{ProductId: 5})
		if err != nil || resp.Product == nil || resp.Product.Price != 7 {
			t.Fatalf("traceOn=%v 行为异常: resp=%+v err=%v", traceOn, resp, err)
		}
	}
}
