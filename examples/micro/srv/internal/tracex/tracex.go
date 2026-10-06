// Package tracex 业务侧分层埋点的极小封装。
//
// 它替换原 srv-order 的 infra/telemetry：那份实现自己 Init + InstallGlobal
// 一个全局 TracerProvider，而 launchx 的 telemetry 步骤已经注册好全局
// provider 与传播器，业务不该再抢一次全局位。这里只保留原包被用到的两个
// 能力（Start / Tracer），签名与命名约定逐字一致 —— 业务代码因此零改动，
// 只换 import 路径。
//
// span 名字约定 "<layer>.<entity>.<op>"（例如 service.order.CreateOrder、
// messaging.outbox.send），与迁移前 Jaeger 上的名字保持一致，迁移前后可比。
package tracex

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// tracerName 业务侧 instrumentation 名（区别于 dbx / mqx / rpcx 各自的 tracer 名）。
const tracerName = "srv"

// Start 开一个分层 span，名字为 layer.entity.op。
//
// 返回的 span **必须**由调用方 End：丢弃句柄会让 span 永不导出，
// 且下游子 span 挂在悬空 parent 上（Jaeger 上表现为 missing-parent）。
func Start(ctx context.Context, layer, entity, op string) (context.Context, trace.Span) {
	return Tracer().Start(ctx, layer+"."+entity+"."+op)
}

// provider 由装配层注入的 launchx provider；为零值时回落 otel 全局。
//
// 为什么要这层间接：业务分层拿不到 *launchx.App（分层边界要求它不认识框架），
// 而"经 launchx 取句柄"又要求 tracer 来源是装配好的 provider 而不是全局单例。
// 由装配层在 Setup 里调 UseProvider 交接，两边都成立。
var (
	providerMu sync.RWMutex
	provider   trace.TracerProvider
)

// UseProvider 由 app.Setup 调用，把 launchx 装配好的 provider 交给本包。
// 传 nil 表示回落到全局（例如 telemetry 未启用）。
func UseProvider(tp trace.TracerProvider) {
	providerMu.Lock()
	provider = tp
	providerMu.Unlock()
}

// Tracer 返回本进程的业务 tracer。
//
// 优先用装配层注入的 provider；未注入时向全局 TracerProvider 取，且不缓存成
// 包级变量：otel 的全局 provider 是委托实现，测试里后注册的 provider 也能被
// 先取到的 tracer 转发到（rpc_span_test.go 与 outbox_test.go 正是靠这一点注入
// 内存 exporter）。
func Tracer() trace.Tracer {
	providerMu.RLock()
	tp := provider
	providerMu.RUnlock()
	if tp == nil {
		return otel.GetTracerProvider().Tracer(tracerName)
	}
	return tp.Tracer(tracerName)
}
