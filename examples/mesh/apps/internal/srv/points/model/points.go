// Package model 表模型：纯数据，不依赖任何中间件句柄。
package model

import "time"

// PointAccount 积分账户表：每用户一行。
//
// user_id 上的唯一索引是 upsert 正确性的前提：repository.AddBalance 用
// gorm clause.OnConflict 让**库**做 `balance = balance + ?`（MySQL 下由
// mysql driver 渲染成 ON DUPLICATE KEY UPDATE），没有唯一约束时 ON CONFLICT
// 无从命中，会静默退化成「每次插入一行新账户」。
type PointAccount struct {
	ID        uint64    `gorm:"primaryKey;column:id"`
	UserID    uint64    `gorm:"column:user_id;uniqueIndex"`
	Balance   int64     `gorm:"column:balance"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// 表名固定为 points_account，避免依赖 gorm 的复数化规则（规则变动会让表名漂移）。
func (PointAccount) TableName() string { return "points_account" }

// PointsLog 积分流水表（append-only）。
//
// order_id 记录本笔积分的来源订单，是「一条消息加一笔分」的可对账凭据。
type PointsLog struct {
	ID        uint64    `gorm:"primaryKey;column:id"`
	UserID    uint64    `gorm:"column:user_id"`
	OrderID   uint64    `gorm:"column:order_id"`
	Delta     int64     `gorm:"column:delta"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

// 表名同样固定，理由见上（不依赖 gorm 的复数化规则）。
func (PointsLog) TableName() string { return "points_log" }
