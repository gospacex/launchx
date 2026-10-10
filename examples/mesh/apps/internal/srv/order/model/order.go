// Package model 表模型：纯数据，不依赖任何中间件句柄。
package model

import "time"

// Order 订单主表。列名与 `examples/scripts/order_db.sh` 的 DDL 真源一致。
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

// TableName 固定表名，避免依赖 gorm 的复数化规则（规则变动会让表名漂移）。
func (Order) TableName() string { return "orders" }

// OrderItem 订单明细。
type OrderItem struct {
	ID          uint64 `gorm:"primaryKey;column:id"`
	OrderID     uint64 `gorm:"column:order_id"`
	ProductID   uint64 `gorm:"column:product_id"`
	ProductName string `gorm:"column:product_name"`
	Quantity    int    `gorm:"column:quantity"`
	UnitPrice   int64  `gorm:"column:unit_price"`
}

func (OrderItem) TableName() string { return "order_items" }

// OrderStatusLog 状态流转日志。
type OrderStatusLog struct {
	ID         uint64    `gorm:"primaryKey;column:id"`
	OrderID    uint64    `gorm:"column:order_id"`
	FromStatus string    `gorm:"column:from_status"`
	ToStatus   string    `gorm:"column:to_status"`
	CreatedAt  time.Time `gorm:"column:created_at"`
}

func (OrderStatusLog) TableName() string { return "order_status_log" }

// 状态常量：散落的字符串字面量是状态机类 bug 的常见来源。
const (
	StatusCreated = "created"
	StatusPaid    = "paid"
	StatusClosed  = "closed"
)
