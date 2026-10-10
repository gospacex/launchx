// Package handler 入参出参转换，零业务判断。
//
// 本层**不实现 IDL 接口**：生成的 product.Product 接口唯一的实现方是 `rpc/`
// —— 由那一层负责开 rpc span、走准入闸门，并持有编译期接口断言（断言放在 rpc
// 层，不放 handler，见该文件的 `var _` 行）。
//
// 这里的方法只做 RPC 结构 ↔ service 出入参的搬运：
//
//	GetProduct    —— 走 service（IDL 方法名 GetProduct，service 侧同名）
//	其余 5 个      —— 本示例未接线，显式返回 codes.Unimplemented（注明「非漏项」）
//
// **codes.Unimplemented 的唯一落点就是本层**：rpc/ 只负责开 span 与委派，
// 不产出任何业务码，避免「哪一层该回 501」出现两个真相。
//
// 结构体与 product.Product 接口都来自 `.../pkg/kitex_gen/product`（kitex 把 pb
// 结构体与服务接口生成在同一包）；只有 NewServer 在 `.../pkg/kitex_gen/product/product`。
package handler

import (
	"context"

	"github.com/gospacex/otelx/layer"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/product/service"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	productpb "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/product"
)

// productService 是 handler 依赖的窄接口：只有这一个已接线的业务方法。
//
// *service.ProductService 天然满足它。抽接口是为了让「只做转换」这件事能被
// 单独驱动到（见 product_test.go），而不是把 handler 绑死在 service 的具体类型上。
type productService interface {
	GetProduct(ctx context.Context, id uint64) (*service.ProductView, error)
}

// ProductHandler 入参出参转换。
type ProductHandler struct {
	svc     productService
	traceOn bool // trace.enabled（总闸），由装配层注入
}

// New 构造 handler。
func New(svc productService, traceOn bool) *ProductHandler {
	return &ProductHandler{svc: svc, traceOn: traceOn}
}

// GetProduct RPC 入口：查商品详情（本域唯一已接线的对外方法，只读）。
//
// 用生成的 Get* 取值而非直接读字段：kitex 传进来的 req 可能是 nil（客户端
// 空请求），getter 对 nil 安全，直接取字段会当场 panic。
//
// 结果列表命名，供 defer 里的 end(err) 覆盖所有 return 路径（Go 要求「全命名
// 或全不命名」）。traceOn=false 时 layer.Begin 返回空闭包，行为不变。
func (h *ProductHandler) GetProduct(ctx context.Context, req *productpb.GetProductReq) (resp *productpb.GetProductResp, err error) {
	ctx, end := layer.Begin(ctx, "handler", "product", "GetProduct", h.traceOn, nil)
	defer func() { end(err) }()

	v, err := h.svc.GetProduct(ctx, uint64(req.GetProductId()))
	if err != nil {
		return nil, err
	}
	// category_name / sku_count 是派生字段（join / 聚合的结果），主表里没有，
	// 故留空 —— 需要它们的列表 / 报表在本示例未接线（见各 Unimplemented 方法）。
	var created int64
	if !v.Product.CreatedAt.IsZero() {
		created = v.Product.CreatedAt.Unix()
	}
	return &productpb.GetProductResp{
		UserId: req.GetUserId(),
		Product: &productpb.ProductInfo{
			ProductId:  int64(v.Product.ID),
			Title:      v.Product.Title,
			Price:      v.Product.Price,
			Status:     v.Product.Status,
			CategoryId: int64(v.Product.CategoryID),
			CreatedAt:  created,
		},
	}, nil
}

// CreateProduct 未接线：商品录入本示例不提供（非漏项）。
func (h *ProductHandler) CreateProduct(_ context.Context, _ *productpb.CreateProductReq) (*productpb.CreateProductResp, error) {
	return nil, codes.New(codes.Unimplemented, "product.CreateProduct not wired in mesh apps demo")
}

// UpdateProduct 未接线：IDL 里有，但本示例的写路径只用于单测驱动「写后失效」
// （见 service.UpdatePrice），不对 RPC 暴露（非漏项）。
func (h *ProductHandler) UpdateProduct(_ context.Context, _ *productpb.UpdateProductReq) (*productpb.UpdateProductResp, error) {
	return nil, codes.New(codes.Unimplemented, "product.UpdateProduct not wired in mesh apps demo")
}

// UpdateStatus 未接线：上下架不在本示例范围（非漏项）。
func (h *ProductHandler) UpdateStatus(_ context.Context, _ *productpb.UpdateStatusReq) (*productpb.UpdateStatusResp, error) {
	return nil, codes.New(codes.Unimplemented, "product.UpdateStatus not wired in mesh apps demo")
}

// ListProducts 未接线：本示例不提供列表页（非漏项）。
func (h *ProductHandler) ListProducts(_ context.Context, _ *productpb.ListProductsReq) (*productpb.ListProductsResp, error) {
	return nil, codes.New(codes.Unimplemented, "product.ListProducts not wired in mesh apps demo")
}

// Report 未接线：本示例不提供报表（非漏项）。
func (h *ProductHandler) Report(_ context.Context, _ *productpb.ReportReq) (*productpb.ReportResp, error) {
	return nil, codes.New(codes.Unimplemented, "product.Report not wired in mesh apps demo")
}
