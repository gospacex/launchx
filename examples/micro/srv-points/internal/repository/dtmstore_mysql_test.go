package repository

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/dtm-labs/client/dtmcli"
	"google.golang.org/grpc/metadata"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/gospacex/dtmx"
	"github.com/gospacex/launchx/examples/micro/srv-points/internal/model"
)

// 本文件是「夹具说的」与「真库做的」之间的那一次对齐。
//
// 为什么需要它：dtmstub 的列名与唯一键是本包自己抄进内存表的 —— 两边抄错
// 同一个列，夹具和被测代码会一起绿。故把同一批用例再在**真 MySQL** 上跑一遍：
// 真库对不存在的列、对不上的唯一键、对不上的 `insert ignore` 都会当场报错。
//
// 口径：默认跳过，只有 HUBX_DTM_TEST_DSN 非空时才跑。DSN 必须指向一个**可丢弃
// 的库**（本机验证用的是 points_dtmbranch_test）：本文件会建表并清表。
// 不做成默认是因为 `go test ./...` 不能依赖本机有 MySQL，也不能往 points_db
// 这种真实业务库里写测试数据。
//
// 跑法（仓库根目录）：
//
//	HUBX_DTM_TEST_DSN='mall_rw:***@tcp(127.0.0.1:3306)/points_dtmbranch_test?charset=utf8mb4&parseTime=True' \
//	  go test ./examples/micro/srv-points/internal/repository/ -count=1 -run RealMysql

// realDDL 逐字取自 examples/micro/scripts/schema-micro.sql 的 DTM 段与
// examples/mall/scripts/schema.sql 的 points 段。写成字符串而不是 import 脚本：
// 建表脚本里有 USE <db>，没法在连接里执行。
const realDDL = `
CREATE TABLE IF NOT EXISTS point_accounts (
  id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  user_id      BIGINT UNSIGNED NOT NULL,
  balance      BIGINT       NOT NULL DEFAULT 0,
  total_earned BIGINT       NOT NULL DEFAULT 0,
  total_spent  BIGINT       NOT NULL DEFAULT 0,
  updated_at   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  frozen       BIGINT       NOT NULL DEFAULT 0,
  PRIMARY KEY (id),
  UNIQUE KEY uk_pa_user (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS point_ledgers (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  user_id       BIGINT UNSIGNED NOT NULL,
  order_no      VARCHAR(40)  NOT NULL,
  type          TINYINT      NOT NULL DEFAULT 1,
  amount        BIGINT       NOT NULL DEFAULT 0,
  balance_after BIGINT       NOT NULL DEFAULT 0,
  remark        VARCHAR(128) NOT NULL DEFAULT '',
  created_at    DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_ledger_order_type (order_no, type),
  KEY idx_ledger_user (user_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS barrier (
  id          BIGINT      NOT NULL AUTO_INCREMENT,
  trans_type  VARCHAR(45) NOT NULL DEFAULT '',
  gid         VARCHAR(128) NOT NULL DEFAULT '',
  branch_id   VARCHAR(128) NOT NULL DEFAULT '',
  op          VARCHAR(45) NOT NULL DEFAULT '',
  barrier_id  VARCHAR(45) NOT NULL DEFAULT '',
  reason      VARCHAR(45) NOT NULL DEFAULT '',
  create_time DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  update_time DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  KEY idx_barrier_create_time (create_time),
  KEY idx_barrier_update_time (update_time),
  UNIQUE KEY uniq_barrier (gid, branch_id, op, barrier_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`

func realMySQLStore(t *testing.T) (*DtmBranchStore, *gorm.DB) {
	t.Helper()
	dsn := os.Getenv("HUBX_DTM_TEST_DSN")
	if dsn == "" {
		t.Skip("未设 HUBX_DTM_TEST_DSN，跳过真库复核（DSN 须指向可丢弃的库）")
	}
	dtmx.SetBarrierTableName("barrier")

	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("连真 MySQL 失败：%v", err)
	}
	sdb, err := db.DB()
	if err != nil {
		t.Fatalf("取底层连接失败：%v", err)
	}
	for _, stmt := range splitDDL(realDDL) {
		if _, err := sdb.Exec(stmt); err != nil {
			t.Fatalf("建表失败（%s）：%v", firstLine(stmt), err)
		}
	}
	// 清表：本文件可重复运行，且断言只关心相对形态。
	for _, tbl := range []string{"barrier", "point_ledgers", "point_accounts"} {
		if _, err := sdb.Exec("DELETE FROM " + tbl); err != nil {
			t.Fatalf("清表 %s 失败：%v", tbl, err)
		}
	}
	store, err := NewDtmBranchStore(db, true)
	if err != nil {
		t.Fatalf("构造分支写通道失败：%v", err)
	}
	return store, db
}

