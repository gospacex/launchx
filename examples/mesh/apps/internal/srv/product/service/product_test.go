// service 层单测：缓存契约的可执行判据。
//
// 本 Task（srv-product 缓存侧）早于 apps/cmd 与 apps/conf，故这里**不起进程、
// 不伪造运行期证据** —— 运行期（真起服 + 真 redis + Jaeger）归 Task 15。
// 本层用「假 cachex.Cache（内存 map）+ sqlite 内存库」把缓存契约钉死在单测里：
//
//  1. 第二次读不回源（repo.FindByID 只被调一次，第二次 Cached=true）；
//  2. 回填真的发生（第一次返回后缓存里已有该键，且 TTL > 0）；
//  3. span 同 trace 且父子正确（service.product.GetProduct 是
//     repository.product.FindByID 的父）；
//  4. 写后失效（UpdatePrice 成功后该键已被删）。
//
// 为什么要一个真的 *repository.ProductRepository（而非假 repo）：第 3 条要
// 观测 repository 层 span，只有走真仓储才会在链上出现它。
package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gospacex/cachex"
	"github.com/gospacex/logx"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/product/model"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/product/repository"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
)

// ---------- fakes ----------

// memCache 是 cachex.Cache 的内存实现（该接口在 cachex 里只有 redis / memcached
// 两个真驱动，没有内存驱动，故单测自备）。它同时记录每条写入的 TTL，供
// 「回填 TTL > 0」这条断言读取。
type memCache struct {
	mu      sync.Mutex
	data    map[string]memEntry
	setErr  error
	delErr  error
	getErr  error
	setSeen []string
	delSeen []string
}

type memEntry struct {
	value string
	ttl   time.Duration
}

var _ cachex.Cache = (*memCache)(nil)

func newMemCache() *memCache { return &memCache{data: make(map[string]memEntry)} }

func (c *memCache) Get(_ context.Context, key string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.getErr != nil {
		return "", c.getErr
	}
	e, ok := c.data[key]
	if !ok {
		return "", nil // 未命中：cachex 约定返回 ("", nil)
	}
	return e.value, nil
}

func (c *memCache) Set(_ context.Context, key, value string, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setSeen = append(c.setSeen, key)
	if c.setErr != nil {
		return c.setErr
	}
	c.data[key] = memEntry{value: value, ttl: ttl}
	return nil
}

func (c *memCache) Delete(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.delSeen = append(c.delSeen, key)
	if c.delErr != nil {
		return c.delErr
	}
	delete(c.data, key)
	return nil
}

func (c *memCache) Exists(_ context.Context, key string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.data[key]
	return ok, nil
}

func (c *memCache) Keys(context.Context, string) ([]string, error) { return nil, nil }
func (c *memCache) Close() error                                   { return nil }
func (c *memCache) HealthCheck(context.Context) error              { return nil }

// peek 取出一条缓存记录（是否存在 + TTL），供断言读取。
func (c *memCache) peek(key string) (memEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.data[key]
	return e, ok
}

// countingRepo 包一层真仓储：既走真库（产出 repository 层 span），
// 又给 service 的回源次数打点（断言「第二次不回源」）。
type countingRepo struct {
	inner     productRepo
	findCalls int
	updCalls  int
}

func (r *countingRepo) FindByID(ctx context.Context, id uint64) (*model.Product, error) {
	r.findCalls++
	return r.inner.FindByID(ctx, id)
}

func (r *countingRepo) UpdatePrice(ctx context.Context, id uint64, price int64) error {
	r.updCalls++
	return r.inner.UpdatePrice(ctx, id, price)
}

// recLogger 只覆写 Warn：用于断言「降级必留痕」。
type recLogger struct {
	logx.Logger
	warns []string
}

func (l *recLogger) Warn(msg string, _ ...logx.Field) { l.warns = append(l.warns, msg) }

func warned(l *recLogger, substr string) bool {
	for _, m := range l.warns {
		if strings.Contains(m, substr) {
			return true
		}
	}
	return false
}

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

func seedProduct(t *testing.T, db *gorm.DB, p model.Product) {
	t.Helper()
	if err := db.Create(&p).Error; err != nil {
		t.Fatalf("seed product: %v", err)
	}
}

