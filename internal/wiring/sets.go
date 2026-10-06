package wiring

import (
	"github.com/gospacex/launchx/internal/component"
	"github.com/gospacex/launchx/internal/mode"
)

// Graph 是注入器的一次性产出：15 个 SDK 的句柄全在这里。
//
// 为什么要有这个聚合结构而不是让调用方逐个调 provider：15 个 provider 若由
// 调用方各自调用，顺序就得靠人记（这是以前踩过的坑）；交给 wire 之后，顺序由
// 字段依赖推导，改动一个 provider 的入参其余自动跟着走。
type Graph struct {
	Config ConfigHandle
	Logger LoggerHandle
	Teleme TelemetryHandle

	DB     DBHandle
	Cache  CacheHandle
	MQ     MQHandle
	Coord  CoordHandle
	Search SearchHandle
	Mongo  MongoHandle
	KV     KVHandle
	Lock   LockHandle

	Object ObjectHandle
	Vector VectorHandle

	RPC RPCHandle
	Web WebHandle
}

// NewGraph 把所有句柄聚成 Graph。它的参数表就是 wire 的求解目标。
func NewGraph(
	cfg ConfigHandle,
	log LoggerHandle,
	tel TelemetryHandle,
	db DBHandle,
	cache CacheHandle,
	mq MQHandle,
	coord CoordHandle,
	search SearchHandle,
	mongo MongoHandle,
	kv KVHandle,
	lock LockHandle,
	object ObjectHandle,
	vector VectorHandle,
	rpc RPCHandle,
	web WebHandle,
) Graph {
	return Graph{
		Config: cfg, Logger: log, Teleme: tel,
		DB: db, Cache: cache, MQ: mq, Coord: coord,
		Search: search, Mongo: mongo, KV: kv, Lock: lock,
		Object: object, Vector: vector, RPC: rpc, Web: web,
	}
}

// InitializeGraphFunc 是注入器的函数形态，便于以变量方式传递（测试替身也用得上）。
type InitializeGraphFunc func(env *component.Env, holders *mode.AppHolders) (Graph, error)

// AssertGraphTypes 让 Graph 与各句柄的类型被显式引用一次，
// 避免「定义了却没人用」的误解 —— 它们的使用方是 wire 生成的注入器。
var AssertGraphTypes = func(g Graph) bool {
	return g.DB.Constructed() && g.Web.Constructed()
}
