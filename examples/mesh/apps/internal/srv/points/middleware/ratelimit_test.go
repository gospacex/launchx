package middleware

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gospacex/kvx"
	"github.com/gospacex/logx"
)

// fakeStore 是 kvx.KVStore 的手写假件。
//
// 内嵌接口让未覆写的方法自动满足签名（限流器只调用 Incr / Set，其余永不执行），
// 故只覆写被测的两条 —— 这就是「实现 kvx.KVStore 的最小 struct」，不引 mock 框架。
type fakeStore struct {
	kvx.KVStore
	incr func(ctx context.Context, key string) (int64, error)
	set  func(ctx context.Context, key string, value string, ttl time.Duration) error

	incrCalls int
	setKeys   []string
	setValues []string
	setTTLs   []time.Duration
}

func (f *fakeStore) Incr(ctx context.Context, key string) (int64, error) {
	f.incrCalls++
	return f.incr(ctx, key)
}

func (f *fakeStore) Set(ctx context.Context, key string, value string, ttl time.Duration) error {
	f.setKeys = append(f.setKeys, key)
	f.setValues = append(f.setValues, value)
	f.setTTLs = append(f.setTTLs, ttl)
	if f.set != nil {
		return f.set(ctx, key, value, ttl)
	}
	return nil
}

// fakeLogger 同理：内嵌 logx.Logger，只覆写 Warn，用于断言「放行必留痕」。
type fakeLogger struct {
	logx.Logger
	warns []string
}

func (l *fakeLogger) Warn(msg string, fields ...logx.Field) {
	l.warns = append(l.warns, msg)
}

func warned(l *fakeLogger, substr string) bool {
	for _, m := range l.warns {
		if strings.Contains(m, substr) {
			return true
		}
	}
	return false
}

// nil 接收者：Allow 的首个判据就是 r == nil，必须放行且不 panic。
func TestAllowNilReceiverFailOpen(t *testing.T) {
	var r *RateLimiter
	if !r.Allow(context.Background(), "u1") {
		t.Fatal("nil 接收者应放行（不 panic）")
	}
}

// 分支 1：store 为 nil（KV 降级）→ 放行 + Warn。
func TestAllowFailOpenWhenStoreUnavailable(t *testing.T) {
	lg := &fakeLogger{}
	r := New(nil, lg)
	if !r.Allow(context.Background(), "u1") {
		t.Fatal("KV 不可用时应放行")
	}
	if !warned(lg, "rate limiter unavailable") {
		t.Fatalf("放行必须留 Warn 痕迹，实际：%v", lg.warns)
	}
}

// 分支 2：subject 为空 → 放行 + Warn，且绝不触达计数后端。
func TestAllowFailOpenWhenSubjectEmpty(t *testing.T) {
	st := &fakeStore{incr: func(context.Context, string) (int64, error) {
		t.Fatal("空 subject 不应触达计数后端")
		return 0, nil
	}}
	lg := &fakeLogger{}
	r := New(st, lg)
	if !r.Allow(context.Background(), "") {
		t.Fatal("空 subject 应放行")
	}
	if st.incrCalls != 0 {
		t.Fatalf("空 subject 不应调用 Incr，实际 %d 次", st.incrCalls)
	}
	if !warned(lg, "rate limiter unavailable") {
		t.Fatalf("应留 Warn，实际：%v", lg.warns)
	}
}

// 分支 3：Incr 报错 → 放行（fail-open）+ Warn，不把限流器变成故障源。
func TestAllowFailOpenOnIncrError(t *testing.T) {
	st := &fakeStore{incr: func(context.Context, string) (int64, error) {
		return 0, errors.New("counter backend down")
	}}
	lg := &fakeLogger{}
	r := New(st, lg)
	if !r.Allow(context.Background(), "u1") {
		t.Fatal("计数后端报错应放行")
	}
	if !warned(lg, "counter failed") {
		t.Fatalf("计数失败必须留 Warn，实际：%v", lg.warns)
	}
}

// 分支 4：正常计数 —— n ≤ limit 放行、n > limit 拒绝；
// 且只有 n==1 那次补设窗口 TTL（每次 Incr 都设会让窗口永不滚动）。
func TestAllowCountsAndLimit(t *testing.T) {
	var n int64
	st := &fakeStore{incr: func(context.Context, string) (int64, error) {
		n++
		return n, nil
	}}
	r := New(st, &fakeLogger{})
	ctx := context.Background()

	// 首个计数：放行，键带命名空间前缀与主体，补设一次窗口 TTL。
	if !r.Allow(ctx, "u1") {
		t.Fatal("首个计数应放行")
	}
	if len(st.setKeys) != 1 {
		t.Fatalf("n==1 应补设一次窗口 TTL，实际 %d 次", len(st.setKeys))
	}
	if !strings.HasPrefix(st.setKeys[0], namespace+":u1:") {
		t.Fatalf("计数键应以 %s:u1: 为前缀，实际 %q", namespace, st.setKeys[0])
	}
	if st.setTTLs[0] != rateLimitWindow {
		t.Fatalf("TTL = %v, want %v", st.setTTLs[0], rateLimitWindow)
	}

	// 到上限仍放行。
	for i := int64(2); i <= rateLimitPerMinute; i++ {
		if !r.Allow(ctx, "u1") {
			t.Fatalf("第 %d 次（≤上限 %d）应放行", i, rateLimitPerMinute)
		}
	}
	// 超上限即拒绝。
	if r.Allow(ctx, "u1") {
		t.Fatalf("第 %d 次（>上限 %d）应拒绝", rateLimitPerMinute+1, rateLimitPerMinute)
	}
	// TTL 只在 n==1 设一次，后续不再 Set。
	if len(st.setKeys) != 1 {
		t.Fatalf("TTL 只应在首计数设一次，实际 %d 次", len(st.setKeys))
	}
}

// 补设 TTL 失败：仍按计数结果放行，但必须留 Warn。
func TestAllowWarnsWhenSetTTLFails(t *testing.T) {
	st := &fakeStore{
		incr: func(context.Context, string) (int64, error) { return 1, nil },
		set:  func(context.Context, string, string, time.Duration) error { return errors.New("no ttl") },
	}
	lg := &fakeLogger{}
	r := New(st, lg)
	if !r.Allow(context.Background(), "u1") {
		t.Fatal("补设 TTL 失败不应影响放行判定")
	}
	if !warned(lg, "ttl failed") {
		t.Fatalf("TTL 设置失败必须留 Warn，实际：%v", lg.warns)
	}
}
