// Package middleware demo-srv 自保护中间件。
//
// RateLimiter 基于 kvx（Redis）的固定窗口限流（brief）：
//   - 门控：app/setup.go 仅在 KV 组件可用时构造并传入
//     （未装配时 Limiter 为 nil，入口直接放行）
//   - fail-open：KV 读/写失败（Redis 不可用）一律放行，不打断主链路
//   - 窗口：key = rate:<service>:<user_id>:<窗口序号>，计数超 limit → 拒绝
//
// 为什么从 cachex 迁到 kvx：cachex.Cache 只有 Get/Set，自保护计数只能做成
// 「读出来 +1 再写回」，并发下同一 key 的计数会互相覆盖（lost update）成
// 低估 —— 单实例是偶发偏差，多实例并发下误差随压力线性放大，最后表现为
// 「明明限了却还是被打穿」。kvx.KVStore 提供原子 Incr，语义正对。
//
// 分工因此是（与 examples/mall 保持一致）：
//
//	kvx    可自增的键值状态（限流计数、幂等键）
//	cachex 可失效的派生缓存（订单详情、令牌映射）
package middleware

import (
	"context"
	"fmt"
	"time"

	"github.com/gospacex/kvx"
)

// RateLimiter kvx 固定窗口限流器
type RateLimiter struct {
	kv     kvx.KVStore
	prefix string
	limit  int64
	window time.Duration
}

// NewRateLimiter 构造（kv 为 nil 时 Allow 恒 true：门控关闭形态）
func NewRateLimiter(kv kvx.KVStore, prefix string, limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{kv: kv, prefix: prefix, limit: int64(limit), window: window}
}

// Allow 判定 userID 在窗口内是否放行。
//
// key 带窗口序号，窗口自然滚动（不必靠过期划 curly boundary）。
// Incr 首建 key 时带不上 TTL，故在 n==1 时用 Set 覆写同一个值把 TTL 补上
// （此刻值必然是 1，覆写不丢计数）—— 不做这一步，冷用户的计数键会永久
// 堆积在 redis 里。
//
// 计数失败 → 放行（fail-open，brief 纪律「计数不可用放行」）：限流器是保护
// 自己的手段，KV 挂掉就让压力进来，也好过把所有请求一起拒掉（那是把中间件的
// 故障放大成整个域的不可用）。
func (r *RateLimiter) Allow(ctx context.Context, userID int64) bool {
	if r == nil || r.kv == nil || r.limit <= 0 {
		return true
	}
	key := fmt.Sprintf("%s:%d:%d", r.prefix, userID, time.Now().UnixNano()/r.window.Nanoseconds())
	n, err := r.kv.Incr(ctx, key)
	if err != nil {
		return true
	}
	if n == 1 {
		_ = r.kv.Set(ctx, key, "1", r.window)
	}
	return n <= r.limit
}
