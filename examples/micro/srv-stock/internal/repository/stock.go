// Package repository srv-stock 持久层（私有库 stock_db，账号 mall_rw）。
//
// 在分层链上的位置：service → **repository** → model。它的上游只有 service，
// 下游只有 model 与 dbx 句柄。
//
// 句柄来源：db 是 launchx App.DB() 交出的 *gorm.DB（dbx 组件，自带 OTel
// 回调）。本包不 gorm.Open、不 import 任何驱动 —— 装配期由
// pkg/handlecheck.VerifyDBX 断言这件事，跑错句柄会在启动时直接失败。
//
// 纪律（与 mall 其他三服一致）：
//   - 连表查询一律 Find（多行投影到 slice），禁用 Scan（会静默丢列）
//   - 1:N 子表 JOIN 必须 GROUP BY 收敛，计数一律 COUNT(DISTINCT ...)，
//     求和一律 COALESCE(SUM(...), 0)（无流水时 SUM 返回 NULL，直接扫进
//     int32 会是 0 之外的未定义行为，且 NULL 参与后续比较恒为假）
//   - 错误一律 %w 包装；库存域额外用**哨兵错误**区分"没记录"与"数量不够"，
//     两者在 service 层翻译成不同错误码（4001 / 4002），混成一个会让
//     调用方把"商品没建库存"误判成"卖完了"
//   - 扣减一律条件更新（WHERE available >= ?）+ RowsAffected 判定，
//     不读出来再算：读-改-写在并发下必然超卖，锁只是第二道闸
//   - 每个方法开一个 repository.stock.<op> span，与 service / handler 的
//     span 串成一条链
package repository

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/model"
	"github.com/gospacex/launchx/examples/pkg/outbox"
	"github.com/gospacex/otelx/layer"
)

// 库存域哨兵错误（service 层据此翻译成 4xxx 业务码）
var (
	// ErrNotFound 库存记录不存在（该商品还没建库存）
	ErrNotFound = errors.New("repository: stock record not found")
	// ErrInsufficient 可用量不足（有记录但不够扣）
	ErrInsufficient = errors.New("repository: stock insufficient")
	// ErrReservationNF 预占流水不存在（重复释放 / 确认了不存在的预占）
	ErrReservationNF = errors.New("repository: reservation not found")
	// ErrOverRelease 释放量超过预占量（幂等释放的越界保护）
	ErrOverRelease = errors.New("repository: release quantity exceeds reservation")
)

// ReserveInput 预占事务输入
type ReserveInput struct {
	OrderNo   string
	ProductID uint64
	Quantity  int32
	UserID    uint64
	// OutboxFill 在事务内、预占流水落库后回调，产出事件体与 traceparent。
	// nil 时 outbox 行 payload/traceparent 为空（Runner 直接放弃该行，
	// 不占重试配额）。
	OutboxFill func(reservationID uint64) (payload, traceparent string)
}

// ReserveOutput 预占事务结果
type ReserveOutput struct {
	ReservationID uint64
	Available     int32
	// Idempotent 为 true 表示这次调用命中了既有预占流水（同一单重放），
	// 没有发生任何扣减。调用方据此在 span 上留痕 —— 否则「重放成功」与
	// 「首次预占成功」在链路上长得一模一样，对账时无从区分。
	Idempotent bool
}

// ReleaseInput 释放事务输入
type ReleaseInput struct {
	OrderNo   string
	ProductID uint64
	Quantity  int32
}

// ConfirmInput 确认事务输入
type ConfirmInput struct {
	OrderNo   string
	ProductID uint64
	Quantity  int32
}

// AdjustInput 调整事务输入（入库 Δ>0 / 盘点 Δ<0）
type AdjustInput struct {
	ProductID  uint64
	Delta      int32
	Reason     string
	OperatorID uint64
}

// StockRepository stock_db 仓储
type StockRepository struct {
	db      *gorm.DB
	traceOn bool
}

// NewStockRepository 构造。
//
// db 必须是 launchx App.DB() 交出的 dbx 句柄：只有它带 OTel 回调，
// 本层开的 span 下面才挂得住每条 SQL 的 dbx span。
func NewStockRepository(db *gorm.DB, traceOn bool) *StockRepository {
	return &StockRepository{db: db, traceOn: traceOn}
}

// DB 暴露句柄（pkg/handlecheck 断言、装配期健康检查用）
func (r *StockRepository) DB() *gorm.DB { return r.db }

