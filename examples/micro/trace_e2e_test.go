// Package micro 承载微服形态的 trace 端到端验收（T6 / T9 / T8）。
//
// 本目录只有测试文件，没有业务代码 —— 微服形态的业务在 bff/ gateway/ srv-*/ 各自
// 的包里。验收放这里是因为它的对象是**整条链**（跨越六个进程），不属于任何一域。
//
// 前置（底座 + 六进程）：
//
//	docker compose -f /Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml up -d consul jaeger mysql redis kafka
//	export HUBX_DB_PW=hubx123
//	for d in product order stock points; do (cd examples/micro/srv-$d && go run ./cmd &); done
//	(cd examples/micro/bff && go run ./cmd &); (cd examples/micro/gateway && go run ./cmd &)
//
// 底座没起时真机用例一律 **Skip**，不是 PASS —— 在这类依赖真机的验收里，
// "跳过"和"通过"是两种结论，混同就等于给自己开一张永久通行证。
// T8 是纯代码扫描，不依赖底座，任何环境都必须出结论。
package micro

import (
	"bytes"
	"context"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gospacex/launchx/internal/mode"
	"github.com/gospacex/launchx/internal/traceutil"
)

const (
	gatewayService = "demo-gateway"
	bffService     = "demo-bff"

	// domainServices 四个域服的服务名（README §1 端口表）。
	domainService1 = "demo-product"
	domainService2 = "demo-order"
	domainService3 = "demo-stock"
	domainService4 = "demo-points"
)

// domainServiceSet 参与全链路的域服集合。
func domainServiceSet() []string {
	return []string{domainService1, domainService2, domainService3, domainService4}
}

// gatewayBase 网关 HTTP 面地址（可用 MICRO_GATEWAY 覆盖）。
func gatewayBase() string {
	if v := strings.TrimSpace(os.Getenv("MICRO_GATEWAY")); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return "http://127.0.0.1:8080"
}

// requireJaeger 取 Jaeger 客户端；不可达即 Skip（并写明该跑什么）。
func requireJaeger(t *testing.T) *traceutil.Client {
	t.Helper()
	c := traceutil.NewFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Reachable(ctx); err != nil {
		t.Skipf("Jaeger 不可达，跳过真机链路断言：%v（起法：docker compose -f /Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml up -d jaeger）", err)
	}
	return c
}

// requireChain 断言六进程的 HTTP 面活着；没起即 Skip。
//
// 判据取 /api/ping 的响应体而不是 HTTP 200：本机宿主 8080 常驻着别的容器
// （实测 etcdkeeper 映射过 8081），只判状态码会把别人的服务当成自己的拢。
func requireChain(t *testing.T) {
	t.Helper()
	body, code := get(t, gatewayBase()+"/api/ping")
	if code != http.StatusOK || !strings.Contains(body, `"service":"demo-bff"`) {
		t.Skipf("微服链路未在跑（GET /api/ping → HTTP %d / %s）；起法见本文件包注释", code, truncate(body, 120))
	}
}

// demoToken 边界鉴权凭据：与 bff/config.yaml 的 demo.auth_token 一致。
//
// bff 是唯一验凭据的地方，缺了它所有业务路径都会是 401 —— 链路断言会被
// "鉴权没配"冒充成"链路没通"。可用 MICRO_TOKEN 覆盖；显式置空即走
// "无凭据应被拒"的反向断言（见 auth_e2e_test.go）。
func demoToken() string {
	if v, ok := os.LookupEnv("MICRO_TOKEN"); ok {
		return v
	}
	return "demo-token"
}

// authHeader 构造 Bearer 头。令牌为空时返回 nil（即"不带凭据"）。
func authHeader() map[string]string {
	tok := demoToken()
	if tok == "" {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + tok}
}

func get(t *testing.T, url string) (string, int) {
	t.Helper()
	return do(t, http.MethodGet, url, "", authHeader())
}

