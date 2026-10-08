// Package monolith 承载单体形态的 trace 端到端验收（T2 / T3 / T4 / T5）。
//
// 本目录只有测试文件：业务代码在 app/ cmd/ internal/ 里，验收对象却是"一次 HTTP
// 请求在 Jaeger 上长什么样"，放在任何一个业务包里都会变成"某个包的自测"。
//
// 前置（底座 + 进程）：
//
//	bash examples/monolith/scripts/bring-up.sh        # 建表 + 授权 + 种子用户（幂等）
//	export HUBX_DB_PW=hubx123
//	cd examples/monolith && go run ./cmd &            # 监听 config.yaml 的 webx.addr
//
// ⚠️ 库名冲突（实测）：三个 demo 默认共用 `order_db`，而 examples/mall/scripts/schema.sql
// 建的 orders 表**没有 item / qty / amount 列**，monolith 起在它上面会报
// "Unknown column 'item' in 'field list'"。同机跑过 mall/micro 之后，单体要用自己的
// schema：`MYSQL_DB=order_mono bash scripts/bring-up.sh` + 配置副本里改 DSN 的库名。
// 这是 demo 之间的库名串扰，不是 launchx 缺陷，但必须知道才能复现本文件的用例。
//
// 进程没起 / Jaeger 不可达时真机用例一律 **Skip**，而不是 PASS —— 在这类依赖真机
// 的验收里两者是不同结论，混同就会拿到一张永久通行证。
// 每条 Skip 都会写明缺什么，方便下次直接补。
package monolith

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gospacex/launchx/internal/traceutil"
)

const appService = "demo-monolith"

// appBase 单体 HTTP 面地址（可用 MONOLITH_BASE 覆盖 —— 本机 8080 常被别的
// demo 占着，改端口是这个 demo 唯一需要改的地方）。
func appBase() string {
	if v := strings.TrimSpace(os.Getenv("MONOLITH_BASE")); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return "http://127.0.0.1:8080"
}

func requireJaeger(t *testing.T) *traceutil.Client {
	t.Helper()
	c := traceutil.NewFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Reachable(ctx); err != nil {
		t.Skipf("Jaeger 不可达，跳过：%v", err)
	}
	return c
}

// requireApp 确认单体进程在跑。
//
// 判据取"list 接口返回的是订单数组"，而不是 HTTP 200：宿主 8080 上常驻着别的容器
// （实测微服 gateway 也占 8080），只判状态码会把别人的服务当成自己的。
func requireApp(t *testing.T) {
	t.Helper()
	body, code := request(t, http.MethodGet, appBase()+"/orders", "", nil)
	if code != http.StatusOK {
		t.Skipf("单体进程未在跑（GET /orders → HTTP %d；%s）", code, truncate(body, 120))
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Skipf("%s 上跑的不是本 demo（GET /orders 返回的不是订单数组：%s）",
			appBase(), truncate(body, 160))
	}
}

