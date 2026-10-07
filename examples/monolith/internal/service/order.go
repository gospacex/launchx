// Package service 业务编排层：参数校验、金额计算、错误映射，自开
// `service.order.*` span。
//
// 边界：向下只调用 repository / 各组件适配层（cache / messaging / search / docs），
// 不直接碰 *gorm.DB、也不直接用任何 SDK 客户端，**也不 import 任何驱动** ——
// 仓储已在持久层边界把 gorm.ErrRecordNotFound 归一化成 repository.ErrNotFound，
// 本层只认领域错误；向上只被 handler 调用，HTTP 状态码由 handler 决定。
//
// 外部组件一律**可为 nil**：装配期降级（组件没配或连不上）时不至于拖垮整个服务，
// 但每次降级都会在对应的 service span 上留下标记 —— 静默跳过会让"这条数据为什么
// 没进 ES"变成无头案。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/gospacex/otelx/layer"

	"github.com/gospacex/launchx/examples/monolith/internal/cache"
	"github.com/gospacex/launchx/examples/monolith/internal/docs"
	"github.com/gospacex/launchx/examples/monolith/internal/messaging"
	"github.com/gospacex/launchx/examples/monolith/internal/model"
	"github.com/gospacex/launchx/examples/monolith/internal/repository"
	"github.com/gospacex/launchx/examples/monolith/internal/search"
)

