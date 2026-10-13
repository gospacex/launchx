package dtm

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/gospacex/logx"
	"google.golang.org/grpc"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"

	productbusi "github.com/gospacex/launchx/examples/micro/srv-product/internal/dtm/protos"
	"github.com/gospacex/launchx/examples/micro/srv-product/internal/dtmstub"
	"github.com/gospacex/launchx/examples/micro/srv-product/internal/model"
	"github.com/gospacex/launchx/examples/micro/srv-product/internal/repository"
	"github.com/gospacex/launchx/examples/micro/srv-product/internal/service"
)

// TestBranchOverRealGrpc 走整条分支链路（真 grpc 连接 + 真 service + 真 SQL
// 夹具），而不是直接调 handler 方法。
//
// 为什么必须过真连接：dtm server 是把 dtm-* 四个键放在 **gRPC metadata** 里
// 送进来的（client/dtmgrpc/dtmgimp.TransInfo2Ctx），而 barrier 的取法是
// metadata.FromIncomingContext。直接调方法时我们自造 ctx，键名写错、大小写不对、
// metadata 没透传这一类问题一个都测不出来 —— 而真机上它们的表现是
// 「每个分支都报 barrier unavailable」，排查要跨到 dtm 源码才看得懂。
func TestBranchOverRealGrpc(t *testing.T) {
	mem := dtmstub.New()
	mem.Seed(42, 100)
	gdb, err := mem.GormDB()
	if err != nil {
		t.Fatalf("开夹具库失败：%v", err)
	}
	svc := service.NewProductService(repository.NewProductRepository(gdb, false), nil, nil, nil, false, logx.GetGlobal())

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
	cli := productbusi.NewProductBusiClient(conn)

	// 与 dtm server 发的 metadata 逐字一致（dtmgimp 的 dtm- 前缀）。
	call := func(reduce bool, productID uint64, qty int64, gid, branchID, op string) error {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		ctx = metadata.AppendToOutgoingContext(ctx,
			"dtm-gid", gid,
			"dtm-trans_type", "saga",
			"dtm-branch_id", branchID,
			"dtm-op", op,
		)
		req := &productbusi.ProductBusiReq{
			Biz: `{"product_id":` + strconv.FormatUint(productID, 10) +
				`,"quantity":` + strconv.FormatInt(qty, 10) + `,"order_no":"SO-G1"}`,
		}
		if reduce {
			_, err = cli.ReduceSales(ctx, req)
			return err
		}
		_, err = cli.AddSales(ctx, req)
		return err
	}

	// ① 正常写成功：真 metadata → barrier 取到 → 事务落库
	if err := call(false, 42, 3, "gid-grpc-1", "01", "action"); err != nil {
		t.Fatalf("AddSales over grpc: %v", err)
	}
	if got := mem.SalesOf(42); got.Sold != 103 {
		t.Fatalf("AddSales 后应 sold=103，实际 %+v", got)
	}

	// ② 业务失败经真连接回来仍是 Aborted（dtm 靠它判「立即补偿」）
	//    先让同一 gid 的正向到达（否则 barrier 判成空补偿直接跳过），再要减一个
	//    远大于账面的量 —— 账不平。
	if err := call(false, 42, 3, "gid-grpc-2", "01", "action"); err != nil {
		t.Fatalf("前置正向: %v", err)
	}
	if got := mem.SalesOf(42); got.Sold != 106 {
		t.Fatalf("前置正向后应 sold=106，实际 %d", got.Sold)
	}
	err = call(true, 42, 200, "gid-grpc-2", "01", "compensate")
	if st, _ := grpcstatus.FromError(err); st.Code() != grpccodes.Aborted {
		t.Fatalf("账不平经真连接应是 Aborted，实际 %v（err=%v）", st.Code(), err)
	}
	if got := mem.SalesOf(42); got.Sold != 106 {
		t.Fatalf("业务失败后销量不许变动，实际 %+v", got)
	}

	// ③ barrier 幂等：同 gid 重放不二次记账
	if err := call(false, 42, 3, "gid-grpc-1", "01", "action"); err != nil {
		t.Fatalf("重放 AddSales: %v", err)
	}
	if got := mem.SalesOf(42); got.Sold != 106 {
		t.Fatalf("重放后销量应仍 106（只认第一次的 +3），实际 %d —— barrier 幂等失效", got.Sold)
	}

	// ④ 正向 → 补偿闭环：精确回到基线（专属判据走真连接那一遍）
	if err := call(false, 42, 7, "gid-grpc-4", "01", "action"); err != nil {
		t.Fatalf("AddSales: %v", err)
	}
	if got := mem.SalesOf(42); got.Sold != 113 {
		t.Fatalf("正向后应 sold=113，实际 %d", got.Sold)
	}
	if err := call(true, 42, 7, "gid-grpc-4", "01", "compensate"); err != nil {
		t.Fatalf("ReduceSales: %v", err)
	}
	if got := mem.SalesOf(42); got.Sold != 106 {
		t.Fatalf("补偿后必须精确回到 106，实际 %d", got.Sold)
	}

	// ⑤ 没有 metadata 的裸调用必须失败，且一个子儿都不许动。
	//    注意这里 service 层把它判成**基础失败**（不是业务失败），所以 status
	//    是 Internal —— dtm 会退避重试，而不是立刻放弃这笔全局事务。
	noMetaCtx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, err = cli.AddSales(noMetaCtx, &productbusi.ProductBusiReq{
		Biz: `{"product_id":42,"quantity":3,"order_no":"SO-NOMETA"}`,
	})
	if st, _ := grpcstatus.FromError(err); st.Code() != grpccodes.Internal {
		t.Fatalf("无 metadata 的裸调用应是 Internal（基础设施故障、可重试），实际 %v", st.Code())
	}
	if got := mem.SalesOf(42); got.Sold != 106 {
		t.Fatalf("无 metadata 的调用不许改动销量，实际 %+v", got)
	}
	// ⑥ 编译期口径：领域 DTO 只有这三个字段，handler 不认识 pb。
	var _ model.DtmSalesCmd
}
