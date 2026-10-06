// Package repository srv-points 持久层（私有库 points_db，账号 mall_rw）。
//
// 纪律（与 mall 其他三服一致）：
//   - 连表查询一律 Find（多行投影到 slice），禁用 Scan（会静默丢列）
//   - 错误一律 %w 包装；积分域额外用**哨兵错误**区分"账户不存在"、
//     "余额不足"与"幂等键命中"，三者业务语义完全不同（不开户 / 真没钱 /
//     重复记账），混成一条会让调用方把"重复下单"报成"余额不足"
//   - 金额一律 int64（分），不用浮点
package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/gospacex/launchx/examples/mall/pkg/outbox"
)

// 积分域哨兵错误（service 层据此翻译成 5xxx 业务码）
var (
	// ErrAccountNotFound 账户不存在（该用户还没开过户）
	ErrAccountNotFound = errors.New("repository: points account not found")
	// ErrInsufficient 余额不足（有账户但不够扣）
	ErrInsufficient = errors.New("repository: points insufficient")
	// ErrDuplicated 幂等键命中（同 order_no + type 已记过账）
	ErrDuplicated = errors.New("repository: duplicated points ledger")
)

// 流水类型
const (
	TypeEarn    int32 = 1 // 入账
	TypeConsume int32 = 2 // 消费
)

