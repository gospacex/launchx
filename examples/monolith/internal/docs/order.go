// Package docs 订单快照适配层：把 mongodbx 的写/读包成本层的 `docs.*` span。
//
// 为什么单独存快照：同一份订单数据在 MySQL 里是行、在 ES 里是可检索文档，
// 快照存的是「当时的一整份订单（含明细）」这类**宽口径**数据 —— 三个库各存一种
// 形态（tiering），也顺带把文档库接进了链路。
package docs

import (
	"context"
	"fmt"

	mongodbx "github.com/gospacex/mongodbx"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
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
	coll mongodbx.Collection
	tr   trace.Tracer
}

// NewOrder 构造快照仓储。
//
// 集合句柄（而非 DB）在装配层就取好：缺库缺集合的情况应当在装配期暴露，
// 不该拖到第一次写数据时才报错。
func NewOrder(coll mongodbx.Collection, tr trace.Tracer) *Order {
	return &Order{coll: coll, tr: tr}
}

// Save 写一份快照，返回文档 ID。同一单多次写入会产生多份历史快照（追加语义），
// 便于演示"文档库存的是时间线"；只想覆盖写时应先 DeleteOne。
func (o *Order) Save(ctx context.Context, snap OrderSnapshot) (string, error) {
	ctx, span := o.tr.Start(ctx, spanPrefix+"SaveSnapshot")
	defer span.End()
	span.SetAttributes(attribute.String("docs.entity", "order_snapshots"))

	id, err := o.coll.Insert(ctx, mongodbx.Document{
		"order_no": snap.OrderNo,
		"user_id":  snap.UserID,
		"item":     snap.Item,
		"qty":      snap.Qty,
		"amount":   snap.Amount,
		"status":   snap.Status,
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", fmt.Errorf("docs: save snapshot %q: %w", snap.OrderNo, err)
	}
	return id, nil
}

// Find 按订单号取最近一份快照；无命中返回 mongodbx.ErrNotFound 的原义错误。
func (o *Order) Find(ctx context.Context, orderNo string) (mongodbx.Document, error) {
	ctx, span := o.tr.Start(ctx, spanPrefix+"FindSnapshot")
	defer span.End()
	span.SetAttributes(attribute.String("docs.entity", "order_snapshots"))

	doc, err := o.coll.FindOne(ctx, mongodbx.Document{"order_no": orderNo})
	if err != nil {
		// ErrNotFound 是一次正常 miss，不算 span 错误；其余要看得见。
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("docs: find snapshot %q: %w", orderNo, err)
	}
	return doc, nil
}
