// Package cacheutil 收口「读缓存 → miss 回源 → 回填」这一串动作。
//
// 为什么必须有它：四个域各写了一份同样的五六行（Get → Unmarshal → 回源 →
// Marshal → Set），micro 与 mall 两份复制一共 8 处。复制的代价不是"多写几行"，
// 而是**任何一处语义修正都要改 8 遍，漏改的那处不会报错，只会静默少一层保护**。
// 缓存穿透就是这么留下来的：查不存在的 ID 不回填 ⇒ 每次都穿到 DB，攻击面是
// "随便造一批不存在的 ID 就能把缓存整层旁路"，而修它要连着改八处，于是一直没改。
//
// 本包把这条链路收成一处后，语义只剩一份：
//
//   - 命中正缓存 → 直接返回，记 cache.hit；
//   - 命中空值哨兵 → 返回 *NotFound（**不回源**），记 cache.hit + cache.negative；
//   - miss → 回源；回源报 not found → 回填**空值哨兵**（负缓存）后返回 *NotFound；
//   - 回源报其他错 → 原样上抛，**不写任何缓存**（故障不能被缓存成"不存在"）；
//   - 回填失败只告警：缓存是加速路径，不是数据源。
//
// 缓存组件未装配（nil）时整体退化为"直接回源"，与改造前各域 `if s.cache != nil`
// 的行为一致 —— 缓存缺失是合法部署形态，不是故障。
package cacheutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gospacex/cachex"
	"github.com/gospacex/logx"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// ErrNotFound 「该键确实不存在」的哨兵错误。
//
// 调用方一律用 `errors.Is(err, cacheutil.ErrNotFound)` 判定，不要比字符串。
var ErrNotFound = errors.New("cacheutil: not found")

// MissSentinel 空值哨兵：缓存里写着它，表示"回源确认过，这个键不存在"。
//
// 取值刻意不是合法 JSON：命中判定时先比等值再 Unmarshal，业务体永远不可能
// 等于它，因此不需要额外的包装结构，也不会和"值为空对象"混淆。
const MissSentinel = "cacheutil:miss:v1"

// DefaultMissTTL 空值哨兵的缺省存活时间。
//
// 为什么比正缓存短得多：负缓存的固有代价是"记录创建后最长 MissTTL 时间内
// 仍然查不到"。代价由**写路径的失效**（见 Invalidate）兜底，但失效调用本身
// 可能失败（只告警），所以窗口必须留出可容忍的上界。30s 是"抗住一波穿透"
// 与"新建后可见性"之间的折中，各域可按数据敏感度覆盖（Options.MissTTL）。
const DefaultMissTTL = 30 * time.Second

// NotFound 「不存在」的具名错误：包住回源给出的原始错误。
//
// 为什么包一层而不是直接返回 ErrNotFound：调用方要按自己的错误码表翻译
// （订单 → CodeOrderNotFound、库存 → CodeStockNotFound…），但仍需要能
// `errors.Is` 到 repository.ErrNotFound 这类原始哨兵 —— Unwrap 保证这条链不断。
type NotFound struct{ Cause error }

// Error 实现 error。
func (e *NotFound) Error() string {
	if e.Cause == nil {
		return ErrNotFound.Error()
	}
	return ErrNotFound.Error() + ": " + e.Cause.Error()
}

// Unwrap 暴露回源的原始错误（保持 errors.Is 链）。
func (e *NotFound) Unwrap() error { return e.Cause }

// Is 让 errors.Is(err, ErrNotFound) 对「命中空值哨兵」（Cause 为 nil）也成立。
func (e *NotFound) Is(target error) bool { return target == ErrNotFound }

// Options 一次 Fetch 的参数。Label 与 Logger 只用于告警，可零值。
type Options struct {
	// Label 域标签，进告警日志（"order" / "product" / ...），便于定位是哪一域。
	Label string
	// TTL 正缓存存活时间（必填；<=0 时按不写缓存处理）。
	TTL time.Duration
	// MissTTL 空值哨兵存活时间，<=0 时取 DefaultMissTTL。
	MissTTL time.Duration
	// NotFound 判定回源错误是否为「不存在」（必填）；nil 时一律按故障处理，
	// 也就不会写空值哨兵 —— 宁可不挡穿透，也不能把故障缓存成"不存在"。
	NotFound func(error) bool
	// Logger 告警出口，nil 时静默（降级形态下各域 logger 可能未装配）。
	Logger logx.Logger
}

