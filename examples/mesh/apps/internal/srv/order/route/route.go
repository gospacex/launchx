// Package route 内层：**身份主体分桶** + 业务维度限流。
// 它在 handler 之前，被限流拒绝的请求不进入业务判断。
//
// ⚠️ 口径勘误（2026-10-10 code review §5.9）：本包**不做身份裁决**。此前包注释写
// 「身份闸门（Auth）」，但 `Guard.Admit` 只按 `id.UserID` 分桶（有身份按 u<id>、
// 匿名按 svc），**既不校验也不拒绝**。真正的**拒绝**发生在 `rpc/`：
//   - 身份头格式坏 / 归属不符 → `rpc.resolveSubject` 回 401 / 403；
//   - 这里只决定「按谁计数、放不放行」→ 超限回 429。
//
// 故包名/方法与注释已按此收窄；「身份闸门」的裁决语义在 rpc 层。
//
// 鉴权口径的减法：传输层身份（「调用方是不是 bff」）下沉给 Istio 的
// AuthorizationPolicy + mTLS；主体解析只认 bff 透传下来的身份（metainfo）。
// micro 的 pkg/auth/auth.go:61 自己就写着「它防不住内网里的恶意调用（那需要 mTLS）」
// —— 那半正是网格补上的。
package route

import (
	"context"
	"strconv"

	"github.com/gospacex/logx"
	"github.com/gospacex/otelx/layer"
	"go.opentelemetry.io/otel/attribute"

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
//
// 自带 span（2026-10-10 加固）：被拒（429）时 Jaeger 上要能看出**是哪一层拒的**。
// 此前 route/middleware 两层一个 span 都没有，限流一旦触发只能从 bff 的 429 反推，
// 排查时会先去怀疑下游业务。
//
// 这里的 span **不接 trace.enabled**：与 bff 侧的 routing.* 三层同口径 ——
// 业务分层（rpc/handler/service/repository）由 trace.enabled 门控，边界骨架层
// 恒定产 span。OFF 档不会因此多出 span：trace.enabled=false 时 telemetry.Init
// 把全局 provider 显式设成 noop，Start 出来的 span 不落地，不必在这里再门控一次。
func (g *Guard) Admit(ctx context.Context, id routing.Identity) bool {
	subject := "svc"
	if id.UserID != 0 {
		subject = "u" + strconv.FormatUint(id.UserID, 10)
	}
	ctx, end := layer.Begin(ctx, "route", "order", "Admit", true, nil)
	defer func() { end(nil) }()
	layer.Attrs(ctx, attribute.String("rate_limit.subject", subject))

	ok := g.limiter.Allow(ctx, subject)
	layer.Attrs(ctx, attribute.Bool("rate_limit.admitted", ok))
	return ok
}
