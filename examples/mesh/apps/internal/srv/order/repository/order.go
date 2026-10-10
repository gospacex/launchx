// Package repository 持久化层：只使用 launchx 交出的 *gorm.DB，不直引驱动、
// 不开连接。db 由 app.Setup 经 App.DB() 取到后注入，本层不感知它从哪来。
package repository

import (
	"context"
	"errors"

	"github.com/gospacex/otelx/layer"
	"gorm.io/gorm"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/model"
)

// OrderRepository 订单存储。
type OrderRepository struct {
	db      *gorm.DB
	traceOn bool // trace.enabled（总闸）：门控本层 repository span 是否入链
}

// New 构造 repository。
func New(db *gorm.DB, traceOn bool) *OrderRepository {
	return &OrderRepository{db: db, traceOn: traceOn}
}

// quietNotFound 「查不到」不是故障：一次未命中是正常 miss，不该污染错误率。
func quietNotFound(err error) bool { return errors.Is(err, gorm.ErrRecordNotFound) }

// Create 落一张订单 + 明细 + 首条流转日志（同一事务）。
func (r *OrderRepository) Create(ctx context.Context, o *model.Order, items []model.OrderItem) (err error) {
	ctx, end := layer.Begin(ctx, "repository", "order", "Create", r.traceOn, quietNotFound)
	defer func() { end(err) }()

	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(o).Error; err != nil {
			return err
		}
		for i := range items {
			items[i].OrderID = o.ID
		}
		if len(items) > 0 {
			if err := tx.Create(&items).Error; err != nil {
				return err
			}
		}
		return tx.Create(&model.OrderStatusLog{
			OrderID: o.ID, FromStatus: "", ToStatus: o.Status,
		}).Error
	})
}

// FindByID 读订单主表。gorm 的 ErrRecordNotFound 原样上抛，由 service 翻成
// 业务码 —— 在这一层翻会让「查不到」与「库报错」混为一谈。
func (r *OrderRepository) FindByID(ctx context.Context, id uint64) (res *model.Order, err error) {
	ctx, end := layer.Begin(ctx, "repository", "order", "FindByID", r.traceOn, quietNotFound)
	defer func() { end(err) }()

	var o model.Order
	if err := r.db.WithContext(ctx).First(&o, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &o, nil
}

// ItemsByOrderID 读明细。
func (r *OrderRepository) ItemsByOrderID(ctx context.Context, id uint64) (items []model.OrderItem, err error) {
	ctx, end := layer.Begin(ctx, "repository", "order", "ItemsByOrderID", r.traceOn, quietNotFound)
	defer func() { end(err) }()

	if err := r.db.WithContext(ctx).Where("order_id = ?", id).Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

// UpdateStatus 改状态并记流转日志（同一事务）。
//
// 先按 fromStatus 做条件更新：并发下两个请求都想把 created 改成 paid 时，
// 只有一个的 RowsAffected 为 1 —— 这把「状态机竞态」变成库层的原子判定，
// 比在 service 里先读后写可靠。
func (r *OrderRepository) UpdateStatus(ctx context.Context, id uint64, from, to string) (err error) {
	ctx, end := layer.Begin(ctx, "repository", "order", "UpdateStatus", r.traceOn, quietNotFound)
	defer func() { end(err) }()

	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.Order{}).
			Where("id = ? AND status = ?", id, from).
			Update("status", to)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errors.New("order status changed by another request")
		}
		return tx.Create(&model.OrderStatusLog{
			OrderID: id, FromStatus: from, ToStatus: to,
		}).Error
	})
}
