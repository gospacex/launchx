// Package repository srv-order 持久层（私有库 order_db，账号 order_rw）。
//
// 纪律（brief）：
//   - GORM 连表查询一律 Find（多行投影到 slice），禁用 Scan（会静默丢列）
//   - 错误一律 %w 包装
package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// ErrNotFound 无记录
var ErrNotFound = errors.New("repository: record not found")

// TopicOrderCompleted 下单完成事件 topic（mqx 生产端）
const TopicOrderCompleted = "order.completed.v1"

// OutboxStatus 常量（outbox.status）
const (
	OutboxStatusPending = 0
	OutboxStatusSent    = 1
	OutboxStatusAbandon = 2
)

// OutboxEventTypeOrderCompleted outbox 事件类型标记
const OutboxEventTypeOrderCompleted = "order.completed"

// Order 订单主表行
type Order struct {
	ID          uint64    `gorm:"primaryKey;column:id"`
	UserID      uint64    `gorm:"column:user_id"`
	OrderNo     string    `gorm:"column:order_no"`
	Status      string    `gorm:"column:status"`
	TotalAmount int64     `gorm:"column:total_amount"`
	PaidAmount  int64     `gorm:"column:paid_amount"`
	PayOrderNo  string    `gorm:"column:pay_order_no"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

// TableName 显式表名（避开 GORM 自动命名歧义）
func (Order) TableName() string { return "orders" }

// OrderItem 订单明细行
type OrderItem struct {
	ID          uint64 `gorm:"primaryKey;column:id"`
	OrderID     uint64 `gorm:"column:order_id"`
	ProductID   uint64 `gorm:"column:product_id"`
	ProductName string `gorm:"column:product_name"`
	Quantity    int    `gorm:"column:quantity"`
	UnitPrice   int64  `gorm:"column:unit_price"`
}

// TableName 显式表名
func (OrderItem) TableName() string { return "order_items" }

// OutboxEvent 事务发件箱行（order_db.outbox，3s 轮询补发）
type OutboxEvent struct {
	ID          uint64    `gorm:"primaryKey;column:id"`
	Topic       string    `gorm:"column:topic"`
	EventType   string    `gorm:"column:event_type"`
	Payload     string    `gorm:"column:payload"`
	Traceparent string    `gorm:"column:traceparent"`
	Status      int       `gorm:"column:status"`
	RetryCount  int       `gorm:"column:retry_count"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

// TableName 显式表名
func (OutboxEvent) TableName() string { return "outbox" }

// OrderWithItems GetOrder 结果（orders ⋈ order_items）
type OrderWithItems struct {
	Order Order
	Items []OrderItem
}

// ReportRow Report 4 表 JOIN 聚合行
type ReportRow struct {
	OrderNo           string `gorm:"column:order_no"`
	Status            string `gorm:"column:status"`
	TotalAmount       int64  `gorm:"column:total_amount"`
	RefundAmount      int64  `gorm:"column:refund_amount"`
	ItemCount         int32  `gorm:"column:item_count"`
	StatusChangeCount int32  `gorm:"column:status_change_count"`
}

// FlowRow ListOrders 只读流水行
type FlowRow struct {
	ID          uint64    `gorm:"column:id"`
	UserID      uint64    `gorm:"column:user_id"`
	OrderNo     string    `gorm:"column:order_no"`
	Status      string    `gorm:"column:status"`
	TotalAmount int64     `gorm:"column:total_amount"`
	CreatedAt   time.Time `gorm:"column:created_at"`
}

// CreateOrderInput 下单事务输入
type CreateOrderInput struct {
	// UserID 下单用户
	UserID uint64
	// OrderNo 订单号（uk_order_no）
	OrderNo string
	// TotalAmount 订单总额（分）
	TotalAmount int64
	// Items 订单明细
	Items []OrderItem
	// OutboxFill 在事务内、订单行落库后（自增 ID 已知）回调，构造
	// OrderCompleted 事件 JSON（可内嵌真实 OrderID）与发送时随 header
	// 透传的 traceparent。nil 时 outbox 行 payload/traceparent 为空（Runner 跳过）。
	OutboxFill func(orderID uint64) (payload, traceparent string)
}

// CreateOrderOutput 下单事务结果
type CreateOrderOutput struct {
	OrderID uint64
	OrderNo string
}

// OrderRepository order_db 仓储
type OrderRepository struct {
	db *gorm.DB
}

// NewOrderRepository 构造
func NewOrderRepository(db *gorm.DB) *OrderRepository {
	return &OrderRepository{db: db}
}

// DB 暴露句柄（组装期健康检查用）
func (r *OrderRepository) DB() *gorm.DB { return r.db }

// GetOrder 查单个订单 + 明细（orders ⋈ order_items 2 表 JOIN，Find 投影）。
// 用户隔离：user_id 条件必带（跨域身份透传后的最小授权面）。
// 明细是 1:N：JOIN 结果按行 Find 到 slice，再在 Go 侧按 order_id 分组展开
// （保持 GORM JOIN 一律 Find、禁 Scan 的纪律，同时不丢多行明细）。
func (r *OrderRepository) GetOrder(ctx context.Context, orderID, userID uint64) (*OrderWithItems, error) {
	// 单订单多明细投影：orders 全列 + order_items 明细列（LEFT JOIN 未命中时明细列为 0/空）
	type row struct {
		OrderID     uint64    `gorm:"column:order_id"`
		UserID      uint64    `gorm:"column:user_id"`
		OrderNo     string    `gorm:"column:order_no"`
		Status      string    `gorm:"column:status"`
		TotalAmount int64     `gorm:"column:total_amount"`
		PaidAmount  int64     `gorm:"column:paid_amount"`
		PayOrderNo  string    `gorm:"column:pay_order_no"`
		CreatedAt   time.Time `gorm:"column:created_at"`
		UpdatedAt   time.Time `gorm:"column:updated_at"`
		ItemID      uint64    `gorm:"column:item_id"`
		ProductID   uint64    `gorm:"column:product_id"`
		ProductName string    `gorm:"column:product_name"`
		Quantity    int       `gorm:"column:quantity"`
		UnitPrice   int64     `gorm:"column:unit_price"`
	}
	var rows []row
	err := r.db.WithContext(ctx).
		Table("orders").
		Select("orders.id AS order_id, orders.user_id, orders.order_no, orders.status, "+
			"orders.total_amount, orders.paid_amount, orders.pay_order_no, orders.created_at, orders.updated_at, "+
			"order_items.id AS item_id, order_items.product_id, order_items.product_name, "+
			"order_items.quantity, order_items.unit_price").
		Joins("LEFT JOIN order_items ON order_items.order_id = orders.id").
		Where("orders.id = ? AND orders.user_id = ?", orderID, userID).
		Order("order_items.id ASC").
		Find(&rows).
		Error
	if err != nil {
		return nil, fmt.Errorf("get order %d: %w", orderID, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("get order %d: %w", orderID, ErrNotFound)
	}
	// 主行取第一行（LEFT JOIN 未命中时仍有一行，明细列为 0/空）
	head := rows[0]
	out := &OrderWithItems{Order: Order{
		ID: head.OrderID, UserID: head.UserID, OrderNo: head.OrderNo, Status: head.Status,
		TotalAmount: head.TotalAmount, PaidAmount: head.PaidAmount, PayOrderNo: head.PayOrderNo,
		CreatedAt: head.CreatedAt, UpdatedAt: head.UpdatedAt,
	}}
	// 明细 1:N 展开（LEFT JOIN 未命中 → item_id=0，跳过）
	for _, rw := range rows {
		if rw.ItemID == 0 {
			continue
		}
		out.Items = append(out.Items, OrderItem{
			ID: rw.ItemID, OrderID: rw.OrderID, ProductID: rw.ProductID,
			ProductName: rw.ProductName, Quantity: rw.Quantity, UnitPrice: rw.UnitPrice,
		})
	}
	return out, nil
}

// Report 4 表 JOIN 报表（orders ⋈ order_items ⋈ order_status_log ⋈ refund_records）。
// fromDate/toDate 为 yyyy-MM-dd（闭区间）；空串表示不限。
// 明细/状态/退款计数用 COUNT(DISTINCT) 抵消多表 JOIN 的行放大；
// 退款额用标量子查询单独 SUM（避免 JOIN 交叉放大 SUM）。
func (r *OrderRepository) Report(ctx context.Context, userID uint64, fromDate, toDate string) ([]ReportRow, error) {
	from, to, err := dateRange(fromDate, toDate)
	if err != nil {
		return nil, fmt.Errorf("report: parse date range: %w", err)
	}
	q := r.db.WithContext(ctx).
		Table("orders").
		Select("orders.id AS order_id, orders.order_no, orders.status, orders.total_amount, "+
			"COUNT(DISTINCT order_items.id) AS item_count, "+
			"COUNT(DISTINCT order_status_log.id) AS status_change_count, "+
			"COALESCE((SELECT SUM(refund_records.amount) FROM refund_records "+
			"WHERE refund_records.order_id = orders.id AND refund_records.status = 'success'), 0) AS refund_amount").
		Joins("LEFT JOIN order_items ON order_items.order_id = orders.id").
		Joins("LEFT JOIN order_status_log ON order_status_log.order_id = orders.id").
		Joins("LEFT JOIN refund_records ON refund_records.order_id = orders.id").
		Where("orders.user_id = ?", userID).
		Group("orders.id, orders.order_no, orders.status, orders.total_amount").
		Order("orders.id ASC")
	if !from.IsZero() {
		q = q.Where("orders.created_at >= ?", from)
	}
	if !to.IsZero() {
		q = q.Where("orders.created_at < ?", to)
	}
	var rows []ReportRow
	if err := q.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("report %d: %w", userID, err)
	}
	return rows, nil
}

// ListOrders 只读订单流水（对账数据源）
func (r *OrderRepository) ListOrders(ctx context.Context, userID uint64, fromDate, toDate string, limit int) ([]FlowRow, error) {
	from, to, err := dateRange(fromDate, toDate)
	if err != nil {
		return nil, fmt.Errorf("list orders: parse date range: %w", err)
	}
	if limit <= 0 {
		limit = 100
	}
	q := r.db.WithContext(ctx).
		Table("orders").
		Select("id, user_id, order_no, status, total_amount, created_at").
		Where("user_id = ?", userID)
	if !from.IsZero() {
		q = q.Where("created_at >= ?", from)
	}
	if !to.IsZero() {
		q = q.Where("created_at < ?", to)
	}
	var rows []FlowRow
	if err := q.Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list orders %d: %w", userID, err)
	}
	return rows, nil
}

