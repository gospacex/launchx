// rpc 层单测：入向适配层的行为契约。
//
// 覆盖四条：
//  1. 接口全实现（编译期断言，见 points.go 的 var _ pointspb.Points）；
//  2. 闸门放行 → 委派 handler（业务真的走通，读到账户）；
//  3. 闸门拒绝（超出该主体的窗口上限）→ RateLimited，且不触碰业务；
//  4. 身份非法（metainfo 写坏）→ Unauthorized，且不触碰业务；
//  5. 越权（主体可信但账户非其所有）→ Forbidden（IDOR 防护，2026-10-10 加固）；
//  6. 未接线方法（Earn/Consume/ListLedgers/Report）rpc 层只开 span 后委派
//     handler，由 handler 回显 codes.Unimplemented（Unimplemented 的唯一落点在 handler）。
//
// 另断言四层埋点：rpc.points.GetAccount → handler.points.GetAccount →
// service.points.GetPoints → repository.points.Balance 的父子链
// （spec mesh-layering「四层 span 逐层可见」的可执行判据），以及
// trace.enabled=false 时四层 span 一个都不出现。
package rpc

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bytedance/gopkg/cloud/metainfo"
	"github.com/gospacex/kvx"
	"github.com/gospacex/logx"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/points/handler"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/points/middleware"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/points/model"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/points/repository"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/points/route"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/points/service"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	pointspb "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/points"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/routing"
)

// nopLogger 内嵌接口 + 只覆写 Warn。
type nopLogger struct{ logx.Logger }

func (nopLogger) Warn(string, ...logx.Field) {}

// fakeRepo 结构性满足 service 依赖的（未导出）仓储窄接口。
//
// 写方法只有 CreditOnce（幂等 + 事务收口在仓储内），假件跟着收窄。
type fakeRepo struct {
	balResult *model.PointAccount
	balErr    error
	balCalls  int
}

func (f *fakeRepo) CreditOnce(context.Context, uint64, uint64, int64) (bool, error) {
	return true, nil
}
func (f *fakeRepo) Balance(context.Context, uint64) (*model.PointAccount, error) {
	f.balCalls++
	if f.balErr != nil {
		return nil, f.balErr
	}
	return f.balResult, nil
}

// recStore 记录限流计数键：本域 GetAccount 读路径经闸门**真实**计数
// （route.Admit 走 limiter.Allow），recStore 记下计数键以钉住主体口径
// （见 TestGetAccountAdmitsMetainfoIdentity 对 u7 桶的断言）。
type recStore struct {
	kvx.KVStore
	keys []string
}

func (r *recStore) Incr(_ context.Context, key string) (int64, error) {
	r.keys = append(r.keys, key)
	return 1, nil
}
func (r *recStore) Set(context.Context, string, string, time.Duration) error { return nil }

// denyStore 让计数恒超阈值 → 闸门拒绝（RateLimited 的真实生产者）。
type denyStore struct{ kvx.KVStore }

func (denyStore) Incr(context.Context, string) (int64, error) { return 1000, nil }
func (denyStore) Set(context.Context, string, string, time.Duration) error {
	return nil
}

// newServer 组装入向适配层：nil store → 闸门 fail-open 放行。
func newServer(repo *fakeRepo, traceOn bool) *Server {
	g := route.New(middleware.New(nil, nopLogger{}), nopLogger{})
	svc := service.New(repo, nopLogger{}, traceOn)
	return New(handler.New(svc, traceOn), g, traceOn)
}

// 放行：GetAccount 委派到 handler（业务走通，读到账户）。
func TestGetAccountDelegatesWhenAdmitted(t *testing.T) {
	repo := &fakeRepo{balResult: &model.PointAccount{UserID: 9, Balance: 250}}
	s := newServer(repo, false)

	resp, err := s.GetAccount(context.Background(), &pointspb.GetAccountReq{UserId: 9})
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if resp == nil || resp.Account == nil || resp.Account.Balance != 250 {
		t.Fatalf("resp = %+v", resp)
	}
	if repo.balCalls != 1 {
		t.Fatalf("放行后业务应被委派（repo.Balance 调用 %d 次）", repo.balCalls)
	}
}

