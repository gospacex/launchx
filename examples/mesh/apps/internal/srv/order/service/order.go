// Package service ★ 唯一允许出现业务判断的层。
//
// 句柄在这里汇合：cache（详情缓存）、lock（同用户下单互斥）、mq（事件投递）。
// 全部按「缺失即降级 + 留日志」处理；db 是硬前提（app.Setup 已用 VerifyOrError 挡过）。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/gospacex/cachex"
	"github.com/gospacex/lockx"
	"github.com/gospacex/logx"
	"github.com/gospacex/mqx"
	"github.com/gospacex/otelx/layer"
	"gorm.io/gorm"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/model"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/events"
)

// orderRepo 是 service 真正依赖的窄接口：只含它调用的四个方法。
//
// 用接口而不是 *repository.OrderRepository：service 只关心「存/取/改」的语义，
// 不关心句柄从哪来；这也让业务判断能脱离真实库被直接驱动到（见 order_test.go）。
// *repository.OrderRepository 天然满足它，装配层无需额外适配。
type orderRepo interface {
	Create(ctx context.Context, o *model.Order, items []model.OrderItem) error
	FindByID(ctx context.Context, id uint64) (*model.Order, error)
	ItemsByOrderID(ctx context.Context, id uint64) ([]model.OrderItem, error)
	UpdateStatus(ctx context.Context, id uint64, from, to string) error
}

// OrderService 订单业务。
type OrderService struct {
	repo     orderRepo
	cache    cachex.Cache
	lock     lockx.Locker
	producer mqx.Producer
	logger   logx.Logger
	traceOn  bool // trace.enabled（总闸），由装配层注入
}

// New 注入全部句柄；cache / lock / producer 允许为 nil（各有降级路径）。
func New(repo orderRepo, cache cachex.Cache, lock lockx.Locker,
	producer mqx.Producer, logger logx.Logger, traceOn bool) *OrderService {
	return &OrderService{repo: repo, cache: cache, lock: lock, producer: producer,
		logger: logger, traceOn: traceOn}
}

// CreateOrderItem 下单明细（service 自己的入参类型，与 RPC 结构解耦）。
//
// 多明细是入口链路的真实形态（spec mesh-entry-chain 按「多明细下单」描述），
// 故入参是切片而非单条。
type CreateOrderItem struct {
	ProductID   uint64
	ProductName string
	Quantity    int
	UnitPrice   int64
}

// CreateOrderInput 下单入参（service 自己的入参类型，与 RPC 结构解耦）。
type CreateOrderInput struct {
	UserID uint64
	Items  []CreateOrderItem
}

// OrderView 查单出参。
type OrderView struct {
	Order  model.Order
	Items  []model.OrderItem
	Cached bool // 是否命中缓存，便于验收时判断缓存是否真的生效
}

// CreateOrder 下单：加锁 → 落库 → 投事件。
func (s *OrderService) CreateOrder(ctx context.Context, in CreateOrderInput) (id uint64, err error) {
	ctx, end := layer.Begin(ctx, "service", "order", "CreateOrder", s.traceOn, nil)
	defer func() { end(err) }()

	if len(in.Items) == 0 {
		return 0, codes.New(codes.InvalidArgument, "items must not be empty")
	}
	var total int64
	for _, it := range in.Items {
		if it.Quantity <= 0 {
			return 0, codes.New(codes.InvalidArgument, "quantity must be positive")
		}
		total += it.UnitPrice * int64(it.Quantity)
	}

	unlock, err := s.acquire(ctx, in.UserID)
	if err != nil {
		return 0, err
	}
	if unlock != nil {
		defer unlock()
	}

	o := &model.Order{
		UserID:      in.UserID,
		OrderNo:     fmt.Sprintf("M%s%06d", time.Now().Format("20060102150405"), in.UserID%1000000),
		Status:      model.StatusCreated,
		TotalAmount: total,
	}
	items := make([]model.OrderItem, 0, len(in.Items))
	for _, it := range in.Items {
		items = append(items, model.OrderItem{
			ProductID: it.ProductID, ProductName: it.ProductName,
			Quantity: it.Quantity, UnitPrice: it.UnitPrice,
		})
	}
	if err := s.repo.Create(ctx, o, items); err != nil {
		return 0, codes.Wrap(codes.OrderCreateFail, "create order", err)
	}

	s.publish(ctx, o)
	return o.ID, nil
}

