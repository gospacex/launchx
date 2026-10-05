// Package events 定义 Kafka 事件体（纯数据，无外部依赖）。
//
// 三个事件：order.completed（srv-order 产出，job 消费）、
// coupon.exchanged（srv-coupon 产出，job 消费）、points.granted（srv-coupon 产出，链路终点）。
// 字段名与 pkg/proto 消息体逐字对齐；SchemaVer 区分 schema 演进。
//
// traceparent 走 Kafka message header（见 TraceHeaderKey），不进消息体。
package events

import (
	"encoding/json"
)

// TraceHeaderKey Kafka message header 中传递 W3C TraceContext 的键
const TraceHeaderKey = "traceparent"

// SchemaVer1 当前事件 schema 版本
const SchemaVer1 = 1

// OrderItem 订单明细（事件内嵌，字段与 pkg/proto/order 的 OrderItem 对齐）
type OrderItem struct {
	ProductID int64  `json:"product_id"`
	Name      string `json:"name"`
	Quantity  int    `json:"quantity"`
	UnitPrice int64  `json:"unit_price"`
}

// OrderCompleted 下单完成事件
type OrderCompleted struct {
	OrderID    int64       `json:"order_id"`
	UserID     int64       `json:"user_id"`
	Amount     int64       `json:"amount"` // 分
	Items      []OrderItem `json:"items"`
	SchemaVer  int         `json:"schema_ver"`
	TraceID    string      `json:"trace_id,omitempty"`
	OccurredAt int64       `json:"occurred_at"` // unix 毫秒
}

// CouponExchanged 用券核销事件
type CouponExchanged struct {
	UserID     int64  `json:"user_id"`
	CouponID   int64  `json:"coupon_id"`
	OrderID    int64  `json:"order_id"`
	Discount   int64  `json:"discount"` // 分
	SchemaVer  int    `json:"schema_ver"`
	TraceID    string `json:"trace_id,omitempty"`
	OccurredAt int64  `json:"occurred_at"`
}

// PointsGranted 积分发放事件（链路终点事件，无下游消费者）
type PointsGranted struct {
	UserID     int64  `json:"user_id"`
	Points     int    `json:"points"`
	Source     string `json:"source"` // "order.completed" / "coupon.exchanged" / "job.retry"
	OrderID    int64  `json:"order_id,omitempty"`
	SchemaVer  int    `json:"schema_ver"`
	TraceID    string `json:"trace_id,omitempty"`
	OccurredAt int64  `json:"occurred_at"`
}

// Marshal 统一序列化入口（事件体 JSON 字节）
func Marshal(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

// UnmarshalOrderCompleted 反序列化下单事件
func UnmarshalOrderCompleted(b []byte) (*OrderCompleted, error) {
	var ev OrderCompleted
	if err := json.Unmarshal(b, &ev); err != nil {
		return nil, err
	}
	return &ev, nil
}

// UnmarshalCouponExchanged 反序列化用券事件
func UnmarshalCouponExchanged(b []byte) (*CouponExchanged, error) {
	var ev CouponExchanged
	if err := json.Unmarshal(b, &ev); err != nil {
		return nil, err
	}
	return &ev, nil
}

// UnmarshalPointsGranted 反序列化积分事件
func UnmarshalPointsGranted(b []byte) (*PointsGranted, error) {
	var ev PointsGranted
	if err := json.Unmarshal(b, &ev); err != nil {
		return nil, err
	}
	return &ev, nil
}
