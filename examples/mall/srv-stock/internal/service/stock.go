// Package service srv-stock 业务编排层。
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
// DB / Cache / KV / Lock / MQ 全部由 app.Setup 从 launchx 的 App 句柄注入。
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
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/gospacex/launchx/examples/mall/pkg/codes"
	"github.com/gospacex/launchx/examples/mall/pkg/events"
	"github.com/gospacex/launchx/examples/mall/pkg/tracex"
	"github.com/gospacex/launchx/examples/mall/srv-stock/internal/repository"

	stockpb "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen/stock"
)

// stockLockTTL 预占互斥的持锁上限。
//
// 取 5s：覆盖"加锁 → 3 表事务 → 解锁"的最坏耗时，且与 lockx 配置里的 10s
// TTL 形成双保险 —— 进程在解锁前崩溃时锁也会自动过期，不会永久锁死该商品。
const stockLockTTL = 5 * time.Second

// stockCacheTTL 库存快照缓存时长（预占/调整成功后 Delete 失效）
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

// Repo 暴露仓储（组装期健康检查 / 对账用）
func (s *StockService) Repo() *repository.StockRepository { return s.repo }

// GetStock 查库存快照（cachex 优先，miss 回源 DB 并回填）
//
// 缓存 TTL 刻意短（30s）：库存是强一致敏感数据，缓存只是削峰手段，
// 所有写路径（预占/释放/确认/调整）都会显式 Delete，双保险。
func (s *StockService) GetStock(ctx context.Context, req *stockpb.GetStockReq) (*stockpb.GetStockResp, error) {
	resp := &stockpb.GetStockResp{ProductId: req.GetProductId()}
	if req.GetProductId() <= 0 {
		c := codes.Fail(codes.CodeInvalidParam, "product_id required")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	key := stockCacheKey(req.GetProductId())
	if s.cache != nil {
		if cached, err := s.cache.Get(ctx, key); err == nil && cached != "" {
			var st repository.Stock
			if json.Unmarshal([]byte(cached), &st) == nil {
				resp.Stock = toPbStock(&st)
				resp.Code, resp.Message = codes.OK().Code, codes.OK().Message
				return resp, nil
			}
			s.logger.WarnContext(ctx, "stock cache decode failed, fallback to db", logx.String("key", key))
		}
	}
	st, err := s.repo.GetStock(ctx, uint64(req.GetProductId()))
	if err != nil {
		resp.Code, resp.Message = s.mapRepoErr(ctx, err)
		return resp, nil
	}
	if s.cache != nil {
		if b, mErr := json.Marshal(st); mErr == nil {
			if sErr := s.cache.Set(ctx, key, string(b), stockCacheTTL); sErr != nil {
				s.logger.WarnContext(ctx, "stock cache fill failed", logx.Err(sErr))
			}
		}
	}
	resp.Stock = toPbStock(st)
	resp.Code, resp.Message = codes.OK().Code, codes.OK().Message
	return resp, nil
}

// ListStocks 单表分页（不走缓存：批量读命中率低，缓存反而增加失效面）
func (s *StockService) ListStocks(ctx context.Context, req *stockpb.ListStocksReq) (*stockpb.ListStocksResp, error) {
	resp := &stockpb.ListStocksResp{}
	rows, err := s.repo.ListStocks(ctx, int(req.GetLimit()), int(req.GetOffset()))
	if err != nil {
		c := codes.Fail(codes.CodeInternal, err.Error())
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	out := make([]*stockpb.StockInfo, 0, len(rows))
	for i := range rows {
		out = append(out, toPbStock(&rows[i]))
	}
	resp.Stocks = out
	resp.Code, resp.Message = codes.OK().Code, codes.OK().Message
	return resp, nil
}

// ReserveStock 预占：熔断计数 → 加锁 → 3 表事务 + outbox → 缓存失效
func (s *StockService) ReserveStock(ctx context.Context, req *stockpb.ReserveStockReq) (*stockpb.ReserveStockResp, error) {
	resp := &stockpb.ReserveStockResp{ProductId: req.GetProductId()}
	if req.GetProductId() <= 0 || req.GetQuantity() <= 0 || req.GetOrderNo() == "" {
		c := codes.Fail(codes.CodeInvalidParam, "product_id/quantity/order_no required")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	// 熔断前置：一个窗口内失败过多的商品直接快速失败，避免每次都走一遍
	// 加锁 + 事务（超卖时这个路径会被打成热点）。
	if s.tripped(ctx, req.GetProductId()) {
		c := codes.Fail(codes.CodeStockInsufficient, "stock sold out (circuit tripped)")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}

	unlock, deny := s.withProductLock(ctx, req.GetProductId())
	if deny != nil {
		resp.Code, resp.Message = deny.Code, deny.Message
		return resp, nil
	}
	defer unlock()

	tp := s.traceparentOf(ctx)
	out, err := s.repo.ReserveTx(ctx, repository.ReserveInput{
		OrderNo:   req.GetOrderNo(),
		ProductID: uint64(req.GetProductId()),
		Quantity:  req.GetQuantity(),
		UserID:    uint64(req.GetUserId()),
		OutboxFill: func(reservationID uint64) (payload, traceparent string) {
			ev := events.StockReserved{
				OrderNo:     req.GetOrderNo(),
				UserID:      req.GetUserId(),
				ProductID:   req.GetProductId(),
				Quantity:    req.GetQuantity(),
				Success:     true,
				Available:   0, // 事务内尚未回读，由消费侧以查询为准（避免事件里带推测值）
				Reservation: int64(reservationID),
				SchemaVer:   events.SchemaVer1,
				TraceID:     evTraceID(ctx),
				OccurredAt:  time.Now().UnixMilli(),
			}
			b, mErr := events.Marshal(ev)
			if mErr != nil {
				s.logger.WarnContext(ctx, "stock event marshal failed", logx.Err(mErr))
				return "", tp
			}
			return string(b), tp
		},
	})
	if err != nil {
		s.bumpFail(ctx, req.GetProductId())
		resp.Code, resp.Message = s.mapRepoErr(ctx, err)
		return resp, nil
	}
	s.invalidate(ctx, req.GetProductId())
	resp.Reserved = req.GetQuantity()
	resp.ReservationId = int64(out.ReservationID)
	resp.Code, resp.Message = codes.OK().Code, codes.OK().Message
	return resp, nil
}

// ReleaseStock 释放预占（幂等：流水不存在按已释放处理）
func (s *StockService) ReleaseStock(ctx context.Context, req *stockpb.ReleaseStockReq) (*stockpb.ReleaseStockResp, error) {
	resp := &stockpb.ReleaseStockResp{ProductId: req.GetProductId()}
	if req.GetProductId() <= 0 || req.GetQuantity() <= 0 || req.GetOrderNo() == "" {
		c := codes.Fail(codes.CodeInvalidParam, "product_id/quantity/order_no required")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	available, err := s.repo.ReleaseTx(ctx, repository.ReleaseInput{
		OrderNo:   req.GetOrderNo(),
		ProductID: uint64(req.GetProductId()),
		Quantity:  req.GetQuantity(),
	})
	if err != nil {
		resp.Code, resp.Message = s.mapRepoErr(ctx, err)
		return resp, nil
	}
	s.invalidate(ctx, req.GetProductId())
	resp.Available = available
	resp.Code, resp.Message = codes.OK().Code, codes.OK().Message
	return resp, nil
}

// ConfirmStock 确认预占（真正出库：扣 total）
func (s *StockService) ConfirmStock(ctx context.Context, req *stockpb.ConfirmStockReq) (*stockpb.ConfirmStockResp, error) {
	resp := &stockpb.ConfirmStockResp{ProductId: req.GetProductId()}
	if req.GetProductId() <= 0 || req.GetQuantity() <= 0 || req.GetOrderNo() == "" {
		c := codes.Fail(codes.CodeInvalidParam, "product_id/quantity/order_no required")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	total, available, err := s.repo.ConfirmTx(ctx, repository.ConfirmInput{
		OrderNo:   req.GetOrderNo(),
		ProductID: uint64(req.GetProductId()),
		Quantity:  req.GetQuantity(),
	})
	if err != nil {
		resp.Code, resp.Message = s.mapRepoErr(ctx, err)
		return resp, nil
	}
	s.invalidate(ctx, req.GetProductId())
	resp.Total = total
	resp.Available = available
	resp.Code, resp.Message = codes.OK().Code, codes.OK().Message
	return resp, nil
}

// AdjustStock 入库 / 盘点（2 表事务）
func (s *StockService) AdjustStock(ctx context.Context, req *stockpb.AdjustStockReq) (*stockpb.AdjustStockResp, error) {
	resp := &stockpb.AdjustStockResp{ProductId: req.GetProductId()}
	if req.GetOperatorId() <= 0 {
		c := codes.Fail(codes.CodeUnauthorized, "operator required")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	if req.GetProductId() <= 0 || req.GetDelta() == 0 {
		c := codes.Fail(codes.CodeInvalidParam, "product_id/delta invalid")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	total, available, err := s.repo.AdjustTx(ctx, repository.AdjustInput{
		ProductID:  uint64(req.GetProductId()),
		Delta:      req.GetDelta(),
		Reason:     req.GetReason(),
		OperatorID: uint64(req.GetOperatorId()),
	})
	if err != nil {
		resp.Code, resp.Message = s.mapRepoErr(ctx, err)
		return resp, nil
	}
	s.invalidate(ctx, req.GetProductId())
	resp.Total = total
	resp.Available = available
	resp.Code, resp.Message = codes.OK().Code, codes.OK().Message
	return resp, nil
}

// Report 3 表 JOIN 报表
func (s *StockService) Report(ctx context.Context, req *stockpb.ReportReq) (*stockpb.ReportResp, error) {
	resp := &stockpb.ReportResp{}
	rows, err := s.repo.Report(ctx, int(req.GetLimit()))
	if err != nil {
		c := codes.Fail(codes.CodeInternal, err.Error())
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	pbRows := make([]*stockpb.StockReportRow, 0, len(rows))
	for i := range rows {
		pbRows = append(pbRows, &stockpb.StockReportRow{
			ProductId:        rows[i].ProductID,
			Total:            rows[i].Total,
			Available:        rows[i].Available,
			Reserved:         rows[i].Reserved,
			ReservationCount: rows[i].ReservationCount,
			LogCount:         rows[i].LogCount,
		})
	}
	resp.Rows = pbRows
	resp.Code, resp.Message = codes.OK().Code, codes.OK().Message
	return resp, nil
}

// ---- 内部 ----

// withProductLock 取商品维度互斥锁，返回解锁闭包。
//
// 锁器为 nil（组件未装配 / 构造降级）时**显式 fail-open 并留 Warn**，且必须
// 在日志里说清后果：这是本示例唯一一处"降级即可能超卖"的路径，
// 静默继续会让事后无法回答"当时有没有加锁"。
func (s *StockService) withProductLock(ctx context.Context, productID int64) (func(), *codes.Code) {
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
		c := codes.Fail(codes.CodeStockLockFailed, "concurrent reserve in progress, retry later")
		return noop, &c
	}
	return func() {
		if uErr := lk.Unlock(context.Background()); uErr != nil {
			s.logger.WarnContext(ctx, "stock: unlock failed (lock will expire by ttl)", logx.Err(uErr))
		}
	}, nil
}

// mapRepoErr 把持久层哨兵错误翻译成业务码。
//
// 翻译集中一处是刻意的：4xxx 各码的语义差异（没记录 vs 不够扣 vs 重复释放）
// 一旦散落到各个方法里，必然出现"某条路径把 4001 报成 4002"。
func (s *StockService) mapRepoErr(ctx context.Context, err error) (int64, string) {
	switch {
	case errors.Is(err, repository.ErrNotFound):
		c := codes.Fail(codes.CodeStockNotFound, "stock record not found")
		return c.Code, c.Message
	case errors.Is(err, repository.ErrInsufficient):
		c := codes.Fail(codes.CodeStockInsufficient, "stock insufficient")
		return c.Code, c.Message
	case errors.Is(err, repository.ErrReservationNF):
		// 幂等释放/重复确认：按成功处理（已经释放/确认过了）
		c := codes.Fail(codes.CodeStockReservationNF, "reservation not found, nothing to do")
		return c.Code, c.Message
	case errors.Is(err, repository.ErrOverRelease):
		c := codes.Fail(codes.CodeStockOverRelease, "quantity exceeds reservation")
		return c.Code, c.Message
	default:
		s.logger.WarnContext(ctx, "stock: repository error", logx.Err(err))
		c := codes.Fail(codes.CodeStockReserveFailed, "stock operation failed")
		return c.Code, c.Message
	}
}

// bumpFail 预占失败计数（kvx 原子自增 + 窗口 TTL）
func (s *StockService) bumpFail(ctx context.Context, productID int64) {
	if s.kv == nil || productID <= 0 {
		return
	}
	key := fmt.Sprintf("stock:fail:%d:%d", productID, time.Now().UnixNano()/failCounterWindow.Nanoseconds())
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
func (s *StockService) tripped(ctx context.Context, productID int64) bool {
	if s.kv == nil || productID <= 0 {
		return false
	}
	key := fmt.Sprintf("stock:fail:%d:%d", productID, time.Now().UnixNano()/failCounterWindow.Nanoseconds())
	v, err := s.kv.Get(ctx, key)
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
func (s *StockService) invalidate(ctx context.Context, productID int64) {
	if s.cache == nil {
		return
	}
	if err := s.cache.Delete(ctx, stockCacheKey(productID)); err != nil {
		s.logger.WarnContext(ctx, "stock: cache invalidate failed", logx.Err(err))
	}
}

// traceparentOf 取当前链路的 W3C traceparent（随 outbox header 透传）
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

func stockCacheKey(productID int64) string {
	return fmt.Sprintf("mall:stock:%d", productID)
}

func toPbStock(st *repository.Stock) *stockpb.StockInfo {
	if st == nil {
		return nil
	}
	return &stockpb.StockInfo{
		ProductId: int64(st.ProductID),
		Total:     st.Total,
		Available: st.Available,
		Reserved:  st.Reserved,
		Version:   st.Version,
	}
}

// tracexStart 业务层埋点（保留一个内部别名，避免 service 包各处重复取 tracer）
var tracexStart = tracex.Start
