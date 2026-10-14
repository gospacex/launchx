// dtm 分支写通道：Saga（Debit/Refund）与 TCC（TryFreeze/ConfirmFreeze/
// CancelFreeze）的**原始 SQL**，全部跑在 dtmx.Barrier 起的事务里。
//
// 为什么这一层是裸 SQL 而不是 gorm：dtm 官方 barrier（dtmcli.BranchBarrier）
// 只认 *sql.Tx，它要在这个事务里先插 barrier 行、再调业务回调。gorm 没有
// 「把外部事务接进来」的口子（DB() 只给连接池不给 Tx），于是凡是要被 barrier
// 包的写，只能在拿到 *sql.Tx 之后用 database/sql 直写。换 gorm 的唯一后果是
// barrier 插不进业务事务 —— 空补偿/重复回调/悬挂三条守卫全部失效，且当场没有
// 任何报错：账照样落，只是重复回调会重复记账。积分是钱，这条不能赌。
//
// 列名全部来自在库 DDL，一处虚构都没有（判据见 dtmstore_test.go 的
// TestDtmStoreSQLUsesOnlySchemaColumns）：
//   - point_accounts：user_id 唯一键、balance、frozen（TCC 增量列）、total_earned、total_spent
//   - point_ledgers ：(order_no, type) 组合唯一键 —— 账变幂等的第二道闸
//   - barrier       ：dtm 官方 (gid, branch_id, op, barrier_id) 唯一键，由 dtm 自己写
//
// 失败语义：余额/冻结不足、账户不存在这类「重试一万次也不会变」的情况一律
// 走 dtmx.BusinessFailure（官方哨兵 dtmcli.ErrFailure），dtm 判它是业务失败
// 并立即逆序补偿；数据库连不上、SQL 语法错这类「重试可能就好了」的原样上抛，
// 由 dtm 按退避重试。把两者混同的代价是前者被无限重试、后者被当成业务失败
// 一刀切掉。
package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/gospacex/dtmx"
	"github.com/gospacex/launchx/examples/micro/srv-points/internal/model"
)

// dtm 分支面的哨兵错误（service 据此翻成 codes 的 5xxx 段）。
//
// 与 points.go 里那三个既有哨兵分开定义，不复用：本文件每个错误都已经带上了
// dtmx.BusinessFailure 哨兵，而既有的 ErrInsufficient 不带。复用同一个值会让
// 「有没有带官方哨兵」这个判据失效 —— dtm 认不出就会无限重试积分不足。
var (
	// ErrDtmAccountNF 积分账户不存在（该用户没开户）
	ErrDtmAccountNF = errors.New("repository/dtm: points account not found")
	// ErrDtmInsufficient 可用余额不足
	ErrDtmInsufficient = errors.New("repository/dtm: points balance insufficient")
	// ErrDtmFrozenShort 冻结量不足（Try 没成功过，或已被 Confirm/Cancel 消掉）
	ErrDtmFrozenShort = errors.New("repository/dtm: points frozen insufficient")
	// ErrDtmOrderNoTooLong 派生流水键超出 order_no 列宽
	ErrDtmOrderNoTooLong = errors.New("repository/dtm: order_no exceeds column width")
)

// ledgerOrderNoMaxLen point_ledgers.order_no 的列宽（VARCHAR(40)，见
// examples/mall/scripts/schema.sql）。硬编码在这里是因为它是**列的定义**，
// 不是可配置项：改它得改 DDL，不是改配置。
const ledgerOrderNoMaxLen = 40

// tccLedgerPrefix TCC Confirm 流水的 order_no 命名空间前缀。
//
// 为什么只有 TCC 需要：Saga 的 Debit 用原始订单号（流水页与对账都按订单号
// 查，编前缀就查不到了）；而 ConfirmFreeze 也是一条 type=消费 的流水，它若
// 也用原始订单号，同一订单「既走 Saga 又走 TCC」时第二笔会撞上
// uk_ledger_order_type，被条件插入判成「已记过账」而**静默不扣** —— 少给用户
// 积分，且两边都显示成功。给 TCC 段一个独立命名空间，两个判据才互不干扰。
const tccLedgerPrefix = "tcc-"

// ---- 分支 SQL（列名逐字取自 schema.sql；where 里的 >= 守卫是「不足」的判据）----

