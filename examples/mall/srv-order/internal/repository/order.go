// Package repository srv-order 持久层（私有库 order_db，账号 mall_rw）。
//
// 在分层链上的位置：service → **repository** → model。它的上游只有 service，
// 下游只有 model 与 dbx 句柄。
//
// 句柄来源：db 是 launchx App.DB() 交出的 *gorm.DB（dbx 组件，自带 OTel
// 回调）。本包不 gorm.Open、不 import 任何驱动 —— 装配期由
// pkg/handlecheck.VerifyDBX 断言这件事，跑错句柄会在启动时直接失败。
//
// 纪律：
//   - 连表查询一律 Find（多行投影到 slice），禁用 Scan（会静默丢列）
//   - 1:N 子表 JOIN 必须 GROUP BY 收敛，计数一律 COUNT(DISTINCT ...)抵消行放大
//   - 错误一律 %w 包装，便于上层 errors.Is 判定
//   - 一次方法 = 一次 SQL / 一次事务：事务里的每条 SQL 由 dbx 的 gorm 回调
//     各自成 span，Jaeger 上能看清"下一单到底动了几张表"
//   - 每个方法开一个 repository.order.<op> span，与 service / handler 的
//     span 串成一条链
package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/gospacex/launchx/examples/mall/srv-order/internal/model"
	"github.com/gospacex/launchx/examples/pkg/outbox"
	"github.com/gospacex/otelx/layer"
)

// ErrNotFound 无记录（service 层据此翻译成 CodeOrderNotFound）
var ErrNotFound = errors.New("repository: record not found")

// ErrBadDateRange 日期区间非法（service 层据此翻译成 CodeInvalidParam，
// 而不是笼统的内部错误 —— 那是调用方传错，重试也没用）
var ErrBadDateRange = errors.New("repository: bad date range")

// CreateOrderInput 下单事务输入
type CreateOrderInput struct {
	UserID      uint64
	OrderNo     string
	TotalAmount int64
	Items       []model.OrderItem
	// OutboxFill 在事务内、订单行落库后（自增 ID 已知）回调，产出事件体与
	// 随 header 透传的 traceparent。nil 时 outbox 行 payload/traceparent
	// 为空（Runner 直接放弃该行，不占重试配额）。
	OutboxFill func(orderID uint64) (payload, traceparent string)
}

// CreateOrderOutput 下单事务结果
type CreateOrderOutput struct {
	OrderID uint64
	OrderNo string
}

// UpdateStatusInput 状态流转输入（orders + order_status_log 两表事务）
type UpdateStatusInput struct {
	OrderID uint64
	UserID  uint64
	To      string
}

// OrderRepository order_db 仓储
type OrderRepository struct {
	db      *gorm.DB
	traceOn bool
}

// NewOrderRepository 构造。
//
// db 必须是 launchx App.DB() 交出的 dbx 句柄：只有它带 OTel 回调，
// 本层开的 span 下面才挂得住每条 SQL 的 dbx span。
func NewOrderRepository(db *gorm.DB, traceOn bool) *OrderRepository {
	return &OrderRepository{db: db, traceOn: traceOn}
}

// DB 暴露句柄（pkg/handlecheck 断言 / 对账用）
func (r *OrderRepository) DB() *gorm.DB { return r.db }

// begin 开 repository 层 span，返回「收 error 再收尾」的闭包。
//
// traceOn=false 时返回空闭包，调用方一律 `defer func() { end(err) }()`，
// 不必在每个方法里写 if。为什么闭包要收 error：span 句柄刻意不交给调用方
// （本层因此零 otel 依赖），代价是调用方也标不了状态 —— 收 error 由闭包
// 代标，失败才在 Jaeger 上可见（否则「SQL 报错」与「SQL 成功」同色）。
//
// quiet 规则：ErrNotFound 是正常 miss，不是故障 —— 与单体示例 recordError
// 的同一条语义，命中即豁免错误标记，避免正常未命中污染错误率。
func (r *OrderRepository) begin(ctx context.Context, op string) (context.Context, layer.End) {
	return layer.Begin(ctx, "repository", "order", op, r.traceOn, quietNotFound)
}

