// Package middleware 入口自保护：基于 kvx 的固定窗口限流。
//
// 为什么用 kvx 而不是 cachex：cachex.Cache 只有 Get/Set/Delete（没有原子
// Incr），自保护计数需要原子自增，否则并发下计数会被读-改-写覆盖成低估。
// kvx.KVStore 提供 Incr/Decr，语义正对。两者分工因此是：
//
//	kvx    可自增的键值状态（限流计数、幂等键、热度计数）
//	cachex 可失效的派生缓存（商品详情、订单详情）
//
// 姿态是 fail-open：KV 不可用时 Allow 恒 true。限流器是**保护自己**的手段，
// 缓存挂掉就让限流失效、把压力放进来，也好过把所有请求一起拒掉（那是把
// 中间件的故障放大成整个域的不可用）。
package middleware

import (
	"context"
	"fmt"
	"time"

	"github.com/gospacex/kvx"
	"github.com/gospacex/logx"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// RateLimiter 固定窗口限流器（单实例、单 key 前缀）
type RateLimiter struct {
	kv     kvx.KVStore
	prefix string
	limit  int64
	window time.Duration
	logger logx.Logger
}

// NewRateLimiter 构造。kv 为 nil（KV 组件降级）或参数非法时返回 nil 表示"不限流"。
//
// logger 允许 nil：退到 logx 全局（与四个域服的 service 构造同一口径）。限流
// 器要打日志是因为它**只在出事时才有话要说** —— KV 挂掉、TTL 补不上都是
// "限流已经失效"的信号，静默等于把发现时机推迟到实例被打爆。
func NewRateLimiter(kv kvx.KVStore, prefix string, limit int, window time.Duration, logger logx.Logger) *RateLimiter {
	if kv == nil || limit <= 0 || window <= 0 {
		return nil
	}
	if logger == nil {
		logger = logx.GetGlobal()
	}
	return &RateLimiter{kv: kv, prefix: prefix, limit: int64(limit), window: window, logger: logger}
}

// Allow 判定给定主体在当前窗口内是否放行。
//
// key 带窗口序号，窗口自然滚动。Incr 首次创建 key 时带不了 TTL，故补一次 Set
// 把 TTL 加上 —— 不做这一步，冷用户的计数键会永久堆积在 redis 里。
//
// 补 TTL 这里有个**真实存在**的竞态（2026-10-10 修正，未彻底修）：旧注释写
// "此时值必然是 1"，那是把 INCR 的单命令原子性误当成了「Incr + Set 两步」的
// 原子性 —— A 看到 n==1 并不代表它去 Set 时值还是 1，B 可能已经累加到 5，
// 此刻 `Set(key, "1")` 会把 5 覆写回 1（计数被抹，限流形同虚设）。
//
// 缓解办法：Set 之前先查一次 TTL，只有**确实还没有 TTL** 才补，把窗口从
// "每次首次创建都撞"缩小到"两个进程同时首次创建才撞"。彻底修需要 kvx 提供
// Expire(key, ttl) 或 SetNX（Redis 原生两者都有）；SDK 补上之前只能做到这一步，
// 残余风险是计数偶发低估（放行偏多），不会把已放行的请求误判成超限。
func (r *RateLimiter) Allow(ctx context.Context, subject int64) bool {
	if r == nil {
		return true
	}
	key := fmt.Sprintf("%s:%d:%d", r.prefix, subject, time.Now().UnixNano()/r.window.Nanoseconds())
	n, err := r.kv.Incr(ctx, key)
	if err != nil {
		// 不吞错：计数拿不到就意味着限流已失效（后面一律放行）。静默的话，
		// "KV 挂了"这件事要等到流量把实例打爆才会被发现。
		r.logger.WarnContext(ctx, "ratelimit: incr failed, allowing by fail-open",
			logx.String("key", key), logx.Err(err))
		if span := trace.SpanFromContext(ctx); span != nil && span.IsRecording() {
			span.SetAttributes(attribute.Bool("ratelimit.degraded", true))
		}
		return true // fail-open：计数不可用就放行，见包注释
	}
	if n != 1 {
		return n <= r.limit
	}
	ttl, err := r.kv.TTL(ctx, key)
	if err != nil {
		r.logger.WarnContext(ctx, "ratelimit: ttl probe failed, skip ttl backfill",
			logx.String("key", key), logx.Err(err))
		return n <= r.limit
	}
	if ttl > 0 {
		// 已有 TTL（别人先补的）：再 Set 一次会把已累加的计数覆写回 1
		return n <= r.limit
	}
	if err := r.kv.Set(ctx, key, "1", r.window); err != nil {
		r.logger.WarnContext(ctx, "ratelimit: ttl backfill failed, counter key may leak",
			logx.String("key", key), logx.Err(err))
	}
	return n <= r.limit
}
