// Package service srv-order 业务编排层。
//
// 在分层链上的位置：handler → **service** → repository → model。
//
// 边界纪律：本层**不 import pb、不 import kitex**。出入参一律走 model 的
// 领域 DTO（*Cmd / *Query / *Result），pb 与领域模型之间的转换归 handler。
// 否则 pb 加一个字段就会顺着 import 渗进业务规则，协议演进与业务演进被绑死。
//
// 业务失败用 codes.Error 向上抛（带码），由 handler 统一翻译成响应体
// Code/Message；本层自己不认识响应体长什么样。
//
// 句柄来源纪律：本层不 import gorm/redis/kafka，也不用全局单例 ——
// DB / Cache / KV / Lock / MQ 全部经 repository 与构造参数注入，
// 换形态（srv → monolith）与换驱动都不改业务代码。
//
// 编排总览（与重构前一致）：
//  1. 参数校验（user ≤ 0 → 1004；明细空/数量非法 → 1001）
//  2. 同用户并发互斥（lockx；锁器缺失时 fail-open 并留 Warn，见 withUserLock）
//  3. 本地事务 orders + order_items + outbox（事件体 pkg/events.OrderCompleted）
//  4. 后台 outbox.Runner 3s 补发（幂等：status 0→1；traceparent 随
//     mqx Message.Headers 透传，仅当调用链开）
package service

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/gospacex/cachex"
	"github.com/gospacex/kvx"
	"github.com/gospacex/launchx"
	"github.com/gospacex/lockx"
	"github.com/gospacex/logx"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/gospacex/launchx/examples/mall/pkg/codes"
	"github.com/gospacex/launchx/examples/mall/pkg/events"
	"github.com/gospacex/launchx/examples/mall/srv-order/internal/model"
	"github.com/gospacex/launchx/examples/mall/srv-order/internal/repository"
	"github.com/gospacex/otelx/layer"
)

// orderLockTTL 同用户下单互斥的持锁上限。
//
// 取 5s（远大于单事务耗时）而非配置化：这是"防并发重复提交"的上限，不是地址、
// 也不是业务阈值，写死在代码里与 lockx 配置里的 10s TTL 形成双保险 ——
// 即使进程在解锁前崩溃，锁也会在 TTL 内自动过期，不会永久阻塞该用户下单。
const orderLockTTL = 5 * time.Second

// orderCacheTTL 订单详情在 cachex 里的缓存时长（状态流转后显式 Delete 失效）
const orderCacheTTL = 5 * time.Minute

// OrderService 订单业务编排
type OrderService struct {
	repo *repository.OrderRepository
	// cache 订单详情缓存（nil 表示缓存组件未装配/已降级 → 直查 DB）
	cache cachex.Cache
	// kv 下单笔数计数（nil 表示 KV 组件未装配 → 计数降级为 no-op）
	kv kvx.KVStore
	// lock 同用户并发下单互斥（nil 表示锁组件未装配 → fail-open，见 withUserLock）
	lock    lockx.Locker
	traceOn bool
	logger  logx.Logger
}

// NewOrderService 构造（cache / kv / lock 允许 nil，见字段注释）
func NewOrderService(
	repo *repository.OrderRepository,
	cache cachex.Cache,
	kv kvx.KVStore,
	lock lockx.Locker,
	traceOn bool,
	logger logx.Logger,
) *OrderService {
	if logger == nil {
		logger = logx.GetGlobal()
	}
	return &OrderService{repo: repo, cache: cache, kv: kv, lock: lock, traceOn: traceOn, logger: logger}
}

// Repo 暴露仓储（pkg/handlecheck 断言 / 对账用）
func (s *OrderService) Repo() *repository.OrderRepository { return s.repo }

