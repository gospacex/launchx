// dtm 分支面的持久层：barrier 事务内的原始 SQL。
//
// 与 stock.go 的 gorm 写路径是**两套**，不是两套写法：
//   - stock.go 的事务由 gorm 自己开（db.Transaction），幂等由 stock_reservations
//     的唯一键 + 幂等查重提供；
//   - 本文件的写必须跑在 dtm 官方 barrier 开的事务里（dtmx.Barrier.Call 把
//     *sql.Tx 递进来），屏障行与业务写同事务提交。因此这里用 *sql.Tx 原始 SQL，
//     不用 gorm —— 官方回调签名（BarrierBusiFunc）本来就是 *sql.Tx，硬套 gorm
//     只会多一层 Statement.ConnPool 强转，屏障与业务写在哪个连接上反而更难确认。
//
// 三条纪律：
//  1. **只条件更新，不读出来再算**。事务里一次 SELECT 就多一个与并发预占互相
//     看见的窗口，而防超卖靠的是 WHERE 里的量判据 + RowsAffected 判定。
//     dbmock 假驱动对事务内任何 Query 直接报错，这条纪律因此是被测试钉住的。
//  2. **不足即业务失败**。返回 dtmx.BusinessFailure（包官方 dtmcli.ErrFailure）：
//     dtm 收到后立刻逆序补偿/回滚；返回普通 error 会被当基础设施故障按退避重试，
//     一次「卖完了」能把整条 Saga 拖成重试风暴。
//  3. **库存更新与流水插入同一个 tx**。分两次连接写，等于给对账留一个
//     「扣了没流水」或「有流水没扣」的中间态，而那正是账实不符最难查的形态。
package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/gospacex/dtmx"
	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/model"
)

// ErrFrozenInsufficient 冻结量不足（Confirm/Cancel 的量超过 frozen 列的量）。
//
// 单独一个哨兵而不是复用 ErrInsufficient：两者的处置完全不同 —— 可用不足是
// 「商品卖完了，去补货」，冻结不足是「账已经漂了，确认/取消的量对不上冻结」，
// 前者是经营问题、后者是数据问题，报同一个码会把排查方向带偏。
var ErrFrozenInsufficient = errors.New("repository: frozen stock insufficient")

// RawDB 取 gorm 句柄自带的 *sql.DB，供 dtm 官方 barrier 开事务。
//
// **必须来自 launchx dbx 句柄**：分支面禁止自行 gorm.Open 另开连接。理由两条，
// 少一条都会出事：
//  1. 事务必须落在业务库的同一条连接上 —— barrier 行与业务写要原子提交/回滚，
//     另开的连接插同一张 barrier 表也只能各提交各的，屏障等于没有；
//  2. dbx 句柄带 OTel 回调，本层开的 repository span 下面才挂得住每条 SQL 的
//     dbx span；裸 gorm.Open 出来的句柄一条都不产（pkg/handlecheck.VerifyDBX
//     断言的正是这件事）。
func RawDB(gdb *gorm.DB) (*sql.DB, error) {
	if gdb == nil {
		return nil, errors.New("repository: dtm 句柄为 nil（拿不到 *sql.DB 就无法把 barrier 插进业务事务）")
	}
	db, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("repository: dtm 句柄不是 launchx dbx 交出的连接（%w）—— 分支面只接受 App.DB()", err)
	}
	if db == nil {
		return nil, errors.New("repository: dtm 句柄底层连接为 nil（barrier 事务无法建立）")
	}
	return db, nil
}

// ---------- 分支 SQL ----------
//
// 列名逐字取自 examples/mall/scripts/schema.sql 的 stocks / stock_logs 建表语句
// 与 examples/micro/scripts/schema-micro.sql 的 frozen 增列。写错列名在真机上
// 是 unknown column，而那时账已经扣了一半。

