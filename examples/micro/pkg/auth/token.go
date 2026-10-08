package auth

import (
	"context"
	"strconv"
	"time"

	"github.com/gospacex/cachex"
)

// tokenKey 令牌 → 用户映射的缓存键。
//
// 集中一处定义：播种侧（装配期）与校验侧（每次请求）必须拼出同一个键，
// 各拼一次迟早会漂移，而漂移的表现是"启动播种成功、请求永远 401"。
func tokenKey(token string) string { return "auth:token:" + token }

// TokenStore 令牌存储：token → userID 的映射托管给缓存，供边界层反查。
//
// 为什么用缓存而不是本地 map：本地 map 让鉴权退化成无外部依赖的纯 CPU 操作，
// 那它既不该有 span，也没法在多实例间共享 —— 一次签发要能被所有 bff 实例认，
// 凭据就得落在进程外。代价是每次校验多一次 redis 往返，这是刻意为之的：
// 「凭据来自外部」正是这条链路上值得被看见的一跳。
type TokenStore struct {
	inst cachex.Cache
}

// NewTokenStore 构造令牌存储。inst 只能来自 App.Cache()（业务不自建连接）。
func NewTokenStore(inst cachex.Cache) TokenStore { return TokenStore{inst: inst} }

// GrantToken 播种一条令牌 → 用户 ID 映射（demo 启动期的一次性写入）。
//
// 生产里这活属于签发服务，含过期轮换与吊销；demo 只在启动时写一条，
// 目的是让「校验时那次缓存查询」真的有东西可查 —— 否则中间件写对了也永远是 401，
// 看不出到底是校验逻辑错还是凭据根本没播种。
func (t TokenStore) GrantToken(ctx context.Context, token string, userID int64, ttl time.Duration) error {
	return t.inst.Set(ctx, tokenKey(token), strconv.FormatInt(userID, 10), ttl)
}

// Resolve 反查令牌对应的用户 ID；false 表示令牌不存在或已失效。
//
// 缓存报错与"查不到"返回同一个 false（不区分）：对调用方而言两者后果一致
// —— 这次请求不能放行。区分它们只会让中间件多一条分支，而排障要看的是
// 「缓存为什么报错」，那由 cache 层的 span 与日志负责，不由鉴权层转述。
func (t TokenStore) Resolve(ctx context.Context, token string) (int64, bool) {
	raw, err := t.inst.Get(ctx, tokenKey(token))
	if err != nil || raw == "" {
		return 0, false
	}
	id, perr := strconv.ParseInt(raw, 10, 64)
	if perr != nil || id <= 0 {
		return 0, false
	}
	return id, true
}
