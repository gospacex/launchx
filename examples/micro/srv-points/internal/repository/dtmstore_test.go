package repository

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/dtm-labs/client/dtmcli"
	"google.golang.org/grpc/metadata"

	"github.com/gospacex/dtmx"
	"github.com/gospacex/launchx/examples/micro/srv-points/internal/dtmstub"
	"github.com/gospacex/launchx/examples/micro/srv-points/internal/model"
)

// TestMain 把 dtm 的 barrier 表名设成本域的 "barrier"。
//
// 这是进程级全局量（dtmcli.SetBarrierTableName → dtmimp.BarrierTableName），
// 不设就是官方默认的 `dtm_barrier.barrier`：barrier 行会插进一个本域根本不
// 存在的库，事务当场回滚，分支永远失败 —— 而 dtm 侧看到的只是「分支一直
// 失败」，排查成本极高。生产路径上这一步在 internal/dtm 的装配里（app 启动
// 时调一次），由 TestSetupSetsBarrierTableName 单独守住。
func TestMain(m *testing.M) {
	dtmx.SetBarrierTableName("barrier")
	os.Exit(m.Run())
}

// 本文件覆盖 dtm 分支写通道的三个语义边界（每个写方法各一遍）：
//   1. 正常写成功（余额/冻结按三态不变式推进，流水落库）；
//   2. 余额/冻结不足 → 业务失败（带官方哨兵，dtm 立即逆序补偿而非无限重试）；
//   3. barrier 幂等重放（第二次回调不重复记账）。
//
// 夹具是真事务（dtmstub 的 Begin 是整库快照），所以「业务失败时同事务里那条
// 流水也要一起回滚」是断言出来的，不是假设出来的。

// dtmCtx 造一条 dtm server 回调的上下文：dtmgimp.TransBaseFromGrpc 从
// incoming metadata 里读 dtm- 前缀的四个键（gid / trans_type / branch_id /
// op），少一个 dtmx.BarrierFromCtx 就取不到 barrier。
func dtmCtx(t *testing.T, gid, branchID, op string) context.Context {
	t.Helper()
	return metadata.NewIncomingContext(t.Context(), metadata.Pairs(
		"dtm-gid", gid,
		"dtm-trans_type", "saga",
		"dtm-branch_id", branchID,
		"dtm-op", op,
	))
}

func newStore(t *testing.T, balance int64) (*DtmBranchStore, *dtmstub.DB) {
	t.Helper()
	mem := dtmstub.New()
	mem.Seed(1001, balance)
	gdb, err := mem.GormDB()
	if err != nil {
		t.Fatalf("开夹具库失败：%v", err)
	}
	st, err := NewDtmBranchStore(gdb, true)
	if err != nil {
		t.Fatalf("构造分支写通道失败：%v", err)
	}
	return st, mem
}

func barrierOf(t *testing.T, ctx context.Context) *dtmx.Barrier {
	t.Helper()
	b, err := dtmx.BarrierFromCtx(ctx)
	if err != nil {
		t.Fatalf("取 barrier 失败：%v", err)
	}
	return b
}

func biz(userID, cost int64, orderNo string) model.DtmBiz {
	return model.DtmBiz{UserID: userID, Cost: cost, OrderNo: orderNo}
}

// ---- RawDB ----

// TestRawDBRejectsNilHandle 钉住「nil 入参报错」。
//
// 反向探针：把这里的 nil 判据去掉（直接 gdb.DB()），本用例必红 —— 而生产上
// 它会变成 nil 解引用 panic，装配期问题被推迟到第一次分支回调才炸。
func TestRawDBRejectsNilHandle(t *testing.T) {
	if db, err := RawDB(nil); err == nil {
		t.Fatalf("RawDB(nil) 必须报错，实际返回了 %v —— 装配期的句柄缺失会被推迟成运行期 panic", db)
	}
}

// ---- Debit（Saga 动作段）----