// begin 开 service 层 span，返回「收 error 再收尾」的闭包。
//
// 不做 quiet 豁免：service 返回非 nil error 意味着这次业务操作没完成
// （参数非法 / 写库失败 / 下游失败），都该在 Jaeger 上看得见 —— 与单体
// 示例 s.end 的口径一致。预期内的业务拒绝由 handler 翻成响应体 Code，
// 不进 error，因此不会把正常的「商品不存在」标成故障。
func (s *OrderService) begin(ctx context.Context, op string) (context.Context, layer.End) {
	return layer.Begin(ctx, "service", "order", op, s.traceOn, nil)
}

// CreateOrder 下单：校验 → 加用户锁 → 3 表事务 → 计数
func (s *OrderService) CreateOrder(ctx context.Context, cmd model.CreateOrderCmd) (out *model.CreateOrderResult, err error) {
	ctx, end := s.begin(ctx, "CreateOrder")
	defer func() { end(err) }()

	if cmd.UserID <= 0 {
		return nil, codes.New(codes.CodeUnauthorized, "user required")
	}
	if len(cmd.Items) == 0 {
		return nil, codes.New(codes.CodeOrderItemsEmpty, "items required")
	}
	for _, it := range cmd.Items {
		if it.ProductID <= 0 || it.Quantity <= 0 || it.UnitPrice < 0 {
			return nil, codes.New(codes.CodeInvalidParam, "invalid item")
		}
	}

	// 总额 = Σ quantity × unit_price（分）。服务端算而不收调用方传值：
	// 总额是后续对账与积分入账的依据，让客户端带进来等于把可篡改字段写进账本。
	var total int64
	for _, it := range cmd.Items {
		total += int64(it.Quantity) * it.UnitPrice
	}

	orderNo := resolveOrderNo(s, ctx, cmd.OrderNo)
	unlock, deny := s.withUserLock(ctx, cmd.UserID)
	if deny != nil {
		return nil, deny
	}
	defer unlock()

	tp := s.traceparentOf(ctx)
	rec, err := s.repo.CreateOrderTx(ctx, repository.CreateOrderInput{
		UserID:      cmd.UserID,
		OrderNo:     orderNo,
		TotalAmount: total,
		Items:       toRepoItems(cmd.Items),
		OutboxFill: func(orderID uint64) (payload, traceparent string) {
			ev := events.OrderCompleted{
				OrderID:    int64(orderID),
				OrderNo:    orderNo,
				UserID:     int64(cmd.UserID),
				Amount:     total,
				Items:      toEventItems(cmd.Items),
				SchemaVer:  events.SchemaVer1,
				TraceID:    evTraceID(ctx),
				OccurredAt: time.Now().UnixMilli(),
			}
			b, mErr := events.Marshal(ev)
			if mErr != nil {
				// 序列化失败不阻断下单主流程：空 payload 时 Runner 直接放弃该行
				s.logger.WarnContext(ctx, "order event marshal failed", logx.Err(mErr))
				return "", tp
			}
			return string(b), tp
		},
	})
	if err != nil {
		s.logger.WarnContext(ctx, "create order tx failed", logx.Err(err))
		return nil, codes.Wrap(codes.CodeInternal, "create order failed", err)
	}
	if span := trace.SpanFromContext(ctx); span != nil && span.IsRecording() {
		span.SetAttributes(
			attribute.Int64("order.id", int64(rec.OrderID)),
			attribute.String("order.no", rec.OrderNo),
		)
	}
	s.bumpUserOrders(ctx, cmd.UserID)
	return &model.CreateOrderResult{
		OrderID:     rec.OrderID,
		OrderNo:     rec.OrderNo,
		TotalAmount: total,
		Status:      model.StatusCreated,
	}, nil
}

