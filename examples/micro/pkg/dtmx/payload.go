package dtmx

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/gospacex/launchx"
)

// InjectKey 分支载荷里承载 traceparent 的字段名。
//
// 为什么编进**业务载荷**而不是让 dtm 透传：dtm server 只按 URL 回调分支，
// 不携带调用方的自定义 metadata。正向链要挂回同一条 Jaeger trace，只能由
// 编排侧（bff）把 traceparent 编进载荷，分支面再 Extract 回来。
// 补偿 / Cancel 由 dtm 异步发起，那时载荷里没有 traceparent，按「新根 trace +
// biz.dtm.gid 关联」口径处理（见 spec 的补偿段要求）。
const InjectKey = "traceparent"

// Payload dtm 分支业务载荷。
//
// 它在本包里的角色是**分支 handler 侧的反序列化载体**：dtm 提交时塞进
// busi.proto 的是 proto 消息（traceparent / fail_inject 是 proto 字段，
// 见各域 internal/dtm/protos/busi.proto），分支 handler 收到后把字段搬进
// 本结构体再交给 service。本包不 import 各域 proto —— 否则四个域共用一个
// 公共件就会被迫认识四个域的消息类型。
type Payload struct {
	// Biz 业务体的原始 JSON。放原始字节而不是解析后的结构体：四个域的
	// 业务体各不相同，在这一层解析等于把四份 model 抄进公共件。
	Biz json.RawMessage
	// Traceparent W3C traceparent，续链用。
	Traceparent string
	// FailInject 失败注入目标分支名（演示用）：编排侧填入后，对应分支的
	// handler 返回业务失败，用于点火补偿 / Cancel 链。真机为零值。
	FailInject string
}

// Encode 载荷序列化（编排侧用；分支侧一般直接拿 proto 字段，见 Payload 文档）。
func Encode(p Payload) (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", errors.Join(errors.New("dtmx: encode payload"), err)
	}
	return string(b), nil
}

// Decode 载荷反序列化（分支面用）。
//
// 坏输入一律报错、不返回零值：零值载荷会让分支拿着空的 order_no 去落库，
// 失败点还会漂移到更远的下游。
func Decode(data string) (Payload, error) {
	var p Payload
	if err := json.Unmarshal([]byte(data), &p); err != nil {
		return Payload{}, errors.Join(errors.New("dtmx: decode payload"), err)
	}
	return p, nil
}

// ExtractParent 用载荷携带的 traceparent 重建上游 OTel 上下文。
//
// 走 launchx.Extract（仓内唯一续链入口，与 launchx.Inject 成对）而不是裸调
// otel 全局传播器：装配期注册的是 otelx/propagation 那套，裸调会在传播器
// 被换掉时静默链接不上——而这种断裂在 Jaeger 上表现为「每个服务都有 span，
// 就是不在一条链上」，极难排查。
//
// tp 为空（dtm 定时补偿 / 重启续推等异步回调）原样返回 ctx：这些段落本就按
// 「新根 + biz.dtm.gid 关联」口径处理，凭空造一个同 ID 的孤儿 span 是自欺。
func ExtractParent(ctx context.Context, tp string) context.Context {
	if tp == "" {
		return ctx
	}
	return launchx.Extract(ctx, carrierOf(tp))
}