// TestDebitMovesBalanceAndWritesLedger：扣减余额 + 累计消费 + 一条 type=2 流水。
func TestDebitMovesBalanceAndWritesLedger(t *testing.T) {
	st, mem := newStore(t, 100)
	ctx := dtmCtx(t, "gid-debit-ok", "01", "action")

	if err := st.Debit(ctx, barrierOf(t, ctx), biz(1001, 30, "SO-1")); err != nil {
		t.Fatalf("Debit: %v", err)
	}
	acc := mem.Account(1001)
	if !acc.Found || acc.Balance != 70 || acc.TotalSpent != 30 {
		t.Fatalf("Debit 后账户应为 balance=70 total_spent=30，实际 %+v", acc)
	}
	ledgers := mem.Ledgers()
	if len(ledgers) != 1 {
		t.Fatalf("Debit 应落 1 条流水，实际 %d 条：%+v", len(ledgers), ledgers)
	}
	lg := ledgers[0]
	// order_no 用**原始订单号**：流水页与对账都是按订单号查的，编上任何前缀
	// 都会让「这笔消费属于哪个订单」在账上答不出来。
	if lg.OrderNo != "SO-1" || lg.Type != int64(model.TypeConsume) || lg.Amount != 30 || lg.BalanceAfter != 70 {
		t.Fatalf("Debit 流水应为 (SO-1, type=消费, 30, balance_after=70)，实际 %+v", lg)
	}
}

// TestDebitInsufficientIsBusinessFailure：余额不足 → 业务失败（官方哨兵），
// 且账户与流水都不许留下半截。
//
// 这是本文件最要紧的判据之一：哨兵若换成自造 error，dtm 会把它当基础失败
// 无限重试 —— 积分不足这种「重试一万次也不会有钱」的场景会被无限放大。
func TestDebitInsufficientIsBusinessFailure(t *testing.T) {
	st, mem := newStore(t, 10)
	ctx := dtmCtx(t, "gid-debit-short", "01", "action")

	err := st.Debit(ctx, barrierOf(t, ctx), biz(1001, 30, "SO-2"))
	if err == nil {
		t.Fatal("余额不足必须返回错误，实际 nil")
	}
	if !errors.Is(err, dtmcli.ErrFailure) {
		t.Fatalf("余额不足必须带官方哨兵 dtmcli.ErrFailure（dtm 据此判定不重试），实际 err=%v", err)
	}
	if !errors.Is(err, ErrDtmInsufficient) {
		t.Fatalf("错误须可判为 ErrDtmInsufficient（供 service 翻成 5002），实际 err=%v", err)
	}
	if acc := mem.Account(1001); acc.Balance != 10 || acc.TotalSpent != 0 {
		t.Fatalf("失败后账户不许有半截改动，实际 %+v", acc)
	}
	if got := mem.Ledgers(); len(got) != 0 {
		t.Fatalf("失败后同事务里的流水必须一起回滚，实际留下 %+v", got)
	}
	// barrier 行同样不该留下：barrier 行与业务写在同一事务，业务失败即回滚，
	// 否则下次重试会被 barrier 判成「已落过账」而静默成功 —— 积分不足会被
	// 误判成已扣款。
	if got := mem.Barriers(); got != 0 {
		t.Fatalf("业务失败时 barrier 行应随事务回滚，实际留下 %d 行", got)
	}
}

// TestDebitAccountNotFoundIsBusinessFailure：账户不存在与余额不足必须可分辨
// （5001 vs 5002），否则调用方会把「没开户」报成「没钱」。
func TestDebitAccountNotFoundIsBusinessFailure(t *testing.T) {
	st, mem := newStore(t, 100)
	ctx := dtmCtx(t, "gid-debit-nf", "01", "action")

	err := st.Debit(ctx, barrierOf(t, ctx), biz(2002, 30, "SO-3"))
	if !errors.Is(err, ErrDtmAccountNF) {
		t.Fatalf("账户不存在须判为 ErrDtmAccountNF，实际 err=%v", err)
	}
	if errors.Is(err, ErrDtmInsufficient) {
		t.Fatal("账户不存在不得被报成余额不足：两者业务语义不同，混了会让调用方补钱而不是开户")
	}
	if !errors.Is(err, dtmcli.ErrFailure) {
		t.Fatalf("账户不存在同样是业务失败，须带官方哨兵，实际 err=%v", err)
	}
	if got := mem.Ledgers(); len(got) != 0 {
		t.Fatalf("账户不存在时不该落流水，实际 %+v", got)
	}
}

