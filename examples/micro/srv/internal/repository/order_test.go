// repository 层单测（sqlite cgo 驱动，仅验证 GORM 查询语义：
// JOIN Find 投影、事务原子性、outbox 状态迁移守卫、dateRange 解析。
// DSN 密码占位 / MySQL 方言差异不在本层覆盖）。
package repository

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// newTestDB 建 sqlite 内存库：GORM AutoMigrate 三个模型表 +
// 手工 DDL 两张纯 SQL 表（order_status_log / refund_records，
// 仅 Report 的 JOIN 计数需要，无 Go 模型）。
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&Order{}, &OrderItem{}, &OutboxEvent{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS order_status_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			order_id INTEGER NOT NULL,
			from_status TEXT NOT NULL DEFAULT '',
			to_status TEXT NOT NULL,
			remark TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE IF NOT EXISTS refund_records (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			order_id INTEGER NOT NULL,
			user_id INTEGER NOT NULL,
			amount INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'pending',
			reason TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
	return db
}

// seedOrder 给订单补 1 条状态流转 + 1 条退款记录（Report JOIN 计数用）
func seedOrder(t *testing.T, db *gorm.DB, orderID uint64, toStatus string, refundAmount int64, refundStatus string) {
	t.Helper()
	if err := db.Exec(
		`INSERT INTO order_status_log (order_id, from_status, to_status) VALUES (?, '', ?)`,
		orderID, toStatus,
	).Error; err != nil {
		t.Fatalf("seed status log: %v", err)
	}
	if err := db.Exec(
		`INSERT INTO refund_records (order_id, user_id, amount, status) VALUES (?, 7, ?, ?)`,
		orderID, refundAmount, refundStatus,
	).Error; err != nil {
		t.Fatalf("seed refund: %v", err)
	}
}

func TestCreateOrderTx(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := NewOrderRepository(db)

	var gotID uint64
	out, err := repo.CreateOrderTx(ctx, CreateOrderInput{
		UserID:      42,
		OrderNo:     "O-TEST-1",
		TotalAmount: 300,
		Items: []OrderItem{
			{ProductID: 1, ProductName: "p1", Quantity: 2, UnitPrice: 100},
			{ProductID: 2, ProductName: "p2", Quantity: 1, UnitPrice: 100},
		},
		OutboxFill: func(orderID uint64) (payload, traceparent string) {
			gotID = orderID
			return `{"order_id":` + strconv.FormatUint(orderID, 10) + `}`, "tp-test"
		},
	})
	if err != nil {
		t.Fatalf("CreateOrderTx: %v", err)
	}
	if out.OrderID == 0 || gotID != out.OrderID {
		t.Fatalf("auto-increment id: out=%d got=%d", out.OrderID, gotID)
	}

	// 明细 2 条（JOIN Find 展开）
	ow, err := repo.GetOrder(ctx, out.OrderID, 42)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if len(ow.Items) != 2 {
		t.Fatalf("items: want 2, got %d", len(ow.Items))
	}

	// outbox 行：payload/traceparent 经 OutboxFill 内嵌真实 ID
	pending, err := repo.PendingOutbox(ctx, 10)
	if err != nil {
		t.Fatalf("PendingOutbox: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending: want 1, got %d", len(pending))
	}
	if pending[0].Status != OutboxStatusPending || pending[0].Traceparent != "tp-test" {
		t.Fatalf("pending[0] = %+v", pending[0])
	}
}

func TestCreateOrderTx_EmptyOutboxPayload(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := NewOrderRepository(db)

	// OutboxFill 返回空 payload（事件体写失败形态）：不阻断事务，
	// outbox 行仍落库（Runner 补发时跳过空 payload）
	_, err := repo.CreateOrderTx(ctx, CreateOrderInput{
		UserID: 1, OrderNo: "O-TX-2", TotalAmount: 10,
		Items:      []OrderItem{{ProductID: 3, Quantity: 1, UnitPrice: 10}},
		OutboxFill: func(orderID uint64) (string, string) { return "", "" },
	})
	if err != nil {
		t.Fatalf("CreateOrderTx: %v", err)
	}
	pending, err := repo.PendingOutbox(ctx, 10)
	if err != nil {
		t.Fatalf("PendingOutbox: %v", err)
	}
	if len(pending) != 1 || pending[0].Payload != "" {
		t.Fatalf("empty-payload pending row not recorded: %+v", pending)
	}
}

func TestGetOrder_NotFoundAndIsolation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := NewOrderRepository(db)

	out, err := repo.CreateOrderTx(ctx, CreateOrderInput{UserID: 5, OrderNo: "O-ISO", TotalAmount: 1})
	if err != nil {
		t.Fatalf("CreateOrderTx: %v", err)
	}
	// 他人查询 → ErrNotFound（user_id 隔离条件不命中）
	if _, err := repo.GetOrder(ctx, out.OrderID, 6); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	// 无明细订单 → Items 为空（LEFT JOIN 未命中，item_id=0 跳过）
	ow, err := repo.GetOrder(ctx, out.OrderID, 5)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if len(ow.Items) != 0 {
		t.Fatalf("items: want 0, got %d", len(ow.Items))
	}
}

