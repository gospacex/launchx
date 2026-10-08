package handlecheck

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gospacex/cachex"
	"github.com/gospacex/launchx"
	"github.com/gospacex/traceflag"
	"gorm.io/gorm"
)

// Check 一条组件的装配期核对结果。
//
// Present / Want / Observed 三个字段分开是刻意的：它们回答三个不同的问题
// （句柄在不在 / 配置期望它入链吗 / 实测它会产 span 吗），合成一个 bool 之后
// 就分不清"没配"与"配了但没接上"，而后者才是要命的那种。
type Check struct {
	Component string // launchx 组件名，即 trace.on.<component> 的那个键
	Backend   string // 中间件的自述名，日志里用
	Present   bool   // 句柄是否存在（没配段 → false，不参与核对）
	Want      bool   // 有效期望值：trace.enabled AND trace.on.<component>
	Observed  bool   // 句柄层实测"会不会产 span"
	Probe     string // 这条判据怎么来的，写进日志便于日后复核
}

// Diverged 只有「句柄在、要求入链、却产不出 span」才算问题。
//
// 没配段（Present=false）或本就没打算入链（Want=false）都放行：前者不会有操作
// 落到这条不通的链路上，后者是明确的配置选择。把这两种也算成背离，会让"少配
// 一个组件"变成启动失败 —— 那是把降级路径堵死。
func (c Check) Diverged() bool { return c.Present && c.Want && !c.Observed }

func (c Check) String() string {
	return fmt.Sprintf("%s(%s): present=%v want=%v observed=%v probe=%s",
		c.Component, c.Backend, c.Present, c.Want, c.Observed, c.Probe)
}

// Verify 逐个核对本形态用到的存储/中间件句柄。
//
// 与 examples/monolith/internal/handlesrc 同源（examples 之间互不引用，
// 故判据是复制而不是引入依赖 —— 那份的包注释写明了理由）。覆盖面按**本形态
// 实际用到的组件**来定：微服四域用 db / cache / kv / mq / lock，没有
// search / mongo，故这里也不验它们（验了只会多出两条恒为 false 的噪声）。
//
// 各 SDK 的判据强度**不一致**，写在这里是希望后来者别把它当成同一类证据：
//
//   - dbx：最强。直接探针 gorm 的四个 trace 回调在不在句柄上 —— 回调是 dbx 的
//     orm.Open 注册进 gorm.DB 实例的，裸 gorm.Open 一个都没有，这是句柄**来源**
//     的实质证据；
//   - cachex：次强。读 SDK 的全局 tracing 开关（IsTracingEnabled），
//     它由 launchx 的 builder 写入，反映"这一侧被希望做成什么样"；
//   - kvx / mqx / lockx：最弱 —— 这三个在构造期就把实例级开关注册进了
//     traceflag 组件位（如 kvx.New → traceflag.Apply("kvx", cfg.EnableTrace)），
//     装配期能观测到的只有这条**开关链**的最终值，拿不到实例级字段。它能挡住
//     「组件开关被谁悄悄关了」这类失效，但挡不住「拿自考句柄冒充」—— 后者由
//     那条源码守卫（根包测试）负责，两者互补，缺任一都不成立。
func Verify(a *launchx.App) []Check {
	if a == nil {
		return nil
	}
	cfg := a.Config()
	name := cfg.GetString("service.name")

	// 期望值不是 yaml 里的原话，而是**有效裁决**：trace.enabled（总闸，决定
	// telemetry 是否装出真 provider）AND trace.on.<name>（组件开关）。
	//
	// 为什么不能直接拿 trace.on.<name>：traceflag 的全局位默认是 true，且 launchx
	// 并不把 trace.enabled 写进去（telemetry.Init 走的是 provider 侧），于是总闸
	// 关掉时组件开关读出来仍是 true。照原话当期望值，会把"总闸关"这种正当配置
	// 判成背离 —— 服务起不来，而它本该带病跑。
	master := cfg.GetBool("trace.enabled")
	want := func(comp string) bool {
		return master && cfg.GetBool("trace.on."+comp)
	}

	out := make([]Check, 0, 5)

	dbRaw := a.DB()
	dbPresent := false
	if db, _ := dbRaw.(*gorm.DB); db != nil {
		dbPresent = true
	}
	wantDB := want("db")
	out = append(out, Check{
		Component: "db", Backend: "gorm/dbx", Present: dbPresent, Want: wantDB,
		Observed: dbPresent && wantDB && dbxTraced(dbRaw),
		Probe:    "dbx gorm trace callbacks (create/query/update/delete)",
	})

	cPresent := a.Cache()[name] != nil
	wantCache := want("cache")
	out = append(out, Check{
		Component: "cache", Backend: "cachex", Present: cPresent, Want: wantCache,
		Observed: cachex.IsTracingEnabled(),
		Probe:    "cachex.IsTracingEnabled (global)",
	})

	kvPresent := a.KV()[name] != nil
	wantKV := want("kv")
	out = append(out, Check{
		Component: "kv", Backend: "kvx", Present: kvPresent, Want: wantKV,
		Observed: traceflag.EnabledFor("kvx"),
		Probe:    "traceflag.EnabledFor(kvx) — 开关链代理，非实例级实读",
	})

	mqPresent := a.MQ() != nil
	wantMQ := want("mq")
	out = append(out, Check{
		Component: "mq", Backend: "mqx", Present: mqPresent, Want: wantMQ,
		Observed: traceflag.EnabledFor("mqx"),
		Probe:    "traceflag.EnabledFor(mqx) — 开关链代理，非实例级实读",
	})

	lockPresent := a.Lock() != nil
	wantLock := want("lock")
	out = append(out, Check{
		Component: "lock", Backend: "lockx", Present: lockPresent, Want: wantLock,
		Observed: traceflag.EnabledFor("lockx"),
		Probe:    "traceflag.EnabledFor(lockx) — 开关链代理，非实例级实读",
	})

	return out
}

// dbxTraced 用 dbx 注册给 gorm 的 trace 回调判定句柄来源（入参是 App.DB() 的
// 原值，未断言前先判类型，避免断言失败时把进程带崩）。
func dbxTraced(v any) bool {
	db, ok := v.(*gorm.DB)
	if !ok || db == nil {
		return false
	}
	return VerifyDBX(db) == nil
}

// VerifyOrError 是 Verify 的门面：有背离即返回 error，让服务起不来。
//
// 为什么是 fail-fast 而不是 Warn：这类失效一律无声 —— 不报错、能读写、只是
// span 没有。留 Warn 等于把"发现"交给人去读日志，而这类问题只有排障时才有人读，
// 届时链路数据已经丢了。
func VerifyOrError(a *launchx.App) ([]Check, error) {
	checks := Verify(a)
	var lines []string
	for _, c := range checks {
		if !c.Diverged() {
			continue
		}
		lines = append(lines, fmt.Sprintf("handlecheck: %s handle is present but will NOT join the trace "+
			"(trace.on.%s=true while %s=false); its operations will work yet stay invisible on Jaeger",
			c.Backend, c.Component, c.Probe))
	}
	if len(lines) == 0 {
		return checks, nil
	}
	return checks, errors.New(strings.Join(lines, "; "))
}
