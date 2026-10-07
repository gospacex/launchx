// Package service demo-srv 业务编排层。
//
// CreateOrder 编排（brief §srv-order）：
//  1. 参数校验（UserId≤0 → 1004；明细空/数量非法 → 1001）
//  2. 逐项调 srv-product.ReserveStock 预占库存（商品域业务码 3001/3002/3003
//     原样透传；本地事务尚未开始 → 失败无需回滚）
//  3. 本地事务 orders + order_items + outbox（repository.CreateOrderTx，
//     事件体 pkg/events.OrderCompleted，SchemaVer1）
//  4. 后台 OutboxRunner 3s ticker 补发（幂等：status 0→1；traceparent 随
//     mqx Message.Headers[events.TraceHeaderKey] 透传，仅当调用链开）
//
// 跨服务调用走 pkg/kitex_gen 生成桩（rpcx 客户端），目标地址由注册中心解析
// （ProductClient 为 nil 时跳过预占，总额按明细计算）。req/resp 直接复用
// pkg/kitex_gen/order 的 pb 类型（转换在本层完成，rpc 层只做桩转发）。
package service

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/gospacex/launchx"
	"github.com/gospacex/logx"

	orderpb "github.com/gospacex/launchx/examples/micro/srv/pkg/kitex_gen/order"
	productpb "github.com/gospacex/launchx/examples/micro/srv/pkg/kitex_gen/product"

	"github.com/gospacex/launchx/examples/micro/srv/internal/repository"
	"github.com/gospacex/launchx/examples/micro/srv/pkg/codes"
	"github.com/gospacex/launchx/examples/micro/srv/pkg/events"
)

// ProductClient 商品域 RPC 客户端（kitex 生成客户端的窄接口，便于测试注入）。
// 构造签名（app/setup.go）：
//
//	productclient.NewClient("Product", rpcx.BuildClientOptions(rpcx.ClientOptions{...})...)
type ProductClient interface {
	ReserveStock(ctx context.Context, req *productpb.ReserveStockReq) (*productpb.ReserveStockResp, error)
}

// OrderService 订单业务编排
type OrderService struct {
	repo    *repository.OrderRepository
	outbox  *OutboxRunner
	product ProductClient
	traceOn bool
	logger  logx.Logger
}

// NewOrderService 构造（product 允许 nil：无服务发现时跳过预占，纯本地单库运行）
func NewOrderService(
	repo *repository.OrderRepository,
	outbox *OutboxRunner,
	product ProductClient,
	traceOn bool,
	logger logx.Logger,
) *OrderService {
	if logger == nil {
		logger = logx.GetGlobal()
	}
	return &OrderService{repo: repo, outbox: outbox, product: product, traceOn: traceOn, logger: logger}
}

// Repo 暴露仓储（组装期健康检查 / 对账用）
func (s *OrderService) Repo() *repository.OrderRepository { return s.repo }