func splitDDL(ddl string) []string {
	var out []string
	for _, part := range strings.Split(ddl, ";") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}

func realCtx(gid, branchID, op string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"dtm-gid", gid,
		"dtm-trans_type", "saga",
		"dtm-branch_id", branchID,
		"dtm-op", op,
	))
}

// TestRealMysqlSagaDebitAndRefund 在真库上跑 Saga 正向 + 补偿 + 幂等。
func TestRealMysqlSagaDebitAndRefund(t *testing.T) {
	store, db := realMySQLStore(t)
	seedReal(t, db, 1001, 100)

	act := realCtx("real-saga-1", "01", "action")
	if err := store.Debit(act, barrierOf(t, act), model.DtmBiz{UserID: 1001, Cost: 30, OrderNo: "RS-1"}); err != nil {
		t.Fatalf("真库 Debit: %v", err)
	}
	// 重放（同一 gid）：必须不二次扣账 —— 这条在真库上还要额外过一层 dtm 官方
	// barrier 的 `insert ignore`，夹具是照着它的语义实现的，真库才是权威。
	if err := store.Debit(act, barrierOf(t, act), model.DtmBiz{UserID: 1001, Cost: 30, OrderNo: "RS-1"}); err != nil {
		t.Fatalf("真库重放 Debit: %v", err)
	}
	assertRealBalance(t, db, 1001, 70, 0, 30)

	cp := realCtx("real-saga-1", "01", "compensate")
	if err := store.Refund(cp, barrierOf(t, cp), model.DtmBiz{UserID: 1001, Cost: 30, OrderNo: "RS-1"}); err != nil {
		t.Fatalf("真库 Refund: %v", err)
	}
	assertRealBalance(t, db, 1001, 100, 0, 30)
	if got := realCount(t, db, "point_ledgers"); got != 2 {
		t.Fatalf("真库流水行数 = %d，want 2（消费 + 回补）", got)
	}
	if got := realCount(t, db, "barrier"); got < 1 {
		t.Fatalf("真库 barrier 行数 = %d，want >=1", got)
	}
}

// TestRealMysqlInsufficientRollsBackLedger：余额不足时同事务里那条流水也不能留。
func TestRealMysqlInsufficientRollsBackLedger(t *testing.T) {
	store, db := realMySQLStore(t)
	seedReal(t, db, 1001, 10)

	ctx := realCtx("real-saga-2", "01", "action")
	err := store.Debit(ctx, barrierOf(t, ctx), model.DtmBiz{UserID: 1001, Cost: 30, OrderNo: "RS-2"})
	if !errors.Is(err, dtmcli.ErrFailure) {
		t.Fatalf("真库余额不足须带官方哨兵，实际 err=%v", err)
	}
	assertRealBalance(t, db, 1001, 10, 0, 0)
	if got := realCount(t, db, "point_ledgers"); got != 0 {
		t.Fatalf("真库流水行数 = %d，want 0（同事务回滚）", got)
	}
	if got := realCount(t, db, "barrier"); got != 0 {
		t.Fatalf("真库 barrier 行数 = %d，want 0（业务失败时 barrier 行随事务回滚）", got)
	}
}

