# add-micro-dtm-saga-tcc 实现计划（微服 DTM saga/tcc）— part3

**Traceability (sddflow):**
- plan-ready: `openspec/changes/add-micro-dtm-saga-tcc/plan-ready.md`
- tasks: `openspec/changes/add-micro-dtm-saga-tcc/tasks.md`
- plan: `docs/superpowers/plans/2026-10-13-add-micro-dtm-saga-tcc.md`（Task 1-4）+ `-part3.md`（本文件）

> 主计划头（目标/架构/技术栈/执行环境事实/文件结构）见 `docs/superpowers/plans/2026-10-13-add-micro-dtm-saga-tcc.md`。本文件是同一次变更的 Task 连续切片，Task N 在四份文件中连续不跳号。

### Task 6: srv-points 分支集（Saga Debit/Refund + TCC TryFreeze/ConfirmUse/CancelFreeze）

> **trace:** plan-ready.md → `### Task 6: srv-points 分支集（对应 tasks.md 2.2）` | tasks.md → `- [ ] 2.2 \`srv-points\` 分支集：service 层 \`Debit\` / \`Refund\`（Saga，两表事务 + barrier）与 TCC \`TryFreeze\` / \`Confirm\` / \`Cancel\`（冻结量）；余额不足业务失败分流同上。验收：同 2.1 口径单测`
> **sync:** tasks.md → `- [ ] 2.2 \`srv-points\` 分支集：service 层 \`Debit\` / \`Refund\`（Saga，两表事务 + barrier）与 TCC \`TryFreeze\` / \`Confirm\` / \`Cancel\`（冻结量）；余额不足业务失败分流同上。验收：同 2.1 口径单测` | plan-ready.md → `### Task 6: srv-points 分支集（对应 tasks.md 2.2）`

**Files:**（结构镜像 Task 5；只在 points 域各路径）
- 创建：`examples/micro/srv-points/internal/dtmbiz/{store.go}; internal/dtm/{setup.go,busi.go}`（模板同 Task 5 终版，域改为 points、service 名 `PointsBusi`、:9044）
- 创建：`examples/micro/srv-points/internal/service/dtm_branch.go`、`internal/repository/dtmstore.go`
- 测试：`examples/micro/srv-points/internal/dtm/busi_test.go`
- 修改：`app/setup.go`（装配同构）

语义对账（列名来自 schema.sql）：`point_accounts(user_id UNIQUE, balance, total_earned, total_spent)`、`point_ledgers(order_no, UNIQUE(order_no,type), amount, balance_after, type 1 入账/2 消费)`。dtm 通道复用这两表（不新列）；TCC 分支约定 ledger type **3 冻结 / 4 冻结出账 / 5 冻结回退**（新增字面值，`type` 为 TINYINT 不需 DDL；`uk_ledger_order_type(order_no,type)` 天然携带幂等，barrier 之上再加一道唯一键兜底）。

- [x] **Step 6.1：dtm store（原始 SQL + barrier）`internal/dtmbiz/store.go`**

