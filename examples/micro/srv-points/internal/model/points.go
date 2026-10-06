// Package model srv-points 的数据模型与领域 DTO。
//
// 为什么单独成层（而不是像之前那样把结构体散在 repository 里）：
//   - 表结构是**契约**：service 的出入参、handler 的 pb 转换、repository 的
//     查询投影，三处引用的是同一批定义。放在 repository 里会逼着 service
//     反向 import 持久层，链子就绕回去了。
//   - 本包**零依赖**：不 import gorm、不 import pb、不 import 任何 SDK。
//     它是分层链的最下游（route → middleware → handler → service →
//     repository → model），只能被别人依赖，不依赖别人。
//
// 三类定义各就各位：
//   - 表结构（Account / Ledger）：一行 = 一张表
//   - 查询投影（AccountDetail / LedgerDetail / ReportRow）：JOIN 出来的结果行
//   - 领域出入参（*Cmd / *Query / *Result）：service 的入参与返回值
package model

import "time"

// 流水类型（point_ledgers.type）
const (
	TypeEarn    int32 = 1 // 入账
	TypeConsume int32 = 2 // 消费
)

// ---------- 表结构 ----------

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

// ---------- 查询投影（JOIN / 聚合结果行）----------

// AccountDetail 账户详情投影（point_accounts ⋈ point_ledgers 聚合）
//
// 为什么不再只查账户主表：账户页要展示"这个账户到底动过几笔、最近一笔是
// 什么时候"，之前只查主表的做法是"页面自己翻流水页去凑"，结果要么多一次
// 往返、要么这一栏永远空着。point_ledgers 相对账户是 1:N，聚合后 GROUP BY
// 收敛回一行，代价只是一次索引扫描 —— 比多一轮往返便宜。
//
// 余额仍以主表 balance 为准（它是每次入账/消费事务内同步推进的权威值），
// 聚合出来的 earned_amount / spent_amount 只用于对账与展示，不替代它。
type AccountDetail struct {
	UserID      uint64 `gorm:"column:user_id"`
	Balance     int64  `gorm:"column:balance"`
	TotalEarned int64  `gorm:"column:total_earned"`
	TotalSpent  int64  `gorm:"column:total_spent"`
	// LedgerCount 流水总笔数（COUNT DISTINCT，抵消 1:N 行放大）
	LedgerCount int32 `gorm:"column:ledger_count"`
	// EarnCount / ConsumeCount 入账与消费笔数（条件 DISTINCT，不为它们各加一次往返）
	EarnCount    int32 `gorm:"column:earn_count"`
	ConsumeCount int32 `gorm:"column:consume_count"`
	// EarnedAmount / SpentAmount 流水侧累计入账/消费额，与主表 total_earned /
	// total_spent 同源却在两条写路径上各自推进，不一致即意味着某次事务只落了一半
	EarnedAmount int64 `gorm:"column:earned_amount"`
	SpentAmount  int64 `gorm:"column:spent_amount"`
	// LastChangeAt 最近变动时间（指针：LEFT JOIN 未命中时为 NULL，
	// 用值类型扫描会让 GORM 报 "unsupported Scan"）
	LastChangeAt *time.Time `gorm:"column:last_change_at"`
	// LastChangeAmount 最近一笔变动金额
	LastChangeAmount int64 `gorm:"column:last_change_amount"`
}

// LedgerDetail 流水投影（point_ledgers ⋈ point_accounts）
//
// 带出账户当前余额：流水页要回答"这笔之后还剩多少"与"现在还剩多少"两个问题，
// 后者只有账户表有。point_accounts 相对流水是 1:1（user_id 唯一），
// LEFT JOIN 不放大行数，因此不必 GROUP BY。
type LedgerDetail struct {
	ID           uint64    `gorm:"column:id"`
	UserID       uint64    `gorm:"column:user_id"`
	OrderNo      string    `gorm:"column:order_no"`
	Type         int32     `gorm:"column:type"`
	Amount       int64     `gorm:"column:amount"`
	BalanceAfter int64     `gorm:"column:balance_after"`
	Remark       string    `gorm:"column:remark"`
	CreatedAt    time.Time `gorm:"column:created_at"`
	// Balance 该用户账户当前余额（账户缺失时 COALESCE 成 0）
	Balance int64 `gorm:"column:balance"`
}

// ReportRow Report 2 表 JOIN 聚合行
type ReportRow struct {
	UserID      int64 `gorm:"column:user_id"`
	Balance     int64 `gorm:"column:balance"`
	TotalEarned int64 `gorm:"column:total_earned"`
	TotalSpent  int64 `gorm:"column:total_spent"`
	LedgerCount int32 `gorm:"column:ledger_count"`
	// LastChangeAt 最近变动时间（指针：LEFT JOIN 未命中时为 NULL）
	LastChangeAt *time.Time `gorm:"column:last_change_at"`
}

// ---------- 领域出入参（service 层流通，不含 pb / 不含 DB）----------

// GetAccountQuery 查账户查询
type GetAccountQuery struct {
	UserID uint64
}

// EarnCmd 入账命令
type EarnCmd struct {
	UserID  uint64
	OrderNo string
	Amount  int64
	Remark  string
}

// ConsumeCmd 消费命令
type ConsumeCmd struct {
	UserID  uint64
	OrderNo string
	Amount  int64
	Remark  string
}

// ChangeResult 入账/消费结果
type ChangeResult struct {
	// LedgerID 本次产生的流水号（幂等重放时为 0）
	LedgerID uint64
	// BalanceAfter 变动后的余额（幂等重放时为 0：重放不重算余额，
	// 调用方应据此不再覆盖本地缓存的余额）
	BalanceAfter int64
}

// ListLedgersQuery 流水列表查询
type ListLedgersQuery struct {
	UserID uint64
	Limit  int
}

// PointsReportQuery 积分报表查询
type PointsReportQuery struct {
	// UserID 为 0 表示不限定用户（出全量账户报表）
	UserID uint64
	Limit  int
}
