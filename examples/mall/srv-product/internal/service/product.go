// Package service srv-product 业务编排层。
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
	"github.com/gospacex/launchx/examples/mall/srv-product/internal/model"
	"github.com/gospacex/launchx/examples/mall/srv-product/internal/repository"
	"github.com/gospacex/otelx/layer"
)

// productLockTTL 上下架互斥的持锁上限。
//
// 取 5s（远大于单事务耗时）而非配置化：这是"防并发改状态互相覆盖"的上限，
// 不是地址、也不是业务阈值。与 lockx 配置里的 10s TTL 形成双保险 ——
// 进程在解锁前崩溃时，锁也会在 TTL 内自动过期，不会永久锁死该商品。
const productLockTTL = 5 * time.Second

// productCacheTTL 商品详情缓存时长（改价/上下架时显式 Delete 失效）
const productCacheTTL = 10 * time.Minute

// ProductService 商品业务编排
type ProductService struct {
	repo *repository.ProductRepository
	// cache 商品详情缓存（nil 表示缓存组件未装配/已降级 → 直查 DB）
	cache cachex.Cache
	// kv 限流与热度计数（nil 表示 KV 组件未装配 → 计数降级为 no-op）
	kv kvx.KVStore
	// lock 上下架互斥（nil 表示锁组件未装配 → fail-open，见 withProductLock）
	lock    lockx.Locker
	traceOn bool
	logger  logx.Logger
}

// NewProductService 构造（cache / kv / lock 允许 nil，见字段注释）
func NewProductService(
	repo *repository.ProductRepository,
	cache cachex.Cache,
	kv kvx.KVStore,
	lock lockx.Locker,
	traceOn bool,
	logger logx.Logger,
) *ProductService {
	if logger == nil {
		logger = logx.GetGlobal()
	}
	return &ProductService{repo: repo, cache: cache, kv: kv, lock: lock, traceOn: traceOn, logger: logger}
}

// Repo 暴露仓储（pkg/handlecheck 断言 / 对账用）
func (s *ProductService) Repo() *repository.ProductRepository { return s.repo }

// begin 开 service 层 span，返回「收 error 再收尾」的闭包。
//
// 不做 quiet 豁免：service 返回非 nil error 意味着这次业务操作没完成
// （参数非法 / 写库失败 / 下游失败），都该在 Jaeger 上看得见 —— 与单体
// 示例 s.end 的口径一致。预期内的业务拒绝由 handler 翻成响应体 Code，
// 不进 error，因此不会把正常的「商品不存在」标成故障。
func (s *ProductService) begin(ctx context.Context, op string) (context.Context, layer.End) {
	return layer.Begin(ctx, "service", "product", op, s.traceOn, nil)
}

// CreateProduct 建商品：校验 → 多表事务 → outbox 事件
func (s *ProductService) CreateProduct(ctx context.Context, cmd model.CreateProductCmd) (out *model.CreateProductResult, err error) {
	ctx, end := s.begin(ctx, "CreateProduct")
	defer func() { end(err) }()

	if cmd.OperatorID <= 0 {
		return nil, codes.New(codes.CodeUnauthorized, "operator required")
	}
	if cmd.Title == "" {
		return nil, codes.New(codes.CodeProductTitleInvalid, "title required")
	}
	if cmd.Price < 0 || cmd.CategoryID <= 0 {
		return nil, codes.New(codes.CodeInvalidParam, "price or category invalid")
	}

	// 类目存在性校验走单表查询：跨域只存 ID，没有跨库外键可依赖
	if _, err := s.repo.GetCategory(ctx, cmd.CategoryID); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, codes.Wrap(codes.CodeCategoryNotFound, "category not found", err)
		}
		return nil, codes.Wrap(codes.CodeInternal, "load category failed", err)
	}

	tp := s.traceparentOf(ctx)
	rec, err := s.repo.CreateProductTx(ctx, repository.CreateProductInput{
		Title:      cmd.Title,
		Price:      cmd.Price,
		CategoryID: cmd.CategoryID,
		OperatorID: cmd.OperatorID,
		InitStock:  cmd.InitStock,
		OutboxFill: func(productID uint64) (payload, traceparent string) {
			ev := events.ProductCreated{
				ProductID:  int64(productID),
				Title:      cmd.Title,
				CategoryID: int64(cmd.CategoryID),
				Price:      cmd.Price,
				OperatorID: int64(cmd.OperatorID),
				SchemaVer:  events.SchemaVer1,
				TraceID:    evTraceID(ctx),
				OccurredAt: time.Now().UnixMilli(),
			}
			b, mErr := events.Marshal(ev)
			if mErr != nil {
				// 序列化失败不阻断建商品：空 payload 时 Runner 直接放弃该行
				s.logger.WarnContext(ctx, "product event marshal failed", logx.Err(mErr))
				return "", tp
			}
			return string(b), tp
		},
	})
	if err != nil {
		s.logger.WarnContext(ctx, "create product tx failed", logx.Err(err))
		return nil, codes.Wrap(codes.CodeProductWriteFailed, "create product failed", err)
	}
	if span := trace.SpanFromContext(ctx); span != nil && span.IsRecording() {
		span.SetAttributes(attribute.Int64("product.id", int64(rec.ProductID)))
	}
	return &model.CreateProductResult{ProductID: rec.ProductID, SkuCount: rec.SkuCount}, nil
}

