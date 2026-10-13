package service

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/dtm-labs/client/dtmcli"
	"github.com/gospacex/logx"
	"google.golang.org/grpc/metadata"

	"github.com/gospacex/dtmx"
	"github.com/gospacex/launchx/examples/micro/pkg/codes"
	"github.com/gospacex/launchx/examples/micro/srv-points/internal/dtmstub"
	"github.com/gospacex/launchx/examples/micro/srv-points/internal/model"
	"github.com/gospacex/launchx/examples/micro/srv-points/internal/repository"
)

// 本文件锁 service 层分支方法的四条纪律（每条都在下游 dtm 包决定生死）：
//
//  1. barrier 必须取到，取不到必须**报错**（不许忽略后照常落库）；
//  2. 业务失败要带 codes 的 5xxx 段（给编排侧/运维一个可读码）；
//  3. 同一个错误还要带 dtm 官方哨兵 dtmcli.ErrFailure —— 少了它 dtm 会把
//     「积分不足」当基础失败无限重试；
//  4. 入参非法按业务失败处理，不当传输错误：编排侧能立刻看到，而不是让 dtm
//     反复重试一个注定失败的分支。
//
// 每个方法覆盖三类：正常写成功 / 余额(冻结)不足 → 业务失败 / barrier 幂等重放。

func TestMain(m *testing.M) {
	dtmx.SetBarrierTableName("barrier")
	os.Exit(m.Run())
}

// dtmCtx 造 dtm server 回调的上下文（metadata 键与 dtmgimp.TransBaseFromGrpc
// 读的那四个一一对应）。
func dtmCtx(gid, branchID, op string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"dtm-gid", gid,
		"dtm-trans_type", "saga",
		"dtm-branch_id", branchID,
		"dtm-op", op,
	))
}

func newDtmSvc(t *testing.T, balance int64) (*PointsService, *dtmstub.DB) {
	t.Helper()
	mem := dtmstub.New()
	mem.Seed(1001, balance)
	gdb, err := mem.GormDB()
	if err != nil {
		t.Fatalf("开夹具库失败：%v", err)
	}
	store, err := repository.NewDtmBranchStore(gdb)
	if err != nil {
		t.Fatalf("构造分支写通道失败：%v", err)
	}
	repo := repository.NewPointsRepository(gdb, false)
	// cache/kv/lock 全 nil：分支面不碰这三样（dtm 回调里没有用户身份、
	// 也没有可安全归属的幂等键），给了反而会让测试看不出依赖边界。
	return NewPointsService(repo, nil, nil, nil, store, false, logx.GetGlobal()), mem
}

func bizPayload(cost int64, orderNo string) model.DtmBiz {
	return model.DtmBiz{UserID: 1001, Cost: cost, OrderNo: orderNo}
}

// ---- 纪律 1：barrier 取不到必须报错 ----

