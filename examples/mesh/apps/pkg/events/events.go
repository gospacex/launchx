// Package events 领域事件载荷：跨进程只有这些结构，MQ 的 topic 常量也在这里。
//
// 为什么单独立包而不是塞进各域的 service：投递侧（order）与消费侧（points）
// 必须共用同一份结构与 topic 名，否则「生产者写 v1、消费者订阅 v2」这类
// 错配会以「消息发了没人收」的形式静默存在。
package events

import (
	"encoding/json"
	"errors"
	"time"
)

// TopicOrderCompleted 订单完成事件。生产方 srv-order，消费方 srv-points。
//
// 键位口径（mqx/kafka 现状，实测）：producer 的目标 topic 读的是
// `consumer.topic`，配 `producer.topic` 不生效（FillDefaults 只把该键填给 writer），
// 发送时会因 topic 为空报 Unknown Topic Or Partition。故配置侧对齐该键。
const TopicOrderCompleted = "order.completed.v1"

// OrderCompleted 订单完成。
type OrderCompleted struct {
	OrderID     uint64    `json:"order_id"`
	UserID      uint64    `json:"user_id"`
	TotalAmount int64     `json:"total_amount"`
	OccurredAt  time.Time `json:"occurred_at"`
}

// Marshal 序列化；失败即返回 error，由调用方决定重试还是落 outbox。
func (e OrderCompleted) Marshal() ([]byte, error) { return json.Marshal(e) }

// UnmarshalOrderCompleted 反序列化；OrderID 为零即报错，避免消费侧
// 拿一个 OrderID=0 的事件去加积分（那会写出一条永远对不上账的流水）。
func UnmarshalOrderCompleted(b []byte) (OrderCompleted, error) {
	var e OrderCompleted
	if err := json.Unmarshal(b, &e); err != nil {
		return OrderCompleted{}, err
	}
	if e.OrderID == 0 {
		return OrderCompleted{}, errors.New("events: order_id is zero")
	}
	return e, nil
}
