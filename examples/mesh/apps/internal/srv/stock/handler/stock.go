// Package handler 入参出参转换，零业务判断。
//
// 本层**不实现 IDL 接口**：生成的 stock.Stock 接口唯一的实现方是 `rpc/`
// —— 由那一层负责开 rpc span、走准入闸门，并持有编译期接口断言（断言放在 rpc
// 层，不放 handler，见该文件的 `var _` 行）。
//
// 这里的方法只做 RPC 结构 ↔ service 出入参的搬运：
//
//	GetStock                                    —— 走 service（已接线，只读）
//	ListStocks/ReserveStock/ReleaseStock/
//	ConfirmStock/AdjustStock/Report             —— 未接线，显式 Unimplemented
//
// **codes.Unimplemented 的唯一落点就是本层**：rpc/ 只负责开 span 与委派，
// 不产出任何业务码，避免「哪一层该回 501」出现两个真相。
//
// 结构体与 stock.Stock 接口都来自 `.../pkg/kitex_gen/stock`；只有 NewServer 在
// `.../pkg/kitex_gen/stock/stock`。
package handler

import (
	"context"

	"github.com/gospacex/otelx/layer"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/stock/service"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	stockpb "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/stock"
)

// stockService 是 handler 依赖的窄接口：只有这一个已接线的业务方法。
//
// *service.StockService 天然满足它。抽接口是为了让「只做转换」这件事能被
// 单独驱动到（见 stock_test.go），而不是把 handler 绑死在 service 的具体类型上。
type stockService interface {
	GetStock(ctx context.Context, productID uint64) (*service.StockView, error)
}

// StockHandler 入参出参转换。
type StockHandler struct {
	svc     stockService
	traceOn bool // trace.enabled（总闸），由装配层注入
}

// New 构造 handler。
func New(svc stockService, traceOn bool) *StockHandler {
	return &StockHandler{svc: svc, traceOn: traceOn}
}

// GetStock RPC 入口：查库存（本域唯一已接线的对外方法，只读）。
//
// 用生成的 Get* 取值而非直接读字段：kitex 传进来的 req 可能是 nil（客户端
// 空请求），getter 对 nil 安全，直接取字段会当场 panic。
//
// 结果列表命名，供 defer 里的 end(err) 覆盖所有 return 路径（Go 要求「全命名
// 或全不命名」）。traceOn=false 时 layer.Begin 返回空闭包，行为不变。
func (h *StockHandler) GetStock(ctx context.Context, req *stockpb.GetStockReq) (resp *stockpb.GetStockResp, err error) {
	ctx, end := layer.Begin(ctx, "handler", "stock", "GetStock", h.traceOn, nil)
	defer func() { end(err) }()

	v, err := h.svc.GetStock(ctx, uint64(req.GetProductId()))
	if err != nil {
		return nil, err
	}
	// StockInfo 字段集与 model.Stock 列一一对应，无派生字段可留空。
	return &stockpb.GetStockResp{
		ProductId: int64(v.Stock.ProductID),
		Stock: &stockpb.StockInfo{
			ProductId: int64(v.Stock.ProductID),
			Total:     v.Stock.Total,
			Available: v.Stock.Available,
			Reserved:  v.Stock.Reserved,
			Version:   v.Stock.Version,
		},
	}, nil
}

// ListStocks 未接线：本示例不提供库存列表页（非漏项）。
func (h *StockHandler) ListStocks(_ context.Context, _ *stockpb.ListStocksReq) (*stockpb.ListStocksResp, error) {
	return nil, codes.New(codes.Unimplemented, "stock.ListStocks not wired in mesh apps demo")
}

// ReserveStock 未接线：预留语义（order 完成后扣减）本示例不提供（非漏项）。
// 扣减路径 service.Decrease 存在，但只由单测驱动，不经 RPC 面暴露。
func (h *StockHandler) ReserveStock(_ context.Context, _ *stockpb.ReserveStockReq) (*stockpb.ReserveStockResp, error) {
	return nil, codes.New(codes.Unimplemented, "stock.ReserveStock not wired in mesh apps demo")
}

// ReleaseStock 未接线：释放预留不在本示例范围（非漏项）。
func (h *StockHandler) ReleaseStock(_ context.Context, _ *stockpb.ReleaseStockReq) (*stockpb.ReleaseStockResp, error) {
	return nil, codes.New(codes.Unimplemented, "stock.ReleaseStock not wired in mesh apps demo")
}

// ConfirmStock 未接线：确认扣减不在本示例范围（非漏项）。
func (h *StockHandler) ConfirmStock(_ context.Context, _ *stockpb.ConfirmStockReq) (*stockpb.ConfirmStockResp, error) {
	return nil, codes.New(codes.Unimplemented, "stock.ConfirmStock not wired in mesh apps demo")
}

// AdjustStock 未接线：盘点调整不在本示例范围（非漏项）。
func (h *StockHandler) AdjustStock(_ context.Context, _ *stockpb.AdjustStockReq) (*stockpb.AdjustStockResp, error) {
	return nil, codes.New(codes.Unimplemented, "stock.AdjustStock not wired in mesh apps demo")
}

// Report 未接线：本示例不提供库存报表（非漏项）。
func (h *StockHandler) Report(_ context.Context, _ *stockpb.ReportReq) (*stockpb.ReportResp, error) {
	return nil, codes.New(codes.Unimplemented, "stock.Report not wired in mesh apps demo")
}
