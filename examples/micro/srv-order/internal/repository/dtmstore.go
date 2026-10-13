// dtm 分支面的持久层：barrier 事务内的原始 SQL。
//
// 与 order.go 的 gorm 写路径是**两套**，不是两套写法：
//   - order.go 的事务由 gorm 自己开（db.Transaction），幂等由应用侧「先查后插」
//   - uk_order_no 唯一键提供；
//   - 本文件的写必须跑在 dtm 官方 barrier 开的事务里（dtmx.Barrier.Call 把
//     *sql.Tx 递进来），屏障行与业务写同事务提交。因此这里用 *sql.Tx 原始 SQL，
//     不用 gorm —— 官方回调签名（BarrierBusiFunc）本来就是 *sql.Tx，硬套 gorm
//     只会多一层 Statement.ConnPool 强转，屏障与业务写在哪个连接上反而更难确认。
//
// 四条纪律：
//  1. **判据在 UPDATE 的 WHERE 里，不靠读出来再算**。事务里先 SELECT 再判状态，
//     会多一个与并发 Confirm/Cancel 互相看见的窗口 —— 而「占位单归谁」这件事
//     只有 `order_no = ? and dtm_gid = ?` 这一条条件更新能原子地裁断。
//     （唯一允许的 SELECT 是回读刚落库的自增主键，用于事件体与状态日志，
//     它不参与任何判据，见 selectOrderID。）
//  2. **不允许的判据一律业务失败**。返回 dtmx.BusinessFailure（包官方
//     dtmcli.ErrFailure）：dtm 收到后立刻逆序补偿/回滚；返回普通 error 会被当
//     基础设施故障按退避重试，一次「订单状态不对」能把整条 Saga 拖成重试风暴。
//  3. **事件只在 Confirm 之后落**（口径②）。落到**方法形状**上而不是靠调用方
//     自觉：只有 DtmCreateOrder / DtmConfirmFreeze 收 OutboxFill，
//     DtmTryPlace / DtmCancelOrder / DtmCancelFreeze 的签名里根本没有它，
//     于是「占位单也发下单完成事件」这件事写不出来。
//  4. **状态与日志同事务推进**。本域既有纪律（见 order.go 的 UpdateStatusTx）
//     是「改了状态必须留 order_status_log」，否则报表里的 status_change_count
//     对 dtm 驱动的流转不可见。
package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/gospacex/dtmx"
	"github.com/gospacex/launchx/examples/micro/pkg/events"
	"github.com/gospacex/launchx/examples/micro/srv-order/internal/model"
	"github.com/gospacex/launchx/examples/pkg/outbox"
)

// ErrStatusNotAllowed 分支的定位 + 状态判据没命中（订单不存在、属于别的全局事务、
// 或当前状态不允许这次流转）。
//
// 单独一个哨兵而不是复用 ErrNotFound：一条条件更新天然分不开「这单没有」与
// 「这单不该在这个状态下动」—— WHERE 里两者是同一个 0 行。分开报码的诱惑很大，
// 但要分开就得先 SELECT 再判状态，而那正是本文件纪律①禁止的读改写。
//
// 处置上两者也不该分：都是「这笔账现在不能按这个分支动」，dtm 收到后都应立即
// 转补偿/回滚，而不是退避重试 —— 重试一万次结果一样。
var ErrStatusNotAllowed = errors.New("repository: order status does not allow this dtm branch")