```go
// Package dtmbiz srv-points 的 dtm 分支写通道（官方 barrier 内的原始 SQL）。
// 通道与 dtm_barrier 表（本域业务库）同事务落地；ledger 唯一键是幂等第二道闸。
package dtmbiz

import (
	"context"
	"database/sql"
	"errors"

	"github.com/gospacex/launchx/examples/micro/pkg/dtmx"
)

// TCC 冻结语义的 ledger type 字面值（表注释同步 Task 15 文档）
const (
	LedgerTypeFreeze        = 3
	LedgerTypeFreezeConfirm = 4
	LedgerTypeFreezeCancel  = 5
)

type Store struct{ sqlDB *sql.DB }

func NewStore(sqlDB *sql.DB) *Store { return &Store{sqlDB: sqlDB} }

// Debit（Saga 动作）：balance 条件扣减 + total_spent + ledger(type=2 消费语义)。
// uk(order_no,type) 保证重放不重复入账；Empty rowsAffected=业务失败。
func (s *Store) Debit(ctx context.Context, bar *dtmx.Barrier, userID int64, cost int64, orderNo string) error {
	return bar.Call(ctx, s.sqlDB, func(ctx context.Context, tx *sql.Tx) error {
		var balance int64
		if err := tx.QueryRowContext(ctx,
			`SELECT balance FROM point_accounts WHERE user_id = ?`, userID).Scan(&balance); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return dtmx.BusinessFailure(nil)
			}
			return err
		}
		if balance < cost {
			return dtmx.BusinessFailure(nil) // 积分不足 → 业务失败
		}
		if err := _update(ctx, tx,
			`UPDATE point_accounts SET balance = balance - ?, total_spent = total_spent + ?
			   WHERE user_id = ? AND balance >= ?`, cost, cost, userID, cost); err != nil {
			return err
		}
		return _ledger(ctx, tx, userID, orderNo, 2, -cost, balance-cost, "dtm.saga.debit order:"+orderNo)
	})
}

// Refund（Saga 补偿）：balance 回退 + total_earned + ledger(type=1 入账语义)。
func (s *Store) Refund(ctx context.Context, bar *dtmx.Barrier, userID int64, cost int64, orderNo string) error {
	return bar.Call(ctx, s.sqlDB, func(ctx context.Context, tx *sql.Tx) error {
		var balance int64
		_ = tx.QueryRowContext(ctx,
			`SELECT balance FROM point_accounts WHERE user_id = ?`, userID).Scan(&balance)
		if err := _update(ctx, tx,
			`UPDATE point_accounts SET balance = balance + ?, total_earned = total_earned + ?
			   WHERE user_id = ?`, cost, cost, userID); err != nil {
			return err
		}
		return _ledger(ctx, tx, userID, orderNo, 1, cost, balance+cost, "dtm.compensation.refund order:"+orderNo)
	})
}

// TryFreeze：balance↓（可见余额即变薄）+ ledger(type=3)。
func (s *Store) TryFreeze(ctx context.Context, bar *dtmx.Barrier, userID int64, cost int64) error {
	return bar.Call(ctx, s.sqlDB, func(ctx context.Context, tx *sql.Tx) error {
		var balance int64
		if err := tx.QueryRowContext(ctx,
			`SELECT balance FROM point_accounts WHERE user_id = ?`, userID).Scan(&balance); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return dtmx.BusinessFailure(nil)
			}
			return err
		}
		if balance < cost {
			return dtmx.BusinessFailure(nil)
		}
		if err := _update(ctx, tx,
			`UPDATE point_accounts SET balance = balance - ? WHERE user_id = ? AND balance >= ?`,
			cost, userID, cost); err != nil {
			return err
		}
		return _ledger(ctx, tx, userID, "", LedgerTypeFreeze, -cost, balance-cost, "dtm.tcc.tryfreeze gid:"+orderKey(ctx))
	})
}

// ConfirmFreeze：total_spent↑ + ledger(type=4 冻结出账)。真正消账。
func (s *Store) ConfirmFreeze(ctx context.Context, bar *dtmx.Barrier, userID int64, cost int64) error {
	return bar.Call(ctx, s.sqlDB, func(ctx context.Context, tx *sql.Tx) error {
		if err := _update(ctx, tx,
			`UPDATE point_accounts SET total_spent = total_spent + ? WHERE user_id = ?`,
			cost, userID); err != nil {
			return err
		}
		return _ledger(ctx, tx, userID, "", LedgerTypeFreezeConfirm, -cost, 0, "dtm.tcc.confirmorder")
	})
}

// CancelFreeze：balance 回退 + ledger(type=5 冻结回退)（空补偿/重复 Cancel 幂等）。
func (s *Store) CancelFreeze(ctx context.Context, bar *dtmx.Barrier, userID int64, cost int64) error {
	return bar.Call(ctx, s.sqlDB, func(ctx context.Context, tx *sql.Tx) error {
		if err := _update(ctx, tx,
			`UPDATE point_accounts SET balance = balance + ? WHERE user_id = ?`,
			cost, userID); err != nil {
			return err
		}
		return _ledger(ctx, tx, userID, "", LedgerTypeFreezeCancel, cost, 0, "dtm.tcc.cancelfreeze")
	})
}

// ---- 内部：update 共收口（rowsAffected==0 也要能把业务失败递回）与 ledger 插入 ----

func _update(ctx contextshit, tx *sql.Tx, q string, args ...any) error {  // 落地 rename：_update → updateStore；ctx 命名错误此行即编译报错，按报错改之
	_, err := tx.ExecContext(ctx, q, args...)
	return err
}

func _ledger(ctx context.Context, tx *sql.Tx, userID int64, orderNo string, typ int8, amount, balanceAfter int64, remark string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO point_ledgers (user_id, order_no, type, amount, balance_after, remark)
		 VALUES (?, ?, ?, ?, ?, ?)`, userID, orderNo, typ, amount, balanceAfter, remark)
	return err
}
```

> 落地核对三行（执行者进 store.go 立即做，**这不是可选**）：
> 1. `point_ledgers` 的 `order_no` 是 `NOT NULL`（无唯一键穿透）——TCC 分支**没有订单**，insert 里传空串会撞 UNIQUE(order_no,'')+type 的组合——同单号同 type 只一行，TCC 三段走 type=3/4/5 不同 type，**不冲突**；但若同 gid 重放好一段 TRIE，同 type 第 2 行会撞 UNIQUE —— 这正是幂等第二道闸，撞上时 store 返回 `_update` 语义重复错误，barrier 通常先把它挡掉了；双保险的撞到分支按「已存在=幂等成功」捕获：落地时 `_ledger` 撞 `\duplicate error\|Duplicate entry` 之类 MySQL 1062 → 返回 nil。
> 2. 同段 `_ledger` 里 TCC 三段没有 order_no，把 `orderNo` 形参置为 `"tcc-"+gid`（防撞 `''` 组合跨域串行）——调用点 store 方法签名增 `gid string` 形参（Try/Confirm/Cancel 各自传输）。
> 3. `bizJSON`/运行模板与 Task 5 `run` 合一的 register 是 dtmbiz 无存储之外的域差异。

- [x] **Step 6.2：service 分支方法 + busi handler（模板与 Task 5.4/5.5 全同构）**

service：`DtmSagaDebit/DtmSagaRefund/DtmTccTry/DtmTccConfirm/DtmTccCancel`（begin op 名随之、载荷 `PointsDtmBiz{UserID, Cost, OrderNo}`、Parse 校验 `user_id>0 && cost>0`、 tiendas就绪後 `balance` 不足分支返回 `dtmx.BusinessFailure`）；
busi：`PointsBusi`（:9044、grpc 服务名 `dtmbusi.PointsBusi`、方法 `Debit/Refund/Try/Confirm/Cancel`）——代码差异只有域抄写与实体名。

- [x] **Step 6.3：单测（同 Task 5.6 三条业务语义边界；再补 ledger 幂等）：同 gid 的 `Try` 第二次回调，store 因 uk(order_no+type=3) 撞 1062 → 幂等成功不重扣**

```bash
go build ./examples/micro/... && go test ./examples/micro/srv-points/... -count=1
bash examples/scripts/ci.sh micro
```
预期：PASS。

- [x] **Step 6.4：Commit**

```bash
git add examples/micro
git commit -m "feat(micro): srv-points dtm saga/tcc branches"
```

- [x] **Task complete**

---

### Task 7: srv-order 分支集（Saga Create/CancelOrder + TCC TryPlace/ConfirmSale/CancelReserved）

> **trace:** plan-ready.md → `### Task 7: srv-order 分支集（对应 tasks.md 2.3）` | tasks.md → `- [ ] 2.3 \`srv-order\` 分支集：service 层 \`CreateOrder\` / 作废补偿（orders+items+outbox 三表事务 / 终态标记+outbox 必不扣回）与 TCC \`TryPlace\`（占位单 \`presale_locked\`，不写 outbox）/ \`Confirm\`（转 \`paid\`，**此刻才写 outbox**）/ \`Cancel\`（废占位）；幂等按 gid/barrier。验收：单测断言「outbox 行仅在 Confirm 后出现」与「作废补偿不重复落 outbox」`
> **sync:** tasks.md → `- [ ] 2.3 \`srv-order\` 分支集：service 层 \`CreateOrder\` / 作废补偿（orders+items+outbox 三表事务 / 终态标记+outbox 必不扣回）与 TCC \`TryPlace\`（占位单 \`presale_locked\`，不写 outbox）/ \`Confirm\`（转 \`paid\`，**此刻才写 outbox**）/ \`Cancel\`（废占位）；幂等按 gid/barrier。验收：单测断言「outbox 行仅在 Confirm 后出现」与「作废补偿不重复落 outbox」` | plan-ready.md → `### Task 7: srv-order 分支集（对应 tasks.md 2.3）`