func TestOutboxTransitions(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := NewOrderRepository(db)

	ids := make([]uint64, 0, 3)
	for i := 0; i < 3; i++ {
		out, err := repo.CreateOrderTx(ctx, CreateOrderInput{
			UserID: 1, OrderNo: "O-OB-" + strconv.Itoa(i), TotalAmount: 1,
		})
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		ids = append(ids, out.OrderID)
	}

	// 待发集合：3 条（按 outbox.id ASC；与订单 id 同序）
	pending, err := repo.PendingOutbox(ctx, 10)
	if err != nil {
		t.Fatalf("PendingOutbox: %v", err)
	}
	if len(pending) != 3 {
		t.Fatalf("want 3 pending, got %d", len(pending))
	}
	ids = []uint64{pending[0].ID, pending[1].ID, pending[2].ID}

	// 1→已发（守卫：WHERE status=0）
	if err := repo.MarkOutboxSent(ctx, []uint64{ids[0]}); err != nil {
		t.Fatalf("MarkOutboxSent: %v", err)
	}
	// 重复标记 → 无影响（status 已 1，守卫不命中）
	if err := repo.MarkOutboxSent(ctx, []uint64{ids[0]}); err != nil {
		t.Fatalf("re-mark: %v", err)
	}
	assertOutboxStatus(t, db, ids[0], OutboxStatusSent)

	// 失败重试：retry_count+1，仍待发
	if err := repo.MarkOutboxRetry(ctx, []uint64{ids[1]}); err != nil {
		t.Fatalf("MarkOutboxRetry: %v", err)
	}
	assertOutbox(t, db, ids[1], OutboxStatusPending, 1)

	// 已发事件不可再迁移（守卫验证）
	if err := repo.AbandonOutbox(ctx, []uint64{ids[0]}); err != nil {
		t.Fatalf("AbandonOutbox on sent: %v", err)
	}
	assertOutboxStatus(t, db, ids[0], OutboxStatusSent)

	// 超限放弃
	if err := repo.AbandonOutbox(ctx, []uint64{ids[1]}); err != nil {
		t.Fatalf("AbandonOutbox: %v", err)
	}
	assertOutboxStatus(t, db, ids[1], OutboxStatusAbandon)

	// 只剩一条待发
	rest, err := repo.PendingOutbox(ctx, 10)
	if err != nil {
		t.Fatalf("PendingOutbox: %v", err)
	}
	if len(rest) != 1 || rest[0].ID != ids[2] {
		t.Fatalf("pending rest = %+v", rest)
	}
}

