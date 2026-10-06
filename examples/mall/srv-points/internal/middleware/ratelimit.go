// Package middleware 入口自保护：基于 kvx 的固定窗口限流。
//
// 为什么用 kvx 而不是 cachex：cachex.Cache 只有 Get/Set/Delete（没有原子
// Incr），自保护计数需要原子自增，否则并发下计数会被读-改-写覆盖成低估。
// kvx.KVStore 提供 Incr/Decr，语义正对。两者分工因此是：
//
//	kvx    可自增的键值状态（限流计数）
//	cachex 可失效的派生缓存（积分账户）
//
// 姿态是 fail-open：KV 不可用时 Allow 恒 true。限流器是**保护自己**的手段，
// 缓存挂掉就让限流失效、把压力放进来，也好过把所有请求一起拒掉。
package middleware

import (
	"context"
	"fmt"
	"time"

	"github.com/gospacex/kvx"
)

// RateLimiter 固定窗口限流器（单实例、单 key 前缀）
type RateLimiter struct {
	kv     kvx.KVStore
	prefix string
	limit  int64
	window time.Duration
}

// NewRateLimiter 构造。kv 为 nil（KV 组件降级）或参数非法时返回 nil 表示"不限流"。
func NewRateLimiter(kv kvx.KVStore, prefix string, limit int, window time.Duration) *RateLimiter {
	if kv == nil || limit <= 0 || window <= 0 {
		return nil
	}
	return &RateLimiter{kv: kv, prefix: prefix, limit: int64(limit), window: window}
}

// Allow 判定给定主体在当前窗口内是否放行。
//
// key 带窗口序号，窗口自然滚动。Incr 首次创建 key 时无法带 TTL，故在 n==1
// 时用 Set 覆写同一个值（此时值必然是 "1"）把 TTL 补上 —— 不做这一步，
// 冷用户的计数键会永久堆积在 redis 里。
func (r *RateLimiter) Allow(ctx context.Context, subject int64) bool {
	if r == nil {
		return true
	}
	key := fmt.Sprintf("%s:%d:%d", r.prefix, subject, time.Now().UnixNano()/r.window.Nanoseconds())
	n, err := r.kv.Incr(ctx, key)
	if err != nil {
		return true // fail-open：计数不可用就放行，见包注释
	}
	if n == 1 {
		_ = r.kv.Set(ctx, key, "1", r.window)
	}
	return n <= r.limit
}
