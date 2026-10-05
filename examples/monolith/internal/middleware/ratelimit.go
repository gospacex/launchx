package middleware

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Counter 窗口计数器（由缓存侧的实现注入）。
//
// 之所以用固定窗口而不是滑动窗口 / 令牌桶：限流是本 demo 的「有外部依赖的
// 中间件」样本，策略精度不是重点，重点是可观测 —— 一次计数必须在链路上留下
// 一次 cache 调用，否则这条链上就少了一个断口。
type Counter interface {
	// Incr 对 key 自增并返回本窗口当前值；ttl 为窗口长度。
	Incr(ctx context.Context, key string, ttl time.Duration) (int64, error)
}

// RateLimit 固定窗口限流中间件。
type RateLimit struct {
	counter Counter
	limit   int64
	window  time.Duration
	tr      trace.Tracer
}

// NewRateLimit 构造限流中间件。limit/window 由配置注入。
func NewRateLimit(counter Counter, limit int64, window time.Duration, tr trace.Tracer) *RateLimit {
	return &RateLimit{counter: counter, limit: limit, window: window, tr: tr}
}

// Handle 按客户端 IP 计数，超限即 429。
//
// 计数失败时**放行**（fail-open）：限流组件挂了不该让全站不可用，
// 但必须留在 span 上 —— 放行 + 无痕 = 事后无法解释"那阵子为什么没限住"。
func (m *RateLimit) Handle(ctx context.Context, c *app.RequestContext) {
	ctx, span := m.tr.Start(ctx, spanPrefix+"RateLimit")
	defer span.End()

	ip := c.ClientIP()
	bucket := strconv.FormatInt(time.Now().Unix()/int64(m.window.Seconds()), 10)
	n, err := m.counter.Incr(ctx, fmt.Sprintf("rate:%s:%s", ip, bucket), m.window)
	span.SetAttributes(
		attribute.String("ratelimit.key", ip),
		attribute.Int64("ratelimit.limit", m.limit),
		attribute.Int64("ratelimit.count", n),
	)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		c.Next(ctx) // 组件不可用 → 放行，错误已在 span 上
		return
	}
	if m.limit > 0 && n > m.limit {
		span.SetStatus(codes.Error, "ratelimit: exceeded")
		fail(span, c, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}
	c.Next(ctx)
}