func TestReport(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := NewOrderRepository(db)

	// 订单 1：2 明细 + 1 状态流转 + 成功退款 50（应计入）
	o1, err := repo.CreateOrderTx(ctx, CreateOrderInput{UserID: 7, OrderNo: "O-R-1", TotalAmount: 500,
		Items: []OrderItem{{ProductID: 1, Quantity: 1, UnitPrice: 100}, {ProductID: 2, Quantity: 1, UnitPrice: 400}}})
	if err != nil {
		t.Fatalf("order 1: %v", err)
	}
	seedOrder(t, db, o1.OrderID, "refunded", 50, "success")
	// 订单 2：1 明细 + 1 状态流转 + 失败退款 30（不应计入 refund_amount）
	o2, err := repo.CreateOrderTx(ctx, CreateOrderInput{UserID: 7, OrderNo: "O-R-2", TotalAmount: 200,
		Items: []OrderItem{{ProductID: 3, Quantity: 2, UnitPrice: 100}}})
	if err != nil {
		t.Fatalf("order 2: %v", err)
	}
	seedOrder(t, db, o2.OrderID, "cancelled", 30, "failed")
	// 他人订单（user 8）→ 报表不得出现
	if _, err := repo.CreateOrderTx(ctx, CreateOrderInput{UserID: 8, OrderNo: "O-R-3", TotalAmount: 1}); err != nil {
		t.Fatalf("order 3: %v", err)
	}

	rows, err := repo.Report(ctx, 7, "", "")
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("report rows: want 2, got %d", len(rows))
	}
	byNo := map[string]ReportRow{}
	for _, r := range rows {
		byNo[r.OrderNo] = r
	}
	if got := byNo["O-R-1"]; got.ItemCount != 2 || got.StatusChangeCount != 1 || got.RefundAmount != 50 {
		t.Fatalf("O-R-1 = %+v (want 2 items, 1 status change, 50 refund)", got)
	}
	if got := byNo["O-R-2"]; got.ItemCount != 1 || got.StatusChangeCount != 1 || got.RefundAmount != 0 {
		t.Fatalf("O-R-2 = %+v (want 1 item, 1 status change, 0 refund)", got)
	}
}

func TestListOrders(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := NewOrderRepository(db)

	for i, no := range []string{"O-L-1", "O-L-2", "O-L-3"} {
		if _, err := repo.CreateOrderTx(ctx, CreateOrderInput{UserID: 9, OrderNo: no, TotalAmount: int64(10 * (i + 1))}); err != nil {
			t.Fatalf("seed %s: %v", no, err)
		}
	}
	// limit=2 → 最新 2 条（id DESC）
	flows, err := repo.ListOrders(ctx, 9, "", "", 2)
	if err != nil {
		t.Fatalf("ListOrders: %v", err)
	}
	if len(flows) != 2 || flows[0].OrderNo != "O-L-3" {
		t.Fatalf("flows = %+v (want newest O-L-3 first, 2 rows)", flows)
	}
	// 他人查不到
	others, err := repo.ListOrders(ctx, 10, "", "", 100)
	if err != nil {
		t.Fatalf("ListOrders others: %v", err)
	}
	if len(others) != 0 {
		t.Fatalf("cross-user rows: %d", len(others))
	}
}

func TestDateRange(t *testing.T) {
	cases := []struct {
		from, to string
		fromZero bool
		toZero   bool
		wantTo   time.Time
		wantErr  bool
	}{
		{"", "", true, true, time.Time{}, false},
		{"2026-01-01", "2026-01-31", false, false, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), false},
		{"bad", "", false, false, time.Time{}, true},
		{"", "nope", false, false, time.Time{}, true},
	}
	for _, c := range cases {
		from, to, err := dateRange(c.from, c.to)
		if c.wantErr {
			if err == nil {
				t.Fatalf("dateRange(%q,%q): want error", c.from, c.to)
			}
			continue
		}
		if err != nil {
			t.Fatalf("dateRange(%q,%q): %v", c.from, c.to, err)
		}
		if c.fromZero && !from.IsZero() {
			t.Fatalf("from not zero: %v", from)
		}
		if c.toZero && !to.IsZero() {
			t.Fatalf("to not zero: %v", to)
		}
		if !c.toZero && !to.Equal(c.wantTo) {
			t.Fatalf("to = %v, want %v", to, c.wantTo)
		}
	}
}

// ---------- helpers ----------

func assertOutboxStatus(t *testing.T, db *gorm.DB, id uint64, want int) {
	t.Helper()
	var st int
	if err := db.Model(&OutboxEvent{}).Where("id = ?", id).Select("status").Find(&st).Error; err != nil {
		t.Fatalf("read status %d: %v", id, err)
	}
	if st != want {
		t.Fatalf("outbox %d status = %d, want %d", id, st, want)
	}
}

func assertOutbox(t *testing.T, db *gorm.DB, id uint64, wantStatus, wantRetry int) {
	t.Helper()
	var ev OutboxEvent
	if err := db.Where("id = ?", id).First(&ev).Error; err != nil {
		t.Fatalf("read outbox %d: %v", id, err)
	}
	if ev.Status != wantStatus || ev.RetryCount != wantRetry {
		t.Fatalf("outbox %d = %+v (want status %d retry %d)", id, ev, wantStatus, wantRetry)
	}
}
