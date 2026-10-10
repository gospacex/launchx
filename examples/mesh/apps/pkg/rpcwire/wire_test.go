package rpcwire_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/bytedance/gopkg/cloud/metainfo"
	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/pkg/kerrors"
	"github.com/cloudwego/kitex/pkg/utils"
	"github.com/cloudwego/kitex/server"

	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	productpb "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/product"
	productrpc "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/product/product"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/rpcwire"
)

// probeSvc 只实现 GetProduct；其余方法走不到，返回 nil 即可。
//
// 它把服务端「看到的」东西（metainfo 里的身份键）记进 seen 通道，
// 让测试能断言身份真的过了进程边界 —— 而不是只断言客户端拿到的码。
type probeSvc struct {
	seen chan string
	err  error
}

func (s *probeSvc) GetProduct(ctx context.Context, _ *productpb.GetProductReq) (*productpb.GetProductResp, error) {
	v, _ := metainfo.GetValue(ctx, "mesh-user-id")
	if s.seen != nil {
		s.seen <- v
	}
	return nil, s.err
}

func (s *probeSvc) CreateProduct(context.Context, *productpb.CreateProductReq) (*productpb.CreateProductResp, error) {
	return nil, nil
}
func (s *probeSvc) UpdateProduct(context.Context, *productpb.UpdateProductReq) (*productpb.UpdateProductResp, error) {
	return nil, nil
}
func (s *probeSvc) UpdateStatus(context.Context, *productpb.UpdateStatusReq) (*productpb.UpdateStatusResp, error) {
	return nil, nil
}
func (s *probeSvc) ListProducts(context.Context, *productpb.ListProductsReq) (*productpb.ListProductsResp, error) {
	return nil, nil
}
func (s *probeSvc) Report(context.Context, *productpb.ReportReq) (*productpb.ReportResp, error) {
	return nil, nil
}

// startProbe 起一个真 kitex 服务端 + 客户端（都经 rpcwire 的选项），
// 跑一次 GetProduct，返回「服务端读到的身份」与「客户端拿到的错误」。
func startProbe(t *testing.T, svcErr error) (string, error) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close probe listener: %v", err)
	}

	seen := make(chan string, 1)
	opts := append([]server.Option{server.WithServiceAddr(utils.NewNetAddr("tcp", addr))},
		rpcwire.ServerOptions()...)
	srv := productrpc.NewServer(&probeSvc{seen: seen, err: svcErr}, opts...)
	go func() { _ = srv.Run() }()
	defer func() { _ = srv.Stop() }()

	time.Sleep(200 * time.Millisecond)

	cliOpts := append([]client.Option{client.WithHostPorts(addr)}, rpcwire.ClientOptions()...)
	cli, err := productrpc.NewClient("probe", cliOpts...)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	// 身份经 metainfo 出向：这正是 bff 在调下游前做的事。
	ctx := metainfo.WithValue(context.Background(), "mesh-user-id", "42")
	_, callErr := cli.GetProduct(ctx, &productpb.GetProductReq{ProductId: 1})

	select {
	case got := <-seen:
		return got, callErr
	case <-time.After(3 * time.Second):
		t.Fatal("服务端没有收到请求")
		return "", nil
	}
}

// TestBizCodeCrossesProcess 业务码必须能被调用方还原成同一个 code。
//
// 这是 bff 错误码映射（401/429/404/501/502）成立的前提：码还原不出来时，
// writeErr 会把每一个业务拒绝都当成 Internal 报 502。
func TestBizCodeCrossesProcess(t *testing.T) {
	_, err := startProbe(t, codes.New(codes.ProductNotFound, "no such product"))
	if err == nil {
		t.Fatal("服务端回了业务错误，客户端却拿到 nil error")
	}
	code, msg := rpcwire.FromRPC(err)
	if code != codes.ProductNotFound {
		t.Fatalf("FromRPC = %d (%q)，期望 %d —— 业务码没能过进程边界；原始错误：%v",
			int32(code), msg, int32(codes.ProductNotFound), err)
	}
	if msg != "no such product" {
		t.Fatalf("消息 = %q，期望 %q", msg, "no such product")
	}
}

