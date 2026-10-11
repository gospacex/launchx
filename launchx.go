package launchx

import (
	"errors"
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
		// 地基失败照样要收干净已初始化的资源；但 Close 的返回值**不能丢**：
		// tracing 冲刷失败、注销失败这类问题只在产物 destructive 时才有感知，
		// 丢掉它等于「回收了，但不知道回收得干不干净」。回收这件事与其结果一并返回。
		if exitErr := a.Close(); exitErr != nil {
			return nil, errors.Join(fmt.Errorf("launchx: bootstrap: %w", err), exitErr)
		}
		return nil, err
	}
	return a, nil
}
