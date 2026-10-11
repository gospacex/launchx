// Package service ★ 唯一允许出现业务判断的层。
//
// 句柄在这里汇合：cache（详情缓存）、lock（同用户下单互斥）。
// 全部按「缺失即降级 + 留日志」处理；db 是硬前提（app.Setup 已用 VerifyOrError 挡过）。
//
// 事件投递不走本层：2026-10-10 起下单事件与业务行同事务落 outbox 表，
// 由 app.Setup 装配的 outbox.Runner 后台补发（此前事务外直发 producer，
// 「订单提交成功、事件丢失」只值一条 Warn —— 那是不能接受的一致性缺口）。
package service

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/gospacex/cachex"
	"github.com/gospacex/lockx"
	"github.com/gospacex/logx"
	"github.com/gospacex/otelx/layer"
	"go.opentelemetry.io/otel/attribute"
	"gorm.io/gorm"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/model"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/events"
	"github.com/gospacex/launchx/examples/pkg/cacheutil"
	"github.com/gospacex/launchx/examples/pkg/traceparent"
)

// orderCacheTTL 订单主表缓存存活时间。
//
// 原先这个值写在 fill 里（`5*time.Minute`），读策略收口后必须提到常量：
// cacheutil 要它作为 Options.TTL，而 fill 已不存在。
const orderCacheTTL = 5 * time.Minute

// orderRepo 是 service 真正依赖的窄接口：只含它调用的四个方法。
//
// 用接口而不是 *repository.OrderRepository：service 只关心「存/取/改」的语义，
// 不关心句柄从哪来；这也让业务判断能脱离真实库被直接驱动到（见 order_test.go）。
// *repository.OrderRepository 天然满足它，装配层无需额外适配。
type orderRepo interface {
	// Create 的第三参是 outbox 事件回调（事务内、订单 ID 已知后调用），
	// nil 时 outbox 行 payload 为空、由 Runner 放弃该行。
	// 返回 Idempotent=true 表示该 order_no 已存在、本次未插任何行。
	Create(ctx context.Context, o *model.Order, items []model.OrderItem,
		fill func(orderID uint64) (payload, traceparent string)) (model.CreateOutput, error)
	FindByID(ctx context.Context, id uint64) (*model.Order, error)
	ItemsByOrderID(ctx context.Context, id uint64) ([]model.OrderItem, error)
	UpdateStatus(ctx context.Context, id uint64, from, to string) error
}

// OrderService 订单业务。
type OrderService struct {
	repo    orderRepo
	cache   cachex.Cache
	lock    lockx.Locker
	logger  logx.Logger
	traceOn bool // trace.enabled（总闸），由装配层注入
}

