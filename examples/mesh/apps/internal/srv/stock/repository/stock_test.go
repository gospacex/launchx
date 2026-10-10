// repository 层单测（sqlite cgo 驱动）：把「条件扣减不超卖」这条最关键的判据
// 钉死在**单线程**的可执行语义上 —— available 不足时 UPDATE 影响 0 行且行值不变。
//
// 为什么这条必须放在 repository 层、且单线程就能验：并发的通过可能来自库把写
// 串行化，证明不了「条件 UPDATE 是唯一护栏」；而条件 UPDATE 的语义（WHERE
// available >= ? 命中与否）单线程即可判定。若把 UPDATE 写成无条件的
// `available = available - ?`，本文件的 TestTryDecreaseConditionalBlocksOversell
// 必然变红。
//
// 需要 CGO_ENABLED=1（mattn/go-sqlite3）。
package repository

import (
	"context"
	"errors"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/stock/model"
)

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB: %v", err)
	}
	// sqlite `:memory:` 的每一条连接是**各自独立**的内存库，且写操作会串行化。
	// 把连接数钉到 1：既保证所有语句看到同一个内存库，又避免并发写触发
	// `database is locked`。
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&model.Stock{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

func seedStock(t *testing.T, db *gorm.DB, s model.Stock) {
	t.Helper()
	if err := db.Create(&s).Error; err != nil {
		t.Fatalf("seed stock: %v", err)
	}
}

// readBack 读回 (available, version)，断言扣减的结果。
func readBack(t *testing.T, db *gorm.DB, productID uint64) (int32, int64) {
	t.Helper()
	var got model.Stock
	if err := db.First(&got, "product_id = ?", productID).Error; err != nil {
		t.Fatalf("read back: %v", err)
	}
	return got.Available, got.Version
}

// ---------- 条件语义（第 3 步第 1 层，强判据） ----------

// available=10 扣 11 → 未命中且行值不变；扣 10 → 成功归 0、version+1；
// 再扣 1 → 仍未命中（不得为负）。这是「条件 UPDATE 是唯一护栏」的直接判据。
func TestTryDecreaseConditionalBlocksOversell(t *testing.T) {
	db := newTestDB(t)
	seedStock(t, db, model.Stock{ProductID: 1, Total: 10, Available: 10, Version: 0})
	repo := New(db, false)
	ctx := context.Background()

	// 1) available=10 扣 11：必须「未命中」，且行值一个字节都不动。
	ok, err := repo.TryDecrease(ctx, 1, 11)
	if err != nil {
		t.Fatalf("TryDecrease(11): %v", err)
	}
	if ok {
		t.Fatal("available=10 时扣 11 不得命中（无条件 UPDATE 会在这里漏判）")
	}
	if av, ver := readBack(t, db, 1); av != 10 || ver != 0 {
		t.Fatalf("未命中不得改行：available=%d version=%d，want 10/0", av, ver)
	}

	// 2) available=10 扣 10：命中，available 归 0、version +1。
	ok, err = repo.TryDecrease(ctx, 1, 10)
	if err != nil {
		t.Fatalf("TryDecrease(10): %v", err)
	}
	if !ok {
		t.Fatal("available=10 时扣 10 应当命中")
	}
	if av, ver := readBack(t, db, 1); av != 0 || ver != 1 {
		t.Fatalf("命中后 available=%d version=%d，want 0/1", av, ver)
	}

	// 3) available=0 再扣 1：仍未命中，available 不得变成负数。
	ok, err = repo.TryDecrease(ctx, 1, 1)
	if err != nil {
		t.Fatalf("TryDecrease(1): %v", err)
	}
	if ok {
		t.Fatal("available=0 再扣 1 不得命中")
	}
	if av, _ := readBack(t, db, 1); av != 0 {
		t.Fatalf("available 不得为负，实际 %d", av)
	}
}

// 商品不存在（无库存行）同样算「未命中」，而不是库报错：调用方据此回 StockNotEnough。
func TestTryDecreaseUnknownProductNotHit(t *testing.T) {
	repo := New(newTestDB(t), false)
	ok, err := repo.TryDecrease(context.Background(), 999, 1)
	if err != nil {
		t.Fatalf("无该商品不应报库错：%v", err)
	}
	if ok {
		t.Fatal("无库存行不应命中")
	}
}

// 库错误（表缺失）必须与「未命中」分开：前者上抛 err，后者 ok=false, err=nil。
func TestTryDecreasePropagatesDBError(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)
	if err := db.Migrator().DropTable(&model.Stock{}); err != nil {
		t.Fatalf("drop stocks: %v", err)
	}
	ok, err := repo.TryDecrease(context.Background(), 1, 1)
	if err == nil {
		t.Fatal("stocks 表缺失时 TryDecrease 应报错")
	}
	if ok {
		t.Fatal("库报错时不应报命中")
	}
}

// ---------- 读语义 ----------

func TestFindByProductIDReturnsRow(t *testing.T) {
	db := newTestDB(t)
	seedStock(t, db, model.Stock{ProductID: 7, Total: 100, Available: 42, Reserved: 8, Version: 3})
	repo := New(db, false)

	s, err := repo.FindByProductID(context.Background(), 7)
	if err != nil {
		t.Fatalf("FindByProductID: %v", err)
	}
	if s.ProductID != 7 || s.Total != 100 || s.Available != 42 || s.Reserved != 8 || s.Version != 3 {
		t.Fatalf("stock = %+v", s)
	}
}

// 未命中：gorm.ErrRecordNotFound 原样上抛，由 service 决定「视为 0」而非报错。
func TestFindByProductIDPropagatesErrRecordNotFound(t *testing.T) {
	repo := New(newTestDB(t), false)
	_, err := repo.FindByProductID(context.Background(), 404)
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("want gorm.ErrRecordNotFound, got %v", err)
	}
}

func TestFindByProductIDPropagatesDBError(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)
	if err := db.Migrator().DropTable(&model.Stock{}); err != nil {
		t.Fatalf("drop stocks: %v", err)
	}
	if _, err := repo.FindByProductID(context.Background(), 1); err == nil {
		t.Fatal("stocks 表缺失时 FindByProductID 应报错")
	}
}

// quietNotFound：ErrRecordNotFound 是正常 miss（豁免），其它错误不豁免。
func TestQuietNotFound(t *testing.T) {
	if !quietNotFound(gorm.ErrRecordNotFound) {
		t.Fatal("ErrRecordNotFound 应被豁免")
	}
	if quietNotFound(errors.New("connection reset")) {
		t.Fatal("库错误不应被豁免")
	}
}

// traceOn=true 时埋点开启不影响读写行为（覆盖读命中 / 读 miss / 条件扣减命中三径）。
func TestTraceOnDoesNotChangeBehavior(t *testing.T) {
	db := newTestDB(t)
	seedStock(t, db, model.Stock{ProductID: 11, Total: 5, Available: 5})
	repo := New(db, true)
	ctx := context.Background()

	s, err := repo.FindByProductID(ctx, 11)
	if err != nil || s.Available != 5 {
		t.Fatalf("FindByProductID 行为异常：s=%+v err=%v", s, err)
	}
	ok, err := repo.TryDecrease(ctx, 11, 2)
	if err != nil || !ok {
		t.Fatalf("TryDecrease 行为异常：ok=%v err=%v", ok, err)
	}
	if _, err := repo.FindByProductID(ctx, 99999); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("miss 应回 ErrRecordNotFound，实际 %v", err)
	}
}
