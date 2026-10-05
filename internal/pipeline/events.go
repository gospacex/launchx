package pipeline

import "sync"

// Events 启动/退出步骤的事件记录器。
//
// 它是顺序断言的**唯一依据**：不靠日志文本匹配（日志格式会变、可能被采样丢弃），
// 而是记录每次步骤进入的顺序，供测试断言「telemetry 先于组件挂载」等红线。
type Events struct {
	mu  sync.Mutex
	seq []string
	idx map[string]int
}

// NewEvents 构造空记录器。
func NewEvents() *Events { return &Events{idx: map[string]int{}} }

// Record 记录一步（同名只记首次出现，便于断言"是否发生过"与"先后位置"）。
func (e *Events) Record(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.idx[name]; ok {
		return
	}
	e.idx[name] = len(e.seq)
	e.seq = append(e.seq, name)
}

// Order 返回调用序快照（副本，调用方可随意使用）。
func (e *Events) Order() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.seq))
	copy(out, e.seq)
	return out
}

// IndexOf 返回该事件首次出现的下标，未出现返回 -1。
func (e *Events) IndexOf(name string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if i, ok := e.idx[name]; ok {
		return i
	}
	return -1
}
