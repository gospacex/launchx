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
	"math"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/gospacex/logx"

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
	// ErrInvalidInput 入参不合法（缺字段 / 数量非正 / 非法状态 / 负单价）。
	ErrInvalidInput = errors.New("order: invalid input")
	// ErrNotFound 订单不存在（由 repository.ErrNotFound 归一化而来）。
	//
	// 同一件事在本层与持久层判定相反，是**有意的**：
	//   - 持久层：一次 miss 是正常查询结果，标进 span 会让错误率失真 → 豁免；
	//   - 本层：「订单不存在」是这次业务操作失败，用户会看到 404 → 照常标错。
	// 判据不同是因为判据问的不是同一个问题（"SQL 出错了吗" vs "这单处理成功了吗"）。
	// 落到代码上就是 quiet 参数：repository 传 quietNotFound，本层传 nil。
	ErrNotFound = errors.New("order: not found")
	// ErrComponentUnavailable 外部组件没装配（配了段但句柄是 nil）。
	//
	// 取代了早先 handler 里按 `strings.Contains(err.Error(), "component unavailable")`
	// 判状态码的做法：那个字符串是两处各写一遍的常量，改一处就会静默退化成 500，
	// 而调用方拿到 500 的第一反应是"服务崩了"，与"缺件"的处理动作完全不同。
	// 哨兵错误让这层判断落在类型上，文案怎么改都不影响。
	ErrComponentUnavailable = errors.New("order: component unavailable")
)

// DemoDefaults demo 的可调参数：缺省单价与列表分页上下界。
//
// 由装配层从 config.yaml 的 demo.* 喂进来，而不是写在本层的字面量里 ——
// 默认值参与"金额计算"与"查询规模"两件有后果的事：单价决定建单金额，
// limit 上界决定一次请求最多扫多少行，写死就只能改代码重编。
type DemoDefaults struct {
	DefaultUnitPrice float64 // unit_price 未提供时的缺省单价
	DefaultLimit     int     // limit 未提供时的缺省条数
	MaxLimit         int     // limit 上界（超过即截断，不接受无界查询）
}

// 装配层没喂（零值）时的保守回落：宁可不用缺省单价，也不能让输出 0 元订单；
// 分页上下界同样不能落到 0（0 在现有语义里是"不传"，会被当成缺省）。
const (
	fallbackUnitPrice = 9.9
	fallbackLimit     = 20
	fallbackMaxLimit  = 200
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
	logger    logx.Logger
	traceOn   bool
	demo      DemoDefaults
}

// New 构造订单服务。
//
// demo 里的可调参数（缺省单价 / 分页上下界）由装配层从配置喂入；零值一律回落到
// 包级 fallback* 常量 —— 丢了一个配置项就让服务"没有单价"或"不限条数"是不可接
// 受的降级方向。
func New(repo *repository.Repository, c *cache.Order, m *messaging.Order, s *search.Order, d *docs.Order, logger logx.Logger, traceOn bool, demo DemoDefaults) *Service {
	if demo.DefaultUnitPrice <= 0 {
		demo.DefaultUnitPrice = fallbackUnitPrice
	}
	if demo.DefaultLimit <= 0 {
		demo.DefaultLimit = fallbackLimit
	}
	if demo.MaxLimit <= 0 {
		demo.MaxLimit = fallbackMaxLimit
	}
	return &Service{repo: repo, cache: c, messaging: m, search: s, docs: d, logger: logger, traceOn: traceOn, demo: demo}
}

// begin 开 service 层 span，返回「收 error 再收尾」的闭包。
//
// quiet 传 nil：本层不豁免任何错误（见 ErrNotFound 注释里的两层判据差异）。
// 方法的结果列表因此必须具名，defer 里的 err 才能覆盖所有 return 路径。
func (s *Service) begin(ctx context.Context, op string) (context.Context, layer.End) {
	return layer.Begin(ctx, "service", "order", op, s.traceOn, nil)
}

