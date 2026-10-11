package handler

import (
	"os"
	"strings"
	"testing"
)

// TestCreateOrderRequestCarriesIdempotencyKey bff 必须真的把幂等键传给订单域。
//
// 为什么是源码级判据而不跑一次真调用：端到端链路需要四域进程 + kafka + 集群，
// CI 走不到；而"有没有传这个字段"是**源码事实**，与运行时无关。
//
// ⚠️ 逐行匹配而不是 `strings.Contains(片段, "OrderNo:")` —— 后者会命中**注释里**
// 的同名串。实测把字段整行注释掉（`// OrderNo: orderNo,`）后，Contains 版判据
// 照样绿，等于空壳。字段名只有在**行首**（去掉缩进后）才是真字段。
//
// 反向探针：把 mall.go 里 `OrderNo: orderNo,` 整行注释掉，本用例立刻红。
func TestCreateOrderRequestCarriesIdempotencyKey(t *testing.T) {
	src, err := os.ReadFile("mall.go")
	if err != nil {
		t.Fatalf("读 mall.go: %v", err)
	}
	text := string(src)
	idx := strings.Index(text, "orderpb.CreateOrderReq{")
	if idx < 0 {
		t.Fatal("mall.go 里找不到 CreateOrderReq 的构造点 —— 下单链路改结构了吗？")
	}
	end := idx + 1200
	if end > len(text) {
		end = len(text)
	}
	found := false
	for _, line := range strings.Split(text[idx:end], "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "OrderNo:") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("CreateOrderReq 没带 OrderNo —— 幂等键没传给订单域，" +
			"重试（rpcx 的失败重试 / 上游重发）会变成重复下单")
	}
}

// TestCreateOrderGeneratesKeyAtEntry 幂等键必须在入口生成，而不是等订单域现编。
//
// 入口才知道"这是一次重试还是一笔新单"；放到下游生成，每次重放的号都不一样，
// 幂等从根上失效 —— 而链路与日志看起来全正常。
//
// 反向探针：删掉 createOrder 里 `orderNo := newOrderNo()`，本用例立刻红。
func TestCreateOrderGeneratesKeyAtEntry(t *testing.T) {
	src, err := os.ReadFile("mall.go")
	if err != nil {
		t.Fatalf("读 mall.go: %v", err)
	}
	found := false
	for _, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "orderNo := newOrderNo()") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("下单入口没有生成幂等键（orderNo := newOrderNo()）—— " +
			"键必须由入口决定并在重试之间保持不变")
	}
}
