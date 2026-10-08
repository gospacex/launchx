// Package repository srv-points 持久层（私有库 points_db，账号 mall_rw）。
//
// 在分层链上的位置：service → **repository** → model。它的上游只有 service，
// 下游只有 model 与 dbx 句柄。
//
// 句柄来源：db 是 launchx App.DB() 交出的 *gorm.DB（dbx 组件，自带 OTel
// 回调）。本包不 gorm.Open、不 import 任何驱动 —— 装配期由
// pkg/handlecheck.VerifyDBX 断言这件事，跑错句柄会在启动时直接失败。
//
// 纪律（与 mall 其他三服一致）：
//   - 连表查询一律 Find（多行投影到 slice），禁用 Scan（会静默丢列）
//   - 1:N 子表 JOIN 必须 GROUP BY 收敛，计数一律 COUNT(DISTINCT ...)，
//     求和一律 COALESCE(SUM(...), 0)（无流水时 SUM 返回 NULL，直接扫进
//     int64 会是 0 之外的未定义行为，且 NULL 参与后续比较恒为假）
//   - 错误一律 %w 包装；积分域额外用**哨兵错误**区分"账户不存在"、
//     "余额不足"与"幂等键命中"，三者业务语义完全不同（不开户 / 真没钱 /
//     重复记账），混成一条会让调用方把"重复下单"报成"余额不足"
//   - 金额一律 int64（分），不用浮点
//   - 每个方法开一个 repository.points.<op> span，与 service / handler 的
//     span 串成一条链
package repository

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/gospacex/launchx/examples/micro/srv-points/internal/model"
	"github.com/gospacex/launchx/examples/pkg/outbox"
	"github.com/gospacex/otelx/layer"
)

// 积分域哨兵错误（service 层据此翻译成 5xxx 业务码）
var (
	// ErrNotFound 账户不存在（该用户还没开过户 → 5001）
	ErrNotFound = errors.New("repository: points account not found")
	// ErrInsufficient 余额不足（有账户但不够扣 → 5002）
	ErrInsufficient = errors.New("repository: points insufficient")
	// ErrDuplicated 幂等键命中（同 order_no + type 已记过账 → 5003）
	ErrDuplicated = errors.New("repository: duplicated points ledger")
)

// ChangeInput 入账/消费事务输入
type ChangeInput struct {
	UserID  uint64
	OrderNo string
	Type    int32
	Amount  int64
	Remark  string
	// OutboxFill 在事务内、流水落库后回调，产出事件体与 traceparent。
	// nil 时 outbox 行 payload/traceparent 为空（Runner 直接放弃该行，
	// 不占重试配额）。
	OutboxFill func(ledgerID uint64, balanceAfter int64) (payload, traceparent string)
}

// ChangeOutput 入账/消费事务结果
type ChangeOutput struct {
	LedgerID     uint64
	BalanceAfter int64
}

// PointsRepository points_db 仓储
type PointsRepository struct {
	db      *gorm.DB
	traceOn bool
}

// NewPointsRepository 构造。
//
// db 必须是 launchx App.DB() 交出的 dbx 句柄：只有它带 OTel 回调，
// 本层开的 span 下面才挂得住每条 SQL 的 dbx span。
func NewPointsRepository(db *gorm.DB, traceOn bool) *PointsRepository {
	return &PointsRepository{db: db, traceOn: traceOn}
}

// DB 暴露句柄（pkg/handlecheck 断言、装配期健康检查用）
func (r *PointsRepository) DB() *gorm.DB { return r.db }

// begin 开 repository 层 span，返回「收 error 再收尾」的闭包。
//
// traceOn=false 时返回空闭包，调用方一律 `defer func() { end(err) }()`，
// 不必在每个方法里写 if。为什么闭包要收 error：span 句柄刻意不交给调用方
// （本层因此零 otel 依赖），代价是调用方也标不了状态 —— 收 error 由闭包
// 代标，失败才在 Jaeger 上可见（否则「SQL 报错」与「SQL 成功」同色）。
//
// quiet 规则：ErrNotFound 是正常 miss，不是故障 —— 与单体示例 recordError
// 的同一条语义，命中即豁免错误标记，避免正常未命污染错误率。
func (r *PointsRepository) begin(ctx context.Context, op string) (context.Context, layer.End) {
	return layer.Begin(ctx, "repository", "points", op, r.traceOn, quietNotFound)
}