// CreateOrderTx 下单本地事务：orders + order_items + outbox 一次提交。
// 调用方须先完成上游预占（srv-product.ReserveStock），再进本事务。
func (r *OrderRepository) CreateOrderTx(ctx context.Context, in CreateOrderInput) (*CreateOrderOutput, error) {
	out := &CreateOrderOutput{OrderNo: in.OrderNo}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		o := Order{
			UserID:      in.UserID,
			OrderNo:     in.OrderNo,
			Status:      "created",
			TotalAmount: in.TotalAmount,
		}
		if err := tx.Create(&o).Error; err != nil {
			return fmt.Errorf("insert order: %w", err)
		}
		items := make([]OrderItem, len(in.Items))
		for i, it := range in.Items {
			items[i] = OrderItem{
				OrderID:     o.ID,
				ProductID:   it.ProductID,
				ProductName: it.ProductName,
				Quantity:    it.Quantity,
				UnitPrice:   it.UnitPrice,
			}
		}
		if len(items) > 0 {
			if err := tx.Create(&items).Error; err != nil {
				return fmt.Errorf("insert order items: %w", err)
			}
		}
		ev := OutboxEvent{
			Topic:     TopicOrderCompleted,
			EventType: OutboxEventTypeOrderCompleted,
			Status:    OutboxStatusPending,
		}
		// 事件体在订单行落库后（自增 ID 已知）定稿：OutboxFill 回调把
		// OrderID 内嵌进 payload（事件体 order_id 字段不再留 0）
		if in.OutboxFill != nil {
			ev.Payload, ev.Traceparent = in.OutboxFill(o.ID)
		}
		if err := tx.Create(&ev).Error; err != nil {
			return fmt.Errorf("insert outbox event: %w", err)
		}
		out.OrderID = o.ID
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("create order tx: %w", err)
	}
	return out, nil
}