// GetOrder 查单：先缓存，未命中回源并回填。
func (s *OrderService) GetOrder(ctx context.Context, id uint64) (v *OrderView, err error) {
	ctx, end := layer.Begin(ctx, "service", "order", "GetOrder", s.traceOn, nil)
	defer func() { end(err) }()

	if s.cache != nil {
		if raw, err := s.cache.Get(ctx, s.key(id)); err == nil && raw != "" {
			if o, ok := decodeOrder(raw); ok {
				items, _ := s.repo.ItemsByOrderID(ctx, id)
				return &OrderView{Order: o, Items: items, Cached: true}, nil
			}
		}
	}

	o, err := s.repo.FindByID(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, codes.New(codes.OrderNotFound, fmt.Sprintf("order %d not found", id))
		}
		return nil, codes.Wrap(codes.Internal, "get order", err)
	}
	items, err := s.repo.ItemsByOrderID(ctx, id)
	if err != nil {
		return nil, codes.Wrap(codes.Internal, "get order items", err)
	}
	s.fill(ctx, *o)
	return &OrderView{Order: *o, Items: items}, nil
}

// MarkPaid 改状态：库层条件更新 → 缓存失效。
func (s *OrderService) MarkPaid(ctx context.Context, id uint64) (err error) {
	ctx, end := layer.Begin(ctx, "service", "order", "MarkPaid", s.traceOn, nil)
	defer func() { end(err) }()

	if err := s.repo.UpdateStatus(ctx, id, model.StatusCreated, model.StatusPaid); err != nil {
		return codes.Wrap(codes.OrderCreateFail, "mark paid", err)
	}
	// 写后失效：顺序必须是「库成功 → 删缓存」。先删缓存再落库会让并发请求
	// 读到空后回填旧值。删除失败只告警：TTL 会兜底。
	if s.cache != nil {
		if err := s.cache.Delete(ctx, s.key(id)); err != nil {
			s.logger.Warn("mesh-order: cache invalidate failed, ttl will expire it",
				logx.String("key", s.key(id)), logx.Err(err))
		}
	}
	return nil
}

// acquire 取锁；返回 nil unlock 表示本次未走锁路径。
//
// 句柄缺失时 fail-open，**但必须说出来** —— 「锁没生效但没人报错」是超卖类
// 故障最难排查的形态。
func (s *OrderService) acquire(ctx context.Context, userID uint64) (func(), error) {
	if s.lock == nil {
		s.logger.Warn("mesh-order: lock unavailable, running without mutex",
			logx.String("consequence", "同用户并发下单不再互斥"),
			logx.String("probe", "检查 xmiddleware.lockx 段"))
		return nil, nil
	}
	lk, err := s.lock.Lock(ctx, "order:user:"+strconv.FormatUint(userID, 10))
	if err != nil {
		return nil, codes.Wrap(codes.Internal, "acquire lock", err)
	}
	return func() {
		if rerr := lk.Unlock(ctx); rerr != nil {
			s.logger.Warn("mesh-order: unlock failed, ttl will expire it", logx.Err(rerr))
		}
	}, nil
}

// publish 投递订单完成事件。失败只告警：本例不带 outbox 表，
// 投递失败由上层重试；真实系统这里必须落 outbox（见 apps/README「删不掉的四类」）。
func (s *OrderService) publish(ctx context.Context, o *model.Order) {
	if s.producer == nil {
		s.logger.Warn("mesh-order: mq producer unavailable, event not published",
			logx.String("order_no", o.OrderNo),
			logx.String("consequence", "积分域收不到该订单"))
		return
	}
	payload, err := events.OrderCompleted{
		OrderID: o.ID, UserID: o.UserID, TotalAmount: o.TotalAmount, OccurredAt: time.Now(),
	}.Marshal()
	if err != nil {
		s.logger.Warn("mesh-order: marshal event failed", logx.Err(err))
		return
	}
	// mqx.Producer.Send 收的是 *mqx.Message（topic 是消息字段，不是位置参数）。
	if err := s.producer.Send(ctx, &mqx.Message{
		Topic: events.TopicOrderCompleted,
		Value: payload,
	}); err != nil {
		s.logger.Warn("mesh-order: publish event failed", logx.Err(err))
	}
}

func (s *OrderService) key(id uint64) string {
	return "mesh:order:detail:" + strconv.FormatUint(id, 10)
}

// decodeOrder 只从缓存解出订单主表；明细仍回源（明细变更频率高于主表）。
//
// cachex 的取值是 string，故这里收 string 而不是 []byte。
func decodeOrder(raw string) (model.Order, bool) {
	var o model.Order
	if err := json.Unmarshal([]byte(raw), &o); err != nil || o.ID == 0 {
		return model.Order{}, false
	}
	return o, true
}

// fill 回填缓存，失败只告警（缓存是加速器，不该影响请求成败）。
func (s *OrderService) fill(ctx context.Context, o model.Order) {
	if s.cache == nil {
		return
	}
	b, err := json.Marshal(o)
	if err != nil {
		return
	}
	if err := s.cache.Set(ctx, s.key(o.ID), string(b), 5*time.Minute); err != nil {
		s.logger.Warn("mesh-order: cache fill failed", logx.Err(err))
	}
}
