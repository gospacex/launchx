// dtm 分支面持久层的单测（纯 Go 假驱动，无 cgo、无真库）。
//
// 本文件钉六件事：
//  1. 每个分支落哪几条语句、动哪几列 —— 列名写错在真机上才会炸（unknown column），
//     而炸的时候账已经扣了一半；
//  2. 条件更新没命中时必须判为**业务失败**而不是继续写事件；
//  3. 三表写（orders / order_items / outbox）在同一个 barrier 事务里，且业务失败时
//     整笔回滚（假驱动是真事务，回滚断言因此成立而不是靠顺序假装成立）；
//  4. 口径②：outbox 行**只在 ConfirmFreeze / CreateOrder 之后出现**；
//  5. 口径②：作废补偿（CancelOrder / CancelFreeze）**不重复落 outbox**；
//  6. 口径③（三态不变式）：占位单存在时不得既写正式单又留占位 —— 全程只有
//     一行 orders，ConfirmFreeze 是**原地改**而不是再插一行。
//
// 与 srv-stock 同名文件的差别：那边断言「执行了哪几条语句」，这边必须断言
// 「表里有没有那一行」—— 订单域最要紧的判据是 outbox 行的有无。
package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gospacex/dtmx"
	"gorm.io/gorm"

	"github.com/gospacex/launchx/examples/micro/srv-order/internal/dbmock"
	"github.com/gospacex/launchx/examples/micro/srv-order/internal/model"
)

// 分支 SQL 片段（断言用；与 dtmstore.go 的常量一一对应）。
const (
	fragInsertOrder    = "insert ignore into orders"
	fragInsertItem     = "insert into order_items"
	fragInsertLog      = "insert into order_status_log"
	fragInsertOutbox   = "insert into outbox"
	fragSelectOrderID  = "select id from orders"
	fragCancelOrder    = "update orders set status = ? where order_no = ? and dtm_gid = ? and status = ?"
	fragConfirmFreeze  = "update orders set status = ?, paid_amount = total_amount where order_no = ? and dtm_gid = ? and status = ?"
	fragOrderNoGuard   = "order_no = ? and dtm_gid = ?"
	fragPresaleGuard   = "and status = ?"
	outboxOrderCreated = "order.completed"
)

// dtmCmd 测试载荷：单号 + gid 是两个定位键，缺一不可（见 model.DtmOrderCmd）。
func dtmCmd() model.DtmOrderCmd {
	return model.DtmOrderCmd{
		UserID:  7,
		OrderNo: "O-DTM-1",
		DtmGID:  "gid-1",
		Items: []model.OrderItemCmd{
			{ProductID: 11, Name: "sku-11", Quantity: 2, UnitPrice: 150},
			{ProductID: 12, Name: "sku-12", Quantity: 1, UnitPrice: 50},
		},
		Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	}
}

// dtmFill 事件填充器：payload 里带上回读到的 order_id（与 HTTP 面同构，
// 下游按 order_no 对账时还能用 id 反查）。
func dtmFill(cmd model.DtmOrderCmd) OutboxFill {
	return func(orderID uint64) (string, string) {
		return fmt.Sprintf(`{"order_id":%d,"order_no":%q}`, orderID, cmd.OrderNo), cmd.Traceparent
	}
}

// newDtmRepo 造一个只挂 dbx 句柄的仓储。
func newDtmRepo(db *dbmock.DB) *OrderRepository {
	return NewOrderRepository(&gorm.DB{Config: &gorm.Config{ConnPool: db.SQL()}}, false)
}

