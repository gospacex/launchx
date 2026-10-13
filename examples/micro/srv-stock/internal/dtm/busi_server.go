// Package dtm srv-stock 的 dtm 分支面：承接 dtm server 的 Saga/TCC gRPC 回调。
//
// 在分层链上的位置：dtm server → **dtm（分支面）** → service → repository。
// 分支面与 kitex 传输面（internal/rpc → route → handler → service）是两条并列的
// 入口，汇到同一个 service 层 —— 业务规则只有一份，dtm 的幂等由 barrier 裁决，
// kitex 的幂等由 stock_reservations 唯一键裁决，两者不互相冒充。
//
// 分支面只做四件事，一件不多：
//  1. 载荷 → 领域 DTO（biz JSON 反序列化；pb 与领域的转换归它，因为只有它认识 pb）
//  2. 从载荷里的 traceparent 续链，开 rpc.dtm.stock.<Method> span
//  3. 失败注入点火（演示用）
//  4. 调 service，并把错误翻成 dtm 认识的 grpc status
//
// **不 import kitex**：这条面走 grpc-go，与 kitex 传输面互不渗透。
//
// 关于「不注册 consul」：分支面不是对外服务入口，dtm server 是按配置里的地址直连
// 它的。注册进注册中心只会让运维多看到一个没有健康检查、没有 auth 的裸 gRPC 端点。
package dtm

import (
	"context"
	"encoding/json"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/grpc/metadata"

	"github.com/dtm-labs/client/dtmgrpc"

	"github.com/gospacex/dtmx"
	"github.com/gospacex/launchx/examples/micro/pkg/codes"
	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/dtm/protos"
	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/model"
	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/service"
	"github.com/gospacex/logx"
	"github.com/gospacex/otelx/layer"
)

// 服务全名（= proto package + service 名）。dtm server 按 URL 回调，
// 而 URL 就是 <listen_addr> + 下面任一 FullMethodName；三处必须一致。
const stockBusiServiceName = "dtmbranch.stock.StockBusi"

// spanNamePrefix 分支入口 span 的前缀。完整名字是 <前缀>.<Method>，例如
// rpc.dtm.stock.Deduct —— 与 dtmx 注释里写的一致：正向链在 Jaeger 上是一条
// bff 提交 → 四域 rpc.dtm.* → service.* → repository.* → db.* 的完整树。
const spanNamePrefix = "rpc.dtm.stock"

// BusiServer 库存域 dtm 分支服务。
type BusiServer struct {
	protos.UnimplementedStockBusiServer

	svc *service.StockService
	// traceOn 业务分层埋点开关（trace.enabled）。与 kitex 面读同一个键。
	traceOn bool
	logger  logx.Logger
}

// NewBusiServer 构造。
func NewBusiServer(svc *service.StockService, traceOn bool, logger logx.Logger) *BusiServer {
	if logger == nil {
		logger = logx.GetGlobal()
	}
	return &BusiServer{svc: svc, traceOn: traceOn, logger: logger}
}

// branchWrite 一个分支的 service 方法。
type branchWrite func(ctx context.Context, cmd model.DtmStockCmd) error

// Deduct Saga 正向分支（编排 URL：grpc://<listen_addr>/dtmbranch.stock.StockBusi/Deduct）
func (s *BusiServer) Deduct(ctx context.Context, req *protos.StockBusiReq) (*protos.StockBusiResp, error) {
	return s.branch(ctx, model.DtmDeductBranch, req, s.svc.DtmDeduct)
}

// Release Saga 补偿分支（由 dtm 异步发起，载荷里没有 traceparent）
func (s *BusiServer) Release(ctx context.Context, req *protos.StockBusiReq) (*protos.StockBusiResp, error) {
	return s.branch(ctx, model.DtmReleaseBranch, req, s.svc.DtmRelease)
}

// TryFreeze TCC 一阶段：可售 → 冻结
func (s *BusiServer) TryFreeze(ctx context.Context, req *protos.StockBusiReq) (*protos.StockBusiResp, error) {
	return s.branch(ctx, model.DtmTryFreezeBranch, req, s.svc.DtmTryFreeze)
}

// ConfirmFreeze TCC 二阶段：冻结 → 实扣
func (s *BusiServer) ConfirmFreeze(ctx context.Context, req *protos.StockBusiReq) (*protos.StockBusiResp, error) {
	return s.branch(ctx, model.DtmConfirmBranch, req, s.svc.DtmConfirmFreeze)
}

// CancelFreeze TCC 二阶段：冻结 → 可售
func (s *BusiServer) CancelFreeze(ctx context.Context, req *protos.StockBusiReq) (*protos.StockBusiResp, error) {
	return s.branch(ctx, model.DtmCancelFreeze, req, s.svc.DtmCancelFreeze)
}

