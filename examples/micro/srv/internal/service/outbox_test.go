// service 层单测：OutboxRunner.flushOnce 补发语义
// （sqlite repository + fake mqx.Producer；避免 3s ticker 等待）。
package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gospacex/mqx"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/gospacex/launchx/examples/micro/srv/internal/repository"
	"github.com/gospacex/launchx/examples/micro/srv/pkg/events"
)

// newSQLiteRepo 建内存 sqlite（表结构与 repository 测试一致）
func newSQLiteRepo(t *testing.T) *repository.OrderRepository {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&repository.Order{}, &repository.OrderItem{}, &repository.OutboxEvent{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return repository.NewOrderRepository(db)
}

// seedOrderWithEvent 造一条订单 + outbox 事件（payload 为 OrderCompleted JSON）
func seedOrderWithEvent(t *testing.T, repo *repository.OrderRepository, orderID int64, tp string) {
	t.Helper()
	ctx := context.Background()
	ev := events.OrderCompleted{
		OrderID:    orderID,
		UserID:     42,
		Amount:     100,
		SchemaVer:  events.SchemaVer1,
		OccurredAt: time.Now().UnixMilli(),
	}
	payload, err := events.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	if _, err := repo.CreateOrderTx(ctx, repository.CreateOrderInput{
		UserID:      42,
		OrderNo:     "O-SVC-" + time.Now().String(),
		TotalAmount: 100,
		OutboxFill:  func(uint64) (string, string) { return string(payload), tp },
	}); err != nil {
		t.Fatalf("CreateOrderTx: %v", err)
	}
}

// fakeProducer 记录发送
type fakeProducer struct {
	mu     sync.Mutex
	sent   []*mqx.Message
	fails  int // 前 N 次 Send 返回错误（模拟补发失败）
	calls  int
	closed bool
	// failKey 精确指定必定失败的消息 key（按订单分区 key = OrderID），
	// 用于在同一轮里让部分事件失败、部分成功。优先级高于 fails。
	failKey string
}

func (f *fakeProducer) Send(_ context.Context, msg *mqx.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failKey != "" && msg.Key == f.failKey {
		return errors.New("kafka: broker unavailable")
	}
	if f.calls <= f.fails {
		return errors.New("kafka: broker unavailable")
	}
	f.sent = append(f.sent, msg)
	return nil
}

func (f *fakeProducer) SendBatch(_ context.Context, _ []*mqx.Message) error { return nil }
func (f *fakeProducer) Close() error                                        { f.closed = true; return nil }
func (f *fakeProducer) Name() string                                        { return "fake" }
func (f *fakeProducer) HealthCheck() bool                                   { return true }

func TestOutboxRunnerFlush(t *testing.T) {
	repo := newSQLiteRepo(t)
	prod := &fakeProducer{}
	runner := NewOutboxRunner(repo, prod, nil)

	// 两条待发（一条带 traceparent）
	seedOrderWithEvent(t, repo, 101, "00-aa-bb-00")
	seedOrderWithEvent(t, repo, 102, "")

	runner.flushOnce()

	prod.mu.Lock()
	sent := len(prod.sent)
	var withTP, withoutTP int
	var keys []string
	for _, m := range prod.sent {
		hdr := m.Headers[events.TraceHeaderKey]
		if hdr == "00-aa-bb-00" {
			withTP++
		} else if hdr == "" {
			withoutTP++
		}
		keys = append(keys, m.Key)
	}
	prod.mu.Unlock()

	if sent != 2 {
		t.Fatalf("sent = %d, want 2", sent)
	}
	// key 取事件内嵌 OrderID（"101"/"102"），而非 outbox.id
	if len(keys) != 2 {
		t.Fatalf("keys = %v", keys)
	}
	if withTP != 1 || withoutTP != 1 {
		t.Fatalf("traceparent headers: with=%d without=%d (want 1/1)", withTP, withoutTP)
	}
	// 全部转 sent
	pending, err := repo.PendingOutbox(context.Background(), 10)
	if err != nil {
		t.Fatalf("PendingOutbox: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending rest = %+v (want 0)", pending)
	}
}

func TestOutboxRunnerFailureRetryAndAbandon(t *testing.T) {
	repo := newSQLiteRepo(t)
	prod := &fakeProducer{fails: 1 << 30} // 永远失败
	runner := NewOutboxRunner(repo, prod, nil)

	seedOrderWithEvent(t, repo, 201, "")

	// 连续补发到重试上限 → 放弃（status=2）
	for i := 0; i < outboxRetryCap; i++ {
		runner.flushOnce()
	}
	pending, err := repo.PendingOutbox(context.Background(), 10)
	if err != nil {
		t.Fatalf("PendingOutbox: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("after %d failed retries, want all abandoned, got %+v", outboxRetryCap, pending)
	}
}

// 回归用例：放弃名单只能统计**本轮发送失败**的事件。
//
// 此前 abandon 遍历的是整个 pending 而不是失败集合，于是一条已重试 cap-1 次、
// 本轮第 cap 次恰好发送成功的事件会因为 `9+1>=10` 被标成放弃 —— 下游其实
// 早就收到过了。人工按下游核对再重投，就会出现重复投递。
func TestOutboxAbandonOnlyFailedEvents(t *testing.T) {
	ctx := context.Background()
	repo := newSQLiteRepo(t)
	// A 已重试到 cap-1 次、本轮发送成功；B 本轮发送失败（key 由 OrderID 决定）
	seedOrderWithEvent(t, repo, 401, "")
	seedOrderWithEvent(t, repo, 402, "")

	pending, err := repo.PendingOutbox(ctx, 10)
	if err != nil {
		t.Fatalf("PendingOutbox: %v", err)
	}
	var idA, idB uint64
	for _, ev := range pending {
		parsed, perr := events.UnmarshalOrderCompleted([]byte(ev.Payload))
		if perr != nil {
			t.Fatalf("unmarshal payload: %v", perr)
		}
		switch parsed.OrderID {
		case 401:
			idA = ev.ID
		case 402:
			idB = ev.ID
		}
	}
	if idA == 0 || idB == 0 {
		t.Fatalf("seed events not found: idA=%d idB=%d", idA, idB)
	}

	// 把 A 推到「再失败一次就到上限」的临界状态（下游此时仍未收到）
	for i := 0; i < outboxRetryCap-1; i++ {
		if err := repo.MarkOutboxRetry(ctx, []uint64{idA}); err != nil {
			t.Fatalf("MarkOutboxRetry: %v", err)
		}
	}

	runner := NewOutboxRunner(repo, &fakeProducer{failKey: "402"}, nil)
	abandoned := runner.flushOnceResult()

	// 核心断言：发送成功的 A 绝不能出现在放弃名单里。
	// 这条只能靠返回值验证 —— DB 状态看不出来（status 守卫会把误判吞掉），
	// 日志也捞不到（logx 不提供 writer 注入）。
	for _, id := range abandoned {
		if id == idA {
			t.Fatalf("已发送成功的事件 A 被计入放弃名单 %v —— 运维据此核对会导致重复投递", abandoned)
		}
	}
	if len(abandoned) != 0 {
		t.Fatalf("abandon = %v, want empty（B 才第 1 次失败，远未到上限）", abandoned)
	}

	rest, err := repo.PendingOutbox(ctx, 10)
	if err != nil {
		t.Fatalf("PendingOutbox: %v", err)
	}
	var aStatus, bRetry int
	aRest, bRest := false, false
	for _, ev := range rest {
		switch ev.ID {
		case idA:
			aStatus, aRest = ev.Status, true
		case idB:
			bRetry, bRest = ev.RetryCount, true
		}
	}
	// A 已投递成功：既不能留在 pending，更不能变成放弃态（status=2）
	if aRest {
		t.Fatalf("已发送成功的事件 A 仍在待发队列（status=%d）：被误计入放弃名单会导致重复投递", aStatus)
	}
	// B 本轮失败且远未到上限：必须留在队列里等下一轮，计数 +1
	if !bRest {
		t.Fatal("事件 B 应仍在待发队列（未达重试上限，不该被放弃）")
	}
	if bRetry != 1 {
		t.Fatalf("事件 B 的 retry_count = %d, want 1（不应被提前放弃）", bRetry)
	}
}

// TestSendOneContinuesTrace：outbox 行内持久化的 traceparent 必须让 send span
// 挂回下单链路（回归 context.Background() 丢链、send/receive 分属两条 trace）。
func TestSendOneContinuesTrace(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(tp)
	// Extract 需要真实传播器：OTel 默认是 noop，不装则 traceparent 读不回来
	otel.SetTextMapPropagator(propagation.TraceContext{})

	r := NewOutboxRunner(&repository.OrderRepository{}, &fakeProducer{}, nil, true)

	const tid = "4bf92f3577b34da6a3ce929d0e0e4736"
	ev := &repository.OutboxEvent{
		Topic:       "order.completed.v1",
		Payload:     `{"order_id":1,"user_id":1,"schema_ver":1}`,
		Traceparent: "00-" + tid + "-00f067aa0ba902b7-01",
	}
	if err := r.sendOne(context.Background(), ev); err != nil {
		t.Fatalf("sendOne: %v", err)
	}
	spans := exporter.GetSpans()
	if len(spans) != 1 || spans[0].Name != "messaging.outbox.send" {
		t.Fatalf("expected messaging.outbox.send span, got %+v", spans)
	}
	if got := spans[0].SpanContext.TraceID().String(); got != tid {
		t.Fatalf("send span not continued from outbox traceparent: %s != %s", got, tid)
	}
}

// TestSendOneNoSpanWhenTraceOff：mq 开关关闭时既不 Extract 也不打 span
// （traceparent 为空已由写入侧保证，这里守的是「关了就别埋」）。
func TestSendOneNoSpanWhenTraceOff(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	r := NewOutboxRunner(&repository.OrderRepository{}, &fakeProducer{}, nil, false)
	ev := &repository.OutboxEvent{
		Topic:   "order.completed.v1",
		Payload: `{"order_id":1,"user_id":1,"schema_ver":1}`,
	}
	if err := r.sendOne(context.Background(), ev); err != nil {
		t.Fatalf("sendOne: %v", err)
	}
	if spans := exporter.GetSpans(); len(spans) != 0 {
		t.Fatalf("trace off: want no span, got %+v", spans)
	}
}

// 纯函数级：放弃名单的挑选规则（边界含 0 / cap-1 / 已超上限）
func TestAbandonCandidates(t *testing.T) {
	cases := []struct {
		name   string
		retry  int
		inWant bool
	}{
		{"首次失败", 0, false},
		{"临界前一档", outboxRetryCap - 2, false},
		{"本轮后刚好触顶", outboxRetryCap - 1, true},
		{"已超上限", outboxRetryCap, true},
	}
	failed := make([]repository.OutboxEvent, 0, len(cases))
	for i, c := range cases {
		failed = append(failed, repository.OutboxEvent{ID: uint64(i + 1), RetryCount: c.retry})
	}
	got := abandonCandidates(failed)
	gotSet := make(map[uint64]bool, len(got))
	for _, id := range got {
		gotSet[id] = true
	}
	for i, c := range cases {
		id := uint64(i + 1)
		if gotSet[id] != c.inWant {
			t.Fatalf("%s: id=%d inAbandon=%v, want %v", c.name, id, gotSet[id], c.inWant)
		}
	}
	if len(got) != 2 {
		t.Fatalf("abandon = %v (want 2 items)", got)
	}
}

func TestOutboxRunnerStartStopIdempotent(t *testing.T) {
	repo := newSQLiteRepo(t)
	// producer 为 nil → Start 不启动，Stop 幂等 no-op
	runner := NewOutboxRunner(repo, nil, nil)
	runner.Start()
	runner.Start()
	runner.Stop()
	runner.Stop()

	// 正常 producer：Start 起 goroutine，Stop 收敛（done 关闭）
	prod := &fakeProducer{}
	runner2 := NewOutboxRunner(repo, prod, nil)
	seedOrderWithEvent(t, repo, 301, "")
	runner2.Start()
	runner2.Start() // 幂等
	runner2.Stop()
	runner2.Stop()
}
