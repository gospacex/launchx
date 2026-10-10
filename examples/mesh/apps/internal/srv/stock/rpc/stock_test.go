// rpc 层单测：入向适配层的行为契约。
//
// 覆盖：
//  1. 接口全实现（编译期断言，见 stock.go 的 var _ stockpb.Stock）；
//  2. 闸门放行 → 委派 handler（业务真的走通，读到库存）；
//  3. 闸门拒绝（超出该主体窗口上限）→ RateLimited，且不触碰业务；
//  4. 身份非法（metainfo 写坏）→ Unauthorized，且不触碰业务；
//  5. 未接线方法（ListStocks/ReserveStock/ReleaseStock/ConfirmStock/AdjustStock/
//     Report）rpc 层只开 span 后委派 handler，由 handler 回显 codes.Unimplemented。
//
// 另断言四层埋点：rpc.stock.GetStock → handler.stock.GetStock →
// service.stock.GetStock → repository.stock.FindByProductID 的父子链。
//
// 本域与 order/points/product 的差异：GetStockReq **不带 user_id**，故 admit
// 没有「回落到本次请求 IDL 的 user_id」这一级（见 stock.go 的 admit 注释）。
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

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/stock/handler"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/stock/middleware"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/stock/model"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/stock/repository"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/stock/route"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/stock/service"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	stockpb "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/stock"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/routing"
)

// nopLogger 内嵌接口 + 只覆写 Warn。
type nopLogger struct{ logx.Logger }

func (nopLogger) Warn(string, ...logx.Field) {}

// fakeRepo 结构性满足 service 依赖的（未导出）仓储窄接口。
type fakeRepo struct {
	findResult *model.Stock
	findErr    error
	findCalls  int
}

func (f *fakeRepo) FindByProductID(context.Context, uint64) (*model.Stock, error) {
	f.findCalls++
	if f.findErr != nil {
		return nil, f.findErr
	}
	return f.findResult, nil
}
func (f *fakeRepo) TryDecrease(context.Context, uint64, int32) (bool, error) { return false, nil }

// recStore 记录限流计数键：GetStock 读路径经闸门**真实**计数（route.Admit 走
// limiter.Allow），记下计数键以钉住主体口径。
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

// repoIface 与 service 依赖的（未导出）仓储窄接口方法集一致；重声明一份好让
// newServer 既能收假 *fakeRepo，也能收真 *repository.StockRepository。
type repoIface interface {
	FindByProductID(context.Context, uint64) (*model.Stock, error)
	TryDecrease(context.Context, uint64, int32) (bool, error)
}

// newServer 组装入向适配层：nil store → 闸门 fail-open 放行。
func newServer(repo repoIface, traceOn bool) *Server {
	g := route.New(middleware.New(nil, nopLogger{}), nopLogger{})
	svc := service.New(repo, nil, nil, nopLogger{}, traceOn)
	return New(handler.New(svc, traceOn), g, traceOn)
}

// 放行：GetStock 委派到 handler（业务走通，读到库存）。
func TestGetStockDelegatesWhenAdmitted(t *testing.T) {
	repo := &fakeRepo{findResult: &model.Stock{ProductID: 5, Total: 100, Available: 42, Version: 3}}
	s := newServer(repo, false)

	resp, err := s.GetStock(context.Background(), &stockpb.GetStockReq{ProductId: 5})
	if err != nil {
		t.Fatalf("GetStock: %v", err)
	}
	if resp == nil || resp.Stock == nil || resp.Stock.Available != 42 || resp.ProductId != 5 {
		t.Fatalf("resp = %+v", resp)
	}
	if repo.findCalls != 1 {
		t.Fatalf("放行后业务应被委派（repo.FindByProductID 调用 %d 次）", repo.findCalls)
	}
}

// 无库存记录：service 视为 0（不报错），经 handler / rpc 返回零值视图。
// 本域请求不带 user_id，也无 StockNotFound 码 —— 读路径「无记录=0」是刻意的。
func TestGetStockNotFoundIsZeroView(t *testing.T) {
	s := newServer(&fakeRepo{findErr: gorm.ErrRecordNotFound}, false)
	resp, err := s.GetStock(context.Background(), &stockpb.GetStockReq{ProductId: 404})
	if err != nil {
		t.Fatalf("无库存记录不应报错，实际 %v", err)
	}
	if resp == nil || resp.Stock == nil {
		t.Fatalf("应返回零值视图，实际 %+v", resp)
	}
	if resp.ProductId != 404 || resp.Stock.Available != 0 {
		t.Fatalf("零值视图应保留 product_id、available=0，实际 %+v", resp.Stock)
	}
}

