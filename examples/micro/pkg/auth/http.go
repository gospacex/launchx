package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/gospacex/logx"
	"go.opentelemetry.io/otel/attribute"

	"github.com/gospacex/otelx/layer"
)

// AuthorizationHeader 凭据头。用 RFC 7235 的标准头而不是自定义头：
// 标准头会被网关、CDN、APM 与各类 SDK 原样透传与识别，自定义头则要在每一跳
// 显式加白名单 —— 少配一处就表现为"过了某个中间件之后鉴权突然全挂"。
const AuthorizationHeader = "Authorization"

// BearerScheme 凭据方案（大小写不敏感比对，见 bearerToken）。
const BearerScheme = "Bearer"

// Verifier 凭据校验器。由装配层注入（实现基于缓存），本包不认识任何存储。
//
// 为什么是接口而不是直接吃一个 *TokenStore：鉴权中间件要能被单测直接驱动，
// 而 *TokenStore 需要一个真的 cachex 实例。接口让"校验通过 / 校验失败 /
// 缓存报错"三条路径在单测里都能被精确构造出来。
type Verifier interface {
	Resolve(ctx context.Context, token string) (int64, bool)
}

// Config 中间件参数。
//
// 全部由装配层注入，本包不读配置、不设默认值里的地址或阈值 ——
// 与其余分层的纪律一致：会随环境变的东西一律不写死在代码里。
type Config struct {
	Verifier Verifier               // 凭据校验器（nil 时中间件不注册，见装配层）
	Logger   logx.Logger            // 可为 nil（只写 span 属性，不落日志）
	TraceOn  bool                   // 分层 span 开关（trace.enabled）
	TTL      time.Duration          // 签发的身份票据有效期；<=0 回落 DefaultTicketTTL
	Skip     func(path string) bool // 免鉴权路径（探针 / 存活判据）
}

// Middleware 边界鉴权中间件：验凭据 → 签身份 → 透传给下游 RPC。
//
// 只装在 **HTTP 边界**（bff）。四个域服走 RPC，由 pkg/routing 的鉴权闸门
// 校验透传下来的身份（见 Gate），两边共用同一个 Identity，但入口形态不同。
//
// 失败一律 401 并中止后续 handler，不 fallback 到"没身份也能过"：
// 鉴权中间件的 fail-open 等于没有鉴权，而且它不像限流那样有"放开压力"的
// 正当理由 —— 这里放过去的不是流量，是越权。
func Middleware(cfg Config) app.HandlerFunc {
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = DefaultTicketTTL
	}
	return func(ctx context.Context, c *app.RequestContext) {
		if cfg.Skip != nil && cfg.Skip(string(c.Path())) {
			c.Next(ctx)
			return
		}

		ctx, end := layer.Begin(ctx, "middleware", "auth", "VerifyToken", cfg.TraceOn, nil)
		var err error
		defer func() { end(err) }()

		token := bearerToken(c)
		if token == "" {
			err = errors.New("auth: missing bearer token")
			unauthorized(c, "missing bearer token")
			return
		}
		layer.Attrs(ctx, attribute.Bool("auth.token_present", true))

		userID, ok := cfg.Verifier.Resolve(ctx, token)
		if !ok {
			// 校验失败不是业务异常，但必须标成 span 错误：
			// 按 Error 扫 Jaeger 时，被拒的请求要能被看见 —— 否则鉴权被绕过多少次
			// 链路上一点痕迹都没有，而"被绕过"恰恰是最该被发现的那一类事件。
			err = errors.New("auth: invalid token")
			unauthorized(c, "invalid token")
			return
		}

		id := New(userID, operatorOf(c), time.Now())
		layer.Attrs(ctx,
			attribute.Int64("auth.user_id", id.UserID),
			attribute.Int64("auth.operator_id", id.OperatorID),
			attribute.String("auth.issuer", id.Issuer),
		)
		if cfg.Logger != nil {
			cfg.Logger.DebugContext(ctx, "auth: identity issued", logx.String("identity", id.String()))
		}

		// 身份挂回 ctx：本进程的后续分层读得到，kitex 也会把它序列化给下游。
		c.Next(WithIdentity(ctx, id))
	}
}

// bearerToken 从 Authorization 头取凭据。
//
// 空头与"有头但没有 Bearer 方案"返回同一个空串：两者对调用方后果一致
// （这次请求不放行）。细分错误只会让 401 的响应体带上"你的头格式错在哪"，
// 那是给攻击者看的调试信息。
func bearerToken(c *app.RequestContext) string {
	raw := strings.TrimSpace(string(c.GetHeader(AuthorizationHeader)))
	if raw == "" {
		return ""
	}
	scheme, value, found := strings.Cut(raw, " ")
	if !found || !strings.EqualFold(scheme, BearerScheme) {
		return ""
	}
	return strings.TrimSpace(value)
}

// operatorOf 取操作者标识（写入口要求它）。
//
// 取自查询串的 operator_id：demo 没有独立的运营身份体系，操作者由调用方在
// 请求里声明，边界层只负责把它**原样封进身份**而不做权限判定 ——
// "这个操作者能不能改这个商品"是业务授权，不是鉴权，混在一起会让中间件
// 开始读业务字段，链子就散了。
func operatorOf(c *app.RequestContext) int64 {
	raw := strings.TrimSpace(c.Query("operator_id"))
	if raw == "" {
		return 0
	}
	var n int64
	for _, r := range raw {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int64(r-'0')
	}
	return n
}

// unauthorized 渲染 401。带上 WWW-Authenticate 是 RFC 7235 的要求：
// 少了它，客户端不知道该用什么方案重试，只能盲试。
func unauthorized(c *app.RequestContext, msg string) {
	c.Header("WWW-Authenticate", `Bearer realm="demo-bff"`)
	c.AbortWithMsg(msg, http.StatusUnauthorized)
}
