// Package repository 数据层：经 launchx 装配的 *gorm.DB 读写订单表。
//
// 边界：只做「一次 SQL / 一次事务」的读写，不含参数校验与业务决策；
// 每个方法自开一个 `repository.*` span，事务里的每条 SQL 由 dbx 的 gorm
// 回调各自成 span，挂在同一个方法 span 下。
package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/gospacex/otelx/layer"
	"gorm.io/gorm"

	"github.com/gospacex/launchx/examples/monolith/internal/model"
)

// ErrNotFound 订单不存在：把驱动层的 gorm.ErrRecordNotFound 在**持久层边界**
// 归一化成领域错误。
//
// 归一化点为什么必须在本层：service 里写 errors.Is(err, gorm.ErrRecordNotFound)
// 就等于让业务分层直引驱动 —— 那些判断与存储实现绑死，换库 / 换 ORM 要改业务。
// 单体与微服示例现在同一口径（微服各 srv 的 repository.ErrNotFound）。
var ErrNotFound = errors.New("repository: order not found")

// Repository 订单仓储：持权 launchx 挂载的 DB 句柄与调用链 tracer。
type Repository struct {
	db      *gorm.DB
	traceOn bool
}

// New 构造仓储。db 必须来自 App.DB() 的断言结果——本层不自建连接。
func New(db *gorm.DB, traceOn bool) *Repository {
	return &Repository{db: db, traceOn: traceOn}
}

// begin 开 repository 层 span，返回「收 error 再收尾」的闭包。
//
// 与微服 / 电商示例的 pkg/tracex 同一口径：span 句柄不交给调用方（本层因此
// 不碰 otel 的 SetStatus / RecordError），由收尾闭包代标错误 —— 少一次手写就
// 少一处漏标，漏标的表现是「SQL 报错但 span 是绿的」。
func (r *Repository) begin(ctx context.Context, op string) (context.Context, layer.End) {
	return layer.Begin(ctx, "repository", "order", op, r.traceOn, quietNotFound)
}

// quietNotFound 「记录不存在」不是故障：一次未命中是正常 miss，标成错误会让
// 「查一个不存在的订单」这种正常路径把错误率顶上去。
//
// 两种错误形态都认：本层已归一化出的 ErrNotFound，以及 dbx/gorm 直接抛出的
// ErrRecordNotFound（未走归一化分支时）。这条豁免规则与微服示例是同一条。
func quietNotFound(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, gorm.ErrRecordNotFound)
}

// List 单表批量查询（SELECT orders）。
//
// userID > 0 时按归属用户过滤（鉴权路径必须传，否则任何人都能翻别人的单）；
// userID == 0 表示不按用户收敛，只允许 demo 的无鉴权列表路径使用，且那条路径
// 已被 RegisterRoutes 的 loopbackOnly 裁决挡在回环之内（见 app/routes.go）：
// 监听地址不是回环时，路由表里所有免鉴权路径一律拒登、装配直接失败。
//
// 注：这句话曾是一句**假承诺** —— 全仓 grep `loopbackOnly` 零命中，注释声称的
// 闸门在代码里并不存在。现已补齐实现与判据（app/routes_test.go）。
func (r *Repository) List(ctx context.Context, userID int64, limit int) (out []model.Order, err error) {
	ctx, end := r.begin(ctx, "List")
	defer func() { end(err) }()
	q := r.db.WithContext(ctx).Order("id DESC").Limit(limit)
	if userID > 0 {
		q = q.Where("user_id = ?", userID)
	}
	err = q.Find(&out).Error
	return out, err
}

// Get 按订单号查单条（SELECT orders）。
//
// 收尾闭包收的是**返回给上层的**错误（已归一化成 ErrNotFound），故 quiet 判据
// 里 ErrNotFound 与 gorm.ErrRecordNotFound 两者都列 —— 覆盖归一化前后的形态。
func (r *Repository) Get(ctx context.Context, orderNo string) (out *model.Order, err error) {
	ctx, end := r.begin(ctx, "Get")
	defer func() { end(err) }()
	var o model.Order
	err = r.db.WithContext(ctx).Where("order_no = ?", orderNo).Take(&o).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = fmt.Errorf("get order %s: %w", orderNo, ErrNotFound)
		return nil, err
	}
	if err != nil {
		err = fmt.Errorf("get order %s: %w", orderNo, err)
		return nil, err
	}
	return &o, nil
}

