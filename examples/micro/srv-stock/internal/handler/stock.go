// Package handler srv-stock 协议适配层。
//
// 在分层链上的位置：route → middleware → **handler** → service →
// repository → model。
//
// 这一层只做三件事，多一件都不做：
//  1. pb 请求 → 领域入参（model.*Cmd / *Query）；领域结果 → pb 响应
//  2. 开 handler.stock.<Method> span（链上的一节）
//  3. 把 service 抛上来的 codes.Error 翻译成响应体的 Code/Message
//
// 不做业务规则校验 —— 那是 service 的事。这里若也校验一遍，规则就会在两层
// 各长一份并逐渐漂移（"为什么调 0 件在这儿通过了、到 service 又被拒？"）。
//
// 与传输错误的分工：codes.Error（业务判定失败）进响应体；其余 error
// （超时/不可达/内部错误）直接抛回 kitex 走错误体，不翻译成业务码 ——
// 否则「下游挂了」会被误报成「库存不足」。
package handler

import (
	"context"

	"github.com/gospacex/launchx/examples/micro/pkg/codes"
	stockpb "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/stock"
	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/model"
	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/service"
	"github.com/gospacex/otelx/layer"
)

// StockHandler 库存域协议适配器
type StockHandler struct {
	svc     *service.StockService
	traceOn bool
}

// NewStockHandler 构造
func NewStockHandler(svc *service.StockService, traceOn bool) *StockHandler {
	return &StockHandler{svc: svc, traceOn: traceOn}
}

// begin 开 handler 层 span，返回「收 error 再收尾」的闭包。
//
// 不设豁免：handler 返回非 nil error 是传输层失败（超时 / 不可达 / 内部
// 错误，业务拒绝已翻进响应体 Code），标红正是运维要看的信号。
func (h *StockHandler) begin(ctx context.Context, method string) (context.Context, layer.End) {
	return layer.Begin(ctx, "handler", "stock", method, h.traceOn, nil)
}

// GetStock 查库存快照（联表聚合未释放预占量）
func (h *StockHandler) GetStock(ctx context.Context, req *stockpb.GetStockReq) (out *stockpb.GetStockResp, err error) {
	resp := &stockpb.GetStockResp{ProductId: req.GetProductId()}
	ctx, end := h.begin(ctx, "GetStock")
	defer func() { end(err) }()

	d, err := h.svc.GetStock(ctx, model.GetStockQuery{
		ProductID: uint64(req.GetProductId()),
	})
	if err != nil {
		if sErr := settle(&resp.Code, &resp.Message, err); sErr != nil {
			return nil, sErr
		}
		return resp, nil
	}
	resp.Stock = toPbStock(d)
	if sErr := settle(&resp.Code, &resp.Message, nil); sErr != nil {
		return nil, sErr
	}
	return resp, nil
}

// ListStocks 库存列表（联表聚合分页）
func (h *StockHandler) ListStocks(ctx context.Context, req *stockpb.ListStocksReq) (out *stockpb.ListStocksResp, err error) {
	resp := &stockpb.ListStocksResp{}
	ctx, end := h.begin(ctx, "ListStocks")
	defer func() { end(err) }()

	rows, err := h.svc.ListStocks(ctx, model.ListStocksQuery{
		Limit:  int(req.GetLimit()),
		Offset: int(req.GetOffset()),
	})
	if err != nil {
		if sErr := settle(&resp.Code, &resp.Message, err); sErr != nil {
			return nil, sErr
		}
		return resp, nil
	}
	resp.Stocks = toPbStocks(rows)
	if sErr := settle(&resp.Code, &resp.Message, nil); sErr != nil {
		return nil, sErr
	}
	return resp, nil
}

// ReserveStock 预占（加锁 + 3 表事务 + outbox）
func (h *StockHandler) ReserveStock(ctx context.Context, req *stockpb.ReserveStockReq) (out *stockpb.ReserveStockResp, err error) {
	resp := &stockpb.ReserveStockResp{ProductId: req.GetProductId()}
	ctx, end := h.begin(ctx, "ReserveStock")
	defer func() { end(err) }()

	rec, err := h.svc.ReserveStock(ctx, model.ReserveCmd{
		OrderNo:   req.GetOrderNo(),
		ProductID: uint64(req.GetProductId()),
		UserID:    uint64(req.GetUserId()),
		Quantity:  req.GetQuantity(),
	})
	if err != nil {
		if sErr := settle(&resp.Code, &resp.Message, err); sErr != nil {
			return nil, sErr
		}
		return resp, nil
	}
	resp.Reserved = rec.Reserved
	resp.ReservationId = int64(rec.ReservationID)
	if sErr := settle(&resp.Code, &resp.Message, nil); sErr != nil {
		return nil, sErr
	}
	return resp, nil
}

