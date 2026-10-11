// rpc 层单测：入向适配层的行为契约。
//
// 覆盖四条：
//  1. 接口全实现（编译期断言，见 order.go 的 var _ orderpb.Order）；
//  2. 闸门放行 → 委派 handler（业务真的走通）；
//  3. 闸门拒绝 → 返回 codes.RateLimited 且不触碰业务；
//  4. 未接线方法（ListOrders / Report）rpc 层只开 span 后委派 handler，
//     由 handler 回显 codes.Unimplemented（Unimplemented 的唯一落点在 handler）；
//  5. traceOn=false 时不 panic、行为不变，且四层分层 span 一个都不出现
//     （埋点关闭，layer.Begin 返回空收尾闭包，调用方一律 defer）。
//
// 另覆盖 admit 的身份四级回落（同进程 identity → metainfo → req.user_id → "svc" 桶）
// 与身份非法分支：限流主体的来源只有这一处，错位的后果是闸门退化成服务级限流；
// 而「带了非法身份」（解析失败 / 为 0）必须回 Unauthorized，不得静默降级成匿名
// （那是把故障伪装成容量问题）。
//
// 最后一条 TestLayerSpanChain 断言 rpc → handler → service → repository 四层 span
// 的父子链（spec mesh-layering「四层 span 逐层可见」的可执行判据）。
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

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/handler"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/middleware"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/model"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/repository"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/route"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/service"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	orderpb "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/order"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/routing"
	"github.com/gospacex/launchx/examples/pkg/outbox"
)

// nopLogger 内嵌接口 + 只覆写 Warn：service / route 在这两条路径上只用 Warn。
type nopLogger struct{ logx.Logger }

func (nopLogger) Warn(string, ...logx.Field) {}

// fakeRepo 结构性满足 service 依赖的（未导出）仓储窄接口。
//
// owner 是「FindByID 返回的订单归属用户」，供归属校验（IDOR 防护）用例构造
// 「订单属于某个主体」的场景：GetOrder 在主体可信时会比对 resp.user_id 与主体，
// owner 不匹配即 403。
type fakeRepo struct {
	created   *model.Order
	createErr error
	owner     uint64
}

func (f *fakeRepo) Create(_ context.Context, o *model.Order, _ []model.OrderItem,
	_ func(orderID uint64) (payload, traceparent string)) error {
	if f.createErr != nil {
		return f.createErr
	}
	o.ID = 777
	f.created = o
	return nil
}
func (f *fakeRepo) FindByID(_ context.Context, id uint64) (*model.Order, error) {
	if f.created != nil {
		return f.created, nil
	}
	return &model.Order{ID: id, UserID: f.owner, OrderNo: "M-X", Status: model.StatusCreated}, nil
}
func (f *fakeRepo) ItemsByOrderID(context.Context, uint64) ([]model.OrderItem, error) {
	return nil, nil
}
func (f *fakeRepo) UpdateStatus(context.Context, uint64, string, string) error { return nil }

// recStore 记录限流计数键（用于断言限流主体口径）。
type recStore struct {
	kvx.KVStore
	keys []string
}

func (r *recStore) Incr(_ context.Context, key string) (int64, error) {
	r.keys = append(r.keys, key)
	return 1, nil // 首次计数 → 放行
}
func (r *recStore) Set(context.Context, string, string, time.Duration) error { return nil }

// denyStore 让计数恒超阈值 → 闸门拒绝。
type denyStore struct{ kvx.KVStore }

func (denyStore) Incr(context.Context, string) (int64, error) { return 1000, nil }
func (denyStore) Set(context.Context, string, string, time.Duration) error {
	return nil
}

// newServer 组装入向适配层：恒放行的闸门（nil store → fail-open）。
func newServer(repo *fakeRepo, traceOn bool) *Server {
	g := route.New(middleware.New(nil, nopLogger{}), nopLogger{})
	svc := service.New(repo, nil, nil, nopLogger{}, traceOn)
	return New(handler.New(svc, traceOn), g, traceOn)
}