// GetOrder 查单个订单 + 明细（cachex 缓存优先，miss 回源 DB 并回填）
//
// 缓存的是"已被用户看到过的订单结果"，语义上是可失效的派生状态（状态流转后
// 由 Delete 显式失效），故走 cachex；限流计数那种需要原子自增的状态走 kvx。
// 缓存读失败（含 key 不存在）一律按 miss 处理：缓存是加速路径，不是数据源。
func (s *OrderService) GetOrder(ctx context.Context, q model.GetOrderQuery) (out *model.OrderWithItems, err error) {
	ctx, end := s.begin(ctx, "GetOrder")
	defer func() { end(err) }()

	if q.UserID <= 0 {
		return nil, codes.New(codes.CodeUnauthorized, "user required")
	}
	if q.OrderID <= 0 {
		return nil, codes.New(codes.CodeInvalidParam, "order_id required")
	}
	key := orderCacheKey(q.UserID, q.OrderID)

	if s.cache != nil {
		if cached, err := s.cache.Get(ctx, key); err == nil && cached != "" {
			var ow model.OrderWithItems
			if json.Unmarshal([]byte(cached), &ow) == nil {
				if span := trace.SpanFromContext(ctx); span != nil && span.IsRecording() {
					span.SetAttributes(attribute.Bool("cache.hit", true))
				}
				return &ow, nil
			}
			s.logger.WarnContext(ctx, "order cache decode failed, fallback to db", logx.String("key", key))
		}
	}

	ow, err := s.repo.GetOrder(ctx, q)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, codes.Wrap(codes.CodeOrderNotFound, "order not found", err)
		}
		return nil, codes.Wrap(codes.CodeInternal, "load order failed", err)
	}
	if s.cache != nil {
		if b, mErr := json.Marshal(ow); mErr == nil {
			// 回填失败只告警：缓存是加速路径，不是数据源
			if sErr := s.cache.Set(ctx, key, string(b), orderCacheTTL); sErr != nil {
				s.logger.WarnContext(ctx, "order cache fill failed", logx.Err(sErr))
			}
		}
	}
	return ow, nil
}

// UpdateStatus 订单状态流转（2 表事务 + 缓存失效）
//
// 状态与日志必须同事务推进，且要顺带把详情缓存失效掉 —— 这两件事都不属于
// repository（它不该知道缓存），故放在本层。
//
// 当前 RPC 面未开放该方法（kitex_gen 由 IDL 生成，本域不自行加方法），
// 因此没有对应的 handler 入口；保留它是为了给 repository 的两表事务一个
// 正确层级的调用点，而不是让状态流转散落到装配层去。
func (s *OrderService) UpdateStatus(ctx context.Context, cmd model.UpdateStatusCmd) (err error) {
	ctx, end := s.begin(ctx, "UpdateStatus")
	defer func() { end(err) }()

	if cmd.UserID <= 0 {
		return codes.New(codes.CodeUnauthorized, "user required")
	}
	if cmd.OrderID <= 0 || cmd.To == "" {
		return codes.New(codes.CodeInvalidParam, "invalid status param")
	}
	if err := s.repo.UpdateStatusTx(ctx, repository.UpdateStatusInput{
		OrderID: cmd.OrderID,
		UserID:  cmd.UserID,
		To:      cmd.To,
	}); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return codes.Wrap(codes.CodeOrderNotFound, "order not found", err)
		}
		s.logger.WarnContext(ctx, "update order status tx failed", logx.Err(err))
		return codes.Wrap(codes.CodeInternal, "update order status failed", err)
	}
	s.invalidate(ctx, cmd.UserID, cmd.OrderID)
	return nil
}

// ListOrders 只读订单流水（对账数据源，不走缓存：流水要求实时）
func (s *OrderService) ListOrders(ctx context.Context, q model.ListOrdersQuery) (out []model.OrderFlowRow, err error) {
	ctx, end := s.begin(ctx, "ListOrders")
	defer func() { end(err) }()
	rows, err := s.repo.ListOrders(ctx, q)
	if err != nil {
		if errors.Is(err, repository.ErrBadDateRange) {
			// 日期区间是调用方传错，回 1001 让他改参数再来，而不是报内部错误
			return nil, codes.Wrap(codes.CodeInvalidParam, "invalid date range", err)
		}
		return nil, codes.Wrap(codes.CodeInternal, "list orders failed", err)
	}
	s.auditTotals(ctx, rows)
	return rows, nil
}