// TestDebitBarrierReplayIsIdempotent：同 gid 同分支重复回调只扣一次。
//
// 这是 barrier 的核心契约（dtm 侧靠 (gid, branch_id, op, barrier_id) 唯一键
// 裁决）。若这条红了，多半是有人把业务写挪出了 barrier 事务 —— 表现是用户
// 被重复扣积分，且日志里两次调用都「成功」。
func TestDebitBarrierReplayIsIdempotent(t *testing.T) {
	st, mem := newStore(t, 100)
	ctx := dtmCtx(t, "gid-debit-replay", "01", "action")

	if err := st.Debit(ctx, barrierOf(t, ctx), biz(1001, 30, "SO-4")); err != nil {
		t.Fatalf("首次 Debit: %v", err)
	}
	if err := st.Debit(ctx, barrierOf(t, ctx), biz(1001, 30, "SO-4")); err != nil {
		t.Fatalf("重放 Debit 必须成功返回（幂等，不是失败）：%v", err)
	}
	if acc := mem.Account(1001); acc.Balance != 70 {
		t.Fatalf("重放后余额应仍为 70，实际 %d —— barrier 幂等失效，用户被重复扣款", acc.Balance)
	}
	if got := mem.Ledgers(); len(got) != 1 {
		t.Fatalf("重放后应仍只有 1 条流水，实际 %d 条", len(got))
	}
}

// TestDebitLedgerKeyIsSecondIdempotencyGate：barrier 行丢了（表被重建、备份
// 回滚）时，靠 (order_no, type) 唯一键挡住第二次扣款。
//
// 与上一条不是同一道闸：上一条的前提是 barrier 表完好；这一条的前提是
// barrier 表**不可信**。只做上一条，barrier 一丢就重复扣款；只做这一条，
// 补偿段的空补偿/悬挂语义就没了（那些判据只有 barrier 表给得出）。
func TestDebitLedgerKeyIsSecondIdempotencyGate(t *testing.T) {
	st, mem := newStore(t, 100)

	first := dtmCtx(t, "gid-a", "01", "action")
	if err := st.Debit(first, barrierOf(t, first), biz(1001, 30, "SO-5")); err != nil {
		t.Fatalf("首次 Debit: %v", err)
	}
	// 换一个 gid = barrier 表里没有这一行（等价于 barrier 表被清空后重放）
	second := dtmCtx(t, "gid-b", "01", "action")
	if err := st.Debit(second, barrierOf(t, second), biz(1001, 30, "SO-5")); err != nil {
		t.Fatalf("barrier 丢失后的重放必须幂等成功，实际 err=%v", err)
	}
	if acc := mem.Account(1001); acc.Balance != 70 {
		t.Fatalf("流水唯一键应挡住二次扣款，余额应仍为 70，实际 %d", acc.Balance)
	}
	if got := mem.Ledgers(); len(got) != 1 {
		t.Fatalf("应仍只有 1 条流水，实际 %d 条：%+v", len(got), got)
	}
}

// ---- Refund（Saga 补偿段）----

// TestRefundRestoresBalance：反向回补余额与累计入账，落 type=1 流水。
func TestRefundRestoresBalanceAndWritesLedger(t *testing.T) {
	st, mem := newStore(t, 100)

	debitCtx := dtmCtx(t, "gid-refund", "01", "action")
	if err := st.Debit(debitCtx, barrierOf(t, debitCtx), biz(1001, 30, "SO-6")); err != nil {
		t.Fatalf("Debit: %v", err)
	}
	refundCtx := dtmCtx(t, "gid-refund", "01", "compensate")
	if err := st.Refund(refundCtx, barrierOf(t, refundCtx), biz(1001, 30, "SO-6")); err != nil {
		t.Fatalf("Refund: %v", err)
	}
	acc := mem.Account(1001)
	// total_earned 是**累计**入账额，不是当前余额：种子给了 100，回补再加 30。
	// 断言写成余额归位 + 累计值，两条各答一个问题（钱回来了吗 / 这笔回补进账了吗）。
	if acc.Balance != 100 || acc.TotalEarned != 130 {
		t.Fatalf("回补后应回到 balance=100 且 total_earned=100+30=130，实际 %+v", acc)
	}
	ledgers := mem.Ledgers()
	if len(ledgers) != 2 {
		t.Fatalf("应为 2 条流水（消费 + 回补），实际 %d 条", len(ledgers))
	}
	back := ledgers[1]
	if back.Type != int64(model.TypeEarn) || back.Amount != 30 || back.OrderNo != "SO-6" {
		t.Fatalf("回补流水应为 (SO-6, type=入账, 30)，实际 %+v", back)
	}
}

