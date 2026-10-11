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

// CreditOnce 幂等入账：按 order_id 只入账一次，且「写流水 + 加余额」在同一事务。
//
// 两条保证各自有可执行的落点，别把它们读成同一件事：
//
//  1. **幂等**（防重投多加分）：以 `points_log.order_id` 的唯一索引为闸门。
//     快路径先 `COUNT` 一次，命中即返回 applied=false；
//     并发下两个消费者都可能读到 0，此时**第二个 insert 会撞唯一键**报错 →
//     事务回滚（余额一分没加）→ 消息按至少一次语义重投 → 下次 COUNT 命中 →
//     applied=false。所以快路径是「优化」，唯一索引才是「正确性」。
//
//  2. **原子**（防余额与流水对不上账）：两条语句在**同一事务**里，
//     任一步失败整体回滚。此前两者是事务外的两条独立语句，落库失败时
//     余额已加而流水缺失 —— 账面上凭空多出一笔无凭据的分。
//
// applied=false 表示「本笔此前已入账」，**不是错误**：调用方据此吞掉即可。
func (r *PointsRepository) CreditOnce(ctx context.Context, userID, orderID uint64, delta int64) (applied bool, err error) {
	ctx, end := layer.Begin(ctx, "repository", "points", "CreditOnce", r.traceOn, nil)
	defer func() { end(err) }()

	terr := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 幂等快路径：已入账过就什么都不做（不重复加分、不重复记流水）。
		var n int64
		if e := tx.Model(&model.PointsLog{}).Where("order_id = ?", orderID).Count(&n).Error; e != nil {
			return e
		}
		if n > 0 {
			applied = false
			return nil
		}
		// 先写流水（同时是幂等闸门的落点），再加余额：顺序有意义 ——
		// 若反过来，余额已加而流水插入撞唯一键时回滚，会让「重投 + 回滚」
		// 反复白跑；先插入则第二次直接走快路径返回。
		if e := r.addLog(ctx, tx, &model.PointsLog{
			UserID: userID, OrderID: orderID, Delta: delta, CreatedAt: time.Now(),
		}); e != nil {
			return e
		}
		if e := r.addBalance(ctx, tx, userID, delta); e != nil {
			return e
		}
		applied = true
		return nil
	})
	if terr != nil {
		return false, terr
	}
	return applied, nil
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

// addBalance 给用户账户加 delta 积分（无账户则建户）。tx 由调用方的事务给出 ——
// 本仓库的写入口只有 CreditOnce，原子性由它负责，这里不再自己开事务
// （嵌套事务在 gorm 里是 savepoint，语义不同，别以为能省事）。
//
// 刻意用 `clause.OnConflict` 把「读旧值 + 写新值」压成库的**一条** upsert 语句
// （MySQL 渲染为 `ON DUPLICATE KEY UPDATE balance = balance + ?`），而不是
// 「事务内先查后写」：后者在两个并发消费各自查到旧值时会把两笔加成算成整数倍，
// 而 upsert 的累加在库侧原子完成。
func (r *PointsRepository) addBalance(ctx context.Context, tx *gorm.DB, userID uint64, delta int64) (err error) {
	ctx, end := layer.Begin(ctx, "repository", "points", "AddBalance", r.traceOn, nil)
	defer func() { end(err) }()

	acc := model.PointAccount{UserID: userID, Balance: delta, UpdatedAt: time.Now()}
	return tx.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "user_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"balance":    gorm.Expr("balance + ?", delta),
			"updated_at": time.Now(),
		}),
	}).Create(&acc).Error
}

// addLog 落一条积分流水（append-only）。tx 由 CreditOnce 的事务给出，理由见 addBalance。
func (r *PointsRepository) addLog(ctx context.Context, tx *gorm.DB, l *model.PointsLog) (err error) {
	ctx, end := layer.Begin(ctx, "repository", "points", "AddLog", r.traceOn, nil)
	defer func() { end(err) }()

	return tx.WithContext(ctx).Create(l).Error
}
