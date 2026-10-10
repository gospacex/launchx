// Package rpcwire apps 的 RPC 传输层桥：把「业务码」与「身份」送过 kitex 的进程边界。
//
// 三条结论都是实测得出的，且**每一条都与直觉相反**，故写在包注释里：
//
//  1. **身份默认就能过界。** kitex 两侧各预装
//     `transmeta.Metainfo{Client,Server}Handler`，`metainfo.WithValue/GetValue`
//     在 bff→srv 这一跳可用。故 srv 侧 `admit` 读 metainfo 的分支是活的，
//     不再需要额外配置。
//
//  2. **业务码默认过不去，而且不报错。** 两侧默认都不装 TTHeader handler，
//     而 biz status error 恰恰由它编解码（kitex/pkg/kerrors/bizerrors.go 的
//     NOTICE）。服务端回 `*codes.Error` 时客户端拿到的是
//     `remote or network error[remote]: biz error: [40404] no such product` ——
//     一个字符串：`errors.As` 找不到 `*codes.Error`，`codes.From` 只能给 Internal。
//     后果是 bff 的错误码映射表整张失效，每个业务拒绝都变成 502「上游坏了」，
//     而链路、日志、HTTP 状态码全都正常 —— 没有任何一处告警。故两侧 MUST
//     叠加 TTHeader handler。
//
//  3. **转换只能在「框架之外」的两端做，中间件不行。** kitex 在
//     `server.go` 的 handleEndpoint 里做 `FromBizStatusError(err)` 并
//     `SetBizStatusErr`，而 `server.WithMiddleware` 装出来的链在它**外面**；
//     客户端同理：中间件链里读到的 err 是 **nil**，biz error 由链外从
//     `Invocation().BizStatusErr()` 物化成 `*kerrors.BizStatusError`。
//     实测：同一个码，最内层返回时客户端 `FromBizStatusError` 命中，
//     中间件里返回时命中不了。这是本包**不提供**任何中间件的原因，别再加回来。
//
// 于是方案是「两端各做一次，都不碰业务代码」：
//
//   - 服务端：让 `*codes.Error` 自己满足 `kerrors.BizStatusErrorIface`
//     （见 pkg/codes 的那三个方法）。业务里每次 `codes.New(...)` 一个字都不用改，
//     kitex 在最内层就认得它，把码写进 TTHeader；
//   - 客户端：调完之后用 `FromRPC` 把 `*kerrors.BizStatusError` 翻回业务码。
//     只有 bff 需要它（跨进程的唯一调用方）。
//
// 顺带一个正面副作用：biz error 在客户端中间件链内不可见 ⇒ rpcx 的
// `ClientSpanMiddleware` 也不会把「业务拒绝」标成 error span，
// 与 examples/micro 的「业务失败不污染错误率」口径一致。
//
// 反向地：网络层失败（超时 / 不可达 / 上游 panic）不经这里 —— 它到不了
// biz 通道，`FromRPC` 回落 `codes.From` 判为 Internal。与 micro 的
// 「两类错误不混用」同一条纪律。
package rpcwire

import (
	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/pkg/kerrors"
	"github.com/cloudwego/kitex/pkg/transmeta"
	"github.com/cloudwego/kitex/server"

	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
)

// 编译期断言：业务码类型必须满足 kitex 的 biz status error 接口。
//
// 这是服务端那条链的**唯一**支点 —— 接口少一个方法，业务码就会静默退回
// 「字符串过界」，而服务照常起、测试照常绿（除非有 rpcwire 的端到端判据）。
var _ kerrors.BizStatusErrorIface = (*codes.Error)(nil)

// IdentityMetaKey 跨进程透传身份的 kitex metainfo 键名。
//
// **只在这里定义一次**：写入侧是 bff（`metainfo.WithValue`），读取侧是四个
// srv 的 `rpc.admit`（`metainfo.GetValue`）。五处各写一份字面量必有
// 「改一处漏四处」，而漏掉的症状是身份永远读成零值 —— 限流静默退化成
// 服务级计数（单用户打满不影响别人），没有任何报错。
//
// 与 examples/micro 的 pkg/auth 用同一个字面值（`MetaKeyUserID`），
// 但不 import 它：apps 与 micro 之间必须零耦合（见 mesh-app-layering）。
const IdentityMetaKey = "mesh-user-id"

// ServerOptions 四个 srv 的 `app/setup.go` MUST 叠加的服务端传输选项：
//
//	opts = append(opts, rpcwire.ServerOptions()...)
//
// 漏掉的症状是「码静默消失」而不是装配失败，故它必须由判据钉住
// （见 Task 9 的依赖面/接线核对），不能靠实现者记得。
func ServerOptions() []server.Option {
	return []server.Option{
		server.WithMetaHandler(transmeta.ServerTTHeaderHandler),
	}
}

// ClientOptions bff 的 downstream MUST 叠加的客户端传输选项。
//
// WithMetaHandler 是**追加**语义：默认的 MetainfoClientHandler 仍在，
// 身份键与 biz status error 各走各的通道，互不影响。
func ClientOptions() []client.Option {
	return []client.Option{
		client.WithMetaHandler(transmeta.ClientTTHeaderHandler),
	}
}

// FromRPC 把一个跨进程回来的错误还原成业务码。
//
// 调用点必须在**调用之后**（不能塞进中间件：那里读不到 biz error，见包注释第 3 条）。
// 只有 bff 用它；srv 之间不互调，故没有第二个调用方。
//
// 回落顺序刻意是「先 biz 后 codes.From」：本进程内直接构造的错误（单测、
// 直连调用）走 codes.From；跨进程回来的走 biz 通道。两者都不命中即 Internal
// ——「下游挂了」不该被报成「商品不存在」。
func FromRPC(err error) (codes.Code, string) {
	if err == nil {
		return codes.OK, ""
	}
	if biz, ok := kerrors.FromBizStatusError(err); ok {
		return codes.Code(biz.BizStatusCode()), biz.BizMessage()
	}
	return codes.From(err)
}
