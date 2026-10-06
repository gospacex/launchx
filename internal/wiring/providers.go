package wiring

import (
	"errors"
	"fmt"

	"go.opentelemetry.io/otel"

	"github.com/gospacex/launchx/internal/component"
	"github.com/gospacex/launchx/internal/mode"
)

// 本文件是 wire 的 provider 面。每个 provider 做三件事里的前两件，第三件不做：
//
//  1. 解决 type identity（返回具名句柄类型，让 wire 能求解）；
//  2. 在**运行时**读 Env 裁决 presence / trace.on / strict；
//  3. **不**自己实现构造逻辑 —— 可选组件一律委派给 component 层的 builder
//     注册表（component.Lookup），避免这里长出第二份构造真相。
//
// 第 3 点是刻意的：component builder 里已经写好了 presence 判定、strict 双分支
// 与 trace.on 的入链开关。provider 若重写一遍，两边必然漂移，而且漂移的形态
// 很难查（一侧改了另一侧没有，表现为「配置写了却不生效」）。

// ErrFoundationNotReady 地基未就绪。地基失败 MUST 即退，不受 strict 影响，
// 因此这里的 error 不做任何降级判断。
var ErrFoundationNotReady = errors.New("wiring: 地基未就绪（pipeline 未完成 configx/logx/otelx 装配）")

// ===== 地基（恒构造，失败即退）=====

// ProvideConfig configx 句柄。
func ProvideConfig(env *component.Env) (ConfigHandle, error) {
	if env == nil || env.Cfg == nil {
		return ConfigHandle{}, fmt.Errorf("%w: configx", ErrFoundationNotReady)
	}
	return ConfigHandle{base: base{name: "configx", raw: env.Cfg}}, nil
}

// ProvideLogger logx 句柄。
func ProvideLogger(env *component.Env) (LoggerHandle, error) {
	if env == nil || env.Logger == nil {
		return LoggerHandle{}, fmt.Errorf("%w: logx", ErrFoundationNotReady)
	}
	return LoggerHandle{base: base{name: "logx", raw: env.Logger}}, nil
}

// ProvideTelemetry otelx 句柄。
//
// 这里读的是全局 TracerProvider：telemetry 步骤把它设进去，其余组件与中间件
// 都从这里取。取不到就意味着 pipeline 没跑到那一步，**必须**报错而不是降级成
// noop —— 静默变成 noop 的后果是「链路看起来全绿实则全是空 span」。
func ProvideTelemetry(env *component.Env) (TelemetryHandle, error) {
	if env == nil {
		return TelemetryHandle{}, fmt.Errorf("%w: otelx", ErrFoundationNotReady)
	}
	tp := otel.GetTracerProvider()
	if tp == nil {
		return TelemetryHandle{}, fmt.Errorf("%w: otelx（全局 TracerProvider 为空）", ErrFoundationNotReady)
	}
	return TelemetryHandle{base: base{name: "otelx", raw: tp, trace: env.TraceOnOf("telemetry")}}, nil
}

// ===== 可选组件：委派给 component builder =====

// runBuilder 执行 component 层的 builder，并完成 strict 裁决。
//
// 返回值语义：
//   - ok=false 且 err=nil → presence 未命中或未登记，降级为零值；
//   - ok=false 且 err!=nil → strict 命中，调用方 MUST 把错误交出去；
//   - ok=true → 构造完成，产物由调用方从 Env 读取。
//
// 注意 builder **不会**把 presence 未命中报成 error，所以「没配」和「配了但失败」
// 在这里是可区分的，这正是我们不能把 presence 搬进图的原因之一。
func runBuilder(env *component.Env, builderName string) (ok bool, err error) {
	b := component.Lookup(builderName)
	if b == nil {
		return false, nil
	}
	if buildErr := b(env); buildErr != nil {
		if env.StrictOf(builderName) {
			return false, fmt.Errorf("init %s: %w", builderName, buildErr)
		}
		return false, nil
	}
	return true, nil
}

// ProvideDB dbx 句柄（Env.DB 是 any，靠 DBHandle 入图）。
func ProvideDB(env *component.Env) (DBHandle, error) {
	ok, err := runBuilder(env, "db")
	if err != nil {
		return DBHandle{}, err
	}
	if !ok {
		return DBHandle{}, nil
	}
	// D1 红线：trace 只记录，不决定是否构造 —— env.DB 的有无与 trace 无关。
	return DBHandle{base: base{name: "dbx", raw: env.DB, trace: env.TraceOnOf("db")}}, nil
}

// ProvideCache cachex 句柄。
func ProvideCache(env *component.Env) (CacheHandle, error) {
	ok, err := runBuilder(env, "cache")
	if err != nil {
		return CacheHandle{}, err
	}
	if !ok {
		return CacheHandle{}, nil
	}
	return CacheHandle{base: base{name: "cachex", raw: env.Cache, trace: env.TraceOnOf("cache")}}, nil
}

// ProvideMQ mqx 句柄。
func ProvideMQ(env *component.Env) (MQHandle, error) {
	ok, err := runBuilder(env, "mq")
	if err != nil {
		return MQHandle{}, err
	}
	if !ok {
		return MQHandle{}, nil
	}
	return MQHandle{base: base{name: "mqx", raw: env.MQ, trace: env.TraceOnOf("mq")}}, nil
}

