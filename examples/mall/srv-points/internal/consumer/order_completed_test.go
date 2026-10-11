package consumer

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gospacex/mqx"

	"github.com/gospacex/launchx/examples/mall/pkg/codes"
	"github.com/gospacex/launchx/examples/mall/pkg/events"
	"github.com/gospacex/launchx/examples/mall/srv-points/internal/model"
)

// ======================= 测试替身 =======================

// fakePoints 积分域替身：自带按 OrderNo 的幂等（模拟 uk_ledger_order_no_type）。
type fakePoints struct {
	mu      sync.Mutex
	calls   []model.EarnCmd
	err     error
	ledgers map[string]bool
	balance int64
}

func (f *fakePoints) Earn(_ context.Context, cmd model.EarnCmd) (*model.ChangeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cmd)
	if f.err != nil {
		return nil, f.err
	}
	if f.ledgers == nil {
		f.ledgers = map[string]bool{}
	}
	if f.ledgers[cmd.OrderNo] {
		return nil, &codes.Error{Code: codes.CodePointsDuplicated, Message: "duplicated"}
	}
	f.ledgers[cmd.OrderNo] = true
	f.balance += cmd.Amount
	return &model.ChangeResult{LedgerID: uint64(len(f.ledgers)), BalanceAfter: f.balance}, nil
}

func (f *fakePoints) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakePoints) last() model.EarnCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return model.EarnCmd{}
	}
	return f.calls[len(f.calls)-1]
}

func (f *fakePoints) balanceOf() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.balance
}

// fakeConsumer 消费者替身，行为可配（立即返回 / 永不返回）。
type fakeConsumer struct {
	hang      bool
	started   chan struct{}
	handlerRc func(ctx context.Context, msg *mqx.Message) error
}

func newFakeConsumer(hang bool) *fakeConsumer {
	return &fakeConsumer{hang: hang, started: make(chan struct{}, 1)}
}

func (f *fakeConsumer) Consume(ctx context.Context, h func(ctx context.Context, msg *mqx.Message) error) error {
	f.handlerRc = h
	f.started <- struct{}{}
	if f.hang {
		// 模拟 kafka-go ConsumerGroup.Close 卡在 joinGroup 的网络读上：
		// 连 ctx 取消都不响应（真实故障形态，见 Runner.Stop 注释）。
		select {}
	}
	return ctx.Err()
}

func (f *fakeConsumer) Start(ctx context.Context, h func(ctx context.Context, msg *mqx.Message) error) error {
	return f.Consume(ctx, h)
}
func (f *fakeConsumer) Close() error      { return nil }
func (f *fakeConsumer) Name() string      { return "fake" }
func (f *fakeConsumer) HealthCheck() bool { return true }

// ======================= 判据 =======================

func validEvent() events.OrderCompleted {
	return events.OrderCompleted{
		OrderID:    1,
		OrderNo:    "M202601011200001234",
		UserID:     42,
		Amount:     500,
		SchemaVer:  events.SchemaVer1,
		OccurredAt: time.Now().UnixMilli(),
	}
}

func msgOf(t *testing.T, ev events.OrderCompleted) *mqx.Message {
	t.Helper()
	b, err := events.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return &mqx.Message{Topic: events.TopicOrderCompleted, Key: ev.OrderNo, Value: b}
}

// TestHandleEarnsOnValidEvent 正常事件必须落到一次入账，参数原样传递。
func TestHandleEarnsOnValidEvent(t *testing.T) {
	p := &fakePoints{}
	h := NewHandler(p, nil)

	if err := h.Handle(context.Background(), msgOf(t, validEvent())); err != nil {
		t.Fatalf("合法事件不应返回 error：%v", err)
	}
	if p.callCount() != 1 {
		t.Fatalf("应入账一次，实际 %d 次", p.callCount())
	}
	cmd := p.last()
	if cmd.OrderNo != "M202601011200001234" || cmd.UserID != 42 || cmd.Amount != 500 {
		t.Fatalf("入账参数与事件不一致：%+v", cmd)
	}
	if st := h.Stats(); st.Processed != 1 || st.Skipped != 0 || st.Failed != 0 {
		t.Fatalf("计数不对：%+v", st)
	}
}

