package service

import (
	"testing"
	"time"

	"github.com/gospacex/launchx/examples/monolith/internal/cache"
)

// TestStoreNilWhenNoCache typed nil 陷阱的运行时守卫。
//
// `s.cache` 是 `*cache.Order`：一个 **nil 指针转成接口后 != nil**。
// store() 若不显式判空，loadthrough 就会以为缓存已装配，转头在 nil 接收者上
// 调 Get —— 而"没配缓存"恰恰是默认配置下最容易走到的路径。
//
// 反向探针（已实测）：把 store() 里的判空删掉、直接 `return s.cache`
// → 本用例红（got 为 *cache.Order，非 nil 接口）。
func TestStoreNilWhenNoCache(t *testing.T) {
	s := &Service{} // 未配缓存
	if got := s.store(); got != nil {
		t.Fatalf("未配缓存时 store() 必须返回 nil 接口，实际 %T —— "+
			"typed nil 会让 loadthrough 以为缓存已装配", got)
	}
}

// TestStoreWiredWhenCacheConfigured 反面：配了缓存时必须真的接上，
// 否则"配了却没用上"会静默退化成每次回源。
func TestStoreWiredWhenCacheConfigured(t *testing.T) {
	s := &Service{cache: cache.NewOrder(nil, time.Minute, false)}
	if got := s.store(); got == nil {
		t.Fatal("配了缓存时 store() 不得返回 nil")
	}
}

// TestCacheOptUsesConfiguredTTL 正缓存 TTL 的真源是装配层给出的配置，
// 不在 service 里再写一份 —— 抄一份会让"改了配置只有一半缓存跟着变"。
func TestCacheOptUsesConfiguredTTL(t *testing.T) {
	s := &Service{cache: cache.NewOrder(nil, 42*time.Second, false)}
	if got := s.cacheOpt().TTL; got != 42*time.Second {
		t.Fatalf("TTL 应取自 cache.Order，实际 %v", got)
	}
}
