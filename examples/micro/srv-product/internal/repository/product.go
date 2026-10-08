// Package repository srv-product 持久层（私有库 product_db，账号 mall_rw）。
//
// 在分层链上的位置：service → **repository** → model。它的上游只有 service，
// 下游只有 model 与 dbx 句柄。
//
// 句柄来源：db 是 launchx App.DB() 交出的 *gorm.DB（dbx 组件，自带 OTel
// 回调）。本包不 gorm.Open、不 import 任何驱动 —— 装配期由
// pkg/handlecheck.VerifyDBX 断言这件事，跑错句柄会在启动时直接失败。
//
// 纪律：
//   - 连表查询一律 Find（多行投影到 slice），禁用 Scan（会静默丢列）
//   - 错误一律 %w 包装，便于上层 errors.Is 判定
//   - 一次方法 = 一次 SQL / 一次事务：事务里的每条 SQL 由 dbx 的 gorm 回调
//     各自成 span，Jaeger 上能看清"改一个商品到底动了几张表"
//   - 每个方法开一个 repository.product.<op> span，与 service / handler 的
//     span 串成一条链
package repository

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/gospacex/launchx/examples/micro/srv-product/internal/model"
	"github.com/gospacex/launchx/examples/pkg/outbox"
	"github.com/gospacex/otelx/layer"
)

// ErrNotFound 无记录（service 层据此翻译成 CodeProductNotFound / CodeCategoryNotFound）
var ErrNotFound = errors.New("repository: record not found")

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

// UpdateStatusInput 上下架输入（products + product_skus + product_logs 三表事务）
type UpdateStatusInput struct {
	ProductID  uint64
	Status     int32
	OperatorID uint64
}

// ProductRepository product_db 仓储
type ProductRepository struct {
	db      *gorm.DB
	traceOn bool
}

// NewProductRepository 构造。
//
// db 必须是 launchx App.DB() 交出的 dbx 句柄：只有它带 OTel 回调，
// 本层开的 span 下面才挂得住每条 SQL 的 dbx span。
func NewProductRepository(db *gorm.DB, traceOn bool) *ProductRepository {
	return &ProductRepository{db: db, traceOn: traceOn}
}

// DB 暴露句柄（pkg/handlecheck 断言、装配期健康检查用）
func (r *ProductRepository) DB() *gorm.DB { return r.db }

// begin 开 repository 层 span，返回「收 error 再收尾」的闭包。
//
// traceOn=false 时返回空闭包，调用方一律 `defer func() { end(err) }()`，
// 不必在每个方法里写 if。为什么闭包要收 error：span 句柄刻意不交给调用方
// （本层因此零 otel 依赖），代价是调用方也标不了状态 —— 收 error 由闭包
// 代标，失败才在 Jaeger 上可见（否则「SQL 报错」与「SQL 成功」同色）。
//
// quiet 规则：ErrNotFound 是正常 miss，不是故障 —— 与单体示例 recordError
// 的同一条语义，命中即豁免错误标记，避免正常未命污染错误率。
func (r *ProductRepository) begin(ctx context.Context, op string) (context.Context, layer.End) {
	return layer.Begin(ctx, "repository", "product", op, r.traceOn, quietNotFound)
}

// quietNotFound 「记录不存在」不是故障。两种形态都认：本层已归一化出的
// ErrNotFound，以及 dbx/gorm 直接抛出的 ErrRecordNotFound（未归一化路径）。
func quietNotFound(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, gorm.ErrRecordNotFound)
}

// ---------- 单表 CRUD ----------

