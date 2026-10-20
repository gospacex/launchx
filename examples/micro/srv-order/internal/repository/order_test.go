// 下单事务的单测（sqlite cgo 驱动）：钉住 order_no 的幂等语义。
//
// 需要 CGO_ENABLED=1（mattn/go-sqlite3），与 examples/mesh/apps 的仓储测试同口径。
package repository

import (
	"context"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/gospacex/launchx/examples/micro/srv-order/internal/model"
	"github.com/gospacex/outboxx"
)

// newOrderTestDB 建 sqlite 内存库并迁移 3 张表（orders + order_items + outbox）。
func newOrderTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.Order{}, &model.OrderItem{}, &outboxx.Event{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

func newCreateInput() CreateOrderInput {
	return CreateOrderInput{
		UserID:      7,
		OrderNo:     "O-IDEMPOTENT-1",
		TotalAmount: 100,
		Items: []model.OrderItem{
			{ProductID: 1, ProductName: "p1", Quantity: 1, UnitPrice: 100},
		},
		OutboxFill: func(orderID uint64) (string, string) { return `{"order":1}`, "00-tp" },
	}
}

// TestCreateOrderTxReplayIsIdempotent 同一个 order_no 重放必须回同一张单，
// 不产生第二行订单、也不产生第二个 outbox 事件。
//
// 背景：RPC 失败重试（rpcx 的 WithFailureRetry）发的是同一个 CreateOrderReq，
// 带的是同一个 order_no。仓储若只会 `INSERT`，重放就撞 uk_order_no 变成一次
// 唯一键冲突错误 —— 上游看到 5xx 会再试，于是永远失败；而事件插重了下游会
// 重复消费，那比重复下单更难收敛。
//
// 反向探针：把 CreateOrderTx 里的幂等查重删掉，本用例立刻红（orders 变 2 行）。
func TestCreateOrderTxReplayIsIdempotent(t *testing.T) {
	db := newOrderTestDB(t)
	repo := NewOrderRepository(db, false)
	ctx := context.Background()

	first, err := repo.CreateOrderTx(ctx, newCreateInput())
	if err != nil {
		t.Fatalf("首次下单: %v", err)
	}
	if first.Idempotent {
		t.Fatal("首次下单 Idempotent 应为 false")
	}

	second, err := repo.CreateOrderTx(ctx, newCreateInput())
	if err != nil {
		t.Fatalf("重放下单: %v（重放不是失败，返回 error 会让上游无限重试）", err)
	}
	if !second.Idempotent {
		t.Fatal("重放必须标记 Idempotent=true —— 否则调用方无法区分「新建」与「命中」")
	}
	if second.OrderID != first.OrderID || second.OrderNo != first.OrderNo {
		t.Fatalf("重放应回同一张单：got (%d,%s), want (%d,%s)",
			second.OrderID, second.OrderNo, first.OrderID, first.OrderNo)
	}

	var orders int64
	if err := db.Model(&model.Order{}).Count(&orders).Error; err != nil {
		t.Fatalf("count orders: %v", err)
	}
	if orders != 1 {
		t.Fatalf("orders 行数 = %d, want 1（重放不得产生第二张单）", orders)
	}
	var events int64
	if err := db.Model(&outboxx.Event{}).Count(&events).Error; err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if events != 1 {
		t.Fatalf("outbox 行数 = %d, want 1（重放不得重复投递事件）", events)
	}
}

// TestCreateOrderTxDistinctOrderNoCreatesBoth 不同 order_no 照常各建一单：
// 幂等不得退化成「整个进程只下一单」。
func TestCreateOrderTxDistinctOrderNoCreatesBoth(t *testing.T) {
	db := newOrderTestDB(t)
	repo := NewOrderRepository(db, false)
	ctx := context.Background()

	a, err := repo.CreateOrderTx(ctx, newCreateInput())
	if err != nil {
		t.Fatalf("第一单: %v", err)
	}
	in := newCreateInput()
	in.OrderNo = "O-IDEMPOTENT-2"
	b, err := repo.CreateOrderTx(ctx, in)
	if err != nil {
		t.Fatalf("第二单: %v", err)
	}
	if a.Idempotent || b.Idempotent {
		t.Fatalf("不同 order_no 都应新建：a=%v b=%v", a.Idempotent, b.Idempotent)
	}
	if a.OrderID == b.OrderID {
		t.Fatal("两单应有不同自增 ID")
	}
}

// TestCreateOrderTxPersistsItems 首次下单落主表 + 明细 + outbox 三张表。
// 幂等分支容易写歪成「只查不插」，这条钉住正常路径没被破坏。
func TestCreateOrderTxPersistsItems(t *testing.T) {
	db := newOrderTestDB(t)
	repo := NewOrderRepository(db, false)
	ctx := context.Background()

	if _, err := repo.CreateOrderTx(ctx, newCreateInput()); err != nil {
		t.Fatalf("CreateOrderTx: %v", err)
	}
	var items int64
	if err := db.Model(&model.OrderItem{}).Count(&items).Error; err != nil {
		t.Fatalf("count items: %v", err)
	}
	if items != 1 {
		t.Fatalf("order_items 行数 = %d, want 1", items)
	}
	var ev outboxx.Event
	if err := db.First(&ev).Error; err != nil {
		t.Fatalf("outbox 事件未落库: %v", err)
	}
	if ev.Payload == "" || ev.Traceparent == "" {
		t.Fatalf("OutboxFill 的结果没写进事件行：payload=%q traceparent=%q", ev.Payload, ev.Traceparent)
	}
}
