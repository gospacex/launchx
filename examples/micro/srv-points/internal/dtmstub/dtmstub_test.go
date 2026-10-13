package dtmstub

import (
	"context"
	"database/sql"
	"testing"
)

// 本文件验夹具自己。
//
// 为什么夹具也需要单测：它一旦说谎，被它验过的所有用例会一起绿 —— 而且绿的
// 理由是假的。夹具最容易出的三类错，逐条在这里钉死：
//   - 「事务」不是真事务（回滚不丢数据）→ 「业务失败必须把同事务的写入一起
//     撤掉」这类断言全部失真；
//   - 「insert ignore」不是真 ignore（重复插入报错或返回 1）→ barrier 的幂等
//     / 空补偿判据全错，而用例照样通过；
//   - 实参按「先守卫后赋值」绑（而 SQL 里 SET 的占位符在 WHERE 之前）→
//     守卫值与写入值互换，测试仍然绿。

func open(t *testing.T) (*DB, *sql.DB) {
	t.Helper()
	mem := New()
	mem.Seed(1001, 100)
	return mem, mem.SQL()
}

func TestRollbackDiscardsEverythingInTheTransaction(t *testing.T) {
	mem, db := open(t)
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`insert into point_ledgers (user_id, order_no, type, amount, balance_after, remark) `+
			`select ?, ?, ?, ?, ?, ? from dual `+
			`where not exists (select 1 from point_ledgers where order_no = ? and type = ?)`,
		1001, "SO-TX", 2, 30, 70, "probe", "SO-TX", 2); err != nil {
		t.Fatalf("insert ledger in tx: %v", err)
	}
	if got := len(mem.Ledgers()); got != 0 {
		t.Fatalf("未提交时外部不该看到行，实际 %d 条", got)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	// 回滚后再查一次：确认「回滚」不是只清掉了未提交的快照视图。
	db.QueryRowContext(ctx, `select balance from point_accounts where user_id = ?`, 1001).Scan(new(int64))
	if got := len(mem.Ledgers()); got != 0 {
		t.Fatalf("回滚后流水应不存在，实际 %d 条：%+v", got, mem.Ledgers())
	}
}

func TestCommitKeepsRows(t *testing.T) {
	mem, db := open(t)
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`insert into point_ledgers (user_id, order_no, type, amount, balance_after, remark) `+
			`select ?, ?, ?, ?, ?, ? from dual `+
			`where not exists (select 1 from point_ledgers where order_no = ? and type = ?)`,
		1001, "SO-OK", 2, 30, 70, "probe", "SO-OK", 2); err != nil {
		t.Fatalf("insert ledger in tx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := len(mem.Ledgers()); got != 1 {
		t.Fatalf("提交后应有 1 条流水，实际 %d", got)
	}
}

// TestInsertIgnoreIsReallyIgnore：dtm 的 barrier 完全建立在「重复插入不报错且
// 受影响 0 行」之上。这条错了，三条守卫（重复回调/空补偿/悬挂）一起失效。
func TestInsertIgnoreIsReallyIgnore(t *testing.T) {
	mem, db := open(t)
	ctx := context.Background()
	stmt := `insert ignore into barrier(trans_type, gid, branch_id, op, barrier_id, reason) values(?,?,?,?,?,?)`

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	res, err := tx.ExecContext(ctx, stmt, "saga", "g1", "01", "action", "01", "action")
	if err != nil {
		t.Fatalf("首次 barrier 插入不应报错：%v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("首次插入受影响行数 = %d，want 1", n)
	}
	res, err = tx.ExecContext(ctx, stmt, "saga", "g1", "01", "action", "01", "action")
	if err != nil {
		t.Fatalf("重复 barrier 插入必须被 ignore（不报错）：%v", err)
	}
	if n, _ := res.RowsAffected(); n != 0 {
		t.Fatalf("重复插入受影响行数 = %d，want 0（dtm 靠它判「已落过账」）", n)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := mem.Barriers(); got != 1 {
		t.Fatalf("库中 barrier 行数 = %d，want 1（ignore 不是跳过写入而是跳过重复）", got)
	}
}

// TestUpdateBindsSetArgsBeforeWhereArgs：守卫额与写入额互换时，扣款会写成
// 「把余额设成守卫值」而断言只盯「有没有变」就放过。
func TestUpdateBindsSetArgsBeforeWhereArgs(t *testing.T) {
	mem, db := open(t)
	ctx := context.Background()
	res, err := db.ExecContext(ctx,
		`update point_accounts set balance = balance - ?, total_spent = total_spent + ? `+
			`where user_id = ? and balance >= ?`, 30, 30, 1001, 30)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("受影响行数 = %d，want 1", n)
	}
	acc := mem.Account(1001)
	if acc.Balance != 70 || acc.TotalSpent != 30 {
		t.Fatalf("实参绑定错位：应 balance=70 total_spent=30，实际 %+v", acc)
	}

	// 守卫不成立 → 受影响 0 行，且行内容不变
	res, err = db.ExecContext(ctx,
		`update point_accounts set balance = balance - ?, total_spent = total_spent + ? `+
			`where user_id = ? and balance >= ?`, 500, 500, 1001, 500)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 0 {
		t.Fatalf("守卫不成立时受影响行数 = %d，want 0（这是「余额不足」的判据）", n)
	}
	if acc := mem.Account(1001); acc.Balance != 70 {
		t.Fatalf("守卫不成立时账户不许被改动，实际 balance=%d", acc.Balance)
	}
}

// TestUnknownStatementIsAnError：夹具必须拒绝没登记的语句。
//
// 放行未知语句的夹具会把「生产 SQL 拼错了」变成一次绿色测试 —— 那时本文件
// 验的就不是业务逻辑，而只是「代码跑完了」。
func TestUnknownStatementIsAnError(t *testing.T) {
	_, db := open(t)
	if _, err := db.ExecContext(context.Background(),
		`update point_accounts set balance = 1`); err == nil {
		t.Fatal("未登记的语句必须报错（夹具放行 = 生产 SQL 拼错也测得过）")
	}
	if _, err := db.QueryContext(context.Background(),
		`select total_spent from point_accounts where user_id = ?`, 1001); err == nil {
		t.Fatal("未登记的读语句必须报错（取错列会让被测代码拿到别的语义的值还测得过）")
	}
}