// Report 4 表 JOIN 报表
func (s *OrderService) Report(ctx context.Context, q model.OrderReportQuery) (out []model.ReportRow, err error) {
	ctx, end := s.begin(ctx, "Report")
	defer func() { end(err) }()
	rows, err := s.repo.Report(ctx, q)
	if err != nil {
		if errors.Is(err, repository.ErrBadDateRange) {
			return nil, codes.Wrap(codes.CodeInvalidParam, "invalid date range", err)
		}
		return nil, codes.Wrap(codes.CodeInternal, "report orders failed", err)
	}
	return rows, nil
}

// ---- 内部 ----

// withUserLock 取同用户下单互斥锁，返回解锁闭包。
//
// 锁器为 nil（组件未装配 / 构造降级）时**显式 fail-open 并留 Warn**：
// 这种降级不会报错，只表现为"极高并发下同一用户可能下出重复单"，
// 若静默继续，事后无法回答"当时有没有加锁"。返回非 nil error 表示拒绝本次请求。
func (s *OrderService) withUserLock(ctx context.Context, userID uint64) (func(), *codes.Error) {
	noop := func() {}
	if s.lock == nil {
		s.logger.WarnContext(ctx, "order: locker unavailable, create-order runs without mutual exclusion",
			logx.Int("user_id", int(userID)),
			logx.String("consequence", "concurrent orders from the same user may duplicate"),
			logx.String("hint", "check xmiddleware.lockx in config.yaml"))
		return noop, nil
	}
	lk, err := s.lock.Lock(ctx, fmt.Sprintf("order:create:user:%d", userID), lockx.WithTTL(orderLockTTL))
	if err != nil {
		// 取锁失败（含重试耗尽）≠ 业务失败：明确告知调用方可重试，
		// 不能笼统报 internal（会被当成需要人工介入的故障）。
		return noop, codes.Wrap(codes.CodeOrderConcurrent, "concurrent order in progress, retry later", err)
	}
	return func() {
		// 解锁必须用请求 ctx：这里传过 context.Background()，lockx 的 release
		// span 因此失去父节点，每次解锁都在 Jaeger 上另起一根孤儿单 span trace。
		// 解锁发生在请求生命周期内，ctx 没有被取消的风险。
		if uErr := lk.Unlock(ctx); uErr != nil {
			// 解锁失败不阻断业务：锁有 TTL，最坏情况是短暂阻塞而非死锁
			s.logger.WarnContext(ctx, "order: unlock failed (lock will expire by ttl)", logx.Err(uErr))
		}
	}, nil
}

// bumpUserOrders 用户下单笔数计数（kvx 原子自增；KV 缺失时静默降级）。
//
// 与限流计数一样走 kvx，但**不参与业务判定**：计数失败既不告警也不重试，
// 笔数只是运营信号，不该在它上面产生故障面。
func (s *OrderService) bumpUserOrders(ctx context.Context, userID uint64) {
	if s.kv == nil || userID <= 0 {
		return
	}
	if _, err := s.kv.Incr(ctx, fmt.Sprintf("orders:user:%d", userID)); err != nil {
		s.logger.WarnContext(ctx, "order: user order counter incr failed", logx.Err(err))
	}
}

// auditTotals 对账：明细合计应当等于订单总额。
//
// 列表 JOIN 明细就是为这件事 —— 两者不等说明下单时总额算错、或明细事后被
// 改过。只记一条汇总告警不逐行刷屏，也绝不拦截：对账差异要人来看，
// 不能让列表因此查不出来。
func (s *OrderService) auditTotals(ctx context.Context, rows []model.OrderFlowRow) {
	var bad int
	for i := range rows {
		if rows[i].ItemCount > 0 && rows[i].ItemAmount != rows[i].TotalAmount {
			bad++
		}
	}
	if bad > 0 {
		s.logger.WarnContext(ctx, "order: item amount does not match order total",
			logx.Int("mismatched", bad),
			logx.Int("total", len(rows)),
			logx.String("hint", "sum(order_items.quantity*unit_price) != orders.total_amount"))
	}
}