// 拒绝：闸门不放行（超出该主体的窗口上限）→ 回 RateLimited，且不触碰业务。
func TestGetAccountRejectedReturnsRateLimited(t *testing.T) {
	repo := &fakeRepo{balResult: &model.PointAccount{UserID: 42, Balance: 250}}
	g := route.New(middleware.New(denyStore{}, nopLogger{}), nopLogger{})
	svc := service.New(repo, nopLogger{}, false)
	s := New(handler.New(svc, false), g, false)

	resp, err := s.GetAccount(context.Background(), &pointspb.GetAccountReq{UserId: 42})
	if resp != nil {
		t.Fatalf("被拒不应返回响应体，实际 %+v", resp)
	}
	if c, _ := codes.From(err); c != codes.RateLimited {
		t.Fatalf("被拒应回 RateLimited，实际 %v (err=%v)", c, err)
	}
	if repo.balCalls != 0 {
		t.Fatal("被拒不得触碰业务（repo 不应被调用）")
	}
}

// 越权（IDOR）：主体可信，但请求的账户不属于该主体 → Forbidden，且不返回响应体。
//
// 这是「主体来自 metainfo / 同进程 ctx」与「来自请求字段回落」两条路径的分水岭：
// 只有前者才触发归属校验（回落值不可信，拿它比等于让调用方自己判自己）。
func TestGetAccountRejectsForeignAccount(t *testing.T) {
	repo := &fakeRepo{balResult: &model.PointAccount{UserID: 42, Balance: 250}}
	s := newServer(repo, false)

	ctx := metainfo.WithValue(context.Background(), identityMetaKey, "7")
	resp, err := s.GetAccount(ctx, &pointspb.GetAccountReq{UserId: 42})
	if resp != nil {
		t.Fatalf("越权不应返回响应体，实际 %+v", resp)
	}
	if c, _ := codes.From(err); c != codes.Forbidden {
		t.Fatalf("越权应回 Forbidden，实际 %v (err=%v)", c, err)
	}
}

// 匿名（无身份、仅请求字段有 user_id）不做归属校验：这一档是 fail-open，
// 由 bff 的入口闸门负责拦（见 routing.WithRequiredIdentity）。
func TestGetAccountAnonymousSkipsOwnership(t *testing.T) {
	repo := &fakeRepo{balResult: &model.PointAccount{UserID: 42, Balance: 250}}
	s := newServer(repo, false)

	resp, err := s.GetAccount(context.Background(), &pointspb.GetAccountReq{UserId: 42})
	if err != nil {
		t.Fatalf("匿名请求按 fail-open 放行，实际 err=%v", err)
	}
	if resp == nil || resp.Account == nil || resp.Account.Balance != 250 {
		t.Fatalf("resp = %+v", resp)
	}
}

// 身份非法（metainfo 写坏）→ Unauthorized：身份头坏了要出声，不得静默降级成匿名，
// 也不得触碰业务。
func TestGetAccountRejectsMalformedIdentity(t *testing.T) {
	for _, bad := range []string{"abc", "0"} {
		t.Run(bad, func(t *testing.T) {
			repo := &fakeRepo{balResult: &model.PointAccount{UserID: 9, Balance: 1}}
			s := newServer(repo, false)

			ctx := metainfo.WithValue(context.Background(), identityMetaKey, bad)
			resp, err := s.GetAccount(ctx, &pointspb.GetAccountReq{UserId: 9})
			if resp != nil {
				t.Fatalf("身份非法不应返回响应体，实际 %+v", resp)
			}
			if c, _ := codes.From(err); c != codes.Unauthorized {
				t.Fatalf("身份非法应回 Unauthorized，实际 %v (err=%v)", c, err)
			}
			if repo.balCalls != 0 {
				t.Fatal("身份非法不得触碰业务（repo 不应被调用）")
			}
		})
	}
}

// 匿名请求（无 metainfo、带 user_id）应放行，不得误判成 401。
func TestGetAccountAdmitsAnonymousRequest(t *testing.T) {
	repo := &fakeRepo{balResult: &model.PointAccount{UserID: 9, Balance: 5}}
	s := newServer(repo, false)
	resp, err := s.GetAccount(context.Background(), &pointspb.GetAccountReq{UserId: 9})
	if err != nil {
		t.Fatalf("匿名请求应放行，实际 err=%v", err)
	}
	if resp == nil || resp.Account == nil {
		t.Fatalf("resp = %+v", resp)
	}
}

