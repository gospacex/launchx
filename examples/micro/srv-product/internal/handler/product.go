// Package handler srv-product 协议适配层。
//
// 在分层链上的位置：route → middleware → **handler** → service →
// repository → model。
//
// 这一层只做三件事，多一件都不做：
//  1. pb 请求 → 领域入参（model.*Cmd / *Query）；领域结果 → pb 响应
//  2. 开 handler.product.<Method> span（链上的一节）
//  3. 把 service 抛上来的 codes.Error 翻译成响应体的 Code/Message
//
// 不做业务规则校验 —— 那是 service 的事。这里若也校验一遍，规则就会在两层
// 各长一份并逐渐漂移（"为什么改标题在这儿通过了、到 service 又被拒？"）。
//
// 与传输错误的分工：codes.Error（业务判定失败）进响应体；其余 error
// （超时/不可达/内部错误）直接抛回 kitex 走错误体，不翻译成业务码 ——
// 否则「下游挂了」会被误报成「商品不存在」。
package handler

import (
	"context"

	"github.com/gospacex/launchx/examples/micro/pkg/codes"
	productpb "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/product"
	"github.com/gospacex/launchx/examples/micro/pkg/tracex"
	"github.com/gospacex/launchx/examples/micro/srv-product/internal/model"
	"github.com/gospacex/launchx/examples/micro/srv-product/internal/service"
)

// ProductHandler 商品域协议适配器
type ProductHandler struct {
	svc     *service.ProductService
	traceOn bool
}

// NewProductHandler 构造
func NewProductHandler(svc *service.ProductService, traceOn bool) *ProductHandler {
	return &ProductHandler{svc: svc, traceOn: traceOn}
}

// begin 开一个 handler 层 span（traceOn=false 时返回空收尾闭包）
func (h *ProductHandler) begin(ctx context.Context, method string) (context.Context, func()) {
	if !h.traceOn {
		return ctx, func() {}
	}
	c, span := tracex.Start(ctx, "handler", "product", method)
	return c, func() { span.End() }
}

// CreateProduct 建商品
func (h *ProductHandler) CreateProduct(ctx context.Context, req *productpb.CreateProductReq) (*productpb.CreateProductResp, error) {
	resp := &productpb.CreateProductResp{}
	ctx, end := h.begin(ctx, "CreateProduct")
	defer end()

	out, err := h.svc.CreateProduct(ctx, model.CreateProductCmd{
		Title:      req.GetTitle(),
		Price:      req.GetPrice(),
		CategoryID: uint64(req.GetCategoryId()),
		OperatorID: uint64(req.GetOperatorId()),
		InitStock:  int(req.GetInitStock()),
	})
	if err != nil {
		if sErr := settle(&resp.Code, &resp.Message, err); sErr != nil {
			return nil, sErr
		}
		return resp, nil
	}
	resp.ProductId = int64(out.ProductID)
	if sErr := settle(&resp.Code, &resp.Message, nil); sErr != nil {
		return nil, sErr
	}
	return resp, nil
}

// GetProduct 查商品详情
func (h *ProductHandler) GetProduct(ctx context.Context, req *productpb.GetProductReq) (*productpb.GetProductResp, error) {
	resp := &productpb.GetProductResp{UserId: req.GetUserId()}
	ctx, end := h.begin(ctx, "GetProduct")
	defer end()

	d, err := h.svc.GetProduct(ctx, model.GetProductQuery{
		ProductID: uint64(req.GetProductId()),
		UserID:    uint64(req.GetUserId()),
	})
	if err != nil {
		if sErr := settle(&resp.Code, &resp.Message, err); sErr != nil {
			return nil, sErr
		}
		return resp, nil
	}
	// GetProductResp 没有独立的 product_id 字段：商品 ID 由内嵌的 Product 承载，
	// 调用方统一从 resp.Product.ProductId 读，避免两处字段不一致时对不上账。
	resp.Product = toPbProduct(d)
	if sErr := settle(&resp.Code, &resp.Message, nil); sErr != nil {
		return nil, sErr
	}
	return resp, nil
}

// UpdateProduct 改商品
func (h *ProductHandler) UpdateProduct(ctx context.Context, req *productpb.UpdateProductReq) (*productpb.UpdateProductResp, error) {
	resp := &productpb.UpdateProductResp{ProductId: req.GetProductId()}
	ctx, end := h.begin(ctx, "UpdateProduct")
	defer end()

	err := h.svc.UpdateProduct(ctx, model.UpdateProductCmd{
		ProductID:  uint64(req.GetProductId()),
		Title:      req.GetTitle(),
		Price:      req.GetPrice(),
		CategoryID: uint64(req.GetCategoryId()),
		OperatorID: uint64(req.GetOperatorId()),
	})
	if sErr := settle(&resp.Code, &resp.Message, err); sErr != nil {
		return nil, sErr
	}
	return resp, nil
}

