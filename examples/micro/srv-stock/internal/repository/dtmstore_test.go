// dtm 分支面持久层的单测（纯 Go 假驱动，无 cgo、无真库）。
//
// 本文件钉三件事：
//  1. 每个分支落哪几条语句、动哪几列 —— 列名写错在真机上才会炸（unknown column），
//     而炸的时候账已经扣了一半；
//  2. 条件更新没命中时必须判为**业务失败**（库存不足/冻结不足）而不是继续写流水；
//  3. 流水插入与库存更新在同一个 barrier 事务里，不能各走各的。
package repository

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/gospacex/dtmx"
	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/dbmock"
	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/model"
	"gorm.io/gorm"
)

// branchCase 一个分支的用例骨架：跑什么、期望哪几条语句。
type branchCase struct {
	name string
	// run 在事务内调该分支的仓储方法
	run func(r *StockRepository, ctx context.Context, tx *sql.Tx, cmd model.DtmStockCmd) error
	// wantStmts 期望执行到的语句片段（按顺序各出现一次）
	wantStmts []string
	// guard 必须出现在条件更新里的判据片段（防超卖那道闸）
	guard string
	// deficitRows 条件更新被判没命中时的 affected 行数
	deficitRows int64
	// deficitErr 不足时应当能 errors.Is 到的哨兵
	deficitErr error
}

func branchCases() []branchCase {
	return []branchCase{
		{
			name: "Deduct",
			run: func(r *StockRepository, ctx context.Context, tx *sql.Tx, cmd model.DtmStockCmd) error {
				return r.DtmDeduct(ctx, tx, cmd)
			},
			wantStmts: []string{
				"update stocks set total = total - ?, available = available - ?",
				"insert into stock_logs",
			},
			guard:       "and available >= ?",
			deficitRows: 0,
			deficitErr:  ErrInsufficient,
		},
		{
			name: "Release",
			run: func(r *StockRepository, ctx context.Context, tx *sql.Tx, cmd model.DtmStockCmd) error {
				return r.DtmRelease(ctx, tx, cmd)
			},
			wantStmts: []string{
				"update stocks set total = total + ?, available = available + ?",
				"insert into stock_logs",
			},
			// 回补没有下限判据：回补只会把量加回去，加到不存在的商品上才是问题，
			// 那由 affected==0 → ErrNotFound 兜住。
			guard:       "",
			deficitRows: 0,
			deficitErr:  ErrNotFound,
		},
		{
			name: "TryFreeze",
			run: func(r *StockRepository, ctx context.Context, tx *sql.Tx, cmd model.DtmStockCmd) error {
				return r.DtmTryFreeze(ctx, tx, cmd)
			},
			wantStmts: []string{
				"update stocks set available = available - ?, frozen = frozen + ?",
				"insert into stock_logs",
			},
			guard:       "and available >= ?",
			deficitRows: 0,
			deficitErr:  ErrInsufficient,
		},
		{
			name: "ConfirmFreeze",
			run: func(r *StockRepository, ctx context.Context, tx *sql.Tx, cmd model.DtmStockCmd) error {
				return r.DtmConfirmFreeze(ctx, tx, cmd)
			},
			wantStmts: []string{
				"update stocks set total = total - ?, frozen = frozen - ?",
				"insert into stock_logs",
			},
			guard:       "and frozen >= ?",
			deficitRows: 0,
			deficitErr:  ErrFrozenInsufficient,
		},
		{
			name: "CancelFreeze",
			run: func(r *StockRepository, ctx context.Context, tx *sql.Tx, cmd model.DtmStockCmd) error {
				return r.DtmCancelFreeze(ctx, tx, cmd)
			},
			wantStmts: []string{
				"update stocks set available = available + ?, frozen = frozen - ?",
				"insert into stock_logs",
			},
			guard:       "and frozen >= ?",
			deficitRows: 0,
			deficitErr:  ErrFrozenInsufficient,
		},
	}
}

func dtmCmd() model.DtmStockCmd {
	return model.DtmStockCmd{OrderNo: "SO-1", ProductID: 42, Quantity: 3, UserID: 9}
}

