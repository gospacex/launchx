// Package repository srv-order 持久层（私有库 order_db，账号 mall_rw）。
//
// 纪律（与 hubx 其他示例一致）：
//   - 连表查询一律 Find（多行投影到 slice），禁用 Scan（会静默丢列）
//   - 错误一律 %w 包装，便于上层 errors.Is 判定
//   - 用户隔离：user_id 条件必带（跨域身份透传后的最小授权面）
package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/gospacex/launchx/examples/mall/pkg/outbox"
)

// ErrNotFound 无记录（service 层据此翻译成 CodeOrderNotFound）
var ErrNotFound = errors.New("repository: record not found")

// 订单状态
const (
	StatusCreated  = "created"
	StatusPaid     = "paid"
	StatusShipped  = "shipped"
	StatusDone     = "done"
	StatusCanceled = "canceled"
)

// Order 订单主表行（orders）
type Order struct {
	ID          uint64    `gorm:"primaryKey;column:id"`
	UserID      uint64    `gorm:"column:user_id"`
	OrderNo     string    `gorm:"column:order_no"`
	Status      string    `gorm:"column:status"`
	TotalAmount int64     `gorm:"column:total_amount"`
	PaidAmount  int64     `gorm:"column:paid_amount"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

// TableName 显式表名（避开 GORM 自动命名歧义）
func (Order) TableName() string { return "orders" }

// OrderItem 订单明细行（order_items）
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

// StatusLog 订单状态流转日志（order_status_log，多表 JOIN 报表的计数源）
type StatusLog struct {
	ID         uint64    `gorm:"primaryKey;column:id"`
	OrderID    uint64    `gorm:"column:order_id"`
	FromStatus string    `gorm:"column:from_status"`
	ToStatus   string    `gorm:"column:to_status"`
	CreatedAt  time.Time `gorm:"column:created_at"`
}

// TableName 显式表名
func (StatusLog) TableName() string { return "order_status_log" }

// Refund 退款记录（refund_records，报表里的退款额来源）
type Refund struct {
	ID        uint64    `gorm:"primaryKey;column:id"`
	OrderID   uint64    `gorm:"column:order_id"`
	Amount    int64     `gorm:"column:amount"`
	Status    string    `gorm:"column:status"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

// TableName 显式表名
func (Refund) TableName() string { return "refund_records" }

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
	UserID      uint64
	OrderNo     string
	TotalAmount int64
	Items       []OrderItem
	// OutboxFill 在事务内、订单行落库后（自增 ID 已知）回调，产出事件体与
	// 随 header 透传的 traceparent。nil 时 outbox 行 payload/traceparent
	// 为空（Runner 直接放弃该行，不占重试配额）。
	OutboxFill func(orderID uint64) (payload, traceparent string)
}

// CreateOrderOutput 下单事务结果
type CreateOrderOutput struct {
	OrderID uint64
	OrderNo string
}

// UpdateStatusInput 状态流转输入（orders + order_status_log 两表事务）
type UpdateStatusInput struct {
	OrderID uint64
	UserID  uint64
	To      string
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

// ---------- 单表 CRUD ----------

// GetOrderByNo 单表查询：按订单号取订单（幂等校验 / 对账入口）
func (r *OrderRepository) GetOrderByNo(ctx context.Context, orderNo string) (*Order, error) {
	var o Order
	err := r.db.WithContext(ctx).Where("order_no = ?", orderNo).Take(&o).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("get order by no %q: %w", orderNo, ErrNotFound)
		}
		return nil, fmt.Errorf("get order by no %q: %w", orderNo, err)
	}
	return &o, nil
}

// ListOrders 单表只读订单流水（对账数据源）
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