// TestHandleDropsBadMessages 坏消息必须**丢弃**（返回 nil），而不是返回 error。
//
// 为什么是丢弃而不是重试：解析不出来的消息重试一万次还是解析不出来，返回
// error 会让它一直被重投，把整个分区堵在一条坏消息上（head-of-line blocking）。
//
// 反向探针（已实测）：把 Handle 里丢弃分支的 `return nil` 改成 `return err` → 本用例红。
func TestHandleDropsBadMessages(t *testing.T) {
	cases := []struct {
		name string
		msg  *mqx.Message
	}{
		{"nil message", nil},
		{"empty payload", &mqx.Message{Topic: events.TopicOrderCompleted}},
		{"bad json", &mqx.Message{Value: []byte("{not-json")}},
		{"missing order_no", func() *mqx.Message {
			ev := validEvent()
			ev.OrderNo = ""
			return msgOf(t, ev)
		}()},
		{"missing user_id", func() *mqx.Message {
			ev := validEvent()
			ev.UserID = 0
			return msgOf(t, ev)
		}()},
		{"non-positive amount", func() *mqx.Message {
			ev := validEvent()
			ev.Amount = 0
			return msgOf(t, ev)
		}()},
		{"future schema_ver", func() *mqx.Message {
			ev := validEvent()
			ev.SchemaVer = events.SchemaVer1 + 1
			return msgOf(t, ev)
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &fakePoints{}
			h := NewHandler(p, nil)
			if err := h.Handle(context.Background(), tc.msg); err != nil {
				t.Fatalf("坏消息必须丢弃（返回 nil），got %v", err)
			}
			if p.callCount() != 0 {
				t.Fatalf("坏消息不应触发入账，调用了 %d 次", p.callCount())
			}
			if st := h.Stats(); st.Skipped != 1 || st.Processed != 0 || st.Failed != 0 {
				t.Fatalf("坏消息应计入 skipped：%+v", st)
			}
		})
	}
}

// TestHandleRetriesTransientFailure 下游抖动必须返回 error（交给传输层重投）。
func TestHandleRetriesTransientFailure(t *testing.T) {
	p := &fakePoints{err: &codes.Error{Code: codes.CodePointsWriteFailed, Message: "write failed"}}
	h := NewHandler(p, nil)

	err := h.Handle(context.Background(), msgOf(t, validEvent()))
	if err == nil {
		t.Fatal("暂时性失败必须返回 error（否则重投机制不会介入）")
	}
	if st := h.Stats(); st.Failed != 1 || st.Processed != 0 {
		t.Fatalf("应计入 failed：%+v", st)
	}
}

// TestHandleDropsBusinessRejection 业务拒绝（重试也不会变）必须丢弃。
func TestHandleDropsBusinessRejection(t *testing.T) {
	p := &fakePoints{err: &codes.Error{Code: codes.CodePointsAmountInvalid, Message: "amount invalid"}}
	h := NewHandler(p, nil)

	if err := h.Handle(context.Background(), msgOf(t, validEvent())); err != nil {
		t.Fatalf("业务拒绝应丢弃，got %v", err)
	}
	if st := h.Stats(); st.Skipped != 1 || st.Failed != 0 {
		t.Fatalf("业务拒绝应计入 skipped 而不是 failed：%+v", st)
	}
}

// TestHandleIdempotentReplayIsSuccess 幂等命中视作成功：重投是常态，
// 把它算成失败会让"正常的重复投递"在计数上冒充故障。
func TestHandleIdempotentReplayIsSuccess(t *testing.T) {
	p := &fakePoints{err: &codes.Error{Code: codes.CodePointsDuplicated, Message: "duplicated"}}
	h := NewHandler(p, nil)

	if err := h.Handle(context.Background(), msgOf(t, validEvent())); err != nil {
		t.Fatalf("幂等命中不应返回 error：%v", err)
	}
	if st := h.Stats(); st.Processed != 1 || st.Failed != 0 || st.Skipped != 0 {
		t.Fatalf("幂等命中应计入 processed：%+v", st)
	}
}

// TestHandleRepeatedDeliveryDoesNotDoubleEarn 同一条消息重投两次只加一次钱。
//
// 幂等由**积分域**保证（唯一键 + kvx 键），消费端自己不再记一份"处理过的
// OrderNo" —— 同一个事实存两处迟早不一致。这条用例钉住的是"交给下游裁决"
// 这个分工确实成立，而不是消费端偷偷去重。
func TestHandleRepeatedDeliveryDoesNotDoubleEarn(t *testing.T) {
	p := &fakePoints{}
	h := NewHandler(p, nil)
	m := msgOf(t, validEvent())

	if err := h.Handle(context.Background(), m); err != nil {
		t.Fatalf("首次：%v", err)
	}
	if err := h.Handle(context.Background(), m); err != nil {
		t.Fatalf("重投不应报错（下游按幂等命中处理）：%v", err)
	}
	if p.callCount() != 2 {
		t.Fatalf("两次投递都应传给下游（由下游裁决），实际 %d 次", p.callCount())
	}
	if got := p.balanceOf(); got != 500 {
		t.Fatalf("重复投递不应重复加钱，余额 = %d", got)
	}
	if st := h.Stats(); st.Processed != 2 {
		t.Fatalf("两次都算处理成功：%+v", st)
	}
}