const (
	// Saga Deduct：可售↓、总量↓。**总量必须同减**：AdjustTx 的入库/盘点是按同量
	// 改 total 与 available；只减 available 不减 total，一次入库就会被算两遍
	// （库存实际少了、账上又补回来），随后就能卖超。
	dtmDeductSQL = "update stocks set total = total - ?, available = available - ?, version = version + 1 " +
		"where product_id = ? and available >= ?"
	// Saga Release：Deduct 的严格逆运算（同量同向反做）。回补没有下限判据 ——
	// 回补只会把量加回去，唯一要挡的是「商品根本没有库存行」，由影响 0 行兜住。
	dtmReleaseSQL = "update stocks set total = total + ?, available = available + ?, version = version + 1 " +
		"where product_id = ?"
	// TCC TryFreeze：可售 → 冻结（可被 Cancel 原样退回，故 total 不动）。
	dtmTryFreezeSQL = "update stocks set available = available - ?, frozen = frozen + ?, version = version + 1 " +
		"where product_id = ? and available >= ?"
	// TCC ConfirmFreeze：冻结 → 实扣。total 同减，理由同 dtmDeductSQL。
	dtmConfirmFreezeSQL = "update stocks set total = total - ?, frozen = frozen - ?, version = version + 1 " +
		"where product_id = ? and frozen >= ?"
	// TCC CancelFreeze：冻结 → 可售，完全回原状。
	dtmCancelFreezeSQL = "update stocks set available = available + ?, frozen = frozen - ?, version = version + 1 " +
		"where product_id = ? and frozen >= ?"
	// 流水：与库存更新同 tx。列名取自 schema.sql 的 stock_logs。
	dtmLogSQL = "insert into stock_logs (product_id, delta, reason, operator_id) values (?, ?, ?, ?)"
)

// 流水 reason 前缀：Saga 与 TCC 的账变混在同一张表上，必须能一眼分清是哪条链写的。
// 带上 order_no 是因为流水没有分支表可关联，全靠这串文本把账变钉回那张单。
const (
	dtmReasonDeduct  = "dtm saga deduct %s"
	dtmReasonRelease = "dtm saga release %s"
	dtmReasonFreeze  = "dtm tcc freeze %s"
	dtmReasonConfirm = "dtm tcc confirm %s"
	dtmReasonCancel  = "dtm tcc cancel %s"
)

// DtmDeduct Saga 正向：扣减可售库存并记流水（dtm 分支序里的第一个分支）。
//
// 判据是 WHERE 里的 available >= ?：条件更新没命中即库存不足，按业务失败上抛，
// dtm 据此立刻逆序补偿已成功的分支，而不是等着重试。
func (r *StockRepository) DtmDeduct(ctx context.Context, tx *sql.Tx, cmd model.DtmStockCmd) (err error) {
	ctx, end := r.begin(ctx, "DtmDeduct")
	defer func() { end(err) }()

	q, n := int64(cmd.Quantity), int64(cmd.ProductID)
	return r.apply(ctx, tx, cmd, dtmDeductSQL, []any{q, q, n, q},
		fmt.Errorf("dtm deduct product %d x%d: %w", cmd.ProductID, cmd.Quantity, ErrInsufficient),
		-int32(n), dtmReasonDeduct)
}

// DtmRelease Saga 补偿：Deduct 的严格逆运算。
//
// 落到本方法时有两种情形，barrier 已替我们分好：正向确实到过（要回补）与正向
// 从未到达（空补偿，官方语义下业务回调根本不会被调用）。故本方法**不需要**自己
// 去查有没有扣过 —— 那正是 barrier 存在的理由。
func (r *StockRepository) DtmRelease(ctx context.Context, tx *sql.Tx, cmd model.DtmStockCmd) (err error) {
	ctx, end := r.begin(ctx, "DtmRelease")
	defer func() { end(err) }()

	q, n := int64(cmd.Quantity), int64(cmd.ProductID)
	return r.apply(ctx, tx, cmd, dtmReleaseSQL, []any{q, q, n},
		fmt.Errorf("dtm release product %d: %w", cmd.ProductID, ErrNotFound),
		int32(n), dtmReasonRelease)
}

// DtmTryFreeze TCC 一阶段：可售 → 冻结。不动 total，Cancel 才好原样退回。
func (r *StockRepository) DtmTryFreeze(ctx context.Context, tx *sql.Tx, cmd model.DtmStockCmd) (err error) {
	ctx, end := r.begin(ctx, "DtmTryFreeze")
	defer func() { end(err) }()

	q, n := int64(cmd.Quantity), int64(cmd.ProductID)
	return r.apply(ctx, tx, cmd, dtmTryFreezeSQL, []any{q, q, n, q},
		fmt.Errorf("dtm freeze product %d x%d: %w", cmd.ProductID, cmd.Quantity, ErrInsufficient),
		-int32(n), dtmReasonFreeze)
}

