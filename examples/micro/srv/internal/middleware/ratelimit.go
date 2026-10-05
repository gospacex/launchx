// Package middleware demo-srv 自保护中间件。
//
// RateLimiter 基于 cachex（Redis）的固定窗口限流（brief）：
//   - 门控：app/setup.go 仅在缓存组件可用时构造并传入
//     （未开缓存时 Limiter 为 nil，入口直接放行）
//   - fail-open：缓存读/写失败（Redis 不可用）一律放行，不打断主链路
//   - 窗口：key = rate:<service>:<user_id>，TTL = window，计数超 limit → 拒绝
//     （cachex 无 Incr 原子操作，Get→+1→Set 非原子；窗口限流的自保护场景
//     允许轻微竞态，优先简单实现，deviation 已记录）
package middleware

import (
	"context"
	"strconv"
	"time"

	"github.com/gospacex/cachex"
	"github.com/gospacex/logx"
)

// RateLimiter cachex 固定窗口限流器
type RateLimiter struct {
	cache  cachex.Cache
	prefix string
	limit  int
	window time.Duration
}

// NewRateLimiter 构造（cache 为 nil 时 Allow 恒 true：门控关闭形态）
func NewRateLimiter(cache cachex.Cache, prefix string, limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{cache: cache, prefix: prefix, limit: limit, window: window}
}

// Allow 判定 userID 在窗口内是否放行。
// 缓存不可用（Get 出错）→ 放行（fail-open，brief 纪律「读不到缓存放行」）。
func (r *RateLimiter) Allow(ctx context.Context, userID int64) bool {
	if r == nil || r.cache == nil || r.limit <= 0 {
		return true
	}
	key := r.prefix + ":" + strconv.FormatInt(userID, 10)
	cur, err := r.cache.Get(ctx, key)
	if err != nil {
		// 缓存读失败 → 放行（限流器故障不能放大为服务不可用）
		return true
	}
	n, _ := strconv.Atoi(cur)
	if n >= r.limit {
		return false
	}
	if err := r.cache.Set(ctx, key, strconv.Itoa(n+1), r.window); err != nil {
		logx.WarnContext(ctx, "ratelimit: counter set failed (fail-open)",
			logx.String("key", key), logx.Err(err))
	}
	return true
}