// begin 开 repository 层 span，返回「收 error 再收尾」的闭包。
//
// traceOn=false 时返回空闭包，调用方一律 `defer func() { end(err) }()`，
// 不必在每个方法里写 if。为什么闭包要收 error：span 句柄刻意不交给调用方
// （本层因此零 otel 依赖），代价是调用方也标不了状态 —— 收 error 由闭包
// 代标，失败才在 Jaeger 上可见（否则「SQL 报错」与「SQL 成功」同色）。
//
// quiet 规则：ErrNotFound 是正常 miss，不是故障 —— 与单体示例 recordError
// 的同一条语义，命中即豁免错误标记，避免正常未命污染错误率。
func (r *StockRepository) begin(ctx context.Context, op string) (context.Context, layer.End) {
	return layer.Begin(ctx, "repository", "stock", op, r.traceOn, quietNotFound)
}

// quietNotFound 「记录不存在」不是故障。两种形态都认：本层已归一化出的
// ErrNotFound，以及 dbx/gorm 直接抛出的 ErrRecordNotFound（未归一化路径）。
func quietNotFound(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, gorm.ErrRecordNotFound)
}

// ---------- 联表查询 ----------

// GetStock 2 表 LEFT JOIN + 聚合：按商品 ID 取库存行，并聚合出预占流水侧
// 的"未释放预占量 / 未释放笔数 / 历史笔数"。
//
// 这里 JOIN stock_reservations 而不像早期版本那样只查主表：主表的 reserved
// 是冗余计数，只有流水表是权威账本。只查主表时，"事务写了一半导致两者
// 不一致"这类账实不符没有任何现象可观测，等到超卖才被发现。
//
// stock_reservations 是 1:N，JOIN 会把库存行放大 N 倍，故用聚合 +
// GROUP BY 收敛回一行；结果仍走 Find 到 slice（保持"JOIN 一律 Find、
// 禁 Scan"）。
func (r *StockRepository) GetStock(ctx context.Context, productID uint64) (out *model.StockDetail, err error) {
	ctx, end := r.begin(ctx, "GetStock")
	defer func() { end(err) }()
	var rows []model.StockDetail
	err = r.db.WithContext(ctx).
		Table("stocks").
		Select("stocks.product_id AS product_id, stocks.total, stocks.available, stocks.reserved, "+
			"stocks.version, stocks.updated_at, "+
			"COALESCE(SUM(CASE WHEN stock_reservations.status = ? THEN stock_reservations.quantity ELSE 0 END), 0) AS open_reserved_qty, "+
			"COUNT(DISTINCT CASE WHEN stock_reservations.status = ? THEN stock_reservations.id END) AS open_reserve_count, "+
			"COUNT(DISTINCT stock_reservations.id) AS reserve_count",
			model.ReserveReserved, model.ReserveReserved).
		Joins("LEFT JOIN stock_reservations ON stock_reservations.product_id = stocks.product_id").
		Where("stocks.product_id = ?", productID).
		Group("stocks.product_id, stocks.total, stocks.available, stocks.reserved, stocks.version, stocks.updated_at").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("get stock %d: %w", productID, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("get stock %d: %w", productID, ErrNotFound)
	}
	out = &rows[0]
	return out, nil
}

// ListStocks 2 表 LEFT JOIN + 聚合分页（与 GetStock 共用投影）
//
// 列表也带预占聚合：运营看板要在列表页直接看出"哪些商品卡着未释放的预占"，
// 逐个商品点进详情去凑这个数会退化成 N+1 次查询。
func (r *StockRepository) ListStocks(ctx context.Context, q model.ListStocksQuery) (out []model.StockDetail, err error) {
	ctx, end := r.begin(ctx, "ListStocks")
	defer func() { end(err) }()
	limit, offset := q.Limit, q.Offset
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200 // 上限写死：库存表是写热点，全表扫描代价高
	}
	if offset < 0 {
		offset = 0
	}
	var rows []model.StockDetail
	err = r.db.WithContext(ctx).
		Table("stocks").
		Select("stocks.product_id AS product_id, stocks.total, stocks.available, stocks.reserved, "+
			"stocks.version, stocks.updated_at, "+
			"COALESCE(SUM(CASE WHEN stock_reservations.status = ? THEN stock_reservations.quantity ELSE 0 END), 0) AS open_reserved_qty, "+
			"COUNT(DISTINCT CASE WHEN stock_reservations.status = ? THEN stock_reservations.id END) AS open_reserve_count, "+
			"COUNT(DISTINCT stock_reservations.id) AS reserve_count",
			model.ReserveReserved, model.ReserveReserved).
		Joins("LEFT JOIN stock_reservations ON stock_reservations.product_id = stocks.product_id").
		Group("stocks.product_id, stocks.total, stocks.available, stocks.reserved, stocks.version, stocks.updated_at").
		Order("stocks.product_id DESC").
		Limit(limit).Offset(offset).
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("list stocks: %w", err)
	}
	return rows, nil
}

