// Package repository srv-stock 持久层（私有库 stock_db，账号 mall_rw）。
//
// 纪律（与 mall 其他三服一致）：
//   - 连表查询一律 Find（多行投影到 slice），禁用 Scan（会静默丢列）
//   - 错误一律 %w 包装；库存域额外用**哨兵错误**区分"没记录"与"数量不够"，
//     两者在 service 层翻译成不同错误码（4001 / 4002），混成一个会让
//     调用方把"商品没建库存"误判成"卖完了"
//   - 扣减一律条件更新（WHERE available >= ?）+ RowsAffected 判定，
//     不读出来再算：读-改-写在并发下必然超卖，锁只是第二道闸
package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/gospacex/launchx/examples/mall/pkg/outbox"
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

// 预占流水状态
const (
	ReserveReserved  int32 = 1 // 预占中
	ReserveConfirmed int32 = 2 // 已确认（下单成功，库存真正扣减）
	ReserveReleased  int32 = 3 // 已释放（下单失败/取消，库存回滚）
)

// Stock 库存主表行（stocks，product_id 唯一）
type Stock struct {
	ID        uint64    `gorm:"primaryKey;column:id"`
	ProductID uint64    `gorm:"column:product_id"`
	Total     int32     `gorm:"column:total"`
	Available int32     `gorm:"column:available"`
	Reserved  int32     `gorm:"column:reserved"`
	Version   int64     `gorm:"column:version"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// TableName 显式表名（避开 GORM 自动命名歧义）
func (Stock) TableName() string { return "stocks" }

// Reservation 预占流水（stock_reservations）
type Reservation struct {
	ID        uint64    `gorm:"primaryKey;column:id"`
	OrderNo   string    `gorm:"column:order_no"`
	ProductID uint64    `gorm:"column:product_id"`
	Quantity  int32     `gorm:"column:quantity"`
	Status    int32     `gorm:"column:status"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// TableName 显式表名
func (Reservation) TableName() string { return "stock_reservations" }

// StockLog 库存变动日志（stock_logs）
type StockLog struct {
	ID         uint64    `gorm:"primaryKey;column:id"`
	ProductID  uint64    `gorm:"column:product_id"`
	Delta      int32     `gorm:"column:delta"`
	Reason     string    `gorm:"column:reason"`
	OperatorID uint64    `gorm:"column:operator_id"`
	CreatedAt  time.Time `gorm:"column:created_at"`
}

// TableName 显式表名
func (StockLog) TableName() string { return "stock_logs" }

// ReportRow Report 3 表 JOIN 聚合行
type ReportRow struct {
	ProductID        int64 `gorm:"column:product_id"`
	Total            int32 `gorm:"column:total"`
	Available        int32 `gorm:"column:available"`
	Reserved         int32 `gorm:"column:reserved"`
	ReservationCount int32 `gorm:"column:reservation_count"`
	LogCount         int32 `gorm:"column:log_count"`
}

// ReserveInput 预占事务输入
type ReserveInput struct {
	OrderNo   string
	ProductID uint64
	Quantity  int32
	UserID    uint64
	// OutboxFill 在事务内、预占流水落库后回调，产出事件体与 traceparent。
	OutboxFill func(reservationID uint64) (payload, traceparent string)
}

// ReserveOutput 预占事务结果
type ReserveOutput struct {
	ReservationID uint64
	Available     int32
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
	db *gorm.DB
}

// NewStockRepository 构造
func NewStockRepository(db *gorm.DB) *StockRepository {
	return &StockRepository{db: db}
}

// DB 暴露句柄（组装期健康检查用）
func (r *StockRepository) DB() *gorm.DB { return r.db }

// ---------- 单表 CRUD ----------

// GetStock 单表查询：按商品 ID 取库存行
func (r *StockRepository) GetStock(ctx context.Context, productID uint64) (*Stock, error) {
	var s Stock
	err := r.db.WithContext(ctx).Where("product_id = ?", productID).Take(&s).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("get stock %d: %w", productID, ErrNotFound)
		}
		return nil, fmt.Errorf("get stock %d: %w", productID, err)
	}
	return &s, nil
}

