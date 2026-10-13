// dtm 分支面的持久层：barrier 事务内的原始 SQL（Saga 销量 +n / -n）。
//
// 与 product.go 的 gorm 写路径是**两套**，不是两套写法：
//   - product.go 的事务由 gorm 自己开（db.Transaction），幂等由 product_logs
//     的业务校验 + 调用方语义提供；
//   - 本文件的写必须跑在 dtm 官方 barrier 开的事务里（dtmx.Barrier.Call 把
//     *sql.Tx 递进来），屏障行与业务写同事务提交。因此这里用 *sql.Tx 原始 SQL，
//     不用 gorm —— 官方回调签名（BarrierBusiFunc）本来就是 *sql.Tx，硬套 gorm
//     只会多一层 Statement.ConnPool 强转，屏障与业务写在哪个连接上反而更难确认。
//
// 三条纪律：
//  1. **写只用条件更新 / upsert，不读出来再算**。事务里一次 SELECT 就多一个与
//     并发写入互相看见的窗口，而「不得扣成负数」靠的是 WHERE 里的量判据 +
//     RowsAffected 判定。唯一的 SELECT（sqlSelectSold）在条件更新**判负之后**
//     才发生，且只用于给错误分类，不参与任何算术 —— 判据与读之间没有窗口。
//  2. **账不平即业务失败**。返回 dtmx.BusinessFailure（包官方 dtmcli.ErrFailure）：
//     dtm 收到后立刻逆序补偿；返回普通 error 会被当基础设施故障按退避重试，
//     一个永远平不了的账能把整条 Saga 拖成重试风暴。
//  3. **只有一个事务、一条写**。销量是单表单行计数，不需要第二条写；而拆成两次
//     连接写，等于给对账留一个「加了没记 / 记了没加」的中间态。
package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/gospacex/dtmx"
	"github.com/gospacex/launchx/examples/micro/srv-product/internal/model"
)

// 本文件的哨兵错误（service 据此翻成商品域 2xxx 业务码）。
//
// 与 product.go 里既有的 ErrNotFound 分开定义，不复用：既有的那个**不带**
// dtmx.BusinessFailure 哨兵（它服务于 gorm 路径，语义是「查询没命中」），复用会
// 让「有没有带官方哨兵」这个判据失效 —— dtm 认不出就会无限重试。
var (
	// ErrSalesNotFound 该商品在 product_sales 里**没有行**（连一条都没有）。
	//
	// 单独一个哨兵而不是并进「账不平」：处置完全不同 —— 补一条行即可恢复，
	// 而账不平要去查是谁把 sold 改坏了。混成一个码，排障方向会直接跑偏。
	ErrSalesNotFound = errors.New("repository/dtm: product_sales row not found")
	// ErrSalesImbalance 账已不平：要减的量超过当前 sold（`sold >= ?` 守卫未命中
	// 但行确实存在）。
	ErrSalesImbalance = errors.New("repository/dtm: sold insufficient to reduce (sales imbalance)")
)

// RawDB 取 gorm 句柄自带的 *sql.DB，供 dtm 官方 barrier 开事务。
//
// **必须来自 launchx dbx 句柄**（App.DB()，见 app/setup.go 的 handlecheck 断言）：
// 分支面禁止自行 gorm.Open 另开连接。理由两条，少一条都会出事：
//  1. 事务必须落在业务库的同一条连接上 —— barrier 行与销量写要原子提交/回滚，
//     另开的连接插同一张 barrier 表也只能各提交各的，屏障等于没有；
//  2. dbx 句柄带 OTel 回调，本层开的 repository span 下面才挂得住每条 SQL 的
//     dbx span；裸 gorm.Open 出来的句柄一条都不产（pkg/handlecheck.VerifyDBX
//     断言的正是这件事）。
func RawDB(gdb *gorm.DB) (*sql.DB, error) {
	if gdb == nil {
		return nil, errors.New("repository/dtm: gorm 句柄为 nil（须来自 launchx App.DB()，见 app/setup.go）")
	}
	db, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("repository/dtm: 取底层 *sql.DB 失败（句柄不是 launchx dbx 交出的连接?）: %w", err)
	}
	if db == nil {
		return nil, errors.New("repository/dtm: 底层 *sql.DB 为 nil（gorm 句柄尚未初始化）")
	}
	return db, nil
}

