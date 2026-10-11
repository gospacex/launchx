package app

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/route"

	srroute "github.com/gospacex/launchx/examples/monolith/internal/route"

	"github.com/gospacex/launchx/examples/monolith/internal/handler"
)

// RegisterRoutes 注册订单路由。
//
// 注册动作改由 `internal/route.Table()` 驱动 —— 表里同时给出路由模板、业务
// 动作名与读写语义，hertz 注册与链路属性读的是同一份数据，不会再出现
// "加了接口但路由层没登记"的半边状态（未登记会在首次请求时告警）。
//
// 根 span 由 webx 默认链的 otel 中间件开，业务中间件（鉴权 / 限流）在
// route span 之下各挂一节，handler / service / repository 再往下延伸。
//
// 鉴权只挂在 /secure 分组上：其余路由是 E1 / T13 的验收入口，加令牌会让既有
// 脚本全变 401 —— 那不是这些脚本要验证的东西。auth 为 nil（组件未装配）时不
// 注册该分组，而不是注册一个恒 401 的空壳。
//
// 但「免鉴权」只是**回环监听下**才成立的豁免，不是无条件的：
// 绑到 0.0.0.0 / 具体网卡地址时，免鉴权路由一律拒登并让装配失败（见下面
// loopbackOnly 的注释 —— 这条约束曾经只写在注释里，代码里根本没有）。
//
// listenAddr 必须是**实际监听地址**：由它决定路由表能不能成立，传错（写死
// 回环字面量）等于把这条约束架空，故调用方一律从配置读（见 setup.go）。
func RegisterRoutes(e *route.Engine, h *handler.OrderHandler, auth app.HandlerFunc, listenAddr string) error {
	// 对外监听时的两条硬裁决。放在注册之前：违规要让进程**起不来**并给出
	// 明确原因，而不是注册完再打一条 Warn —— 后者等于"裸奔但日志里说了"。
	if !loopbackOnly(listenAddr) {
		if auth == nil {
			return fmt.Errorf("monolith: 监听地址 %q 不是回环地址，而鉴权中间件未装配"+
				"（缓存不可达或 demo.auth_token 为空）—— 对外暴露无鉴权接口不可接受；"+
				"修法：绑回环（webx.addr: 127.0.0.1:PORT），或修好鉴权依赖", listenAddr)
		}
		var open []string
		for _, en := range srroute.Table() {
			if !en.Secure {
				open = append(open, en.Method+" "+en.Pattern)
			}
		}
		if len(open) > 0 {
			return fmt.Errorf("monolith: 监听地址 %q 不是回环地址，而路由表里有 %d 条"+
				"免鉴权路径（%v）—— 含 POST/PUT/DELETE /orders，任何人可下单/改单/删单；"+
				"修法：绑回环（webx.addr: 127.0.0.1:PORT），或把这些 Entry 改成 Secure: true",
				listenAddr, len(open), open)
		}
	}

	var secure *route.RouterGroup
	if auth != nil {
		secure = e.Group("/secure", auth)
	}

	for _, en := range srroute.Table() {
		fn := handlerOf(h, en.Op)
		if en.Secure {
			// secure 为 nil 说明鉴权中间件没装配（缓存降级 / 令牌为空），
			// /secure 分组根本不存在。此处必须显式跳过：往 nil 分组上注册会让
			// hertz 在 nil 接收者里取字段而 panic —— 实测路径是 redis 不可达 →
			// cache 降级 → auth 为 nil → 进程在启动装配期直接崩溃（退出码 2）。
			// 降级场景下崩溃是最坏的一种姿态：旁路边车挂了把主流程一起带走。
			if secure == nil {
				continue
			}
			// 表里的 Pattern 是**完整路径**（含 /secure 前缀），而分组本身已经带
			// 了这段前缀：直接照抄会注册出 /secure/secure/orders —— 鉴权路径
			// 实际挂在谁都访问不到的地址上，而 /secure/orders 一直是 404。
			// 故挂进分组前先摘掉前缀。
			//
			// 反过来不能把表里的 Pattern 改成 /orders：route 层按 `c.FullPath()`
			// 查表，FullPath 是注册后的真实模板（/secure/orders），改了就查不到，
			// 每笔鉴权请求都会被记成 route.order.Unregistered。
			pattern := strings.TrimPrefix(en.Pattern, "/secure")

			// 分组路由只支持单个方法注册 API，按表里的 GET/POST 分派。
			if en.Method == http.MethodGet {
				secure.GET(pattern, fn)
				continue
			}
			secure.Handle(en.Method, pattern, fn)
			continue
		}
		e.Handle(en.Method, en.Pattern, fn)
	}
	return nil
}

// loopbackOnly 判定监听地址是否只对本机可见。
//
// 这条判定此前只存在于 `repository.List` 与 `service.List` 的注释里（"那条路径
// 已被 RegisterRoutes 限制为只能绑回环地址"）—— 全仓 grep `loopbackOnly` 零命中，
// 即注释承诺了一个并不存在的安全约束：改一行 `webx.addr` 就能把无鉴权的写接口
// 暴露到网卡地址上，而读注释的人以为有闸门。现在它是真的判据。
//
// 收紧口径：解析不出 host 的地址一律按**非回环**处理（空 host / 缺端口 / 非法串）。
// 配错的人应该起不来并看到明确原因，而不是被猜一个宽松答案放行 —— 与
// `selfAddr` 拒空 host 是同一条口径。
func loopbackOnly(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	// 名字形式只认 localhost：其余主机名要 DNS 才能定性，猜不得。
	return host == "localhost"
}

// handlerOf 把路由表中的业务动作名映射到 handler 方法。
//
// 显式 switch 而不是反射：接口面只有九个动作，编译期就能发现"表里写了
// handler 没有的 Op"（default 分支直接给 —— 比反射按名字找不到再 panic 清楚）。
func handlerOf(h *handler.OrderHandler, op string) app.HandlerFunc {
	switch op {
	case "ListOrders":
		return h.List
	case "CreateOrder":
		return h.Create
	case "SearchOrders":
		return h.Search
	case "GetOrder":
		return h.Get
	case "GetOrderWithUser":
		return h.GetWithUser
	case "GetOrderSnapshot":
		return h.Snapshot
	case "UpdateOrder":
		return h.Update
	case "DeleteOrder":
		return h.Delete
	default:
		return func(_ context.Context, c *app.RequestContext) {
			c.String(http.StatusNotImplemented, "route: unbound op "+op)
		}
	}
}
