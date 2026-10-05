// Package repository 数据层：经 launchx 装配的 *gorm.DB 读写订单表。
//
// 边界：只做「一次 SQL / 一次事务」的读写，不含参数校验与业务决策；
// 每个方法自开一个 `repository.*` span，事务里的每条 SQL 由 dbx 的 gorm
// 回调各自成 span，挂在同一个方法 span 下。
package repository

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/gorm"

	"github.com/gospacex/launchx/examples/monolith/internal/model"
)

// Repository 订单仓储：持权 launchx 挂载的 DB 句柄与调用链 tracer。
type Repository struct {
	db     *gorm.DB
	tracer trace.Tracer
}

// New 构造仓储。db 必须来自 App.DB() 的断言结果——本层不自建连接。
func New(db *gorm.DB, tracer trace.Tracer) *Repository {
	return &Repository{db: db, tracer: tracer}
}

// span 开启本层的子 span（层前缀 repository），并把错误计入 span 状态。
func (r *Repository) span(ctx context.Context, name string) (context.Context, trace.Span) {
	return r.tracer.Start(ctx, "repository."+name)
}

// recordError 把非「记录不存在」的错误标进 span：gorm 的 ErrRecordNotFound
// 是一次正常 miss，不算 span 错误。
func recordError(span trace.Span, err error) {
	if err == nil || errors.Is(err, gorm.ErrRecordNotFound) {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// Create 单表写入（INSERT orders）。
func (r *Repository) Create(ctx context.Context, o *model.Order) error {
	ctx, span := r.span(ctx, "Create")
	defer span.End()
	err := r.db.WithContext(ctx).Create(o).Error
	recordError(span, err)
	return err
}

// List 单表批量查询（SELECT orders）。
func (r *Repository) List(ctx context.Context, limit int) ([]model.Order, error) {
	ctx, span := r.span(ctx, "List")
	defer span.End()
	var out []model.Order
	err := r.db.WithContext(ctx).Order("id DESC").Limit(limit).Find(&out).Error
	recordError(span, err)
	return out, err
}

// Get 按订单号查单条（SELECT orders）。
func (r *Repository) Get(ctx context.Context, orderNo string) (*model.Order, error) {
	ctx, span := r.span(ctx, "Get")
	defer span.End()
	var out model.Order
	err := r.db.WithContext(ctx).Where("order_no = ?", orderNo).Take(&out).Error
	recordError(span, err)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateWithItems 多表事务：orders 一条 INSERT + order_items 若干条 INSERT。
//
// 事务里的每条 SQL 各自成 span（同一父 = 本方法 span，互为兄弟），
// 这是 T13「事务中的多条 SQL 各自成 span」的落点，故刻意不把两次写入
// 合并成一条批量语句。
func (r *Repository) CreateWithItems(ctx context.Context, o *model.Order, items []model.OrderItem) error {
	ctx, span := r.span(ctx, "CreateWithItems")
	defer span.End()
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		tx = tx.WithContext(ctx) // 保证事务内 SQL 仍挂在同一个 span 下
		if err := tx.Create(o).Error; err != nil {
			return err
		}
		for i := range items {
			items[i].OrderNo = o.OrderNo
			if err := tx.Create(&items[i]).Error; err != nil {
				return err
			}
		}
		return nil
	})
	recordError(span, err)
	return err
}

// GetWithUser 连表查询：orders JOIN users。
//
// 单独一个方法、单独一条 SQL——JOIN 语句必须自己成一个 span，
// 与本层其它 SELECT 混在一起（比如预加载合并）会让 T13 的连表判据失效。
func (r *Repository) GetWithUser(ctx context.Context, orderNo string) (*model.OrderWithUser, error) {
	ctx, span := r.span(ctx, "GetWithUser")
	defer span.End()
	const joinSQL = "SELECT o.id, o.order_no, o.user_id, o.item, o.qty, o.amount, o.status, u.name AS user_name, u.level AS user_level FROM orders o JOIN users u ON o.user_id = u.id WHERE o.order_no = ?"
	var out model.OrderWithUser
	err := r.db.WithContext(ctx).Raw(joinSQL, orderNo).Take(&out).Error
	recordError(span, err)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateStatus 更新订单状态（UPDATE orders）。
func (r *Repository) UpdateStatus(ctx context.Context, orderNo, status string) error {
	ctx, span := r.span(ctx, "UpdateStatus")
	defer span.End()
	err := r.db.WithContext(ctx).Model(&model.Order{}).
		Where("order_no = ?", orderNo).Update("status", status).Error
	recordError(span, err)
	return err
}

// Delete 物理删除订单（DELETE orders）。
func (r *Repository) Delete(ctx context.Context, orderNo string) error {
	ctx, span := r.span(ctx, "Delete")
	defer span.End()
	err := r.db.WithContext(ctx).Where("order_no = ?", orderNo).Delete(&model.Order{}).Error
	recordError(span, err)
	return err
}

// DeleteItems 删除订单明细（DELETE order_items）。
//
// 物理删除主子数据时一并清理，避免 demo 反复跑留下孤儿明细；单独开 span，
// 便于一眼看清「删两张表 = 两条 SQL」。
func (r *Repository) DeleteItems(ctx context.Context, orderNo string) error {
	ctx, span := r.span(ctx, "DeleteItems")
	defer span.End()
	err := r.db.WithContext(ctx).Where("order_no = ?", orderNo).Delete(&model.OrderItem{}).Error
	recordError(span, err)
	return err
}
