package configsrc

import (
	"fmt"
	"os"

	"github.com/gospacex/configx"
)

func init() { Register("local", localLoader) }

// localLoader 经 configx 读本地 yaml，并以 envConfig 包装完成 ${ENV} 展开。
// configx 本身不做 env 展开（micro_apps 既有语义），展开点唯一在本包装层。
func localLoader(o Options) (configx.Config, error) {
	// configx 对不存在的路径不报错：viper.go 吞掉 ConfigFileNotFoundError 后
	// 回退空配置并返回可用对象，错拼路径会静默降级为空配置。launchx 的原则是
	// 地基失败即退，故在此显式硬失败，不依赖 configx 的行为。
	if _, err := os.Stat(o.Path); err != nil {
		return nil, fmt.Errorf("config path %q: %w", o.Path, err)
	}
	c, err := configx.ReadConfig(configx.ConfigOptions{
		ConfigFile: o.Path,
		ConfigType: "yaml",
	})
	if err != nil {
		return nil, fmt.Errorf("read local config %q: %w", o.Path, err)
	}
	return &envConfig{Config: c}, nil
}
