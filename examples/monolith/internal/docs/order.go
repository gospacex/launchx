// Package docs 订单快照适配层：把 mongodbx 的写/读包成本层的 `docs.*` span。
//
// 为什么单独存快照：同一份订单数据在 MySQL 里是行、在 ES 里是可检索文档，
// 快照存的是「当时的一整份订单（含明细）」这类**宽口径**数据 —— 三个库各存一种
// 形态（tiering），也顺带把文档库接进了链路。
package docs

import (
	"context"
	"errors"
	"fmt"

	mongodbx "github.com/gospacex/mongodbx"
	"go.opentelemetry.io/otel/attribute"

	"github.com/gospacex/otelx/layer"
)

// spanPrefix 本层前缀。
const spanPrefix = "docs."

// OrderSnapshot 订单快照（写入 Mongo 的文档形态）。
type OrderSnapshot struct {
	OrderNo string  `bson:"order_no"`
	UserID  int64   `bson:"user_id"`
	Item    string  `bson:"item"`
	Qty     int     `bson:"qty"`
	Amount  float64 `bson:"amount"`
	Status  string  `bson:"status"`
}

// Order 订单快照仓储。
type Order struct {
	coll    mongodbx.Collection
	traceOn bool
}

// NewOrder 构造快照仓储。
//
// 集合句柄（而非 DB）在装配层就取好：缺库缺集合的情况应当在装配期暴露，
// 不该拖到第一次写数据时才报错。
func NewOrder(coll mongodbx.Collection, traceOn bool) *Order {
	return &Order{coll: coll, traceOn: traceOn}
}

// begin 开本层 span（沿用两段式 docs.<Op> 命名），返回「收 error 再收尾」的闭包。
func (o *Order) begin(ctx context.Context, op string, quiet func(error) bool) (context.Context, layer.End) {
	return layer.Begin(ctx, "docs", "order", op, o.traceOn, quiet)
}

// Save 写一份快照，返回文档 ID。同一单多次写入会产生多份历史快照（追加语义），
// 便于演示"文档库存的是时间线"；只想覆盖写时应先 DeleteOne。
func (o *Order) Save(ctx context.Context, snap OrderSnapshot) (id string, err error) {
	ctx, end := o.begin(ctx, "SaveSnapshot", nil)
	defer func() { end(err) }()
	layer.Attrs(ctx, attribute.String("docs.entity", "order_snapshots"))

	id, err = o.coll.Insert(ctx, mongodbx.Document{
		"order_no": snap.OrderNo,
		"user_id":  snap.UserID,
		"item":     snap.Item,
		"qty":      snap.Qty,
		"amount":   snap.Amount,
		"status":   snap.Status,
	})
	if err != nil {
		err = fmt.Errorf("docs: save snapshot %q: %w", snap.OrderNo, err)
	}
	return id, err
}

// Find 按订单号取最近一份快照；无命中返回 mongodbx.ErrNotFound 的原义错误。
//
// quiet 例外：ErrNotFound 是一次正常 miss（快照本来就可能没写），不算 span 错误。
// 原实现里这条注释写了、代码却无条件 RecordError —— 注释与行为不符，借这次
// 收口一并改齐：豁免规则由 quiet 表达，不再是"注释说一套、代码做一套"。
func (o *Order) Find(ctx context.Context, orderNo string) (doc mongodbx.Document, err error) {
	ctx, end := o.begin(ctx, "FindSnapshot", func(err error) bool {
		return errors.Is(err, mongodbx.ErrNotFound)
	})
	defer func() { end(err) }()
	layer.Attrs(ctx, attribute.String("docs.entity", "order_snapshots"))

	doc, err = o.coll.FindOne(ctx, mongodbx.Document{"order_no": orderNo})
	if err != nil {
		err = fmt.Errorf("docs: find snapshot %q: %w", orderNo, err)
	}
	return doc, err
}
