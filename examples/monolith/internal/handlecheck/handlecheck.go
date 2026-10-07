// Package handlecheck 装配期对 launchx 交出的 DB 句柄做一次**来源确认**。
//
// 背景与 examples/mall 的同名包一致，这里再讲一遍是因为本示例单独持有一份：
// examples 之间互不引用（各自能单独拷走），80 行的判据复制比引入一条跨示例
// 依赖便宜。
//
// 为什么要这么验：只用 App.DB() 断言类型是不够的。自己 gorm.Open 一个同名库
// 照样通过类型断言，照样能查能写，唯独 SQL 不进调用链 —— 这种退化没有任何
// 报错，等要排障时打开 Jaeger 才发现整层 repository span 的下游是空的。
// 写成可执行断言后，起错句柄的服务直接起不来。
package handlecheck

import (
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// dbxCallbackNames dbx 的 orm.Open 给四类操作注册的 trace 回调名。
//
// 判据来源：dbx/orm/gorm_tracing.go 的 installTracing 注册
// "dbx:trace_before_{create,query,update,delete}"。裸 gorm.Open 出来的句柄
// 一个都没有。仅当 trace.on.db 为真时才调 VerifyDBX —— enable_trace=false 时
// dbx 走 OpenNoTrace，那时没有回调是**配置选择**，不该让服务起不来。
var dbxCallbackNames = []string{
	"dbx:trace_before_create",
	"dbx:trace_before_query",
	"dbx:trace_before_update",
	"dbx:trace_before_delete",
}

// dbxProbes 逐个取回调的探针。
//
// 写成函数切片而不是存 processor：gorm 的 processor 类型未导出，只能"拿到值
// 就立刻调方法"，不能用它的类型声明变量。
var dbxProbes = []func(*gorm.DB) func(*gorm.DB){
	func(db *gorm.DB) func(*gorm.DB) { return db.Callback().Create().Get("dbx:trace_before_create") },
	func(db *gorm.DB) func(*gorm.DB) { return db.Callback().Query().Get("dbx:trace_before_query") },
	func(db *gorm.DB) func(*gorm.DB) { return db.Callback().Update().Get("dbx:trace_before_update") },
	func(db *gorm.DB) func(*gorm.DB) { return db.Callback().Delete().Get("dbx:trace_before_delete") },
}

// VerifyDBX 确认 db 来自 launchx 的 dbx 组件（即经它执行的 SQL 会进调用链）。
//
// 判据是"回调在不在"而不是"能不能连"：连通性由 db 组件自检负责，这里只回答
// 一件事 —— 我用它查库，SQL 会进链吗。
func VerifyDBX(db *gorm.DB) error {
	if db == nil {
		return errors.New("handlecheck: db handle is nil")
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

// DialectorName 取句柄的驱动名（装配期日志用，回答"我连的是哪个库"）。
func DialectorName(db *gorm.DB) string {
	if db == nil || db.Dialector == nil {
		return "unknown"
	}
	if n := db.Dialector.Name(); n != "" {
		return n
	}
	return "unknown"
}
