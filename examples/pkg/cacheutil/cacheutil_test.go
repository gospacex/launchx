package cacheutil

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// ======================= 测试替身 =======================

// fakeCache 内存缓存，记录回源无关的动作次数（判据要看"有没有写/写几次"）。
type fakeCache struct {
	mu      sync.Mutex
	data    map[string]string
	sets    map[string]time.Duration
	deletes []string
	failSet bool
}

func newFakeCache() *fakeCache {
	return &fakeCache{data: map[string]string{}, sets: map[string]time.Duration{}}
}

func (f *fakeCache) Get(_ context.Context, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data[key], nil
}

func (f *fakeCache) Set(_ context.Context, key, value string, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSet {
		return errors.New("cache set failed on purpose")
	}
	f.data[key] = value
	f.sets[key] = ttl
	return nil
}

func (f *fakeCache) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, key)
	delete(f.data, key)
	return nil
}

func (f *fakeCache) Exists(_ context.Context, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.data[key]
	return ok, nil
}

func (f *fakeCache) Keys(_ context.Context, _ string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.data))
	for k := range f.data {
		out = append(out, k)
	}
	return out, nil
}

func (f *fakeCache) Close() error                        { return nil }
func (f *fakeCache) HealthCheck(_ context.Context) error { return nil }

func (f *fakeCache) setCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sets)
}

func (f *fakeCache) valueOf(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data[key]
}

// ======================= 领域替身 =======================

type detail struct {
	ID   uint64 `json:"id"`
	Name string `json:"name"`
}

// errMissing 模拟 repository 的「不存在」哨兵。
var errMissing = errors.New("repository: record not found")

// errBroken 模拟回源故障（不是 not found）。
var errBroken = errors.New("repository: connection reset")

func opt() Options {
	return Options{
		Label:    "test",
		TTL:      5 * time.Minute,
		NotFound: func(err error) bool { return errors.Is(err, errMissing) },
	}
}

// ======================= 判据 =======================

// TestFetchFillsNegativeOnNotFound 缓存穿透的核心判据：回源确认不存在之后，
// 必须回填空值哨兵，使后续同一键**不再回源**。
//
// 反向探针（已实测）：把 Fetch 里的空值回填整行删掉 → 本用例红
// （第二次 Fetch 仍会回源，loads 变 2）。
func TestFetchFillsNegativeOnNotFound(t *testing.T) {
	c := newFakeCache()
	loads := 0
	load := func(context.Context) (*detail, error) {
		loads++
		return nil, errMissing
	}

	_, err := Fetch(context.Background(), c, opt(), "k", load)
	if err == nil {
		t.Fatal("回源报 not found 时 Fetch 必须返回错误")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("必须是 ErrNotFound，got %v", err)
	}
	if got := c.valueOf("k"); got != MissSentinel {
		t.Fatalf("回源 miss 后必须回填空值哨兵，缓存里是 %q", got)
	}

	if _, err := Fetch(context.Background(), c, opt(), "k", load); !errors.Is(err, ErrNotFound) {
		t.Fatalf("第二次仍应是 ErrNotFound，got %v", err)
	}
	if loads != 1 {
		t.Fatalf("命中空值哨兵后不应再回源，回源次数 = %d", loads)
	}

	// 正缓存 TTL 与空值 TTL 必须分开：空值用 Options.MissTTL / 默认窗口。
	if ttl := c.sets["k"]; ttl != DefaultMissTTL {
		t.Fatalf("空值哨兵的 TTL 应为 %s，实际 %s", DefaultMissTTL, ttl)
	}
}

// TestFetchNegativeHitKeepsNoCause 命中空值哨兵时没有原始错误可附，
// 但 errors.Is(err, ErrNotFound) 必须照样成立 —— 调用方按它翻译成错误码。
func TestFetchNegativeHitKeepsNoCause(t *testing.T) {
	c := newFakeCache()
	load := func(context.Context) (*detail, error) { return nil, errMissing }
	if _, err := Fetch(context.Background(), c, opt(), "k", load); err == nil {
		t.Fatal("首次应回源并回填")
	}

	_, err := Fetch(context.Background(), c, opt(), "k", func(context.Context) (*detail, error) {
		t.Fatal("命中空值哨兵后不应回源")
		return nil, nil
	})
	var nf *NotFound
	if !errors.As(err, &nf) {
		t.Fatalf("应是 *NotFound，got %T: %v", err, nf)
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("命中哨兵时 errors.Is(ErrNotFound) 必须成立，got %v", err)
	}
	if nf.Cause != nil {
		t.Fatalf("命中哨兵时 Cause 应为 nil（缓存里没有原始错误），got %v", nf.Cause)
	}
}

// TestFetchNotFoundKeepsCause 回源 miss 给出的 *NotFound 必须保住原始错误链：
// 各域的 mapRepoErr / codes.Wrap 靠 errors.Is 认 repository.ErrNotFound。
func TestFetchNotFoundKeepsCause(t *testing.T) {
	c := newFakeCache()
	_, err := Fetch(context.Background(), c, opt(), "k", func(context.Context) (*detail, error) {
		return nil, errMissing
	})
	if !errors.Is(err, errMissing) {
		t.Fatalf("原始错误链断了：errors.Is(err, errMissing) 为 false，got %v", err)
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("errors.Is(err, ErrNotFound) 为 false，got %v", err)
	}
}

