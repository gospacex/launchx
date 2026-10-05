// Package middleware 业务中间件层：鉴权与限流。
//
// 与 webx 内置中间件（recovery / cors / timeout / ratelimit）的区分：
//   - webx 那些是**框架切面**：无外部依赖、也无业务语义，验收矩阵里列为可豁免；
//   - 本层的两个都有外部依赖（经缓存查凭据 / 计限额），断口在这儿看不见就等于漏，
//     故必须各自开 span 挂在请求根之下。
//
// 两个中间件都**不硬编码**任何地址或阈值：令牌、窗口统统由 setup 注入。
package middleware

import (
	"context"
	"net/http"

	"github.com/cloudwego/hertz/pkg/app"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// 本层 span 前缀。
const spanPrefix = "middleware."

// Token 令牌校验中间件。
//
// 判据是「凭据来自外部」：token → user 的映射走缓存，因此每次校验都会在链路上
// 留下一次 cache 调用。改成本地 map 比对会让这里变成无调用的纯 CPU 操作 —— 那就
// 不该有 span，也不该出现在验收矩阵的"必须入链"栏里。
type Token struct {
	lookup func(ctx context.Context, token string) (int64, bool)
	tr     trace.Tracer
}

// NewToken 构造令牌校验中间件。lookup 由装配层喂（实现基于缓存）。
func NewToken(lookup func(ctx context.Context, token string) (int64, bool), tr trace.Tracer) *Token {
	return &Token{lookup: lookup, tr: tr}
}

// Handle 校验 X-Token：不合法即 401 并中止后续 handler。
//
// 通过时把 user_id 塞进 request context（hertz 的 c.Set），下游 handler 不必再
// 解一次令牌 —— 重复解析等于把"谁在调用"这件事在层间传两遍。
func (m *Token) Handle(ctx context.Context, c *app.RequestContext) {
	ctx, span := m.tr.Start(ctx, spanPrefix+"AuthToken")
	defer span.End()

	token := string(c.GetHeader("X-Token"))
	if token == "" {
		fail(span, c, http.StatusUnauthorized, "missing X-Token")
		return
	}
	span.SetAttributes(attribute.Bool("auth.token_present", true))

	userID, ok := m.lookup(ctx, token)
	if !ok {
		// 校验失败不是「业务错误」，但必须标记为 span 错误：
		// Jaeger 上扫 Error 状态码时，被拒的请求要能被看见。
		span.SetStatus(codes.Error, "auth: invalid token")
		fail(span, c, http.StatusUnauthorized, "invalid token")
		return
	}
	span.SetAttributes(attribute.Int64("auth.user_id", userID))
	c.Set("user_id", userID)
	c.Next(ctx)
}

// fail 渲染错误响应并记录 span 状态。
func fail(span trace.Span, c *app.RequestContext, status int, msg string) {
	span.SetStatus(codes.Error, msg)
	c.AbortWithMsg(msg, status)
}
