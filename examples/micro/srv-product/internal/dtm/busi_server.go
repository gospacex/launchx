// Package dtm srv-product 的 **dtm 分支面**：dtm server 按分支 URL 回调的
// gRPC 服务（Saga 销量两个方向：AddSales / ReduceSales），以及它的装配
// （listener）。
//
// 在分层链上的位置：dtm(gRPC) → service → repository → model。它是 handler
// 层的**兄弟**而不是替代品：对外 RPC 面（kitex，走 pkg/kitex_gen）与分支面
// （dtm 自带的 proto）各自独立，共用同一个 service 层的分支方法。
//
// 三条协议事实决定了本包几乎全部写法，它们都来自 dtm-labs/client v1.18.7 与
// dtm server 的真实实现：
//
//  1. **分支 URL 就是全方法名**：`grpc://` 不带 scheme，dtmdriver 的
//     ParseServerMethod 按第一个 "/" 切开得到 server 与 method。所以
//     proto package（dtmbranch.product）与方法名（AddSales / ReduceSales）一字
//     都不能改，而改动首先表现为「dtm 一直重试、这个分支从没被调到」。
//
//  2. **失败靠 gRPC status，不靠响应体**：dtm 调用分支时用 raw codec，响应
//     字节被塞进一个 `*[]byte` 之后**从不解析**（dtmsvr/trans_status.go 的
//     getGrpcResult）。它只看 status：Aborted ⇒ 业务失败（Saga 立即逆序补偿）；
//     FailedPrecondition + ONGOING ⇒ 稍后重试；其余码 ⇒ 按退避重试（见
//     dtmgrpc.GrpcError2DtmError）。因此 resp.Code/Message 只作人读与将来 HTTP
//     桥接用，**判定失败不依赖它**。
//
//  3. **补偿段与正向同一条 trace**：dtm 按 step 共享载荷（Saga 的 compensate 与
//     action 同取 BinPayloads[i]；TCC 的 cancel 与 confirm 是同一份 TransBranch
//     的副本），补偿分支拿到的是**真 traceparent**，dtmx.ExtractParent 把它挂回
//     正向那条链。一笔 saga 的正向与回滚因此在一棵树上，不必跨 trace 拼接。
//     biz.dtm.gid 仍是按业务号检索的关联键，biz.compensation=true 区分两类段。
//     （只有 dtm 重启续推这类没有原始载荷的推进才落新 trace。）
//
// **不 import kitex**：这条面走 grpc-go，与 kitex 传输面互不渗透。
//
// 关于「不注册 consul」：分支面不是对外服务入口，dtm server 是按配置里的地址
// 直连它的。注册进注册中心只会让调用方多看到一个没有健康检查、没有 auth 的裸
// gRPC 端点 —— 把「分支面没起来」变成「一堆下游报 500」。
package dtm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gospacex/logx"
	"github.com/gospacex/otelx/layer"
	"go.opentelemetry.io/otel/attribute"
	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	dtmgrpcmeta "github.com/dtm-labs/client/dtmgrpc/dtmgimp"

	"github.com/gospacex/dtmx"
	"github.com/gospacex/launchx/examples/micro/pkg/codes"
	productbusi "github.com/gospacex/launchx/examples/micro/srv-product/internal/dtm/protos"
	"github.com/gospacex/launchx/examples/micro/srv-product/internal/model"
)

// 分支方法名。单独钉成常量有两个用处：span 名与「是否补偿」判据都用它，
// 写成字面量散落各处时，改名就会漏掉一处（漏掉的判据 = 补偿段不带
// biz.compensation 标签，排障时看不出这段是回滚来的）。
//
// 它们与 model.DtmXxxBranch 恒等 —— 方法名就是分支名，编排侧按 URL 里的方法名
// 点火才不会写错（见 TestBranchMethodsUseDtmFullMethodNames）。
const (
	methodAddSales    = model.DtmAddSalesBranch
	methodReduceSales = model.DtmReduceSalesBranch
)

// spanEntity 参与 span 名的第二段，拼出的 span 名是 rpc.dtm.product.<Method>。
const spanEntity = "dtm.product"

