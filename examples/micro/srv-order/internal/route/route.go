// Package route srv-order 的入口路由表。
//
// 在分层链上的位置：**route** → middleware → handler → service →
// repository → model。它是链的第一节，kitex 的每个方法在这里登记读写语义
// 与中间件链。
//
// 为什么路由表要显式登记而不是"有中间件就全局套一遍"：读和写的保护策略
// 不一样（写限流更严、读限流更宽），一张表能让"哪个方法没被保护"这个问题
// 有唯一答案。漏登记的方法会在运行期告警（见 pkg/routing.Router.Enter），
// 而不是悄悄裸奔。
package route

import (
	"github.com/gospacex/logx"

	"github.com/gospacex/launchx/examples/micro/pkg/routing"
	"github.com/gospacex/launchx/examples/micro/srv-order/internal/middleware"
)

// NewRouter 构造订单域路由表。
//
// 中间件顺序有讲究：
//   - 审计在**链首**：被后面任何一节拒掉的请求都要留痕，否则"到底有没有进来过"
//     这个问题回答不了；
//   - 鉴权在审计之后、限流之前：先证明"你是谁"，再决定"给不给你配额"；
//   - 限流在链尾：只有确定要执行才计数，否则被鉴权拦下的请求也会消耗配额
//     —— 那等于让未授权流量有能力把合法用户的额度打光。
//
// gate 为 nil 表示鉴权未启用（路由层恒放行，见 routing.Authenticate）；
// 是否启用由装配层按配置决定并留痕，不是这里的默认值。
func NewRouter(limiter *middleware.RateLimiter, gate routing.Authenticator, traceOn bool, logger logx.Logger) *routing.Router {
	chain := []routing.Middleware{
		routing.Audit(logger),
		routing.Authenticate(gate, logger),
		routing.RateLimit(limiter),
	}
	return routing.NewRouter("order", traceOn, logger, []routing.Route{
		{Method: "CreateOrder", Kind: routing.KindWrite, Middlewares: chain},
		{Method: "GetOrder", Kind: routing.KindRead, Middlewares: chain},
		{Method: "ListOrders", Kind: routing.KindRead, Middlewares: chain},
		{Method: "Report", Kind: routing.KindRead, Middlewares: chain},
	}...)
}
