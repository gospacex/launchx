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
	"errors"
	"net/http"

	"github.com/cloudwego/hertz/pkg/app"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/gospacex/launchx/examples/monolith/internal/tracex"
)

// 本层 span 前缀。
const spanPrefix = "middleware."

// begin 开本层 span（沿用两段式 middleware.<Op> 命名），返回「收 error 再收尾」的闭包。
//
// 中间件与 handler 都是 hertz 的回调形态（**没有返回值**），拿不到「返回的 err」
// 交给收尾闭包，故在函数体里显式声明一个 err 变量：所有失败分支给它赋值，
// defer 里的 end(err) 才能看到。少了这一步，失败路径就一个都标不上 —— 比不统一更糟。
func begin(tr trace.Tracer, ctx context.Context, op string) (context.Context, tracex.End) {
	return tracex.BeginSpan(tr, ctx, spanPrefix+op, nil)
}

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
	ctx, end := begin(m.tr, ctx, "AuthToken")
	var err error
	defer func() { end(err) }()

	token := string(c.GetHeader("X-Token"))
	if token == "" {
		err = errors.New("auth: missing X-Token")
		fail(c, http.StatusUnauthorized, "missing X-Token")
		return
	}
	tracex.Attrs(ctx, attribute.Bool("auth.token_present", true))

	userID, ok := m.lookup(ctx, token)
	if !ok {
		// 校验失败不是「业务异常」，但必须标成 span 错误：
		// Jaeger 上按 Error 扫时，被拒的请求要能被看见 —— 否则鉴权被绕过多少次，
		// 链路上一点痕迹都没有。
		err = errors.New("auth: invalid token")
		fail(c, http.StatusUnauthorized, "invalid token")
		return
	}
	tracex.Attrs(ctx, attribute.Int64("auth.user_id", userID))
	c.Set("user_id", userID)
	c.Next(ctx)
}

// fail 渲染错误响应。span 状态交给收尾闭包标（本函数不再持 span 句柄）。
func fail(c *app.RequestContext, status int, msg string) {
	c.AbortWithMsg(msg, status)
}