// quietNotFound 「记录不存在」不是故障。两种形态都认：本层已归一化出的
// ErrNotFound，以及 dbx/gorm 直接抛出的 ErrRecordNotFound（未归一化路径）。
func quietNotFound(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, gorm.ErrRecordNotFound)
}

// ---------- 单表 CRUD ----------

// GetOrderByNo 单表查询：按订单号取订单（幂等校验 / 对账入口）
func (r *OrderRepository) GetOrderByNo(ctx context.Context, orderNo string) (out *model.Order, err error) {
	ctx, end := r.begin(ctx, "GetOrderByNo")
	defer func() { end(err) }()
	var o model.Order
	err = r.db.WithContext(ctx).Where("order_no = ?", orderNo).Take(&o).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("get order by no %q: %w", orderNo, ErrNotFound)
		}
		return nil, fmt.Errorf("get order by no %q: %w", orderNo, err)
	}
	return &o, nil
}

// ---------- 多表 CRUD ----------

// ListOrders 2 表 LEFT JOIN + 聚合（orders ⋈ order_items），订单流水列表。
//
// 为什么不继续单表查主表：流水页要回答"这一单几件、明细合计是不是等于
// 订单总额"，只查主表就得为每一行再补一次明细查询（N+1），或者让调用方
// 自己拿总额去猜。明细是 1:N，JOIN 会放大行数，故 GROUP BY 收敛回一行、
// 计数用 COUNT(DISTINCT)；只 JOIN 了一张子表，SUM(quantity*unit_price)
// 不会被交叉放大，可以放心直接求和。
func (r *OrderRepository) ListOrders(ctx context.Context, q model.ListOrdersQuery) (out []model.OrderFlowRow, err error) {
	ctx, end := r.begin(ctx, "ListOrders")
	defer func() { end(err) }()
	from, to, err := dateRange(q.FromDate, q.ToDate)
	if err != nil {
		return nil, fmt.Errorf("list orders: %w", err)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500 // 上限写死：流水接口被刷是常态，不设上限等于给了全表扫描入口
	}
	qb := r.db.WithContext(ctx).
		Table("orders").
		Select("orders.id, orders.user_id, orders.order_no, orders.status, "+
			"orders.total_amount, orders.created_at, "+
			"COUNT(DISTINCT order_items.id) AS item_count, "+
			"COALESCE(SUM(order_items.quantity * order_items.unit_price), 0) AS item_amount").
		Joins("LEFT JOIN order_items ON order_items.order_id = orders.id").
		Where("orders.user_id = ?", q.UserID).
		Group("orders.id, orders.user_id, orders.order_no, orders.status, orders.total_amount, orders.created_at").
		Order("orders.id DESC")
	if !from.IsZero() {
		qb = qb.Where("orders.created_at >= ?", from)
	}
	if !to.IsZero() {
		qb = qb.Where("orders.created_at < ?", to)
	}
	var rows []model.OrderFlowRow
	if err := qb.Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list orders %d: %w", q.UserID, err)
	}
	return rows, nil
}

// UpdateStatusTx 2 表事务：orders.status 与 order_status_log 必须同生共死 ——
// 只改状态不落日志，对账时无法回答"这一单什么时候变成 paid 的"。
//
// 幂等：已是目标态时直接返回 nil，不重复记日志 —— 重试/重复点击不该把
// 状态流转次数刷高（报表里的 status_change_count 会失真）。
func (r *OrderRepository) UpdateStatusTx(ctx context.Context, in UpdateStatusInput) (err error) {
	ctx, end := r.begin(ctx, "UpdateStatusTx")
	defer func() { end(err) }()
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var o model.Order
		if err := tx.Where("id = ? AND user_id = ?", in.OrderID, in.UserID).Take(&o).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("update status %d: %w", in.OrderID, ErrNotFound)
			}
			return fmt.Errorf("load order %d: %w", in.OrderID, err)
		}
		if o.Status == in.To {
			return nil // 幂等重放：已是目标态，不重复记日志
		}
		if err := tx.Model(&model.Order{}).
			Where("id = ? AND user_id = ?", in.OrderID, in.UserID).
			Update("status", in.To).Error; err != nil {
			return fmt.Errorf("update order status %d: %w", in.OrderID, err)
		}
		log := model.StatusLog{OrderID: in.OrderID, FromStatus: o.Status, ToStatus: in.To}
		if err := tx.Create(&log).Error; err != nil {
			return fmt.Errorf("insert status log %d: %w", in.OrderID, err)
		}
		return nil
	})
}

