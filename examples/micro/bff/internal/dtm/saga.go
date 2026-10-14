// Package dtm 是 bff 的 dtm 事务编排面：把「一次下单」编成一条 dtm Saga 提交给
// dtm server，由 dtm 负责推进四个分支、失败时逆序补偿。
//
// 与被它替换掉的手动编排相比，职责边界发生了三处变化，改动时按这三条对齐：
//
//  1. **bff 不再持有补偿语义**。手动链里是 bff 自己判「订单没建成 → 调一次
//     ReleaseStock」，那只覆盖了编排代码里能看到的失败；Saga 下补偿由 dtm 按
//     已成功分支**逆序**发起，覆盖面是「已经落库的全部前序分支」。因此本包
//     没有任何补偿调用 —— 编排侧一旦再出现 ReleaseStock，就是新旧两套补偿
//     并存，会重复回补。
//
//  2. **bff 不再解析分支结果**。dtm 回调用 raw codec，dtm server 从不解析
//     响应体；业务失败由各域分支面翻成 gRPC status（Aborted = 不重试、转补偿，
//     见 design.md D7a）。所以编排侧不判断分支成败，只判断「事务提交成功没有」，
//     其余交给 dtm 与分支面。
//
//  3. **bff 不再同步拿业务结果**。Saga 提交是异步的，Submit 返回时四个分支
//     一个都还没跑完。因此编排器只回 gid，不回 order_id / reservation_id。
//
// 地址纪律（examples_addr_hygiene_test.go 的 TestServiceSourceHasNoHardcodedLoopbackAddr
// 是硬门禁）：本包出现的每一个地址都来自 Config 字段，由 app/setup.go 从
// config.yaml 读入 —— dtm server 走 `xmiddleware.dtm.grpc_addr`，四个分支面走
// `dtm.branches.<domain>`。代码里连「缺省 127.0.0.1:36790」都不写：把缺省放进
// 配置文件，改部署形态只动配置；放进代码就要重新编译，且门禁会红。
package dtm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/dtm-labs/client/dtmgrpc"
	"github.com/gospacex/logx"
	otelprop "go.opentelemetry.io/otel/propagation"
	"google.golang.org/protobuf/proto"

	"github.com/gospacex/dtmx"
	"github.com/gospacex/launchx"
	bffbranch "github.com/gospacex/launchx/examples/micro/bff/internal/dtm/protos"
)

// ---------- 分支寻址 ----------
//
// 服务全名 = 各域 busi.proto 的 proto package + service 名，逐字对齐
// srv-*/internal/dtm/protos/busi_grpc.pb.go 里的 *_FullMethodName 常量。
// 一个字符对不上，dtm server 的 gRPC 调用就是 Unimplemented（分支 404），
// 而症状只出现在 dtm server 日志里，本进程毫无迹象。
//
// 这组字面量的守卫在 saga_test.go 的
// TestBranchServiceNamesMatchDomainFullMethodNames：它直接读四域 pb.go 的
// FullMethodName 常量比对，不是自我声明。
const (
	stockService   = "dtmbranch.stock.StockBusi"
	orderService   = "dtmbranch.order.OrderBusi"
	productService = "dtmbranch.product.ProductBusi"
	pointsService  = "dtmbranch.points.PointsBusi"
)

// 分支方法名（= 各域 model 里的 DtmXxxBranch 常量，fail_inject 按它点火）。
const (
	stockDeduct  = "Deduct"
	stockRelease = "Release"

	orderCreateOrder = "CreateOrder"
	orderCancelOrder = "CancelOrder"

	productAddSales    = "AddSales"
	productReduceSales = "ReduceSales"

	pointsDebit  = "Debit"
	pointsRefund = "Refund"
)

// 域键（Config.BranchAddrs 的键）。用具名字符串而不是结构体字段索引：
// 装配层从配置取地址时按域键查，键名拼错的表现是「某个域的地址取空」，
// 那会在构造期被 Validate 拦下，而不是悄悄编出一个少一条分支的 URL。
const (
	DomainStock   = "stock"
	DomainOrder   = "order"
	DomainProduct = "product"
	DomainPoints  = "points"
)

