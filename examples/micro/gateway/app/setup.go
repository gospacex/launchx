// Package app 业务装配层：把 launchx 装配好的句柄（webx 引擎、注册中心句柄）
// 接成路由表 + 转发器，并把通配路由挂到引擎上。
//
// 这一层不含任何形态分支（不出现 monolith / bff / gateway 等取值）：
// 换形态只改 config.yaml 的 service.mode，与本装配无关。
package app

import (
	"errors"
	"fmt"

	"github.com/gospacex/webx"

	"github.com/gospacex/launchx"
	"github.com/gospacex/launchx/examples/micro/gateway/internal/proxy"
	"github.com/gospacex/launchx/examples/micro/gateway/internal/routing"
)

// tracerName 业务侧 instrumentation 名（区别于 webx 自己的 tracer 名）。
const tracerName = "gateway"

// upstreamKeyPrefix 直连兜底配置键前缀：service.upstream.<被调服务名>。
// 与 launchx 内部读上游兜底目标的键完全一致（internal/mode/webx.go）。
const upstreamKeyPrefix = "service.upstream."

// Setup 是注册给 App.OnSetup 的装配回调：
// 取 webx 引擎 + 注册中心句柄 → 加载路由表 → 构造转发器 → 挂通配路由。
//
// 失败即返回 error：装配期报错好过带着半装配的状态把服务起出去。
//
// 本函数不注册/注销注册中心 —— gateway 档自身不注册（只有 bff/srv 注册，
// 注册由 launchx 按 mode 做），demo 侧只做发现与转发。
func Setup(a *launchx.App) error {
	// webx 引擎由 gateway 档装配产出，路由须在 Run 之前挂完。
	engine, ok := a.Engine().(*webx.App)
	if !ok || engine == nil {
		return errors.New("gateway: webx engine is unavailable in this mode")
	}

	cfg := a.Config()
	table, err := routing.LoadTable(cfg)
	if err != nil {
		return fmt.Errorf("gateway: load routes: %w", err)
	}

	// 发现句柄只能经 App.Registry() 取（demo 不自建注册中心客户端）。
	// 句柄若不含 Discovery（coordx 的 consul Register 实现当前只有
	// Register/Deregister），断言失败即走配置直连兜底。
	var finder proxy.Finder
	if reg := a.Registry(); reg != nil {
		finder, _ = reg.(proxy.Finder)
	}
	resolver := proxy.NewResolver(finder, func(upstream string) string {
		return cfg.GetString(upstreamKeyPrefix + upstream)
	})

	p := proxy.New(table, resolver, a.Tracer().TracerProvider().Tracer(tracerName))
	for _, r := range table.Routes() {
		// hertz 的通配参数写作 *p；一条路由注册一个通配，命中后由
		// proxy.Handle 按路径再匹配一次（注册与匹配共用同一张表）。
		engine.Engine.Any(r.Prefix+"/*p", p.Handle)
	}
	return nil
}