// TestDtmMethodsRejectRequestsWithoutBarrierMetadata 是本文件最要紧的一条。
//
// 没有 barrier 就没有空补偿 / 重复回调 / 悬挂三条守卫：分支会在无保护状态下
// 落库 —— 重复回调重复扣积分、补偿先到则凭空写出脏数据 —— 而当场没有任何
// 迹象（函数返回成功、日志干净）。所以「取不到 barrier」必须变成错误上抛。
//
// 反向探针：把 dtmBranch 里的 barrier 判据短路掉（`if err != nil` 改成
// `if false && err != nil`），本用例必红 —— 五个子用例同时红，且失败信息是
// 「这条错误须带官方哨兵」而不是「有数据被动过」，因为 dtmx.Barrier.Call 还有
// 一道 nil 兜底；一旦那道兜底也被绕过，本用例的「账户不许有半截改动」断言
// 就会接手。
func TestDtmMethodsRejectRequestsWithoutBarrierMetadata(t *testing.T) {
	svc, mem := newDtmSvc(t, 100)
	b := bizPayload(30, "SO-NB-1")

	calls := map[string]func(ctx context.Context, b model.DtmBiz) error{
		"DtmDebit":         svc.DtmDebit,
		"DtmRefund":        svc.DtmRefund,
		"DtmTryFreeze":     svc.DtmTryFreeze,
		"DtmConfirmFreeze": svc.DtmConfirmFreeze,
		"DtmCancelFreeze":  svc.DtmCancelFreeze,
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call(context.Background(), b)
			if err == nil {
				t.Fatalf("%s：无 dtm metadata 时必须报错（无 barrier 落库 = 无保护记账），实际 nil", name)
			}
			if !errors.Is(err, dtmcli.ErrFailure) {
				t.Fatalf("%s：这条错误须带官方哨兵（否则 dtm 当基础失败无限重试），实际 err=%v", name, err)
			}
			be, ok := codes.As(err)
			if !ok {
				t.Fatalf("%s：错误须带 codes 业务码（编排侧与运维要看），实际 err=%v", name, err)
			}
			if be.Code != codes.CodeInternal {
				t.Fatalf("%s：装配/协议类失败应报 %d（内部错误），实际 %d", name, codes.CodeInternal, be.Code)
			}
		})
	}
	// 一个子儿都不许动：无保护路径上的落库是这里要防的唯一后果。
	acc := mem.Account(1001)
	if acc.Balance != 100 || acc.Frozen != 0 || acc.TotalSpent != 0 {
		t.Fatalf("无 barrier 时账户不许有任何改动，实际 %+v", acc)
	}
	if got := mem.Ledgers(); len(got) != 0 {
		t.Fatalf("无 barrier 时不许落流水，实际 %+v", got)
	}
	if got := mem.Barriers(); got != 0 {
		t.Fatalf("无 barrier 时不该插 barrier 行，实际 %d 行", got)
	}
}

// ---- 每个方法的三类语义 ----

// dtmCases 五个分支方法 × 三类语义。表格化而不是写十来个用例：分支方法之间
// 只有「改哪几个列、缺哪一项守卫」的差别，逐个复制粘贴的代价是将来只改一边
// （比如给 Cancel 补了冻结守卫却漏了 Confirm）。
var dtmCases = []struct {
	name string
	// op 是 dtm 的分支 op（决定 barrier 的 originOp，空补偿判据要用）
	op      string
	call    func(s *PointsService, ctx context.Context, b model.DtmBiz) error
	setup   func(t *testing.T, svc *PointsService, gid string) // 正常路径前置（TCC/Saga 补偿都要先有正向段）
	balance int64
	cost    int64
	// wantAfter 一次成功写之后账户应有的形态（seed 已有的累计入账不在其中，见 checkAccount）
	wantAfter dtmAccount
	// 失败路径：cost 取 wantFailCost，wantFailOtherUser=true 时改打到不存在的账户
	wantFailCost     int64
	wantFailOtherUsr bool
	wantFailCode     int64
	wantFailSnt      error
}{
	{
		name: "Debit", op: "action",
		call:    func(s *PointsService, ctx context.Context, b model.DtmBiz) error { return s.DtmDebit(ctx, b) },
		balance: 100, cost: 30,
		wantAfter:    dtmAccount{balance: 70, totalSpent: 30},
		wantFailCost: 300, wantFailCode: codes.CodePointsInsufficient, wantFailSnt: repository.ErrDtmInsufficient,
	},
	{
		name: "Refund", op: "compensate",
		call:    func(s *PointsService, ctx context.Context, b model.DtmBiz) error { return s.DtmRefund(ctx, b) },
		balance: 100, cost: 30,
		// 补偿段的前置必须是同 gid 下的正向段：dtm 对 compensate 有空补偿判据
		// （没有 action 行就跳过），不先扣那 30 分，这一段根本不会进业务写。
		setup:            debitFirst,
		wantAfter:        dtmAccount{balance: 100, totalEarned: 30, totalSpent: 30},
		wantFailCost:     30,
		wantFailOtherUsr: true,
		wantFailCode:     codes.CodePointsAccountNF, wantFailSnt: repository.ErrDtmAccountNF,
	},
	{
		name: "TryFreeze", op: "try",
		call:    func(s *PointsService, ctx context.Context, b model.DtmBiz) error { return s.DtmTryFreeze(ctx, b) },
		balance: 100, cost: 40,
		wantAfter:    dtmAccount{balance: 60, frozen: 40},
		wantFailCost: 300, wantFailCode: codes.CodePointsInsufficient, wantFailSnt: repository.ErrDtmInsufficient,
	},
	{
		name: "ConfirmFreeze", op: "confirm",
		call:    func(s *PointsService, ctx context.Context, b model.DtmBiz) error { return s.DtmConfirmFreeze(ctx, b) },
		balance: 100, cost: 40,
		// 前置 TryFreeze(40)：冻结了 40，再 Confirm(300) 就是冻结不足。
		setup:        freezeFirst,
		wantAfter:    dtmAccount{balance: 60, frozen: 0, totalSpent: 40},
		wantFailCost: 300, wantFailCode: codes.CodePointsInsufficient, wantFailSnt: repository.ErrDtmFrozenShort,
	},
	{
		name: "CancelFreeze", op: "cancel",
		call:    func(s *PointsService, ctx context.Context, b model.DtmBiz) error { return s.DtmCancelFreeze(ctx, b) },
		balance: 100, cost: 40,
		setup:        freezeFirst,
		wantAfter:    dtmAccount{balance: 100, frozen: 0},
		wantFailCost: 300, wantFailCode: codes.CodePointsInsufficient, wantFailSnt: repository.ErrDtmFrozenShort,
	},
}

