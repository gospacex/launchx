package middleware

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/gospacex/launchx/examples/monolith/internal/tracex"
)

// Counter 窗口计数器（由 internal/ratelimit 注入 kvx 实现）。
//
// 之所以用固定窗口而不是滑动窗口 / 令牌桶：限流是本 demo 的「有外部依赖的
// 中间件」样本，策略精度不是重点，重点是可观测 —— 一次计数必须在链路上留下
// 一次 KV 调用（db.kvx.incr span），否则这条链上就少了一个断口。
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
//
// 两种「标 Error」的语义不同，靠属性区分而不是靠状态码：
//   - 计数失败 = 依赖故障（fail-open 放行），属性 ratelimit.degraded=true
//   - 超限 = 主动拒绝，属性 ratelimit.exceeded=true
//     混在一起的话，「限流没生效」和「限流生效了但被打爆」在错误率上长得一样。
func (m *RateLimit) Handle(ctx context.Context, c *app.RequestContext) {
	ctx, end := begin(m.tr, ctx, "RateLimit")
	var err error
	defer func() { end(err) }()

	ip := c.ClientIP()
	bucket := strconv.FormatInt(time.Now().Unix()/int64(m.window.Seconds()), 10)
	n, cerr := m.counter.Incr(ctx, fmt.Sprintf("rate:%s:%s", ip, bucket), m.window)
	tracex.Attrs(ctx,
		attribute.String("ratelimit.key", ip),
		attribute.Int64("ratelimit.limit", m.limit),
		attribute.Int64("ratelimit.count", n),
	)
	if cerr != nil {
		err = cerr
		tracex.Attrs(ctx, attribute.Bool("ratelimit.degraded", true))
		c.Next(ctx) // 组件不可用 → 放行，错误已在 span 上
		return
	}
	if m.limit > 0 && n > m.limit {
		err = errors.New("ratelimit: exceeded")
		tracex.Attrs(ctx, attribute.Bool("ratelimit.exceeded", true))
		fail(c, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}
	c.Next(ctx)
}
