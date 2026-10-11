// Package auth mall 示例的 **HTTP 边界鉴权**（bff 档专用）。
//
// 为什么必须有它：bff 是四个域服唯一的 HTTP 入口，而 user_id 原本直接取
// 自请求参数（query / path / body）—— 等于把"我是谁"交给调用方声明，任何人
// 都能改一个数字读到别人的订单与积分账户（IDOR）。四个域服走 RPC，它们只
// 能信上游送来的身份，因此**边界层是唯一能验凭据的地方**，这一层漏了，
// 下游再怎么校验都是形式。
//
// 与 examples/micro 的 pkg/auth 是同口径的两个实现（键名 auth.require /
// demo.auth_token / demo.seed_user_id 一致），但**刻意不 import 它**：
// 两个示例必须零耦合，各自的失败姿态与装配方式允许各自演进。
//
// 三条裁决规则：
//  1. 无凭据 / 错令牌 → 401（fail-closed：鉴权中间件没有 fail-open 的正当理由，
//     它放过去的不是流量，是越权）
//  2. 有凭据但请求里自带的 user_id 与身份不符 → 403（不允许调用方自带身份）
//  3. 一致 → 以身份为准**覆盖**请求参数里的 user_id，再交给 handler
//
// 配置缺省按「要求鉴权」处理（安全默认值）：只有显式 auth.require=false
// 才关闭，且关闭时留 Warn。
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/gospacex/logx"
	"go.opentelemetry.io/otel/attribute"

	"github.com/gospacex/otelx/layer"
)

const (
	// AuthorizationHeader 凭据头。用 RFC 7235 的标准头而不是自定义头：
	// 标准头会被网关、CDN、APM 与各类 SDK 原样透传与识别，自定义头则要在
	// 每一跳显式加白名单 —— 少配一处就表现为"过了某个中间件之后鉴权突然全挂"。
	// mall 的 srv-gateway 转发时逐跳头只剔 Connection/Proxy-* 一类，
	// Authorization 会原样透到 bff，故网关侧不需要再验一次。
	AuthorizationHeader = "Authorization"

	// BearerScheme 凭据方案（大小写不敏感比对，见 bearerToken）
	BearerScheme = "Bearer"

	// userIDParam 请求里声明身份的参数名（query / path / JSON body 同名）
	userIDParam = "user_id"
)

// Identity 边界层签发的身份。
//
// 只带 UserID：mall 的 demo 没有独立的运营身份体系，operator_id 那一维留给
// 接入真实 IdP 时再加 —— 现在就把它设计进去，会逼着每个调用点都传一个永远
// 为 0 的字段。
type Identity struct {
	UserID int64
}

type identityKey struct{}

// WithIdentity 把身份挂回 ctx（本进程后续分层读得到）
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// IdentityFrom 取身份。未鉴权（或 auth.require=false 的放行路径）返回 false，
// 调用方据此回落到"匿名"，不得把它当成 0 号用户去查数据。
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok && id.UserID > 0
}

// Config 中间件参数（全部由装配层注入，本包不读配置、不设默认值）。
type Config struct {
	// Tokens 凭据表：token → user_id。空表时中间件拒绝所有请求（401）。
	Tokens map[string]int64
	// Logger 可为 nil（只写 span 属性，不落日志）
	Logger logx.Logger
	// TraceOn 分层 span 开关（trace.enabled）
	TraceOn bool
	// Skip 免鉴权路径判定（探针 / 存活判据 / 指标口）
	Skip func(path string) bool
}

