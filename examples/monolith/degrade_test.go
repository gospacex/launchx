// 本文件：降级路径验收（tasks.md 3.9）——组件后端不可达时服务"带病运行"，
// 以及 strict 把同一处失败翻成"启动即退"的对照。
//
// 为什么这批用例必须自己改配置起进程：降级是**启动期**行为，靠给常驻进程发请求
// 试不出来；而"死地址"这种配置在生产里也不会有人预先写好。
//
// ⚠️ 本文件第一条实测结论（写在这里是因为它解释了下面判据为什么长这样）：
// **各 SDK 的构造是否真的去连后端并不一致** —— redis 侧（cachex / kvx）构造即
// ping，连不上就走 launchx 的降级分支；而 searchx / mongodbx / coordx 是**惰性
// 构造**，给个死地址照样"client created"，故障推迟到第一次真正读写时才暴露。
// 于是"配了段、句柄非 nil、进程起得来"与"这个后端其实是死的"可以同时成立，
// 启动期日志干干净净。所以下面对惰性构造的三类组件，判据不是"有没有降级 Warn"，
// 而是"后端死了之后能不能在链路上被发现" —— 后者才是运维真正需要的性质。
//
// 每条用例统一回答：这个组件挂了之后，服务是"还活着但少了这块能力"还是
// "整个起不来"。两种姿态都对，但必须由 strict.<name> 显式选择。
package monolith

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gospacex/launchx/internal/component"
	"github.com/gospacex/launchx/internal/traceutil"
)

// 死地址：指向确定没有进程监听的端口，连不上而不是连错地方。
// 刻意不用 "0.0.0.0" 之类会被 SDK 当成缺省值的写法。
//
// ⚠️ 端口不是随手挑的：本机 27018/27019/27020 上跑着三个 mongo 容器，第一次把
// Mongo 的死地址写成 27019 时它是**活的**，于是"不可达"用例变成了"一切正常"，
// 断言"后端失败必须可见"理所当然地红了 —— 是判据抓出了地址选错，不是用例写坏。
// 故每条用例开工前先 requireDeadPort，杜绝这类静默退化。
var (
	deadSearchAddr = cfgEdit{old: `"http://127.0.0.1:9200"`, new: `"http://127.0.0.1:9299"`}
	deadMongoURI   = cfgEdit{old: `"mongodb://127.0.0.1:27018"`, new: `"mongodb://127.0.0.1:27099"`}
	strictCache    = cfgEdit{old: "strict:\n  db: true", new: "strict:\n  db: true\n  cache: true"}
	traceOff       = cfgEdit{old: "trace:\n  enabled: true", new: "trace:\n  enabled: false"}
)

// deadAddrs 各用例用到的死地址，开工前逐一确认它们确实是死的。
var deadAddrs = []string{
	"127.0.0.1:9299",  // ES
	"127.0.0.1:27099", // Mongo
	"127.0.0.1:6399",  // redis（cachex + kvx）
	"127.0.0.1:8599",  // consul
}

// requireDeadPorts 确认死地址上没有进程在听。
//
// 为什么显式校验：降级用例的全部前提就是"后端不可达"。地址一旦被别的服务占了
// （本机容器众多，端口撞车是常态），用例会跑出一个全绿的"没有降级发生"——
// 比失败危险，因为它看起来像验收通过。
func requireDeadPorts(t *testing.T) {
	t.Helper()
	for _, a := range deadAddrs {
		if portOpen(a) {
			t.Fatalf("%s 上有进程在听，本用例的前提（后端不可达）不成立：换一个确定空闲的端口", a)
		}
	}
}

// deadRedis 把 cachex 与 kvx 一起指向死端口。
//
// 为什么两个一起改：config.yaml 里两者的 addr 串完全相同（同一个 redis 的不同
// db），只替换一处需要靠上下文定位，脆弱且容易替换错；一起改也不影响判据 ——
// 本用例验的是"这两个组件都挂了核心路径还通不通"。
var deadRedis = cfgEdit{old: "127.0.0.1:6379", new: "127.0.0.1:6399"}

// deadConsul 追加一段指向死地址的 coordx。
//
// 单体形态本来不注册（config.yaml 刻意不写 coordx 段），这里**新增**一段是为了
// 覆盖"注册组件构造失败"这条路径 —— 它在微服形态下就是"consul 挂了"。
// 写在文件尾：xmiddleware 是最后一个顶层键，缩进两格即成为它的子键。
var deadConsul = cfgEdit{new: "  coordx:\n    driver: consul\n    addr: \"127.0.0.1:8599\""}

