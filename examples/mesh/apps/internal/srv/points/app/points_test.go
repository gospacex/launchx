// app 层单测：用假 mqx.Consumer 驱动「消费一条 order.completed.v1 → 积分落库
// → 四层 span 齐」的整条链。
//
// 这里做的是**单元级端到端**：真起 srv-order + srv-points 查 MySQL / Jaeger
// 的运行期校验归 Task 15（apps/cmd、apps/conf 是 Task 8 的产物）。本层把
// app.Setup 里那段消费闭包抽成 consumeHandler，就是为了让它能被直接驱动到 ——
// app.Setup 依赖 *launchx.App，外部包构造不出来。
package app

import (
	"context"
	"errors"
	"testing"

	"github.com/gospacex/logx"
	"github.com/gospacex/mqx"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/points/model"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/points/repository"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/points/service"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/events"
)

// fakeRepo 只满足 service 依赖的（未导出）仓储窄接口：用它可以精确制导
// service 的出错路径，把「可恢复失败」与「确定性失败」分别送到 consumeHandler。
type fakeRepo struct {
	creditErr error
}

func (f *fakeRepo) CreditOnce(context.Context, uint64, uint64, int64) (bool, error) {
	if f.creditErr != nil {
		return false, f.creditErr
	}
	return true, nil
}
func (f *fakeRepo) Balance(context.Context, uint64) (*model.PointAccount, error) {
	return nil, nil
}

// nopLogger 内嵌接口 + 覆写 Warn/Error：消费链只在这两条路径上用日志。
type nopLogger struct{ logx.Logger }

func (nopLogger) Warn(string, ...logx.Field)  {}
func (nopLogger) Error(string, ...logx.Field) {}

// fakeConsumer 用内存消息驱动 handler：对 queued 的每条消息调一次 handler，
// 复刻 mqx.Consumer.Consume 的「阻塞消费」语义；handler 返回的 error 收集起来
// 供断言（真实驱动会据此重投）。
type fakeConsumer struct {
	mqx.Consumer
	msgs     []*mqx.Message
	returned []error
}

