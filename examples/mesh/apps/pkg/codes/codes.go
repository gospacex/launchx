// Package codes 业务错误码：全局段 + 四域各自的段，避免跨域撞码。
//
// 与 examples/micro/pkg/codes 的差别：本包**不带** HTTP 状态映射（那是 bff 边界的事），
// 只提供码值与带 cause 的错误包装，让它在 srv 侧也能用。
package codes

import (
	"errors"
	"fmt"
)

// Code 业务错误码。四域段位：
//
//	1xxxx 通用 / 2xxxx order / 3xxxx points / 4xxxx product / 5xxxx stock
type Code int32

const (
	OK Code = 0

	InvalidArgument Code = 10001
	Internal        Code = 10500

	// Unimplemented 下游明确声明「本示例未实现」。
	// 与 Internal 分开是刻意的：Internal 会被 bff 映射成 502（上游坏了），
	// 而 501 如实表达「方法存在但没接线」，排查时不必去翻下游日志。
	Unimplemented Code = 10501

	// Unauthorized 身份**非法**：metainfo 里的 identity 解析不出来（bff 侧写坏），
	// 重试无意义（bff 映射 401）。
	//
	// 注意「请求**没带**身份」不走这个码：apps 把传输层身份下沉给 Istio，应用侧对
	// 匿名请求是 fail-open（按 svc 桶计数），不是拒绝。别把这个码的注释写成
	// 「身份缺失」—— 那与 admit 的实际行为相反。
	Unauthorized Code = 10401

	// Forbidden 身份合法但**无权访问该资源**（IDOR 防护：订单归属 / 账户归属与
	// 主体不一致，见 2026-10-10 企业级加固的 rpc.<域> 归属校验）。bff 映射 403。
	//
	// 与 Unauthorized 分开是刻意的：401 说「你是谁我不知道，重试无意义」，
	// 403 说「我知道你是谁，但这不是你的东西」—— 客户端与运维对两者的处置不同
	//（401 要去取凭据，403 是越权尝试，值得单独告警）。
	Forbidden Code = 10403

	// RateLimited 被限流：值得退避重试（bff 映射 429）。
	// 与 Unauthorized 分开是刻意的：客户端对两者的重试策略相反
	// —— 被限流等一会儿可以再来，身份非法重试一万次也是同一个结果。
	RateLimited Code = 10429

	OrderNotFound   Code = 20404
	OrderCreateFail Code = 20500
	PointsNotEnough Code = 30400
	ProductNotFound Code = 40404
	StockNotEnough  Code = 50300
)

// String 便于日志与错误串里直接读值。
func (c Code) String() string { return fmt.Sprintf("code=%d", int32(c)) }

// Error 业务错误：码 + 人读信息 + 可选根因。
//
// cause 单独存放而不是拼进 msg：errors.Is/As 要能穿到根因（例如
// gorm.ErrRecordNotFound），拼成字符串就把这条链掐断了。
//
// 本类型**同时满足 kitex 的 kerrors.BizStatusErrorIface**（下面三个方法就是
// 为此而存在，见 rpcwire 包注释）—— 这不是巧合，是让业务码能过进程边界的手段：
// kitex 在最内层 dispatch 的返回值上做 `kerrors.FromBizStatusError(err)`，
// 命中才把码写进 TTHeader；不命中就把错误**降级成字符串**（客户端只拿得到
// 一段 message，码没了）。接口是结构化的，故这里不需要 import kitex。
type Error struct {
	Code  Code
	Msg   string
	Cause error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("[%d] %s: %v", int32(e.Code), e.Msg, e.Cause)
	}
	return fmt.Sprintf("[%d] %s", int32(e.Code), e.Msg)
}

// Unwrap 让 errors.Is/As 能穿到 Cause。
func (e *Error) Unwrap() error { return e.Cause }

// BizStatusCode 跨进程后客户端能取回的码。语义见 kerrors.BizStatusErrorIface。
func (e *Error) BizStatusCode() int32 { return int32(e.Code) }

// BizMessage 跨进程后客户端能取回的消息。
//
// 刻意只带 Msg、不带 Cause：根因里可能有 SQL 片段、内网地址这类不该出界的细节。
// 根因留在服务端日志（kitex 会把非 biz 的部分打出来），客户端拿到码与一句话即可。
func (e *Error) BizMessage() string { return e.Msg }

// BizExtra 本示例不携带附加信息（接口要求，返回 nil 表示「无额外字段」）。
func (e *Error) BizExtra() map[string]string { return nil }

// New 构造业务错误。
func New(c Code, msg string) *Error { return &Error{Code: c, Msg: msg} }

// Wrap 在业务错误上挂根因。
func Wrap(c Code, msg string, cause error) *Error {
	return &Error{Code: c, Msg: msg, Cause: cause}
}

// From 从任意 error 提取业务码；非业务错误一律 Internal。
func From(err error) (Code, string) {
	if err == nil {
		return OK, ""
	}
	var be *Error
	if errors.As(err, &be) {
		return be.Code, be.Msg
	}
	return Internal, err.Error()
}
