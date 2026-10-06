// Command e2e 是 mall 四微服的**验收驱动**：一次性把四域的单表 CRUD 与多表
// JOIN/事务全打一遍，每条调用都从同一个根 span 出发，便于在 Jaeger 上按
// trace 直接复核分层。
//
// 为什么单独放一个命令而不是写 shell curl：四服是 kitex RPC（私有二进制 +
// TTHeader），没有 HTTP 面可 curl。要驱动它们只能起一个 kitex 客户端，而
// 客户端的续链注入又必须走 rpcx.BuildClientOptions（里面挂了
// ClientSpanMiddleware，负责把 traceparent 写进 metainfo）——自己裸拼
// client.Option 会静默断链。
//
// 用法：
//
//	export HUBX_DB_PW=hubx123
//	go run ./examples/mall/e2e                     # 默认走 consul 发现
//	go run ./examples/mall/e2e --mode=direct       # 不经注册中心，直连四个端口
//
// 退出码：0 = 全部步骤的业务码为 OK；1 = 有步骤返回非 OK 或调用失败。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/cloudwego/kitex/client"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/gospacex/rpcx"

	orderpb "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen/order"
	orderrpc "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen/order/order"
	pointspb "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen/points"
	pointsrpc "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen/points/points"
	productpb "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen/product"
	productrpc "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen/product/product"
	stockpb "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen/stock"
	stockrpc "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen/stock/stock"
)

// 四服地址。与 dockers/docker-compose.yml 收编后的本机中间件一致；
// 走 registry 模式时这些地址只是兜底（实际由 consul 发现给出）。
const (
	productTarget = "127.0.0.1:9101"
	orderTarget   = "127.0.0.1:9102"
	stockTarget   = "127.0.0.1:9103"
	pointsTarget  = "127.0.0.1:9104"

	registryAddr = "127.0.0.1:8500" // consul
	otlpEndpoint = "127.0.0.1:4318" // jaeger OTLP/HTTP

	// stockProductID 库存域单独用一个**已存在**的商品 ID。
	//
	// 为什么不是新建的那个：库存域没有"建库存"RPC，AdjustTx 是纯 UPDATE
	// （RowsAffected==0 时区分"没记录"与"不够扣"，翻译成 4001/4002），
	// 新建商品的库存行只能由 schema.sql 的 seed 或运营侧初始化补。
	// 示例的 seed 给了 product_id=1，故库存域步骤固定打它；
	// 跨域只存 ID，商品域与库存域用不同 ID 不影响链路正确性。
	stockProductID = 1
)

var (
	mode     = flag.String("mode", "registry", "registry=经 consul 发现（默认，顺带验 Discovery）；direct=直连四端口")
	keepOpen = flag.Duration("wait", 0, "结束后等待多久再退出，便于观察 Jaeger（如 5s）")
)

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e FAILED: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("e2e OK")
}

