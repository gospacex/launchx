// Package handles 在装配期对 apps 的存储类句柄做双重确认：**来源是否合规**，
// 以及经它执行的操作**会不会真的进调用链**。
//
// 为什么必须有这一层：句柄错了不报错。自己开一个同名 redis / db 照样能读写，
// 组件级开关没打开也照样能读写 —— 唯一差别在链路上（这类 span 整片消失），
// 而进程看着完全健康、没有任何告警。等排障时打开 Jaeger 才发现「配了
// xmiddleware、跑通了业务、span 一个没有」，已经晚了。故这里 fail-fast。
//
// 判据强度**按组件如实分级**（对齐 examples/monolith 的 handlesrc，apps 各域
// 互不引用，故判据是复制而非引依赖）：
//
//   - db：**最强**。dbx 的 orm.Open 会把四类 trace 回调注册进 gorm.DB 实例，
//     裸 gorm.Open 一个都没有 —— 这是句柄**来源**的实质证据（见 dbxTraced），
//     也是本包唯一能当场拦下「自建句柄冒充」的判据。
//   - cache：次强。读 cachex 的全局 IsTracingEnabled —— 它由 launchx 的
//     builder 写入，反映「这一侧被希望做成什么样」；但它是**全局**开关，
//     分不出某个实例是不是自建。
//   - kv / mq / lock：**最弱**。这三者在构造期把实例级开关注册进 traceflag
//     组件位（kvx.New → traceflag.Apply("kvx", …)，mqx 同，lockx 同），
//     装配期只能观测到这条**开关链**的最终值。它能挡住「组件开关被谁悄悄关了」
//     这类失效，但挡不住「拿自考句柄冒充」。
//
// 换言之：来源合规的**装配期兜底**只有 db 有实质判据；cache/kv/mq/lock 的来源
// 合规用现有公开 API 在装配期读不出来，只能靠**源码守卫**（Task 9 的依赖面 /
// 来源扫描）与这里的开关链探针互补，缺任一都不成立。本包**不把开关链探针
// 冒充成来源实锤** —— 每条 Probe 都写明它是「实测」还是「代理」。
package handles

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gospacex/cachex"
	"github.com/gospacex/configx"
	"github.com/gospacex/kvx"
	"github.com/gospacex/launchx"
	"github.com/gospacex/lockx"
	"github.com/gospacex/mqx"
	"github.com/gospacex/traceflag"
	"gorm.io/gorm"
)

// Check 一条组件的核对结果。
type Check struct {
	Component string // launchx 组件名，即 trace.on.<component> 的那个键
	Backend   string // 中间件的自述名，日志里用
	Present   bool   // 句柄是否存在（没配段 → false，不参与核对）
	Want      bool   // 入链的**有效裁决**：trace.enabled AND trace.on.<component>
	Observed  bool   // 句柄层实测 / 代理「会不会产 span」
	Probe     string // 判据怎么来的，写进日志便于日后复核
}

// Diverged 报告期望与实际是否背离：只有「句柄在、要求入链、却产不出 span」
// 才算问题。没配段（Present=false）或本就没打算入链（Want=false）都放行。
func (c Check) Diverged() bool { return c.Present && c.Want && !c.Observed }

// String 渲染成启动日志的一行。键名对齐 spec 要求：
// component / backend / joins_trace / probe（joins_trace 即 Observed），
// 另附 present / want 便于排障。
func (c Check) String() string {
	return fmt.Sprintf("component=%s backend=%s present=%v want=%v joins_trace=%v probe=%q",
		c.Component, c.Backend, c.Present, c.Want, c.Observed, c.Probe)
}

// source 是自检真正需要的窄接口：只含六个访问器。
//
// *launchx.App 天然满足它，故 Verify 的对外签名不变；而测试可注入假件，
// 把每条判定（Present / Want / Observed / Diverged）单独驱动到 —— 真 App
// 在外部包构造不出来（App 字段未导出），不抽窄接口则核心逻辑无从覆盖。
type source interface {
	Config() configx.Config
	DB() any
	Cache() cachex.Instances
	KV() kvx.Stores
	MQ() mqx.Producer
	Lock() lockx.Locker
}

// componentChecks 五类句柄的判定表：**名字、顺序、判定逻辑的唯一来源**。
// verify 按它迭代；knownComponents / knownComponent 也由它派生，
// 避免「一张名单 + 五个手写分支」两处各写一遍而漂移。
var componentChecks = []struct {
	name  string
	check func(s source, name string, want bool) Check
}{
	{"db", checkDB},
	{"cache", checkCache},
	{"kv", checkKV},
	{"mq", checkMQ},
	{"lock", checkLock},
}

