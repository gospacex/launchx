// Package service srv-stock 业务编排层。
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
// 防超卖的两道闸（本域最重要的设计）：
//  1. lockx 按商品维度互斥 —— 让同一商品的预占串行化
//  2. SQL 条件更新（WHERE available >= ?）+ RowsAffected 判定 ——
//     即使锁失效（组件降级 / 跨进程绕过锁的调用），数据库侧仍然不会扣成负数
//
// 只留一道都会在并发下超卖：只有锁 → 锁降级即超卖；只有条件更新 →
// 高并发下大量请求竞争同一行，失败面从"拒绝"退化成"抖动"。
//
// 句柄来源纪律：本层不 import gorm/redis/kafka，也不用全局单例 ——
// DB / Cache / KV / Lock / MQ 全部经 repository 与构造参数注入，
// 换形态（srv → monolith）与换驱动都不改业务代码。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/gospacex/launchx/examples/mall/srv-stock/internal/model"
	"github.com/gospacex/launchx/examples/mall/srv-stock/internal/repository"
	"github.com/gospacex/otelx/layer"
)

// stockLockTTL 预占互斥的持锁上限。
//
// 取 5s：覆盖"加锁 → 3 表事务 → 解锁"的最坏耗时，且与 lockx 配置里的 10s
// TTL 形成双保险 —— 进程在解锁前崩溃时锁也会自动过期，不会永久锁死该商品。
const stockLockTTL = 5 * time.Second

// stockCacheTTL 库存快照缓存时长（预占/释放/确认/调整成功后 Delete 失效）
const stockCacheTTL = 30 * time.Second

// 失败熔断计数：同一商品连续预占失败的窗口与阈值（kvx 计数）。
const (
	failCounterWindow = time.Minute
	failCounterCap    = 20 // 一个窗口内失败 20 次即视为该商品已售罄，快速失败
)

// StockService 库存业务编排
type StockService struct {
	repo *repository.StockRepository
	// cache 库存快照缓存（nil 表示缓存组件未装配/已降级 → 直查 DB）
	cache cachex.Cache
	// kv 限流与失败熔断计数（nil 表示 KV 组件未装配 → 计数降级为 no-op）
	kv kvx.KVStore
	// lock 预占互斥（nil 表示锁组件未装配 → fail-open，见 withProductLock）
	lock    lockx.Locker
	traceOn bool
	logger  logx.Logger
}

// NewStockService 构造（cache / kv / lock 允许 nil，见字段注释）
func NewStockService(
	repo *repository.StockRepository,
	cache cachex.Cache,
	kv kvx.KVStore,
	lock lockx.Locker,
	traceOn bool,
	logger logx.Logger,
) *StockService {
	if logger == nil {
		logger = logx.GetGlobal()
	}
	return &StockService{repo: repo, cache: cache, kv: kv, lock: lock, traceOn: traceOn, logger: logger}
}

// Repo 暴露仓储（pkg/handlecheck 断言 / 对账用）
func (s *StockService) Repo() *repository.StockRepository { return s.repo }

// begin 开 service 层 span，返回「收 error 再收尾」的闭包。
//
// 不做 quiet 豁免：service 返回非 nil error 意味着这次业务操作没完成
// （参数非法 / 写库失败 / 下游失败），都该在 Jaeger 上看得见 —— 与单体
// 示例 s.end 的口径一致。预期内的业务拒绝由 handler 翻成响应体 Code，
// 不进 error，因此不会把正常的「商品不存在」标成故障。
func (s *StockService) begin(ctx context.Context, op string) (context.Context, layer.End) {
	return layer.Begin(ctx, "service", "stock", op, s.traceOn, nil)
}

