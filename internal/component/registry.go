// Package component 是 launchx 的组件层：内置八类中间件组件的构造与裁决。
//
// 三条设计红线：
//
//  1. **presence-based 启用**：只有 yaml 里配了对应段（xmiddleware.sqlx /
//     xmiddleware.cachex / xmiddleware.mqx / xmiddleware.kvx / xmiddleware.lockx /
//     xmiddleware.coordx / xmiddleware.searchx / xmiddleware.mongodbx）才构造，
//     未配置即静默跳过。
//  2. **trace.on.<name> 只控入链，不控构造**：它决定调 `orm.Open` 还是
//     `orm.OpenNoTrace`（以及是否把 EnableTrace 传给 SDK），**绝不**决定组件是否
//     被创建。历史 bug：把该开关当构造开关，producer 变 nil，outbox 静默堆积到
//     retry_count=10 才放弃——故障形态是"消息没发出去但没人报错"。
//  3. **strict.<name> 决定失败姿态**：默认降级（字段置 nil + 带后果文案的 Warn，
//     启动继续）；true 则返回 error，交由 Bootstrap 走完整反序退出链。
//     地基（配置/telemetry/logx）失败不受 strict 影响，永远即退。
package component

import (
	"github.com/gospacex/cachex"
	"github.com/gospacex/configx"
	"github.com/gospacex/coordx"
	"github.com/gospacex/kvx"
	"github.com/gospacex/lockx"
	"github.com/gospacex/logx"
	mongodbx "github.com/gospacex/mongodbx" // 包名为 docx，此处显式改名以对齐模块名
	"github.com/gospacex/mqx"
	"github.com/gospacex/searchx"
)

// Env 组件构造环境。组件包只面向 Env，不回指 launchx 根包（避免 import 环）。
type Env struct {
	Cfg         configx.Config
	ServiceName string
	Logger      logx.Logger
	// TraceOn 组件级调用链开关，来自 yaml 的 trace.on.<name>。
	TraceOn map[string]bool
	// Strict 组件级失败裁决，来自 yaml 的 strict.<name>。
	Strict map[string]bool

	// ===== 构造产物（降级时保持零值）=====

	// DB 是 *gorm.DB。此处刻意用 any：launchx 库自身不直接依赖 gorm，
	// 调用方按需断言（保持"业务经 launchx 取句柄、不直引驱动"的依赖面）。
	DB    any
	Cache cachex.Instances
	MQ    mqx.Producer
	// Registry 注册句柄；Discover 发现句柄。coordx 把「注册」与「发现」拆成两个
	// 接口，而 HTTP 网关（按服务名解析上游实例）和 RPC 客户端都要用发现侧，
	// 故两个都留。降级或未配段时为零值。
	Registry coordx.Register
	Discover coordx.Discover
	Search   searchx.SearchClient
	Mongo    mongodbx.Instances
	// KV 键值存储命名实例表（kvx）；Lock 分布式锁器（lockx，单实例）。
	KV   kvx.Stores
	Lock lockx.Locker

	stops []func() error
}

// AddStop 注册组件级关闭函数（反序退出链按注册逆序执行）。
func (e *Env) AddStop(name string, fn func() error) {
	if fn == nil {
		return
	}
	e.stops = append(e.stops, fn)
}

// Stops 返回关闭函数快照（反序退出链用）。
func (e *Env) Stops() []func() error {
	out := make([]func() error, len(e.stops))
	copy(out, e.stops)
	return out
}

// TraceOnOf 返回组件级调用链开关（缺省 false）。
func (e *Env) TraceOnOf(name string) bool { return e.TraceOn[name] }

// StrictOf 返回组件级严格裁决（缺省 false = 降级）。
func (e *Env) StrictOf(name string) bool { return e.Strict[name] }

// Builder 组件构造器
type Builder func(*Env) error

var builders = map[string]Builder{}

// Register 注册组件构造器（同名覆盖）。
func Register(name string, b Builder) { builders[name] = b }

// Lookup 按名取构造器，未注册返回 nil。
func Lookup(name string) Builder { return builders[name] }

// Names 返回已注册组件名（稳定顺序，用于错误信息与断言）。
func Names() []string {
	out := make([]string, 0, len(builders))
	for k := range builders {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// Mount 按给定顺序构造组件（单个失败的处理由各 builder 自行裁决）。
func Mount(e *Env, order []string) error {
	for _, name := range order {
		b := Lookup(name)
		if b == nil {
			return errUnknownComponent(name)
		}
		if err := b(e); err != nil {
			return err
		}
	}
	return nil
}
