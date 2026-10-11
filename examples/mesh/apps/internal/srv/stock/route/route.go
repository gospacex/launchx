// Package route 内层：**身份主体分桶** + 业务维度限流。
// 它在 handler 之前，被限流拒绝的请求不进入业务判断。
//
// ⚠️ 口径勘误（2026-10-10 code review §5.9）：本包**不做身份裁决** ——
// `Guard.Admit` 只按 `id.UserID` 分桶，既不校验也不拒绝；真正的身份/归属拒绝在
// `rpc/`（401 / 403），这里只决定「按谁计数、放不放行」（超限 429）。
//
// 鉴权口径的减法（与 order / points / product 域同款）：传输层身份（「调用方
// 是不是 bff」）下沉给 Istio 的 AuthorizationPolicy + mTLS；本层只校验 bff
// 透传下来的身份主体。
//
// **本域的限流为什么必须是真的**：对外 RPC 面确实只读（IDL 里唯一接线的是
// GetStock，其余六个方法在 handler 显式 Unimplemented），但读路径同样打 DB；
// 库存查询在读路径高频（商品列表 / 详情都要读库存），无上限的读本身就是经典的
// 滥用面。而这一跳走 Kitex TTHeader/TCP，Envoy 只按 tcp_proxy 处理、拿不到 L7，
// middleware 的限流是本域在这一跳上**唯一**的治理手段。写路径 Decrease 只由单测
// 驱动、不经本闸门，但这不构成「读路径就不限」的理由。
package route

import (
	"context"
	"strconv"

	"github.com/gospacex/logx"
	"github.com/gospacex/otelx/layer"
	"go.opentelemetry.io/otel/attribute"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/stock/middleware"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/routing"
)

// Guard 身份闸门 + 限流。
//
// limiter 由装配层喂入本域 middleware 的 180/分钟上限（见 middleware/ratelimit.go）。
type Guard struct {
	limiter *middleware.RateLimiter
	logger  logx.Logger
}

// New 构造闸门。
func New(limiter *middleware.RateLimiter, logger logx.Logger) *Guard {
	return &Guard{limiter: limiter, logger: logger}
}

// Admit 返回是否放行；subject 为空表示无身份（按服务级计数）。
//
// 恒真会让 route/ 退化成空目录、RateLimited 变成死码 —— 这里**真的**把请求送进
// limiter，由它按固定窗口判超限。
//
// 自带 span（2026-10-10 加固）：被拒（429）时 Jaeger 上要能看出**是哪一层拒的**。
// 这里的 span **不接 trace.enabled**（与 bff 侧 routing.* 三层同口径）：业务分层
// 由 trace.enabled 门控，边界骨架层恒定产 span；OFF 档 telemetry.Init 会把全局
// provider 设成 noop，Start 的 span 不落地。
func (g *Guard) Admit(ctx context.Context, id routing.Identity) bool {
	subject := "svc"
	if id.UserID != 0 {
		subject = "u" + strconv.FormatUint(id.UserID, 10)
	}
	ctx, end := layer.Begin(ctx, "route", "stock", "Admit", true, nil)
	defer func() { end(nil) }()
	layer.Attrs(ctx, attribute.String("rate_limit.subject", subject))

	ok := g.limiter.Allow(ctx, subject)
	layer.Attrs(ctx, attribute.Bool("rate_limit.admitted", ok))
	return ok
}