// Verify 逐个核对五类句柄。返回全部结果（含正常的），让调用方能记一行状态。
// 顺序固定（取自 componentChecks）。nil App 返回 nil（句柄缺失不该崩进程）。
func Verify(a *launchx.App) []Check {
	if a == nil {
		return nil
	}
	return verify(a)
}

// verify 是判定主体，只依赖窄接口 source，故测试可注入假件。
func verify(s source) []Check {
	cfg := s.Config()
	name := cfg.GetString("service.name")
	// 期望值不是 yaml 里的原话，而是**有效裁决**：trace.enabled（总闸）AND
	// trace.on.<name>（组件开关）。
	//
	// 为什么不能直接拿 trace.on.<name>：traceflag 的全局位默认 true、launchx
	// 也不把 trace.enabled 写进去，于是总闸关掉时组件开关读出来仍是 true。
	// 照原话当期望值，会把「总闸关」这种正当配置判成背离 —— 服务起不来，
	// 而它本该带病跑。
	master := cfg.GetBool("trace.enabled")

	out := make([]Check, 0, len(componentChecks))
	for _, cc := range componentChecks {
		want := master && cfg.GetBool("trace.on."+cc.name)
		out = append(out, cc.check(s, name, want))
	}
	return out
}

// db：判据是 dbx 注册在 gorm 句柄上的 trace 回调 —— 本包唯一能验**来源**的实锤。
func checkDB(s source, _ string, want bool) Check {
	db, ok := s.DB().(*gorm.DB)
	present := ok && db != nil
	return Check{
		Component: "db", Backend: "gorm/dbx", Present: present, Want: want,
		Observed: present && want && dbxTraced(db),
		Probe:    "dbx gorm trace callbacks create/query/update/delete — 来源实测（裸 gorm.Open 无回调）",
	}
}

// cache：判据是 cachex 的全局 tracing 开关（由 launchx builder 写入）。
func checkCache(s source, name string, want bool) Check {
	present := s.Cache()[name] != nil
	return Check{
		Component: "cache", Backend: "cachex", Present: present, Want: want,
		Observed: cachex.IsTracingEnabled(),
		Probe:    "cachex.IsTracingEnabled (global) — 全局开关代理，非实例级实读，分不出自建实例",
	}
}

// kv：kvx.New 注册了 traceflag 组件位 "kvx"。
func checkKV(s source, name string, want bool) Check {
	present := s.KV()[name] != nil
	return Check{
		Component: "kv", Backend: "kvx", Present: present, Want: want,
		Observed: traceflag.EnabledFor("kvx"),
		Probe:    "traceflag.EnabledFor(kvx) — 开关链代理，非实例级实读",
	}
}

// mq：mqx 注册了组件位 "mqx"。
func checkMQ(s source, _ string, want bool) Check {
	present := s.MQ() != nil
	return Check{
		Component: "mq", Backend: "mqx", Present: present, Want: want,
		Observed: traceflag.EnabledFor("mqx"),
		Probe:    "traceflag.EnabledFor(mqx) — 开关链代理，非实例级实读",
	}
}

// lock：lockx 在 lockx/interface.go 里注册了组件位 "lockx"，但**未暴露实例级
// tracing 访问器**，故判据强度与 kv / mq 同档（开关链代理）。
func checkLock(s source, _ string, want bool) Check {
	present := s.Lock() != nil
	return Check{
		Component: "lock", Backend: "lockx", Present: present, Want: want,
		Observed: traceflag.EnabledFor("lockx"),
		Probe:    "traceflag.EnabledFor(lockx) — 开关链代理，非实例级实读（lockx 无实例级 tracing 访问器）",
	}
}

// knownComponents 合法组件名的有序集合，由 componentChecks 派生 —— 单一来源。
var knownComponents = func() []string {
	names := make([]string, 0, len(componentChecks))
	for _, c := range componentChecks {
		names = append(names, c.name)
	}
	return names
}()

func knownComponent(name string) bool {
	for _, n := range knownComponents {
		if n == name {
			return true
		}
	}
	return false
}

// VerifyOrError 汇总两类问题并返回 error，让服务起不来（spec：任一不满足
// MUST 让 Setup 返回 error）：
//
//  1. **背离（Diverged）**：句柄在、要求入链、却产不出 span —— 来源不合规
//     （如自建裸 gorm 句柄）或组件开关被悄悄关，都会落到这里；
//  2. **必需句柄缺失**：由调用方声明（srv-* 传 db，bff 传 cache）。
//
// 校验顺序刻意是「先校验组件名、再触达 App」：名字写错是调用方的**参数错误**，
// 不该依赖 App 可用才能发现 —— 若先跑 Verify(a)，nil 或半装配的 App 会在
// a.Config() 处 panic（且 nil *launchx.App 装进 source 接口后是**非 nil 接口**，
// 更隐蔽），把「参数打错」伪装成崩溃。
func VerifyOrError(a *launchx.App, required ...string) error {
	for _, name := range required {
		if !knownComponent(name) {
			return fmt.Errorf("handles: unknown component %q", name)
		}
	}
	var checks []Check
	if a != nil {
		checks = verify(a)
	}
	return checkError(checks, required)
}

