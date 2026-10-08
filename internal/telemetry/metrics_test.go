package telemetry

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
)

// TestMetricsDisabledExposesNothing 关闭态：没有采集口，也没有 provider。
//
// 断言"没有"而不是"有但空着"：一个能返回 200 却永远没数据的采集口，
// 比没有采集口更难排查。
func TestMetricsDisabledExposesNothing(t *testing.T) {
	m, err := NewMetrics("metrics-test", false)
	if err != nil {
		t.Fatalf("NewMetrics(disabled): %v", err)
	}
	if m.Handler() != nil {
		t.Fatal("关闭态不应有采集口")
	}
	if m.MeterProvider() != nil {
		t.Fatal("关闭态不应有 MeterProvider")
	}
	// 关闭态仍要保证全局是 noop（而不是留着上一个 provider 继续产指标）。
	if _, err := otel.GetMeterProvider().Meter("probe").Int64Counter("probe_calls"); err != nil {
		t.Fatalf("关闭态全局 meter 应可用（noop）：%v", err)
	}
	// 幂等：没起过监听也能安全关闭。
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown on disabled metrics: %v", err)
	}
}

// TestMetricsPipelineExportsInstruments 是本文件的核心链路断言：
// instrument（经 otel 全局 meter，与各 SDK 同口径）→ MeterProvider → 注册表 → 采集口。
//
// 反向探针：把 NewMetrics 里的 otel.SetMeterProvider 去掉，instrument 会落在
// 默认 noop 上，采集口输出里就没有这条序列，本用例立刻红。
func TestMetricsPipelineExportsInstruments(t *testing.T) {
	m, err := NewMetrics("metrics-test", true)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })

	// **经全局取用**：这正是 rpcx / webx 的做法，用别的方式取就测不到装配顺序。
	counter, err := otel.GetMeterProvider().Meter("metrics-test").Int64Counter("probe_calls")
	if err != nil {
		t.Fatalf("create counter: %v", err)
	}
	counter.Add(context.Background(), 3)

	body := scrape(t, m)
	if !strings.Contains(body, "probe_calls") {
		t.Fatalf("采集口输出里没有探针指标，装配链路断了。全文：\n%s", body)
	}
	if !strings.Contains(body, "3") {
		t.Fatalf("探针指标的值没到采集口。全文：\n%s", body)
	}
	// 资源属性要能区分进程（面板按 service_name 筛），否则多实例部署时无法定位。
	if !strings.Contains(body, "metrics-test") {
		t.Fatalf("采集口输出里缺 service.name（target_info）。全文：\n%s", body)
	}
}

// TestMetricsServeBindsAndShutsDown 采集口监听真起得来、关得掉，且可重复关闭。
func TestMetricsServeBindsAndShutsDown(t *testing.T) {
	m, err := NewMetrics("metrics-test", true)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	addr := freeAddr(t)
	if err := m.Serve(addr); err != nil {
		t.Fatalf("Serve(%s): %v", addr, err)
	}

	// 用不带 keep-alive 的客户端：默认 Transport 会把连接留在池里，
	// 服务端 Shutdown 要等它们回到 idle 才收工，用例会白等一整个超时。
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	for _, path := range []string{MetricsEndpointPath, "/healthz/live"} {
		resp, err := client.Get("http://" + addr + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if path == MetricsEndpointPath && resp.StatusCode != 200 {
			t.Fatalf("采集口 = %d, want 200", resp.StatusCode)
		}
		// 只服务采集口一个路径：它不是业务端口。
		if path != MetricsEndpointPath && resp.StatusCode == 200 {
			t.Fatalf("%s 不该由采集口监听服务", path)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := m.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown 必须幂等（退出链里会被调两次）：%v", err)
	}
}

// TestMetricsServeRejectsBadAddr 端口非法必须报错（起不来就得说，不能"指标默默没有"）。
func TestMetricsServeRejectsBadAddr(t *testing.T) {
	m, err := NewMetrics("metrics-test", true)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	if err := m.Serve("127.0.0.1:99999"); err == nil {
		t.Fatal("非法端口必须返回 error")
	}
}

// TestMetricsDisabledServeIsNoop 关闭态调 Serve 不该占端口、也不该报错。
func TestMetricsDisabledServeIsNoop(t *testing.T) {
	m, err := NewMetrics("metrics-test", false)
	if err != nil {
		t.Fatalf("NewMetrics(disabled): %v", err)
	}
	addr := freeAddr(t)
	if err := m.Serve(addr); err != nil {
		t.Fatalf("关闭态 Serve 应是 no-op：%v", err)
	}
	if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		_ = c.Close()
		t.Fatal("关闭态不该真的监听端口")
	}
}

// scrape 从采集口取一次快照。
func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, MetricsEndpointPath, nil))
	if rec.Code != 200 {
		t.Fatalf("采集口 = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

// freeAddr 占一个空闲端口再放掉（测试用；与真正的 Serve 之间有极小的竞态窗口，
// 对本地用例足够）。
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freeAddr: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}