// ---------- 多表事务 ----------

// ReserveTx 3 表事务 + outbox：幂等查重 → stocks 条件扣减 → 预占流水 →
// 变动日志 → outbox。
//
// 幂等（2026-10-10 修）：同 (order_no, product_id) 已有流水时**直接返回既有
// 流水**，不再扣减、不再插新行。缺了这一步，「重放一次 POST /order」会多冻结
// 一份 available，而释放路径（ReleaseTx 的 `Order("id DESC").Take`）只认最新
// 一条 —— 旧流水永久停在 status=1，冻结量再也放不出来。
//
// 查重必须在**事务内、扣减之前**：放在事务外就退化成 check-then-act，两个并发
// 重放都能查到"没有"，然后各扣一次。表上的 uk_sr_order_product
// （见 examples/micro/scripts/schema-micro.sql）是最后一道闸，它让并发下的
// 重复插入直接失败，而不是静默变成两笔预占。
//
// 条件更新是防超卖的第二道闸（第一道是 lockx）：只判 RowsAffected==0 时
// 再回查一次区分"没记录"与"不够扣"，两次判定都只在事务内发生，
// 中间不会被别的预占插进来。
func (r *StockRepository) ReserveTx(ctx context.Context, in ReserveInput) (out *ReserveOutput, err error) {
	ctx, end := r.begin(ctx, "ReserveTx")
	defer func() { end(err) }()
	out = &ReserveOutput{}
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// ① 幂等查重：命中即原样返回（ReservationID 取既有流水，available 取当前值）
		var exist model.Reservation
		e := tx.Where("order_no = ? AND product_id = ?", in.OrderNo, in.ProductID).Take(&exist).Error
		switch {
		case e == nil:
			var s model.Stock
			if err := tx.Where("product_id = ?", in.ProductID).Take(&s).Error; err != nil {
				return fmt.Errorf("reload stock %d: %w", in.ProductID, err)
			}
			out.ReservationID = exist.ID
			out.Available = s.Available
			out.Idempotent = true
			return nil
		case !errors.Is(e, gorm.ErrRecordNotFound):
			return fmt.Errorf("probe reservation %s/%d: %w", in.OrderNo, in.ProductID, e)
		}
		// ② 条件扣减
		res := tx.Model(&model.Stock{}).
			Where("product_id = ? AND available >= ?", in.ProductID, in.Quantity).
			Updates(map[string]any{
				"available": gorm.Expr("available - ?", in.Quantity),
				"reserved":  gorm.Expr("reserved + ?", in.Quantity),
				"version":   gorm.Expr("version + 1"),
			})
		if res.Error != nil {
			return fmt.Errorf("reserve stock %d: %w", in.ProductID, res.Error)
		}
		if res.RowsAffected == 0 {
			return r.classifyMiss(tx, in.ProductID, ErrInsufficient)
		}
		rv := model.Reservation{
			OrderNo:   in.OrderNo,
			ProductID: in.ProductID,
			Quantity:  in.Quantity,
			Status:    model.ReserveReserved,
		}
		if err := tx.Create(&rv).Error; err != nil {
			return fmt.Errorf("insert reservation: %w", err)
		}
		if err := tx.Create(&model.StockLog{
			ProductID:  in.ProductID,
			Delta:      -in.Quantity,
			Reason:     fmt.Sprintf("reserve order %s", in.OrderNo),
			OperatorID: in.UserID,
		}).Error; err != nil {
			return fmt.Errorf("insert stock log: %w", err)
		}
		ev := outbox.Event{
			Topic:     "stock.reserved.v1",
			EventType: "stock.reserved",
			Status:    outbox.StatusPending,
		}
		if in.OutboxFill != nil {
			ev.Payload, ev.Traceparent = in.OutboxFill(rv.ID)
		}
		if err := tx.Create(&ev).Error; err != nil {
			return fmt.Errorf("insert outbox event: %w", err)
		}
		out.ReservationID = rv.ID
		var s model.Stock
		if err := tx.Where("product_id = ?", in.ProductID).Take(&s).Error; err != nil {
			return fmt.Errorf("reload stock %d: %w", in.ProductID, err)
		}
		out.Available = s.Available
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reserve tx: %w", err)
	}
	return out, nil
}