// GetOrder 2 表 JOIN：orders ⋈ order_items，查单个订单 + 明细。
//
// 明细是 1:N：JOIN 结果按行 Find 到 slice 后在 Go 侧按 order_id 分组展开
// （保持"JOIN 一律 Find、禁 Scan"的纪律，同时不丢多行明细）。
//
// user_id 条件必带：订单是用户私有数据，跨域身份透传后这里是最小授权面，
// 少了它就能凭 order_id 遍历别人的订单。
func (r *OrderRepository) GetOrder(ctx context.Context, q model.GetOrderQuery) (out *model.OrderWithItems, err error) {
	ctx, end := r.begin(ctx, "GetOrder")
	defer func() { end(err) }()
	var rows []model.OrderJoinRow
	err = r.db.WithContext(ctx).
		Table("orders").
		Select("orders.id AS order_id, orders.user_id, orders.order_no, orders.status, "+
			"orders.total_amount, orders.paid_amount, orders.created_at, orders.updated_at, "+
			"order_items.id AS item_id, order_items.product_id, order_items.product_name, "+
			"order_items.quantity, order_items.unit_price").
		Joins("LEFT JOIN order_items ON order_items.order_id = orders.id").
		Where("orders.id = ? AND orders.user_id = ?", q.OrderID, q.UserID).
		Order("order_items.id ASC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("get order %d: %w", q.OrderID, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("get order %d: %w", q.OrderID, ErrNotFound)
	}
	head := rows[0]
	out = &model.OrderWithItems{Order: model.Order{
		ID: head.OrderID, UserID: head.UserID, OrderNo: head.OrderNo, Status: head.Status,
		TotalAmount: head.TotalAmount, PaidAmount: head.PaidAmount,
		CreatedAt: head.CreatedAt, UpdatedAt: head.UpdatedAt,
	}}
	for _, rw := range rows {
		if rw.ItemID == 0 { // LEFT JOIN 未命中明细
			continue
		}
		out.Items = append(out.Items, model.OrderItem{
			ID: rw.ItemID, OrderID: rw.OrderID, ProductID: rw.ProductID,
			ProductName: rw.ProductName, Quantity: rw.Quantity, UnitPrice: rw.UnitPrice,
		})
	}
	return out, nil
}

// CreateOrderTx 3 表事务：orders + order_items + outbox 一次提交。
//
// outbox 行与业务行同事务是本包的立身之本：不同事务就会出现"订单落了、
// 事件没落"（下游永远不知道这单存在）或"事件落了、订单回滚"（幽灵事件）。
func (r *OrderRepository) CreateOrderTx(ctx context.Context, in CreateOrderInput) (out *CreateOrderOutput, err error) {
	ctx, end := r.begin(ctx, "CreateOrderTx")
	defer func() { end(err) }()
	out = &CreateOrderOutput{OrderNo: in.OrderNo}
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		o := model.Order{
			UserID:      in.UserID,
			OrderNo:     in.OrderNo,
			Status:      model.StatusCreated,
			TotalAmount: in.TotalAmount,
		}
		if err := tx.Create(&o).Error; err != nil {
			return fmt.Errorf("insert order: %w", err)
		}
		items := make([]model.OrderItem, len(in.Items))
		for i, it := range in.Items {
			items[i] = model.OrderItem{
				OrderID:     o.ID,
				ProductID:   it.ProductID,
				ProductName: it.ProductName,
				Quantity:    it.Quantity,
				UnitPrice:   it.UnitPrice,
			}
		}
		if len(items) > 0 {
			if err := tx.Create(&items).Error; err != nil {
				return fmt.Errorf("insert order items: %w", err)
			}
		}
		ev := outbox.Event{
			Topic:     "order.completed.v1",
			EventType: "order.completed",
			Status:    outbox.StatusPending,
		}
		if in.OutboxFill != nil {
			ev.Payload, ev.Traceparent = in.OutboxFill(o.ID)
		}
		if err := tx.Create(&ev).Error; err != nil {
			return fmt.Errorf("insert outbox event: %w", err)
		}
		out.OrderID = o.ID
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("create order tx: %w", err)
	}
	return out, nil
}

