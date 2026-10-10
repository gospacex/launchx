// Package app 装配层：把 launchx 交出的句柄接成
//
//	rpc → route(auth>ratelimit) → handler → service → repository → model
//
// 这条链，并把 kitex 服务端回存给 launchx 启动与停止。
//
// 与 order 域装配的差异只有两处：
//  1. 句柄面更窄：service 只用 db + kv（扣减旁路计数）+ lock（同商品扣减互斥），
//     没有 cache / mq，也没有消费循环；route 限流另取一份同源 KV；
//  2. **不**配置 strict.lockx：锁缺失必须是**降级**（acquire 打 Warn 后照常工作）
//     而不是装配期即退，故这里不把 lockx 声明成硬前提（配置侧的 strict 声明
//     归 conf，不在本层）。
package app

import (
	"errors"
	"strconv"

	"github.com/cloudwego/kitex/server"
	"github.com/gospacex/logx"
	"gorm.io/gorm"

	"github.com/gospacex/launchx"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/stock/handler"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/stock/middleware"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/stock/repository"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/stock/route"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/stock/rpc"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/stock/service"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/handles"
	stockrpc "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/stock/stock"
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
		return errors.New("srv-stock: db handle unavailable (check xmiddleware.sqlx + strict.db)")
	}
	opts, ok := a.RPCServerOptions().([]server.Option)
	if !ok {
		return errors.New("srv-stock: rpc server options unavailable (check service.mode=srv)")
	}
	// 业务码要过进程边界：kitex 默认只装 metainfo handler，业务码会被降级成
	// 一段字符串（客户端取不回码）。漏掉的症状是「服务照常起、链路照常通，
	// bff 却把每个业务拒绝都当 502」—— 不是可选优化项，见 rpcwire 包注释。
	opts = append(opts, rpcwire.ServerOptions()...)
	cfg := a.Config()
	// 实例名必须等于 service.name（spec mesh-app-handles）：它同时是 kv 实例的键，
	// 写成字面量会在改名后悄悄取到 nil 句柄。
	name := cfg.GetString("service.name")
	logger := a.Logger()
	// traceOn 是分层埋点的总闸：一次取出，分别交给 repository / service / handler / rpc。
	traceOn := cfg.GetBool("trace.enabled")

	// ---- 2. 句柄自检：来源合规 + 入链有效 ----
	// db 是硬前提（strict.db: true 已在配置里声明），缺了必须装配期即退。
	if err := handles.VerifyOrError(a, "db"); err != nil {
		return errors.New("srv-stock: " + err.Error())
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
	// lock 缺失（nil）→ 降级：service.acquire 打「未走锁路径」Warn 后照常扣减
	// （条件 UPDATE 仍是不超卖的护栏）。这里**不**拦、**不**即退 —— 把 nil 当致命
	// 错误会让一次 lockx 配错演变成整域不可用。这条降级路径的执行期内核由
	// service 层单测覆盖。
	svc := service.New(repo, a.KV()[name], a.Lock(), logger, traceOn)
	guard := route.New(middleware.New(a.KV()[name], logger), logger)

	// ---- 4. 建服务端并回存 ----
	// rpc/ 实现 stock.Stock 全部 7 个方法，并在委派 handler 前走 route.Guard
	// （身份闸门 + 按主体限流；被拒回 codes.RateLimited，bff 映射 429）。
	// kitex 按接口签名约束入参，少一个方法编译期即失败。
	srv := stockrpc.NewServer(rpc.New(handler.New(svc, traceOn), guard, traceOn), opts...)
	a.SetRPCServer(srv)

	logger.Info("layer chain assembled",
		logx.String("service", name),
		logx.String("chain", "rpc>route(auth>ratelimit)>handler>service>repository>model"))
	return nil
}
