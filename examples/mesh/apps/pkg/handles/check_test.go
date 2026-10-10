package handles

import (
	"strings"
	"testing"

	"github.com/gospacex/cachex"
	"github.com/gospacex/configx"
	"github.com/gospacex/kvx"
	"github.com/gospacex/lockx"
	"github.com/gospacex/mqx"
	"gorm.io/gorm"
)

// ---- 测试假件 ------------------------------------------------------------

// fakeConfig 实现 configx.Config：只覆写 verify 真正读的两条，其余靠内嵌接口满足签名。
type fakeConfig struct {
	configx.Config
	vals map[string]any
}

func (c fakeConfig) GetBool(k string) bool     { v, _ := c.vals[k].(bool); return v }
func (c fakeConfig) GetString(k string) string { v, _ := c.vals[k].(string); return v }

// 以下四个内嵌接口的 stub 只为「句柄非空」提供最小实现。
type stubCache struct{ cachex.Cache }
type stubKV struct{ kvx.KVStore }
type stubProducer struct{ mqx.Producer }
type stubLocker struct{ lockx.Locker }

// fakeSource 是 source 窄接口的假实现：各访问器返回可调值，
// 让 verify 的每条判据都能被单独驱动到（真 App 无法在外部包构造）。
type fakeSource struct {
	cfg   configx.Config
	db    any
	cache cachex.Instances
	kv    kvx.Stores
	mq    mqx.Producer
	lock  lockx.Locker
}

func (f fakeSource) Config() configx.Config  { return f.cfg }
func (f fakeSource) DB() any                 { return f.db }
func (f fakeSource) Cache() cachex.Instances { return f.cache }
func (f fakeSource) KV() kvx.Stores          { return f.kv }
func (f fakeSource) MQ() mqx.Producer        { return f.mq }
func (f fakeSource) Lock() lockx.Locker      { return f.lock }

func byComponent(checks []Check) map[string]Check {
	m := make(map[string]Check, len(checks))
	for _, c := range checks {
		m[c.Component] = c
	}
	return m
}

// ---- 核心判定 ------------------------------------------------------------

// Verify(nil) 必须是 nil 而不是 panic（spec：句柄缺失只影响核对，不应崩进程）。
func TestVerifyNilAppReturnsNil(t *testing.T) {
	if got := Verify(nil); got != nil {
		t.Fatalf("Verify(nil) 应返回 nil，得到 %v", got)
	}
}

