package cacheutil

import (
	"os"
	"strings"
	"testing"
)

// readSite 一处读缓存的落点。
//
// 这些路径是**枚举**而不是 glob：逐行核对矩阵时把公共目录并进一行会让
// 判据失去分辨力（少一处也照样绿），故逐个列出。
type readSite struct {
	path string
	// call 该处实际调用的形态。分两种：多数域只取值（Fetch），
	// mesh 两域还要命中状态（FetchResult —— 它们的 View 带 Cached 出参）。
	//
	// 带上 `(ctx,` 是为了让 Fetch 与 FetchResult 互不误命中
	// （后者包含前者的前缀）；不带 `:=` 是因为具名返回值的调用点用的是 `=`
	// （monolith 的 `out, err = cacheutil.Fetch(ctx, ...)`）。
	call string
}

var readSites = []readSite{
	// micro / mall 各四域
	{path: "../../micro/srv-order/internal/service/order.go", call: "cacheutil.Fetch(ctx,"},
	{path: "../../micro/srv-product/internal/service/product.go", call: "cacheutil.Fetch(ctx,"},
	{path: "../../micro/srv-points/internal/service/points.go", call: "cacheutil.Fetch(ctx,"},
	{path: "../../micro/srv-stock/internal/service/stock.go", call: "cacheutil.Fetch(ctx,"},
	{path: "../../mall/srv-order/internal/service/order.go", call: "cacheutil.Fetch(ctx,"},
	{path: "../../mall/srv-product/internal/service/product.go", call: "cacheutil.Fetch(ctx,"},
	{path: "../../mall/srv-points/internal/service/points.go", call: "cacheutil.Fetch(ctx,"},
	{path: "../../mall/srv-stock/internal/service/stock.go", call: "cacheutil.Fetch(ctx,"},
	// monolith：走自己的 cache.Order 适配层（外面还包一层业务 span）
	{path: "../../monolith/internal/service/order.go", call: "cacheutil.Fetch(ctx,"},
	// mesh apps（独立 module，与本包同一仓库）
	{path: "../../mesh/apps/internal/srv/order/service/order.go", call: "cacheutil.FetchResult(ctx,"},
	{path: "../../mesh/apps/internal/srv/product/service/product.go", call: "cacheutil.FetchResult(ctx,"},
}

// hasCall 逐行找调用点，跳过注释行。
//
// 为什么逐行而不是 `strings.Contains` 全文：注释里写着"读缓存走 cacheutil.Fetch
// （含空值哨兵）"，全文匹配会让**注释**满足判据 —— 判据必须只在真接线时才满足。
func hasCall(src, call string) bool {
	for _, raw := range strings.Split(src, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "//") {
			continue
		}
		if strings.Contains(line, call) {
			return true
		}
	}
	return false
}

// TestAllReadPathsUseFetch 断言**每一处**读缓存都走本包，而不是各自手搓。
//
// 为什么必须有这条：手搓的版本**不会报错**，只会静默少一层保护 —— 缓存穿透
// 就是十几份复制各写各的、谁也改不全留下的。接入了十处的第十一处能正常编译、
// 正常跑、正常通过所有业务测试，唯独不挡穿透。
//
// 匹配串取 `:= cacheutil.Fetch(` 而不是 `cacheutil.Fetch`：后者会命中
// 注释里"读缓存走 cacheutil.Fetch（含空值哨兵）"这句 —— 源码级判据必须
// 匹配"只有真接线才有的那一段"。
//
// 反向探针（已实测）：把任一域改回 `s.cache.Get` 直读 → 本用例红。
func TestAllReadPathsUseFetch(t *testing.T) {
	for _, site := range readSites {
		b, err := os.ReadFile(site.path)
		if err != nil {
			t.Fatalf("读 %s 失败（路径挪了？判据必须跟着改）：%v", site.path, err)
		}
		src := string(b)
		if !hasCall(src, site.call) {
			t.Errorf("%s 没有走 %s —— 这一处的缓存穿透没人挡", site.path, site.call)
		}
		if !strings.Contains(src, "cacheOpt() cacheutil.Options") {
			t.Errorf("%s 没有统一的 cacheOpt —— 负缓存策略又散回了各域", site.path)
		}
		if !strings.Contains(src, "examples/pkg/cacheutil") {
			t.Errorf("%s 没有 import 本包", site.path)
		}
	}
}

// TestMeshModelsDeclareValidatable 「反序列化成功 ≠ 值完整」这条判据的落点。
//
// mesh 两域此前在各自的 decode* 里用 `ID == 0` 挡半截对象，收口后搬进了
// model 的 Valid()。枚举在这里是为了防止有人后来"清理"掉这两个方法 ——
// 删掉它们不会让任何编译或业务测试失败，只会让半个订单被当成命中返回。
func TestMeshModelsDeclareValidatable(t *testing.T) {
	for _, site := range []struct{ path, decl string }{
		{"../../mesh/apps/internal/srv/order/model/order.go", "func (o Order) Valid() bool"},
		{"../../mesh/apps/internal/srv/product/model/product.go", "func (p Product) Valid() bool"},
	} {
		b, err := os.ReadFile(site.path)
		if err != nil {
			t.Fatalf("读 %s 失败：%v", site.path, err)
		}
		if !strings.Contains(string(b), site.decl) {
			t.Errorf("%s 缺少 %s —— 残缺缓存值会被当成命中", site.path, site.decl)
		}
	}
}

// TestMonolithStoreHandlesNilTypedPointer typed nil 陷阱的守卫。
//
// `*cache.Order` 的 nil 指针转成接口后 != nil，直接交给本包会让"没配缓存"
// 这条降级路径上出现 nil 接收者调用。判据钉住 service 侧那个显式判空还在。
func TestMonolithStoreHandlesNilTypedPointer(t *testing.T) {
	b, err := os.ReadFile("../../monolith/internal/service/order.go")
	if err != nil {
		t.Fatalf("读失败：%v", err)
	}
	src := string(b)
	if !strings.Contains(src, "func (s *Service) store() cacheutil.Store") {
		t.Fatal("monolith 缺少 store() 适配方法")
	}
	if !strings.Contains(src, "if s.cache == nil {\n\t\treturn nil") {
		t.Fatal("store() 里没有显式判空 —— typed nil 转成接口后 != nil，会让本包以为缓存已装配")
	}
}

// TestCreatePathsInvalidate 断言会新建记录的写路径做了缓存失效。
//
// 这是负缓存的**配套**：不失效的话，"查过一次不存在的 ID → 该 ID 被创建"
// 会让新记录在最长 MissTTL 内查不到 —— 负缓存把一次读的副作用延长到了写之后。
// 本包替不了这一步（各域的写方法名不同），只能钉住它没被漏掉。
func TestCreatePathsInvalidate(t *testing.T) {
	// 只有 product / order 两域需要在 Create 里失效：
	// stock 的首次建库存在 Reserve 内、points 的开户在 apply 内，两者都已失效。
	for _, path := range []string{
		"../../micro/srv-product/internal/service/product.go",
		"../../mall/srv-product/internal/service/product.go",
		"../../micro/srv-order/internal/service/order.go",
		"../../mall/srv-order/internal/service/order.go",
	} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读 %s 失败：%v", path, err)
		}
		if !strings.Contains(string(b), "s.invalidate(ctx,") {
			t.Errorf("%s 的写路径没有失效缓存 —— 新建的记录会被此前的空值哨兵挡住", path)
		}
	}
}
