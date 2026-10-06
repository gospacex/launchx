// Package outbox 事务发件箱（mall 四服共用）。
//
// 为什么抽成共享包而不是每服抄一份：四个服务都要「本地事务提交后可靠投递事件」，
// 语义完全一致（pending → sent / retry → abandon）。抄四份的代价不是代码量，
// 而是**语义漂移** —— 重试上限、空 payload 处理、轮询 ctx 是否入链这三条
// 只要有一处写歪，故障表现就是"某个域的事件静默丢失"，最难排查。
//
// 落库侧用法（业务事务内，与业务行同事务）：
//
//	tx.Create(&outbox.Event{Topic: ..., EventType: ..., Payload: ..., Traceparent: ...})
//
// 补发侧用法（进程内单 goroutine）：
//
//	runner := outbox.NewRunner(db, app.MQ(), app.Logger(), outbox.Config{TraceOn: true})
//	runner.Start()
//	defer runner.Stop()
package outbox

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
	"gorm.io/gorm"

	malltrace "github.com/gospacex/launchx/examples/mall/pkg/tracex"
)

// 事件状态
const (
	StatusPending = 0
	StatusSent    = 1
	StatusAbandon = 2
)

// 补发参数（brief 沿用 hubx 既有纪律：3s ticker / 单轮 50 条 / 重试 10 次放弃）
const (
	DefaultPollInterval = 3 * time.Second
	DefaultBatchLimit   = 50
	DefaultRetryCap     = 10
)