// 全量判定：总闸开、五类句柄都在、组件开关全开。
// db 故意给「裸 gorm 句柄」（非空但无 dbx 回调）——这正是 spec 的
// 「自建句柄被拦下」场景：句柄在、要求入链、却产不出 span → Diverged。
func TestVerifyJudgesEachComponent(t *testing.T) {
	src := fakeSource{
		cfg: fakeConfig{vals: map[string]any{
			"service.name":   "order",
			"trace.enabled":  true,
			"trace.on.db":    true,
			"trace.on.cache": true,
			"trace.on.kv":    true,
			"trace.on.mq":    true,
			"trace.on.lock":  true,
		}},
		db:    &gorm.DB{}, // 裸句柄：非空，但无 dbx 注册的 trace 回调
		cache: cachex.Instances{"order": &stubCache{}},
		kv:    kvx.Stores{"order": &stubKV{}},
		mq:    &stubProducer{},
		lock:  &stubLocker{},
	}

	checks := verify(src)
	if len(checks) != 5 {
		t.Fatalf("应产出 5 条，实际 %d", len(checks))
	}
	// 顺序固定：db → cache → kv → mq → lock。
	wantOrder := []string{"db", "cache", "kv", "mq", "lock"}
	for i, n := range wantOrder {
		if checks[i].Component != n {
			t.Fatalf("顺序[%d] = %s, want %s", i, checks[i].Component, n)
		}
	}

	by := byComponent(checks)

	// db：Present+Want 都为真，但来源不合规（无回调）→ Observed=false → Diverged。
	if !by["db"].Present || !by["db"].Want {
		t.Fatalf("db Present/Want 应为真：%+v", by["db"])
	}
	if by["db"].Observed {
		t.Fatalf("裸 gorm 句柄不应观测到入链：%+v", by["db"])
	}
	if !by["db"].Diverged() {
		t.Fatalf("db 应判为背离（句柄在、要求入链、产不出 span）：%+v", by["db"])
	}

	// cache：全局 IsTracingEnabled 默认关 → 观测不到 → Diverged。
	if !by["cache"].Present || !by["cache"].Want {
		t.Fatalf("cache Present/Want 应为真：%+v", by["cache"])
	}
	if !by["cache"].Diverged() {
		t.Fatalf("cache 全局开关关时应背离：%+v", by["cache"])
	}

	// kv / mq / lock：traceflag 组件位默认放行 → Observed=true → 不背离。
	for _, n := range []string{"kv", "mq", "lock"} {
		if !by[n].Present || !by[n].Want {
			t.Fatalf("%s Present/Want 应为真：%+v", n, by[n])
		}
		if !by[n].Observed {
			t.Fatalf("%s 开关链代理应观测到入链：%+v", n, by[n])
		}
		if by[n].Diverged() {
			t.Fatalf("%s 不应背离：%+v", n, by[n])
		}
	}

	// 每条 Probe 必须非空（日志要靠它区分实测/代理）。
	for _, c := range checks {
		if c.Probe == "" {
			t.Fatalf("%s 的 Probe 不应为空", c.Component)
		}
	}
}

// 总闸 trace.enabled=false：组件开关即使是 true，Want 也必须被收敛为 false，
// 且一律不背离（“总闸关”是正当配置，服务本该带病跑）。
func TestVerifyMasterSwitchOffNoDiverge(t *testing.T) {
	src := fakeSource{
		cfg: fakeConfig{vals: map[string]any{
			"service.name":   "order",
			"trace.enabled":  false,
			"trace.on.db":    true,
			"trace.on.cache": true,
			"trace.on.kv":    true,
			"trace.on.mq":    true,
			"trace.on.lock":  true,
		}},
		db:    &gorm.DB{},
		cache: cachex.Instances{"order": &stubCache{}},
		kv:    kvx.Stores{"order": &stubKV{}},
		mq:    &stubProducer{},
		lock:  &stubLocker{},
	}
	for _, c := range verify(src) {
		if c.Want {
			t.Fatalf("%s：总闸关时 Want 必须为 false：%+v", c.Component, c)
		}
		if c.Diverged() {
			t.Fatalf("%s：总闸关时不应背离：%+v", c.Component, c)
		}
	}
}

// 组件开关关（但总闸开）：Want=false → 不背离（本就没打算入链）。
func TestVerifyComponentSwitchOffNoDiverge(t *testing.T) {
	src := fakeSource{
		cfg: fakeConfig{vals: map[string]any{
			"service.name":  "order",
			"trace.enabled": true,
			// trace.on.* 全部缺省 false
		}},
		db:    &gorm.DB{},
		cache: cachex.Instances{"order": &stubCache{}},
		kv:    kvx.Stores{"order": &stubKV{}},
		mq:    &stubProducer{},
		lock:  &stubLocker{},
	}
	for _, c := range verify(src) {
		if c.Want {
			t.Fatalf("%s：组件开关关时 Want 应为 false：%+v", c.Component, c)
		}
		if c.Diverged() {
			t.Fatalf("%s：组件开关关时不应背离：%+v", c.Component, c)
		}
	}
}