// OutboxFill 在事务内、订单行落库后（自增 ID 已知）回调，产出事件体与随 header
// 透传的 traceparent。与 CreateOrderInput.OutboxFill 同签名、同语义：
// nil 时 payload 为空（Runner 直接放弃该行，不占重试配额）。
//
// 只有需要写 outbox 的两个分支收它（见纪律③）。
type OutboxFill func(orderID uint64) (payload, traceparent string)

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
// 列名逐字取自 examples/mall/scripts/schema.sql 的 orders / order_items /
// order_status_log / outbox 建表语句，加上 examples/micro/scripts/schema-micro.sql
// 的 dtm_gid 增列。写错列名在真机上是 unknown column，而那时账已经扣了一半。
//
// 状态流转的三条 UPDATE 只差 set 子句与「目标 from 状态」，故形如
// `update orders set status = ? where order_no = ? and dtm_gid = ? and status = ?`，
// 第三个参数是**允许流转出去的那个状态**（判据的一部分，不是过滤条件之外的东西）。
const (
	// 建单（Saga 正向 / TCC 一阶段共用）：status 参数决定是正式单还是占位单。
	// insert ignore 而非 insert：同一个 order_no 在第二个全局事务里再走一遍时，
	// 这里返回 0 行而不是抛唯一键冲突 —— 重放必须是「成功但什么都没做」。
	dtmInsertOrderSQL = "insert ignore into orders (user_id, order_no, status, total_amount, dtm_gid) values (?, ?, ?, ?, ?)"
	// 明细。order_id 取刚落库那行的 LastInsertId。
	dtmInsertItemSQL = "insert into order_items (order_id, product_id, product_name, quantity, unit_price) values (?, ?, ?, ?, ?)"
	// 状态流转日志（本域纪律④）。
	dtmInsertStatusLogSQL = "insert into order_status_log (order_id, from_status, to_status) values (?, ?, ?)"
	// 事件。列名取自 schema.sql 的 outbox（status=0 即 outbox.StatusPending，
	// Runner 只挑 status=0 的行）。
	dtmInsertOutboxSQL = "insert into outbox (topic, event_type, payload, traceparent, status, retry_count) values (?, ?, ?, ?, ?, ?)"
	// 回读自增主键。**唯一**允许的 SELECT，且不参与任何判据（纪律①的例外），
	// 它读的是本事务刚落库/刚改过的那一行 —— 自己的写在任何隔离级别下对自己
	// 都可见，故这里没有并发窗口。
	dtmSelectOrderIDSQL = "select id from orders where order_no = ? and dtm_gid = ?"
	// 状态流转：置为终态 canceled（正式单作废）。
	dtmCancelOrderSQL = "update orders set status = ? where order_no = ? and dtm_gid = ? and status = ?"
	// 状态流转：占位单转正式单，并把实收额对齐订单总额（确认即实收）。
	dtmConfirmFreezeSQL = "update orders set status = ?, paid_amount = total_amount where order_no = ? and dtm_gid = ? and status = ?"
)

// 事件口径（与 order.go 的 CreateOrderTx 逐字一致）：
// topic 取 events.TopicOrderCompleted（config.yaml 的 mqx.producer 就是它，
// 改一边必须改两边）；event_type 是 outbox 表里的类型标识。
const dtmOutboxEventType = "order.completed"

// DtmCreateOrder Saga 正向：正式单落库（orders + order_items + outbox 同事务）。
//
// 幂等按 uk_order_no：insert ignore 命中唯一键即 0 行 —— 那意味着同一个单号
// 已经建过（上一个全局事务建过，或本事务被重试过）。此时**返回成功且一行都不写**：
// 明细不重复插，事件不重复落（事件插重了下游会重复送积分，那比重复下单更难收敛）。
// 与 HTTP 面 CreateOrderTx 的 Idempotent 分支同义，只是那边要回既有订单的 id，
// 而分支回调的返回值只有 error，拿不到也不需要。
//
// 返回自增 order_id：service 靠它失效详情缓存（读侧可能缓存过「不存在的 id」）。
func (r *OrderRepository) DtmCreateOrder(
	ctx context.Context, tx *sql.Tx, cmd model.DtmOrderCmd, fill OutboxFill,
) (orderID uint64, err error) {
	ctx, end := r.begin(ctx, "DtmCreateOrder")
	defer func() { end(err) }()

	if err := requireTx(tx); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, dtmInsertOrderSQL,
		int64(cmd.UserID), cmd.OrderNo, model.StatusCreated, cmd.Total(), cmd.DtmGID)
	if err != nil {
		return 0, fmt.Errorf("dtm 建单 %s: %w", cmd.OrderNo, err)
	}
	affected, aerr := res.RowsAffected()
	if aerr != nil {
		return 0, fmt.Errorf("dtm 建单 %s: 读不到影响行数: %w", cmd.OrderNo, aerr)
	}
	if affected == 0 {
		// 幂等重放：单号已占用。**不是**失败 —— 报失败会让 dtm 把它当基础故障
		// 重试，而重试的结果还是这一行。
		return 0, nil
	}
	id, lerr := res.LastInsertId()
	if lerr != nil || id <= 0 {
		// 拿不到刚插入的自增 id 就无法写明细与事件：那不是「可以不写」，
		// 是一张查不出明细的单。整笔失败（barrier 行随之回滚，dtm 会重试）。
		return 0, fmt.Errorf("dtm 建单 %s: 读不到自增 order_id: %w", cmd.OrderNo, lerr)
	}
	if err := insertItems(ctx, tx, cmd, uint64(id)); err != nil {
		return 0, err
	}
	if err := insertOutbox(ctx, tx, cmd, uint64(id), fill); err != nil {
		return 0, err
	}
	return uint64(id), nil
}

