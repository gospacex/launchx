package configsrc

import (
	"fmt"

	"github.com/gospacex/configx"
)

func init() { Register("local", localLoader) }

// localLoader 经 configx 读本地 yaml，并以 envConfig 包装完成 ${ENV} 展开。
// configx 本身不做 env 展开（micro_apps 既有语义），展开点唯一在本包装层。
func localLoader(o Options) (configx.Config, error) {
	c, err := configx.ReadConfig(configx.ConfigOptions{
		ConfigFile: o.Path,
		ConfigType: "yaml",
	})
	if err != nil {
		return nil, fmt.Errorf("read local config %q: %w", o.Path, err)
	}
	return &envConfig{Config: c}, nil
}
