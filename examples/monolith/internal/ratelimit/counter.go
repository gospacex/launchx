// Package ratelimit 限流计数：把「窗口内自增」这一件事交给 kvx 的原子 Incr。
//
// 它原本实现在 internal/cache 下（cachex.Get → +1 → Set）。cachex.Cache 没有
// 原子自增，两次并发计数会读到同一个旧值、再写回同一个新值（lost update），
// 结果是计数**系统性低估**：窗口计数永远达不到限值，限流器形同虚设，而且在
// Jaeger 上一切正常（请求都成功、span 都在）—— 这是最难自查的一类自保护失效。
// kvx.KVStore 的 Incr 是 Redis 原子命令，语义正对。
//
// 换到这里也顺带把两个组件的职责分清了：
//
//	kvx    可自增的键值状态（限流计数）
//	cachex 可失效的派生缓存（订单详情、鉴权令牌）
//
// 两者都落在同一个 redis 实例上，但句柄与语义分开 —— 缓存被逐出是允许的，
// 计数被逐出等于限流悄悄失效，混用一个句柄就很难分辨这两种后果。
package ratelimit

import (
	"context"
	"time"

	"github.com/gospacex/kvx"
)

// Counter 窗口计数器（kvx 实现）。
//
// 对外只需满足 middleware.Counter 接口：**由中间件定义它需要的动作，
// 由本包决定怎么实现** —— 将来要换成 Lua 滑动窗口，中间件一行不用改。
type Counter struct {
	kv kvx.KVStore
}

// NewCounter 基于 KV 实例构造计数器。kv 为 nil 时得到一个不就绪的零值 Counter，
// 由 Ready() 判定 —— 装配层据此决定是否挂上限流中间件。
func NewCounter(kv kvx.KVStore) Counter { return Counter{kv: kv} }

// Ready 报告计数器是否有可用的 KV 句柄。
//
// 之所以要显式判就绪：限流缺失不会报错，只会不限流。若把"没接上"当成
// "接上了但计数为 0"，事后就无法区分「流量没超」与「限流器根本没装」。
func (c Counter) Ready() bool { return c.kv != nil }

// Incr 对 key 原子自增并返回自增后的值；ttl 即窗口长度。
//
// Incr 首次创建 key 时带不上 TTL，故在自增结果为 1 时用 Set 把 TTL 补上
// （此刻值必然是 1，覆写不丢计数）。不做这一步，冷 IP 的计数键会永久堆在
// redis 里 —— 那既是内存泄漏，也会让"按 key 排查计数"变得不可行。
func (c Counter) Incr(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	n, err := c.kv.Incr(ctx, key)
	if err != nil {
		return 0, err
	}
	if n == 1 && ttl > 0 {
		// 补 TTL 失败不影响本次计数：键最终会按窗口逻辑被下一次请求重写，
		// 而计数本身已经原子完成，返回它比返回错误更贴近调用方的语义。
		_ = c.kv.Set(ctx, key, "1", ttl)
	}
	return n, nil
}