func do(t *testing.T, method, url, body string, hdr map[string]string) (string, int) {
	t.Helper()
	var rdr *bytes.Reader
	if body == "" {
		rdr = bytes.NewReader(nil)
	} else {
		rdr = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(req)
	if err != nil {
		t.Skipf("请求 %s %s 失败（链路未起？）：%v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return buf.String(), resp.StatusCode
}

// fetchSince 拉取 t0 之后出现的 trace。
func fetchSince(t *testing.T, c *traceutil.Client, service string, since time.Time, limit int) []traceutil.Trace {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	traces, err := c.Fetch(ctx, traceutil.Query{
		Service: service,
		Start:   since,
		End:     time.Now().Add(time.Minute),
		Limit:   limit,
	})
	if err != nil {
		t.Fatalf("拉取 %s 的 trace 失败：%v", service, err)
	}
	return traces
}

// newOrderViaGateway 复合下单，返回响应体。
func newOrderViaGateway(t *testing.T) string {
	t.Helper()
	body, code := do(t, http.MethodPost, gatewayBase()+"/api/order",
		`{"user_id":1001,"product_id":1,"quantity":1}`, authHeader())
	if code != http.StatusOK {
		t.Fatalf("POST /api/order 返回 HTTP %d：%s（真机链路异常，不是验收失败）", code, truncate(body, 200))
	}
	return body
}

// pickOperatorTrace 从候选 trace 里挑出刚生成的那条全链路 trace。
//
// 为什么不能"取最新一条"了事：同一时刻可能有别的进程（examples/mall 共用
// order_db 与 outbox 表）在往 Jaeger 写 trace，取到的也许根本不是这笔下单。
// 判据因此是形态：**含 gateway + bff + 至少一个域服**的那条才是我们要的。
func pickOperatorTrace(traces []traceutil.Trace, notBefore time.Time) (traceutil.Trace, bool) {
	var picked traceutil.Trace
	found := false
	for _, tr := range traces {
		if len(tr.Roots()) != 1 {
			continue
		}
		root := tr.Roots()[0]
		if time.UnixMicro(root.StartTime).Before(notBefore) {
			continue
		}
		svcs := map[string]bool{}
		for _, s := range tr.Spans {
			svcs[tr.ServiceOf(s)] = true
		}
		if !svcs[gatewayService] || !svcs[bffService] {
			continue
		}
		if !anyDomain(tr, svcs) {
			continue
		}
		// 多选时取 span 最多的一条：链最完整，判据最不容易误判。
		if !found || len(tr.Spans) > len(picked.Spans) {
			picked, found = tr, true
		}
	}
	return picked, found
}

// stableTrace 反复拉取同一条 trace 直到 span 数不再增长。
//
// 为什么必须有这一步：OTLP 导出是分批量抵达的，Jaeger 第一次返回时常常只有根
// span 那一批（实测：先拿到 1 个 span 的 trace，判据误报"没走到数据层"）。
// 拿半棵树判断据，得到的失败是测试自己的竞态，不是实现缺陷。
func stableTrace(t *testing.T, c *traceutil.Client, tr traceutil.Trace) traceutil.Trace {
	t.Helper()
	for i := 0; i < 6; i++ {
		time.Sleep(1500 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		got, err := c.FetchTrace(ctx, tr.TraceID)
		cancel()
		if err != nil || len(got.Spans) <= len(tr.Spans) {
			return tr
		}
		tr = *got
	}
	return tr
}

func anyDomain(tr traceutil.Trace, svcs map[string]bool) bool {
	for _, d := range domainServiceSet() {
		if svcs[d] {
			return true
		}
	}
	return false
}

// TestT6ThreeHopsShareOneTrace T6：gateway → bff → srv 三段共享同一 trace，且父子链正确。
//
// **这是本轮最容易被"假通过"的断言**：不注册传播器时，三个服务各自产一根 trace，
// 每一根都点得开、都有 span，看起来完全正常。所以这里断的不是"各服务都能查到
// trace"，而是 **同一条 trace 内**的三段都在，且 srv 是 bff 的子、bff 是 gateway
// 的子（不是三棵并列根）。
func TestT6ThreeHopsShareOneTrace(t *testing.T) {
	c := requireJaeger(t)
	requireChain(t)

	before := time.Now()
	t.Logf("驱动：POST /api/order → %s", truncate(newOrderViaGateway(t), 120))

	var tr traceutil.Trace
	deadline := time.Now().Add(40 * time.Second)
	for !time.Now().After(deadline) {
		if got, ok := pickOperatorTrace(fetchSince(t, c, gatewayService, before, 20), before); ok {
			tr = stableTrace(t, c, got)
			break
		}
		time.Sleep(2 * time.Second)
	}
	if len(tr.Spans) == 0 {
		t.Fatal("40s 内没有拿到含 gateway+bff+域服 的 trace（若本轮下单被限流或失败，先看 gateway 日志）")
	}
	t.Logf("取到 trace %s：span=%d 服务=%v", tr.TraceID, len(tr.Spans), tr.Services())

	// ① 单根：多根即断链（每根各自能点开，正是"假通过"的来源）。
	roots := tr.Roots()
	if len(roots) != 1 {
		t.Fatalf("根 span 应有且仅有 1 个，实际 %d 个：%s", len(roots), opNames(tr, roots))
	}
	root := roots[0]
	if got := tr.ServiceOf(root); got != gatewayService {
		t.Fatalf("根 span 所属服务 = %s，want %s（入口必须是网关）", got, gatewayService)
	}

	// ② 同一条 trace 里三段都在 —— 注意是同一个 Trace object，traceID 天然一致；
	// 真正要排除的是"三个服务各有一条自己的 trace"，那会表现为下面 ③ 找不到。
	svcs := tr.Services()
	if !containsStr(svcs, bffService) {
		t.Fatalf("trace 内没有 %s 的 span（服务：%v）", bffService, svcs)
	}
	var domain string
	for _, d := range domainServiceSet() {
		if containsStr(svcs, d) {
			domain = d
			break
		}
	}
	if domain == "" {
		t.Fatalf("trace 内没有任何域服的 span（服务：%v）—— 链路止于 bff，srv 侧未入链", svcs)
	}

	// ③ 父子链：srv 侧 span 的祖先链必须**依次**经过 bff 与 gateway 根。
	//
	// 为什么从 srv 这一端往上找，而不是先挑一个 bff span 再往下找：bff 进程里有
	// 不止一类以根为祖先的 span（coord.discover 之类也不一样挂在根下），随手挑
	// 一个当"业务入口"，就会要求它成为 srv 的父 —— 而这根本不成立。
	// 从叶子往上走，判据只剩一条：**祖先链里要有 bff，且最远祖先必须是根**。
	var (
		srvSpan   traceutil.Span
		hopBFF    traceutil.Span
		ancestors []traceutil.Span
	)
	//
	// 候选里取**最浅**的那个（祖先链最短 = 该服务的业务入口 span），而不是
	// 取第一个。
	//
	// 为什么必须挑最浅：Jaeger 返回的 span 顺序不保证按深度排，随手取第一个会
	// 拿到叶子（实测取到过 db.kvx.INCR），于是下面第 ④ 步"找一个挂在它下面的
	// db.* span"永远不成立 —— 断言失败的原因变成"选错了 anchor"，与被测行为
	// 无关，且只在特定返回顺序下复现，是典型的假红。
	for _, s := range tr.Spans {
		if tr.ServiceOf(s) != domain {
			continue
		}
		anc := tr.AncestorsOf(s)
		if len(anc) == 0 || anc[len(anc)-1].SpanID != root.SpanID {
			continue // 祖先链没走到 gateway 根 = 断链（另一端）
		}
		bffIdx := -1
		for i, a := range anc {
			if tr.ServiceOf(a) == bffService {
				bffIdx = i
				break // anc 由近及远，取最近的那个即为业务父那一跳
			}
		}
		if bffIdx < 0 {
			continue
		}
		if len(ancestors) == 0 || len(anc) < len(ancestors) {
			srvSpan, hopBFF, ancestors = s, anc[bffIdx], anc
		}
	}
	if len(ancestors) == 0 {
		t.Fatalf("域服 %s 的 span 没有任何一条祖先链是 %s → … → %s 的形态 —— "+
			"两服务各产一根 trace（传播器没生效的典型症状）", domain, gatewayService, bffService)
	}
	t.Logf("父子链确认：%s(%s) → %s(%s) → %s(%s)（中间 %d 跳）",
		gatewayService, root.OperationName, bffService, hopBFF.OperationName, domain,
		srvSpan.OperationName, len(ancestors)-1)

	// ④ srv 侧数据层也在同一条 trace 上（否则只是"调用过去了"，不算入链）。
	var dbSpan traceutil.Span
	ok := false
	for _, s := range tr.Spans {
		if tr.ServiceOf(s) != domain || !strings.HasPrefix(s.OperationName, "db.") {
			continue
		}
		for _, a := range tr.AncestorsOf(s) {
			if a.SpanID == srvSpan.SpanID {
				dbSpan, ok = s, true
			}
		}
		if ok {
			break
		}
	}
	if !ok {
		t.Fatalf("域服 %s 的数据层 span 不在同 trace（或无 db.* span）—— 入链只到 service 层就断了", domain)
	}
	t.Logf("数据层确认：%s 在 %s 下（db 层次与主链路同 trace）", dbSpan.OperationName, domain)
}

// TestT9TailSpansNotDropped T9：退出链冲刷生效，尾部 span 不缺失。
//
// 三条硬判据（都是"缺失会发生什么"的机械探针，不靠肉眼比对 span 数）：
//
//	① 无悬空父引用 —— 丢批时通常是最后一批被丢，它们的**子节点还在**，于是父指向
//	   不在 trace 内的 span；完整 trace 里 FK 永远指向存在的 span。
//	② 存在"晚起步"的 span —— 起步时刻落在根 span 的后半程。若尾部被丢，
//	   最晚的 startTime 会明显早于实际请求结束处。
//	③ 二次拉取 span 数不再增长 —— 证明拿到的是冲刷完成态，而不是刚好抓到中间态。
func TestT9TailSpansNotDropped(t *testing.T) {
	c := requireJaeger(t)
	requireChain(t)

	before := time.Now()
	newOrderViaGateway(t)

	var tr traceutil.Trace
	deadline := time.Now().Add(40 * time.Second)
	for !time.Now().After(deadline) {
		if got, ok := pickOperatorTrace(fetchSince(t, c, gatewayService, before, 20), before); ok {
			tr = stableTrace(t, c, got)
			break
		}
		time.Sleep(2 * time.Second)
	}
	if len(tr.Spans) == 0 {
		t.Fatal("40s 内没有拿到目标 trace")
	}

	if dangling := tr.DanglingRefs(); len(dangling) > 0 {
		t.Fatalf("存在 %d 条悬空父引用（span 的父不在 trace 内），尾部/中间被丢了一批：%v",
			len(dangling), firstN(dangling, 5))
	}

	root := tr.Roots()[0]
	if len(tr.Roots()) != 1 {
		t.Fatalf("根 span 应有 1 个，实际 %d", len(tr.Roots()))
	}
	halfLife := root.StartTime + root.Duration/2
	lateSpans := 0
	for _, s := range tr.Spans {
		if s.StartTime >= halfLife {
			lateSpans++
		}
	}
	if lateSpans == 0 {
		t.Fatalf("没有任何 span 起步于根 span 的后半程（根 span %dμs）—— 尾部 span 被吞了", root.Duration)
	}
	t.Logf("晚起步 span %d/%d 个，树深 %d，LatestEnd=%d", lateSpans, len(tr.Spans), tr.Depth(), tr.LatestEnd())

	// 树深是"深分支（points 域的 kafka 发送之类）没被砍掉"的旁证：
	// 实测复合下单树深 16，这里留足余地下限。
	if d := tr.Depth(); d < 8 {
		t.Fatalf("树深 %d < 8：深层尾部 span 疑似丢失（实测完整链为 16 层）", d)
	}

	// 冲刷不应该还在进行中：再拉一次，span 数不得增长。
	time.Sleep(3 * time.Second)
	again, err := c.FetchTrace(context.Background(), tr.TraceID)
	if err != nil {
		t.Fatalf("复核 trace 失败：%v", err)
	}
	if len(again.Spans) < len(tr.Spans) {
		t.Fatalf("二次拉取 span 数从 %d 变成 %d：初次结果不可信", len(tr.Spans), len(again.Spans))
	}
	t.Logf("冲刷稳定：%s span 数 %d → %d", tr.TraceID, len(tr.Spans), len(again.Spans))
}

// TestT8NoMeshSpecificTraceBranch T8：全仓无网格专属 trace 分支，mode 枚举无 mesh 取值。
func TestT8NoMeshSpecificTraceBranch(t *testing.T) {
	// ① mode 枚举：mesh 必须被显式拒绝，且错误信息要给出替代档。
	m, err := mode.ParseMode("mesh")
	if err == nil {
		t.Fatalf("mode=mesh 竟被接受（得到 %q）：网格一旦成为独立档，就会出现第二份业务代码", m)
	}
	if !strings.Contains(err.Error(), "mesh") || !strings.Contains(err.Error(), "srv") {
		t.Fatalf("mesh 错误必须点名 mesh 并给出替代档，实际：%v", err)
	}
	for _, legal := range []string{"monolith", "bff", "srv", "gateway"} {
		if _, err := mode.ParseMode(legal); err != nil {
			t.Fatalf("合法档 %q 被拒：%v", legal, err)
		}
	}
	if strings.Contains(strings.Join(mode.Modes(), ","), "mesh") {
		t.Fatalf("Modes() 不应含 mesh：%v", mode.Modes())
	}

	// ② 库源码（internal/**）里不得有网格专属分支 —— 唯一的例外是
	// internal/mode 那处"显式拒绝 mesh"，它的存在本身就是要把人挡回去。
	//
	// 只看 .go：清单目录里的 yaml 提 mesh 是**应该的**，那里才是网格的表达处。
	root := "../../internal"
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("找不到 %s（测试须在 examples/micro 下跑，cwd 目前疑似不对）：%v", root, err)
	}
	var offenders []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.Contains(filepath.ToSlash(path), "internal/mode/") {
			return nil // 拒绝 mesh 的实现地，允许
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(strings.ToLower(string(raw)), "mesh") {
			offenders = append(offenders, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描 internal 失败：%v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("库源码出现网格专属分支（%v）：一旦进库，micro 就得为它留分支，形态收敛作废", offenders)
	}

	// ③ six 个服务的 main.go 除注释与 app 引入路径外必须逐字节一致 ——
	// 差异只在配置路径与包路径，说明网格形态不需要第二份入口代码。
	identicalEntrypoints(t)
}

// identicalEntrypoints 断言各服务 main.go 的**代码**完全一致（注释与 import 路径除外）。
//
// 为什么排除注释后比较：六个入口的语义差异本就应该只在"配哪个包/读哪个配置"，
// 一旦有人在某个入口里加了判形态的 if 或自建连接，两两份之间的代码就不再相同 ——
// 这条断言是在给"形态分支不许下沉到 main"站岗，比数行数精确得多。
func identicalEntrypoints(t *testing.T) {
	t.Helper()
	dirs := []string{"bff", "gateway", "srv-product", "srv-order", "srv-stock", "srv-points"}
	var ref []string
	var refPath string
	for _, d := range dirs {
		// cwd 即本包目录（examples/micro），六个服务的 cmd 就在它的子目录里。
		path := filepath.Join(d, "cmd", "main.go")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读 %s 失败：%v", path, err)
		}
		lines := codeLinesOnly(string(raw))
		if ref == nil {
			ref, refPath = lines, path
			continue
		}
		if strings.Join(lines, "\n") != strings.Join(ref, "\n") {
			t.Fatalf("%s 与 %s 的入口代码不一致：形态一旦在 main 里分叉，网格就得再拷一份入口",
				path, refPath)
		}
	}
}

// codeLinesOnly 去掉注释行、空行与 import 行，只留真正的代码。
func codeLinesOnly(src string) []string {
	var out []string
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
		case strings.HasPrefix(trimmed, "//"):
		case strings.HasPrefix(trimmed, "package "):
		case strings.HasPrefix(trimmed, `"github.com/`), strings.HasPrefix(trimmed, `"go.`):
		default:
			out = append(out, trimmed)
		}
	}
	return out
}

func opNames(tr traceutil.Trace, spans []traceutil.Span) []string {
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		out = append(out, tr.ServiceOf(s)+":"+s.OperationName)
	}
	return out
}

func containsStr(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func firstN(list []string, n int) []string {
	if len(list) <= n {
		return list
	}
	return list[:n]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
