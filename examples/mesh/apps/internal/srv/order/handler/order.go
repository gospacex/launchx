// Package handler 入参出参转换，零业务判断。
//
// 本层**不实现 IDL 接口**：生成的 order.Order 接口唯一的实现方是 `rpc/`
// —— 由那一层负责开 rpc span、走准入闸门，并持有编译期接口断言（断言放在 rpc
// 层，不放 handler，见该文件的 `var _` 行）。
//
// 这里的方法只做 RPC 结构 ↔ service 出入参的搬运：
//
//	CreateOrder / GetOrder —— 走 service，已接线，入口路由见 mesh-entry-chain
//	ListOrders / Report    —— 本示例未接线，显式返回 codes.Unimplemented（注明「非漏项」）
//
// 结构体与 order.Order 接口都来自 `.../pkg/kitex_gen/order`（kitex 把 pb 结构体
// 与服务接口生成在同一包）；只有 NewServer 在 `.../pkg/kitex_gen/order/order`。
package handler

import (
	"context"

	"github.com/gospacex/otelx/layer"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/service"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	orderpb "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/order"
)

// orderService 是 handler 依赖的窄接口：只有两个已接线的业务方法。
//
// *service.OrderService 天然满足它。抽接口是为了让「只做转换」这件事能被
// 单独驱动到（见 order_test.go），而不是把 handler 绑死在 service 的具体类型上。
type orderService interface {
	CreateOrder(ctx context.Context, in service.CreateOrderInput) (uint64, error)
	GetOrder(ctx context.Context, id uint64) (*service.OrderView, error)
}

// OrderHandler 入参出参转换。
type OrderHandler struct {
	svc     orderService
	traceOn bool // trace.enabled（总闸），由装配层注入
}

// New 构造 handler。
func New(svc orderService, traceOn bool) *OrderHandler {
	return &OrderHandler{svc: svc, traceOn: traceOn}
}

// CreateOrder RPC 入口。这是 `POST /order` 的下游，也是 `order → MQ → points`
// 异步链的起点（service 层在落库后调 publish）。
//
// 用生成的 Get* 取值而非直接读字段：kitex 传进来的 req 可能是 nil（客户端
// 空请求），getter 对 nil 安全，直接取字段会当场 panic。
//
// 结果列表命名，供 defer 里的 end(err) 覆盖所有 return 路径（Go 要求「全命名
// 或全不命名」）。traceOn=false 时 layer.Begin 返回空闭包，行为不变。
func (h *OrderHandler) CreateOrder(ctx context.Context, req *orderpb.CreateOrderReq) (resp *orderpb.CreateOrderResp, err error) {
	ctx, end := layer.Begin(ctx, "handler", "order", "CreateOrder", h.traceOn, nil)
	defer func() { end(err) }()

	in := service.CreateOrderInput{UserID: uint64(req.GetUserId())}
	for _, it := range req.GetItems() {
		in.Items = append(in.Items, service.CreateOrderItem{
			ProductID:   uint64(it.GetProductId()),
			ProductName: it.GetName(),
			Quantity:    int(it.GetQuantity()),
			UnitPrice:   it.GetUnitPrice(),
		})
	}
	id, cerr := h.svc.CreateOrder(ctx, in)
	if cerr != nil {
		return nil, cerr
	}
	v, verr := h.svc.GetOrder(ctx, id)
	if verr != nil {
		return nil, verr
	}
	return &orderpb.CreateOrderResp{
		UserId: int64(v.Order.UserID), OrderId: int64(v.Order.ID),
		OrderNo: v.Order.OrderNo, TotalAmount: v.Order.TotalAmount,
	}, nil
}

// GetOrder RPC 入口。
func (h *OrderHandler) GetOrder(ctx context.Context, req *orderpb.GetOrderReq) (resp *orderpb.GetOrderResp, err error) {
	ctx, end := layer.Begin(ctx, "handler", "order", "GetOrder", h.traceOn, nil)
	defer func() { end(err) }()

	v, err := h.svc.GetOrder(ctx, uint64(req.GetOrderId()))
	if err != nil {
		return nil, err
	}
	items := make([]*orderpb.OrderItem, 0, len(v.Items))
	for _, it := range v.Items {
		items = append(items, &orderpb.OrderItem{
			ProductId: int64(it.ProductID), Name: it.ProductName,
			Quantity: int32(it.Quantity), UnitPrice: it.UnitPrice,
		})
	}
	return &orderpb.GetOrderResp{
		UserId: int64(v.Order.UserID), OrderId: int64(v.Order.ID), OrderNo: v.Order.OrderNo,
		Status: v.Order.Status, TotalAmount: v.Order.TotalAmount, PaidAmount: v.Order.PaidAmount,
		Items: items,
	}, nil
}

// ListOrders 未接线：本示例不提供列表页（非漏项）。显式声明，不留 panic / 空实现。
func (h *OrderHandler) ListOrders(_ context.Context, _ *orderpb.ListOrdersReq) (*orderpb.ListOrdersResp, error) {
	return nil, codes.New(codes.Unimplemented, "order.ListOrders not wired in mesh apps demo")
}

// Report 未接线：同上（非漏项）。
func (h *OrderHandler) Report(_ context.Context, _ *orderpb.ReportReq) (*orderpb.ReportResp, error) {
	return nil, codes.New(codes.Unimplemented, "order.Report not wired in mesh apps demo")
}