// UpdateStatusTx 单表更新 + 日志表插入的两表事务：orders.status 与
// order_status_log 必须同生共死 —— 只改状态不落日志，对账时无法回答
// "这一单什么时候变成 paid 的"。
func (r *OrderRepository) UpdateStatusTx(ctx context.Context, in UpdateStatusInput) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var o Order
		if err := tx.Where("id = ? AND user_id = ?", in.OrderID, in.UserID).Take(&o).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("update status %d: %w", in.OrderID, ErrNotFound)
			}
			return fmt.Errorf("load order %d: %w", in.OrderID, err)
		}
		if o.Status == in.To {
			return nil // 幂等重放：已是目标态，不重复记日志
		}
		if err := tx.Model(&Order{}).
			Where("id = ? AND user_id = ?", in.OrderID, in.UserID).
			Update("status", in.To).Error; err != nil {
			return fmt.Errorf("update order status %d: %w", in.OrderID, err)
		}
		log := StatusLog{OrderID: in.OrderID, FromStatus: o.Status, ToStatus: in.To}
		if err := tx.Create(&log).Error; err != nil {
			return fmt.Errorf("insert status log %d: %w", in.OrderID, err)
		}
		return nil
	})
}

// ---------- 多表 ----------

// GetOrder 2 表 JOIN：orders ⋈ order_items，查单个订单 + 明细。
//
// 明细是 1:N：JOIN 结果按行 Find 到 slice 后在 Go 侧按 order_id 分组展开
// （保持"JOIN 一律 Find、禁 Scan"的纪律，同时不丢多行明细）。
func (r *OrderRepository) GetOrder(ctx context.Context, orderID, userID uint64) (*OrderWithItems, error) {
	type row struct {
		OrderID     uint64    `gorm:"column:order_id"`
		UserID      uint64    `gorm:"column:user_id"`
		OrderNo     string    `gorm:"column:order_no"`
		Status      string    `gorm:"column:status"`
		TotalAmount int64     `gorm:"column:total_amount"`
		PaidAmount  int64     `gorm:"column:paid_amount"`
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
			"orders.total_amount, orders.paid_amount, orders.created_at, orders.updated_at, "+
			"order_items.id AS item_id, order_items.product_id, order_items.product_name, "+
			"order_items.quantity, order_items.unit_price").
		Joins("LEFT JOIN order_items ON order_items.order_id = orders.id").
		Where("orders.id = ? AND orders.user_id = ?", orderID, userID).
		Order("order_items.id ASC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("get order %d: %w", orderID, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("get order %d: %w", orderID, ErrNotFound)
	}
	head := rows[0]
	out := &OrderWithItems{Order: Order{
		ID: head.OrderID, UserID: head.UserID, OrderNo: head.OrderNo, Status: head.Status,
		TotalAmount: head.TotalAmount, PaidAmount: head.PaidAmount,
		CreatedAt: head.CreatedAt, UpdatedAt: head.UpdatedAt,
	}}
	for _, rw := range rows {
		if rw.ItemID == 0 { // LEFT JOIN 未命中明细
			continue
		}
		out.Items = append(out.Items, OrderItem{
			ID: rw.ItemID, OrderID: rw.OrderID, ProductID: rw.ProductID,
			ProductName: rw.ProductName, Quantity: rw.Quantity, UnitPrice: rw.UnitPrice,
		})
	}
	return out, nil
}

// CreateOrderTx 3 表事务：orders + order_items + outbox 一次提交。
//
// outbox 行与业务行同事务是本包的立身之本：不同事务就会出现"订单落了、
// 事件没落"（下游永远不知道这单存在）或"事件落了、订单回滚"（幽灵事件）。
func (r *OrderRepository) CreateOrderTx(ctx context.Context, in CreateOrderInput) (*CreateOrderOutput, error) {
	out := &CreateOrderOutput{OrderNo: in.OrderNo}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		o := Order{
			UserID:      in.UserID,
			OrderNo:     in.OrderNo,
			Status:      StatusCreated,
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
		ev := outbox.Event{
			Topic:     "order.completed.v1",
			EventType: "order.completed",
			Status:    outbox.StatusPending,
		}
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

// Report 4 表 JOIN 报表（orders ⋈ order_items ⋈ order_status_log ⋈ refund_records）。
//
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
		to = t.Add(24 * time.Hour) // 闭区间转半开区间
	}
	return from, to, nil
}
