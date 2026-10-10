package events

import (
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	in := OrderCompleted{
		OrderID:     42,
		UserID:      7,
		TotalAmount: 1990,
		OccurredAt:  time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC),
	}
	b, err := in.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out, err := UnmarshalOrderCompleted(b)
	if err != nil {
		t.Fatalf("UnmarshalOrderCompleted: %v", err)
	}
	if out.OrderID != in.OrderID || out.UserID != in.UserID || out.TotalAmount != in.TotalAmount || !out.OccurredAt.Equal(in.OccurredAt) {
		t.Fatalf("round trip 不一致: in=%+v out=%+v", in, out)
	}
}

func TestUnmarshalRejectsZeroOrderID(t *testing.T) {
	// OrderID=0 必须被拒：否则消费侧会加出一笔对不上账的积分流水。
	if _, err := UnmarshalOrderCompleted([]byte(`{"order_id":0,"user_id":1}`)); err == nil {
		t.Fatal("OrderID=0 应报错，实际通过")
	}
}

func TestUnmarshalRejectsMalformedJSON(t *testing.T) {
	if _, err := UnmarshalOrderCompleted([]byte(`not json`)); err == nil {
		t.Fatal("非法 JSON 应报错，实际通过")
	}
}