// 放行：CreateOrder 委派到 handler（业务走通，落库被驱动到）。
func TestCreateOrderDelegatesWhenAdmitted(t *testing.T) {
	repo := &fakeRepo{}
	s := newServer(repo, false)

	resp, err := s.CreateOrder(context.Background(), &orderpb.CreateOrderReq{
		UserId: 9,
		Items:  []*orderpb.OrderItem{{ProductId: 11, Name: "a", Quantity: 2, UnitPrice: 100}},
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if resp == nil || resp.OrderId != 777 {
		t.Fatalf("resp = %+v", resp)
	}
	if repo.created == nil {
		t.Fatal("放行后业务应被委派（repo 未收到 Create）")
	}
}

// 拒绝：闸门不放行 → 回 RateLimited，且不进入 handler / service / repo。
func TestCreateOrderRejectedReturnsRateLimited(t *testing.T) {
	repo := &fakeRepo{}
	g := route.New(middleware.New(denyStore{}, nopLogger{}), nopLogger{})
	svc := service.New(repo, nil, nil, nopLogger{}, false)
	s := New(handler.New(svc, false), g, false)

	resp, err := s.CreateOrder(context.Background(), &orderpb.CreateOrderReq{
		UserId: 9,
		Items:  []*orderpb.OrderItem{{ProductId: 11, Name: "a", Quantity: 1, UnitPrice: 100}},
	})
	if resp != nil {
		t.Fatalf("被拒不应返回响应体，实际 %+v", resp)
	}
	if c, _ := codes.From(err); c != codes.RateLimited {
		t.Fatalf("被拒应回 RateLimited，实际 %v (err=%v)", c, err)
	}
	if repo.created != nil {
		t.Fatal("被拒不得触碰业务（repo 不应收到 Create）")
	}
}

// GetOrder 被拒：同样回 RateLimited（闸门对所有已接线方法一致）。
func TestGetOrderRejectedReturnsRateLimited(t *testing.T) {
	g := route.New(middleware.New(denyStore{}, nopLogger{}), nopLogger{})
	svc := service.New(&fakeRepo{}, nil, nil, nopLogger{}, false)
	s := New(handler.New(svc, false), g, false)

	_, err := s.GetOrder(context.Background(), &orderpb.GetOrderReq{UserId: 1, OrderId: 1})
	if c, _ := codes.From(err); c != codes.RateLimited {
		t.Fatalf("被拒应回 RateLimited，实际 %v (err=%v)", c, err)
	}
}

// GetOrder 放行：委派 handler 并返回响应。
func TestGetOrderDelegatesWhenAdmitted(t *testing.T) {
	s := newServer(&fakeRepo{created: &model.Order{ID: 5, UserID: 3, OrderNo: "M-5",
		Status: model.StatusPaid}}, false)

	resp, err := s.GetOrder(context.Background(), &orderpb.GetOrderReq{UserId: 3, OrderId: 5})
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if resp == nil || resp.OrderId != 5 || resp.OrderNo != "M-5" {
		t.Fatalf("resp = %+v", resp)
	}
}

// 未接线方法：rpc 层只开 span 后委派 handler，由 handler 回显 Unimplemented
// （不是 panic，也不是空 resp,nil；Unimplemented 的唯一落点在 handler）。
//
// 观测手段：注入内存 span recorder。旧实现在 rpc 层直接 return、不开 span，
// 因此下面「存在 rpc.order.<Op> span」的断言在旧实现上必然失败 —— 这正是 RED。
func TestUnwiredMethodsDelegateToHandler(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})

	s := newServer(&fakeRepo{}, true) // traceOn=true：rpc 层开 span 才可观测

	if resp, err := s.ListOrders(context.Background(), &orderpb.ListOrdersReq{UserId: 1}); resp != nil {
		t.Fatalf("ListOrders 不应返回响应体，实际 %+v", resp)
	} else if c, _ := codes.From(err); c != codes.Unimplemented {
		t.Fatalf("ListOrders 应回 Unimplemented，实际 %v (err=%v)", c, err)
	}

	if resp, err := s.Report(context.Background(), &orderpb.ReportReq{UserId: 1}); resp != nil {
		t.Fatalf("Report 不应返回响应体，实际 %+v", resp)
	} else if c, _ := codes.From(err); c != codes.Unimplemented {
		t.Fatalf("Report 应回 Unimplemented，实际 %v (err=%v)", c, err)
	}

	names := make([]string, 0, len(sr.Ended()))
	for _, sp := range sr.Ended() {
		names = append(names, sp.Name())
	}
	if !slices.Contains(names, "rpc.order.ListOrders") {
		t.Fatalf("ListOrders 未在 rpc 层开 span/委派，spans=%v", names)
	}
	if !slices.Contains(names, "rpc.order.Report") {
		t.Fatalf("Report 未在 rpc 层开 span/委派，spans=%v", names)
	}
}