// branch 五个分支共用的协议转换。
//
// 方法名与分支名**恒等**（busi.proto 里 service 的方法名就是分支名），所以这里
// 只传一个 branch：fail_inject 命中、span 名、biz.dtm.branch 三处共用同一个字面量，
// 编排侧按 URL 里的方法名点火才不会写错。TestFullMethodNamesMatchDtmCallbackURL
// 把「方法名 == 分支名」也钉住了。
func (s *BusiServer) branch(ctx context.Context, branch string, req *protos.StockBusiReq, write branchWrite) (*protos.StockBusiResp, error) {
	// ① 续链。载荷无 traceparent 时 ExtractParent 原样返回 ctx —— 补偿 / Cancel
	//    就是这种情况，它们按「新根 trace + biz.dtm.gid 关联」记录，凭空造一个
	//    同 ID 的孤儿 span 只是让 Jaeger 看起来像连上了。
	ctx = dtmx.ExtractParent(ctx, req.GetTraceparent())

	ctx, end := s.begin(ctx, branch)
	var err error
	defer func() { end(err) }()

	layer.Attrs(ctx,
		attribute.String("biz.dtm.gid", dtmMeta(ctx, "gid")),
		attribute.String("biz.dtm.branch", dtmMeta(ctx, "branch_id")),
		attribute.Bool("biz.compensation", isCompensation(branch)))

	// ② 失败注入：在解码之前判，这样「注入命中」这件事不依赖载荷本身是否合法。
	if req.GetFailInject() == branch {
		err = dtmx.BusinessFailure(codes.New(codes.CodeInternal,
			fmt.Sprintf("fail_inject: 分支 %s 被编排侧指定为失败", branch)))
		return failResp(err), dtmgrpc.DtmError2GrpcError(err)
	}

	// ③ 载荷 → 领域 DTO。坏载荷按业务失败处理：重试一万次也还是坏，而放过去
	//    就带着零值 product_id 去落库，失败点还会漂到更远的下游。
	var cmd model.DtmStockCmd
	if jerr := json.Unmarshal([]byte(req.GetBiz()), &cmd); jerr != nil {
		err = dtmx.BusinessFailure(codes.Wrap(codes.CodeInvalidParam, "dtm 分支载荷 biz 不是合法 JSON", jerr))
		return failResp(err), dtmgrpc.DtmError2GrpcError(err)
	}

	// ④ 交棒给 service（barrier、事务、库存语义都在那一层之下）。
	if werr := write(ctx, cmd); werr != nil {
		err = werr
		return failResp(err), dtmgrpc.DtmError2GrpcError(err)
	}
	return &protos.StockBusiResp{Code: codes.CodeOK, Message: "ok"}, nil
}

// begin 开 rpc.dtm.stock.<Method> span。
//
// 不设 quiet 豁免：分支返回 error 一定意味着这次账变没完成，入链才算看得见。
func (s *BusiServer) begin(ctx context.Context, method string) (context.Context, layer.End) {
	return layer.Begin(ctx, "rpc", "dtm.stock", method, s.traceOn, nil)
}

// failResp 把错误翻成响应体的 code/message。
//
// **给人和排障看**，dtm server 并不读这两个字段（它只看 grpc status，见
// busi.proto 里 StockBusiResp 的注释）。留着它们是为了让「分支日志里 code=4002」
// 与 kitex 面的响应体口径一致 —— 否则同一个拒绝在两条链上会报成两个码。
func failResp(err error) *protos.StockBusiResp {
	be, ok := codes.As(err)
	if !ok {
		return &protos.StockBusiResp{Code: codes.CodeInternal, Message: "dtm branch failed"}
	}
	return &protos.StockBusiResp{Code: be.Code, Message: be.Message}
}

// isCompensation 该分支是不是逆向分支。
//
// Release 是 Saga 补偿、CancelFreeze 是 TCC 的一阶段回退；两者都由 dtm 异步发起，
// 都没有 traceparent。反过来 Deduct / TryFreeze / ConfirmFreeze 属正向段。
var compensationBranches = map[string]bool{
	model.DtmReleaseBranch: true,
	model.DtmCancelFreeze:  true,
}

func isCompensation(branch string) bool { return compensationBranches[branch] }

// dtmMeta 取 dtm server 随回调带上的 metadata。
//
// 走官方 dtmgimp.GetDtmMetaFromContext（它内部补 "dtm-" 前缀），而不是自己拼
// metadata.NewOutgoingContext 之类的手写键：前缀写错的表现是「gid 读出来是空串」，
// 而按 gid 检索不到任何分支时没人会想到是键名少了一段。
func dtmMeta(ctx context.Context, key string) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	return firstValue(md, "dtm-"+key)
}

func firstValue(md metadata.MD, key string) string {
	if v := md.Get(key); len(v) > 0 {
		return v[0]
	}
	return ""
}