// quietNotFound 「记录不存在」不是故障。两种形态都认：本层已归一化出的
// ErrNotFound，以及 dbx/gorm 直接抛出的 ErrRecordNotFound（未归一化路径）。
func quietNotFound(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, gorm.ErrRecordNotFound)
}

// ---------- 单表 CRUD ----------

// HasLedger 单表存在性查询：同 order_no + type 是否已记过账。
//
// 与事务内的幂等判定（ChangeTx 里的 hasLedgerTx）分工：这一条供事务之外的
// 对账/补偿路径用（例如 MQ 重投时先探一次，避免为已知重复的请求开事务）。
func (r *PointsRepository) HasLedger(ctx context.Context, orderNo string, typ int32) (out bool, err error) {
	ctx, end := r.begin(ctx, "HasLedger")
	defer func() { end(err) }()
	var cnt int64
	if err := r.db.WithContext(ctx).Model(&model.Ledger{}).
		Where("order_no = ? AND type = ?", orderNo, typ).Count(&cnt).Error; err != nil {
		return false, fmt.Errorf("probe ledger %s/%d: %w", orderNo, typ, err)
	}
	return cnt > 0, nil
}

// ---------- 多表 JOIN 查询 ----------

// GetAccount 2 表 LEFT JOIN + 聚合：按用户 ID 取账户，并把流水侧聚合出来的
// 笔数 / 累计额 / 最近一笔变动一起带出（point_accounts ⋈ point_ledgers）。
//
// point_ledgers 是 1:N，JOIN 会把账户行放大 N 倍，故一律 COUNT(DISTINCT ...)
// 计数、COALESCE(SUM(...), 0) 求和，并 GROUP BY 收敛回一行；结果仍走 Find
// 到 slice（保持"JOIN 一律 Find、禁 Scan"）。
//
// last_change_amount 为什么写成"时间戳等于最大时间戳"的条件 MAX：聚合函数只
// 能给出极值本身，给不出"取得该极值的那一行的其它列"—— 直接 MAX(amount) 拿到
// 的是历史最大金额而不是最后一笔。这里用相关子查询把最大时间戳求出来做条件，
// 在同一次聚合里把同行金额取出来，不为它再开一次往返。
func (r *PointsRepository) GetAccount(ctx context.Context, userID uint64) (out *model.AccountDetail, err error) {
	ctx, end := r.begin(ctx, "GetAccount")
	defer func() { end(err) }()
	var rows []model.AccountDetail
	err = r.db.WithContext(ctx).
		Table("point_accounts").
		Select("point_accounts.user_id AS user_id, point_accounts.balance, "+
			"point_accounts.total_earned, point_accounts.total_spent, "+
			"COUNT(DISTINCT point_ledgers.id) AS ledger_count, "+
			"COUNT(DISTINCT CASE WHEN point_ledgers.type = ? THEN point_ledgers.id END) AS earn_count, "+
			"COUNT(DISTINCT CASE WHEN point_ledgers.type = ? THEN point_ledgers.id END) AS consume_count, "+
			"COALESCE(SUM(CASE WHEN point_ledgers.type = ? THEN point_ledgers.amount ELSE 0 END), 0) AS earned_amount, "+
			"COALESCE(SUM(CASE WHEN point_ledgers.type = ? THEN point_ledgers.amount ELSE 0 END), 0) AS spent_amount, "+
			"MAX(point_ledgers.created_at) AS last_change_at, "+
			"COALESCE(MAX(CASE WHEN point_ledgers.created_at = "+
			"(SELECT MAX(l2.created_at) FROM point_ledgers l2 WHERE l2.user_id = point_accounts.user_id) "+
			"THEN point_ledgers.amount ELSE 0 END), 0) AS last_change_amount",
			model.TypeEarn, model.TypeConsume, model.TypeEarn, model.TypeConsume).
		Joins("LEFT JOIN point_ledgers ON point_ledgers.user_id = point_accounts.user_id").
		Where("point_accounts.user_id = ?", userID).
		Group("point_accounts.user_id, point_accounts.balance, point_accounts.total_earned, point_accounts.total_spent").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("get account %d: %w", userID, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("get account %d: %w", userID, ErrNotFound)
	}
	out = &rows[0]
	return out, nil
}