// inTx 在事务内跑一次分支写（branch 由调用方给，通常直接调仓储方法）。
func inTx(t *testing.T, db *dbmock.DB, body func(tx *sql.Tx) error) {
	t.Helper()
	tx, err := db.SQL().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := body(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("分支写应成功：%v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// runTx 同 inTx，但把分支写的返回值与错误交回调用方（写失败不 Fatal），
// 用来断言「业务失败 → 整笔回滚」。
func runTx(t *testing.T, db *dbmock.DB, body func(tx *sql.Tx) (uint64, error)) (uint64, error) {
	t.Helper()
	tx, err := db.SQL().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	id, err := body(tx)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return id, nil
}

// seedOrder 预置一行订单（构造「不存在 / 状态不是目标态」的起点）。
func seedOrder(db *dbmock.DB, o dbmock.OrderRow) {
	db.SeedOrder(o)
}

func createdOrder(cmd model.DtmOrderCmd) dbmock.OrderRow {
	return dbmock.OrderRow{
		UserID: int64(cmd.UserID), OrderNo: cmd.OrderNo, Status: model.StatusCreated,
		TotalAmount: cmd.Total(), DtmGID: cmd.DtmGID,
	}
}

func placeholderOrder(cmd model.DtmOrderCmd) dbmock.OrderRow {
	o := createdOrder(cmd)
	o.Status = model.StatusPresaleLocked
	return o
}

// TestDtmCreateOrderWritesThreeTablesInOneTx 正常路径：订单 + 明细 + outbox 事件
// 一次提交，且事件 payload 里带的是刚落库那行的自增 id。
//
// 反向探针：把 DtmCreateOrder 里的明细插入删掉，本用例必红（Items() 为空）。
func TestDtmCreateOrderWritesThreeTablesInOneTx(t *testing.T) {
	db := dbmock.New(t)
	repo := newDtmRepo(db)
	cmd := dtmCmd()

	var orderID uint64
	inTx(t, db, func(tx *sql.Tx) error {
		var err error
		orderID, err = repo.DtmCreateOrder(context.Background(), tx, cmd, dtmFill(cmd))
		return err
	})

	if orderID == 0 {
		t.Fatalf("必须回传自增 order_id（service 靠它失效详情缓存），实得 %d", orderID)
	}
	o, ok := db.OrderByNo(cmd.OrderNo)
	if !ok {
		t.Fatalf("订单未落库：%v", db.Stmts())
	}
	if o.Status != model.StatusCreated {
		t.Errorf("status = %q, want %q", o.Status, model.StatusCreated)
	}
	if o.DtmGID != cmd.DtmGID {
		t.Errorf("dtm_gid = %q, want %q（占位与补偿按它定位）", o.DtmGID, cmd.DtmGID)
	}
	if o.TotalAmount != cmd.Total() {
		t.Errorf("total_amount = %d, want %d（Σ quantity × unit_price，服务端算）", o.TotalAmount, cmd.Total())
	}
	if n := len(db.Items()); n != len(cmd.Items) {
		t.Fatalf("明细行数 = %d, want %d：%v", n, len(cmd.Items), db.Stmts())
	}
	for _, it := range db.Items() {
		if uint64(it.OrderID) != orderID {
			t.Errorf("明细 order_id = %d, want %d（明细挂错单 = 账实不符）", it.OrderID, orderID)
		}
	}
	evs := db.Outboxes()
	if len(evs) != 1 {
		t.Fatalf("outbox 行数 = %d, want 1（Saga 正向要发下单完成事件）：%v", len(evs), db.Stmts())
	}
	if !strings.Contains(evs[0].Payload, fmt.Sprintf(`"order_id":%d`, orderID)) {
		t.Errorf("事件 payload 必须带刚落库的 order_id，实得 %q", evs[0].Payload)
	}
	if evs[0].Traceparent != cmd.Traceparent {
		t.Errorf("outbox.traceparent = %q, want %q（补发时靠它续回同一条 trace）",
			evs[0].Traceparent, cmd.Traceparent)
	}
	if evs[0].EventType != outboxOrderCreated {
		t.Errorf("event_type = %q, want %q", evs[0].EventType, outboxOrderCreated)
	}
	if evs[0].Status != 0 {
		t.Errorf("outbox.status = %d, want 0（StatusPending，Runner 只挑 status=0 的行）", evs[0].Status)
	}
}

// TestDtmCreateOrderReplayOnSameOrderNoIsIdempotent 同一个 order_no 在第二个全局
// 事务里再走一遍 → insert ignore 命中 uk_order_no 返回 0 行 → 本次不插明细、
// 不写事件，且**报成功**（重放不是失败；报失败会让 dtm 把它当基础故障重试）。
//
// 反向探针：把 DtmCreateOrder 里的 `if affected == 0 { return 0, nil }` 删掉
// （改成继续插明细），本用例必红（明细变成 4 行、事件 2 条）。
func TestDtmCreateOrderReplayOnSameOrderNoIsIdempotent(t *testing.T) {
	db := dbmock.New(t)
	repo := newDtmRepo(db)
	first := dtmCmd()
	seedOrder(db, createdOrder(first))

	// 第二个全局事务：同一个单号，不同 gid。
	replay := dtmCmd()
	replay.DtmGID = "gid-2"
	inTx(t, db, func(tx *sql.Tx) error {
		_, err := repo.DtmCreateOrder(context.Background(), tx, replay, dtmFill(replay))
		return err
	})

	if n := len(db.Orders()); n != 1 {
		t.Fatalf("重放不得产生第二张单，实际 %d 行：%v", n, db.Stmts())
	}
	if n := len(db.Items()); n != 0 {
		t.Fatalf("重放不得重复插明细，实际 %d 行", n)
	}
	if n := len(db.Outboxes()); n != 0 {
		t.Fatalf("重放不得重复落 outbox（事件插重了下游会重复送积分），实际 %d 条", n)
	}
}

// TestDtmCancelOrderMarksCancelledWithoutOutbox Saga 补偿：单子置终态 canceled
// 并记一条状态流转，**绝不写 outbox**。
//
// 反向探针：把 DtmCancelOrder 改成也调写 outbox 的那条 helper，本用例必红
// （outbox 行数从 0 变 1）。
func TestDtmCancelOrderMarksCancelledWithoutOutbox(t *testing.T) {
	db := dbmock.New(t)
	repo := newDtmRepo(db)
	cmd := dtmCmd()
	seedOrder(db, createdOrder(cmd))

	orderID, err := runTx(t, db, func(tx *sql.Tx) (uint64, error) {
		return repo.DtmCancelOrder(context.Background(), tx, cmd)
	})
	if err != nil {
		t.Fatalf("作废补偿应成功：%v", err)
	}

	o, _ := db.OrderByNo(cmd.OrderNo)
	if o.Status != model.StatusCanceled {
		t.Errorf("status = %q, want %q", o.Status, model.StatusCanceled)
	}
	logs := db.StatusLogs()
	if len(logs) != 1 {
		t.Fatalf("状态流转行数 = %d, want 1（本域纪律：状态与日志同事务推进）", len(logs))
	}
	if uint64(logs[0].OrderID) != orderID || logs[0].FromStatus != model.StatusCreated ||
		logs[0].ToStatus != model.StatusCanceled {
		t.Errorf("状态流转行不对：%+v（order_id=%d）", logs[0], orderID)
	}
	if n := len(db.Outboxes()); n != 0 {
		t.Fatalf("作废补偿不得落 outbox，实际 %d 条：%v", n, db.Stmts())
	}
	if db.ExecCount(fragInsertOrder) != 0 {
		t.Fatal("作废补偿不得再插一行订单（那是新单，不是作废）")
	}
}

// TestDtmCancelOrderRejectsForeignOrWrongState 定位键不匹配（别人的 gid）或状态
// 不是 created → 业务失败，且**一行都不许改**。
//
// 反向探针：把 DtmCancelOrder 的 WHERE 里的 `and dtm_gid = ?` 删掉，本用例必红
// （A 的全局事务会去作废 B 的单）。
func TestDtmCancelOrderRejectsForeignOrWrongState(t *testing.T) {
	cases := []struct {
		name string
		row  dbmock.OrderRow
		cmd  model.DtmOrderCmd
	}{
		{
			name: "另一个全局事务的单",
			row:  dbmock.OrderRow{UserID: 7, OrderNo: "O-DTM-1", Status: model.StatusCreated, DtmGID: "gid-other"},
			cmd:  dtmCmd(),
		},
		{
			name: "已经是终态",
			row:  dbmock.OrderRow{UserID: 7, OrderNo: "O-DTM-1", Status: model.StatusPaid, DtmGID: "gid-1"},
			cmd:  dtmCmd(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := dbmock.New(t)
			repo := newDtmRepo(db)
			seedOrder(db, tc.row)

			tx, err := db.SQL().Begin()
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			_, err = repo.DtmCancelOrder(context.Background(), tx, tc.cmd)
			if err == nil {
				t.Fatal("定位不到 / 状态不允许时必须报错，不能静默当成功 —— 那会让全局事务以为回滚完了")
			}
			if !dtmx.IsBusinessFailure(err) {
				t.Fatalf("必须包 dtmx.BusinessFailure，dtm 才会不重试；实际 %v", err)
			}
			if !errors.Is(err, ErrStatusNotAllowed) {
				t.Fatalf("错误须保留哨兵 ErrStatusNotAllowed（service 据此翻成 3xxx），实际 %v", err)
			}
			_ = tx.Rollback()

			o, _ := db.OrderByNo(tc.row.OrderNo)
			if o.Status != tc.row.Status {
				t.Errorf("被拒的写不得留下痕迹：status %q → %q", tc.row.Status, o.Status)
			}
			if n := len(db.StatusLogs()); n != 0 {
				t.Errorf("被拒时不得记状态流转，实际 %d 行", n)
			}
			if db.ExecCount(fragInsertLog) != 0 {
				// 判据没命中就必须在写日志**之前**返回：先记日志再更新的话，
				// 这条「没发生的流转」会靠事务回滚兜住，而一旦有人把回滚去掉，
				// 报表里就会凭空多出一次状态变化。
				t.Errorf("判据未命中时不得发出状态流转插入，实际 %d 次", db.ExecCount(fragInsertLog))
			}
		})
	}
}

// TestDtmTryPlaceCreatesPlaceholderWithoutOutbox TCC 一阶段：建 presale_locked
// 占位单并写 dtm_gid，**不写 outbox**（单还没成，发「下单完成」等于通知下游去
// 扣款/送积分，而那两笔随时会被回退）。
//
// 反向探针：把 DtmTryPlace 里的 status 参数从 model.StatusPresaleLocked 改成
// model.StatusCreated，本用例必红（占位单不再是占位单）。
func TestDtmTryPlaceCreatesPlaceholderWithoutOutbox(t *testing.T) {
	db := dbmock.New(t)
	repo := newDtmRepo(db)
	cmd := dtmCmd()

	inTx(t, db, func(tx *sql.Tx) error {
		_, err := repo.DtmTryPlace(context.Background(), tx, cmd)
		return err
	})

	o, ok := db.OrderByNo(cmd.OrderNo)
	if !ok {
		t.Fatalf("占位单未落库：%v", db.Stmts())
	}
	if o.Status != model.StatusPresaleLocked {
		t.Errorf("status = %q, want %q", o.Status, model.StatusPresaleLocked)
	}
	if o.DtmGID != cmd.DtmGID {
		t.Errorf("dtm_gid = %q, want %q（Confirm/Cancel 只带 gid，不带 order_no 就靠这一列定位）",
			o.DtmGID, cmd.DtmGID)
	}
	if n := len(db.Outboxes()); n != 0 {
		t.Fatalf("TCC 一阶段不得写 outbox，实际 %d 条：%v", n, db.Stmts())
	}
	if db.ExecCount(fragInsertOutbox) != 0 {
		t.Fatal("TCC 一阶段连 outbox 的插入语句都不许发出")
	}
}

// TestDtmConfirmFreezeConvertsPlaceholderInPlace TCC 二阶段：占位单**原地**转正式
// paid 单（实收额对齐总额），此刻才写 outbox。
//
// 反向探针：把 DtmConfirmFreeze 改成「先 insert 一张新单再把占位单删掉」，
// 本用例必红（orders 变 2 行 / 出现过第二条 insert）。
func TestDtmConfirmFreezeConvertsPlaceholderInPlace(t *testing.T) {
	db := dbmock.New(t)
	repo := newDtmRepo(db)
	cmd := dtmCmd()
	seedOrder(db, placeholderOrder(cmd))

	orderID, err := runTx(t, db, func(tx *sql.Tx) (uint64, error) {
		return repo.DtmConfirmFreeze(context.Background(), tx, cmd, dtmFill(cmd))
	})
	if err != nil {
		t.Fatalf("确认冻结应成功：%v", err)
	}

	o, _ := db.OrderByNo(cmd.OrderNo)
	if o.Status != model.StatusPaid {
		t.Errorf("status = %q, want %q（占位转正式）", o.Status, model.StatusPaid)
	}
	if o.PaidAmount != o.TotalAmount {
		t.Errorf("paid_amount = %d, want total_amount = %d（确认即实收）", o.PaidAmount, o.TotalAmount)
	}
	if n := len(db.Orders()); n != 1 {
		t.Fatalf("口径③：全程只能有一行 orders，实际 %d 行：%v", n, db.Stmts())
	}
	if db.ExecCount(fragInsertOrder) != 0 {
		t.Fatal("确认必须是原地改，不得再插一行订单（占位与正式单并存 = 同一张单卖两次）")
	}
	logs := db.StatusLogs()
	if len(logs) != 1 || logs[0].FromStatus != model.StatusPresaleLocked ||
		logs[0].ToStatus != model.StatusPaid || uint64(logs[0].OrderID) != orderID {
		t.Errorf("状态流转行不对：%+v（order_id=%d）", logs, orderID)
	}
	evs := db.Outboxes()
	if len(evs) != 1 {
		t.Fatalf("确认后必须落 1 条 outbox 事件，实际 %d 条：%v", len(evs), db.Stmts())
	}
	if !strings.Contains(evs[0].Payload, fmt.Sprintf(`"order_id":%d`, orderID)) {
		t.Errorf("事件 payload 必须带该单的 id，实得 %q", evs[0].Payload)
	}
}

// TestDtmConfirmFreezeRejectsWhenPlaceholderMissing 占位单不存在 / 已被作废 /
// 属于别的 gid → 业务失败，**且 outbox 一行都不许出现**。
//
// 反向探针：把 DtmConfirmFreeze 的 affected==0 分支从「业务失败」改成「照写
// outbox 再返回 nil」，本用例必红（err==nil 或 outbox 有行）。
func TestDtmConfirmFreezeRejectsWhenPlaceholderMissing(t *testing.T) {
	cases := []struct {
		name string
		row  *dbmock.OrderRow
	}{
		{name: "完全没有这张单", row: nil},
		{name: "占位单已被作废", row: ptr(dbmock.OrderRow{UserID: 7, OrderNo: "O-DTM-1", Status: model.StatusCanceled, DtmGID: "gid-1"})},
		{name: "占位单属于别的全局事务", row: ptr(dbmock.OrderRow{UserID: 7, OrderNo: "O-DTM-1", Status: model.StatusPresaleLocked, DtmGID: "gid-other"})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := dbmock.New(t)
			repo := newDtmRepo(db)
			cmd := dtmCmd()
			if tc.row != nil {
				seedOrder(db, *tc.row)
			}

			tx, err := db.SQL().Begin()
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			_, err = repo.DtmConfirmFreeze(context.Background(), tx, cmd, dtmFill(cmd))
			if err == nil {
				_ = tx.Rollback()
				t.Fatal("确认不到占位单必须报错：发出去的事件会通知下游去扣款，而这张单根本不存在")
			}
			if !dtmx.IsBusinessFailure(err) {
				t.Fatalf("必须包 dtmx.BusinessFailure（dtm 据此不重试），实际 %v", err)
			}
			if !errors.Is(err, ErrStatusNotAllowed) {
				t.Fatalf("错误须保留哨兵 ErrStatusNotAllowed，实际 %v", err)
			}
			_ = tx.Rollback()

			if n := len(db.Outboxes()); n != 0 {
				t.Fatalf("被拒的确认不得留下事件行，实际 %d 条", n)
			}
			if n := len(db.StatusLogs()); n != 0 {
				t.Fatalf("被拒的确认不得留下状态流转行，实际 %d 条", n)
			}
		})
	}
}

// TestDtmCancelFreezeDiscardsPlaceholderWithoutOutbox TCC 回退：占位单作废、
// 记状态流转、**不写 outbox**。
func TestDtmCancelFreezeDiscardsPlaceholderWithoutOutbox(t *testing.T) {
	db := dbmock.New(t)
	repo := newDtmRepo(db)
	cmd := dtmCmd()
	seedOrder(db, placeholderOrder(cmd))

	inTx(t, db, func(tx *sql.Tx) error {
		_, err := repo.DtmCancelFreeze(context.Background(), tx, cmd)
		return err
	})

	o, _ := db.OrderByNo(cmd.OrderNo)
	if o.Status != model.StatusCanceled {
		t.Errorf("status = %q, want %q（占位单被废）", o.Status, model.StatusCanceled)
	}
	logs := db.StatusLogs()
	if len(logs) != 1 || logs[0].FromStatus != model.StatusPresaleLocked ||
		logs[0].ToStatus != model.StatusCanceled {
		t.Errorf("状态流转行不对：%+v", logs)
	}
	if n := len(db.Outboxes()); n != 0 {
		t.Fatalf("回退不得落 outbox，实际 %d 条：%v", n, db.Stmts())
	}
}

// TestDtmCancelFreezeRejectsWhenAlreadyPaid 已经确认的单不允许被回退（悬挂防护的
// 业务侧兜底）：报业务失败，且不得把 paid 改回 canceled。
func TestDtmCancelFreezeRejectsWhenAlreadyPaid(t *testing.T) {
	db := dbmock.New(t)
	repo := newDtmRepo(db)
	cmd := dtmCmd()
	paid := placeholderOrder(cmd)
	paid.Status = model.StatusPaid
	paid.PaidAmount = paid.TotalAmount
	seedOrder(db, paid)

	tx, err := db.SQL().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err = repo.DtmCancelFreeze(context.Background(), tx, cmd); err == nil {
		t.Fatal("已确认的单不得被回退 —— 那等于把已经卖出的单凭空作废")
	} else if !dtmx.IsBusinessFailure(err) {
		t.Fatalf("必须包 dtmx.BusinessFailure，实际 %v", err)
	}
	_ = tx.Rollback()

	o, _ := db.OrderByNo(cmd.OrderNo)
	if o.Status != model.StatusPaid {
		t.Fatalf("已确认的单状态被改了：%q → %q", model.StatusPaid, o.Status)
	}
}

// TestOutboxRowOnlyAppearsAfterConfirm 专属判据（口径②）：TryPlace 落占位单后
// outbox 仍是空的，只有 ConfirmFreeze 之后才出现那一行。
//
// 这是本域最容易被写坏的一处：TCC 一阶段就发「下单完成」，下游会去扣库存、送
// 积分，而那两笔随时会被 Cancel 回退 —— 下游收到的事件无法回退。
func TestOutboxRowOnlyAppearsAfterConfirm(t *testing.T) {
	db := dbmock.New(t)
	repo := newDtmRepo(db)
	cmd := dtmCmd()

	inTx(t, db, func(tx *sql.Tx) error {
		_, err := repo.DtmTryPlace(context.Background(), tx, cmd)
		return err
	})
	if n := len(db.Outboxes()); n != 0 {
		t.Fatalf("TryPlace 之后 outbox 必须仍为空，实际 %d 条：%v", n, db.Stmts())
	}
	o, _ := db.OrderByNo(cmd.OrderNo)
	if o.Status != model.StatusPresaleLocked {
		t.Fatalf("前置状态不是占位单，后续断言无意义：%q", o.Status)
	}

	inTx(t, db, func(tx *sql.Tx) error {
		_, err := repo.DtmConfirmFreeze(context.Background(), tx, cmd, dtmFill(cmd))
		return err
	})
	evs := db.Outboxes()
	if len(evs) != 1 {
		t.Fatalf("ConfirmFreeze 之后 outbox 必须恰好 1 条，实际 %d 条：%v", len(evs), db.Stmts())
	}
	o, _ = db.OrderByNo(cmd.OrderNo)
	if o.Status != model.StatusPaid {
		t.Fatalf("确认后应是正式单，实得 %q", o.Status)
	}
}

// TestCancelCompensationDoesNotDuplicateOutbox 专属判据（口径②）：Saga 正向写过
// 一条事件，作废补偿**不得**再写一条 —— 下游会收到两次「下单完成」，积分按
// uk_ledger_order_type 幂等掉一次，可账上就是一次对不上的重复消费。
func TestCancelCompensationDoesNotDuplicateOutbox(t *testing.T) {
	db := dbmock.New(t)
	repo := newDtmRepo(db)
	cmd := dtmCmd()

	inTx(t, db, func(tx *sql.Tx) error {
		_, err := repo.DtmCreateOrder(context.Background(), tx, cmd, dtmFill(cmd))
		return err
	})
	if n := len(db.Outboxes()); n != 1 {
		t.Fatalf("前置：Saga 正向应写 1 条事件，实际 %d 条", n)
	}

	inTx(t, db, func(tx *sql.Tx) error {
		_, err := repo.DtmCancelOrder(context.Background(), tx, cmd)
		return err
	})
	if n := len(db.Outboxes()); n != 1 {
		t.Fatalf("作废补偿后事件数必须仍是 1，实际 %d 条：%v", n, db.Stmts())
	}

	// TCC 侧同理：一张单走到「已作废」之后到来的 CancelFreeze 必须被拒（那是不该
	// 存在的悬挂回调），而拒绝的过程中事件数一条都不许增加。
	if _, err := runTx(t, db, func(tx *sql.Tx) (uint64, error) {
		return repo.DtmCancelFreeze(context.Background(), tx, cmd)
	}); err == nil {
		t.Fatal("已作废的单不该再被回退 —— 这里断言的是「悬挂回调被拒」，不是它成功")
	} else if !dtmx.IsBusinessFailure(err) {
		t.Fatalf("悬挂回调必须报业务失败（否则 dtm 会退避重试一整天），实际 %v", err)
	}
	if n := len(db.Outboxes()); n != 1 {
		t.Fatalf("悬挂回调被拒后事件数仍须是 1，实际 %d 条", n)
	}
}

// TestDtmBranchRejectionRollsBackWholeTx 业务失败必须把同事务里的写一起回滚
// （假驱动是真事务，故这个断言是「表里没有」而不是「没发过语句」）。
//
// 反向探针：把仓储里的 `tx.Rollback()`（由 dtmx.Barrier.Call 负责）换成提交，
// 本用例必红（被拒的写留在表里）。
func TestDtmBranchRejectionRollsBackWholeTx(t *testing.T) {
	db := dbmock.New(t)
	repo := newDtmRepo(db)
	cmd := dtmCmd()
	// 占位单先在**另一个事务**里落好，确认失败时它必须原封不动。
	inTx(t, db, func(tx *sql.Tx) error {
		_, err := repo.DtmTryPlace(context.Background(), tx, cmd)
		return err
	})
	before, _ := db.OrderByNo(cmd.OrderNo)

	// 换一个 gid 去确认：定位不到 → 业务失败。
	other := dtmCmd()
	other.DtmGID = "gid-other"
	tx, err := db.SQL().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err = repo.DtmConfirmFreeze(context.Background(), tx, other, dtmFill(other)); err == nil {
		t.Fatal("跨事务确认必须失败")
	}
	_ = tx.Rollback()

	after, _ := db.OrderByNo(cmd.OrderNo)
	if after != before {
		t.Fatalf("被回滚的事务不得留下痕迹：%+v → %+v", before, after)
	}
	if n := len(db.StatusLogs()); n != 0 {
		t.Fatalf("被回滚的事务不得留下状态流转行，实际 %d 条", n)
	}
	if n := len(db.Outboxes()); n != 0 {
		t.Fatalf("被回滚的事务不得留下事件行，实际 %d 条", n)
	}
}

// TestDtmBranchMustRunInsideBarrierTx nil tx 直接报错，不许 panic：
// 分支写脱离 barrier 事务就等于没有屏障，而那时当场不会有任何迹象。
func TestDtmBranchMustRunInsideBarrierTx(t *testing.T) {
	db := dbmock.New(t)
	repo := newDtmRepo(db)
	cmd := dtmCmd()
	ctx := context.Background()

	cases := map[string]func() error{
		model.DtmCreateOrderBranch:   func() error { _, err := repo.DtmCreateOrder(ctx, nil, cmd, dtmFill(cmd)); return err },
		model.DtmCancelOrderBranch:   func() error { _, err := repo.DtmCancelOrder(ctx, nil, cmd); return err },
		model.DtmTryPlaceBranch:      func() error { _, err := repo.DtmTryPlace(ctx, nil, cmd); return err },
		model.DtmConfirmFreezeBranch: func() error { _, err := repo.DtmConfirmFreeze(ctx, nil, cmd, dtmFill(cmd)); return err },
		model.DtmCancelFreezeBranch:  func() error { _, err := repo.DtmCancelFreeze(ctx, nil, cmd); return err },
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			err := fn()
			if err == nil {
				t.Fatal("nil tx 必须报错")
			}
			if !strings.Contains(err.Error(), "barrier") {
				t.Fatalf("错误文案须点明 barrier 事务，实际：%v", err)
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

// TestRawDBReturnsTheDbxConnection 合格句柄原样取出底层 *sql.DB（不另开连接）。
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

func ptr(o dbmock.OrderRow) *dbmock.OrderRow { return &o }
