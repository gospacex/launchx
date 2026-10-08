// Package route srv-points 的入口路由表。
//
// 在分层链上的位置：**route** → middleware → handler → service →
// repository → model。它是链的第一节，kitex 的每个方法在这里登记读写语义
// 与中间件链。
//
// 为什么路由表要显式登记而不是"有中间件就全局套一遍"：读和写的保护策略
// 不一样（写按用户维度限流更严、读按用户限流更宽），一张表能让"哪个方法
// 没被保护"这个问题有唯一答案。漏登记的方法会在运行期告警（见
// pkg/routing.Router.Enter），而不是悄悄裸奔。
package route

import (
	"github.com/gospacex/logx"

	"github.com/gospacex/launchx/examples/micro/pkg/routing"
	"github.com/gospacex/launchx/examples/micro/srv-points/internal/middleware"
)

// NewRouter 构造积分域路由表。
//
// 中间件顺序有讲究：审计在链首（被拒的请求也要留痕），鉴权居中
// （先证明"你是谁"再决定"给不给配额"），限流在链尾（只有确定要执行才计数，
// 否则未授权流量也能把合法用户的额度打光）。详见 srv-order/internal/route。
func NewRouter(limiter *middleware.RateLimiter, gate routing.Authenticator, traceOn bool, logger logx.Logger) *routing.Router {
	chain := []routing.Middleware{
		routing.Audit(logger),
		routing.Authenticate(gate, logger),
		routing.RateLimit(limiter),
	}
	return routing.NewRouter("points", traceOn, logger, []routing.Route{
		{Method: "Earn", Kind: routing.KindWrite, Middlewares: chain},
		{Method: "Consume", Kind: routing.KindWrite, Middlewares: chain},
		{Method: "GetAccount", Kind: routing.KindRead, Middlewares: chain},
		{Method: "ListLedgers", Kind: routing.KindRead, Middlewares: chain},
		{Method: "Report", Kind: routing.KindRead, Middlewares: chain},
	}...)
}
