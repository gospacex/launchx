package launchx

import "fmt"

// Bootstrap 是唯一引导入口：加载配置并构造 App。骨架步骤由 pipeline 执行，
// 失败即退（地基错误不降级）。
func Bootstrap(path string) (*App, error) {
	if path == "" {
		return nil, fmt.Errorf("launchx: config path is empty")
	}
	a := &App{configPath: path}
	return a, nil
}