// mark 记录「非致命」依赖失败：记日志 + 记 event 与属性，但**不改 span 状态码**。
//
// 与收尾闭包的错误标记区别是刻意的：span 变 Error 会让 Jaeger 把它算进错误率，
// 而缓存写失败这类降级并不代表请求失败；混在一起会让错误率指标失真。
//
// 日志必须在 span 之外**单独**打：早先这里只在 span 上留痕，而 span 不录时
// （trace.enabled=false —— 这是生产上为了成本常态关闭的开关）函数直接返回，
// 于是「事件没投出去 / 索引没写 / 快照没存」在线上完全静默。降级不是请求失败，
// 但不该是不可观测的：查「为什么 ES 里没有这单」时日志是唯一能回答的东西。
func (s *Service) mark(ctx context.Context, orderNo string, reason string, err error) {
	if s.logger != nil {
		s.logger.Warn("订单下游动作失败（不回滚已落库订单）",
			logx.String("order_no", orderNo),
			logx.String("degrade.reason", reason),
			logx.String("degrade.error", err.Error()))
	}
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

// List 查询订单列表。
//
// userID > 0 时按归属用户收敛；userID == 0 表示**不按用户过滤**。后者不是"忘了传
// 就漏数据"，而是给默认业务含义的：它在仓储侧不加 user_id 条件，故只允许出现在
// 演示用的无鉴权列表路径上 —— 那条路径已被 RegisterRoutes 锁成"只能绑回环地址"，
// 服务一旦对外监听就注册不上（详见 app/routes.go 的 loopbackOnly 注释）。
//
// limit 在这里统一 clamp 到 [1, demo.MaxLimit]：
// 透传会让 ?limit=99999999 变成一次无界全表扫描，而列表接口恰恰是最容易被外部
// 拿去打的接口。
func (s *Service) List(ctx context.Context, userID int64, limit int) (out []model.Order, err error) {
	ctx, end := s.begin(ctx, "ListOrders")
	defer func() { end(err) }()
	limit = s.clampLimit(limit)
	layer.Attrs(ctx,
		attribute.Int64("order.user_id", userID),
		attribute.Bool("order.scoped_by_user", userID > 0),
	)
	out, err = s.repo.List(ctx, userID, limit)
	return out, err
}

// clampLimit 把 limit 收敛到 [1, MaxLimit]。0 或负数取 DefaultLimit。
func (s *Service) clampLimit(limit int) int {
	switch {
	case limit <= 0:
		return s.demo.DefaultLimit
	case limit > s.demo.MaxLimit:
		return s.demo.MaxLimit
	default:
		return limit
	}
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
				s.mark(ctx, orderNo, "cache.decode_failed", derr)
			} else {
				layer.Attrs(ctx, attribute.Bool("cache.hit", true))
				return &cached, nil
			}
		} else if cerr != nil {
			s.mark(ctx, orderNo, "cache.read_failed", cerr)
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
				s.mark(ctx, orderNo, "cache.write_failed", serr)
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
	// unit_price 的三种取值分开处理：>0 照用；==0 表示"没提供"→ 取缺省单价；
	// <0 一律拒绝。早先是 `if unitPrice <= 0 { unitPrice = 9.9 }`，于是 -100 会
	// 被静默改成一个正数，生成一笔金额看着完全正常的订单 —— 比报错糟得多：
	// 调用方的 bug 进了库，而且账单上还看不出来。
	switch {
	case in.UnitPrice < 0:
		return nil, ErrInvalidInput
	case in.UnitPrice == 0:
		in.UnitPrice = s.demo.DefaultUnitPrice
	}
	sku := strings.TrimSpace(in.SKU)
	if sku == "" {
		sku = "SKU-" + in.Item
	}

	amount := money(float64(in.Qty) * in.UnitPrice)
	order := &model.Order{
		OrderNo: newOrderNo(),
		UserID:  in.UserID,
		Item:    in.Item,
		Qty:     in.Qty,
		Amount:  amount,
		Status:  "created",
	}
	items := []model.OrderItem{{
		SKU:      sku,
		Qty:      in.Qty,
		Price:    in.UnitPrice,
		Subtotal: amount,
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
		s.mark(ctx, order.OrderNo, "event.encode_failed", err)
		return
	}

	if s.messaging != nil {
		if perr := s.messaging.Publish(ctx, order.OrderNo, payload); perr != nil {
			s.mark(ctx, order.OrderNo, "event.publish_failed", perr)
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
			s.mark(ctx, order.OrderNo, "search.index_failed", ierr)
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
			s.mark(ctx, order.OrderNo, "docs.save_failed", serr)
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

// UpdateStatus 推进订单状态。
//
// 改完之后**回写**缓存而不是单纯失效：Get 是 read-through（未命中打库后回写），
// 只 Delete 的话，删除与"另一个并发读请求打库并回填旧值"之间存在一个窗口 ——
// 旧值会被重新写进缓存，之后所有读都拿到旧状态，直到 TTL 到期才自愈。
// 直接写回本次读到的新值让竞态退化成"一次冗余写"：最后一个写进去的仍是库里的
// 最新值。（彻底解仍是穿透加锁或版本号，demo 不引入。）
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
		payload, eerr := cache.Encode(out)
		if eerr != nil {
			s.mark(ctx, orderNo, "cache.encode_failed", eerr)
			return out, nil
		}
		if serr := s.cache.Set(ctx, orderNo, payload); serr != nil {
			s.mark(ctx, orderNo, "cache.write_failed", serr)
		}
	}
	return out, nil
}

// Delete 删除订单及其明细（一个事务删两张表，并失效缓存）。
func (s *Service) Delete(ctx context.Context, orderNo string) (err error) {
	ctx, end := s.begin(ctx, "DeleteOrder")
	defer func() { end(err) }()
	if strings.TrimSpace(orderNo) == "" {
		return ErrInvalidInput
	}
	// 两张表必须在同一个事务里删（中途失败会留下"主单在、明细没了"的孤儿订单），
	// 事务做不到的另一件事是 RowsAffected==0 → ErrNotFound，由持久层归一化
	// （见 repository.DeleteWithItems），本层只负责翻译成领域错误交给 handler 翻 404。
	err = s.repo.DeleteWithItems(ctx, orderNo)
	if errors.Is(err, repository.ErrNotFound) {
		err = ErrNotFound
	}
	if s.cache != nil {
		if derr := s.cache.Delete(ctx, orderNo); derr != nil {
			s.mark(ctx, orderNo, "cache.invalidate_failed", derr)
		}
		// 延迟双删：上面那次删除与"并发读请求把旧值回填"之间还有窗口，
		// 隔一会儿再删一次可以覆盖它。
		s.delayedInvalidate(ctx, orderNo, delayedInvalidateWindow)
	}
	return err
}

// delayedInvalidateWindow 延迟双删的第二删间隔。
//
// 取值只需大于一次「读 → 打库 → 回写缓存」的耗时；写死 500ms 是因为它是 demo 的
// 工程近似，不值得为它加一条配置。
const delayedInvalidateWindow = 500 * time.Millisecond

// delayedInvalidate 在一个延迟之后再删一次缓存。
//
// 两条约束：
//   - 必须脱离请求 ctx 的取消链：响应返回后请求 ctx 随即 Done，挂着它等于第二删
//     永远不执行（内存对话实现还会顺带报 context canceled，让人以为是 Redis 挂了）；
//   - 必须自己结束：这里用 time.AfterFunc 而不是裸 goroutine + Sleep，定时器最多
//     活 500ms，且触发后 goroutine 即退出，不构成泄漏。
func (s *Service) delayedInvalidate(ctx context.Context, orderNo string, delay time.Duration) {
	// WithoutCancel 保留 span 上下文（Degrade 第二删仍然挂在同一条 trace 下），
	// 只丢掉取消信号。
	detached := context.WithoutCancel(ctx)
	time.AfterFunc(delay, func() {
		if derr := s.cache.Delete(detached, orderNo); derr != nil {
			s.mark(detached, orderNo, "cache.delayed_invalidate_failed", derr)
		}
	})
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
		// 用哨兵错误而不是拼一段含 "component unavailable" 的文本：
		// handler 按 errors.Is 判状态码，文案再改也不会退化成 500。
		return nil, fmt.Errorf("%w: check xmiddleware.searchx", ErrComponentUnavailable)
	}
	// 与 List 同一套上下界：检索同样不接受 ?limit=99999999。
	out, err = s.search.Search(ctx, query, s.clampLimit(limit))
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
		return nil, fmt.Errorf("%w: check xmiddleware.mongodbx", ErrComponentUnavailable)
	}
	doc, err := s.docs.Find(ctx, orderNo)
	if err != nil {
		return nil, err
	}
	return map[string]any(doc), nil
}