// requireDegradeEnv 环境不具备就跳过（与真机验收保持同一口径）。
func requireDegradeEnv(t *testing.T) {
	t.Helper()
	if err := ensureSchema(); err != nil {
		t.Skipf("库表未就绪，跳过：%v", err)
	}
	if _, err := buildBinary(); err != nil {
		t.Skipf("构建单体二进制失败，跳过：%v", err)
	}
}

// assertCorePathAlive 断言核心下单路径在组件降级后仍然通，返回订单号。
func assertCorePathAlive(t *testing.T, in *instance) string {
	t.Helper()
	item := fmt.Sprintf("degrade-%d", time.Now().UnixNano())
	body := mustStatus(t, in, http.MethodPost, "/orders",
		fmt.Sprintf(`{"user_id":1,"item":%q,"sku":"SKU-DEG","qty":1,"unit_price":1.5}`, item),
		http.StatusCreated)
	no := fieldString(t, body, "order_no")
	if no == "" {
		t.Fatalf("降级后下单没有返回 order_no：%s", truncate(body, 200))
	}
	got := mustStatus(t, in, http.MethodGet, "/orders/"+no, "", http.StatusOK)
	if !strings.Contains(got, item) {
		t.Errorf("降级后读回的订单不含 item=%q：%s", item, truncate(got, 200))
	}
	return no
}

// assertDegraded 断言"急切构造型"组件的降级痕迹：库侧 component degraded +
// 装配层的能力缺失 Warn。
//
// 两处都查是因为只查一处会漏掉一类半吊子状态：只有库侧 Warn，可能是"降级了但
// 业务照旧调用 nil 句柄"；只有业务侧 Warn，可能是"业务自己兜住了而库侧其实即退了"。
func assertDegraded(t *testing.T, in *instance, consequence, demoWarn string) {
	t.Helper()
	logs := in.logs.String()
	if !strings.Contains(logs, "component degraded") {
		t.Errorf("日志里没有降级 Warn（%s）：\n%s", consequence, tail(logs, 25))
		return
	}
	if !strings.Contains(logs, consequence) {
		t.Errorf("降级 Warn 里没有后果文案 %q：\n%s", consequence, tail(logs, 25))
	}
	if !strings.Contains(logs, demoWarn) {
		t.Errorf("装配层没有为缺失组件留痕（want %q）：\n%s", demoWarn, tail(logs, 25))
	}
}

