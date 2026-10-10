// service 层单测：本域两条最关键的执行期内核在这里钉死。
//
//  1. 锁缺失的**降级**：Decrease 缺锁仍正常返回，但必须打含 "lock unavailable"
//     的 Warn（对应「未装配时显式声明未走锁路径」）；注入真锁时不出现该 Warn。
//  2. 并发不超卖：available=10 起 20 个 goroutine 各扣 1，成功数必须为 10、
//     终值 0 且不为负。
//
// ⚠️ 如实说明第 2 条的**强度上限**：sqlite 内存库会把写连接串行化（本文件用
// SetMaxOpenConns(1) 保证同一内存库），故这层的通过**部分**来自串行化，不是
// 条件 UPDATE 是唯一护栏的证明。它证明的是「并发下终值正确、无负数」；
// 「条件 UPDATE 是唯一护栏」由 repository 层的单线程条件语义承担。
//
// 本层用「假 kvx.KVStore / 假 lockx.Locker + sqlite 内存库」驱动，不起进程、
// 不伪造运行期证据（真起服 + 真 redis 归运行期校验）。
package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gospacex/kvx"
	"github.com/gospacex/lockx"
	"github.com/gospacex/logx"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/stock/model"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/stock/repository"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
)

// ---------- fakes ----------

// memKVStore 是 kvx.KVStore 的内存替身：只覆写服务真正用到的 Incr / Set，
// 其余方法由内嵌接口兜底（不会被调用）。带锁以支持并发用例。
type memKVStore struct {
	kvx.KVStore
	mu       sync.Mutex
	incrKeys []string
	setKeys  []string
	setTTL   map[string]time.Duration
	incrErr  error
	setErr   error
}

func newMemKV() *memKVStore { return &memKVStore{setTTL: make(map[string]time.Duration)} }

func (s *memKVStore) Incr(_ context.Context, key string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.incrErr != nil {
		return 0, s.incrErr
	}
	s.incrKeys = append(s.incrKeys, key)
	// 本假件对每个键第一次计到 1、其后递增（够用即可，不模拟真实计数）。
	return int64(len(s.incrKeys)), nil
}

func (s *memKVStore) Set(_ context.Context, key, _ string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setErr != nil {
		return s.setErr
	}
	s.setKeys = append(s.setKeys, key)
	s.setTTL[key] = ttl
	return nil
}

func (s *memKVStore) snapshotKeys() (incr, set []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.incrKeys...), append([]string(nil), s.setKeys...)
}

// fakeLock 是 lockx.Lock 的替身。
type fakeLock struct {
	lockx.Lock
	unlockErr error
}

func (l *fakeLock) Unlock(context.Context) error { return l.unlockErr }

// fakeLocker 是 lockx.Locker 的替身：记录是否真的走过锁路径与锁 key。
type fakeLocker struct {
	lockx.Locker
	mu       sync.Mutex
	calls    int
	key      string
	lockErr  error
	lastLock *fakeLock
}

func (l *fakeLocker) Lock(_ context.Context, key string, _ ...lockx.LockOption) (lockx.Lock, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	l.key = key
	if l.lockErr != nil {
		return nil, l.lockErr
	}
	l.lastLock = &fakeLock{}
	return l.lastLock, nil
}

func (l *fakeLocker) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// recLogger 只覆写 Warn，带锁以支持并发用例。
type recLogger struct {
	logx.Logger
	mu    sync.Mutex
	warns []string
}

func (l *recLogger) Warn(msg string, _ ...logx.Field) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg)
}

func (l *recLogger) hasWarn(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, m := range l.warns {
		if contains(m, sub) {
			return true
		}
	}
	return false
}

// codeOf 取业务码（非业务错误一律 Internal，与 codes.From 一致）。
func codeOf(err error) codes.Code {
	c, _ := codes.From(err)
	return c
}

func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// countingRepo 包一层真仓储：既走真库（产出 repository 层 span），又记录调用次数。
type countingRepo struct {
	inner     stockRepo
	findCalls int
	tryCalls  int
}

func (r *countingRepo) FindByProductID(ctx context.Context, id uint64) (*model.Stock, error) {
	r.findCalls++
	return r.inner.FindByProductID(ctx, id)
}

func (r *countingRepo) TryDecrease(ctx context.Context, id uint64, n int32) (bool, error) {
	r.tryCalls++
	return r.inner.TryDecrease(ctx, id, n)
}

// ---------- helpers ----------

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
	// sqlite `:memory:` 每条连接一个独立库、且写串行化：钉到 1 条连接，既让所有
	// 语句看同一个内存库，又避免并发写 `database is locked`。
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