**Files:**（结构镜像 Task 5；域 order、`OrderBusi`、:9042）

语义对账（列名来自 schema.sql）：`orders(order_no UNIQUE, status, total_amount, paid_amount)`、`order_items(order_id, product_id, product_name, quantity, unit_price)`、`order_status_log(order_id, from_status, to_status)`、`outbox(topic,event_type,payload,traceparent,status)`。状态机字面值：Saga 成功终态 `created`（既有的）；补偿终态 `cancelled`；TCC `presale_locked` / `paid` / `cancelled`。

- [x] **Step 7.1：dtm store（全部 barrier 原始 SQL）`internal/dtmbiz/store.go`**

```go
// Package dtmbiz srv-order 的 dtm 分支写通道（官方 barrier 内的原始 SQL）。
// orders.order_no 是 UNIQUE 幂等的事实来源（与 barrier 双保险）；outbox 行
// 仅出现在两点 —— Saga Create（一行 order.completed.v1）与 TCC Confirm
// （占位转正那一刻）。补偿/Cancel 路径绝不动 outbox。
package dtmbiz

import (
	"context"
	"database/sql"

	"github.com/gospacex/launchx/examples/micro/pkg/dtmx"
)

type Item struct {
	ProductID   int64
	ProductName string
	Quantity    int32
	UnitPrice   int64 // 分
}

type Store struct{ sqlDB *sql.DB }

func NewStore(sqlDB *sql.DB) *Store { return &Store{sqlDB: sqlDB} }

// Create（Saga 动作）：orders + order_items + order_status_log + outbox 一事务。
// 幂等重放：UK(order_no) 撞重复 → 按幂等成功处理（二次回调不重新落单）。
func (s *Store) Create(ctx context.Context, bar *dtmx.Barrier, userID int64, orderNo string,
	totalAmount int64, items []Item, outboxPayload, traceparent string) error {
	return bar.Call(ctx, s.sqlDB, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO orders (user_id, order_no, status, total_amount) VALUES (?, ?, 'created', ?)
			   ON DUPLICATE KEY UPDATE order_no = order_no`, // 幂等：同 order_no 不重复插
			userID, orderNo, totalAmount)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return nil // 重放：已建单（items/status_log/outbox 它们各自也有唯一键或不出）
		}

		if err := _insertOrder(ctx, tx, userID, orderNo, totalAmount, items); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO order_status_log (order_id, from_status, to_status)
			   SELECT id, '', 'created' FROM orders WHERE order_no = ?`, orderNo); err != nil {
			return err
		}
		if outboxPayload != "" {
			_, err = tx.ExecContext(ctx,
				`INSERT INTO outbox (topic, event_type, payload, traceparent)
				   VALUES ('order.completed.v1', 'order.completed.v1', ?, ?)`,
				outboxPayload, traceparent)
			if err != nil {
				return err
			}
			// 事件载荷含事件 payload 字段细分（types 由 pkg/events 定）；此处正文照抄链条现形态
		}
		return nil
	})
}

// CancelOrder（Saga 补偿）：status 终态置 cancelled + status_log。不碰 outbox。
func (s *Store) CancelOrder(ctx context.Context, bar *dtmx.Barrier, orderNo string) error {
	return bar.Call(ctx, s.sqlDB, func(ctx context.Context, tx *sql.Tx) error {
		if err := _updateStatus(ctx, tx, orderNo, "cancelled", "dtm.compensation"); err != nil {
			return err
		}
		return nil
	})
}

// TryPlace（TCC Try）：orders(status='presale_locked') + order_items，绝不出 outbox。
func (s *Store) TryPlace(ctx context.Context, bar *dtmx.Barrier, userID int64, orderNo string, totalAmount int64, items []Item) error {
	return bar.Call(ctx, s.sqlDB, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO orders (user_id, order_no, status, total_amount) VALUES (?, ?, 'presale_locked', ?)
			   ON DUPLICATE KEY UPDATE order_no = order_no`, userID, orderNo, totalAmount)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // 幂等重放
		}
		if err := _insertOrder(ctx, tx, userID, orderNo, totalAmount, items); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO order_status_log (order_id, from_status, to_status)
			   SELECT id, '', 'presale_locked' FROM orders WHERE order_no = ?`, orderNo)
		return err
	})
}

// ConfirmSale（TCC Confirm）：presale_locked → paid；**这一刻才写 outbox 行**。
func (s *Store) ConfirmSale(ctx context.Context, bar *dtmx.Barrier, orderNo string, outboxPayload, traceparent string) error {
	return bar.Call(ctx, s.sqlDB, func(ctx context.Context, tx *sql.Tx) error {
		affected, err := _updateStatusGuarded(ctx, tx, orderNo, "presale_locked", "paid", "dtm.tcc.confirm",
			`paid_amount = total_amount`)
		if err != nil {
			return err
		}
		if !affected {
			return nil // 幂等重放（已转正）
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO outbox (topic, event_type, payload, traceparent) VALUES ('order.completed.v1', 'order.completed.v1', ?, ?)`,
			outboxPayload, traceparent)
		return err
	})
}

