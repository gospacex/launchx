// Package traceutil 提供 Jaeger REST API（jaeger-query v1）的检索助手。
//
// 为什么单开一个包而不是每个 _test.go 里手写 http.Get + json.Unmarshal：
// 「这些人会踩的坑」在同一份代码里已经被踩过三遍，散在测试里等于每份起，而且
// 每一份都会踩出新花样：
//
//  1. span 属性在 `.spans[].tags[]`（{key,type,value} 三元），**不是** `.attributes[]`
//     —— 后者恒为空，照它写判据会静默"全过"（最危险的一种失败）；
//  2. 服务名在 `.data[].processes[<processID>].serviceName`，取
//     `.spans[].process.serviceName` 恒为 ""（不是 null，空串不会报错）；
//  3. `?lookback=` 参数对返回条数**没有影响**（jaeger 1.63 + ES 存储实测），
//     必须自己算 start/end 微秒时间戳，否则一直在拿陈旧 trace 判新代码；
//  4. 本机 HTTP_PROXY 会把发往 Jaeger 的请求拦走 —— Go 的 Transport 默认读环境
//     代理，虽然 loopback 会被豁免，但显式置 Proxy:nil 才能把这个豁免写死在代码里
//     （等价于 curl 必须带 --noproxy '*' 那条纪律）。
//
// 本包只做检索与树形计算，不含任何断言 —— 判据写在各 *_test.go 里。
// 判据与检索分开是有意的：同一棵树，不同验收案例看的切面不同。
package traceutil

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL 本机 Jaeger UI 地址（compose 映射 16686）。
const DefaultBaseURL = "http://127.0.0.1:16686"

// refTypeChildOf 父子引用类型（区别于 FOLLOWS_FROM：后者不参与树形计算）。
const refTypeChildOf = "CHILD_OF"

// Client Jaeger 查询客户端。
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New 构造客户端；base 为空或 "default" 时回落 DefaultBaseURL。
func New(base string) *Client {
	if base == "" || base == "default" {
		base = DefaultBaseURL
	}
	return &Client{
		BaseURL: strings.TrimSuffix(base, "/"),
		// 显式禁用环境的 HTTP_PROXY：本机 shell 带代理时，跑 HTTP_PROXY 的
		// Transport 会把 127.0.0.1 的请求也送进代理，症状是 "upstream connect
		// failed" 这种看起来像 Jaeger 挂了的假故障。
		HTTP: &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: nil}},
	}
}

// NewFromEnv 按环境变量构造（JAEGER_URL，缺省 DefaultBaseURL）。
func NewFromEnv() *Client {
	if v := strings.TrimSpace(os.Getenv("JAEGER_URL")); v != "" {
		return New(v)
	}
	return New(DefaultBaseURL)
}

// Query 检索条件。
//
// Start/End 必填且为**微秒**：见包注释第 3 条，`lookback` 不可靠。
type Query struct {
	Service string
	Start   time.Time
	End     time.Time
	Limit   int
}

// Reachable 探活：Jaeger 没起时返回明确的 error，调用方据此 t.Skip 而不是
// 让一堆 "connection refused" 淹没真正的失败原因。
func (c *Client) Reachable(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/services", nil)
	if err != nil {
		return err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("jaeger %s 不可达：%w", c.BaseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jaeger %s 返回 HTTP %d", c.BaseURL, resp.StatusCode)
	}
	return nil
}

// Fetch 按服务与窗口拉取 trace。
func (c *Client) Fetch(ctx context.Context, q Query) ([]Trace, error) {
	if q.Service == "" {
		return nil, fmt.Errorf("traceutil: Query.Service 为空")
	}
	if q.Limit <= 0 {
		q.Limit = 50
	}
	v := url.Values{
		"service": {q.Service},
		"start":   {strconv.FormatInt(q.Start.UnixMicro(), 10)},
		"end":     {strconv.FormatInt(q.End.UnixMicro(), 10)},
		"limit":   {strconv.Itoa(q.Limit)},
	}
	var payload struct {
		Data  []Trace  `json:"data"`
		Total int      `json:"total"`
		Errs  []string `json:"errors"`
	}
	if err := c.getJSON(ctx, "/api/traces?"+v.Encode(), &payload); err != nil {
		return nil, err
	}
	if len(payload.Errs) > 0 {
		return nil, fmt.Errorf("traceutil: jaeger 返回错误 %v", payload.Errs)
	}
	return payload.Data, nil
}

// FetchTrace 按 traceID 精取一条（16 或 32 位十六进制均可）。
func (c *Client) FetchTrace(ctx context.Context, traceID string) (*Trace, error) {
	var payload struct {
		Data  []Trace  `json:"data"`
		Errs  []string `json:"errors"`
		Total int      `json:"total"`
	}
	if err := c.getJSON(ctx, "/api/traces/"+url.PathEscape(traceID), &payload); err != nil {
		return nil, err
	}
	if len(payload.Data) == 0 {
		return nil, fmt.Errorf("traceutil: trace %s 未找到", traceID)
	}
	t := payload.Data[0]
	return &t, nil
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("traceutil: GET %s：%w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("traceutil: 读响应体：%w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("traceutil: GET %s 返回 HTTP %d：%s", path, resp.StatusCode, truncate(string(body), 200))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("traceutil: 解析 %s 响应失败：%w", path, err)
	}
	return nil
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return New(c.BaseURL).HTTP
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