// TestRefundAfterFailedActionIsNullCompensate：补偿先于正向到达（正向那条因
// 余额不足失败）时，dtm 仍会发补偿段。此时补偿必须**什么都不做** —— 否则
// 会凭空给用户加钱。
//
// 这条判据由 dtm 官方 barrier 给出（补偿段会先插一行 op=action 的 barrier，
// 命中即判空补偿）。本用例锁的是「我们没有绕过它」。
func TestRefundAfterFailedActionIsNullCompensate(t *testing.T) {
	st, mem := newStore(t, 100)
	// 正向段因余额不足失败 → 同事务回滚，barrier 行也没留下
	failCtx := dtmCtx(t, "gid-null", "01", "action")
	if err := st.Debit(failCtx, barrierOf(t, failCtx), biz(1001, 300, "SO-7")); !errors.Is(err, ErrDtmInsufficient) {
		t.Fatalf("前置：正向段应因余额不足失败，实际 err=%v", err)
	}
	refundCtx := dtmCtx(t, "gid-null", "01", "compensate")
	if err := st.Refund(refundCtx, barrierOf(t, refundCtx), biz(1001, 300, "SO-7")); err != nil {
		t.Fatalf("空补偿必须返回成功：%v", err)
	}
	if acc := mem.Account(1001); acc.Balance != 100 || acc.TotalEarned != 100 {
		t.Fatalf("空补偿不得改账户，实际 %+v", acc)
	}
	if got := mem.Ledgers(); len(got) != 0 {
		t.Fatalf("空补偿不得落流水，实际 %+v", got)
	}
}

// TestRefundReplayIsIdempotent：补偿段重复回调只回补一次（dtm 会重试补偿）。
func TestRefundReplayIsIdempotent(t *testing.T) {
	st, mem := newStore(t, 100)

	act := dtmCtx(t, "gid-ref-replay", "01", "action")
	if err := st.Debit(act, barrierOf(t, act), biz(1001, 30, "SO-8")); err != nil {
		t.Fatalf("Debit: %v", err)
	}
	for i := 0; i < 2; i++ {
		cp := dtmCtx(t, "gid-ref-replay", "01", "compensate")
		if err := st.Refund(cp, barrierOf(t, cp), biz(1001, 30, "SO-8")); err != nil {
			t.Fatalf("第 %d 次补偿: %v", i+1, err)
		}
	}
	if acc := mem.Account(1001); acc.Balance != 100 {
		t.Fatalf("补偿重放后余额应仍为 100，实际 %d", acc.Balance)
	}
	if got := mem.Ledgers(); len(got) != 2 {
		t.Fatalf("补偿重放后应仍只有 2 条流水，实际 %d 条：%+v", len(got), got)
	}
}

// ---- TryFreeze（TCC 第一段）----

// TestTryFreezeMovesBalanceToFrozen：可用↓ 冻结↑，总额守恒，且不落流水。
//
// 不落流水是有意的：Try 不产生已消费的事实，写流水会让对账把「冻结」当成
// 「花掉」。它的幂等只由 barrier 承担（TCC 的三段靠 op 区分，不会互相判重）。
func TestTryFreezeMovesBalanceToFrozen(t *testing.T) {
	st, mem := newStore(t, 100)
	ctx := dtmCtx(t, "gid-try", "01", "try")

	if err := st.TryFreeze(ctx, barrierOf(t, ctx), biz(1001, 40, "SO-9")); err != nil {
		t.Fatalf("TryFreeze: %v", err)
	}
	acc := mem.Account(1001)
	if acc.Balance != 60 || acc.Frozen != 40 {
		t.Fatalf("TryFreeze 后应 balance=60 frozen=40，实际 %+v", acc)
	}
	if acc.Balance+acc.Frozen != 100 {
		t.Fatalf("三态不变式被破坏：balance+frozen=%d", acc.Balance+acc.Frozen)
	}
	if got := mem.Ledgers(); len(got) != 0 {
		t.Fatalf("TryFreeze 不该落流水，实际 %+v", got)
	}
}

// TestTryFreezeInsufficientIsBusinessFailure：可用余额不足 → 业务失败，
// 且 balance/frozen 一个都不许动。
func TestTryFreezeInsufficientIsBusinessFailure(t *testing.T) {
	st, mem := newStore(t, 30)
	ctx := dtmCtx(t, "gid-try-short", "01", "try")

	err := st.TryFreeze(ctx, barrierOf(t, ctx), biz(1001, 40, "SO-10"))
	if !errors.Is(err, ErrDtmInsufficient) {
		t.Fatalf("可用余额不足须判为 ErrDtmInsufficient，实际 err=%v", err)
	}
	if !errors.Is(err, dtmcli.ErrFailure) {
		t.Fatalf("须带官方哨兵，实际 err=%v", err)
	}
	acc := mem.Account(1001)
	if acc.Balance != 30 || acc.Frozen != 0 {
		t.Fatalf("冻结失败后账户不许有半截改动，实际 %+v", acc)
	}
}

