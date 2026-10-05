// Package proxy 网关转发：路由匹配 → 上游地址解析 → 反向代理 → 状态码原样透传。
//
// 本包不感知形态（不出现任何形态取值），也不自建注册中心客户端 —— 发现句柄
// 由装配层从 App.Registry() 取来后注入。
package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/gospacex/coordx"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/gospacex/launchx/examples/micro/gateway/internal/routing"
)

const (
	// spanPrefix 转发 span 名前缀（不覆盖 webx 入口中间件的根 span）。
	spanPrefix = "gateway.proxy."
	// defaultTimeout 单次转发的整体超时。
	defaultTimeout = 5 * time.Second
	// upstreamScheme 上游协议：demo 全链路在可信网络内，不引入 TLS 配置面。
	upstreamScheme = "http"
	// fallbackTracerName tracer 为空时的回落 instrumentation 名。
	fallbackTracerName = "gateway.proxy"
)

// hopHeaders 逐跳首部：不能透传给上游，否则连接语义会被上游误解。
var hopHeaders = map[string]bool{
	"Connection":          true,
	"Proxy-Connection":    true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Keep-Alive":          true,
	"Transfer-Encoding":   true,
	"Te":                  true,
	"Trailer":             true,
	"Upgrade":             true,
}

// skipRequestHeaders 请求侧额外剔除：这两个由 net/http 按出站请求自行决定，
// 照抄会与真实报文不一致。
var skipRequestHeaders = map[string]bool{"Host": true, "Content-Length": true}

// Finder 注册中心发现能力（coordx.Discover 的同形子集）。
//
// 之所以只声明这一个方法：launchx 的 App.Registry() 返回 coordx.Register
// （Register/Deregister/HealthCheck/Close），只有同时实现了发现的驱动句柄
// 才断言得出来；断言失败即降级到配置直连，不因此自建客户端。
type Finder interface {
	Discovery(ctx context.Context, service string) ([]*coordx.ServiceInstance, error)
}

// Resolver 上游地址解析器：注册中心发现优先，配置直连兜底。
type Resolver struct {
	finder   Finder
	fallback func(upstream string) string
}

// NewResolver 构造解析器。finder 可为 nil（注册中心未装配或句柄不含发现能力）；
// fallback 为 nil 表示没有直连兜底（发现不到即 502）。
func NewResolver(finder Finder, fallback func(upstream string) string) *Resolver {
	return &Resolver{finder: finder, fallback: fallback}
}

// Resolve 把被调服务名解析成 host:port。
//
// 两条路都拿不到地址就返回 error（由调用方映射成 502）：静默换成一个
// 「猜出来的」上游比 502 更难排查。
func (r *Resolver) Resolve(ctx context.Context, upstream string) (string, error) {
	if r.finder != nil {
		if insts, err := r.finder.Discovery(ctx, upstream); err == nil {
			for _, inst := range insts {
				if inst == nil || !inst.Healthy || inst.Addr == "" {
					continue
				}
				return net.JoinHostPort(inst.Addr, strconv.Itoa(inst.Port)), nil
			}
		}
	}
	if r.fallback != nil {
		if target := strings.TrimSpace(r.fallback(upstream)); target != "" {
			return target, nil
		}
	}
	return "", fmt.Errorf("proxy: no instance of upstream %q: discovery and static fallback both miss", upstream)
}

// Proxy 网关转发器。
type Proxy struct {
	routes   routing.Table
	resolver *Resolver
	client   *http.Client
	tracer   trace.Tracer
}

// New 构造转发器。tracer 为空时回落到 otel 全局（缺省即 noop）。
func New(routes routing.Table, resolver *Resolver, tracer trace.Tracer) *Proxy {
	if tracer == nil {
		tracer = otel.Tracer(fallbackTracerName)
	}
	return &Proxy{
		routes:   routes,
		resolver: resolver,
		client:   &http.Client{Timeout: defaultTimeout},
		tracer:   tracer,
	}
}

