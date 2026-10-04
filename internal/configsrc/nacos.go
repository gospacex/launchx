package configsrc

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/gospacex/configx"
)

func init() { Register("nacos", nacosLoader) }

func nacosLoader(o Options) (configx.Config, error) {
	local, err := localLoader(o)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(o.DataID) == "" {
		return local, nil
	}

	var addr string
	if o.AddrEnv != "" {
		addr = os.Getenv(o.AddrEnv)
	}
	if strings.TrimSpace(addr) == "" {
		addr = local.GetString("configcenter.nacos.server_addr")
	}
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return local, nil
	}

	remote, err := configx.ReadConfig(configx.ConfigOptions{
		ConfigFile: o.Path,
		ConfigType: "yaml",
		Remote: configx.RemoteConfig{
			Enabled:  true,
			Provider: "nacos",
			Nacos: configx.NacosConfig{
				DataID:        o.DataID,
				Group:         o.Group,
				ServerConfigs: parseServerAddrs(addr),
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("nacos dataId=%q addr=%q: %w", o.DataID, addr, err)
	}
	return &envConfig{Config: remote}, nil
}

// parseServerAddrs 解析逗号分隔的 host:port，忽略空项与非法项。
func parseServerAddrs(addr string) []configx.NacosServerConfig {
	var servers []configx.NacosServerConfig
	for _, item := range strings.Split(addr, ",") {
		host, portText, err := net.SplitHostPort(strings.TrimSpace(item))
		if err != nil || host == "" {
			continue
		}
		port, err := strconv.ParseUint(portText, 10, 16)
		if err != nil || port == 0 {
			continue
		}
		servers = append(servers, configx.NacosServerConfig{IP: host, Port: port})
	}
	return servers
}
