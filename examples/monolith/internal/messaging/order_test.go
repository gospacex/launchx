package messaging

import (
	"context"
	"errors"
	"testing"
)

// TestPublishWithoutProducerReturnsError 钉住「producer 缺席返回 error，不 panic」。
//
// 包注释曾经承诺「producer 为空时返回 error 并打进 span」，实现却是直接
// `o.producer.Send(...)` —— nil 接口上调方法是 panic，注释与实现相反。而且它
// 崩在 MQ 降级这个最不该崩的时刻：订单已经落库，事件把进程带走。
//
// 反向探针：把 Publish 里 `if o.producer == nil` 这段删掉，本用例以 panic 失败
// （不是断言失败 —— 正好说明旧实现的失败姿态比"返回 error"严重一档）。
func TestPublishWithoutProducerReturnsError(t *testing.T) {
	o := NewOrder(nil, "order.completed.v1", false)
	err := o.Publish(context.Background(), "NO-1", []byte(`{}`))
	if err == nil {
		t.Fatal("producer 为空时必须返回 error：静默成功会把丢事件伪装成已投递")
	}
	if !errors.Is(err, ErrProducerUnavailable) {
		t.Fatalf("err = %v, want 包住 ErrProducerUnavailable（调用方要能判降级）", err)
	}
}

// TestPublishOnNilOrderReturnsError 投递器本身为 nil 时也要返回 error。
//
// 装配层在 MQ 句柄缺席时不构造 *Order，调用方虽已判 `s.messaging != nil`，但
// 这条路径一旦被复用（新增 fanout 分支）就会踩到 nil receiver —— 让它可判定，
// 而不是留给 panic。
func TestPublishOnNilOrderReturnsError(t *testing.T) {
	var o *Order
	err := o.Publish(context.Background(), "NO-1", []byte(`{}`))
	if !errors.Is(err, ErrProducerUnavailable) {
		t.Fatalf("err = %v, want ErrProducerUnavailable", err)
	}
}