// 同进程 ctx 里的 identity 也应被接受（不因解析路径不同而 401）。
func TestGetAccountAdmitsInProcessIdentity(t *testing.T) {
	repo := &fakeRepo{balResult: &model.PointAccount{UserID: 42, Balance: 8}}
	s := newServer(repo, false)
	ctx := routing.WithIdentity(context.Background(), routing.Identity{UserID: 42})
	if _, err := s.GetAccount(ctx, &pointspb.GetAccountReq{UserId: 42}); err != nil {
		t.Fatalf("同进程身份应放行，实际 err=%v", err)
	}
}

// metainfo 里的合法身份（跨进程载体）应被接受，并落到该主体的 u7 桶。
//
// req.user_id 为 0 是刻意的：钉住「metainfo 优先于请求字段」，也覆盖合法解析分支。
func TestGetAccountAdmitsMetainfoIdentity(t *testing.T) {
	st := &recStore{}
	repo := &fakeRepo{balResult: &model.PointAccount{UserID: 7, Balance: 1}}
	g := route.New(middleware.New(st, nopLogger{}), nopLogger{})
	svc := service.New(repo, nopLogger{}, false)
	s := New(handler.New(svc, false), g, false)

	ctx := metainfo.WithValue(context.Background(), identityMetaKey, "7")
	if _, err := s.GetAccount(ctx, &pointspb.GetAccountReq{UserId: 0}); err != nil {
		t.Fatalf("metainfo 身份应放行，实际 err=%v", err)
	}
	if len(st.keys) != 1 || !strings.Contains(st.keys[0], ":u7:") {
		t.Fatalf("metainfo 身份应落到 u7 桶，实际 %v", st.keys)
	}
}

// 未接线方法：rpc 层只开 span 后委派 handler，由 handler 回显 Unimplemented。
//
// 观测手段：注入内存 span recorder，断言 rpc.points.<Op> span 存在
// （旧实现在 rpc 层直接 return、不开 span —— 这正是 RED 的判据）。
func TestUnwiredMethodsDelegateToHandler(t *testing.T) {
	sr, restore := withRecorder(t)
	defer restore()

	s := newServer(&fakeRepo{}, true) // traceOn=true：rpc 层开 span 才可观测

	cases := []struct {
		name string
		call func() (respNil bool, err error)
	}{
		{"Earn", func() (bool, error) {
			resp, err := s.Earn(context.Background(), &pointspb.EarnReq{UserId: 1})
			return resp == nil, err
		}},
		{"Consume", func() (bool, error) {
			resp, err := s.Consume(context.Background(), &pointspb.ConsumeReq{UserId: 1})
			return resp == nil, err
		}},
		{"ListLedgers", func() (bool, error) {
			resp, err := s.ListLedgers(context.Background(), &pointspb.ListLedgersReq{UserId: 1})
			return resp == nil, err
		}},
		{"Report", func() (bool, error) {
			resp, err := s.Report(context.Background(), &pointspb.ReportReq{UserId: 1})
			return resp == nil, err
		}},
	}
	for _, tc := range cases {
		respNil, err := tc.call()
		if !respNil {
			t.Fatalf("%s 不应返回响应体", tc.name)
		}
		if c, _ := codes.From(err); c != codes.Unimplemented {
			t.Fatalf("%s 应回 Unimplemented，实际 %v (err=%v)", tc.name, c, err)
		}
	}

	names := spanNames(sr)
	for _, m := range []string{"Earn", "Consume", "ListLedgers", "Report"} {
		if want := "rpc.points." + m; !slices.Contains(names, want) {
			t.Fatalf("%s 未在 rpc 层开 span/委派，spans=%v", want, names)
		}
	}
}

// traceOn=false：埋点关闭，行为不变、不 panic（返回空收尾闭包）。
func TestTraceOffDoesNotChangeBehavior(t *testing.T) {
	repo := &fakeRepo{balResult: &model.PointAccount{UserID: 9, Balance: 3}}
	s := newServer(repo, false)
	resp, err := s.GetAccount(context.Background(), &pointspb.GetAccountReq{UserId: 9})
	if err != nil || resp == nil || resp.Account.Balance != 3 {
		t.Fatalf("traceOn=false 行为应不变：resp=%+v err=%v", resp, err)
	}
	if repo.balCalls != 1 {
		t.Fatal("traceOn=false 业务仍应被委派")
	}
}