// GetStock 查库存快照（cachex 优先，miss 回源 DB 并回填）
//
// 缓存 TTL 刻意短（30s）：库存是强一致敏感数据，缓存只是削峰手段，
// 所有写路径（预占/释放/确认/调整）都会显式 Delete，双保险。
//
// 回源后拿主表冗余计数与流水表聚合值对一次账：两者同源却在两条写路径上
// 各自推进，不一致即意味着某次事务只落了一半。只对账而不自动修正 ——
// 修正需要锁 + 事务，读路径上做会把"查询"变成"写入"，让读请求也承担
// 写热点；这里只留告警，由人工/对账任务处置。
func (s *StockService) GetStock(ctx context.Context, q model.GetStockQuery) (out *model.StockDetail, err error) {
	ctx, end := s.begin(ctx, "GetStock")
	defer func() { end(err) }()

	if q.ProductID <= 0 {
		return nil, codes.New(codes.CodeInvalidParam, "product_id required")
	}
	key := stockCacheKey(q.ProductID)

	if s.cache != nil {
		if cached, err := s.cache.Get(ctx, key); err == nil && cached != "" {
			var d model.StockDetail
			if json.Unmarshal([]byte(cached), &d) == nil {
				if span := trace.SpanFromContext(ctx); span != nil && span.IsRecording() {
					span.SetAttributes(attribute.Bool("cache.hit", true))
				}
				return &d, nil
			}
			s.logger.WarnContext(ctx, "stock cache decode failed, fallback to db", logx.String("key", key))
		}
	}

	d, err := s.repo.GetStock(ctx, q.ProductID)
	if err != nil {
		return nil, s.mapRepoErr(ctx, err)
	}
	s.auditDrift(ctx, d)
	if s.cache != nil {
		if b, mErr := json.Marshal(d); mErr == nil {
			if sErr := s.cache.Set(ctx, key, string(b), stockCacheTTL); sErr != nil {
				s.logger.WarnContext(ctx, "stock cache fill failed", logx.Err(sErr))
			}
		}
	}
	return d, nil
}

// ListStocks 联表分页（不走缓存：批量读命中率低，缓存反而增加失效面）
func (s *StockService) ListStocks(ctx context.Context, q model.ListStocksQuery) (out []model.StockDetail, err error) {
	ctx, end := s.begin(ctx, "ListStocks")
	defer func() { end(err) }()
	rows, err := s.repo.ListStocks(ctx, q)
	if err != nil {
		return nil, codes.Wrap(codes.CodeInternal, "list stocks failed", err)
	}
	return rows, nil
}

// ReserveStock 预占：熔断计数 → 加锁 → 3 表事务 + outbox → 缓存失效
func (s *StockService) ReserveStock(ctx context.Context, cmd model.ReserveCmd) (out *model.ReserveResult, err error) {
	ctx, end := s.begin(ctx, "ReserveStock")
	defer func() { end(err) }()

	if cmd.ProductID <= 0 || cmd.Quantity <= 0 || cmd.OrderNo == "" {
		return nil, codes.New(codes.CodeInvalidParam, "product_id/quantity/order_no required")
	}
	// 熔断前置：一个窗口内失败过多的商品直接快速失败，避免每次都走一遍
	// 加锁 + 事务（超卖时这个路径会被打成热点）。
	if s.tripped(ctx, cmd.ProductID) {
		return nil, codes.New(codes.CodeStockInsufficient, "stock sold rec (circuit tripped)")
	}

	unlock, deny := s.withProductLock(ctx, cmd.ProductID)
	if deny != nil {
		return nil, deny
	}
	defer unlock()

	tp := s.traceparentOf(ctx)
	rec, err := s.repo.ReserveTx(ctx, repository.ReserveInput{
		OrderNo:   cmd.OrderNo,
		ProductID: cmd.ProductID,
		Quantity:  cmd.Quantity,
		UserID:    cmd.UserID,
		OutboxFill: func(reservationID uint64) (payload, traceparent string) {
			ev := events.StockReserved{
				OrderNo:     cmd.OrderNo,
				UserID:      int64(cmd.UserID),
				ProductID:   int64(cmd.ProductID),
				Quantity:    cmd.Quantity,
				Success:     true,
				Available:   0, // 事务内尚未回读，由消费侧以查询为准（避免事件里带推测值）
				Reservation: int64(reservationID),
				SchemaVer:   events.SchemaVer1,
				TraceID:     evTraceID(ctx),
				OccurredAt:  time.Now().UnixMilli(),
			}
			b, mErr := events.Marshal(ev)
			if mErr != nil {
				// 序列化失败不阻断预占：空 payload 时 Runner 直接放弃该行
				s.logger.WarnContext(ctx, "stock event marshal failed", logx.Err(mErr))
				return "", tp
			}
			return string(b), tp
		},
	})
	if err != nil {
		s.bumpFail(ctx, cmd.ProductID)
		return nil, s.mapRepoErr(ctx, err)
	}
	s.invalidate(ctx, cmd.ProductID)
	if span := trace.SpanFromContext(ctx); span != nil && span.IsRecording() {
		span.SetAttributes(attribute.Int64("stock.reservation_id", int64(rec.ReservationID)))
	}
	return &model.ReserveResult{
		ReservationID: rec.ReservationID,
		Reserved:      cmd.Quantity,
		Available:     rec.Available,
	}, nil
}

