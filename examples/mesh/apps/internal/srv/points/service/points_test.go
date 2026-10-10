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
type fakeRepo struct {
	addErr    error
	balErr    error
	logErr    error
	balResult *model.PointAccount

	addCalls int
	addUser  uint64
	addDelta int64
	logged   *model.PointsLog
	balCalls int
}

func (f *fakeRepo) AddBalance(_ context.Context, userID uint64, delta int64) error {
	f.addCalls++
	f.addUser, f.addDelta = userID, delta
	return f.addErr
}

func (f *fakeRepo) Balance(context.Context, uint64) (*model.PointAccount, error) {
	f.balCalls++
	if f.balErr != nil {
		return nil, f.balErr
	}
	return f.balResult, nil
}

func (f *fakeRepo) AddLog(_ context.Context, l *model.PointsLog) error {
	f.logged = l
	return f.logErr
}

func newSvc(repo *fakeRepo, traceOn bool) *PointsService {
	return New(repo, traceOn)
}

// ---------- ConsumeOrderCompleted ----------

// 主路径：加余额 + 记一条流水，且都以事件里的 user_id / order_id / 金额为准。
func TestConsumeOrderCompletedAddsBalanceAndLog(t *testing.T) {
	repo := &fakeRepo{}
	svc := newSvc(repo, false)

	err := svc.ConsumeOrderCompleted(context.Background(), events.OrderCompleted{
		OrderID: 42, UserID: 9, TotalAmount: 100,
	})
	if err != nil {
		t.Fatalf("ConsumeOrderCompleted: %v", err)
	}
	if repo.addCalls != 1 || repo.addUser != 9 || repo.addDelta != 100 {
		t.Fatalf("AddBalance 参数错误: calls=%d user=%d delta=%d", repo.addCalls, repo.addUser, repo.addDelta)
	}
	if repo.logged == nil {
		t.Fatal("应落一条积分流水")
	}
	if repo.logged.OrderID != 42 || repo.logged.UserID != 9 || repo.logged.Delta != 100 {
		t.Fatalf("流水 = %+v", repo.logged)
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
		if repo.addCalls != 0 || repo.logged != nil {
			t.Fatalf("amount=%d 不得触碰存储: addCalls=%d", amount, repo.addCalls)
		}
	}
}

func TestConsumeOrderCompletedPropagatesAddBalanceError(t *testing.T) {
	repo := &fakeRepo{addErr: errors.New("db down")}
	svc := newSvc(repo, false)
	err := svc.ConsumeOrderCompleted(context.Background(), events.OrderCompleted{
		OrderID: 42, UserID: 9, TotalAmount: 100,
	})
	if c, _ := codes.From(err); c != codes.Internal {
		t.Fatalf("加余额失败应回 Internal，实际 %v (err=%v)", c, err)
	}
	if repo.logged != nil {
		t.Fatal("加余额失败不得再写流水")
	}
}

func TestConsumeOrderCompletedPropagatesAddLogError(t *testing.T) {
	repo := &fakeRepo{logErr: errors.New("db down")}
	svc := newSvc(repo, false)
	err := svc.ConsumeOrderCompleted(context.Background(), events.OrderCompleted{
		OrderID: 42, UserID: 9, TotalAmount: 100,
	})
	if c, _ := codes.From(err); c != codes.Internal {
		t.Fatalf("写流水失败应回 Internal，实际 %v (err=%v)", c, err)
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
		if repo.addCalls != 1 || repo.logged == nil {
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
