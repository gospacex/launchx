// Package handler srv-order 协议适配层。
//
// 在分层链上的位置：route → middleware → **handler** → service →
// repository → model。
//
// 这一层只做三件事，多一件都不做：
//  1. pb 请求 → 领域入参（model.*Cmd / *Query）；领域结果 → pb 响应
//  2. 开 handler.order.<Method> span（链上的一节）
//  3. 把 service 抛上来的 codes.Error 翻译成响应体的 Code/Message
//
// 不做业务规则校验 —— 那是 service 的事。这里若也校验一遍，规则就会在两层
// 各长一份并逐渐漂移（"为什么这笔下单在这儿通过了、到 service 又被拒？"）。
//
// 与传输错误的分工：codes.Error（业务判定失败）进响应体；其余 error
// （超时/不可达/内部错误）直接抛回 kitex 走错误体，不翻译成业务码 ——
// 否则「下游挂了」会被误报成「订单不存在」。
package handler

import (
	"context"

	"github.com/gospacex/launchx/examples/mall/pkg/codes"
	orderpb "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen/order"
	"github.com/gospacex/launchx/examples/mall/srv-order/internal/model"
	"github.com/gospacex/launchx/examples/mall/srv-order/internal/service"
	"github.com/gospacex/otelx/layer"
)

// OrderHandler 订单域协议适配器
type OrderHandler struct {
	svc     *service.OrderService
	traceOn bool
}

// NewOrderHandler 构造
func NewOrderHandler(svc *service.OrderService, traceOn bool) *OrderHandler {
	return &OrderHandler{svc: svc, traceOn: traceOn}
}

// begin 开 handler 层 span，返回「收 error 再收尾」的闭包。
//
// 不设豁免：handler 返回非 nil error 是传输层失败（超时 / 不可达 / 内部
// 错误，业务拒绝已翻进响应体 Code），标红正是运维要看的信号。
func (h *OrderHandler) begin(ctx context.Context, method string) (context.Context, layer.End) {
	return layer.Begin(ctx, "handler", "order", method, h.traceOn, nil)
}

// CreateOrder 下单（3 表事务 + outbox 事件）
func (h *OrderHandler) CreateOrder(ctx context.Context, req *orderpb.CreateOrderReq) (out *orderpb.CreateOrderResp, err error) {
	resp := &orderpb.CreateOrderResp{UserId: req.GetUserId()}
	ctx, end := h.begin(ctx, "CreateOrder")
	defer func() { end(err) }()

	rec, err := h.svc.CreateOrder(ctx, model.CreateOrderCmd{
		UserID: uint64(req.GetUserId()),
		Items:  toModelItems(req.GetItems()),
	})
	if err != nil {
		if sErr := settle(&resp.Code, &resp.Message, err); sErr != nil {
			return nil, sErr
		}
		return resp, nil
	}
	resp.OrderId = int64(rec.OrderID)
	resp.OrderNo = rec.OrderNo
	resp.TotalAmount = rec.TotalAmount
	if sErr := settle(&resp.Code, &resp.Message, nil); sErr != nil {
		return nil, sErr
	}
	return resp, nil
}

// GetOrder 查单个订单 + 明细
func (h *OrderHandler) GetOrder(ctx context.Context, req *orderpb.GetOrderReq) (out *orderpb.GetOrderResp, err error) {
	resp := &orderpb.GetOrderResp{UserId: req.GetUserId(), OrderId: req.GetOrderId()}
	ctx, end := h.begin(ctx, "GetOrder")
	defer func() { end(err) }()

	ow, err := h.svc.GetOrder(ctx, model.GetOrderQuery{
		OrderID: uint64(req.GetOrderId()),
		UserID:  uint64(req.GetUserId()),
	})
	if err != nil {
		if sErr := settle(&resp.Code, &resp.Message, err); sErr != nil {
			return nil, sErr
		}
		return resp, nil
	}
	resp.OrderNo = ow.Order.OrderNo
	resp.Status = ow.Order.Status
	resp.TotalAmount = ow.Order.TotalAmount
	resp.PaidAmount = ow.Order.PaidAmount
	resp.Items = toPbItems(ow.Items)
	if sErr := settle(&resp.Code, &resp.Message, nil); sErr != nil {
		return nil, sErr
	}
	return resp, nil
}