func spanNames(sr *tracetest.SpanRecorder) []string {
	names := make([]string, 0, len(sr.Ended()))
	for _, sp := range sr.Ended() {
		names = append(names, sp.Name())
	}
	return names
}

// ---------- GetStock 读语义 ----------

func TestGetStockReturnsRow(t *testing.T) {
	db := newTestDB(t)
	seedStock(t, db, model.Stock{ProductID: 7, Total: 100, Available: 42, Reserved: 8, Version: 3})
	svc := New(repository.New(db, false), nil, nil, &recLogger{}, false)

	v, err := svc.GetStock(context.Background(), 7)
	if err != nil {
		t.Fatalf("GetStock: %v", err)
	}
	if v.Stock.ProductID != 7 || v.Stock.Total != 100 || v.Stock.Available != 42 ||
		v.Stock.Reserved != 8 || v.Stock.Version != 3 {
		t.Fatalf("view = %+v", v.Stock)
	}
}

// 无库存记录 → 零值视图（不报错）：读接口不该因为「该商品还没建库存行」而失败，
// available=0 在业务上就是扣不动。与 points.GetPoints「未开户=0」同构。
func TestGetStockMissReturnsZeroView(t *testing.T) {
	svc := New(repository.New(newTestDB(t), false), nil, nil, &recLogger{}, false)

	v, err := svc.GetStock(context.Background(), 123)
	if err != nil {
		t.Fatalf("无库存记录不应报错：%v", err)
	}
	if v == nil {
		t.Fatal("无库存记录应返回零值视图而非 nil")
	}
	if v.Stock.ProductID != 123 || v.Stock.Available != 0 || v.Stock.Total != 0 {
		t.Fatalf("零值视图应保留 product_id、其余为 0：%+v", v.Stock)
	}
}

func TestGetStockZeroProductIDIsInvalidArgument(t *testing.T) {
	svc := New(repository.New(newTestDB(t), false), nil, nil, &recLogger{}, false)
	_, err := svc.GetStock(context.Background(), 0)
	if c, _ := codes.From(err); c != codes.InvalidArgument {
		t.Fatalf("product_id=0 应回 InvalidArgument，实际 %v (err=%v)", c, err)
	}
}

func TestGetStockRepoErrorIsInternal(t *testing.T) {
	db := newTestDB(t)
	if err := db.Migrator().DropTable(&model.Stock{}); err != nil {
		t.Fatalf("drop stocks: %v", err)
	}
	svc := New(repository.New(db, false), nil, nil, &recLogger{}, false)
	_, err := svc.GetStock(context.Background(), 1)
	if c, _ := codes.From(err); c != codes.Internal {
		t.Fatalf("库错误应回 Internal，实际 %v (err=%v)", c, err)
	}
}

// span 父子：service.stock.GetStock 是 repository.stock.FindByProductID 的父。
func TestGetStockSpanParentChild(t *testing.T) {
	sr, restore := withRecorder(t)
	defer restore()

	db := newTestDB(t)
	seedStock(t, db, model.Stock{ProductID: 200, Total: 5, Available: 5})
	svc := New(repository.New(db, true), nil, nil, &recLogger{}, true)

	if _, err := svc.GetStock(context.Background(), 200); err != nil {
		t.Fatalf("GetStock: %v", err)
	}

	seen := make(map[string]sdktrace.ReadOnlySpan)
	names := spanNames(sr)
	for _, sp := range sr.Ended() {
		seen[sp.Name()] = sp
	}
	parent, ok := seen["service.stock.GetStock"]
	if !ok {
		t.Fatalf("缺少 span service.stock.GetStock，实际=%v", names)
	}
	child, ok := seen["repository.stock.FindByProductID"]
	if !ok {
		t.Fatalf("缺少 span repository.stock.FindByProductID，实际=%v", names)
	}
	if got, want := child.Parent().SpanID(), parent.SpanContext().SpanID(); got != want {
		t.Fatalf("repository span 的父应为 service span：child.Parent=%s parent.SpanID=%s（spans=%v）",
			got, want, names)
	}
	if got, want := child.SpanContext().TraceID(), parent.SpanContext().TraceID(); got != want {
		t.Fatalf("两层 span 应同 trace：%s != %s", got, want)
	}
}

// ---------- Decrease 写语义 ----------

