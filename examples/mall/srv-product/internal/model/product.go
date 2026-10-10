// Package model srv-product 的数据模型与领域 DTO。
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
//   - 表结构（Product / Category / SKU / ProductLog）：一行 = 一张表
//   - 查询投影（ProductDetail / ReportRow）：JOIN 出来的结果行
//   - 领域出入参（*Cmd / *Query / *Result）：service 的入参与返回值
package model

import "time"

// 商品状态（与 pb 的 ProductInfo.Status 同义，落库用 int32）
const (
	StatusOnSale   int32 = 1 // 上架
	StatusOffShelf int32 = 2 // 下架
)

// 商品操作日志 action 取值（product_logs.action）
const (
	ActionCreate   = "create"
	ActionUpdate   = "update"
	ActionOnSale   = "on_sale"
	ActionOffShelf = "off_shelf"
	ActionSold     = "sold"
)

// ---------- 表结构 ----------

// Product 商品主表行（products）
type Product struct {
	ID         uint64    `gorm:"primaryKey;column:id"`
	Title      string    `gorm:"column:title"`
	Price      int64     `gorm:"column:price"`
	Status     int32     `gorm:"column:status"`
	CategoryID uint64    `gorm:"column:category_id"`
	CreatedAt  time.Time `gorm:"column:created_at"`
	UpdatedAt  time.Time `gorm:"column:updated_at"`
}

// TableName 显式表名（避开 GORM 自动命名歧义）
func (Product) TableName() string { return "products" }

// Category 类目（categories，联表查询里类目名的来源）
type Category struct {
	ID        uint64    `gorm:"primaryKey;column:id"`
	Name      string    `gorm:"column:name"`
	ParentID  uint64    `gorm:"column:parent_id"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

// TableName 显式表名
func (Category) TableName() string { return "categories" }

// SKU 商品规格行（product_skus，建商品时按 InitStock 铺初始规格）
type SKU struct {
	ID        uint64    `gorm:"primaryKey;column:id"`
	ProductID uint64    `gorm:"column:product_id"`
	SkuCode   string    `gorm:"column:sku_code"`
	Price     int64     `gorm:"column:price"`
	OnSale    int32     `gorm:"column:on_sale"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

// TableName 显式表名
func (SKU) TableName() string { return "product_skus" }

// ProductLog 商品操作日志（product_logs，改价/上下架的审计流水）
type ProductLog struct {
	ID         uint64    `gorm:"primaryKey;column:id"`
	ProductID  uint64    `gorm:"column:product_id"`
	Action     string    `gorm:"column:action"`
	Detail     string    `gorm:"column:detail"`
	OperatorID uint64    `gorm:"column:operator_id"`
	CreatedAt  time.Time `gorm:"column:created_at"`
}

// TableName 显式表名
func (ProductLog) TableName() string { return "product_logs" }

// ---------- 查询投影（JOIN / 聚合结果行）----------

// ProductDetail 商品详情投影（products ⋈ categories ⋈ product_skus）
//
// 列表与详情共用同一投影：两者取的列完全一致，差别只在 WHERE 与 LIMIT。
// 各自定义一份会让"列表里有没有类目名"这种问题反复漂移。
type ProductDetail struct {
	ID           uint64    `gorm:"column:id"`
	Title        string    `gorm:"column:title"`
	Price        int64     `gorm:"column:price"`
	Status       int32     `gorm:"column:status"`
	CategoryID   uint64    `gorm:"column:category_id"`
	CategoryName string    `gorm:"column:category_name"`
	SkuCount     int32     `gorm:"column:sku_count"`
	CreatedAt    time.Time `gorm:"column:created_at"`
}

// ReportRow 报表行（products ⋈ categories ⋈ product_skus ⋈ product_logs 聚合）
type ReportRow struct {
	ProductID    int64  `gorm:"column:product_id"`
	Title        string `gorm:"column:title"`
	CategoryName string `gorm:"column:category_name"`
	Price        int64  `gorm:"column:price"`
	SkuCount     int32  `gorm:"column:sku_count"`
	OnSaleSkus   int32  `gorm:"column:on_sale_skus"`
	SoldCount    int32  `gorm:"column:sold_count"`
}

// ---------- 领域出入参（service 层流通，不含 pb / 不含 DB）----------

// CreateProductCmd 建商品命令
type CreateProductCmd struct {
	Title      string
	Price      int64
	CategoryID uint64
	OperatorID uint64
	// InitStock 初始规格条数（>0 时铺同等条数的 product_skus）
	InitStock int
}

// CreateProductResult 建商品结果
type CreateProductResult struct {
	ProductID uint64
	SkuCount  int
}

// GetProductQuery 查商品详情查询
type GetProductQuery struct {
	ProductID uint64
	UserID    uint64
}

// UpdateProductCmd 改商品命令
type UpdateProductCmd struct {
	ProductID  uint64
	Title      string
	Price      int64
	CategoryID uint64
	OperatorID uint64
}

// UpdateStatusCmd 上下架命令
type UpdateStatusCmd struct {
	ProductID  uint64
	Status     int32
	OperatorID uint64
}

// UpdateStatusResult 上下架结果
type UpdateStatusResult struct {
	// Changed 是否真的改了状态。false 表示已是目标态（幂等重放），
	// service 据此返回 CodeProductStatusDup。
	Changed bool
}

// ListProductsQuery 商品列表查询
type ListProductsQuery struct {
	CategoryID uint64
	Status     int32
	Limit      int
	Offset     int
}

// ProductReportQuery 商品报表查询
type ProductReportQuery struct {
	CategoryID uint64
	Limit      int
}
