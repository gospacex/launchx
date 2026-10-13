// dtm 分支编排层（Saga Debit/Refund + TCC TryFreeze/ConfirmFreeze/CancelFreeze）。
//
// 边界纪律与本包既有方法完全一致，且三条都带 dtm 场景的具体理由：
//   - **不 import pb / kitex / grpc**：本文件只知道「有一个业务方法被调用」，
//     协议转换在 internal/dtm。反过来 import 会让 dtm 的 gRPC 依赖顺着
//     import 渗进业务规则 —— 之后想把分支面换成 HTTP（或干脆撤掉）就得重写业务。
//   - **必须经 dtmx.BarrierFromCtx 取 barrier，且 error 必须上抛**：见
//     TestDtmMethodsRejectRequestsWithoutBarrierMetadata。
//   - **业务失败同时带 codes 码与 dtm 官方哨兵**：前者给人和编排侧读，后者决定
//     dtm 会不会重试。少任何一个的代价都写在各自的注释里。
package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/gospacex/logx"

	"github.com/gospacex/dtmx"
	"github.com/gospacex/launchx/examples/micro/pkg/codes"
	"github.com/gospacex/launchx/examples/micro/srv-points/internal/model"
	"github.com/gospacex/launchx/examples/micro/srv-points/internal/repository"
)

// dtmStoreNotReady 分支写通道未装配。
var dtmStoreNotReady = errors.New("service/dtm: 分支写通道未装配")

// DtmDebit Saga 动作段：扣减可用积分并记一笔消费流水。
func (s *PointsService) DtmDebit(ctx context.Context, b model.DtmBiz) (err error) {
	return s.dtmBranch(ctx, "DtmDebit", b, func(ctx context.Context, bar *dtmx.Barrier) error {
		return s.dtmStore.Debit(ctx, bar, b)
	})
}

// DtmRefund Saga 补偿段：把 Debit 扣掉的积分原路回补。
//
// 补偿段与动作段共用同一份载荷：dtm 的 Saga 只按 URL 区分两段，业务体不变。
// 幂等不在本层做 —— 补偿会被 dtm 重试，去重是 barrier 与流水唯一键的职责。
func (s *PointsService) DtmRefund(ctx context.Context, b model.DtmBiz) (err error) {
	return s.dtmBranch(ctx, "DtmRefund", b, func(ctx context.Context, bar *dtmx.Barrier) error {
		return s.dtmStore.Refund(ctx, bar, b)
	})
}

// DtmTryFreeze TCC 第一段：可用积分转冻结。
func (s *PointsService) DtmTryFreeze(ctx context.Context, b model.DtmBiz) (err error) {
	return s.dtmBranch(ctx, "DtmTryFreeze", b, func(ctx context.Context, bar *dtmx.Barrier) error {
		return s.dtmStore.TryFreeze(ctx, bar, b)
	})
}

// DtmConfirmFreeze TCC 第二段：冻结转已消费（真正实扣 + 流水）。
func (s *PointsService) DtmConfirmFreeze(ctx context.Context, b model.DtmBiz) (err error) {
	return s.dtmBranch(ctx, "DtmConfirmFreeze", b, func(ctx context.Context, bar *dtmx.Barrier) error {
		return s.dtmStore.ConfirmFreeze(ctx, bar, b)
	})
}

// DtmCancelFreeze TCC 第三段：冻结退回可用。
func (s *PointsService) DtmCancelFreeze(ctx context.Context, b model.DtmBiz) (err error) {
	return s.dtmBranch(ctx, "DtmCancelFreeze", b, func(ctx context.Context, bar *dtmx.Barrier) error {
		return s.dtmStore.CancelFreeze(ctx, bar, b)
	})
}

