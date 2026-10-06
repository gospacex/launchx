// Package repository srv-product 持久层（私有库 product_db，账号 mall_rw）。
//
// 纪律（与 mall 其他三服一致）：
//   - 连表查询一律 Find（多行投影到 slice），禁用 Scan（会静默丢列）
//   - 错误一律 %w 包装，便于上层 errors.Is 判定
//   - 一次方法 = 一次 SQL / 一次事务：事务里的每条 SQL 由 dbx 的 gorm 回调
//     各自成 span，Jaeger 上能看清"改一个商品到底动了几张表"
package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/gospacex/launchx/examples/mall/pkg/outbox"
)

// ErrNotFound 无记录（service 层据此翻译成 CodeProductNotFound / CodeCategoryNotFound）
var ErrNotFound = errors.New("repository: record not found")

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

// Category 类目（categories，多表 JOIN 报表的类目名来源）
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

// ProductDetail GetProduct 结果（products ⋈ categories ⋈ product_skus）
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

// ReportRow Report 4 表 JOIN 聚合行
type ReportRow struct {
	ProductID    int64  `gorm:"column:product_id"`
	Title        string `gorm:"column:title"`
	CategoryName string `gorm:"column:category_name"`
	Price        int64  `gorm:"column:price"`
	SkuCount     int32  `gorm:"column:sku_count"`
	OnSaleSkus   int32  `gorm:"column:on_sale_skus"`
	SoldCount    int32  `gorm:"column:sold_count"`
}

// CreateProductInput 建商品事务输入
type CreateProductInput struct {
	Title      string
	Price      int64
	CategoryID uint64
	OperatorID uint64
	// InitStock 初始规格条数（>0 时铺同等条数的 product_skus）
	InitStock int
	// OutboxFill 在事务内、商品落库后（自增 ID 已知）回调，产出事件体与
	// 随 header 透传的 traceparent。nil 时 outbox 行 payload/traceparent
	// 为空（Runner 直接放弃该行，不占重试配额）。
	OutboxFill func(productID uint64) (payload, traceparent string)
}

// CreateProductOutput 建商品事务结果
type CreateProductOutput struct {
	ProductID uint64
	SkuCount  int
}

// UpdateProductInput 改商品输入（products + product_logs 两表事务）
type UpdateProductInput struct {
	ProductID  uint64
	Title      string
	Price      int64
	CategoryID uint64
	OperatorID uint64
}

// UpdateStatusInput 上下架输入（products + product_logs 两表事务）
type UpdateStatusInput struct {
	ProductID  uint64
	Status     int32
	OperatorID uint64
}

// ProductRepository product_db 仓储
type ProductRepository struct {
	db *gorm.DB
}

// NewProductRepository 构造
func NewProductRepository(db *gorm.DB) *ProductRepository {
	return &ProductRepository{db: db}
}

// DB 暴露句柄（组装期健康检查用）
func (r *ProductRepository) DB() *gorm.DB { return r.db }

// ---------- 单表 CRUD ----------

// GetCategory 单表查询：按类目 ID 取类目（建商品前的存在性校验）。
//
// 为什么在写之前先查而不是靠外键：product_db 是商品域私有库，跨域引用
// 只存 ID，没有跨库外键可用；校验放持久层之上（service）会多一次往返，
// 放这里则与后续写入同连接，且错误可原地包装成 ErrNotFound。
func (r *ProductRepository) GetCategory(ctx context.Context, categoryID uint64) (*Category, error) {
	var c Category
	err := r.db.WithContext(ctx).Where("id = ?", categoryID).Take(&c).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("get category %d: %w", categoryID, ErrNotFound)
		}
		return nil, fmt.Errorf("get category %d: %w", categoryID, err)
	}
	return &c, nil
}

