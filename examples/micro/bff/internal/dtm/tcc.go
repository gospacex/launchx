// Package dtm（续）—— 本文件是同一包里的 **TCC** 编排器，与 saga.go 的 Saga
// 编排器并列构成两种事务模式的对照样板（spec: dtm-tcc-presale
// 「TCC 三对分支方法 SHALL 与 Saga 四对分支各自独立建模……SHALL 在同一 bff
// 编排包内以同构的配置、提交、trace 口径实现」）。
//
// 三条与 Saga 不同的边界，先说清再写代码 —— 这三条正是 TCC 与 Saga 语义分岔
// 的地方，混起来就会得到「提交了两遍」或「永远等不到 Confirm」的事故：
//
//  1. **bff 只发 Try，Confirm/Cancel 全部由 dtm 驱动**。官方
//     `dtmgrpc.TccGlobalTransaction` 的实现是 Prepare → 调本文件的 tccFunc →
//     `defer dtmimp.DeferDo(Submit, Abort)`：tccFunc 返回 nil 则 Submit，
//     dtm 拿到 submitted 状态后**自己**逐分支回调 confirm（dtmsvr/
//     trans_type_tcc.go：`op := If(status == submitted, OpConfirm, OpCancel)`
//     再从末尾往前推进）；tccFunc 返回 error 则 Abort，dtm 同样自己逐分支回调
//     cancel。因此本文件的 tccFunc 体内**只允许出现 CallBranch 一次**（发
//     Try），任何"确认阶段再来一次"的写法都是第二次提交同一条全局事务。
//
//  2. **Try 的次序就是 dtm 的 Cancel 次序（逆序）**。TCC 与 Saga 一样把序
//     编在代码顺序里（`tccBranchPlan`），dtm 按 `for current := len(branches)-1;
//     current >= 0; current--` 逆序推进二阶段。故「先点便宜资源」既是业界惯例，
//     也让失败时回退的代价按从贵到便宜反着撤。
//
//  3. **bff 不解析分支结果**。dtm server 从不读分支响应体（trans_status.go 的
//     getGrpcResult 把响应字节整个塞进 *[]byte），只认 gRPC status：Aborted =
//     业务失败（不重试、转 Cancel），其余视作基础失败。与 saga.go 同一条纪律，
//     编排侧只判断「事务提交成功没有」。
//
// 地址纪律（同 saga.go）：dtm server 走 `xmiddleware.dtm.grpc_addr`，分支面走
// `dtm.branches.<domain>`，全部由 app/setup.go 从 config.yaml 读入 —— 代码里
// 连「缺省 127.0.0.1:36790」都不写（examples_addr_hygiene_test.go 的
// TestServiceSourceHasNoHardcodedLoopbackAddr 是硬门禁）。
//
// TCC 涉及的域是三个（stock / points / order），**不含 product**：商品域没有
// 冻结语义，它的 dtm 分支面只有 Saga 的 AddSales / ReduceSales（见 srv-product
// 的 busi.proto），拿扣减接口充当冻结会违反 spec 的「不复用扣减接口充当冻结」。
// 故 TCC 的 validate 只要求这三个域的地址，product 配没配与本链无关。
package dtm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dtm-labs/client/dtmgrpc"
	"github.com/gospacex/logx"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	bffbranch "github.com/gospacex/launchx/examples/micro/bff/internal/dtm/protos"
)

// ---------- 分支方法名 ----------
//
// 与 saga.go 那组（stockDeduct / orderCreateOrder / pointsDebit …）**互不重叠**
// 是 spec 的硬要求（ Saga 四分支与 TCC 三对分支方法名互不重叠、语义互不借用），
// 所以这里另起一组字面量而不复用。守卫在 tcc_test.go 的
// TestTCCBranchMethodsDoNotOverlapSagaMethods：它逐字比对两组常量，出现交集
// 即红 —— 出现交集就是「两条链共用一个分支方法」，那正是 spec 禁止的事。
//
// 注意 order 的 Try 叫 **TryPlace** 而不是 TryFreeze：订单域的 TCC 一阶段不是
// 「冻结一个数」，是「建一张 presale_locked 占位单」（model.DtmTryPlaceBranch）。
// 写成 TryFreeze 会拼出一个不存在的全方法名，dtm 回调得到 Unimplemented，
// 而症状只在 dtm server 日志里可见。
const (
	// stock 的三段。
	stockTryFreeze     = "TryFreeze"
	stockConfirmFreeze = "ConfirmFreeze"
	stockCancelFreeze  = "CancelFreeze"

	// points 的三段（与 stock 同名但不同域 —— 服务全名不同，URL 不会撞）。
	pointsTryFreeze     = "TryFreeze"
	pointsConfirmFreeze = "ConfirmFreeze"
	pointsCancelFreeze  = "CancelFreeze"

	// order 的三段：一阶段建占位单，二阶段转正式单或废单。
	orderTryPlace      = "TryPlace"
	orderConfirmFreeze = "ConfirmFreeze"
	orderCancelFreeze  = "CancelFreeze"
)

