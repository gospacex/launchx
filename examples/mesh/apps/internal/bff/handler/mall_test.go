package handler

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/bytedance/gopkg/cloud/metainfo"
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/route/param"
	"github.com/cloudwego/kitex/client/callopt"
	"github.com/cloudwego/kitex/pkg/kerrors"
	"github.com/gospacex/webx"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/bff/downstream"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/routing"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/rpcwire"

	orderpb "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/order"
	pointspb "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/points"
	productpb "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/product"
	stockpb "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/stock"
)

// 四个假客户端各只实现 downstream 的**窄接口**（1~2 个方法）。
// 这正是把 Clients 的字段声明成窄接口、而不是 kitex 生成的宽接口的收益：
// 假件不必为 22 个生成方法写空实现。四个假件共同记录「谁被调用了」，
// 用来把「路由 → 目标域」这张表变成可执行判据。

type fakeOrder struct {
	getCalls    int
	createCalls int
	gotCtx      context.Context
	lastCreate  *orderpb.CreateOrderReq
	getErr      error
	createErr   error
}

func (f *fakeOrder) GetOrder(ctx context.Context, _ *orderpb.GetOrderReq, _ ...callopt.Option) (*orderpb.GetOrderResp, error) {
	f.getCalls++
	f.gotCtx = ctx
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &orderpb.GetOrderResp{OrderId: 1}, nil
}

func (f *fakeOrder) CreateOrder(ctx context.Context, req *orderpb.CreateOrderReq, _ ...callopt.Option) (*orderpb.CreateOrderResp, error) {
	f.createCalls++
	f.gotCtx = ctx
	f.lastCreate = req
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &orderpb.CreateOrderResp{OrderId: 9}, nil
}

type fakePoints struct {
	calls int
	err   error
}

func (f *fakePoints) GetAccount(context.Context, *pointspb.GetAccountReq, ...callopt.Option) (*pointspb.GetAccountResp, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &pointspb.GetAccountResp{UserId: 1}, nil
}

type fakeProduct struct {
	calls int
	err   error
}

func (f *fakeProduct) GetProduct(context.Context, *productpb.GetProductReq, ...callopt.Option) (*productpb.GetProductResp, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &productpb.GetProductResp{Product: &productpb.ProductInfo{ProductId: 1}}, nil
}

type fakeStock struct {
	calls int
	err   error
}

func (f *fakeStock) GetStock(context.Context, *stockpb.GetStockReq, ...callopt.Option) (*stockpb.GetStockResp, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &stockpb.GetStockResp{ProductId: 1}, nil
}

// 编译期断言：四个假件必须满足 downstream 的窄接口。
var (
	_ downstream.OrderClient   = (*fakeOrder)(nil)
	_ downstream.PointsClient  = (*fakePoints)(nil)
	_ downstream.ProductClient = (*fakeProduct)(nil)
	_ downstream.StockClient   = (*fakeStock)(nil)
)

// call 把一个 handler 真正跑一遍（真 hertz RequestContext），返回状态码与响应体。
//
// 用真 RequestContext 而不是自造桩：参数解析（`c.Param`）与 JSON 渲染（`c.JSON`）
// 正是本层唯一的实质动作，绕过它们等于没测。
func call(t *testing.T, ctx context.Context, fn app.HandlerFunc, params map[string]string, body string) (int, map[string]any) {
	t.Helper()

	c := app.NewContext(0)
	c.Request.SetMethod("GET")
	c.Request.SetRequestURI("/x")
	for k, v := range params {
		c.Params = append(c.Params, param.Param{Key: k, Value: v})
	}
	if body != "" {
		c.Request.SetBody([]byte(body))
	}
	fn(ctx, c)

	out := map[string]any{}
	if b := c.Response.Body(); len(b) > 0 {
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("响应体不是 JSON 对象：%v（原文 %s）", err, b)
		}
	}
	return c.Response.StatusCode(), out
}

