package service

import (
	"context"
	"strings"
	"testing"
)

// TestResolveOrderNoPrefersCallerKey 调用方给了幂等键就必须用它的。
//
// 「给了就用」是幂等的全部意义：入口生成的号在重试之间保持不变，订单域据此认出
// 同一笔请求。订单域自己另起一个号，等于每次重放都是新单 —— 而链路与日志看起来
// 全正常，是最难自查的那类失效。
//
// 反向探针：把 resolveOrderNo 改成无条件 newOrderNo()，本用例立刻红。
func TestResolveOrderNoPrefersCallerKey(t *testing.T) {
	s := &OrderService{} // logger 为 nil：resolveOrderNo 内部已判空
	if got := s.resolveOrderNo(context.Background(), "M-GIVEN-1"); got != "M-GIVEN-1" {
		t.Errorf("调用方给了键却没用上：got %q, want %q（重放会变成新单）", got, "M-GIVEN-1")
	}
}

// TestResolveOrderNoGeneratesWhenAbsent 没给键时要自己生成，且生成的是可用格式。
//
// 空号是灾难性的：order_no 为空的单会互相"命中"查重（第一个空号单之后的所有
// 空号单都被当成重放），故必须保证永不返回空串。
func TestResolveOrderNoGeneratesWhenAbsent(t *testing.T) {
	s := &OrderService{}
	got := s.resolveOrderNo(context.Background(), "")
	if got == "" {
		t.Fatal("调用方没给键时必须自己生成 —— 空 order_no 会让后续所有无键单互相命中查重")
	}
	if !strings.HasPrefix(got, "M") || len(got) < 10 {
		t.Errorf("生成的单号格式不对：%q（期望 M + 时间戳 + 随机段）", got)
	}
}

// TestNewOrderNoDoesNotCollide 单号必须带随机段。
//
// 此前这里是 `M%s%06d`（时间戳 + userID%1000000）：时间精度只到秒、用户段只取
// 后 6 位 —— **同一用户同一秒下两单必然撞号**。撞号在有唯一键时表现为第二单
// duplicate error（用户看到下单失败），没唯一键时表现为两单同号（后续按 order_no
// 查询 / 对账串单）。
//
// 反向探针：把 newOrderNo 里的随机段去掉（只留时间戳 + 用户段），本用例立刻红。
func TestNewOrderNoDoesNotCollide(t *testing.T) {
	const n = 2000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		no := newOrderNo()
		if _, dup := seen[no]; dup {
			t.Fatalf("第 %d 个单号与前面重复：%q（同一秒内的两笔单必须可区分）", i, no)
		}
		seen[no] = struct{}{}
	}
}
