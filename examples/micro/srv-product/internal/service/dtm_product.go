// srv-product 的 dtm 分支业务方法（Saga 销量 +n / -n 两个分支）。
//
// 边界纪律（沿用 product.go 文件头的写法，不因为「这是给 dtm 调的」而松一条）：
//   - 本层**不 import pb、不 import kitex、不 import grpc**。出入参一律是
//     model.DtmSalesCmd；载荷的反序列化在 internal/dtm 分支面，pb 与领域模型之间
//     的转换也归它。否则 proto 改一个字段就会顺着 import 渗进业务规则。
//   - 句柄不来自全局单例：*sql.DB 经 repository.RawDB 从 launchx dbx 句柄取。
//   - 业务失败用 codes.Error 带上码上抛，并保留 dtmx.BusinessFailure 的官方哨兵
//     —— dtm 只认那个哨兵，不认码。
//
// 为什么分支方法仍走六层链而不是 handler 直连 repository：barrier 事务必须在
// service 之下、repository 之上被交出来（service 取 barrier、开事务、把 tx 递给
// repository）。这正是分层链唯一能把「幂等裁决」放在「业务写」外面的位置 ——
// handler 直连 repository 就只能靠唯一键去猜幂等，而空补偿（补偿早于正向到达）
// 那种情形没有任何唯一键能挡。
//
// **本域只有 Saga 一个步骤的两个方向，没有 TCC**：商品域没有「可售/冻结」两态
// 可分 —— 冻结只涉及库存与积分。给销量硬加三段冻结分支只会让编排侧多两个回调
// URL，而其中两段必然是空实现。
package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/gospacex/logx"

	"github.com/gospacex/dtmx"
	"github.com/gospacex/launchx/examples/micro/pkg/codes"
	"github.com/gospacex/launchx/examples/micro/srv-product/internal/model"
	"github.com/gospacex/launchx/examples/micro/srv-product/internal/repository"
)

// branchWrite 一个分支写：签名固定为「ctx + barrier 事务里的 tx + 业务体」，
// 收敛成一个函数值让两个公开方法共用同一条编排（取 barrier → 开事务 → 写 →
// 判错分流），而不是两份复制。
type branchWrite func(ctx context.Context, tx *sql.Tx, cmd model.DtmSalesCmd) error

// DtmAddSales Saga 正向分支：累计销量 +n（行不存在则建行）。
//
// 商品域在整条 Saga 里是**第三个分支**，作用是让四个域都参与：编排序为
// stock.Deduct → order.CreateOrder → **product.AddSales** → points.Debit。
// 它排在中间偏后，于是 points 失败时会连带把它一起回退 —— 这一点让它成为
// 「四服全参与」这件事唯一可观察的证据。
func (s *ProductService) DtmAddSales(ctx context.Context, cmd model.DtmSalesCmd) (err error) {
	ctx, end := s.begin(ctx, "DtmAddSales")
	defer func() { end(err) }()

	return s.dtmBranch(ctx, cmd, s.repo.DtmAddSales)
}

// DtmReduceSales Saga 补偿分支：销量 -n，精确回到正向之前的基线。
//
// 由 dtm 异步发起，载荷里没有 traceparent —— 链路按「新根 trace + biz.dtm.gid
// 关联」口径记录（见 internal/dtm 的分支 handler）。
//
// 落到本方法时 barrier 已替我们分好两种情形：正向确实到过（要回退）与正向从未
// 到达（空补偿，官方语义下业务回调根本不会被调用）。本方法不需要自己查有没有
// 加过 —— 那正是 barrier 存在的理由。
func (s *ProductService) DtmReduceSales(ctx context.Context, cmd model.DtmSalesCmd) (err error) {
	ctx, end := s.begin(ctx, "DtmReduceSales")
	defer func() { end(err) }()

	return s.dtmBranch(ctx, cmd, s.repo.DtmReduceSales)
}

