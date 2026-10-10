// Package service ★ 唯一允许出现业务判断的层。
//
// 本域的句柄面：db（经 repository 注入，硬前提）+ kv（扣减旁路计数）+ lock
// （同商品扣减互斥）。kv / lock / logger 允许为 nil，各有降级路径。
//
// 两条路径：
//
//	GetStock —— 只读 RPC 路径（IDL 的 GetStock 落到这里）
//	Decrease —— **内部写路径，只由单测驱动**（见下方注释）
//
// Decrease 存在的理由：让「同商品扣减互斥 + 条件扣减不超卖」有可执行的判据。
// 它**不是**一条留有隐藏入口的生产写路径 —— IDL 的 ReserveStock 一族在 handler
// 显式 Unimplemented，rpc 面不暴露它。
package service

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/gospacex/kvx"
	"github.com/gospacex/lockx"
	"github.com/gospacex/logx"
	"github.com/gospacex/otelx/layer"
	"gorm.io/gorm"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/stock/model"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
)

// decreaseKeyTTL 当日扣减计数键的存活时间。
//
// 取 48h（跨到次日仍有窗口读昨日的计数），与 middleware/ratelimit.go 同款手法：
// 只在键首次建立（Incr 返回 1）时 Set 挂过期，避免当日键无限堆积。
const decreaseKeyTTL = 48 * time.Hour

// stockRepo 是 service 真正依赖的窄接口：只含它调用的两个方法。
//
// 用接口而不是 *repository.StockRepository：service 只关心「读 / 条件扣减」的
// 语义，不关心句柄从哪来；这也让业务判断能脱离真实库被直接驱动到。
// *repository.StockRepository 天然满足它，装配层无需额外适配。
type stockRepo interface {
	FindByProductID(ctx context.Context, productID uint64) (*model.Stock, error)
	TryDecrease(ctx context.Context, productID uint64, n int32) (bool, error)
}

// StockService 库存业务。
type StockService struct {
	repo    stockRepo
	kv      kvx.KVStore
	lock    lockx.Locker
	logger  logx.Logger
	traceOn bool // trace.enabled（总闸），由装配层注入
}

// New 注入仓储与句柄；kv / lock / logger 允许为 nil（各有降级路径）。
func New(repo stockRepo, kv kvx.KVStore, lock lockx.Locker, logger logx.Logger, traceOn bool) *StockService {
	return &StockService{repo: repo, kv: kv, lock: lock, logger: logger, traceOn: traceOn}
}

// StockView 查库存出参（service 自己的类型，与 RPC 结构解耦）。
type StockView struct {
	Stock model.Stock
}

// GetStock 查库存（IDL 的 GetStock 落到这里）。
//
// 「无库存记录」视为零值视图而非错误：读接口不该因为「该商品还没建库存行」而
// 失败 —— available=0 在业务上就是扣不动。与 points.GetPoints「未开户 = 0」同构，
// 也是本域与 product.GetProduct（未命中回 ProductNotFound）的刻意差异。
func (s *StockService) GetStock(ctx context.Context, productID uint64) (v *StockView, err error) {
	ctx, end := layer.Begin(ctx, "service", "stock", "GetStock", s.traceOn, nil)
	defer func() { end(err) }()

	if productID == 0 {
		return nil, codes.New(codes.InvalidArgument, "product_id must be positive")
	}

	st, err := s.repo.FindByProductID(ctx, productID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return &StockView{Stock: model.Stock{ProductID: productID}}, nil
		}
		return nil, codes.Wrap(codes.Internal, "get stock", err)
	}
	return &StockView{Stock: *st}, nil
}