func run() error {
	ctx := context.Background()

	tp, err := newTracerProvider(ctx)
	if err != nil {
		return err
	}
	defer func() {
		// 批量导出，必须显式 Shutdown 才 flush 得出去。
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tp.Shutdown(sctx)
	}()
	otel.SetTracerProvider(tp)
	// 与 launchx telemetry 步骤同一套传播器：Composite{TraceContext, Baggage}。
	// 少了这一行 traceparent 既不写也不读，四服各成一串孤儿 trace。
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	useRegistry := *mode == "registry"
	pc, oc, sc, tc, err := newClients(useRegistry)
	if err != nil {
		return err
	}

	tracer := tp.Tracer("github.com/gospacex/launchx/examples/mall/e2e")
	rootCtx, root := tracer.Start(ctx, "mall.e2e.session")
	defer root.End()
	fmt.Printf("traceID=%s\n", root.SpanContext().TraceID().String())

	failed := 0
	step := func(name string, fn func(context.Context) error) {
		sctx, span := tracer.Start(rootCtx, "e2e."+name)
		defer span.End()
		if err := fn(sctx); err != nil {
			failed++
			span.RecordError(err)
			fmt.Printf("  [FAIL] %-34s %v\n", name, err)
			return
		}
		fmt.Printf("  [ OK ] %s\n", name)
	}

	// ---------------- 商品域 ----------------
	var productID int64
	step("product.CreateProduct", func(c context.Context) error {
		r, err := pc.CreateProduct(c, &productpb.CreateProductReq{
			OperatorId: 1, Title: "e2e-商品", Price: 1999, CategoryId: 2, InitStock: 100,
		})
		if err != nil {
			return err
		}
		if r.GetCode() != 0 {
			return fmt.Errorf("code=%d msg=%s", r.GetCode(), r.GetMessage())
		}
		productID = r.GetProductId()
		fmt.Printf("           product_id=%d\n", productID)
		return nil
	})
	step("product.GetProduct(miss→DB)", func(c context.Context) error {
		r, err := pc.GetProduct(c, &productpb.GetProductReq{UserId: 1, ProductId: productID})
		if err != nil {
			return err
		}
		return bizErr(r.GetCode(), r.GetMessage())
	})
	step("product.GetProduct(hit cache)", func(c context.Context) error {
		r, err := pc.GetProduct(c, &productpb.GetProductReq{UserId: 1, ProductId: productID})
		if err != nil {
			return err
		}
		return bizErr(r.GetCode(), r.GetMessage())
	})
	step("product.UpdateProduct(2表事务)", func(c context.Context) error {
		r, err := pc.UpdateProduct(c, &productpb.UpdateProductReq{
			OperatorId: 1, ProductId: productID, Title: "e2e-商品-改价", Price: 2999, CategoryId: 2,
		})
		if err != nil {
			return err
		}
		return bizErr(r.GetCode(), r.GetMessage())
	})
	step("product.UpdateStatus(加锁+事务)", func(c context.Context) error {
		r, err := pc.UpdateStatus(c, &productpb.UpdateStatusReq{OperatorId: 1, ProductId: productID, Status: 2})
		if err != nil {
			return err
		}
		return bizErr(r.GetCode(), r.GetMessage())
	})
	step("product.ListProducts(单表分页)", func(c context.Context) error {
		r, err := pc.ListProducts(c, &productpb.ListProductsReq{UserId: 1, CategoryId: 2, Limit: 10})
		if err != nil {
			return err
		}
		if e := bizErr(r.GetCode(), r.GetMessage()); e != nil {
			return e
		}
		fmt.Printf("           rows=%d\n", len(r.GetProducts()))
		return nil
	})
	step("product.Report(4表JOIN)", func(c context.Context) error {
		r, err := pc.Report(c, &productpb.ReportReq{UserId: 1, CategoryId: 2, Limit: 10})
		if err != nil {
			return err
		}
		if e := bizErr(r.GetCode(), r.GetMessage()); e != nil {
			return e
		}
		fmt.Printf("           rows=%d\n", len(r.GetRows()))
		return nil
	})

	// ---------------- 库存域 ----------------
	//
	// 预占 → 确认 与 预占 → 释放 是**两条互斥的终态路径**：ReleaseTx 与
	// ConfirmTx 都只认 status=ReserveReserved 的预占流水，且各自会把整行
	// 翻成终态（3/2）。所以对同一 orderNo 先释放再确认，第二步必然报
	// 4005 reservation not found —— 这不是 bug，是"一张预占单只能结算一次"。
	ns := time.Now().UnixNano()
	orderNoConfirm := fmt.Sprintf("NOC%d", ns)
	orderNoRelease := fmt.Sprintf("NOR%d", ns)
	step("stock.AdjustStock(建库存)", func(c context.Context) error {
		r, err := sc.AdjustStock(c, &stockpb.AdjustStockReq{
			OperatorId: 1, ProductId: stockProductID, Delta: 500, Reason: "e2e-init",
		})
		if err != nil {
			return err
		}
		return bizErr(r.GetCode(), r.GetMessage())
	})
	step("stock.GetStock(单表)", func(c context.Context) error {
		r, err := sc.GetStock(c, &stockpb.GetStockReq{ProductId: stockProductID})
		if err != nil {
			return err
		}
		if e := bizErr(r.GetCode(), r.GetMessage()); e != nil {
			return e
		}
		fmt.Printf("           total=%d available=%d\n", r.GetStock().GetTotal(), r.GetStock().GetAvailable())
		return nil
	})
	step("stock.ReserveStock#1(加锁+3表事务)", func(c context.Context) error {
		r, err := sc.ReserveStock(c, &stockpb.ReserveStockReq{
			UserId: 1, OrderNo: orderNoConfirm, ProductId: stockProductID, Quantity: 3,
		})
		if err != nil {
			return err
		}
		return bizErr(r.GetCode(), r.GetMessage())
	})
	step("stock.ConfirmStock#1(真正扣减)", func(c context.Context) error {
		r, err := sc.ConfirmStock(c, &stockpb.ConfirmStockReq{
			OrderNo: orderNoConfirm, ProductId: stockProductID, Quantity: 3,
		})
		if err != nil {
			return err
		}
		if e := bizErr(r.GetCode(), r.GetMessage()); e != nil {
			return e
		}
		fmt.Printf("           total=%d available=%d\n", r.GetTotal(), r.GetAvailable())
		return nil
	})
	step("stock.ReserveStock#2(再预占)", func(c context.Context) error {
		r, err := sc.ReserveStock(c, &stockpb.ReserveStockReq{
			UserId: 1, OrderNo: orderNoRelease, ProductId: stockProductID, Quantity: 2,
		})
		if err != nil {
			return err
		}
		return bizErr(r.GetCode(), r.GetMessage())
	})
	step("stock.ReleaseStock#2(回滚)", func(c context.Context) error {
		r, err := sc.ReleaseStock(c, &stockpb.ReleaseStockReq{
			OrderNo: orderNoRelease, ProductId: stockProductID, Quantity: 2,
		})
		if err != nil {
			return err
		}
		if e := bizErr(r.GetCode(), r.GetMessage()); e != nil {
			return e
		}
		fmt.Printf("           available=%d\n", r.GetAvailable())
		return nil
	})
	step("stock.ListStocks(单表分页)", func(c context.Context) error {
		r, err := sc.ListStocks(c, &stockpb.ListStocksReq{Limit: 10})
		if err != nil {
			return err
		}
		if e := bizErr(r.GetCode(), r.GetMessage()); e != nil {
			return e
		}
		fmt.Printf("           rows=%d\n", len(r.GetStocks()))
		return nil
	})
	step("stock.Report(3表JOIN)", func(c context.Context) error {
		r, err := sc.Report(c, &stockpb.ReportReq{Limit: 10})
		if err != nil {
			return err
		}
		if e := bizErr(r.GetCode(), r.GetMessage()); e != nil {
			return e
		}
		fmt.Printf("           rows=%d\n", len(r.GetRows()))
		return nil
	})

	// ---------------- 订单域 ----------------
	var orderID int64
	var realOrderNo string
	step("order.CreateOrder(3表事务)", func(c context.Context) error {
		r, err := oc.CreateOrder(c, &orderpb.CreateOrderReq{
			UserId: 1,
			Items: []*orderpb.OrderItem{
				{ProductId: productID, Name: "e2e-商品-改价", Quantity: 2, UnitPrice: 2999},
			},
		})
		if err != nil {
			return err
		}
		if e := bizErr(r.GetCode(), r.GetMessage()); e != nil {
			return e
		}
		orderID, realOrderNo = r.GetOrderId(), r.GetOrderNo()
		fmt.Printf("           order_id=%d order_no=%s total=%d\n", orderID, realOrderNo, r.GetTotalAmount())
		return nil
	})
	step("order.GetOrder(2表JOIN)", func(c context.Context) error {
		r, err := oc.GetOrder(c, &orderpb.GetOrderReq{UserId: 1, OrderId: orderID})
		if err != nil {
			return err
		}
		if e := bizErr(r.GetCode(), r.GetMessage()); e != nil {
			return e
		}
		fmt.Printf("           items=%d\n", len(r.GetItems()))
		return nil
	})
	step("order.ListOrders(单表流水)", func(c context.Context) error {
		r, err := oc.ListOrders(c, &orderpb.ListOrdersReq{UserId: 1, Limit: 10})
		if err != nil {
			return err
		}
		if e := bizErr(r.GetCode(), r.GetMessage()); e != nil {
			return e
		}
		fmt.Printf("           rows=%d\n", len(r.GetFlows()))
		return nil
	})
	step("order.Report(4表JOIN)", func(c context.Context) error {
		r, err := oc.Report(c, &orderpb.ReportReq{UserId: 1})
		if err != nil {
			return err
		}
		if e := bizErr(r.GetCode(), r.GetMessage()); e != nil {
			return e
		}
		fmt.Printf("           rows=%d\n", len(r.GetRows()))
		return nil
	})

	// ---------------- 积分域 ----------------
	step("points.Earn(2表事务+幂等键)", func(c context.Context) error {
		r, err := tc.Earn(c, &pointspb.EarnReq{UserId: 1, OrderNo: realOrderNo, Amount: 100, Remark: "e2e"})
		if err != nil {
			return err
		}
		if e := bizErr(r.GetCode(), r.GetMessage()); e != nil {
			return e
		}
		fmt.Printf("           balance=%d\n", r.GetBalance())
		return nil
	})
	step("points.Earn(重复→幂等)", func(c context.Context) error {
		r, err := tc.Earn(c, &pointspb.EarnReq{UserId: 1, OrderNo: realOrderNo, Amount: 100, Remark: "e2e-dup"})
		if err != nil {
			return err
		}
		// 幂等命中也应返回 OK（重复加钱才是事故，重复返回不算）
		fmt.Printf("           code=%d msg=%s\n", r.GetCode(), r.GetMessage())
		return nil
	})
	step("points.Consume(2表事务)", func(c context.Context) error {
		r, err := tc.Consume(c, &pointspb.ConsumeReq{UserId: 1, OrderNo: realOrderNo + "-c", Amount: 30, Remark: "e2e"})
		if err != nil {
			return err
		}
		if e := bizErr(r.GetCode(), r.GetMessage()); e != nil {
			return e
		}
		fmt.Printf("           balance=%d\n", r.GetBalance())
		return nil
	})
	step("points.GetAccount(单表)", func(c context.Context) error {
		r, err := tc.GetAccount(c, &pointspb.GetAccountReq{UserId: 1})
		if err != nil {
			return err
		}
		if e := bizErr(r.GetCode(), r.GetMessage()); e != nil {
			return e
		}
		fmt.Printf("           balance=%d\n", r.GetAccount().GetBalance())
		return nil
	})
	step("points.ListLedgers(单表分页)", func(c context.Context) error {
		r, err := tc.ListLedgers(c, &pointspb.ListLedgersReq{UserId: 1, Limit: 10})
		if err != nil {
			return err
		}
		if e := bizErr(r.GetCode(), r.GetMessage()); e != nil {
			return e
		}
		fmt.Printf("           rows=%d\n", len(r.GetLedgers()))
		return nil
	})
	step("points.Report(2表JOIN)", func(c context.Context) error {
		r, err := tc.Report(c, &pointspb.ReportReq{UserId: 1})
		if err != nil {
			return err
		}
		if e := bizErr(r.GetCode(), r.GetMessage()); e != nil {
			return e
		}
		fmt.Printf("           rows=%d\n", len(r.GetRows()))
		return nil
	})

	if *keepOpen > 0 {
		time.Sleep(*keepOpen)
	}
	if failed > 0 {
		return fmt.Errorf("%d 个步骤未通过", failed)
	}
	return nil
}

