// srv-order 的 dtm 分支业务方法（Saga CreateOrder/CancelOrder +
// TCC TryPlace/ConfirmFreeze/CancelFreeze）。
//
// 边界纪律（沿用 order.go 文件头的写法，不因为「这是给 dtm 调的」而松一条）：
//   - 本层**不 import pb、不 import kitex、不 import grpc**。出入参一律是
//     model.DtmOrderCmd；载荷的反序列化在 internal/dtm 分支面，pb 与领域模型之间
//     的转换也归它。否则 proto 改一个字段就会顺着 import 渗进业务规则。
//   - 句柄不来自全局单例：*sql.DB 经 repository.RawDB 从 launchx dbx 句柄取。
//   - 必须 dtmx.BarrierFromCtx 取 barrier，且 **error 必须上抛**：见
//     TestMethodsRejectRequestsWithoutBarrierMetadata。
//   - 业务失败同时带 codes 码与 dtm 官方哨兵（dtmx.BusinessFailure）：前者给人和
//     编排侧读，后者决定 dtm 会不会重试。少任何一个的代价都写在各自的注释里。
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
	"time"

	"github.com/gospacex/logx"

	"github.com/gospacex/dtmx"
	"github.com/gospacex/launchx/examples/micro/pkg/codes"
	"github.com/gospacex/launchx/examples/micro/pkg/events"
	"github.com/gospacex/launchx/examples/micro/srv-order/internal/model"
	"github.com/gospacex/launchx/examples/micro/srv-order/internal/repository"
)

// branchWrite 一个分支写：签名固定为「ctx + barrier 事务里的 tx + 业务体」，
// 收敛成一个函数值让五个公开方法共用同一条编排（取 barrier → 开事务 → 写 →
// 判错分流），而不是五份复制。
//
// 回传 orderID 是给缓存失效用的（见 dtmBranch 末尾）。
type branchWrite func(ctx context.Context, tx *sql.Tx, cmd model.DtmOrderCmd) (uint64, error)

// DtmCreateOrder Saga 正向分支：正式单落库（orders + order_items + outbox）。
//
// 这是「正向分支」那一类里唯一写 outbox 的 Saga 分支，与 HTTP 面 CreateOrder
// 的落库口径逐字一致（同一个 events.OrderCompleted、同一个 topic）。
func (s *OrderService) DtmCreateOrder(ctx context.Context, cmd model.DtmOrderCmd) (err error) {
	ctx, end := s.begin(ctx, "DtmCreateOrder")
	defer func() { end(err) }()

	return s.dtmBranch(ctx, cmd, func(ctx context.Context, tx *sql.Tx, c model.DtmOrderCmd) (uint64, error) {
		return s.repo.DtmCreateOrder(ctx, tx, c, s.dtmOutboxFill(ctx, c))
	})
}

// DtmCancelOrder Saga 补偿分支：把 CreateOrder 建的单作废。
//
// 由 dtm 异步发起，载荷里没有 traceparent —— 链路按「新根 trace + biz.dtm.gid
// 关联」口径记录（见 internal/dtm 的分支 handler）。**不写 outbox**。
func (s *OrderService) DtmCancelOrder(ctx context.Context, cmd model.DtmOrderCmd) (err error) {
	ctx, end := s.begin(ctx, "DtmCancelOrder")
	defer func() { end(err) }()

	return s.dtmBranch(ctx, cmd, s.repo.DtmCancelOrder)
}

// DtmTryPlace TCC 一阶段：建 presale_locked 占位单（**不写 outbox**）。
func (s *OrderService) DtmTryPlace(ctx context.Context, cmd model.DtmOrderCmd) (err error) {
	ctx, end := s.begin(ctx, "DtmTryPlace")
	defer func() { end(err) }()

	return s.dtmBranch(ctx, cmd, s.repo.DtmTryPlace)
}

// DtmConfirmFreeze TCC 二阶段：占位单转正式 paid 单，**此刻才写 outbox**。
func (s *OrderService) DtmConfirmFreeze(ctx context.Context, cmd model.DtmOrderCmd) (err error) {
	ctx, end := s.begin(ctx, "DtmConfirmFreeze")
	defer func() { end(err) }()

	return s.dtmBranch(ctx, cmd, func(ctx context.Context, tx *sql.Tx, c model.DtmOrderCmd) (uint64, error) {
		return s.repo.DtmConfirmFreeze(ctx, tx, c, s.dtmOutboxFill(ctx, c))
	})
}