// 拒绝：闸门不放行（超出该主体窗口上限）→ 回 RateLimited，且不触碰业务。
func TestGetStockRejectedReturnsRateLimited(t *testing.T) {
	repo := &fakeRepo{findResult: &model.Stock{ProductID: 42}}
	g := route.New(middleware.New(denyStore{}, nopLogger{}), nopLogger{})
	svc := service.New(repo, nil, nil, nopLogger{}, false)
	s := New(handler.New(svc, false), g, false)

	resp, err := s.GetStock(context.Background(), &stockpb.GetStockReq{ProductId: 42})
	if resp != nil {
		t.Fatalf("被拒不应返回响应体，实际 %+v", resp)
	}
	if c, _ := codes.From(err); c != codes.RateLimited {
		t.Fatalf("被拒应回 RateLimited，实际 %v (err=%v)", c, err)
	}
	if repo.findCalls != 0 {
		t.Fatal("被拒不得触碰业务（repo 不应被调用）")
	}
}

// 身份非法（metainfo 写坏）→ Unauthorized：身份头坏了要出声，不得静默降级成匿名。
//
// 本域**没有**「回落请求里的 user_id」这一级（IDL 不带 user_id），故写坏 =
// 必然 401，不存在被请求字段兜住的可能。
func TestGetStockRejectsMalformedIdentity(t *testing.T) {
	for _, bad := range []string{"abc", "0"} {
		t.Run(bad, func(t *testing.T) {
			repo := &fakeRepo{findResult: &model.Stock{ProductID: 9}}
			s := newServer(repo, false)

			ctx := metainfo.WithValue(context.Background(), identityMetaKey, bad)
			resp, err := s.GetStock(ctx, &stockpb.GetStockReq{ProductId: 9})
			if resp != nil {
				t.Fatalf("身份非法不应返回响应体，实际 %+v", resp)
			}
			if c, _ := codes.From(err); c != codes.Unauthorized {
				t.Fatalf("身份非法应回 Unauthorized，实际 %v (err=%v)", c, err)
			}
			if repo.findCalls != 0 {
				t.Fatal("身份非法不得触碰业务（repo 不应被调用）")
			}
		})
	}
}

// 匿名请求（无 metainfo）应放行，落 "svc" 桶，不得误判成 401。
func TestGetStockAdmitsAnonymousRequest(t *testing.T) {
	st := &recStore{}
	repo := &fakeRepo{findResult: &model.Stock{ProductID: 9}}
	g := route.New(middleware.New(st, nopLogger{}), nopLogger{})
	svc := service.New(repo, nil, nil, nopLogger{}, false)
	s := New(handler.New(svc, false), g, false)

	resp, err := s.GetStock(context.Background(), &stockpb.GetStockReq{ProductId: 9})
	if err != nil {
		t.Fatalf("匿名请求应放行，实际 err=%v", err)
	}
	if resp == nil {
		t.Fatal("应返回响应体")
	}
	if len(st.keys) != 1 || !strings.Contains(st.keys[0], ":svc:") {
		t.Fatalf("匿名请求应落 svc 桶，实际 %v", st.keys)
	}
}

// 同进程 ctx 里的 identity 也应被接受（不因解析路径不同而 401）。
func TestGetStockAdmitsInProcessIdentity(t *testing.T) {
	repo := &fakeRepo{findResult: &model.Stock{ProductID: 42}}
	s := newServer(repo, false)
	ctx := routing.WithIdentity(context.Background(), routing.Identity{UserID: 42})
	if _, err := s.GetStock(ctx, &stockpb.GetStockReq{ProductId: 42}); err != nil {
		t.Fatalf("同进程身份应放行，实际 err=%v", err)
	}
}

// metainfo 里的合法身份（跨进程载体）应被接受，并落到 u7 桶。
func TestGetStockAdmitsMetainfoIdentity(t *testing.T) {
	st := &recStore{}
	repo := &fakeRepo{findResult: &model.Stock{ProductID: 7}}
	g := route.New(middleware.New(st, nopLogger{}), nopLogger{})
	svc := service.New(repo, nil, nil, nopLogger{}, false)
	s := New(handler.New(svc, false), g, false)

	ctx := metainfo.WithValue(context.Background(), identityMetaKey, "7")
	if _, err := s.GetStock(ctx, &stockpb.GetStockReq{ProductId: 7}); err != nil {
		t.Fatalf("metainfo 身份应放行，实际 err=%v", err)
	}
	if len(st.keys) != 1 || !strings.Contains(st.keys[0], ":u7:") {
		t.Fatalf("metainfo 身份应落到 u7 桶，实际 %v", st.keys)
	}
}