// ProvideCoord coordx 句柄（注册侧与发现侧一起返回）。
func ProvideCoord(env *component.Env) (CoordHandle, error) {
	ok, err := runBuilder(env, "registry")
	if err != nil {
		return CoordHandle{}, err
	}
	if !ok {
		return CoordHandle{}, nil
	}
	return CoordHandle{
		base:     base{name: "coordx", raw: env.Registry, trace: env.TraceOnOf("registry")},
		Registry: env.Registry,
		Discover: env.Discover,
	}, nil
}

// ProvideSearch searchx 句柄。
func ProvideSearch(env *component.Env) (SearchHandle, error) {
	ok, err := runBuilder(env, "search")
	if err != nil {
		return SearchHandle{}, err
	}
	if !ok {
		return SearchHandle{}, nil
	}
	return SearchHandle{base: base{name: "searchx", raw: env.Search, trace: env.TraceOnOf("search")}}, nil
}

// ProvideMongo mongodbx 句柄。
func ProvideMongo(env *component.Env) (MongoHandle, error) {
	ok, err := runBuilder(env, "mongodb")
	if err != nil {
		return MongoHandle{}, err
	}
	if !ok {
		return MongoHandle{}, nil
	}
	return MongoHandle{base: base{name: "mongodbx", raw: env.Mongo, trace: env.TraceOnOf("mongodb")}}, nil
}

// ProvideKV kvx 句柄。
func ProvideKV(env *component.Env) (KVHandle, error) {
	ok, err := runBuilder(env, "kv")
	if err != nil {
		return KVHandle{}, err
	}
	if !ok {
		return KVHandle{}, nil
	}
	return KVHandle{base: base{name: "kvx", raw: env.KV, trace: env.TraceOnOf("kv")}}, nil
}

// ProvideLock lockx 句柄。
func ProvideLock(env *component.Env) (LockHandle, error) {
	ok, err := runBuilder(env, "lock")
	if err != nil {
		return LockHandle{}, err
	}
	if !ok {
		return LockHandle{}, nil
	}
	return LockHandle{base: base{name: "lockx", raw: env.Lock, trace: env.TraceOnOf("lock")}}, nil
}

// ===== 登记在册但默认不构造 =====

// ProvideObject objectx 句柄。
//
// 目前恒返回零值：objectx 尚未进入 launchx 的 go.mod（接进来会拖入阿里云 /
// 华为云 / 七牛 / minio / 腾讯 COS 全套依赖，与依赖面收敛冲突）。一旦接入，
// 只需在这里补真正的构造调用，图不用动 —— 这就是「登记在册」的价值。
func ProvideObject(env *component.Env) (ObjectHandle, error) {
	if env == nil || !hasSection(env, "objectx") {
		return ObjectHandle{}, nil
	}
	return ObjectHandle{}, nil
}

// ProvideVector vectorx 句柄，理由同 ProvideObject。
func ProvideVector(env *component.Env) (VectorHandle, error) {
	if env == nil || !hasSection(env, "vectorx") {
		return VectorHandle{}, nil
	}
	return VectorHandle{}, nil
}

// hasSection 判断 yaml 里有没有配置该段。
//
// presence 判定**必须**在运行时做（见文件头第 2 点）。这里之所以还要判一次，
// 是为了让「配了却没人接」这种情况能留下痕迹，而不是静默返回零值 —— 配了段
// 却拿不到句柄是最难排查的一类问题。
func hasSection(env *component.Env, key string) bool {
	if env.Cfg == nil {
		return false
	}
	return env.Cfg.Get(key) != nil
}

// ===== 形态装配产物 =====

// ModeReady 是「形态装配已执行」的标记类型。
//
// 它存在的唯一目的是让 wire 求解出正确的顺序：ProvideRPC / ProvideWeb 依赖它，
// 于是 mode.Assemble 必然排在读 Out 之前。没有这个标记，两个 provider 各自读
// Out 会拿到未装配的零值 —— 这正是 wire 值得用的地方（顺序不再是靠人记）。
type ModeReady struct {
	holders *mode.AppHolders
}

// ProvideModeReady 执行形态装配。
func ProvideModeReady(holders *mode.AppHolders) (ModeReady, error) {
	if holders == nil {
		return ModeReady{}, errors.New("wiring: mode.AppHolders 为空")
	}
	if err := mode.Assemble(holders); err != nil {
		return ModeReady{}, fmt.Errorf("wiring: 形态装配失败: %w", err)
	}
	return ModeReady{holders: holders}, nil
}

// ProvideRPC rpcx 句柄：kitex 服务端选项与下游客户端选项工厂的产出。
func ProvideRPC(r ModeReady) (RPCHandle, error) {
	out := r.holders.Out
	if out.RPCServer == nil && out.RPCClientOptions == nil {
		// srv 档只有服务端，bff/gateway 只有客户端工厂，两者都空才是真没装配。
		return RPCHandle{}, nil
	}
	return RPCHandle{base: base{name: "rpcx", raw: out.RPCServer, trace: r.holders.Tracer != nil}}, nil
}

// ProvideWeb webx 句柄：HTTP 引擎。
func ProvideWeb(r ModeReady) (WebHandle, error) {
	out := r.holders.Out
	if out.Webx == nil {
		// srv 档不建 webx，这是**预期行为**不是错误（四档互斥，见 mode 包）。
		return WebHandle{}, nil
	}
	return WebHandle{base: base{name: "webx", raw: out.Webx}}, nil
}