// dtmBranch 五个分支方法的公共骨架：开 span → 校验载荷 → 取 barrier → 执行 →
// 翻译错误。
//
// 抽出来的理由与本包 apply 同款：五个方法之间只差「调哪个 store 方法」，
// 复制五份的代价是「将来只改一边」—— 比如给入参校验补了 cost 上界，
// 却漏了其中两个方法。
func (s *PointsService) dtmBranch(ctx context.Context, op string, b model.DtmBiz,
	run func(ctx context.Context, bar *dtmx.Barrier) error) (err error) {

	ctx, end := s.begin(ctx, op)
	defer func() { end(err) }()

	// 校验排在取 barrier 之前：载荷非法时无论请求来自谁都必然失败，先把这件事
	// 判掉，省掉一次 metadata 解析。
	if err := validateDtmBiz(b); err != nil {
		return err
	}

	// barrier 必须取到，取不到就报错。
	//
	// 「忽略它继续往下写」是本文件最贵的写法：没有 barrier 就没有空补偿 /
	// 重复回调 / 悬挂三条守卫，重复回调会重复扣积分、补偿先到会凭空写出脏
	// 数据，而函数返回成功、日志干净 —— 事后无从追责。
	bar, err := dtmx.BarrierFromCtx(ctx)
	if err != nil {
		// 按业务失败上报（带官方哨兵）：请求不是 dtm server 发的（或 metadata
		// 残缺），重试不会变好，让 dtm 立刻放弃比让它撞超时强。码取
		// CodeInternal 而不是 5xxx 段：这不是「积分不够」，是「这条链路配错了」。
		return dtmx.BusinessFailure(codes.Wrap(codes.CodeInternal,
			"dtm branch callback without barrier metadata", err))
	}
	if s.dtmStore == nil {
		return codes.Wrap(codes.CodeInternal, "dtm branch store not wired", dtmStoreNotReady)
	}
	return s.mapDtmErr(ctx, run(ctx, bar))
}

// validateDtmBiz 载荷校验。
//
// 非法入参一律判**业务失败**（带官方哨兵）：重试一万次也不会变好的事，不能占用
// dtm 的重试配额，更不能把一笔全局事务拖到超时 —— 那段时间里其它域的分支也
// 悬着。码取 5005（积分数量非法），与本域既有命令的参数校验对齐。
func validateDtmBiz(b model.DtmBiz) error {
	switch {
	case b.UserID <= 0:
		return dtmx.BusinessFailure(codes.New(codes.CodePointsAmountInvalid, "dtm biz: user_id required"))
	case b.Cost <= 0:
		return dtmx.BusinessFailure(codes.New(codes.CodePointsAmountInvalid, "dtm biz: cost must be positive"))
	case b.OrderNo == "":
		return dtmx.BusinessFailure(codes.New(codes.CodePointsAmountInvalid, "dtm biz: order_no required"))
	}
	return nil
}

// mapDtmErr 把持久层哨兵翻成积分域业务码，并**保持 dtm 官方哨兵不被摘掉**。
//
// 「翻成 codes.Error」与「保留 dtmcli.ErrFailure」两件事同时成立，靠的是
// codes.Wrap 的 Cause 链（codes.Error.Unwrap 暴露根因）：handler 取码用
// errors.As，dtm 判业务失败用 errors.Is(err, dtmcli.ErrFailure)，两者互不干扰。
// 写成 fmt.Errorf("%w", ...) 丢掉 codes 码的话，handler 只能报 1003，
// 「积分不够」在编排侧就变成了「服务坏了」。
func (s *PointsService) mapDtmErr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if !dtmx.IsBusinessFailure(err) {
		// 基础失败（连不上库 / SQL 报错）：原样上抛，由 dtm 按退避重试。
		// 这一条必须留在业务码翻译**之前**：把它翻成 5xxx 就等于告诉 dtm
		// 「别重试」，一笔可能只是网络抖动的账就被永久判死。
		s.logger.WarnContext(ctx, "points: dtm branch base failure (dtm will retry)", logx.Err(err))
		return err
	}
	switch {
	case errors.Is(err, repository.ErrDtmAccountNF):
		return codes.Wrap(codes.CodePointsAccountNF, "points account not found", err)
	case errors.Is(err, repository.ErrDtmInsufficient):
		return codes.Wrap(codes.CodePointsInsufficient, "points insufficient", err)
	case errors.Is(err, repository.ErrDtmFrozenShort):
		// 冻结不足也报 5002：codes 是四域共用件（examples/micro/pkg/codes），
		// 本分支不改它，故没有单独的「冻结不足」码。归到「积分不足」这一段
		// 语义上也站得住 —— 冻结的额度不足以完成本次扣减。
		return codes.Wrap(codes.CodePointsInsufficient, "points frozen insufficient", err)
	case errors.Is(err, repository.ErrDtmOrderNoTooLong):
		// 载荷问题，不是账的问题：报参数错，编排侧能立刻定位到是谁拼的订单号。
		return codes.Wrap(codes.CodeInvalidParam, "order_no too long for point_ledgers", err)
	default:
		// 带哨兵但没对上任何哨兵错误：说明是别处（如 fail_inject）打的业务失败，
		// 或出现了没归一化的新判据。翻译成写失败码并记日志，别让它掉进 default
		// 变成 1003。
		s.logger.WarnContext(ctx, "points: dtm branch business failure without known sentinel",
			logx.Err(err), logx.String("hint", "fail_inject 或新增判据？请确认映射是否完整"))
		return codes.Wrap(codes.CodePointsWriteFailed, fmt.Sprintf("dtm branch rejected: %v", err), err)
	}
}