// Report 4 表 JOIN 报表（orders ⋈ order_items ⋈ order_status_log ⋈ refund_records）。
//
// fromDate/toDate 为 yyyy-MM-dd（闭区间）；空串表示不限。
// 明细/状态/退款计数用 COUNT(DISTINCT) 抵消多表 JOIN 的行放大；
// 退款额用标量子查询单独 SUM（三个 1:N 子表交叉 JOIN 会把 SUM 放大成
// 笛卡尔积的倍数，标量子查询是这里唯一正确的写法）。
func (r *OrderRepository) Report(ctx context.Context, q model.OrderReportQuery) (out []model.ReportRow, err error) {
	ctx, end := r.begin(ctx, "Report")
	defer func() { end(err) }()
	from, to, err := dateRange(q.FromDate, q.ToDate)
	if err != nil {
		return nil, fmt.Errorf("report: %w", err)
	}
	qb := r.db.WithContext(ctx).
		Table("orders").
		Select("orders.id AS order_id, orders.order_no, orders.status, orders.total_amount, "+
			"COUNT(DISTINCT order_items.id) AS item_count, "+
			"COUNT(DISTINCT order_status_log.id) AS status_change_count, "+
			"COALESCE((SELECT SUM(refund_records.amount) FROM refund_records "+
			"WHERE refund_records.order_id = orders.id AND refund_records.status = 'success'), 0) AS refund_amount").
		Joins("LEFT JOIN order_items ON order_items.order_id = orders.id").
		Joins("LEFT JOIN order_status_log ON order_status_log.order_id = orders.id").
		Joins("LEFT JOIN refund_records ON refund_records.order_id = orders.id").
		Where("orders.user_id = ?", q.UserID).
		Group("orders.id, orders.order_no, orders.status, orders.total_amount").
		Order("orders.id ASC")
	if !from.IsZero() {
		qb = qb.Where("orders.created_at >= ?", from)
	}
	if !to.IsZero() {
		qb = qb.Where("orders.created_at < ?", to)
	}
	var rows []model.ReportRow
	if err := qb.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("report %d: %w", q.UserID, err)
	}
	return rows, nil
}

// dateRange 解析 yyyy-MM-dd 闭区间为 [from, to)；空串为零值。
//
// 解析失败统一包成 ErrBadDateRange：service 要据此回 CodeInvalidParam，
// 让调用方知道"改参数再来"，而不是报一个看起来需要运维介入的内部错误。
func dateRange(fromDate, toDate string) (time.Time, time.Time, error) {
	var from, to time.Time
	if fromDate != "" {
		t, err := time.Parse("2006-01-02", fromDate)
		if err != nil {
			return from, to, fmt.Errorf("from_date %q: %w: %w", fromDate, ErrBadDateRange, err)
		}
		from = t
	}
	if toDate != "" {
		t, err := time.Parse("2006-01-02", toDate)
		if err != nil {
			return from, to, fmt.Errorf("to_date %q: %w: %w", toDate, ErrBadDateRange, err)
		}
		to = t.Add(24 * time.Hour) // 闭区间转半开区间
	}
	return from, to, nil
}
