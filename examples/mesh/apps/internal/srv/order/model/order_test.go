package model

import "testing"

// 表名是硬契约：它必须与 examples/scripts/order_db.sh 的 DDL 真源一致。
// 用显式 TableName() 固定下来，避免 gorm 复数化规则变动让表名漂移。
func TestTableNames(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"Order", Order{}.TableName(), "orders"},
		{"OrderItem", OrderItem{}.TableName(), "order_items"},
		{"OrderStatusLog", OrderStatusLog{}.TableName(), "order_status_log"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Fatalf("%s.TableName() = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// 状态常量是状态机的字面量真源，值被改会静默改变流转条件（如 MarkPaid）。
func TestStatusConstants(t *testing.T) {
	if StatusCreated != "created" || StatusPaid != "paid" || StatusClosed != "closed" {
		t.Fatalf("状态常量漂移: created=%q paid=%q closed=%q",
			StatusCreated, StatusPaid, StatusClosed)
	}
}