// dtmBranch 两个分支方法的统一编排。
//
// 顺序是刻意定的，每一步都对应一种「顺序反了就出事」：
//  1. 先校验参数 —— 放在 barrier.Call 之前，坏载荷就不会在 barrier 表里留行
//     （每次重试都留一行的话，那笔永远失败的分支会被下一次判成「已落过账」），
//     且它被当成业务失败，dtm 不会为一个错单号重试一整天；
//  2. 再取 *sql.DB —— 取不到就直接报错。绝不能「没有事务也照样写」：那样写是
//     成功了，但重复回调会重复记销量，且当场没有任何迹象；
//  3. 再取 barrier —— **error 必须上抛**（见 dtmx.BarrierFromCtx 的注释）：
//     没有 barrier 就没有空补偿 / 重复回调 / 悬挂三条守卫；
//  4. 最后才在 barrier 事务里写。
//
// 分支面**不失效商品详情缓存**：缓存里放的是 ProductDetail（id / 标题 / 价格 /
// 类目 / 规格数），sold 根本不在那个投影里。凭「改了数据就删缓存」的直觉去删，
// 只会让每笔订单都把详情缓存打穿一遍 —— 缓存命中率掉到 0，而读放大打到 DB。
func (s *ProductService) dtmBranch(ctx context.Context, cmd model.DtmSalesCmd, write branchWrite) error {
	if err := validateDtmCmd(cmd); err != nil {
		return dtmx.BusinessFailure(codes.Wrap(codes.CodeInvalidParam, "dtm 分支参数非法", err))
	}
	db, err := repository.RawDB(s.repo.DB())
	if err != nil {
		// 基础设施故障：原样上抛，dtm 按退避重试。标成业务失败会让整条全局事务
		// 被判死并触发对已成功分支的回补 —— 一次装配错位变成一次真实回滚。
		return fmt.Errorf("dtm 分支取不到业务库连接: %w", err)
	}
	barrier, err := dtmx.BarrierFromCtx(ctx)
	if err != nil {
		// **不**包 BusinessFailure：取不到 barrier 说明「这条请求不是 dtm server
		// 的回调」或「metadata 残缺」—— 是链路/装配问题，不是业务拒绝。
		// 判成业务失败会让 dtm 立刻放弃这笔全局事务并回补已成功的分支；让它按
		// 退避重试至少给运维留出「补上 dtm metadata」的窗口，且重试本身无害
		// （barrier 保证不会重复记账）。
		return fmt.Errorf("dtm 分支取不到 barrier（请求不是 dtm server 回调？）: %w", err)
	}

	err = barrier.Call(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
		return write(ctx, tx, cmd)
	})
	if err != nil {
		return s.dtmFail(ctx, err)
	}
	return nil
}

// dtmFail 把分支写失败翻译成带业务码、且保留 dtm 官方哨兵的错误。
//
// 分流判据是「错误种类」而不是「有没有错」：
//   - 已是业务失败（账不平 / 行不存在 / 参数非法）→ 翻译成 2xxx 后**继续**带哨兵，
//     dtm 读到哨兵就不重试、直接转补偿；
//   - 其余（DB 不可达、句柄取不到）→ 原样上抛，dtm 退避重试。把它标成业务失败
//     等于告诉 dtm「别重试了，去回滚」，一次 DB 抖动会连带把已经记上的销量回退掉。
func (s *ProductService) dtmFail(ctx context.Context, err error) error {
	if !dtmx.IsBusinessFailure(err) {
		s.logger.WarnContext(ctx, "product: dtm 分支基础设施故障，不计入业务失败（dtm 将退避重试）", logx.Err(err))
		return err
	}
	return dtmx.BusinessFailure(s.dtmMapErr(ctx, err))
}

// dtmMapErr 分支错误 → 商品域业务码。
//
// 不复用 product.go 的 mapRepoErr：那条把「查询没命中」一律归成 CodeProductNotFound，
// 而分支面里「销量行不存在」只占业务失败的一半 —— 另一半是「账不平」，那是数据
// 已经漂了，处置完全不同（补一行 vs 查谁改坏了 sold）。混成一个码会让排障方向
// 直接跑偏。
func (s *ProductService) dtmMapErr(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, repository.ErrSalesNotFound):
		return codes.Wrap(codes.CodeProductNotFound, "product sales row not found", err)
	case errors.Is(err, repository.ErrSalesImbalance):
		// 2003 是商品域里唯一「写库被拒」的码。账不平不是「商品不存在」也不是
		// 「参数非法」：商品在、参数对，是库里的 sold 对不上。
		return codes.Wrap(codes.CodeProductWriteFailed, "product sales imbalance (sold < quantity)", err)
	default:
		s.logger.WarnContext(ctx, "product: dtm 分支写失败", logx.Err(err))
		return codes.Wrap(codes.CodeProductWriteFailed, "dtm branch write failed", err)
	}
}

// validateDtmCmd 分支入参校验。
//
// 单独一个函数而不是内联：两个方法各自内联一遍的话，改一条规则要改两处，漏改的
// 那处会表现为「正向能拒、补偿把负数量也写进销量」。
func validateDtmCmd(cmd model.DtmSalesCmd) error {
	switch {
	case cmd.ProductID == 0:
		return errors.New("product_id required")
	case cmd.Quantity <= 0:
		// 负数量会让条件更新恒成立（sold >= -n 谁都过），于是「减 -5」变成
		// 「加 5」—— 补偿一次凭空给销量加 5；0 则白跑一次事务。两者都必须拒。
		return errors.New("quantity must be positive")
	case cmd.OrderNo == "":
		// order_no 不落 product_sales（那张表只有 product_id/sold/updated_at），
		// 但它是失败日志里唯一能把「这笔销量属于哪张单」说清楚的锚点。
		return errors.New("order_no required（销量账变与 saga 分支的对账锚点）")
	default:
		return nil
	}
}
