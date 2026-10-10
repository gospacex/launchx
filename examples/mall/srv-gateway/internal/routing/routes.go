// Package routing 网关路由表：从配置加载前缀路由，并按最长前缀匹配入站路径。
//
// 本包不感知形态（不出现任何形态取值），也不感知上游地址 —— 它只回答
// 「这个路径该转给哪个服务名」。
package routing

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/gospacex/configx"
)

// routesKey 路由表在配置里的键（yaml 段 gateway.routes）。
const routesKey = "gateway.routes"

// Route 一条前缀路由。
type Route struct {
	// Name 标识：用于 span 名后缀与日志，缺省取 Prefix。
	Name string
	// Prefix 入站路径前缀，须以 '/' 开头。
	Prefix string
	// Upstream 被调服务名：既是注册中心里的服务名，也是
	// service.upstream.<name> 直连兜底键的后缀。
	Upstream string
	// Strip 转发前是否剥掉 Prefix。
	Strip bool
}

// Table 路由表。Match 按最长前缀匹配（表内已按前缀长度降序排好）。
type Table struct {
	routes []Route
}

// Routes 返回表内全部路由（顺序为最长前缀优先）。
func (t Table) Routes() []Route { return t.routes }

// LoadTable 从配置加载路由表。
//
// 表缺失、为空或任一条目的 prefix/upstream 不合法都返回 error：网关带着
// 半张路由表起来，表现为「个别路径静默 404」，比启动失败更难排查。
func LoadTable(cfg configx.Config) (Table, error) {
	if cfg == nil {
		return Table{}, errors.New("routing: config handle is nil")
	}
	if !cfg.IsSet(routesKey) {
		return Table{}, fmt.Errorf("routing: config key %s not found", routesKey)
	}
	items, err := routeItems(cfg.Get(routesKey))
	if err != nil {
		return Table{}, err
	}
	if len(items) == 0 {
		return Table{}, fmt.Errorf("routing: config key %s is empty", routesKey)
	}

	routes := make([]Route, 0, len(items))
	for i, item := range items {
		r := Route{
			Prefix:   strings.TrimSpace(stringField(item, "prefix")),
			Upstream: strings.TrimSpace(stringField(item, "upstream")),
			Name:     strings.TrimSpace(stringField(item, "name")),
			Strip:    boolField(item, "strip"),
		}
		if r.Name == "" {
			r.Name = r.Prefix
		}
		if !strings.HasPrefix(r.Prefix, "/") {
			return Table{}, fmt.Errorf("routing: %s[%d].prefix must start with '/', got %q", routesKey, i, r.Prefix)
		}
		if r.Upstream == "" {
			return Table{}, fmt.Errorf("routing: %s[%d].upstream is empty", routesKey, i)
		}
		routes = append(routes, r)
	}
	// 降序：Match 取首个命中即最长前缀，避免 /api 抢在 /api/v2 前面。
	sort.Slice(routes, func(i, j int) bool { return len(routes[i].Prefix) > len(routes[j].Prefix) })
	return Table{routes: routes}, nil
}

// Match 按最长前缀匹配路径，返回命中的路由。
//
// 只有整段前缀边界才算命中：前缀 /api 匹配 /api 与 /api/x，不匹配 /apix。
func (t Table) Match(path string) (Route, bool) {
	for _, r := range t.routes {
		if path == r.Prefix || strings.HasPrefix(path, r.Prefix+"/") {
			return r, true
		}
	}
	return Route{}, false
}

// routeItems 把配置里的路由表取成条目列表（viper 对 yaml 列表给出的形态不定）。
func routeItems(raw any) ([]any, error) {
	switch v := raw.(type) {
	case []any:
		return v, nil
	case []map[string]any:
		items := make([]any, 0, len(v))
		for _, m := range v {
			items = append(items, m)
		}
		return items, nil
	default:
		return nil, fmt.Errorf("routing: config key %s must be a list of objects", routesKey)
	}
}

// stringField 从条目里取字符串字段；键不存在或类型不符即空串。
func stringField(item any, key string) string {
	m, ok := asMap(item)
	if !ok {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// boolField 从条目里取布尔字段；兼容 yaml 的 true/false 与字符串写法。
func boolField(item any, key string) bool {
	m, ok := asMap(item)
	if !ok {
		return false
	}
	switch v := m[key].(type) {
	case bool:
		return v
	case string:
		b, _ := strconv.ParseBool(strings.TrimSpace(v))
		return b
	default:
		return false
	}
}

// asMap 归一化为 map[string]any（yaml 也可能给出 map[any]any）。
func asMap(item any) (map[string]any, bool) {
	switch v := item.(type) {
	case map[string]any:
		return v, true
	case map[any]any:
		out := make(map[string]any, len(v))
		for k, val := range v {
			if ks, ok := k.(string); ok {
				out[ks] = val
			}
		}
		return out, true
	default:
		return nil, false
	}
}
