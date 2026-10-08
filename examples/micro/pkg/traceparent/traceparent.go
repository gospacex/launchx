// Package traceparent 取当前链路的 W3C traceparent，供"把链路 ID 落库/落消息头"
// 这类跨进程续链场景使用。
//
// 为什么单独拆一个包：取 traceparent 要走库侧的传播器入口（launchx.Inject），
// 而**业务分层不该认识框架**（这条纪律在 micro/srv/internal/tracex 的包注释里
// 就写着）。此前四份 `traceparentOf` 各自直接 import launchx，等于把这条纪律
// 破了四次，且四份实现必须永远保持一致 —— 一致性靠人工同步，迟早会漂。
//
// 收口之后：只有本包认识 launchx，四个域服的 service 层只认识本包。
// 库侧换传播器实现时，改动落在一处。
package traceparent

import (
	"context"

	"go.opentelemetry.io/otel/propagation"

	"github.com/gospacex/launchx"
)

// Key W3C TraceContext 的头部键（与 pkg/events.TraceHeaderKey 同一个值）。
const Key = "traceparent"

// Of 取当前链路的 traceparent；traceOn 为假或无活动 span 时返回空串。
//
// 为什么 traceOn 也管这里：这一段是**业务分层**的行为（要不要把链路 ID 带出去），
// 不是传播器的行为。trace.enabled=false 时业务不该主动造链路元数据，
// 否则落库的 traceparent 会指向一条根本没有被采样的 trace。
func Of(ctx context.Context, traceOn bool) string {
	if !traceOn {
		return ""
	}
	carrier := propagation.MapCarrier(map[string]string{})
	launchx.Inject(ctx, carrier)
	return carrier.Get(Key)
}