// DtmCancelOrder Saga 补偿：把 CreateOrder 建的单置为终态 canceled。
//
// **不写 outbox**（口径②）：正向下单已经发过一次「下单完成」，作废时再发一条，
// 下游就收到两次；积分侧靠 uk_ledger_order_type 幂等掉一次，可账上就是一次
// 对不上的重复消费。故本方法的签名里连 OutboxFill 都没有。
//
// 落到本方法时正向必然到过（否则 dtm 的 barrier 已判空补偿、业务回调根本不会被
// 调用），所以 WHERE 的 0 行只可能是「定位不到」或「已经不是 created」。
func (r *OrderRepository) DtmCancelOrder(
	ctx context.Context, tx *sql.Tx, cmd model.DtmOrderCmd,
) (orderID uint64, err error) {
	ctx, end := r.begin(ctx, "DtmCancelOrder")
	defer func() { end(err) }()

	id, err := r.transition(ctx, tx, cmd, dtmCancelOrderSQL, model.StatusCreated, model.StatusCanceled)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// DtmTryPlace TCC 一阶段：建 presale_locked 占位单并写 dtm_gid。
//
// **不写 outbox**（口径②，纪律③）：单还没成，此刻发「下单完成」等于通知下游去
// 扣款/送积分，而那两笔随时会被 Cancel 回退 —— 下游收到的事件收不回去。
//
// 幂等同 DtmCreateOrder：单号已占用即 0 行，返回成功且一行不写。这条路径的判据
// 只能到「这个号已被占用」为止 —— insert ignore 不区分占用者是谁；要区分就得先
// SELECT 再判状态，而那会与并发的 Confirm/Cancel 互相看见窗口。
// 万一占用者不是占位单（例如同号的正式单已存在），下一个分支 ConfirmFreeze 的
// 条件更新（status = 'presale_locked'）会以 0 行**响亮失败**（业务失败 + codes），整条 TCC 随即回退 ——
// 漏判在这一处被下游挡住，而不是在这里被猜出来。
func (r *OrderRepository) DtmTryPlace(
	ctx context.Context, tx *sql.Tx, cmd model.DtmOrderCmd,
) (orderID uint64, err error) {
	ctx, end := r.begin(ctx, "DtmTryPlace")
	defer func() { end(err) }()

	if err := requireTx(tx); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, dtmInsertOrderSQL,
		int64(cmd.UserID), cmd.OrderNo, model.StatusPresaleLocked, cmd.Total(), cmd.DtmGID)
	if err != nil {
		return 0, fmt.Errorf("dtm 占位 %s: %w", cmd.OrderNo, err)
	}
	affected, aerr := res.RowsAffected()
	if aerr != nil {
		return 0, fmt.Errorf("dtm 占位 %s: 读不到影响行数: %w", cmd.OrderNo, aerr)
	}
	if affected == 0 {
		return 0, nil // 幂等重放：单号已占用
	}
	id, lerr := res.LastInsertId()
	if lerr != nil || id <= 0 {
		return 0, fmt.Errorf("dtm 占位 %s: 读不到自增 order_id: %w", cmd.OrderNo, lerr)
	}
	return uint64(id), nil
}