// PendingOutbox 取待发事件（3s 轮询补发用，status=0）
func (r *OrderRepository) PendingOutbox(ctx context.Context, limit int) ([]OutboxEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	var rows []OutboxEvent
	err := r.db.WithContext(ctx).
		Where("status = ?", OutboxStatusPending).
		Order("id ASC").
		Limit(limit).
		Find(&rows).
		Error
	if err != nil {
		return nil, fmt.Errorf("pending outbox: %w", err)
	}
	return rows, nil
}

// MarkOutboxSent 标记已发（幂等：仅 status=0 的行可迁移到 1）
func (r *OrderRepository) MarkOutboxSent(ctx context.Context, ids []uint64) error {
	if len(ids) == 0 {
		return nil
	}
	err := r.db.WithContext(ctx).
		Model(&OutboxEvent{}).
		Where("id IN ? AND status = ?", ids, OutboxStatusPending).
		Update("status", OutboxStatusSent).
		Error
	if err != nil {
		return fmt.Errorf("mark outbox sent: %w", err)
	}
	return nil
}

// MarkOutboxRetry 补发失败：retry_count+1
func (r *OrderRepository) MarkOutboxRetry(ctx context.Context, ids []uint64) error {
	if len(ids) == 0 {
		return nil
	}
	err := r.db.WithContext(ctx).
		Model(&OutboxEvent{}).
		Where("id IN ? AND status = ?", ids, OutboxStatusPending).
		UpdateColumn("retry_count", gorm.Expr("retry_count + 1")).
		Error
	if err != nil {
		return fmt.Errorf("mark outbox retry: %w", err)
	}
	return nil
}

// AbandonOutbox 重试超限：放弃（status=2，人工对账兜底）
func (r *OrderRepository) AbandonOutbox(ctx context.Context, ids []uint64) error {
	if len(ids) == 0 {
		return nil
	}
	err := r.db.WithContext(ctx).
		Model(&OutboxEvent{}).
		Where("id IN ? AND status = ?", ids, OutboxStatusPending).
		Update("status", OutboxStatusAbandon).
		Error
	if err != nil {
		return fmt.Errorf("abandon outbox: %w", err)
	}
	return nil
}

// dateRange 解析 yyyy-MM-dd 闭区间为 [from, to)；空串为零值
func dateRange(fromDate, toDate string) (time.Time, time.Time, error) {
	var from, to time.Time
	if fromDate != "" {
		t, err := time.Parse("2006-01-02", fromDate)
		if err != nil {
			return from, to, fmt.Errorf("from_date %q: %w", fromDate, err)
		}
		from = t
	}
	if toDate != "" {
		t, err := time.Parse("2006-01-02", toDate)
		if err != nil {
			return from, to, fmt.Errorf("to_date %q: %w", toDate, err)
		}
		// 闭区间转半开区间
		to = t.Add(24 * time.Hour)
	}
	return from, to, nil
}
