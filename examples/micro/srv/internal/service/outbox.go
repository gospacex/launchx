// Package service srv-order 业务编排层（outbox 补发器）。
//
// OutboxRunner 事务发件箱补发：3s ticker 拉 status=0 的待发事件经
// mqx 生产者发出；发送成功标记 sent（0→1，WHERE status=0 守卫防重复
// 迁移），失败 retry_count+1，超 outboxRetryCap 转放弃（status=2，
// 人工对账兜底）。traceparent 随 mqx Message.Headers[events.TraceHeaderKey]
// 透传（批 2 已核实：mqx 无独立 Header 类型，header 走 Message.Headers map）。
//
// producer 为 nil（MQ 装配失败）时不启动补发，Stop 幂等。启动前会显式
// WARN —— 这是唯一提示「outbox 会无限堆积」的地方，静默跳过等于埋雷。
package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gospacex/dbx/orm"
	"github.com/gospacex/launchx"
	"github.com/gospacex/logx"
	"github.com/gospacex/mqx"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/gospacex/launchx/examples/micro/srv/internal/repository"
	"github.com/gospacex/launchx/examples/micro/srv/internal/tracex"
	"github.com/gospacex/launchx/examples/micro/srv/pkg/events"
)

// outboxPollInterval 3s 轮询补发间隔（brief §srv-order）
const outboxPollInterval = 3 * time.Second

// outboxBatchLimit 单轮补发上限（防积压时一次性打爆 broker）
const outboxBatchLimit = 50

// outboxRetryCap 补发重试上限：超过即放弃（status=2，人工对账兜底）
const outboxRetryCap = 10

// OutboxRunner 事务发件箱补发器
type OutboxRunner struct {
	repo     *repository.OrderRepository
	producer mqx.Producer
	logger   logx.Logger
	// traceOn 语义 = config Trace.On("mq")：mq 不入链时不打 messaging span。
	traceOn bool
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
}

// NewOutboxRunner 构造（producer 允许 nil：MQ 装配失败时由调用方传 nil）
//
// traceOn 为可变参：调用点（app/setup.go）传 trace.on.mq，缺省取
// true——mq 关闭时写入侧落库的 traceparent 必为空串，sendOne 的续链分支
// 本就不会进入，缺省值不会破「mq 关则无 span」的语义。
func NewOutboxRunner(repo *repository.OrderRepository, producer mqx.Producer, logger logx.Logger, traceOn ...bool) *OutboxRunner {
	if logger == nil {
		logger = logx.GetGlobal()
	}
	on := true
	for _, v := range traceOn {
		on = v
	}
	return &OutboxRunner{repo: repo, producer: producer, logger: logger, traceOn: on}
}

// Start 启动 3s ticker 补发（幂等：重复 Start 忽略；producer 为 nil 不启动）
func (r *OutboxRunner) Start() {
	r.once.Do(func() {
		if r.producer == nil {
			// 不能静默 return：这种情况下下单仍在持续写 status=0 的事件，
			// 却没有任何投递者，表会无声膨胀到爆。这里必须留一条可观测痕迹。
			r.logger.Error("outbox: no producer, relay disabled — events will accumulate unflushed")
			return
		}
		r.stop = make(chan struct{})
		r.done = make(chan struct{})
		go r.loop()
	})
}

// Stop 停止补发（优雅退出调用；阻塞至本轮结束；未 Start 时 no-op）
func (r *OutboxRunner) Stop() {
	if r.stop == nil {
		return
	}
	select {
	case <-r.done:
		return
	default:
		close(r.stop)
		<-r.done
	}
}

func (r *OutboxRunner) loop() {
	ticker := time.NewTicker(outboxPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			close(r.done)
			return
		case <-ticker.C:
			r.flushOnce()
		}
	}
}

// flushOnce 单轮补发（导出供单测直接驱动，避免依赖 3s ticker 等待）
func (r *OutboxRunner) flushOnce() {
	r.flushOnceResult()
}