// tccDomains TCC 涉及的域（= Config.BranchAddrs 里本链真正会读的键），按 Try 序。
//
// 它是「顺序即决策」的又一处落点：validate 按它点名缺哪个配置键，
// 启动日志按它回显地址。**不含 DomainProduct**，理由见包注释第 3 段。
var tccDomains = []string{DomainStock, DomainPoints, DomainOrder}

// ---------- 可注入依赖 ----------

// TCC 是 dtmgrpc.*TccGrpc 满足的最小面。
//
// 为什么抽接口而不是直接持有 *dtmgrpc.TccGrpc：它是具体类型，且一经构造就
// 持有一条到 dtm server 的真实 gRPC 连接；「Try 分支序 + 三个 URL 是否都填全」
// 是本编排器最容易写错、又最难在真机上分辨的两件事，编排单测必须在不起
// dtm server 的情况下断言它们。
type TCC interface {
	// CallBranch 登记一对分支并发起 Try 阶段调用。
	//
	// confirmURL / cancelURL 是**登记**给 dtm 的二阶段地址，由 dtm 在
	// Submit 之后异步回调 —— bff 不参与。tryURL 是现在就要拨的地址。
	CallBranch(busiMsg proto.Message, tryURL, confirmURL, cancelURL string, reply any, opts ...grpc.CallOption) error
}

// TCCRunner 执行一条全局 TCC 事务（官方 dtmgrpc.TccGlobalTransaction 的形）。
//
// 取成依赖而不是直接调官方函数：官方那个一旦进去就会 Prepare、并在 defer 里
// Submit/Abort，不起 dtm server 的编排单测连「tccFunc 被调了几次、每次
// CallBranch 的三个 URL 是什么」都观察不到（见 package 测试替身一节）。
type TCCRunner func(ctx context.Context, dtmAddr, gid string, fn func(TCC) error) error

// GlobalTCC 默认执行器：官方 dtmgrpc.TccGlobalTransaction。
//
// tccFunc 里只有 Try：Confirm / Cancel 是 dtm 在 Submit 之后驱动的事，编排侧
// 再发一次就是第二次提交同一条全局事务（dtm 侧表现为重复分支、
// TransBranchBarrier 判重后静默跳过，或者干脆把事务推进到意外状态）。
func GlobalTCC(_ context.Context, dtmAddr, gid string, fn func(TCC) error) error {
	return dtmgrpc.TccGlobalTransaction(dtmAddr, gid, func(tcc *dtmgrpc.TccGrpc) error {
		return fn(tcc)
	})
}

// TCCConfig TCC 编排器的全部外部依赖。
//
// 与 saga.go 的 Config 同构：每个字段都有「配错了会怎样」的判据（见各自
// 注释），位置参数会把这些判据挤到调用点一次性丢掉。
type TCCConfig struct {
	// DTMAddr dtm server 的 gRPC 地址（xmiddleware.dtm.grpc_addr）。与 Saga
	// 共用同一个键：同一台 dtm server 同时服务两条链，分开配只会让两份配置
	// 指向两台 server，而分支 URL 全在另一台机器上。
	DTMAddr string
	// BranchAddrs 分支面地址（dtm.branches.<domain>）。本链只读 tccDomains
	// 三个键；product 的取值会被忽略（商品域不参与 TCC）。
	BranchAddrs map[string]string
	// GenGID 生成全局事务号。零值取 ServerGID（向 dtm server 要，panic 已
	// recover 成 error）。
	GenGID GIDFunc
	// Traceparent 取续链串。零值取 TraceparentFromCtx。
	Traceparent TraceparentFunc
	// BranchURL 拼分支回调 URL。零值取 JoinBranchURL（**不带 scheme**）。
	BranchURL URLBuilder
	// RunTCC 执行全局事务。零值取 GlobalTCC（真实 dtm 客户端）。
	RunTCC TCCRunner
	// Logger 提交失败的 Warn 出口。零值取 logx 全局。
	Logger logx.Logger
}

