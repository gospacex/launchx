// Package model 订单领域的持久化模型：三张表 orders / order_items / users，
// 以及连表查询的结果行 OrderWithUser。
//
// 本包只描述表结构与结果行形态，不含任何读写逻辑（读写在 repository 层）。
package model

import "time"

// Order 订单主表（orders）。
type Order struct {
	ID        int64     `gorm:"primaryKey;column:id"          json:"id"`
	OrderNo   string    `gorm:"column:order_no;size:64;uniqueIndex" json:"order_no"`
	UserID    int64     `gorm:"column:user_id;index"          json:"user_id"`
	Item      string    `gorm:"column:item;size:128"          json:"item"`
	Qty       int       `gorm:"column:qty"                    json:"qty"`
	Amount    float64   `gorm:"column:amount;type:decimal(10,2)" json:"amount"`
	Status    string    `gorm:"column:status;size:32"         json:"status"`
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime" json:"updated_at"`
}

// TableName 映射表名。
func (Order) TableName() string { return "orders" }

// OrderItem 订单明细表（order_items）：事务写入场景的第二张表。
type OrderItem struct {
	ID       int64   `gorm:"primaryKey;column:id"      json:"id"`
	OrderNo  string  `gorm:"column:order_no;size:64;index" json:"order_no"`
	SKU      string  `gorm:"column:sku;size:64"        json:"sku"`
	Qty      int     `gorm:"column:qty"                json:"qty"`
	Price    float64 `gorm:"column:price;type:decimal(10,2)" json:"price"`
	Subtotal float64 `gorm:"column:subtotal;type:decimal(10,2)" json:"subtotal"`
}

// TableName 映射表名。
func (OrderItem) TableName() string { return "order_items" }

// User 用户表（users）：连表查询的 join 目标。
type User struct {
	ID    int64  `gorm:"primaryKey;column:id"     json:"id"`
	Name  string `gorm:"column:name;size:64"      json:"name"`
	Level string `gorm:"column:level;size:16"     json:"level"`
}

// TableName 映射表名。
func (User) TableName() string { return "users" }

// OrderWithUser orders JOIN users 的结果行。
//
// 单独定义而非复用 Order：连表结果带的是用户冗余字段，混进业务模型会让
// 「单表 CRUD」与「连表查询」两条链路的返回形态粘连，T13 判据就不好按方法分辨。
type OrderWithUser struct {
	ID        int64   `gorm:"column:id"              json:"id"`
	OrderNo   string  `gorm:"column:order_no"        json:"order_no"`
	UserID    int64   `gorm:"column:user_id"         json:"user_id"`
	Item      string  `gorm:"column:item"            json:"item"`
	Qty       int     `gorm:"column:qty"             json:"qty"`
	Amount    float64 `gorm:"column:amount"          json:"amount"`
	Status    string  `gorm:"column:status"          json:"status"`
	UserName  string  `gorm:"column:user_name"       json:"user_name"`
	UserLevel string  `gorm:"column:user_level"      json:"user_level"`
}
