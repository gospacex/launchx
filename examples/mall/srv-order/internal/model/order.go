// Package model srv-order 的数据模型与领域 DTO。
//
// 为什么单独成层（而不是像之前那样把结构体散在 repository 里）：
//   - 表结构是**契约**：service 的出入参、handler 的 pb 转换、repository 的
//     查询投影，三处引用的是同一批定义。放在 repository 里会逼着 service
//     反向 import 持久层，链子就绕回去了。
//   - 本包**零依赖**：不 import gorm、不 import pb、不 import 任何 SDK。
//     它是分层链的最下游（rpc → route → middleware → handler → service →
//     repository → model），只能被别人依赖，不依赖别人。
//
// 三类定义各就各位：
//   - 表结构（Order / OrderItem / StatusLog / Refund）：一行 = 一张表
//   - 查询投影（OrderJoinRow / OrderWithItems / OrderFlowRow / ReportRow）：
//     JOIN 出来的结果行
//   - 领域出入参（*Cmd / *Query / *Result）：service 的入参与返回值
package model

import "time"

// 订单状态（orders.status，落库用字符串，与 pb 的 GetOrderResp.Status 同义）
const (
	StatusCreated  = "created"
	StatusPaid     = "paid"
	StatusShipped  = "shipped"
	StatusDone     = "done"
	StatusCanceled = "canceled"
)

// ---------- 表结构 ----------

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

// ---------- 查询投影（JOIN / 聚合结果行）----------

// OrderJoinRow GetOrder 的平展行（orders ⋈ order_items）。
//
// 明细是 1:N，JOIN 会把订单行放大 N 倍，故结果必须 Find 到 slice（禁 Scan）
// 再由 repository 按 order_id 收敛回一个 OrderWithItems。这一层投影因此要
// 显式定义：靠 gorm 猜列名会在 orders / order_items 同名字段（id、created_at）
// 上静默取错。
type OrderJoinRow struct {
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

// OrderWithItems 订单详情投影（订单行 + 展开后的明细）
type OrderWithItems struct {
	Order Order
	Items []OrderItem
}

// OrderFlowRow 列表行（orders ⋈ order_items 聚合）
//
// ItemCount / ItemAmount 是 JOIN 出来的明细聚合，用途是**对账**：明细合计
// （Σ quantity × unit_price）应当等于 orders.total_amount，不等说明下单时
// 总额算错、或明细事后被改过。只有主表列的话，列表页看不出这件事。
type OrderFlowRow struct {
	ID          uint64    `gorm:"column:id"`
	UserID      uint64    `gorm:"column:user_id"`
	OrderNo     string    `gorm:"column:order_no"`
	Status      string    `gorm:"column:status"`
	TotalAmount int64     `gorm:"column:total_amount"`
	ItemCount   int32     `gorm:"column:item_count"`
	ItemAmount  int64     `gorm:"column:item_amount"`
	CreatedAt   time.Time `gorm:"column:created_at"`
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

// ---------- 领域出入参（service 层流通，不含 pb / 不含 DB）----------

// CreateOrderCmd 下单命令
type CreateOrderCmd struct {
	UserID uint64
	Items  []OrderItemCmd
	// OrderNo 调用方（bff）给的幂等键，落 uk_order_no。
	//
	// 空串表示调用方没带 —— 订单域会自己生成一个，但那样**就没有幂等了**：
	// 重试一次就是一个新号、一张新单。故 caller 必须带；这里不把它设成必填
	// 是为了不让老调用方直接 400（降级为"无幂等保护的下单"，并在 service
	// 侧记一笔，见 resolveOrderNo）。
	OrderNo string
}

// OrderItemCmd 下单明细（与 pb 的 OrderItem 对齐，但不含 pb）
type OrderItemCmd struct {
	ProductID uint64
	Name      string
	Quantity  int
	UnitPrice int64
}

// CreateOrderResult 下单结果
type CreateOrderResult struct {
	OrderID uint64
	OrderNo string
	// TotalAmount 服务端算出的总额（Σ quantity × unit_price，分）。
	// 不信任调用方传值：总额是后续对账与积分入账的依据，让客户端带进来
	// 等于把可篡改字段写进账本。
	TotalAmount int64
	Status      string
}

// GetOrderQuery 查订单详情查询
type GetOrderQuery struct {
	OrderID uint64
	UserID  uint64
}

// UpdateStatusCmd 订单状态流转命令（orders + order_status_log 同事务推进）
type UpdateStatusCmd struct {
	OrderID uint64
	UserID  uint64
	To      string
}

// ListOrdersQuery 订单列表查询
type ListOrdersQuery struct {
	UserID uint64
	// FromDate / ToDate 为 yyyy-MM-dd 闭区间，空串表示不限
	FromDate string
	ToDate   string
	Limit    int
}

// OrderReportQuery 订单报表查询
type OrderReportQuery struct {
	UserID   uint64
	FromDate string
	ToDate   string
}
