// repository 层单测（sqlite cgo 驱动）：验证 4 表事务原子性（orders +
// order_items + order_status_log + outbox）、ErrRecordNotFound 原样上抛、
// 以及 UpdateStatus 的 RowsAffected==0（状态机竞态）分支。
// 需要 CGO_ENABLED=1（mattn/go-sqlite3）。
package repository

import (
	"context"
	"errors"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/model"
	"github.com/gospacex/outboxx"
)

// newTestDB 建 sqlite 内存库并迁移四张表（含 outbox：事件与业务行同事务）。
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&model.Order{}, &model.OrderItem{}, &model.OrderStatusLog{}, &outboxx.Event{},
	); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// Create 必须在一个事务里落 orders + order_items + 首条 order_status_log
// + outbox 待发事件（四张表），且 fill 在订单 ID 已知后被回调。
func TestCreatePersistsFourTables(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)
	ctx := context.Background()

	o := &model.Order{
		UserID: 42, OrderNo: "M-1", Status: model.StatusCreated, TotalAmount: 300,
	}
	items := []model.OrderItem{
		{ProductID: 1, ProductName: "p1", Quantity: 2, UnitPrice: 100},
		{ProductID: 2, ProductName: "p2", Quantity: 1, UnitPrice: 100},
	}
	filledID := uint64(0)
	if _, err := repo.Create(ctx, o, items, func(orderID uint64) (string, string) {
		filledID = orderID
		return `{"order_id":` + "1" + `}`, "00-trace"
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if o.ID == 0 {
		t.Fatal("Create 应回填自增 ID")
	}
	if filledID != o.ID {
		t.Fatalf("fill 应在自增 ID 已知后回调：got %d, want %d", filledID, o.ID)
	}

	// 主表
	got, err := repo.FindByID(ctx, o.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.OrderNo != "M-1" || got.Status != model.StatusCreated || got.TotalAmount != 300 {
		t.Fatalf("order = %+v", got)
	}

	// 明细：OrderID 应被回填为自增 ID
	rows, err := repo.ItemsByOrderID(ctx, o.ID)
	if err != nil {
		t.Fatalf("ItemsByOrderID: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("items: want 2, got %d", len(rows))
	}
	for _, it := range rows {
		if it.OrderID != o.ID {
			t.Fatalf("item.order_id = %d, want %d", it.OrderID, o.ID)
		}
	}

	// 流转日志：首条 from="" → to=created
	var logs []model.OrderStatusLog
	if err := db.Where("order_id = ?", o.ID).Find(&logs).Error; err != nil {
		t.Fatalf("read status log: %v", err)
	}
	if len(logs) != 1 || logs[0].FromStatus != "" || logs[0].ToStatus != model.StatusCreated {
		t.Fatalf("status logs = %+v", logs)
	}

	// outbox 待发事件：与业务行同事务落库（status=pending，payload/traceparent 由 fill 给出）。
	var evs []outboxx.Event
	if err := db.Order("id ASC").Find(&evs).Error; err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("outbox 行数 = %d, want 1", len(evs))
	}
	if evs[0].Status != outboxx.StatusPending {
		t.Fatalf("outbox.status = %d, want pending(%d)", evs[0].Status, outboxx.StatusPending)
	}
	if evs[0].Payload != `{"order_id":1}` || evs[0].Traceparent != "00-trace" {
		t.Fatalf("outbox 事件体/traceparent 未被 fill 写入: %+v", evs[0])
	}
	if evs[0].Topic == "" || evs[0].EventType == "" {
		t.Fatalf("outbox topic/event_type 不应为空: %+v", evs[0])
	}
}

// 明细为空时只落主表 + 流转日志（不因 items 空而跳过日志）。
func TestCreateWithNoItemsStillLogs(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)
	ctx := context.Background()

	o := &model.Order{UserID: 1, OrderNo: "M-2", Status: model.StatusCreated}
	if _, err := repo.Create(ctx, o, nil, nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	var n int64
	if err := db.Model(&model.OrderStatusLog{}).Where("order_id = ?", o.ID).Count(&n).Error; err != nil {
		t.Fatalf("count log: %v", err)
	}
	if n != 1 {
		t.Fatalf("status log count = %d, want 1", n)
	}
}

// 查不到：gorm.ErrRecordNotFound 必须原样上抛，由 service 翻成业务码
// （在这一层翻会让「查不到」与「库报错」混为一谈）。
func TestFindByIDPropagatesErrRecordNotFound(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)

	_, err := repo.FindByID(context.Background(), 999)
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("want gorm.ErrRecordNotFound, got %v", err)
	}
}

