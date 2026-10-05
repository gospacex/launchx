// Package app 业务装配层：把 launchx 装配好的中间件句柄（DB / 调用链 / HTTP 引擎）
// 接成 repository → service → handler 三层，并注册路由。
//
// 这一层不含任何形态分支（不出现 monolith / bff / gateway 等取值）：
// 换形态只改 config.yaml 的 service.mode，与本装配无关。
package app

import (
	"errors"

	"github.com/gospacex/webx"
	"gorm.io/gorm"

	"github.com/gospacex/launchx"
	"github.com/gospacex/launchx/examples/monolith/internal/handler"
	"github.com/gospacex/launchx/examples/monolith/internal/repository"
	"github.com/gospacex/launchx/examples/monolith/internal/service"
)

// tracerName 业务侧 instrumentation 名（区别于 dbx / webx 各自的 tracer 名）。
const tracerName = "order"

// Setup 是注册给 App.OnSetup 的装配回调：
// 取 DB 句柄 → 构造三层 → 把路由挂到 webx 引擎上。
//
// 失败即返回 error：装配期报错好过带着半装配的状态把服务起出去。
func Setup(a *launchx.App) error {
	// DB 句柄只能经 App.DB() 取（业务不自建连接、不直引驱动）。
	db, ok := a.DB().(*gorm.DB)
	if !ok || db == nil {
		return errors.New("monolith: db component is unavailable, check xmiddleware.sqlx in config.yaml")
	}
	// webx 引擎由 monolith 档装配产出，路由须在 Run 之前挂完。
	engine, ok := a.Engine().(*webx.App)
	if !ok || engine == nil {
		return errors.New("monolith: webx engine is unavailable in this mode")
	}

	tr := a.Tracer().TracerProvider().Tracer(tracerName)
	repo := repository.New(db, tr)
	svc := service.New(repo, tr)
	RegisterRoutes(engine.Engine, handler.New(svc, tr))
	return nil
}