// ListProducts 单表分页（列表页不需要类目名：JOIN 只为取一个 name 而拖慢
// 高频列表不划算，类目名只在详情与报表里补）。
func (r *ProductRepository) ListProducts(ctx context.Context, categoryID uint64, status int32, limit, offset int) ([]ProductDetail, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200 // 上限写死：分页接口被刷是常态，不设上限等于给了全表扫描入口
	}
	if offset < 0 {
		offset = 0
	}
	q := r.db.WithContext(ctx).
		Table("products").
		Select("id, title, price, status, category_id, created_at").
		Order("id DESC")
	if categoryID > 0 {
		q = q.Where("category_id = ?", categoryID)
	}
	if status > 0 {
		q = q.Where("status = ?", status)
	}
	var rows []ProductDetail
	if err := q.Limit(limit).Offset(offset).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list products: %w", err)
	}
	return rows, nil
}

// ---------- 多表 ----------

// GetProduct 3 表 JOIN：products ⋈ categories ⋈ product_skus。
//
// 规格是 1:N，JOIN 会把商品行放大 N 倍，故用 COUNT(DISTINCT) 统计规格数并
// GROUP BY 收敛回一行；结果仍走 Find 到 slice（保持"JOIN 一律 Find、禁 Scan"）。
func (r *ProductRepository) GetProduct(ctx context.Context, productID uint64) (*ProductDetail, error) {
	var rows []ProductDetail
	err := r.db.WithContext(ctx).
		Table("products").
		Select("products.id, products.title, products.price, products.status, "+
			"products.category_id, products.created_at, "+
			"COALESCE(categories.name, '') AS category_name, "+
			"COUNT(DISTINCT product_skus.id) AS sku_count").
		Joins("LEFT JOIN categories ON categories.id = products.category_id").
		Joins("LEFT JOIN product_skus ON product_skus.product_id = products.id").
		Where("products.id = ?", productID).
		Group("products.id, products.title, products.price, products.status, products.category_id, products.created_at, categories.name").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("get product %d: %w", productID, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("get product %d: %w", productID, ErrNotFound)
	}
	out := rows[0]
	return &out, nil
}

// CreateProductTx 3 表事务 + outbox：products + product_skus + product_logs + outbox。
//
// 初始规格与商品同生共死：只建商品不铺规格，库存域来预占时找不到 sku，
// 症状是"商品存在但永远卖不出去"。日志行同理 —— 事后无法回答"谁建的"。
func (r *ProductRepository) CreateProductTx(ctx context.Context, in CreateProductInput) (*CreateProductOutput, error) {
	out := &CreateProductOutput{}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		p := Product{
			Title:      in.Title,
			Price:      in.Price,
			Status:     StatusOnSale,
			CategoryID: in.CategoryID,
		}
		if err := tx.Create(&p).Error; err != nil {
			return fmt.Errorf("insert product: %w", err)
		}
		if in.InitStock > 0 {
			skus := make([]SKU, 0, in.InitStock)
			for i := 0; i < in.InitStock; i++ {
				skus = append(skus, SKU{
					ProductID: p.ID,
					SkuCode:   fmt.Sprintf("P%d-%02d", p.ID, i+1),
					Price:     in.Price,
					OnSale:    1,
				})
			}
			if err := tx.Create(&skus).Error; err != nil {
				return fmt.Errorf("insert product skus: %w", err)
			}
			out.SkuCount = len(skus)
		}
		if err := tx.Create(&ProductLog{
			ProductID:  p.ID,
			Action:     ActionCreate,
			Detail:     fmt.Sprintf("create product %q price=%d", in.Title, in.Price),
			OperatorID: in.OperatorID,
		}).Error; err != nil {
			return fmt.Errorf("insert product log: %w", err)
		}
		ev := outbox.Event{
			Topic:     "product.created.v1",
			EventType: "product.created",
			Status:    outbox.StatusPending,
		}
		if in.OutboxFill != nil {
			ev.Payload, ev.Traceparent = in.OutboxFill(p.ID)
		}
		if err := tx.Create(&ev).Error; err != nil {
			return fmt.Errorf("insert outbox event: %w", err)
		}
		out.ProductID = p.ID
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("create product tx: %w", err)
	}
	return out, nil
}