func TestDecreaseSucceedsAndReturnsRemaining(t *testing.T) {
	db := newTestDB(t)
	seedStock(t, db, model.Stock{ProductID: 1, Total: 5, Available: 5})
	svc := New(repository.New(db, false), nil, nil, &recLogger{}, false)

	remaining, err := svc.Decrease(context.Background(), 1, 2)
	if err != nil {
		t.Fatalf("Decrease: %v", err)
	}
	if remaining != 3 {
		t.Fatalf("remaining = %d, want 3", remaining)
	}
	var got model.Stock
	if err := db.First(&got, "product_id = ?", 1).Error; err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Available != 3 || got.Version != 1 {
		t.Fatalf("扣减后 available=%d version=%d，want 3/1", got.Available, got.Version)
	}
}

func TestDecreaseNotEnoughIsStockNotEnough(t *testing.T) {
	db := newTestDB(t)
	seedStock(t, db, model.Stock{ProductID: 1, Total: 1, Available: 1})
	kv := newMemKV()
	svc := New(repository.New(db, false), kv, nil, &recLogger{}, false)

	_, err := svc.Decrease(context.Background(), 1, 2)
	if c, _ := codes.From(err); c != codes.StockNotEnough {
		t.Fatalf("库存不足应回 StockNotEnough，实际 %v (err=%v)", c, err)
	}
	// 未命中不得触发旁路计数（扣减没发生，不该记一笔）。
	if incr, _ := kv.snapshotKeys(); len(incr) != 0 {
		t.Fatalf("扣减未命中不应旁路计数，实际 %v", incr)
	}
}

func TestDecreaseInvalidArgs(t *testing.T) {
	svc := New(repository.New(newTestDB(t), false), nil, nil, &recLogger{}, false)
	if _, err := svc.Decrease(context.Background(), 0, 1); codeOf(err) != codes.InvalidArgument {
		t.Fatalf("product_id=0 应 InvalidArgument，实际 %v", err)
	}
	for _, n := range []int32{0, -3} {
		if _, err := svc.Decrease(context.Background(), 1, n); codeOf(err) != codes.InvalidArgument {
			t.Fatalf("n=%d 应 InvalidArgument，实际 %v", n, err)
		}
	}
}

func TestDecreaseRepoErrorIsInternal(t *testing.T) {
	db := newTestDB(t)
	if err := db.Migrator().DropTable(&model.Stock{}); err != nil {
		t.Fatalf("drop stocks: %v", err)
	}
	svc := New(repository.New(db, false), nil, nil, &recLogger{}, false)
	if _, err := svc.Decrease(context.Background(), 1, 1); codeOf(err) != codes.Internal {
		t.Fatalf("库错误应回 Internal，实际 %v", err)
	}
}

// ---------- Step 2：锁缺失的降级 ----------

// 缺锁：Decrease 必须正常返回（缺锁不使调用失败）——这是「MUST NOT 因缺锁而
// 启动/调用失败」的执行期内核；同时必须打含 "lock unavailable" 的 Warn。
func TestDecreaseWithoutLockStillSucceedsAndWarns(t *testing.T) {
	db := newTestDB(t)
	seedStock(t, db, model.Stock{ProductID: 1, Total: 5, Available: 5})
	lg := &recLogger{}
	svc := New(repository.New(db, false), nil, nil /* lock 缺省 */, lg, false)

	remaining, err := svc.Decrease(context.Background(), 1, 2)
	if err != nil {
		t.Fatalf("缺锁不得使 Decrease 失败：%v", err)
	}
	if remaining != 3 {
		t.Fatalf("缺锁下扣减结果应照常：remaining=%d want 3", remaining)
	}
	if !lg.hasWarn("lock unavailable") {
		t.Fatalf("缺锁必须留含 \"lock unavailable\" 的 Warn（不得静默当作已加锁），实际 %v", lg.warns)
	}
}

// 对照组：注入真锁（内存假 Locker）时**不出现**该 Warn，且确实走了锁路径。
func TestDecreaseWithLockDoesNotWarnLockUnavailable(t *testing.T) {
	db := newTestDB(t)
	seedStock(t, db, model.Stock{ProductID: 1, Total: 5, Available: 5})
	lg := &recLogger{}
	lk := &fakeLocker{}
	svc := New(repository.New(db, false), nil, lk, lg, false)

	if _, err := svc.Decrease(context.Background(), 1, 2); err != nil {
		t.Fatalf("Decrease: %v", err)
	}
	if lg.hasWarn("lock unavailable") {
		t.Fatalf("注入了锁不应出现缺锁 Warn，实际 %v", lg.warns)
	}
	if lk.callCount() != 1 || lk.key != "stock:product:1" {
		t.Fatalf("应走过一次锁路径且 key=stock:product:1，实际 calls=%d key=%q", lk.callCount(), lk.key)
	}
}

