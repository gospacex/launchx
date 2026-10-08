// Package outbox 事务发件箱（三个示例共用一份）。
//
// 为什么抽成共享包而不是每个服务抄一份：所有服务都要「本地事务提交后
// 可靠投递事件」，语义完全一致（pending → sent / retry → abandon）。抄多份的
// 代价不是代码量，而是**语义漂移** —— 重试上限、空 payload 处理、轮询 ctx
// 是否入链这三条只要有一处写歪，故障表现就是「某个域的事件静默丢失」，
// 最难排查。
//
// 为什么它留在 examples/ 而不是上提进 launchx 或 mqx（2026-10-07 的结论）：
//
//   - launchx 是**库**，库源码有一条硬约束 ——
//     `dependency_surface_test.go` 的 TestLibrarySourceHasNoDirectMiddlewareImports
//     规定库源码不得直引 gorm / kitex / hertz / redis / kafka / consul 等具体
//     客户端，一律经 *_x SDK 取用。而本包整个就是对一个 gorm 表做簿记
//     （`*gorm.DB`、`gorm.Expr` 自增），放进库源码即违反这条约束（实测会被
//     守卫直接判失败）。
//   - mqx 是消息 SDK，不依赖 gorm；把 ORM 拖进消息层是同一个错误的反向版。
//   - dbx 与 mqx 谁都不单独拥有「事务发件箱」—— 它一半是「与业务行同事务的
//     落库」，一半是「MQ 的可靠投递」。这种跨域粘合是应用层的活。
//
// 于是把两份/多份副本合并成 examples 树内的一份，既消掉漂移，也不越界。
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
	"github.com/gospacex/otelx/layer"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/gorm"
)

// 事件状态
const (
	StatusPending = 0
	StatusSent    = 1
	StatusAbandon = 2
)

// 补发参数（沿用 hubx 既有纪律：3s ticker / 单轮 50 条 / 重试 10 次放弃）
const (
	DefaultPollInterval = 3 * time.Second
	DefaultBatchLimit   = 50
	DefaultRetryCap     = 10
)

// TraceHeaderKey 随事件一起落库的 traceparent 头名（幂等键之外的第二样东西）
const TraceHeaderKey = "traceparent"

// Event 事务发件箱行。各服务的库里都有一张同名表 outbox。
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
	// 事件时才恢复可追踪 —— 投递与簿记分别用什么 ctx 见下方 txCtx / bookCtx。
	ctx := orm.WithoutTracing(context.Background())
	pending, err := r.pending(ctx, r.cfg.BatchLimit)
	if err != nil {
		r.logger.ErrorContext(ctx, "outbox: list pending failed", logx.Err(err))
		return nil
	}
	if len(pending) == 0 {
		return nil
	}
	// 投递用可追踪 ctx（txCtx）：sendOne 内有 traceparent 时会 Extract 并把
	// send/kafka.send 挂回原下单链 —— 这是 outbox 与业务 trace 的唯一汇合点。
	// 簿记回写（markSent / markRetry / abandon）用免追踪 ctx（bookCtx）：它们是
	// outbox 表的内部状态迁移，不挂任何父 span，放出来只会各自成为根 —— 发送
	// 成功时是一根孤零零的 db.update，失败重试时更是每轮一根。空转免追踪防的
	// 是「没事件也产 span」，这里防的是「事件发完了，簿记痕迹在 Jaeger 上成了
	// 主角」。
	txCtx := context.Background()
	bookCtx := orm.WithoutTracing(context.Background())
	sent, retried := make([]uint64, 0, len(pending)), make([]uint64, 0)
	var failed []Event
	for i := range pending {
		ev := &pending[i]
		if ev.Payload == "" {
			// 事件体为空（上游 marshal 失败已记 warn）：直接放弃，不占用重试配额
			r.logger.WarnContext(txCtx, "outbox: empty payload, abandoning", logx.Int("id", int(ev.ID)))
			if err := r.abandon(bookCtx, []uint64{ev.ID}); err != nil {
				r.logger.ErrorContext(txCtx, "outbox: abandon empty-payload event failed", logx.Err(err))
			}
			continue
		}
		if err := r.sendOne(txCtx, ev); err != nil {
			r.logger.WarnContext(txCtx, "outbox: send failed, will retry",
				logx.Int("id", int(ev.ID)), logx.Err(err))
			retried = append(retried, ev.ID)
			failed = append(failed, *ev)
			continue
		}
		sent = append(sent, ev.ID)
	}
	if len(sent) > 0 {
		if err := r.markSent(bookCtx, sent); err != nil {
			r.logger.ErrorContext(txCtx, "outbox: mark sent failed", logx.Err(err))
		}
	}
	if len(retried) == 0 {
		return nil
	}
	if err := r.markRetry(bookCtx, retried); err != nil {
		r.logger.ErrorContext(txCtx, "outbox: mark retry failed", logx.Err(err))
		return nil
	}
	abandoned := AbandonCandidates(failed, r.cfg.RetryCap)
	if len(abandoned) > 0 {
		if err := r.abandon(bookCtx, abandoned); err != nil {
			r.logger.ErrorContext(txCtx, "outbox: abandon failed", logx.Err(err))
			return nil
		}
		r.logger.WarnContext(txCtx, "outbox: retry cap reached, abandoned for manual reconcile",
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

// sendOne 单条投递。续链分两段职责，别混在一起：
//
//  1. **还原上下文**（还原成本极低，且与开关无关）—— 只要落库时留了 traceparent
//     就 Extract。Extract 只往 ctx 里放一个 remote span context，不产生 span。
//  2. **开 outbox.send span** —— 这才是埋点，受 TraceOn 管；关掉时链路照样由第
//     1 步接上，只是少一层 hop。
//
// header 不再手写：producer.Send 内部会按 ctx 自动 Inject（mqx.InjectTrace），
// 所以这里只要保证 ctx 站对位置即可。轮询循环的 ctx 本身是干净的 root，若不做
// 第 1 步，这条链就断在 broker 这一跳。
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
		ctx = launchx.Extract(ctx,
			propagation.MapCarrier{TraceHeaderKey: ev.Traceparent})
	}
	if r.cfg.TraceOn && ev.Traceparent != "" {
		var span trace.Span
		ctx, span = layer.Start(ctx, "messaging", "outbox", "send")
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