// UpdateProductTx 2 表事务：products 改标题/价格/类目 + product_logs 记一笔。
//
// 改价是强审计动作：只改主表不落日志，事后对账无法回答"这个价是谁改的"。
func (r *ProductRepository) UpdateProductTx(ctx context.Context, in UpdateProductInput) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var p Product
		if err := tx.Where("id = ?", in.ProductID).Take(&p).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("update product %d: %w", in.ProductID, ErrNotFound)
			}
			return fmt.Errorf("load product %d: %w", in.ProductID, err)
		}
		updates := map[string]any{"title": in.Title, "price": in.Price, "category_id": in.CategoryID}
		if err := tx.Model(&Product{}).Where("id = ?", in.ProductID).Updates(updates).Error; err != nil {
			return fmt.Errorf("update product %d: %w", in.ProductID, err)
		}
		if err := tx.Create(&ProductLog{
			ProductID:  in.ProductID,
			Action:     ActionUpdate,
			Detail:     fmt.Sprintf("price %d -> %d, category %d -> %d", p.Price, in.Price, p.CategoryID, in.CategoryID),
			OperatorID: in.OperatorID,
		}).Error; err != nil {
			return fmt.Errorf("insert product log %d: %w", in.ProductID, err)
		}
		return nil
	})
}

// UpdateStatusTx 2 表事务：products 改上下架 + product_logs 记一笔。
//
// 幂等：已是目标态时返回 (false, nil)，由 service 翻译成 CodeProductStatusDup
// —— 重试/重复点击不该产生第二条日志，否则报表里的"上下架次数"会被刷高。
func (r *ProductRepository) UpdateStatusTx(ctx context.Context, in UpdateStatusInput) (bool, error) {
	changed := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var p Product
		if err := tx.Where("id = ?", in.ProductID).Take(&p).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("update status %d: %w", in.ProductID, ErrNotFound)
			}
			return fmt.Errorf("load product %d: %w", in.ProductID, err)
		}
		if p.Status == in.Status {
			return nil // 幂等重放：已是目标态，不重复记日志
		}
		if err := tx.Model(&Product{}).
			Where("id = ?", in.ProductID).
			Update("status", in.Status).Error; err != nil {
			return fmt.Errorf("update product status %d: %w", in.ProductID, err)
		}
		action := ActionOffShelf
		if in.Status == StatusOnSale {
			action = ActionOnSale
		}
		if err := tx.Create(&ProductLog{
			ProductID:  in.ProductID,
			Action:     action,
			Detail:     fmt.Sprintf("status %d -> %d", p.Status, in.Status),
			OperatorID: in.OperatorID,
		}).Error; err != nil {
			return fmt.Errorf("insert status log %d: %w", in.ProductID, err)
		}
		changed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// Report 4 表 JOIN 报表（products ⋈ categories ⋈ product_skus ⋈ product_logs）。
//
// 三个 1:N 子表 JOIN 会互相放大行数，故三个计数一律 COUNT(DISTINCT ...)；
// 售出数用条件 DISTINCT（只数 action='sold' 的日志），不为它单独加一次往返。
func (r *ProductRepository) Report(ctx context.Context, categoryID uint64, limit int) ([]ReportRow, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	q := r.db.WithContext(ctx).
		Table("products").
		Select("products.id AS product_id, products.title, products.price, " +
			"COALESCE(categories.name, '') AS category_name, " +
			"COUNT(DISTINCT product_skus.id) AS sku_count, " +
			"COUNT(DISTINCT CASE WHEN product_skus.on_sale = 1 THEN product_skus.id END) AS on_sale_skus, " +
			"COUNT(DISTINCT CASE WHEN product_logs.action = 'sold' THEN product_logs.id END) AS sold_count").
		Joins("LEFT JOIN categories ON categories.id = products.category_id").
		Joins("LEFT JOIN product_skus ON product_skus.product_id = products.id").
		Joins("LEFT JOIN product_logs ON product_logs.product_id = products.id").
		Group("products.id, products.title, products.price, categories.name").
		Order("products.id ASC")
	if categoryID > 0 {
		q = q.Where("products.category_id = ?", categoryID)
	}
	var rows []ReportRow
	if err := q.Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("report products: %w", err)
	}
	return rows, nil
}
