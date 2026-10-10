// Package handler srv-points 协议适配层。
//
// 在分层链上的位置：route → middleware → **handler** → service →
// repository → model。
//
// 这一层只做三件事，多一件都不做：
//  1. pb 请求 → 领域入参（model.*Cmd / *Query）；领域结果 → pb 响应
//  2. 开 handler.points.<Method> span（链上的一节）
//  3. 把 service 抛上来的 codes.Error 翻译成响应体的 Code/Message
//
// 不做业务规则校验 —— 那是 service 的事。这里若也校验一遍，规则就会在两层
// 各长一份并逐渐漂移（"为什么消费 0 分在这儿通过了、到 service 又被拒？"）。
//
// 与传输错误的分工：codes.Error（业务判定失败）进响应体；其余 error
// （超时/不可达/内部错误）直接抛回 kitex 走错误体，不翻译成业务码 ——
// 否则「下游挂了」会被误报成「余额不足」。
package handler

import (
	"context"

	"github.com/gospacex/launchx/examples/mall/pkg/codes"
	pointspb "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen/points"
	"github.com/gospacex/launchx/examples/mall/srv-points/internal/model"
	"github.com/gospacex/launchx/examples/mall/srv-points/internal/service"
	"github.com/gospacex/otelx/layer"
)

// PointsHandler 积分域协议适配器
type PointsHandler struct {
	svc     *service.PointsService
	traceOn bool
}

// NewPointsHandler 构造
func NewPointsHandler(svc *service.PointsService, traceOn bool) *PointsHandler {
	return &PointsHandler{svc: svc, traceOn: traceOn}
}

// begin 开 handler 层 span，返回「收 error 再收尾」的闭包。
//
// 不设豁免：handler 返回非 nil error 是传输层失败（超时 / 不可达 / 内部
// 错误，业务拒绝已翻进响应体 Code），标红正是运维要看的信号。
func (h *PointsHandler) begin(ctx context.Context, method string) (context.Context, layer.End) {
	return layer.Begin(ctx, "handler", "points", method, h.traceOn, nil)
}

// GetAccount 查积分账户（point_accounts ⋈ point_ledgers 联表聚合）
func (h *PointsHandler) GetAccount(ctx context.Context, req *pointspb.GetAccountReq) (out *pointspb.GetAccountResp, err error) {
	resp := &pointspb.GetAccountResp{UserId: req.GetUserId()}
	ctx, end := h.begin(ctx, "GetAccount")
	defer func() { end(err) }()

	a, err := h.svc.GetAccount(ctx, model.GetAccountQuery{
		UserID: uint64(req.GetUserId()),
	})
	if err != nil {
		if sErr := settle(&resp.Code, &resp.Message, err); sErr != nil {
			return nil, sErr
		}
		return resp, nil
	}
	resp.Account = toPbAccount(a)
	if sErr := settle(&resp.Code, &resp.Message, nil); sErr != nil {
		return nil, sErr
	}
	return resp, nil
}

// Earn 入账（两层幂等 + 用户锁 + 2 表事务 + outbox）
func (h *PointsHandler) Earn(ctx context.Context, req *pointspb.EarnReq) (out *pointspb.EarnResp, err error) {
	resp := &pointspb.EarnResp{UserId: req.GetUserId()}
	ctx, end := h.begin(ctx, "Earn")
	defer func() { end(err) }()

	rec, err := h.svc.Earn(ctx, model.EarnCmd{
		UserID:  uint64(req.GetUserId()),
		OrderNo: req.GetOrderNo(),
		Amount:  req.GetAmount(),
		Remark:  req.GetRemark(),
	})
	if err != nil {
		if sErr := settle(&resp.Code, &resp.Message, err); sErr != nil {
			return nil, sErr
		}
		return resp, nil
	}
	resp.Balance, resp.LedgerId = rec.BalanceAfter, int64(rec.LedgerID)
	if sErr := settle(&resp.Code, &resp.Message, nil); sErr != nil {
		return nil, sErr
	}
	return resp, nil
}

