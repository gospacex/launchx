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

// Valid 判定这份值是不是一个完整订单（供 loadthrough.Validatable 使用）。
//
// 反序列化成功不等于值完整：缓存里混进一个缺主键的半截对象时，不校验就会
// 把它当命中直接返回，用户看到一个"没有 ID 的订单"。此前这条判据写在
// service 的 decodeOrder 里，收口到 loadthrough 后由值自己声明。
func (o Order) Valid() bool { return o.ID != 0 }

// CreateOutput 下单事务的落库结果。
//
// 放在 model 而不是 repository：service 用窄接口面向 repository 编程（不 import
// 它），而接口方法签名要求返回类型**完全相同**，故这个出参必须是两边都认的
// 共享类型。它是跨层契约，不是某一层的私有结构。
//
// Idempotent=true 表示「这个 order_no 已存在，本次一行都没插（含 outbox 事件）」，
// 调用方应把既有订单原样返回而不是报错：重放来自失败重试 / 上游重发 / 用户双击，
// 把幂等命中的重放当错误处理，会让调用方再试一次 —— 而重试必然再次撞同一个键，
// 于是永远失败。
type CreateOutput struct {
	ID          uint64
	TotalAmount int64
	Idempotent  bool
}

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
