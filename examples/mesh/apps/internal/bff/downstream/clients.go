// Package downstream bff 的四域出向客户端。
//
// 与 examples/micro/bff/internal/downstream 的差别：**没有**
// 「注册中心地址 + 类型断言 + 静默兜底」三件套（约 60 行）。apps 下
// launchx 的 clientOptionsFactory 读到 RegistryAddr 为空，直接走
// `service.upstream.<域>` 的 K8s Service FQDN（kitex DNS 解析 + Envoy
// 负载均衡），业务侧只需要把 opts 传进去。
//
// 包内声明的是**窄接口**（只含 bff 真正调用的方法），不是 kitex 生成的全量
// `Client` 接口：
//   - 编译期就能发现「客户端建了没用」—— 接口里多一个没人调的方法就是死的；
//   - 单测的假件只需实现用到的那几个方法，不必为 22 个方法写 22 个空实现。
//
// 生成的 `Client` 接口天然满足这些窄接口（方法集更宽），赋值不需要适配层。
package downstream

import (
	"context"
	"fmt"

	"github.com/cloudwego/kitex/client"
	callopt "github.com/cloudwego/kitex/client/callopt"

	"github.com/gospacex/launchx"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/rpcwire"

	orderpb "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/order"
	orderrpc "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/order/order"
	pointspb "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/points"
	pointsrpc "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/points/points"
	productpb "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/product"
	productrpc "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/product/product"
	stockpb "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/stock"
	stockrpc "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/stock/stock"
)

// OrderClient bff 用到的 order 域方法面：查单 + 下单（两条路由）。
type OrderClient interface {
	GetOrder(ctx context.Context, req *orderpb.GetOrderReq, opts ...callopt.Option) (*orderpb.GetOrderResp, error)
	CreateOrder(ctx context.Context, req *orderpb.CreateOrderReq, opts ...callopt.Option) (*orderpb.CreateOrderResp, error)
}

// PointsClient bff 用到的 points 域方法面：查积分（写路径在 MQ 消费侧，不经 HTTP）。
type PointsClient interface {
	GetAccount(ctx context.Context, req *pointspb.GetAccountReq, opts ...callopt.Option) (*pointspb.GetAccountResp, error)
}

// ProductClient bff 用到的 product 域方法面：查商品详情。
type ProductClient interface {
	GetProduct(ctx context.Context, req *productpb.GetProductReq, opts ...callopt.Option) (*productpb.GetProductResp, error)
}

// StockClient bff 用到的 stock 域方法面：查库存。
type StockClient interface {
	GetStock(ctx context.Context, req *stockpb.GetStockReq, opts ...callopt.Option) (*stockpb.GetStockResp, error)
}

// Clients 四域客户端集合。字段名是 mesh-entry-chain 的「不留死客户端」判据
// （`grep -cE 'clients\.(Order|Points|Product|Stock)'`）所依赖的名字 ——
// 改名会让那条判据变成永远失败，而它保护的正是「某个域的业务逻辑永远无法
// 从入口触达」这类静默失效。
type Clients struct {
	Order   OrderClient
	Points  PointsClient
	Product ProductClient
	Stock   StockClient
}

// 四个下游域名。**三处必须完全一致**：这里的字面量、`conf/bff.yaml` 的
// `service.upstream.<域>` 键、以及各域 `service.name`。
//
// 任一处不一致的后果都不是报错，而是「客户端建好了但没有目标」——
// 直到第一次调用才炸，且报错指向地址解析而不是配置写错。
const (
	DomainOrder   = "srv-order"
	DomainPoints  = "srv-points"
	DomainProduct = "srv-product"
	DomainStock   = "srv-stock"
)

// New 按域名建四个客户端。
//
// 每个客户端的选项由两部分拼成：
//   - `a.RPCClientOptions(<域>)`：地址与发现（apps 下是 K8s Service FQDN）；
//   - `rpcwire.ClientOptions()`：TTHeader handler —— 没有它，下游回的业务码
//     会被 kitex 降级成一段字符串（见 rpcwire 包注释）。它不是可选优化项。
func New(a *launchx.App) (*Clients, error) {
	orderOpts, err := optionsOf(a, DomainOrder)
	if err != nil {
		return nil, err
	}
	order, err := orderrpc.NewClient(DomainOrder, orderOpts...)
	if err != nil {
		return nil, fmt.Errorf("bff: new order client: %w", err)
	}

	pointsOpts, err := optionsOf(a, DomainPoints)
	if err != nil {
		return nil, err
	}
	points, err := pointsrpc.NewClient(DomainPoints, pointsOpts...)
	if err != nil {
		return nil, fmt.Errorf("bff: new points client: %w", err)
	}

	productOpts, err := optionsOf(a, DomainProduct)
	if err != nil {
		return nil, err
	}
	product, err := productrpc.NewClient(DomainProduct, productOpts...)
	if err != nil {
		return nil, fmt.Errorf("bff: new product client: %w", err)
	}

	stockOpts, err := optionsOf(a, DomainStock)
	if err != nil {
		return nil, err
	}
	stock, err := stockrpc.NewClient(DomainStock, stockOpts...)
	if err != nil {
		return nil, fmt.Errorf("bff: new stock client: %w", err)
	}

	return &Clients{Order: order, Points: points, Product: product, Stock: stock}, nil
}

// optionsOf 取一个下游的客户端选项并叠加传输层设置。
//
// 类型断言失败要报**实际类型**：`RPCClientOptions` 以 any 承载，返错时
// 「expected []client.Option, got X」比「options unavailable」有用得多。
func optionsOf(a *launchx.App, domain string) ([]client.Option, error) {
	v, err := a.RPCClientOptions(domain)
	if err != nil {
		return nil, fmt.Errorf("bff: rpc client options for %s: %w", domain, err)
	}
	opts, ok := v.([]client.Option)
	if !ok {
		return nil, fmt.Errorf("bff: rpc client options for %s is %T, want []client.Option "+
			"(check service.mode=bff)", domain, v)
	}
	return append(opts, rpcwire.ClientOptions()...), nil
}
