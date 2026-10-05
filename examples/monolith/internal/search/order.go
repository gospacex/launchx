// Package search 订单检索适配层：把 searchx 的写/查包成本层的 `search.*` span。
//
// 边界：只做「一条文档的 Index / 一次 query 的 Search」，不做检索 relevance 调优；
// 索引名由配置传入，本层不出现任何字面地址或字面索引名。
package search

import (
	"context"
	"fmt"

	"github.com/gospacex/searchx"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// spanPrefix 本层前缀。
const spanPrefix = "search."

// Order 订单索引。
type Order struct {
	cli   searchx.SearchClient
	index string
	tr    trace.Tracer
}

// NewOrder 构造订单索引。cli 由装配层从 App.Search() 取（业务不自建 ES 客户端）。
func NewOrder(cli searchx.SearchClient, index string, tr trace.Tracer) *Order {
	return &Order{cli: cli, index: index, tr: tr}
}

// Index 写一条订单进索引（订单号即文档 ID：幂等，覆盖写 = 更新）。
func (o *Order) Index(ctx context.Context, id string, doc map[string]any) error {
	ctx, span := o.tr.Start(ctx, spanPrefix+"IndexOrder")
	defer span.End()
	span.SetAttributes(
		attribute.String("search.index", o.index),
		attribute.String("search.document_id", id),
	)

	if err := o.cli.Index(ctx, o.index, searchx.Document(doc)); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("search: index order %q: %w", id, err)
	}
	return nil
}

// Search 按关键词检索订单，返回原始文档切片。
//
// 返回值保留原始 Document：命中结果的字段组合由调用方决定要不要规范化，
// 本层做映射只会把 ES 的真实返回形态藏起来。
func (o *Order) Search(ctx context.Context, query string, limit int) ([]map[string]any, error) {
	ctx, span := o.tr.Start(ctx, spanPrefix+"SearchOrder")
	defer span.End()
	span.SetAttributes(
		attribute.String("search.index", o.index),
		attribute.String("search.query", query),
		attribute.Int("search.limit", limit),
	)

	res, err := o.cli.Search(ctx, o.index, query)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("search: search order %q: %w", query, err)
	}
	span.SetAttributes(attribute.Int("search.hits", res.Total))

	out := make([]map[string]any, 0, len(res.Hits))
	for i, hit := range res.Hits {
		if limit > 0 && i >= limit {
			break
		}
		out = append(out, map[string]any(hit))
	}
	return out, nil
}