// UpdateStatus 正常路径：条件更新 + 记一条流转日志。
func TestUpdateStatusHappyPath(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)
	ctx := context.Background()

	o := &model.Order{UserID: 1, OrderNo: "M-3", Status: model.StatusCreated}
	if _, err := repo.Create(ctx, o, nil, nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.UpdateStatus(ctx, o.ID, model.StatusCreated, model.StatusPaid); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	got, err := repo.FindByID(ctx, o.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.Status != model.StatusPaid {
		t.Fatalf("status = %q, want paid", got.Status)
	}
	var logs []model.OrderStatusLog
	if err := db.Where("order_id = ? AND to_status = ?", o.ID, model.StatusPaid).Find(&logs).Error; err != nil {
		t.Fatalf("read log: %v", err)
	}
	if len(logs) != 1 || logs[0].FromStatus != model.StatusCreated {
		t.Fatalf("paid log = %+v", logs)
	}
}

// 竞态分支：from 条件不命中（已被别的请求改过）→ RowsAffected==0 → 报错，
// 且不得写入第二条流转日志（日志与状态更新同事务）。
func TestUpdateStatusRaceRidesOnRowsAffected(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)
	ctx := context.Background()

	o := &model.Order{UserID: 1, OrderNo: "M-4", Status: model.StatusCreated}
	if _, err := repo.Create(ctx, o, nil, nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// 第一次成功：created → paid
	if err := repo.UpdateStatus(ctx, o.ID, model.StatusCreated, model.StatusPaid); err != nil {
		t.Fatalf("first UpdateStatus: %v", err)
	}
	// 第二次用过期 from=created：状态已是 paid，RowsAffected==0 → 报错
	err := repo.UpdateStatus(ctx, o.ID, model.StatusCreated, model.StatusPaid)
	if err == nil {
		t.Fatal("过期 from 条件应报错（RowsAffected==0）")
	}
	// 事务回滚：不应再写第三条日志（Create 一条 + 首次一条 = 2）
	var n int64
	if err := db.Model(&model.OrderStatusLog{}).Where("order_id = ?", o.ID).Count(&n).Error; err != nil {
		t.Fatalf("count log: %v", err)
	}
	if n != 2 {
		t.Fatalf("status log count = %d, want 2（竞态那次必须回滚）", n)
	}
}

// ItemsByOrderID 空结果返回空 slice（非 error）。
func TestItemsByOrderIDEmpty(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)
	rows, err := repo.ItemsByOrderID(context.Background(), 12345)
	if err != nil {
		t.Fatalf("ItemsByOrderID: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("want empty, got %d", len(rows))
	}
}

// ---------- 失败回滚分支（驱动事务内的每条错误返回） ----------

// 主表插入失败（orders 表不存在）→ 错误上抛，事务内无残留。
func TestCreateFailsWhenOrdersTableMissing(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)
	if err := db.Migrator().DropTable(&model.Order{}); err != nil {
		t.Fatalf("drop orders: %v", err)
	}
	_, err := repo.Create(context.Background(),
		&model.Order{UserID: 1, OrderNo: "M-X", Status: model.StatusCreated},
		[]model.OrderItem{{ProductID: 1, Quantity: 1, UnitPrice: 1}}, nil)
	if err == nil {
		t.Fatal("orders 表缺失时 Create 应报错")
	}
}

// 明细插入失败（order_items 表不存在）→ 整事务回滚，orders 行不得残留。
func TestCreateRollsBackWhenItemInsertFails(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)
	if err := db.Migrator().DropTable(&model.OrderItem{}); err != nil {
		t.Fatalf("drop order_items: %v", err)
	}
	_, err := repo.Create(context.Background(),
		&model.Order{UserID: 1, OrderNo: "M-RB", Status: model.StatusCreated},
		[]model.OrderItem{{ProductID: 1, Quantity: 1, UnitPrice: 1}}, nil)
	if err == nil {
		t.Fatal("order_items 表缺失时 Create 应报错")
	}
	var n int64
	if err := db.Model(&model.Order{}).Where("order_no = ?", "M-RB").Count(&n).Error; err != nil {
		t.Fatalf("count orders: %v", err)
	}
	if n != 0 {
		t.Fatalf("事务应回滚，orders 残留 %d 行", n)
	}
}

// 流转日志插入失败（order_status_log 表不存在）→ 整事务回滚。
func TestCreateRollsBackWhenStatusLogInsertFails(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)
	if err := db.Migrator().DropTable(&model.OrderStatusLog{}); err != nil {
		t.Fatalf("drop order_status_log: %v", err)
	}
	_, err := repo.Create(context.Background(),
		&model.Order{UserID: 1, OrderNo: "M-SL", Status: model.StatusCreated}, nil, nil)
	if err == nil {
		t.Fatal("order_status_log 表缺失时 Create 应报错")
	}
	var n int64
	if err := db.Model(&model.Order{}).Where("order_no = ?", "M-SL").Count(&n).Error; err != nil {
		t.Fatalf("count orders: %v", err)
	}
	if n != 0 {
		t.Fatalf("事务应回滚，orders 残留 %d 行", n)
	}
}