// DtmConfirmFreeze TCC 二阶段：占位单**原地**转正式 paid 单，此刻才写 outbox。
//
// 「原地」是口径③（三态不变式）在本域的落法：uk_order_no 保证一行订单，
// Confirm 是 UPDATE 而不是「再插一行 + 删占位」。后者在中间态（插了新行、还没删
// 占位）会让同一张单被算两次，且那两个窗口没有任何唯一键能挡。
//
// 回读 id 的用途只有两个：状态日志的 order_id 与事件体的 order_id —— 都不参与判据。
func (r *OrderRepository) DtmConfirmFreeze(
	ctx context.Context, tx *sql.Tx, cmd model.DtmOrderCmd, fill OutboxFill,
) (orderID uint64, err error) {
	ctx, end := r.begin(ctx, "DtmConfirmFreeze")
	defer func() { end(err) }()

	if err := requireTx(tx); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, dtmConfirmFreezeSQL,
		model.StatusPaid, cmd.OrderNo, cmd.DtmGID, model.StatusPresaleLocked)
	if err != nil {
		return 0, fmt.Errorf("dtm 确认冻结 %s: %w", cmd.OrderNo, err)
	}
	affected, aerr := res.RowsAffected()
	if aerr != nil {
		return 0, fmt.Errorf("dtm 确认冻结 %s: 读不到影响行数: %w", cmd.OrderNo, aerr)
	}
	if affected == 0 {
		// 占位单不存在 / 已被作废 / 属于别的全局事务。三种都不许发事件：
		// 发出去的事件会通知下游去扣款，而这张单根本不该成立。
		return 0, dtmx.BusinessFailure(fmt.Errorf("dtm 确认冻结 %s: %w", cmd.OrderNo, ErrStatusNotAllowed))
	}
	id, err := selectOrderID(ctx, tx, cmd)
	if err != nil {
		return 0, err
	}
	if err := insertStatusLog(ctx, tx, id, model.StatusPresaleLocked, model.StatusPaid); err != nil {
		return 0, err
	}
	if err := insertOutbox(ctx, tx, cmd, id, fill); err != nil {
		return 0, err
	}
	return id, nil
}

// DtmCancelFreeze TCC 二阶段：废掉占位单（置终态 canceled）。
//
// **不写 outbox**（口径②）：这单从来没成立过，没有任何下游该收到它的消息。
//
// 为什么置 canceled 而不是 DELETE：那行是「这笔全局事务走到过哪」的唯一凭证；
// 删掉之后「这张单号上曾经起过一次 TCC」这件事就再也答不上了，而排查悬挂
// 恰恰要靠它。置终态还带来一条免费守卫 —— 之后再来的 ConfirmFreeze 会因为
// `status = 'presale_locked'` 不成立而响亮失败。
func (r *OrderRepository) DtmCancelFreeze(
	ctx context.Context, tx *sql.Tx, cmd model.DtmOrderCmd,
) (orderID uint64, err error) {
	ctx, end := r.begin(ctx, "DtmCancelFreeze")
	defer func() { end(err) }()

	id, err := r.transition(ctx, tx, cmd, dtmCancelOrderSQL, model.StatusPresaleLocked, model.StatusCanceled)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// transition 三个状态流转分支的共同骨架：条件更新 → 0 行即业务失败 → 记状态流转日志。
//
// 「0 行即业务失败」这条不可放宽：把它当成功会有一条全局事务带着一张没被作废的
// 单报成功，而下游库存/积分已经被回补过了 —— 那是最难收敛的一种不一致。
func (r *OrderRepository) transition(
	ctx context.Context, tx *sql.Tx, cmd model.DtmOrderCmd,
	query, from, to string,
) (uint64, error) {
	if err := requireTx(tx); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, query, to, cmd.OrderNo, cmd.DtmGID, from)
	if err != nil {
		return 0, fmt.Errorf("dtm 状态流转 %s → %s: %w", from, to, err)
	}
	affected, aerr := res.RowsAffected()
	if aerr != nil {
		return 0, fmt.Errorf("dtm 状态流转 %s → %s: 读不到影响行数: %w", from, to, aerr)
	}
	if affected == 0 {
		return 0, dtmx.BusinessFailure(
			fmt.Errorf("dtm 状态流转 %s → %s（单 %s / gid %s）: %w", from, to, cmd.OrderNo, cmd.DtmGID, ErrStatusNotAllowed))
	}
	id, err := selectOrderID(ctx, tx, cmd)
	if err != nil {
		return 0, err
	}
	if err := insertStatusLog(ctx, tx, id, from, to); err != nil {
		return 0, err
	}
	return id, nil
}