// TestTryFreezeReplayIsIdempotent：同 gid 的 Try 重复回调只冻结一次。
func TestTryFreezeReplayIsIdempotent(t *testing.T) {
	st, mem := newStore(t, 100)
	ctx := dtmCtx(t, "gid-try-replay", "01", "try")

	if err := st.TryFreeze(ctx, barrierOf(t, ctx), biz(1001, 40, "SO-11")); err != nil {
		t.Fatalf("首次 TryFreeze: %v", err)
	}
	if err := st.TryFreeze(ctx, barrierOf(t, ctx), biz(1001, 40, "SO-11")); err != nil {
		t.Fatalf("重放 TryFreeze 必须成功返回：%v", err)
	}
	acc := mem.Account(1001)
	if acc.Balance != 60 || acc.Frozen != 40 {
		t.Fatalf("重放后应仍 balance=60 frozen=40，实际 %+v —— 同一笔被冻结两次等于把用户的钱锁死", acc)
	}
}

// ---- ConfirmFreeze（TCC 第二段）----

// TestConfirmFreezeConsumesFrozen：冻结↓ 真正实扣（累计消费↑）并落一条流水。
func TestConfirmFreezeConsumesFrozen(t *testing.T) {
	st, mem := newStore(t, 100)

	tryCtx := dtmCtx(t, "gid-confirm", "01", "try")
	if err := st.TryFreeze(tryCtx, barrierOf(t, tryCtx), biz(1001, 40, "SO-12")); err != nil {
		t.Fatalf("TryFreeze: %v", err)
	}
	cfmCtx := dtmCtx(t, "gid-confirm", "01", "confirm")
	if err := st.ConfirmFreeze(cfmCtx, barrierOf(t, cfmCtx), biz(1001, 40, "SO-12")); err != nil {
		t.Fatalf("ConfirmFreeze: %v", err)
	}
	acc := mem.Account(1001)
	if acc.Frozen != 0 || acc.Balance != 60 || acc.TotalSpent != 40 {
		t.Fatalf("Confirm 后应 frozen=0 balance=60 total_spent=40，实际 %+v", acc)
	}
	ledgers := mem.Ledgers()
	if len(ledgers) != 1 {
		t.Fatalf("Confirm 应落 1 条流水，实际 %d 条", len(ledgers))
	}
	lg := ledgers[0]
	// Confirm 流水也记「消费」，但 order_no 带 tcc- 前缀：Saga 的 Debit 用的是
	// 原始订单号，两者同为 type=消费，若不区分，同一订单既走 Saga 又走 TCC 时
	// 第二笔会被流水唯一键判成重复而**静默不扣**。
	if lg.Type != int64(model.TypeConsume) {
		t.Fatalf("Confirm 流水类型应为消费，实际 %d", lg.Type)
	}
	if lg.OrderNo == "SO-12" {
		t.Fatalf("Confirm 流水 order_no 必须带 tcc- 命名空间前缀，实际 %q —— 会与 Saga Debit 的流水键撞车", lg.OrderNo)
	}
	if lg.BalanceAfter != 60 {
		t.Fatalf("Confirm 流水 balance_after 应为 60，实际 %d", lg.BalanceAfter)
	}
}

// TestConfirmFreezeFrozenShortIsBusinessFailure：冻结量不足（Try 没成功过、
// 或已被别处释放）→ 业务失败，绝不允许把冻结扣成负数。
func TestConfirmFreezeFrozenShortIsBusinessFailure(t *testing.T) {
	st, mem := newStore(t, 100)
	ctx := dtmCtx(t, "gid-confirm-short", "01", "confirm")

	err := st.ConfirmFreeze(ctx, barrierOf(t, ctx), biz(1001, 40, "SO-13"))
	if !errors.Is(err, ErrDtmFrozenShort) {
		t.Fatalf("冻结不足须判为 ErrDtmFrozenShort，实际 err=%v", err)
	}
	if !errors.Is(err, dtmcli.ErrFailure) {
		t.Fatalf("须带官方哨兵，实际 err=%v", err)
	}
	acc := mem.Account(1001)
	if acc.Frozen != 0 || acc.Balance != 100 || acc.TotalSpent != 0 {
		t.Fatalf("确认失败后账户不许有半截改动，实际 %+v", acc)
	}
}