// ReleaseTx 3 表事务：回滚 available/reserved + 预占流水置为已释放 + 变动日志。
//
// 幂等：流水不存在（重复释放）返回 ErrReservationNF，由 service 翻译成
// CodeStockReservationNF —— 这是**成功**语义（已释放过了），不是错误。
func (r *StockRepository) ReleaseTx(ctx context.Context, in ReleaseInput) (out int32, err error) {
	ctx, end := r.begin(ctx, "ReleaseTx")
	defer func() { end(err) }()
	var available int32
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rv model.Reservation
		err := tx.Where("order_no = ? AND product_id = ? AND status = ?", in.OrderNo, in.ProductID, model.ReserveReserved).
			Order("id DESC").Take(&rv).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("release %s/%d: %w", in.OrderNo, in.ProductID, ErrReservationNF)
			}
			return fmt.Errorf("load reservation %s: %w", in.OrderNo, err)
		}
		if in.Quantity > rv.Quantity {
			return fmt.Errorf("release %s: %w (reserved %d, release %d)", in.OrderNo, ErrOverRelease, rv.Quantity, in.Quantity)
		}
		res := tx.Model(&model.Stock{}).
			Where("product_id = ? AND reserved >= ?", in.ProductID, in.Quantity).
			Updates(map[string]any{
				"available": gorm.Expr("available + ?", in.Quantity),
				"reserved":  gorm.Expr("reserved - ?", in.Quantity),
				"version":   gorm.Expr("version + 1"),
			})
		if res.Error != nil {
			return fmt.Errorf("release stock %d: %w", in.ProductID, res.Error)
		}
		if res.RowsAffected == 0 {
			return r.classifyMiss(tx, in.ProductID, ErrInsufficient)
		}
		if err := tx.Model(&model.Reservation{}).Where("id = ?", rv.ID).
			Updates(map[string]any{"status": model.ReserveReleased}).Error; err != nil {
			return fmt.Errorf("mark reservation released %d: %w", rv.ID, err)
		}
		if err := tx.Create(&model.StockLog{
			ProductID: in.ProductID,
			Delta:     in.Quantity,
			Reason:    fmt.Sprintf("release order %s", in.OrderNo),
		}).Error; err != nil {
			return fmt.Errorf("insert stock log: %w", err)
		}
		var s model.Stock
		if err := tx.Where("product_id = ?", in.ProductID).Take(&s).Error; err != nil {
			return fmt.Errorf("reload stock %d: %w", in.ProductID, err)
		}
		available = s.Available
		return nil
	})
	if err != nil {
		return 0, err
	}
	return available, nil
}

// ConfirmTx 3 表事务：真正扣减 total/reserved + 流水置为已确认 + 变动日志。
//
// 与预占的分工：预占只动 available（冻结可卖量），确认才动 total（出库）。
// 不分开的话，"下单取消"与"已发货"在库存账上无法区分。
func (r *StockRepository) ConfirmTx(ctx context.Context, in ConfirmInput) (total, available int32, err error) {
	ctx, end := r.begin(ctx, "ConfirmTx")
	defer func() { end(err) }()
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rv model.Reservation
		e := tx.Where("order_no = ? AND product_id = ? AND status = ?", in.OrderNo, in.ProductID, model.ReserveReserved).
			Order("id DESC").Take(&rv).Error
		if e != nil {
			if errors.Is(e, gorm.ErrRecordNotFound) {
				return fmt.Errorf("confirm %s/%d: %w", in.OrderNo, in.ProductID, ErrReservationNF)
			}
			return fmt.Errorf("load reservation %s: %w", in.OrderNo, e)
		}
		if in.Quantity > rv.Quantity {
			return fmt.Errorf("confirm %s: %w (reserved %d, confirm %d)", in.OrderNo, ErrOverRelease, rv.Quantity, in.Quantity)
		}
		res := tx.Model(&model.Stock{}).
			Where("product_id = ? AND reserved >= ? AND total >= ?", in.ProductID, in.Quantity, in.Quantity).
			Updates(map[string]any{
				"total":    gorm.Expr("total - ?", in.Quantity),
				"reserved": gorm.Expr("reserved - ?", in.Quantity),
				"version":  gorm.Expr("version + 1"),
			})
		if res.Error != nil {
			return fmt.Errorf("confirm stock %d: %w", in.ProductID, res.Error)
		}
		if res.RowsAffected == 0 {
			return r.classifyMiss(tx, in.ProductID, ErrInsufficient)
		}
		if err := tx.Model(&model.Reservation{}).Where("id = ?", rv.ID).
			Updates(map[string]any{"status": model.ReserveConfirmed}).Error; err != nil {
			return fmt.Errorf("mark reservation confirmed %d: %w", rv.ID, err)
		}
		if err := tx.Create(&model.StockLog{
			ProductID: in.ProductID,
			Delta:     -in.Quantity,
			Reason:    fmt.Sprintf("confirm order %s", in.OrderNo),
		}).Error; err != nil {
			return fmt.Errorf("insert stock log: %w", err)
		}
		var s model.Stock
		if err := tx.Where("product_id = ?", in.ProductID).Take(&s).Error; err != nil {
			return fmt.Errorf("reload stock %d: %w", in.ProductID, err)
		}
		total, available = s.Total, s.Available
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return total, available, nil
}

