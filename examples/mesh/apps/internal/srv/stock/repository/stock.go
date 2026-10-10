// Package repository 持久化层：只使用 launchx 交出的 *gorm.DB，不直引驱动、
// 不开连接。db 由 app.Setup 经 App.DB() 取到后注入，本层不感知它从哪来。
package repository

import (
	"context"
	"errors"
	"time"

	"github.com/gospacex/otelx/layer"
	"gorm.io/gorm"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/stock/model"
)

// StockRepository 库存存储。
type StockRepository struct {
	db      *gorm.DB
	traceOn bool // trace.enabled（总闸）：门控本层 repository span 是否入链
}

// New 构造 repository。
func New(db *gorm.DB, traceOn bool) *StockRepository {
	return &StockRepository{db: db, traceOn: traceOn}
}

// quietNotFound 「查不到」不是故障：一次未命中是正常 miss，不该污染错误率。
func quietNotFound(err error) bool { return errors.Is(err, gorm.ErrRecordNotFound) }

// FindByProductID 读库存主表。gorm 的 ErrRecordNotFound 原样上抛，由 service
// 翻成业务语义（读路径「无库存记录」视为 0，不报错）—— 在这一层翻会让
// 「查不到」与「库报错」混为一谈。
func (r *StockRepository) FindByProductID(ctx context.Context, productID uint64) (res *model.Stock, err error) {
	ctx, end := layer.Begin(ctx, "repository", "stock", "FindByProductID", r.traceOn, quietNotFound)
	defer func() { end(err) }()

	var s model.Stock
	if err := r.db.WithContext(ctx).First(&s, "product_id = ?", productID).Error; err != nil {
		return nil, err
	}
	return &s, nil
}

// TryDecrease 条件扣减：仅当可售量足够时才把 available 减 n，否则影响 0 行。
//
// 返回值刻意是 (ok, err)，让调用方能区分**两种完全不同的 0 行语义**：
//   - ok=false, err=nil：条件未命中（库存不足 / 无该商品）→ 上层回 StockNotEnough；
//   - ok=true,  err!=nil 不会出现；err!=nil 表示库报错（如连接断）→ 上层回 Internal。
//
// 若只返回 error（像 order 的 UpdateStatus 那样把 0 行编成 error），上层就分不出
// 「库存不够」与「库坏了」—— 前者是业务 4xx，后者是故障 5xx，映射必须不同。
//
// 扣减的真相就在这条**带 `available >= ?` 条件的 UPDATE** 上：并发的每个请求
// 各自提交一条语句，由库在行锁内判定条件；条件不满足者影响 0 行、不写值。
// 这把「读旧值 → 判断 → 写新值」压成单条原子语句，从根上杜绝超卖 ——
// 在 service 里「先读后写」无论加不加进程内锁，都有跨进程失效的窗口。
//
// version 每次成功扣减 +1（乐观锁版本位，便于外部观测扣减次数）；updated_at
// 显式写 —— gorm 只对**结构体**形式的 Updates 自动维护该列，本处用 map + Expr
// 表达原子表达式，自动维护不生效，故带上。
func (r *StockRepository) TryDecrease(ctx context.Context, productID uint64, n int32) (ok bool, err error) {
	ctx, end := layer.Begin(ctx, "repository", "stock", "TryDecrease", r.traceOn, quietNotFound)
	defer func() { end(err) }()

	res := r.db.WithContext(ctx).Model(&model.Stock{}).
		Where("product_id = ? AND available >= ?", productID, n).
		Updates(map[string]any{
			"available":  gorm.Expr("available - ?", n),
			"version":    gorm.Expr("version + 1"),
			"updated_at": time.Now(),
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}
