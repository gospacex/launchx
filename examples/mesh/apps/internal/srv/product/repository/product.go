// Package repository 持久化层：只使用 launchx 交出的 *gorm.DB，不直引驱动、
// 不开连接。db 由 app.Setup 经 App.DB() 取到后注入，本层不感知它从哪来。
package repository

import (
	"context"
	"errors"

	"github.com/gospacex/otelx/layer"
	"gorm.io/gorm"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/product/model"
)

// ProductRepository 商品存储。
type ProductRepository struct {
	db      *gorm.DB
	traceOn bool // trace.enabled（总闸）：门控本层 repository span 是否入链
}

// New 构造 repository。
func New(db *gorm.DB, traceOn bool) *ProductRepository {
	return &ProductRepository{db: db, traceOn: traceOn}
}

// quietNotFound 「查不到」不是故障：一次未命中是正常 miss，不该污染错误率。
func quietNotFound(err error) bool { return errors.Is(err, gorm.ErrRecordNotFound) }

// FindByID 读商品主表。gorm 的 ErrRecordNotFound 原样上抛，由 service 翻成
// 业务码 —— 在这一层翻会让「查不到」与「库报错」混为一谈。
func (r *ProductRepository) FindByID(ctx context.Context, id uint64) (res *model.Product, err error) {
	ctx, end := layer.Begin(ctx, "repository", "product", "FindByID", r.traceOn, quietNotFound)
	defer func() { end(err) }()

	var p model.Product
	if err := r.db.WithContext(ctx).First(&p, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &p, nil
}

// UpdatePrice 改商品价格。
//
// 方法集刻意只有 FindByID / UpdatePrice 两个：读的是详情（FindByID），写的是
// 价格（UpdatePrice）。**UpdatePrice 只由单测驱动** —— IDL 的 UpdateProduct 在
// 本示例未接线，它存在的理由仅仅是让「写后失效」这条缓存契约有可执行的判据
// （见 service.UpdatePrice 与 spec mesh-entry-chain 的 product 行），
// **不是**一条留有隐藏入口的生产写路径。
//
// 这里**刻意不查 RowsAffected**（别顺手补上）：MySQL 默认对「值没有变化」的
// UPDATE 也报 0 行受影响，故 0 行在语义上是**二义**的 —— 既可能是「该 id 不存在」，
// 也可能是「改的就是同一个值」。据此报错会把一次幂等的重复改价变成 404/500。
// 要区分两者得先查存在性（或开 CLIENT_FOUND_ROWS），属另一件事；
// order 域的 UpdateStatus 之所以查 RowsAffected，是因为它带 `status = ?` 条件、
// 0 行只可能意味着「别人已经改过」，那里不二义。
func (r *ProductRepository) UpdatePrice(ctx context.Context, id uint64, price int64) (err error) {
	ctx, end := layer.Begin(ctx, "repository", "product", "UpdatePrice", r.traceOn, quietNotFound)
	defer func() { end(err) }()

	return r.db.WithContext(ctx).Model(&model.Product{}).
		Where("id = ?", id).
		Update("price", price).Error
}
