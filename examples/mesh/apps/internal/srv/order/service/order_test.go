package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gospacex/lockx"
	"github.com/gospacex/logx"
	"github.com/gospacex/mqx"
	"gorm.io/gorm"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/order/model"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/events"
)

// ---------- fakes ----------

// fakeRepo 是 service 依赖的窄接口（orderRepo）的手写假件。
type fakeRepo struct {
	createErr   error
	findErr     error
	itemsErr    error
	updErr      error
	findResult  *model.Order
	itemsResult []model.OrderItem

	created    *model.Order
	createdIts []model.OrderItem
	createCall int
	findCalls  int
	updFrom    string
	updTo      string
	updID      uint64
	updCalls   int
}

func (f *fakeRepo) Create(_ context.Context, o *model.Order, items []model.OrderItem) error {
	f.createCall++
	f.created, f.createdIts = o, items
	if f.createErr == nil && o.ID == 0 {
		o.ID = 777
	}
	return f.createErr
}

func (f *fakeRepo) FindByID(context.Context, uint64) (*model.Order, error) {
	f.findCalls++
	if f.findErr != nil {
		return nil, f.findErr
	}
	return f.findResult, nil
}

func (f *fakeRepo) ItemsByOrderID(context.Context, uint64) ([]model.OrderItem, error) {
	if f.itemsErr != nil {
		return nil, f.itemsErr
	}
	return f.itemsResult, nil
}

func (f *fakeRepo) UpdateStatus(_ context.Context, id uint64, from, to string) error {
	f.updCalls++
	f.updID, f.updFrom, f.updTo = id, from, to
	return f.updErr
}

// fakeCache 记录调用顺序，用于断言「先写库后删缓存」。
type fakeCache struct {
	val      string
	getErr   error
	setErr   error
	delErr   error
	sets     []string
	deletes  []string
	getCalls int
	setCalls int
	getKeys  []string
}

func (c *fakeCache) Get(_ context.Context, key string) (string, error) {
	c.getCalls++
	c.getKeys = append(c.getKeys, key)
	return c.val, c.getErr
}
func (c *fakeCache) Set(_ context.Context, key, value string, _ time.Duration) error {
	c.setCalls++
	c.sets = append(c.sets, key)
	return c.setErr
}
func (c *fakeCache) Delete(_ context.Context, key string) error {
	c.deletes = append(c.deletes, key)
	return c.delErr
}
func (c *fakeCache) Exists(context.Context, string) (bool, error) { return false, nil }
func (c *fakeCache) Keys(context.Context, string) ([]string, error) {
	return nil, nil
}
func (c *fakeCache) Close() error                      { return nil }
func (c *fakeCache) HealthCheck(context.Context) error { return nil }

// fakeLock 实现 lockx.Locker；内嵌接口让未覆写方法自动满足签名。
type fakeLock struct {
	lockx.Locker
	lockErr   error
	unlockErr error
	locked    []string
	unlocked  int
}

func (l *fakeLock) Lock(_ context.Context, key string, _ ...lockx.LockOption) (lockx.Lock, error) {
	l.locked = append(l.locked, key)
	if l.lockErr != nil {
		return nil, l.lockErr
	}
	return &fakeHandle{unlock: func(context.Context) error { l.unlocked++; return l.unlockErr }}, nil
}

type fakeHandle struct{ unlock func(context.Context) error }

func (h *fakeHandle) Unlock(ctx context.Context) error { return h.unlock(ctx) }
func (h *fakeHandle) Renew(context.Context) error      { return nil }
func (h *fakeHandle) Key() string                      { return "k" }
func (h *fakeHandle) Expired() bool                    { return false }

// fakeProducer 实现 mqx.Producer，记录发出的 topic 与 payload。
type fakeProducer struct {
	mqx.Producer
	sendErr error
	topics  []string
	values  [][]byte
}

func (p *fakeProducer) Send(_ context.Context, msg *mqx.Message) error {
	p.topics = append(p.topics, msg.Topic)
	p.values = append(p.values, msg.Value)
	return p.sendErr
}

// fakeLogger 只覆写 Warn，用于断言「降级必留痕」。
type fakeLogger struct {
	logx.Logger
	warns []string
}

func (l *fakeLogger) Warn(msg string, _ ...logx.Field) { l.warns = append(l.warns, msg) }

func warned(l *fakeLogger, substr string) bool {
	for _, m := range l.warns {
		if strings.Contains(m, substr) {
			return true
		}
	}
	return false
}