// Account 积分账户（point_accounts，user_id 唯一）
type Account struct {
	ID          uint64    `gorm:"primaryKey;column:id"`
	UserID      uint64    `gorm:"column:user_id"`
	Balance     int64     `gorm:"column:balance"`
	TotalEarned int64     `gorm:"column:total_earned"`
	TotalSpent  int64     `gorm:"column:total_spent"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

// TableName 显式表名（避开 GORM 自动命名歧义）
func (Account) TableName() string { return "point_accounts" }

// Ledger 积分流水（point_ledgers，order_no + type 唯一 = 幂等键）
type Ledger struct {
	ID           uint64    `gorm:"primaryKey;column:id"`
	UserID       uint64    `gorm:"column:user_id"`
	OrderNo      string    `gorm:"column:order_no"`
	Type         int32     `gorm:"column:type"`
	Amount       int64     `gorm:"column:amount"`
	BalanceAfter int64     `gorm:"column:balance_after"`
	Remark       string    `gorm:"column:remark"`
	CreatedAt    time.Time `gorm:"column:created_at"`
}

// TableName 显式表名
func (Ledger) TableName() string { return "point_ledgers" }

// ReportRow Report 2 表 JOIN 聚合行
type ReportRow struct {
	UserID      int64 `gorm:"column:user_id"`
	Balance     int64 `gorm:"column:balance"`
	TotalEarned int64 `gorm:"column:total_earned"`
	TotalSpent  int64 `gorm:"column:total_spent"`
	LedgerCount int32 `gorm:"column:ledger_count"`
	// LastChangeAt 最近变动时间（指针：LEFT JOIN 未命中时为 NULL，
	// 用值类型扫描会让 GORM 报 "unsupported Scan"）
	LastChangeAt *time.Time `gorm:"column:last_change_at"`
}

// ChangeInput 入账/消费事务输入
type ChangeInput struct {
	UserID  uint64
	OrderNo string
	Type    int32
	Amount  int64
	Remark  string
	// OutboxFill 在事务内、流水落库后回调，产出事件体与 traceparent。
	OutboxFill func(ledgerID uint64, balanceAfter int64) (payload, traceparent string)
}

// ChangeOutput 入账/消费事务结果
type ChangeOutput struct {
	LedgerID     uint64
	BalanceAfter int64
}

// PointsRepository points_db 仓储
type PointsRepository struct {
	db *gorm.DB
}

// NewPointsRepository 构造
func NewPointsRepository(db *gorm.DB) *PointsRepository {
	return &PointsRepository{db: db}
}

// DB 暴露句柄（组装期健康检查用）
func (r *PointsRepository) DB() *gorm.DB { return r.db }

// ---------- 单表 CRUD ----------

// GetAccount 单表查询：按用户 ID 取账户
func (r *PointsRepository) GetAccount(ctx context.Context, userID uint64) (*Account, error) {
	var a Account
	err := r.db.WithContext(ctx).Where("user_id = ?", userID).Take(&a).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("get account %d: %w", userID, ErrAccountNotFound)
		}
		return nil, fmt.Errorf("get account %d: %w", userID, err)
	}
	return &a, nil
}

// ListLedgers 单表分页：按用户查流水（id 倒序，最新在前）
func (r *PointsRepository) ListLedgers(ctx context.Context, userID uint64, limit int) ([]Ledger, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200 // 上限写死：流水表增长最快，翻页深度必须封顶
	}
	var rows []Ledger
	if err := r.db.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list ledgers %d: %w", userID, err)
	}
	return rows, nil
}

// HasLedger 单表存在性查询：同 order_no + type 是否已记过账（第二层幂等）
func (r *PointsRepository) HasLedger(ctx context.Context, orderNo string, typ int32) (bool, error) {
	var cnt int64
	if err := r.db.WithContext(ctx).Model(&Ledger{}).
		Where("order_no = ? AND type = ?", orderNo, typ).Count(&cnt).Error; err != nil {
		return false, fmt.Errorf("probe ledger %s/%d: %w", orderNo, typ, err)
	}
	return cnt > 0, nil
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
func (r *PointsRepository) ChangeTx(ctx context.Context, in ChangeInput) (*ChangeOutput, error) {
	out := &ChangeOutput{}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		exists, err := hasLedgerTx(tx, in.OrderNo, in.Type)
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("change %s/%d: %w", in.OrderNo, in.Type, ErrDuplicated)
		}

		var a Account
		err = tx.Where("user_id = ?", in.UserID).Take(&a).Error
		switch {
		case err == nil:
			// 已有账户：按类型加减，并做余额下限校验
		case errors.Is(err, gorm.ErrRecordNotFound):
			if in.Type == TypeConsume {
				return fmt.Errorf("consume %d: %w", in.UserID, ErrAccountNotFound)
			}
			a = Account{UserID: in.UserID} // 首次入账即开户
		default:
			return fmt.Errorf("load account %d: %w", in.UserID, err)
		}

		if in.Type == TypeConsume && a.Balance < in.Amount {
			return fmt.Errorf("consume %d: %w (balance %d, amount %d)", in.UserID, ErrInsufficient, a.Balance, in.Amount)
		}
		if in.Type == TypeEarn {
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
		} else if err := tx.Model(&Account{}).Where("id = ?", a.ID).
			Updates(map[string]any{
				"balance":      a.Balance,
				"total_earned": a.TotalEarned,
				"total_spent":  a.TotalSpent,
			}).Error; err != nil {
			return fmt.Errorf("update account %d: %w", in.UserID, err)
		}

		lg := Ledger{
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

// ---------- 多表 JOIN ----------

// Report 2 表 LEFT JOIN 报表（point_accounts ⋈ point_ledgers）。
//
// 流水是 1:N，计数用 COUNT(DISTINCT) 抵消行放大；最近变动时间用
// MAX(created_at)（MySQL 返回 time.Time，SQLite 返回字符串，故统一
// 用 UNIX_TIMESTAMP 风格的秒值由 SQL 侧转换）。
func (r *PointsRepository) Report(ctx context.Context, userID uint64, limit int) ([]ReportRow, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	q := r.db.WithContext(ctx).
		Table("point_accounts").
		Select("point_accounts.user_id AS user_id, point_accounts.balance, " +
			"point_accounts.total_earned, point_accounts.total_spent, " +
			"COUNT(DISTINCT point_ledgers.id) AS ledger_count, " +
			"COALESCE(MAX(point_ledgers.created_at), point_accounts.updated_at) AS last_change_at").
		Joins("LEFT JOIN point_ledgers ON point_ledgers.user_id = point_accounts.user_id").
		Group("point_accounts.user_id, point_accounts.balance, point_accounts.total_earned, point_accounts.total_spent, point_accounts.updated_at").
		Order("point_accounts.user_id ASC")
	if userID > 0 {
		q = q.Where("point_accounts.user_id = ?", userID)
	}
	var rows []ReportRow
	if err := q.Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("report points: %w", err)
	}
	return rows, nil
}

// hasLedgerTx 事务内查幂等键（复用同一连接，避免事务外读到的"幻象不存在"）
func hasLedgerTx(tx *gorm.DB, orderNo string, typ int32) (bool, error) {
	var cnt int64
	if err := tx.Model(&Ledger{}).Where("order_no = ? AND type = ?", orderNo, typ).Count(&cnt).Error; err != nil {
		return false, fmt.Errorf("probe ledger %s/%d: %w", orderNo, typ, err)
	}
	return cnt > 0, nil
}