// 句柄全缺：Present=false → 即便 Want=true 也不背离（没配段不是错，是缺省）。
func TestVerifyAbsentHandleNoDiverge(t *testing.T) {
	src := fakeSource{
		cfg: fakeConfig{vals: map[string]any{
			"service.name":   "order",
			"trace.enabled":  true,
			"trace.on.db":    true,
			"trace.on.cache": true,
			"trace.on.kv":    true,
			"trace.on.mq":    true,
			"trace.on.lock":  true,
		}},
		// db/cache/kv/mq/lock 全为 nil
	}
	for _, c := range verify(src) {
		if c.Present {
			t.Fatalf("%s：句柄缺失时 Present 应为 false：%+v", c.Component, c)
		}
		if c.Diverged() {
			t.Fatalf("%s：句柄缺失不应背离：%+v", c.Component, c)
		}
	}
}

// Check.Diverged 真值表。
func TestCheckDiverged(t *testing.T) {
	cases := []struct {
		present, want, observed, wantDiv bool
	}{
		{true, true, false, true}, // 唯一的问题形态
		{true, true, true, false},
		{true, false, false, false},
		{true, false, true, false},
		{false, true, false, false},
		{false, false, false, false},
	}
	for _, tc := range cases {
		c := Check{Present: tc.present, Want: tc.want, Observed: tc.observed}
		if got := c.Diverged(); got != tc.wantDiv {
			t.Fatalf("Diverged(present=%v want=%v observed=%v) = %v, want %v",
				tc.present, tc.want, tc.observed, got, tc.wantDiv)
		}
	}
}

// String() 必须含规格要求的四个键名。
func TestCheckStringHasSpecKeys(t *testing.T) {
	c := Check{Component: "cache", Backend: "cachex", Present: true, Want: true, Observed: true, Probe: "cachex.IsTracingEnabled (global)"}
	s := c.String()
	for _, k := range []string{"component=", "backend=", "joins_trace=", "probe="} {
		if !strings.Contains(s, k) {
			t.Fatalf("String() 缺键 %q：%s", k, s)
		}
	}
}

// ---- 门面与参数校验 ------------------------------------------------------

// 名字非法：必须在触达 App 之前返回（nil App 也不会 panic）——坑 A 的前置顺序。
func TestVerifyOrErrorRejectsUnknownComponent(t *testing.T) {
	err := VerifyOrError(nil, "nope")
	if err == nil {
		t.Fatal("期望 unknown component 报错，实际为 nil")
	}
	if !strings.Contains(err.Error(), "unknown component") {
		t.Fatalf("错误串未点明 unknown component：%v", err)
	}
}

// nil App + 合法名：不得 panic，按“全部必需句柄缺失”返回 error。
func TestVerifyOrErrorNilAppReportsUnavailable(t *testing.T) {
	err := VerifyOrError(nil, "db")
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("nil App + 合法名应报 unavailable（不得 panic），得到 %v", err)
	}
}

// checkError：三类情形的汇总。
func TestCheckError(t *testing.T) {
	// 1) 必需句柄缺失。
	checks := []Check{
		{Component: "db", Present: false},
		{Component: "cache", Present: true},
	}
	if err := checkError(checks, []string{"cache"}); err != nil {
		t.Fatalf("cache 就位时不应报错，得到 %v", err)
	}
	err := checkError(checks, []string{"cache", "db"})
	if err == nil || !strings.Contains(err.Error(), "unavailable") || !strings.Contains(err.Error(), "db") {
		t.Fatalf("db 缺失应报 unavailable 且点出 db，得到 %v", err)
	}
	if err := checkError(nil, []string{"db"}); err == nil || !strings.Contains(err.Error(), "db") {
		t.Fatalf("空 checks 应判 db 缺失，得到 %v", err)
	}

	// 2) 背离（句柄在、要求入链、产不出 span）→ 必须失败。
	diverged := []Check{{Component: "db", Backend: "gorm/dbx", Present: true, Want: true, Observed: false, Probe: "p"}}
	err = checkError(diverged, nil)
	if err == nil || !strings.Contains(err.Error(), "join the trace") {
		t.Fatalf("背离应导致失败并点明 join the trace，得到 %v", err)
	}

	// 3) 两类同时出现：都应出现在 error 里。
	err = checkError(append(diverged, Check{Component: "cache", Present: false}), []string{"cache"})
	if err == nil || !strings.Contains(err.Error(), "join the trace") || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("两类问题应一并报出，得到 %v", err)
	}

	// 4) 无问题 → nil。
	if err := checkError([]Check{{Component: "db", Present: true, Want: false}}, nil); err != nil {
		t.Fatalf("无问题时应为 nil，得到 %v", err)
	}
}

