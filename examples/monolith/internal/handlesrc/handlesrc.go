// Package handlesrc 在装配期对「存储类句柄」做双重确认：来源是否合规，以及
// 经它执行的操作会不会真的进调用链。
//
// 背景与 examples/micro/pkg/handlecheck 同源，这里是 monolith 自己持有的那份：
// examples 之间互不引用（各自能单独拷走），所以判据是复制而不是引入依赖。
//
// 为什么不把它交给 review 而是落成可执行断言：句柄错了不报错。自己 gorm.Open
// 一个同名库照样能查能写，组件级开关没打开也照样能读写 —— 唯一的差别在链路
// 上（这类 span 整片消失），而进程看着完全健康、没有任何告警。等到排障时打开
// Jaeger 才发现「配了 xmiddleware、跑通了业务、span 一个没有」，已经晚了。
package handlesrc

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gospacex/cachex"
	"github.com/gospacex/launchx"
	mongodbx "github.com/gospacex/mongodbx" // 包名为 docx，此处显式改名以对齐模块名
	"github.com/gospacex/traceflag"
	"gorm.io/gorm"
)

// Check 一条组件的核对结果。
type Check struct {
	Component string // launchx 组件名，即 trace.on.<component> 的那个键
	Backend   string // 中间件的自述名，日志里用
	Present   bool   // 句柄是否存在（没配段 → false，不参与核对）
	Want      bool   // trace.on 的期望值
	Observed  bool   // 句柄层实测「会不会产 span」
	Probe     string // 这条判据怎么来的，写进日志便于日后复核
}

// Diverged 报告期望与实际是否背离：只有「句柄在、要求入链、却产不出 span」
// 才算问题。没配段（Present=false）或本就没打算入链（Want=false）都放行。
func (c Check) Diverged() bool { return c.Present && c.Want && !c.Observed }

func (c Check) String() string {
	return fmt.Sprintf("%s(%s): present=%v want=%v observed=%v probe=%s",
		c.Component, c.Backend, c.Present, c.Want, c.Observed, c.Probe)
}

// Verify 逐个核对存储组件。返回全部结果（含正常的），让调用方能记一行状态。
//
// 各 SDK 的判据强度**不一致**，写在这里是希望后来者别把它当成同一类证据：
//
//   - dbx：最强。直接探针 gorm 的四个 trace 回调在不在句柄上 —— 回调是被
//     dbx 的 orm.Open 注册进 gorm.DB 实例的，裸 gorm.Open 一个都没有，这是
//     句柄**来源**的实质证据；
//   - cachex / mongodbx：次强。读 SDK 的全局 tracing 开关（IsTracingEnabled），
//     它由 launchx 的 builder 写入，反映「这一侧被希望做成什么样」；
//   - kvx / mqx / searchx：最弱 —— 这三个在构造期就把实例级开关注册进了
//     traceflag 组件位（如 kvx.New → traceflag.Apply("kvx", cfg.EnableTrace)），
//     装配期能观测到的只有这条**开关链**的最终值，拿不到实例级字段。它能挡住
//     「组件开关被谁悄悄关了」这类失效，但挡不住「拿自考句柄冒充」—— 后者由
//     VerifyStorageSources 那条源码守卫负责，两者互补，缺任一都不成立。
func Verify(a *launchx.App) []Check {
	if a == nil {
		return nil
	}
	cfg := a.Config()
	name := cfg.GetString("service.name")
	// 期望值不是 yaml 里的原话，而是**有效裁决**：trace.enabled（总闸，决定
	// telemetry 是否装出真 provider）AND trace.on.<name>（组件开关）。
	//
	// 为什么不能直接拿 trace.on.<name>：traceflag 的全局位默认是 true 且 launchx
	// 并不把 trace.enabled 写进去（telemetry.Init 走的是 provider 侧），于是总闸
	// 关掉时组件开关读出来仍是 true。照原话当期望值，会把"总闸关"这种正当配置
	// 判成背离 —— 服务起不来，而它本该带病跑。
	master := cfg.GetBool("trace.enabled")
	want := func(comp string) bool {
		return master && cfg.GetBool("trace.on."+comp)
	}

	out := make([]Check, 0, 6)

	// db：判据是 dbx 注册在 gorm 句柄上的四个 trace 回调。
	dbPresent := false
	if db, _ := a.DB().(*gorm.DB); db != nil {
		dbPresent = true
	}
	wantDB := want("db")
	out = append(out, Check{
		Component: "db", Backend: "gorm/dbx", Present: dbPresent, Want: wantDB,
		Observed: dbPresent && wantDB && dbxTraced(a.DB()),
		Probe:    "dbx gorm trace callbacks (create/query/update/delete)",
	})

	// cache：cachex 的 redis 驱动把 span 全挂在全局 IsTracingEnabled 上。
	cPresent := a.Cache()[name] != nil
	wantCache := want("cache")
	out = append(out, Check{
		Component: "cache", Backend: "cachex", Present: cPresent, Want: wantCache,
		Observed: cachex.IsTracingEnabled(),
		Probe:    "cachex.IsTracingEnabled (global)",
	})

	// kv：kvx.New 注册了 traceflag 组件位 "kvx"。
	kvPresent := a.KV()[name] != nil
	wantKV := want("kv")
	out = append(out, Check{
		Component: "kv", Backend: "kvx", Present: kvPresent, Want: wantKV,
		Observed: traceflag.EnabledFor("kvx"),
		Probe:    "traceflag.EnabledFor(kvx) — 开关链代理，非实例级实读",
	})

	// mq：同上，组件位 "mqx"。
	mqPresent := a.MQ() != nil
	wantMQ := want("mq")
	out = append(out, Check{
		Component: "mq", Backend: "mqx", Present: mqPresent, Want: wantMQ,
		Observed: traceflag.EnabledFor("mqx"),
		Probe:    "traceflag.EnabledFor(mqx) — 开关链代理，非实例级实读",
	})

	// search：组件位 "searchx"。
	searchPresent := a.Search() != nil
	wantSearch := want("search")
	out = append(out, Check{
		Component: "search", Backend: "searchx", Present: searchPresent, Want: wantSearch,
		Observed: traceflag.EnabledFor("searchx"),
		Probe:    "traceflag.EnabledFor(searchx) — 开关链代理，非实例级实读",
	})

	// mongodb：全局开关，与 cachex 同级。
	mongoPresent := a.Mongo()[name] != nil
	wantMongo := want("mongodb")
	out = append(out, Check{
		Component: "mongodb", Backend: "mongodbx", Present: mongoPresent, Want: wantMongo,
		Observed: mongodbx.IsTracingEnabled(),
		Probe:    "mongodbx.IsTracingEnabled (global)",
	})

	return out
}

