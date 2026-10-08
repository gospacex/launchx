package auth

import (
	"context"
	"testing"
	"time"

	"github.com/bytedance/gopkg/cloud/metainfo"
)

// 本文件的判据：身份的三件事必须各自成立 —— 签得出来、传得过去、验得出来。
// 三者任一断裂的表现都是"下游 1004"，但修法完全不同，故分开断言。

func TestNewSetsIssuerAndTimestamp(t *testing.T) {
	now := time.Unix(1700000000, 0)
	id := New(1001, 7, now)

	if id.UserID != 1001 || id.OperatorID != 7 {
		t.Fatalf("identity = %+v, want user=1001 operator=7", id)
	}
	if id.Issuer != IssuerBFF {
		t.Errorf("issuer = %q, want %q（签发方错了，域服会一律拒）", id.Issuer, IssuerBFF)
	}
	if id.IssuedAt != now.Unix() {
		t.Errorf("issued_at = %d, want %d", id.IssuedAt, now.Unix())
	}
}

func TestValidRejects(t *testing.T) {
	now := time.Unix(1700000000, 0)
	ttl := 5 * time.Minute

	cases := []struct {
		name string
		id   Identity
		want bool
		why  string
	}{
		{
			name: "正常签发",
			id:   New(1001, 0, now),
			want: true,
			why:  "签发方 + 主体 + 时效都在，必须放行",
		},
		{
			name: "签发方不对",
			id:   Identity{UserID: 1001, Issuer: "someone-else", IssuedAt: now.Unix()},
			want: false,
			why:  "伪造者最容易造的形态：主体对但签发方不是边界层",
		},
		{
			name: "没有任何主体",
			id:   Identity{Issuer: IssuerBFF, IssuedAt: now.Unix()},
			want: false,
			why:  "没有归属的调用，限流与审计都无从下手",
		},
		{
			name: "票据过期",
			id:   New(1001, 0, now.Add(-2*ttl)),
			want: false,
			why:  "重放一个旧身份不该被接受",
		},
		{
			name: "签发时刻缺失",
			id:   Identity{UserID: 1001, Issuer: IssuerBFF},
			want: false,
			why:  "半张票据（有主体无时效）正是伪造者最容易造出来的形态",
		},
		{
			name: "时钟大幅前移",
			id:   New(1001, 0, now.Add(time.Hour)),
			want: false,
			why:  "未来时间戳会让票据永久化，必须拒",
		},
		{
			name: "小幅时钟偏移应被容忍",
			id:   New(1001, 0, now.Add(30*time.Second)),
			want: true,
			why:  "机房内几十秒的偏移不该让鉴权随机失败",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.id.Valid(now, ttl); got != c.want {
				t.Errorf("Valid() = %v, want %v —— %s", got, c.want, c.why)
			}
		})
	}
}

// TestInjectExtractRoundTrip metainfo 往返：这是"身份能不能传下去"的唯一判据。
//
// 为什么必须测这一条：metainfo 的 transient 与 persistent 语义不同，
// 用错的话 bff → srv 这一跳能过、srv → srv 第二跳就断，
// 而那种断法只在链变长之后才暴露。
func TestInjectExtractRoundTrip(t *testing.T) {
	id := New(1001, 7, time.Unix(1700000000, 0))

	ctx := Inject(context.Background(), id)
	got, ok := Extract(ctx)
	if !ok {
		t.Fatal("Extract 取不到身份：metainfo 没写对，下游会一律 1004")
	}
	if got != id {
		t.Fatalf("往返后身份变了：got %+v want %+v", got, id)
	}
}

func TestExtractRejectsPartialTicket(t *testing.T) {
	// 只填 user_id，不填 issuer —— 伪造者最容易造出的形态，不得按零值放行。
	ctx := metainfo.WithPersistentValue(context.Background(), MetaKeyUserID, "1001")
	if _, ok := Extract(ctx); ok {
		t.Fatal("半张票据被接受了：只有 user_id 没有 issuer 的请求必须被拒")
	}
}

func TestExtractRejectsGarbage(t *testing.T) {
	ctx := metainfo.WithPersistentValue(context.Background(), MetaKeyUserID, "not-a-number")
	if _, ok := Extract(ctx); ok {
		t.Fatal("非数字的 user_id 被接受了")
	}
}