// DtmCancelFreeze TCC 二阶段：废掉占位单（**不写 outbox**）。
func (s *OrderService) DtmCancelFreeze(ctx context.Context, cmd model.DtmOrderCmd) (err error) {
	ctx, end := s.begin(ctx, "DtmCancelFreeze")
	defer func() { end(err) }()

	return s.dtmBranch(ctx, cmd, s.repo.DtmCancelFreeze)
}

// dtmBranch 分支方法的统一编排。
//
// 顺序是刻意定的，每一步都对应一种「顺序反了就出事」：
//  1. 先校验参数 —— 放在 barrier.Call 之前，坏载荷就不会在 barrier 表里留行
//     （留了行，下次重放会被 barrier 判成「已处理」而静默跳过，坏单就这么被吞了），
//     且它被当成业务失败，dtm 不会为一个错单号重试一整天；
//  2. 再取 *sql.DB —— 取不到就直接报错。绝不能「没有事务也照样写」：那样写是成功了，
//     但重复回调会重复建单/重复落事件，且当场没有任何迹象；
//  3. 再取 barrier —— **error 必须上抛**（见 dtmx.BarrierFromCtx 的注释）：
//     没有 barrier 就没有空补偿/重复回调/悬挂三条守卫；
//  4. 最后才在 barrier 事务里写。
func (s *OrderService) dtmBranch(ctx context.Context, cmd model.DtmOrderCmd, write branchWrite) error {
	if err := validateDtmCmd(cmd); err != nil {
		return dtmx.BusinessFailure(codes.Wrap(validateCode(err), "dtm 分支参数非法", err))
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

	// orderID 在 barrier 事务内回填：CreateOrder / ConfirmFreeze 拿到自增 id，
	// 三个逆向分支与幂等重放拿不到（0），那正是「没资格失效缓存」的两种情形。
	var orderID uint64
	err = barrier.Call(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
		var werr error
		orderID, werr = write(ctx, tx, cmd)
		return werr
	})
	if err != nil {
		return s.dtmFail(ctx, err)
	}
	// 提交之后再失效缓存：分支改的是订单状态，而详情缓存 key 是
	// (user_id, order_id)。不失效的话，ConfirmFreeze 之后 GetOrder 会把缓存里的
	// presale_locked 一直返回下去（TTL 5 分钟），调用方看到的是一张永远不存在的单。
	// 在 barrier.Call 内失效的话，并发读可能在提交前把旧值回填进缓存，而那次回填
	// 之后没人再删 —— 于是那 5 分钟里谁都读到旧状态。
	if orderID > 0 {
		s.invalidate(ctx, cmd.UserID, orderID)
	}
	return nil
}

// dtmOutboxFill 事件填充器：拼 events.OrderCompleted（与 HTTP 面 CreateOrder 同一
// 事件体、同一 schema 版本）。
//
// 只交给 DtmCreateOrder / DtmConfirmFreeze 两个方法 —— 另外三个方法的签名里根本
// 没有 OutboxFill 参数，「占位单 / 作废也发下单完成」这件事在本层写不出来
// （口径②钉在方法形状上，而不是靠注释提醒）。
func (s *OrderService) dtmOutboxFill(ctx context.Context, cmd model.DtmOrderCmd) repository.OutboxFill {
	return func(orderID uint64) (string, string) {
		ev := events.OrderCompleted{
			OrderID:    int64(orderID),
			OrderNo:    cmd.OrderNo,
			UserID:     int64(cmd.UserID),
			Amount:     cmd.Total(),
			Items:      toEventItems(cmd.Items),
			SchemaVer:  events.SchemaVer1,
			TraceID:    evTraceID(ctx),
			OccurredAt: time.Now().UnixMilli(),
		}
		b, err := events.Marshal(ev)
		if err != nil {
			// 序列化失败不阻断主流程：空 payload 时 Runner 直接放弃该行，
			// 不占重试配额（与 HTTP 面同一处置）。
			s.logger.WarnContext(ctx, "order dtm event marshal failed", logx.Err(err))
			return "", cmd.Traceparent
		}
		return string(b), cmd.Traceparent
	}
}

