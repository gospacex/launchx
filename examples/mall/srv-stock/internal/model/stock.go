// Package model srv-stock 的数据模型与领域 DTO。
//
// 为什么单独成层（而不是像之前那样把结构体散在 repository 里）：
//   - 表结构是**契约**：service 的出入参、handler 的 pb 转换、repository 的
//     查询投影，三处引用的是同一批定义。放在 repository 里会逼着 service
//     反向 import 持久层，链子就绕回去了。
//   - 本包**零依赖**：不 import gorm、不 import pb、不 import 任何 SDK。
//     它是分层链的最下游（route → middleware → handler → service →
//     repository → model），只能被别人依赖，不依赖别人。
//
// 三类定义各就各位：
//   - 表结构（Stock / Reservation / StockLog）：一行 = 一张表
//   - 查询投影（StockDetail / ReportRow）：JOIN 出来的结果行
//   - 领域出入参（*Cmd / *Query / *Result）：service 的入参与返回值
package model

import "time"

// 预占流水状态（stock_reservations.status）
const (
	ReserveReserved  int32 = 1 // 预占中（尚未释放/确认）
	ReserveConfirmed int32 = 2 // 已确认（下单成功，库存真正出库）
	ReserveReleased  int32 = 3 // 已释放（下单失败/取消，库存回滚）
)

// 库存变动日志 reason 取值（stock_logs.reason）
const (
	ReasonAdjust = "adjust" // 未指定原因时的兜底值（入库 / 盘点）
)

// ---------- 表结构 ----------

// Stock 库存主表行（stocks，product_id 唯一）
//
// Reserved 是**冗余计数**：它由预占/释放/确认的条件更新同步维护，
// 权威账本在 stock_reservations 里。两者不一致即意味着某次事务只落了一半，
// service 读详情时会用联表聚合出来的值与之对账（见 StockDetail）。
type Stock struct {
	ID        uint64    `gorm:"primaryKey;column:id"`
	ProductID uint64    `gorm:"column:product_id"`
	Total     int32     `gorm:"column:total"`
	Available int32     `gorm:"column:available"`
	Reserved  int32     `gorm:"column:reserved"`
	Version   int64     `gorm:"column:version"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// TableName 显式表名（避开 GORM 自动命名歧义）
func (Stock) TableName() string { return "stocks" }

// Reservation 预占流水（stock_reservations，相对 stocks 是 1:N）
type Reservation struct {
	ID        uint64    `gorm:"primaryKey;column:id"`
	OrderNo   string    `gorm:"column:order_no"`
	ProductID uint64    `gorm:"column:product_id"`
	Quantity  int32     `gorm:"column:quantity"`
	Status    int32     `gorm:"column:status"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// TableName 显式表名
func (Reservation) TableName() string { return "stock_reservations" }

// StockLog 库存变动日志（stock_logs）
type StockLog struct {
	ID         uint64    `gorm:"primaryKey;column:id"`
	ProductID  uint64    `gorm:"column:product_id"`
	Delta      int32     `gorm:"column:delta"`
	Reason     string    `gorm:"column:reason"`
	OperatorID uint64    `gorm:"column:operator_id"`
	CreatedAt  time.Time `gorm:"column:created_at"`
}

// TableName 显式表名
func (StockLog) TableName() string { return "stock_logs" }

// ---------- 查询投影（JOIN / 聚合结果行）----------

// StockDetail 库存详情投影（stocks ⋈ stock_reservations）
//
// 详情与列表共用同一投影：两者取的列完全一致，差别只在 WHERE 与 LIMIT。
// 各自定义一份会让"列表里有没有未释放预占量"这种问题反复漂移。
//
// OpenReservedQty / OpenReserveCount 由流水表聚合而来（权威），
// Reserved 是主表冗余计数（快）。两者同源却在两条写路径上各自推进，
// 因此读详情时要把权威值一起取出来对账 —— 只取冗余计数的话，
// "事务落了一半"这种账实不符没有任何现象可观测。
type StockDetail struct {
	ProductID        uint64    `gorm:"column:product_id"`
	Total            int32     `gorm:"column:total"`
	Available        int32     `gorm:"column:available"`
	Reserved         int32     `gorm:"column:reserved"`
	Version          int64     `gorm:"column:version"`
	OpenReservedQty  int32     `gorm:"column:open_reserved_qty"`
	OpenReserveCount int32     `gorm:"column:open_reserve_count"`
	ReserveCount     int32     `gorm:"column:reserve_count"`
	UpdatedAt        time.Time `gorm:"column:updated_at"`
}

// ReportRow 报表行（stocks ⋈ stock_reservations ⋈ stock_logs 聚合）
type ReportRow struct {
	ProductID        int64 `gorm:"column:product_id"`
	Total            int32 `gorm:"column:total"`
	Available        int32 `gorm:"column:available"`
	Reserved         int32 `gorm:"column:reserved"`
	ReservationCount int32 `gorm:"column:reservation_count"`
	LogCount         int32 `gorm:"column:log_count"`
}

// ---------- 领域出入参（service 层流通，不含 pb / 不含 DB）----------

// GetStockQuery 查库存详情查询。
//
// 只有商品维度：库存是**商品维度**的资源，不按用户分片（同一个商品的库存
// 对所有人是同一份），pb 侧也因此没有 user_id 字段。
type GetStockQuery struct {
	ProductID uint64
}

// ListStocksQuery 库存列表查询
type ListStocksQuery struct {
	Limit  int
	Offset int
}

// ReserveCmd 预占命令
type ReserveCmd struct {
	OrderNo   string
	ProductID uint64
	UserID    uint64
	Quantity  int32
}

// ReserveResult 预占结果
type ReserveResult struct {
	ReservationID uint64
	// Reserved 本次实际预占量（与入参 Quantity 相同：预占不部分成交，
	// 不够就整单拒绝，避免"扣了一半又回滚"的中间态外泄）
	Reserved int32
	// Available 事务内回读的剩余可用量
	Available int32
}

// ReleaseCmd 释放预占命令
type ReleaseCmd struct {
	OrderNo   string
	ProductID uint64
	Quantity  int32
}

// ReleaseResult 释放预占结果
type ReleaseResult struct {
	Available int32
}

// ConfirmCmd 确认预占命令（出库）
type ConfirmCmd struct {
	OrderNo   string
	ProductID uint64
	Quantity  int32
}

// ConfirmResult 确认预占结果
type ConfirmResult struct {
	Total     int32
	Available int32
}

// AdjustCmd 调整命令（入库 Δ>0 / 盘点 Δ<0）
type AdjustCmd struct {
	ProductID  uint64
	Delta      int32
	Reason     string
	OperatorID uint64
}

// AdjustResult 调整结果
type AdjustResult struct {
	Total     int32
	Available int32
}

// StockReportQuery 库存报表查询
type StockReportQuery struct {
	Limit int
}
