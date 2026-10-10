// Package handler 入参出参转换，零业务判断。
//
// 本层**不实现 IDL 接口**：生成的 points.Points 接口唯一的实现方是 `rpc/`
// —— 由那一层负责开 rpc span、走准入闸门，并持有编译期接口断言（断言放在 rpc
// 层，不放 handler，见该文件的 `var _` 行）。
//
// 这里的方法只做 RPC 结构 ↔ service 出入参的搬运：
//
//	GetAccount              —— 走 service（IDL 方法名 GetAccount，service 侧为 GetPoints）
//	Earn/Consume/ListLedgers/Report —— 本示例未接线，显式返回 codes.Unimplemented（注明「非漏项」）
//
// **codes.Unimplemented 的唯一落点就是本层**：rpc/ 只负责开 span 与委派，
// 不产出任何业务码，避免「哪一层该回 501」出现两个真相。
//
// 结构体与 points.Points 接口都来自 `.../pkg/kitex_gen/points`（kitex 把 pb
// 结构体与服务接口生成在同一包）；只有 NewServer 在 `.../pkg/kitex_gen/points/points`。
package handler

import (
	"context"

	"github.com/gospacex/otelx/layer"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/points/service"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	pointspb "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/points"
)

// pointsService 是 handler 依赖的窄接口：只有这一个已接线的业务方法。
//
// *service.PointsService 天然满足它。抽接口是为了让「只做转换」这件事能被
// 单独驱动到（见 points_test.go），而不是把 handler 绑死在 service 的具体类型上。
type pointsService interface {
	GetPoints(ctx context.Context, userID uint64) (*service.AccountView, error)
}

// PointsHandler 入参出参转换。
type PointsHandler struct {
	svc     pointsService
	traceOn bool // trace.enabled（总闸），由装配层注入
}

// New 构造 handler。
func New(svc pointsService, traceOn bool) *PointsHandler {
	return &PointsHandler{svc: svc, traceOn: traceOn}
}

// GetAccount RPC 入口：查积分账户。
//
// 用生成的 Get* 取值而非直接读字段：kitex 传进来的 req 可能是 nil（客户端
// 空请求），getter 对 nil 安全，直接取字段会当场 panic。
//
// 结果列表命名，供 defer 里的 end(err) 覆盖所有 return 路径（Go 要求「全命名
// 或全不命名」）。traceOn=false 时 layer.Begin 返回空闭包，行为不变。
func (h *PointsHandler) GetAccount(ctx context.Context, req *pointspb.GetAccountReq) (resp *pointspb.GetAccountResp, err error) {
	ctx, end := layer.Begin(ctx, "handler", "points", "GetAccount", h.traceOn, nil)
	defer func() { end(err) }()

	v, err := h.svc.GetPoints(ctx, uint64(req.GetUserId()))
	if err != nil {
		return nil, err
	}
	return &pointspb.GetAccountResp{
		UserId:  int64(v.UserID),
		Account: &pointspb.PointAccount{UserId: int64(v.UserID), Balance: v.Balance},
	}, nil
}

// Earn 未接线：积分入账在网格示例里只经 MQ 消费（ConsumeOrderCompleted）
// 触发，不对 RPC 暴露写入路径（非漏项）。
func (h *PointsHandler) Earn(_ context.Context, _ *pointspb.EarnReq) (*pointspb.EarnResp, error) {
	return nil, codes.New(codes.Unimplemented, "points.Earn not wired in mesh apps demo")
}

// Consume 未接线：同上，扣分路径同样只经内部逻辑（非漏项）。
func (h *PointsHandler) Consume(_ context.Context, _ *pointspb.ConsumeReq) (*pointspb.ConsumeResp, error) {
	return nil, codes.New(codes.Unimplemented, "points.Consume not wired in mesh apps demo")
}

// ListLedgers 未接线：本示例不提供流水列表页（非漏项）。
func (h *PointsHandler) ListLedgers(_ context.Context, _ *pointspb.ListLedgersReq) (*pointspb.ListLedgersResp, error) {
	return nil, codes.New(codes.Unimplemented, "points.ListLedgers not wired in mesh apps demo")
}

// Report 未接线：本示例不提供报表（非漏项）。
func (h *PointsHandler) Report(_ context.Context, _ *pointspb.ReportReq) (*pointspb.ReportResp, error) {
	return nil, codes.New(codes.Unimplemented, "points.Report not wired in mesh apps demo")
}