// TestRunnerStopIsBounded 停止必须有界：消费循环卡死时 Stop 仍要在超时内返回。
//
// 这是"进程退得出来"的一条（2026-10-07 实测：kafka-go 的 ConsumerGroup.Close
// 卡在 joinGroup 的网络读上，把整个进程拖到优雅期耗尽被 SIGKILL）。
//
// 反向探针（已实测）：去掉 Stop 里的 time.After 分支 → 本用例挂住（不是变红，是超时）。
func TestRunnerStopIsBounded(t *testing.T) {
	c := newFakeConsumer(true)
	r := NewRunner(c, NewHandler(&fakePoints{}, nil), nil)
	r.Start(context.Background())
	<-c.started

	done := make(chan bool, 1)
	go func() { done <- r.Stop(100 * time.Millisecond) }()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("消费循环没退出时 Stop 应返回 false（有一环被放弃）")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop 跟着卡住的消费循环一起挂住了 —— 又变回无界了")
	}
}

// TestRunnerStopReportsCleanExit 正常退出时 Stop 返回 true（区分"收干净"与"放弃"）。
func TestRunnerStopReportsCleanExit(t *testing.T) {
	c := newFakeConsumer(false)
	r := NewRunner(c, NewHandler(&fakePoints{}, nil), nil)
	r.Start(context.Background())
	<-c.started

	if ok := r.Stop(time.Second); !ok {
		t.Fatal("消费循环已退出时 Stop 应返回 true")
	}
}

// TestRunnerDeliversToHandler Runner 必须把消息真的交给 Handler（接线不是摆设）。
func TestRunnerDeliversToHandler(t *testing.T) {
	c := newFakeConsumer(false)
	p := &fakePoints{}
	r := NewRunner(c, NewHandler(p, nil), nil)
	r.Start(context.Background())
	<-c.started

	if c.handlerRc == nil {
		t.Fatal("consumer 没有收到 handler —— 接线断了")
	}
	if err := c.handlerRc(context.Background(), msgOf(t, validEvent())); err != nil {
		t.Fatalf("handler 处理失败：%v", err)
	}
	if p.callCount() != 1 {
		t.Fatalf("消息应到达下游一次，实际 %d", p.callCount())
	}
}

// TestRunnerNilConsumerIsNoop 消费者未装配（降级）时启停都不该 panic。
func TestRunnerNilConsumerIsNoop(t *testing.T) {
	r := NewRunner(nil, NewHandler(&fakePoints{}, nil), nil)
	r.Start(context.Background())
	if ok := r.Stop(10 * time.Millisecond); !ok {
		t.Fatal("consumer 为 nil 时 Stop 应直接返回 true")
	}
}

// TestOrderConsumerUsesExplicitGroupID 订阅侧配置必须显式写 group_id。
//
// 为什么值得单列（2026-10 实测过的坑）：mqx 缺省把它填成 `mqx-group`，
// 于是几个服务挤进同一个消费组、互相抢分区，表现为"消息投出去了但没人收到"
// —— 而且没有任何报错。配置面唯一能挡住它的就是这条判据。
//
// 反向探针（已实测）：把 group_id 改成 mqx-group → 本用例红。
func TestOrderConsumerUsesExplicitGroupID(t *testing.T) {
	const cfgPath = "../../config.yaml"
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("读 %s 失败：%v", cfgPath, err)
	}
	lines := strings.Split(string(b), "\n")
	inInstance := false
	sawTopic := false
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "srv-points-order-in:") {
			inInstance = true
			continue
		}
		if !inInstance {
			continue
		}
		// 实例块结束（回到同级或更外层缩进的键）
		if trimmed != "" && !strings.HasPrefix(ln, "      ") {
			break
		}
		if strings.HasPrefix(trimmed, "topic: order.completed.v1") {
			sawTopic = true
		}
		if strings.HasPrefix(trimmed, "group_id:") {
			if !sawTopic {
				t.Fatalf("%s 里 group_id 出现在 topic 之前，判据逻辑要改", cfgPath)
			}
			id := strings.TrimSpace(strings.TrimPrefix(trimmed, "group_id:"))
			if id == "" || id == "mqx-group" {
				t.Fatalf("订阅侧 group_id 必须显式命名且不是缺省值，got %q", id)
			}
			return
		}
	}
	t.Fatalf("%s 里没有 srv-points-order-in 的 group_id —— 消费组会落到缺省值 mqx-group", cfgPath)
}

// TestDecodeKeepsRejectReason 丢弃原因要能落到日志里（排障时只看计数没用）。
func TestDecodeKeepsRejectReason(t *testing.T) {
	if _, why, ok := decode(nil); ok || why == "" {
		t.Fatal("丢弃必须带原因")
	}
	if _, why, ok := decode(&mqx.Message{}); ok || !strings.Contains(why, "empty payload") {
		t.Fatalf("空 payload 的原因应说明是空 payload，got %q", why)
	}
}
