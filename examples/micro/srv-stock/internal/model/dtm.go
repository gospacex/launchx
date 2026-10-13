package model

// Dtm 分支面（dtm Saga/TCC）的领域出入参。
//
// 单独成文件而不是并进 stock.go：这些类型的调用方是 dtm server 而不是 kitex，
// 走的是 internal/dtm 分支面而非 handler。混在一起会让「哪些方法能被
// POST /stock/{id}/reserve 调到」这件事看不出来 —— 而分支方法带 barrier，
// 被普通 HTTP 面调到就是一个无屏障的写路径。

// 分支方法名（失败注入 fail_inject 与 span 名共用同一套字面量）。
//
// 为什么要显式列举而不是拿方法名反射：fail_inject 靠字符串命中分支，字符串
// 拼错就是"点火失败"—— 演示脚本以为补偿被点着了，实际正向一路跑绿。故这些
// 字面量在分支 handler 与编排侧共用，且有单测钉住。
const (
	DtmDeductBranch    = "Deduct"
	DtmReleaseBranch   = "Release"
	DtmTryFreezeBranch = "TryFreeze"
	DtmConfirmBranch   = "ConfirmFreeze"
	DtmCancelFreeze    = "CancelFreeze"
)

// DtmStockCmd 分支业务体（Saga Deduct/Release 与 TCC Try/Confirm/Cancel 共用同一形状）。
//
// 共用是 dtm 的载荷语义决定的，不是偷懒：Saga 的 Add(action, compensate, payload)
// 只有一个 payload，补偿分支收的是**同一份**业务体；TCC 的 Try 载荷同样被
// Confirm / Cancel 复用。拆成五个结构体只会在字段漂移时多出五处要同步的地方，
// 而字段本来就必须一致 —— 不一致等于正向与补偿算的不是同一笔账。
type DtmStockCmd struct {
	// OrderNo 业务单号：进 stock_logs.reason，是账变流水与 Saga 分支的对账锚点。
	// TCC 三兄弟不依赖它定位库存（冻结量是全局的），但仍要求填 —— 流水上留得下
	// "这笔冻结属于哪张单"，排查时才知道该回退哪一条。
	OrderNo string `json:"order_no"`
	// ProductID 商品维度：库存是商品维度资源，不按用户分片。
	ProductID uint64 `json:"product_id"`
	// Quantity 数量。必须为正：传 0 或负数会让条件更新恒成立或恒不成立，
	// 前者白写一条零流水，后者变成永久业务失败 —— 两种都该在入口被拒。
	Quantity int32 `json:"quantity"`
	// UserID 操作人（可选，透传给 stock_logs.operator_id）。
	UserID uint64 `json:"user_id,omitempty"`
}