// dtmFail 把分支写失败翻译成带业务码、且保留 dtm 官方失败哨兵的错误。
//
// 分流判据是「错误种类」而不是「有没有错」：
//   - 已是业务失败（占位单不存在 / 状态不允许 / 参数非法）→ 翻译成 3xxx 后**继续**
//     带哨兵，dtm 读到哨兵就不重试、直接转补偿；
//   - 其余（DB 不可达、句柄取不到、回读不到刚写的行）→ 原样上抛，dtm 退避重试。
//     把它标成业务失败等于告诉 dtm「别重试了，去回滚」，一次 DB 抖动会连带把
//     已经建好的订单回补掉。
func (s *OrderService) dtmFail(ctx context.Context, err error) error {
	if !dtmx.IsBusinessFailure(err) {
		s.logger.WarnContext(ctx, "order: dtm 分支基础设施故障，不计入业务失败（dtm 将退避重试）", logx.Err(err))
		return err
	}
	return dtmx.BusinessFailure(s.dtmMapErr(ctx, err))
}

// dtmMapErr 分支错误 → 订单域业务码。
//
// 「定位不到单」与「状态不允许这次流转」在 SQL 层是同一种现象（一条条件更新
// 命中 0 行，见 repository.ErrStatusNotAllowed 的注释），故两者都翻成 3003
// 「订单状态不允许该操作」：那是这条链上唯一真正该做的动作 —— dtm 立即逆序
// 补偿/回退，不重试。
//
// ErrNotFound 也映射在这里：本包共用的哨兵，当前分支写路径不产生它（写路径
// 全是条件更新，没有「查了没查到」这一步），留着是为了将来某个分支改用共用
// 哨兵时不必再回来补一条翻译 —— 漏翻的代价是「3xxx 被报成 1003」。
//
// 为什么不复用 order.go 的 mapRepoErr：那条把 ErrNotFound 一律翻成
// CodeInternal（500 语义），而分支面里这些都是「dtm 立刻该转补偿」的判定，
// 报 500 会让人以为要人工介入。
func (s *OrderService) dtmMapErr(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, repository.ErrStatusNotAllowed):
		return codes.Wrap(codes.CodeOrderStatus, "order status does not allow this dtm branch", err)
	case errors.Is(err, repository.ErrNotFound):
		return codes.Wrap(codes.CodeOrderNotFound, "order not found", err)
	default:
		s.logger.WarnContext(ctx, "order: dtm 分支写库失败", logx.Err(err))
		return codes.Wrap(codes.CodeInternal, "dtm branch write failed", err)
	}
}

// validateDtmCmd 分支入参校验。
//
// 单独一个函数而不是内联：五个方法各自内联一遍的话，改一条规则要改五处，
// 漏改的那处会表现为「别的分支能拒、这个分支把空明细也写进库」。
//
// 每条判据都对应一种「不拒就会落一张错账」的形态：
//   - order_no 空：落一张查不出明细、没有幂等键的单，且跨域对账没有锚点；
//   - dtm_gid 空：占位单/正式单与全局事务失联，Confirm 与 Cancel 都定位不到它；
//   - items 空：总额恒为 0 的空壳单，下游按它发的事件无意义；
//   - quantity ≤ 0 / unit_price < 0：负单价会让总额算成负数，订单域的账直接反号。
func validateDtmCmd(cmd model.DtmOrderCmd) error {
	switch {
	case cmd.UserID <= 0:
		return errors.New("user_id required")
	case cmd.OrderNo == "":
		return errors.New("order_no required（幂等键与跨域对账锚点）")
	case cmd.DtmGID == "":
		return errors.New("dtm_gid required（占位与补偿按它定位；由编排侧编进载荷）")
	case len(cmd.Items) == 0:
		return errDtmItemsEmpty{}
	default:
	}
	for _, it := range cmd.Items {
		if it.ProductID <= 0 || it.Quantity <= 0 || it.UnitPrice < 0 {
			return errors.New("invalid item（product_id > 0、quantity > 0、unit_price >= 0）")
		}
	}
	return nil
}

// errDtmItemsEmpty 明细为空（单独一个类型，好让码翻成 3004 而不是笼统的 1001）。
type errDtmItemsEmpty struct{}

func (errDtmItemsEmpty) Error() string { return "items required" }

// validateCode 校验失败的分类：空明细是 3004（业务上就是「没东西可下单」），
// 其余是 1001（调用方拼错了载荷）。两者都是业务失败，dtm 都不会重试。
func validateCode(err error) int64 {
	var empty errDtmItemsEmpty
	if errors.As(err, &empty) {
		return codes.CodeOrderItemsEmpty
	}
	return codes.CodeInvalidParam
}
