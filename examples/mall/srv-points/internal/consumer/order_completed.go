// Package consumer srv-points 的异步消费端：`order.completed.v1` → 送积分。
//
// 这是 mall 异步链路**第一次**有消费方。此前 pkg/events 只定义事件体、outbox
// 只负责把事件可靠地投出去，链路在 broker 这一跳就断了 —— 文档里"下单送积分"
// 是设计意图，不是已交付事实（事件包注释已如实改写）。
//
// 两条硬前提（事件包注释里点名过，这里落成代码）：
//  1. 消费组必须显式命名（`consumer.group_id: srv-points`）。mqx 缺省填
//     `mqx-group`，几个服务挤进同一组会互相抢分区，表现为"消息投出去了
//     但没人收到"。配置侧已写，判据见 order_completed_test.go。
//  2. 消费侧必须自带幂等。本包**不自己**去重 —— 重投时照旧调 Earn，由积分域的
//     `uk_ledger_order_no_type` 唯一键与 kvx 幂等键裁决，命中即返回
//     CodePointsDuplicated（由 Handle 视作成功）。自己再记一份"处理过的
//     OrderNo"等于把同一个事实存两处，两处迟早不一致。
//
// 与本层无关的传输细节：traceparent 由 mqx 消费侧统一 Extract 进 ctx
// （不进消息体，见 pkg/events 注释），本包因此不碰 header。
package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gospacex/logx"
	"github.com/gospacex/mqx"

	"github.com/gospacex/launchx/examples/mall/pkg/codes"
	"github.com/gospacex/launchx/examples/mall/pkg/events"
	"github.com/gospacex/launchx/examples/mall/srv-points/internal/model"
)

// PointsWriter 积分入账能力（service 层实现）。
//
// 用窄接口而不是 *service.PointsService：消费端只需要 Earn 一个动作，
// 面向接口才能在单测里注入替身（真入账要 DB + redis 锁）。
type PointsWriter interface {
	Earn(ctx context.Context, cmd model.EarnCmd) (*model.ChangeResult, error)
}

// Stats 消费计数（可观测性的最小面：没有它，"消费者活着但一条没处理"看不出来）。
type Stats struct {
	Processed int64 // 入账成功（含幂等命中）
	Skipped   int64 // 坏消息丢弃（重试一万次结果一样）
	Failed    int64 // 下游失败，已交给传输层重投
}

// Handler 处理 order.completed.v1。
type Handler struct {
	points PointsWriter
	logger logx.Logger

	processed int64
	skipped   int64
	failed    int64
}

// NewHandler 构造消费处理器。
func NewHandler(points PointsWriter, logger logx.Logger) *Handler {
	return &Handler{points: points, logger: logger}
}

// Stats 返回计数快照。
func (h *Handler) Stats() Stats {
	return Stats{
		Processed: atomic.LoadInt64(&h.processed),
		Skipped:   atomic.LoadInt64(&h.skipped),
		Failed:    atomic.LoadInt64(&h.failed),
	}
}

// Handle 处理一条消息。返回值决定这条消息的去向：
//
//	nil   —— 到此为止（入账成功，或判定为坏消息丢弃）；
//	error —— 可重试（下游抖动/写库失败），交给传输层决定是否重投。
//
// 坏消息**必须丢弃而不是返回 error**：一条解析不出来的消息如果每次都返回
// error，它会一直被重投、一直失败，把整个分区堵在原地（head-of-line blocking）
// —— 后面成千上万条好消息跟着一起不处理。没有 DLQ 时的取舍是"丢一条可对账
// 补偿的积分，也不让整条队列停摆"，故丢弃时打 Error 级日志并计数。
func (h *Handler) Handle(ctx context.Context, msg *mqx.Message) error {
	ev, why, ok := decode(msg)
	if !ok {
		atomic.AddInt64(&h.skipped, 1)
		h.error(ctx, "order.completed event dropped", logx.String("reason", why))
		return nil
	}

	res, err := h.points.Earn(ctx, model.EarnCmd{
		UserID:  uint64(ev.UserID),
		OrderNo: ev.OrderNo,
		Amount:  ev.Amount,
		Remark:  "order completed",
	})
	if err != nil {
		// 幂等命中视作成功：重投是常态（at-least-once），
		// 把它算成失败会让"正常重投"在计数上冒充故障。
		var ce *codes.Error
		if errors.As(err, &ce) && ce.Code == codes.CodePointsDuplicated {
			atomic.AddInt64(&h.processed, 1)
			return nil
		}
		if !retryable(err) {
			atomic.AddInt64(&h.skipped, 1)
			h.error(ctx, "order.completed rejected, dropping",
				logx.String("order_no", ev.OrderNo), logx.Err(err))
			return nil
		}
		atomic.AddInt64(&h.failed, 1)
		h.warn(ctx, "points earn failed, will retry",
			logx.String("order_no", ev.OrderNo), logx.Err(err))
		return err
	}
	atomic.AddInt64(&h.processed, 1)
	if h.logger != nil {
		h.logger.InfoContext(ctx, "points earned from order.completed",
			logx.String("order_no", ev.OrderNo),
			logx.Any("user_id", ev.UserID),
			logx.Any("amount", ev.Amount),
			logx.Any("balance_after", balanceOf(res)))
	}
	return nil
}