// TestFetchNeverCachesFailure 回源故障**不能**被缓存成"不存在"。
//
// 为什么值得单列：把故障写成空值哨兵，会让一次抖动变成"最长 MissTTL 内该键
// 查不到"，且不会有任何错误日志 —— 数据看起来凭空消失了。
func TestFetchNeverCachesFailure(t *testing.T) {
	c := newFakeCache()
	_, err := Fetch(context.Background(), c, opt(), "k", func(context.Context) (*detail, error) {
		return nil, errBroken
	})
	if !errors.Is(err, errBroken) {
		t.Fatalf("故障应原样上抛，got %v", err)
	}
	if got := c.valueOf("k"); got != "" {
		t.Fatalf("回源故障时不得写任何缓存，缓存里是 %q", got)
	}
	if c.setCount() != 0 {
		t.Fatalf("回源故障时不得有写入，sets = %d", c.setCount())
	}
}

// TestFetchPositivePath 命中正缓存不回源；miss 回源后按 TTL 回填正缓存。
func TestFetchPositivePath(t *testing.T) {
	c := newFakeCache()
	loads := 0
	load := func(context.Context) (*detail, error) {
		loads++
		return &detail{ID: 7, Name: "x"}, nil
	}

	got, err := Fetch(context.Background(), c, opt(), "k", load)
	if err != nil || got == nil || got.ID != 7 {
		t.Fatalf("首次应回源成功，got %+v, err %v", got, err)
	}
	if ttl := c.sets["k"]; ttl != 5*time.Minute {
		t.Fatalf("正缓存 TTL 应为 5m，实际 %s", ttl)
	}
	if _, err := Fetch(context.Background(), c, opt(), "k", load); err != nil {
		t.Fatalf("第二次应命中缓存：%v", err)
	}
	if loads != 1 {
		t.Fatalf("命中正缓存后不应再回源，回源次数 = %d", loads)
	}
}

// TestFetchNilCacheDegradesToLoad 缓存组件未装配（降级形态）时直回源且不 panic。
func TestFetchNilCacheDegradesToLoad(t *testing.T) {
	loads := 0
	got, err := Fetch(context.Background(), nil, opt(), "k", func(context.Context) (*detail, error) {
		loads++
		return &detail{ID: 1}, nil
	})
	if err != nil || got == nil || got.ID != 1 {
		t.Fatalf("无缓存时应直回源，got %+v, err %v", got, err)
	}
	if _, err := Fetch(context.Background(), nil, opt(), "k", func(context.Context) (*detail, error) {
		loads++
		return &detail{ID: 2}, nil
	}); err != nil {
		t.Fatalf("无缓存时第二次仍应回源：%v", err)
	}
	if loads != 2 {
		t.Fatalf("无缓存时每次都该回源，loads = %d", loads)
	}
}

// TestInvalidateClearsNegative 写路径失效必须能把空值哨兵一并删掉。
//
// 这是负缓存的**前提**：没有它，"查过一次不存在的 ID → 随后该 ID 被创建"
// 会让新记录在最长 MissTTL 内查不到（各域 Create 路径因此都要调失效）。
func TestInvalidateClearsNegative(t *testing.T) {
	c := newFakeCache()
	if _, err := Fetch(context.Background(), c, opt(), "k", func(context.Context) (*detail, error) {
		return nil, errMissing
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("首次应回填哨兵，got %v", err)
	}

	Invalidate(context.Background(), c, opt(), "k")
	if got := c.valueOf("k"); got != "" {
		t.Fatalf("失效后哨兵必须消失，缓存里是 %q", got)
	}

	got, err := Fetch(context.Background(), c, opt(), "k", func(context.Context) (*detail, error) {
		return &detail{ID: 9}, nil
	})
	if err != nil || got == nil || got.ID != 9 {
		t.Fatalf("失效后应能读到新写入的记录，got %+v, err %v", got, err)
	}
}

// TestFetchFillFailureIsNonFatal 回填失败只告警：缓存是加速路径，不是数据源。
func TestFetchFillFailureIsNonFatal(t *testing.T) {
	c := newFakeCache()
	c.failSet = true
	got, err := Fetch(context.Background(), c, opt(), "k", func(context.Context) (*detail, error) {
		return &detail{ID: 3}, nil
	})
	if err != nil || got == nil || got.ID != 3 {
		t.Fatalf("回填失败不应影响业务结果，got %+v, err %v", got, err)
	}
}

// TestFetchDecodedGarbageFallsBack 缓存里是认不出的值时按 miss 处理并覆盖。
func TestFetchDecodedGarbageFallsBack(t *testing.T) {
	c := newFakeCache()
	_ = c.Set(context.Background(), "k", "{not-json", time.Minute)

	got, err := Fetch(context.Background(), c, opt(), "k", func(context.Context) (*detail, error) {
		return &detail{ID: 4}, nil
	})
	if err != nil || got == nil || got.ID != 4 {
		t.Fatalf("脏值应回源并覆盖，got %+v, err %v", got, err)
	}
}