// spec Scenario（db 版）：把 App.DB() 换成自建裸 gorm 句柄，装配期必须报
// 来源不合规并让启动失败 —— 这是本包唯一能当场拦下「自建句柄冒充」的实锤。
func TestCheckErrorFailsOnNonCompliantDBHandle(t *testing.T) {
	src := fakeSource{
		cfg: fakeConfig{vals: map[string]any{
			"service.name":  "order",
			"trace.enabled": true,
			"trace.on.db":   true,
		}},
		db: &gorm.DB{}, // 自建裸句柄：非空，但无 dbx 回调
	}
	err := checkError(verify(src), []string{"db"})
	if err == nil || !strings.Contains(err.Error(), "join the trace") {
		t.Fatalf("自建 db 句柄应导致启动失败，得到 %v", err)
	}
}

// dbx 探针的分支：nil / 非 gorm / 未初始化 / 无回调 / 有默认回调但缺 dbx 回调。
func TestDBXProbes(t *testing.T) {
	if dbxTraced(nil) {
		t.Fatal("nil 不应视为已入链")
	}
	if dbxTraced("not-a-db") {
		t.Fatal("非 gorm 句柄不应视为已入链")
	}
	if err := verifyDBX(nil); err == nil || !strings.Contains(err.Error(), "nil") {
		t.Fatalf("nil 句柄应报错，得到 %v", err)
	}
	if err := verifyDBX(&gorm.DB{}); err == nil || !strings.Contains(err.Error(), "not initialized") {
		t.Fatalf("裸 &gorm.DB{} 应报 gorm.Config is nil，得到 %v", err)
	}
	if err := verifyDBX(&gorm.DB{Config: &gorm.Config{}}); err == nil || !strings.Contains(err.Error(), "no gorm callbacks") {
		t.Fatalf("无回调应报错，得到 %v", err)
	}
	// gorm.Open(nil)：不接驱动也能拿到「有默认回调、但缺 dbx 回调」的句柄，
	// 用来覆盖回调探针的缺失分支。
	db, err := gorm.Open(nil)
	if err != nil {
		t.Fatalf("gorm.Open(nil) 不应报错：%v", err)
	}
	if err := verifyDBX(db); err == nil || !strings.Contains(err.Error(), "dbx:trace_before_create") {
		t.Fatalf("默认 gorm 句柄应判来源不合规并点出缺失回调，得到 %v", err)
	}
}

// knownComponents 必须与 verify 实际产出的组件名完全一致（单一来源，防漂移）。
func TestKnownComponentsMatchesVerify(t *testing.T) {
	src := fakeSource{
		cfg: fakeConfig{vals: map[string]any{"service.name": "order"}},
	}
	got := make([]string, 0, len(knownComponents))
	for _, c := range verify(src) {
		got = append(got, c.Component)
	}
	if len(got) != len(knownComponents) {
		t.Fatalf("verify 产出 %d 个组件，knownComponents 有 %d 个", len(got), len(knownComponents))
	}
	for i := range got {
		if got[i] != knownComponents[i] {
			t.Fatalf("组件名/顺序不一致：verify[%d]=%q knownComponents[%d]=%q", i, got[i], i, knownComponents[i])
		}
	}
	if knownComponent("search") {
		t.Fatal("search 不在 apps 范围（spec 只五类），knownComponent(\"search\") 应为 false")
	}
}