// 四个分支服务名，按域键索引（与 BranchAddrs 同一套键）。
var serviceOfDomain = map[string]string{
	DomainStock:   stockService,
	DomainOrder:   orderService,
	DomainProduct: productService,
	DomainPoints:  pointsService,
}

// ---------- 可注入依赖 ----------

// GIDFunc 生成全局事务号。
//
// 抽成依赖而不是在 Submit 里直接调官方 MustGenGid：一是官方那个用 panic 表达
// 不可达（见 ServerGID），而本仓对 dtm 的失败姿态是「5xx + Warn 带病运行」，
// 崩进程是错的；二是不起 dtm server 就没法跑编排单测。
type GIDFunc func(ctx context.Context, dtmAddr string) (string, error)

// TraceparentFunc 取本次请求要编进分支载荷的 traceparent。
//
// 空串不是异常：没有活跃 span（如未装 telemetry 或本仓关闭 trace）时就是空，
// 各域分支面 dtmx.ExtractParent 对空值原样放行，按「新根 trace」记录。
type TraceparentFunc func(ctx context.Context) string

// URLBuilder 由分支面地址 + gRPC 全方法名拼出 dtm 的分支回调 URL。
type URLBuilder func(addr, fullMethod string) string

// Saga 是 dtmgrpc.SagaGrpc 满足的最小面。
//
// 为什么要这层而不是直接持有 *dtmgrpc.SagaGrpc：官方的 Add 返回 *SagaGrpc
// 自身（链式），签名与本接口不一致；更重要的是它是具体类型且内部持有一条
// 到 dtm server 的真实 gRPC 连接，编排单测没法在不起 dtm server 的情况下
// 断言「Add 的序列」。
type Saga interface {
	// Add 追加一个分支（正向 action + 逆向 compensate + 载荷）。
	Add(action, compensate string, payload proto.Message) Saga
	// Submit 把整条 Saga 提交给 dtm server。
	Submit() error
}

// SagaFactory 构造一条待提交的 Saga。
type SagaFactory func(dtmAddr, gid string) Saga

// Config 编排器的全部外部依赖。
//
// 全部做成字段而不是散在 New 的位置参数里：每个字段都有「配错了会怎样」的
// 判据（见各自注释），位置参数会把这些判据挤到调用点一次性丢掉。
type Config struct {
	// DTMAddr dtm server 的 gRPC 地址（xmiddleware.dtm.grpc_addr）。
	DTMAddr string
	// BranchAddrs 四域分支面地址（dtm.branches.<domain>）。
	BranchAddrs map[string]string
	// GenGID 生成全局事务号。零值取 ServerGID。
	GenGID GIDFunc
	// Traceparent 取续链串。零值取 TraceparentFromCtx。
	Traceparent TraceparentFunc
	// BranchURL 拼分支回调 URL。零值取 JoinBranchURL。
	BranchURL URLBuilder
	// NewSaga 构造 Saga。零值取 NewGRPCSaga（真实 dtm 客户端）。
	NewSaga SagaFactory
	// Logger 提交失败的 Warn 出口。零值取 logx 全局。
	Logger logx.Logger
}

// Orchestrator 下单 Saga 的编排器。并发安全：只读依赖，无内部状态。
type Orchestrator struct {
	dtmAddr  string
	branches map[string]string
	genGID   GIDFunc
	tp       TraceparentFunc
	url      URLBuilder
	newSaga  SagaFactory
	logger   logx.Logger
}