// orderSeq 订单号的进程内单调后缀。
//
// 只靠 time.Now().UnixNano() 不够：同一进程里并发建单拿到同一纳秒的概率不为零
// （NTP 调一下时钟还会让时间戳回退），撞上 order_no 唯一索引就是一句
// "Duplicate entry"，而 demo 的表现只是"这次下单失败了，再试一次就好了"。
// 后缀取 atomic 自增的低位：单进程内严格递增，够用且不需要外部发号器。
var orderSeq uint32

// newOrderNo 生成订单号：纳秒时间戳 + 进程内原子后缀。
func newOrderNo() string {
	return fmt.Sprintf("NO%d%04X", time.Now().UnixNano(), atomic.AddUint32(&orderSeq, 1)%0x10000)
}

// money 把金额收敛到两位小数（四舍五入）。
//
// float64 参与乘加会让 9.9*3 得到 29.699999999999996：MySQL 的 decimal(10,2) 列
// 在写库时替我们四舍五入，于是库内是干净的；但 ES 索引与 Mongo 快照存的是未取整
// 的原值 —— 三份副本对账时会凭空出现一批"不一致"。在 fanout 之前统一取整比在
// 三个出口各修一次便宜。
//
// 彻底解是整数分（或 shopspring/decimal），那要动模型字段类型与三处落地口径，
// 超出本轮范围；这里只做 fanout 前的收口，让三份副本看到同一个数。
func money(v float64) float64 { return math.Round(v*100) / 100 }

// validStatus 合法状态集合（超出即为调用方错误，不静默落库）。
func validStatus(status string) bool {
	switch status {
	case "created", "paid", "shipped", "done", "cancelled":
		return true
	}
	return false
}