// ReleaseStock 释放预占（幂等）
func (h *StockHandler) ReleaseStock(ctx context.Context, req *stockpb.ReleaseStockReq) (out *stockpb.ReleaseStockResp, err error) {
	resp := &stockpb.ReleaseStockResp{ProductId: req.GetProductId()}
	ctx, end := h.begin(ctx, "ReleaseStock")
	defer func() { end(err) }()

	rec, err := h.svc.ReleaseStock(ctx, model.ReleaseCmd{
		OrderNo:   req.GetOrderNo(),
		ProductID: uint64(req.GetProductId()),
		Quantity:  req.GetQuantity(),
	})
	if err != nil {
		if sErr := settle(&resp.Code, &resp.Message, err); sErr != nil {
			return nil, sErr
		}
		return resp, nil
	}
	resp.Available = rec.Available
	if sErr := settle(&resp.Code, &resp.Message, nil); sErr != nil {
		return nil, sErr
	}
	return resp, nil
}

// ConfirmStock 确认预占（出库）
func (h *StockHandler) ConfirmStock(ctx context.Context, req *stockpb.ConfirmStockReq) (out *stockpb.ConfirmStockResp, err error) {
	resp := &stockpb.ConfirmStockResp{ProductId: req.GetProductId()}
	ctx, end := h.begin(ctx, "ConfirmStock")
	defer func() { end(err) }()

	rec, err := h.svc.ConfirmStock(ctx, model.ConfirmCmd{
		OrderNo:   req.GetOrderNo(),
		ProductID: uint64(req.GetProductId()),
		Quantity:  req.GetQuantity(),
	})
	if err != nil {
		if sErr := settle(&resp.Code, &resp.Message, err); sErr != nil {
			return nil, sErr
		}
		return resp, nil
	}
	resp.Total = rec.Total
	resp.Available = rec.Available
	if sErr := settle(&resp.Code, &resp.Message, nil); sErr != nil {
		return nil, sErr
	}
	return resp, nil
}

// AdjustStock 入库 / 盘点（2 表事务）
func (h *StockHandler) AdjustStock(ctx context.Context, req *stockpb.AdjustStockReq) (out *stockpb.AdjustStockResp, err error) {
	resp := &stockpb.AdjustStockResp{ProductId: req.GetProductId()}
	ctx, end := h.begin(ctx, "AdjustStock")
	defer func() { end(err) }()

	rec, err := h.svc.AdjustStock(ctx, model.AdjustCmd{
		ProductID:  uint64(req.GetProductId()),
		Delta:      req.GetDelta(),
		Reason:     req.GetReason(),
		OperatorID: uint64(req.GetOperatorId()),
	})
	if err != nil {
		if sErr := settle(&resp.Code, &resp.Message, err); sErr != nil {
			return nil, sErr
		}
		return resp, nil
	}
	resp.Total = rec.Total
	resp.Available = rec.Available
	if sErr := settle(&resp.Code, &resp.Message, nil); sErr != nil {
		return nil, sErr
	}
	return resp, nil
}

// Report 3 表 JOIN 报表
func (h *StockHandler) Report(ctx context.Context, req *stockpb.ReportReq) (out *stockpb.ReportResp, err error) {
	resp := &stockpb.ReportResp{}
	ctx, end := h.begin(ctx, "Report")
	defer func() { end(err) }()

	rows, err := h.svc.Report(ctx, model.StockReportQuery{
		Limit: int(req.GetLimit()),
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

// toPbStock 库存详情投影 → pb。
//
// pb 的 StockInfo 只承载主表口径（total/available/reserved/version）：
// 联表聚合出来的未释放预占量是本域的对账信号，没有对外字段，不往 pb 里塞
// （塞进去就要在协议里承诺它的语义，而它目前只服务于内部对账）。
func toPbStock(d *model.StockDetail) *stockpb.StockInfo {
	if d == nil {
		return nil
	}
	return &stockpb.StockInfo{
		ProductId: int64(d.ProductID),
		Total:     d.Total,
		Available: d.Available,
		Reserved:  d.Reserved,
		Version:   d.Version,
	}
}

func toPbStocks(rows []model.StockDetail) []*stockpb.StockInfo {
	out := make([]*stockpb.StockInfo, 0, len(rows))
	for i := range rows {
		out = append(out, toPbStock(&rows[i]))
	}
	return out
}

func toPbReportRows(rows []model.ReportRow) []*stockpb.StockReportRow {
	out := make([]*stockpb.StockReportRow, 0, len(rows))
	for i := range rows {
		out = append(out, &stockpb.StockReportRow{
			ProductId:        rows[i].ProductID,
			Total:            rows[i].Total,
			Available:        rows[i].Available,
			Reserved:         rows[i].Reserved,
			ReservationCount: rows[i].ReservationCount,
			LogCount:         rows[i].LogCount,
		})
	}
	return out
}
