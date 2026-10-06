package traceutil

import (
	"encoding/json"
	"testing"
)

// fixtureTrace 一条三层的最小根系 trace，字段口径与 Jaeger REST **实测一致**：
// tags 是三元组列表（不是 attributes）、服务名只在 processes 里。
//
// 这条 fixture 同时是「别照 schema 想象写判据」的护栏：只要有人按
// `.spans[].process.serviceName` 或 `.attributes[]` 改 retrieval，这些用例会红。
const fixtureTrace = `{
  "traceID": "e91626f74db710d00f",
  "spans": [
    {
      "traceID": "e91626f74db710d00f",
      "spanID": "aaaaaaaaaaaaaaaa",
      "operationName": "POST /api/order",
      "references": [],
      "startTime": 1000,
      "duration": 1000,
      "tags": [{"key": "http.method", "type": "string", "value": "POST"}],
      "processID": "p1"
    },
    {
      "traceID": "e91626f74db710d00f",
      "spanID": "bbbbbbbbbbbbbbbb",
      "operationName": "handler.PlaceOrder",
      "references": [{"refType": "CHILD_OF", "traceID": "e91626f74db710d00f", "spanID": "aaaaaaaaaaaaaaaa"}],
      "startTime": 1010,
      "duration": 900,
      "tags": [],
      "processID": "p2"
    },
    {
      "traceID": "e91626f74db710d00f",
      "spanID": "cccccccccccccccc",
      "operationName": "db.query",
      "references": [{"refType": "CHILD_OF", "traceID": "e91626f74db710d00f", "spanID": "bbbbbbbbbbbbbbbb"}],
      "startTime": 1050,
      "duration": 100,
      "tags": [
        {"key": "db.query.text", "type": "string", "value": "SELECT * FROM orders WHERE id = 1"},
        {"key": "db.sql.table", "type": "string", "value": "orders"},
        {"key": "db.rows_affected", "type": "int64", "value": 1}
      ],
      "processID": "p3"
    },
    {
      "traceID": "e91626f74db710d00f",
      "spanID": "dddddddddddddddd",
      "operationName": "service.Failed",
      "references": [{"refType": "CHILD_OF", "traceID": "e91626f74db710d00f", "spanID": "aaaaaaaaaaaaaaaa"}],
      "startTime": 1200,
      "duration": 50,
      "tags": [{"key": "error", "type": "bool", "value": true}],
      "processID": "p2"
    }
  ],
  "processes": {
    "p1": {"serviceName": "demo-gateway", "tags": []},
    "p2": {"serviceName": "demo-bff", "tags": []},
    "p3": {"serviceName": "demo-order", "tags": []}
  }
}`

// parseTrace 反序列化 fixture；失败直接终止（fixture 是本包的输入契约）。
func parseTrace(t *testing.T, raw string) Trace {
	t.Helper()
	var got struct {
		Data []Trace `json:"data"`
	}
	// 走一遍真实载荷的形状：Jaeger 的 /api/traces 返回 {"data":[...]}。
	if err := json.Unmarshal([]byte(`{"data":[`+raw+`]}`), &got); err != nil {
		t.Fatalf("fixture 解析失败：%v", err)
	}
	if len(got.Data) != 1 {
		t.Fatalf("fixture 应有 1 条 trace，实际 %d", len(got.Data))
	}
	return got.Data[0]
}

func spanByID(t *testing.T, tr Trace, id string) Span {
	t.Helper()
	for _, s := range tr.Spans {
		if s.SpanID == id {
			return s
		}
	}
	t.Fatalf("fixture 里没有 span %s", id)
	return Span{}
}

// TestServiceComesFromProcesses 断言服务名从 processes 表取，而不是 span 自带字段。
func TestServiceComesFromProcesses(t *testing.T) {
	tr := parseTrace(t, fixtureTrace)
	if got := tr.ServiceOf(spanByID(t, tr, "aaaaaaaaaaaaaaaa")); got != "demo-gateway" {
		t.Fatalf("根 span 服务名 = %q，want demo-gateway", got)
	}
	if got := tr.ServiceOf(spanByID(t, tr, "cccccccccccccccc")); got != "demo-order" {
		t.Fatalf("数据层 span 服务名 = %q，want demo-order", got)
	}
	// 进程表缺失时必须给出可辨识的占位，而不是空串（空串会把断链藏起来）
	tr.Processes = nil
	if got := tr.ServiceOf(spanByID(t, tr, "aaaaaaaaaaaaaaaa")); got != "<unknown>" {
		t.Fatalf("进程表缺失时应回落 <unknown>，实际 %q", got)
	}
}