// VerifyOrError 是 Verify 的门面：有背离即返回 error，让服务起不来。
//
// 为什么是 fail-fast 而不是 Warn：上面的失效形态一律无声 —— 不报错、能读写、
// 只是 span 没有。留 Warn 等于把「发现」这件事交给人去读日志，而这类问题只有
// 排障时才有人读，届时链路数据已经丢了。
func VerifyOrError(a *launchx.App) ([]Check, error) {
	checks := Verify(a)
	var lines []string
	for _, c := range checks {
		if !c.Diverged() {
			continue
		}
		lines = append(lines, fmt.Sprintf("handlesrc: %s handle is present but will NOT join the trace "+
			"(trace.on.%s=true while %s=false); its operations will work yet stay invisible on Jaeger",
			c.Backend, c.Component, c.Probe))
	}
	if len(lines) == 0 {
		return checks, nil
	}
	return checks, errors.New(strings.Join(lines, "; "))
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
	return VerifyDBX(db) == nil
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

// VerifyDBX 确认 db 来自 launchx 的 dbx 组件（即经它执行的 SQL 会进调用链）。
//
// 判据是「回调在不在」而不是「能不能连」：连通性由 db 组件自检负责，这里只回答
// 一件事 —— 我用它查库，SQL 会进链吗。
func VerifyDBX(db *gorm.DB) error {
	if db == nil {
		return errors.New("handlesrc: db handle is nil")
	}
	// 这道判 nil 的顺序不能改：gorm.DB 内嵌的是 **指针** type DB struct{ *Config }，
	// 下面所有 callbacks 都是经它提升上来的字段。Config 为 nil 时 db.Callback()
	// 不是返回 nil，而是当场 nil dereference panic —— 于是这道「句柄不合格就让
	// 服务起不来」的检查自己先把进程带崩了（本包测试 TestVerifyDBXRejectsBareHandle
	// 构造 &gorm.DB{} 复现，代价是一个 crash loop 而不是一条 error）。
	if db.Config == nil {
		return errors.New("handlesrc: db handle is not initialized (gorm.Config is nil); " +
			"a dbx-produced handle always carries one")
	}
	if db.Callback() == nil {
		return errors.New("handlesrc: db handle carries no gorm callbacks")
	}
	for i, probe := range dbxProbes {
		if probe(db) == nil {
			return fmt.Errorf(
				"handlesrc: db handle is NOT from launchx dbx (missing gorm callback %q); "+
					"sql executed through it will not join the trace; "+
					"take the handle from App.DB() instead of gorm.Open",
				dbxCallbackNames[i])
		}
	}
	return nil
}

// DialectorName 取句柄的驱动名（装配期日志用，回答「我连的是哪个库」）。
func DialectorName(db *gorm.DB) string {
	if db == nil || db.Dialector == nil {
		return "unknown"
	}
	if n := db.Dialector.Name(); n != "" {
		return n
	}
	return "unknown"
}