// GetCategory 单表查询：按类目 ID 取类目（建商品前的存在性校验）。
//
// 为什么在写之前先查而不是靠外键：product_db 是商品域私有库，跨域引用
// 只存 ID，没有跨库外键可用；校验放持久层之上（service）会多一次往返，
// 放这里则与后续写入同连接，且错误可原地包装成 ErrNotFound。
func (r *ProductRepository) GetCategory(ctx context.Context, categoryID uint64) (out *model.Category, err error) {
	ctx, end := r.begin(ctx, "GetCategory")
	defer func() { end(err) }()
	var c model.Category
	err = r.db.WithContext(ctx).Where("id = ?", categoryID).Take(&c).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("get category %d: %w", categoryID, ErrNotFound)
		}
		return nil, fmt.Errorf("get category %d: %w", categoryID, err)
	}
	return &c, nil
}

// ---------- 多表 CRUD ----------

// ListProducts 2 表 LEFT JOIN（products ⋈ categories）分页。
//
// 这里 JOIN 类目表而不像早期版本那样只查主表：列表页要展示类目名，之前
// 的做法是"列表不带类目名、点进详情再补"，结果前端要么多一次请求、要么
// 显示一栏空的类目。categories 是 1:1 维度表，LEFT JOIN 不放大行数，
// 代价只是一次索引回表 —— 比多一轮往返便宜。
func (r *ProductRepository) ListProducts(ctx context.Context, q model.ListProductsQuery) (out []model.ProductDetail, err error) {
	ctx, end := r.begin(ctx, "ListProducts")
	defer func() { end(err) }()
	limit, offset := q.Limit, q.Offset
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200 // 上限写死：分页接口被刷是常态，不设上限等于给了全表扫描入口
	}
	if offset < 0 {
		offset = 0
	}
	qb := r.db.WithContext(ctx).
		Table("products").
		Select("products.id, products.title, products.price, products.status, " +
			"products.category_id, products.created_at, " +
			"COALESCE(categories.name, '') AS category_name, 0 AS sku_count").
		Joins("LEFT JOIN categories ON categories.id = products.category_id").
		Order("products.id DESC")
	if q.CategoryID > 0 {
		qb = qb.Where("products.category_id = ?", q.CategoryID)
	}
	if q.Status > 0 {
		qb = qb.Where("products.status = ?", q.Status)
	}
	var rows []model.ProductDetail
	if err := qb.Limit(limit).Offset(offset).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list products: %w", err)
	}
	return rows, nil
}

// GetProduct 3 表 JOIN：products ⋈ categories ⋈ product_skus。
//
// 规格是 1:N，JOIN 会把商品行放大 N 倍，故用 COUNT(DISTINCT) 统计规格数并
// GROUP BY 收敛回一行；结果仍走 Find 到 slice（保持"JOIN 一律 Find、禁 Scan"）。
func (r *ProductRepository) GetProduct(ctx context.Context, productID uint64) (out *model.ProductDetail, err error) {
	ctx, end := r.begin(ctx, "GetProduct")
	defer func() { end(err) }()
	var rows []model.ProductDetail
	err = r.db.WithContext(ctx).
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
	out = &rows[0]
	return out, nil
}