// TestConfirmFreezeReplayIsIdempotent：Confirm 重复回调只实扣一次。
func TestConfirmFreezeReplayIsIdempotent(t *testing.T) {
	st, mem := newStore(t, 100)
	tryCtx := dtmCtx(t, "gid-confirm-replay", "01", "try")
	if err := st.TryFreeze(tryCtx, barrierOf(t, tryCtx), biz(1001, 40, "SO-14")); err != nil {
		t.Fatalf("TryFreeze: %v", err)
	}
	cfmCtx := dtmCtx(t, "gid-confirm-replay", "01", "confirm")
	for i := 0; i < 2; i++ {
		if err := st.ConfirmFreeze(cfmCtx, barrierOf(t, cfmCtx), biz(1001, 40, "SO-14")); err != nil {
			t.Fatalf("第 %d 次 ConfirmFreeze: %v", i+1, err)
		}
	}
	acc := mem.Account(1001)
	if acc.Frozen != 0 || acc.TotalSpent != 40 {
		t.Fatalf("重放后应仍 total_spent=40 frozen=0，实际 %+v —— 重复实扣会把用户余额记成负账", acc)
	}
	if got := mem.Ledgers(); len(got) != 1 {
		t.Fatalf("重放后应仍只有 1 条流水，实际 %d 条", len(got))
	}
}

// ---- CancelFreeze（TCC 第三段）----

// TestCancelFreezeRestoresBalance：冻结↓ 可用↑，完全回到 Try 之前。
func TestCancelFreezeRestoresBalance(t *testing.T) {
	st, mem := newStore(t, 100)
	tryCtx := dtmCtx(t, "gid-cancel", "01", "try")
	if err := st.TryFreeze(tryCtx, barrierOf(t, tryCtx), biz(1001, 40, "SO-15")); err != nil {
		t.Fatalf("TryFreeze: %v", err)
	}
	cxlCtx := dtmCtx(t, "gid-cancel", "01", "cancel")
	if err := st.CancelFreeze(cxlCtx, barrierOf(t, cxlCtx), biz(1001, 40, "SO-15")); err != nil {
		t.Fatalf("CancelFreeze: %v", err)
	}
	acc := mem.Account(1001)
	if acc.Balance != 100 || acc.Frozen != 0 || acc.TotalSpent != 0 {
		t.Fatalf("取消后应完全回到 balance=100 frozen=0，实际 %+v", acc)
	}
	if got := mem.Ledgers(); len(got) != 0 {
		t.Fatalf("取消不得落流水（这笔积分从未花出去），实际 %+v", got)
	}
}

// TestCancelFreezeFrozenShortIsBusinessFailure：冻结量不足以回退 → 业务失败。
//
// 前置必须先有一次成功的 Try：dtm 的 barrier 对 cancel 段有「空补偿」判据
// —— 同 gid 下若从来没有 try 段落过账（它先补插一行 op=try 的 barrier 行），
// cancel 段会被**直接跳过**，根本不进业务写。这不是缺陷，是「没冻结过就别
// 退钱」的语义；本用例要验的是跳过之后那一层：冻结量真的不够时，账不能被
// 加回去。
func TestCancelFreezeFrozenShortIsBusinessFailure(t *testing.T) {
	st, mem := newStore(t, 100)
	tryCtx := dtmCtx(t, "gid-cancel-short", "01", "try")
	if err := st.TryFreeze(tryCtx, barrierOf(t, tryCtx), biz(1001, 40, "SO-16")); err != nil {
		t.Fatalf("前置 TryFreeze: %v", err)
	}
	// 取消 60 > 冻结 40：编排侧金额算错或做部分取消时就是这个形态。
	ctx := dtmCtx(t, "gid-cancel-short", "01", "cancel")
	err := st.CancelFreeze(ctx, barrierOf(t, ctx), biz(1001, 60, "SO-16"))
	if !errors.Is(err, ErrDtmFrozenShort) {
		t.Fatalf("冻结不足须判为 ErrDtmFrozenShort，实际 err=%v", err)
	}
	if !errors.Is(err, dtmcli.ErrFailure) {
		t.Fatalf("须带官方哨兵，实际 err=%v", err)
	}
	acc := mem.Account(1001)
	if acc.Balance != 60 || acc.Frozen != 40 {
		t.Fatalf("取消失败后账户不许有半截改动，实际 %+v", acc)
	}
}

// TestCancelWithoutTryIsNullCompensate：没有 Try 就来 Cancel（乱序回调）时，
// dtm 官方 barrier 判空补偿并跳过 —— 一个子儿都不许加。
//
// 这条锁的是「我们没有绕过官方判据」：一旦有人图省事把 cancel 直接写成
// 「balance += cost」，凭空加钱就在这里发生，且每次都「成功」。
func TestCancelWithoutTryIsNullCompensate(t *testing.T) {
	st, mem := newStore(t, 100)
	ctx := dtmCtx(t, "gid-cancel-no-try", "01", "cancel")

	if err := st.CancelFreeze(ctx, barrierOf(t, ctx), biz(1001, 40, "SO-19")); err != nil {
		t.Fatalf("空补偿必须返回成功：%v", err)
	}
	acc := mem.Account(1001)
	if acc.Balance != 100 || acc.Frozen != 0 {
		t.Fatalf("空补偿不得改账户，实际 %+v —— 没有 Try 就回退等于凭空给用户加分", acc)
	}
}

