package mode

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gospacex/webx"
)

// fakeMetrics 最小 MetricsEndpoint 桩：只给一个采集口。
type fakeMetrics struct{ h http.Handler }

func (f fakeMetrics) Handler() http.Handler { return f.h }

const metricsHTTPYAML = `
service: {name: bff-metrics, mode: bff, addr: "127.0.0.1:18087"}
webx: {addr: "127.0.0.1:18087"}
trace: {enabled: false}
metrics: {enabled: true}
`

// TestMetricsEndpointAttachedToEngine HTTP 档：采集口必须真的挂到 webx 引擎上。
//
// 这条是「配了指标不等于指标暴露了」的守卫：mode 只把 handler 传给 webx，
// 少传一次（或 webx 那边 WithMetricsEndpoint 的 presence 判断写反）都不会报错，
// 只会让采集口 404。
func TestMetricsEndpointAttachedToEngine(t *testing.T) {
	h := holdersYAML(t, metricsHTTPYAML)
	served := false
	h.Metrics = fakeMetrics{h: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served = true
		w.WriteHeader(http.StatusOK)
	})}
	if err := Assemble(h); err != nil {
		t.Fatalf("assemble: %v", err)
	}
	app, ok := h.Out.Webx.(*webx.App)
	if !ok {
		t.Fatalf("Out.Webx = %T, want *webx.App", h.Out.Webx)
	}
	hnd := app.Config().MetricsHandler
	if hnd == nil {
		t.Fatal("metrics.enabled=true 却没有把采集口挂到 webx 引擎上")
	}
	if app.Config().MetricsPrefix != webx.DefaultMetricsPrefix {
		t.Fatalf("MetricsPrefix = %q, want %q", app.Config().MetricsPrefix, webx.DefaultMetricsPrefix)
	}
	hnd.ServeHTTP(discardWriter{}, httptestNewRequest())
	if !served {
		t.Fatal("挂上去的不是注入的采集口")
	}
}

// TestMetricsAbsentLeavesEngineClean 未注入指标门面时引擎上不该有采集口
// （presence-based：不要指标是合法形态）。
func TestMetricsAbsentLeavesEngineClean(t *testing.T) {
	h := holders(t, "bff.yaml")
	if err := Assemble(h); err != nil {
		t.Fatalf("assemble: %v", err)
	}
	app, ok := h.Out.Webx.(*webx.App)
	if !ok {
		t.Fatalf("Out.Webx = %T, want *webx.App", h.Out.Webx)
	}
	if app.Config().MetricsHandler != nil {
		t.Fatal("没注入指标门面，引擎上却出现了采集口")
	}
}

// TestSRVMetricsRequiresAddr srv 档没有 HTTP 面：开了指标却没配地址必须即退。
//
// 不配地址的后果是「指标被采集但永不暴露」—— 面板空着而进程一切正常，
// 属于本仓一律即退的那类静默配置错误。
func TestSRVMetricsRequiresAddr(t *testing.T) {
	const tmpl = `
service: {name: srv-metrics, mode: srv, addr: "127.0.0.1:19021"}
trace: {enabled: false}
metrics:
  enabled: true
__ADDR__
`
	t.Run("缺 addr 即退", func(t *testing.T) {
		h := holdersYAML(t, strings.Replace(tmpl, "__ADDR__", "", 1))
		err := Assemble(h)
		if err == nil {
			t.Fatal("srv + metrics.enabled 缺 metrics.addr 必须即退")
		}
		if !strings.Contains(err.Error(), "metrics.addr") {
			t.Fatalf("错误信息必须点名 metrics.addr：%v", err)
		}
	})

	t.Run("配了 addr 正常装配", func(t *testing.T) {
		h := holdersYAML(t, strings.Replace(tmpl, "__ADDR__", "  addr: \"127.0.0.1:19101\"", 1))
		if err := Assemble(h); err != nil {
			t.Fatalf("assemble: %v", err)
		}
		if h.Out.MetricsAddr != "127.0.0.1:19101" {
			t.Fatalf("Out.MetricsAddr = %q", h.Out.MetricsAddr)
		}
		if h.Out.Webx != nil {
			t.Fatal("srv 档不该有 webx 引擎（采集口靠独立监听）")
		}
	})
}

// discardWriter 丢弃写入的 http.ResponseWriter 桩。
type discardWriter struct{}

func (discardWriter) Header() http.Header         { return http.Header{} }
func (discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (discardWriter) WriteHeader(int)             {}

func httptestNewRequest() *http.Request {
	req, err := http.NewRequest(http.MethodGet, "/metrics", nil)
	if err != nil {
		panic(err)
	}
	return req
}