// traceOn=false：四层分层 span 一个都不出现（总闸关闭的代码层判据）。
func TestTraceOffEmitsNoLayerSpans(t *testing.T) {
	sr, restore := withRecorder(t)
	defer restore()

	g := route.New(middleware.New(nil, nopLogger{}), nopLogger{})
	db := newRpcTestDB(t)
	svc := service.New(repository.New(db, false), nopLogger{}, false)
	s := New(handler.New(svc, false), g, false)

	if _, err := s.GetAccount(context.Background(), &pointspb.GetAccountReq{UserId: 9}); err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	for _, n := range spanNames(sr) {
		for _, p := range []string{"rpc.", "handler.", "service.", "repository."} {
			if strings.HasPrefix(n, p) {
				t.Fatalf("traceOn=false 时不应出现分层 span，实际出现 %q", n)
			}
		}
	}
}

// ---------- 四层 span 父子链 ----------

// TestLayerSpanChain 断言 rpc → handler → service → repository 四层 span 的父子链。
//
// 判据不是「四个名字都出现」，而是「每一个的 Parent() 是上一层」。
func TestLayerSpanChain(t *testing.T) {
	sr, restore := withRecorder(t)
	defer restore()

	s := newServerWithRepo(t, newRpcTestDB(t))
	if _, err := s.GetAccount(context.Background(), &pointspb.GetAccountReq{UserId: 9}); err != nil {
		t.Fatalf("GetAccount: %v", err)
	}

	seen := make(map[string]sdktrace.ReadOnlySpan, len(sr.Ended()))
	names := make([]string, 0, len(sr.Ended()))
	for _, sp := range sr.Ended() {
		seen[sp.Name()] = sp
		names = append(names, sp.Name())
	}

	// 链的规范顺序（唯一来源）。service 侧 op 名为 GetPoints（IDL 名为 GetAccount）。
	chain := []string{
		"rpc.points.GetAccount",
		"handler.points.GetAccount",
		"service.points.GetPoints",
		"repository.points.Balance",
	}
	root, ok := seen[chain[0]]
	if !ok {
		t.Fatalf("缺少 span %q，实际=%v", chain[0], names)
	}
	traceID := root.SpanContext().TraceID()
	for i := 1; i < len(chain); i++ {
		parent, ok := seen[chain[i-1]]
		if !ok {
			t.Fatalf("缺少 span %q，实际=%v", chain[i-1], names)
		}
		child, ok := seen[chain[i]]
		if !ok {
			t.Fatalf("缺少 span %q，实际=%v", chain[i], names)
		}
		if got, want := child.Parent().SpanID(), parent.SpanContext().SpanID(); got != want {
			t.Fatalf("%s 的父 span 应为 %s：child.Parent=%s，parent.SpanID=%s（spans=%v）",
				chain[i], chain[i-1], got, want, names)
		}
		if got, want := child.SpanContext().TraceID(), traceID; got != want {
			t.Fatalf("%s 的 TraceID 应与 %s 相同：%s != %s", chain[i], chain[0], got, want)
		}
	}
}

// ---------- helpers ----------

func spanNames(sr *tracetest.SpanRecorder) []string {
	names := make([]string, 0, len(sr.Ended()))
	for _, sp := range sr.Ended() {
		names = append(names, sp.Name())
	}
	return names
}

// withRecorder 注入内存 span recorder，返回 restore 供 defer 复原全局 provider。
func withRecorder(t *testing.T) (*tracetest.SpanRecorder, func()) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	return sr, func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	}
}

// newRpcTestDB 建 sqlite 内存库并迁移两张表（与 repository 层单测同款）。
func newRpcTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.PointAccount{}, &model.PointsLog{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// newServerWithRepo 组装真实四层链路：sqlite 内存库 → repository → service →
// handler → rpc。用真的 *repository.PointsRepository 才会有 repository 层 span。
func newServerWithRepo(t *testing.T, db *gorm.DB) *Server {
	t.Helper()
	g := route.New(middleware.New(nil, nopLogger{}), nopLogger{})
	svc := service.New(repository.New(db, true), nopLogger{}, true)
	return New(handler.New(svc, true), g, true)
}