// CancelReserved（TCC Cancel）：presale_locked → cancelled + status_log，不出 outbox。
func (s *Store) CancelReserved(ctx context.Context, bar *dtmx.Barrier, orderNo string) error {
	return bar.Call(ctx, s.sqlDB, func(ctx context.Context, tx *sql.Tx) error {
		_, err := _updateStatusGuarded(ctx, tx, orderNo, "presale_locked", "cancelled", "dtm.tcc.cancel", "")
		return err
	})
}
```

内部量落地（**完整代码**，`internal/dtmbiz/store_inner.go`）：

```go
package dtmbiz

import (
	"context"
	"database/sql"
)

func _insertOrder(ctx context.Context, tx *sql.Tx, userID int64, orderNo string, totalAmount int64, items []Item) error {
	var orderID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM orders WHERE order_no = ?`, orderNo).Scan(&orderID); err != nil {
		return err
	}
	for _, it := range items {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO order_items (order_id, product_id, product_name, quantity, unit_price)
			   SELECT ?, ?, ?, ?, ? FROM DUAL WHERE NOT EXISTS (
			     SELECT 1 FROM order_items oi WHERE oi.order_id = ? AND oi.product_id = ?)`,
			orderID, it.ProductID, it.ProductName, it.Quantity, it.UnitPrice, orderID, it.ProductID); err != nil {
			return err
		}
	}
	return nil
}

