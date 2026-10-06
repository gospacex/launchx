package traceutil

import (
	"sort"
	"strconv"
	"strings"
)

// Tag span 属性。
//
// Jaeger REST 的 tag 是三元组 {key, type, value}，value 的实际 Go 类型由 Type
// 决定（string / bool / float64 / ...）。这里保留 any + Type 交给调用方：
// 阈值比较要按数字比，而"取 SQL 文本"只需字符串，两边对类型的容错要求不同，
// 在底层替它们选一边都会出错。
type Tag struct {
	Key   string `json:"key"`
	Type  string `json:"type"`
	Value any    `json:"value"`
}

// Reference span 之间的引用。RefType 为 CHILD_OF 时构成树，FOLLOWS_FROM 不构成。
type Reference struct {
	RefType string `json:"refType"`
	TraceID string `json:"traceID"`
	SpanID  string `json:"spanID"`
}

// Log 结构化日志条目（Jaeger 里是 {timestamp, fields[]}）。
type Log struct {
	Timestamp int64 `json:"timestamp"`
	Fields    []Tag `json:"fields"`
}

// Span 一条 trace 中的一段。
type Span struct {
	TraceID       string      `json:"traceID"`
	SpanID        string      `json:"spanID"`
	OperationName string      `json:"operationName"`
	References    []Reference `json:"references"`
	StartTime     int64       `json:"startTime"` // 微秒
	Duration      int64       `json:"duration"`  // 微秒
	Tags          []Tag       `json:"tags"`
	Logs          []Log       `json:"logs"`
	ProcessID     string      `json:"processID"`
	Warnings      []string    `json:"warnings"`
}

// Process 上报 span 的进程（服务）。
type Process struct {
	ServiceName string `json:"serviceName"`
	Tags        []Tag  `json:"tags"`
}

// Trace 一条完整的调用链：span 集合 + 进程表。
//
// 服务名必须经 Processes 查到（包注释第 2 条），故所有涉及服务名的查询方法
// 都挂在 Trace 上，而不是让调用方自己去 join。
type Trace struct {
	TraceID   string             `json:"traceID"`
	Spans     []Span             `json:"spans"`
	Processes map[string]Process `json:"processes"`
	Warnings  []string           `json:"warnings"`
}

// ServiceOf 取 span 所属服务名；进程表缺失时返回 "<unknown>"，**不返回空串**
// —— 空串会静默通过所有 `== "demo-bff"` 之外的比较，把断链伪装成"服务名不同"。
func (t Trace) ServiceOf(s Span) string {
	if p, ok := t.Processes[s.ProcessID]; ok && p.ServiceName != "" {
		return p.ServiceName
	}
	return "<unknown>"
}

// TagsOf 把 span 的 tag 列表摊平成 map。
//
// 重名 tag 取最后一个：Jaeger 允许同名多次写入，实践中后写的是修正值。
func (t Trace) TagsOf(s Span) map[string]any {
	out := make(map[string]any, len(s.Tags))
	for _, tag := range s.Tags {
		out[tag.Key] = tag.Value
	}
	return out
}

// StringTag 取字符串形式的 tag 值（非字符串类型做无损转写）。
func (t Trace) StringTag(s Span, key string) string {
	v, ok := t.TagsOf(s)[key]
	if !ok {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return ""
	}
}

// IsError 判断 span 是否被标错。
//
// 两个口径都要认：otel SDK 写 `otel.status_code=ERROR`（常配 `error=true`），
// 而部分埋点只写 `error=true`。只认前者会把"只写了 error=true"的老埋点判成成功
// —— 这种方向的假阴性比漏判更危险。
func (t Trace) IsError(s Span) bool {
	tags := t.TagsOf(s)
	if v, ok := tags["error"]; ok {
		switch b := v.(type) {
		case bool:
			if b {
				return true
			}
		case string:
			if strings.EqualFold(b, "true") {
				return true
			}
		}
	}
	if v, ok := tags["otel.status_code"].(string); ok && strings.EqualFold(v, "ERROR") {
		return true
	}
	return false
}

// ParentOf 取 span 的父 span（仅认 CHILD_OF）。
func (t Trace) ParentOf(s Span) (Span, bool) {
	pid := parentSpanID(s)
	if pid == "" {
		return Span{}, false
	}
	for _, c := range t.Spans {
		if c.SpanID == pid {
			return c, true
		}
	}
	return Span{}, false
}

