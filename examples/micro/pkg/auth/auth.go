// Package auth 微服形态的边界鉴权与身份透传。
//
// # 威胁模型与信任边界（先说清"防什么"，否则加出来的中间件只是仪式）
//
// 微服形态有两类入口，攻击面不同，防线也不同：
//
//   - HTTP 面（gateway:8080 / bff:8081）：**不可信**。
//     任何能连到端口的人都能构造请求，故必须验凭据。
//   - RPC 面（四个 srv，9021-9024）：**仅内网可达**。
//     它们不验凭据，但也不接受"裸调" —— 只接受由边界层签发并透传下来的身份。
//
// 因此本包分两半，且刻意**不在 srv 侧重复验签**：
//
//	HTTP 边界（bff）        验凭据 → 签发身份票据 → 随 RPC 透传
//	RPC 边界（四个 srv）    只校验票据"存在且合法"（签发方、主体、时效）
//
// 为什么 srv 不各验一次签：四个域服 × 每个请求一次缓存查询，等于把一次鉴权
// 放大成五次，且任何一处缓存抖动都会让四个域同时 401 —— 那是把中间件的故障
// 放大成整个系统的不可用。边界信任模型下，内网那一段的可信性由网络与部署保证，
// 不由每个服务各自再验一次来保证。
//
// # 为什么用 kitex metainfo 而不是在 IDL 里加字段
//
// 身份是**传输元数据**，不是业务参数。塞进 IDL 会让每个方法的请求体都多出
// 一个字段，业务层被迫在每次调用里搬运它 —— 搬运就会漏，漏了就裸奔。
// metainfo 由 kitex 的客户端/服务端 MetaHandler 自动序列化与还原
// （`internal/client/option.go` 与 `internal/server/option.go` 默认各注册一个），
// 业务层完全不需要参与搬运。
//
// # 为什么是 persistent 而不是 transient
//
// metainfo 的 transient 值按"单跳"语义传播，persistent 值才会被继续往下游串。
// bff → srv 只有一个跳，两者此刻等价；但一旦链上多出一跳（srv → srv），
// transient 会在第一跳就被吃掉，表现为"下游突然都 401 了"，且只在加跳之后才暴露。
// 用 persistent 让"身份能不能传下去"这个问题与链的长度无关。
package auth

import (
	"context"
	"strconv"
	"time"

	"github.com/bytedance/gopkg/cloud/metainfo"
)

// metainfo 键名（集中一处定义，签发与校验两侧才不会拼法不一致）。
//
// 前缀 `hubx-` 是刻意的：metainfo 是一个扁平的全局命名空间，不加前缀迟早会
// 和框架或别的中间件撞键，而撞键的表现是"身份偶尔失效"，极难反查。
const (
	MetaKeyUserID   = "hubx-auth-user-id"
	MetaKeyOperator = "hubx-auth-operator"
	MetaKeyIssuedAt = "hubx-auth-issued-at"
	MetaKeyIssuer   = "hubx-auth-issuer"
)

// IssuerBFF 身份的签发方标识：只有边界层（bff）签发的身份才被域服接受。
//
// 为什么要这一个字段：没有它，任何人只要知道 metainfo 的键名就能自己填一个
// user_id 直连 9021 —— 防线等于没有。有了它，域服至少能回答"这个身份是谁签的"，
// 伪造者要同时伪造签发方与主体。它防不住内网里的恶意调用（那需要 mTLS），
// 但能挡住绝大多数"随手造一个请求"的场景。
const IssuerBFF = "demo-bff"

// DefaultTicketTTL 身份票据的默认有效期。
//
// 短 TTL 是**时效**防线：即便一个身份被泄露，它的可用窗口也有限。
// 取 5 分钟而不是更长：这是一次 RPC 调用链的长度量级，不涉及会话保持；
// 取更短则会让跨时钟偏移的部署出现"刚签就过期"，那种故障表现是随机 401。
const DefaultTicketTTL = 5 * time.Minute

// Identity 一次调用的身份。
//
// OperatorID 是"操作者"（商品/库存域的写入口要求它），UserID 是"归属者"。
// 两者分开是因为它们回答的不是同一个问题：谁在调用 vs 这条数据属于谁。
// 类型与 IDL 里的 operator_id 对齐（int64），免得层间来回换算。
type Identity struct {
	UserID     int64  // 归属用户（0 表示未携带）
	OperatorID int64  // 操作者（读路径可为 0）
	Issuer     string // 签发方，空表示未经签发
	IssuedAt   int64  // 签发时刻（Unix 秒）
}

