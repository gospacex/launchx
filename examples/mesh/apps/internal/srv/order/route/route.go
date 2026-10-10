// Package route 内层：身份闸门（Auth）→ 业务维度限流。
// 两层都在 handler 之前，被拒绝的请求不进入业务判断。
//
// 鉴权口径的减法：传输层身份（「调用方是不是 bff」）下沉给 Istio 的
// AuthorizationPolicy + mTLS；本层只校验 bff 透传下来的身份主体。
// micro 的 pkg/auth/auth.go:61 自己就写着「它防不住内网里的恶意调用（那需要 mTLS）」
// —— 那半正是网格补上的。
package route

import (
	"context"
	"strconv"

	"github.com/gospacex/logx"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/middleware"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/routing"
)

// Guard 身份闸门 + 限流。
type Guard struct {
	limiter *middleware.RateLimiter
	logger  logx.Logger
}

// New 构造闸门。
func New(limiter *middleware.RateLimiter, logger logx.Logger) *Guard {
	return &Guard{limiter: limiter, logger: logger}
}

// Admit 返回是否放行；subject 为空表示无身份（按服务级计数）。
func (g *Guard) Admit(ctx context.Context, id routing.Identity) bool {
	subject := "svc"
	if id.UserID != 0 {
		subject = "u" + strconv.FormatUint(id.UserID, 10)
	}
	return g.limiter.Allow(ctx, subject)
}