// ---------- 分支 SQL ----------
//
// 列名逐字取自 examples/micro/scripts/schema-micro.sql 的 product_db 段：
//
//	create table if not exists product_sales (
//	  product_id BIGINT UNSIGNED NOT NULL,
//	  sold       BIGINT NOT NULL DEFAULT 0,
//	  updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
//	  PRIMARY KEY (product_id))
//
// 没有第五个列可写 —— 销量分支的账变只落 sold 一列。

const (
	// sqlAddSales Saga 正向：销量 +n，**行不存在则 upsert 建行**。
	//
	// 为什么必须是 upsert 而不是 update：product_sales 是销量专表，一个从没卖过
	// 的商品在表里就没有行。update-only 写法会让「第一单」什么都不扣 —— 销量
	// 永久少 1，而四个分支全部报成功，没有任何报错可查。
	//
	// 为什么不写 `values(sold)`（老式 upsert 惯用法）：MySQL 8.0.20 起该函数被
	// 标记废弃，且在 VALUES 里出现列名时语义容易与 ODKU 的加量混淆。显式把加量
	// 再写一遍（第三个占位符）虽然多一个实参，但「建行初值」与「重复行加量」
	// 两个语义在语句里各自独立、可分别断言。
	sqlAddSales = "insert into product_sales (product_id, sold) values (?, ?) " +
		"on duplicate key update sold = sold + ?"

	// sqlReduceSales Saga 补偿：销量 -n，且带 `sold >= ?` 守卫。
	//
	// **守卫是「不得扣成负数」的唯一执行点**：扣成负销量意味着账已不平
	// （正向加了 n 却扣了超过 n，或有人手改过 sold），而负销量不会被任何
	// 告警发现 —— 首页只是「卖得比买的多」。守卫把这条不可能性下推到数据库，
	// 事务里一次读出来再算的写法则把它换成一个有窗口的判断。
	sqlReduceSales = "update product_sales set sold = sold - ? where product_id = ? and sold >= ?"

	// sqlSelectSold 诊断读：条件更新判 0 行之后，**只为给错误分类**用。
	//
	// 它不参与任何算术，也不参与「要不要写」的判断（那是 WHERE 守卫 + 影响行数
	// 的事）。没有它，0 行就只有一个笼统的「销量不足」，而调用方无法区分
	// 「补一行」与「查数据漂移」两种处置。
	sqlSelectSold = "select sold from product_sales where product_id = ?"
)

// DtmAddSales Saga 正向分支：销量 +n（行不存在则建行）。
//
// 不判 RowsAffected：upsert 的两种结局（建行=1 / 命中重复=2）都代表「写成功了」，
// 只有执行本身报错才是失败。判一个可能为 2 的值反而会写出「affected == 1 才算
// 成功」这种把命中重复判成失败的错。
func (r *ProductRepository) DtmAddSales(ctx context.Context, tx *sql.Tx, cmd model.DtmSalesCmd) (err error) {
	ctx, end := r.begin(ctx, model.DtmAddSalesBranch)
	defer func() { end(err) }()

	if tx == nil {
		return errors.New("repository/dtm: 销量写必须在 barrier 事务内执行（nil *sql.Tx）")
	}
	q, pid := cmd.Quantity, int64(cmd.ProductID)
	if _, err := tx.ExecContext(ctx, sqlAddSales, pid, q, q); err != nil {
		// 执行失败是基础设施故障：原样上抛，dtm 按退避重试。**不能**包成业务失败
		// —— 那等于告诉 dtm「别重试了，去回滚」，一次连接抖动会变成一次真实回退。
		return fmt.Errorf("dtm add sales product %d x%d order %s: %w", cmd.ProductID, q, cmd.OrderNo, err)
	}
	return nil
}

