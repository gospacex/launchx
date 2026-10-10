// Package repository 持久化层：只使用 launchx 交出的 *gorm.DB，不直引驱动、
// 不开连接。db 由 app.Setup 经 App.DB() 取到后注入，本层不感知它从哪来。
package repository

import (
	"context"
	"errors"
	"time"

	"github.com/gospacex/otelx/layer"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/points/model"
)

// PointsRepository 积分存储。
type PointsRepository struct {
	db      *gorm.DB
	traceOn bool // trace.enabled（总闸）：门控本层 repository span 是否入链
}

// New 构造 repository。
func New(db *gorm.DB, traceOn bool) *PointsRepository {
	return &PointsRepository{db: db, traceOn: traceOn}
}

// quietNotFound 「查不到」不是故障：一次未命中是正常 miss，不该污染错误率。
func quietNotFound(err error) bool { return errors.Is(err, gorm.ErrRecordNotFound) }

// AddBalance 给用户账户加 delta 积分（无账户则建户）。
//
// 刻意用 `clause.OnConflict` 把「读旧值 + 写新值」压成库的**一条** upsert 语句
// （MySQL 渲染为 `ON DUPLICATE KEY UPDATE balance = balance + ?`），而不是
// 「事务内先查后写」：后者在两个并发消费各自查到旧值时会把两笔加成算成整数倍，
// 而 upsert 的累加在库侧原子完成。
func (r *PointsRepository) AddBalance(ctx context.Context, userID uint64, delta int64) (err error) {
	ctx, end := layer.Begin(ctx, "repository", "points", "AddBalance", r.traceOn, quietNotFound)
	defer func() { end(err) }()

	acc := model.PointAccount{UserID: userID, Balance: delta, UpdatedAt: time.Now()}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "user_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"balance":    gorm.Expr("balance + ?", delta),
			"updated_at": time.Now(),
		}),
	}).Create(&acc).Error
}

// Balance 读账户。gorm 的 ErrRecordNotFound 原样上抛，由 service 翻成业务语义
// （「未开户 = 0 积分」）—— 在这一层翻会让「查不到」与「库报错」混为一谈。
func (r *PointsRepository) Balance(ctx context.Context, userID uint64) (acc *model.PointAccount, err error) {
	ctx, end := layer.Begin(ctx, "repository", "points", "Balance", r.traceOn, quietNotFound)
	defer func() { end(err) }()

	var a model.PointAccount
	if err := r.db.WithContext(ctx).First(&a, "user_id = ?", userID).Error; err != nil {
		return nil, err
	}
	return &a, nil
}

// AddLog 落一条积分流水（append-only）。
func (r *PointsRepository) AddLog(ctx context.Context, l *model.PointsLog) (err error) {
	ctx, end := layer.Begin(ctx, "repository", "points", "AddLog", r.traceOn, quietNotFound)
	defer func() { end(err) }()

	return r.db.WithContext(ctx).Create(l).Error
}