func withRecorder(t *testing.T) (*tracetest.SpanRecorder, func()) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	return sr, func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	}
}

const detailKey100 = "mesh:product:detail:100"

// ---------- 1 + 2：两次读 → 第二次命中缓存、且回填真的发生 ----------

func TestGetProductSecondReadHitsCache(t *testing.T) {
	db := newTestDB(t)
	seedProduct(t, db, model.Product{ID: 100, Title: "demo", Price: 1999, Status: 1, CategoryID: 7})
	ca := newMemCache()
	repo := &countingRepo{inner: repository.New(db, false)}
	svc := New(repo, ca, &recLogger{}, false)

	// 第一次：必然回源 + 回填。
	v1, err := svc.GetProduct(context.Background(), 100)
	if err != nil {
		t.Fatalf("first GetProduct: %v", err)
	}
	if v1.Cached {
		t.Fatal("第一次不应命中缓存")
	}
	if repo.findCalls != 1 {
		t.Fatalf("第一次应回源一次，findCalls=%d", repo.findCalls)
	}
	if v1.Product.Title != "demo" || v1.Product.Price != 1999 {
		t.Fatalf("回源结果有误：%+v", v1.Product)
	}

	// 断言 2：回填真的发生 —— 键已存在且 TTL > 0。
	e, ok := ca.peek(detailKey100)
	if !ok {
		t.Fatalf("第一次返回后缓存里应有 %s（回填未发生）", detailKey100)
	}
	if e.ttl <= 0 {
		t.Fatalf("回填 TTL 应 > 0，实际 %v", e.ttl)
	}

	// 第二次：命中缓存，不再回源。
	v2, err := svc.GetProduct(context.Background(), 100)
	if err != nil {
		t.Fatalf("second GetProduct: %v", err)
	}
	// 断言 1：第二次不回源。
	if !v2.Cached {
		t.Fatal("第二次应命中缓存（Cached=true）")
	}
	if repo.findCalls != 1 {
		t.Fatalf("第二次不得回源，findCalls=%d（应仍为 1）", repo.findCalls)
	}
	if v2.Product.Price != 1999 || v2.Product.Title != "demo" {
		t.Fatalf("缓存返回值有误：%+v", v2.Product)
	}
}

// ---------- 3：span 同 trace 且父子正确 ----------

func TestGetProductSpanSameTraceParentChild(t *testing.T) {
	sr, restore := withRecorder(t)
	defer restore()

	db := newTestDB(t)
	seedProduct(t, db, model.Product{ID: 200, Title: "p", Price: 1})
	// 真仓储 + traceOn=true：才会有 repository 层 span。
	svc := New(repository.New(db, true), newMemCache(), &recLogger{}, true)

	if _, err := svc.GetProduct(context.Background(), 200); err != nil {
		t.Fatalf("GetProduct: %v", err)
	}

	seen := make(map[string]sdktrace.ReadOnlySpan, len(sr.Ended()))
	names := make([]string, 0, len(sr.Ended()))
	for _, sp := range sr.Ended() {
		seen[sp.Name()] = sp
		names = append(names, sp.Name())
	}

	parent, ok := seen["service.product.GetProduct"]
	if !ok {
		t.Fatalf("缺少 span service.product.GetProduct，实际=%v", names)
	}
	child, ok := seen["repository.product.FindByID"]
	if !ok {
		t.Fatalf("缺少 span repository.product.FindByID，实际=%v", names)
	}
	if got, want := child.Parent().SpanID(), parent.SpanContext().SpanID(); got != want {
		t.Fatalf("repository span 的父应为 service span：child.Parent=%s，parent.SpanID=%s（spans=%v）",
			got, want, names)
	}
	if got, want := child.SpanContext().TraceID(), parent.SpanContext().TraceID(); got != want {
		t.Fatalf("两层 span 应同 trace：%s != %s", got, want)
	}
}

// ---------- 4：写后失效 ----------