// DtmConfirmFreeze TCC 二阶段：冻结 → 实扣，永久生效。
func (r *StockRepository) DtmConfirmFreeze(ctx context.Context, tx *sql.Tx, cmd model.DtmStockCmd) (err error) {
	ctx, end := r.begin(ctx, "DtmConfirmFreeze")
	defer func() { end(err) }()

	q, n := int64(cmd.Quantity), int64(cmd.ProductID)
	return r.apply(ctx, tx, cmd, dtmConfirmFreezeSQL, []any{q, q, n, q},
		fmt.Errorf("dtm confirm freeze product %d x%d: %w", cmd.ProductID, cmd.Quantity, ErrFrozenInsufficient),
		-int32(n), dtmReasonConfirm)
}

// DtmCancelFreeze TCC 二阶段：冻结 → 可售。
//
// 冻结不足在这里**必须报出来**而不是当无事发生：Cancel 的量对不上 Try 的量，
// 说明冻结账已经漂了；静默放过等于把差额永远留在 frozen 列里，而 frozen 不参与
// 可售判定，账面可用看着正常、实际已经锁死了一批货。
func (r *StockRepository) DtmCancelFreeze(ctx context.Context, tx *sql.Tx, cmd model.DtmStockCmd) (err error) {
	ctx, end := r.begin(ctx, "DtmCancelFreeze")
	defer func() { end(err) }()

	q, n := int64(cmd.Quantity), int64(cmd.ProductID)
	return r.apply(ctx, tx, cmd, dtmCancelFreezeSQL, []any{q, q, n, q},
		fmt.Errorf("dtm cancel freeze product %d x%d: %w", cmd.ProductID, cmd.Quantity, ErrFrozenInsufficient),
		int32(n), dtmReasonCancel)
}

// apply 一次「条件更新 + 流水」；deficit 是影响 0 行时要上抛的**已格式化**错误
// （哨兵在调用点包好了上下文，service 据此翻译成对应业务码）。
//
// 五个分支都收敛到这里，是为了让「不足 → 业务失败 → 流水不写」这条链只有一个地方：
// 复制五份的话，某一处漏包 dtmx.BusinessFailure 就是一次永久重试风暴，而症状要等到
// 真机跑一遍 saga 才看得见。
func (r *StockRepository) apply(
	ctx context.Context, tx *sql.Tx, cmd model.DtmStockCmd,
	query string, args []any, deficit error, delta int32, reasonFmt string,
) error {
	if tx == nil {
		return errors.New("repository: dtm 分支写必须在 barrier 事务内执行（nil *sql.Tx）")
	}
	res, err := dtmx.ExecTx(ctx, tx, dtmx.OpQuery,
		query, r.traceOn, args...)
	if err != nil {
		// 执行失败是基础设施故障：原样上抛，dtm 按退避重试。**不能**包成业务失败。
		return fmt.Errorf("dtm branch write product %d: %w", cmd.ProductID, err)
	}
	affected, aerr := res.RowsAffected()
	if aerr != nil {
		// 取不到影响行数时我们**不知道**这笔写有没有生效，绝不能当成「没命中」
		// 判成业务失败 —— 那会让一笔可能已经提交的写被无限重试（重复扣减）。
		return fmt.Errorf("dtm branch write product %d: 读不到影响行数: %w", cmd.ProductID, aerr)
	}
	if affected == 0 {
		return dtmx.BusinessFailure(deficit)
	}
	if _, err := dtmx.ExecTx(ctx, tx, dtmx.OpQuery,
		dtmLogSQL, r.traceOn,
		int64(cmd.ProductID), int64(delta), fmt.Sprintf(reasonFmt, cmd.OrderNo), int64(cmd.UserID)); err != nil {
		// 流水写失败必须连库存更新一起回滚：两者构成同一笔账，只落一半就是
		// 「扣了没流水」——对账时既查不出原因也无法补。
		return fmt.Errorf("insert dtm stock log %d: %w", cmd.ProductID, err)
	}
	return nil
}