// invalidate 删详情缓存（状态流转后必须失效，否则读缓存会拿到旧状态）
func (s *OrderService) invalidate(ctx context.Context, userID, orderID uint64) {
	if s.cache == nil {
		return
	}
	// 失效失败只告警：下一次读会拿到旧值，但 TTL 兜底（5 分钟）不会永久错
	if err := s.cache.Delete(ctx, orderCacheKey(userID, orderID)); err != nil {
		s.logger.WarnContext(ctx, "order: cache invalidate failed", logx.Err(err))
	}
}

// traceparentOf 取当前链路的 W3C traceparent（随 outbox header 透传）。
// 仅当调用链开时经 otel 全局传播器注入；无活动 span 时为空串。
func (s *OrderService) traceparentOf(ctx context.Context) string {
	if !s.traceOn {
		return ""
	}
	carrier := propagation.MapCarrier(map[string]string{})
	launchx.Inject(ctx, carrier)
	return carrier.Get(events.TraceHeaderKey)
}

// evTraceID 事件体内嵌 trace_id（无活动 span 时为空）
func evTraceID(ctx context.Context) string {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		return sc.TraceID().String()
	}
	return ""
}

// resolveOrderNo 决定本单的单号：调用方给了就用它的（那是幂等键），没给才自己生成。
//
// 为什么必须有这一层：bff 用**同一个** order_no 同时驱动库存预占、积分入账与
// 下单，三处共用一个号才能对账（跨域 report 的 JOIN 也靠它）。订单域若自己另起
// 一个号，重放一次就是「新号 + 新单 + 新预占」，而积分侧会被 uk_ledger_order_type
// 幂等掉 ⇒ 出现「下了两单、只送一次积分」的对账差异。
//
// 没给时**不报错**：老调用方（仓内 demo 脚本、旧客户端）还没有这个字段，直接
// 拒会让它们全 400。代价是这段调用没有幂等保护 —— 所以必须留痕（Warn + span
// 属性），否则"幂等没生效"这件事会静默到对账那天才暴露。
func resolveOrderNo(s *OrderService, ctx context.Context, given string) string {
	if given != "" {
		return given
	}
	generated := newOrderNo()
	if span := trace.SpanFromContext(ctx); span != nil && span.IsRecording() {
		span.SetAttributes(attribute.Bool("order.idempotent_key_missing", true))
	}
	if s.logger != nil {
		s.logger.WarnContext(ctx, "order: create without idempotency key, replay protection is off",
			logx.String("generated_order_no", generated))
	}
	return generated
}

// newOrderNo 订单号（uk_order_no，≤40 字符）：O + yyyyMMddHHmmss + 8 位 hex 随机
func newOrderNo() string {
	buf := make([]byte, 4)
	_, _ = rand.Read(buf)
	return "O" + time.Now().Format("20060102150405") + strconv.FormatUint(uint64(binary.BigEndian.Uint32(buf)), 16)
}

func orderCacheKey(userID, orderID uint64) string {
	return fmt.Sprintf("mall:order:%d:%d", userID, orderID)
}

func toRepoItems(items []model.OrderItemCmd) []model.OrderItem {
	out := make([]model.OrderItem, 0, len(items))
	for _, it := range items {
		out = append(out, model.OrderItem{
			ProductID:   it.ProductID,
			ProductName: it.Name,
			Quantity:    it.Quantity,
			UnitPrice:   it.UnitPrice,
		})
	}
	return out
}

func toEventItems(items []model.OrderItemCmd) []events.OrderItem {
	out := make([]events.OrderItem, 0, len(items))
	for _, it := range items {
		out = append(out, events.OrderItem{
			ProductID: int64(it.ProductID),
			Name:      it.Name,
			Quantity:  it.Quantity,
			UnitPrice: it.UnitPrice,
		})
	}
	return out
}