const (
	// sqlLockAccount 锁账户行并读出余额/冻结量。
	//
	// FOR UPDATE 不是可选的：余额要先读出来算 ledger 的 balance_after，而
	// 读-改之间若不锁，两个并发分支会各读到同一个旧余额，写出两条都自称
	// 「变动后余额 = 旧余额 - 40」的流水，对账时账实不符。
	sqlLockAccount = `select balance, frozen from point_accounts where user_id = ? for update`

	// sqlInsertLedger 条件插流水：(order_no, type) 已有行时受影响行数为 0。
	//
	// 用「insert … select … where not exists」而不是「先查再插」：判据与插入
	// 落在同一条语句里，不存在两个事务同时查到「没有」再各插一行的窗口。
	// MySQL 8 支持该写法（from dual 让它无需再联一张表）。
	sqlInsertLedger = `insert into point_ledgers (user_id, order_no, type, amount, balance_after, remark) ` +
		`select ?, ?, ?, ?, ?, ? from dual ` +
		`where not exists (select 1 from point_ledgers where order_no = ? and type = ?)`

	// Saga 动作段：可用余额↓ 累计消费↑。守卫 balance >= cost 兜住「读到的余额
	// 在加锁前已被别的分支花掉」这一种竞态。
	sqlDebit = `update point_accounts set balance = balance - ?, total_spent = total_spent + ? ` +
		`where user_id = ? and balance >= ?`

	// Saga 补偿段：反向回补。不带守卫：补偿必须能把钱还回去，哪怕期间账户被
	// 别的操作动过（守卫只会让补偿失败并被 dtm 无限重试，用户的钱回不来）。
	sqlRefund = `update point_accounts set balance = balance + ?, total_earned = total_earned + ? ` +
		`where user_id = ?`

	// TCC Try：可用↓ 冻结↑。不落流水 —— 冻结不是消费，记流水会让对账把
	// 「冻结」当成「花掉」，累计消费凭空多一笔。
	sqlTryFreeze = `update point_accounts set balance = balance - ?, frozen = frozen + ? ` +
		`where user_id = ? and balance >= ?`

	// TCC Confirm：冻结↓（真正实扣）+ 累计消费↑。余额已在 Try 段扣过，这里
	// 不再动 balance。
	sqlConfirmFreeze = `update point_accounts set frozen = frozen - ?, total_spent = total_spent + ? ` +
		`where user_id = ? and frozen >= ?`

	// TCC Cancel：冻结↓ 可用↑，完全回到 Try 之前。守卫防的是「重复取消把
	// 余额加成双倍」。
	sqlCancelFreeze = `update point_accounts set frozen = frozen - ?, balance = balance + ? ` +
		`where user_id = ? and frozen >= ?`
)

// RawDB 取 gorm 句柄底层的 *sql.DB。
//
// 唯一的合法入参是 launchx App.DB() 交出的 dbx 句柄（app/setup.go 有
// handlecheck.VerifyOrError 断言它的来源）。它同时也是 dtm barrier 事务的
// 载体 —— barrier 行必须与业务写在**同一个库、同一个事务**里，跨库就插不进
// 这个事务，屏障形同虚设。
//
// nil 入参直接报错：装配期少了一层句柄时若返回 (nil, nil)，故障会被推迟到
// 第一次分支回调，且那时已经在处理一笔真实订单。
func RawDB(gdb *gorm.DB) (*sql.DB, error) {
	if gdb == nil {
		return nil, errors.New("repository/dtm: gorm 句柄为空（须来自 launchx App.DB()，见 app/setup.go）")
	}
	sdb, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("repository/dtm: 取底层 *sql.DB: %w", err)
	}
	if sdb == nil {
		return nil, errors.New("repository/dtm: 底层 *sql.DB 为 nil（gorm 句柄尚未初始化？）")
	}
	return sdb, nil
}

// DtmBranchStore dtm 分支写通道。
type DtmBranchStore struct {
	db *sql.DB
	// traceOn 决定分支面的裸 SQL 写要不要产 db.* span。barrier 只吃 *sql.Tx
	// （gorm 无法外接事务），这层埋点因此不走 dbx 的 gorm callback，得自己开。
	traceOn bool
}

// NewDtmBranchStore 从 gorm 句柄构造（句柄来源的纪律见 RawDB）。
func NewDtmBranchStore(gdb *gorm.DB, traceOn bool) (*DtmBranchStore, error) {
	sdb, err := RawDB(gdb)
	if err != nil {
		return nil, err
	}
	return &DtmBranchStore{db: sdb}, nil
}

// dbOf 返回可用连接（nil 构造的 store 直接判错，不让 nil 解引用发生）。
func (s *DtmBranchStore) dbOf() (*sql.DB, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("repository/dtm: 分支写通道未装配（app/setup.go 是否漏了 NewDtmBranchStore？）")
	}
	return s.db, nil
}

// account 分支事务内读到的账户快照。
type account struct {
	Balance int64
	Frozen  int64
}