// insertItems 明细插入（与建单同事务）。
func insertItems(ctx context.Context, tx *sql.Tx, cmd model.DtmOrderCmd, orderID uint64) error {
	for _, it := range cmd.Items {
		if _, err := tx.ExecContext(ctx, dtmInsertItemSQL,
			int64(orderID), int64(it.ProductID), it.Name, int64(it.Quantity), it.UnitPrice); err != nil {
			// 明细写失败必须连订单一起回滚：两者构成同一笔账，只落一半就是
			// 「有单没明细」—— 总额对得上、明细查不出来，对账时无法归因。
			return fmt.Errorf("dtm 插明细 %s/%d: %w", cmd.OrderNo, it.ProductID, err)
		}
	}
	return nil
}

// insertStatusLog 状态流转日志（纪律④）。
func insertStatusLog(ctx context.Context, tx *sql.Tx, orderID uint64, from, to string) error {
	if _, err := tx.ExecContext(ctx, dtmInsertStatusLogSQL, int64(orderID), from, to); err != nil {
		return fmt.Errorf("dtm 记状态流转 %d (%s → %s): %w", orderID, from, to, err)
	}
	return nil
}

// insertOutbox 事件落库（只有建单与确认冻结会走到这里，纪律③）。
//
// fill 为 nil 时写空 payload：outbox.Runner 见空 payload 直接放弃该行且不占重试
// 配额（见 examples/pkg/outbox），语义与 HTTP 面一致。
func insertOutbox(ctx context.Context, tx *sql.Tx, cmd model.DtmOrderCmd, orderID uint64, fill OutboxFill) error {
	var payload, traceparent string
	if fill != nil {
		payload, traceparent = fill(orderID)
	}
	if _, err := tx.ExecContext(ctx, dtmInsertOutboxSQL,
		events.TopicOrderCompleted, dtmOutboxEventType, payload, traceparent,
		outbox.StatusPending, 0); err != nil {
		return fmt.Errorf("dtm 落事件 %s: %w", cmd.OrderNo, err)
	}
	return nil
}

// selectOrderID 回读刚落库 / 刚改过那行的自增主键。
//
// 读不到按**基础设施故障**上抛而不是业务失败：它紧跟在一条刚刚命中（affected>0）
// 的 UPDATE 之后，读不到意味着「更新生效了却看不到它」—— 那不是判据不满足，
// 是账漂了，标成业务失败会让 dtm 不重试，把一笔已生效的写永远留在这个状态。
func selectOrderID(ctx context.Context, tx *sql.Tx, cmd model.DtmOrderCmd) (uint64, error) {
	var id uint64
	err := tx.QueryRowContext(ctx, dtmSelectOrderIDSQL, cmd.OrderNo, cmd.DtmGID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("dtm 回读 order_id %s: %w", cmd.OrderNo, err)
	}
	if id == 0 {
		return 0, fmt.Errorf("dtm 回读 order_id %s: 自增 id 为 0（不该发生）", cmd.OrderNo)
	}
	return id, nil
}

// requireTx 分支写必须跑在 barrier 开的事务里。
//
// 脱离事务意味着屏障行与业务写各提交各的：空补偿 / 重复回调 / 悬挂三条守卫
// 全部失效，而当场没有任何迹象 —— 所以这里直接拒绝，而不是"照样写下去"。
func requireTx(tx *sql.Tx) error {
	if tx == nil {
		return errors.New("repository: dtm 分支写必须在 barrier 事务内执行（nil *sql.Tx）")
	}
	return nil
}