// outbox 表缺失 → 事件与业务行同事务的铁证：整事务回滚，orders 行不得残留。
// 这条是「事件不再事务外直发」的判据 —— 若事件仍走事务外发送，缺 outbox 表
// 也不会让 Create 报错。
func TestCreateRollsBackWhenOutboxInsertFails(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)
	if err := db.Migrator().DropTable(&outboxx.Event{}); err != nil {
		t.Fatalf("drop outbox: %v", err)
	}
	_, err := repo.Create(context.Background(),
		&model.Order{UserID: 1, OrderNo: "M-OB", Status: model.StatusCreated}, nil, nil)
	if err == nil {
		t.Fatal("outbox 表缺失时 Create 应报错（事件须与业务行同事务）")
	}
	var n int64
	if err := db.Model(&model.Order{}).Where("order_no = ?", "M-OB").Count(&n).Error; err != nil {
		t.Fatalf("count orders: %v", err)
	}
	if n != 0 {
		t.Fatalf("事务应回滚，orders 残留 %d 行", n)
	}
}
func TestItemsByOrderIDPropagatesDBError(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)
	if err := db.Migrator().DropTable(&model.OrderItem{}); err != nil {
		t.Fatalf("drop order_items: %v", err)
	}
	if _, err := repo.ItemsByOrderID(context.Background(), 1); err == nil {
		t.Fatal("表缺失时 ItemsByOrderID 应报错")
	}
}

// UpdateStatus 的 Update 语句失败分支（orders 表缺失）。
func TestUpdateStatusPropagatesUpdateError(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)
	if err := db.Migrator().DropTable(&model.Order{}); err != nil {
		t.Fatalf("drop orders: %v", err)
	}
	if err := repo.UpdateStatus(context.Background(), 1, model.StatusCreated, model.StatusPaid); err == nil {
		t.Fatal("orders 表缺失时 UpdateStatus 应报错")
	}
}

// UpdateStatus 的日志插入失败分支（order_status_log 表缺失）→ 状态更新回滚。
func TestUpdateStatusRollsBackWhenLogInsertFails(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)
	ctx := context.Background()
	o := &model.Order{UserID: 1, OrderNo: "M-UB", Status: model.StatusCreated}
	if _, err := repo.Create(ctx, o, nil, nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := db.Migrator().DropTable(&model.OrderStatusLog{}); err != nil {
		t.Fatalf("drop order_status_log: %v", err)
	}
	if err := repo.UpdateStatus(ctx, o.ID, model.StatusCreated, model.StatusPaid); err == nil {
		t.Fatal("order_status_log 表缺失时 UpdateStatus 应报错")
	}
	// 状态更新与日志同事务：日志写失败 → 状态必须回滚为 created。
	got, err := repo.FindByID(ctx, o.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.Status != model.StatusCreated {
		t.Fatalf("status = %q, want created（日志写失败必须回滚状态）", got.Status)
	}
}

// ---------- 分层埋点：开关与 quiet 判定 ----------

// quietNotFound：ErrRecordNotFound 是正常 miss（豁免），其它错误不豁免。
func TestQuietNotFound(t *testing.T) {
	if !quietNotFound(gorm.ErrRecordNotFound) {
		t.Fatal("ErrRecordNotFound 应被豁免")
	}
	if quietNotFound(errors.New("connection reset")) {
		t.Fatal("库错误不应被豁免")
	}
}

// traceOn=true：埋点开启不影响业务行为（覆盖命中 / miss 的 quiet 命中 / 库错误 quiet 不命中）。
func TestTraceOnDoesNotChangeBehavior(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, true)
	ctx := context.Background()

	// 命中路径
	o := &model.Order{UserID: 1, OrderNo: "M-T", Status: model.StatusCreated}
	if _, err := repo.Create(ctx, o, nil, nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got, err := repo.FindByID(ctx, o.ID); err != nil || got.OrderNo != "M-T" {
		t.Fatalf("FindByID 行为异常：got=%+v err=%v", got, err)
	}
	// miss 路径：quiet 命中（不改 span 状态），但仍原样回 ErrRecordNotFound
	if _, err := repo.FindByID(ctx, 99999); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("miss 应回 ErrRecordNotFound，实际 %v", err)
	}
	// 库错误路径：quiet 不命中，错误照常上抛
	if err := db.Migrator().DropTable(&model.OrderItem{}); err != nil {
		t.Fatalf("drop order_items: %v", err)
	}
	if _, err := repo.ItemsByOrderID(ctx, 1); err == nil {
		t.Fatal("表缺失时 ItemsByOrderID 应报错")
	}
}