// flushOnceResult 在 flushOnce 之上返回本轮判定为「放弃」的事件 ID。
//
// 返回值是为可测性专门加的：这条信息既落不进 DB 状态（发送成功的事件会被
// MarkOutboxSent 先推走，Abandon 的 status 守卫让误判看不出痕迹），也捞不到
// 日志（logx 不暴露 writer 注入）。没有返回值就只能靠读 stdout 才能验证。
func (r *OutboxRunner) flushOnceResult() []uint64 {
	// 空转轮询用免追踪 ctx：轮询查询没有上游请求，若产 span 会每 3s 另起一根
	// 孤儿单 span trace（db.query），Jaeger 列表被噪声淹没，真实请求的 trace
	// 反而翻不到。只有真拉到待发事件时才换回可追踪 ctx，让本轮补发工作
	// （投递 / 标记 / 重试）以一个批次根 trace 入链，保留可观测性。
	ctx := orm.WithoutTracing(context.Background())
	pending, err := r.repo.PendingOutbox(ctx, outboxBatchLimit)
	if err != nil {
		r.logger.ErrorContext(ctx, "outbox: list pending failed", logx.Err(err))
		return nil
	}
	if len(pending) == 0 {
		return nil
	}
	ctx = context.Background()
	sent, retried := []uint64{}, []uint64{}
	var failed []repository.OutboxEvent
	for i := range pending {
		ev := &pending[i]
		if ev.Payload == "" {
			// 事件体为空（下单主流程 marshal 失败已记 warn）：直接放弃
			r.logger.WarnContext(ctx, "outbox: empty payload, abandoning", logx.Int("id", int(ev.ID)))
			if err := r.repo.AbandonOutbox(ctx, []uint64{ev.ID}); err != nil {
				r.logger.ErrorContext(ctx, "outbox: abandon empty-payload event failed", logx.Err(err))
			}
			continue
		}
		if err := r.sendOne(ctx, ev); err != nil {
			r.logger.WarnContext(ctx, "outbox: send failed, will retry",
				logx.Int("id", int(ev.ID)), logx.Err(err))
			retried = append(retried, ev.ID)
			failed = append(failed, *ev)
			continue
		}
		sent = append(sent, ev.ID)
	}
	if len(sent) > 0 {
		if err := r.repo.MarkOutboxSent(ctx, sent); err != nil {
			r.logger.ErrorContext(ctx, "outbox: mark sent failed", logx.Err(err))
		}
	}
	// 重试计数 +1；重试已达上限的那部分转放弃（status=2）
	if len(retried) > 0 {
		if err := r.repo.MarkOutboxRetry(ctx, retried); err != nil {
			r.logger.ErrorContext(ctx, "outbox: mark retry failed", logx.Err(err))
			return nil
		}
		abandon := abandonCandidates(failed)
		if len(abandon) > 0 {
			if err := r.repo.AbandonOutbox(ctx, abandon); err != nil {
				r.logger.ErrorContext(ctx, "outbox: abandon failed", logx.Err(err))
				return nil
			}
			r.logger.WarnContext(ctx, "outbox: retry cap reached, abandoned for manual reconcile",
				logx.Int("count", len(abandon)))
		}
		return abandon
	}
	return nil
}

// abandonCandidates 从**本轮发送失败**的事件里挑出重试已达上限的那些。
//
// 入参刻意只接收失败集合。此前这里遍历的是整个 pending，于是「重试 9 次、
// 第 10 次刚好成功」的事件会因 9+1>=cap 被一起塞进放弃名单。
// 下层 Mark* / Abandon 都带 `status = pending` 守卫，所以这个误判**没有**
// 真的把行改成放弃态（先执行的 MarkOutboxSent 已经把它推走），但它带来三个
// 实打实的后果：
//  1. 误导告警：打出 "abandoned for manual reconcile count=N"，N 里混着投递
//     成功的事件，运维按数去查会对不上账；
//  2. 每轮多一次覆盖全表的 UPDATE；
//  3. 一旦守卫放宽（比如给 Abandon 加"允许重试后重投"的通道），
//     这里立刻从噪声升级为数据损坏。
//
// 抽成独立纯函数是为了让这条语义能被单测直接钉住，不必依赖 3s ticker 的时序。
func abandonCandidates(failed []repository.OutboxEvent) []uint64 {
	out := make([]uint64, 0, len(failed))
	for i := range failed {
		if failed[i].RetryCount+1 >= outboxRetryCap {
			out = append(out, failed[i].ID)
		}
	}
	return out
}

// sendOne 单条事件发送（topic 来自 outbox 行；traceparent 走 header，
// 仅当写入时非空才携带——mq 关时为空串，与 trace 总闸语义一致）
func (r *OutboxRunner) sendOne(ctx context.Context, ev *repository.OutboxEvent) error {
	// key 取事件内嵌的订单 ID（消费端按订单分区去重）；
	// 解析失败回退 outbox.id（顺序语义不变）
	key := fmt.Sprintf("%d", ev.ID)
	if parsed, err := events.UnmarshalOrderCompleted([]byte(ev.Payload)); err == nil && parsed.OrderID > 0 {
		key = fmt.Sprintf("%d", parsed.OrderID)
	}
	// Topic 刻意留空：producer 由 xmiddleware.mqx 配的目标 topic 构造（writer 自带
	// topic），kafka-go 会拒绝 writer 与 message 同时带 topic
	// ——2026-10-05 实测报 "Topic must not be specified for both Writer and Message"。
	// 目标 topic 只作为 span 属性携带（下面 SetAttributes），不再写进消息体。
	msg := &mqx.Message{
		Key:     key,
		Value:   []byte(ev.Payload),
		Headers: map[string]string{},
	}
	if ev.Traceparent != "" {
		msg.Headers[events.TraceHeaderKey] = ev.Traceparent
	}
	// messaging 续链：mq 开关开时，从 outbox 行内持久化的 traceparent 重建
	// 下单链路 ctx，send span 挂回原 trace（send/receive 同 trace 的断言锚点）；
	// mq 关时（traceparent 必为空串）退化为无 span 的裸发送。
	if r.traceOn && ev.Traceparent != "" {
		ctx = launchx.Extract(ctx,
			propagation.MapCarrier{events.TraceHeaderKey: ev.Traceparent})
		var span trace.Span
		ctx, span = tracex.Start(ctx, "messaging", "outbox", "send")
		defer span.End()
		span.SetAttributes(attribute.String("messaging.topic", ev.Topic))
	}
	return r.producer.Send(ctx, msg)
}