// 取锁失败：回 Internal（不许静默降级续跑 —— 那等于假装加了锁）。
func TestDecreaseLockAcquireErrorIsInternal(t *testing.T) {
	db := newTestDB(t)
	seedStock(t, db, model.Stock{ProductID: 1, Total: 5, Available: 5})
	lg := &recLogger{}
	lk := &fakeLocker{lockErr: errors.New("lock backend down")}
	svc := New(repository.New(db, false), nil, lk, lg, false)

	if _, err := svc.Decrease(context.Background(), 1, 1); codeOf(err) != codes.Internal {
		t.Fatalf("取锁失败应回 Internal，实际 %v", err)
	}
	var got model.Stock
	if err := db.First(&got, "product_id = ?", 1).Error; err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Available != 5 {
		t.Fatalf("取锁失败不得扣减，available=%d want 5", got.Available)
	}
}

// 解锁失败：只告警，不影响扣减结果。
func TestDecreaseUnlockFailureOnlyWarns(t *testing.T) {
	db := newTestDB(t)
	seedStock(t, db, model.Stock{ProductID: 1, Total: 5, Available: 5})
	lg := &recLogger{}
	// 这个 Locker 返回的锁在 Unlock 时必报错。
	svc := New(repository.New(db, false), nil, unlockErrLocker{}, lg, false)

	if _, err := svc.Decrease(context.Background(), 1, 1); err != nil {
		t.Fatalf("解锁失败不应影响扣减：%v", err)
	}
	if !lg.hasWarn("unlock failed") {
		t.Fatalf("解锁失败必须留 Warn，实际 %v", lg.warns)
	}
}

// unlockErrLocker 总是返回一个 Unlock 会失败的锁。
type unlockErrLocker struct{ lockx.Locker }

func (unlockErrLocker) Lock(context.Context, string, ...lockx.LockOption) (lockx.Lock, error) {
	return &fakeLock{unlockErr: errors.New("unlock boom")}, nil
}

// ---------- 旁路计数口径 ----------

// 扣减成功 → kv.Incr 记一次；n==1 时对当日键挂 TTL（避免键无限堆积）。
func TestDecreaseRecordsBypassCounter(t *testing.T) {
	db := newTestDB(t)
	seedStock(t, db, model.Stock{ProductID: 1, Total: 5, Available: 5})
	kv := newMemKV()
	svc := New(repository.New(db, false), kv, nil, &recLogger{}, false)

	if _, err := svc.Decrease(context.Background(), 1, 1); err != nil {
		t.Fatalf("Decrease: %v", err)
	}
	incr, set := kv.snapshotKeys()
	wantKey := "mesh:stock:dec:1:" + time.Now().Format("20060102")
	if len(incr) != 1 || incr[0] != wantKey {
		t.Fatalf("旁路计数键 = %v, want [%s]", incr, wantKey)
	}
	if len(set) != 1 || set[0] != wantKey {
		t.Fatalf("首次计数应对当日键挂 TTL，set=%v", set)
	}
	if ttl := kv.setTTL[wantKey]; ttl != 48*time.Hour {
		t.Fatalf("当日键 TTL = %v, want 48h", ttl)
	}
}

// KV 为 nil：只 Warn，不影响扣减结果。
func TestDecreaseNilKVOnlyWarns(t *testing.T) {
	db := newTestDB(t)
	seedStock(t, db, model.Stock{ProductID: 1, Total: 5, Available: 5})
	lg := &recLogger{}
	svc := New(repository.New(db, false), nil, nil, lg, false)

	remaining, err := svc.Decrease(context.Background(), 1, 1)
	if err != nil || remaining != 4 {
		t.Fatalf("KV 缺失不应影响扣减：remaining=%d err=%v", remaining, err)
	}
	if !lg.hasWarn("kv unavailable") {
		t.Fatalf("KV 缺失必须留 Warn，实际 %v", lg.warns)
	}
}

