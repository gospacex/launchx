// Package handler HTTP 层：只做参数绑定、状态码映射与响应封装，业务一律下沉
// 到 downstream 层；每个 handler 自开 `handler.*` span。
//
// 本包是 webx/hertz 唯一出现的地方（handler 签名是 hertz 的 app.HandlerFunc）。
package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/cloudwego/hertz/pkg/app"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/gospacex/launchx/examples/micro/bff/internal/downstream"
	srvcodes "github.com/gospacex/launchx/examples/micro/srv/pkg/codes"
	orderpb "github.com/gospacex/launchx/examples/micro/srv/pkg/kitex_gen/order"
)

// OrderHandler 订单 HTTP 入口。
type OrderHandler struct {
	orders  *downstream.Order
	tracer  trace.Tracer
	service string
}

// New 构造订单 handler。service 是 /ping 回显的形态标识，由装配层传入
// （本包不写形态字面量：业务分层内不得出现形态取值或 mode 分支）。
func New(orders *downstream.Order, tracer trace.Tracer, service string) *OrderHandler {
	return &OrderHandler{orders: orders, tracer: tracer, service: service}
}

// Ping GET /ping —— 自响应，不触达下游。
//
// 它的作用是给「服务活着且 HTTP 那一跳能成 span」留一个不依赖任何下游的判据：
// 下游挂掉时 /ping 仍须 200。
func (h *OrderHandler) Ping(ctx context.Context, c *app.RequestContext) {
	_, span := h.tracer.Start(ctx, "handler.Ping")
	defer span.End()

	c.JSON(http.StatusOK, map[string]string{"service": h.service})
}

// GetOrder GET /order/:id —— 经下游订单服务取单个订单。
func (h *OrderHandler) GetOrder(ctx context.Context, c *app.RequestContext) {
	ctx, span := h.tracer.Start(ctx, "handler.GetOrder")
	defer span.End()

	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		fail(span, c, http.StatusBadRequest, err)
		return
	}
	// user_id 是可选的查询参数：下游按它做归属校验，缺省即 0。
	userID, _ := strconv.ParseInt(c.Query("user_id"), 10, 64)

	resp, err := h.orders.GetOrder(ctx, &orderpb.GetOrderReq{OrderId: id, UserId: userID})
	if err != nil {
		// 下游不可达 / 超时是上游故障，不是本服务的 500。
		fail(span, c, http.StatusBadGateway, err)
		return
	}
	if code := resp.GetCode(); code != srvcodes.CodeOK {
		fail(span, c, statusOf(code), errors.New(msgOr(code, resp.GetMessage())))
		return
	}
	c.JSON(http.StatusOK, resp)
}

// statusOf 领域错误码 → HTTP 状态码。
//
// 只映射语义确定的两类，其余一律按下游故障透传 502 —— 猜出来的状态码比
// 502 更难排查。
func statusOf(code int64) int {
	switch code {
	case srvcodes.CodeInvalidParam:
		return http.StatusBadRequest
	case srvcodes.CodeNotFound, srvcodes.CodeOrderNotFound:
		return http.StatusNotFound
	default:
		return http.StatusBadGateway
	}
}

// msgOr 补一条兜底文案：领域错误可以只带 code 不带 message，
// 空 message 直接进响应体等于什么都不说。
func msgOr(code int64, msg string) string {
	if msg != "" {
		return msg
	}
	return "downstream error " + strconv.FormatInt(code, 10)
}

// fail 统一错误渲染：错误码记进 span，响应体只带文案（不泄漏内部细节）。
func fail(span trace.Span, c *app.RequestContext, status int, err error) {
	span.RecordError(err)
	span.SetStatus(otelcodes.Error, err.Error())
	c.JSON(status, map[string]string{"error": err.Error()})
}