// TestCancelFreezeReplayIsIdempotent：Cancel 重复回调只回退一次。
func TestCancelFreezeReplayIsIdempotent(t *testing.T) {
	st, mem := newStore(t, 100)
	tryCtx := dtmCtx(t, "gid-cancel-replay", "01", "try")
	if err := st.TryFreeze(tryCtx, barrierOf(t, tryCtx), biz(1001, 40, "SO-17")); err != nil {
		t.Fatalf("TryFreeze: %v", err)
	}
	cxlCtx := dtmCtx(t, "gid-cancel-replay", "01", "cancel")
	for i := 0; i < 2; i++ {
		if err := st.CancelFreeze(cxlCtx, barrierOf(t, cxlCtx), biz(1001, 40, "SO-17")); err != nil {
			t.Fatalf("第 %d 次 CancelFreeze: %v", i+1, err)
		}
	}
	acc := mem.Account(1001)
	if acc.Balance != 100 || acc.Frozen != 0 {
		t.Fatalf("重放后应仍 balance=100 frozen=0，实际 %+v —— 重复回退等于凭空给用户加分", acc)
	}
}

// TestCancelAfterConfirmMustNotRefundConsumed：Try→Confirm 已实扣后再收到
// Cancel（dtm 的 Try 段其实不会这么排布，但乱序/重试都可能造出这个组合）。
// 冻结量已被 Confirm 消掉，故 Cancel 必须判失败而不是给用户白加钱。
func TestCancelAfterConfirmMustNotRefundConsumed(t *testing.T) {
	st, mem := newStore(t, 100)
	tryCtx := dtmCtx(t, "gid-confirm-then-cancel", "01", "try")
	if err := st.TryFreeze(tryCtx, barrierOf(t, tryCtx), biz(1001, 40, "SO-18")); err != nil {
		t.Fatalf("TryFreeze: %v", err)
	}
	cfmCtx := dtmCtx(t, "gid-confirm-then-cancel", "01", "confirm")
	if err := st.ConfirmFreeze(cfmCtx, barrierOf(t, cfmCtx), biz(1001, 40, "SO-18")); err != nil {
		t.Fatalf("ConfirmFreeze: %v", err)
	}
	cxlCtx := dtmCtx(t, "gid-confirm-then-cancel", "01", "cancel")
	if err := st.CancelFreeze(cxlCtx, barrierOf(t, cxlCtx), biz(1001, 40, "SO-18")); !errors.Is(err, ErrDtmFrozenShort) {
		t.Fatalf("已实扣后再 Cancel 必须判失败（不得凭空回分），实际 err=%v", err)
	}
	if acc := mem.Account(1001); acc.Balance != 60 || acc.TotalSpent != 40 {
		t.Fatalf("账户应保持已实扣状态，实际 %+v", acc)
	}
}

// ---- 列名不得虚构（守卫）----

// schemaColumns 逐字抄自 examples/micro/scripts/schema-micro.sql 与
// examples/mall/scripts/schema.sql（points 段）。抄一份在这里而不是去读
// 脚本文件：脚本是部署产物、未必在本仓检出；判据必须自己站得住。
var schemaColumns = map[string][]string{
	"point_accounts": {"id", "user_id", "balance", "frozen", "total_earned", "total_spent", "updated_at"},
	"point_ledgers":  {"id", "user_id", "order_no", "type", "amount", "balance_after", "remark", "created_at"},
	"barrier":        {"id", "trans_type", "gid", "branch_id", "op", "barrier_id", "reason", "create_time", "update_time"},
}

// sqlKeywords 求值时要放过的词（非列名）。
var sqlKeywords = map[string]bool{
	"select": true, "from": true, "where": true, "insert": true, "into": true,
	"values": true, "dual": true, "not": true, "exists": true, "and": true,
	"for": true, "update": true, "set": true, "ignore": true,
}

var identRe = regexp.MustCompile(`[a-z_][a-z0-9_]*`)

