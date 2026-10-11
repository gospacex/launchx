package app

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestSetupMountsRouteLayer 保护「setup.go 真的把路由层挂上了」。
//
// 背景：`internal/route` 的 Router 此前只有类型定义、从未实例化 —— `route.order.<Op>`
// 整层 span 缺失，而 T2–T5 的判据里没有一条断言它存在，所以没人发现。e2e 判据依赖
// Jaeger 与已启动进程（requireJaeger / requireApp 会 skip），CI 根本走不到；而
// 直接跑 mountRouteLayer 的用例只能证明 helper 可用，证明不了**有人调它** ——
// 实测把 setup.go 里那一行注释掉，那条用例照样绿。
//
// 故这条判据落在源码层面（与根包 TestLibrarySourceHasNo* 系列同形）：接线本身
// 是源码事实，没有进程内可观测的副作用时，静态断言就是唯一能抓住它的东西。
//
// 反向探针：把 setup.go 里 mountRouteLayer 那一行注释掉，本用例立刻红。
func TestSetupMountsRouteLayer(t *testing.T) {
	src, err := os.ReadFile("setup.go")
	if err != nil {
		t.Fatalf("读 setup.go: %v", err)
	}
	// 匹配**调用点**而不是函数名：`mountRouteLayer(` 这种短串会命中它自己的
	// 函数定义（`func mountRouteLayer(e *route.Engine ...)`）—— 实测把调用整行
	// 注释掉后，短串判据照样绿，等于判据是空壳。必须匹配能把调用与定义区分
	// 开的那一段，即紧跟的实参 `engine.Engine`。
	if !strings.Contains(string(src), "mountRouteLayer(engine.Engine") {
		t.Fatalf("setup.go 未调用 mountRouteLayer(engine.Engine, ...) —— " +
			"路由层没挂上，`route.order.<Op>` 整层会无声缺失")
	}
}

// TestRouteLayerProducesSpan 验证 helper 的运行时行为：挂上之后请求确实产出一节
// route span，且带上可聚合的 http.route 属性。
//
// 这条不替代上一条：它证明 helper 本身能用（span 名、属性、未登记回退），
// 上一条证明 setup.go 调了它。两者缺一，缺口都会回来。
//
// 反向探针：把 mountRouteLayer 里的 e.Use 去掉，本用例立刻红（实测：no span）。
func TestRouteLayerProducesSpan(t *testing.T) {
	// 用内存 recorder 接 span：不依赖 Jaeger，退出链也不需要。
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	e := route.NewEngine(config.NewOptions(nil))
	mountRouteLayer(e, nil, true)
	// 末端 handler 只回应，不产 span：断言里不能把别人的 span 当成 route 层的。
	e.GET("/orders", func(_ context.Context, c *app.RequestContext) {
		c.String(200, "ok")
	})

	ut.PerformRequest(e, "GET", "/orders", nil)

	var names []string
	var routeAttr string
	for _, s := range rec.Ended() {
		names = append(names, s.Name())
		if s.Name() == "route.order.ListOrders" {
			for _, kv := range s.Attributes() {
				if string(kv.Key) == "http.route" {
					routeAttr = kv.Value.AsString()
				}
			}
		}
	}

	want := "route.order.ListOrders"
	found := false
	for _, n := range names {
		if n == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("未产出 %s span，实际 span 名 = %v（路由层没挂上）", want, names)
	}
	if routeAttr != "/orders" {
		t.Fatalf("http.route = %q, want %q（属性没打上就没法按路由聚合耗时/错误率）", routeAttr, "/orders")
	}
}

// TestRouteLayerMarksUnregistered 补一条反向语义：未登记的路由照样记一节，但
// 必须能一眼认出（Op=Unregistered + route.registered=false），而不是静默消失。
func TestRouteLayerMarksUnregistered(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	e := route.NewEngine(config.NewOptions(nil))
	mountRouteLayer(e, nil, true)
	// 刻意不登记：模拟"加了接口但忘了写进 route.Table()"。
	e.GET("/forgotten", func(_ context.Context, c *app.RequestContext) { c.String(200, "ok") })

	ut.PerformRequest(e, "GET", "/forgotten", nil)

	var registered *bool
	found := false
	for _, s := range rec.Ended() {
		if s.Name() != "route.order.Unregistered" {
			continue
		}
		found = true
		for _, kv := range s.Attributes() {
			if string(kv.Key) == "route.registered" {
				v := kv.Value.AsBool()
				registered = &v
			}
		}
	}
	if !found {
		t.Fatal("未登记的路由也应记一节 route.order.Unregistered（否则漏登记会静默）")
	}
	if registered == nil || *registered {
		t.Fatalf("route.registered = %v, want false", registered)
	}
}
