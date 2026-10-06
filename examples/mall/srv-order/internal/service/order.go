// Package service srv-order 业务编排层。
//
// CreateOrder 编排：
//  1. 参数校验（UserId≤0 → 1004；明细空/数量非法 → 1001）
//  2. 同用户并发互斥（lockx；锁器缺失时 fail-open 并留 Warn，见 withUserLock）
//  3. 本地事务 orders + order_items + outbox（事件体 pkg/events.OrderCompleted）
//  4. 后台 outbox.Runner 3s 补发（幂等：status 0→1；traceparent 随
//     mqx Message.Headers 透传，仅当调用链开）
//
// 句柄来源纪律：本层不 import gorm/redis/kafka，也不用全局单例 ——
// DB / KV / Lock / MQ 全部由 app.Setup 从 launchx 的 App 句柄注入，
// 这样换形态（srv → monolith）与换驱动都不改业务代码。
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
	"github.com/gospacex/launchx"
	"github.com/gospacex/lockx"
	"github.com/gospacex/logx"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/gospacex/launchx/examples/mall/pkg/codes"
	"github.com/gospacex/launchx/examples/mall/pkg/events"
	"github.com/gospacex/launchx/examples/mall/pkg/tracex"
	"github.com/gospacex/launchx/examples/mall/srv-order/internal/repository"

	orderpb "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen/order"
)

// orderLockTTL 同用户下单互斥的持锁上限。
//
// 取 5s（远大于单事务耗时）而非配置化：这是"防并发重复提交"的上限，不是地址、
// 也不是业务阈值，写死在代码里与 lockx 配置里的 10s TTL 形成双保险 ——
// 即使进程在解锁前崩溃，锁也会在 TTL 内自动过期，不会永久阻塞该用户下单。
const orderLockTTL = 5 * time.Second

// orderCacheTTL 订单详情在 KV 里的缓存时长
const orderCacheTTL = 5 * time.Minute

// OrderService 订单业务编排
type OrderService struct {
	repo *repository.OrderRepository
	// cache 订单详情缓存（nil 表示缓存组件未装配/已降级 → 直查 DB）
	cache cachex.Cache
	// lock 同用户并发下单互斥（nil 表示锁组件未装配 → fail-open，见 withUserLock）
	lock    lockx.Locker
	traceOn bool
	logger  logx.Logger
}

// NewOrderService 构造（cache/lock 允许 nil，见字段注释）
func NewOrderService(repo *repository.OrderRepository, cache cachex.Cache, lock lockx.Locker, traceOn bool, logger logx.Logger) *OrderService {
	if logger == nil {
		logger = logx.GetGlobal()
	}
	return &OrderService{repo: repo, cache: cache, lock: lock, traceOn: traceOn, logger: logger}
}

// Repo 暴露仓储（组装期健康检查 / 对账用）
func (s *OrderService) Repo() *repository.OrderRepository { return s.repo }

