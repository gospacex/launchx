package cache

import (
	"context"
	"strconv"
	"time"

	"github.com/gospacex/cachex"
)

// Counter 窗口计数器：缓存侧唯一的自增实现。
//
// ⚠️ 非原子：cachex 的接口只有 Get/Set，没有 Incr，所以这里是「读出来 +1 再写回」。
// demo 单实例够用；多实例并发下同一 key 会漏计 —— 真要保证精度得上 Lua 脚本或
// Redis 原子命令，那超出本 demo 的范围，故在注释里写明而不是假装没这个问题。
type Counter struct {
	inst cachex.Cache
}

// NewCounter 基于缓存实例构造计数器。
func NewCounter(inst cachex.Cache) Counter { return Counter{inst: inst} }

// Incr 对 key 自增并返回自增后的值；ttl 即窗口长度（每次写回都续满，
// 与固定窗口"整窗口过期"的语义一致）。
func (c Counter) Incr(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	raw, err := c.inst.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	var n int64
	if raw != "" {
		if parsed, perr := strconv.ParseInt(raw, 10, 64); perr == nil {
			n = parsed
		}
		// 解析失败按 0 重新开始：脏值不是错误，继续计数好过打断整条链路。
	}
	n++
	if err := c.inst.Set(ctx, key, strconv.FormatInt(n, 10), ttl); err != nil {
		return 0, err
	}
	return n, nil
}

// tokenKey 令牌 → 用户映射的缓存键（集中一处，读写两侧才不会拼法不一致）。
func tokenKey(token string) string { return "token:" + token }

// TokenStore 令牌存储：把 token → userID 的映射托管给缓存，供鉴权中间件查询。
type TokenStore struct {
	inst cachex.Cache
}

// NewTokenStore 构造令牌存储。
func NewTokenStore(inst cachex.Cache) TokenStore { return TokenStore{inst: inst} }

// GrantToken 把令牌 → 用户 ID 的映射写进缓存（demo 启动期的凭据播种）。
//
// 生产里令牌应该落在签发服务、过期轮换也由它负责；demo 只在启动时写一条，
// 目的是让鉴权中间件的那次缓存查询真的有东西可查。
func (t TokenStore) GrantToken(ctx context.Context, token string, userID int64, ttl time.Duration) error {
	return t.inst.Set(ctx, tokenKey(token), strconv.FormatInt(userID, 10), ttl)
}

// Resolve 反查令牌对应用户 ID；false 表示令牌不存在或已失效。
func (t TokenStore) Resolve(ctx context.Context, token string) (int64, bool) {
	raw, err := t.inst.Get(ctx, tokenKey(token))
	if err != nil || raw == "" {
		return 0, false
	}
	id, perr := strconv.ParseInt(raw, 10, 64)
	if perr != nil {
		return 0, false
	}
	return id, true
}
