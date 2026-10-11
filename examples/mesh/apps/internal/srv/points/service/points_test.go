package service

import (
	"context"
	"errors"
	"testing"

	"gorm.io/gorm"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/points/model"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/events"
)

// fakeRepo 是 service 依赖的窄接口（pointsRepo）的手写假件。
//
// 只假一个写方法（CreditOnce）而不是两个：幂等与原子性收口在仓储的一个事务里，
// service 只消费「入账了没有」这个答案 —— 假件跟着收窄，才不会出现「service
// 单测全绿但仓储没开事务」这种判据与实现脱节的形态。
type fakeRepo struct {
	creditErr error
	balErr    error
	balResult *model.PointAccount

	creditCalls   int
	creditUser    uint64
	creditOrderID uint64
	creditDelta   int64
	// dup=true 表示「本笔此前已入账」（幂等命中）→ CreditOnce 回 applied=false。
	dup      bool
	balCalls int
}

func (f *fakeRepo) CreditOnce(_ context.Context, userID, orderID uint64, delta int64) (bool, error) {
	f.creditCalls++
	f.creditUser, f.creditOrderID, f.creditDelta = userID, orderID, delta
	if f.creditErr != nil {
		return false, f.creditErr
	}
	return !f.dup, nil
}

func (f *fakeRepo) Balance(context.Context, uint64) (*model.PointAccount, error) {
	f.balCalls++
	if f.balErr != nil {
		return nil, f.balErr
	}
	return f.balResult, nil
}

func newSvc(repo *fakeRepo, traceOn bool) *PointsService {
	// logger 传 nil：service 对 logger 判空（幂等命中才用），传 nil 能顺带把
	// 「忘记判空」这类 panic 在单测里暴露出来。
	return New(repo, nil, traceOn)
}

// ---------- ConsumeOrderCompleted ----------

// 主路径：以事件里的 user_id / order_id / 金额为准入账一次。
func TestConsumeOrderCompletedCreditsOnce(t *testing.T) {
	repo := &fakeRepo{}
	svc := newSvc(repo, false)

	err := svc.ConsumeOrderCompleted(context.Background(), events.OrderCompleted{
		OrderID: 42, UserID: 9, TotalAmount: 100,
	})
	if err != nil {
		t.Fatalf("ConsumeOrderCompleted: %v", err)
	}
	if repo.creditCalls != 1 || repo.creditUser != 9 || repo.creditOrderID != 42 || repo.creditDelta != 100 {
		t.Fatalf("CreditOnce 参数错误: calls=%d user=%d order=%d delta=%d",
			repo.creditCalls, repo.creditUser, repo.creditOrderID, repo.creditDelta)
	}
}

// 幂等命中（applied=false）不是错误：吞掉并按正常消费处理，否则驱动会无限重投
// 一条已经入过账的消息。
func TestConsumeOrderCompletedDuplicateIsNoop(t *testing.T) {
	repo := &fakeRepo{dup: true}
	svc := newSvc(repo, false)

	if err := svc.ConsumeOrderCompleted(context.Background(), events.OrderCompleted{
		OrderID: 42, UserID: 9, TotalAmount: 100,
	}); err != nil {
		t.Fatalf("幂等命中必须回 nil（否则驱动无限重投），实际 %v", err)
	}
	if repo.creditCalls != 1 {
		t.Fatalf("仍应尝试入账一次（由仓储判重复），实际 calls=%d", repo.creditCalls)
	}
}

// 非正金额：直接 InvalidArgument，不触碰存储（加 0 或负分是永远对不上账的流水）。
func TestConsumeOrderCompletedRejectsNonPositiveAmount(t *testing.T) {
	for _, amount := range []int64{0, -1} {
		repo := &fakeRepo{}
		svc := newSvc(repo, false)
		err := svc.ConsumeOrderCompleted(context.Background(), events.OrderCompleted{
			OrderID: 42, UserID: 9, TotalAmount: amount,
		})
		if c, _ := codes.From(err); c != codes.InvalidArgument {
			t.Fatalf("amount=%d 应回 InvalidArgument，实际 %v (err=%v)", amount, c, err)
		}
		if repo.creditCalls != 0 {
			t.Fatalf("amount=%d 不得触碰存储: calls=%d", amount, repo.creditCalls)
		}
	}
}

