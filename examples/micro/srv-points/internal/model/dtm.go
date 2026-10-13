package model

// ---------- dtm 分支载荷（分层链内部流通，不含 pb / 不含 DB）----------

// DtmBiz 是 dtm 分支载荷的业务体（对应 busi.proto 的 biz 字段，编排侧以
// JSON 字符串塞进来）。
//
// 为什么放 model 而不是 service 或 dtm 包：它同时被三个层引用 —— dtm 协议
// 适配层做 JSON 反序列化、service 做入参校验、repository 做 SQL 取值。
// 放最下游的 model 才能让 service 不反向 import dtm 包（那是六层链的禁令，
// 见 service 包注释），同时 dtm 包也不必认识 repository 的入参类型。
//
// 字段口径与积分域既有命令（EarnCmd / ConsumeCmd）刻意一致：user_id / amount
// / order_no。差别只有金额叫 cost 而不是 amount —— cost 是「这笔事务该动多少」
// 的口径，amount 在既有命令里是「这笔流水记多少」，两者在本域当前相等，但
// 混用会让补偿段读起来像在记一笔新流水。
type DtmBiz struct {
	// UserID 积分账户所属用户。
	UserID int64 `json:"user_id"`
	// Cost 本次分支要动的积分数（恒为正：方向由方法语义决定，Debit/Confirm
	// 是减、Refund/Cancel 是加，载荷里不再传符号，避免"负数扣款"这类歧义）。
	Cost int64 `json:"cost"`
	// OrderNo 订单号（Saga 两段记流水用；TCC 三段用它派生命名空间键）。
	OrderNo string `json:"order_no"`
}