// ListOrders 只读订单流水（orders ⋈ order_items 聚合）
func (h *OrderHandler) ListOrders(ctx context.Context, req *orderpb.ListOrdersReq) (out *orderpb.ListOrdersResp, err error) {
	resp := &orderpb.ListOrdersResp{UserId: req.GetUserId()}
	ctx, end := h.begin(ctx, "ListOrders")
	defer func() { end(err) }()

	rows, err := h.svc.ListOrders(ctx, model.ListOrdersQuery{
		UserID:   uint64(req.GetUserId()),
		FromDate: req.GetFromDate(),
		ToDate:   req.GetToDate(),
		Limit:    int(req.GetLimit()),
	})
	if err != nil {
		if sErr := settle(&resp.Code, &resp.Message, err); sErr != nil {
			return nil, sErr
		}
		return resp, nil
	}
	// pb 的 OrderFlow 只承载主表六列：JOIN 出来的明细聚合列（件数/明细合计）
	// 是给 service 对账用的，暂不上协议面，故不映射。
	resp.Flows = toPbFlows(rows)
	if sErr := settle(&resp.Code, &resp.Message, nil); sErr != nil {
		return nil, sErr
	}
	return resp, nil
}

// Report 4 表 JOIN 报表
func (h *OrderHandler) Report(ctx context.Context, req *orderpb.ReportReq) (out *orderpb.ReportResp, err error) {
	resp := &orderpb.ReportResp{UserId: req.GetUserId()}
	ctx, end := h.begin(ctx, "Report")
	defer func() { end(err) }()

	rows, err := h.svc.Report(ctx, model.OrderReportQuery{
		UserID:   uint64(req.GetUserId()),
		FromDate: req.GetFromDate(),
		ToDate:   req.GetToDate(),
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

func toModelItems(items []*orderpb.OrderItem) []model.OrderItemCmd {
	out := make([]model.OrderItemCmd, 0, len(items))
	for _, it := range items {
		out = append(out, model.OrderItemCmd{
			ProductID: uint64(it.GetProductId()),
			Name:      it.GetName(),
			Quantity:  int(it.GetQuantity()),
			UnitPrice: it.GetUnitPrice(),
		})
	}
	return out
}

func toPbItems(items []model.OrderItem) []*orderpb.OrderItem {
	out := make([]*orderpb.OrderItem, 0, len(items))
	for i := range items {
		out = append(out, &orderpb.OrderItem{
			ProductId: int64(items[i].ProductID),
			Name:      items[i].ProductName,
			Quantity:  int32(items[i].Quantity),
			UnitPrice: items[i].UnitPrice,
		})
	}
	return out
}

func toPbFlows(rows []model.OrderFlowRow) []*orderpb.OrderFlow {
	out := make([]*orderpb.OrderFlow, 0, len(rows))
	for i := range rows {
		out = append(out, &orderpb.OrderFlow{
			Id:          int64(rows[i].ID),
			UserId:      int64(rows[i].UserID),
			OrderNo:     rows[i].OrderNo,
			Status:      rows[i].Status,
			TotalAmount: rows[i].TotalAmount,
			CreatedAt:   rows[i].CreatedAt.Unix(),
		})
	}
	return out
}

func toPbReportRows(rows []model.ReportRow) []*orderpb.ReportRow {
	out := make([]*orderpb.ReportRow, 0, len(rows))
	for i := range rows {
		out = append(out, &orderpb.ReportRow{
			OrderNo:           rows[i].OrderNo,
			Status:            rows[i].Status,
			TotalAmount:       rows[i].TotalAmount,
			RefundAmount:      rows[i].RefundAmount,
			ItemCount:         rows[i].ItemCount,
			StatusChangeCount: rows[i].StatusChangeCount,
		})
	}
	return out
}
