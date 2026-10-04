// Package configsrc 抽象配置来源：默认 local（本地 yaml），任务 5 起可注册
// nacos 等远端配置中心。业务代码只面向 Resolve，切换来源不改调用方。
package configsrc

import (
	"fmt"
	"sort"

	"github.com/gospacex/configx"
)

// Options 配置源输入
type Options struct {
	Path string // 本地 yaml 路径
	// ConfigCenter 段：Type 空则走 local
	Type    string
	DataID  string
	Group   string
	AddrEnv string // 覆盖远端地址的 env 名，如 HUBX_NACOS_SERVER
}

// Loader 配置源加载器
type Loader func(o Options) (configx.Config, error)

var loaders = map[string]Loader{}

// Register 注册配置源加载器（同名覆盖）。
func Register(name string, l Loader) { loaders[name] = l }

// Names 返回已注册加载器名（用于错误信息）。
func Names() []string {
	out := make([]string, 0, len(loaders))
	for k := range loaders {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Resolve 按名选择加载器；name 为空时使用 local。
func Resolve(name string, o Options) (configx.Config, error) {
	if name == "" {
		name = "local"
	}
	l, ok := loaders[name]
	if !ok {
		return nil, fmt.Errorf("configsrc: unknown loader %q, available: %v", name, Names())
	}
	c, err := l(o)
	if err != nil {
		return nil, fmt.Errorf("configsrc: load %q: %w", name, err)
	}
	return c, nil
}
