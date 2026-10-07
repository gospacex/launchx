// Package tracex 分层埋点的极小封装，与微服 / 电商示例的 pkg/tracex 同口径。
//
// 统一后的调用形态（三个示例一致）：
//
//	ctx, end := x.begin(ctx, "Op")
//	defer func() { end(err) }()
//
// 为什么收尾要收 error：span 句柄不交给调用方（业务分层因此不碰 otel 的
// SetStatus / RecordError），代价是调用方也标不了状态 —— 由收尾闭包代标，
// 失败才在 Jaeger 上可见。否则「SQL 报错」与「SQL 成功」同色，错误率指标
// 会系统性低估：没人能在 trace 上看出来这一路是失败的。
//
// 前提是方法的结果列表已命名（Go 要求「全命名或全不命名」），defer 里的 err
// 才能覆盖所有 return 路径，包括直接返回字面量错误的那些路径。
//
// 与微服版的唯一差别是 tracer 从哪来：微服是四服各自一个进程、共享包级
// provider（pkg/tracex.UseProvider 由装配层交接）；单体是单进程，装配层把
// tracer **显式注入各层**更直白，不引入包级可变状态。两边的 span 命名、
// 错误标记与 quiet 豁免规则完全一致。
package tracex

import (
	"context"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// End 收尾闭包：传本次调用的 error，由它决定 span 状态并收尾。
type End func(err error)

// Begin 开一个 layer.entity.op span，返回收尾闭包。
//
// quiet 判定「不是故障」的错误（如持久层的 ErrNotFound —— 一次未命中是正常
// miss，不是错误）：命中时不改 span 状态，避免它污染错误率。nil 表示不豁免。
//
// 注意：err 必须是**接口意义上的**非空判断 —— 传 *codes.Error 这类具体指针时
// 先判空再进接口，否则 nil 指针装进 error 接口后 != nil，成功路径会被标成错误。
func Begin(tr trace.Tracer, ctx context.Context, layer, entity, op string, quiet func(error) bool) (context.Context, End) {
	c, span := tr.Start(ctx, layer+"."+entity+"."+op)
	return c, func(err error) {
		if err != nil && (quiet == nil || !quiet(err)) {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}
}