// ListLedgers 2 表 LEFT JOIN 分页：按用户查流水（id 倒序，最新在前），
// 并把账户表的当前余额一起带出（point_ledgers ⋈ point_accounts）。
//
// point_accounts 相对流水是 1:1（user_id 唯一），LEFT JOIN 不放大行数，
// 因此不必 GROUP BY —— 加了反而会把"取前 N 笔"变成聚合后再 LIMIT，语义就变了。
//
// 余额为什么要在流水页带出：流水本身只有 balance_after（那时点的值），
// 回答不了"现在还剩多少"。不 JOIN 的话调用方要再调一次 GetAccount，
// 而流水是"这一页里每笔都要配一个数"的场景，N+1 次往返不可接受。
func (r *PointsRepository) ListLedgers(ctx context.Context, q model.ListLedgersQuery) (out []model.LedgerDetail, err error) {
	ctx, end := r.begin(ctx, "ListLedgers")
	defer func() { end(err) }()
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200 // 上限写死：流水表增长最快，翻页深度必须封顶
	}
	var rows []model.LedgerDetail
	if err := r.db.WithContext(ctx).
		Table("point_ledgers").
		Select("point_ledgers.id, point_ledgers.user_id, point_ledgers.order_no, point_ledgers.type, "+
			"point_ledgers.amount, point_ledgers.balance_after, point_ledgers.remark, point_ledgers.created_at, "+
			"COALESCE(point_accounts.balance, 0) AS balance").
		Joins("LEFT JOIN point_accounts ON point_accounts.user_id = point_ledgers.user_id").
		Where("point_ledgers.user_id = ?", q.UserID).
		Order("point_ledgers.id DESC").
		Limit(limit).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list ledgers %d: %w", q.UserID, err)
	}
	return rows, nil
}

// ---------- 多表事务 ----------