// GetProduct 查商品详情（cachex 优先，miss 回源 DB 并回填；命中路径记热度）
//
// 热度计数走 kvx 而非 cachex：它需要原子自增，且**不要求强一致** —— 丢了
// 一次计数只是热度偏低，不影响正确性；反过来若为了它去动 cachex 的缓存值，
// 读-改-写会在并发下互相覆盖。两者因此分工明确。
func (s *ProductService) GetProduct(ctx context.Context, q model.GetProductQuery) (out *model.ProductDetail, err error) {
	ctx, end := s.begin(ctx, "GetProduct")
	defer func() { end(err) }()

	if q.ProductID <= 0 {
		return nil, codes.New(codes.CodeInvalidParam, "product_id required")
	}
	key := productCacheKey(q.ProductID)
	s.bumpHot(ctx, q.ProductID)

	if s.cache != nil {
		if cached, err := s.cache.Get(ctx, key); err == nil && cached != "" {
			var d model.ProductDetail
			if json.Unmarshal([]byte(cached), &d) == nil {
				if span := trace.SpanFromContext(ctx); span != nil && span.IsRecording() {
					span.SetAttributes(attribute.Bool("cache.hit", true))
				}
				return &d, nil
			}
			s.logger.WarnContext(ctx, "product cache decode failed, fallback to db", logx.String("key", key))
		}
	}

	d, err := s.repo.GetProduct(ctx, q.ProductID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, codes.Wrap(codes.CodeProductNotFound, "product not found", err)
		}
		return nil, codes.Wrap(codes.CodeInternal, "load product failed", err)
	}
	if s.cache != nil {
		if b, mErr := json.Marshal(d); mErr == nil {
			if sErr := s.cache.Set(ctx, key, string(b), productCacheTTL); sErr != nil {
				s.logger.WarnContext(ctx, "product cache fill failed", logx.Err(sErr))
			}
		}
	}
	return d, nil
}

// UpdateProduct 改商品（2 表事务 + 缓存失效）
func (s *ProductService) UpdateProduct(ctx context.Context, cmd model.UpdateProductCmd) (err error) {
	ctx, end := s.begin(ctx, "UpdateProduct")
	defer func() { end(err) }()

	if cmd.OperatorID <= 0 {
		return codes.New(codes.CodeUnauthorized, "operator required")
	}
	if cmd.ProductID <= 0 || cmd.Title == "" || cmd.Price < 0 || cmd.CategoryID <= 0 {
		return codes.New(codes.CodeInvalidParam, "invalid update param")
	}
	if err := s.repo.UpdateProductTx(ctx, repository.UpdateProductInput{
		ProductID:  cmd.ProductID,
		Title:      cmd.Title,
		Price:      cmd.Price,
		CategoryID: cmd.CategoryID,
		OperatorID: cmd.OperatorID,
	}); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return codes.Wrap(codes.CodeProductNotFound, "product not found", err)
		}
		s.logger.WarnContext(ctx, "update product tx failed", logx.Err(err))
		return codes.Wrap(codes.CodeProductWriteFailed, "update product failed", err)
	}
	s.invalidate(ctx, cmd.ProductID)
	return nil
}

// UpdateStatus 上下架（商品维度加锁 + 3 表事务 + 缓存失效）
//
// 幂等：已是目标态返回 CodeProductStatusDup。这是**成功**语义而非错误 ——
// 重复点击不该报内部错误，也不该再记一条审计日志。
func (s *ProductService) UpdateStatus(ctx context.Context, cmd model.UpdateStatusCmd) (out *model.UpdateStatusResult, err error) {
	ctx, end := s.begin(ctx, "UpdateStatus")
	defer func() { end(err) }()

	if cmd.OperatorID <= 0 {
		return nil, codes.New(codes.CodeUnauthorized, "operator required")
	}
	if cmd.ProductID <= 0 || (cmd.Status != model.StatusOnSale && cmd.Status != model.StatusOffShelf) {
		return nil, codes.New(codes.CodeInvalidParam, "invalid status param")
	}

	unlock, deny := s.withProductLock(ctx, cmd.ProductID)
	if deny != nil {
		return nil, deny
	}
	defer unlock()

	changed, err := s.repo.UpdateStatusTx(ctx, repository.UpdateStatusInput{
		ProductID:  cmd.ProductID,
		Status:     cmd.Status,
		OperatorID: cmd.OperatorID,
	})
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, codes.Wrap(codes.CodeProductNotFound, "product not found", err)
		}
		s.logger.WarnContext(ctx, "update product status tx failed", logx.Err(err))
		return nil, codes.Wrap(codes.CodeProductWriteFailed, "update status failed", err)
	}
	if !changed {
		return nil, codes.New(codes.CodeProductStatusDup, "product already in target status")
	}
	s.invalidate(ctx, cmd.ProductID)
	return &model.UpdateStatusResult{Changed: true}, nil
}