// UpdateStatus 上下架
func (h *ProductHandler) UpdateStatus(ctx context.Context, req *productpb.UpdateStatusReq) (*productpb.UpdateStatusResp, error) {
	resp := &productpb.UpdateStatusResp{ProductId: req.GetProductId(), Status: req.GetStatus()}
	ctx, end := h.begin(ctx, "UpdateStatus")
	defer end()

	_, err := h.svc.UpdateStatus(ctx, model.UpdateStatusCmd{
		ProductID:  uint64(req.GetProductId()),
		Status:     req.GetStatus(),
		OperatorID: uint64(req.GetOperatorId()),
	})
	if sErr := settle(&resp.Code, &resp.Message, err); sErr != nil {
		return nil, sErr
	}
	return resp, nil
}

// ListProducts 商品列表
func (h *ProductHandler) ListProducts(ctx context.Context, req *productpb.ListProductsReq) (*productpb.ListProductsResp, error) {
	resp := &productpb.ListProductsResp{UserId: req.GetUserId()}
	ctx, end := h.begin(ctx, "ListProducts")
	defer end()

	rows, err := h.svc.ListProducts(ctx, model.ListProductsQuery{
		CategoryID: uint64(req.GetCategoryId()),
		Status:     req.GetStatus(),
		Limit:      int(req.GetLimit()),
		Offset:     int(req.GetOffset()),
	})
	if err != nil {
		if sErr := settle(&resp.Code, &resp.Message, err); sErr != nil {
			return nil, sErr
		}
		return resp, nil
	}
	resp.Products = toPbProducts(rows)
	if sErr := settle(&resp.Code, &resp.Message, nil); sErr != nil {
		return nil, sErr
	}
	return resp, nil
}

// Report 商品报表（4 表 JOIN）
func (h *ProductHandler) Report(ctx context.Context, req *productpb.ReportReq) (*productpb.ReportResp, error) {
	resp := &productpb.ReportResp{UserId: req.GetUserId()}
	ctx, end := h.begin(ctx, "Report")
	defer end()

	rows, err := h.svc.Report(ctx, model.ProductReportQuery{
		CategoryID: uint64(req.GetCategoryId()),
		Limit:      int(req.GetLimit()),
	})
	if err != nil {
		if sErr := settle(&resp.Code, &resp.Message, err); sErr != nil {
			return nil, sErr
		}
		return resp, nil
	}
	resp.Rows = toPbReportRows(rows)
	if sErr := settle(&resp.Code, &resp.Message, nil); sErr != nil {
		return nil, sErr
	}
	return resp, nil
}

// ---- 内部 ----

// settle 统一收尾：把业务错误翻译成响应码，非业务错误上抛给传输层。
//
// 返回 nil 表示"已写进响应体，按成功返回"；返回非 nil 表示"这不是业务问题，
// 交给 kitex 走错误体"。两类错误不混用，否则运维按错误码排查会对不上账。
func settle(code *int64, message *string, err error) error {
	if err == nil {
		ok := codes.OK()
		*code, *message = ok.Code, ok.Message
		return nil
	}
	if be, ok := codes.As(err); ok {
		*code, *message = be.Code, be.Message
		return nil
	}
	return err
}

func toPbProduct(d *model.ProductDetail) *productpb.ProductInfo {
	if d == nil {
		return nil
	}
	return &productpb.ProductInfo{
		ProductId:    int64(d.ID),
		Title:        d.Title,
		Price:        d.Price,
		Status:       d.Status,
		CategoryId:   int64(d.CategoryID),
		CategoryName: d.CategoryName,
		SkuCount:     d.SkuCount,
		CreatedAt:    d.CreatedAt.Unix(),
	}
}

func toPbProducts(rows []model.ProductDetail) []*productpb.ProductInfo {
	out := make([]*productpb.ProductInfo, 0, len(rows))
	for i := range rows {
		out = append(out, toPbProduct(&rows[i]))
	}
	return out
}

func toPbReportRows(rows []model.ReportRow) []*productpb.ProductReportRow {
	out := make([]*productpb.ProductReportRow, 0, len(rows))
	for i := range rows {
		out = append(out, &productpb.ProductReportRow{
			ProductId:    rows[i].ProductID,
			Title:        rows[i].Title,
			CategoryName: rows[i].CategoryName,
			Price:        rows[i].Price,
			SkuCount:     rows[i].SkuCount,
			OnSaleSkus:   rows[i].OnSaleSkus,
			SoldCount:    rows[i].SoldCount,
		})
	}
	return out
}