// KV Incr 报错：只 Warn，不影响扣减结果（KV 只是旁路计数，真相在库层条件 UPDATE）。
func TestDecreaseKVIncrErrorOnlyWarns(t *testing.T) {
	db := newTestDB(t)
	seedStock(t, db, model.Stock{ProductID: 1, Total: 5, Available: 5})
	kv := newMemKV()
	kv.incrErr = errors.New("kv down")
	lg := &recLogger{}
	svc := New(repository.New(db, false), kv, nil, lg, false)

	remaining, err := svc.Decrease(context.Background(), 1, 1)
	if err != nil || remaining != 4 {
		t.Fatalf("旁路计数失败不应影响扣减：remaining=%d err=%v", remaining, err)
	}
	if !lg.hasWarn("decrease counter incr failed") {
		t.Fatalf("旁路计数失败必须留 Warn，实际 %v", lg.warns)
	}
}

// KV Set 报错：只 Warn。
func TestDecreaseKVSetErrorOnlyWarns(t *testing.T) {
	db := newTestDB(t)
	seedStock(t, db, model.Stock{ProductID: 1, Total: 5, Available: 5})
	kv := newMemKV()
	kv.setErr = errors.New("kv down")
	lg := &recLogger{}
	svc := New(repository.New(db, false), kv, nil, lg, false)

	if _, err := svc.Decrease(context.Background(), 1, 1); err != nil {
		t.Fatalf("TTL 设置失败不应影响扣减：%v", err)
	}
	if !lg.hasWarn("set decrease counter ttl failed") {
		t.Fatalf("TTL 设置失败必须留 Warn，实际 %v", lg.warns)
	}
}

// 扣减已提交、但随后的回读失败：不得把成功的扣减报成失败（否则调用方会重试、
// 反而多扣），只 Warn 并回 remaining=0。
func TestDecreaseObserveRemainingFailureOnlyWarns(t *testing.T) {
	lg := &recLogger{}
	svc := New(&scriptedRepo{findErr: errors.New("read back boom")}, nil, nil, lg, false)

	remaining, err := svc.Decrease(context.Background(), 1, 1)
	if err != nil {
		t.Fatalf("回读失败不应把已提交的扣减报成失败：%v", err)
	}
	if remaining != 0 {
		t.Fatalf("回读失败时 remaining 应回 0，实际 %d", remaining)
	}
	if !lg.hasWarn("observe remaining failed") {
		t.Fatalf("回读失败必须留 Warn，实际 %v", lg.warns)
	}
}

// scriptedRepo 用可控回执驱动 service：TryDecrease 命中，但 FindByProductID 必错。
type scriptedRepo struct{ findErr error }

func (r *scriptedRepo) FindByProductID(context.Context, uint64) (*model.Stock, error) {
	return nil, r.findErr
}

func (r *scriptedRepo) TryDecrease(context.Context, uint64, int32) (bool, error) { return true, nil }

// ---------- Step 3 第 2 层：并发计数（端到端，强度受限，见文件头） ----------

func TestDecreaseConcurrentNoOversell(t *testing.T) {
	db := newTestDB(t)
	seedStock(t, db, model.Stock{ProductID: 1, Total: 10, Available: 10})
	svc := New(repository.New(db, false), nil, nil, &recLogger{}, false)

	const goroutines = 20
	var wg sync.WaitGroup
	var success, notEnough, other int32
	var mu sync.Mutex
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.Decrease(context.Background(), 1, 1)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				success++
			default:
				if c, _ := codes.From(err); c == codes.StockNotEnough {
					notEnough++
				} else {
					other++
					t.Errorf("并发扣减出现非预期错误：%v", err)
				}
			}
		}()
	}
	wg.Wait()

	if success != 10 {
		t.Fatalf("成功数 = %d, want 10（超卖或少卖）", success)
	}
	if notEnough != 10 {
		t.Fatalf("未命中数 = %d, want 10", notEnough)
	}
	if other != 0 {
		t.Fatalf("非预期错误数 = %d, want 0", other)
	}
	var got model.Stock
	if err := db.First(&got, "product_id = ?", 1).Error; err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Available != 0 {
		t.Fatalf("终值 available = %d, want 0", got.Available)
	}
	if got.Available < 0 {
		t.Fatalf("available 不得为负，实际 %d", got.Available)
	}
}

// ---------- 埋点开关不改变行为 ----------

func TestGetStockTraceFlagDoesNotChangeBehavior(t *testing.T) {
	for _, traceOn := range []bool{true, false} {
		db := newTestDB(t)
		seedStock(t, db, model.Stock{ProductID: 1, Total: 5, Available: 4})
		svc := New(repository.New(db, traceOn), nil, nil, &recLogger{}, traceOn)
		v, err := svc.GetStock(context.Background(), 1)
		if err != nil || v.Stock.Available != 4 {
			t.Fatalf("traceOn=%v 行为异常：v=%+v err=%v", traceOn, v, err)
		}
	}
}
