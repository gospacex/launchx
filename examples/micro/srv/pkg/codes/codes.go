// Package codes 统一定义跨服务错误码（纯数据，无外部依赖）。
//
// 分域约定（只定一次，全 11 服务共用）：
//
//	0           成功
//	1xxx        通用
//	2xxx        用户域（srv-user）
//	3xxx        商品域（srv-product）
//	4xxx        订单域（srv-order）
//	5xxx        支付域（srv-pay）
//	6xxx        券/积分域（srv-coupon）
//
// 业务错误通过 rpcx 客户端错误体（Response.Code/Message）返回，
// 网络层失败由 rpcx/kitex 自带错误表达，两套错误不混用。
package codes

// 统一响应码结构（RPC 响应体的通用头部字段）
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
	CodeTimeout      = 1006 // 上游超时（降级兜底前使用）
)

// 用户段（srv-user）
const (
	CodeUserExists      = 2001 // 账号已存在
	CodeBadCredential   = 2002 // 用户名或密码错误
	CodeSessionExpired  = 2003 // 会话过期
	CodeUserDisabled    = 2004 // 账号被禁用
	CodeCheckUserFailed = 2005 // 跨域身份校验不通过
)

// 商品段（srv-product）
const (
	CodeProductOffShelf  = 3001 // 商品已下架
	CodeStockNotEnough   = 3002 // 库存不足
	CodeReserveStockFail = 3003 // 库存预占失败
)

// 订单段（srv-order）
const (
	CodeOrderNotFound  = 4001 // 订单不存在
	CodeOrderCancelled = 4002 // 订单已取消
	CodeOrderStatus    = 4003 // 订单状态不允许该操作
)

// 支付段（srv-pay）
const (
	CodePayFailed      = 5001 // 支付失败
	CodePayDuplicate   = 5002 // 重复支付
	CodeCallbackReject = 5003 // 回调数据校验失败
)

// 券/积分段（srv-coupon）
const (
	CodeCouponUsed      = 6001 // 券已使用（幂等重放）
	CodeCouponExpired   = 6002 // 券已过期
	CodeCouponNotMatch  = 6003 // 券不适用
	CodePointsNotEnough = 6004 // 积分不足
	CodePointsFailed    = 6005 // 积分发放失败
)

// OK 返回成功码
func OK() Code {
	return Code{Code: CodeOK}
}

// Fail 返回指定错误码，message 可为空（空则不给调用方细节）
func Fail(code int64, message string) Code {
	return Code{Code: code, Message: message}
}