// bizErr 把业务码翻译成 error：四服一律「code=0 即成功」，非 0 只体现在
// resp.Code 上而不返回 RPC error，驱动侧必须自己判，否则全绿假象。
func bizErr(code int64, msg string) error {
	if code == 0 {
		return nil
	}
	return fmt.Errorf("code=%d msg=%s", code, msg)
}

func newTracerProvider(ctx context.Context) (*sdktrace.TracerProvider, error) {
	exp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(otlpEndpoint),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("otlp exporter: %w", err)
	}
	// 不 Merge resource.Default()：默认资源的 schema URL 是 1.43.0，与
	// semconv/v1.26.0 冲突会直接报 "conflicting Schema URL"（连 provider 都建不出）。
	// 验收驱动只需要 service.name，故自建一份单属性资源。
	res := sdkresource.NewWithAttributes(
		semconv.SchemaURL, semconv.ServiceName("mall-e2e"),
	)
	return sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		// 验收要看到每一条，不做采样，否则「十次看不见一条」会被误判成断链。
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	), nil
}

// newClients 建四个 kitex 客户端。
//
// 关键：Option 必须经 rpcx.BuildClientOptions 产出 —— 里面挂了
// kitextracing.NewClientSuite 与 ClientSpanMiddleware，后者负责把本驱动的
// span context 注入 metainfo 给下游。裸拼 client.WithHostPorts 会静默断链。
func newClients(useRegistry bool) (productrpc.Client, orderrpc.Client, stockrpc.Client, pointsrpc.Client, error) {
	opts := func(service, target string) []client.Option {
		o := rpcx.ClientOptions{
			ServiceName:  "mall-e2e",
			TraceEnabled: true,
		}
		if useRegistry {
			o.RegistryAddr = registryAddr
		} else {
			o.Target = target
		}
		return rpcx.BuildClientOptions(o)
	}
	if useRegistry {
		fmt.Printf("模式：registry（consul %s）\n", registryAddr)
	} else {
		fmt.Printf("模式：direct（直连四端口）\n")
	}

	pc, err := productrpc.NewClient("srv-product", opts("srv-product", productTarget)...)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("product client: %w", err)
	}
	oc, err := orderrpc.NewClient("srv-order", opts("srv-order", orderTarget)...)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("order client: %w", err)
	}
	sc, err := stockrpc.NewClient("srv-stock", opts("srv-stock", stockTarget)...)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("stock client: %w", err)
	}
	tc, err := pointsrpc.NewClient("srv-points", opts("srv-points", pointsTarget)...)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("points client: %w", err)
	}
	return pc, oc, sc, tc, nil
}

// 保证 oteltrace 被用到（root span 的类型断言与调试打印会用到）。
var _ oteltrace.Tracer