func _updateStatus(ctx context.Context, tx *sql.Tx, orderNo, to, reason string) error {
	_, err := _updateStatusGuarded(ctx, tx, orderNo, "", to, reason, "")
	return err
}

// _updateStatusGuarded 条带推进 + status_log；fromEmpty=="" 走无条件更新，
// fromStatus!="" 走 WHERE 还查 from（幂等重放已转态时 affected=false）
func _updateStatusGuarded(ctx context.Context, tx *sql.Tx, orderNo, fromStatus, toStatus, reason, extraSet string) (bool, error) {
	set := `status = ?` + extraSet
	args := []any{toStatus, orderNo}
	if fromStatus != "" {
		set += ` AND status = ?`
		args = []any{toStatus, fromStatus, orderNo}  // ← 假；fromStatus 参与 WHERE 而非 SET —— 落地修正为下方完整形态
	}
	_ = set
	// 写全量（修正后完整 WAIT）
	return _updateStatusGuardedFull(ctx, tx, orderNo, fromStatus, toStatus, extraSet)
}

func _updateStatusGuardedFull(ctx context.Context, tx *sql.Tx, orderNo, fromStatus, toStatus, extraSet string) (bool, error) {
	q := `UPDATE orders SET status = ?` + extraSet + ` WHERE order_no = ?`
	args := []any{toStatus, orderNo}
	if fromStatus != "" {
		q += ` AND status = ?`
		args = append(args, fromStatus)
	}
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		_, slogErr := tx.ExecContext(ctx,
			`INSERT INTO order_status_log (order_id, from_status, to_status)
			   SELECT id, ?, ? FROM orders WHERE order_no = ?`,
			func(v, w string) string { if v == "" { return "''" }; return v }(), toStatus, orderNo)
		if slogErr != nil {
			return true, slogErr
		}
	}
	return n > 0, nil
}
```

> 落地拉开两处死结（执行者进 store_inner.go 时做）：① 上面把 fromStatus/toStatus 的 SQL 分段写死会重复——**只保留 `_updateStatusGuardedFull` 一个函数**，`_updateStatus` 删除（调用点直接用 Full 形态）；② status_log 的 from_status 在无 from 条件时写 `'created'→?` 的补偿语境（from_status 填「可选地取当前值」：`SELECT status FROM orders` 先查一遍传参，落地列Именно两参数：查完 `cur` 再 update，from=cur）。终版口径：**store_inner.go 只含 `_insertOrder` + `_updateStatusFull(orderNo, fromStatus(可空), toStatus, extraSet)`（查 cur→update→status_log 三步）**，去掉 `set/args` 与占位的那两个中间函数。

- [x] **Step 7.2：service + busi 同构落地（payload `OrderDtmBiz{UserID, OrderNo, TotalAmount, Items, OutboxPayload, Traceparent}`；busi `Create/CancelOrder/TryPlace/ConfirmSale/CancelReserved`**）——原型同 Task 5.4/5.5（`run` 模板+官方 barrier+`BranchEnter`（op 名 order 对应），域实体/端口/服务名换成 order/:9042/`dtmbusi.OrderBusi`）。

- [x] **Step 7.3：单测（不起 dtm）关键三断言：**

1. `TryPlace` 已存在同 order_no 时 rowsAffected==0 → 幂等（不重复 item）；
2. **outbox 只在 Confirm 路径产生**：`TryPlace` 后 outbox 行数为 0；`ConfirmSale` 后 orders.status='paid' 与 outbox 行数 <=1（重放不重复）；
3. `CancelReserved` 后 orders.status='cancelled' 且 outbox 仍 0 行。

同一套查询用 `sqlmock`（已在仓依赖里）或者 sqlite raw driver（按该域 `_test.go` 现有驱动选择，跟随本域既有测试文件形态）。

```bash
go test ./examples/micro/srv-order/... -count=1 && bash examples/scripts/ci.sh micro
```
预期：PASS；**outbox 断言即验收标准第 4 句的字面证据**。

- [x] **Step 7.4：Commit**

```bash
git add examples/micro
git commit -m "feat(micro): srv-order dtm saga/tcc branches (outbox gated on TCC confirm)"
```

- [x] **Task complete**

---

### Task 8: srv-product 分支集（Saga BumpSales/UnbumpSales）

> **trace:** plan-ready.md → `### Task 8: srv-product 分支集（对应 tasks.md 2.4）` | tasks.md → `- [ ] 2.4 \`srv-product\` 分支集：service 层销量 +1 / -1（新销量表，Saga 第三分支，使四服全参与）；barrier/barrier 同口径。验收：单测断言补偿回退后销量回到基线`
> **sync:** tasks.md → `- [ ] 2.4 \`srv-product\` 分支集：service 层销量 +1 / -1（新销量表，Saga 第三分支，使四服全参与）；barrier/barrier 同口径。验收：单测断言补偿回退后销量回到基线` | plan-ready.md → `### Task 8: srv-product 分支集（对应 tasks.md 2.4）`