// Decrease 扣减库存（内部写路径，只由单测驱动）。
//
// 流程：取锁（nil 则 Warn 降级）→ 库层条件扣减 → kv.Incr 记当日扣减量。
//
// **扣减的真相在库层的条件 UPDATE**（见 repository.TryDecrease）：即使本次没
// 走锁路径，条件 UPDATE 本身也不会让 available 落到负数。锁的作用是让「同商品
// 扣减互斥」—— 减少条件 UPDATE 的失败重试，并把旁路计数的并发写入收敛；它**不是**
// 不超卖的必要条件，缺它会 Warn 但不会让调用失败。
//
// kv 只是旁路计数：KV 为 nil 或 Incr 报错都只 Warn，**不影响扣减结果**。
//
// 返回值 remaining 是扣减后**观测**到的剩余可售量（best-effort）：读失败只 Warn
// 并回 0，不回退已成功的扣减 —— 否则一次观测读的抖动会让调用方以为没扣成功而
// 重试，反而多扣。
func (s *StockService) Decrease(ctx context.Context, productID uint64, n int32) (remaining int32, err error) {
	ctx, end := layer.Begin(ctx, "service", "stock", "Decrease", s.traceOn, nil)
	defer func() { end(err) }()

	if productID == 0 {
		return 0, codes.New(codes.InvalidArgument, "product_id must be positive")
	}
	if n <= 0 {
		return 0, codes.New(codes.InvalidArgument, "quantity must be positive")
	}

	unlock, err := s.acquire(ctx, productID)
	if err != nil {
		return 0, err
	}
	if unlock != nil {
		defer unlock()
	}

	ok, err := s.repo.TryDecrease(ctx, productID, n)
	if err != nil {
		return 0, codes.Wrap(codes.Internal, "decrease stock", err)
	}
	if !ok {
		// 条件 UPDATE 影响 0 行：库存不足 / 无该商品。这是 StockNotEnough 的真实生产者。
		return 0, codes.New(codes.StockNotEnough, "stock not enough for product "+strconv.FormatUint(productID, 10))
	}

	s.recordDecrease(ctx, productID)
	return s.observeRemaining(ctx, productID), nil
}

// acquire 取锁；返回 nil unlock 表示本次未走锁路径。
//
// 句柄缺失时 fail-open，**但必须说出来** —— 「锁没生效但没人报错」是超卖类故障
// 最难排查的形态。锁缺失不使调用失败（条件 UPDATE 仍是不超卖的护栏）。
func (s *StockService) acquire(ctx context.Context, productID uint64) (func(), error) {
	if s.lock == nil {
		s.warn("mesh-stock: lock unavailable, running without mutex",
			logx.String("consequence", "同商品并发扣减不再互斥"),
			logx.String("probe", "检查 xmiddleware.lockx 段"))
		return nil, nil
	}
	lk, err := s.lock.Lock(ctx, "stock:product:"+strconv.FormatUint(productID, 10))
	if err != nil {
		// 取不到锁时必须出声失败，不许静默降级续跑 —— 那等于假装加了锁。
		return nil, codes.Wrap(codes.Internal, "acquire lock", err)
	}
	return func() {
		if rerr := lk.Unlock(ctx); rerr != nil {
			s.warn("mesh-stock: unlock failed, ttl will expire it", logx.Err(rerr))
		}
	}, nil
}

// recordDecrease 记一次「当日该商品被扣减」的旁路计数。失败只 Warn，不影响扣减结果。
func (s *StockService) recordDecrease(ctx context.Context, productID uint64) {
	if s.kv == nil {
		s.warn("mesh-stock: kv unavailable, decrease counter skipped",
			logx.String("consequence", "当日扣减次数不计数"),
			logx.String("probe", "检查 xmiddleware.kvx 段"))
		return
	}
	key := s.decreaseKey(productID)
	n, err := s.kv.Incr(ctx, key)
	if err != nil {
		s.warn("mesh-stock: decrease counter incr failed", logx.Err(err))
		return
	}
	if n == 1 {
		// 首次计数才挂过期：每次 Incr 都 Set 会让当日键永不滚动（同
		// middleware/ratelimit.go 的理由）。kvx 没有 Expire / IncrBy，带 TTL 的
		// 原语只有 Set，故用 Set 写回同一计数并顺带设 TTL。
		if serr := s.kv.Set(ctx, key, "1", decreaseKeyTTL); serr != nil {
			s.warn("mesh-stock: set decrease counter ttl failed", logx.Err(serr))
		}
	}
}

// decreaseKey 当日扣减计数键：mesh:stock:dec:<productID>:<yyyymmdd>。
// 命名空间与限流器（mesh:stock:rl）刻意分开，避免计数互相污染。
func (s *StockService) decreaseKey(productID uint64) string {
	return "mesh:stock:dec:" + strconv.FormatUint(productID, 10) + ":" + time.Now().Format("20060102")
}

// observeRemaining 扣减后回读剩余可售量（仅观测，失败只 Warn 并回 0）。
func (s *StockService) observeRemaining(ctx context.Context, productID uint64) int32 {
	st, err := s.repo.FindByProductID(ctx, productID)
	if err != nil {
		s.warn("mesh-stock: observe remaining failed, decrement already committed",
			logx.Err(err))
		return 0
	}
	return st.Available
}

// warn 统一收口 logger 判空：New 允许传 nil logger，业务路径不该因日志缺失而 panic。
func (s *StockService) warn(msg string, fields ...logx.Field) {
	if s.logger != nil {
		s.logger.Warn(msg, fields...)
	}
}
