// Package model 表模型：纯数据，不依赖任何中间件句柄。
package model

import "time"

// Product 商品主表（单表）。
//
// 只落「商品自身的字段」，不含分类名 / SKU 数 / 销量这类派生字段：它们是 join
// 或聚合的结果，缓存进主表会让「哪份是真相」出现两个来源。IDL 的 ProductInfo
// 里那几个字段在 handler 转换时留空，由需要它们的下游（列表 / 报表）自行补齐。
type Product struct {
	ID         uint64    `gorm:"primaryKey;column:id"`
	Title      string    `gorm:"column:title"`
	Price      int64     `gorm:"column:price"`
	Status     int32     `gorm:"column:status"`
	CategoryID uint64    `gorm:"column:category_id"`
	CreatedAt  time.Time `gorm:"column:created_at"`
	UpdatedAt  time.Time `gorm:"column:updated_at"`
}

// TableName 固定表名，避免依赖 gorm 的复数化规则（规则变动会让表名漂移）。
func (Product) TableName() string { return "products" }