// TestWithIdentityCarriesBoth WithIdentity 必须同时写进程内键与 metainfo。
//
// 只写前者，下游收不到；只写后者，本进程读一次身份还得解析字符串。
func TestWithIdentityCarriesBoth(t *testing.T) {
	id := New(42, 0, time.Now())
	ctx := WithIdentity(context.Background(), id)

	local, ok := IdentityFrom(ctx)
	if !ok || local.UserID != 42 {
		t.Fatalf("本进程读不到身份：got %+v ok=%v", local, ok)
	}
	remote, ok := Extract(ctx)
	if !ok || remote.Issuer != IssuerBFF {
		t.Fatalf("metainfo 里没有身份：got %+v ok=%v", remote, ok)
	}
}

func TestIdentityStringIsReadable(t *testing.T) {
	// 进日志与 span 属性的东西必须是人能读的，别让排障的人对着结构体猜。
	if got := (Identity{UserID: 1001}).String(); got != "user=1001" {
		t.Errorf("String() = %q", got)
	}
	if got := (Identity{UserID: 1001, OperatorID: 7}).String(); got != "user=1001 operator=7" {
		t.Errorf("String() = %q", got)
	}
}

// TestTokenStoreRoundTrip 令牌存储往返（用假 cache，不依赖真 redis）。
func TestTokenStoreRoundTrip(t *testing.T) {
	store := NewTokenStore(&fakeCache{data: map[string]string{}})

	if _, ok := store.Resolve(context.Background(), "absent"); ok {
		t.Fatal("不存在的令牌被解析成功了")
	}
	if err := store.GrantToken(context.Background(), "tok-1", 1001, time.Minute); err != nil {
		t.Fatalf("GrantToken: %v", err)
	}
	got, ok := store.Resolve(context.Background(), "tok-1")
	if !ok || got != 1001 {
		t.Fatalf("Resolve = (%d, %v), want (1001, true)", got, ok)
	}
}

// TestTokenStoreRejectsNonNumeric 缓存里被塞了非数字（别的服务误写同一个键）
// 时不得按 0 放行 —— 0 会被当成"没有身份"，鉴权闸门与"没带身份"混为一谈。
func TestTokenStoreRejectsNonNumeric(t *testing.T) {
	store := NewTokenStore(&fakeCache{data: map[string]string{tokenKey("bad"): "not-a-number"}})
	if _, ok := store.Resolve(context.Background(), "bad"); ok {
		t.Fatal("非数字的 user_id 被接受了")
	}
}

// TestGateAuthenticate 闸门的三条路径：无身份 / 身份过期 / 正常。
func TestGateAuthenticate(t *testing.T) {
	g := Gate{TTL: time.Minute}
	now := time.Now()

	if _, ok := g.Authenticate(context.Background()); ok {
		t.Fatal("没有身份却被放行了 —— 裸调直连域服必须被拒")
	}
	if _, ok := g.Authenticate(WithIdentity(context.Background(), New(1, 0, now.Add(-time.Hour)))); ok {
		t.Fatal("过期身份被放行了")
	}
	identity, ok := g.Authenticate(WithIdentity(context.Background(), New(1001, 7, now)))
	if !ok {
		t.Fatal("正常身份被拒了")
	}
	if identity != "user=1001 operator=7" {
		t.Errorf("identity = %q", identity)
	}
}

// fakeCache 最小 cachex.Cache 实现，只为把鉴权逻辑与 redis 解耦。
//
// 不引真 redis 是刻意的：鉴权的判定逻辑（过期、伪造、半张票据）与存储用什么
// 无关，用真件只会让用例在没底座的机器上一律 Skip —— 那等于没测。
type fakeCache struct {
	data map[string]string
}

func (f *fakeCache) Get(_ context.Context, key string) (string, error) {
	return f.data[key], nil
}

func (f *fakeCache) Set(_ context.Context, key, value string, _ time.Duration) error {
	if f.data == nil {
		f.data = map[string]string{}
	}
	f.data[key] = value
	return nil
}

func (f *fakeCache) Delete(_ context.Context, key string) error {
	delete(f.data, key)
	return nil
}

func (f *fakeCache) Exists(_ context.Context, key string) (bool, error) {
	_, ok := f.data[key]
	return ok, nil
}

func (f *fakeCache) Keys(_ context.Context, _ string) ([]string, error) { return nil, nil }

func (f *fakeCache) Close() error { return nil }

func (f *fakeCache) HealthCheck(_ context.Context) error { return nil }