// Valid 判定身份是否可被下游接受：签发方正确、有主体、且在时效内。
//
// 三个判据缺一不可，但严格度不同：
//   - 签发方：硬判据，错了一律拒（拒的是"谁签的"）；
//   - 主体：至少一个（user 或 operator），否则这条调用没有任何归属，
//     日志与限流都无从下手；
//   - 时效：软判据 —— 时钟偏移在机房内通常 <1s，故容忍 60s 的未来时间，
//     超过一律拒（拒的是"重放一个旧身份"）。
func (id Identity) Valid(now time.Time, ttl time.Duration) bool {
	if id.Issuer != IssuerBFF {
		return false
	}
	if id.UserID <= 0 && id.OperatorID <= 0 {
		return false
	}
	if id.IssuedAt <= 0 {
		return false
	}
	ts := time.Unix(id.IssuedAt, 0)
	if ts.After(now.Add(time.Minute)) { // 容忍 60s 前向时钟偏移
		return false
	}
	if ttl > 0 && now.Sub(ts) > ttl {
		return false
	}
	return true
}

// String 身份的可读形式（进日志与 span 属性，别用 %v 打结构体）。
//
// 刻意不打全字段：IssuedAt / Issuer 属于凭证元数据，打进日志只会让"谁在调用"
// 这一行变得难读；排障要看它们时是另一回事（验签失败），有专门的拒绝日志。
func (id Identity) String() string {
	if id.OperatorID > 0 {
		return "user=" + strconv.FormatInt(id.UserID, 10) + " operator=" + strconv.FormatInt(id.OperatorID, 10)
	}
	return "user=" + strconv.FormatInt(id.UserID, 10)
}

// ctxKey 进程内承载身份的 context 键（本包私有，外部取不到也改不了）。
type ctxKey struct{}

// WithIdentity 把身份挂到 ctx 上：**同时**写进程内键与 metainfo 持久值。
//
// 写两处不是冗余：进程内键给本进程的分层代码读（handler / service），
// metainfo 给 kitex 序列化出去（跨进程）。只写前者，下游收不到；只写后者，
// 本进程想读一次身份还得走一遍 metainfo 的字符串解析。
func WithIdentity(ctx context.Context, id Identity) context.Context {
	ctx = context.WithValue(ctx, ctxKey{}, id) //nolint:staticcheck // 本包私有键，不会与外部冲突
	return Inject(ctx, id)
}

// Inject 只写 metainfo（供"身份已在本进程、只想让它随 RPC 出去"的场景）。
func Inject(ctx context.Context, id Identity) context.Context {
	ctx = metainfo.WithPersistentValue(ctx, MetaKeyUserID, strconv.FormatInt(id.UserID, 10))
	ctx = metainfo.WithPersistentValue(ctx, MetaKeyOperator, strconv.FormatInt(id.OperatorID, 10))
	ctx = metainfo.WithPersistentValue(ctx, MetaKeyIssuedAt, strconv.FormatInt(id.IssuedAt, 10))
	ctx = metainfo.WithPersistentValue(ctx, MetaKeyIssuer, id.Issuer)
	return ctx
}

// IdentityFrom 取当前调用的身份；返回 false 表示**没有**身份（未鉴权或透传断裂）。
//
// 读不到不等于不合法 —— 合法性由 Valid 单独判。两者分开是因为失败原因不同：
// 前者要报"边界没鉴权"，后者要报"身份过期/伪造"，排障时不能混成一个 401。
func IdentityFrom(ctx context.Context) (Identity, bool) {
	if id, ok := ctx.Value(ctxKey{}).(Identity); ok {
		return id, true
	}
	return Extract(ctx)
}

// Extract 只从 metainfo 还原（RPC 服务端侧的路径）。
//
// 缺任何一个键都按"没有身份"处理而不是按零值放行：半张票据（比如只有 user_id
// 没有 issuer）正是伪造者最容易造出来的形态，放行它就等于把防线降到"填个数字"。
func Extract(ctx context.Context) (Identity, bool) {
	// 三个键都是**必需**的，缺任一即按"没有身份"处理。
	// 只有 operator 可选（读路径不要求操作者）。
	rawUser, okUser := metainfo.GetPersistentValue(ctx, MetaKeyUserID)
	issuer, okIssuer := metainfo.GetPersistentValue(ctx, MetaKeyIssuer)
	rawAt, okAt := metainfo.GetPersistentValue(ctx, MetaKeyIssuedAt)
	if !okUser || !okIssuer || !okAt {
		return Identity{}, false
	}
	userID, err := strconv.ParseInt(rawUser, 10, 64)
	if err != nil {
		return Identity{}, false
	}
	issuedAt, err := strconv.ParseInt(rawAt, 10, 64)
	if err != nil {
		return Identity{}, false
	}
	id := Identity{UserID: userID, Issuer: issuer, IssuedAt: issuedAt}
	if v, ok := metainfo.GetPersistentValue(ctx, MetaKeyOperator); ok {
		id.OperatorID, _ = strconv.ParseInt(v, 10, 64)
	}
	return id, true
}

// New 签一份身份票据（由边界层在鉴权通过后调用）。
//
// 签发时刻取当前时间而不是由调用方传：让"什么时候签的"这件事只有一个来源，
// 调用方无法伪造一个未来时间把票据永久化。
func New(userID, operatorID int64, now time.Time) Identity {
	return Identity{
		UserID:     userID,
		OperatorID: operatorID,
		Issuer:     IssuerBFF,
		IssuedAt:   now.Unix(),
	}
}