func TestUpdatePriceInvalidatesCache(t *testing.T) {
	db := newTestDB(t)
	seedProduct(t, db, model.Product{ID: 300, Title: "p", Price: 100})
	ca := newMemCache()
	svc := New(repository.New(db, false), ca, &recLogger{}, false)

	// 前置：先读一次，让缓存里确实有该键（否则「删掉了」无从谈起）。
	if _, err := svc.GetProduct(context.Background(), 300); err != nil {
		t.Fatalf("warm up GetProduct: %v", err)
	}
	if _, ok := ca.peek("mesh:product:detail:300"); !ok {
		t.Fatal("前置失败：缓存应已被回填")
	}

	if err := svc.UpdatePrice(context.Background(), 300, 250); err != nil {
		t.Fatalf("UpdatePrice: %v", err)
	}

	if _, ok := ca.peek("mesh:product:detail:300"); ok {
		t.Fatal("写后应删除详情缓存（写后失效未发生）")
	}
	var got model.Product
	if err := db.First(&got, 300).Error; err != nil {
		t.Fatalf("read product: %v", err)
	}
	if got.Price != 250 {
		t.Fatalf("库内 price = %d, want 250", got.Price)
	}
}

// ---------- GetProduct 其余分支 ----------

func TestGetProductNotFound(t *testing.T) {
	svc := New(repository.New(newTestDB(t), false), newMemCache(), &recLogger{}, false)
	_, err := svc.GetProduct(context.Background(), 404)
	if c, _ := codes.From(err); c != codes.ProductNotFound {
		t.Fatalf("查不到应回 ProductNotFound，实际 %v (err=%v)", c, err)
	}
}

func TestGetProductRepoErrorIsInternal(t *testing.T) {
	db := newTestDB(t)
	if err := db.Migrator().DropTable(&model.Product{}); err != nil {
		t.Fatalf("drop products: %v", err)
	}
	svc := New(repository.New(db, false), newMemCache(), &recLogger{}, false)
	_, err := svc.GetProduct(context.Background(), 1)
	if c, _ := codes.From(err); c != codes.Internal {
		t.Fatalf("库错误应回 Internal，实际 %v (err=%v)", c, err)
	}
}

// 缓存里的字节解不出商品（ID 缺失）→ 视为 miss，回源而不是报错。
func TestGetProductCorruptCacheFallsBack(t *testing.T) {
	db := newTestDB(t)
	seedProduct(t, db, model.Product{ID: 400, Title: "p", Price: 5})
	ca := newMemCache()
	// 故意写入缺 ID 的 JSON。
	_ = ca.Set(context.Background(), "mesh:product:detail:400", `{"title":"broken"}`, time.Minute)
	repo := &countingRepo{inner: repository.New(db, false)}
	svc := New(repo, ca, &recLogger{}, false)

	v, err := svc.GetProduct(context.Background(), 400)
	if err != nil {
		t.Fatalf("GetProduct: %v", err)
	}
	if v.Cached {
		t.Fatal("损坏缓存不应算命中")
	}
	if repo.findCalls != 1 {
		t.Fatalf("损坏缓存应回源，findCalls=%d", repo.findCalls)
	}
}

// 缓存 Get 直接报错：只降级回源，不影响请求成败。
func TestGetProductCacheGetErrorFallsBack(t *testing.T) {
	db := newTestDB(t)
	seedProduct(t, db, model.Product{ID: 500, Title: "p", Price: 5})
	ca := newMemCache()
	ca.getErr = errors.New("cache down")
	repo := &countingRepo{inner: repository.New(db, false)}
	svc := New(repo, ca, &recLogger{}, false)

	v, err := svc.GetProduct(context.Background(), 500)
	if err != nil {
		t.Fatalf("缓存读失败不应影响请求: %v", err)
	}
	if v.Cached || repo.findCalls != 1 {
		t.Fatalf("缓存读失败应回源：Cached=%v findCalls=%d", v.Cached, repo.findCalls)
	}
}

// 回填失败：只告警，请求仍成功返回（缓存是加速器，不该影响成败）。
func TestGetProductFillFailureOnlyWarns(t *testing.T) {
	db := newTestDB(t)
	seedProduct(t, db, model.Product{ID: 600, Title: "p", Price: 5})
	ca := newMemCache()
	ca.setErr = errors.New("cache down")
	lg := &recLogger{}
	svc := New(repository.New(db, false), ca, lg, false)

	if _, err := svc.GetProduct(context.Background(), 600); err != nil {
		t.Fatalf("回填失败不应影响请求: %v", err)
	}
	if !warned(lg, "cache fill failed") {
		t.Fatalf("回填失败必须留 Warn，实际 %v", lg.warns)
	}
}