// ReleaseStock 释放预占（幂等：流水不存在按已释放处理）
func (s *StockService) ReleaseStock(ctx context.Context, cmd model.ReleaseCmd) (out *model.ReleaseResult, err error) {
	ctx, end := s.begin(ctx, "ReleaseStock")
	defer func() { end(err) }()

	if cmd.ProductID <= 0 || cmd.Quantity <= 0 || cmd.OrderNo == "" {
		return nil, codes.New(codes.CodeInvalidParam, "product_id/quantity/order_no required")
	}
	available, err := s.repo.ReleaseTx(ctx, repository.ReleaseInput{
		OrderNo:   cmd.OrderNo,
		ProductID: cmd.ProductID,
		Quantity:  cmd.Quantity,
	})
	if err != nil {
		return nil, s.mapRepoErr(ctx, err)
	}
	s.invalidate(ctx, cmd.ProductID)
	return &model.ReleaseResult{Available: available}, nil
}

// ConfirmStock 确认预占（真正出库：扣 total）
func (s *StockService) ConfirmStock(ctx context.Context, cmd model.ConfirmCmd) (out *model.ConfirmResult, err error) {
	ctx, end := s.begin(ctx, "ConfirmStock")
	defer func() { end(err) }()

	if cmd.ProductID <= 0 || cmd.Quantity <= 0 || cmd.OrderNo == "" {
		return nil, codes.New(codes.CodeInvalidParam, "product_id/quantity/order_no required")
	}
	total, available, err := s.repo.ConfirmTx(ctx, repository.ConfirmInput{
		OrderNo:   cmd.OrderNo,
		ProductID: cmd.ProductID,
		Quantity:  cmd.Quantity,
	})
	if err != nil {
		return nil, s.mapRepoErr(ctx, err)
	}
	s.invalidate(ctx, cmd.ProductID)
	return &model.ConfirmResult{Total: total, Available: available}, nil
}

// AdjustStock 入库 / 盘点（2 表事务）
func (s *StockService) AdjustStock(ctx context.Context, cmd model.AdjustCmd) (out *model.AdjustResult, err error) {
	ctx, end := s.begin(ctx, "AdjustStock")
	defer func() { end(err) }()

	if cmd.OperatorID <= 0 {
		return nil, codes.New(codes.CodeUnauthorized, "operator required")
	}
	if cmd.ProductID <= 0 || cmd.Delta == 0 {
		return nil, codes.New(codes.CodeInvalidParam, "product_id/delta invalid")
	}
	total, available, err := s.repo.AdjustTx(ctx, repository.AdjustInput{
		ProductID:  cmd.ProductID,
		Delta:      cmd.Delta,
		Reason:     cmd.Reason,
		OperatorID: cmd.OperatorID,
	})
	if err != nil {
		return nil, s.mapRepoErr(ctx, err)
	}
	s.invalidate(ctx, cmd.ProductID)
	return &model.AdjustResult{Total: total, Available: available}, nil
}

// Report 3 表 JOIN 报表
func (s *StockService) Report(ctx context.Context, q model.StockReportQuery) (out []model.ReportRow, err error) {
	ctx, end := s.begin(ctx, "Report")
	defer func() { end(err) }()
	rows, err := s.repo.Report(ctx, q)
	if err != nil {
		return nil, codes.Wrap(codes.CodeInternal, "report stocks failed", err)
	}
	return rows, nil
}

// ---- 内部 ----

// withProductLock 取商品维度互斥锁，返回解锁闭包。
//
// 锁器为 nil（组件未装配 / 构造降级）时**显式 fail-open 并留 Warn**，且必须
// 在日志里说清后果：这是本示例唯一一处"降级即可能超卖"的路径，
// 静默继续会让事后无法回答"当时有没有加锁"。返回非 nil error 表示拒绝本次请求。
func (s *StockService) withProductLock(ctx context.Context, productID uint64) (func(), *codes.Error) {
	noop := func() {}
	if s.lock == nil {
		s.logger.WarnContext(ctx, "stock: locker unavailable, reserve runs WITHOUT mutual exclusion",
			logx.Int("product_id", int(productID)),
			logx.String("consequence", "oversell is possible; only the conditional UPDATE guards stock"),
			logx.String("hint", "check xmiddleware.lockx in config.yaml"))
		return noop, nil
	}
	lk, err := s.lock.Lock(ctx, fmt.Sprintf("stock:reserve:%d", productID), lockx.WithTTL(stockLockTTL))
	if err != nil {
		// 取锁失败（含重试耗尽）≠ 业务失败：明确告知调用方可重试
		return noop, codes.Wrap(codes.CodeStockLockFailed, "concurrent reserve in progress, retry later", err)
	}
	return func() {
		// 解锁必须用请求 ctx：传 Background 会让 release span 失去父节点，
		// 每次解锁在 Jaeger 上另起一根孤儿单 span trace（见 order 域同款注释）。
		if uErr := lk.Unlock(ctx); uErr != nil {
			s.logger.WarnContext(ctx, "stock: unlock failed (lock will expire by ttl)", logx.Err(uErr))
		}
	}, nil
}