// TestClientSideErrorIsNotCodesError 钉住一个反直觉事实：跨进程回来的错误
// **不是** *codes.Error（是 kitex 从 TTHeader 物化出来的 *kerrors.BizStatusError）。
//
// 为什么值得单列一条：这决定了「还原」只能发生在调用**之后**（FromRPC），
// 不能塞进客户端中间件 —— 实测中间件链里读到的是 nil。后来者若把
// writeErr 改回 codes.From(err)，或想「顺手把转换挪进中间件」，这条会告诉
// 他为什么不行；而只跑业务测试是发现不了的（业务路径照样 200）。
func TestClientSideErrorIsNotCodesError(t *testing.T) {
	_, err := startProbe(t, codes.New(codes.ProductNotFound, "no such product"))
	if err == nil {
		t.Fatal("服务端回了业务错误，客户端却拿到 nil error")
	}
	var be *codes.Error
	if errors.As(err, &be) {
		t.Fatal("跨进程回来的错误竟是 *codes.Error —— 说明还原发生在了中间件里，与实测不符（见 wire.go 包注释）")
	}
	if code, _ := codes.From(err); code != codes.Internal {
		t.Fatalf("codes.From 对跨进程 biz error = %d，期望 Internal(%d)：这正是必须走 FromRPC 的原因",
			int32(code), int32(codes.Internal))
	}
	if _, ok := kerrors.FromBizStatusError(err); !ok {
		t.Fatalf("跨进程回来的错误不是 biz status error：%v", err)
	}
}

// TestNonBizErrorStaysInternal 反向判据：非业务错误不得被当成本域业务码。
//
// 混为一谈的后果是「下游挂了」被报成「商品不存在」—— 运维按码排查会对不上账。
func TestNonBizErrorStaysInternal(t *testing.T) {
	_, err := startProbe(t, errors.New("boom"))
	if err == nil {
		t.Fatal("服务端回了错误，客户端却拿到 nil error")
	}
	if code, _ := rpcwire.FromRPC(err); code != codes.Internal {
		t.Fatalf("非业务错误的码 = %d，期望 Internal(%d)", int32(code), int32(codes.Internal))
	}
}

// TestCodesErrorSatisfiesBizInterface 业务码类型必须满足 kitex 的 biz error 接口。
//
// 这是服务端那条链的唯一支点：接口少一个方法，码就静默退回「字符串过界」，
// 而服务照常起、端到端也照常 200（除非有本文件的判据）。
func TestCodesErrorSatisfiesBizInterface(t *testing.T) {
	var _ kerrors.BizStatusErrorIface = codes.New(codes.ProductNotFound, "x")

	err := codes.New(codes.StockNotEnough, "not enough")
	biz, ok := kerrors.FromBizStatusError(err)
	if !ok {
		t.Fatalf("kerrors.FromBizStatusError 认不出 *codes.Error：%v", err)
	}
	if biz.BizStatusCode() != int32(codes.StockNotEnough) || biz.BizMessage() != "not enough" {
		t.Fatalf("biz = (%d, %q)，期望 (%d, %q)",
			biz.BizStatusCode(), biz.BizMessage(), int32(codes.StockNotEnough), "not enough")
	}
}

// TestIdentityCrossesProcess 身份键必须原样过界：srv 侧 admit 读的正是它。
//
// 没有这条，路由的限流主体会永远退化成 "svc" 桶 —— 闸门看着在工作，
// 实际按服务级计数（单用户打满不影响别人，限流形同虚设）。
func TestIdentityCrossesProcess(t *testing.T) {
	got, _ := startProbe(t, nil)
	if got != "42" {
		t.Fatalf("服务端读到的 mesh-user-id = %q，期望 \"42\"（身份没跨过进程边界）", got)
	}
}

// TestOptionsAreNonEmpty 选项列表不能是空的：空列表意味着「按默认装」，
// 而默认不装 TTHeader handler —— 业务码与身份会静默失效，进程看着完全健康。
func TestOptionsAreNonEmpty(t *testing.T) {
	if n := len(rpcwire.ServerOptions()); n == 0 {
		t.Fatal("ServerOptions() 为空")
	}
	if n := len(rpcwire.ClientOptions()); n == 0 {
		t.Fatal("ClientOptions() 为空")
	}
	fmt.Printf("server opts=%d client opts=%d\n",
		len(rpcwire.ServerOptions()), len(rpcwire.ClientOptions()))
}