// TCCOrchestrator 预售锁定的 TCC 编排器。并发安全：只读依赖，无内部状态。
type TCCOrchestrator struct {
	dtmAddr  string
	branches map[string]string
	genGID   GIDFunc
	tp       TraceparentFunc
	url      URLBuilder
	run      TCCRunner
	logger   logx.Logger
}

// NewTCC 构造编排器。配置缺失时返回 error，由调用方按 strict.dtm 裁决失败
// 姿态（缺省 false → Warn + UnavailableTCC，进程不退出）。
func NewTCC(c TCCConfig) (*TCCOrchestrator, error) {
	o := &TCCOrchestrator{
		dtmAddr:  c.DTMAddr,
		branches: make(map[string]string, len(tccDomains)),
		genGID:   c.GenGID,
		tp:       c.Traceparent,
		url:      c.BranchURL,
		run:      c.RunTCC,
		logger:   c.Logger,
	}
	if o.genGID == nil {
		o.genGID = ServerGID
	}
	if o.tp == nil {
		o.tp = TraceparentFromCtx
	}
	if o.url == nil {
		o.url = JoinBranchURL
	}
	if o.run == nil {
		o.run = GlobalTCC
	}
	if o.logger == nil {
		o.logger = logx.GetGlobal()
	}
	// 拷一份而不是直接持有 c.BranchAddrs（同 saga.go）：调用方复用自己的 map
	// 就能在运行期改掉已构造好的编排器，而 tccBranchPlan 的序是编译期常量、
	// 地址却是运行期值，两者错配时没有任何迹象。
	for _, d := range tccDomains {
		o.branches[d] = c.BranchAddrs[d]
	}
	if err := o.validate(); err != nil {
		return nil, err
	}
	return o, nil
}

// UnavailableTCC 造一个「恒提交失败」的编排器。
//
// 与 saga.go 的 Unavailable 同理由：dtm 配错时进程仍必须起来
// （strict.dtm: false 的口径）。让编排器**可构造但提交恒失败**，失败点收敛成
// 一个有明确文案的位置，而不是让每个入口各判一次 nil。
func UnavailableTCC(cause error) *TCCOrchestrator {
	if cause == nil {
		cause = errors.New("dtm 编排不可用（原因未给出）")
	}
	return &TCCOrchestrator{
		genGID: func(context.Context, string) (string, error) { return "", cause },
		tp:     func(context.Context) string { return "" },
		url:    JoinBranchURL,
		logger: logx.GetGlobal(),
	}
}

