// Package service ★ 唯一允许出现业务判断的层。
//
// 本域的句柄面比 order 窄：只有 db（经 repository 注入）与 cache（详情缓存）
// —— 没有 lock / mq。两条路径：
//
//	GetProduct  —— 只读 RPC 路径（IDL 的 GetProduct）：读缓存 → 未命中回源 → 回填
//	UpdatePrice —— 写路径：落库 → 删缓存（写后失效）
//
// 句柄按「缺失即降级 + 留日志」处理；db 是硬前提（app.Setup 已用不变量自检挡过）。
package service

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/gospacex/cachex"
	"github.com/gospacex/logx"
	"github.com/gospacex/otelx/layer"
	"gorm.io/gorm"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/product/model"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	"github.com/gospacex/launchx/examples/pkg/cacheutil"
)

// detailTTL 商品详情缓存存活时间。
//
// 取 5 分钟与 order 域的详情缓存同口径：商品详情读多写少，缓存能挡住绝大部分
// 回源。为什么不设更长 —— 写后失效那一步是**尽力而为**（删缓存失败只 Warn），
// price 又属资金敏感字段；5 分钟的过期是这条链上「失效没删干净」时最坏情况的
// 兜底上界，而不是期望常驻时长。
const detailTTL = 5 * time.Minute

// productRepo 是 service 真正依赖的窄接口：只含它调用的两个方法。
//
// 用接口而不是 *repository.ProductRepository：service 只关心「读/改」的语义，
// 不关心句柄从哪来；这也让业务判断能脱离真实库被直接驱动到（见 product_test.go）。
// *repository.ProductRepository 天然满足它，装配层无需额外适配。
type productRepo interface {
	FindByID(ctx context.Context, id uint64) (*model.Product, error)
	UpdatePrice(ctx context.Context, id uint64, price int64) error
}

// ProductService 商品业务。
type ProductService struct {
	repo    productRepo
	cache   cachex.Cache
	logger  logx.Logger
	traceOn bool // trace.enabled（总闸），由装配层注入
}

// New 注入仓储、缓存与埋点开关。cache 允许为 nil（降级为「每次都回源」）。
func New(repo productRepo, cache cachex.Cache, logger logx.Logger, traceOn bool) *ProductService {
	return &ProductService{repo: repo, cache: cache, logger: logger, traceOn: traceOn}
}

// ProductView 查商品出参（service 自己的类型，与 RPC 结构解耦）。
type ProductView struct {
	Product model.Product
	Cached  bool // 是否命中缓存，便于验收时判断缓存是否真的生效
}

// GetProduct 查商品详情：先缓存，未命中回源并回填。
//
// 未命中（库里没有该 id）回 ProductNotFound，与「库报错」区分开：前者是用户
// 拿错了 id，后者是下游故障，bff 对两者的映射不同（404 vs 502）。
//
// 读缓存走 cacheutil.FetchResult（含空值哨兵）：查不存在的 id 会回填一条短期
// 负缓存，否则"批量请求不存在的 id"能把整层缓存旁路掉，压力直接落到 DB。
// 用 FetchResult 而不是 Fetch 是因为 ProductView.Cached 是验收判据的一部分，
// 进程内要能断言"这次到底命中了没有"（span 上的 cache.hit 属性读不到）。
func (s *ProductService) GetProduct(ctx context.Context, id uint64) (v *ProductView, err error) {
	ctx, end := layer.Begin(ctx, "service", "product", "GetProduct", s.traceOn, nil)
	defer func() { end(err) }()

	res, err := cacheutil.FetchResult(ctx, s.cache, s.cacheOpt(), s.key(id),
		func(ctx context.Context) (*model.Product, error) {
			return s.repo.FindByID(ctx, id)
		})
	if err != nil {
		var nf *cacheutil.NotFound
		if errors.As(err, &nf) {
			return nil, codes.New(codes.ProductNotFound, "product "+strconv.FormatUint(id, 10)+" not found")
		}
		return nil, codes.Wrap(codes.Internal, "get product", err)
	}
	return &ProductView{Product: *res.Value, Cached: res.Hit}, nil
}

// cacheOpt 本域读缓存的统一参数（正缓存 TTL 与空值哨兵只在这一处定义）。
//
// 为什么要抽：各域各写一份 Options 的话，改负缓存策略就要改 N 遍，
// 漏改的那处不会报错，只会静默少一层保护 —— 这正是缓存穿透留下的方式。
func (s *ProductService) cacheOpt() cacheutil.Options {
	return cacheutil.Options{
		Label: "product",
		TTL:   detailTTL,
		NotFound: func(err error) bool {
			return errors.Is(err, gorm.ErrRecordNotFound)
		},
		Logger: s.logger,
	}
}

// UpdatePrice 改价格：落库 → 删缓存。
//
// **写后失效的顺序不可反**：必须先落库成功再删缓存。反过来的话，并发读者会在
// 缓存被删、库还没更新的窗口里回源读到旧值，再把它回填回去 —— 旧值反而被续了
// 一个 TTL。删除失败只告警：TTL 会兜底（见 detailTTL）。
//
// 本方法是**单测驱动的写路径**：IDL 的 UpdateProduct 在本示例未接线（见
// handler.UpdateProduct 的 Unimplemented），它在这里的唯一目的是让「写后失效」
// 这条缓存契约有一条可执行判据，不是一条留有隐藏入口的生产写路径。
func (s *ProductService) UpdatePrice(ctx context.Context, id uint64, price int64) (err error) {
	ctx, end := layer.Begin(ctx, "service", "product", "UpdatePrice", s.traceOn, nil)
	defer func() { end(err) }()

	if err := s.repo.UpdatePrice(ctx, id, price); err != nil {
		return codes.Wrap(codes.Internal, "update product price", err)
	}
	if s.cache != nil {
		if derr := s.cache.Delete(ctx, s.key(id)); derr != nil {
			s.logger.Warn("mesh-product: cache invalidate failed, ttl will expire it",
				logx.String("key", s.key(id)), logx.Err(derr))
		}
	}
	return nil
}

func (s *ProductService) key(id uint64) string {
	return "mesh:product:detail:" + strconv.FormatUint(id, 10)
}

// 解码与回填不再在本域手写：都归 cacheutil（见 GetProduct）。
//
// 留在域内的代价不是多几行，而是**语义各写一份**：此前"解码失败 → 当 miss"和
// "回填失败 → 只告警"这两条规则只有 product 域遵守，改规则永远改不全。