// AdjustTx 2 表事务：调整 total/available + 变动日志。
//
// 盘点（Δ<0）同样走条件更新：available + delta < 0 时 RowsAffected==0，
// 由 classifyMiss 区分"没记录"与"扣成负数"。盘点扣到负数意味着账实不符，
// 必须拒绝而不是留一条负库存。
func (r *StockRepository) AdjustTx(ctx context.Context, in AdjustInput) (total, available int32, err error) {
	ctx, end := r.begin(ctx, "AdjustTx")
	defer func() { end(err) }()
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		where := "product_id = ?"
		args := []any{in.ProductID}
		if in.Delta < 0 {
			where += " AND available >= ?"
			args = append(args, -in.Delta)
		}
		res := tx.Model(&model.Stock{}).Where(where, args...).
			Updates(map[string]any{
				"total":     gorm.Expr("total + ?", in.Delta),
				"available": gorm.Expr("available + ?", in.Delta),
				"version":   gorm.Expr("version + 1"),
			})
		if res.Error != nil {
			return fmt.Errorf("adjust stock %d: %w", in.ProductID, res.Error)
		}
		if res.RowsAffected == 0 {
			return r.classifyMiss(tx, in.ProductID, ErrInsufficient)
		}
		reason := in.Reason
		if reason == "" {
			reason = model.ReasonAdjust
		}
		if err := tx.Create(&model.StockLog{
			ProductID:  in.ProductID,
			Delta:      in.Delta,
			Reason:     reason,
			OperatorID: in.OperatorID,
		}).Error; err != nil {
			return fmt.Errorf("insert stock log %d: %w", in.ProductID, err)
		}
		var s model.Stock
		if err := tx.Where("product_id = ?", in.ProductID).Take(&s).Error; err != nil {
			return fmt.Errorf("reload stock %d: %w", in.ProductID, err)
		}
		total, available = s.Total, s.Available
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return total, available, nil
}

// classifyMiss 区分"库存记录不存在"与"数量不够"。
//
// 只在条件更新未命中时调用：先按商品查一次计数，0 行即没记录（ErrNotFound），
// 否则是有记录但条件不满足（deficit）。两个哨兵错误在 service 层映射到
// 4001 / 4002，混成一个会让调用方把"没建库存"误判成"卖完了"。
func (r *StockRepository) classifyMiss(tx *gorm.DB, productID uint64, deficit error) error {
	var cnt int64
	if err := tx.Model(&model.Stock{}).Where("product_id = ?", productID).Count(&cnt).Error; err != nil {
		return fmt.Errorf("probe stock %d: %w", productID, err)
	}
	if cnt == 0 {
		return fmt.Errorf("stock %d: %w", productID, ErrNotFound)
	}
	return fmt.Errorf("stock %d: %w", productID, deficit)
}

// ---------- 多表 JOIN ----------

// Report 3 表 LEFT JOIN 报表（stocks ⋈ stock_reservations ⋈ stock_logs）。
//
// 两个 1:N 子表 JOIN 会互相放大行数，故计数一律 COUNT(DISTINCT ...)。
func (r *StockRepository) Report(ctx context.Context, q model.StockReportQuery) (out []model.ReportRow, err error) {
	ctx, end := r.begin(ctx, "Report")
	defer func() { end(err) }()
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	var rows []model.ReportRow
	err = r.db.WithContext(ctx).
		Table("stocks").
		Select("stocks.product_id AS product_id, stocks.total, stocks.available, stocks.reserved, " +
			"COUNT(DISTINCT stock_reservations.id) AS reservation_count, " +
			"COUNT(DISTINCT stock_logs.id) AS log_count").
		Joins("LEFT JOIN stock_reservations ON stock_reservations.product_id = stocks.product_id").
		Joins("LEFT JOIN stock_logs ON stock_logs.product_id = stocks.product_id").
		Group("stocks.product_id, stocks.total, stocks.available, stocks.reserved").
		Order("stocks.product_id ASC").
		Limit(limit).
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("report stocks: %w", err)
	}
	return rows, nil
}