// 领域错误。handler 按类型映射 HTTP 状态码，故这里的取值是业务语义而非 HTTP 语义。
var (
	// ErrInvalidInput 入参不合法（缺字段 / 数量非正 / 非法状态）。
	ErrInvalidInput = errors.New("order: invalid input")
	// ErrNotFound 订单不存在（由 repository.ErrNotFound 归一化而来）。
	//
	// 同一件事在本层与持久层判定相反，是**有意的**：
	//   - 持久层：一次 miss 是正常查询结果，标进 span 会让错误率失真 → 豁免；
	//   - 本层：「订单不存在」是这次业务操作失败，用户会看到 404 → 照常标错。
	// 判据不同是因为判据问的不是同一个问题（"SQL 出错了吗" vs "这单处理成功了吗"）。
	// 落到代码上就是 quiet 参数：repository 传 quietNotFound，本层传 nil。
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
//
// 四个外部组件指针可为 nil（对应 yml 未配或降级），各方法据此走「有则用、无则记」分支。
type Service struct {
	repo      *repository.Repository
	cache     *cache.Order
	messaging *messaging.Order
	search    *search.Order
	docs      *docs.Order
	traceOn   bool
}

// New 构造订单服务。
func New(repo *repository.Repository, c *cache.Order, m *messaging.Order, s *search.Order, d *docs.Order, traceOn bool) *Service {
	return &Service{repo: repo, cache: c, messaging: m, search: s, docs: d, traceOn: traceOn}
}

// begin 开 service 层 span，返回「收 error 再收尾」的闭包。
//
// quiet 传 nil：本层不豁免任何错误（见 ErrNotFound 注释里的两层判据差异）。
// 方法的结果列表因此必须具名，defer 里的 err 才能覆盖所有 return 路径。
func (s *Service) begin(ctx context.Context, op string) (context.Context, layer.End) {
	return layer.Begin(ctx, "service", "order", op, s.traceOn, nil)
}

// mark 记录「非致命」依赖失败：记 event 与属性，但**不改 span 状态码**。
//
// 与收尾闭包的错误标记区别是刻意的：span 变 Error 会让 Jaeger 把它算进错误率，
// 而缓存写失败这类降级并不代表请求失败；混在一起会让错误率指标失真。
func (s *Service) mark(ctx context.Context, reason string, err error) {
	span := trace.SpanFromContext(ctx)
	if span == nil || !span.IsRecording() {
		return
	}
	span.AddEvent(fmt.Sprintf("order: %s", reason))
	span.SetAttributes(
		attribute.String("degrade.reason", reason),
		attribute.String("degrade.error", err.Error()),
	)
}

// List 查询最近 limit 条订单。
func (s *Service) List(ctx context.Context, limit int) (out []model.Order, err error) {
	ctx, end := s.begin(ctx, "ListOrders")
	defer func() { end(err) }()
	if limit <= 0 {
		limit = 20
	}
	out, err = s.repo.List(ctx, limit)
	return out, err
}

// Get 按订单号查单条订单（单表路径 + 缓存 read-through）。
//
// 缓存是**可选优化**，不是数据源：命中直接回，未命中打库并把结果回写。
// 缓存出错刻意**不返回错误** —— 它只是加速件，坏了必须降级到打库而不是让请求失败；
// 但要在 span 上留 "cache.enabled / cache.hit" 之外的失败标记，否则命中率问题无从查。
func (s *Service) Get(ctx context.Context, orderNo string) (out *model.Order, err error) {
	ctx, end := s.begin(ctx, "GetOrder")
	defer func() { end(err) }()
	if strings.TrimSpace(orderNo) == "" {
		return nil, ErrInvalidInput
	}
	layer.Attrs(ctx, attribute.Bool("cache.enabled", s.cache != nil))

	if s.cache != nil {
		raw, cerr := s.cache.Get(ctx, orderNo)
		if cerr == nil && raw != "" {
			var cached model.Order
			if derr := json.Unmarshal([]byte(raw), &cached); derr != nil {
				// 缓存里的数据解析器都解析不出来，只能当 miss 处理；但必须留痕，
				// 否则"命中率看着很高却每次都打库"会完全无从解释。
				s.mark(ctx, "cache.decode_failed", derr)
			} else {
				layer.Attrs(ctx, attribute.Bool("cache.hit", true))
				return &cached, nil
			}
		} else if cerr != nil {
			s.mark(ctx, "cache.read_failed", cerr)
		}
	}

	out, err = s.repo.Get(ctx, orderNo)
	if errors.Is(err, repository.ErrNotFound) {
		err = ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	// 回写失败不影响本次响应：值已经拿到了，缓存没写进去下次再取即可。
	if s.cache != nil {
		payload, eerr := cache.Encode(out)
		if eerr == nil {
			if serr := s.cache.Set(ctx, orderNo, payload); serr != nil {
				s.mark(ctx, "cache.write_failed", serr)
			}
		}
	}
	return out, nil
}

// Create 建单：校验入参 → 算金额 → 事务写 orders + order_items → 投递/索引/快照。
//
// 三个外部动作（MQ 投递 / ES 索引 / Mongo 快照）在 demo 里**同步**做：
// 异步投递会产生无父上下文的孤儿 trace（正是本轮在 bff/srv 上修掉的那类噪声），
// 生产应由 outbox/后台任务承接，与本 demo 的演示意图无关，故不做。
func (s *Service) Create(ctx context.Context, in CreateInput) (out *model.Order, err error) {
	ctx, end := s.begin(ctx, "CreateOrder")
	defer func() { end(err) }()

	in.Item = strings.TrimSpace(in.Item)
	if in.Item == "" || in.Qty <= 0 {
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
	if err = s.repo.CreateWithItems(ctx, order, items); err != nil {
		return nil, err
	}

	// 以下三者在依赖面上互不相干，任一失败都不回滚订单（订单已落库是事实），
	// 只在 span 上留痕 —— 与 outbox 的"落库即事实、投递尽力"是同一套语义。
	s.fanout(ctx, order)
	out = order
	return out, nil
}

// fanout 把一单的下游动作分别落到 MQ / ES / Mongo，各产各自的 span 子树。
func (s *Service) fanout(ctx context.Context, order *model.Order) {
	payload, err := json.Marshal(order)
	if err != nil {
		s.mark(ctx, "event.encode_failed", err)
		return
	}

	if s.messaging != nil {
		if perr := s.messaging.Publish(ctx, order.OrderNo, payload); perr != nil {
			s.mark(ctx, "event.publish_failed", perr)
		}
	} else {
		layer.Attrs(ctx, attribute.Bool("mq.skipped", true))
	}

	if s.search != nil {
		doc := map[string]any{
			"order_no": order.OrderNo,
			"user_id":  order.UserID,
			"item":     order.Item,
			"qty":      order.Qty,
			"amount":   order.Amount,
			"status":   order.Status,
		}
		if ierr := s.search.Index(ctx, order.OrderNo, doc); ierr != nil {
			s.mark(ctx, "search.index_failed", ierr)
		}
	} else {
		layer.Attrs(ctx, attribute.Bool("search.skipped", true))
	}

	if s.docs != nil {
		snap := docs.OrderSnapshot{
			OrderNo: order.OrderNo,
			UserID:  order.UserID,
			Item:    order.Item,
			Qty:     order.Qty,
			Amount:  order.Amount,
			Status:  order.Status,
		}
		if _, serr := s.docs.Save(ctx, snap); serr != nil {
			s.mark(ctx, "docs.save_failed", serr)
		}
	} else {
		layer.Attrs(ctx, attribute.Bool("docs.skipped", true))
	}
}

// GetWithUser 连表查询：订单 + 下单用户。
//
// 刻意保留 raw JOIN 而不是拆成两次单表查询：T13 的判据是「JOIN 语句必须单独成
// span 且文本含 join」，拆成两条 SQL 会把它变成两个普通 SELECT，判据失效。
func (s *Service) GetWithUser(ctx context.Context, orderNo string) (out *model.OrderWithUser, err error) {
	ctx, end := s.begin(ctx, "GetOrderWithUser")
	defer func() { end(err) }()
	if strings.TrimSpace(orderNo) == "" {
		return nil, ErrInvalidInput
	}
	out, err = s.repo.GetWithUser(ctx, orderNo)
	if errors.Is(err, repository.ErrNotFound) {
		err = ErrNotFound
	}
	return out, err
}

// UpdateStatus 推进订单状态（改完失效缓存，避免读到旧值）。
func (s *Service) UpdateStatus(ctx context.Context, orderNo, status string) (out *model.Order, err error) {
	ctx, end := s.begin(ctx, "UpdateOrder")
	defer func() { end(err) }()
	if strings.TrimSpace(orderNo) == "" || !validStatus(status) {
		return nil, ErrInvalidInput
	}
	if err = s.repo.UpdateStatus(ctx, orderNo, status); err != nil {
		return nil, err
	}
	// 回读一次让响应体带上新状态（一次额外的 SELECT，入链可见）。
	out, err = s.repo.Get(ctx, orderNo)
	if err != nil {
		return nil, err
	}
	if s.cache != nil {
		if derr := s.cache.Delete(ctx, orderNo); derr != nil {
			s.mark(ctx, "cache.invalidate_failed", derr)
		}
	}
	return out, nil
}

// Delete 删除订单及其明细（两张表各一条 DELETE，并失效缓存）。
func (s *Service) Delete(ctx context.Context, orderNo string) (err error) {
	ctx, end := s.begin(ctx, "DeleteOrder")
	defer func() { end(err) }()
	if strings.TrimSpace(orderNo) == "" {
		return ErrInvalidInput
	}
	if err = s.repo.DeleteItems(ctx, orderNo); err != nil {
		return err
	}
	err = s.repo.Delete(ctx, orderNo)
	if errors.Is(err, repository.ErrNotFound) {
		err = ErrNotFound
	}
	if s.cache != nil {
		if derr := s.cache.Delete(ctx, orderNo); derr != nil {
			s.mark(ctx, "cache.invalidate_failed", derr)
		}
	}
	return err
}

// Search 按关键词检索订单（ES 路径）。关键词为空视为调用方错误，不查。
func (s *Service) Search(ctx context.Context, query string, limit int) (out []map[string]any, err error) {
	ctx, end := s.begin(ctx, "SearchOrders")
	defer func() { end(err) }()
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, ErrInvalidInput
	}
	if s.search == nil {
		// 组件不可用时不能返回"空列表"假装检索成功 —— 那是把降级伪装成没数据。
		return nil, errors.New("order: search component unavailable, check xmiddleware.searchx")
	}
	out, err = s.search.Search(ctx, query, limit)
	return out, err
}

// Snapshot 读取某单最近一份 Mongo 快照。
func (s *Service) Snapshot(ctx context.Context, orderNo string) (out map[string]any, err error) {
	ctx, end := s.begin(ctx, "GetOrderSnapshot")
	defer func() { end(err) }()
	if strings.TrimSpace(orderNo) == "" {
		return nil, ErrInvalidInput
	}
	if s.docs == nil {
		return nil, errors.New("order: docs component unavailable, check xmiddleware.mongodbx")
	}
	doc, err := s.docs.Find(ctx, orderNo)
	if err != nil {
		return nil, err
	}
	return map[string]any(doc), nil
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
