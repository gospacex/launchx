// Package codes 定义 mall 四个微服共用的错误码（纯数据，无外部依赖）。
//
// 分域约定（只在这里定一次，四服共用）：
//
//	0        成功
//	1xxx     通用
//	2xxx     商品域（srv-product）
//	3xxx     订单域（srv-order）
//	4xxx     库存域（srv-stock）
//	5xxx     积分域（srv-points）
//
// 两套错误不混用：业务错误走 RPC 响应体的 Code/Message 字段；网络层失败
// （超时 / 不可达 / 上游 panic）由 kitex 错误体表达，不翻译成业务码 ——
// 否则「下游挂了」会被误报成「库存不足」，运维按错误码排查会对不上账。
package codes

// Code 统一响应码结构（RPC 响应体的通用头部字段）
type Code struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
}

// 成功
const CodeOK = 0

// 通用段
const (
	CodeInvalidParam = 1001 // 参数非法
	CodeNotFound     = 1002 // 资源不存在
	CodeInternal     = 1003 // 内部错误
	CodeUnauthorized = 1004 // 身份缺失/非法
	CodeRateLimited  = 1005 // 被限流
	CodeUpstream     = 1006 // 下游调用失败（区别于本域业务失败）
)

// 商品域（srv-product）
const (
	CodeProductNotFound     = 2001 // 商品不存在
	CodeProductOffShelf     = 2002 // 商品已下架
	CodeProductWriteFailed  = 2003 // 商品写库失败
	CodeProductStatusDup    = 2004 // 上下架状态已是目标态（幂等重放）
	CodeCategoryNotFound    = 2005 // 类目不存在
	CodeProductTitleInvalid = 2006 // 标题非法
)

// 订单域（srv-order）
const (
	CodeOrderNotFound    = 3001 // 订单不存在
	CodeOrderCancelled   = 3002 // 订单已取消
	CodeOrderStatus      = 3003 // 订单状态不允许该操作
	CodeOrderItemsEmpty  = 3004 // 下单明细为空
	CodeOrderStockFailed = 3005 // 下单前置的库存预占失败
	CodeOrderConcurrent  = 3006 // 同用户并发下单（分布式锁未获取，可重试）
)

// 库存域（srv-stock）
const (
	CodeStockNotFound      = 4001 // 库存记录不存在
	CodeStockInsufficient  = 4002 // 可用库存不足
	CodeStockReserveFailed = 4003 // 预占写库失败
	CodeStockLockFailed    = 4004 // 分布式锁获取失败（并发冲突，可重试）
	CodeStockReservationNF = 4005 // 预占流水不存在（幂等释放/重复确认）
	CodeStockOverRelease   = 4006 // 释放数量超过预占量
)

// 积分域（srv-points）
const (
	CodePointsAccountNF     = 5001 // 积分账户不存在
	CodePointsInsufficient  = 5002 // 积分余额不足
	CodePointsDuplicated    = 5003 // 幂等键命中（同订单号已入账/已消费）
	CodePointsWriteFailed   = 5004 // 积分流水写库失败
	CodePointsAmountInvalid = 5005 // 积分数量非法
)

// OK 返回成功码
func OK() Code {
	return Code{Code: CodeOK, Message: "ok"}
}

// Fail 返回指定错误码，message 为空时不给调用方细节（避免把内部错误外泄）
func Fail(code int64, message string) Code {
	return Code{Code: code, Message: message}
}
