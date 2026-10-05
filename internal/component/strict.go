package component

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gospacex/logx"
	"github.com/gospacex/traceflag"
)

// 各组件降级时的后果文案。写"后果"而不是"错误"，
// 因为降级后进程仍在跑，运维需要知道的是"接下来会发生什么"。
const (
	ConsequenceDB       = "数据库不可用，落库请求将 5xx"
	ConsequenceCache    = "缓存穿透 / 限流 fail-open"
	ConsequenceMQ       = "事件投递中断，outbox 将堆积重试"
	ConsequenceRegistry = "服务不被发现，上游调用将 502"
	ConsequenceSearch   = "搜索功能不可用，检索请求将报错"
	ConsequenceMongo    = "文档库不可用，非结构化读写将报错"
)

// degrade 按 strict.<name> 裁决组件构造失败：
//   - strict=false（缺省）→ 产物保持零值（降级），打一条带后果与提示的 Warn，启动继续；
//   - strict=true → 返回带 "init <name>" 上下文的 error（Bootstrap 会走完整反序退出链）。
//
// 地基（配置 / telemetry / logx）失败不经过本函数，永远即退。
func degrade(env *Env, name, consequence string, err error) error {
	if env.StrictOf(name) {
		return fmt.Errorf("init %s: %w", name, err)
	}
	if env.Logger != nil {
		env.Logger.Warn("launchx: component degraded",
			logx.String("component", name),
			logx.Err(err),
			logx.String("consequence", consequence),
			logx.String("hint", "set strict."+name+": true to fail fast"),
		)
	}
	return nil
}

// traceOn 判定组件是否入链：经 traceflag 做
// 「实例级 AND 组件级 AND 全局总闸」，launchx 只把 yaml 的 trace.on.<name>
// 映射为组件级，不自造布尔逻辑（否则总闸失效，traceflag.SetEnabled(false) 关不掉）。
func traceOn(env *Env, name string) bool {
	return traceflag.Apply(name, env.TraceOnOf(name))
}

// pickSection 在候选段键中选第一个存在的（用于兼容多种历史键名）。
// 都不存在返回 ""，调用方据此静默跳过（presence-based）。
func pickSection(cfg interface{ IsSet(string) bool }, candidates ...string) string {
	for _, k := range candidates {
		if cfg.IsSet(k) {
			return k
		}
	}
	return ""
}

func sortStrings(s []string) { sort.Strings(s) }

func errUnknownComponent(name string) error {
	return fmt.Errorf("launchx: unknown component %q, registered: %s", name, strings.Join(Names(), " / "))
}