// TestSingleRoot 断言恰好一个根 span（多根 = 断链形态）。
func TestSingleRoot(t *testing.T) {
	tr := parseTrace(t, fixtureTrace)
	if roots := tr.Roots(); len(roots) != 1 {
		t.Fatalf("根 span 应恰好 1 个，实际 %d", len(roots))
	}
}

// TestDepthCountsLongestChain 断言树深按最长链算，不是按 span 数算。
func TestDepthCountsLongestChain(t *testing.T) {
	tr := parseTrace(t, fixtureTrace)
	if got := tr.Depth(); got != 3 {
		t.Fatalf("树深 = %d，want 3（root → handler → db.query）", got)
	}
}

// TestAncestorsOfOrderedNearestFirst 断言祖先链由近及远且止于根。
func TestAncestorsOfOrderedNearestFirst(t *testing.T) {
	tr := parseTrace(t, fixtureTrace)
	got := tr.AncestorsOf(spanByID(t, tr, "cccccccccccccccc"))
	if len(got) != 2 {
		t.Fatalf("祖先数 = %d，want 2", len(got))
	}
	if got[0].SpanID != "bbbbbbbbbbbbbbbb" {
		t.Fatalf("最近祖先 = %s，want bbbbbbbbbbbbbbbb", got[0].SpanID)
	}
	if got[1].SpanID != "aaaaaaaaaaaaaaaa" {
		t.Fatalf("最远祖先 = %s，want aaaaaaaaaaaaaaaa（须含入口 span）", got[1].SpanID)
	}
}

// TestDanglingRefsDetectsMissingMiddle 断言能抓出「父不在 trace 内」的缺口 ——
// 这是导出器丢批时最典型的形态。
func TestDanglingRefsDetectsMissingMiddle(t *testing.T) {
	tr := parseTrace(t, fixtureTrace)
	if dangling := tr.DanglingRefs(); len(dangling) != 0 {
		t.Fatalf("完整 trace 不应有悬空引用，实际 %v", dangling)
	}

	// 抽掉中间那一跳：db.query 的父就看不到了
	kept := tr.Spans[:1]
	kept = append(kept, tr.Spans[2])
	broken := Trace{TraceID: tr.TraceID, Spans: kept, Processes: tr.Processes}
	dangling := broken.DanglingRefs()
	if len(dangling) != 1 {
		t.Fatalf("断链后应恰好 1 条悬空引用，实际 %v", dangling)
	}
}

// TestIsErrorHonoursBothMarkers 断言 error=true 与 otel.status_code=ERROR 都算错。
func TestIsErrorHonoursBothMarkers(t *testing.T) {
	tr := parseTrace(t, fixtureTrace)
	if tr.IsError(spanByID(t, tr, "dddddddddddddddd")) != true {
		t.Fatal("只写 error=true 的 span 也须判为错（否则失败会伪装成成功）")
	}

	// 另一种写法：只有 otel.status_code
	tr.Spans[3].Tags = []Tag{{Key: "otel.status_code", Type: "string", Value: "ERROR"}}
	if !tr.IsError(spanByID(t, tr, "dddddddddddddddd")) {
		t.Fatal("otel.status_code=ERROR 应判为错")
	}
	if tr.IsError(spanByID(t, tr, "cccccccccccccccc")) {
		t.Fatal("正常 span 不应判为错")
	}
}

// TestLatestEndAndTags 断言末态时刻与 tag 取值。
func TestLatestEndAndTags(t *testing.T) {
	tr := parseTrace(t, fixtureTrace)
	// 最晚结束的是根 span（1000+1000），不是最后起步的那个（1200+50）——
	// 末尾才起的短 span 未必是最后结束的，断言按结束时刻算才站得住。
	if got := tr.LatestEnd(); got != 2000 {
		t.Fatalf("LatestEnd = %d，want 2000（根 1000+1000）", got)
	}
	dbSpan := spanByID(t, tr, "cccccccccccccccc")
	if got := tr.StringTag(dbSpan, "db.query.text"); got == "" {
		t.Fatal("db.query.text 取不到：tag 口径写错时这个值会静默为空")
	}
	if got := tr.StringTag(dbSpan, "db.rows_affected"); got != "1" {
		t.Fatalf("db.rows_affected = %q，want \"1\"（数字型 tag 也要能取字符串形式）", got)
	}
}
