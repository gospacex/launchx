package launchx

import "testing"

// 本文件是「wire 图真的被主装配链路用上了」的集成判据。
//
// 与 internal/wiring 包内单测的分工：那边证明图**能**求解（provider 各自正确、
// 产物零 wire 依赖），这边证明图**被用了**——Bootstrap 走完骨架之后，App 上必须
// 能看到这次求解的产物。缺了这一层，注册面照样可以是"建好了但没人调"的孤岛，
// 单测全绿也照孤岛不误。

// TestComponentMountProducesWireGraph component.mount 必须产出注册面快照。
//
// 地基三条恒构造：配置是后续一切的输入，日志器与 TracerProvider 是组件与形态
// 装配都要用的。任何一个没构造出来，后面所有句柄都无从谈起。
func TestComponentMountProducesWireGraph(t *testing.T) {
	app, err := Bootstrap("testdata/monolith.yaml")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	defer func() { _ = app.Close() }()

	g := app.Graph()
	if g == nil {
		t.Fatal("component.mount 之后 Graph() 不应为 nil —— 说明装配没走 wire")
	}
	for name, ok := range map[string]bool{
		"configx": g.Config.Constructed(),
		"logx":    g.Logger.Constructed(),
		"otelx":   g.Teleme.Constructed(),
	} {
		if !ok {
			t.Errorf("地基 %s 未构造 —— 地基失败不降级，它必须恒在", name)
		}
	}
}

// TestGraphWebHandleIsTheOneAppUses 图里的 webx 句柄必须与 App.Engine() 是同一个。
//
// 这条锁的是「形态装配只做一次」：pipeline 先跑 mode.assemble（产物交给
// a.engine），component.mount 再求解图，图里的 ProvideModeReady 必须判出"已装配"
// 并跳过。若它又装一遍，Out.Webx 会换成新引擎，而业务早就按 a.engine 挂好了路由
// —— 服务能起来、端口在听，就是没有任何一条路由，且不报错。
func TestGraphWebHandleIsTheOneAppUses(t *testing.T) {
	app, err := Bootstrap("testdata/monolith.yaml")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	defer func() { _ = app.Close() }()

	g := app.Graph()
	if g == nil {
		t.Fatal("Graph() 为 nil")
	}
	if !g.Web.Constructed() {
		t.Fatal("monolith 档必须装出 webx 引擎，图里却是空的")
	}
	if g.Web.Raw() != app.Engine() {
		t.Error("图里的 webx 句柄与 App.Engine() 不是同一个 —— mode 被装配了两次，" +
			"业务挂路由的那个引擎已经不是 Out 里的这个")
	}
}

// TestGraphHandlesMirrorPresence 可选组件按 presence 裁决：没配段就不构造。
//
// testdata/monolith.yaml 里没有任何 xmiddleware 段，八类组件与两个"登记在册但
// 默认不构造"的 SDK 都应保持零值。同时它们的 raw 必须与 App 访问器同源 ——
// 图不是另一份构造真相，它只是把 Env 上的产物聚起来。
func TestGraphHandlesMirrorPresence(t *testing.T) {
	app, err := Bootstrap("testdata/monolith.yaml")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	defer func() { _ = app.Close() }()

	g := app.Graph()
	if g == nil {
		t.Fatal("Graph() 为 nil")
	}
	// 逐项点名，不合并成一条 || —— 合并的断言只会说"有一个不对"，
	// 排查时还得回头逐个 printf。
	for name, h := range map[string]interface{ Constructed() bool }{
		"dbx": g.DB, "cachex": g.Cache, "mqx": g.MQ, "coordx": g.Coord,
		"searchx": g.Search, "mongodbx": g.Mongo, "kvx": g.KV, "lockx": g.Lock,
	} {
		if h.Constructed() {
			t.Errorf("%s 被构造了，但 yaml 里没有对应的 xmiddleware 段（presence-based 失效）", name)
		}
	}
	if g.Object.Constructed() || g.Vector.Constructed() {
		t.Error("objectx / vectorx 登记在册但默认不构造，实际却被构造了")
	}
	if g.RPC.Constructed() {
		t.Error("monolith 档不该有 kitex 服务端选项")
	}
	// 同源性：图只是 Env 产物的聚合视图，不是第二份真相。
	if g.DB.Raw() != app.DB() {
		t.Error("Graph.DB 与 App.DB() 不同源 —— 图若另起一份构造，两边必然漂移")
	}
	// 生产侧与消费侧都留了位置：只留 producer 会逼业务自己 NewConsumer。
	if g.MQ.Producer != nil || g.MQ.Consumer != nil {
		t.Error("mqx 未配置时，生产侧与消费侧句柄都应为空")
	}
}