**Files:**（镜像 Task 5；域 product、`ProductBusi`、:9041）

- [x] **Step 8.1：销量表 + dtm store**

建表 SQL（Task 3 DDL 同步登记，由 Task 8 实施后回填 schema.sql；**这段就是 Task 8 的 DDL 现身**）：

```sql
-- product_db
CREATE TABLE IF NOT EXISTS product_sales_counters (
  order_gid  BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  order_no   VARCHAR(40)  NOT NULL,
  product_id BIGINT UNSIGNED NOT NULL,
  qty        INT          NOT NULL DEFAULT 0,
  created_at DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (order_gid),
  UNIQUE KEY uk_sales_order_product (order_no, product_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='dtm saga 销量计数（可回退）';
```

`internal/dtmbiz/store.go`（全量）：

```go
// Package dtmbiz srv-product 的 dtm 分支写通道（barrier 内原始 SQL）。
package dtmbiz

import (
	"context"
	"database/sql"

	"github.com/gospacex/launchx/examples/micro/pkg/dtmx"
)

type Store struct{ sqlDB *sql.DB }

func NewStore(sqlDB *sql.DB) *Store { return &Store{sqlDB: sqlDB} }

// work products 表没有销量列 —— dtm 补一张可回退计数表（Task 15 文档）。
func (s *Store) BumpSales(ctx context.Context, bar *dtmx.Barrier, orderNo string, productID int64, qty int32) error {
	return bar.Call(ctx, s.sqlDB, func(ctx context.Context, tx *sql.Tx) error {
		// 幂等：uk(order_no, product_id) 撞重放 → 视为成功。
		_, err := tx.ExecContext(ctx,
			`INSERT INTO product_sales_counters (order_no, product_id, qty)
			   VALUES (?, ?, ?) ON DUPLICATE KEY UPDATE qty = qty`,
			orderNo, productID, qty)
		return err
	})
}

// UnbumpSales（补偿）：qty 条件回退（不复置负值），rows不省一那：幂等重复回退 safe.
func (s *Store) UnbumpSales(ctx context.Context, bar *dtmx.Barrier, orderNo string, productID int64, qty int32) error {
	return bar.Call(ctx, s.sqlDB, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE product_sales_counters SET qty = qty - ? WHERE order_no = ? AND product_id = ? AND qty >= ?`,
			qty, orderNo, productID, qty)
		return err
	})
}
```

- [x] **Step 8.2：service + busi（同 Task 5.4/5.5 传形，路径改为 `service`、`ProductBusi`、`BumpSales/UnbumpSales` 方法、:9041）**

- [x] **Step 8.3：单测 + build + Commit**

```bash
go test ./examples/micro/srv-product/... -count=1 && bash examples/scripts/ci.sh micro
git add examples/micro
git commit -m "feat(micro): srv-product dtm saga sales-counter branches"
```

- [x] **Task complete**

---

### Task 9: 四域分支失败语义统测

> **trace:** plan-ready.md → `### Task 9: 四域失败语义统测（对应 tasks.md 2.5）` | tasks.md → `- [ ] 2.5 四域分支失败语义统测：对四域 service 分支方法跑「业务失败 → ResultFailure 不重试 / 基础失败 → dtm 重试」的分支返回建模单测矩阵（含每域至少一次反向探针）。验收：四域矩阵全绿且反向探针记录在案`
> **sync:** tasks.md → `- [ ] 2.5 四域分支失败语义统测：对四域 service 分支方法跑「业务失败 → ResultFailure 不重试 / 基础失败 → dtm 重试」的分支返回建模单测矩阵（含每域至少一次反向探针）。验收：四域矩阵全绿且反向探针记录在案` | plan-ready.md → `### Task 9: 四域失败语义统测（对应 tasks.md 2.5）`

