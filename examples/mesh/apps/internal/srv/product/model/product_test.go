package model

import "testing"

// 表名是硬契约：用显式 TableName() 固定下来，避免 gorm 复数化规则变动让表名漂移。
//
// 本域是**单表**模型 —— 只有 products 一张，没有 order_items / order_status_log
// 那类子表（这是与 order 域 model 的差异点）。
func TestTableName(t *testing.T) {
	if got, want := (Product{}).TableName(), "products"; got != want {
		t.Fatalf("Product.TableName() = %q, want %q", got, want)
	}
}
