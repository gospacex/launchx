// 下单幂等判据：同一个 order_no 的第二次落库必须「一行都不插」。
//
// 为什么单独一个文件：幂等不是"建了唯一键就有的"，而是查重必须发生在**事务内**
// 且覆盖所有副作用。只查订单表、漏掉 outbox，会出现「订单没重复、但积分域又收到
// 一次『订单已完成』」—— 那才是最难查的一种重复（订单侧一切正常）。
package repository

import (
	"context"
	"testing"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/model"
	"github.com/gospacex/outboxx"
)

// TestCreateIsIdempotentByOrderNo 重放同一个 order_no：回既有订单，不多插任何行。
//
// 背景：mesh apps 此前连幂等键都没有 —— 订单号是 service 层现编的
// `M%s%06d`（时间戳 + userID%1000000，同一用户同一秒必然撞号），调用方也没有
// 传键的通道，于是任何一次重试 / 重放都是一笔新单。
//
// 反向探针：把 Create 里事务内的查重分支删掉，本用例立刻红
// （重放返回新 ID，orders 变 2 行、outbox 变 2 条）。
func TestCreateIsIdempotentByOrderNo(t *testing.T) {
	db := newTestDB(t)
	r := New(db, false)
	ctx := context.Background()

	const no = "MIDEM0001"
	newOne := func() (*model.Order, []model.OrderItem) {
		return &model.Order{UserID: 7, OrderNo: no, Status: model.StatusCreated, TotalAmount: 100},
			[]model.OrderItem{{ProductID: 1, ProductName: "p", Quantity: 1, UnitPrice: 100}}
	}

	o1, it1 := newOne()
	out1, err := r.Create(ctx, o1, it1, func(uint64) (string, string) { return `{"order_id":1}`, "tp" })
	if err != nil {
		t.Fatalf("首次落库: %v", err)
	}
	if out1.Idempotent {
		t.Fatal("首次下单不该命中幂等（否则真单也会被当成重放丢掉）")
	}

	// 重放：同一个号，全新的一组模型对象（模拟另一次进程内/跨进程重试）。
	o2, it2 := newOne()
	out2, err := r.Create(ctx, o2, it2, func(uint64) (string, string) { return `{"order_id":2}`, "tp" })
	if err != nil {
		t.Fatalf("重放落库: %v", err)
	}
	if !out2.Idempotent {
		t.Fatal("重放同一个 order_no 必须命中幂等 —— 否则 rpcx 的失败重试 / 上游重发 / " +
			"用户双击都会变成重复下单")
	}
	if out2.ID != out1.ID {
		t.Errorf("幂等命中应回既有订单 ID：got %d, want %d", out2.ID, out1.ID)
	}

	var orders int64
	db.Model(&model.Order{}).Where("order_no = ?", no).Count(&orders)
	if orders != 1 {
		t.Errorf("重放后 orders 应仍为 1 行，实际 %d（重复下单）", orders)
	}

	var events int64
	db.Model(&outboxx.Event{}).Count(&events)
	if events != 1 {
		t.Errorf("重放不得再插一条 outbox 事件（否则积分域会再收一次「订单已完成」），实际 %d 条", events)
	}
}

// TestCreateDistinguishesDifferentOrderNo 反向语义：不同单号必须各落一单。
//
// 没有这条，把 Create 写成「有单就返回第一条」也能让上一条用例绿。
func TestCreateDistinguishesDifferentOrderNo(t *testing.T) {
	db := newTestDB(t)
	r := New(db, false)
	ctx := context.Background()

	mk := func(no string) (*model.Order, []model.OrderItem) {
		return &model.Order{UserID: 7, OrderNo: no, Status: model.StatusCreated, TotalAmount: 10},
			[]model.OrderItem{{ProductID: 1, ProductName: "p", Quantity: 1, UnitPrice: 10}}
	}
	o1, i1 := mk("MIDEM-A")
	if _, err := r.Create(ctx, o1, i1, nil); err != nil {
		t.Fatalf("第一单: %v", err)
	}
	o2, i2 := mk("MIDEM-B")
	out2, err := r.Create(ctx, o2, i2, nil)
	if err != nil {
		t.Fatalf("第二单: %v", err)
	}
	if out2.Idempotent {
		t.Fatal("不同 order_no 不应命中幂等（那会让第二笔真单被吞掉）")
	}

	var n int64
	db.Model(&model.Order{}).Count(&n)
	if n != 2 {
		t.Errorf("两个不同单号应落 2 单，实际 %d", n)
	}
}