// BranchService 分支面依赖的 service 能力面。
//
// 为什么是接口而不是 *service.ProductService：handler 的单测要注入桩（否则
// 「协议翻译」与「业务规则」只能靠一个连着真库的真服务一起验，失败时定位不到
// 是哪一层）；更重要的是，它让「本包不认识 repository、不认识 gorm」这条
// 边界在类型上成立 —— 编译期就能看出来有没有把持久层捅进协议层。
type BranchService interface {
	DtmAddSales(ctx context.Context, cmd model.DtmSalesCmd) error
	DtmReduceSales(ctx context.Context, cmd model.DtmSalesCmd) error
}

// BusiServer 商品域 dtm 分支服务实现。
type BusiServer struct {
	productbusi.UnimplementedProductBusiServer

	svc     BranchService
	traceOn bool
	logger  logx.Logger
}

// NewBusiServer 构造（logger 为 nil 时取全局）。
func NewBusiServer(svc BranchService, traceOn bool, logger logx.Logger) *BusiServer {
	if logger == nil {
		logger = logx.GetGlobal()
	}
	return &BusiServer{svc: svc, traceOn: traceOn, logger: logger}
}

// AddSales Saga 正向分支（编排 URL：grpc://<listen_addr>/dtmbranch.product.ProductBusi/AddSales）
func (s *BusiServer) AddSales(ctx context.Context, req *productbusi.ProductBusiReq) (*productbusi.ProductBusiResp, error) {
	return s.run(ctx, methodAddSales, req, s.svc.DtmAddSales)
}

// ReduceSales Saga 补偿分支（与正向同一条 trace：dtm 按 step 共享载荷，故这里仍有真 traceparent）。
func (s *BusiServer) ReduceSales(ctx context.Context, req *productbusi.ProductBusiReq) (*productbusi.ProductBusiResp, error) {
	return s.run(ctx, methodReduceSales, req, s.svc.DtmReduceSales)
}

// run 两个分支方法的公共骨架：续链/开 span → 失败注入 → 解析载荷 → 调 service
// → 翻成 gRPC status。handler 只做协议转换，业务判定一律在 service 层。
func (s *BusiServer) run(ctx context.Context, method string, req *productbusi.ProductBusiReq,
	call func(ctx context.Context, cmd model.DtmSalesCmd) error) (resp *productbusi.ProductBusiResp, err error) {

	compensation := method == methodReduceSales
	// 先续链再开 span：顺序反了的话 span 会挂在 dtm 传入的那个**无 tracestate
	// 的**裸 ctx 上，等于白续。
	ctx = dtmx.ExtractParent(ctx, req.GetTraceparent())

	ctx, end := s.begin(ctx, method)
	defer func() { end(err) }()
	s.tagBranch(ctx, method, compensation, req.GetTraceparent())

	// 失败注入（演示用，真机恒空）：编排侧点名某一段让它失败，用来点火 Saga 的
	// 逆序补偿。放在调 service **之前** —— 注入的语义是「这一段不许落库」，
	// 事后报错已经晚了（销量已经记上）。
	if want := req.GetFailInject(); want != "" && want == method {
		return s.reject(ctx, method, dtmx.BusinessFailure(
			fmt.Errorf("fail_inject=%s: %w", method,
				codes.New(codes.CodeProductWriteFailed, "product branch failure injected"))))
	}

	var biz model.DtmSalesCmd
	if jerr := json.Unmarshal([]byte(req.GetBiz()), &biz); jerr != nil {
		// 载荷坏掉同样按业务失败：重试不会让编排侧拼对的 JSON 变对，而报成
		// Internal 会让 dtm 一直重试到超时，把整笔全局事务吊在那儿。
		s.logger.WarnContext(ctx, "product: dtm branch payload is not valid json",
			logx.String("method", method), logx.Err(jerr))
		return s.reject(ctx, method, dtmx.BusinessFailure(fmt.Errorf("decode biz payload: %w", jerr)))
	}

	if err := call(ctx, biz); err != nil {
		return s.reject(ctx, method, err)
	}
	ok := codes.OK()
	return &productbusi.ProductBusiResp{Code: ok.Code, Message: ok.Message}, nil
}