// mapRepoErr 把持久层哨兵错误翻译成业务码并上抛。
//
// 翻译集中一处是刻意的：4xxx 各码的语义差异（没记录 vs 不够扣 vs 重复释放）
// 一旦散落到各个方法里，必然出现"某条路径把 4001 报成 4002"。
func (s *StockService) mapRepoErr(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, repository.ErrNotFound):
		return codes.Wrap(codes.CodeStockNotFound, "stock record not found", err)
	case errors.Is(err, repository.ErrInsufficient):
		return codes.Wrap(codes.CodeStockInsufficient, "stock insufficient", err)
	case errors.Is(err, repository.ErrReservationNF):
		// 幂等释放/重复确认：按成功处理（已经释放/确认过了）
		return codes.Wrap(codes.CodeStockReservationNF, "reservation not found, nothing to do", err)
	case errors.Is(err, repository.ErrOverRelease):
		return codes.Wrap(codes.CodeStockOverRelease, "quantity exceeds reservation", err)
	default:
		s.logger.WarnContext(ctx, "stock: repository error", logx.Err(err))
		return codes.Wrap(codes.CodeStockReserveFailed, "stock operation failed", err)
	}
}

// auditDrift 对账：主表冗余 reserved 与流水表未释放预占量是否一致。
//
// 只告警不修正：读路径上做修正要把"查询"变成"写入"，让读请求也去抢写热点。
// 差异留给人/对账任务处置。
func (s *StockService) auditDrift(ctx context.Context, d *model.StockDetail) {
	if d == nil || d.OpenReservedQty == d.Reserved {
		return
	}
	s.logger.WarnContext(ctx, "stock: reserved counter drifted from reservation ledger",
		logx.Int("product_id", int(d.ProductID)),
		logx.Int("stock.reserved", int(d.Reserved)),
		logx.Int("ledger.open_reserved_qty", int(d.OpenReservedQty)),
		logx.Int("ledger.open_reserve_count", int(d.OpenReserveCount)),
		logx.String("consequence", "a reserve/release/confirm tx may have been half-applied"),
		logx.String("hint", "reconcile stock_reservations against stocks.reserved for this product"))
}

// bumpFail 预占失败计数（kvx 原子自增 + 窗口 TTL）
func (s *StockService) bumpFail(ctx context.Context, productID uint64) {
	if s.kv == nil || productID <= 0 {
		return
	}
	key := failCounterKey(productID)
	n, err := s.kv.Incr(ctx, key)
	if err != nil {
		return // 计数失败不影响业务：熔断只是削峰手段，不是正确性依赖
	}
	if n == 1 {
		// 首次创建时补 TTL：Incr 无法带过期时间，不补则计数键永久堆积
		_ = s.kv.Set(ctx, key, "1", failCounterWindow)
	}
}

// tripped 熔断判定：当前窗口失败数是否已达上限
func (s *StockService) tripped(ctx context.Context, productID uint64) bool {
	if s.kv == nil || productID <= 0 {
		return false
	}
	v, err := s.kv.Get(ctx, failCounterKey(productID))
	if err != nil || v == "" {
		return false
	}
	var n int64
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return false
	}
	return n >= failCounterCap
}

// invalidate 删库存快照缓存（写路径必须失效，否则读缓存会拿到旧值）
func (s *StockService) invalidate(ctx context.Context, productID uint64) {
	if s.cache == nil {
		return
	}
	if err := s.cache.Delete(ctx, stockCacheKey(productID)); err != nil {
		s.logger.WarnContext(ctx, "stock: cache invalidate failed", logx.Err(err))
	}
}

// traceparentOf 取当前链路的 W3C traceparent（随 outbox header 透传）。
// 仅当调用链开时经 otel 全局传播器注入；无活动 span 时为空串。
func (s *StockService) traceparentOf(ctx context.Context) string {
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

func stockCacheKey(productID uint64) string {
	return fmt.Sprintf("mall:stock:%d", productID)
}

// failCounterKey 熔断计数键：按商品 + 窗口序号分桶，窗口自然滚动
func failCounterKey(productID uint64) string {
	return fmt.Sprintf("stock:fail:%d:%d", productID, time.Now().UnixNano()/failCounterWindow.Nanoseconds())
}
