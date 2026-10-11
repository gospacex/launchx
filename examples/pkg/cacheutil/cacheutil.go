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
//   - 读/解码/回填失败一律只告警并留 span 属性 —— 缓存坏了必须降级到回源，
//     但**不能静默**（"命中率看着很高却每次都打库"就是这么来的）。
//
// 缓存组件未装配（nil）时整体退化为"直接回源"，与改造前各域 `if s.cache != nil`
// 的行为一致 —— 缓存缺失是合法部署形态，不是故障。
//
// 缓存侧只依赖 Store 窄接口而非 `cachex.Cache`：monolith 的 `cache.Order` 在自己
// 那层还要包一层业务 span（"谁在读缓存"），它的 Set 签名又自带 TTL，逼它去实现
// cachex 的完整接口没有意义。**收口的是策略，不是连接**。
package cacheutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gospacex/logx"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Store 本包对缓存的全部要求。`cachex.Cache` 天然满足。
//
// 只收这三个方法而不是整个 cachex.Cache：后者还有 Exists/Keys/Close/HealthCheck
// 等一整套连接生命周期方法，而本包是**读策略**，不负责这些。收窄之后，
// monolith 那种"外面还包了一层业务 span"的适配器也能直接进来。
type Store interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
}

// ErrNotFound 「该键确实不存在」的哨兵错误。
//
// 调用方一律用 `errors.Is(err, cacheutil.ErrNotFound)` 判定，不要比字符串。
var ErrNotFound = errors.New("cacheutil: not found")

// Validatable 可选实现：让值自己声明"这份缓存值完不完整"。
//
// 为什么需要它：反序列化成功 ≠ 值完整。mesh 两个域此前用 `ID == 0` 判半截对象
// —— 那是一条真实的防御（缓存里混进半个对象时会一路当命中返回给用户），
// 收口时不能悄悄丢掉，否则"修了穿透"换来"返回半个订单"。
//
// 做成接口而不是 Options 里的一个回调字段：`Options` 不是泛型的，塞不进
// `func(*T) bool`；再者"什么样的值算完整"本来就只有值自己知道。
// 不实现即不校验（micro/mall 的聚合出参本来就没有这一层）。
type Validatable interface {
	Valid() bool
}

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

// Result 一次 FetchResult 的结果。Value 与错误的关系同 Fetch，
// 多出来的两个标志供调用方区分"值从哪来"。
type Result[T any] struct {
	// Value 取到的值；出错时为 nil。
	Value *T
	// Hit 命中正缓存（未回源）。
	Hit bool
	// Negative 命中空值哨兵（未回源，且此前回源确认过该键不存在）。
	Negative bool
}

// Fetch 读缓存 → miss 回源 → 回填。store 为 nil 时退化为直接回源。
//
// load 返回 (nil, not-found-error) 时，本函数返回 *NotFound；
// 命中空值哨兵时同样返回 *NotFound，但 Cause 为 nil（缓存里没有原始错误）。
func Fetch[T any](ctx context.Context, store Store, opt Options, key string,
	load func(context.Context) (*T, error)) (*T, error) {
	r, err := FetchResult[T](ctx, store, opt, key, load)
	return r.Value, err
}

// FetchResult 同 Fetch，但把命中状态一并返回。
//
// 需要它的原因：mesh 两个域的读接口带 `Cached` 出参（"这次是不是命中缓存"），
// 那是验收判据的一部分，不能因为收口就丢。只靠 span 上的 cache.hit 属性不够 ——
// 判据在进程内断言，读不到 span。
func FetchResult[T any](ctx context.Context, store Store, opt Options, key string,
	load func(context.Context) (*T, error)) (Result[T], error) {
	if store != nil {
		raw, err := store.Get(ctx, key)
		switch {
		case err != nil:
			// 缓存不可用 ≠ 没有这条数据。降级回源，但必须留痕：
			// 不记的话"命中率归零"只能靠猜。
			mark(ctx, attribute.Bool("cache.read_failed", true))
			opt.warn(ctx, "cache read failed, fallback to db", key, err)
		case raw == MissSentinel:
			mark(ctx, attribute.Bool("cache.hit", true), attribute.Bool("cache.negative", true))
			return Result[T]{Negative: true}, &NotFound{}
		case raw != "":
			var v T
			if json.Unmarshal([]byte(raw), &v) == nil {
				if vv, ok := any(&v).(Validatable); ok && !vv.Valid() {
					// 解得出但字段是残缺的（如主表 ID 为 0）：按 miss 处理，
					// 回源拿到完整值后会覆盖它。
					mark(ctx, attribute.Bool("cache.invalid", true))
					opt.warn(ctx, "cache value incomplete, fallback to db", key, nil)
					break
				}
				mark(ctx, attribute.Bool("cache.hit", true))
				return Result[T]{Value: &v, Hit: true}, nil
			}
			// 解码失败按 miss 处理：缓存里是一份认不出的值，
			// 回源拿到正确结果后会覆盖它。
			mark(ctx, attribute.Bool("cache.decode_failed", true))
			opt.warn(ctx, "cache decode failed, fallback to db", key, nil)
		}
	}

	v, err := load(ctx)
	if err != nil {
		if opt.NotFound != nil && opt.NotFound(err) {
			// 回填空值哨兵：不写的话，每次查不存在的 ID 都会穿到 DB。
			put(ctx, store, opt, key, MissSentinel, opt.missTTL())
			return Result[T]{}, &NotFound{Cause: err}
		}
		return Result[T]{}, err
	}
	if v == nil {
		return Result[T]{}, nil
	}
	if b, mErr := json.Marshal(v); mErr == nil {
		put(ctx, store, opt, key, string(b), opt.TTL)
	} else {
		opt.warn(ctx, "cache encode failed, skip fill", key, mErr)
	}
	return Result[T]{Value: v}, nil
}

// Invalidate 删键（连同可能存在的空值哨兵）。
//
// 写路径必须调它，而这一点在有负缓存之后变得更要紧：不失效的话，
// "查过一次不存在的 ID → 该 ID 被创建" 会让新记录在最长 MissTTL 内查不到
// —— 负缓存把一次读的副作用延长到了写之后。
func Invalidate(ctx context.Context, store Store, opt Options, key string) {
	if store == nil {
		return
	}
	if err := store.Delete(ctx, key); err != nil {
		opt.warn(ctx, "cache invalidate failed", key, err)
	}
}

// put 写缓存，失败只告警（缓存是加速路径，不是数据源）。
func put(ctx context.Context, store Store, opt Options, key, raw string, ttl time.Duration) {
	if store == nil || ttl <= 0 {
		return
	}
	if err := store.Set(ctx, key, raw, ttl); err != nil {
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
