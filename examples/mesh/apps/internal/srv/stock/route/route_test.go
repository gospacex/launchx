package route

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gospacex/kvx"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/stock/middleware"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/routing"
)

// recStore 记录限流计数键，用于断言主体口径。
type recStore struct {
	kvx.KVStore
	keys []string
}

func (r *recStore) Incr(_ context.Context, key string) (int64, error) {
	r.keys = append(r.keys, key)
	return 1, nil // 首次计数 → 放行
}
func (r *recStore) Set(context.Context, string, string, time.Duration) error { return nil }

// denyStore 让计数恒超阈值：闸门若真的把请求送进限流器，它必然拒绝。
type denyStore struct{ kvx.KVStore }

func (denyStore) Incr(context.Context, string) (int64, error) { return 1000, nil }
func (denyStore) Set(context.Context, string, string, time.Duration) error {
	return nil
}

// 无身份：按服务维度计数（subject = "svc"），键含本域 namespace 前缀。
func TestAdmitWithoutIdentityCountsPerService(t *testing.T) {
	st := &recStore{}
	g := New(middleware.New(st, nil), nil)

	if !g.Admit(context.Background(), routing.Identity{}) {
		t.Fatal("首次计数应放行")
	}
	if len(st.keys) != 1 {
		t.Fatalf("keys = %v", st.keys)
	}
	if !strings.Contains(st.keys[0], "mesh:stock:rl") {
		t.Fatalf("计数键应含本域 namespace 前缀，实际键 %q", st.keys[0])
	}
	if !strings.Contains(st.keys[0], ":svc:") {
		t.Fatalf("无身份应按 svc 维度计数，实际键 %q", st.keys[0])
	}
}

// 有身份：按用户维度计数（subject = "u<id>"），键含本域 namespace 前缀。
func TestAdmitWithIdentityCountsPerUser(t *testing.T) {
	st := &recStore{}
	g := New(middleware.New(st, nil), nil)

	if !g.Admit(context.Background(), routing.Identity{UserID: 42}) {
		t.Fatal("首次计数应放行")
	}
	if len(st.keys) != 1 {
		t.Fatalf("keys = %v", st.keys)
	}
	if !strings.Contains(st.keys[0], "mesh:stock:rl") {
		t.Fatalf("计数键应含本域 namespace 前缀，实际键 %q", st.keys[0])
	}
	if !strings.Contains(st.keys[0], ":u42:") {
		t.Fatalf("有身份应按 u42 维度计数，实际键 %q", st.keys[0])
	}
}

// 两个主体的计数键必须落在不同桶（否则身份维度形同虚设）。
func TestAdmitSeparatesSubjects(t *testing.T) {
	st := &recStore{}
	g := New(middleware.New(st, nil), nil)
	g.Admit(context.Background(), routing.Identity{UserID: 42})
	g.Admit(context.Background(), routing.Identity{UserID: 43})
	g.Admit(context.Background(), routing.Identity{})

	if len(st.keys) != 3 || st.keys[0] == st.keys[1] || st.keys[1] == st.keys[2] {
		t.Fatalf("不同主体应落到不同计数桶，实际 %v", st.keys)
	}
}

// 拒绝路径：计数恒超阈值时 Admit 必须返回 false（闸门是**真的**，不是恒真）。
//
// 这条用例是 route/ 不退化空目录的判据：恒真实现下它必然变红。
func TestAdmitRejectsWhenOverLimit(t *testing.T) {
	g := New(middleware.New(denyStore{}, nil), nil)
	if g.Admit(context.Background(), routing.Identity{UserID: 42}) {
		t.Fatal("超过窗口上限应拒绝（Admit 不得恒真）")
	}
	if g.Admit(context.Background(), routing.Identity{}) {
		t.Fatal("无身份超限同样应拒绝（服务级桶）")
	}
}

// 限流器缺失（KV 未装配）时放行，且不 panic（fail-open，不因后端抖动打成 429）。
func TestAdmitWithoutLimiter(t *testing.T) {
	g := New(middleware.New(nil, nil), nil)
	if !g.Admit(context.Background(), routing.Identity{}) {
		t.Fatal("限流器缺失应 fail-open 放行")
	}
}
