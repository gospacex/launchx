// Package events 定义 mall 四服之间的异步事件体（纯数据，无外部依赖）。
//
// 三个事件对应三段跨域协作：
//
//	product.created.v1  srv-product 产出（供检索索引与缓存预热侧消费）
//	order.completed.v1  srv-order 产出 → srv-points 消费（下单送积分）
//	stock.reserved.v1   srv-stock 产出 → srv-order 消费（预占结果回执）
//	points.changed.v1   srv-points 产出（链路终点，供对账与 BI）
//
// traceparent 走 mqx Message.Headers[TraceHeaderKey]，**不进消息体**：
// 进体会让每个事件都携带一份随 payload 变化的字符串，破坏「同 payload 同哈希」
// 的去重前提；放 header 则由 mqx 消费侧统一注入 ctx。
package events

import "encoding/json"

// TraceHeaderKey mqx Message.Headers 中传递 W3C TraceContext 的键
const TraceHeaderKey = "traceparent"

// Topic 常量（与 config.yaml 的 mqx.producer.topic 保持一致）
const (
	TopicProductCreated = "product.created.v1"
	TopicOrderCompleted = "order.completed.v1"
	TopicStockReserved  = "stock.reserved.v1"
	TopicPointsChanged  = "points.changed.v1"
)

// SchemaVer1 当前事件 schema 版本
const SchemaVer1 = 1

// OrderItem 订单明细（事件内嵌，字段与 kitex_gen/order 的 OrderItem 对齐）
type OrderItem struct {
	ProductID int64  `json:"product_id"`
	Name      string `json:"name"`
	Quantity  int    `json:"quantity"`
	UnitPrice int64  `json:"unit_price"`
}

// ProductCreated 商品创建事件（srv-product 产出，供检索/缓存侧消费）
type ProductCreated struct {
	ProductID  int64  `json:"product_id"`
	Title      string `json:"title"`
	CategoryID int64  `json:"category_id"`
	Price      int64  `json:"price"` // 分
	OperatorID int64  `json:"operator_id"`
	SchemaVer  int    `json:"schema_ver"`
	TraceID    string `json:"trace_id,omitempty"`
	OccurredAt int64  `json:"occurred_at"` // unix 毫秒
}

// OrderCompleted 下单完成事件（srv-order → srv-points）
type OrderCompleted struct {
	OrderID    int64       `json:"order_id"`
	OrderNo    string      `json:"order_no"`
	UserID     int64       `json:"user_id"`
	Amount     int64       `json:"amount"` // 分
	Items      []OrderItem `json:"items"`
	SchemaVer  int         `json:"schema_ver"`
	TraceID    string      `json:"trace_id,omitempty"`
	OccurredAt int64       `json:"occurred_at"` // unix 毫秒
}

// StockReserved 库存预占结果事件（srv-stock → srv-order）
type StockReserved struct {
	OrderNo     string `json:"order_no"`
	UserID      int64  `json:"user_id"`
	ProductID   int64  `json:"product_id"`
	Quantity    int32  `json:"quantity"`
	Success     bool   `json:"success"`
	Available   int32  `json:"available"`
	Reservation int64  `json:"reservation_id"`
	SchemaVer   int    `json:"schema_ver"`
	TraceID     string `json:"trace_id,omitempty"`
	OccurredAt  int64  `json:"occurred_at"`
}

// PointsChanged 积分变动事件（srv-points 产出，链路终点）
type PointsChanged struct {
	UserID     int64  `json:"user_id"`
	OrderNo    string `json:"order_no"`
	Type       int32  `json:"type"` // 1 入账 / 2 消费
	Amount     int64  `json:"amount"`
	Balance    int64  `json:"balance"`
	SchemaVer  int    `json:"schema_ver"`
	TraceID    string `json:"trace_id,omitempty"`
	OccurredAt int64  `json:"occurred_at"`
}

// Marshal 统一序列化入口（事件体 JSON 字节）
func Marshal(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

// UnmarshalProductCreated 反序列化商品创建事件
func UnmarshalProductCreated(b []byte) (*ProductCreated, error) {
	var ev ProductCreated
	if err := json.Unmarshal(b, &ev); err != nil {
		return nil, err
	}
	return &ev, nil
}

// UnmarshalOrderCompleted 反序列化下单事件
func UnmarshalOrderCompleted(b []byte) (*OrderCompleted, error) {
	var ev OrderCompleted
	if err := json.Unmarshal(b, &ev); err != nil {
		return nil, err
	}
	return &ev, nil
}

// UnmarshalStockReserved 反序列化库存预占事件
func UnmarshalStockReserved(b []byte) (*StockReserved, error) {
	var ev StockReserved
	if err := json.Unmarshal(b, &ev); err != nil {
		return nil, err
	}
	return &ev, nil
}

// UnmarshalPointsChanged 反序列化积分变动事件
func UnmarshalPointsChanged(b []byte) (*PointsChanged, error) {
	var ev PointsChanged
	if err := json.Unmarshal(b, &ev); err != nil {
		return nil, err
	}
	return &ev, nil
}
