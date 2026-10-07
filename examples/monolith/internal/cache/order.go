// Package cache 订单缓存适配层：把 cachex 的读写包成本层的 `cache.*` span。
//
// 存在原因：cachex 自己会产 span（EnableTrace 打开时），但那是 SDK 视角的
// 「一次 redis 命令」；调用方视角的「这次读缓存是为了拿哪个订单」只有本层知道。
// 两层并存不是重复埋点 —— Jaeger 上 `cache.GetOrder` 是业务动作、
// 其下的 redis span 是外部调用，丢掉前者就无法回答"谁在访问缓存"。
//
// 边界：只做「一个 key 的一次读写」，不做缓存策略（穿透/击穿/雪崩由 service 决定）。
package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gospacex/cachex"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/gospacex/launchx/examples/monolith/internal/tracex"
)

// 本层 span 前缀，与服务 / 数据层的前缀互不相犯。
const spanPrefix = "cache."

// Order 订单缓存。
type Order struct {
	inst cachex.Cache
	ttl  time.Duration
	tr   trace.Tracer
}

// NewOrder 构造订单缓存。inst 由装配层从 App.Cache() 取（业务不自建连接）。
func NewOrder(inst cachex.Cache, ttl time.Duration, tr trace.Tracer) *Order {
	return &Order{inst: inst, ttl: ttl, tr: tr}
}

// key 订单缓存键。集中一处，避免读写两侧拼法不一致导致「写了读不到」。
func key(orderNo string) string { return "order:" + orderNo }

// begin 开本层 span（名字沿用既有的两段式 cache.<Op>，不走三段式，
// 改名会打断既有 trace 判据），返回「收 error 再收尾」的闭包。
func (c *Order) begin(ctx context.Context, op string) (context.Context, tracex.End) {
	return tracex.BeginSpan(c.tr, ctx, spanPrefix+op, nil)
}

// Get 读订单缓存。未命中返回 ("", nil) —— miss 不是错误，
// 与「缓存不可用」是两个语义：后者才是 span 上的 Error。
func (c *Order) Get(ctx context.Context, orderNo string) (v string, err error) {
	ctx, end := c.begin(ctx, "GetOrder")
	defer func() { end(err) }()
	tracex.Attrs(ctx, attribute.String("cache.key", key(orderNo)))

	v, err = c.inst.Get(ctx, key(orderNo))
	if err != nil {
		err = fmt.Errorf("cache: get order %q: %w", orderNo, err)
		return "", err
	}
	tracex.Attrs(ctx, attribute.Bool("cache.hit", v != ""))
	return v, nil
}

// Set 写订单缓存。
func (c *Order) Set(ctx context.Context, orderNo, payload string) (err error) {
	ctx, end := c.begin(ctx, "SetOrder")
	defer func() { end(err) }()
	tracex.Attrs(ctx,
		attribute.String("cache.key", key(orderNo)),
		attribute.Int("cache.ttl_ms", int(c.ttl.Milliseconds())),
	)

	if err = c.inst.Set(ctx, key(orderNo), payload, c.ttl); err != nil {
		err = fmt.Errorf("cache: set order %q: %w", orderNo, err)
	}
	return err
}

// Delete 删订单缓存（改状态/删单后必须失效，否则读 cache 会拿到旧值）。
func (c *Order) Delete(ctx context.Context, orderNo string) (err error) {
	ctx, end := c.begin(ctx, "DeleteOrder")
	defer func() { end(err) }()
	tracex.Attrs(ctx, attribute.String("cache.key", key(orderNo)))

	if err = c.inst.Delete(ctx, key(orderNo)); err != nil {
		err = fmt.Errorf("cache: delete order %q: %w", orderNo, err)
	}
	return err
}

// Encode 把任意值序列化成缓存值（JSON），供 service 层统一编码口径。
func Encode(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("cache: encode: %w", err)
	}
	return string(b), nil
}