func request(t *testing.T, method, url, body string, _ map[string]string) (string, int) {
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
	client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(req)
	if err != nil {
		t.Skipf("请求 %s %s 失败（进程未起？）：%v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return buf.String(), resp.StatusCode
}

// createOrder 下单并返回 order_no。
func createOrder(t *testing.T) string {
	t.Helper()
	body, code := request(t, http.MethodPost, appBase()+"/orders",
		fmt.Sprintf(`{"user_id":1,"item":"t-span-%d","sku":"SKU-T","qty":1,"unit_price":9.9}`, time.Now().UnixNano()), nil)
	if code != http.StatusCreated {
		t.Fatalf("POST /orders 返回 HTTP %d：%s（进程异常，不是验收失败）", code, truncate(body, 200))
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("下单响应解析失败：%v", err)
	}
	no, _ := got["order_no"].(string)
	if no == "" {
		t.Fatalf("下单响应里没有 order_no：%s", truncate(body, 200))
	}
	return no
}

// awaitTrace 等一跳新 trace：给定 HTTP 方法 token（根 span 的 operationName 形如
// "POST /orders"），取 **t0 之后最新**的那一跳。
//
// 为什么按方法 token 过滤而不是取最新：本机还有别的 demo（micro 侧共用 mysql/
// kafka）在往 Jaeger 写 trace，取最新可能抓到别人的东西，然后拿着别人的 span
// 判自己的验收 —— 这种"绿"比红危险得多。
func awaitTrace(t *testing.T, c *traceutil.Client, since time.Time, methodToken string) traceutil.Trace {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !time.Now().After(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		traces, err := c.Fetch(ctx, traceutil.Query{
			Service: appService, Start: since, End: time.Now().Add(time.Minute), Limit: 30,
		})
		cancel()
		if err != nil {
			t.Fatalf("拉取 trace 失败：%v", err)
		}
		var best traceutil.Trace
		for _, tr := range traces {
			if len(tr.Roots()) != 1 {
				continue
			}
			root := tr.Roots()[0]
			if !strings.Contains(root.OperationName, methodToken) {
				continue
			}
			// 只取至少 3 个 span 的候选：单 span 的 trace 是"刚写到一半"
			// （OTLP 批量导出按批抵达），拿它判断据会误判成"没走到数据层"。
			if len(tr.Spans) < 3 {
				continue
			}
			if best.Spans == nil || root.StartTime > best.Roots()[0].StartTime {
				best = tr
			}
		}
		if best.Spans != nil {
			return stableTrace(t, c, best)
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("30s 内没有出现在 %q 之后、根 span 含 %q 的 trace（Jaeger 导出有 ~5s 延迟）",
		since.Format(time.RFC3339), methodToken)
	return traceutil.Trace{}
}

// stableTrace 反复拉取同一条 trace，直到 span 数不再增长。
//
// 为什么必须有这一步：OTLP 导出是**分批量**抵达的，Jaeger 第一次返回时常常只有
// 根 span 那一批。直接拿第一批判断据，得到的失败是"span 没到"而不是"span 没产生"
// —— 前者是测试自己的竞态，后者才是实现缺陷，混在一起会浪费大量排查时间。
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

// sqlSpans 取指定 SQL 类别的 span（db.create / db.query / db.update / db.delete）。
func sqlSpans(tr traceutil.Trace, opPrefix string) []traceutil.Span {
	var out []traceutil.Span
	for _, s := range tr.Spans {
		if s.OperationName == opPrefix {
			out = append(out, s)
		}
	}
	return out
}

// TestT2SingleTableCRUDCarriesSQL 断言 T2：单表 CRUD 四类 span 都带完整 SQL 与表名。
func TestT2SingleTableCRUDCarriesSQL(t *testing.T) {
	c := requireJaeger(t)
	requireApp(t)

	cases := []struct {
		name    string
		method  string
		url     func(orderNo string) string
		body    string
		dbOp    string
		prepare bool // 需要先下一单
	}{
		{name: "INSERT", method: http.MethodPost, dbOp: "db.create", prepare: true,
			url:  func(string) string { return appBase() + "/orders" },
			body: fmt.Sprintf(`{"user_id":1,"item":"t2-ins-%d","qty":1,"unit_price":9.9}`, time.Now().UnixNano())},
		{name: "SELECT", method: http.MethodGet, dbOp: "db.query", prepare: true,
			url: func(no string) string { return appBase() + "/orders/" + no }},
		{name: "UPDATE", method: http.MethodPut, dbOp: "db.update", prepare: true,
			url:  func(no string) string { return appBase() + "/orders/" + no },
			body: `{"status":"paid"}`},
		{name: "DELETE", method: http.MethodDelete, dbOp: "db.delete", prepare: true,
			url: func(no string) string { return appBase() + "/orders/" + no }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var orderNo string
			if tc.prepare {
				orderNo = createOrder(t)
			}
			since := time.Now()
			body, code := request(t, tc.method, tc.url(orderNo), tc.body, nil)
			if code >= 500 {
				t.Fatalf("%s %s 返回 HTTP %d：%s（进程异常，非验收失败）", tc.method, tc.url(orderNo), code, truncate(body, 160))
			}
			tr := awaitTrace(t, c, since, tc.method)
			t.Logf("trace %s span=%d ops=%v", tr.TraceID, len(tr.Spans), tr.OperationNames())

			got := sqlSpans(tr, tc.dbOp)
			if len(got) == 0 {
				t.Fatalf("trace %s 内没有 %s span（ops=%v）—— 该操作没有走到数据层", tr.TraceID, tc.dbOp, tr.OperationNames())
			}
			for _, s := range got {
				text := tr.StringTag(s, "db.query.text")
				table := tr.StringTag(s, "db.sql.table")
				if len(text) < 8 {
					t.Fatalf("%s 的 db.query.text 为空/过短（%q）：空壳 span 会让 SQL 排查无从下手", s.OperationName, text)
				}
				if table == "" {
					t.Fatalf("%s 缺 db.sql.table（SQL=%q）：没有表名就无法按表聚合慢查询", s.OperationName, truncate(text, 60))
				}
				t.Logf("%s ✓ text=%q table=%q", s.OperationName, truncate(text, 64), table)
			}
		})
	}
}

// TestT3MultiTableSpansInOneTrace T3：多表操作入链 —— JOIN 文本可见、跨表写为兄弟 span。
//
// 第三条要求（回滚时相关 span 标 Error）见下方 TestT3RollbackMarksErrorSpans。
func TestT3MultiTableSpansInOneTrace(t *testing.T) {
	c := requireJaeger(t)
	requireApp(t)

	orderNo := createOrder(t)

	// ① JOIN 查询：db.query.text 必须含 join（大写与否都认）。
	since := time.Now()
	request(t, http.MethodGet, appBase()+"/orders/"+orderNo+"/join", "", nil)
	tr := awaitTrace(t, c, since, http.MethodGet)
	var joined bool
	for _, s := range sqlSpans(tr, "db.query") {
		text := strings.ToLower(tr.StringTag(s, "db.query.text"))
		if strings.Contains(text, "join") {
			joined = true
			t.Logf("JOIN span ✓ %s", truncate(tr.StringTag(s, "db.query.text"), 96))
		}
	}
	if !joined {
		t.Fatalf("trace %s 的 db.query 里没有 JOIN 文本（ops=%v）—— 连表被拆成单表查询或文本丢失都会这样",
			tr.TraceID, tr.OperationNames())
	}

	// ② 跨表事务：下单写 orders + order_items 两条 INSERT，必须为**同一父**的兄弟 span
	// （同父 = 同一个事务内；散在不同父下说明它们各自开事务，"跨表事务"不成立）。
	since = time.Now()
	newNo := createOrder(t)
	tr = awaitTrace(t, c, since, http.MethodPost)
	creates := sqlSpans(tr, "db.create")
	if len(creates) < 2 {
		t.Fatalf("下单应有 ≥2 条 db.create（orders + order_items），实际 %d", len(creates))
	}
	parents := map[string][]string{}
	for _, s := range creates {
		p := traceutil.Span{}
		if got, ok := tr.ParentOf(s); ok {
			p = got
		}
		parents[p.SpanID] = append(parents[p.SpanID], tr.StringTag(s, "db.sql.table"))
	}
	if len(parents) != 1 {
		t.Fatalf("多条 INSERT 落在 %d 个不同父 span 下（%v）：不是同一个事务", len(parents), parents)
	}
	var tables []string
	for id := range parents {
		tables = parents[id]
		if id == "" {
			t.Fatal("跨表 INSERT 没有父 span：它们应挂在同一个 repository 事务 span 下")
		}
	}
	t.Logf("跨表事务 ✓ 订单 %s 的同事务 INSERT：%v", newNo, tables)
}

// TestT4ComponentSpansShareTrace T4：同一条 trace 里 cache / mq / ES / Mongo 四类 span 共存。
//
// ⚠️ 与计划的一处诚实偏差：计划要求"同一请求内四类共存"，但本 demo **没有任何一条
// 业务路径同时触达四类** —— service.Create 扇出到 mq / ES / Mongo，cache 的读取与失效
// 分别在 Get / Update 两条路径上（见 internal/service/order.go 的 Create 与 UpdateStatus）。
// 硬凑一条"看起来四合一"的请求会让断言失真，故按实际拓扑拆成两段：
// 一段验 Create 的 mq+ES+Mongo 三类共 trace 且同父，一段验 Update 里的 cache。
func TestT4ComponentSpansShareTrace(t *testing.T) {
	c := requireJaeger(t)
	requireApp(t)

	// ① Create：mq / ES / Mongo 三类必须在同一条 trace，且都是业务 span 的后代。
	since := time.Now()
	orderNo := createOrder(t)
	tr := awaitTrace(t, c, since, http.MethodPost)
	bizSpan, ok := findSpan(tr, "service.order.CreateOrder")
	if !ok {
		t.Fatalf("trace %s 内没有 service.order.CreateOrder 业务 span（ops=%v）", tr.TraceID, tr.OperationNames())
	}
	for _, kind := range []struct {
		label  string
		prefix string
	}{
		{"mq", "messaging.kafka.send"},
		{"elasticsearch", "db.elasticsearch."},
		{"mongodb", "db.mongodb."},
	} {
		spans := tr.SpansWith(kind.prefix)
		if len(spans) == 0 {
			t.Fatalf("trace %s 内没有 %s 的 span（%q）：%s 组件没有入链，或该系统当前不可用",
				tr.TraceID, kind.label, kind.prefix, kind.label)
		}
		found := false
		for _, s := range spans {
			for _, a := range tr.AncestorsOf(s) {
				if a.SpanID == bizSpan.SpanID {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("%s span 不是 %s 的后代：它是独立的一棵树，同 trace 与否在这里不成立",
				kind.label, bizSpan.OperationName)
		}
		t.Logf("%s ✓ %v 挂在 %s 下", kind.label, opNamesOf(tr, spans), bizSpan.OperationName)
	}

	// ② Update：cache 那一类另起一条路径验（.Create 不走缓存，是 demo 的拓扑事实）。
	since = time.Now()
	updateNo := createOrder(t)
	request(t, http.MethodPut, appBase()+"/orders/"+updateNo, `{"status":"paid"}`, nil)
	tr = awaitTrace(t, c, since, http.MethodPut)
	bizSpan, ok = findSpan(tr, "service.order.UpdateOrder")
	if !ok {
		t.Fatalf("Update 的 trace %s 内没有 service.UpdateOrder（ops=%v）", tr.TraceID, tr.OperationNames())
	}
	cacheSpans := tr.SpansWith("db.redis.")
	if len(cacheSpans) == 0 {
		t.Fatalf("Update 的 trace %s 内没有 cache span（db.redis.*）—— 更新后必须失效缓存，否则拿到旧值",
			tr.TraceID)
	}
	for _, s := range cacheSpans {
		var under bool
		for _, a := range tr.AncestorsOf(s) {
			if a.SpanID == bizSpan.SpanID {
				under = true
			}
		}
		if !under {
			t.Fatalf("cache span %s 不是 %s 的后代", s.OperationName, bizSpan.OperationName)
		}
	}
	t.Logf("cache ✓ %v 挂在 %s 下（订单 %s）", opNamesOf(tr, cacheSpans), bizSpan.OperationName, orderNo)
}

// TestT3RollbackMarksErrorSpans T3 第三条：跨表事务回滚时相关 span 标 Error。
//
// 触发方式必须是"经公共 API 真回滚"，而不是直接连库造 —— 后者回滚了也只能证明
// gorm 会回滚，证明不了**这一跳的失败有没有进链路**。这里喂一组会撞
// `amount decimal(10,2)` 上界的数量/单价，让 service.Create 的同事务里第二句
// INSERT 报错（HTTP 500，订单与明细一同回滚）。
func TestT3RollbackMarksErrorSpans(t *testing.T) {
	c := requireJaeger(t)
	requireApp(t)

	since := time.Now()
	body, code := request(t, http.MethodPost, appBase()+"/orders",
		`{"user_id":1,"item":"t3-rollback","qty":1000000000,"unit_price":99999999}`, nil)
	if code != http.StatusInternalServerError {
		t.Fatalf("预期 HTTP 500（金额越界触发回滚），实际 %d：%s —— 若实现已修，本用例的触发方式需同步调整",
			code, truncate(body, 160))
	}

	tr := awaitTrace(t, c, since, http.MethodPost)
	t.Logf("回滚 trace %s：ops=%v", tr.TraceID, tr.OperationNames())

	var (
		errDB   bool
		errRepo bool
		errBiz  bool
	)
	for _, s := range tr.Spans {
		if !tr.IsError(s) {
			continue
		}
		switch {
		case strings.HasPrefix(s.OperationName, "db."):
			errDB = true
		case strings.HasPrefix(s.OperationName, "repository."):
			errRepo = true
		case strings.HasPrefix(s.OperationName, "service."), strings.HasPrefix(s.OperationName, "handler."):
			errBiz = true
		}
	}
	// ① 回滚的事实必须落在**数据层 span** 上 —— 只有业务层标错、SQL span 却绿着，
	// 排查时会误判成"这条 SQL 是成功的，错在别处"。
	if !errDB {
		t.Fatalf("回滚的 trace %s 内没有任何带 error 标记的 db.* span（ops=%v）", tr.TraceID, tr.OperationNames())
	}
	// ② 事务边界那一层也要标错：repository 是事务持有者，它干净就等于"没人认领这次失败"。
	if !errRepo {
		t.Fatalf("回滚的 trace %s 内没有带 error 标记的 repository.* span（ops=%v）", tr.TraceID, tr.OperationNames())
	}
	if !errBiz {
		t.Fatalf("回滚的 trace %s 内没有带 error 标记的 service/handler span（ops=%v）", tr.TraceID, tr.OperationNames())
	}
	t.Logf("T3 回滚 ✓ 数据层 / repository / 业务层均标 Error（trace %s）", tr.TraceID)
}

// TestT5MonolithSpanTreeShape T5：span 树形态 —— 入口 → 业务层 → 数据层，无孤儿。
func TestT5MonolithSpanTreeShape(t *testing.T) {
	c := requireJaeger(t)
	requireApp(t)

	since := time.Now()
	request(t, http.MethodPost, appBase()+"/orders",
		fmt.Sprintf(`{"user_id":1,"item":"t5-%d","qty":1,"unit_price":9.9}`, time.Now().UnixNano()), nil)
	tr := awaitTrace(t, c, since, http.MethodPost)

	// ① 单根：多根即断链（每个安装都能点开，但那是两棵树）。
	roots := tr.Roots()
	if len(roots) != 1 {
		t.Fatalf("根 span 应有且仅有 1 个，实际 %d：%v", len(roots), opNamesOf(tr, roots))
	}
	root := roots[0]

	// ② 树深 ≥ 3（入口 → 业务层 → 数据层）。
	if d := tr.Depth(); d < 3 {
		t.Fatalf("树深 %d < 3（ops=%v）：数据层没有挂在业务层下", d, tr.OperationNames())
	}

	// ③ 无孤儿：每个非根 span 的父都必须在这条 trace 里。
	if dangling := tr.DanglingRefs(); len(dangling) > 0 {
		t.Fatalf("存在 %d 条悬空父引用：%v", len(dangling), firstN(dangling, 5))
	}

	// ④ 数据层 span 的祖先链必须含入口 span（数据层不是独立于入口的第二棵树）。
	dbs := tr.SpansWith("db.create", "db.query", "db.update", "db.delete")
	if len(dbs) == 0 {
		t.Fatalf("trace %s 内没有任何 SQL span（ops=%v）", tr.TraceID, tr.OperationNames())
	}
	for _, s := range dbs {
		anc := tr.AncestorsOf(s)
		if len(anc) == 0 {
			t.Fatalf("%s 没有父：数据层 span 直接成了根（业务层没把它包进来）", s.OperationName)
		}
		var reachesEntry bool
		for _, a := range anc {
			if a.SpanID == root.SpanID {
				reachesEntry = true
			}
		}
		if !reachesEntry {
			t.Fatalf("%s 的祖先链不含入口 span %q（祖先：%v）—— 这是孤儿子树",
				s.OperationName, root.OperationName, opNamesOf(tr, anc))
		}
	}
	t.Logf("T5 ✓ trace %s：根=%q 树深=%d 数据层 %d 个全部可达入口",
		tr.TraceID, root.OperationName, tr.Depth(), len(dbs))
}

func findSpan(tr traceutil.Trace, opName string) (traceutil.Span, bool) {
	for _, s := range tr.Spans {
		if s.OperationName == opName {
			return s, true
		}
	}
	return traceutil.Span{}, false
}

func opNamesOf(tr traceutil.Trace, spans []traceutil.Span) []string {
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		out = append(out, s.OperationName)
	}
	return out
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
