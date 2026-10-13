// srv-stock 的 dtm 分支业务方法（Saga Deduct/Release + TCC Try/Confirm/Cancel）。
//
// 边界纪律（沿用 stock.go 文件头的写法，不因为「这是给 dtm 调的」而松一条）：
//   - 本层**不 import pb、不 import kitex、不 import grpc**。出入参一律是
//     model.DtmStockCmd；载荷的反序列化在 internal/dtm 分支面，pb 与领域模型之间
//     的转换也归它。否则 proto 改一个字段就会顺着 import 渗进业务规则。
//   - 句柄不来自全局单例：*sql.DB 经 repository.RawDB 从 launchx dbx 句柄取。
//   - 业务失败用 codes.Error 带上码上抛，并保留 dtmx.BusinessFailure 的官方哨兵
//     —— dtm 只认那个哨兵，不认码。
//
// 为什么分支方法仍走六层链而不是 handler 直连 repository：barrier 事务必须在
// service 之下、repository 之上被交出来（service 取 barrier、开事务、把 tx 递给
// repository）。这正是分层链唯一能把「幂等裁决」放在「业务写」外面的位置 ——
// handler 直连 repository 就只能靠唯一键去猜幂等，而空补偿（补偿早于正向到达）
// 那种情形没有任何唯一键能挡。
package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/gospacex/logx"

	"github.com/gospacex/dtmx"
	"github.com/gospacex/launchx/examples/micro/pkg/codes"
	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/model"
	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/repository"
)

// branchWrite 一个分支写：签名固定为「ctx + barrier 事务里的 tx + 业务体」，
// 收敛成一个函数值让五个公开方法共用同一条编排（取 barrier → 开事务 → 写 →
// 判错分流），而不是五份复制。
type branchWrite func(ctx context.Context, tx *sql.Tx, cmd model.DtmStockCmd) error

// DtmDeduct Saga 正向分支：扣减可售库存。
//
// dtm 分支序里的第一个：order.CreateOrder / product.销+1 / points.Debit 任一失败，
// dtm 逆序调 DtmRelease 回补。
func (s *StockService) DtmDeduct(ctx context.Context, cmd model.DtmStockCmd) (err error) {
	ctx, end := s.begin(ctx, "DtmDeduct")
	defer func() { end(err) }()

	return s.dtmBranch(ctx, cmd, s.repo.DtmDeduct)
}

// DtmRelease Saga 补偿分支：回补 DtmDeduct 扣掉的量。
//
// 由 dtm 异步发起，载荷里没有 traceparent —— 链路按「新根 trace + biz.dtm.gid
// 关联」口径记录（见 internal/dtm 的分支 handler）。
func (s *StockService) DtmRelease(ctx context.Context, cmd model.DtmStockCmd) (err error) {
	ctx, end := s.begin(ctx, "DtmRelease")
	defer func() { end(err) }()

	return s.dtmBranch(ctx, cmd, s.repo.DtmRelease)
}

// DtmTryFreeze TCC 一阶段：可售 → 冻结（不实扣，Cancel ���原样退回）。
func (s *StockService) DtmTryFreeze(ctx context.Context, cmd model.DtmStockCmd) (err error) {
	ctx, end := s.begin(ctx, "DtmTryFreeze")
	defer func() { end(err) }()

	return s.dtmBranch(ctx, cmd, s.repo.DtmTryFreeze)
}

// DtmConfirmFreeze TCC 二阶段：冻结 → 实扣。
func (s *StockService) DtmConfirmFreeze(ctx context.Context, cmd model.DtmStockCmd) (err error) {
	ctx, end := s.begin(ctx, "DtmConfirmFreeze")
	defer func() { end(err) }()

	return s.dtmBranch(ctx, cmd, s.repo.DtmConfirmFreeze)
}

// DtmCancelFreeze TCC 二阶段：冻结 → 可售。
func (s *StockService) DtmCancelFreeze(ctx context.Context, cmd model.DtmStockCmd) (err error) {
	ctx, end := s.begin(ctx, "DtmCancelFreeze")
	defer func() { end(err) }()

	return s.dtmBranch(ctx, cmd, s.repo.DtmCancelFreeze)
}