// cache 为 nil：直接回源，不 panic，不标 Cached。
func TestGetProductNilCache(t *testing.T) {
	db := newTestDB(t)
	seedProduct(t, db, model.Product{ID: 700, Title: "p", Price: 5})
	svc := New(repository.New(db, false), nil, &recLogger{}, false)

	v, err := svc.GetProduct(context.Background(), 700)
	if err != nil {
		t.Fatalf("GetProduct: %v", err)
	}
	if v.Cached {
		t.Fatal("无缓存时不应标 Cached")
	}
}

// ---------- UpdatePrice 其余分支 ----------

func TestUpdatePriceRepoErrorIsInternal(t *testing.T) {
	db := newTestDB(t)
	if err := db.Migrator().DropTable(&model.Product{}); err != nil {
		t.Fatalf("drop products: %v", err)
	}
	svc := New(repository.New(db, false), newMemCache(), &recLogger{}, false)
	err := svc.UpdatePrice(context.Background(), 1, 10)
	if c, _ := codes.From(err); c != codes.Internal {
		t.Fatalf("落库失败应回 Internal，实际 %v (err=%v)", c, err)
	}
}

// 写库失败不得删缓存：先删缓存会让并发请求回源后回填旧值。这里用「表缺失」
// 制造写库失败，断言 Delete 从未被调用。
func TestUpdatePriceDoesNotInvalidateWhenDBFails(t *testing.T) {
	db := newTestDB(t)
	if err := db.Migrator().DropTable(&model.Product{}); err != nil {
		t.Fatalf("drop products: %v", err)
	}
	ca := newMemCache()
	svc := New(repository.New(db, false), ca, &recLogger{}, false)
	if err := svc.UpdatePrice(context.Background(), 1, 10); err == nil {
		t.Fatal("表缺失时 UpdatePrice 应报错")
	}
	if len(ca.delSeen) != 0 {
		t.Fatalf("写库失败不得删缓存，delSeen=%v", ca.delSeen)
	}
}

// 删缓存失败：只告警，返回 nil（TTL 兜底）。
func TestUpdatePriceDeleteFailureOnlyWarns(t *testing.T) {
	db := newTestDB(t)
	seedProduct(t, db, model.Product{ID: 800, Title: "p", Price: 5})
	ca := newMemCache()
	ca.delErr = errors.New("cache down")
	lg := &recLogger{}
	svc := New(repository.New(db, false), ca, lg, false)

	if err := svc.UpdatePrice(context.Background(), 800, 9); err != nil {
		t.Fatalf("删缓存失败不应报错: %v", err)
	}
	if !warned(lg, "cache invalidate failed") {
		t.Fatalf("删缓存失败必须留 Warn，实际 %v", lg.warns)
	}
}

// cache 为 nil：UpdatePrice 仍成功（无缓存可失效）。
func TestUpdatePriceNilCacheOK(t *testing.T) {
	db := newTestDB(t)
	seedProduct(t, db, model.Product{ID: 900, Title: "p", Price: 5})
	svc := New(repository.New(db, false), nil, &recLogger{}, false)
	if err := svc.UpdatePrice(context.Background(), 900, 20); err != nil {
		t.Fatalf("无缓存时 UpdatePrice 应成功: %v", err)
	}
}

// ---------- 埋点开关不改变行为 ----------

func TestGetProductTraceFlagDoesNotChangeBehavior(t *testing.T) {
	for _, traceOn := range []bool{true, false} {
		db := newTestDB(t)
		seedProduct(t, db, model.Product{ID: 1000, Title: "p", Price: 5})
		svc := New(repository.New(db, traceOn), newMemCache(), &recLogger{}, traceOn)
		v, err := svc.GetProduct(context.Background(), 1000)
		if err != nil || v.Cached || v.Product.Price != 5 {
			t.Fatalf("traceOn=%v 行为异常：v=%+v err=%v", traceOn, v, err)
		}
	}
}
