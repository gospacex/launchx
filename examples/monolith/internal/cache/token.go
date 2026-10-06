package cache

import (
	"context"
	"strconv"
	"time"

	"github.com/gospacex/cachex"
)

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