// dtmBranch 分支方法的统一编排。
//
// 顺序是刻意定的，每一步都对应一种「顺序反了就出事」：
//  1. 先校验参数 —— 放在 barrier.Call 之前，坏载荷就不会在 barrier 表里留行，
//     且它被当成业务失败，dtm 不会为一个错单号重试一整天；
//  2. 再取 *sql.DB —— 取不到就直接报错。绝不能「没有事务也照样写」：那样
//     写是成功了，但重复回调会重复扣减，且当场没有任何迹象；
//  3. 再取 barrier —— **error 必须上抛**（见 dtmx.BarrierFromCtx 的注释）：
//     没有 barrier 就没有空补偿/重复回调/悬挂三条守卫；
//  4. 最后才在 barrier 事务里写。
func (s *StockService) dtmBranch(ctx context.Context, cmd model.DtmStockCmd, write branchWrite) error {
	if err := validateDtmCmd(cmd); err != nil {
		return dtmx.BusinessFailure(codes.Wrap(codes.CodeInvalidParam, "dtm 分支参数非法", err))
	}
	db, err := repository.RawDB(s.repo.DB())
	if err != nil {
		// 基础设施故障：原样上抛，dtm 按退避重试。标成业务失败会让整条全局事务
		// 被判死并触发对已成功分支的回补 —— 一次装配错位变成一次真实回滚。
		return fmt.Errorf("dtm 分支取不到业务库连接: %w", err)
	}
	barrier, err := dtmx.BarrierFromCtx(ctx)
	if err != nil {
		return fmt.Errorf("dtm 分支取不到 barrier（请求不是 dtm server 回调？）: %w", err)
	}

	err = barrier.Call(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
		return write(ctx, tx, cmd)
	})
	if err != nil {
		return s.dtmFail(ctx, err)
	}
	// 提交之后再失效缓存：在 barrier.Call 内失效的话，并发读可能在提交前把旧值
	// 回填进缓存，而那次回填之后没人再删 —— 于是 HTTP 面上会读到 30s 的旧库存。
	s.invalidate(ctx, cmd.ProductID)
	return nil
}

// dtmFail 把分支写失败翻译成带业务码、且保留 dtm 官方失败哨兵的错误。
//
// 分流判据是「错误种类」而不是「有没有错」：
//   - 已是业务失败（库存/冻结不足、参数非法）→ 翻译成 4xxx/1xxx 后**继续**带哨兵，
//     dtm 读到哨兵就不重试、直接转补偿；
//   - 其余（DB 不可达、句柄取不到）→ 原样上抛，dtm 退避重试。把它标成业务失败
//     等于告诉 dtm「别重试了，去回滚」，一次 DB 抖动会连带把已经卖出的库存回补掉。
func (s *StockService) dtmFail(ctx context.Context, err error) error {
	if !dtmx.IsBusinessFailure(err) {
		s.logger.WarnContext(ctx, "stock: dtm 分支基础设施故障，不计入业务失败（dtm 将退避重试）", logx.Err(err))
		return err
	}
	return dtmx.BusinessFailure(s.dtmMapErr(ctx, err))
}

// dtmMapErr 分支错误 → 业务码。
//
// 不复用 stock.go 的 mapRepoErr：那条把「预占流水不存在」当成功语义处理
// （CodeStockReservationNF），而分支面里「该行不存在」在条件更新上就是不足，
// 沿用它会把 Release 的空回补报成「流水不存在，什么都不用做」。
func (s *StockService) dtmMapErr(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, repository.ErrFrozenInsufficient):
		return codes.Wrap(codes.CodeStockInsufficient, "frozen stock insufficient", err)
	case errors.Is(err, repository.ErrInsufficient):
		return codes.Wrap(codes.CodeStockInsufficient, "stock insufficient", err)
	case errors.Is(err, repository.ErrNotFound):
		return codes.Wrap(codes.CodeStockNotFound, "stock record not found", err)
	default:
		s.logger.WarnContext(ctx, "stock: dtm 分支写库失败", logx.Err(err))
		return codes.Wrap(codes.CodeStockReserveFailed, "dtm branch write failed", err)
	}
}

// validateDtmCmd 分支入参校验。
//
// 单独一个函数而不是内联：五个方法各自内联一遍的话，改一条规则要改五处，
// 漏改的那处会表现为「别的分支能拒、这个分支把负数量也写进流水」。
func validateDtmCmd(cmd model.DtmStockCmd) error {
	switch {
	case cmd.ProductID <= 0:
		return errors.New("product_id required")
	case cmd.Quantity <= 0:
		// 负数量会让条件更新恒成立（available >= -n 谁都过），把库存加上去；
		// 0 则白写一条零流水 —— 两者都必须在这里被拒。
		return errors.New("quantity must be positive")
	case cmd.OrderNo == "":
		return errors.New("order_no required（账变流水与 saga 分支的对账锚点）")
	default:
		return nil
	}
}