// TestRoutesReachTheirDomain 路由 → 目标域：每一条都必须打到**它自己**那个客户端。
//
// 这是 mesh-entry-chain 的「路由覆盖矩阵完整」与「接线错误被拦下（反例）」两条
// Scenario 的可执行形式。关键在判据的形状：断言的是「对应域的调用数 +1」而不是
// 「拿到了 200」—— 把 `/product/:id` 误接到 `clients.Order`，HTTP 依然 200
// （订单域会正常回一个订单），只有按域计数才看得出来。
//
// POST /order 一行的 ctx 带身份（UserID=1）且 body 的 user_id 也为 1：
// createOrder 自 2026-10-10 起要求边界解析出主体（匿名 401）、且 body 的 user_id
// 必须与主体一致（不符 403）。其余只读路由不带身份也能通（本层不拦读）。
func TestRoutesReachTheirDomain(t *testing.T) {
	o, p, pr, s := &fakeOrder{}, &fakePoints{}, &fakeProduct{}, &fakeStock{}
	h := New(&downstream.Clients{Order: o, Points: p, Product: pr, Stock: s}, false)

	authed := routing.WithIdentity(context.Background(), routing.Identity{UserID: 1})
	cases := []struct {
		route  string
		fn     app.HandlerFunc
		ctx    context.Context
		params map[string]string
		body   string
		hits   func() int
		domain string
	}{
		{"GET /order/:id", h.getOrder, context.Background(), map[string]string{"id": "2"}, "", func() int { return o.getCalls }, "srv-order"},
		{"POST /order", h.createOrder, authed, nil,
			`{"user_id":1,"items":[{"product_id":1,"name":"n","quantity":1,"unit_price":100}]}`,
			func() int { return o.createCalls }, "srv-order"},
		{"GET /points/:user_id", h.getPoints, context.Background(), map[string]string{"user_id": "1"}, "", func() int { return p.calls }, "srv-points"},
		{"GET /product/:id", h.getProduct, context.Background(), map[string]string{"id": "1"}, "", func() int { return pr.calls }, "srv-product"},
		{"GET /stock/:product_id", h.getStock, context.Background(), map[string]string{"product_id": "1"}, "", func() int { return s.calls }, "srv-stock"},
	}

	for _, tc := range cases {
		t.Run(tc.route, func(t *testing.T) {
			before := tc.hits()
			status, _ := call(t, tc.ctx, tc.fn, tc.params, tc.body)
			if got := tc.hits(); got != before+1 {
				t.Fatalf("没有打到 %s 的客户端（调用数 %d → %d）—— 该域的业务逻辑无法从入口触达",
					tc.domain, before, got)
			}
			if status != 200 {
				t.Fatalf("status=%d，期望 200", status)
			}
		})
	}
}

// TestIdentityWinsOverBodyAndCrossesProcess POST /order 的两条身份判据：
//
//  1. 归属用户取**边界解析出的身份**而不是请求体（否则客户端改个 user_id 就能
//     以别人的名义下单）；
//  2. 身份同时写进出向 metainfo（跨进程唯一载体，四域 srv 的限流按它分桶）。
//
// 请求体的 user_id 写成与主体一致（7）：自 2026-10-10 起，body 与主体不符是**显式
// 403**（见 TestCreateOrderRejectsBodySubjectMismatch），不再是「静默改写」。
// 这条用例的落点是「身份是权威来源且跨进程传导」，故让二者一致。
func TestIdentityWinsOverBodyAndCrossesProcess(t *testing.T) {
	o := &fakeOrder{}
	h := New(&downstream.Clients{Order: o}, false)
	ctx := routing.WithIdentity(context.Background(), routing.Identity{UserID: 7})

	const body = `{"user_id":7,"items":[{"product_id":1,"name":"n","quantity":1,"unit_price":100}]}`
	status, _ := call(t, ctx, h.createOrder, nil, body)
	if status != 200 {
		t.Fatalf("status=%d，期望 200", status)
	}
	if o.lastCreate.GetUserId() != 7 {
		t.Fatalf("下单归属 user_id = %d，期望 7（取边界身份，不由请求体决定）", o.lastCreate.GetUserId())
	}
	v, ok := metainfo.GetValue(o.gotCtx, rpcwire.IdentityMetaKey)
	if !ok {
		t.Fatalf("出向 ctx 里没有身份键 %q —— srv 侧限流会退化成服务级计数", rpcwire.IdentityMetaKey)
	}
	if v != "7" {
		t.Fatalf("身份键值 = %q，期望 \"7\"", v)
	}
}

