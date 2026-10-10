// repository 层单测（sqlite cgo 驱动）：验证 FindByID 的读语义（命中 / 未命中 /
// 库报错三者分清）、UpdatePrice 的落库，以及 quiet 判定与埋点开关不改变行为。
// 需要 CGO_ENABLED=1（mattn/go-sqlite3）。
package repository

import (
	"context"
	"errors"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/product/model"
)

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.Product{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

func TestFindByIDReturnsRow(t *testing.T) {
	db := newTestDB(t)
	if err := db.Create(&model.Product{ID: 7, Title: "demo", Price: 1999, Status: 1, CategoryID: 3}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	repo := New(db, false)

	p, err := repo.FindByID(context.Background(), 7)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if p.ID != 7 || p.Title != "demo" || p.Price != 1999 || p.Status != 1 || p.CategoryID != 3 {
		t.Fatalf("product = %+v", p)
	}
}

// 未命中：gorm.ErrRecordNotFound 必须原样上抛，由 service 翻成 ProductNotFound。
func TestFindByIDPropagatesErrRecordNotFound(t *testing.T) {
	repo := New(newTestDB(t), false)
	_, err := repo.FindByID(context.Background(), 404)
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("want gorm.ErrRecordNotFound, got %v", err)
	}
}

func TestFindByIDPropagatesDBError(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)
	if err := db.Migrator().DropTable(&model.Product{}); err != nil {
		t.Fatalf("drop products: %v", err)
	}
	if _, err := repo.FindByID(context.Background(), 1); err == nil {
		t.Fatal("products 表缺失时 FindByID 应报错")
	}
}

func TestUpdatePricePersists(t *testing.T) {
	db := newTestDB(t)
	if err := db.Create(&model.Product{ID: 9, Title: "p", Price: 100}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	repo := New(db, false)

	if err := repo.UpdatePrice(context.Background(), 9, 250); err != nil {
		t.Fatalf("UpdatePrice: %v", err)
	}
	var got model.Product
	if err := db.First(&got, 9).Error; err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Price != 250 {
		t.Fatalf("price = %d, want 250", got.Price)
	}
}

func TestUpdatePricePropagatesDBError(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, false)
	if err := db.Migrator().DropTable(&model.Product{}); err != nil {
		t.Fatalf("drop products: %v", err)
	}
	if err := repo.UpdatePrice(context.Background(), 1, 1); err == nil {
		t.Fatal("products 表缺失时 UpdatePrice 应报错")
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

// traceOn=true：埋点开启不影响业务行为（覆盖命中 / miss / 写三条路径）。
func TestTraceOnDoesNotChangeBehavior(t *testing.T) {
	db := newTestDB(t)
	repo := New(db, true)
	ctx := context.Background()

	if err := db.Create(&model.Product{ID: 11, Title: "p", Price: 100}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	if p, err := repo.FindByID(ctx, 11); err != nil || p.Price != 100 {
		t.Fatalf("FindByID 行为异常：p=%+v err=%v", p, err)
	}
	if err := repo.UpdatePrice(ctx, 11, 120); err != nil {
		t.Fatalf("UpdatePrice: %v", err)
	}
	if _, err := repo.FindByID(ctx, 99999); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("miss 应回 ErrRecordNotFound，实际 %v", err)
	}
}
