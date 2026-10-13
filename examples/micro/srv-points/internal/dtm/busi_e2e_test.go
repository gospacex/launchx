package dtm

import (
	"context"
	"testing"
	"time"

	"github.com/gospacex/configx"
	"github.com/gospacex/logx"
	"google.golang.org/grpc"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"

	bizcodes "github.com/gospacex/launchx/examples/micro/pkg/codes"
	pointsbusi "github.com/gospacex/launchx/examples/micro/srv-points/internal/dtm/protos"
	"github.com/gospacex/launchx/examples/micro/srv-points/internal/dtmstub"
	"github.com/gospacex/launchx/examples/micro/srv-points/internal/repository"
	"github.com/gospacex/launchx/examples/micro/srv-points/internal/service"
)

// TestBranchOverRealGrpc exercises 整条分支链路（真 grpc 连接 + 真 service +
// 真 SQL 夹具），而不是直接调 handler 方法。
//
// 为什么必须过真连接：dtm server 是把 dtm-* 四个键放在 **gRPC metadata** 里
// 送进来的（client/dtmgrpc/dtmgimp.TransInfo2Ctx），而 barrier 的取法是
// metadata.FromIncomingContext。直接调方法时我们自造 ctx，键名写错、大小写不对、
// metadata 没透传这一类问题一个都测不出来 —— 而真机上它们的表现是
// 「每个分支都报 barrier unavailable」，排查要跨到 dtm 源码才看得懂。

func TestBranchOverRealGrpc(t *testing.T) {
	mem := dtmstub.New()
	mem.Seed(1001, 100)
	gdb, err := mem.GormDB()
	if err != nil {
		t.Fatalf("开夹具库失败：%v", err)
	}
	store, err := repository.NewDtmBranchStore(gdb)
	if err != nil {
		t.Fatalf("构造分支写通道失败：%v", err)
	}
	svc := service.NewPointsService(
		repository.NewPointsRepository(gdb, false), nil, nil, nil, store, false, logx.GetGlobal())

	srv, err := Start("127.0.0.1:0", svc, false, logx.GetGlobal())
	if err != nil {
		t.Fatalf("起分支面 listener 失败：%v", err)
	}
	t.Cleanup(func() { _ = srv.Stop(t.Context()) })

	conn, err := grpc.NewClient(srv.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("连分支面失败：%v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	cli := pointsbusi.NewPointsBusiClient(conn)

	// 与 dtm server 发的 metadata 逐字一致（dtmgimp.TransInfo2Ctx 的 dtm- 前缀）
	// order_no 逐笔不同：流水幂等键是 (order_no, type)，同一订单号的第二笔
	// 扣款会被判成重放而直接成功 —— 那是设计（一个订单只扣一次），但会让
	// 「余额不足」这一路测不到。故每笔给一个独立单号。
	call := func(method string, orderNo string, cost int64, gid, branchID, op string) error {
		ctx, cancel := contextWithTimeout(t, 3*time.Second)
		defer cancel()
		ctx = metadata.AppendToOutgoingContext(ctx,
			"dtm-gid", gid,
			"dtm-trans_type", "saga",
			"dtm-branch_id", branchID,
			"dtm-op", op,
		)
		req := &pointsbusi.PointsBusiReq{
			Biz: `{"user_id":1001,"cost":` + itoa(cost) + `,"order_no":"` + orderNo + `"}`,
		}
		switch method {
		case "Debit":
			_, err = cli.Debit(ctx, req)
		case "Refund":
			_, err = cli.Refund(ctx, req)
		case "TryFreeze":
			_, err = cli.TryFreeze(ctx, req)
		case "ConfirmFreeze":
			_, err = cli.ConfirmFreeze(ctx, req)
		case "CancelFreeze":
			_, err = cli.CancelFreeze(ctx, req)
		default:
			t.Fatalf("未知方法 %s", method)
		}
		return err
	}

	// ① 正常写成功：真 metadata → barrier 取到 → 事务落库
	if err := call("Debit", "SO-G1", 30, "gid-grpc-1", "01", "action"); err != nil {
		t.Fatalf("Debit over grpc: %v", err)
	}
	if acc := mem.Account(1001); acc.Balance != 70 || acc.TotalSpent != 30 {
		t.Fatalf("Debit 后应 balance=70 total_spent=30，实际 %+v", acc)
	}

	// ② 业务失败经真连接回来仍是 Aborted（dtm 靠它判「立即补偿」）
	err = call("Debit", "SO-G2", 300, "gid-grpc-2", "01", "action")
	st, _ := grpcstatus.FromError(err)
	if st.Code() != grpccodes.Aborted {
		t.Fatalf("余额不足经真连接应是 Aborted，实际 %v（err=%v）", st.Code(), err)
	}

	// ③ barrier 幂等：同 gid 重放不二次扣账
	if err := call("Debit", "SO-G1", 30, "gid-grpc-1", "01", "action"); err != nil {
		t.Fatalf("重放 Debit: %v", err)
	}
	if acc := mem.Account(1001); acc.Balance != 70 {
		t.Fatalf("重放后余额应仍 70，实际 %d", acc.Balance)
	}

	// ④ TCC 三段：Try → Confirm 真连接闭环
	if err := call("TryFreeze", "SO-G3", 20, "gid-grpc-3", "01", "try"); err != nil {
		t.Fatalf("TryFreeze: %v", err)
	}
	if err := call("ConfirmFreeze", "SO-G3", 20, "gid-grpc-3", "01", "confirm"); err != nil {
		t.Fatalf("ConfirmFreeze: %v", err)
	}
	acc := mem.Account(1001)
	if acc.Balance != 50 || acc.Frozen != 0 || acc.TotalSpent != 50 {
		t.Fatalf("TCC 闭环后应 balance=50 frozen=0 total_spent=50，实际 %+v", acc)
	}

	// ⑤ 没有 metadata 的裸调用必须失败，且一个子儿都不许动
	err = callNoMeta(t, cli, 30)
	st, _ = grpcstatus.FromError(err)
	if st.Code() != grpccodes.Aborted {
		t.Fatalf("无 metadata 的裸调用应是 Aborted（判定为业务失败、不重试），实际 %v", st.Code())
	}
	if got := mem.Ledgers(); len(got) != 2 {
		t.Fatalf("前四步共应落 2 条流水（Debit + Confirm），实际 %d 条：%+v", len(got), got)
	}
}

// callNoMeta 不带 dtm-* metadata 直调（模拟「有人把分支端口当普通 RPC 调」）。
func callNoMeta(t *testing.T, cli pointsbusi.PointsBusiClient, cost int64) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, err := cli.Debit(ctx, &pointsbusi.PointsBusiReq{
		Biz: `{"user_id":1001,"cost":` + itoa(cost) + `,"order_no":"SO-NOMETA"}`,
	})
	return err
}

// contextWithTimeout 给一次分支调用留 3s：真连接下 barrier 事务要走一次往返，
// 超时给太紧会把「慢」误报成「错」。
func contextWithTimeout(t *testing.T, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(t.Context(), d)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// loadRepoConfig 读本域 config.yaml（dtm.branch.listen_addr 的事实来源）。
func loadRepoConfig(t *testing.T) (configx.Config, error) {
	t.Helper()
	return configx.New(&configx.ConfigOptions{ConfigFile: "../../config.yaml"})
}

// 断言用：显式引用，避免「误删一个 import」安静地让上面的守卫只剩一半。
var _ = bizcodes.CodeOK