// TestCreateOrderRejectsBodySubjectMismatch body 的 user_id 与主体不符 ⇒ **403**，且不触达下游。
//
// 2026-10-10 企业级加固：此前是「静默用主体覆盖 body」，隐蔽的问题是越权尝试
// 不留痕（调用方以为代他人下单成功了，日志里只有一次 200）。改成显式 403。
func TestCreateOrderRejectsBodySubjectMismatch(t *testing.T) {
	o := &fakeOrder{}
	h := New(&downstream.Clients{Order: o}, false)
	ctx := routing.WithIdentity(context.Background(), routing.Identity{UserID: 7})

	const body = `{"user_id":9,"items":[{"product_id":1,"name":"n","quantity":1,"unit_price":100}]}`
	status, rb := call(t, ctx, h.createOrder, nil, body)
	if status != 403 {
		t.Fatalf("status=%d，期望 403（响应体 %v）", status, rb)
	}
	if int32(rb["code"].(float64)) != int32(codes.Forbidden) {
		t.Fatalf("响应体 code=%v，期望 %d", rb["code"], int32(codes.Forbidden))
	}
	if o.createCalls != 0 {
		t.Fatalf("归属不符时不应触达下游，实际 createCalls=%d", o.createCalls)
	}
}

// TestCreateOrderRejectsAnonymous 匿名（ctx 里无主体）下单 ⇒ **401**，且不触达下游。
//
// 这是入口鉴权的最后一道本地兜底：identity 中间件本应已拦下匿名请求（见 app/setup.go），
// 但「中间件没挂上」这类装配错误若不报错、就会静默变成「匿名可下单」，故本层再判一次。
func TestCreateOrderRejectsAnonymous(t *testing.T) {
	o := &fakeOrder{}
	h := New(&downstream.Clients{Order: o}, false)

	const body = `{"user_id":1,"items":[{"product_id":1,"name":"n","quantity":1,"unit_price":100}]}`
	status, rb := call(t, context.Background(), h.createOrder, nil, body)
	if status != 401 {
		t.Fatalf("status=%d，期望 401（响应体 %v）", status, rb)
	}
	if int32(rb["code"].(float64)) != int32(codes.Unauthorized) {
		t.Fatalf("响应体 code=%v，期望 %d", rb["code"], int32(codes.Unauthorized))
	}
	if o.createCalls != 0 {
		t.Fatalf("匿名不应触达下游，实际 createCalls=%d", o.createCalls)
	}
}

// TestAnonymousStaysAnonymous 匿名请求**不写**身份键。
//
// 写 "0" 会让 srv 的 admit 判成「身份非法」回 Unauthorized（它对解析失败/为 0
// 是出声拒绝，对**没带键**才是 fail-open）。这一条钉住两者的边界。
func TestAnonymousStaysAnonymous(t *testing.T) {
	o := &fakeOrder{}
	h := New(&downstream.Clients{Order: o}, false)

	if status, _ := call(t, context.Background(), h.getOrder, map[string]string{"id": "2"}, ""); status != 200 {
		t.Fatalf("status=%d，期望 200", status)
	}
	if _, ok := metainfo.GetValue(o.gotCtx, rpcwire.IdentityMetaKey); ok {
		t.Fatal("匿名请求不应写入身份键（写 0 会被 srv 判成身份非法）")
	}
}