// New 注入全部句柄；cache / lock 允许为 nil（各有降级路径）。
// MQ 生产者不再是本层依赖：事件经 outbox 表由 Runner 投递（见包注释）。
func New(repo orderRepo, cache cachex.Cache, lock lockx.Locker,
	logger logx.Logger, traceOn bool) *OrderService {
	return &OrderService{repo: repo, cache: cache, lock: lock,
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
	// OrderNo 幂等键：由入口（bff）生成并贯穿调用链。
	//
	// 为空表示调用方没给（老客户端 / 直连 srv 的调用），此时由本层生成并打
	// `order.idempotent_key_missing=true` —— 那种情况下重放**无法**被识别，
	// 留痕是为了让它在 Jaeger 上能被查出来，而不是静默退化。
	OrderNo string
}

// OrderView 查单出参。
type OrderView struct {
	Order  model.Order
	Items  []model.OrderItem
	Cached bool // 是否命中缓存，便于验收时判断缓存是否真的生效
}

// CreateOrderResult 下单结果：**创建时就已经持有**的那几个字段。
//
// 让 CreateOrder 直接把它们交出来，是为了消掉一次无谓的回读：此前 handler 在
// 创建成功后又调 GetOrder(id) 去填响应，等于白多两次 SQL（FindByID +
// ItemsByOrderID）加一次缓存回填，而它要的 order_no / total_amount 在创建那一刻
// 就在手里（见 2026-10-10 code review §5.8）。顺带把「读路径」留给真正的读请求。
//
// 只有三个字段、没有 Items：创建响应用不到明细，把 Items 也带出来等于让 service
// 去关心「响应长什么样」——那是 handler 的事。
type CreateOrderResult struct {
	ID          uint64
	OrderNo     string
	TotalAmount int64
	// Idempotent=true 表示这是一次**重放命中**：返回的是既有订单，本次没有
	// 新建任何行。调用方（bff）据此可以把它渲成成功而不是错误。
	Idempotent bool
}

// CreateOrder 下单：加锁 → 落库（订单 + 明细 + 流转日志 + outbox 事件，同一事务）。
// 事件投递由 outbox.Runner 异步补发，不在请求路径上。
func (s *OrderService) CreateOrder(ctx context.Context, in CreateOrderInput) (res CreateOrderResult, err error) {
	ctx, end := layer.Begin(ctx, "service", "order", "CreateOrder", s.traceOn, nil)
	defer func() { end(err) }()

	if len(in.Items) == 0 {
		return CreateOrderResult{}, codes.New(codes.InvalidArgument, "items must not be empty")
	}
	var total int64
	for _, it := range in.Items {
		if it.Quantity <= 0 {
			return CreateOrderResult{}, codes.New(codes.InvalidArgument, "quantity must be positive")
		}
		total += it.UnitPrice * int64(it.Quantity)
	}

	unlock, err := s.acquire(ctx, in.UserID)
	if err != nil {
		return CreateOrderResult{}, err
	}
	if unlock != nil {
		defer unlock()
	}

	o := &model.Order{
		UserID:      in.UserID,
		OrderNo:     s.resolveOrderNo(ctx, in.OrderNo),
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
	// traceparent 在事务外取好：它只依赖 ctx 里的 span context，与库无关；
	// fill 在事务内被回调时直接闭包引用。
	tp := traceparent.Of(ctx, s.traceOn)
	out, err := s.repo.Create(ctx, o, items, func(orderID uint64) (string, string) {
		payload, mErr := events.OrderCompleted{
			OrderID: orderID, UserID: in.UserID, TotalAmount: total, OccurredAt: time.Now(),
		}.Marshal()
		if mErr != nil {
			// 序列化失败不阻断下单主流程：空 payload 的行 Runner 直接放弃。
			s.logger.Warn("mesh-order: marshal event failed", logx.Err(mErr))
			return "", tp
		}
		return string(payload), tp
	})
	if err != nil {
		return CreateOrderResult{}, codes.Wrap(codes.OrderCreateFail, "create order", err)
	}
	if out.Idempotent {
		// 重放命中：既有订单原样返回。这里**不**报错 —— 报错会让调用方重试，
		// 而重试必然再次撞同一个幂等键，于是永远失败。
		layer.Attrs(ctx, attribute.Bool("order.idempotent_hit", true))
		return CreateOrderResult{ID: out.ID, OrderNo: o.OrderNo, TotalAmount: out.TotalAmount, Idempotent: true}, nil
	}
	return CreateOrderResult{ID: o.ID, OrderNo: o.OrderNo, TotalAmount: o.TotalAmount}, nil
}

// resolveOrderNo 决定这一单用哪个号：调用方给了就用，没给才生成。
//
// 「给了就用」是幂等的全部意义所在 —— 入口生成的号才能贯穿重试：同一个请求
// 重放时号不变，订单域据此认出它是同一笔。订单域自己另起一个号，等于每次
// 重放都是新单。
//
// 没给时**留痕**（属性 + Warn）：那种情况下重放无法被识别，问题不在这一层，
// 但沉默会让排查的人假设幂等一直生效。
func (s *OrderService) resolveOrderNo(ctx context.Context, given string) string {
	if given != "" {
		return given
	}
	layer.Attrs(ctx, attribute.Bool("order.idempotent_key_missing", true))
	if s.logger != nil {
		s.logger.Warn("mesh-order: create order without idempotency key; replay cannot be deduped",
			logx.String("hint", "caller should pass OrderNo (generated at the entry, stable across retries)"))
	}
	return newOrderNo()
}

// newOrderNo 订单号（uk_order_no，≤40 字符）：M + yyyyMMddHHmmss + 8 位 hex 随机。
//
// 随机段不是装饰：此前这里是 `M%s%06d`（时间戳 + userID%1000000），**同一用户
// 同一秒下两单必然撞号** —— 时间精度到秒、用户段只取后 6 位，两个维度都不够。
// 撞号在有唯一键时表现为第二单 duplicate error（用户看到下单失败），没唯一键时
// 表现为两单同号（后续按 order_no 查询 / 对账串单）。
func newOrderNo() string {
	buf := make([]byte, 4)
	_, _ = rand.Read(buf)
	return "M" + time.Now().Format("20060102150405") + strconv.FormatUint(uint64(binary.BigEndian.Uint32(buf)), 16)
}

// GetOrder 查单：先缓存，未命中回源并回填。
//
// 读缓存走 cacheutil.FetchResult（含空值哨兵）：查不存在的 id 会回填一条短期
// 负缓存，否则"批量请求不存在的 id"能把整层缓存旁路掉，压力直接落到 DB。
// 用 FetchResult 而不是 Fetch 是因为 OrderView.Cached 是验收判据的一部分。
//
// 明细**不进缓存**（与主表分开查）：明细条数随下单变化、主表不随，
// 塞进同一个键会让"改明细"和"改状态"共用一个失效点，反而更容易读到半截数据。
func (s *OrderService) GetOrder(ctx context.Context, id uint64) (v *OrderView, err error) {
	ctx, end := layer.Begin(ctx, "service", "order", "GetOrder", s.traceOn, nil)
	defer func() { end(err) }()

	res, err := cacheutil.FetchResult(ctx, s.cache, s.cacheOpt(), s.key(id),
		func(ctx context.Context) (*model.Order, error) {
			return s.repo.FindByID(ctx, id)
		})
	if err != nil {
		var nf *cacheutil.NotFound
		if errors.As(err, &nf) {
			return nil, codes.New(codes.OrderNotFound, fmt.Sprintf("order %d not found", id))
		}
		return nil, codes.Wrap(codes.Internal, "get order", err)
	}

	items, ierr := s.repo.ItemsByOrderID(ctx, id)
	if ierr != nil {
		// 命中缓存时明细缺席只降级不失败：主表已经拿到了，返回"没有明细的订单"
		// 好过整个请求失败；回源路径上则不豁免 —— 那说明库本身有问题。
		// 两条路径的差别刻意保留，改一致会让"缓存命中"掩盖真实的库故障。
		if res.Hit {
			s.logger.Warn("mesh-order: load items failed, serve order without items",
				logx.String("order_id", strconv.FormatUint(id, 10)), logx.Err(ierr))
		} else {
			return nil, codes.Wrap(codes.Internal, "get order items", ierr)
		}
	}
	return &OrderView{Order: *res.Value, Items: items, Cached: res.Hit}, nil
}

// cacheOpt 本域读缓存的统一参数（正缓存 TTL 与空值哨兵只在这一处定义）。
func (s *OrderService) cacheOpt() cacheutil.Options {
	return cacheutil.Options{
		Label: "order",
		TTL:   orderCacheTTL,
		NotFound: func(err error) bool {
			return errors.Is(err, gorm.ErrRecordNotFound)
		},
		Logger: s.logger,
	}
}

// MarkPaid 改状态：库层条件更新 → 缓存失效。
//
// **单测驱动、非隐藏入口**（2026-10-10 code review §5.9）：本方法只被
// service/order_test.go 直接调用，未在 rpc / handler 接线（IDL 里没有对应的
// 支付回调）。保留它是为演示「条件更新 + 缓存失效」这一写路径形态，与
// product.UpdatePrice / stock.Decrease 同类。
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

func (s *OrderService) key(id uint64) string {
	return "mesh:order:detail:" + strconv.FormatUint(id, 10)
}

// 解码与回填不再在本域手写：都归 cacheutil（见 GetOrder）。
//
// 原先 decodeOrder 里那条 `o.ID == 0 → 视为 miss` 的判据没有丢，它搬进了
// model.Order.Valid（cacheutil.Validatable）—— 规则跟着值走，而不是留在
// 某一个域的解码函数里、让其它域各写一份。
// 「明细不进缓存」这条仍在本域表达（GetOrder 里显式分开查），因为它不是
// 通用规则，是本域对明细变更频率的判断。