// traceOn=false：埋点关闭，行为不变、不 panic（返回空收尾闭包）。
func TestTraceOffDoesNotChangeBehavior(t *testing.T) {
	repo := &fakeRepo{}
	s := newServer(repo, false)

	resp, err := s.CreateOrder(context.Background(), &orderpb.CreateOrderReq{
		UserId: 1,
		Items:  []*orderpb.OrderItem{{ProductId: 1, Name: "a", Quantity: 1, UnitPrice: 1}},
	})
	if err != nil || resp == nil || resp.OrderId != 777 {
		t.Fatalf("traceOn=false 行为应不变：resp=%+v err=%v", resp, err)
	}
}

// traceOn=true：开 span 路径同样不改变行为、不 panic。
func TestTraceOnDoesNotChangeBehavior(t *testing.T) {
	repo := &fakeRepo{}
	s := newServer(repo, true)

	resp, err := s.CreateOrder(context.Background(), &orderpb.CreateOrderReq{
		UserId: 1,
		Items:  []*orderpb.OrderItem{{ProductId: 1, Name: "a", Quantity: 1, UnitPrice: 1}},
	})
	if err != nil || resp == nil || resp.OrderId != 777 {
		t.Fatalf("traceOn=true 行为应不变：resp=%+v err=%v", resp, err)
	}
	if repo.created == nil {
		t.Fatal("traceOn=true 业务仍应被委派")
	}
}

// traceOn=false：四层分层 span 一个都不出现（总闸关闭的代码层判据）。
//
// 只断言「四层业务 span 缺席」，不断言 recorder 为空 —— 组件级 span（db.query /
// messaging.*）由 trace.on.* 那一套开关单独管，不属于本判据（两套开关的分工见
// mesh-app-layering）。四层 span 名一律以 <layer>. 开头，故按前缀判。
func TestTraceOffEmitsNoLayerSpans(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})

	g := route.New(middleware.New(nil, nopLogger{}), nopLogger{})
	svc := service.New(repository.New(newRpcTestDB(t), false), nil, nil, nopLogger{}, false)
	s := New(handler.New(svc, false), g, false)

	if _, err := s.CreateOrder(context.Background(), &orderpb.CreateOrderReq{
		UserId: 9,
		Items:  []*orderpb.OrderItem{{ProductId: 11, Name: "a", Quantity: 2, UnitPrice: 100}},
	}); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	names := make([]string, 0, len(sr.Ended()))
	for _, sp := range sr.Ended() {
		names = append(names, sp.Name())
	}
	for _, n := range names {
		for _, p := range []string{"rpc.", "handler.", "service.", "repository."} {
			if strings.HasPrefix(n, p) {
				t.Fatalf("traceOn=false 时不应出现分层 span，实际出现 %q（全部 spans=%v）", n, names)
			}
		}
	}
}

// newRpcTestDB 建 sqlite 内存库并迁移四张表（与 repository 层单测同款，含 outbox）。
// 父子链单测需要真实 repository（它才会开 repository.order.Create span）。
func newRpcTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&model.Order{}, &model.OrderItem{}, &model.OrderStatusLog{}, &outbox.Event{},
	); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// newServerWithRepo 组装真实四层链路：sqlite 内存库 → repository → service →