// TestErrorMappingAtBoundary 错误码 → HTTP 状态码，三种来源各一档。
//
// 「跨进程业务码」那一档是 rpcwire 存在的全部理由：kitex 在客户端把 biz error
// 物化成 `*kerrors.BizStatusError`，若退化回 `codes.From`（认不出这个类型），
// 它会给 Internal ⇒ 404 变 502。本条判据就是防这个静默退化。
func TestErrorMappingAtBoundary(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   int32
	}{
		{"本进程构造的业务码", codes.New(codes.ProductNotFound, "nope"), 404, int32(codes.ProductNotFound)},
		{"跨进程回来的业务码", kerrors.NewBizStatusError(int32(codes.ProductNotFound), "nope"), 404, int32(codes.ProductNotFound)},
		{"传输层失败", errors.New("dial tcp 10.0.0.1:9013: connect: connection refused"), 502, int32(codes.Internal)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := New(&downstream.Clients{Product: &fakeProduct{err: tc.err}}, false)
			status, body := call(t, context.Background(), h.getProduct, map[string]string{"id": "1"}, "")

			if status != tc.wantStatus {
				t.Fatalf("status=%d，期望 %d（响应体 %v）", status, tc.wantStatus, body)
			}
			got, ok := body["code"].(float64)
			if !ok {
				t.Fatalf("响应体缺 code 字段：%v", body)
			}
			if int32(got) != tc.wantCode {
				t.Fatalf("响应体 code=%d，期望 %d", int32(got), tc.wantCode)
			}
		})
	}
}

// TestStatusOfCoversEveryBizCode 映射表必须覆盖 pkg/codes 的**全部**业务码。
//
// 漏一个码不会有任何编译期提示，它只会静默落到 default（502「上游坏了」）——
// 例如漏掉 StockNotEnough 之后，"库存不足" 会以 502 返回，排查方向被带到
// "下游挂了"。清单与 pkg/codes 的常量块一一对应，增删码时同步改这里。
func TestStatusOfCoversEveryBizCode(t *testing.T) {
	want := map[codes.Code]int{
		codes.InvalidArgument: 400,
		codes.Unauthorized:    401,
		codes.Forbidden:       403,
		codes.OrderNotFound:   404,
		codes.ProductNotFound: 404,
		codes.StockNotEnough:  409,
		codes.PointsNotEnough: 409,
		codes.RateLimited:     429,
		codes.OrderCreateFail: 500,
		codes.Unimplemented:   501,
		codes.Internal:        502,
	}
	for code, status := range want {
		if got := statusOf(code); got != status {
			t.Errorf("statusOf(%d) = %d，期望 %d", int32(code), got, status)
		}
	}

	// 未登记的码（含上游换了码表的情形）一律按下游故障透传 502，
	// 而不是 500 —— 500 会被读成「bff 自己崩了」。
	if got := statusOf(codes.Code(99999)); got != 502 {
		t.Errorf("未登记码 statusOf = %d，期望 502", got)
	}
}

// TestBadPathParamRejected 路径参数非法：400，且**不触达下游**。
func TestBadPathParamRejected(t *testing.T) {
	pr := &fakeProduct{}
	h := New(&downstream.Clients{Product: pr}, false)

	status, body := call(t, context.Background(), h.getProduct, map[string]string{"id": "abc"}, "")
	if status != 400 {
		t.Fatalf("status=%d，期望 400（响应体 %v）", status, body)
	}
	if int32(body["code"].(float64)) != int32(codes.InvalidArgument) {
		t.Fatalf("响应体 code=%v，期望 %d", body["code"], int32(codes.InvalidArgument))
	}
	if pr.calls != 0 {
		t.Fatal("参数非法时不应触达下游")
	}
}

// TestRegisterCoversSixRoutes 六条路由必须真的挂到引擎上。
//
// 用真 webx 引擎（不监听）取 Routes()：`Register` 里写错一个路径模板
// （例如给 POST /order 漏了方法、或把 `:product_id` 写成 `:id`）在编译期
// 完全无声，只有对着路由表才看得出来。
func TestRegisterCoversSixRoutes(t *testing.T) {
	e, err := webx.New()
	if err != nil {
		t.Fatalf("webx.New: %v", err)
	}
	New(&downstream.Clients{}, false).Register(e)

	got := map[string]bool{}
	for _, r := range e.Routes() {
		got[r.Method+" "+r.Path] = true
	}
	for _, want := range []string{
		"GET /ping",
		"GET /order/:id",
		"POST /order",
		"GET /points/:user_id",
		"GET /product/:id",
		"GET /stock/:product_id",
	} {
		if !got[want] {
			t.Errorf("路由未注册：%s（已注册：%v）", want, got)
		}
	}
}
