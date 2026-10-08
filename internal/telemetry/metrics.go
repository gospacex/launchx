// Package telemetry 的指标半边：Prometheus 拉取式指标门面。
//
// 为什么是**拉取**（Prometheus）而不是 OTLP 推送：本仓 srv 档只有 RPC 面，
// 推送式导出要求对端有一个常驻的 OTLP 指标接收端，而 demo 底座里只有 Jaeger
// （它不存指标）；拉取式只需一个 HTTP 采集口，四档形态都能自证（curl 可验）。
//
// 装配顺序与 tracer 同一条红线：**必须早于任何组件构造**。它设的是 otel 全局
// MeterProvider，而 HTTP/RPC 的 instrument 都在各自 SDK 构造时向全局取用。
package telemetry

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// MetricsEndpointPath 采集口的路径。与 webx 的默认前缀一致；
// srv 档自建的监听只服务这一个路径。
const MetricsEndpointPath = "/metrics"

// Metrics 指标门面：一个独立的 prometheus 注册表 + 由它派生的 MeterProvider。
//
// 用**独立注册表**而不是 prometheus.DefaultRegisterer：默认注册表里混着 go
// runtime 与 process collector，以及任何第三方库自行注册的指标；发布方的指标
// 与它们混在一个空间里，既难筛也容易被同名项撞出 panic。
type Metrics struct {
	provider *sdkmetric.MeterProvider
	handler  http.Handler

	mu  sync.Mutex
	srv *http.Server
	ln  net.Listener

	// Shutdown 由退出链**两处**调用（srv 档的 OnStop 关监听、Close 收口刷数据），
	// 故必须自己保证幂等：prometheus reader 的 Shutdown 第二次会返回
	// "reader is shutdown"，把它当错误聚合进退出链会凭空多一条噪声错误。
	shutdownOnce sync.Once
	shutdownErr  error
}

// NewMetrics 装配指标门面并注册 otel 全局 MeterProvider。
//
// enabled=false 时把全局**显式**换成 noop：与 trace 同口径 —— 「关掉」的语义
// 必须由本函数钉死，不能依赖「上游恰好没设过」这种偶然事实（否则上一个进程内
// 遗留的 provider 会让指标照产，开关形同虚设）。
func NewMetrics(serviceName string, enabled bool) (*Metrics, error) {
	m := &Metrics{}
	if !enabled {
		otel.SetMeterProvider(noopmetric.NewMeterProvider())
		return m, nil
	}
	if serviceName == "" {
		return nil, fmt.Errorf("telemetry: metrics requires a service name")
	}
	reg := prometheus.NewRegistry()
	exporter, err := otelprom.New(otelprom.WithRegisterer(reg))
	if err != nil {
		return nil, fmt.Errorf("telemetry: prometheus exporter: %w", err)
	}
	m.provider = sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(exporter),
		// 资源属性只放 service.name：它是 prometheus 侧 target_info 的唯一
		// 有用字段（面板按它区分进程），version/env 本地演示用不上。
		sdkmetric.WithResource(resource.NewWithAttributes("",
			attribute.String("service.name", serviceName),
		)),
	)
	otel.SetMeterProvider(m.provider)
	m.handler = promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	return m, nil
}

// Handler 返回采集口；未启用时返回 nil（调用方据此不注册路由、不起监听）。
func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return nil
	}
	return m.handler
}

// MeterProvider 返回本门面的 provider；未启用时返回 **nil 接口**。
//
// 必须显式判空再返回：直接 `return m.provider` 会把 (*sdkmetric.MeterProvider)(nil)
// 装进接口，得到一个 != nil 的接口值 —— 调用方据此判断"指标开了"，随后每一次
// 取 instrument 都落在空实现上，且没有任何报错。
func (m *Metrics) MeterProvider() metric.MeterProvider {
	if m == nil || m.provider == nil {
		return nil
	}
	return m.provider
}

// Serve 起一个只服务采集口的 HTTP 监听（srv 档用：它没有 HTTP 面）。
//
// 绑定失败即返回 error（起不来就得说，不能"指标默默没有"）；起完之后的
// Serve 返回错误只可能是被 Shutdown 关掉，故不再上报。
//
// 只挂 MetricsEndpointPath 一个路径：这不是业务端口，不该出现在任何服务发现
// 或探针里，也不需要任何中间件（采集器不背债）。
func (m *Metrics) Serve(addr string) error {
	if m == nil || m.handler == nil {
		return nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("telemetry: metrics listen %s: %w", addr, err)
	}
	mux := http.NewServeMux()
	mux.Handle(MetricsEndpointPath, m.handler)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	m.mu.Lock()
	m.srv, m.ln = srv, ln
	m.mu.Unlock()

	go func() {
		if serr := srv.Serve(ln); serr != nil && serr != http.ErrServerClosed {
			// 到这里说明监听期出错（不是 Shutdown）。没有 logger 可用，
			// 且此刻业务已在跑，唯一诚实的做法是不吞掉：写进 stderr。
			fmt.Fprintf(os.Stderr, "telemetry: metrics server stopped: %v\n", serr)
		}
	}()
	return nil
}

// Shutdown 关掉采集口并收口 MeterProvider（可重复调用，结果与首次一致）。
//
// 关 provider 会让已聚合的指标做最后一次收集：不关的话退出链末端会缺一段
// （与 tracer 的 flush 同一个道理）。
func (m *Metrics) Shutdown(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.shutdownOnce.Do(func() { m.shutdownErr = m.shutdown(ctx) })
	return m.shutdownErr
}

func (m *Metrics) shutdown(ctx context.Context) error {
	m.mu.Lock()
	srv := m.srv
	m.mu.Unlock()
	if srv != nil {
		if err := srv.Shutdown(ctx); err != nil {
			return fmt.Errorf("telemetry: metrics shutdown: %w", err)
		}
	}
	if m.provider != nil {
		if err := m.provider.Shutdown(ctx); err != nil {
			return fmt.Errorf("telemetry: meter provider shutdown: %w", err)
		}
	}
	return nil
}