// CreateWithItems 多表事务：orders 一条 INSERT + order_items 若干条 INSERT。
//
// 事务里的每条 SQL 各自成 span（同一父 = 本方法 span，互为兄弟），
// 这是 T13「事务中的多条 SQL 各自成 span」的落点，故刻意不把两次写入
// 合并成一条批量语句。
func (r *Repository) CreateWithItems(ctx context.Context, o *model.Order, items []model.OrderItem) (err error) {
	ctx, end := r.begin(ctx, "CreateWithItems")
	defer func() { end(err) }()
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		tx = tx.WithContext(ctx) // 保证事务内 SQL 仍挂在同一个 span 下
		if err := tx.Create(o).Error; err != nil {
			return err
		}
		for i := range items {
			items[i].OrderNo = o.OrderNo
			if err := tx.Create(&items[i]).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

// GetWithUser 连表查询：orders JOIN users。
//
// 单独一个方法、单独一条 SQL——JOIN 语句必须自己成一个 span，
// 与本层其它 SELECT 混在一起（比如预加载合并）会让 T13 的连表判据失效。
func (r *Repository) GetWithUser(ctx context.Context, orderNo string) (out *model.OrderWithUser, err error) {
	ctx, end := r.begin(ctx, "GetWithUser")
	defer func() { end(err) }()
	const joinSQL = "SELECT o.id, o.order_no, o.user_id, o.item, o.qty, o.amount, o.status, u.name AS user_name, u.level AS user_level FROM orders o JOIN users u ON o.user_id = u.id WHERE o.order_no = ?"
	var o model.OrderWithUser
	err = r.db.WithContext(ctx).Raw(joinSQL, orderNo).Take(&o).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = fmt.Errorf("get order with user %s: %w", orderNo, ErrNotFound)
		return nil, err
	}
	if err != nil {
		err = fmt.Errorf("get order with user %s: %w", orderNo, err)
		return nil, err
	}
	return &o, nil
}

// UpdateStatus 更新订单状态（UPDATE orders）。
func (r *Repository) UpdateStatus(ctx context.Context, orderNo, status string) (err error) {
	ctx, end := r.begin(ctx, "UpdateStatus")
	defer func() { end(err) }()
	err = r.db.WithContext(ctx).Model(&model.Order{}).
		Where("order_no = ?", orderNo).Update("status", status).Error
	return err
}

// DeleteWithItems 同事务删除订单与明细（DELETE order_items + DELETE orders）。
//
// 必须是**一个事务**而不是两条各自提交的 DELETE：先删明细再删主单，中间任何一步
// 失败都会留下「主单还在、明细没了」的孤儿订单 —— 读接口 JOIN 出来的结果是半张
// 单，而对账侧看到的是主子表条数对不上，两侧都说不清是哪次操作造成的。
//
// 为什么还要额外判 RowsAffected：gorm 的 Delete 在 RowsAffected=0 时 Error=nil
// （"没有行被删除"按 SQL 语义不算错误），于是"删一个不存在的订单"
// 在持久层是成功、在业务层是失败。这里把 RowsAffected==0 归一化成 ErrNotFound，
// 让上层能把它翻成 404 —— 否则任何 order_no 都返回 200 {"deleted":true}。
//
// 事务里的两条 SQL 各自成 span（同一父 = 本方法 span，互为兄弟），README §3 里
// "DELETE 删主子数据 → db.delete × 2" 指的就是这两条。
func (r *Repository) DeleteWithItems(ctx context.Context, orderNo string) (err error) {
	ctx, end := r.begin(ctx, "DeleteWithItems")
	defer func() { end(err) }()
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		tx = tx.WithContext(ctx) // 保证事务内 SQL 仍挂在同一个 span 下
		if err := tx.Where("order_no = ?", orderNo).Delete(&model.OrderItem{}).Error; err != nil {
			return err
		}
		res := tx.Where("order_no = ?", orderNo).Delete(&model.Order{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("delete order %s: %w", orderNo, ErrNotFound)
		}
		return nil
	})
	return err
}
