package handler

import (
	"os"
	"strings"
	"testing"
)

// TestCreateOrderRequestCarriesIdempotencyKey 钉住「bff 把幂等键传给了订单域」。
//
// 为什么用源码判据：bff 是**唯一**知道这个号的地方，它传没传，在运行期没有任何
// 可观测的副作用 —— 不传也不会报错，订单域只是自己另起一个号。这正是当前最危险
// 的失效形态：两端代码看着都对，重试一次就是一张新单。
//
// 反向探针：把 mall.go 里 CreateOrderReq 的 `OrderNo: orderNo` 一行注释掉，
// 本用例立刻红（实测过）。
func TestCreateOrderRequestCarriesIdempotencyKey(t *testing.T) {
	src, err := os.ReadFile("mall.go")
	if err != nil {
		t.Fatalf("读 mall.go: %v", err)
	}
	text := string(src)
	idx := strings.Index(text, "orderpb.CreateOrderReq{")
	if idx < 0 {
		t.Fatal("mall.go 里找不到 CreateOrderReq 的构造点（下单调用改结构了吗）")
	}
	end := idx + 1200
	if end > len(text) {
		end = len(text)
	}
	// 逐行比对而不是 `strings.Contains(片段, "OrderNo:")` —— 后者会命中**注释里**
	// 的同名串：实测把字段整行注释掉（`// OrderNo: orderNo,`）后判据照样绿，
	// 等于空壳。字段名只有在**行首**（去掉缩进后）才是真的字段。
	found := false
	for _, line := range strings.Split(text[idx:end], "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "OrderNo:") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("CreateOrderReq 没带 OrderNo —— 幂等键没传给订单域，" +
			"重试（rpcx 的失败重试 / 上游重发）会重复下单")
	}
}
