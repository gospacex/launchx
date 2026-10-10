// Package app 装配层：把 launchx 交出的句柄接成
//
//	rpc → route(auth>ratelimit) → handler → service → repository → model
//
// 这条链，并把 kitex 服务端回存给 launchx 启动与停止。
//
// 与 examples/micro/srv-order/app/setup.go 的装配差异只有三处：
//  1. 不注册/不发现（没有 xmiddleware.coordx 段，launchx 也不会去注册）；
//  2. 句柄自检用 pkg/handles（micro 用的是自己的 pkg/handlecheck）；
//  3. 没有 outbox Runner（本例不带 outbox 表，投递失败只告警）。
package app

import (
	"errors"
	"strconv"

	"github.com/cloudwego/kitex/server"
	"github.com/gospacex/logx"
	"gorm.io/gorm"

	"github.com/gospacex/launchx"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/handler"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/middleware"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/repository"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/route"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/rpc"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/service"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/handles"
	orderrpc "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/order/order"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/rpcwire"
)

// Setup 注册给 App.OnSetup。
//
// 顺序即纪律：先取句柄 → 再自检 → 后装配。反过来（先装配再自检）判据就失效了：
// 句柄被业务包过一层之后，自检看到的已不是 launchx 交出的那个实例。
func Setup(a *launchx.App) error {
	// ---- 1. 取句柄 ----
	db, ok := a.DB().(*gorm.DB)
	if !ok || db == nil {
		return errors.New("srv-order: db handle unavailable (check xmiddleware.sqlx + strict.db)")
	}
	opts, ok := a.RPCServerOptions().([]server.Option)
	if !ok {
		return errors.New("srv-order: rpc server options unavailable (check service.mode=srv)")
	}
	// 业务码要过进程边界：kitex 默认只装 metainfo handler，业务码会被降级成
	// 一段字符串（客户端取不回码）。漏掉的症状是「服务照常起、链路照常通，
	// bff 却把每个业务拒绝都当 502」—— 不是可选优化项，见 rpcwire 包注释。
	opts = append(opts, rpcwire.ServerOptions()...)
	cfg := a.Config()
	// 实例名必须等于 service.name（spec mesh-app-handles）：它同时是
	// cache / kv 实例的键，写成字面量会在改名后悄悄取到 nil 句柄。
	name := cfg.GetString("service.name")
	logger := a.Logger()
	// traceOn 是分层埋点的总闸：一次取出，分别交给 repository / service / handler。
	traceOn := cfg.GetBool("trace.enabled")

	// ---- 2. 句柄自检：来源合规 + 入链有效 ----
	// db 是硬前提（strict.db: true 已在配置里声明），缺了必须装配期即退。
	if err := handles.VerifyOrError(a, "db"); err != nil {
		return errors.New("srv-order: " + err.Error())
	}
	for _, c := range handles.Verify(a) {
		if !c.Present {
			continue
		}
		logger.Info("handle verified",
			logx.String("component", c.Component), logx.String("backend", c.Backend),
			logx.String("joins_trace", strconv.FormatBool(c.Observed)), logx.String("probe", c.Probe))
	}

	// ---- 3. 分层装配：自下而上 ----
	repo := repository.New(db, traceOn)
	svc := service.New(repo, a.Cache()[name], a.Lock(), a.MQ(), logger, traceOn)
	guard := route.New(middleware.New(a.KV()[name], logger), logger)

	// ---- 4. 建服务端并回存 ----
	// 身份闸门已移进 rpc/ 层（连同 401/429 两个拒绝码）：rpc/ 实现 order.Order
	// 全部方法，并在委派 handler 前走 route.Guard。这里只把适配层交给生成的
	// NewServer —— kitex 按接口签名约束入参，少一个方法编译期即失败。
	srv := orderrpc.NewServer(rpc.New(handler.New(svc, traceOn), guard, traceOn), opts...)
	a.SetRPCServer(srv)

	logger.Info("layer chain assembled",
		logx.String("service", name),
		logx.String("chain", "rpc>route(auth>ratelimit)>handler>service>repository>model"))
	return nil
}