func (o Options) missTTL() time.Duration {
	if o.MissTTL > 0 {
		return o.MissTTL
	}
	return DefaultMissTTL
}

// Fetch 读缓存 → miss 回源 → 回填。cache 为 nil 时退化为直接回源。
//
// load 返回 (nil, not-found-error) 时，本函数返回 *NotFound；
// 命中空值哨兵时同样返回 *NotFound，但 Cause 为 nil（缓存里没有原始错误）。
func Fetch[T any](ctx context.Context, cache cachex.Cache, opt Options, key string,
	load func(context.Context) (*T, error)) (*T, error) {
	if cache != nil {
		if raw, err := cache.Get(ctx, key); err == nil && raw != "" {
			if raw == MissSentinel {
				mark(ctx, attribute.Bool("cache.hit", true), attribute.Bool("cache.negative", true))
				return nil, &NotFound{}
			}
			var v T
			if json.Unmarshal([]byte(raw), &v) == nil {
				mark(ctx, attribute.Bool("cache.hit", true))
				return &v, nil
			}
			// 解码失败按 miss 处理：缓存里是一份认不出的值，
			// 回源拿到正确结果后会覆盖它。
			opt.warn(ctx, "cache decode failed, fallback to db", key, nil)
		}
	}

	v, err := load(ctx)
	if err != nil {
		if opt.NotFound != nil && opt.NotFound(err) {
			// 回填空值哨兵：不写的话，每次查不存在的 ID 都会穿到 DB。
			put(ctx, cache, opt, key, MissSentinel, opt.missTTL())
			return nil, &NotFound{Cause: err}
		}
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	if b, mErr := json.Marshal(v); mErr == nil {
		put(ctx, cache, opt, key, string(b), opt.TTL)
	} else {
		opt.warn(ctx, "cache encode failed, skip fill", key, mErr)
	}
	return v, nil
}

// Invalidate 删键（连同可能存在的空值哨兵）。
//
// 写路径必须调它，而这一点在有负缓存之后变得更要紧：不失效的话，
// "查过一次不存在的 ID → 该 ID 被创建" 会让新记录在最长 MissTTL 内查不到
// —— 负缓存把一次读的副作用延长到了写之后。
func Invalidate(ctx context.Context, cache cachex.Cache, opt Options, key string) {
	if cache == nil {
		return
	}
	if err := cache.Delete(ctx, key); err != nil {
		opt.warn(ctx, "cache invalidate failed", key, err)
	}
}

// put 写缓存，失败只告警（缓存是加速路径，不是数据源）。
func put(ctx context.Context, cache cachex.Cache, opt Options, key, raw string, ttl time.Duration) {
	if cache == nil || ttl <= 0 {
		return
	}
	if err := cache.Set(ctx, key, raw, ttl); err != nil {
		opt.warn(ctx, "cache fill failed", key, err)
	}
}

// mark 往当前 span 打属性（无活动 span 时静默）。
func mark(ctx context.Context, attrs ...attribute.KeyValue) {
	if span := trace.SpanFromContext(ctx); span != nil && span.IsRecording() {
		span.SetAttributes(attrs...)
	}
}

// warn 告警（Logger 为 nil 时静默）。
func (o Options) warn(ctx context.Context, msg, key string, err error) {
	if o.Logger == nil {
		return
	}
	fields := []logx.Field{logx.String("key", key)}
	if o.Label != "" {
		fields = append(fields, logx.String("cache", o.Label))
	}
	if err != nil {
		fields = append(fields, logx.Err(err))
	}
	o.Logger.WarnContext(ctx, fmt.Sprintf("%s: %s", labelOf(o.Label), msg), fields...)
}

func labelOf(label string) string {
	if label == "" {
		return "cacheutil"
	}
	return label + " cache"
}
