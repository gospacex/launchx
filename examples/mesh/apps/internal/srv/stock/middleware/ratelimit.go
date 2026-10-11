// Package middleware 应用侧兜底：RPC 跳的固定窗口限流。
//
// 为什么这层删不掉（两道独立理由，任一成立都留下它）：
//  1. bff→srv 走 Kitex 默认 TTHeader/TCP，Istio 对该端口按 tcp_proxy 处理 ——
//     Envoy 只看得见连接、看不见请求，「按请求限流」交不出去；
//  2. 它按 user_id / operator_id 计数，是**业务维度** —— 就算换成 gRPC 让 Envoy
//     读到请求，网格也认不出这两个字段属于谁。
package middleware

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/gospacex/kvx"
	"github.com/gospacex/logx"
	"github.com/gospacex/otelx/layer"
	"go.opentelemetry.io/otel/attribute"
)

const (
	// rateLimitPerMinute 每主体每分钟上限。
	//
	// 库存域取 180：介于订单（120）与纯读的商品域（300）之间 —— 库存查询在读路径
	// 高频（商品列表/详情都要读库存），故不宜像积分那样紧；但扣减库存属关键写、
	// 直接关联超卖，又不能像纯读的商品域那样宽。
	rateLimitPerMinute = 180
	rateLimitWindow    = time.Minute
	// namespace 计数键前缀，避免与其它域的计数互相污染（同 redis 实例）。
	namespace = "mesh:stock:rl"
)

// RateLimiter 固定窗口限流器。KV 为 nil 时**放行**（fail-open）并打 Warn。
type RateLimiter struct {
	store  kvx.KVStore
	logger logx.Logger
	limit  int64
	window time.Duration
}

// New 构造限流器；store 允许为 nil（表示 KV 不可用）。
func New(store kvx.KVStore, logger logx.Logger) *RateLimiter {
	return &RateLimiter{store: store, logger: logger, limit: rateLimitPerMinute, window: rateLimitWindow}
}

// Allow 判断该主体本轮是否放行。
//
// 失败姿态是**放行**而不是拒绝：限流是自保护，不该因为计数后端抖动就把正常
// 请求打成 429（那等于把限流器自己变成故障源）。但必须打 Warn ——
// 「限了却被打穿且无痕可查」是最难排查的形态。
//
// 自带 span（2026-10-10 加固）：返回 429 时，Jaeger 上要能一眼看出**拒在哪一层**
// 以及**按谁计的数**。span 恒定开启（与 bff 的 routing.rate_limit 同口径）。
func (r *RateLimiter) Allow(ctx context.Context, subject string) bool {
	ctx, span := layer.Start(ctx, "middleware", "stock", "RateLimit")
	defer span.End()
	span.SetAttributes(attribute.String("rate_limit.subject", subject))

	if r == nil || r.store == nil || subject == "" {
		span.SetAttributes(attribute.Bool("rate_limit.degraded", true))
		r.warn("mesh-stock: rate limiter unavailable, request allowed",
			logx.String("subject", subject),
			logx.String("consequence", "本轮不受限流保护"),
			logx.String("probe", "检查 xmiddleware.kvx 段"))
		return true
	}
	key := fmt.Sprintf("%s:%s:%d", namespace, subject, time.Now().Unix()/int64(r.window.Seconds()))
	n, err := r.store.Incr(ctx, key)
	if err != nil {
		span.SetAttributes(attribute.Bool("rate_limit.degraded", true))
		r.warn("mesh-stock: rate limit counter failed, request allowed",
			logx.String("subject", subject), logx.Err(err))
		return true
	}
	allowed := n <= r.limit
	span.SetAttributes(attribute.Int64("rate_limit.count", n),
		attribute.Int64("rate_limit.limit", r.limit),
		attribute.Bool("rate_limit.allowed", allowed))
	if n == 1 {
		// 首次计数才设过期：每次 Incr 都 Expire/Set 会让窗口永不滚动。
		//
		// 现场核对结论：kvx.KVStore **没有 Expire**，带 TTL 的原语只有 Set，
		// 故这里用 Set 写回同一计数值并顺带设窗口 TTL。n==1 表示键刚由本次
		// Incr 建立，Set 只为「给窗口键挂一个随窗口滚动的过期」，避免键无限堆积。
		// Set 会覆写值：并发首写时最多把这轮的少量并发计数抹回 1，属 fail-open
		// 姿态下的可接受偏差，下个窗口自愈。
		if err := r.store.Set(ctx, key, strconv.FormatInt(n, 10), r.window); err != nil {
			r.warn("mesh-stock: set rate limit ttl failed", logx.Err(err))
		}
	}
	return allowed
}

// warn 统一收口 logger 判空：New 允许传 nil logger，装配期不该因日志缺失而 panic。
func (r *RateLimiter) warn(msg string, fields ...logx.Field) {
	if r != nil && r.logger != nil {
		r.logger.Warn(msg, fields...)
	}
}