// ChangeTx 2 表事务 + outbox：账户余额更新 + 流水插入 + outbox 事件。
//
// 幂等由**数据库唯一键**兜底（point_ledgers 的 uk_order_no_type）：事务内先查
// 同键流水，命中即返回 ErrDuplicated 且不改余额。这层是事实来源，
// kvx 那层只是快速失败（见 service 注释）—— 只有 kvx 时，Redis 丢 key
// 就等于幂等失效，会重复加钱。
//
// 账户首次入账即开户（upsert）：积分账户是"用到了才有"，独立开户接口
// 会让调用方多一次往返，且失败时留下"有流水没账户"的脏数据。
func (r *PointsRepository) ChangeTx(ctx context.Context, in ChangeInput) (out *ChangeOutput, err error) {
	ctx, end := r.begin(ctx, "ChangeTx")
	defer func() { end(err) }()
	out = &ChangeOutput{}
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		exists, err := hasLedgerTx(tx, in.OrderNo, in.Type)
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("change %s/%d: %w", in.OrderNo, in.Type, ErrDuplicated)
		}

		var a model.Account
		err = tx.Where("user_id = ?", in.UserID).Take(&a).Error
		switch {
		case err == nil:
			// 已有账户：按类型加减，并做余额下限校验
		case errors.Is(err, gorm.ErrRecordNotFound):
			if in.Type == model.TypeConsume {
				return fmt.Errorf("consume %d: %w", in.UserID, ErrNotFound)
			}
			a = model.Account{UserID: in.UserID} // 首次入账即开户
		default:
			return fmt.Errorf("load account %d: %w", in.UserID, err)
		}

		if in.Type == model.TypeConsume && a.Balance < in.Amount {
			return fmt.Errorf("consume %d: %w (balance %d, amount %d)", in.UserID, ErrInsufficient, a.Balance, in.Amount)
		}
		if in.Type == model.TypeEarn {
			a.Balance += in.Amount
			a.TotalEarned += in.Amount
		} else {
			a.Balance -= in.Amount
			a.TotalSpent += in.Amount
		}
		if a.ID == 0 {
			if err := tx.Create(&a).Error; err != nil {
				return fmt.Errorf("insert account %d: %w", in.UserID, err)
			}
		} else if err := tx.Model(&model.Account{}).Where("id = ?", a.ID).
			Updates(map[string]any{
				"balance":      a.Balance,
				"total_earned": a.TotalEarned,
				"total_spent":  a.TotalSpent,
			}).Error; err != nil {
			return fmt.Errorf("update account %d: %w", in.UserID, err)
		}

		lg := model.Ledger{
			UserID:       in.UserID,
			OrderNo:      in.OrderNo,
			Type:         in.Type,
			Amount:       in.Amount,
			BalanceAfter: a.Balance,
			Remark:       in.Remark,
		}
		if err := tx.Create(&lg).Error; err != nil {
			// 唯一键冲突：并发下两个请求同时通过存在性检查，由数据库裁决
			return fmt.Errorf("insert ledger %s: %w", in.OrderNo, err)
		}
		ev := outbox.Event{
			Topic:     "points.changed.v1",
			EventType: "points.changed",
			Status:    outbox.StatusPending,
		}
		if in.OutboxFill != nil {
			ev.Payload, ev.Traceparent = in.OutboxFill(lg.ID, a.Balance)
		}
		if err := tx.Create(&ev).Error; err != nil {
			return fmt.Errorf("insert outbox event: %w", err)
		}
		out.LedgerID = lg.ID
		out.BalanceAfter = a.Balance
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("points change tx: %w", err)
	}
	return out, nil
}

// ---------- 多表 JOIN 报表 ----------

// Report 2 表 LEFT JOIN 报表（point_accounts ⋈ point_ledgers）。
//
// 流水是 1:N，计数用 COUNT(DISTINCT) 抵消行放大；最近变动时间用
// MAX(created_at)（MySQL 返回 time.Time，SQLite 返回字符串，故统一
// 用 UNIX_TIMESTAMP 风格的秒值由 SQL 侧转换）。
func (r *PointsRepository) Report(ctx context.Context, q model.PointsReportQuery) (out []model.ReportRow, err error) {
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
		Table("point_accounts").
		Select("point_accounts.user_id AS user_id, point_accounts.balance, " +
			"point_accounts.total_earned, point_accounts.total_spent, " +
			"COUNT(DISTINCT point_ledgers.id) AS ledger_count, " +
			"COALESCE(MAX(point_ledgers.created_at), point_accounts.updated_at) AS last_change_at").
		Joins("LEFT JOIN point_ledgers ON point_ledgers.user_id = point_accounts.user_id").
		Group("point_accounts.user_id, point_accounts.balance, point_accounts.total_earned, point_accounts.total_spent, point_accounts.updated_at").
		Order("point_accounts.user_id ASC")
	if q.UserID > 0 {
		qb = qb.Where("point_accounts.user_id = ?", q.UserID)
	}
	var rows []model.ReportRow
	if err := qb.Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("report points: %w", err)
	}
	return rows, nil
}

// hasLedgerTx 事务内查幂等键（复用同一连接，避免事务外读到的"幻象不存在"）
func hasLedgerTx(tx *gorm.DB, orderNo string, typ int32) (bool, error) {
	var cnt int64
	if err := tx.Model(&model.Ledger{}).Where("order_no = ? AND type = ?", orderNo, typ).Count(&cnt).Error; err != nil {
		return false, fmt.Errorf("probe ledger %s/%d: %w", orderNo, typ, err)
	}
	return cnt > 0, nil
}