// TestDtmBranchWritesLedgerRow 正常路径：库存更新与流水插入都落到同一个事务里。
//
// 反向探针：把某个分支里的 insertIntoLog 调用删掉，本用例必红（少一条语句）。
func TestDtmBranchWritesLedgerRow(t *testing.T) {
	for _, tc := range branchCases() {
		t.Run(tc.name, func(t *testing.T) {
			db := dbmock.New(t)
			repo := NewStockRepository(&gorm.DB{Config: &gorm.Config{ConnPool: db.SQL()}}, false)

			tx, err := db.SQL().Begin()
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			if err := tc.run(repo, context.Background(), tx, dtmCmd()); err != nil {
				t.Fatalf("分支写应成功：%v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}

			for i, frag := range tc.wantStmts {
				if db.ExecCount(frag) != 1 {
					t.Fatalf("第 %d 条语句 %q 未恰好执行一次，实际执行序列：\n  %s",
						i+1, frag, strings.Join(db.Stmts(), "\n  "))
				}
			}
			if tc.guard != "" && db.ExecCount(tc.guard) == 0 {
				t.Fatalf("条件更新缺少判据 %q，实际执行序列：\n  %s",
					tc.guard, strings.Join(db.Stmts(), "\n  "))
			}
		})
	}
}

// TestDtmBranchDeficitIsBusinessFailure 库存/冻结不足必须被判为业务失败：
// dtm 收到业务失败会立刻逆序补偿，收到普通 error 会按退避无限重试 ——
// 把「卖完了」当基础设施故障重试，就是把一次正常拒单放大成无数次回补。
//
// 反向探针：把 DtmDeduct 里的 dtmx.BusinessFailure(...) 换成裸 return err，
// 本用例必红（IsBusinessFailure 变 false）。
func TestDtmBranchDeficitIsBusinessFailure(t *testing.T) {
	for _, tc := range branchCases() {
		t.Run(tc.name, func(t *testing.T) {
			db := dbmock.New(t)
			repo := NewStockRepository(&gorm.DB{Config: &gorm.Config{ConnPool: db.SQL()}}, false)
			db.SetAffected("update stocks", tc.deficitRows)

			tx, err := db.SQL().Begin()
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			err = tc.run(repo, context.Background(), tx, dtmCmd())

			if err == nil {
				t.Fatalf("条件更新未命中却返回 nil —— 分支会把没扣成的那一笔照样写成流水")
			}
			if !dtmx.IsBusinessFailure(err) {
				t.Fatalf("不足必须包 dtmx.BusinessFailure，dtm 才会立刻转补偿；实际 %v", err)
			}
			if !errors.Is(err, tc.deficitErr) {
				t.Fatalf("错误须保留哨兵 %v（service 据此翻译成 4xxx），实际 %v", tc.deficitErr, err)
			}
			if n := db.ExecCount("insert into stock_logs"); n != 0 {
				t.Fatalf("条件更新未命中时不得插流水，实际插了 %d 条", n)
			}
		})
	}
}

// TestDtmBranchRefusesQueriesInBarrierTx 分支事务里不允许查询。
//
// 反向探针：把 DtmTryFreeze 改成先 QueryRow 读 available、再算新值后写回，
// 本用例必红（假驱动对任何 Query 直接报错）。
func TestDtmBranchRefusesQueriesInBarrierTx(t *testing.T) {
	for _, tc := range branchCases() {
		t.Run(tc.name, func(t *testing.T) {
			db := dbmock.New(t)
			repo := NewStockRepository(&gorm.DB{Config: &gorm.Config{ConnPool: db.SQL()}}, false)
			tx, err := db.SQL().Begin()
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			if err := tc.run(repo, context.Background(), tx, dtmCmd()); err != nil {
				t.Fatalf("分支写应成功：%v", err)
			}
			if n := db.BarrierCount(); n != 0 {
				t.Fatalf("仓储层不得自己插 barrier（那是 dtmx.Barrier.Call 的事），实际 %d 条", n)
			}
		})
	}
}

// TestRawDBRejectsBadHandles RawDB 只接受 launchx dbx 交出的 gorm 句柄。
//
// 反向探针：把 RawDB 里的 nil 判断去掉（直接 gdb.DB()），传 nil 会 panic 而不是报错。
func TestRawDBRejectsBadHandles(t *testing.T) {
	if _, err := RawDB(nil); err == nil {
		t.Fatal("nil 句柄必须报错：分支面没有 *sql.DB 就无法把 barrier 插进业务事务，屏障静默失效")
	}
	if _, err := RawDB(&gorm.DB{Config: &gorm.Config{}}); err == nil {
		t.Fatal("没有 ConnPool 的裸 gorm 句柄必须报错：那不是 launchx dbx 交出的连接")
	}
}

// TestRawDBReturnsTheDbxConnection 合格句柄原样取出底层 *sql.DB。
func TestRawDBReturnsTheDbxConnection(t *testing.T) {
	db := dbmock.New(t)
	gdb := &gorm.DB{Config: &gorm.Config{ConnPool: db.SQL()}}

	got, err := RawDB(gdb)
	if err != nil {
		t.Fatalf("RawDB: %v", err)
	}
	if got != db.SQL() {
		t.Fatal("RawDB 必须返回句柄自带的连接池，不能另开一条")
	}
}
