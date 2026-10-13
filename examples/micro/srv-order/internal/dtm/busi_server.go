// Package dtm srv-order 的 dtm 分支面：承接 dtm server 的 Saga/TCC gRPC 回调。
//
// 在分层链上的位置：dtm server → **dtm（分支面）** → service → repository。
// 分支面与 kitex 传输面（internal/rpc → route → handler → service）是两条并列的
// 入口，汇到同一个 service 层 —— 业务规则只有一份，dtm 的幂等由 barrier 裁决，
// kitex 的幂等由 uk_order_no 裁决，两者不互相冒充。
//
// 分支面只做四件事，一件不多：
//  1. 载荷 → 领域 DTO（biz JSON 反序列化；pb 与领域的转换归它，因为只有它认识 pb）
//  2. 从载荷里的 traceparent 续链，开 rpc.dtm.order.<Method> span
//  3. 失败注入点火（演示用）
//  4. 调 service，并把错误翻成 dtm 认识的 grpc status（口径①）
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
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/dtm-labs/client/dtmgrpc"

	"github.com/gospacex/dtmx"
	"github.com/gospacex/launchx/examples/micro/pkg/codes"
	"github.com/gospacex/launchx/examples/micro/srv-order/internal/dtm/protos"
	"github.com/gospacex/launchx/examples/micro/srv-order/internal/model"
	"github.com/gospacex/launchx/examples/micro/srv-order/internal/service"
	"github.com/gospacex/logx"
	"github.com/gospacex/otelx/layer"
)

// 服务全名（= proto package + service 名）。dtm server 按 URL 回调，
// 而 URL 就是 <listen_addr> + 下面任一 FullMethodName；三处必须一致。
const orderBusiServiceName = "dtmbranch.order.OrderBusi"

// BusiServer 订单域 dtm 分支服务。
type BusiServer struct {
	protos.UnimplementedOrderBusiServer

	svc *service.OrderService
	// traceOn 业务分层埋点开关（trace.enabled）。与 kitex 面读同一个键。
	traceOn bool
	logger  logx.Logger
}

// NewBusiServer 构造。
func NewBusiServer(svc *service.OrderService, traceOn bool, logger logx.Logger) *BusiServer {
	if logger == nil {
		logger = logx.GetGlobal()
	}
	return &BusiServer{svc: svc, traceOn: traceOn, logger: logger}
}

// branchWrite 一个分支的 service 方法。
type branchWrite func(ctx context.Context, cmd model.DtmOrderCmd) error

// CreateOrder Saga 正向分支
// （编排 URL：grpc://<listen_addr>/dtmbranch.order.OrderBusi/CreateOrder）
func (s *BusiServer) CreateOrder(ctx context.Context, req *protos.OrderBusiReq) (*protos.OrderBusiResp, error) {
	return s.branch(ctx, model.DtmCreateOrderBranch, req, s.svc.DtmCreateOrder)
}

// CancelOrder Saga 补偿分支（由 dtm 异步发起，载荷里没有 traceparent）
func (s *BusiServer) CancelOrder(ctx context.Context, req *protos.OrderBusiReq) (*protos.OrderBusiResp, error) {
	return s.branch(ctx, model.DtmCancelOrderBranch, req, s.svc.DtmCancelOrder)
}

// TryPlace TCC 一阶段：建 presale_locked 占位单（不写 outbox）
func (s *BusiServer) TryPlace(ctx context.Context, req *protos.OrderBusiReq) (*protos.OrderBusiResp, error) {
	return s.branch(ctx, model.DtmTryPlaceBranch, req, s.svc.DtmTryPlace)
}

// ConfirmFreeze TCC 二阶段：占位单转正式 paid 单（此刻才写 outbox）
func (s *BusiServer) ConfirmFreeze(ctx context.Context, req *protos.OrderBusiReq) (*protos.OrderBusiResp, error) {
	return s.branch(ctx, model.DtmConfirmFreezeBranch, req, s.svc.DtmConfirmFreeze)
}

// CancelFreeze TCC 二阶段：废掉占位单（不写 outbox）
func (s *BusiServer) CancelFreeze(ctx context.Context, req *protos.OrderBusiReq) (*protos.OrderBusiResp, error) {
	return s.branch(ctx, model.DtmCancelFreezeBranch, req, s.svc.DtmCancelFreeze)
}

