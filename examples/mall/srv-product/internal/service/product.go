// Package service srv-product 业务编排层。
//
// 三类写操作与它们的并发保护：
//  1. CreateProduct   校验类目 → 3 表事务（products + product_skus + product_logs + outbox）
//  2. UpdateProduct   2 表事务（products + product_logs）→ 删详情缓存
//  3. UpdateStatus    lockx 按商品加锁 → 2 表事务 → 删详情缓存（幂等重放返回 dup）
//
// 句柄来源纪律：本层不 import gorm/redis/kafka，也不用全局单例 ——
// DB / Cache / KV / Lock / MQ 全部由 app.Setup 从 launchx 的 App 句柄注入，
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
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/gospacex/launchx/examples/mall/pkg/codes"
	"github.com/gospacex/launchx/examples/mall/pkg/events"
	"github.com/gospacex/launchx/examples/mall/pkg/tracex"
	"github.com/gospacex/launchx/examples/mall/srv-product/internal/repository"

	productpb "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen/product"
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

// Repo 暴露仓储（组装期健康检查 / 对账用）
func (s *ProductService) Repo() *repository.ProductRepository { return s.repo }

// CreateProduct 建商品：校验 → 3 表事务 → outbox 事件
func (s *ProductService) CreateProduct(ctx context.Context, req *productpb.CreateProductReq) (*productpb.CreateProductResp, error) {
	resp := &productpb.CreateProductResp{}
	if req.GetOperatorId() <= 0 {
		c := codes.Fail(codes.CodeUnauthorized, "operator required")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	if req.GetTitle() == "" {
		c := codes.Fail(codes.CodeProductTitleInvalid, "title required")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	if req.GetPrice() < 0 || req.GetCategoryId() <= 0 {
		c := codes.Fail(codes.CodeInvalidParam, "price or category invalid")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}

	// 类目存在性校验走单表查询：跨域只存 ID，没有跨库外键可依赖
	if _, err := s.repo.GetCategory(ctx, uint64(req.GetCategoryId())); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			c := codes.Fail(codes.CodeCategoryNotFound, "category not found")
			resp.Code, resp.Message = c.Code, c.Message
			return resp, nil
		}
		c := codes.Fail(codes.CodeInternal, err.Error())
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}

	tp := s.traceparentOf(ctx)
	out, err := s.repo.CreateProductTx(ctx, repository.CreateProductInput{
		Title:      req.GetTitle(),
		Price:      req.GetPrice(),
		CategoryID: uint64(req.GetCategoryId()),
		OperatorID: uint64(req.GetOperatorId()),
		InitStock:  int(req.GetInitStock()),
		OutboxFill: func(productID uint64) (payload, traceparent string) {
			ev := events.ProductCreated{
				ProductID:  int64(productID),
				Title:      req.GetTitle(),
				CategoryID: req.GetCategoryId(),
				Price:      req.GetPrice(),
				OperatorID: req.GetOperatorId(),
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
		c := codes.Fail(codes.CodeProductWriteFailed, "create product failed")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}

	resp.ProductId = int64(out.ProductID)
	resp.Code, resp.Message = codes.OK().Code, codes.OK().Message
	return resp, nil
}

// GetProduct 查商品详情（cachex 优先，miss 回源 DB 并回填；命中路径记热度）
//
// 热度计数走 kvx 而非 cachex：它需要原子自增，且**不要求强一致** —— 丢了
// 一次计数只是热度偏低，不影响正确性；反过来若为了它去动 cachex 的缓存值，
// 读-改-写会在并发下互相覆盖。两者因此分工明确。
func (s *ProductService) GetProduct(ctx context.Context, req *productpb.GetProductReq) (*productpb.GetProductResp, error) {
	resp := &productpb.GetProductResp{UserId: req.GetUserId()}
	if req.GetProductId() <= 0 {
		c := codes.Fail(codes.CodeInvalidParam, "product_id required")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	key := productCacheKey(req.GetProductId())
	s.bumpHot(ctx, req.GetProductId())

	if s.cache != nil {
		if cached, err := s.cache.Get(ctx, key); err == nil && cached != "" {
			var d repository.ProductDetail
			if json.Unmarshal([]byte(cached), &d) == nil {
				return fillProductResp(resp, &d), nil
			}
			s.logger.WarnContext(ctx, "product cache decode failed, fallback to db", logx.String("key", key))
		}
	}

	d, err := s.repo.GetProduct(ctx, uint64(req.GetProductId()))
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			c := codes.Fail(codes.CodeProductNotFound, "product not found")
			resp.Code, resp.Message = c.Code, c.Message
			return resp, nil
		}
		c := codes.Fail(codes.CodeInternal, err.Error())
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	if s.cache != nil {
		if b, mErr := json.Marshal(d); mErr == nil {
			if sErr := s.cache.Set(ctx, key, string(b), productCacheTTL); sErr != nil {
				s.logger.WarnContext(ctx, "product cache fill failed", logx.Err(sErr))
			}
		}
	}
	return fillProductResp(resp, d), nil
}

// UpdateProduct 改商品（2 表事务 + 缓存失效）
func (s *ProductService) UpdateProduct(ctx context.Context, req *productpb.UpdateProductReq) (*productpb.UpdateProductResp, error) {
	resp := &productpb.UpdateProductResp{ProductId: req.GetProductId()}
	if req.GetOperatorId() <= 0 {
		c := codes.Fail(codes.CodeUnauthorized, "operator required")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	if req.GetProductId() <= 0 || req.GetTitle() == "" || req.GetPrice() < 0 || req.GetCategoryId() <= 0 {
		c := codes.Fail(codes.CodeInvalidParam, "invalid update param")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	if err := s.repo.UpdateProductTx(ctx, repository.UpdateProductInput{
		ProductID:  uint64(req.GetProductId()),
		Title:      req.GetTitle(),
		Price:      req.GetPrice(),
		CategoryID: uint64(req.GetCategoryId()),
		OperatorID: uint64(req.GetOperatorId()),
	}); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			c := codes.Fail(codes.CodeProductNotFound, "product not found")
			resp.Code, resp.Message = c.Code, c.Message
			return resp, nil
		}
		s.logger.WarnContext(ctx, "update product tx failed", logx.Err(err))
		c := codes.Fail(codes.CodeProductWriteFailed, "update product failed")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	s.invalidate(ctx, req.GetProductId())
	resp.Code, resp.Message = codes.OK().Code, codes.OK().Message
	return resp, nil
}

// UpdateStatus 上下架（商品维度加锁 + 2 表事务 + 缓存失效）
//
// 幂等：已是目标态返回 CodeProductStatusDup。这是**成功**语义而非错误 ——
// 重复点击不该报内部错误，也不该再记一条审计日志。
func (s *ProductService) UpdateStatus(ctx context.Context, req *productpb.UpdateStatusReq) (*productpb.UpdateStatusResp, error) {
	resp := &productpb.UpdateStatusResp{ProductId: req.GetProductId(), Status: req.GetStatus()}
	if req.GetOperatorId() <= 0 {
		c := codes.Fail(codes.CodeUnauthorized, "operator required")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	if req.GetProductId() <= 0 || (req.GetStatus() != repository.StatusOnSale && req.GetStatus() != repository.StatusOffShelf) {
		c := codes.Fail(codes.CodeInvalidParam, "invalid status param")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}

	unlock, deny := s.withProductLock(ctx, req.GetProductId())
	if deny != nil {
		resp.Code, resp.Message = deny.Code, deny.Message
		return resp, nil
	}
	defer unlock()

	changed, err := s.repo.UpdateStatusTx(ctx, repository.UpdateStatusInput{
		ProductID:  uint64(req.GetProductId()),
		Status:     req.GetStatus(),
		OperatorID: uint64(req.GetOperatorId()),
	})
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			c := codes.Fail(codes.CodeProductNotFound, "product not found")
			resp.Code, resp.Message = c.Code, c.Message
			return resp, nil
		}
		s.logger.WarnContext(ctx, "update product status tx failed", logx.Err(err))
		c := codes.Fail(codes.CodeProductWriteFailed, "update status failed")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	if !changed {
		c := codes.Fail(codes.CodeProductStatusDup, "product already in target status")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	s.invalidate(ctx, req.GetProductId())
	resp.Code, resp.Message = codes.OK().Code, codes.OK().Message
	return resp, nil
}

// ListProducts 单表分页（不走缓存：列表随上下架实时变化，缓存只会拖慢一致性）
func (s *ProductService) ListProducts(ctx context.Context, req *productpb.ListProductsReq) (*productpb.ListProductsResp, error) {
	resp := &productpb.ListProductsResp{UserId: req.GetUserId()}
	rows, err := s.repo.ListProducts(ctx, uint64(req.GetCategoryId()), req.GetStatus(), int(req.GetLimit()), int(req.GetOffset()))
	if err != nil {
		c := codes.Fail(codes.CodeInternal, err.Error())
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	resp.Products = toPbProducts(rows)
	resp.Code, resp.Message = codes.OK().Code, codes.OK().Message
	return resp, nil
}

// Report 4 表 JOIN 报表
func (s *ProductService) Report(ctx context.Context, req *productpb.ReportReq) (*productpb.ReportResp, error) {
	resp := &productpb.ReportResp{UserId: req.GetUserId()}
	rows, err := s.repo.Report(ctx, uint64(req.GetCategoryId()), int(req.GetLimit()))
	if err != nil {
		c := codes.Fail(codes.CodeInternal, err.Error())
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	pbRows := make([]*productpb.ProductReportRow, 0, len(rows))
	for i := range rows {
		pbRows = append(pbRows, &productpb.ProductReportRow{
			ProductId:    rows[i].ProductID,
			Title:        rows[i].Title,
			CategoryName: rows[i].CategoryName,
			Price:        rows[i].Price,
			SkuCount:     rows[i].SkuCount,
			OnSaleSkus:   rows[i].OnSaleSkus,
			SoldCount:    rows[i].SoldCount,
		})
	}
	resp.Rows = pbRows
	resp.Code, resp.Message = codes.OK().Code, codes.OK().Message
	return resp, nil
}

// ---- 内部 ----

// withProductLock 取商品维度互斥锁，返回解锁闭包。
//
// 锁器为 nil（组件未装配 / 构造降级）时**显式 fail-open 并留 Warn**：
// 这种降级不会报错，只表现为"极高并发下同一商品的上下架可能互相覆盖"，
// 若静默继续，事后无法回答"当时有没有加锁"。返回值非 nil 时表示拒绝本次请求。
func (s *ProductService) withProductLock(ctx context.Context, productID int64) (func(), *codes.Code) {
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
		c := codes.Fail(codes.CodeInternal, "concurrent update in progress, retry later")
		return noop, &c
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
func (s *ProductService) bumpHot(ctx context.Context, productID int64) {
	if s.kv == nil || productID <= 0 {
		return
	}
	if _, err := s.kv.Incr(ctx, fmt.Sprintf("hot:product:%d", productID)); err != nil {
		s.logger.WarnContext(ctx, "product: hot counter incr failed", logx.Err(err))
	}
}

// invalidate 删详情缓存（改价/上下架后必须失效，否则读缓存会拿到旧值）
func (s *ProductService) invalidate(ctx context.Context, productID int64) {
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

func productCacheKey(productID int64) string {
	return fmt.Sprintf("mall:product:%d", productID)
}

func fillProductResp(resp *productpb.GetProductResp, d *repository.ProductDetail) *productpb.GetProductResp {
	// GetProductResp 没有独立的 product_id 字段：商品 ID 由内嵌的 Product 承载，
	// 调用方统一从 resp.Product.ProductId 读，避免两处字段不一致时对不上账。
	resp.Product = &productpb.ProductInfo{
		ProductId:    int64(d.ID),
		Title:        d.Title,
		Price:        d.Price,
		Status:       d.Status,
		CategoryId:   int64(d.CategoryID),
		CategoryName: d.CategoryName,
		SkuCount:     d.SkuCount,
		CreatedAt:    d.CreatedAt.Unix(),
	}
	resp.Code, resp.Message = codes.OK().Code, codes.OK().Message
	return resp
}

func toPbProducts(rows []repository.ProductDetail) []*productpb.ProductInfo {
	out := make([]*productpb.ProductInfo, 0, len(rows))
	for i := range rows {
		out = append(out, &productpb.ProductInfo{
			ProductId:  int64(rows[i].ID),
			Title:      rows[i].Title,
			Price:      rows[i].Price,
			Status:     rows[i].Status,
			CategoryId: int64(rows[i].CategoryID),
			SkuCount:   rows[i].SkuCount,
			CreatedAt:  rows[i].CreatedAt.Unix(),
		})
	}
	return out
}

// tracexStart 业务层埋点（保留一个内部别名，避免 service 包各处重复取 tracer）
var tracexStart = tracex.Start