// validate 配置判据。缺地址 = 拼出来的 URL 是半截，dtm server 会拿它去拨号
// 然后静默失败 —— 与其让它在真机上表现成「三个分支全 404」，不如在装配期
// 把配置缺口说出来。
//
// 只查 tccDomains 三个域：product 不参与本链，把它算进来会让一条与预售无关的
// 配置缺口把整条锁单链挡在装配期之外（表现为「商品域地址没配 → 预售也不可用」）。
func (o *TCCOrchestrator) validate() error {
	if o.dtmAddr == "" {
		return errors.New("dtm server 地址未配置（xmiddleware.dtm.grpc_addr）")
	}
	var missing []string
	for _, d := range tccDomains {
		if o.branches[d] == "" {
			missing = append(missing, KeyBranchPrefix+d)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("dtm 分支面地址未配置：%v", missing)
	}
	return nil
}

// ---------- 锁定命令 ----------

// LockCmd 一次预售锁定在编排层的输入。
//
// 为什么与 saga.go 的 OrderCmd 是两个类型而不是共用一个：spec 要求两条链
// 「各自独立建模」。共用一个命令类型意味着改一边会静默改掉另一边 —— 而 TCC
// 的载荷要带 dtm_gid、要建占位单，Saga 的载荷不带这些，两者的演进节奏本来
// 就不同。字段重合是刻意的（同一批资源的两种冻结方式），类型分开是纪律。
type LockCmd struct {
	// UserID 锁定用户（鉴权身份，非请求体自报）。
	UserID int64
	// ProductID 商品。
	ProductID int64
	// Quantity 锁定数量。
	Quantity int32
	// OrderNo 业务单号，bff 生成。库存/订单/积分三域的幂等键与对账锚点，
	// 且是积分域派生命名空间键的那一段（model.DtmBiz.OrderNo 注释）。
	OrderNo string
	// Title 商品标题（预检取回，进订单明细）。
	Title string
	// UnitPrice 单价（分）。订单域 validateDtmCmd 拒 unit_price < 0。
	UnitPrice int64
	// PointsCost 本单要冻结的积分数。**恒为正**：积分域 validateDtmBiz 拒
	// cost<=0，而 0 在那里不是「不冻结积分」而是「这笔分支必然失败并把整条
	// 事务拖去 Cancel」。
	PointsCost int64
	// FailInject 演示用：点名一个分支方法名（TryFreeze / TryPlace），该分支
	// 直接返业务失败，用来点火 dtm 的逆序 Cancel 链。真机恒为空。
	FailInject string
}

// Total 本单总额（分）。口径与订单域 model.DtmOrderCmd.Total() 逐字相同
// （Σ quantity × unit_price，服务端算）—— 与 saga.go 同一条不信任调用方的
// 账本纪律。
func (c LockCmd) Total() int64 { return int64(c.Quantity) * c.UnitPrice }

// Validate 编排前置校验。
//
// 判据全部来自各域分支的 validateDtmCmd / validateDtmBiz，理由与 saga.go
// 同一个：把「一定失败」挡在事务之外，比在事务里失败再 Cancel 回来更省事也
// 更可信（后者要付三个域的冻结与解冻两轮写）。
func (c LockCmd) Validate() error {
	switch {
	case c.UserID <= 0:
		return errors.New("user_id required")
	case c.ProductID <= 0:
		return errors.New("product_id required")
	case c.Quantity <= 0:
		return errors.New("quantity must be positive")
	case c.OrderNo == "":
		return errors.New("order_no required（幂等键与跨域对账锚点）")
	case c.UnitPrice < 0:
		return errors.New("unit_price must not be negative")
	case c.PointsCost <= 0:
		return errors.New("points cost must be positive（积分域分支拒 cost<=0）")
	}
	return nil
}

// ---------- 分支载荷 ----------
//
// 三个业务体与 saga.go 同名的那几个**逐字相同**（同一批 DTO 的同一份 JSON），
// 但这里各自重新声明而不是复用 saga.go 的 stockBiz / orderBiz / pointsBiz：
// 同上，两条链的载荷要能各自漂移。守卫在 tcc_test.go 的
// TestTCCBizBodiesMatchDomainDTOs —— 它对着各域 model 的 DTO 逐字段核对。

// tccStockBiz 库存域 TCC 分支业务体，对齐 srv-stock/internal/model.DtmStockCmd。
type tccStockBiz struct {
	OrderNo   string `json:"order_no"`
	ProductID uint64 `json:"product_id"`
	Quantity  int32  `json:"quantity"`
	UserID    uint64 `json:"user_id,omitempty"`
}

// tccOrderBiz 订单域 TCC 分支业务体，对齐 srv-order/internal/model.DtmOrderCmd。
//
// **DtmGID 必填**：占位单与它的补偿（ConfirmFreeze / CancelFreeze）都按
// `order_no = ? and dtm_gid = ?` 定位，缺了它 Confirm 找不到单（订单域
// validateDtmCmd 直接判业务失败，整条事务被 Cancel）。
type tccOrderBiz struct {
	UserID  uint64         `json:"user_id"`
	OrderNo string         `json:"order_no"`
	DtmGID  string         `json:"dtm_gid"`
	Items   []tccOrderItem `json:"items"`
	// Traceparent 落 outbox.traceparent：Confirm 写 outbox 时早已脱离本次
	// ctx（那是 dtm 异步回调的一跳），只能靠行里存下的这一串续。
	Traceparent string `json:"traceparent,omitempty"`
}

// tccOrderItem 订单域分支载荷里的单条明细。
//
// **键名逐字取 srv-order 的 model.OrderItemCmd 字段名，不是下划线写法**：那个
// 结构体没有 json tag，encoding/json 只能按字段名（或其大小写不敏感变体）
// 匹配，而 EqualFold("product_id", "ProductID") 为 false（多一个下划线）。
// 载荷若写成下划线风格，分支侧解出来就是零值明细，订单域以 "invalid item"
// 判业务失败。守卫见 tcc_test.go 的 TestTCCOrderItemKeysMatchDomainStruct。
type tccOrderItem struct {
	ProductID uint64
	Name      string
	Quantity  int
	UnitPrice int64
}

// tccPointsBiz 积分域 TCC 分支业务体，对齐 srv-points/internal/model.DtmBiz。
type tccPointsBiz struct {
	UserID  int64  `json:"user_id"`
	Cost    int64  `json:"cost"`
	OrderNo string `json:"order_no"`
}

// ---------- 分支表 ----------

// tccBranchSpec 一个分支的寻址与载荷。
type tccBranchSpec struct {
	domain  string
	service string
	try     string
	confirm string
	cancel  string
	// biz 该分支的业务体。Try / Confirm / Cancel 三段**共用同一份载荷**：
	// dtm 侧的 svcRegisterBranch 对 TCC 只构造两条 TransBranch（cancel 与
	// confirm），两者由同一个 *TransBranch 克隆而来，BusiPayload 是同一份字节
	// （dtm v1.19.0 dtmsvr/api.go）。拆成三个函数只会让字段在三处漂移。
	biz func(cmd LockCmd, gid, traceparent string) any
}

// tccBranchPlan 三对 TCC 分支的编排表。
//
// **顺序是这份代码里唯一关于「先锁什么后锁什么」的决策**，且它必须是代码顺序：
// dtm 按 CallBranch 的调用序**逆序**推进二阶段（dtmsvr/trans_type_tcc.go）。
// 把序写成数据再排序等于把决策挪到排序函数里，而排序函数的正确性没有任何
// 单测能覆盖。
//
// 序为 stock.TryFreeze → points.TryFreeze → order.TryPlace：
//   - 先点便宜资源（库存是一行条件更新，积点是账户冻结，占位单要插三张表
//     且带唯一键），失败概率与失败代价都递增；
//   - order 垫底：它是唯一会产生「业务可见痕迹」（一张 presale_locked 占位单）
//     的分支，放在最后意味着只有前两域都锁成了才建单，占位单的生命周期最短；
//   - 前两域任一失败时 order 还没建，Cancel 只需回退两处冻结 —— 代价最低。
//
// 反向探针（实测过，见 tcc_test.go 注释）：把 order 段挪到 stock 段之前，
// TestTCCTryOrderIsStockPointsOrder 红。
func tccBranchPlan() []tccBranchSpec {
	return []tccBranchSpec{
		{
			domain: DomainStock, service: stockService,
			try: stockTryFreeze, confirm: stockConfirmFreeze, cancel: stockCancelFreeze,
			biz: func(cmd LockCmd, _, _ string) any {
				return tccStockBiz{
					OrderNo:   cmd.OrderNo,
					ProductID: uint64(cmd.ProductID),
					Quantity:  cmd.Quantity,
					UserID:    uint64(cmd.UserID),
				}
			},
		},
		{
			domain: DomainPoints, service: pointsService,
			try: pointsTryFreeze, confirm: pointsConfirmFreeze, cancel: pointsCancelFreeze,
			biz: func(cmd LockCmd, _, _ string) any {
				return tccPointsBiz{
					UserID:  cmd.UserID,
					Cost:    cmd.PointsCost,
					OrderNo: cmd.OrderNo,
				}
			},
		},
		{
			domain: DomainOrder, service: orderService,
			try: orderTryPlace, confirm: orderConfirmFreeze, cancel: orderCancelFreeze,
			biz: func(cmd LockCmd, gid, traceparent string) any {
				return tccOrderBiz{
					UserID:  uint64(cmd.UserID),
					OrderNo: cmd.OrderNo,
					DtmGID:  gid,
					Items: []tccOrderItem{{
						ProductID: uint64(cmd.ProductID),
						Name:      cmd.Title,
						Quantity:  int(cmd.Quantity),
						UnitPrice: cmd.UnitPrice,
					}},
					Traceparent: traceparent,
				}
			},
		},
	}
}

// TCCMethods 一个分支的三段方法名（URL 的末段，如 TryFreeze）。
//
// 单独带出来而不是让调用方从 URL 里切字符串：调用方要的是「名字」，切出来的
// 名字在 URL 配错时仍是一个看起来合理的值（`.../TryFreez`），而名字正是提交
// 响应里最该被一眼核对的东西。
type TCCMethods struct {
	Try     string
	Confirm string
	Cancel  string
}

// TCCStep 一个已构造好的分支（三段 URL + 三段方法名 + 载荷）。
type TCCStep struct {
	Domain string
	// Try / Confirm / Cancel 三个 URL，原样交 dtm server：
	// Try 现在拨，Confirm / Cancel 只是**登记**（dtm 在 Submit 之后自己拨）。
	// 三者都是 `<host:port>/<proto-service>/<method>`，不带 scheme。
	Try     string
	Confirm string
	Cancel  string
	Methods TCCMethods
	Payload *bffbranch.BusiReq
}

// TCCResult 一次提交的结果。
type TCCResult struct {
	// GID 全局事务号。调用方拿它去 dtm server 查事务状态/分支明细。
	GID string
	// Steps 实际提交的分支序列（提交成功后才有意义）。
	Steps []TCCStep
}

// Lock 构造并提交三对 TCC 分支。
//
// 返回的 error **只表示「事务没提交成功」**，不表示「冻结没发生」——
// TCC 的二阶段是异步的，返回那一刻三个域一个分支都还没跑完。三种失败必须
// 分清，故 fail 的文案逐条写明后果：
//
//   - gid 拿不到 / Prepare 失败：事务**未建立**，四域零写入，调用方可原样重试；
//   - 任一 Try 业务失败：dtm 已 Abort，正在逆序 Cancel 已冻结的两处，**此刻
//     不要立刻重试**（冻结还在退）；
//   - Submit 失败：事务停在 prepared，dtm cron 会在 TimeoutToFail 后把它 abort。
//
// 三档文案**既进 Warn 日志也进返回的 error**（见 fail 的注释），故 handler
// 原样上抛时调用方就能读到该做什么 —— 这是与 saga.go 的一处刻意分歧。
//
// 失败后不回落到任何手动编排链：TCC 的 Cancel 是 dtm 发起的，编排侧再来一次
// 解冻就是两套补偿并存。
func (o *TCCOrchestrator) Lock(ctx context.Context, cmd LockCmd) (TCCResult, error) {
	if err := cmd.Validate(); err != nil {
		// 命令非法属于调用方错误（4xx），不是 dtm 故障，不打 Warn。
		return TCCResult{}, fmt.Errorf("dtm tcc: 锁定命令非法: %w", err)
	}
	gid, err := o.genGID(ctx, o.dtmAddr)
	if err != nil {
		return TCCResult{}, o.fail(ctx, gid, "生成全局事务号",
			"全局事务未建立，四域没有任何写入；调用方可原样重试", err)
	}
	steps, err := o.buildSteps(ctx, cmd, gid)
	if err != nil {
		return TCCResult{}, err
	}

	// 两枚记账位，用来把「事务建了没有」从错误文案里区分出来。官方
	// TccGlobalTransaction 的 Prepare 在 tccFunc 之前、Abort/Submit 在 defer
	// 里：prepared 翻 true 就说明 dtm server 已经知道这条 gid 了。
	var (
		prepared bool
		tryErr   error
	)
	err = o.run(ctx, o.dtmAddr, gid, func(tcc TCC) error {
		prepared = true
		for _, s := range steps {
			// reply 必须是**非 nil 的 proto 消息**：grpc v1.83 的 proto codec
			// 对 nil 直接报 "failed to unmarshal, message is <nil>"。它只是个
			// 合法的反序列化目标 —— dtm server 从不读分支响应体（只看 gRPC
			// status），故这里的回包内容不参与任何判断。
			reply := &bffbranch.BusiResp{}
			if berr := tcc.CallBranch(s.Payload, s.Try, s.Confirm, s.Cancel, reply); berr != nil {
				tryErr = berr
				return berr
			}
		}
		return nil
	})
	if err != nil {
		switch {
		case !prepared:
			// Prepare 失败：gid 在 dtm server 侧不存在，什么都没建。
			return TCCResult{}, o.fail(ctx, gid, "准备全局事务",
				"全局事务未建立，四域没有任何写入；调用方可原样重试", err)
		case tryErr != nil:
			// tccFunc 返回 error → defer 走 Abort（dtmimp.DeferDo 在这条路上
			// **保留**原 rerr、丢弃 Abort 自身的返回值，所以这里的 err 就是那个
			// Try 分支的失败）。三段二阶段里跑的是 cancel，且是逆序。
			return TCCResult{}, o.fail(ctx, gid, "Try 阶段",
				"dtm 已 Abort，正在逆序 Cancel 已冻结的分支（三域仍在解冻途中；"+
					"请勿立刻重试，先拿 gid 确认事务已 failed）", err)
		default:
			// tccFunc 全绿、Submit 失败：事务停在 prepared，等 dtm cron 超时后 abort。
			return TCCResult{}, o.fail(ctx, gid, "提交全局事务",
				"三个 Try 已冻结，dtm 尚未收到 Submit；它会在超时后 Abort 并解冻，"+
					"请勿立刻重试，先用 gid 确认事务已 failed", err)
		}
	}
	return TCCResult{GID: gid, Steps: steps}, nil
}

// buildSteps 按 tccBranchPlan 构造分支序列与载荷。抽出来是为了让单测能在不
// 构造任何 dtm 客户端的情况下断言「序 + 三段 URL + 载荷」—— 那正是本编排器
// 最容易写错、又最难在真机上分辨的三件事。
func (o *TCCOrchestrator) buildSteps(ctx context.Context, cmd LockCmd, gid string) ([]TCCStep, error) {
	tp := o.tp(ctx)
	plan := tccBranchPlan()
	steps := make([]TCCStep, 0, len(plan))
	for _, b := range plan {
		biz, err := json.Marshal(b.biz(cmd, gid, tp))
		if err != nil {
			return nil, fmt.Errorf("dtm tcc: 构造 %s 分支载荷: %w", b.domain, err)
		}
		steps = append(steps, TCCStep{
			Domain:  b.domain,
			Try:     o.url(o.branches[b.domain], "/"+b.service+"/"+b.try),
			Confirm: o.url(o.branches[b.domain], "/"+b.service+"/"+b.confirm),
			Cancel:  o.url(o.branches[b.domain], "/"+b.service+"/"+b.cancel),
			Methods: TCCMethods{Try: b.try, Confirm: b.confirm, Cancel: b.cancel},
			Payload: &bffbranch.BusiReq{
				Biz:         string(biz),
				Traceparent: tp,
				FailInject:  cmd.FailInject,
			},
		})
	}
	return steps, nil
}

// fail 提交类失败的统一出口：Warn 带后果文案，然后原样上抛。
//
// 与 saga.go 的 fail 有**一处刻意的不同**：这里的后果文案也进了返回的 error，
// 而 saga 只打进日志。理由是 TCC 的三种失败在调用方那里的处置动作不同（只有
// 事务未建立可以原样重试，另两种必须先等冻结退干净），而本层上抛的 error 会
// 被 handler 原样渲进 5xx 响应体 —— 那是调用方唯一能看到的地方。把它留在日志
// 里等于让最需要它的人（重试决策的发起方）看不到它。
//
// 另外两处仍与 saga 相同：Warn 用固定措辞便于日志检索；底层错误用 %w 透传，
// 便于单测 errors.Is 与上层按需取码。
func (o *TCCOrchestrator) fail(ctx context.Context, gid, stage, consequence string, err error) error {
	o.logger.WarnContext(ctx, "bff: dtm tcc "+stage+"失败（"+consequence+"）",
		logx.String("dtm_addr", o.dtmAddr),
		logx.String("gid", gid),
		logx.String("branch_order", DescribeTCCBranchOrder()),
		logx.Err(err),
		logx.String("hint", "确认 dtm server 在监听 xmiddleware.dtm.grpc_addr，且各域分支面在 dtm.branches.<domain>"))
	return fmt.Errorf("dtm tcc: %s失败（%s）: %w", stage, consequence, err)
}
