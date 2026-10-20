package auth

// 本文件的判据：静态演示令牌**不许过期**。
//
// 背景（本机 2026-10-18 实测）：micro 六进程连跑 4 天后，T9 尾部 span 断言
// 失败，失败信息是 `POST /api/order 返回 HTTP 401：invalid token`。查下来
// 凭据本身一个字都没错 —— 是它**过期**了：装配期播种演示令牌时给了一个
// 固定的 1 小时 TTL，进程跑过 1 小时之后缓存里那条映射就没了，于是所有业务
// 路由一起 401。
//
// 这个报错最坑的地方是方向性：它说 "invalid token"（凭据无效），人会照着
// "凭据配错了"去查 —— 比对 yaml、比对 Secret、比对注入，全是白费；真相是
// "凭据曾经有效，但自己过期了"。一条静态播种的常量凭据本来就没有签发者、
// 没有轮换、没有吊销，给它一个过期时间换不来任何防线，只换来一次自我了断。
//
// 所以这里把两件事分开断言：
//   ① 静态令牌播种后，即便时间推进远超任何票据窗口，仍然解析得出来（本文件）
//   ② 带 TTL 的令牌到点确实会失效（反向对照，证明 ① 不是因为假件恒命中）

import (
	"context"
	"testing"
	"time"
)

// expiringCache 带过期语义的假 cache：Set 记下到期时刻，Get 按**可注入的时钟**
// 判断。
//
// 为什么不复用 auth_test.go 的 fakeCache：那个忽略 ttl（Get 永远命中），用它
// 测"过期"会得到一条恒绿的假判据 —— 缺陷就在眼前也看不见。
type expiringCache struct {
	data map[string]string
	exp  map[string]time.Time
	now  time.Time
}

func (c *expiringCache) Get(_ context.Context, key string) (string, error) {
	if t, ok := c.exp[key]; ok && c.now.After(t) {
		return "", nil // 到期即不可见，与 redis 一致
	}
	return c.data[key], nil
}

func (c *expiringCache) Set(_ context.Context, key, value string, ttl time.Duration) error {
	if c.data == nil {
		c.data = map[string]string{}
		c.exp = map[string]time.Time{}
	}
	c.data[key] = value
	if ttl > 0 {
		c.exp[key] = c.now.Add(ttl)
	} else {
		delete(c.exp, key) // ttl<=0 ⇒ 不过期
	}
	return nil
}

func (c *expiringCache) Delete(_ context.Context, key string) error {
	delete(c.data, key)
	delete(c.exp, key)
	return nil
}

func (c *expiringCache) Exists(_ context.Context, key string) (bool, error) {
	v, err := c.Get(context.Background(), key)
	return v != "", err
}

func (c *expiringCache) Keys(_ context.Context, _ string) ([]string, error) { return nil, nil }
func (c *expiringCache) Close() error                                       { return nil }
func (c *expiringCache) HealthCheck(_ context.Context) error                { return nil }

// TestSeedDemoTokenSurvivesLongRun：进程连跑数日，静态令牌仍须有效。
func TestSeedDemoTokenSurvivesLongRun(t *testing.T) {
	c := &expiringCache{now: time.Unix(1700000000, 0)}
	store := NewTokenStore(c)

	if err := store.SeedDemoToken(context.Background(), "demo-token", 1001); err != nil {
		t.Fatalf("SeedDemoToken: %v", err)
	}
	if id, ok := store.Resolve(context.Background(), "demo-token"); !ok || id != 1001 {
		t.Fatalf("刚播种的令牌就解析不出来：Resolve = (%d, %v)", id, ok)
	}

	// 推进 48 小时 —— 远超任何票据窗口（默认 5m，旧播种窗口 1h）。
	c.now = c.now.Add(48 * time.Hour)
	if _, ok := store.Resolve(context.Background(), "demo-token"); !ok {
		t.Fatal("静态演示令牌在长跑后失效：它是 config.yaml 里的常量，没有签发/轮换/吊销，给 TTL 换不来防线，只会让进程跑久了全部业务路由一起 401（且报错指向错误的方向）")
	}
}

// TestGrantTokenStillExpires 反向对照：带 TTL 的令牌到点必须失效。
//
// 缺了这一条，上一条的"PASS"可能只是因为假件永远命中 —— 那等于没测。
func TestGrantTokenStillExpires(t *testing.T) {
	c := &expiringCache{now: time.Unix(1700000000, 0)}
	store := NewTokenStore(c)

	if err := store.GrantToken(context.Background(), "short", 1001, time.Hour); err != nil {
		t.Fatalf("GrantToken: %v", err)
	}
	c.now = c.now.Add(2 * time.Hour)
	if _, ok := store.Resolve(context.Background(), "short"); ok {
		t.Fatal("带 TTL 的令牌在 2 倍 TTL 之后仍未失效 —— 假件的过期语义是坏的，上一条用例的结论不可信")
	}
}
