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
	"time"

	"github.com/gospacex/otelx/layer"
	"gorm.io/gorm"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/points/model"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/events"
)

// pointsRepo 是 service 真正依赖的窄接口：只含它调用的三个方法。
//
// 用接口而不是 *repository.PointsRepository：service 只关心「加/读/记」的语义，
// 不关心句柄从哪来；这也让业务判断能脱离真实库被直接驱动到（见 points_test.go）。
// *repository.PointsRepository 天然满足它，装配层无需额外适配。
type pointsRepo interface {
	AddBalance(ctx context.Context, userID uint64, delta int64) error
	Balance(ctx context.Context, userID uint64) (*model.PointAccount, error)
	AddLog(ctx context.Context, l *model.PointsLog) error
}

// PointsService 积分业务。
type PointsService struct {
	repo    pointsRepo
	traceOn bool // trace.enabled（总闸），由装配层注入
}

// New 注入仓储与埋点开关。
func New(repo pointsRepo, traceOn bool) *PointsService {
	return &PointsService{repo: repo, traceOn: traceOn}
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
// 幂等：本例不带幂等键（order_id 未做唯一约束），同一事件被重投会重复加分；
// 真实系统应在 points_log(order_id) 上加唯一约束、并把「加余额 + 写流水」放进
// 同一事务（见 apps/README「删不掉的四类」）。本示例按最小可用形态落地。
func (s *PointsService) ConsumeOrderCompleted(ctx context.Context, e events.OrderCompleted) (err error) {
	ctx, end := layer.Begin(ctx, "service", "points", "ConsumeOrderCompleted", s.traceOn, nil)
	defer func() { end(err) }()

	delta := e.TotalAmount
	if delta <= 0 {
		// InvalidArgument 是**确定性**错误：载荷里的字段决定了重投一万次也是同一个
		// 结果。消费侧据此码判定为不可重试、记 Warn 后吞掉（见 app.consumeHandler
		// 的 nonRetryable），不丢给驱动反复重投 —— 否则就是毒丸。
		return codes.New(codes.InvalidArgument, "total amount must be positive")
	}
	if err := s.repo.AddBalance(ctx, e.UserID, delta); err != nil {
		return codes.Wrap(codes.Internal, "add balance", err)
	}
	if err := s.repo.AddLog(ctx, &model.PointsLog{
		UserID: e.UserID, OrderID: e.OrderID, Delta: delta, CreatedAt: time.Now(),
	}); err != nil {
		return codes.Wrap(codes.Internal, "add points log", err)
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