// debitFirst 先扣一笔，制造「正向段落过账」的前置（Saga 补偿必需）。
func debitFirst(t *testing.T, svc *PointsService, gid string) {
	t.Helper()
	if err := svc.DtmDebit(dtmCtx(gid, "01", "action"), bizPayload(30, "SO-PRE")); err != nil {
		t.Fatalf("前置 Debit: %v", err)
	}
}

// freezeFirst 造「已冻结 40」的账户。Confirm/Cancel 都要求同 gid 下先有一次
// 成功的 Try：dtm 的 barrier 对 cancel 有空补偿判据（没 try 过就直接跳过），
// 而 confirm 没有这层判据、不做前置也能进业务写 —— 前置统一加上，两条路径
// 测的才是「冻结量够/不够」这一件事。
func freezeFirst(t *testing.T, svc *PointsService, gid string) {
	t.Helper()
	if err := svc.DtmTryFreeze(dtmCtx(gid, "01", "try"), bizPayload(40, "SO-TCC")); err != nil {
		t.Fatalf("前置 TryFreeze: %v", err)
	}
}

type dtmAccount struct {
	balance     int64
	frozen      int64
	totalEarned int64
	totalSpent  int64
}

// seedEarned 是 dtmstub.Seed 给账户的初始累计入账额（= 初始余额 100）。
// 单独拎出来是因为它是「种子的属性」而不是「本次写入的效果」：断言写
// totalEarned: 130 指的是「本次回补加了 30」，含不含种子都在同一处相加，
// 免得每张表都写一遍 100+30。
const seedEarned = 100

func checkAccount(t *testing.T, mem *dtmstub.DB, want dtmAccount, what string) {
	t.Helper()
	acc := mem.Account(1001)
	want.totalEarned += seedEarned
	got := dtmAccount{acc.Balance, acc.Frozen, acc.TotalEarned, acc.TotalSpent}
	if got != want {
		t.Fatalf("%s：账户形态应为 %+v（含种子累计入账 %d），实际 %+v", what, want, seedEarned, got)
	}
}

func TestDtmMethodsHappyPath(t *testing.T) {
	for _, c := range dtmCases {
		t.Run(c.name, func(t *testing.T) {
			svc, mem := newDtmSvc(t, c.balance)
			gid := "gid-" + c.name + "-ok"
			if c.setup != nil {
				c.setup(t, svc, gid)
			}
			if err := c.call(svc, dtmCtx(gid, "01", c.op), bizPayload(c.cost, "SO-"+c.name)); err != nil {
				t.Fatalf("%s 正常写失败：%v", c.name, err)
			}
			checkAccount(t, mem, c.wantAfter, c.name+" 之后")
		})
	}
}