// decode 解析事件。返回 (事件, 丢弃原因, 是否可用)。
func decode(msg *mqx.Message) (events.OrderCompleted, string, bool) {
	var zero events.OrderCompleted
	if msg == nil {
		return zero, "nil message", false
	}
	if len(msg.Value) == 0 {
		// 空 payload 是 outbox 的正常形态之一（事件序列化失败时放弃该行），
		// 不是投递故障 —— 重试也不会变出内容。
		return zero, "empty payload", false
	}
	var ev events.OrderCompleted
	if err := json.Unmarshal(msg.Value, &ev); err != nil {
		return zero, "bad json: " + err.Error(), false
	}
	if ev.SchemaVer != events.SchemaVer1 {
		// 版本不认识：消费者落后于生产者。丢弃（并留 Error 日志）而不是
		// 返回 error —— 后者会让分区卡在一条自己读不懂的消息上。
		return zero, fmt.Sprintf("unsupported schema_ver %d (want %d)", ev.SchemaVer, events.SchemaVer1), false
	}
	if ev.OrderNo == "" {
		return zero, "missing order_no", false
	}
	if ev.UserID <= 0 {
		return zero, "missing user_id", false
	}
	if ev.Amount <= 0 {
		// 送 0 分或负分没有意义：前者空转一次事务，后者是生产者算错了。
		return zero, fmt.Sprintf("non-positive amount %d", ev.Amount), false
	}
	return ev, "", true
}

// retryable 判定失败是否值得重试。
//
// 业务拒绝（参数非法之类）重试一万次结果一样，只会把分区堵住；
// 其余（锁冲突、写库失败、下游不可用）都是**暂时**的，值得重投。
func retryable(err error) bool {
	var ce *codes.Error
	if !errors.As(err, &ce) {
		return true // 非业务错误按可重试处理（宁可多试，不要静默丢积分）
	}
	switch ce.Code {
	case codes.CodeInvalidParam, codes.CodeUnauthorized, codes.CodePointsAmountInvalid:
		return false
	}
	return true
}

// balanceOf 取出入账后的余额（res 可能为 nil，替身实现也可能不填）。
func balanceOf(res *model.ChangeResult) int64 {
	if res == nil {
		return 0
	}
	return res.BalanceAfter
}

// error / warn：logger 缺席（降级形态）时静默。
func (h *Handler) error(ctx context.Context, msg string, fields ...logx.Field) {
	if h.logger != nil {
		h.logger.ErrorContext(ctx, msg, fields...)
	}
}

func (h *Handler) warn(ctx context.Context, msg string, fields ...logx.Field) {
	if h.logger != nil {
		h.logger.WarnContext(ctx, msg, fields...)
	}
}

// ======================= 消费循环的启停 =======================

// Runner 把 mqx 传输与 Handler 接起来，并负责**有界**停止。
//
// 为什么单独有这一层：Handle 是纯逻辑（可注入替身单测），启停是传输侧的事。
// 混在一起就没法在不连 kafka 的前提下测"停不下来"这类问题。
type Runner struct {
	consumer mqx.Consumer
	handler  *Handler
	logger   logx.Logger

	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
	stopMu sync.Mutex
}

// NewRunner 构造消费循环。consumer 为 nil 时 Start 直接返回（不消费）。
func NewRunner(consumer mqx.Consumer, handler *Handler, logger logx.Logger) *Runner {
	return &Runner{consumer: consumer, handler: handler, logger: logger, done: make(chan struct{})}
}

// Start 起消费循环（非阻塞；Consume 本身是阻塞调用，故放进 goroutine）。
func (r *Runner) Start(ctx context.Context) {
	if r.consumer == nil {
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	r.stopMu.Lock()
	r.cancel = cancel
	r.stopMu.Unlock()

	go func() {
		defer close(r.done)
		// Consume 返回 error 只可能是 ctx 取消或驱动层故障；
		// 后者必须说出来，否则"消费者悄悄死了"没有任何痕迹。
		if err := r.consumer.Consume(runCtx, r.handler.Handle); err != nil &&
			!errors.Is(err, context.Canceled) {
			if r.logger != nil {
				r.logger.ErrorContext(ctx, "order.completed consumer stopped", logx.Err(err))
			}
		}
	}()
}

// Stop 有界停止：先 cancel，再等 done，超时即放弃等待。
//
// 为什么必须有界（2026-10-07 实测过的同款故障）：kafka-go 的 ConsumerGroup
// Close 会等 joinGroup 的网络读，而不设读 deadline —— 一个从没消费成功的
// consumer 句柄能让整个进程退不出去，在 K8s 上表现为优雅期耗尽被 SIGKILL。
// 与退出链 `runStop` 同口径：超时是已处理的降级，只记账不报错。
//
// 返回的 bool 表示是否在超时前收干净（false = 有一环被放弃了）。
func (r *Runner) Stop(timeout time.Duration) bool {
	if r.consumer == nil {
		return true
	}
	r.once.Do(func() {
		r.stopMu.Lock()
		cancel := r.cancel
		r.stopMu.Unlock()
		if cancel != nil {
			cancel()
		}
	})
	select {
	case <-r.done:
		return true
	case <-time.After(timeout):
		if r.logger != nil {
			r.logger.WarnContext(context.Background(), "order.completed consumer stop timed out",
				logx.String("timeout", timeout.String()),
				logx.String("consequence", "消费协程未显式退出，进程退出时由运行时回收"))
		}
		return false
	}
}
