package model

// Dtm 分支面（dtm Saga/TCC）的领域出入参。
//
// 单独成文件而不是并进 order.go：这些类型的调用方是 dtm server 而不是 kitex，
// 走的是 internal/dtm 分支面而非 handler。混在一起会让「哪些方法能被
// POST /order 调到」这件事看不出来 —— 而分支方法带 barrier，被普通 HTTP 面
// 调到就是一个无屏障的写路径。
//
// 本包仍然零依赖（不 import pb / gorm / grpc），与 order.go 的纪律一致。

// 分支方法名（失败注入 fail_inject 与 span 名共用同一套字面量）。
//
// 为什么要显式列举而不是拿方法名反射：fail_inject 靠字符串命中分支，字符串
// 拼错就是「点火失败」—— 演示脚本以为补偿被点着了，实际正向一路跑绿。故这些
// 字面量在分支 handler 与编排侧共用，且有单测钉住（见 internal/dtm 的
// TestFullMethodNamesMatchDtmCallbackURL：proto 方法名必须与本组常量逐字相等）。
const (
	// DtmCreateOrderBranch Saga 正向：正式单落库（orders + order_items + outbox）。
	DtmCreateOrderBranch = "CreateOrder"
	// DtmCancelOrderBranch Saga 补偿：正式单作废（**不写 outbox**）。
	DtmCancelOrderBranch = "CancelOrder"
	// DtmTryPlaceBranch TCC 一阶段：建 presale_locked 占位单（**不写 outbox**）。
	DtmTryPlaceBranch = "TryPlace"
	// DtmConfirmFreezeBranch TCC 二阶段：占位单转正式 paid 单（**此刻才写 outbox**）。
	DtmConfirmFreezeBranch = "ConfirmFreeze"
	// DtmCancelFreezeBranch TCC 二阶段：废掉占位单（**不写 outbox**）。
	DtmCancelFreezeBranch = "CancelFreeze"
)

// StatusPresaleLocked TCC 占位单状态（orders.status 的取值，不是 schema 变更）。
//
// 为什么落在 model 而不是 dtm.go 私有常量：它进的是 orders 表的 status 列，而
// status 列的取值域由本域 model/order.go 的那组常量定义（报表 COUNT(status)、
// kitex 响应的 Status 字段都按它解读）。私有常量等于给同一列开第二个取值域，
// 而「占位单该不该出现在报表里」正是这个取值唯一要回答的问题 —— 故并入同一处。
const StatusPresaleLocked = "presale_locked"

// DtmOrderCmd dtm 分支业务体（Saga CreateOrder/CancelOrder 与 TCC
// TryPlace/ConfirmFreeze/CancelFreeze 共用同一形状）。
//
// 共用是 dtm 的载荷语义决定的，不是偷懒：Saga 的 Add(action, compensate, payload)
// 只有一个 payload，补偿分支收的是**同一份**业务体；TCC 的 Try 载荷同样被
// Confirm / Cancel 复用。拆成五个结构体只会在字段漂移时多出五处要同步的地方，
// 而字段本来就必须一致 —— 不一致等于正向与补偿算的不是同一笔账。
type DtmOrderCmd struct {
	// UserID 下单用户。与 HTTP 面同义：跨域身份由 bff 透传，本域不验签。
	UserID uint64 `json:"user_id"`
	// OrderNo 业务单号，落 orders.uk_order_no。
	//
	// 它同时是**幂等键**：同一个 order_no 在第二个全局事务里再走一遍分支，
	// insert ignore 命中唯一键返回 0 行 —— 那就是「这一单已经建过」，分支不再
	// 重复落明细与事件（与 HTTP 面 CreateOrderTx 的 Idempotent 语义同源）。
	OrderNo string `json:"order_no"`
	// DtmGID 本次全局事务号（bff 提交时生成的那个 gid）。
	//
	// 必须由**编排侧编进载荷**而不是分支侧去读 metadata：Confirm/Cancel 的回调
	// 虽然带着 gid，但它是 dtm 的路由信息而非业务载荷；把它编进载荷后，
	// 「这张占位单属于哪个全局事务」就随业务体一起流转，也才写得出
	// orders.dtm_gid 这一列（schema-micro.sql 第 4 节的注释即为此）。
	//
	// 落库判据一律 `order_no = ? and dtm_gid = ?`：少一个条件就允许 A 的全局
	// 事务去确认 B 的占位单，而那在账上表现为「货冻结了但订单归了别人」。
	DtmGID string `json:"dtm_gid"`
	// Items 订单明细。必须非空：空明细的订单总额恒为 0，落库后是一张查不出
	// 明细、也永远对不上账的单（见 repository 的判据）。
	Items []OrderItemCmd `json:"items"`
	// Traceparent 载荷携带的 W3C 链路上下文，落到 outbox.traceparent。
	//
	// 为什么不在 service 里现取：正向下单由 bff 发起，整条链（含 outbox 补发
	// 出去的那一跳）应挂在**同一条** trace 上，而 outbox Runner 补发时早就脱离
	// 了本次请求的 ctx —— 只能靠行里存下来的这一串续。
	//
	// 补偿 / Cancel 由 dtm 异步发起，载荷里没有它，此处为空：那种事件按「新根
	// trace + biz.dtm.gid 关联」口径记录（且它们本来就不写 outbox）。
	Traceparent string `json:"traceparent,omitempty"`
}

// Total 载荷明细的合计金额（分）。
//
// 口径与 HTTP 面 CreateOrder 完全一致（Σ quantity × unit_price，服务端算）：
// 总额是后续对账与积分入账的依据，让调用方带进来等于把可篡改字段写进账本。
// 放在 model 上而不是 service/Repository 各算一遍：Saga 的 CreateOrder 与 TCC
// 的 TryPlace 都要用它落 orders.total_amount，两处算两遍就会漂。
func (c DtmOrderCmd) Total() int64 {
	var total int64
	for _, it := range c.Items {
		total += int64(it.Quantity) * it.UnitPrice
	}
	return total
}
