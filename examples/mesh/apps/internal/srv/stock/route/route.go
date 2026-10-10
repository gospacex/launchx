// Package route 内层：身份闸门（Auth）→ 业务维度限流。
// 两层都在 handler 之前，被拒绝的请求不进入业务判断。
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
func (g *Guard) Admit(ctx context.Context, id routing.Identity) bool {
	subject := "svc"
	if id.UserID != 0 {
		subject = "u" + strconv.FormatUint(id.UserID, 10)
	}
	return g.limiter.Allow(ctx, subject)
}