// DtmReduceSales Saga 补偿分支：销量 -n，带 `sold >= ?` 守卫。
//
// 落到本方法时 dtm 的 barrier 已替我们分好两种情形：正向确实到过（要回退）与
// 正向从未到达（空补偿，官方语义下业务回调根本不会被调用）。故本方法**不需要**
// 自己查有没有加过 —— 那正是 barrier 存在的理由。
//
// 条件更新影响 0 行时**必须报错而不是放行**：两种成因都不是「无事发生」，
// 见 classifyReduceMiss。
func (r *ProductRepository) DtmReduceSales(ctx context.Context, tx *sql.Tx, cmd model.DtmSalesCmd) (err error) {
	ctx, end := r.begin(ctx, model.DtmReduceSalesBranch)
	defer func() { end(err) }()

	if tx == nil {
		return errors.New("repository/dtm: 销量写必须在 barrier 事务内执行（nil *sql.Tx）")
	}
	q, pid := cmd.Quantity, int64(cmd.ProductID)
	res, err := tx.ExecContext(ctx, sqlReduceSales, q, pid, q)
	if err != nil {
		return fmt.Errorf("dtm reduce sales product %d x%d order %s: %w", cmd.ProductID, q, cmd.OrderNo, err)
	}
	affected, aerr := res.RowsAffected()
	if aerr != nil {
		// 取不到影响行数时我们**不知道**这笔写有没有生效，绝不能当成「没命中」
		// 判成业务失败 —— 那会让一笔可能已经提交的写被重复扣减。
		return fmt.Errorf("dtm reduce sales product %d: 读不到影响行数: %w", cmd.ProductID, aerr)
	}
	if affected > 0 {
		return nil
	}
	sentinel, diagErr := classifyReduceMiss(ctx, tx, cmd)
	if diagErr != nil {
		// 连「为什么没命中」都读不出来：这是基础设施故障，不能猜一个业务失败
		// 上报 —— 猜错的后果是 dtm 判死这笔全局事务（或反过来无限重试）。
		return diagErr
	}
	return dtmx.BusinessFailure(sentinel)
}

// classifyReduceMiss 给「条件更新 0 行」分类。
//
// 返回值二选一：
//   - (业务错误, nil)：行不存在（ErrSalesNotFound）或账不平（ErrSalesImbalance）；
//   - (nil, 诊断错误)：诊断读本身失败，调用方按基础失败上抛让 dtm 重试。
//
// 诊断读刻意放在条件更新**之后**：守卫与判 0 行都不依赖它，它只是把一个笼统的
// 「没扣成」翻译成可处置的成因。先读后判的写法才是危险的那种 —— 那才是真正的
// read-modify-write。
func classifyReduceMiss(ctx context.Context, tx *sql.Tx, cmd model.DtmSalesCmd) (error, error) {
	var sold int64
	err := tx.QueryRowContext(ctx, sqlSelectSold, int64(cmd.ProductID)).Scan(&sold)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("dtm reduce sales product %d x%d order %s: %w",
			cmd.ProductID, cmd.Quantity, cmd.OrderNo, ErrSalesNotFound), nil
	case err != nil:
		return nil, fmt.Errorf("dtm reduce sales product %d: 诊断读失败，无法判定 0 行成因: %w", cmd.ProductID, err)
	default:
		// 行在、守卫不过 —— 只可能是 sold < quantity，即账不平。
		return fmt.Errorf("dtm reduce sales product %d x%d order %s: sold=%d: %w",
			cmd.ProductID, cmd.Quantity, cmd.OrderNo, sold, ErrSalesImbalance), nil
	}
}