// 未接线方法：rpc 层只开 span 后委派 handler，由 handler 回显 Unimplemented。
func TestUnwiredMethodsDelegateToHandler(t *testing.T) {
	sr, restore := withRecorder(t)
	defer restore()

	s := newServer(&fakeRepo{}, true) // traceOn=true：rpc 层开 span 才可观测

	cases := []struct {
		name string
		call func() (respNil bool, err error)
	}{
		{"ListStocks", func() (bool, error) {
			resp, err := s.ListStocks(context.Background(), &stockpb.ListStocksReq{Limit: 10})
			return resp == nil, err
		}},
		{"ReserveStock", func() (bool, error) {
			resp, err := s.ReserveStock(context.Background(), &stockpb.ReserveStockReq{ProductId: 1, Quantity: 1})
			return resp == nil, err
		}},
		{"ReleaseStock", func() (bool, error) {
			resp, err := s.ReleaseStock(context.Background(), &stockpb.ReleaseStockReq{ProductId: 1, Quantity: 1})
			return resp == nil, err
		}},
		{"ConfirmStock", func() (bool, error) {
			resp, err := s.ConfirmStock(context.Background(), &stockpb.ConfirmStockReq{ProductId: 1, Quantity: 1})
			return resp == nil, err
		}},
		{"AdjustStock", func() (bool, error) {
			resp, err := s.AdjustStock(context.Background(), &stockpb.AdjustStockReq{ProductId: 1, Delta: 5})
			return resp == nil, err
		}},
		{"Report", func() (bool, error) {
			resp, err := s.Report(context.Background(), &stockpb.ReportReq{Limit: 10})
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
	for _, m := range []string{"ListStocks", "ReserveStock", "ReleaseStock", "ConfirmStock", "AdjustStock", "Report"} {
		if want := "rpc.stock." + m; !slices.Contains(names, want) {
			t.Fatalf("%s 未在 rpc 层开 span/委派，spans=%v", want, names)
		}
	}
}

// traceOn=false：埋点关闭，行为不变、不 panic（返回空收尾闭包）。
func TestTraceOffDoesNotChangeBehavior(t *testing.T) {
	repo := &fakeRepo{findResult: &model.Stock{ProductID: 9, Available: 3}}
	s := newServer(repo, false)
	resp, err := s.GetStock(context.Background(), &stockpb.GetStockReq{ProductId: 9})
	if err != nil || resp == nil || resp.Stock.Available != 3 {
		t.Fatalf("traceOn=false 行为应不变：resp=%+v err=%v", resp, err)
	}
	if repo.findCalls != 1 {
		t.Fatal("traceOn=false 业务仍应被委派")
	}
}

// traceOn=false：四层分层 span 一个都不出现（总闸关闭的代码层判据）。
func TestTraceOffEmitsNoLayerSpans(t *testing.T) {
	sr, restore := withRecorder(t)
	defer restore()

	g := route.New(middleware.New(nil, nopLogger{}), nopLogger{})
	db := newRpcTestDB(t)
	seed(t, db, model.Stock{ProductID: 9, Total: 5, Available: 5})
	svc := service.New(repository.New(db, false), nil, nil, nopLogger{}, false)
	s := New(handler.New(svc, false), g, false)

	if _, err := s.GetStock(context.Background(), &stockpb.GetStockReq{ProductId: 9}); err != nil {
		t.Fatalf("GetStock: %v", err)
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

func TestLayerSpanChain(t *testing.T) {
	sr, restore := withRecorder(t)
	defer restore()

	db := newRpcTestDB(t)
	seed(t, db, model.Stock{ProductID: 9, Total: 5, Available: 5})
	s := newServerWithRepo(t, db)
	if _, err := s.GetStock(context.Background(), &stockpb.GetStockReq{ProductId: 9}); err != nil {
		t.Fatalf("GetStock: %v", err)
	}

	seen := make(map[string]sdktrace.ReadOnlySpan, len(sr.Ended()))
	names := make([]string, 0, len(sr.Ended()))
	for _, sp := range sr.Ended() {
		seen[sp.Name()] = sp
		names = append(names, sp.Name())
	}

	// 链的规范顺序（唯一来源）。本域 op 名：四层同名 GetStock，最内层 FindByProductID。
	chain := []string{
		"rpc.stock.GetStock",
		"handler.stock.GetStock",
		"service.stock.GetStock",
		"repository.stock.FindByProductID",
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

func newRpcTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&model.Stock{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

func seed(t *testing.T, db *gorm.DB, s model.Stock) {
	t.Helper()
	if err := db.Create(&s).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// newServerWithRepo 组装真实四层链路：sqlite 内存库 → repository → service →
// handler → rpc。用真的 *repository.StockRepository 才会有 repository 层 span。
func newServerWithRepo(t *testing.T, db *gorm.DB) *Server {
	t.Helper()
	g := route.New(middleware.New(nil, nopLogger{}), nopLogger{})
	svc := service.New(repository.New(db, true), nil, nil, nopLogger{}, true)
	return New(handler.New(svc, true), g, true)
}