// Roots 返回没有父的 span（理论上恰好 1 个；多个 = 断链）。
func (t Trace) Roots() []Span {
	var out []Span
	for _, s := range t.Spans {
		if parentSpanID(s) == "" {
			out = append(out, s)
		}
	}
	return out
}

// ChildrenOf 返回直接子节点。
func (t Trace) ChildrenOf(parentID string) []Span {
	var out []Span
	for _, s := range t.Spans {
		if parentSpanID(s) == parentID {
			out = append(out, s)
		}
	}
	return out
}

// AncestorsOf 返回从直接父到最远祖先的链（不含自身），按由近及远排列。
//
// 循环引用防护：病态数据里 A→B→A 会让朴素递归栈溢出，这里按 SpanID 去重 + 上限。
func (t Trace) AncestorsOf(s Span) []Span {
	var out []Span
	seen := map[string]bool{s.SpanID: true}
	cur := s
	for len(out) < len(t.Spans) {
		p, ok := t.ParentOf(cur)
		if !ok || seen[p.SpanID] {
			return out
		}
		seen[p.SpanID] = true
		out = append(out, p)
		cur = p
	}
	return out
}

// Depth 返回 span 树的**最大深度**（根为 1）。
func (t Trace) Depth() int {
	byID := map[string]Span{}
	for _, s := range t.Spans {
		byID[s.SpanID] = s
	}
	depth := map[string]int{}
	var walk func(id string, guard int) int
	walk = func(id string, guard int) int {
		if d, ok := depth[id]; ok {
			return d
		}
		if guard > len(byID) {
			return 0
		}
		s, ok := byID[id]
		if !ok {
			return 0
		}
		pid := parentSpanID(s)
		d := 1
		if pid != "" {
			if _, exists := byID[pid]; exists {
				d = walk(pid, guard+1) + 1
			}
		}
		depth[id] = d
		return d
	}
	max := 0
	for id := range byID {
		if d := walk(id, 0); d > max {
			max = d
		}
	}
	return max
}

// Services 返回本 trace 涉及的全部服务名（升序去重）。
func (t Trace) Services() []string {
	set := map[string]bool{}
	for _, s := range t.Spans {
		set[t.ServiceOf(s)] = true
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// OperationNames 返回本 trace 的全部 operationName（升序去重）。
func (t Trace) OperationNames() []string {
	set := map[string]bool{}
	for _, s := range t.Spans {
		set[s.OperationName] = true
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// DanglingRefs 返回"父 spanID 在本 trace 内找不到"的悬空引用。
//
// 这条判据是「尾部 span 缺失」最机械的探针：批量导出器丢掉最后一批 span 时，
// 它们的**父**留在 trace 里，而它们自己不在 —— 但反过来更简单：如果一个 span
// 的父不在 trace 内且它自己也不是根，说明中间被挖走了一段。
// 注意：真正的根（完全无 CHILD_OF 引用）不算悬空。
func (t Trace) DanglingRefs() []string {
	present := map[string]bool{}
	for _, s := range t.Spans {
		present[s.SpanID] = true
	}
	var out []string
	for _, s := range t.Spans {
		for _, r := range s.References {
			if r.RefType != refTypeChildOf {
				continue
			}
			if !present[r.SpanID] {
				out = append(out, s.SpanID+" → "+r.SpanID)
			}
		}
	}
	sort.Strings(out)
	return out
}

// LatestEnd 返回 trace 内最晚的结束时刻（微秒）。
//
// 配合根 span 的 Duration 用：退出链没冲刷时，最后结束的那批 span 不在，
// LatestEnd 会明显小于 根.startTime + 根.duration。
func (t Trace) LatestEnd() int64 {
	var latest int64
	for _, s := range t.Spans {
		if end := s.StartTime + s.Duration; end > latest {
			latest = end
		}
	}
	return latest
}

// SpansWith 返回 operationName 命中任一前缀的 span。
func (t Trace) SpansWith(prefixes ...string) []Span {
	var out []Span
	for _, s := range t.Spans {
		for _, p := range prefixes {
			if strings.HasPrefix(s.OperationName, p) {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

// parentSpanID 取 CHILD_OF 引用的目标 spanID。
func parentSpanID(s Span) string {
	for _, r := range s.References {
		if r.RefType == refTypeChildOf {
			return r.SpanID
		}
	}
	return ""
}
