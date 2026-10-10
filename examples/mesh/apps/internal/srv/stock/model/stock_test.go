package model

import (
	"reflect"
	"testing"
)

// 表名是硬契约：用显式 TableName() 固定下来，避免 gorm 复数化规则变动让表名漂移。
//
// 本域是**单表**模型 —— 只有 stocks 一张（与 order 域的多子表结构相对）。
func TestStockTableName(t *testing.T) {
	if got, want := (Stock{}).TableName(), "stocks"; got != want {
		t.Fatalf("Stock.TableName() = %q, want %q", got, want)
	}
}

// 列集与 IDL 的 StockInfo 一一对应：handler 因此零派生字段，不做任何字段留空。
//
// 判据取 gorm 的 column tag（而非字段名），因为真正落地到表的列名由 tag 决定；
// 若 tag 拼错，handler 的 StockInfo 映射会与表里实际列对不上，这是在 model 层
// 就该钉住的契约。
func TestStockColumnsMatchIDLStockInfo(t *testing.T) {
	// IDL StockInfo 的字段集（蛇形列名）+ 主键 id + 时间戳 updated_at。
	want := map[string]bool{
		"id": true, "product_id": true, "total": true,
		"available": true, "reserved": true, "version": true, "updated_at": true,
	}
	got := make(map[string]bool)
	typ := reflect.TypeOf(Stock{})
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("gorm")
		name := columnOf(tag)
		if name == "" {
			t.Fatalf("字段 %s 缺 column tag（列名会随风飘）", typ.Field(i).Name)
		}
		got[name] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stocks 列集 = %v, want %v", got, want)
	}
}

// columnOf 从 gorm tag 里取出 column= 后的列名（够用即可，不引 gorm 解析器）。
func columnOf(tag string) string {
	const key = "column:"
	for i := 0; i+len(key) <= len(tag); i++ {
		if tag[i:i+len(key)] == key {
			j := i + len(key)
			k := j
			for k < len(tag) && tag[k] != ';' {
				k++
			}
			return tag[j:k]
		}
	}
	return ""
}