// ListStocks 单表分页
func (r *StockRepository) ListStocks(ctx context.Context, limit, offset int) ([]Stock, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200 // 上限写死：库存表是写热点，全表扫描代价高
	}
	if offset < 0 {
		offset = 0
	}
	var rows []Stock
	if err := r.db.WithContext(ctx).Order("id DESC").Limit(limit).Offset(offset).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list stocks: %w", err)
	}
	return rows, nil
}

// ---------- 多表事务 ----------

// ReserveTx 3 表事务 + outbox：stocks 条件扣减 + 预占流水 + 变动日志 + outbox。
//
// 条件更新是防超卖的第二道闸（第一道是 lockx）：只判 RowsAffected==0 时
// 再回查一次区分"没记录"与"不够扣"，两次判定都只在事务内发生，
// 中间不会被别的预占插进来。
func (r *StockRepository) ReserveTx(ctx context.Context, in ReserveInput) (*ReserveOutput, error) {
	out := &ReserveOutput{}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&Stock{}).
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
		rv := Reservation{
			OrderNo:   in.OrderNo,
			ProductID: in.ProductID,
			Quantity:  in.Quantity,
			Status:    ReserveReserved,
		}
		if err := tx.Create(&rv).Error; err != nil {
			return fmt.Errorf("insert reservation: %w", err)
		}
		if err := tx.Create(&StockLog{
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
		var s Stock
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
func (r *StockRepository) ReleaseTx(ctx context.Context, in ReleaseInput) (int32, error) {
	var available int32
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rv Reservation
		err := tx.Where("order_no = ? AND product_id = ? AND status = ?", in.OrderNo, in.ProductID, ReserveReserved).
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
		res := tx.Model(&Stock{}).
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
		if err := tx.Model(&Reservation{}).Where("id = ?", rv.ID).
			Updates(map[string]any{"status": ReserveReleased}).Error; err != nil {
			return fmt.Errorf("mark reservation released %d: %w", rv.ID, err)
		}
		if err := tx.Create(&StockLog{
			ProductID: in.ProductID,
			Delta:     in.Quantity,
			Reason:    fmt.Sprintf("release order %s", in.OrderNo),
		}).Error; err != nil {
			return fmt.Errorf("insert stock log: %w", err)
		}
		var s Stock
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
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rv Reservation
		e := tx.Where("order_no = ? AND product_id = ? AND status = ?", in.OrderNo, in.ProductID, ReserveReserved).
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
		res := tx.Model(&Stock{}).
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
		if err := tx.Model(&Reservation{}).Where("id = ?", rv.ID).
			Updates(map[string]any{"status": ReserveConfirmed}).Error; err != nil {
			return fmt.Errorf("mark reservation confirmed %d: %w", rv.ID, err)
		}
		if err := tx.Create(&StockLog{
			ProductID: in.ProductID,
			Delta:     -in.Quantity,
			Reason:    fmt.Sprintf("confirm order %s", in.OrderNo),
		}).Error; err != nil {
			return fmt.Errorf("insert stock log: %w", err)
		}
		var s Stock
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
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		where := "product_id = ?"
		args := []any{in.ProductID}
		if in.Delta < 0 {
			where += " AND available >= ?"
			args = append(args, -in.Delta)
		}
		res := tx.Model(&Stock{}).Where(where, args...).
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
			reason = "adjust"
		}
		if err := tx.Create(&StockLog{
			ProductID:  in.ProductID,
			Delta:      in.Delta,
			Reason:     reason,
			OperatorID: in.OperatorID,
		}).Error; err != nil {
			return fmt.Errorf("insert stock log %d: %w", in.ProductID, err)
		}
		var s Stock
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
	if err := tx.Model(&Stock{}).Where("product_id = ?", productID).Count(&cnt).Error; err != nil {
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
func (r *StockRepository) Report(ctx context.Context, limit int) ([]ReportRow, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	var rows []ReportRow
	err := r.db.WithContext(ctx).
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
