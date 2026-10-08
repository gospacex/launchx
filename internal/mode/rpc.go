package mode

import (
	"fmt"
	"strings"

	"github.com/gospacex/configx"
	"github.com/gospacex/rpcx"
)

// rpcSection 出向 RPC 治理配置所在的段。
//
// 放在 xmiddleware 下与其他组件段同构（xmiddleware.mqx / kvx / lockx ...），
// 但它**不是组件**：这三项只在客户端选项装配那一刻起作用，故由本包读取、
// 由 rpcx 落地，不注册成 component 的 builder（组件层挂载晚于 mode，会成环）。
const rpcSection = "xmiddleware.rpcx"

// RPCGovernance 出向 RPC 治理参数（装配期解析并校验一次）。
type RPCGovernance struct {
	Retry          rpcx.RetryOptions
	CircuitBreaker rpcx.CircuitBreakerOptions
	LoadBalancer   rpcx.LoadBalancer
}

// metricsSection 指标段的键前缀。enabled 决定是否装配 MeterProvider 与是否挂
// RPC 指标中间件；addr 只对**没有 HTTP 面**的 srv 档有意义（HTTP 档的 /metrics
// 由 webx 引擎自带）。
const (
	metricsEnabledKey = "metrics.enabled"
	metricsAddrKey    = "metrics.addr"
)

// readRPCGovernance 从配置读出向治理参数；未配置即全关（presence-based）。
//
// **校验即退**：取值非法（未知负载均衡器、越界重试次数、超范围错误率）当场返回
// error，不做降级。理由：这三项都是"配错了也不会报错"的类型 —— 静默退回默认值
// 的症状是「按默认策略跑了很久却没人在看」，比启动失败贵得多。
func readRPCGovernance(cfg configx.Config) (RPCGovernance, error) {
	var g RPCGovernance

	lb, err := rpcx.ParseLoadBalancer(cfg.GetString(rpcSection + ".load_balancer"))
	if err != nil {
		return g, fmt.Errorf("launchx: %s.load_balancer: %w", rpcSection, err)
	}
	g.LoadBalancer = lb

	g.Retry = rpcx.RetryOptions{
		Enabled:       cfg.GetBool(rpcSection + ".retry.enabled"),
		MaxRetryTimes: cfg.GetInt(rpcSection + ".retry.max_attempts"),
		MaxDurationMS: uint32(max(0, cfg.GetInt(rpcSection+".retry.max_duration_ms"))),
		BackoffMS:     cfg.GetInt(rpcSection + ".retry.backoff_ms"),
	}
	if g.Retry.MaxRetryTimes < 0 || g.Retry.MaxRetryTimes > rpcx.RetryTimesLimit {
		return g, fmt.Errorf(
			"launchx: %s.retry.max_attempts = %d 超出 [0,%d]（不含首次调用；kitex 对越界值直接 panic，故在此拦住）",
			rpcSection, g.Retry.MaxRetryTimes, rpcx.RetryTimesLimit)
	}
	if g.Retry.BackoffMS < 0 {
		return g, fmt.Errorf("launchx: %s.retry.backoff_ms = %d 不能为负", rpcSection, g.Retry.BackoffMS)
	}

	errRate := cfg.GetFloat64(rpcSection + ".circuit_breaker.error_rate")
	if errRate < 0 || errRate > 1 {
		return g, fmt.Errorf("launchx: %s.circuit_breaker.error_rate = %v 超出 (0,1]", rpcSection, errRate)
	}
	minSample := cfg.GetInt64(rpcSection + ".circuit_breaker.min_sample")
	if minSample < 0 {
		return g, fmt.Errorf("launchx: %s.circuit_breaker.min_sample = %d 不能为负", rpcSection, minSample)
	}
	g.CircuitBreaker = rpcx.CircuitBreakerOptions{
		Enabled:   cfg.GetBool(rpcSection + ".circuit_breaker.enabled"),
		ErrRate:   errRate,
		MinSample: minSample,
	}
	return g, nil
}

// rpcGovernanceSummary 产出**只含已生效项**的一行摘要（启动日志用）。
//
// 为什么要有它：这三个开关"看起来配了但没生效"是本轮修的一类病（配置写了
// 没人读、开关串错段键）。把装配期真实的裁决结果打出来，人对照 yaml 一眼能看出
// 差异在哪 —— 比让他去翻代码路径便宜。
func rpcGovernanceSummary(g RPCGovernance, metrics bool) string {
	parts := make([]string, 0, 4)
	if g.Retry.Enabled {
		parts = append(parts, fmt.Sprintf("retry(max_attempts=%d, backoff_ms=%d, max_duration_ms=%d)",
			g.Retry.MaxRetryTimes, g.Retry.BackoffMS, g.Retry.MaxDurationMS))
	}
	if g.CircuitBreaker.Enabled {
		parts = append(parts, fmt.Sprintf("circuit_breaker(error_rate=%g, min_sample=%d)",
			g.CircuitBreaker.ErrRate, g.CircuitBreaker.MinSample))
	}
	if g.LoadBalancer != rpcx.LoadBalanceDefault {
		parts = append(parts, "load_balancer="+string(g.LoadBalancer))
	}
	if metrics {
		parts = append(parts, "metrics=on")
	}
	if len(parts) == 0 {
		return "rpc governance: all off (xmiddleware.rpcx 未配置)"
	}
	return "rpc governance: " + strings.Join(parts, " ")
}
