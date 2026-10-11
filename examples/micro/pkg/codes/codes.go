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

import "errors"

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
	// CodeForbidden 身份与请求参数冲突：鉴权身份存在，但请求里声明了另一个
	// 主体（换 user_id 读别人的数据）。与 1004 的区别是**凭据本身没问题** ——
	// 1004 是"没带身份"，1007 是"带了身份却在够别人的东西"，后者要单独可见，
	// 否则越权尝试在日志里和"没登录"混成一类，审计时抓不出来。
	CodeForbidden = 1007
	// CodeConcurrent 并发冲突（分布式锁未获取 / 重试耗尽）：**可重试**。
	//
	// 四个域的取锁失败一律用这一个码，不再各报各的（订单 3006 / 库存 4004 /
	// 商品与积分此前报 1003）。分开报的代价是调用方要为每个域写一遍重试判断，
	// 漏一个域就表现为"那个域的失败不敢重试"。1003 尤其不可用：它是内部错误
	// 语义，客户端看到 500 只会放弃而不是重试 —— 与"明确告知可重试"的注释
	// 正好相反。
	//
	// 3006 / 4004 作为域内别名保留（老客户端可能仍在按码分支），
	// 新的写路径一律用 CodeConcurrent。
	CodeConcurrent = 1008
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
	// CodeStockCircuitOpen 预占熔断已打开：**基础设施故障**语义（trigger=infra）。
	//
	// 计数只吃业务性失败，连续失败到阈值的现实含义通常是"这一行写不进去"
	// 而不是"这个商品没货"。故它不能复用 CodeStockInsufficient —— 后者会
	// 让调用方去补货，而真正的故障在 DB/网络。
	CodeStockCircuitOpen = 4007
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

// Error 业务错误：分层之间用 error 向上传递，由最外层（handler）统一翻译成
// 响应体的 Code/Message。
//
// 为什么 service 层要返回 error 而不是自己写 resp.Code：分层链要求
// service 不认识 pb（否则 pb 改动会顺着 import 渗进业务规则），而"业务上
// 拒绝"又必须带码上浮。用 error 承载码值，handler 一次翻译，两层都干净。
//
// 与传输错误的分工：本类型 = 业务判定失败（库存不足、订单不存在…），
// 走响应体 Code；非本类型的 error = 传输/内部失败，由 kitex 错误体表达，
// 不翻译成业务码（否则"下游挂了"会被误报成"库存不足"）。
type Error struct {
	Code    int64
	Message string
	Cause   error
}

// Error 实现 error 接口
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Cause != nil {
		return e.Message + ": " + e.Cause.Error()
	}
	return e.Message
}

// Unwrap 暴露根因，便于 errors.Is / errors.As 穿透到驱动错误
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// New 构造业务错误
func New(code int64, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Wrap 构造带根因的业务错误（内部错误码用得多：日志要留因，响应不带因）
func Wrap(code int64, message string, cause error) *Error {
	return &Error{Code: code, Message: message, Cause: cause}
}

// As 从任意 error 里取出业务错误（等价于 errors.As 的便捷封装）
func As(err error) (*Error, bool) {
	var be *Error
	if errors.As(err, &be) {
		return be, true
	}
	return nil, false
}