// assertBackendFailureIsVisible 断言"后端是死的"这件事在链路上看得见。
//
// 三条判据命中任意一条即算可发现，因为它们对应两种不同的降级形态：
//   - 构造期就失败（急切型 SDK）→ 句柄为 nil → 业务 span 上打 `<n>.skipped=true`；
//   - 构造期成功（惰性 SDK）→ 句柄非 nil → 首次读写失败，组件 span 标 error，
//     同时业务 span 记 degrade.reason。
//
// 为什么必须有这条而不是只验"服务还能用"：两种形态下启动期都可能一片干净
// （惰性 SDK 连都不连），若失败再不在链路上留痕，就变成"配了 ES、进程正常、
// 下单 201，但索引一直没写"的静默失效 —— 只能等业务侧对不上数据才发现。
func assertBackendFailureIsVisible(t *testing.T, tr traceutil.Trace, spanPrefix, wantReason, skipAttr string) {
	t.Helper()
	var errSpans []string
	var reasons []string
	var skipped bool
	for _, s := range tr.Spans {
		if r := tr.StringTag(s, "degrade.reason"); r != "" {
			reasons = append(reasons, r)
		}
		if strings.HasPrefix(s.OperationName, spanPrefix) && tr.IsError(s) {
			errSpans = append(errSpans, s.OperationName)
		}
		if v, ok := tr.TagsOf(s)[skipAttr]; ok && fmt.Sprint(v) == "true" {
			skipped = true
		}
	}
	if len(errSpans) == 0 && !containsString(reasons, wantReason) && !skipped {
		t.Errorf("trace %s 上看不到 %s 的失败（error span=%v，degrade.reason=%v，%s 未置位）：\n"+
			"后端不可达却没有任何留痕 = 静默失效，ops=%v",
			tr.TraceID, spanPrefix, errSpans, reasons, skipAttr, tr.OperationNames())
		return
	}
	t.Logf("可发现 ✓ trace %s：error span=%v degrade.reason=%v %s=%v",
		tr.TraceID, errSpans, reasons, skipAttr, skipped)
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestDegradeSearchUnreachableKeepsServing ES 不可达：下单照旧，失败必须在链路上可见。
func TestDegradeSearchUnreachableKeepsServing(t *testing.T) {
	c := requireJaeger(t)
	requireDegradeEnv(t)
	requireDeadPorts(t)
	in := launch(t, "degrade-search", deadSearchAddr)

	since := time.Now()
	no := assertCorePathAlive(t, in)
	tr := awaitTrace(t, c, since, http.MethodPost)
	assertBackendFailureIsVisible(t, tr, "search.", "search.index_failed", "search.skipped")

	// 构造期是否降级只记录不断言：惰性构造的 SDK 不会在 New 时连后端，
	// 断言"必须有/必须没有降级 Warn"都会把 SDK 的实现细节钉成契约。
	if strings.Contains(in.logs.String(), "component degraded") {
		t.Logf("注：本次 searchx 在构造期即失败并降级（SDK 行为可能随版本变化）")
	} else {
		t.Logf("降级 ✓ ES 不可达时进程带病运行（构造期无 Warn：惰性构造），下单 %s 仍成功", no)
	}
}

// TestDegradeMongoUnreachableKeepsServing Mongo 不可达：下单照旧，失败必须在链路上可见。
func TestDegradeMongoUnreachableKeepsServing(t *testing.T) {
	c := requireJaeger(t)
	requireDegradeEnv(t)
	requireDeadPorts(t)
	in := launch(t, "degrade-mongo", deadMongoURI)

	since := time.Now()
	no := assertCorePathAlive(t, in)
	tr := awaitTrace(t, c, since, http.MethodPost)
	assertBackendFailureIsVisible(t, tr, "docs.", "docs.save_failed", "docs.skipped")

	if strings.Contains(in.logs.String(), "component degraded") {
		t.Logf("注：本次 mongodbx 在构造期即失败并降级（SDK 行为可能随版本变化）")
	} else {
		t.Logf("降级 ✓ Mongo 不可达时进程带病运行（构造期无 Warn：惰性构造），下单 %s 仍成功", no)
	}
}

// TestDegradeRedisUnreachableKeepsServing redis 不可达：缓存与限流一起下线。
//
// 这是**急切构造**的一类（构造即 ping），故降级 Warn 是硬断言。
// 另外断言 /secure 分组确实没注册：鉴权依赖缓存里的令牌，缓存没了就该整组不挂，
// 而不是挂一个恒 401 的空壳 —— 恒 401 会把"组件降级"伪装成"调用方没带令牌"。
func TestDegradeRedisUnreachableKeepsServing(t *testing.T) {
	requireDegradeEnv(t)
	requireDeadPorts(t)
	in := launch(t, "degrade-redis", deadRedis)

	logs := in.logs.String()
	if !strings.Contains(logs, "component degraded") {
		t.Errorf("redis 不可达却没有降级 Warn：\n%s", tail(logs, 30))
	}
	for _, want := range []string{component.ConsequenceCache, "cache unavailable", "kv unavailable"} {
		if !strings.Contains(logs, want) {
			t.Errorf("降级痕迹缺失（want %q）：\n%s", want, tail(logs, 30))
		}
	}
	assertCorePathAlive(t, in)

	// /secure 分组在鉴权不可装配时不应存在。
	if _, code, err := in.do(http.MethodGet, "/secure/orders", ""); err == nil && code != http.StatusNotFound {
		t.Errorf("缓存降级后 GET /secure/orders → HTTP %d，want 404：/secure 分组应整组不注册", code)
	}
	t.Logf("降级 ✓ redis 不可达时进程带病运行（缓存穿透/限流 fail-open，/secure 未注册）")
}

// TestDegradeConsulUnreachableKeepsServing consul 不可达：进程照起、下单照旧。
//
// 实测结论：coordx 是惰性构造 —— 死地址上照样打出 "register initialized"，
// 启动期没有降级 Warn。故障推迟到真正注册/发现那一跳才暴露（单体不注册，
// 所以这条路径在单体形态上运行时无感）。这里断言的是"组件确实被构造了" +
// "核心路径不受影响"，把惰性这一事实作为发现记录进 close-issues。
func TestDegradeConsulUnreachableKeepsServing(t *testing.T) {
	requireDegradeEnv(t)
	requireDeadPorts(t)
	in := launch(t, "degrade-consul", deadConsul)

	logs := in.logs.String()
	if !strings.Contains(logs, "consul register initialized") {
		t.Errorf("coordx 段已配却没有构造注册组件：\n%s", tail(logs, 30))
	}
	assertCorePathAlive(t, in)

	if strings.Contains(logs, "component degraded") {
		t.Logf("注：本次 coordx 在构造期即失败并降级（SDK 行为可能随版本变化）")
	} else {
		t.Logf("降级 ✓ consul 不可达时进程带病运行（惰性构造，故障推迟到注册/发现那一跳）")
	}
}

// TestStrictTurnsDegradeIntoFailFast 同一处失败，strict 翻面即退（验收案例 D2 的另一半）。
//
// 用 cache 而不是 search 做样本：只有急切构造型的组件才会在启动期产生可供裁决的
// 失败，惰性构造型在启动期压根没有失败可裁 —— 用后者做样本会得到"strict 没生效"
// 的假结论（本轮先踩到了）。
func TestStrictTurnsDegradeIntoFailFast(t *testing.T) {
	requireDegradeEnv(t)
	requireDeadPorts(t)
	in, err := launchRaw("strict-cache", deadRedis, strictCache)
	if err != nil {
		t.Fatalf("拉起进程失败：%v", err)
	}
	t.Cleanup(in.stop)

	code, err := in.waitExit(90 * time.Second)
	if err != nil {
		t.Fatalf("strict.cache=true 时进程没有退出：%v", err)
	}
	if code == 0 {
		t.Errorf("strict.cache=true，进程却以 0 退出：失败没被裁决成即退，\n%s", tail(in.logs.String(), 30))
	}
	if !strings.Contains(in.logs.String(), "init cache") {
		t.Errorf("退出原因里没有 init cache 上下文：\n%s", tail(in.logs.String(), 25))
	}
	t.Logf("strict ✓ redis 不可达 + strict.cache=true → 退出码 %d（启动即退）", code)
}

// TestDegradeTraceDisabledServesWithoutSpans 关掉调用链总闸：服务照常，Jaeger 上零 span。
//
// 为什么必须带正向对照：Jaeger 上"查不到"既可能是真没埋点，也可能是服务名写错、
// 时间窗算错、导出延迟 —— 这些都会让反向断言假绿。故先用共享实例（开埋点）
// 证明"这个查法能查到"，再用同一查法断言"关掉后查不到"。
func TestDegradeTraceDisabledServesWithoutSpans(t *testing.T) {
	c := requireJaeger(t)
	requireApp(t)

	// ① 正向对照：开埋点时同一条下单路径必须能查到 trace。
	since := time.Now()
	createOrder(t)
	tr := awaitTrace(t, c, since, http.MethodPost)
	t.Logf("正向对照 ✓ 开埋点时查到 trace %s（%d span）", tr.TraceID, len(tr.Spans))

	// 导出是分批抵达的，等对照那批落定再开窗口，避免它溢出到负向窗口里。
	time.Sleep(3 * time.Second)

	// ② 关总闸起一份进程：服务必须照常可用。
	in := launch(t, "trace-off", traceOff)
	since = time.Now()
	no := assertCorePathAlive(t, in)

	// ③ 负向断言：该窗口内 demo-monolith 不得出现新 trace。
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	traces, err := c.Fetch(ctx, traceutil.Query{
		Service: appService, Start: since, End: time.Now().Add(time.Minute), Limit: 50,
	})
	if err != nil {
		t.Fatalf("拉取 trace 失败：%v", err)
	}
	if len(traces) != 0 {
		ids := make([]string, 0, len(traces))
		for _, x := range traces {
			ids = append(ids, x.TraceID)
		}
		t.Errorf("trace.enabled=false 仍查到 %d 条 trace（%v）：总闸没关住，订单 %s 的下单请求仍在埋点",
			len(traces), ids, no)
	} else {
		t.Logf("反向断言 ✓ 关总闸后 %s 零 span（下单 %s 仍成功）", appService, no)
	}
}
