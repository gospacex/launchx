// repository 层单测（sqlite cgo 驱动）：验证 AddBalance 的 upsert 语义
// （唯一键冲突时走累加而非插新行）、Balance 的「查不到」原样上抛、
// 以及单条流水的写入。需要 CGO_ENABLED=1（mattn/go-sqlite3）。
package repository

import (
	"context"
	"errors"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/points/model"
)

// newTestDB 建 sqlite 内存库并迁移两张表。
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.PointAccount{}, &model.PointsLog{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// 首次 AddBalance = 插入一行，余额等于 delta。
func TestAddBalanceInsertsOnFirstEarn(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)

	if err := repo.AddBalance(context.Background(), 9, 100); err != nil {
		t.Fatalf("AddBalance: %v", err)
	}
	acc, err := repo.Balance(context.Background(), 9)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if acc.Balance != 100 {
		t.Fatalf("balance = %d, want 100", acc.Balance)
	}
}

// 再次 AddBalance 必须命中唯一键冲突 → 累加，而不是插出第二行。
//
// 这是「upsert 而非先查后写」的可执行判据：先查后写在并发下会各自查到旧值
// 再写回，两笔各 +100 的结果是 100 而非 200；upsert 把累加交给库的一条语句。
func TestAddBalanceAccumulatesOnConflict(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)
	ctx := context.Background()

	if err := repo.AddBalance(ctx, 9, 100); err != nil {
		t.Fatalf("first AddBalance: %v", err)
	}
	if err := repo.AddBalance(ctx, 9, 50); err != nil {
		t.Fatalf("second AddBalance: %v", err)
	}

	acc, err := repo.Balance(ctx, 9)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if acc.Balance != 150 {
		t.Fatalf("balance = %d, want 150（应累加，不是覆盖）", acc.Balance)
	}

	var n int64
	if err := db.Model(&model.PointAccount{}).Where("user_id = ?", 9).Count(&n).Error; err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	if n != 1 {
		t.Fatalf("account 行数 = %d, want 1（绝不能插出第二行）", n)
	}
}

// 两个用户各自成行，互不干扰。
func TestAddBalanceSeparatesUsers(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)
	ctx := context.Background()

	if err := repo.AddBalance(ctx, 1, 10); err != nil {
		t.Fatalf("AddBalance u1: %v", err)
	}
	if err := repo.AddBalance(ctx, 2, 20); err != nil {
		t.Fatalf("AddBalance u2: %v", err)
	}
	a1, err := repo.Balance(ctx, 1)
	if err != nil {
		t.Fatalf("Balance u1: %v", err)
	}
	a2, err := repo.Balance(ctx, 2)
	if err != nil {
		t.Fatalf("Balance u2: %v", err)
	}
	if a1.Balance != 10 || a2.Balance != 20 {
		t.Fatalf("balances = %d/%d, want 10/20", a1.Balance, a2.Balance)
	}
}

// 查不到：gorm.ErrRecordNotFound 必须原样上抛，由 service 决定「未开户 = 0 分」。
func TestBalancePropagatesErrRecordNotFound(t *testing.T) {
	repo := New(newTestDB(t), false)
	_, err := repo.Balance(context.Background(), 404)
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("want gorm.ErrRecordNotFound, got %v", err)
	}
}

// AddLog 落一条流水并回填自增 ID。
func TestAddLogPersists(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)
	ctx := context.Background()

	l := &model.PointsLog{UserID: 9, OrderID: 42, Delta: 100}
	if err := repo.AddLog(ctx, l); err != nil {
		t.Fatalf("AddLog: %v", err)
	}
	if l.ID == 0 {
		t.Fatal("AddLog 应回填自增 ID")
	}
	var got []model.PointsLog
	if err := db.Where("order_id = ?", 42).Find(&got).Error; err != nil {
		t.Fatalf("read log: %v", err)
	}
	if len(got) != 1 || got[0].UserID != 9 || got[0].Delta != 100 {
		t.Fatalf("points_log = %+v", got)
	}
}

// ---------- 失败分支（驱动库错误上抛） ----------

func TestAddBalancePropagatesDBError(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)
	if err := db.Migrator().DropTable(&model.PointAccount{}); err != nil {
		t.Fatalf("drop points_account: %v", err)
	}
	if err := repo.AddBalance(context.Background(), 1, 1); err == nil {
		t.Fatal("points_account 表缺失时 AddBalance 应报错")
	}
}

func TestBalancePropagatesDBError(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)
	if err := db.Migrator().DropTable(&model.PointAccount{}); err != nil {
		t.Fatalf("drop points_account: %v", err)
	}
	if _, err := repo.Balance(context.Background(), 1); err == nil {
		t.Fatal("points_account 表缺失时 Balance 应报错")
	}
}

func TestAddLogPropagatesDBError(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)
	if err := db.Migrator().DropTable(&model.PointsLog{}); err != nil {
		t.Fatalf("drop points_log: %v", err)
	}
	if err := repo.AddLog(context.Background(), &model.PointsLog{UserID: 1, OrderID: 1, Delta: 1}); err == nil {
		t.Fatal("points_log 表缺失时 AddLog 应报错")
	}
}

// ---------- 分层埋点：quiet 判定与开关不改变行为 ----------

// quietNotFound：ErrRecordNotFound 是正常 miss（豁免），其它错误不豁免。
func TestQuietNotFound(t *testing.T) {
	if !quietNotFound(gorm.ErrRecordNotFound) {
		t.Fatal("ErrRecordNotFound 应被豁免")
	}
	if quietNotFound(errors.New("connection reset")) {
		t.Fatal("库错误不应被豁免")
	}
}

// traceOn=true：埋点开启不影响业务行为（覆盖命中 / miss / 错误三条路径）。
func TestTraceOnDoesNotChangeBehavior(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, true)
	ctx := context.Background()

	if err := repo.AddBalance(ctx, 9, 100); err != nil {
		t.Fatalf("AddBalance: %v", err)
	}
	if err := repo.AddLog(ctx, &model.PointsLog{UserID: 9, OrderID: 42, Delta: 100}); err != nil {
		t.Fatalf("AddLog: %v", err)
	}
	if acc, err := repo.Balance(ctx, 9); err != nil || acc.Balance != 100 {
		t.Fatalf("Balance 行为异常：acc=%+v err=%v", acc, err)
	}
	// miss 路径：quiet 命中，但仍原样回 ErrRecordNotFound
	if _, err := repo.Balance(ctx, 99999); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("miss 应回 ErrRecordNotFound，实际 %v", err)
	}
}
