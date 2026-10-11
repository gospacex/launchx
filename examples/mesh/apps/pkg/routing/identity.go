package routing

import (
	"context"
	"strconv"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/gospacex/cachex"
	"github.com/gospacex/logx"
)

const (
	// AuthorizationHeader 凭据头。与 examples/micro 的 pkg/auth 同名同义
	// （不 import 它：apps 与 micro 必须零耦合）。
	AuthorizationHeader = "Authorization"

	// bearerPrefix 凭据方案前缀。只认 `Bearer <token>`。
	bearerPrefix = "Bearer "

	// TokenKeyPrefix 凭据缓存的键前缀：`token → user_id`。
	//
	// 与 srv 侧的 `mesh:product:detail:` / `mesh:stock:rl` 同在 cachex / kvx
	// 的同一实例里，靠前缀分域 —— 前缀不同即互不干扰。
	TokenKeyPrefix = "mesh:bff:token:"
)

// NewTokenResolver 产出「从 `Authorization: Bearer <token>` 反查主体」的解析器。
//
// **本函数只负责取证，不负责裁决。** 查不到就是零值身份；「零值要不要拒」由
// `WithRequiredIdentity` 在 identityLayer 里决定（见 routing.go 包注释）。
// 拆开是因为这两件事的失败姿态相反：取证要**宽容**（一次缓存抖动不该变成 401），
// 裁决要**严格**（匿名不能下单）。
//
// 三种「取不到」分开处理，是为了让**排查有信号**：
//   - 没带凭据（最常见）：静默匿名 —— 是否被拒由调用方配；
//   - 头格式不对 / 缓存里存的不是数字：Warn —— 这两种都是**配错了**。
//     静默会让「token 播种写错值」变成一个查不出的匿名退化；
//   - 缓存查询报错 / 未命中：Warn —— 前者是一次抖动（装配期已对 bff 的 cache
//     做 fail-fast 自检），后者要说清「这次请求落 svc 桶」，否则「按用户限流
//     不起作用」会被误读成限流器坏了。
//
// store 为 nil 时返回 nil Resolver（identity 层退化为只开 span 的骨架）。
// ⚠️ 此时若上层开了 `WithRequiredIdentity`，所有非豁免路径都会被 401 ——
// 这正是 bff 的 app.Setup 对 store == nil **报错不起**的原因。
func NewTokenResolver(store cachex.Cache, logger logx.Logger) Resolver {
	if store == nil {
		return nil
	}
	warn := func(msg string, fields ...logx.Field) {
		if logger != nil {
			logger.Warn(msg, fields...)
		}
	}
	return func(ctx context.Context, c *app.RequestContext) context.Context {
		raw := strings.TrimSpace(string(c.Request.Header.Peek(AuthorizationHeader)))
		if raw == "" {
			return ctx // 匿名：没带凭据是正常请求，不必出声
		}
		if !strings.HasPrefix(raw, bearerPrefix) {
			warn("identity header is not a Bearer token, treated as anonymous",
				logx.String("header", AuthorizationHeader),
				logx.String("hint", "expected `Bearer <token>`"))
			return ctx
		}
		token := strings.TrimSpace(strings.TrimPrefix(raw, bearerPrefix))
		if token == "" {
			warn("identity header carries an empty token, treated as anonymous",
				logx.String("header", AuthorizationHeader))
			return ctx
		}

		v, err := store.Get(ctx, TokenKeyPrefix+token)
		if err != nil {
			warn("credential lookup failed, treated as anonymous",
				logx.Err(err),
				logx.String("hint", "cachex 抖动：只影响限流分桶，不影响请求成败"))
			return ctx
		}
		v = strings.TrimSpace(v)
		if v == "" {
			warn("credential not found, treated as anonymous",
				logx.String("bucket", "svc"),
				logx.String("hint", "凭据播种见 bff 的 conf `demo.auth_token` → `demo.seed_user_id`（bff app.Setup 启动期写入 cachex）"))
			return ctx
		}
		uid, err := strconv.ParseUint(v, 10, 64)
		if err != nil || uid == 0 {
			warn("credential payload is not a valid user id, treated as anonymous",
				logx.String("payload", v))
			return ctx
		}
		return WithIdentity(ctx, Identity{UserID: uid})
	}
}
