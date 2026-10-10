package route

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gospacex/kvx"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/middleware"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/routing"
)

// recStore 记录限流计数键，用于断言主体口径。
type recStore struct {
	kvx.KVStore
	keys []string
}

func (r *recStore) Incr(_ context.Context, key string) (int64, error) {
	r.keys = append(r.keys, key)
	return 1, nil
}
func (r *recStore) Set(context.Context, string, string, time.Duration) error { return nil }

// 无身份：按服务维度计数（subject = "svc"）。
func TestAdmitWithoutIdentityCountsPerService(t *testing.T) {
	st := &recStore{}
	g := New(middleware.New(st, nil), nil)

	if !g.Admit(context.Background(), routing.Identity{}) {
		t.Fatal("首次计数应放行")
	}
	if len(st.keys) != 1 {
		t.Fatalf("keys = %v", st.keys)
	}
	if !strings.Contains(st.keys[0], ":svc:") {
		t.Fatalf("无身份应按 svc 维度计数，实际键 %q", st.keys[0])
	}
}

// 有身份：按用户维度计数（subject = "u<id>"）。
func TestAdmitWithIdentityCountsPerUser(t *testing.T) {
	st := &recStore{}
	g := New(middleware.New(st, nil), nil)

	if !g.Admit(context.Background(), routing.Identity{UserID: 7, OperatorID: 3}) {
		t.Fatal("首次计数应放行")
	}
	if len(st.keys) != 1 {
		t.Fatalf("keys = %v", st.keys)
	}
	if !strings.Contains(st.keys[0], ":u7:") {
		t.Fatalf("有身份应按 u7 维度计数，实际键 %q", st.keys[0])
	}
}

// 两个主体的计数键必须落在不同桶（否则身份维度形同虚设）。
func TestAdmitSeparatesSubjects(t *testing.T) {
	st := &recStore{}
	g := New(middleware.New(st, nil), nil)
	g.Admit(context.Background(), routing.Identity{UserID: 7})
	g.Admit(context.Background(), routing.Identity{UserID: 8})
	g.Admit(context.Background(), routing.Identity{})

	if len(st.keys) != 3 || st.keys[0] == st.keys[1] || st.keys[1] == st.keys[2] {
		t.Fatalf("不同主体应落到不同计数桶，实际 %v", st.keys)
	}
}