// ListProducts 商品列表（2 表联查：products ⋈ categories）
//
// 不走缓存：列表随上下架实时变化，缓存只会拖慢一致性。
func (s *ProductService) ListProducts(ctx context.Context, q model.ListProductsQuery) (out []model.ProductDetail, err error) {
	ctx, end := s.begin(ctx, "ListProducts")
	defer func() { end(err) }()
	rows, err := s.repo.ListProducts(ctx, q)
	if err != nil {
		return nil, codes.Wrap(codes.CodeInternal, "list products failed", err)
	}
	return rows, nil
}

// Report 4 表 JOIN 报表
func (s *ProductService) Report(ctx context.Context, q model.ProductReportQuery) (out []model.ReportRow, err error) {
	ctx, end := s.begin(ctx, "Report")
	defer func() { end(err) }()
	rows, err := s.repo.Report(ctx, q)
	if err != nil {
		return nil, codes.Wrap(codes.CodeInternal, "report products failed", err)
	}
	return rows, nil
}

// ---- 内部 ----

// withProductLock 取商品维度互斥锁，返回解锁闭包。
//
// 锁器为 nil（组件未装配 / 构造降级）时**显式 fail-open 并留 Warn**：
// 这种降级不会报错，只表现为"极高并发下同一商品的上下架可能互相覆盖"，
// 若静默继续，事后无法回答"当时有没有加锁"。返回非 nil error 表示拒绝本次请求。
func (s *ProductService) withProductLock(ctx context.Context, productID uint64) (func(), *codes.Error) {
	noop := func() {}
	if s.lock == nil {
		s.logger.WarnContext(ctx, "product: locker unavailable, update-status runs without mutual exclusion",
			logx.Int("product_id", int(productID)),
			logx.String("consequence", "concurrent status updates may overwrite each other"),
			logx.String("hint", "check xmiddleware.lockx in config.yaml"))
		return noop, nil
	}
	lk, err := s.lock.Lock(ctx, fmt.Sprintf("product:status:%d", productID), lockx.WithTTL(productLockTTL))
	if err != nil {
		// 取锁失败（含重试耗尽）≠ 业务失败：明确告知调用方可重试
		return noop, codes.Wrap(codes.CodeInternal, "concurrent update in progress, retry later", err)
	}
	return func() {
		// 解锁必须用请求 ctx：传 Background 会让 release span 失去父节点，
		// 每次解锁在 Jaeger 上另起一根孤儿单 span trace（见 order 域同款注释）。
		if uErr := lk.Unlock(ctx); uErr != nil {
			s.logger.WarnContext(ctx, "product: unlock failed (lock will expire by ttl)", logx.Err(uErr))
		}
	}, nil
}

// bumpHot 商品访问热度计数（kvx 原子自增；KV 缺失时静默降级）。
//
// 与限流计数一样走 kvx，但**不参与业务判定**：计数失败既不告警也不重试，
// 热度只是运营信号，不该在它上面产生故障面。
func (s *ProductService) bumpHot(ctx context.Context, productID uint64) {
	if s.kv == nil || productID <= 0 {
		return
	}
	if _, err := s.kv.Incr(ctx, fmt.Sprintf("hot:product:%d", productID)); err != nil {
		s.logger.WarnContext(ctx, "product: hot counter incr failed", logx.Err(err))
	}
}

// invalidate 删详情缓存（改价/上下架后必须失效，否则读缓存会拿到旧值）
func (s *ProductService) invalidate(ctx context.Context, productID uint64) {
	if s.cache == nil {
		return
	}
	if err := s.cache.Delete(ctx, productCacheKey(productID)); err != nil {
		// 失效失败只告警：下一次读会拿到旧值，但 TTL 兜底（10 分钟）不会永久错
		s.logger.WarnContext(ctx, "product: cache invalidate failed", logx.Err(err))
	}
}

// traceparentOf 取当前链路的 W3C traceparent（随 outbox header 透传）。
// 仅当调用链开时经 otel 全局传播器注入；无活动 span 时为空串。
func (s *ProductService) traceparentOf(ctx context.Context) string {
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

func productCacheKey(productID uint64) string {
	return fmt.Sprintf("mall:product:%d", productID)
}