func TestDtmMethodsBusinessFailureOnShort(t *testing.T) {
	for _, c := range dtmCases {
		t.Run(c.name, func(t *testing.T) {
			svc, mem := newDtmSvc(t, c.balance)
			gid := "gid-" + c.name + "-short"
			if c.setup != nil {
				c.setup(t, svc, gid)
			}
			before := mem.Account(1001)

			b := bizPayload(c.wantFailCost, "SO-"+c.name+"-short")
			if c.wantFailOtherUsr {
				b.UserID = 2002 // 打不存在的账户 → 账户不存在这一类
			}
			err := c.call(svc, dtmCtx(gid, "01", c.op), b)
			if err == nil {
				t.Fatalf("%s：不足场景必须返回错误，实际 nil（无错误 = dtm 判成功 = 这笔账被记下了）", c.name)
			}
			if !errors.Is(err, c.wantFailSnt) {
				t.Fatalf("%s：应判为 %v，实际 err=%q", c.name, c.wantFailSnt, err)
			}
			if !errors.Is(err, dtmcli.ErrFailure) {
				t.Fatalf("%s：业务失败必须带官方哨兵 dtmcli.ErrFailure，实际 err=%q", c.name, err)
			}
			be, ok := codes.As(err)
			if !ok || be.Code != c.wantFailCode {
				t.Fatalf("%s：业务码应为 %d，实际 %v（ok=%v）", c.name, c.wantFailCode, be, ok)
			}
			if acc := mem.Account(1001); acc != before {
				t.Fatalf("%s：业务失败后账户不许有半截改动，%v → %v", c.name, before, acc)
			}
		})
	}
}

func TestDtmMethodsBarrierReplay(t *testing.T) {
	for _, c := range dtmCases {
		t.Run(c.name, func(t *testing.T) {
			svc, mem := newDtmSvc(t, c.balance)
			gid := "gid-" + c.name + "-replay"
			if c.setup != nil {
				c.setup(t, svc, gid)
			}
			// 重复调用同一 (gid, branch, op)：dtm 会重试分支，第二次必须被
			// barrier 判成「已落过账」，不许二次改账。
			for i := 0; i < 2; i++ {
				if err := c.call(svc, dtmCtx(gid, "01", c.op), bizPayload(c.cost, "SO-"+c.name)); err != nil {
					t.Fatalf("%s 第 %d 次调用失败：%v", c.name, i+1, err)
				}
			}
			checkAccount(t, mem, c.wantAfter, c.name+" 重放之后")
		})
	}
}

// ---- 入参校验 ----

// TestDtmMethodsRejectInvalidPayloadAsBusinessFailure：非法入参按**业务失败**
// 处理，而不是传输错误。
//
// 理由：入参非法时重试一万次也不会变好。报成基础失败的话，dtm 会一直重试到
// 事务超时，那段时间里这笔全局事务悬着，其它域的补偿也被拖住；而它本来就该
// 当场失败、立刻逆序补偿。
func TestDtmMethodsRejectInvalidPayloadAsBusinessFailure(t *testing.T) {
	svc, mem := newDtmSvc(t, 100)
	cases := []struct {
		name string
		b    model.DtmBiz
	}{
		{"user_id<=0", model.DtmBiz{UserID: 0, Cost: 10, OrderNo: "SO-V1"}},
		{"cost<=0", model.DtmBiz{UserID: 1001, Cost: 0, OrderNo: "SO-V2"}},
		{"order_no 空", model.DtmBiz{UserID: 1001, Cost: 10}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, op := range []string{"Debit", "TryFreeze"} {
				err := dtmCallByName(svc, op, dtmCtx("gid-invalid-"+op, "01", "action"), c.b)
				if err == nil {
					t.Fatalf("%s（%s）：非法入参必须返回错误，实际 nil", c.name, op)
				}
				if !errors.Is(err, dtmcli.ErrFailure) {
					t.Fatalf("%s（%s）：非法入参应判业务失败（带官方哨兵），实际 err=%v", c.name, op, err)
				}
				be, ok := codes.As(err)
				if !ok || be.Code != codes.CodePointsAmountInvalid {
					t.Fatalf("%s（%s）：业务码应为 %d，实际 %v", c.name, op, codes.CodePointsAmountInvalid, be)
				}
			}
		})
	}
	acc := mem.Account(1001)
	if acc.Balance != 100 || acc.Frozen != 0 {
		t.Fatalf("非法入参不许改账，实际 %+v", acc)
	}
}

func dtmCallByName(svc *PointsService, name string, ctx context.Context, b model.DtmBiz) error {
	switch name {
	case "Debit":
		return svc.DtmDebit(ctx, b)
	case "TryFreeze":
		return svc.DtmTryFreeze(ctx, b)
	default:
		return errors.New("unknown op " + name)
	}
}
