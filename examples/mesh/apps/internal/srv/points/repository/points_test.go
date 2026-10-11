// repository 层单测（sqlite cgo 驱动）：验证 CreditOnce 的 upsert 累加语义、
// 幂等闸门（同一 order_id 只入账一次）、余额与流水**同事务**（记账失败必须把
// 余额一起回滚）、Balance 的「查不到」原样上抛，以及 traceOn 不改变行为。
// 需要 CGO_ENABLED=1（mattn/go-sqlite3）。
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
//
// AutoMigrate 会带上 model 里声明的唯一索引（points_log.order_id）——这不是
// 可有可无的细节：幂等闸门的正确性由它兜底，单测里少了它，「并发下重复插入」
// 这条路径就永远不会被触发，本文件也就测不出真东西。
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

func countRows(t *testing.T, db *gorm.DB, dst any) int64 {
	t.Helper()
	var n int64
	if err := db.Model(dst).Count(&n).Error; err != nil {
		t.Fatalf("count %T: %v", dst, err)
	}
	return n
}

// 首次入账 = 建户一行，余额等于 delta，并落一条流水。
func TestCreditOnceInsertsOnFirstCredit(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)

	applied, err := repo.CreditOnce(context.Background(), 9, 42, 100)
	if err != nil {
		t.Fatalf("CreditOnce: %v", err)
	}
	if !applied {
		t.Fatal("首次入账 applied 应为 true")
	}
	acc, err := repo.Balance(context.Background(), 9)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if acc.Balance != 100 {
		t.Fatalf("balance = %d, want 100", acc.Balance)
	}
	var logs []model.PointsLog
	if err := db.Where("order_id = ?", 42).Find(&logs).Error; err != nil {
		t.Fatalf("read log: %v", err)
	}
	if len(logs) != 1 || logs[0].UserID != 9 || logs[0].Delta != 100 {
		t.Fatalf("points_log = %+v", logs)
	}
}

// 幂等闸门：同一 order_id 的第二次入账必须**完全不生效**（余额不涨、不新增流水），
// 且以 applied=false 明确告知调用方，而不是报错。
//
// 这是「消息重投」（kafka 至少一次 + mqx 重投）的可执行判据：修复前同一事件重投
// 会重复加分，且账面自洽到看不出来。
func TestCreditOnceIsIdempotentPerOrder(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)
	ctx := context.Background()

	if _, err := repo.CreditOnce(ctx, 9, 42, 100); err != nil {
		t.Fatalf("first CreditOnce: %v", err)
	}
	applied, err := repo.CreditOnce(ctx, 9, 42, 100)
	if err != nil {
		t.Fatalf("second CreditOnce: %v", err)
	}
	if applied {
		t.Fatal("同一 order_id 的第二次入账 applied 应为 false（幂等命中）")
	}

	acc, err := repo.Balance(ctx, 9)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if acc.Balance != 100 {
		t.Fatalf("balance = %d, want 100（重投不得重复加分）", acc.Balance)
	}
	if n := countRows(t, db, &model.PointsLog{}); n != 1 {
		t.Fatalf("points_log 行数 = %d, want 1（重投不得新增流水）", n)
	}
}

// 不同订单累加（命中唯一键 → upsert 累加，而不是插出第二行账户）。
//
// 这是「upsert 而非先查后写」的可执行判据：先查后写在并发下会各自查到旧值
// 再写回，两笔各 +100 的结果是 100 而非 200；upsert 把累加交给库的一条语句。
func TestCreditOnceAccumulatesAcrossOrders(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)
	ctx := context.Background()

	if _, err := repo.CreditOnce(ctx, 9, 1, 100); err != nil {
		t.Fatalf("first CreditOnce: %v", err)
	}
	if _, err := repo.CreditOnce(ctx, 9, 2, 50); err != nil {
		t.Fatalf("second CreditOnce: %v", err)
	}

	acc, err := repo.Balance(ctx, 9)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if acc.Balance != 150 {
		t.Fatalf("balance = %d, want 150（应累加，不是覆盖）", acc.Balance)
	}
	if n := countRows(t, db, &model.PointAccount{}); n != 1 {
		t.Fatalf("account 行数 = %d, want 1（绝不能插出第二行）", n)
	}
}

// 两个用户各自成行，互不干扰。
func TestCreditOnceSeparatesUsers(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)
	ctx := context.Background()

	if _, err := repo.CreditOnce(ctx, 1, 1, 10); err != nil {
		t.Fatalf("CreditOnce u1: %v", err)
	}
	if _, err := repo.CreditOnce(ctx, 2, 2, 20); err != nil {
		t.Fatalf("CreditOnce u2: %v", err)
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

// ---------- 失败分支：驱动库错误上抛，且**整体回滚** ----------

// points_account 表缺失（库故障）→ 报错，且先前写入的流水必须被回滚。
//
// 这条判据的正向版本是「余额与流水同事务」：修复前两条语句在事务外，
// 流水会留下来而余额没加 —— 账面上凭空多出一笔无凭据的分。把 points_account
// 拿掉（在旧实现里是「加余额失败、流水已落」）后 points_log 必须为 0 行。
func TestCreditOnceRollsBackLedgerWhenBalanceFails(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)
	if err := db.Migrator().DropTable(&model.PointAccount{}); err != nil {
		t.Fatalf("drop points_account: %v", err)
	}

	applied, err := repo.CreditOnce(context.Background(), 1, 7, 100)
	if err == nil {
		t.Fatal("points_account 表缺失时 CreditOnce 应报错")
	}
	if applied {
		t.Fatal("失败时 applied 必须为 false（不得声称已入账）")
	}
	if n := countRows(t, db, &model.PointsLog{}); n != 0 {
		t.Fatalf("事务应回滚：points_log 残留 %d 行（修复前这里会留下无凭据的流水）", n)
	}
}

// points_log 表缺失 → 报错（且因为它在事务第一步，余额也不会被加）。
func TestCreditOncePropagatesLedgerError(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)
	if err := db.Migrator().DropTable(&model.PointsLog{}); err != nil {
		t.Fatalf("drop points_log: %v", err)
	}
	if _, err := repo.CreditOnce(context.Background(), 1, 7, 1); err == nil {
		t.Fatal("points_log 表缺失时 CreditOnce 应报错")
	}
	if n := countRows(t, db, &model.PointAccount{}); n != 0 {
		t.Fatalf("事务应回滚：points_account 残留 %d 行", n)
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

// traceOn=true：埋点开启不影响业务行为（覆盖命中 / 幂等 / miss 三条路径）。
func TestTraceOnDoesNotChangeBehavior(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, true)
	ctx := context.Background()

	if _, err := repo.CreditOnce(ctx, 9, 42, 100); err != nil {
		t.Fatalf("CreditOnce: %v", err)
	}
	if applied, err := repo.CreditOnce(ctx, 9, 42, 100); err != nil || applied {
		t.Fatalf("幂等路径行为异常：applied=%v err=%v", applied, err)
	}
	if acc, err := repo.Balance(ctx, 9); err != nil || acc.Balance != 100 {
		t.Fatalf("Balance 行为异常：acc=%+v err=%v", acc, err)
	}
	// miss 路径：quiet 命中，但仍原样回 ErrRecordNotFound
	if _, err := repo.Balance(ctx, 99999); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("miss 应回 ErrRecordNotFound，实际 %v", err)
	}
}
