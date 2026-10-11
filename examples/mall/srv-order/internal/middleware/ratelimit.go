// Package middleware 入口自保护：基于 kvx 的固定窗口限流。
//
// 为什么用 kvx 而不是 cachex：cachex.Cache 只有 Get/Set/Delete（没有原子
// Incr），自保护计数需要原子自增，否则并发下计数会被读-改-写覆盖成低估。
// kvx.KVStore 提供 Incr/Decr，语义正对。两者分工因此是：
//
//	kvx    可自增的键值状态（限流计数、幂等键）
//	cachex 可失效的派生缓存（订单详情、商品详情）
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

	"github.com/gospacex/otelx/layer"
)

// RateLimiter 固定窗口限流器（单实例、单 key 前缀）
type RateLimiter struct {
	kv     kvx.KVStore
	prefix string
	// limit 有身份主体（user_id / operator_id > 0）的每窗口配额
	limit int64
	// anonLimit 0 号桶（无身份调用方）的配额，远小于 limit —— 见 Allow
	anonLimit int64
	window    time.Duration
	logger    logx.Logger
}

// anonymousQuotaDivisor 未显式配置时，0 号桶配额 = per-user 配额 / 本值。
//
// 取 10 的理由是量级而不是精度：0 号桶是**全体无身份来源共用**的一个桶，
// 它只要比 per-user 小一个量级，单个来源就打不满全域名额；具体是 1/8 还是
// 1/12 对结论没有影响，故不给它一个需要调参的旋钮。
const anonymousQuotaDivisor = 10

// NewRateLimiter 构造。kv 为 nil（KV 组件降级）时返回 nil 表示"不限流"。
func NewRateLimiter(kv kvx.KVStore, prefix string, limit int, window time.Duration, anonLimit int, logger logx.Logger) *RateLimiter {
	if kv == nil || limit <= 0 || window <= 0 {
		return nil
	}
	anon := int64(anonLimit)
	if anon <= 0 {
		anon = int64(limit) / anonymousQuotaDivisor
	}
	if anon < 1 {
		anon = 1 // 配额再小也得允许 1 次：0 会让"无身份"路径全量 429，那不是限流是断流
	}
	return &RateLimiter{kv: kv, prefix: prefix, limit: int64(limit), anonLimit: anon, window: window, logger: logger}
}

// Allow 判定给定主体在当前窗口内是否放行。
//
// 0 号桶（subject <= 0，即调用方没带身份）走**独立的、小得多的配额**：rpc 层
// 对拿不到 user_id 的方法（列表 / 报表 / 按商品维度的接口）一律传 0，于是全体
// 无身份来源共用一个桶。给它与 per-user 同等的配额，等于让单个来源就能打满
// 整个域的名额、把有身份的正常流量一起挤掉 —— 限流从"按主体配额"退化成
// "全域共享一个桶"，形同没有限流。
//
// key 带窗口序号，窗口自然滚动。Incr 无法带过期时间，故在 n==1 时补一次 TTL
// （见 ensureTTL）—— 不补则冷 key 永久堆积在 redis 里。
func (r *RateLimiter) Allow(ctx context.Context, subject int64) bool {
	if r == nil {
		return true
	}
	limit := r.limit
	if subject <= 0 {
		limit = r.anonLimit
		layer.Attrs(ctx, attribute.Bool("rate_limit.anonymous", true))
	}
	key := fmt.Sprintf("%s:%d:%d", r.prefix, subject, time.Now().UnixNano()/r.window.Nanoseconds())
	n, err := r.kv.Incr(ctx, key)
	if err != nil {
		return true // fail-open：计数不可用就放行，见包注释
	}
	if n == 1 {
		r.ensureTTL(ctx, key)
	}
	if n > limit {
		// 命中限流必须能按主体检索：限流一旦误伤，最先要回答的就是
		// "到底是谁被打回来了"，没有这个属性只能靠猜。
		layer.Attrs(ctx,
			attribute.Bool("rate_limit.blocked", true),
			attribute.Int64("rate_limit.subject", subject),
			attribute.Int64("rate_limit.limit", limit),
		)
		return false
	}
	return true
}

// ensureTTL 给刚由 Incr 创建的计数键补一个 TTL。
//
// 只在 n==1 时调、且补之前先 TTL 判一次，两件事各有理由：
//   - kvx 的 Incr 不能带过期时间，不补 TTL 冷 key 就永久堆积；
//   - Set(key, "1") 会**覆写**并发期间别人已经累加的值：A 拿到 n==1 去补 TTL，
//     此时 B 已经把值加到 3，A 这一写把它按回 1（少算两次，表现为限流偏松）。
//     先 `TTL(key)` 判一次，只在确实没有 TTL（key 刚建 / TTL 丢了）时才写，
//     把这段竞态窗口从"每次窗口滚动"压到"仅首次" —— 注意 n==1 的那一位可能
//     不止一个，所以这里**不能**再说"此时值必然是 1"。
//
// ⚠️ 这不是彻底修：只要 kvx 只提供 Set，覆写竞态就消不掉（判空与写入之间仍
// 有窗口）。彻底修要等 kvx 提供 Expire（只设 TTL 不动值）或 SetNX；在那之前
// 这里只能缩小窗口 + 出错留痕。降级方向是"限流偏松"，可接受。
func (r *RateLimiter) ensureTTL(ctx context.Context, key string) {
	ttl, err := r.kv.TTL(ctx, key)
	if err != nil {
		layer.Attrs(ctx, attribute.Bool("rate_limit.ttl_probe_failed", true))
		if r.logger != nil {
			r.logger.WarnContext(ctx, "rate limit: ttl probe failed, key may leak",
				logx.String("key", key), logx.Err(err))
		}
		return
	}
	if ttl > 0 {
		return // 已有 TTL：不覆写，避免把并发累加的值按回去
	}
	if err := r.kv.Set(ctx, key, "1", r.window); err != nil {
		layer.Attrs(ctx, attribute.Bool("rate_limit.ttl_set_failed", true))
		if r.logger != nil {
			r.logger.WarnContext(ctx, "rate limit: ttl set failed, key may leak",
				logx.String("key", key), logx.Err(err))
		}
	}
}