// ---------- CreateOrder ----------

func TestCreateOrderRejectsEmptyItems(t *testing.T) {
	svc := New(&fakeRepo{}, nil, nil, nil, &fakeLogger{}, false)
	_, err := svc.CreateOrder(context.Background(), CreateOrderInput{UserID: 1})
	if c, _ := codes.From(err); c != codes.InvalidArgument {
		t.Fatalf("空明细应回 InvalidArgument，实际 %v (err=%v)", c, err)
	}
}

func TestCreateOrderRejectsNonPositiveQuantity(t *testing.T) {
	svc := New(&fakeRepo{}, nil, nil, nil, &fakeLogger{}, false)
	_, err := svc.CreateOrder(context.Background(), CreateOrderInput{
		UserID: 1,
		Items:  []CreateOrderItem{{ProductID: 1, Quantity: 0}},
	})
	if c, _ := codes.From(err); c != codes.InvalidArgument {
		t.Fatalf("quantity<=0 应回 InvalidArgument，实际 %v (err=%v)", c, err)
	}
}

// 多明细：总额 = Σ quantity×unit_price，明细原样下传。
func TestCreateOrderHappyPathMultiItems(t *testing.T) {
	repo := &fakeRepo{}
	prod := &fakeProducer{}
	svc := New(repo, nil, nil, prod, &fakeLogger{}, false)

	id, err := svc.CreateOrder(context.Background(), CreateOrderInput{
		UserID: 42,
		Items: []CreateOrderItem{
			{ProductID: 1, ProductName: "p1", Quantity: 2, UnitPrice: 100},
			{ProductID: 2, ProductName: "p2", Quantity: 3, UnitPrice: 50},
		},
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if id != 777 {
		t.Fatalf("id = %d, want 777", id)
	}
	if repo.created.TotalAmount != 2*100+3*50 {
		t.Fatalf("total = %d, want 350", repo.created.TotalAmount)
	}
	if len(repo.createdIts) != 2 {
		t.Fatalf("items = %d, want 2", len(repo.createdIts))
	}
	if repo.created.Status != model.StatusCreated {
		t.Fatalf("status = %q, want created", repo.created.Status)
	}
	if repo.created.OrderNo == "" {
		t.Fatal("order_no 应已生成")
	}
	// 事件已投递到约定的 topic
	if len(prod.topics) != 1 || prod.topics[0] != events.TopicOrderCompleted {
		t.Fatalf("topics = %v, want [%s]", prod.topics, events.TopicOrderCompleted)
	}
	var ev events.OrderCompleted
	if err := json.Unmarshal(prod.values[0], &ev); err != nil {
		t.Fatalf("event payload 非法: %v", err)
	}
	if ev.OrderID != 777 || ev.UserID != 42 || ev.TotalAmount != 350 {
		t.Fatalf("event = %+v", ev)
	}
}

// 锁缺失：fail-open 且必须留 Warn。
func TestCreateOrderFailOpenWhenLockMissing(t *testing.T) {
	lg := &fakeLogger{}
	repo := &fakeRepo{}
	svc := New(repo, nil, nil, nil, lg, false)
	if _, err := svc.CreateOrder(context.Background(), CreateOrderInput{
		UserID: 7, Items: []CreateOrderItem{{ProductID: 1, Quantity: 1, UnitPrice: 1}},
	}); err != nil {
		t.Fatalf("锁缺失不应失败: %v", err)
	}
	if repo.createCall != 1 {
		t.Fatalf("fail-open 仍应落库，createCall=%d", repo.createCall)
	}
	if !warned(lg, "lock unavailable") {
		t.Fatalf("锁缺失必须留 Warn，实际 %v", lg.warns)
	}
}

// 锁可用：按 user 维度加锁并释放。
func TestCreateOrderUsesLock(t *testing.T) {
	lk := &fakeLock{}
	svc := New(&fakeRepo{}, nil, lk, nil, &fakeLogger{}, false)
	if _, err := svc.CreateOrder(context.Background(), CreateOrderInput{
		UserID: 99, Items: []CreateOrderItem{{ProductID: 1, Quantity: 1, UnitPrice: 1}},
	}); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if len(lk.locked) != 1 || lk.locked[0] != "order:user:99" {
		t.Fatalf("locked = %v, want [order:user:99]", lk.locked)
	}
	if lk.unlocked != 1 {
		t.Fatalf("unlock 次数 = %d, want 1", lk.unlocked)
	}
}

// 解锁失败：只告警，不影响已成功的下单（锁有 TTL 兜底）。
func TestCreateOrderWarnsWhenUnlockFails(t *testing.T) {
	lg := &fakeLogger{}
	lk := &fakeLock{unlockErr: errors.New("lock backend down")}
	svc := New(&fakeRepo{}, nil, lk, nil, lg, false)
	if _, err := svc.CreateOrder(context.Background(), CreateOrderInput{
		UserID: 5, Items: []CreateOrderItem{{ProductID: 1, Quantity: 1, UnitPrice: 1}},
	}); err != nil {
		t.Fatalf("解锁失败不应让下单失败: %v", err)
	}
	if !warned(lg, "unlock failed") {
		t.Fatalf("解锁失败必须留 Warn，实际 %v", lg.warns)
	}
}

func TestCreateOrderPropagatesLockError(t *testing.T) {
	lk := &fakeLock{lockErr: errors.New("lock backend down")}
	repo := &fakeRepo{}
	svc := New(repo, nil, lk, nil, &fakeLogger{}, false)
	_, err := svc.CreateOrder(context.Background(), CreateOrderInput{
		UserID: 1, Items: []CreateOrderItem{{ProductID: 1, Quantity: 1, UnitPrice: 1}},
	})
	if c, _ := codes.From(err); c != codes.Internal {
		t.Fatalf("取锁失败应回 Internal，实际 %v (err=%v)", c, err)
	}
	if repo.createCall != 0 {
		t.Fatal("取锁失败不得落库")
	}
}

func TestCreateOrderPropagatesRepoError(t *testing.T) {
	repo := &fakeRepo{createErr: errors.New("db down")}
	svc := New(repo, nil, nil, &fakeProducer{}, &fakeLogger{}, false)
	_, err := svc.CreateOrder(context.Background(), CreateOrderInput{
		UserID: 1, Items: []CreateOrderItem{{ProductID: 1, Quantity: 1, UnitPrice: 1}},
	})
	if c, _ := codes.From(err); c != codes.OrderCreateFail {
		t.Fatalf("落库失败应回 OrderCreateFail，实际 %v (err=%v)", c, err)
	}
}

// producer 为 nil：只告警不报错，下单仍成功。
func TestCreateOrderPublishDegradesWhenProducerNil(t *testing.T) {
	lg := &fakeLogger{}
	svc := New(&fakeRepo{}, nil, nil, nil, lg, false)
	if _, err := svc.CreateOrder(context.Background(), CreateOrderInput{
		UserID: 1, Items: []CreateOrderItem{{ProductID: 1, Quantity: 1, UnitPrice: 1}},
	}); err != nil {
		t.Fatalf("producer 缺失不应失败: %v", err)
	}
	if !warned(lg, "producer unavailable") {
		t.Fatalf("producer 缺失必须留 Warn，实际 %v", lg.warns)
	}
}

// producer 发送失败：只告警，不把下单打成失败。
func TestCreateOrderPublishFailureOnlyWarns(t *testing.T) {
	lg := &fakeLogger{}
	svc := New(&fakeRepo{}, nil, nil, &fakeProducer{sendErr: errors.New("broker down")}, lg, false)
	if _, err := svc.CreateOrder(context.Background(), CreateOrderInput{
		UserID: 1, Items: []CreateOrderItem{{ProductID: 1, Quantity: 1, UnitPrice: 1}},
	}); err != nil {
		t.Fatalf("发送失败不应让下单失败: %v", err)
	}
	if !warned(lg, "publish event failed") {
		t.Fatalf("发送失败必须留 Warn，实际 %v", lg.warns)
	}
}

// ---------- GetOrder ----------

func TestGetOrderCacheHitSkipsRepoButLoadsItems(t *testing.T) {
	order := model.Order{ID: 5, UserID: 1, OrderNo: "M-5", Status: model.StatusCreated}
	raw, _ := json.Marshal(order)
	repo := &fakeRepo{
		findErr:     errors.New("must not be called"),
		itemsResult: []model.OrderItem{{OrderID: 5, ProductID: 1, Quantity: 1}},
	}
	ca := &fakeCache{val: string(raw)}
	svc := New(repo, ca, nil, nil, &fakeLogger{}, false)

	v, err := svc.GetOrder(context.Background(), 5)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if !v.Cached {
		t.Fatal("命中缓存应标 Cached=true")
	}
	if repo.findCalls != 0 {
		t.Fatalf("命中缓存不得回源主表，findCalls=%d", repo.findCalls)
	}
	if len(v.Items) != 1 {
		t.Fatalf("明细仍应回源，items=%d", len(v.Items))
	}
	if ca.getKeys[0] != "mesh:order:detail:5" {
		t.Fatalf("cache key = %q", ca.getKeys[0])
	}
}

func TestGetOrderCacheMissLoadsAndFills(t *testing.T) {
	repo := &fakeRepo{findResult: &model.Order{ID: 6, UserID: 1, OrderNo: "M-6"}}
	ca := &fakeCache{} // Get 返回 "" → miss
	svc := New(repo, ca, nil, nil, &fakeLogger{}, false)

	v, err := svc.GetOrder(context.Background(), 6)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if v.Cached {
		t.Fatal("未命中缓存不应标 Cached")
	}
	if repo.findCalls != 1 {
		t.Fatalf("未命中应回源，findCalls=%d", repo.findCalls)
	}
	if ca.setCalls != 1 || ca.sets[0] != "mesh:order:detail:6" {
		t.Fatalf("未命中应回填缓存，sets=%v", ca.sets)
	}
}

// 缓存里的字节解不出订单（ID 为 0）→ 视为 miss，回源。
func TestGetOrderCorruptCacheFallsBack(t *testing.T) {
	repo := &fakeRepo{findResult: &model.Order{ID: 7, OrderNo: "M-7"}}
	ca := &fakeCache{val: `{"order_no":"broken"}`} // 缺 ID
	svc := New(repo, ca, nil, nil, &fakeLogger{}, false)

	v, err := svc.GetOrder(context.Background(), 7)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if v.Cached {
		t.Fatal("损坏缓存不应算命中")
	}
	if repo.findCalls != 1 {
		t.Fatalf("损坏缓存应回源，findCalls=%d", repo.findCalls)
	}
}

// 查不到 → OrderNotFound（而非 Internal）。
func TestGetOrderNotFound(t *testing.T) {
	repo := &fakeRepo{findErr: gorm.ErrRecordNotFound}
	svc := New(repo, nil, nil, nil, &fakeLogger{}, false)
	_, err := svc.GetOrder(context.Background(), 404)
	if c, _ := codes.From(err); c != codes.OrderNotFound {
		t.Fatalf("ErrRecordNotFound 应回 OrderNotFound，实际 %v (err=%v)", c, err)
	}
}

// 库/网络类错误 → Internal（与「查不到」区分开）。
func TestGetOrderRepoErrorIsInternal(t *testing.T) {
	repo := &fakeRepo{findErr: errors.New("connection reset")}
	svc := New(repo, nil, nil, nil, &fakeLogger{}, false)
	_, err := svc.GetOrder(context.Background(), 1)
	if c, _ := codes.From(err); c != codes.Internal {
		t.Fatalf("库错误应回 Internal，实际 %v (err=%v)", c, err)
	}
}

// 明细读取失败 → Internal。
func TestGetOrderItemsErrorIsInternal(t *testing.T) {
	repo := &fakeRepo{
		findResult: &model.Order{ID: 8, OrderNo: "M-8"},
		itemsErr:   errors.New("items query failed"),
	}
	svc := New(repo, nil, nil, nil, &fakeLogger{}, false)
	_, err := svc.GetOrder(context.Background(), 8)
	if c, _ := codes.From(err); c != codes.Internal {
		t.Fatalf("明细读取失败应回 Internal，实际 %v (err=%v)", c, err)
	}
}

// 缓存 Set 失败：只告警，请求仍成功返回。
func TestGetOrderFillFailureOnlyWarns(t *testing.T) {
	lg := &fakeLogger{}
	repo := &fakeRepo{findResult: &model.Order{ID: 9, OrderNo: "M-9"}}
	ca := &fakeCache{setErr: errors.New("cache down")}
	svc := New(repo, ca, nil, nil, lg, false)
	if _, err := svc.GetOrder(context.Background(), 9); err != nil {
		t.Fatalf("回填失败不应影响请求: %v", err)
	}
	if !warned(lg, "cache fill failed") {
		t.Fatalf("回填失败必须留 Warn，实际 %v", lg.warns)
	}
}

// ---------- MarkPaid ----------

// 写后失效：顺序必须是「库成功 → 删缓存」。
func TestMarkPaidInvalidatesAfterDBWrite(t *testing.T) {
	repo := &fakeRepo{}
	ca := &fakeCache{}
	svc := New(repo, ca, nil, nil, &fakeLogger{}, false)

	if err := svc.MarkPaid(context.Background(), 11); err != nil {
		t.Fatalf("MarkPaid: %v", err)
	}
	if repo.updCalls != 1 || repo.updFrom != model.StatusCreated || repo.updTo != model.StatusPaid {
		t.Fatalf("状态流转参数错误: id=%d %s→%s", repo.updID, repo.updFrom, repo.updTo)
	}
	if len(ca.deletes) != 1 || ca.deletes[0] != "mesh:order:detail:11" {
		t.Fatalf("应删除详情缓存，deletes=%v", ca.deletes)
	}
}

// 写库失败：不得删缓存（先删缓存会让并发回填旧值）。
func TestMarkPaidDoesNotInvalidateWhenDBFails(t *testing.T) {
	repo := &fakeRepo{updErr: errors.New("status changed by another request")}
	ca := &fakeCache{}
	svc := New(repo, ca, nil, nil, &fakeLogger{}, false)

	err := svc.MarkPaid(context.Background(), 12)
	if c, _ := codes.From(err); c != codes.OrderCreateFail {
		t.Fatalf("写库失败应回 OrderCreateFail，实际 %v (err=%v)", c, err)
	}
	if len(ca.deletes) != 0 {
		t.Fatalf("写库失败不得删缓存，deletes=%v", ca.deletes)
	}
}

// 删缓存失败：只告警，返回 nil（TTL 兜底）。
func TestMarkPaidDeleteFailureOnlyWarns(t *testing.T) {
	lg := &fakeLogger{}
	ca := &fakeCache{delErr: errors.New("cache down")}
	svc := New(&fakeRepo{}, ca, nil, nil, lg, false)
	if err := svc.MarkPaid(context.Background(), 13); err != nil {
		t.Fatalf("删缓存失败不应报错: %v", err)
	}
	if !warned(lg, "cache invalidate failed") {
		t.Fatalf("删缓存失败必须留 Warn，实际 %v", lg.warns)
	}
}

// cache 为 nil：MarkPaid 仍成功（无缓存可失效）。
func TestMarkPaidNilCacheOK(t *testing.T) {
	svc := New(&fakeRepo{}, nil, nil, nil, &fakeLogger{}, false)
	if err := svc.MarkPaid(context.Background(), 14); err != nil {
		t.Fatalf("无缓存时 MarkPaid 应成功: %v", err)
	}
}

// GetOrder 无缓存：直接回源，且不 panic。
func TestGetOrderNilCache(t *testing.T) {
	repo := &fakeRepo{findResult: &model.Order{ID: 15, OrderNo: "M-15"}}
	svc := New(repo, nil, nil, nil, &fakeLogger{}, false)
	v, err := svc.GetOrder(context.Background(), 15)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if v.Cached {
		t.Fatal("无缓存时不应标 Cached")
	}
}

// ---------- 埋点开关不改变行为 ----------

// traceOn=true / false 的业务返回值必须一致（埋点只影响 span，不影响结果）。
func TestCreateOrderTraceFlagDoesNotChangeBehavior(t *testing.T) {
	for _, traceOn := range []bool{true, false} {
		repo := &fakeRepo{}
		svc := New(repo, nil, nil, nil, &fakeLogger{}, traceOn)
		id, err := svc.CreateOrder(context.Background(), CreateOrderInput{
			UserID: 1, Items: []CreateOrderItem{{ProductID: 1, Quantity: 1, UnitPrice: 1}},
		})
		if err != nil || id != 777 {
			t.Fatalf("traceOn=%v 行为异常：id=%d err=%v", traceOn, id, err)
		}
		if repo.created == nil || repo.created.TotalAmount != 1 {
			t.Fatalf("traceOn=%v 业务未走通：%+v", traceOn, repo.created)
		}
	}
}

// 错误路径同样不受开关影响：miss → OrderNotFound 在两种取值下一致。
func TestGetOrderTraceFlagDoesNotChangeBehavior(t *testing.T) {
	for _, traceOn := range []bool{true, false} {
		svc := New(&fakeRepo{findErr: gorm.ErrRecordNotFound}, nil, nil, nil, &fakeLogger{}, traceOn)
		_, err := svc.GetOrder(context.Background(), 404)
		if c, _ := codes.From(err); c != codes.OrderNotFound {
			t.Fatalf("traceOn=%v 应回 OrderNotFound，实际 %v (err=%v)", traceOn, c, err)
		}
	}
}
