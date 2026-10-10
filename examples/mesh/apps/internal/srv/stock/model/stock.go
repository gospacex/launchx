// Package model 表模型：纯数据，不依赖任何中间件句柄。
package model

import "time"

// Stock 库存主表（单表）。
//
// 列集与 IDL 的 StockInfo 字段集**一一对应**（product_id / total / available /
// reserved / version），故 handler 转换时零派生字段、不做任何留空。多出的
// id / updated_at 是持久化自身需要的主键与时间戳，不进 RPC 结构。
//
// 关于 Reserved：本示例没有写它的路径（IDL 的 ReserveStock 一族在 handler
// 显式 Unimplemented），列存在是为了**不做凭空造的字段** —— 真实库存系统里
// 「可售 available / 已预留 reserved」是一对语义，只留 available 会让 IDL 的
// StockInfo.Reserved 永远无源可依。
type Stock struct {
	ID        uint64    `gorm:"primaryKey;column:id"`
	ProductID uint64    `gorm:"column:product_id;uniqueIndex"`
	Total     int32     `gorm:"column:total"`
	Available int32     `gorm:"column:available"`
	Reserved  int32     `gorm:"column:reserved"`
	Version   int64     `gorm:"column:version"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// TableName 固定表名，避免依赖 gorm 的复数化规则（规则变动会让表名漂移）。
func (Stock) TableName() string { return "stocks" }