// CreateProductTx 多表事务 + outbox：products + product_skus + product_logs + outbox（4 张表）。
//
// 初始规格与商品同生共死：只建商品不铺规格，库存域来预占时找不到 sku，
// 症状是"商品存在但永远卖不出去"。日志行同理 —— 事后无法回答"谁建的"。
func (r *ProductRepository) CreateProductTx(ctx context.Context, in CreateProductInput) (out *CreateProductOutput, err error) {
	ctx, end := r.begin(ctx, "CreateProductTx")
	defer func() { end(err) }()
	out = &CreateProductOutput{}
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		p := model.Product{
			Title:      in.Title,
			Price:      in.Price,
			Status:     model.StatusOnSale,
			CategoryID: in.CategoryID,
		}
		if err := tx.Create(&p).Error; err != nil {
			return fmt.Errorf("insert product: %w", err)
		}
		if in.InitStock > 0 {
			skus := make([]model.SKU, 0, in.InitStock)
			for i := 0; i < in.InitStock; i++ {
				skus = append(skus, model.SKU{
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
		if err := tx.Create(&model.ProductLog{
			ProductID:  p.ID,
			Action:     model.ActionCreate,
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
func (r *ProductRepository) UpdateProductTx(ctx context.Context, in UpdateProductInput) (err error) {
	ctx, end := r.begin(ctx, "UpdateProductTx")
	defer func() { end(err) }()
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var p model.Product
		if err := tx.Where("id = ?", in.ProductID).Take(&p).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("update product %d: %w", in.ProductID, ErrNotFound)
			}
			return fmt.Errorf("load product %d: %w", in.ProductID, err)
		}
		updates := map[string]any{"title": in.Title, "price": in.Price, "category_id": in.CategoryID}
		if err := tx.Model(&model.Product{}).Where("id = ?", in.ProductID).Updates(updates).Error; err != nil {
			return fmt.Errorf("update product %d: %w", in.ProductID, err)
		}
		if err := tx.Create(&model.ProductLog{
			ProductID:  in.ProductID,
			Action:     model.ActionUpdate,
			Detail:     fmt.Sprintf("price %d -> %d, category %d -> %d", p.Price, in.Price, p.CategoryID, in.CategoryID),
			OperatorID: in.OperatorID,
		}).Error; err != nil {
			return fmt.Errorf("insert product log %d: %w", in.ProductID, err)
		}
		return nil
	})
}

// UpdateStatusTx 3 表事务：products 改上下架 + product_skus 联动可售 +
// product_logs 记一笔。
//
// 为什么要动 product_skus：下架只改主表状态，规格仍是 on_sale=1，此后任何
// 按 sku 维度统计"在售规格数"的报表都会把已下架商品的规格算进去 —— 报表
// 与商品状态对不上账。上架同理要把它翻回来。
//
// 幂等：已是目标态时返回 (false, nil)，由 service 翻译成 CodeProductStatusDup
// —— 重试/重复点击不该产生第二条日志，否则报表里的"上下架次数"会被刷高。
func (r *ProductRepository) UpdateStatusTx(ctx context.Context, in UpdateStatusInput) (out bool, err error) {
	ctx, end := r.begin(ctx, "UpdateStatusTx")
	defer func() { end(err) }()
	changed := false
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var p model.Product
		if err := tx.Where("id = ?", in.ProductID).Take(&p).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("update status %d: %w", in.ProductID, ErrNotFound)
			}
			return fmt.Errorf("load product %d: %w", in.ProductID, err)
		}
		if p.Status == in.Status {
			return nil // 幂等重放：已是目标态，不重复记日志
		}
		if err := tx.Model(&model.Product{}).
			Where("id = ?", in.ProductID).
			Update("status", in.Status).Error; err != nil {
			return fmt.Errorf("update product status %d: %w", in.ProductID, err)
		}
		// 规格联动：下架置 0，上架置 1。与商品状态同事务，不允许出现
		// "商品已下架但规格仍可售"的中间态。
		onSale := int32(0)
		if in.Status == model.StatusOnSale {
			onSale = 1
		}
		if err := tx.Model(&model.SKU{}).
			Where("product_id = ?", in.ProductID).
			Update("on_sale", onSale).Error; err != nil {
			return fmt.Errorf("sync sku on_sale %d: %w", in.ProductID, err)
		}
		action := model.ActionOffShelf
		if in.Status == model.StatusOnSale {
			action = model.ActionOnSale
		}
		if err := tx.Create(&model.ProductLog{
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
func (r *ProductRepository) Report(ctx context.Context, q model.ProductReportQuery) (out []model.ReportRow, err error) {
	ctx, end := r.begin(ctx, "Report")
	defer func() { end(err) }()
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	qb := r.db.WithContext(ctx).
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
	if q.CategoryID > 0 {
		qb = qb.Where("products.category_id = ?", q.CategoryID)
	}
	var rows []model.ReportRow
	if err := qb.Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("report products: %w", err)
	}
	return rows, nil
}