// Middleware 边界鉴权中间件：验凭据 → 校验请求身份 → 覆盖 user_id → 放行。
//
// 被拒一律中止后续 handler，不 fallback 到"没身份也能过"。
func Middleware(cfg Config) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		path := normalizePath(c)
		if cfg.Skip != nil && cfg.Skip(path) {
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
		userID, ok := cfg.Tokens[token]
		if !ok {
			// 校验失败不是业务异常，但必须标成 span 错误：按 Error 扫 Jaeger 时
			// 被拒的请求要能被看见 —— 否则鉴权被试探了多少次链路上一点痕迹都没有，
			// 而"有人在试"恰恰是最该被发现的那一类事件。
			err = errors.New("auth: invalid token")
			unauthorized(c, "invalid token")
			return
		}
		layer.Attrs(ctx, attribute.Int64("auth.user_id", userID))

		// 请求自带身份与凭据不符 → 403。注意"请求里没带 user_id"是合法的
		// （此时一律以身份为准），只有**带了且不一致**才是越权尝试。
		if want, claimed := requestedUserID(c); claimed && want != userID {
			err = fmt.Errorf("auth: identity %d does not match requested %s %d", userID, userIDParam, want)
			forbidden(c, userIDParam+" does not match the authenticated identity")
			return
		}

		// 身份以凭据为准写回请求参数：handler 无论从 query 还是 path 取值，
		// 拿到的都是验过的身份，而不是调用方自己声明的那个数字。
		overrideUserID(c, userID)

		if cfg.Logger != nil {
			cfg.Logger.DebugContext(ctx, "auth: identity accepted",
				logx.String("path", path),
				logx.Int("user_id", int(userID)))
		}
		c.Next(WithIdentity(ctx, Identity{UserID: userID}))
	}
}

// bearerToken 从 Authorization 头取凭据。
//
// 空头与"有头但不是 Bearer 方案"返回同一个空串：两者对调用方后果一致
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

// requestedUserID 取请求里声明的 user_id（query → path → JSON body）。
//
// 三个来源都看是因为本 bff 三种入参形态都有（GET 走 query/path，POST 下单
// 走 JSON body）；漏掉 body 那一路等于给 PlaceOrder 留一个 IDOR 口子 ——
// 那正是最值钱的一个口子（能用别人的身份下单）。
//
// 返回值 claimed 表示"调用方确实声明了身份"。声明了但解析不出正整数的，
// 按"声明了一个必然与身份不符的值"处理（回 0），由调用方判成不符。
func requestedUserID(c *app.RequestContext) (userID int64, claimed bool) {
	for _, raw := range []string{strings.TrimSpace(c.Query(userIDParam)), strings.TrimSpace(c.Param(userIDParam))} {
		if raw == "" {
			continue
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 {
			return 0, true
		}
		return n, true
	}
	// body 只在有内容时看一眼，且只解这一个字段：中间件不该因为一个畸形
	// body 就把请求打回去（那是 handler 的 BindJSON 该报的 400）。
	if body := c.Request.Body(); len(body) > 0 {
		var probe struct {
			UserID int64 `json:"user_id"`
		}
		if json.Unmarshal(body, &probe) == nil && probe.UserID > 0 {
			return probe.UserID, true
		}
	}
	return 0, false
}

// overrideUserID 用验过的身份覆盖请求参数里的 user_id。
//
// 覆盖而不是"让 handler 自己去 ctx 里取"，是为了让**漏改的 handler 也安全**：
// 即便某个 handler 忘了读身份，它从 query/path 拿到的也是被覆盖后的值。
// JSON body 无法就地改写（改写要重新序列化整个 body，代价与风险都不划算），
// 那一路由 handler 显式读 ctx 里的身份兜住。
func overrideUserID(c *app.RequestContext, userID int64) {
	v := strconv.FormatInt(userID, 10)
	c.Request.URI().QueryArgs().Set(userIDParam, v)
	for i := range c.Params {
		if c.Params[i].Key == userIDParam {
			c.Params[i].Value = v
		}
	}
}

// normalizePath 规整路径（去掉结尾斜杠），让 Skip 的判定不被 "/metrics/" 绕开。
func normalizePath(c *app.RequestContext) string {
	p := string(c.Path())
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	return p
}

// unauthorized 渲染 401。带 WWW-Authenticate 是 RFC 7235 的要求：
// 少了它，客户端不知道该用什么方案重试，只能盲试。
func unauthorized(c *app.RequestContext, msg string) {
	c.Header("WWW-Authenticate", `Bearer realm="mall-bff"`)
	c.AbortWithMsg(msg, http.StatusUnauthorized)
}

// forbidden 渲染 403：凭据有效，但它指向的主体与请求里的 user_id 不是同一个。
//
// 与 401 分开是刻意的：401 是"没带对凭据"（客户端重试换令牌即可），
// 403 是"拿 A 的令牌去访问 B 的数据"（重试没用，且是安全事件）——
// 混成一个码会让告警侧分不出"配置写错"与"有人在越权"。
func forbidden(c *app.RequestContext, msg string) {
	c.AbortWithMsg(msg, http.StatusForbidden)
}
