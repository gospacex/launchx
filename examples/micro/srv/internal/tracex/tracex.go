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

// Tracer 返回本进程的业务 tracer。
//
// 每次都向全局 TracerProvider 取，不缓存成包级变量：otel 的全局 provider 是
// 委托实现，测试里后注册的 provider 也能被先取到的 tracer 转发到
// （rpc_span_test.go 与 outbox_test.go 正是靠这一点注入内存 exporter）。
func Tracer() trace.Tracer {
	return otel.GetTracerProvider().Tracer(tracerName)
}
