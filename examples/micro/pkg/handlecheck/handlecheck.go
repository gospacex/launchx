// Package handlecheck 装配期对 launchx 交出的中间件句柄做一次**来源确认**。
//
// 背景：分层链要求业务只用 App.DB() / App.Cache() / App.KV() / App.Lock()
// 交出的句柄，不自建连接。但"用错句柄"是**静默**的 —— 自己 gorm.Open 一个
// 库照样能查能写，唯独 SQL 不进链路。等排障时打开 Jaeger 才发现整层
// repository span 是空的，那时候已经晚了。
//
// 所以把这条纪律写成可执行断言：装配期验一次，不符合就直接起不来。
// 四个微服共用，判据只在这里定一次。
package handlecheck

import (
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// dbxCallbackNames dbx 的 orm.Open 给四类操作注册的 trace 回调名。
//
// 判据来源：dbx/orm/gorm_tracing.go 的 installTracing 注册
// "dbx:trace_before_{create,query,update,delete}"。裸 gorm.Open 出来的
// 句柄一个都没有。config 里 enable_trace=false 时 dbx 走 OpenNoTrace，
// 同样没有 —— 那种情况不该调本校验（见 VerifyDBX 注释）。
var dbxCallbackNames = []string{
	"dbx:trace_before_create",
	"dbx:trace_before_query",
	"dbx:trace_before_update",
	"dbx:trace_before_delete",
}

// dbxProbes 逐个取回调的探针。
//
// 写成函数切片而不是存 *processor：gorm 的 processor 类型是未导出的，
// 只能"拿到值就立刻调方法"，不能拿它的类型去声明变量。
var dbxProbes = []func(*gorm.DB) func(*gorm.DB){
	func(db *gorm.DB) func(*gorm.DB) { return db.Callback().Create().Get("dbx:trace_before_create") },
	func(db *gorm.DB) func(*gorm.DB) { return db.Callback().Query().Get("dbx:trace_before_query") },
	func(db *gorm.DB) func(*gorm.DB) { return db.Callback().Update().Get("dbx:trace_before_update") },
	func(db *gorm.DB) func(*gorm.DB) { return db.Callback().Delete().Get("dbx:trace_before_delete") },
}

// VerifyDBX 确认 db 来自 launchx 的 dbx 组件（即 SQL 会进调用链）。
//
// 只在 config 的 trace.on.db 为真时调用：dbx 在 enable_trace=false 时走
// OpenNoTrace，句柄上没有这些回调，那是**配置选择**而不是用错句柄，
// 不该因此让服务起不来。
//
// 判据是"回调在不在"而不是"能不能连"：连通性由 launchx 的 db 组件自检
// 负责，这里只回答一件事 —— 我用它查库，SQL 会进链吗。
func VerifyDBX(db *gorm.DB) error {
	if db == nil {
		return errors.New("handlecheck: db handle is nil")
	}
	// 顺序不能改：gorm.DB 内嵌的是 **指针**（type DB struct{ *Config }），
	// callbacks 是经它提升上来的字段。Config 为 nil 时 db.Callback() 不是返回
	// nil，而是当场 nil dereference panic —— 这道"句柄不合格就让服务起不来"的
	// 检查会把进程自己带崩。见 examples/monolith/internal/handlesrc 的同名判据
	// （那边用 TestVerifyDBXRejectsBareHandle 构造 &gorm.DB{} 锁住这条）。
	if db.Config == nil {
		return errors.New("handlecheck: db handle is not initialized (gorm.Config is nil); " +
			"a dbx-produced handle always carries one")
	}
	if db.Callback() == nil {
		return errors.New("handlecheck: db handle carries no gorm callbacks")
	}
	for i, probe := range dbxProbes {
		if probe(db) == nil {
			return fmt.Errorf(
				"handlecheck: db handle is NOT from launchx dbx (missing gorm callback %q); "+
					"sql executed through it will not join the trace; "+
					"take the handle from App.DB() instead of gorm.Open",
				dbxCallbackNames[i])
		}
	}
	return nil
}

// DialectorName 取句柄的驱动名（装配期日志用，回答"我连的是哪个库"）
func DialectorName(db *gorm.DB) string {
	if db == nil || db.Dialector == nil {
		return "unknown"
	}
	if n := db.Dialector.Name(); n != "" {
		return n
	}
	return "unknown"
}