// checkError 汇总两类问题（背离 + 必需缺失），任一成立即返回 error；
// checks 为空（nil App）视为「全部缺失」。
func checkError(checks []Check, required []string) error {
	var problems []string
	for _, c := range checks {
		if c.Diverged() {
			problems = append(problems, fmt.Sprintf(
				"handles: %s handle is present and trace.on.%s is on, but it will NOT join the trace (%s); "+
					"its operations will work yet stay invisible on Jaeger",
				c.Backend, c.Component, c.Probe))
		}
	}
	if missing := missingComponents(checks, required); len(missing) > 0 {
		problems = append(problems, fmt.Sprintf(
			"handles: required component(s) unavailable: %s (check xmiddleware section in config)",
			strings.Join(missing, ",")))
	}
	if len(problems) == 0 {
		return nil
	}
	return errors.New(strings.Join(problems, "; "))
}

// missingComponents 从 checks 里挑出 required 中尚未就位的组件名，顺序随 required。
func missingComponents(checks []Check, required []string) []string {
	byName := make(map[string]Check, len(checks))
	for _, c := range checks {
		byName[c.Component] = c
	}
	var missing []string
	for _, name := range required {
		if !byName[name].Present {
			missing = append(missing, name)
		}
	}
	return missing
}

// dbxTraced 用 dbx 注册给 gorm 的 trace 回调判定句柄来源。
//
// 判据来源：dbx/orm/gorm_tracing.go 的 installTracing 注册
// "dbx:trace_before_{create,query,update,delete}"。裸 gorm.Open 出来的句柄
// 一个都没有，而自己开的库照样能查能写 —— 这是装配期唯一能当场区分两者的手段。
func dbxTraced(v any) bool {
	db, ok := v.(*gorm.DB)
	if !ok || db == nil {
		return false
	}
	return verifyDBX(db) == nil
}

// dbxCallbackNames dbx 的 orm.Open 注册的四类操作 trace 回调名。
var dbxCallbackNames = []string{
	"dbx:trace_before_create",
	"dbx:trace_before_query",
	"dbx:trace_before_update",
	"dbx:trace_before_delete",
}

// dbxProbes 逐个取回调的探针。
//
// 写成函数切片而不是存 processor：gorm 的 processor 类型未导出，只能「拿到值
// 就立刻调方法」，不能用它的类型声明变量。
var dbxProbes = []func(*gorm.DB) func(*gorm.DB){
	func(db *gorm.DB) func(*gorm.DB) { return db.Callback().Create().Get("dbx:trace_before_create") },
	func(db *gorm.DB) func(*gorm.DB) { return db.Callback().Query().Get("dbx:trace_before_query") },
	func(db *gorm.DB) func(*gorm.DB) { return db.Callback().Update().Get("dbx:trace_before_update") },
	func(db *gorm.DB) func(*gorm.DB) { return db.Callback().Delete().Get("dbx:trace_before_delete") },
}

// verifyDBX 确认 db 来自 launchx 的 dbx 组件（即经它执行的 SQL 会进调用链）。
//
// 判据是「回调在不在」而不是「能不能连」：连通性由 db 组件自检负责，这里只
// 回答一件事 —— 我用它查库，SQL 会进链吗。
func verifyDBX(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("handles: db handle is nil")
	}
	// 这道判 nil 的顺序不能改：gorm.DB 内嵌的是 **指针** type DB struct{ *Config }，
	// 下面所有 callbacks 都是经它提升上来的字段。Config 为 nil 时 db.Callback()
	// 不是返回 nil，而是当场 nil dereference panic —— 于是这道「句柄不合格就让
	// 服务起不来」的检查自己先把进程带崩了（裸 &gorm.DB{} 即可复现），
	// 代价是一个 crash loop 而不是一条 error。
	if db.Config == nil {
		return fmt.Errorf("handles: db handle is not initialized (gorm.Config is nil); " +
			"a dbx-produced handle always carries one")
	}
	if db.Callback() == nil {
		return fmt.Errorf("handles: db handle carries no gorm callbacks")
	}
	for i, probe := range dbxProbes {
		if probe(db) == nil {
			return fmt.Errorf(
				"handles: db handle is NOT from launchx dbx (missing gorm callback %q); "+
					"sql executed through it will not join the trace; "+
					"take the handle from App.DB() instead of gorm.Open",
				dbxCallbackNames[i])
		}
	}
	return nil
}