func (c *fakeConsumer) Consume(ctx context.Context, handler func(context.Context, *mqx.Message) error) error {
	for _, m := range c.msgs {
		if err := handler(ctx, m); err != nil {
			c.returned = append(c.returned, err)
		}
	}
	return nil
}

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.PointAccount{}, &model.PointsLog{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

func mustPayload(t *testing.T, e events.OrderCompleted) []byte {
	t.Helper()
	b, err := e.Marshal()
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return b
}

// ---------- 1. 落库 ----------

// 消费一条合法订单完成事件 → points_log 新增一行（order_id=42）且账户余额增加。
func TestConsumeLoopPersistsLedger(t *testing.T) {
	db := newTestDB(t)
	svc := service.New(repository.New(db, false), nopLogger{}, false)

	c := &fakeConsumer{msgs: []*mqx.Message{{
		Topic: events.TopicOrderCompleted,
		Value: mustPayload(t, events.OrderCompleted{OrderID: 42, UserID: 9, TotalAmount: 100}),
	}}}
	if err := c.Consume(context.Background(), consumeHandler(svc, nopLogger{}, false)); err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if len(c.returned) != 0 {
		t.Fatalf("合法载荷不应回 error（会被重投），实际 %v", c.returned)
	}

	var logs []model.PointsLog
	if err := db.Where("order_id = ?", 42).Find(&logs).Error; err != nil {
		t.Fatalf("read points_log: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("points_log 行数 = %d, want 1", len(logs))
	}
	if logs[0].UserID != 9 || logs[0].Delta != 100 {
		t.Fatalf("points_log = %+v", logs[0])
	}

	var acc model.PointAccount
	if err := db.Where("user_id = ?", 9).First(&acc).Error; err != nil {
		t.Fatalf("read points_account: %v", err)
	}
	if acc.Balance != 100 {
		t.Fatalf("balance = %d, want 100", acc.Balance)
	}
}

// ---------- 2. 坏载荷不重投 ----------

// 喂 `{}`（order_id=0，UnmarshalOrderCompleted 会报错）→ handler 返回 nil
// （不重投，否则无限循环），且库内无新行。
func TestConsumeBadPayloadNotRetried(t *testing.T) {
	db := newTestDB(t)
	svc := service.New(repository.New(db, false), nopLogger{}, false)
	h := consumeHandler(svc, nopLogger{}, false)

	err := h(context.Background(), &mqx.Message{Value: []byte(`{}`)})
	if err != nil {
		t.Fatalf("坏载荷必须回 nil（不重投），实际 %v", err)
	}

	var logs, accs int64
	if err := db.Model(&model.PointsLog{}).Count(&logs).Error; err != nil {
		t.Fatalf("count points_log: %v", err)
	}
	if err := db.Model(&model.PointAccount{}).Count(&accs).Error; err != nil {
		t.Fatalf("count points_account: %v", err)
	}
	if logs != 0 || accs != 0 {
		t.Fatalf("坏载荷不得落任何行：logs=%d accounts=%d", logs, accs)
	}
}

// 非法 JSON 同样跳过（同上）。
func TestConsumeMalformedJSONNotRetried(t *testing.T) {
	svc := service.New(repository.New(newTestDB(t), false), nopLogger{}, false)
	h := consumeHandler(svc, nopLogger{}, false)
	if err := h(context.Background(), &mqx.Message{Value: []byte(`not json`)}); err != nil {
		t.Fatalf("非法 JSON 必须回 nil（不重投），实际 %v", err)
	}
}

// ---------- 2b. 错误分类：确定性失败吞掉、可恢复失败外抛 ----------

// 确定性失败（total_amount ≤ 0 → service 回 codes.InvalidArgument）必须被
// consumeHandler 吞掉（返回 nil）。这类错误重投一万次也是同一个结果，
// 交给驱动重投即毒丸：永远失败、永远重投，把队列卡死。
func TestConsumeNonRetryableDropped(t *testing.T) {
	svc := service.New(&fakeRepo{}, nopLogger{}, false)
	h := consumeHandler(svc, nopLogger{}, false)

	// TotalAmount=0 能过 Unmarshal（order_id 非零），但 service 判为非法。
	err := h(context.Background(), &mqx.Message{
		Value: mustPayload(t, events.OrderCompleted{OrderID: 42, UserID: 9, TotalAmount: 0}),
	})
	if err != nil {
		t.Fatalf("确定性失败（InvalidArgument）必须回 nil（不重投，否则毒丸），实际 %v", err)
	}
}

// 可恢复失败（repo 报 Internal，如 DB 抖动）必须原样外抛，交给驱动按策略重投。
func TestConsumeRecoverableFailureReturnsError(t *testing.T) {
	repo := &fakeRepo{creditErr: codes.Wrap(codes.Internal, "add balance", errors.New("db down"))}
	svc := service.New(repo, nopLogger{}, false)
	h := consumeHandler(svc, nopLogger{}, false)

	err := h(context.Background(), &mqx.Message{
		Value: mustPayload(t, events.OrderCompleted{OrderID: 42, UserID: 9, TotalAmount: 100}),
	})
	if err == nil {
		t.Fatal("可恢复失败必须回 error（交给驱动重投），实际 nil")
	}
	if c, _ := codes.From(err); c != codes.Internal {
		t.Fatalf("可恢复失败应保留 Internal 码，实际 %v (err=%v)", c, err)
	}
}

// ---------- 3. span 父子链 ----------

// 消费链的 span 父子链：
//
//	messaging.points.Consume → service.points.ConsumeOrderCompleted →
//	repository.points.CreditOnce → repository.points.AddLog / AddBalance
//
// 判据是「每个子 span 的 Parent() 是上一层」—— 少了这条，各层各自成根也能通过
// 「名字都在」的检查。根是 **messaging.points.Consume**（2026-10-10 加固补的
// 消费侧应用 span，替代 SDK 侧缺失的消费 driver span），不再是 service 层。
func TestConsumeChainSpanParentChild(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})

	svc := service.New(repository.New(newTestDB(t), true), nopLogger{}, true)
	h := consumeHandler(svc, nopLogger{}, true)
	if err := h(context.Background(), &mqx.Message{
		Value: mustPayload(t, events.OrderCompleted{OrderID: 42, UserID: 9, TotalAmount: 100}),
	}); err != nil {
		t.Fatalf("consume: %v", err)
	}

	seen := make(map[string]sdktrace.ReadOnlySpan, len(sr.Ended()))
	names := make([]string, 0, len(sr.Ended()))
	for _, sp := range sr.Ended() {
		seen[sp.Name()] = sp
		names = append(names, sp.Name())
	}

	chain := []string{
		"messaging.points.Consume",
		"service.points.ConsumeOrderCompleted",
		"repository.points.CreditOnce",
		"repository.points.AddLog",
	}
	root, ok := seen[chain[0]]
	if !ok {
		t.Fatalf("缺少根 span %q，实际=%v", chain[0], names)
	}
	if root.Parent().IsValid() {
		t.Fatalf("消费侧根 span 不应有父，实际父=%s", root.Parent().SpanID())
	}
	traceID := root.SpanContext().TraceID()
	for i := 1; i < len(chain); i++ {
		parent, ok := seen[chain[i-1]]
		if !ok {
			t.Fatalf("缺少 span %q，实际=%v", chain[i-1], names)
		}
		child, ok := seen[chain[i]]
		if !ok {
			t.Fatalf("缺少 span %q，实际=%v", chain[i], names)
		}
		if got, want := child.Parent().SpanID(), parent.SpanContext().SpanID(); got != want {
			t.Fatalf("%s 的父 span 应为 %s：child.Parent=%s，parent.SpanID=%s（spans=%v）",
				chain[i], chain[i-1], got, want, names)
		}
		if got, want := child.SpanContext().TraceID(), traceID; got != want {
			t.Fatalf("%s 的 TraceID 应与根相同：%s != %s", chain[i], got, want)
		}
	}
	// AddBalance 的父是 CreditOnce（同一事务里的第二个写语句），仍在同一条链上。
	bal, ok := seen["repository.points.AddBalance"]
	if !ok {
		t.Fatalf("缺少 span repository.points.AddBalance，实际=%v", names)
	}
	if got, want := bal.Parent().SpanID(), seen["repository.points.CreditOnce"].SpanContext().SpanID(); got != want {
		t.Fatalf("AddBalance 的父应为 CreditOnce：child.Parent=%s，want=%s（spans=%v）", got, want, names)
	}
}