// TestRealMysqlTccFreezeCycle：Try → Confirm 在真库上闭环，并验 ledger 唯一键
// 在真 MySQL 上真的拦住第二次插入。
func TestRealMysqlTccFreezeCycle(t *testing.T) {
	store, db := realMySQLStore(t)
	seedReal(t, db, 1001, 100)

	tryCtx := realCtx("real-tcc-1", "01", "try")
	if err := store.TryFreeze(tryCtx, barrierOf(t, tryCtx), model.DtmBiz{UserID: 1001, Cost: 40, OrderNo: "RT-1"}); err != nil {
		t.Fatalf("真库 TryFreeze: %v", err)
	}
	assertRealBalance(t, db, 1001, 60, 40, 0)

	cfmCtx := realCtx("real-tcc-1", "01", "confirm")
	if err := store.ConfirmFreeze(cfmCtx, barrierOf(t, cfmCtx), model.DtmBiz{UserID: 1001, Cost: 40, OrderNo: "RT-1"}); err != nil {
		t.Fatalf("真库 ConfirmFreeze: %v", err)
	}
	assertRealBalance(t, db, 1001, 60, 0, 40)
	if got := realCount(t, db, "point_ledgers"); got != 1 {
		t.Fatalf("真库 Confirm 后流水行数 = %d，want 1", got)
	}

	// 换 gid 重放同一笔（barrier 不可信的情形）：靠 (order_no, type) 唯一键挡住。
	// 这一条在真库上才有意义 —— 夹具的「条件插入」是我实现的语义，真 MySQL 才是
	// 「insert … select … where not exists + 唯一键」的实际裁决者。
	again := realCtx("real-tcc-1-dup", "01", "confirm")
	if err := store.ConfirmFreeze(again, barrierOf(t, again), model.DtmBiz{UserID: 1001, Cost: 40, OrderNo: "RT-1"}); err != nil {
		t.Fatalf("真库重放 Confirm 应幂等成功：%v", err)
	}
	assertRealBalance(t, db, 1001, 60, 0, 40)
	if got := realCount(t, db, "point_ledgers"); got != 1 {
		t.Fatalf("真库重放后流水行数 = %d，want 1（唯一键必须挡住二次实扣）", got)
	}

	// Cancel：冻结已被 Confirm 消掉 → 业务失败，不得凭空回分
	cxlCtx := realCtx("real-tcc-1", "01", "cancel")
	if err := store.CancelFreeze(cxlCtx, barrierOf(t, cxlCtx), model.DtmBiz{UserID: 1001, Cost: 40, OrderNo: "RT-1"}); !errors.Is(err, ErrDtmFrozenShort) {
		t.Fatalf("真库已实扣后 Cancel 应判冻结不足，实际 err=%v", err)
	}
	assertRealBalance(t, db, 1001, 60, 0, 40)
}

// TestRealMysqlCancelRestoresBalance：真库上的 Try → Cancel 闭环。
func TestRealMysqlCancelRestoresBalance(t *testing.T) {
	store, db := realMySQLStore(t)
	seedReal(t, db, 1001, 100)

	tryCtx := realCtx("real-tcc-2", "01", "try")
	if err := store.TryFreeze(tryCtx, barrierOf(t, tryCtx), model.DtmBiz{UserID: 1001, Cost: 40, OrderNo: "RT-2"}); err != nil {
		t.Fatalf("真库 TryFreeze: %v", err)
	}
	cxlCtx := realCtx("real-tcc-2", "01", "cancel")
	if err := store.CancelFreeze(cxlCtx, barrierOf(t, cxlCtx), model.DtmBiz{UserID: 1001, Cost: 40, OrderNo: "RT-2"}); err != nil {
		t.Fatalf("真库 CancelFreeze: %v", err)
	}
	assertRealBalance(t, db, 1001, 100, 0, 0)
	if got := realCount(t, db, "point_ledgers"); got != 0 {
		t.Fatalf("取消不得落流水，真实行数 = %d", got)
	}
}

// ---- 真库断言工具 ----

func seedReal(t *testing.T, db *gorm.DB, userID, balance int64) {
	t.Helper()
	if err := db.Exec(
		`INSERT INTO point_accounts (user_id, balance, frozen, total_earned, total_spent) VALUES (?, ?, 0, ?, 0)`,
		userID, balance, balance).Error; err != nil {
		t.Fatalf("种账户失败：%v", err)
	}
}

func assertRealBalance(t *testing.T, db *gorm.DB, userID, wantBalance, wantFrozen, wantSpent int64) {
	t.Helper()
	var acc struct {
		Balance    int64
		Frozen     int64
		TotalSpent int64
	}
	if err := db.Raw(`SELECT balance, frozen, total_spent FROM point_accounts WHERE user_id = ?`, userID).
		Scan(&acc).Error; err != nil {
		t.Fatalf("查账户失败：%v", err)
	}
	if acc.Balance != wantBalance || acc.Frozen != wantFrozen || acc.TotalSpent != wantSpent {
		t.Fatalf("真库账户应为 balance=%d frozen=%d total_spent=%d，实际 %+v",
			wantBalance, wantFrozen, wantSpent, acc)
	}
}

func realCount(t *testing.T, db *gorm.DB, table string) int64 {
	t.Helper()
	var n int64
	if err := db.Raw("SELECT COUNT(*) FROM " + table).Scan(&n).Error; err != nil {
		t.Fatalf("count %s 失败：%v", table, err)
	}
	return n
}

var _ = sql.ErrNoRows