// Debit Saga 动作段：扣可用余额 + 记一笔消费流水。
func (s *DtmBranchStore) Debit(ctx context.Context, bar *dtmx.Barrier, b model.DtmBiz) error {
	db, err := s.dbOf()
	if err != nil {
		return err
	}
	return bar.Call(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
		acc, err := s.lockAccount(ctx, tx, b.UserID)
		if err != nil {
			return err
		}
		inserted, err := s.insertLedger(ctx, tx, b.OrderNo, b, model.TypeConsume, b.Cost, acc.Balance-b.Cost, "dtm.saga.debit")
		if err != nil || !inserted {
			return err
		}
		res, err := dtmx.ExecTx(ctx, tx,
			sqlDebit, s.traceOn, b.Cost, b.Cost, b.UserID, b.Cost)
		if err != nil {
			return fmt.Errorf("repository/dtm: debit %s: %w", b.OrderNo, err)
		}
		return checkAffected(res, b.Cost, acc, "debit "+b.OrderNo, ErrDtmInsufficient)
	})
}

// Refund Saga 补偿段：反向回补余额 + 记一笔入账流水。
//
// 幂等由 barrier 承担（同一 gid 的 compensate 段只会真正执行一次）；流水
// 唯一键再兜一层：若 barrier 表不可信，(order_no, type=入账) 已有行同样
// 判成功，不会把用户重复加钱。
func (s *DtmBranchStore) Refund(ctx context.Context, bar *dtmx.Barrier, b model.DtmBiz) error {
	db, err := s.dbOf()
	if err != nil {
		return err
	}
	return bar.Call(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
		acc, err := s.lockAccount(ctx, tx, b.UserID)
		if err != nil {
			return err
		}
		inserted, err := s.insertLedger(ctx, tx, b.OrderNo, b, model.TypeEarn, b.Cost, acc.Balance+b.Cost, "dtm.saga.refund")
		if err != nil || !inserted {
			return err
		}
		res, err := dtmx.ExecTx(ctx, tx,
			sqlRefund, s.traceOn, b.Cost, b.Cost, b.UserID)
		if err != nil {
			return fmt.Errorf("repository/dtm: refund %s: %w", b.OrderNo, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("repository/dtm: refund %s rows: %w", b.OrderNo, err)
		} else if n == 0 {
			// 账户在加锁后就不存在了：不可能（我们刚锁住它），出现即数据异常。
			// 报基础失败让 dtm 重试，而不是当业务失败放过去 —— 后者会让
			// 「补偿没还钱」被记成成功。
			return fmt.Errorf("repository/dtm: refund %s: 账户 %d 在事务内消失", b.OrderNo, b.UserID)
		}
		return nil
	})
}

// TryFreeze TCC 第一段：可用余额↓ 冻结↑。不写流水（见 sqlTryFreeze 注释）。
func (s *DtmBranchStore) TryFreeze(ctx context.Context, bar *dtmx.Barrier, b model.DtmBiz) error {
	db, err := s.dbOf()
	if err != nil {
		return err
	}
	return bar.Call(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
		acc, err := s.lockAccount(ctx, tx, b.UserID)
		if err != nil {
			return err
		}
		res, err := dtmx.ExecTx(ctx, tx,
			sqlTryFreeze, s.traceOn, b.Cost, b.Cost, b.UserID, b.Cost)
		if err != nil {
			return fmt.Errorf("repository/dtm: try freeze %s: %w", b.OrderNo, err)
		}
		return checkAffected(res, b.Cost, acc, "try freeze", ErrDtmInsufficient)
	})
}

// ConfirmFreeze TCC 第二段：冻结↓（真正实扣）+ 一条带 tcc- 命名空间的消费流水。
func (s *DtmBranchStore) ConfirmFreeze(ctx context.Context, bar *dtmx.Barrier, b model.DtmBiz) error {
	db, err := s.dbOf()
	if err != nil {
		return err
	}
	return bar.Call(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
		acc, err := s.lockAccount(ctx, tx, b.UserID)
		if err != nil {
			return err
		}
		inserted, err := s.insertLedger(ctx, tx, tccLedgerOrderNo(b.OrderNo), b,
			model.TypeConsume, b.Cost, acc.Balance, "dtm.tcc.confirm")
		if err != nil || !inserted {
			return err
		}
		res, err := dtmx.ExecTx(ctx, tx,
			sqlConfirmFreeze, s.traceOn, b.Cost, b.Cost, b.UserID, b.Cost)
		if err != nil {
			return fmt.Errorf("repository/dtm: confirm freeze %s: %w", b.OrderNo, err)
		}
		return checkAffected(res, b.Cost, acc, "confirm freeze", ErrDtmFrozenShort)
	})
}

