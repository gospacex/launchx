package cacheutil

import (
	"os"
	"strings"
	"testing"
)

// 八处读缓存的落点（micro / mall 各四域）。
//
// 这些路径是**枚举**而不是 glob：逐行核对矩阵时把公共目录并进一行会让
// 判据失去分辨力（少一处也照样绿），故逐个列出。
var readPaths = []string{
	"../../micro/srv-order/internal/service/order.go",
	"../../micro/srv-product/internal/service/product.go",
	"../../micro/srv-points/internal/service/points.go",
	"../../micro/srv-stock/internal/service/stock.go",
	"../../mall/srv-order/internal/service/order.go",
	"../../mall/srv-product/internal/service/product.go",
	"../../mall/srv-points/internal/service/points.go",
	"../../mall/srv-stock/internal/service/stock.go",
}

// TestEightReadPathsUseFetch 断言八处读缓存全部走本包，而不是各自手搓。
//
// 为什么必须有这条：手搓的版本**不会报错**，只会静默少一层保护 —— 缓存穿透
// 就是八份复制各写各的、谁也改不全留下的。接入了七处的第八处能正常编译、
// 正常跑、正常通过所有业务测试，唯独不挡穿透。
//
// 匹配串取 `:= cacheutil.Fetch(` 而不是 `cacheutil.Fetch`：后者会命中
// 注释里"读缓存走 cacheutil.Fetch（含空值哨兵）"这句 —— 源码级判据必须
// 匹配"只有真接线才有的那一段"。
//
// 反向探针（已实测）：把任一域改回 `s.cache.Get` 直读 → 本用例红。
func TestEightReadPathsUseFetch(t *testing.T) {
	for _, path := range readPaths {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读 %s 失败（路径挪了？判据必须跟着改）：%v", path, err)
		}
		src := string(b)
		if !strings.Contains(src, ":= cacheutil.Fetch(") {
			t.Errorf("%s 没有走 cacheutil.Fetch —— 这一处的缓存穿透没人挡", path)
		}
		if !strings.Contains(src, "cacheOpt() cacheutil.Options") {
			t.Errorf("%s 没有统一的 cacheOpt —— 负缓存策略又散回了各域", path)
		}
		if !strings.Contains(src, "examples/pkg/cacheutil") {
			t.Errorf("%s 没有 import 本包", path)
		}
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