// Consume 消费（同上反向）
func (h *PointsHandler) Consume(ctx context.Context, req *pointspb.ConsumeReq) (out *pointspb.ConsumeResp, err error) {
	resp := &pointspb.ConsumeResp{UserId: req.GetUserId()}
	ctx, end := h.begin(ctx, "Consume")
	defer func() { end(err) }()

	rec, err := h.svc.Consume(ctx, model.ConsumeCmd{
		UserID:  uint64(req.GetUserId()),
		OrderNo: req.GetOrderNo(),
		Amount:  req.GetAmount(),
		Remark:  req.GetRemark(),
	})
	if err != nil {
		if sErr := settle(&resp.Code, &resp.Message, err); sErr != nil {
			return nil, sErr
		}
		return resp, nil
	}
	resp.Balance, resp.LedgerId = rec.BalanceAfter, int64(rec.LedgerID)
	if sErr := settle(&resp.Code, &resp.Message, nil); sErr != nil {
		return nil, sErr
	}
	return resp, nil
}

// ListLedgers 流水列表（point_ledgers ⋈ point_accounts，带出账户余额）
func (h *PointsHandler) ListLedgers(ctx context.Context, req *pointspb.ListLedgersReq) (out *pointspb.ListLedgersResp, err error) {
	resp := &pointspb.ListLedgersResp{UserId: req.GetUserId()}
	ctx, end := h.begin(ctx, "ListLedgers")
	defer func() { end(err) }()

	rows, err := h.svc.ListLedgers(ctx, model.ListLedgersQuery{
		UserID: uint64(req.GetUserId()),
		Limit:  int(req.GetLimit()),
	})
	if err != nil {
		if sErr := settle(&resp.Code, &resp.Message, err); sErr != nil {
			return nil, sErr
		}
		return resp, nil
	}
	resp.Ledgers = toPbLedgers(rows)
	if sErr := settle(&resp.Code, &resp.Message, nil); sErr != nil {
		return nil, sErr
	}
	return resp, nil
}

// Report 2 表 JOIN 报表
func (h *PointsHandler) Report(ctx context.Context, req *pointspb.ReportReq) (out *pointspb.ReportResp, err error) {
	resp := &pointspb.ReportResp{}
	ctx, end := h.begin(ctx, "Report")
	defer func() { end(err) }()

	rows, err := h.svc.Report(ctx, model.PointsReportQuery{
		UserID: uint64(req.GetUserId()),
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

// toPbAccount 账户联表投影 → pb。
//
// pb 的 PointAccount 只承载账户口径（余额 / 累计入账 / 累计消费）：联表聚合
// 出来的流水笔数与最近变动是本域的对账信号，没有对外字段，不往 pb 里塞
// （塞进去就要在协议里承诺它们的语义，而它们目前只服务于内部对账与展示）。
func toPbAccount(a *model.AccountDetail) *pointspb.PointAccount {
	if a == nil {
		return nil
	}
	return &pointspb.PointAccount{
		UserId:      int64(a.UserID),
		Balance:     a.Balance,
		TotalEarned: a.TotalEarned,
		TotalSpent:  a.TotalSpent,
	}
}

func toPbLedgers(rows []model.LedgerDetail) []*pointspb.PointLedger {
	out := make([]*pointspb.PointLedger, 0, len(rows))
	for i := range rows {
		out = append(out, &pointspb.PointLedger{
			Id:           int64(rows[i].ID),
			UserId:       int64(rows[i].UserID),
			OrderNo:      rows[i].OrderNo,
			Type:         rows[i].Type,
			Amount:       rows[i].Amount,
			BalanceAfter: rows[i].BalanceAfter,
			CreatedAt:    rows[i].CreatedAt.Unix(),
		})
	}
	return out
}

func toPbReportRows(rows []model.ReportRow) []*pointspb.ReportRow {
	out := make([]*pointspb.ReportRow, 0, len(rows))
	for i := range rows {
		row := &pointspb.ReportRow{
			UserId:      rows[i].UserID,
			Balance:     rows[i].Balance,
			TotalEarned: rows[i].TotalEarned,
			TotalSpent:  rows[i].TotalSpent,
			LedgerCount: rows[i].LedgerCount,
		}
		if rows[i].LastChangeAt != nil {
			row.LastChangeAt = rows[i].LastChangeAt.Unix()
		}
		out = append(out, row)
	}
	return out
}