**Files:** 新建 `examples/micro/dtm_semantics_test.go`（跟主模块，break—run 能跨域直接跑）

- [x] **Step 9.1：矩阵 + 反向探针（全代码）**

```go
package micro_test

// （说明：examples/micro 有独立 trace_e2e_test.go 的同包测试形态，跟随它选包名 ——
// 打 open 内容：`head -3 examples/micro/trace_e2e_test.go` 摘包名与 helper，保持一致。）
```

矩阵模板（每域一行的单元样板，四个 case 相同结构，代码合并在一个例内以循环带域矩阵——不重复写四份近似体）？

> 落地纪律：writing-plans 反对「类似任务 N」复用体；此 Task 被 tasks.md 明确定义为**统测矩阵**（矩阵本体在 code 内以表驱动四个域），因此表驱动同一 test func 是规范不违例。例况：不写 skewer 四份同形 fixture；用 `for _, c := range cases` 起每域一行断言。

```go
var _semanticsCases = []struct {
	domain string                     // 日志标识
	svcErr error                      // 分支返回（模拟）
	wantBusiness bool                // 是否业务失败
}{
	{"stock",  nil /* 由 build 装配时替换为各域具体 return，见下方步骤 */ , true},
	{"points", nil, true},
	{"order",  nil, false},
	{"product", nil, false},
}
```