// CreateOrder 下单：校验 → 加用户锁 → 3 表事务 → KV 回填
func (s *OrderService) CreateOrder(ctx context.Context, req *orderpb.CreateOrderReq) (*orderpb.CreateOrderResp, error) {
	resp := &orderpb.CreateOrderResp{UserId: req.GetUserId()}
	if req.GetUserId() <= 0 {
		c := codes.Fail(codes.CodeUnauthorized, "user required")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	items := req.GetItems()
	if len(items) == 0 {
		c := codes.Fail(codes.CodeOrderItemsEmpty, "items required")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	for _, it := range items {
		if it.GetProductId() <= 0 || it.GetQuantity() <= 0 || it.GetUnitPrice() < 0 {
			c := codes.Fail(codes.CodeInvalidParam, "invalid item")
			resp.Code, resp.Message = c.Code, c.Message
			return resp, nil
		}
	}

	// 总额 = Σ quantity × unit_price（分）
	var total int64
	for _, it := range items {
		total += int64(it.GetQuantity()) * it.GetUnitPrice()
	}

	orderNo := newOrderNo()
	unlock, deny := s.withUserLock(ctx, req.GetUserId())
	if deny != nil {
		resp.Code, resp.Message = deny.Code, deny.Message
		return resp, nil
	}
	defer unlock()

	evTraceparent := s.traceparentOf(ctx)
	out, err := s.repo.CreateOrderTx(ctx, repository.CreateOrderInput{
		UserID:      uint64(req.GetUserId()),
		OrderNo:     orderNo,
		TotalAmount: total,
		Items:       toRepoItems(items),
		OutboxFill: func(orderID uint64) (payload, tp string) {
			ev := events.OrderCompleted{
				OrderID:    int64(orderID),
				OrderNo:    orderNo,
				UserID:     req.GetUserId(),
				Amount:     total,
				Items:      toEventItems(items),
				SchemaVer:  events.SchemaVer1,
				TraceID:    evTraceID(ctx),
				OccurredAt: time.Now().UnixMilli(),
			}
			b, mErr := events.Marshal(ev)
			if mErr != nil {
				// 序列化失败不阻断下单主流程：空 payload 时 Runner 直接放弃该行
				s.logger.WarnContext(ctx, "order event marshal failed", logx.Err(mErr))
				return "", evTraceparent
			}
			return string(b), evTraceparent
		},
	})
	if err != nil {
		s.logger.WarnContext(ctx, "create order tx failed", logx.Err(err))
		c := codes.Fail(codes.CodeInternal, err.Error())
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}

	resp.OrderId = int64(out.OrderID)
	resp.OrderNo = out.OrderNo
	resp.TotalAmount = total
	resp.Code, resp.Message = codes.OK().Code, codes.OK().Message
	return resp, nil
}

// GetOrder 查单个订单 + 明细（cachex 缓存优先，miss 回源 DB 并回填）
//
// 缓存的是"已被用户看到过的订单结果"，语义上是可失效的派生状态（状态流转后
// 由 Delete 显式失效），故走 cachex；限流计数那种需要原子自增的状态走 kvx。
// 缓存读失败（含 key 不存在）一律按 miss 处理：缓存是加速路径，不是数据源。
func (s *OrderService) GetOrder(ctx context.Context, req *orderpb.GetOrderReq) (*orderpb.GetOrderResp, error) {
	resp := &orderpb.GetOrderResp{UserId: req.GetUserId(), OrderId: req.GetOrderId()}
	if req.GetUserId() <= 0 {
		c := codes.Fail(codes.CodeUnauthorized, "user required")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	userID, orderID := uint64(req.GetUserId()), uint64(req.GetOrderId())
	key := orderCacheKey(userID, orderID)

	if s.cache != nil {
		if cached, err := s.cache.Get(ctx, key); err == nil && cached != "" {
			var ow repository.OrderWithItems
			if json.Unmarshal([]byte(cached), &ow) == nil {
				return fillOrderResp(resp, &ow), nil
			}
			s.logger.WarnContext(ctx, "order cache decode failed, fallback to db", logx.String("key", key))
		}
	}

	ow, err := s.repo.GetOrder(ctx, orderID, userID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			c := codes.Fail(codes.CodeOrderNotFound, "order not found")
			resp.Code, resp.Message = c.Code, c.Message
			return resp, nil
		}
		c := codes.Fail(codes.CodeInternal, err.Error())
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	if s.cache != nil {
		if b, mErr := json.Marshal(ow); mErr == nil {
			// 回填失败只告警：缓存是加速路径，不是数据源
			if sErr := s.cache.Set(ctx, key, string(b), orderCacheTTL); sErr != nil {
				s.logger.WarnContext(ctx, "order cache fill failed", logx.Err(sErr))
			}
		}
	}
	return fillOrderResp(resp, ow), nil
}

// ListOrders 只读订单流水（对账数据源，不走缓存：流水要求实时）
func (s *OrderService) ListOrders(ctx context.Context, req *orderpb.ListOrdersReq) (*orderpb.ListOrdersResp, error) {
	resp := &orderpb.ListOrdersResp{UserId: req.GetUserId()}
	if req.GetUserId() <= 0 {
		c := codes.Fail(codes.CodeUnauthorized, "user required")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	flows, err := s.repo.ListOrders(ctx, uint64(req.GetUserId()), req.GetFromDate(), req.GetToDate(), int(req.GetLimit()))
	if err != nil {
		c := codes.Fail(codes.CodeInvalidParam, err.Error())
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	pbFlows := make([]*orderpb.OrderFlow, 0, len(flows))
	for i := range flows {
		pbFlows = append(pbFlows, &orderpb.OrderFlow{
			Id:          int64(flows[i].ID),
			UserId:      int64(flows[i].UserID),
			OrderNo:     flows[i].OrderNo,
			Status:      flows[i].Status,
			TotalAmount: flows[i].TotalAmount,
			CreatedAt:   flows[i].CreatedAt.Unix(),
		})
	}
	resp.Flows = pbFlows
	resp.Code, resp.Message = codes.OK().Code, codes.OK().Message
	return resp, nil
}

// Report 4 表 JOIN 报表
func (s *OrderService) Report(ctx context.Context, req *orderpb.ReportReq) (*orderpb.ReportResp, error) {
	resp := &orderpb.ReportResp{UserId: req.GetUserId()}
	if req.GetUserId() <= 0 {
		c := codes.Fail(codes.CodeUnauthorized, "user required")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	rows, err := s.repo.Report(ctx, uint64(req.GetUserId()), req.GetFromDate(), req.GetToDate())
	if err != nil {
		c := codes.Fail(codes.CodeInvalidParam, err.Error())
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	pbRows := make([]*orderpb.ReportRow, 0, len(rows))
	for i := range rows {
		pbRows = append(pbRows, &orderpb.ReportRow{
			OrderNo:           rows[i].OrderNo,
			Status:            rows[i].Status,
			TotalAmount:       rows[i].TotalAmount,
			RefundAmount:      rows[i].RefundAmount,
			ItemCount:         rows[i].ItemCount,
			StatusChangeCount: rows[i].StatusChangeCount,
		})
	}
	resp.Rows = pbRows
	resp.Code, resp.Message = codes.OK().Code, codes.OK().Message
	return resp, nil
}

// ---- 内部 ----

// withUserLock 取同用户下单互斥锁，返回解锁闭包。
//
// 锁器为 nil（组件未装配 / 构造降级）时**显式 fail-open 并留 Warn**：
// 这种降级不会报错，只表现为"极高并发下同一用户可能下出重复单"，
// 若静默继续，事后无法回答"当时有没有加锁"。返回值非 nil 时表示拒绝本次请求。
func (s *OrderService) withUserLock(ctx context.Context, userID int64) (func(), *codes.Code) {
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
		c := codes.Fail(codes.CodeOrderConcurrent, "concurrent order in progress, retry later")
		return noop, &c
	}
	return func() {
		// 解锁必须用请求 ctx（闭包捕获的外层变量）：这里传过 context.Background()，
		// lockx 的 release span 因此失去父节点，每次解锁都在 Jaeger 上另起一根
		// 孤儿单 span trace。解锁发生在请求生命周期内，ctx 没有被取消的风险。
		if uErr := lk.Unlock(ctx); uErr != nil {
			// 解锁失败不阻断业务：锁有 TTL，最坏情况是短暂阻塞而非死锁
			s.logger.WarnContext(ctx, "order: unlock failed (lock will expire by ttl)", logx.Err(uErr))
		}
	}, nil
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

// newOrderNo 订单号（uk_order_no，≤40 字符）：O + yyyyMMddHHmmss + 8 位 hex 随机
func newOrderNo() string {
	buf := make([]byte, 4)
	_, _ = rand.Read(buf)
	return "O" + time.Now().Format("20060102150405") + strconv.FormatUint(uint64(binary.BigEndian.Uint32(buf)), 16)
}

func orderCacheKey(userID, orderID uint64) string {
	return fmt.Sprintf("mall:order:%d:%d", userID, orderID)
}

func fillOrderResp(resp *orderpb.GetOrderResp, ow *repository.OrderWithItems) *orderpb.GetOrderResp {
	resp.OrderNo = ow.Order.OrderNo
	resp.Status = ow.Order.Status
	resp.TotalAmount = ow.Order.TotalAmount
	resp.PaidAmount = ow.Order.PaidAmount
	resp.Items = toPbItems(ow.Items)
	resp.Code, resp.Message = codes.OK().Code, codes.OK().Message
	return resp
}

func toRepoItems(items []*orderpb.OrderItem) []repository.OrderItem {
	out := make([]repository.OrderItem, 0, len(items))
	for _, it := range items {
		out = append(out, repository.OrderItem{
			ProductID:   uint64(it.GetProductId()),
			ProductName: it.GetName(),
			Quantity:    int(it.GetQuantity()),
			UnitPrice:   it.GetUnitPrice(),
		})
	}
	return out
}

func toEventItems(items []*orderpb.OrderItem) []events.OrderItem {
	out := make([]events.OrderItem, 0, len(items))
	for _, it := range items {
		out = append(out, events.OrderItem{
			ProductID: it.GetProductId(),
			Name:      it.GetName(),
			Quantity:  int(it.GetQuantity()),
			UnitPrice: it.GetUnitPrice(),
		})
	}
	return out
}

func toPbItems(items []repository.OrderItem) []*orderpb.OrderItem {
	out := make([]*orderpb.OrderItem, 0, len(items))
	for i := range items {
		out = append(out, &orderpb.OrderItem{
			ProductId: int64(items[i].ProductID),
			Name:      items[i].ProductName,
			Quantity:  int32(items[i].Quantity),
			UnitPrice: items[i].UnitPrice,
		})
	}
	return out
}

// tracexStart 业务层埋点（保留一个内部别名，避免 service 包各处重复取 tracer）
var tracexStart = tracex.Start
