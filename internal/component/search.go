package component

import (
	"fmt"
	"strings"

	"github.com/gospacex/searchx"
	_ "github.com/gospacex/searchx/elasticsearch" // 注册 elasticsearch 驱动（blank import）
)

func init() { Register("search", buildSearch) }

// buildSearch 构造搜索组件（yaml 段：xmiddleware.searchx）。
//
// 段内 address 支持字符串或数组：configx 的 GetSlice 对单值也能返回一项，
// 但对 "a,b" 形态不拆分，故这里再做一次拆分兜底（历史配置两种写法都出现过）。
func buildSearch(env *Env) error {
	key := pickSection(env.Cfg, "xmiddleware.searchx", "searchx")
	if key == "" {
		return nil
	}
	driver := env.Cfg.GetString(key + ".driver")
	if driver == "" {
		driver = "elasticsearch"
	}
	addrs := env.Cfg.GetSlice(key + ".address")
	if len(addrs) == 0 {
		addrs = env.Cfg.GetSlice(key + ".addresses")
	}
	if len(addrs) == 0 {
		addrs = splitAddrs(env.Cfg.GetString(key + ".addr"))
	}
	if len(addrs) == 0 {
		addrs = []string{"http://127.0.0.1:9200"}
	}
	name := env.Cfg.GetString(key + ".name")
	if name == "" {
		name = env.ServiceName
	}
	cfg := &searchx.Config{
		Driver:      driver,
		Name:        name,
		Address:     addrs,
		Username:    env.Cfg.GetString(key + ".username"),
		Password:    env.Cfg.GetString(key + ".password"),
		EnableTrace: env.Cfg.GetBool(key+".enable_trace") && traceOn(env, "search"),
		// 旧集群（ES < 7.14）不返回 X-Elastic-Product，v8 客户端会拒绝全部响应。
		// 这里只做透传：是否宽容由部署方在配置里显式声明，库侧不擅自放宽校验。
		LegacyCluster: env.Cfg.GetBool(key + ".legacy_cluster"),
	}
	client, err := searchx.New(cfg)
	if err != nil {
		return degrade(env, "search", ConsequenceSearch, err)
	}
	env.Search = client
	env.AddStop("search", func() error {
		client.Close()
		return nil
	})
	return nil
}

// splitAddrs 把 "a,b" 形态拆成地址列表（去空白、丢空项）。
func splitAddrs(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

var _ = fmt.Sprintf // 保留 fmt（错误文案扩展时使用）