// CancelFreeze TCC 第三段：冻结↓ 可用↑，完全回退。
func (s *DtmBranchStore) CancelFreeze(ctx context.Context, bar *dtmx.Barrier, b model.DtmBiz) error {
	db, err := s.dbOf()
	if err != nil {
		return err
	}
	return bar.Call(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
		acc, err := s.lockAccount(ctx, tx, b.UserID)
		if err != nil {
			return err
		}
		res, err := dtmx.ExecTx(ctx, tx,
			sqlCancelFreeze, s.traceOn, b.Cost, b.Cost, b.UserID, b.Cost)
		if err != nil {
			return fmt.Errorf("repository/dtm: cancel freeze %s: %w", b.OrderNo, err)
		}
		return checkAffected(res, b.Cost, acc, "cancel freeze", ErrDtmFrozenShort)
	})
}

// ---- 事务内公用片段 ----

// lockAccount 锁住并读出账户行。
//
// 账户不存在与余额不足必须可分辨（ErrDtmAccountNF vs ErrDtmInsufficient）：
// 调用方对前者该去开户，对后者该去催款，混成一个码就没法处置。
// 两者都带官方哨兵 —— 都是重试一万次也不会变的业务事实。
func (s *DtmBranchStore) lockAccount(ctx context.Context, tx *sql.Tx, userID int64) (account, error) {
	var acc account
	err := dtmx.QueryRowTx(ctx, tx,
		sqlLockAccount, s.traceOn, []any{&acc.Balance, &acc.Frozen}, userID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return account{}, dtmx.BusinessFailure(fmt.Errorf("lock point_accounts %d: %w", userID, ErrDtmAccountNF))
	case err != nil:
		return account{}, fmt.Errorf("repository/dtm: lock point_accounts %d: %w", userID, err)
	}
	return acc, nil
}

// insertLedger 插流水，返回是否真插了。
//
// orderNo 由调用点显式给出（Saga 两段给原始订单号，TCC Confirm 给命名空间键），
// 理由见 tccLedgerPrefix。
//
// false = (order_no, type) 已有行，即这一笔此前已经记过账。调用方必须据此
// **跳过余额改动并返回成功**：这不是错误（dtm 重试是常态），也不是「照做」
// （照做就是重复记账）。这条判据是 barrier 之外唯一的幂等闸。
func (s *DtmBranchStore) insertLedger(ctx context.Context, tx *sql.Tx, orderNo string, b model.DtmBiz,
	typ int32, amount, balanceAfter int64, remark string) (bool, error) {

	if len(orderNo) > ledgerOrderNoMaxLen {
		// 超列宽会被 MySQL 严格模式当成 SQL 错误 → dtm 判基础失败 → 无限重试
		// 同一个注定失败的分支。这里提前判成业务失败。
		return false, dtmx.BusinessFailure(fmt.Errorf("ledger %q: %w (max %d)", orderNo, ErrDtmOrderNoTooLong, ledgerOrderNoMaxLen))
	}
	res, err := dtmx.ExecTx(ctx, tx,
		sqlInsertLedger, s.traceOn,
		b.UserID, orderNo, typ, amount, balanceAfter, remark, orderNo, typ)
	if err != nil {
		return false, fmt.Errorf("repository/dtm: insert ledger %s/%d: %w", orderNo, typ, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("repository/dtm: insert ledger %s/%d rows: %w", orderNo, typ, err)
	}
	return n > 0, nil
}

// tccLedgerOrderNo 给 TCC Confirm 的流水派生命名空间键。理由见 tccLedgerPrefix。
func tccLedgerOrderNo(orderNo string) string { return tccLedgerPrefix + orderNo }

// checkAffected 把「受影响 0 行」翻成业务失败。
//
// 0 行的唯一成因是 WHERE 守卫不成立（余额/冻结不足）：守卫已把余额写进 SQL
// 的表达式里，数据库层面就不可能把它扣成负数，所以 0 行是可信判据，不是
// 「也许失败了」的模糊信号 —— 用它当错误条件不会误伤并发写（并发写命中的是
// 自己那条加锁后的行）。
//
// 一个必须知道的 MySQL 细节：驱动返回的是**实际改变**的行数，不是匹配到的
// 行数（除非连 DSN 带 clientFoundRows=true）。本文件每条 UPDATE 的 SET 里都
// 带 ± cost 的算术，值一定变，故「匹配到 ⇒ 改变 ⇒ 1 行」成立；但若将来有人
// 加一条「值没变的 UPDATE」（例如把 status 重新设成同一个值），0 行就会被
// 误判成「余额不足」，把一笔正常写报成业务失败。改这两处之一时要一起看这里。
func checkAffected(res sql.Result, cost int64, acc account, op string, short error) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("repository/dtm: %s rows: %w", op, err)
	}
	if n == 0 {
		return dtmx.BusinessFailure(fmt.Errorf("%s: %w (cost %d, balance %d, frozen %d)",
			op, short, cost, acc.Balance, acc.Frozen))
	}
	return nil
}