// begin 开分支入口 span（rpc.dtm.product.<Method>）。
//
// 不设 quiet 豁免：非 nil error 一律标红。业务失败标红是对的 —— 它意味着这笔
// 全局事务正在被回滚，正是运维要看见的那类失败。
func (s *BusiServer) begin(ctx context.Context, method string) (context.Context, layer.End) {
	return layer.Begin(ctx, "rpc", spanEntity, method, s.traceOn, nil)
}

// tagBranch 挂分支上下文标签：gid / branch / op / 是否补偿 / 链路来源。
//
// biz.dtm.gid 是补偿段与正向段的**唯一**关联手段（两段不在一条 trace 上），
// 排障时「这个 gid 正向加了多少、退了多少」全靠它串起来，故即便埋点关闭也
// 保持代码路径存在（layer.Attrs 自己判空）。
func (s *BusiServer) tagBranch(ctx context.Context, method string, compensation bool, traceparent string) {
	if !s.traceOn {
		return
	}
	layer.Attrs(ctx,
		attribute.String("biz.dtm.gid", dtmgrpcmeta.GetDtmMetaFromContext(ctx, "gid")),
		attribute.String("biz.dtm.branch", dtmgrpcmeta.GetDtmMetaFromContext(ctx, "branch_id")),
		attribute.String("biz.dtm.op", dtmgrpcmeta.GetDtmMetaFromContext(ctx, "op")),
		attribute.Bool("biz.compensation", compensation),
		// continued = 挂在编排侧那条链上；new_root = 补偿段独立成链，
		// 靠 biz.dtm.gid 关联。两者在 Jaeger 上的形态完全不同，标签不写就分不出。
		attribute.String("biz.dtm.trace", traceSource(traceparent)),
		attribute.String("biz.dtm.method", method),
	)
}

func traceSource(traceparent string) string {
	if traceparent == "" {
		return "new_root"
	}
	return "continued"
}

// reject 把 service/协议层的错误翻成「响应体 + gRPC status」。
//
// 判据只有一个：errors.Is(err, dtmcli.ErrFailure)（dtmx.IsBusinessFailure）。
// 命中 → Aborted（dtm 判业务失败，立即逆序补偿，不再重试）；否则 → Internal
// （dtm 按退避重试，等于把「连不上库」这类故障交给时间解决）。
//
// 刻意用 errors.Is 而不是 errors.As(codes.Error)：能不能重试取决于**业务语义**
// （这个失败会不会自己好），不取决于错误被翻译成了哪个码。
//
// 响应体在返回 error 时会被 grpc 丢弃（客户端只拿到 status），它仍被填满的
// 理由是：① 直调 handler 的单测与将来的 HTTP 桥接都要读它；② 服务端日志里的
// 失败现场比一个裸 status 信息量大。
func (s *BusiServer) reject(ctx context.Context, method string, err error) (*productbusi.ProductBusiResp, error) {
	resp := &productbusi.ProductBusiResp{Message: err.Error()}
	code := int64(codes.CodeInternal)
	grpcCode := grpccodes.Internal
	if dtmx.IsBusinessFailure(err) {
		grpcCode = grpccodes.Aborted
		if be, ok := codes.As(err); ok {
			code = be.Code
		} else {
			// 带哨兵却没有 codes 码：fail_inject 之外出现的未知业务失败。
			// 报商品域写失败码而不是 0 —— 0 是成功，用它会让调用方以为这段过了。
			code = codes.CodeProductWriteFailed
		}
	} else {
		s.logger.WarnContext(ctx, "product: dtm branch base failure (dtm will retry)",
			logx.String("method", method), logx.Err(err))
	}
	resp.Code = code
	if be, ok := codes.As(err); ok && be.Message != "" {
		// 保留业务文案（首行），FAILURE 哨兵文案在后半段 —— dtm 对 grpc 分支
		// 只看 status code，不解析响应体，故这里不会误导判定。
		resp.Message = be.Message + ": " + err.Error()
	}
	return resp, grpcstatus.Error(grpcCode, resp.Message)
}

// errNotWired 分支面未装配。
var errNotWired = errors.New("dtm: branch service not wired")