// New 构造编排器。配置缺失时返回 error，由调用方按 strict.dtm 裁决失败姿态
// （缺省 false → Warn + dtm.Unavailable，进程不退出）。
func New(c Config) (*Orchestrator, error) {
	o := &Orchestrator{
		dtmAddr:  c.DTMAddr,
		branches: make(map[string]string, len(serviceOfDomain)),
		genGID:   c.GenGID,
		tp:       c.Traceparent,
		url:      c.BranchURL,
		newSaga:  c.NewSaga,
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
	if o.newSaga == nil {
		o.newSaga = NewGRPCSaga
	}
	if o.logger == nil {
		o.logger = logx.GetGlobal()
	}
	// 拷一份而不是直接持有 c.BranchAddrs：调用方复用自己的 map 就能在运行期
	// 改掉已构造好的编排器，而 branchPlan 的序是编译期常量、地址却是运行期
	// 值，两者错配时没有任何迹象。
	for _, d := range []string{DomainStock, DomainOrder, DomainProduct, DomainPoints} {
		o.branches[d] = c.BranchAddrs[d]
	}
	if err := o.validate(); err != nil {
		return nil, err
	}
	return o, nil
}

// Unavailable 造一个「恒提交失败」的编排器。
//
// 存在的理由：dtm 配错时进程仍必须起来（strict.dtm: false 的口径）。如果
// setup 层选择「构造失败就返回 error」以外的另一条路 —— 比如把 handler 里的
// 编排器留成 nil —— 那处 nil 判断会散落到每个下单入口，且第一次忘判就是
// panic。让编排器**可构造但提交恒失败**，失败点收敛成一个有明确文案的位置。
func Unavailable(cause error) *Orchestrator {
	if cause == nil {
		cause = errors.New("dtm 编排不可用（原因未给出）")
	}
	return &Orchestrator{
		genGID: func(context.Context, string) (string, error) { return "", cause },
		tp:     func(context.Context) string { return "" },
		url:    JoinBranchURL,
		logger: logx.GetGlobal(),
	}
}

// validate 配置判据。缺地址 = 拼出来的 URL 是半截，dtm server 会把它当
// 分支地址去拨号然后静默失败 —— 与其让它在真机上表现成「四个分支全 404」，
// 不如在装配期把配置缺口说出来。
func (o *Orchestrator) validate() error {
	if o.dtmAddr == "" {
		return errors.New("dtm server 地址未配置（xmiddleware.dtm.grpc_addr）")
	}
	var missing []string
	for _, d := range []string{DomainStock, DomainOrder, DomainProduct, DomainPoints} {
		if o.branches[d] == "" {
			missing = append(missing, "dtm.branches."+d)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("dtm 分支面地址未配置：%v", missing)
	}
	return nil
}

// ---------- 下单命令 ----------

// OrderCmd 一次下单在编排层的输入。
//
// 字段直接对应四域分支各自的必填项（各域 service 层 validateDtmCmd 的判据）：
// 少一个字段，dtm 会判业务失败并逆序补偿掉已经跑成功的分支 —— 那是「提交成功、
// 事务失败」，对调用方最难解释的一种结局。故 Validate 在**建立全局事务之前**
// 挡掉（handler 映成 4xx），而不是让 dtm 兜回来。
type OrderCmd struct {
	// UserID 下单用户（鉴权身份，非请求体自报）。
	UserID int64
	// ProductID 商品。
	ProductID int64
	// Quantity 购买数量。
	Quantity int32
	// OrderNo 业务单号，bff 生成。同时是库存/订单/积分三域的幂等键与对账锚点。
	OrderNo string
	// Title 商品标题（预检取回，进订单明细）。
	Title string
	// UnitPrice 单价（分）。与商品域的 p.Product.Price 同一口径。
	UnitPrice int64
	// PointsCost 本单要扣的积分。**恒为正**：积分域 validateDtmBiz 拒 cost<=0，
	// 而 0 不是「不扣积分」而是「这笔分支必然失败并把整条 Saga 拖去补偿」。
	PointsCost int64
	// FailInject 演示用：点名一个分支方法名（Deduct/CreateOrder/AddSales/
	// Debit），该分支直接返业务失败，用来点火逆序补偿链。真机恒为空。
	FailInject string
}

// Total 本单总额（分）。口径与订单域 model.DtmOrderCmd.Total() 逐字相同
// （Σ quantity × unit_price，服务端算）—— 不信任调用方传值是账本纪律，不是
// 这里特有的选择。
func (c OrderCmd) Total() int64 { return int64(c.Quantity) * c.UnitPrice }

// Validate 编排前置校验。
//
// 判据全部来自各域分支的 validateDtmCmd/validateDtmBiz，理由是同一个：把
// 「一定失败」挡在事务之外，比在事务里失败再补偿回来更省事也更可信。
func (c OrderCmd) Validate() error {
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

// stockBiz 库存域分支业务体，对齐 srv-stock/internal/model.DtmStockCmd 的
// json tag 逐字一致（分支面 json.Unmarshal 到那个结构体，键名对不上就是零值
// → 业务失败）。
type stockBiz struct {
	OrderNo   string `json:"order_no"`
	ProductID uint64 `json:"product_id"`
	Quantity  int32  `json:"quantity"`
	UserID    uint64 `json:"user_id,omitempty"`
}

// orderBiz 订单域分支业务体，对齐 srv-order/internal/model.DtmOrderCmd。
type orderBiz struct {
	UserID  uint64      `json:"user_id"`
	OrderNo string      `json:"order_no"`
	DtmGID  string      `json:"dtm_gid"`
	Items   []orderItem `json:"items"`
	// Traceparent 落 outbox.traceparent：正向下单由 bff 发起，整条链（含 outbox
	// 补发那一跳）应挂在同一条 trace 上，而 Runner 补发时早已脱离本次 ctx ——
	// 只能靠行里存下的这一串续。
	Traceparent string `json:"traceparent,omitempty"`
}

// orderItem 订单域分支载荷里的单条明细。
//
// **键名逐字取 srv-order 的 model.OrderItemCmd 字段名，不是下划线写法**：那个
// 结构体没有 json tag，encoding/json 只能按字段名（或其大小写不敏感变体）匹配，
// 而 EqualFold("product_id", "ProductID") 为 false（多一个下划线）。载荷若写成
// 下划线风格，分支侧解出来的就是零值明细，订单域以 "invalid item" 判业务失败。
// 这个坑的守卫在 saga_test.go 的 TestOrderItemKeysMatchDomainStruct。
type orderItem struct {
	ProductID uint64
	Name      string
	Quantity  int
	UnitPrice int64
}

// salesBiz 商品域分支业务体，对齐 srv-product/internal/model.DtmSalesCmd。
type salesBiz struct {
	ProductID uint64 `json:"product_id"`
	Quantity  int64  `json:"quantity"`
	OrderNo   string `json:"order_no"`
}

// pointsBiz 积分域分支业务体，对齐 srv-points/internal/model.DtmBiz。
type pointsBiz struct {
	UserID  int64  `json:"user_id"`
	Cost    int64  `json:"cost"`
	OrderNo string `json:"order_no"`
}

// ---------- 分支表 ----------

// branchSpec 一个分支的寻址与载荷。
type branchSpec struct {
	domain     string
	service    string
	action     string
	compensate string
	// biz 该分支的业务体。补偿分支与正向分支共用同一份载荷 —— dtm 的 Saga
	// 只有一个 BinPayloads[i]，两段按 URL 区分，载荷不分叉。故这里只一个函数。
	biz func(cmd OrderCmd, gid, traceparent string) any
}

// branchPlan 四分支 Saga 的编排表。
//
// **顺序是这份代码里唯一关于「先扣什么后扣什么」的决策**，且它必须是代码顺序：
// dtm 按 Add 的调用序推进正向、按逆序补偿。把序写成数据再排序等于把决策挪到
// 排序函数里，而排序函数的正确性没有任何单测能覆盖。
//
// 序本身取自 spec（dtm-saga-ordering）：stock.Deduct → order.CreateOrder →
// product.AddSales → points.Debit。前置依赖决定顺序：扣库存先于建单（建单要
// 记着这笔预占），加销量先于扣积分（积分扣完就是终态，前面任何一步失败都还能
// 干净回滚）。
func branchPlan() []branchSpec {
	return []branchSpec{
		{
			domain: DomainStock, service: stockService,
			action: stockDeduct, compensate: stockRelease,
			biz: func(cmd OrderCmd, _, _ string) any {
				return stockBiz{
					OrderNo:   cmd.OrderNo,
					ProductID: uint64(cmd.ProductID),
					Quantity:  cmd.Quantity,
					UserID:    uint64(cmd.UserID),
				}
			},
		},
		{
			domain: DomainOrder, service: orderService,
			action: orderCreateOrder, compensate: orderCancelOrder,
			biz: func(cmd OrderCmd, gid, traceparent string) any {
				return orderBiz{
					UserID:  uint64(cmd.UserID),
					OrderNo: cmd.OrderNo,
					DtmGID:  gid,
					Items: []orderItem{{
						ProductID: uint64(cmd.ProductID),
						Name:      cmd.Title,
						Quantity:  int(cmd.Quantity),
						UnitPrice: cmd.UnitPrice,
					}},
					Traceparent: traceparent,
				}
			},
		},
		{
			domain: DomainProduct, service: productService,
			action: productAddSales, compensate: productReduceSales,
			biz: func(cmd OrderCmd, _, _ string) any {
				return salesBiz{
					ProductID: uint64(cmd.ProductID),
					Quantity:  int64(cmd.Quantity),
					OrderNo:   cmd.OrderNo,
				}
			},
		},
		{
			domain: DomainPoints, service: pointsService,
			action: pointsDebit, compensate: pointsRefund,
			biz: func(cmd OrderCmd, _, _ string) any {
				return pointsBiz{
					UserID:  cmd.UserID,
					Cost:    cmd.PointsCost,
					OrderNo: cmd.OrderNo,
				}
			},
		},
	}
}

// Step 一个已构造好的分支（action / compensate URL + 载荷）。
type Step struct {
	Domain     string
	Action     string
	Compensate string
	Payload    *bffbranch.BusiReq
}

// Result 一次提交的结果。
type Result struct {
	// GID 全局事务号。调用方拿它去 dtm server 查事务状态/分支明细。
	GID string
	// Steps 实际提交的分支序列（提交成功后才有意义）。
	Steps []Step
}

// Submit 构造并提交四分支 Saga。
//
// 返回的 error **只表示「事务没提交」**，不表示「事务执行失败」：四分支的成败
// 由 dtm 异步推进，提交那一刻还没有任何分支跑过。两种失败必须分清 —— 前者是
// 「什么都没发生，调用方可以原样重试」，后者是「dtm 正在回滚」，混成一个错误
// 会让调用方在回滚途中重复下单。
//
// 提交失败只 Warn 不抛给 handler 之外的任何地方，更不会回落到手动编排链：
// 手动补偿已退役（见包注释第 1 条），回落等于把两套补偿同时打开。
func (o *Orchestrator) Submit(ctx context.Context, cmd OrderCmd) (Result, error) {
	if err := cmd.Validate(); err != nil {
		// 命令非法属于调用方错误（4xx），不是 dtm 故障，不打 Warn。
		return Result{}, fmt.Errorf("dtm saga: 下单命令非法: %w", err)
	}
	gid, err := o.genGID(ctx, o.dtmAddr)
	if err != nil {
		return Result{}, o.fail(ctx, gid, "生成全局事务号", err)
	}
	steps, err := o.buildSteps(ctx, cmd, gid)
	if err != nil {
		return Result{}, err
	}
	saga := o.newSaga(o.dtmAddr, gid)
	for _, s := range steps {
		saga = saga.Add(s.Action, s.Compensate, s.Payload)
	}
	if err := saga.Submit(); err != nil {
		return Result{}, o.fail(ctx, gid, "提交全局事务", err)
	}
	return Result{GID: gid, Steps: steps}, nil
}

// buildSteps 按 branchPlan 构造分支序列与载荷。抽出来是为了让单测能在不构造
// 任何 dtm 客户端的情况下断言「序 + 载荷」—— 那正是本编排器最容易写错、
// 又最难在真机上分辨的两件事。
func (o *Orchestrator) buildSteps(ctx context.Context, cmd OrderCmd, gid string) ([]Step, error) {
	tp := o.tp(ctx)
	plan := branchPlan()
	steps := make([]Step, 0, len(plan))
	for _, b := range plan {
		biz, err := json.Marshal(b.biz(cmd, gid, tp))
		if err != nil {
			return nil, fmt.Errorf("dtm saga: 构造 %s 分支载荷: %w", b.domain, err)
		}
		steps = append(steps, Step{
			Domain:     b.domain,
			Action:     o.url(o.branches[b.domain], "/"+b.service+"/"+b.action),
			Compensate: o.url(o.branches[b.domain], "/"+b.service+"/"+b.compensate),
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
// 后果文案必须写清「没发生什么」：调用方看到 5xx 会想「是不是扣了库存」，而
// 「事务没提交」与「已扣但要退」在外部观察上都是 5xx。
func (o *Orchestrator) fail(ctx context.Context, gid, stage string, err error) error {
	o.logger.WarnContext(ctx, "bff: dtm saga "+stage+"失败（全局事务未提交，四域没有任何写入；调用方可原样重试）",
		logx.String("dtm_addr", o.dtmAddr),
		logx.String("gid", gid),
		logx.Err(err),
		logx.String("hint", "确认 dtm server 在监听 xmiddleware.dtm.grpc_addr"))
	return fmt.Errorf("dtm saga: %s失败: %w", stage, err)
}

// ---------- 默认依赖实现 ----------

// JoinBranchURL 默认分支 URL 拼法：`<host:port>` + `<全方法名>`。
//
// **不带 scheme，这是硬要求而不是风格**：dtm server 的
// dtmdriver.defaultDriver.ParseServerMethod 先按第一个 '/' 切出拨号目标与全方法名，
// 紧接着 `if strings.Contains(url, "://") { return error("call dtmdriver.Use() before you use custom scheme") }`
// —— 写 `grpc://…` 得到的不是「也能用」，而是每次回调都报错，且只在 dtm server
// 日志里可见（dtm v1.19.0 dtmdriver v0.0.6 driver-default.go）。
//
// 各域 internal/dtm/setup.go 的启动日志里那行 `callback_url` 打的是 grpc:// 前缀，
// 那是给人看的示意串，不是可以直接抄进编排侧的形态。
func JoinBranchURL(addr, fullMethod string) string { return addr + fullMethod }

// grpcSaga 把官方 *dtmgrpc.SagaGrpc 适配到 Saga 接口。
type grpcSaga struct{ inner *dtmgrpc.SagaGrpc }

// NewGRPCSaga 官方 dtm 客户端构造（SagaGrpc）。零值依赖的默认实现。
func NewGRPCSaga(dtmAddr, gid string) Saga { return grpcSaga{inner: dtmgrpc.NewSagaGrpc(dtmAddr, gid)} }

func (g grpcSaga) Add(action, compensate string, payload proto.Message) Saga {
	g.inner.Add(action, compensate, payload)
	return g
}

func (g grpcSaga) Submit() error { return g.inner.Submit() }

// TraceparentFromCtx 默认 traceparent 取法。
//
// 走 launchx.Inject（它转调 otelx/propagation）而不是裸调 otel 全局传播器：
// 仓里所有注入端都走那一个，裸调会在传播器被换掉时静默链接不上 —— 症状是
// 「每个服务都有 span，就是不在一条链上」。
func TraceparentFromCtx(ctx context.Context) string {
	carrier := otelprop.MapCarrier{}
	launchx.Inject(ctx, carrier)
	return carrier.Get(dtmx.InjectKey)
}

// ServerGID 默认 gid 生成：向 dtm server 要一个。
//
// 官方 MustGenGid 用 panic（dtmimp.E2P）表达「dtm 不可达」，而本仓对 dtm 的
// 失败姿态是 5xx + Warn 带病运行 —— 崩进程是错的。故这里 recover 成 error，
// 让 Submit 走 fail() 那条 Warn 路径。
//
// 为什么不用本地生成当默认：dtm server 侧生成的号与其 id 生成器同源，排查时
// 「号是谁发的」是确定的。需要脱离 dtm server 也能出号（例如单测、或将来在
// 提交前做点别的事）时，注入 LocalGID 即可。
func ServerGID(ctx context.Context, dtmAddr string) (gid string, err error) {
	defer func() {
		if r := recover(); r != nil {
			gid = ""
			err = fmt.Errorf("dtm server %s 不可达（官方 MustGenGid 以 panic 表达）: %v", dtmAddr, r)
		}
	}()
	return dtmgrpc.MustGenGid(dtmAddr), nil
}

// LocalGID 本地生成 gid：不与 dtm server 往返。
//
// dtm server 接受客户端自带 gid（TransBase.Gid 的注释原话：可以自定义生成规则
// 而不用服务端生成），故这不是私造协议。号形态为 `<毫秒 base36>-<8 字节随机 hex>`，
// 前半段让号在日志/表里按时间可读，后半段保证同毫秒内不撞 —— 撞号的后果是
// 新事务顶掉旧事务，全局事务直接错乱。
func LocalGID(_ context.Context, _ string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("dtm saga: 生成 gid 随机数失败: %w", err)
	}
	return strconv.FormatInt(time.Now().UnixMilli(), 36) + "-" + hex.EncodeToString(b[:]), nil
}