// Event 事务发件箱行。四服各自的库里都有一张同名表 outbox。
type Event struct {
	ID          uint64    `gorm:"primaryKey;column:id"`
	Topic       string    `gorm:"column:topic"`
	EventType   string    `gorm:"column:event_type"`
	Payload     string    `gorm:"column:payload"`
	Traceparent string    `gorm:"column:traceparent"`
	Status      int       `gorm:"column:status"`
	RetryCount  int       `gorm:"column:retry_count"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

// TableName 显式表名
func (Event) TableName() string { return "outbox" }

// Config 补发器参数
type Config struct {
	// PollInterval 轮询间隔，<=0 取 DefaultPollInterval
	PollInterval time.Duration
	// BatchLimit 单轮补发上限（防积压时一次性打爆 broker），<=0 取 DefaultBatchLimit
	BatchLimit int
	// RetryCap 重试上限，超过即放弃（status=2，人工对账兜底），<=0 取 DefaultRetryCap
	RetryCap int
	// TraceOn 是否产 messaging span（对应配置 trace.on.mq）
	TraceOn bool
}

func (c Config) withDefaults() Config {
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultPollInterval
	}
	if c.BatchLimit <= 0 {
		c.BatchLimit = DefaultBatchLimit
	}
	if c.RetryCap <= 0 {
		c.RetryCap = DefaultRetryCap
	}
	return c
}

// Runner 事务发件箱补发器。
//
// producer 为 nil（MQ 未装配或降级）时**不启动补发并打 Error**：这种情况下
// 业务仍在持续写 status=0 的事件，却没有任何投递者，表会无声膨胀到爆 ——
// 静默跳过等于埋雷，必须留可观测痕迹。
type Runner struct {
	db       *gorm.DB
	producer mqx.Producer
	logger   logx.Logger
	cfg      Config

	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// NewRunner 构造补发器（producer 允许 nil，见类型注释）
func NewRunner(db *gorm.DB, producer mqx.Producer, logger logx.Logger, cfg Config) *Runner {
	if logger == nil {
		logger = logx.GetGlobal()
	}
	return &Runner{db: db, producer: producer, logger: logger, cfg: cfg.withDefaults()}
}

// Start 启动轮询补发（幂等；producer 为 nil 时不启动并打 Error）
func (r *Runner) Start() {
	r.once.Do(func() {
		if r.producer == nil {
			r.logger.Error("outbox: no producer, relay disabled - events will accumulate unflushed")
			return
		}
		r.stop = make(chan struct{})
		r.done = make(chan struct{})
		go r.loop()
	})
}

// Stop 停止补发（阻塞至本轮结束；未 Start 时 no-op）
func (r *Runner) Stop() {
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

func (r *Runner) loop() {
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			close(r.done)
			return
		case <-ticker.C:
			r.FlushOnce()
		}
	}
}

// FlushOnce 单轮补发（导出供单测直接驱动，避免依赖 ticker 时序）
func (r *Runner) FlushOnce() []uint64 {
	// 空转轮询用免追踪 ctx：轮询查询没有上游请求，若产 span 会每 3s 另起一根
	// 孤儿单 span trace（db.query），Jaeger 列表被噪声淹没。只有真拉到待发
	// 事件时才换回可追踪 ctx，让本轮投递以一个批次根 trace 入链。
	ctx := orm.WithoutTracing(context.Background())
	pending, err := r.pending(ctx, r.cfg.BatchLimit)
	if err != nil {
		r.logger.ErrorContext(ctx, "outbox: list pending failed", logx.Err(err))
		return nil
	}
	if len(pending) == 0 {
		return nil
	}
	ctx = context.Background()
	sent, retried := make([]uint64, 0, len(pending)), make([]uint64, 0)
	var failed []Event
	for i := range pending {
		ev := &pending[i]
		if ev.Payload == "" {
			// 事件体为空（上游 marshal 失败已记 warn）：直接放弃，不占用重试配额
			r.logger.WarnContext(ctx, "outbox: empty payload, abandoning", logx.Int("id", int(ev.ID)))
			if err := r.abandon(ctx, []uint64{ev.ID}); err != nil {
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
		if err := r.markSent(ctx, sent); err != nil {
			r.logger.ErrorContext(ctx, "outbox: mark sent failed", logx.Err(err))
		}
	}
	if len(retried) == 0 {
		return nil
	}
	if err := r.markRetry(ctx, retried); err != nil {
		r.logger.ErrorContext(ctx, "outbox: mark retry failed", logx.Err(err))
		return nil
	}
	abandoned := AbandonCandidates(failed, r.cfg.RetryCap)
	if len(abandoned) > 0 {
		if err := r.abandon(ctx, abandoned); err != nil {
			r.logger.ErrorContext(ctx, "outbox: abandon failed", logx.Err(err))
			return nil
		}
		r.logger.WarnContext(ctx, "outbox: retry cap reached, abandoned for manual reconcile",
			logx.Int("count", len(abandoned)))
	}
	return abandoned
}

// AbandonCandidates 从**本轮发送失败**的事件里挑出重试已达上限的那些。
//
// 入参刻意只接收失败集合。若遍历整个 pending，「重试 9 次、第 10 次刚好成功」
// 的事件会因 9+1>=cap 被一起塞进放弃名单。下层 mark*/abandon 都带
// `status = pending` 守卫，所以这个误判不会真的把行改成放弃态，但它会：
//  1. 打出与事实不符的 "abandoned count=N" 告警，运维按数对账会对不上；
//  2. 每轮多一次覆盖全表的 UPDATE；
//  3. 一旦守卫放宽，立刻从噪声升级为数据损坏。
//
// 独立纯函数是为了让这条语义能被单测直接钉住。
func AbandonCandidates(failed []Event, cap int) []uint64 {
	out := make([]uint64, 0, len(failed))
	for i := range failed {
		if failed[i].RetryCount+1 >= cap {
			out = append(out, failed[i].ID)
		}
	}
	return out
}

// sendOne 单条投递（traceparent 走 header；mq 关时为空串，不续链）
func (r *Runner) sendOne(ctx context.Context, ev *Event) error {
	key := fmt.Sprintf("%d", ev.ID)
	// Topic 刻意不写进消息体：producer 由 xmiddleware.mqx 构造时已绑定目标
	// topic，kafka-go 会拒绝 writer 与 message 同时带 topic（实测报
	// "Topic must not be specified for both Writer and Message"）。
	msg := &mqx.Message{
		Key:     key,
		Value:   []byte(ev.Payload),
		Headers: map[string]string{},
	}
	if ev.Traceparent != "" {
		msg.Headers[TraceHeaderKey] = ev.Traceparent
	}
	if r.cfg.TraceOn && ev.Traceparent != "" {
		ctx = launchx.Extract(ctx,
			propagation.MapCarrier{TraceHeaderKey: ev.Traceparent})
		var span trace.Span
		ctx, span = malltrace.Start(ctx, "messaging", "outbox", "send")
		defer span.End()
		span.SetAttributes(attribute.String("messaging.topic", ev.Topic))
	}
	return r.producer.Send(ctx, msg)
}

// pending 取待发事件（status=0）
func (r *Runner) pending(ctx context.Context, limit int) ([]Event, error) {
	var rows []Event
	err := r.db.WithContext(ctx).
		Where("status = ?", StatusPending).
		Order("id ASC").
		Limit(limit).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// markSent 标记已发（幂等：仅 status=0 的行可迁移到 1）
func (r *Runner) markSent(ctx context.Context, ids []uint64) error {
	if len(ids) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Model(&Event{}).
		Where("id IN ? AND status = ?", ids, StatusPending).
		Update("status", StatusSent).Error
}

// markRetry 补发失败：retry_count+1
func (r *Runner) markRetry(ctx context.Context, ids []uint64) error {
	if len(ids) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Model(&Event{}).
		Where("id IN ? AND status = ?", ids, StatusPending).
		UpdateColumn("retry_count", gorm.Expr("retry_count + 1")).Error
}

// abandon 重试超限：放弃（status=2，人工对账兜底）
func (r *Runner) abandon(ctx context.Context, ids []uint64) error {
	if len(ids) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Model(&Event{}).
		Where("id IN ? AND status = ?", ids, StatusPending).
		Update("status", StatusAbandon).Error
}

// TraceHeaderKey 复用 events 包的常量（避免两个包各写一份 trace header 键，
// 将来改一处漏一处）
const TraceHeaderKey = "traceparent"