// Handle 是挂到 webx 引擎上的 hertz handler：
// 匹配路由 → 解析上游 → 开转发 span → 转发 → 状态码原样透传。
func (p *Proxy) Handle(ctx context.Context, c *app.RequestContext) {
	path := string(c.URI().Path())
	route, ok := p.routes.Match(path)
	if !ok {
		// 能进到这个 handler 说明通配路由命中了，走到这里即路由表与注册不一致。
		fail(ctx, c, http.StatusBadGateway, fmt.Errorf("proxy: no route matches %q", path))
		return
	}
	target, err := p.resolver.Resolve(ctx, route.Upstream)
	if err != nil {
		fail(ctx, c, http.StatusBadGateway, err)
		return
	}

	// 转发 span 是 webx 入口根 span 的子 span：不在这里另起根，
	// 否则 gateway→上游会断成两根 trace。
	ctx, span := p.tracer.Start(ctx, spanPrefix+route.Name)
	defer span.End()
	span.SetAttributes(
		attribute.String("proxy.upstream", route.Upstream),
		attribute.String("proxy.target", target),
		attribute.String("proxy.path", path),
	)

	status, err := p.forward(ctx, c, path, target, route)
	span.SetAttributes(attribute.Int("http.status_code", status))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		c.JSON(http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status >= http.StatusBadRequest {
		span.SetStatus(otelcodes.Error, http.StatusText(status))
	}
}

// forward 执行一次转发，返回上游状态码。
//
// 判据是「状态码原样透传」：上游 404/429/502 必须落在同码上，不得统一转 500
// —— 网关改写状态码会把上游的错误语义抹掉。
func (p *Proxy) forward(ctx context.Context, c *app.RequestContext, path, target string, route routing.Route) (int, error) {
	u := url.URL{
		Scheme:   upstreamScheme,
		Host:     target,
		Path:     upstreamPath(path, route),
		RawQuery: string(c.URI().QueryString()),
	}
	req, err := http.NewRequestWithContext(ctx, string(c.Request.Method()), u.String(), bytes.NewReader(c.Request.Body()))
	if err != nil {
		return 0, fmt.Errorf("proxy: build upstream request: %w", err)
	}
	copyRequestHeaders(c, req)

	// webx 的 otel 中间件只做 Extract、不做 Inject：不在这里显式注入的话，
	// 上游收不到 traceparent 会另起一根 trace（验收 T6/T7 的断链判据）。
	// TODO: Task 23 落地 launchx.Inject 后替换为该入口。
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))

	resp, err := p.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("proxy: dial upstream %s: %w", target, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("proxy: read upstream response: %w", err)
	}
	copyResponseHeaders(c, resp)
	c.Status(resp.StatusCode)
	c.Response.SetBody(body)
	return resp.StatusCode, nil
}

// upstreamPath 计算转发给上游的路径：Strip 为真时剥掉前缀（剥完为空补 '/'）。
func upstreamPath(path string, route routing.Route) string {
	if !route.Strip {
		return path
	}
	if up := strings.TrimPrefix(path, route.Prefix); up != "" {
		return up
	}
	return "/"
}

// copyRequestHeaders 把入站首部抄给出站请求，逐跳首部与长度类首部除外。
func copyRequestHeaders(c *app.RequestContext, req *http.Request) {
	c.Request.Header.VisitAll(func(k, v []byte) {
		key := string(k)
		if hopHeaders[key] || skipRequestHeaders[key] {
			return
		}
		req.Header.Set(key, string(v))
	})
}

// copyResponseHeaders 把上游响应首部抄回客户端响应。
func copyResponseHeaders(c *app.RequestContext, resp *http.Response) {
	for key, values := range resp.Header {
		if hopHeaders[http.CanonicalHeaderKey(key)] {
			continue
		}
		for _, v := range values {
			c.Response.Header.Add(key, v)
		}
	}
	// 长度由 hertz 按最终 body 计算，照抄上游值会在改写 body 时错位。
	c.Response.Header.Del("Content-Length")
}

// fail 渲染错误响应：错误记进当前 span，响应体只带文案（不泄漏内部细节）。
func fail(ctx context.Context, c *app.RequestContext, status int, err error) {
	span := trace.SpanFromContext(ctx)
	span.RecordError(err)
	span.SetStatus(otelcodes.Error, err.Error())
	c.JSON(status, map[string]string{"error": err.Error()})
}
