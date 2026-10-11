// Package service ★ 唯一允许出现业务判断的层。
//
// 本域的句柄面比 order 窄：只有 db（经 repository 注入）与 mqConsumer（只在
// app.Setup 的消费循环里用到）—— 没有 cache / lock / mq producer。两条主路径：
//
//	ConsumeOrderCompleted —— 消费 order.completed.v1，加积分 + 记流水（写路径）
//	GetPoints            —— 只读 RPC 路径，供 IDL 的 GetAccount 取账户
package service

import (
	"context"
	"errors"
	"strconv"

	"github.com/gospacex/logx"
	"github.com/gospacex/otelx/layer"
	"gorm.io/gorm"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/points/model"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/events"
)

// pointsRepo 是 service 真正依赖的窄接口：只含它调用的两个方法。
//
// 用接口而不是 *repository.PointsRepository：service 只关心「入账/读」的语义，
// 不关心句柄从哪来；这也让业务判断能脱离真实库被直接驱动到（见 points_test.go）。
// *repository.PointsRepository 天然满足它，装配层无需额外适配。
//
// 写路径只有 **CreditOnce** 一个方法（不是 AddBalance + AddLog 两个）：幂等与
// 原子性都不是「业务判断」，而是落库这件事自身的属性，故收口在仓储内的一个事务里
// —— 拆成两个调用，调用方就有机会只调一半，而那正是修复前的形态。
type pointsRepo interface {
	// CreditOnce 按 order_id 幂等入账；applied=false 表示本笔已入账过。
	CreditOnce(ctx context.Context, userID, orderID uint64, delta int64) (applied bool, err error)
	Balance(ctx context.Context, userID uint64) (*model.PointAccount, error)
}

// PointsService 积分业务。
type PointsService struct {
	repo    pointsRepo
	logger  logx.Logger // 可为 nil（只有幂等命中这类非故障路径用它留痕）
	traceOn bool        // trace.enabled（总闸），由装配层注入
}

// New 注入仓储、日志器与埋点开关。
func New(repo pointsRepo, logger logx.Logger, traceOn bool) *PointsService {
	return &PointsService{repo: repo, logger: logger, traceOn: traceOn}
}

// AccountView 查账户出参（service 自己的类型，与 RPC 结构解耦）。
type AccountView struct {
	UserID  uint64
	Balance int64
}

// ConsumeOrderCompleted 消费订单完成事件：按订单总额加积分并记一条流水。
//
// 分值口径：示例取 1:1（订单总额即积分数），是**最简单可验证**的口径 ——
// 真实系统的折算规则属业务配置，不在本示例的判据里。
//
// 幂等与原子性（2026-10-10 企业级加固）：本方法不再自己拼「加余额 + 写流水」，
// 而是交给 repository.CreditOnce —— 它以 `points_log.order_id` 的唯一索引为
// 幂等闸门，并把两条语句放进同一事务。修复前的形态有两处真实缺陷：同一事件
// 重投会**重复加分**（没有幂等键），且加余额与记账不在同一事务（记账失败时
// 余额已加、账面凭空多出一笔无凭据的分）。
func (s *PointsService) ConsumeOrderCompleted(ctx context.Context, e events.OrderCompleted) (err error) {
	ctx, end := layer.Begin(ctx, "service", "points", "ConsumeOrderCompleted", s.traceOn, nil)
	defer func() { end(err) }()

	// order_id 是幂等键，为 0 就没有闸门可言（所有无 id 的事件会互相顶掉）。
	// 这是**确定性**错误（载荷决定，重投一万次同一个结果），故回 InvalidArgument
	// 让消费侧按不可重试处理，而不是当成抖动反复重投。
	if e.OrderID == 0 {
		return codes.New(codes.InvalidArgument, "order id must be positive")
	}
	delta := e.TotalAmount
	if delta <= 0 {
		// InvalidArgument 是**确定性**错误：载荷里的字段决定了重投一万次也是同一个
		// 结果。消费侧据此码判定为不可重试、记 Warn 后吞掉（见 app.consumeHandler
		// 的 nonRetryable），不丢给驱动反复重投 —— 否则就是毒丸。
		return codes.New(codes.InvalidArgument, "total amount must be positive")
	}
	applied, cerr := s.repo.CreditOnce(ctx, e.UserID, e.OrderID, delta)
	if cerr != nil {
		return codes.Wrap(codes.Internal, "credit points", cerr)
	}
	if !applied && s.logger != nil {
		// 幂等命中是**正常现象**（消息中间件是至少一次语义），不是降级、不是错误。
		// 但仍要留痕：否则「积分没涨」与「重复事件被正确丢弃」在运维眼里长得一模
		// 一样，排查时只能去翻库 —— 这是本条链路上唯一能区分两者的信号。
		s.logger.Warn("mesh-points: duplicate order.completed ignored (already credited)",
			logx.String("order_id", strconv.FormatUint(e.OrderID, 10)),
			logx.String("user_id", strconv.FormatUint(e.UserID, 10)),
			logx.String("consequence", "本笔积分不重复入账（幂等命中）"))
	}
	return nil
}

// GetPoints 查积分账户（IDL 的 GetAccount 落到这里）。
//
// 未开户（库里没有该 user 的行）视为 0 积分而非错误：读路径不该因为「还没
// 消费过」而失败 —— 那会把一个正常的新用户显示成下游故障。
func (s *PointsService) GetPoints(ctx context.Context, userID uint64) (v *AccountView, err error) {
	ctx, end := layer.Begin(ctx, "service", "points", "GetPoints", s.traceOn, nil)
	defer func() { end(err) }()

	acc, err := s.repo.Balance(ctx, userID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return &AccountView{UserID: userID, Balance: 0}, nil
		}
		return nil, codes.Wrap(codes.Internal, "get points balance", err)
	}
	return &AccountView{UserID: acc.UserID, Balance: acc.Balance}, nil
}
