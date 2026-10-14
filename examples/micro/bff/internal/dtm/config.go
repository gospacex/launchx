// 配置读取：把 config.yaml 的两段地址翻成本包用的形态。
//
// 单独成文件而不是塞进 saga.go：这两段是**装配期**与本包解耦的唯一接触面，
// 它们要回答的问题（哪个键、缺了会怎样、谁来裁决失败姿态）与编排逻辑无关，
// 混在一起会让 saga.go 的每个函数都先解释一遍「地址从哪来」。
//
// 键名：
//
//	xmiddleware.dtm.grpc_addr   dtm server 的 gRPC 端口（提交面）
//	dtm.branches.<domain>       四域分支面 gRPC 监听地址（回调面）
//
// 两段**都在 Go 代码之外**：仓纪律要求服务源码零硬编码回环地址
// （examples_addr_hygiene_test.go 的 TestServiceSourceHasNoHardcodedLoopbackAddr），
// 所以连「缺省 127.0.0.1:36790」也只写在 config.yaml。代码里写了它，那条门禁
// 立刻红，且换部署形态就得重新编译。
package dtm

import (
	"github.com/gospacex/configx"
)

// ConfigKeys 配置键名。导出是为了让 app/setup.go 的日志与文档用同一份字面量，
// 而不是各自抄一遍（抄错的那个不会报错，只会让日志指着一个不存在的键）。
const (
	// KeyDTMAddr dtm server 的 gRPC 地址。
	KeyDTMAddr = "xmiddleware.dtm.grpc_addr"
	// KeyBranchPrefix 四域分支面地址段的键前缀（补域键即完整键）。
	KeyBranchPrefix = "dtm.branches."
)

// BranchAddrsFrom 从配置读四域分支面地址。
//
// 缺项不在这里报错：缺哪个键由 New 的 validate 点名（一次列全比逐个早退更有用），
// 这里保持「读到就返回」的纯读取语义。
func BranchAddrsFrom(cfg configx.Config) map[string]string {
	return map[string]string{
		DomainStock:   cfg.GetString(KeyBranchPrefix + DomainStock),
		DomainOrder:   cfg.GetString(KeyBranchPrefix + DomainOrder),
		DomainProduct: cfg.GetString(KeyBranchPrefix + DomainProduct),
		DomainPoints:  cfg.GetString(KeyBranchPrefix + DomainPoints),
	}
}

// DescribeBranchAddrs 供启动日志回显：分支序 × 地址，一行看清编排去向。
//
// 顺序取自 branchPlan（= 真实推进序），不是域键的字典序 —— 日志里地址的排列
// 顺序与 dtm 推进它们的顺序一致，排查时才不会把 9043 和 9041 读反。
func DescribeBranchAddrs(cfg configx.Config) []string {
	addrs := BranchAddrsFrom(cfg)
	out := make([]string, 0, len(branchPlan()))
	for _, b := range branchPlan() {
		out = append(out, b.domain+"="+addrs[b.domain])
	}
	return out
}

// DescribeBranchOrder 供启动日志回显编排序（含补偿分支名）。
//
// 打到日志而不是只留在代码里：真机上「dtm 到底按什么次序回调」是最常被反复
// 确认的一件事，而答案只存在于编排代码的一行 slice 里 —— 把它与 gid 一起留在
// 进程日志里，对账时才不用去翻代码。
func DescribeBranchOrder() string {
	plan := branchPlan()
	parts := make([]string, 0, len(plan))
	for i, b := range plan {
		if i > 0 {
			parts = append(parts, "→")
		}
		parts = append(parts, b.domain+"."+b.action+"/"+b.compensate)
	}
	return joinWith(parts, " ")
}

// joinWith 极简拼接（不引 strings.Join 的可变参数切片，只是省一次分配）。
func joinWith(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += sep + p
	}
	return out
}

// ---------- TCC 侧的回显（tcc.go 的配套） ----------
//
// 与上面 Saga 那两个同构但分开：两链的分支序不同、涉及的域不同（日志里把两串
// 挨着打出来，运维一眼能看出「哪串是 Saga、哪串是 TCC」），共用一个函数就得
// 加参数区分，那还不如各写各的。

// DescribeTCCBranchAddrs 供启动日志回显：TCC 分支序 × 地址。
//
// 顺序取自 tccBranchPlan（= 真实 Try 序），且**只含 TCC 涉及的三个域** ——
// 把 product 也打进来会让人以为预售锁单要动商品域。
func DescribeTCCBranchAddrs(cfg configx.Config) []string {
	addrs := BranchAddrsFrom(cfg)
	out := make([]string, 0, len(tccBranchPlan()))
	for _, b := range tccBranchPlan() {
		out = append(out, b.domain+"="+addrs[b.domain])
	}
	return out
}

// DescribeTCCBranchOrder 供启动日志回显 TCC 分支序（含 confirm / cancel 名）。
//
// 三段一起打：真机上最常被反复确认的就是「dtm 到底会回调哪个 confirm」，
// 而答案只存在于编排代码的一行 slice 里。
func DescribeTCCBranchOrder() string {
	plan := tccBranchPlan()
	parts := make([]string, 0, len(plan))
	for i, b := range plan {
		if i > 0 {
			parts = append(parts, "→")
		}
		parts = append(parts, b.domain+"."+b.try+"/"+b.confirm+"/"+b.cancel)
	}
	return joinWith(parts, " ")
}
