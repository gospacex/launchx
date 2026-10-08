package auth

import (
	"context"
	"time"

	"github.com/gospacex/logx"
)

// Gate 域服侧的鉴权闸门：校验边界层透传下来的身份，不重复验签。
//
// 它实现 pkg/routing 需要的 Authenticator 接口（routing 只认那个接口，
// 两个包因此互不 import —— 鉴权方案换掉时 routing 一行都不用动）。
//
// 为什么只校验"存在且合法"而不查凭据：见包注释的信任边界。内网这一段的可信性
// 由网络与部署保证；在这里再查一次缓存，等于把一次鉴权放大成五份（四个域服各
// 一次 + 边界一次），且任何一处缓存抖动都会让整个系统同时 401。
type Gate struct {
	Logger logx.Logger // 可为 nil
	TTL    time.Duration
}

// Authenticate 判定本次调用是否放行，并返回身份的可读形式（进日志与 span 属性）。
//
// 返回 ok=false 时 routing 会短路后续链路（含限流与业务），
// 由 rpc 层翻成 CodeUnauthorized 响应码。
func (g Gate) Authenticate(ctx context.Context) (string, bool) {
	ttl := g.TTL
	if ttl <= 0 {
		ttl = DefaultTicketTTL
	}
	id, ok := IdentityFrom(ctx)
	if !ok {
		if g.Logger != nil {
			g.Logger.WarnContext(ctx, "auth: no identity on inbound rpc",
				logx.String("consequence", "request rejected"),
				logx.String("hint", "calls must come from the edge (bff), not directly"))
		}
		return "", false
	}
	if !id.Valid(time.Now(), ttl) {
		if g.Logger != nil {
			g.Logger.WarnContext(ctx, "auth: identity rejected",
				logx.String("identity", id.String()),
				logx.String("issuer", id.Issuer),
				logx.String("issued_at", time.Unix(id.IssuedAt, 0).UTC().Format(time.RFC3339)))
		}
		return "", false
	}
	return id.String(), true
}