// handler → rpc。与 newServer 的差别是这里用真的 *repository.OrderRepository
// （而非 fakeRepo）—— 否则 repository 层 span 不出现，链断在 service→repository。
func newServerWithRepo(t *testing.T, db *gorm.DB) *Server {
	t.Helper()
	g := route.New(middleware.New(nil, nopLogger{}), nopLogger{})
	svc := service.New(repository.New(db, true), nil, nil, nopLogger{}, true)
	return New(handler.New(svc, true), g, true)
}

// TestLayerSpanChain 断言 rpc → handler → service → repository 四层 span 的父子链。
//
// 判据不是「四个名字都出现」，而是「每一个的 Parent() 是上一层」—— 少了这条，
// 四层各自成根（或挂在 kitex 那个不落地的 span 上）同样能通过「名字都在」的检查，
// 而那正是 rpcx 包注释里记着的「span 树在调用边界断成两棵子树」那类故障。
func TestLayerSpanChain(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})

	s := newServerWithRepo(t, newRpcTestDB(t))
	if _, err := s.CreateOrder(context.Background(), &orderpb.CreateOrderReq{
		UserId: 9,
		Items:  []*orderpb.OrderItem{{ProductId: 11, Name: "a", Quantity: 2, UnitPrice: 100}},
	}); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	seen := make(map[string]sdktrace.ReadOnlySpan, len(sr.Ended()))
	names := make([]string, 0, len(sr.Ended()))
	for _, sp := range sr.Ended() {
		seen[sp.Name()] = sp
		names = append(names, sp.Name())
	}

	// 链的规范顺序（唯一来源）。
	chain := []string{
		"rpc.order.CreateOrder",
		"handler.order.CreateOrder",
		"service.order.CreateOrder",
		"repository.order.Create",
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

// -------- admit 的身份四级回落（限流主体口径） --------

// 同进程 ctx 里的 identity 最优先（单测 / 直连场景）。
//
// 它的值过不了进程边界，所以线上这条通常是空的 —— 正因如此它最容易在重构里
// 被误删而不被发现，必须有用例钉住。
//
// 订单 owner 设为 42：主体可信时 GetOrder 会执行归属校验（2026-10-10 IDOR 加固），
// owner 与主体一致才放行 —— 这条用例的落点是**限流分桶**，故让归属校验通过。
func TestAdmitUsesInProcessIdentity(t *testing.T) {
	st := &recStore{}
	g := route.New(middleware.New(st, nopLogger{}), nopLogger{})
	svc := service.New(&fakeRepo{owner: 42}, nil, nil, nopLogger{}, false)
	s := New(handler.New(svc, false), g, false)

	ctx := routing.WithIdentity(context.Background(), routing.Identity{UserID: 42})
	if _, err := s.GetOrder(ctx, &orderpb.GetOrderReq{UserId: 0, OrderId: 1}); err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if len(st.keys) != 1 || !strings.Contains(st.keys[0], ":u42:") {
		t.Fatalf("同进程身份应落到 u42 桶，实际 %v", st.keys)
	}
}

// metainfo 里的身份优先（跨进程载体），req.user_id 为 0 也应落到 u7 桶。
//
// owner 设为 7，理由同 TestAdmitUsesInProcessIdentity（让归属校验通过）。
func TestAdmitUsesMetainfoIdentity(t *testing.T) {
	st := &recStore{}
	g := route.New(middleware.New(st, nopLogger{}), nopLogger{})
	svc := service.New(&fakeRepo{owner: 7}, nil, nil, nopLogger{}, false)
	s := New(handler.New(svc, false), g, false)

	ctx := metainfo.WithValue(context.Background(), identityMetaKey, "7")
	if _, err := s.GetOrder(ctx, &orderpb.GetOrderReq{UserId: 0, OrderId: 1}); err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if len(st.keys) != 1 || !strings.Contains(st.keys[0], ":u7:") {
		t.Fatalf("metainfo 身份应落到 u7 桶，实际 %v", st.keys)
	}
}

// metainfo 缺失 → 回落 req.user_id。
func TestAdmitFallsBackToRequestUserID(t *testing.T) {
	st := &recStore{}
	g := route.New(middleware.New(st, nopLogger{}), nopLogger{})
	svc := service.New(&fakeRepo{}, nil, nil, nopLogger{}, false)
	s := New(handler.New(svc, false), g, false)

	if _, err := s.GetOrder(context.Background(), &orderpb.GetOrderReq{UserId: 42, OrderId: 1}); err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if len(st.keys) != 1 || !strings.Contains(st.keys[0], ":u42:") {
		t.Fatalf("无 metainfo 应回落 req.user_id 到 u42 桶，实际 %v", st.keys)
	}
}

// metainfo 缺失且 req.user_id 缺失 → 服务级 "svc" 桶（不得 panic，也不得误判成 401）。
//
// 注意这里是「没带身份」（metainfo 键不存在）：apps 的鉴权口径把传输层身份下沉给
// Istio，应用侧对匿名请求 fail-open 按服务级计数 —— 只有「带了但非法」才 401。
func TestAdmitFallsBackToServiceBucket(t *testing.T) {
	st := &recStore{}
	g := route.New(middleware.New(st, nopLogger{}), nopLogger{})
	svc := service.New(&fakeRepo{}, nil, nil, nopLogger{}, false)
	s := New(handler.New(svc, false), g, false)

	if _, err := s.GetOrder(context.Background(), &orderpb.GetOrderReq{UserId: 0, OrderId: 1}); err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if len(st.keys) != 1 || !strings.Contains(st.keys[0], ":svc:") {
		t.Fatalf("身份全缺应落服务级 svc 桶，实际 %v", st.keys)
	}
}

// -------- 身份非法的拒绝路径（Unauthorized 的真实生产者） --------

// metainfo 带坏了身份（解析失败 / 为 0）→ Unauthorized：身份头坏了要出声，
// 不得静默降级成匿名，也不得进闸门计数 / 触碰业务。
func TestCreateOrderRejectsMalformedIdentity(t *testing.T) {
	for _, bad := range []string{"abc", "0"} {
		t.Run(bad, func(t *testing.T) {
			st := &recStore{}
			g := route.New(middleware.New(st, nopLogger{}), nopLogger{})
			repo := &fakeRepo{}
			svc := service.New(repo, nil, nil, nopLogger{}, false)
			s := New(handler.New(svc, false), g, false)

			ctx := metainfo.WithValue(context.Background(), identityMetaKey, bad)
			resp, err := s.CreateOrder(ctx, &orderpb.CreateOrderReq{
				UserId: 9,
				Items:  []*orderpb.OrderItem{{ProductId: 1, Name: "a", Quantity: 1, UnitPrice: 1}},
			})
			if resp != nil {
				t.Fatalf("身份非法不应返回响应体，实际 %+v", resp)
			}
			if c, _ := codes.From(err); c != codes.Unauthorized {
				t.Fatalf("身份非法应回 Unauthorized，实际 %v (err=%v)", c, err)
			}
			if len(st.keys) != 0 {
				t.Fatalf("身份非法不应进闸门计数，实际 %v", st.keys)
			}
			if repo.created != nil {
				t.Fatal("身份非法不得触碰业务（repo 不应收到 Create）")
			}
		})
	}
}

// metainfo 缺失 + req.user_id>0 → 正常放行（按 user_id 分桶），不得误判成 401。
// 防的是「把匿名请求也拒了」—— 那会把 fail-open 的减法做成了拒绝。
func TestCreateOrderAdmitsAnonymousRequest(t *testing.T) {
	st := &recStore{}
	g := route.New(middleware.New(st, nopLogger{}), nopLogger{})
	repo := &fakeRepo{}
	svc := service.New(repo, nil, nil, nopLogger{}, false)
	s := New(handler.New(svc, false), g, false)

	resp, err := s.CreateOrder(context.Background(), &orderpb.CreateOrderReq{
		UserId: 9,
		Items:  []*orderpb.OrderItem{{ProductId: 1, Name: "a", Quantity: 1, UnitPrice: 1}},
	})
	if err != nil {
		t.Fatalf("匿名请求（无 metainfo、带 user_id）应放行，实际 err=%v", err)
	}
	if resp == nil || resp.OrderId != 777 {
		t.Fatalf("resp = %+v", resp)
	}
	if len(st.keys) != 1 || !strings.Contains(st.keys[0], ":u9:") {
		t.Fatalf("应回落 req.user_id 到 u9 桶，实际 %v", st.keys)
	}
}

// -------- 归属校验（IDOR 防护，Forbidden 的真实生产者） --------

// 主体可信但订单不属于它 → Forbidden（403）。
//
// 2026-10-10 企业级加固：此前 GetOrder 只校验 id 是正整数，任何人可读任意订单
// （BOLA/IDOR）。现在主体可信时比对订单归属。这条用例钉住「读路径也有授权」。
func TestGetOrderRejectsForeignOrder(t *testing.T) {
	st := &recStore{}
	g := route.New(middleware.New(st, nopLogger{}), nopLogger{})
	svc := service.New(&fakeRepo{owner: 9}, nil, nil, nopLogger{}, false) // 订单属于 9
	s := New(handler.New(svc, false), g, false)

	ctx := routing.WithIdentity(context.Background(), routing.Identity{UserID: 7}) // 主体是 7
	resp, err := s.GetOrder(ctx, &orderpb.GetOrderReq{OrderId: 5})
	if resp != nil {
		t.Fatalf("归属不符不应返回响应体，实际 %+v", resp)
	}
	if c, _ := codes.From(err); c != codes.Forbidden {
		t.Fatalf("读他人订单应回 Forbidden，实际 %v (err=%v)", c, err)
	}
}

// 主体可信、订单归属一致 → 放行（反向断言：别把归属校验做成一律拒绝）。
func TestGetOrderAdmitsOwnOrder(t *testing.T) {
	st := &recStore{}
	g := route.New(middleware.New(st, nopLogger{}), nopLogger{})
	svc := service.New(&fakeRepo{owner: 7}, nil, nil, nopLogger{}, false)
	s := New(handler.New(svc, false), g, false)

	ctx := routing.WithIdentity(context.Background(), routing.Identity{UserID: 7})
	resp, err := s.GetOrder(ctx, &orderpb.GetOrderReq{OrderId: 5})
	if err != nil {
		t.Fatalf("读自己的订单应放行，实际 %v", err)
	}
	if resp == nil || resp.GetUserId() != 7 {
		t.Fatalf("resp = %+v", resp)
	}
}

// 主体可信但请求体声明的 user_id 与主体不符 → Forbidden（不得静默改写，也不得放行）。
func TestCreateOrderRejectsSubjectMismatch(t *testing.T) {
	st := &recStore{}
	g := route.New(middleware.New(st, nopLogger{}), nopLogger{})
	repo := &fakeRepo{}
	svc := service.New(repo, nil, nil, nopLogger{}, false)
	s := New(handler.New(svc, false), g, false)

	ctx := metainfo.WithValue(context.Background(), identityMetaKey, "7")
	resp, err := s.CreateOrder(ctx, &orderpb.CreateOrderReq{
		UserId: 9, // 与主体 7 不符
		Items:  []*orderpb.OrderItem{{ProductId: 1, Name: "a", Quantity: 1, UnitPrice: 1}},
	})
	if resp != nil {
		t.Fatalf("归属不符不应返回响应体，实际 %+v", resp)
	}
	if c, _ := codes.From(err); c != codes.Forbidden {
		t.Fatalf("body user_id 与主体不符应回 Forbidden，实际 %v (err=%v)", c, err)
	}
	if repo.created != nil {
		t.Fatal("归属不符不得触碰业务（repo 不应收到 Create）")
	}
}