// CreateOrder 下单
func (s *OrderService) CreateOrder(ctx context.Context, req *orderpb.CreateOrderReq) (*orderpb.CreateOrderResp, error) {
	resp := &orderpb.CreateOrderResp{UserId: req.GetUserId()}
	if req.GetUserId() <= 0 {
		c := codes.Fail(codes.CodeUnauthorized, "user required")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	items := req.GetItems()
	if len(items) == 0 {
		c := codes.Fail(codes.CodeInvalidParam, "items required")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	for _, it := range items {
		if it.GetProductId() <= 0 || it.GetQuantity() <= 0 {
			c := codes.Fail(codes.CodeInvalidParam, "invalid item")
			resp.Code, resp.Message = c.Code, c.Message
			return resp, nil
		}
	}

	// 2) 预占库存；总额 = Σ quantity × unit_price（分）
	total := int64(0)
	if s.product != nil {
		for _, it := range items {
			rr, err := s.product.ReserveStock(ctx, &productpb.ReserveStockReq{
				UserId:    req.GetUserId(),
				ProductId: it.GetProductId(),
				Quantity:  it.GetQuantity(),
			})
			if err != nil {
				// 网络层失败（超时/不可达）走 kitex 错误体，不混业务码
				resp.Code, resp.Message = codes.Fail(codes.CodeInternal, err.Error()).Code, err.Error()
				return resp, nil
			}
			if rr.GetCode() != 0 {
				// 商品域业务码（3001 下架 / 3002 库存不足 / 3003 预占失败）原样透传
				resp.Code, resp.Message = rr.GetCode(), rr.GetMessage()
				return resp, nil
			}
			total += int64(it.GetQuantity()) * it.GetUnitPrice()
		}
	} else {
		// 无服务发现（纯本地单库运行）：跳过预占，总额按明细计算
		for _, it := range items {
			total += int64(it.GetQuantity()) * it.GetUnitPrice()
		}
	}

	// 3) 本地事务：orders + order_items + outbox（同事务，Runner 3s 补发）。
	// 事件体在订单行落库后（自增 ID 已知）经 OutboxFill 回调定稿，
	// OrderCompleted.OrderID 内嵌真实值（事务发件箱与下单同事务提交）。
	fillItems := toEventItems(items)
	evTraceparent := s.traceparentOf(ctx)
	out, err := s.repo.CreateOrderTx(ctx, repository.CreateOrderInput{
		UserID:      uint64(req.GetUserId()),
		OrderNo:     newOrderNo(),
		TotalAmount: total,
		Items:       toRepoItems(items),
		OutboxFill: func(orderID uint64) (payload, tp string) {
			ev := events.OrderCompleted{
				OrderID:    int64(orderID),
				UserID:     req.GetUserId(),
				Amount:     total,
				Items:      fillItems,
				SchemaVer:  events.SchemaVer1,
				TraceID:    evTraceID(ctx),
				OccurredAt: time.Now().UnixMilli(),
			}
			b, err := events.Marshal(ev)
			if err != nil {
				// 序列化失败不阻断下单主流程：空 payload 时 Runner 跳过该事件
				s.logger.WarnContext(ctx, "order event marshal failed", logx.Err(err))
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

// traceparentOf 取当前链路的 W3C traceparent（随 outbox header 透传）。
// 仅当调用链开（traceOn）时经 launchx 的传播入口注入（telemetry 步骤注册
// Composite(TraceContext, Baggage)；无活动 span 时为空串）。
//
// 走 launchx.Inject 而不是裸调 otel 全局：传播器由 telemetry 步骤注册，
// 库侧入口是唯一被承认的取用路径，业务层不该直接依赖 otel 全局单例。
func (s *OrderService) traceparentOf(ctx context.Context) string {
	if !s.traceOn {
		return ""
	}
	carrier := propagation.MapCarrier(map[string]string{})
	launchx.Inject(ctx, carrier)
	return carrier.Get(events.TraceHeaderKey)
}

// evTraceID 事件体内嵌 trace_id（span 的 TraceID；无活动 span 时为空）
func evTraceID(ctx context.Context) string {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		return sc.TraceID().String()
	}
	return ""
}

// GetOrder 查单个订单 + 明细（repository 2 表 JOIN Find）
func (s *OrderService) GetOrder(ctx context.Context, req *orderpb.GetOrderReq) (*orderpb.GetOrderResp, error) {
	resp := &orderpb.GetOrderResp{UserId: req.GetUserId(), OrderId: req.GetOrderId()}
	if req.GetUserId() <= 0 {
		c := codes.Fail(codes.CodeUnauthorized, "user required")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	ow, err := s.repo.GetOrder(ctx, uint64(req.GetOrderId()), uint64(req.GetUserId()))
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
	resp.OrderNo = ow.Order.OrderNo
	resp.Status = ow.Order.Status
	resp.TotalAmount = ow.Order.TotalAmount
	resp.PaidAmount = ow.Order.PaidAmount
	resp.Items = toPbItems(ow.Items)
	resp.Code, resp.Message = codes.OK().Code, codes.OK().Message
	return resp, nil
}

// Report 4 表 JOIN 报表（Find 投影聚合行，COUNT(DISTINCT) 抵消行放大）
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

// ListOrders 只读订单流水（对账数据源）
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
			OrderId:     int64(flows[i].ID),
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

// ---------- helpers ----------

// newOrderNo 订单号（uk_order_no，≤40 字符）：O + yyyyMMddHHmmss + 8 位 hex 随机
func newOrderNo() string {
	buf := make([]byte, 4)
	_, _ = rand.Read(buf)
	return "O" + time.Now().Format("20060102150405") + strconv.FormatUint(uint64(binary.BigEndian.Uint32(buf)), 16)
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