// TestDtmStoreSQLUsesOnlySchemaColumns 是「不得虚构列」这条纪律的可执行判据。
//
// 为什么它必须存在：本文件的 SQL 是裸 SQL，编译器不管列名，写错一个列
// （frozen_amount / balances / balance_after 串到别的表）要等到真机第一次执行
// 才炸 —— 而真机上那一刻可能正处理一笔真实订单。更糟的是 MySQL 的宽松模式
// 会把部分拼错静默当成 0，账就悄悄错了。夹具（dtmstub）只认本包自己抄的列，
// 它能挡住「夹具没抄到的列」，挡不住「两边抄错同一个列」。
//
// 反向探针：把 sqlTryFreeze 里的 frozen 改成 frozen_amount，本用例必红。
func TestDtmStoreSQLUsesOnlySchemaColumns(t *testing.T) {
	// 逐个点名：常量改名/删除时这里立刻红。只写「至少取到 N 条」是不够的 ——
	// 新常量忘了登记会静默漏检。
	want := []string{
		"sqlLockAccount", "sqlInsertLedger", "sqlDebit", "sqlRefund",
		"sqlTryFreeze", "sqlConfirmFreeze", "sqlCancelFreeze",
	}
	stmts := extractSQLConsts(t, "dtmstore.go")
	for _, name := range want {
		if _, ok := stmts[name]; !ok {
			t.Fatalf("没从 dtmstore.go 取到 SQL 常量 %s —— 守卫失效（常量被改名或删除？）", name)
		}
	}
	for name, q := range stmts {
		table := sqlTable(q)
		if table == "" {
			t.Fatalf("%s：认不出作用表，SQL=%q", name, q)
		}
		cols, ok := schemaColumns[table]
		if !ok {
			t.Fatalf("%s：SQL 指向本域不存在的表 %q", name, table)
		}
		allowed := map[string]bool{table: true}
		for _, c := range cols {
			allowed[c] = true
		}
		for _, id := range identRe.FindAllString(strings.ToLower(q), -1) {
			if sqlKeywords[id] || allowed[id] {
				continue
			}
			t.Errorf("%s：列/表 %q 不在 schema.sql 的 %s 里 —— 虚构列，真机上才会炸", name, id, table)
		}
	}
}

// extractSQLConsts 从本包源码里取出所有 SQL 常量字面量。
//
// 走 AST 而不是手抄一份清单：手抄的清单会与实际发出的 SQL 漂移（改了语句
// 忘了改清单，守卫就只验着旧的那几条），而漂移的方向恰好是守卫失效的方向。
//
// 必须能处理「多段字面量拼接」——本文件的 SQL 为了 gofmt 对齐写成
// `...` + `...` 两段，取值是 BinaryExpr 而不是 BasicLit；只认 BasicLit 的
// 提取器会一条都取不到，然后**全绿**：这正是本用例第一版的样子（反向探针
// 改 frozen → frozen_amount 竟然通过）。
func extractSQLConsts(t *testing.T, file string) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("解析 %s 失败：%v", file, err)
	}
	out := map[string]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, v := range vs.Values {
			val, ok := literalString(v)
			if !ok {
				continue
			}
			low := strings.ToLower(val)
			if !strings.HasPrefix(low, "select ") && !strings.HasPrefix(low, "insert ") &&
				!strings.HasPrefix(low, "update ") {
				continue
			}
			out[vs.Names[i].Name] = val
		}
		return true
	})
	return out
}

// literalString 取常量表达式里的字符串值（支持 `a` + `b` 拼接）。
func literalString(expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(e.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		l, ok1 := literalString(e.X)
		r, ok2 := literalString(e.Y)
		return l + r, ok1 && ok2
	case *ast.ParenExpr:
		return literalString(e.X)
	default:
		return "", false
	}
}

// sqlTable 从语句里取作用表（insert 的表在 into 之后，update 的在动词之后）。
func sqlTable(q string) string {
	q = strings.ToLower(q)
	switch {
	case strings.HasPrefix(q, "insert ignore into "):
		return firstToken(q[len("insert ignore into "):])
	case strings.HasPrefix(q, "insert into "):
		return firstToken(q[len("insert into "):])
	case strings.HasPrefix(q, "update "):
		return firstToken(q[len("update "):])
	case strings.HasPrefix(q, "select "):
		i := strings.Index(q, " from ")
		if i < 0 {
			return ""
		}
		return firstToken(q[i+len(" from "):])
	}
	return ""
}

func firstToken(s string) string {
	stop := strings.IndexAny(s, " (")
	if stop < 0 {
		stop = len(s)
	}
	return strings.TrimSpace(s[:stop])
}