// branch 五个分支共用的协议转换。
//
// 方法名与分支名**恒等**（busi.proto 里 service 的方法名就是分支名），所以这里
// 只传一个 branch：fail_inject 命中、span 名、biz.dtm.branch 三处共用同一个字面量，
// 编排侧按 URL 里的方法名点火才不会写错。TestFullMethodNamesMatchDtmCallbackURL
// 把「方法名 == 分支名」也钉住了。
func (s *BusiServer) branch(ctx context.Context, branch string, req *protos.OrderBusiReq, write branchWrite) (*protos.OrderBusiResp, error) {
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
		return failResp(err), branchError(err)
	}

	// ③ 载荷 → 领域 DTO。坏载荷按业务失败处理：重试一万次也还是坏，而放过去
	//    就带着零值 order_no 去落库，失败点还会漂到更远的下游。
	var cmd model.DtmOrderCmd
	if jerr := json.Unmarshal([]byte(req.GetBiz()), &cmd); jerr != nil {
		err = dtmx.BusinessFailure(codes.Wrap(codes.CodeInvalidParam, "dtm 分支载荷 biz 不是合法 JSON", jerr))
		return failResp(err), branchError(err)
	}

	// ④ 交棒给 service（barrier、事务、订单状态机都在那一层之下）。
	if werr := write(ctx, cmd); werr != nil {
		err = werr
		return failResp(err), branchError(err)
	}
	return &protos.OrderBusiResp{Code: codes.CodeOK, Message: "ok"}, nil
}

// begin 开 rpc.dtm.order.<Method> span（layer.Begin 拼成 layerName.entity.op）。
//
// 不设 quiet 豁免：分支返回 error 一定意味着这次账变没完成，入链才算看得见。
func (s *BusiServer) begin(ctx context.Context, method string) (context.Context, layer.End) {
	return layer.Begin(ctx, "rpc", "dtm.order", method, s.traceOn, nil)
}

// branchError 把 service 错误翻成 **dtm 真正会看的那一位**：gRPC status（口径①）。
//
// 为什么必须翻而不是直接 return err：
//   - dtm server 侧（dtmsvr/trans_status.go::getGrpcResult）把分支的响应字节
//     整个塞进 *[]byte，**从不解析** OrderBusiResp.code —— 所以「响应体里写上
//     3003」对 dtm 没有任何意义；
//   - dtm 客户端侧（dtmgrpc/type.go::GrpcError2DtmError）只看 status：
//     codes.Aborted → 业务失败（不重试，转而补偿），
//     codes.FailedPrecondition + ONGOING → 仍在进行中，
//     其余（含 Unknown / Internal）→ 基础失败，按退避重试。
//
// 于是分流判据是「错误是不是带 dtm 官方哨兵」：带（IsBusinessFailure）为业务失败
// → Aborted；不带就是基础设施故障 → Internal。
//
// 为什么基础设施故障用 Internal 而不是让 grpc-go 编成 Unknown：两者在 dtm 侧的
// 行为完全相同（都不是 Aborted，都会重试），但日志与抓包上 Internal 才说明得出
// 「是本域自己判的失败」，而 Unknown 在排查时几乎总是被误读成「回调根本没到」。
func branchError(err error) error {
	if err == nil {
		return nil
	}
	if dtmx.IsBusinessFailure(err) {
		return dtmgrpc.DtmError2GrpcError(err)
	}
	return status.Error(grpccodes.Internal, err.Error())
}

// failResp 把错误翻成响应体的 code/message。
//
// **给人和排障看**，dtm server 并不读这两个字段（它只看 grpc status，见上面的
// branchError）。留着它们是为了让「分支日志里 code=3003」与 kitex 面的响应体口径
// 一致 —— 否则同一个拒绝在两条链上会报成两个码。
func failResp(err error) *protos.OrderBusiResp {
	be, ok := codes.As(err)
	if !ok {
		return &protos.OrderBusiResp{Code: codes.CodeInternal, Message: "dtm branch failed"}
	}
	return &protos.OrderBusiResp{Code: be.Code, Message: be.Message}
}

// isCompensation 该分支是不是逆向分支。
//
// CancelOrder 是 Saga 补偿、CancelFreeze 是 TCC 的回退；两者都由 dtm 异步发起，
// 都没有 traceparent。反过来 CreateOrder / TryPlace / ConfirmFreeze 属正向段。
var compensationBranches = map[string]bool{
	model.DtmCancelOrderBranch:  true,
	model.DtmCancelFreezeBranch: true,
}

func isCompensation(branch string) bool { return compensationBranches[branch] }

// dtmMeta 取 dtm server 随回调带上的 metadata。
//
// 键名带 "dtm-" 前缀是官方约定（dtmgrpc/dtmgimp 的 const dtmpre = "dtm-"，
// TransBaseFromGrpc 就是按它取的）。自己拼一个不带前缀的键，读出来会是空串 ——
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