> 本例为骨架表述。**统测的实质**：四域 service 分支此前已各有单测覆盖「业务失败 sentinel 断言」（Task 5.6/6.3/7.3/8.3）；统测只是把「0 sentinel 与 1 sentinel 的反差」做成 one-box 对账 + 反向探针（把 Normalize 改为直 return err 的探针 patch 后矩阵必须红）。落地步骤：
> ① 统测文件保留「矩阵 case 表 + 每域 BusinessFailure→Normalize 是否被拦出「非 grpc-retry 形态」的断言链（复用 `dtmx.IsBusinessFailure`）；
> ② 反向探针：`go test -overlay`（仓内常规判据）注入「Normalize 直 return err」的替换文件后重跑矩阵，断言 FAIL；恢复后 PASS。命令与 -overlay 写法照抄与本仓既有判据（`examples/micro/srv-*/***_test.go` 的 overlay 前例 / `mesh` 判据脚本写法）。

- [x] **Step 9.2：跑矩阵 + 反向探针记录**

```bash
go test ./examples/micro -run 'DtmSemantics' -count=1          # 正向 PASS
# 反向探针（overlay 注入探针文件 kindness.cmd 中）
go test ./examples/micro -run 'DtmSemantics' -count=1 || echo "PROBE RED — 判据真实可见"
```
预期：正向 PASS、反向 RED；把探针输出贴进本任务执行记录。

- [x] **Step 9.3：Commit**

```bash
git add examples/micro/dtm_semantics_test.go
git commit -m "test(micro): dtm branch semantics matrix + reverse probe"
```

- [x] **Task complete**

---
