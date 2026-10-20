// Package repository 持久化层：只使用 launchx 交出的 *gorm.DB，不直引驱动、
// 不开连接。db 由 app.Setup 经 App.DB() 取到后注入，本层不感知它从哪来。
package repository

import (
	"context"
	"errors"

	"github.com/gospacex/otelx/layer"
	"gorm.io/gorm"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/model"
	"github.com/gospacex/outboxx"
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

// Create 落一张订单 + 明细 + 首条流转日志 + outbox 待发事件（同一事务）。
//
// outbox 是 2026-10-10 加固补的：此前事件在事务外直接 producer.Send，
// 「订单已提交、事件投递失败」= 积分域永远收不到（只留一条 Warn）。
// 现在事件与业务行同事务落库，投递由后台 Runner 补发（app.Setup 装配）。
//
// fill 在事务内、订单行落库后（自增 ID 已知）回调，产出事件体与随事件落库的
// traceparent；传 nil 或返回空 payload 时该行由 Runner 直接放弃（不占重试配额）。
// 投递目标 topic 由 Runner 的 producer 绑定（mqx 配置侧键位），这里只登记
// 逻辑 topic 供对账。
//
// Idempotent=true 时**一行都没插**（含 outbox 事件）：重放不该让积分域再收一次
// 「订单已完成」，否则幂等只做在了订单表上、副作用照旧重复。
func (r *OrderRepository) Create(ctx context.Context, o *model.Order, items []model.OrderItem,
	fill func(orderID uint64) (payload, traceparent string)) (out model.CreateOutput, err error) {
	ctx, end := layer.Begin(ctx, "repository", "order", "Create", r.traceOn, quietNotFound)
	defer func() { end(err) }()

	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 幂等查重必须在**事务内**：放在事务外，两个并发重放会各自查到「没有」
		// 然后双双插入 —— 唯一键只会把第二个变成 duplicate error，那不是幂等，
		// 是「第二次请求失败」。
		var exist model.Order
		switch e := tx.Where("order_no = ?", o.OrderNo).First(&exist).Error; {
		case e == nil:
			out.ID = exist.ID
			out.TotalAmount = exist.TotalAmount
			out.Idempotent = true
			return nil
		case !errors.Is(e, gorm.ErrRecordNotFound):
			return e // 真故障（连接/语法）不能当成「没查到」继续插入
		}

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
		if err := tx.Create(&model.OrderStatusLog{
			OrderID: o.ID, FromStatus: "", ToStatus: o.Status,
		}).Error; err != nil {
			return err
		}
		ev := outboxx.Event{
			Topic:     "order.completed.v1", // = events.TopicOrderCompleted；repository 不 import 事件包（分层纪律），对账时两处必须一致
			EventType: "order.completed",
			Status:    outboxx.StatusPending,
		}
		if fill != nil {
			ev.Payload, ev.Traceparent = fill(o.ID)
		}
		if err := tx.Create(&ev).Error; err != nil {
			return err
		}
		out.ID = o.ID
		out.TotalAmount = o.TotalAmount
		return nil
	})
	return out, err
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
