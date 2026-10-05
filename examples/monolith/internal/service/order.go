// Package service 业务编排层：参数校验、金额计算、错误映射，自开 `service.*` span。
//
// 边界：向下只调用 repository（不直接碰 *gorm.DB），向上只被 handler 调用；
// 仓储返回的 gorm 语义错误在这里映射为领域错误，HTTP 状态码由 handler 决定。
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/gorm"

	"github.com/gospacex/launchx/examples/monolith/internal/model"
	"github.com/gospacex/launchx/examples/monolith/internal/repository"
)

// 领域错误。handler 按类型映射 HTTP 状态码，故这里的取值是业务语义而非 HTTP 语义。
var (
	// ErrInvalidInput 入参不合法（缺字段 / 数量非正 / 非法状态）。
	ErrInvalidInput = errors.New("order: invalid input")
	// ErrNotFound 订单不存在（由 gorm.ErrRecordNotFound 归一化而来）。
	ErrNotFound = errors.New("order: not found")
)

// CreateInput 创建订单入参。
type CreateInput struct {
	UserID    int64   `json:"user_id"`
	Item      string  `json:"item"`
	SKU       string  `json:"sku"`
	Qty       int     `json:"qty"`
	UnitPrice float64 `json:"unit_price"`
}

// Service 订单服务。
type Service struct {
	repo   *repository.Repository
	tracer trace.Tracer
}

// New 构造订单服务。
func New(repo *repository.Repository, tracer trace.Tracer) *Service {
	return &Service{repo: repo, tracer: tracer}
}

// span 开启本层的子 span（层前缀 service），并把错误计入 span 状态。
func (s *Service) span(ctx context.Context, name string) (context.Context, trace.Span) {
	return s.tracer.Start(ctx, "service."+name)
}

// List 查询最近 limit 条订单。
func (s *Service) List(ctx context.Context, limit int) ([]model.Order, error) {
	ctx, span := s.span(ctx, "ListOrders")
	defer span.End()
	if limit <= 0 {
		limit = 20
	}
	out, err := s.repo.List(ctx, limit)
	s.end(span, err)
	return out, err
}

// Get 按订单号查单条订单（单表路径）。
func (s *Service) Get(ctx context.Context, orderNo string) (*model.Order, error) {
	ctx, span := s.span(ctx, "GetOrder")
	defer span.End()
	if strings.TrimSpace(orderNo) == "" {
		s.end(span, ErrInvalidInput)
		return nil, ErrInvalidInput
	}
	out, err := s.repo.Get(ctx, orderNo)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = ErrNotFound
	}
	s.end(span, err)
	return out, err
}

// Create 建单：校验入参 → 算金额 → 事务写 orders + order_items。
func (s *Service) Create(ctx context.Context, in CreateInput) (*model.Order, error) {
	ctx, span := s.span(ctx, "CreateOrder")
	defer span.End()

	in.Item = strings.TrimSpace(in.Item)
	if in.Item == "" || in.Qty <= 0 {
		s.end(span, ErrInvalidInput)
		return nil, ErrInvalidInput
	}
	if in.UnitPrice <= 0 {
		in.UnitPrice = 9.9 // demo 缺省单价：让 amount 有非零值可见，不引入外部定价服务
	}
	sku := strings.TrimSpace(in.SKU)
	if sku == "" {
		sku = "SKU-" + in.Item
	}

	order := &model.Order{
		OrderNo: newOrderNo(),
		UserID:  in.UserID,
		Item:    in.Item,
		Qty:     in.Qty,
		Amount:  float64(in.Qty) * in.UnitPrice,
		Status:  "created",
	}
	items := []model.OrderItem{{
		SKU:      sku,
		Qty:      in.Qty,
		Price:    in.UnitPrice,
		Subtotal: order.Amount,
	}}
	err := s.repo.CreateWithItems(ctx, order, items)
	s.end(span, err)
	if err != nil {
		return nil, err
	}
	return order, nil
}

// GetWithUser 连表查询：订单 + 下单用户。
func (s *Service) GetWithUser(ctx context.Context, orderNo string) (*model.OrderWithUser, error) {
	ctx, span := s.span(ctx, "GetOrderWithUser")
	defer span.End()
	if strings.TrimSpace(orderNo) == "" {
		s.end(span, ErrInvalidInput)
		return nil, ErrInvalidInput
	}
	out, err := s.repo.GetWithUser(ctx, orderNo)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = ErrNotFound
	}
	s.end(span, err)
	return out, err
}

// UpdateStatus 推进订单状态。
func (s *Service) UpdateStatus(ctx context.Context, orderNo, status string) (*model.Order, error) {
	ctx, span := s.span(ctx, "UpdateOrder")
	defer span.End()
	if strings.TrimSpace(orderNo) == "" || !validStatus(status) {
		s.end(span, ErrInvalidInput)
		return nil, ErrInvalidInput
	}
	err := s.repo.UpdateStatus(ctx, orderNo, status)
	if err == nil {
		// 回读一次让响应体带上新状态（一次额外的 SELECT，入链可见）。
		out, getErr := s.repo.Get(ctx, orderNo)
		err = getErr
		s.end(span, err)
		return out, err
	}
	s.end(span, err)
	return nil, err
}

// Delete 删除订单及其明细（两张表各一条 DELETE）。
func (s *Service) Delete(ctx context.Context, orderNo string) error {
	ctx, span := s.span(ctx, "DeleteOrder")
	defer span.End()
	if strings.TrimSpace(orderNo) == "" {
		s.end(span, ErrInvalidInput)
		return ErrInvalidInput
	}
	if err := s.repo.DeleteItems(ctx, orderNo); err != nil {
		s.end(span, err)
		return err
	}
	err := s.repo.Delete(ctx, orderNo)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = ErrNotFound
	}
	s.end(span, err)
	return err
}

// end 统一结束 span 的错误标记。
func (s *Service) end(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// newOrderNo 生成订单号：纳秒时间戳，demo 级唯一足够。
func newOrderNo() string { return fmt.Sprintf("NO%d", time.Now().UnixNano()) }

// validStatus 合法状态集合（超出即为调用方错误，不静默落库）。
func validStatus(status string) bool {
	switch status {
	case "created", "paid", "shipped", "done", "cancelled":
		return true
	}
	return false
}
