package launchx

import (
	"fmt"

	"github.com/gospacex/launchx/internal/pipeline"
)

// Bootstrap 是唯一引导入口：读配置 → telemetry → logx → mode 装配 → 组件挂载。
//
// 骨架顺序固定，任一步失败即退：已初始化的资源按反序退出链回收后再返回错误
// （地基失败不降级 —— 配错路径/服务名/形态都不允许"带到运行时再炸"）。
func Bootstrap(path string) (*App, error) {
	if path == "" {
		return nil, fmt.Errorf("launchx: config path is empty")
	}
	a := &App{configPath: path, events: pipeline.NewEvents()}
	if err := pipeline.Run(a.env(), a.steps()); err != nil {
		_ = a.Close()
		return nil, err
	}
	return a, nil
}