// order_id=0 没有幂等闸门可言：确定性拒绝，同样不触碰存储。
func TestConsumeOrderCompletedRejectsZeroOrderID(t *testing.T) {
	repo := &fakeRepo{}
	svc := newSvc(repo, false)
	err := svc.ConsumeOrderCompleted(context.Background(), events.OrderCompleted{
		OrderID: 0, UserID: 9, TotalAmount: 100,
	})
	if c, _ := codes.From(err); c != codes.InvalidArgument {
		t.Fatalf("order_id=0 应回 InvalidArgument，实际 %v (err=%v)", c, err)
	}
	if repo.creditCalls != 0 {
		t.Fatalf("order_id=0 不得触碰存储: calls=%d", repo.creditCalls)
	}
}

func TestConsumeOrderCompletedPropagatesCreditError(t *testing.T) {
	repo := &fakeRepo{creditErr: errors.New("db down")}
	svc := newSvc(repo, false)
	err := svc.ConsumeOrderCompleted(context.Background(), events.OrderCompleted{
		OrderID: 42, UserID: 9, TotalAmount: 100,
	})
	if c, _ := codes.From(err); c != codes.Internal {
		t.Fatalf("入账失败应回 Internal，实际 %v (err=%v)", c, err)
	}
}

// ---------- GetPoints（IDL 名为 GetAccount） ----------

func TestGetPointsHappyPath(t *testing.T) {
	repo := &fakeRepo{balResult: &model.PointAccount{UserID: 9, Balance: 250}}
	svc := newSvc(repo, false)

	v, err := svc.GetPoints(context.Background(), 9)
	if err != nil {
		t.Fatalf("GetPoints: %v", err)
	}
	if v.UserID != 9 || v.Balance != 250 {
		t.Fatalf("view = %+v", v)
	}
}

// 未开户 = 0 积分：读路径不该因「还没消费过」而报错。
func TestGetPointsMissingAccountIsZero(t *testing.T) {
	repo := &fakeRepo{balErr: gorm.ErrRecordNotFound}
	svc := newSvc(repo, false)

	v, err := svc.GetPoints(context.Background(), 9)
	if err != nil {
		t.Fatalf("未开户应返回 0 积分而非报错: %v", err)
	}
	if v.UserID != 9 || v.Balance != 0 {
		t.Fatalf("view = %+v, want {9 0}", v)
	}
}

func TestGetPointsRepoErrorIsInternal(t *testing.T) {
	repo := &fakeRepo{balErr: errors.New("connection reset")}
	svc := newSvc(repo, false)
	_, err := svc.GetPoints(context.Background(), 9)
	if c, _ := codes.From(err); c != codes.Internal {
		t.Fatalf("库错误应回 Internal，实际 %v (err=%v)", c, err)
	}
}

// ---------- 埋点开关不改变行为 ----------

func TestConsumeOrderCompletedTraceFlagDoesNotChangeBehavior(t *testing.T) {
	for _, traceOn := range []bool{true, false} {
		repo := &fakeRepo{}
		svc := newSvc(repo, traceOn)
		if err := svc.ConsumeOrderCompleted(context.Background(), events.OrderCompleted{
			OrderID: 1, UserID: 1, TotalAmount: 5,
		}); err != nil {
			t.Fatalf("traceOn=%v 行为异常: %v", traceOn, err)
		}
		if repo.creditCalls != 1 {
			t.Fatalf("traceOn=%v 业务未走通", traceOn)
		}
	}
}

func TestGetPointsTraceFlagDoesNotChangeBehavior(t *testing.T) {
	for _, traceOn := range []bool{true, false} {
		repo := &fakeRepo{balErr: gorm.ErrRecordNotFound}
		svc := newSvc(repo, traceOn)
		v, err := svc.GetPoints(context.Background(), 9)
		if err != nil || v.Balance != 0 {
			t.Fatalf("traceOn=%v 行为异常: v=%+v err=%v", traceOn, v, err)
		}
	}
}
