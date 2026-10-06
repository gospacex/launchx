// Package service srv-points 业务编排层（积分是"钱"，幂等优先）。
//
// 两层幂等的取舍（本域最核心的设计）：
//  1. 数据库唯一键 uk_order_no_type（point_ledgers）—— **事实来源**。
//     事务内先查同键流水，命中即返回 dup；并发下两个请求同时通过检查时，
//     由唯一键冲突裁决（谁后到谁失败）。
//  2. kvx 幂等键 `points:idem:{order_no}:{type}` —— **快速失败**。
//     它挡在加锁与事务之前，省掉一次持锁 + 一次事务的开销。
//
// 只有第 2 层是不够的：Redis 是缓存，key 丢了（重启 / 逐出 / TTL 到期）
// 就等于幂等失效，会重复加钱。因此第 2 层只做"提前返回"，
// 绝不作为"允许写入"的依据；反过来第 1 层在每次写前都要查库，
// 高并发下多一次往返，故两层并存而非二选一。
//
// 句柄来源纪律：本层不 import gorm/redis/kafka，也不用全局单例 ——
// DB / Cache / KV / Lock / MQ 全部由 app.Setup 从 launchx 的 App 句柄注入。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gospacex/cachex"
	"github.com/gospacex/kvx"
	"github.com/gospacex/launchx"
	"github.com/gospacex/lockx"
	"github.com/gospacex/logx"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/gospacex/launchx/examples/mall/pkg/codes"
	"github.com/gospacex/launchx/examples/mall/pkg/events"
	"github.com/gospacex/launchx/examples/mall/pkg/tracex"
	"github.com/gospacex/launchx/examples/mall/srv-points/internal/repository"

	pointspb "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen/points"
)

// pointsLockTTL 入账/消费互斥的持锁上限（与 lockx 配置的 10s 形成双保险）
const pointsLockTTL = 5 * time.Second

// accountCacheTTL 账户缓存时长（入账/消费后 Delete 失效）
const accountCacheTTL = 5 * time.Minute

// idempotentTTL 幂等键在 kvx 里的存活时长。
//
// 取 24h 而不是"永久"：幂等键是加速路径（事实来源是数据库唯一键），
// 设 TTL 可避免冷数据永久堆积；TTL 到期后的重复请求只会多查一次库，
// 不会算错账。
const idempotentTTL = 24 * time.Hour

// PointsService 积分业务编排
type PointsService struct {
	repo *repository.PointsRepository
	// cache 账户缓存（nil 表示缓存组件未装配/已降级 → 直查 DB）
	cache cachex.Cache
	// kv 限流计数 + 幂等键（nil 表示 KV 组件未装配 → 幂等降级为只靠数据库）
	kv kvx.KVStore
	// lock 入账/消费按用户互斥（nil 表示锁组件未装配 → fail-open）
	lock    lockx.Locker
	traceOn bool
	logger  logx.Logger
}

// NewPointsService 构造（cache / kv / lock 允许 nil，见字段注释）
func NewPointsService(
	repo *repository.PointsRepository,
	cache cachex.Cache,
	kv kvx.KVStore,
	lock lockx.Locker,
	traceOn bool,
	logger logx.Logger,
) *PointsService {
	if logger == nil {
		logger = logx.GetGlobal()
	}
	return &PointsService{repo: repo, cache: cache, kv: kv, lock: lock, traceOn: traceOn, logger: logger}
}

// Repo 暴露仓储（组装期健康检查 / 对账用）
func (s *PointsService) Repo() *repository.PointsRepository { return s.repo }

// GetAccount 查账户（cachex 优先，miss 回源 DB 并回填）
func (s *PointsService) GetAccount(ctx context.Context, req *pointspb.GetAccountReq) (*pointspb.GetAccountResp, error) {
	resp := &pointspb.GetAccountResp{UserId: req.GetUserId()}
	if req.GetUserId() <= 0 {
		c := codes.Fail(codes.CodeUnauthorized, "user required")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	key := accountCacheKey(req.GetUserId())
	if s.cache != nil {
		if cached, err := s.cache.Get(ctx, key); err == nil && cached != "" {
			var a repository.Account
			if json.Unmarshal([]byte(cached), &a) == nil {
				resp.Account = toPbAccount(&a)
				resp.Code, resp.Message = codes.OK().Code, codes.OK().Message
				return resp, nil
			}
			s.logger.WarnContext(ctx, "points cache decode failed, fallback to db", logx.String("key", key))
		}
	}
	a, err := s.repo.GetAccount(ctx, uint64(req.GetUserId()))
	if err != nil {
		if errors.Is(err, repository.ErrAccountNotFound) {
			c := codes.Fail(codes.CodePointsAccountNF, "points account not found")
			resp.Code, resp.Message = c.Code, c.Message
			return resp, nil
		}
		c := codes.Fail(codes.CodeInternal, err.Error())
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	if s.cache != nil {
		if b, mErr := json.Marshal(a); mErr == nil {
			if sErr := s.cache.Set(ctx, key, string(b), accountCacheTTL); sErr != nil {
				s.logger.WarnContext(ctx, "points cache fill failed", logx.Err(sErr))
			}
		}
	}
	resp.Account = toPbAccount(a)
	resp.Code, resp.Message = codes.OK().Code, codes.OK().Message
	return resp, nil
}

// Earn 入账：幂等键 → 用户锁 → 2 表事务 + outbox → 缓存失效
func (s *PointsService) Earn(ctx context.Context, req *pointspb.EarnReq) (*pointspb.EarnResp, error) {
	resp := &pointspb.EarnResp{UserId: req.GetUserId()}
	if req.GetUserId() <= 0 || req.GetAmount() <= 0 || req.GetOrderNo() == "" {
		c := codes.Fail(codes.CodePointsAmountInvalid, "user_id/amount/order_no invalid")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	balance, ledgerID, c := s.apply(ctx, req.GetUserId(), req.GetOrderNo(), repository.TypeEarn, req.GetAmount(), req.GetRemark())
	resp.Balance, resp.LedgerId = balance, ledgerID
	resp.Code, resp.Message = c.Code, c.Message
	return resp, nil
}

// Consume 消费：幂等键 → 用户锁 → 2 表事务 + outbox → 缓存失效
func (s *PointsService) Consume(ctx context.Context, req *pointspb.ConsumeReq) (*pointspb.ConsumeResp, error) {
	resp := &pointspb.ConsumeResp{UserId: req.GetUserId()}
	if req.GetUserId() <= 0 || req.GetAmount() <= 0 || req.GetOrderNo() == "" {
		c := codes.Fail(codes.CodePointsAmountInvalid, "user_id/amount/order_no invalid")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	balance, ledgerID, c := s.apply(ctx, req.GetUserId(), req.GetOrderNo(), repository.TypeConsume, req.GetAmount(), req.GetRemark())
	resp.Balance, resp.LedgerId = balance, ledgerID
	resp.Code, resp.Message = c.Code, c.Message
	return resp, nil
}

// ListLedgers 单表分页流水（不走缓存：流水要求实时，缓存只会造成对账偏差）
func (s *PointsService) ListLedgers(ctx context.Context, req *pointspb.ListLedgersReq) (*pointspb.ListLedgersResp, error) {
	resp := &pointspb.ListLedgersResp{UserId: req.GetUserId()}
	if req.GetUserId() <= 0 {
		c := codes.Fail(codes.CodeUnauthorized, "user required")
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	rows, err := s.repo.ListLedgers(ctx, uint64(req.GetUserId()), int(req.GetLimit()))
	if err != nil {
		c := codes.Fail(codes.CodeInternal, err.Error())
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	out := make([]*pointspb.PointLedger, 0, len(rows))
	for i := range rows {
		out = append(out, &pointspb.PointLedger{
			Id:           int64(rows[i].ID),
			UserId:       int64(rows[i].UserID),
			OrderNo:      rows[i].OrderNo,
			Type:         rows[i].Type,
			Amount:       rows[i].Amount,
			BalanceAfter: rows[i].BalanceAfter,
			CreatedAt:    rows[i].CreatedAt.Unix(),
		})
	}
	resp.Ledgers = out
	resp.Code, resp.Message = codes.OK().Code, codes.OK().Message
	return resp, nil
}

// Report 2 表 JOIN 报表
func (s *PointsService) Report(ctx context.Context, req *pointspb.ReportReq) (*pointspb.ReportResp, error) {
	resp := &pointspb.ReportResp{}
	rows, err := s.repo.Report(ctx, uint64(req.GetUserId()), 0)
	if err != nil {
		c := codes.Fail(codes.CodeInternal, err.Error())
		resp.Code, resp.Message = c.Code, c.Message
		return resp, nil
	}
	pbRows := make([]*pointspb.ReportRow, 0, len(rows))
	for i := range rows {
		row := &pointspb.ReportRow{
			UserId:      rows[i].UserID,
			Balance:     rows[i].Balance,
			TotalEarned: rows[i].TotalEarned,
			TotalSpent:  rows[i].TotalSpent,
			LedgerCount: rows[i].LedgerCount,
		}
		if rows[i].LastChangeAt != nil {
			row.LastChangeAt = rows[i].LastChangeAt.Unix()
		}
		pbRows = append(pbRows, row)
	}
	resp.Rows = pbRows
	resp.Code, resp.Message = codes.OK().Code, codes.OK().Message
	return resp, nil
}

// ---- 内部 ----

// apply 入账/消费的公共编排：幂等 → 加锁 → 2 表事务 → 写幂等键 → 缓存失效。
//
// 抽成一个函数而不是两套复制粘贴：两条路径的差别只有 type，
// 复制一份的代价是"将来只改一边"（比如给消费加了风控却漏了入账）。
// 返回 (余额, 流水号, 业务码)，由 Earn/Consume 各自回填响应体。
func (s *PointsService) apply(ctx context.Context, userID int64, orderNo string,
	typ int32, amount int64, remark string) (int64, int64, codes.Code) {

	// 第一层幂等（kvx）：挡在加锁之前，命中即返回，且**不再查库**
	if s.idempotentHit(ctx, orderNo, typ) {
		c := dupCode()
		return 0, 0, c
	}

	unlock, deny := s.withUserLock(ctx, userID)
	if deny != nil {
		return 0, 0, *deny
	}
	defer unlock()

	tp := s.traceparentOf(ctx)
	out, err := s.repo.ChangeTx(ctx, repository.ChangeInput{
		UserID:  uint64(userID),
		OrderNo: orderNo,
		Type:    typ,
		Amount:  amount,
		Remark:  remark,
		OutboxFill: func(ledgerID uint64, balanceAfter int64) (payload, traceparent string) {
			ev := events.PointsChanged{
				UserID:     userID,
				OrderNo:    orderNo,
				Type:       typ,
				Amount:     amount,
				Balance:    balanceAfter,
				SchemaVer:  events.SchemaVer1,
				TraceID:    evTraceID(ctx),
				OccurredAt: time.Now().UnixMilli(),
			}
			b, mErr := events.Marshal(ev)
			if mErr != nil {
				s.logger.WarnContext(ctx, "points event marshal failed", logx.Err(mErr))
				return "", tp
			}
			return string(b), tp
		},
	})
	if err != nil {
		s.logger.WarnContext(ctx, "points change tx failed", logx.Err(err))
		code, msg := s.mapRepoErr(err)
		return 0, 0, codes.Fail(code, msg)
	}
	// 成功后才写幂等键：失败路径不写，否则一次失败会把后续重试全挡掉
	s.markIdempotent(ctx, orderNo, typ)
	s.invalidate(ctx, userID)
	return out.BalanceAfter, int64(out.LedgerID), codes.OK()
}

func dupCode() codes.Code {
	return codes.Fail(codes.CodePointsDuplicated, "duplicated points change, ignored")
}

// withUserLock 取用户维度互斥锁，返回解锁闭包。
//
// 锁器为 nil（组件未装配 / 构造降级）时**显式 fail-open 并留 Warn**：
// 后果是"同一用户并发入账/消费可能出现余额竞态"，必须在日志里说清 ——
// 积分是钱，静默降级的事后追责无从下手。
func (s *PointsService) withUserLock(ctx context.Context, userID int64) (func(), *codes.Code) {
	noop := func() {}
	if s.lock == nil {
		s.logger.WarnContext(ctx, "points: locker unavailable, change runs without mutual exclusion",
			logx.Int("user_id", int(userID)),
			logx.String("consequence", "concurrent changes may race on balance"),
			logx.String("hint", "check xmiddleware.lockx in config.yaml"))
		return noop, nil
	}
	lk, err := s.lock.Lock(ctx, fmt.Sprintf("points:change:user:%d", userID), lockx.WithTTL(pointsLockTTL))
	if err != nil {
		c := codes.Fail(codes.CodeInternal, "concurrent points change in progress, retry later")
		return noop, &c
	}
	return func() {
		// 解锁必须用请求 ctx：传 Background 会让 release span 失去父节点，
		// 每次解锁在 Jaeger 上另起一根孤儿单 span trace（见 order 域同款注释）。
		if uErr := lk.Unlock(ctx); uErr != nil {
			s.logger.WarnContext(ctx, "points: unlock failed (lock will expire by ttl)", logx.Err(uErr))
		}
	}, nil
}

// mapRepoErr 把持久层哨兵错误翻译成业务码（集中一处，避免各路径口径不一）
func (s *PointsService) mapRepoErr(err error) (int64, string) {
	switch {
	case errors.Is(err, repository.ErrDuplicated):
		c := dupCode()
		return c.Code, c.Message
	case errors.Is(err, repository.ErrAccountNotFound):
		c := codes.Fail(codes.CodePointsAccountNF, "points account not found")
		return c.Code, c.Message
	case errors.Is(err, repository.ErrInsufficient):
		c := codes.Fail(codes.CodePointsInsufficient, "points insufficient")
		return c.Code, c.Message
	default:
		c := codes.Fail(codes.CodePointsWriteFailed, "points change failed")
		return c.Code, c.Message
	}
}

// idempotentHit 第一层幂等：kvx 键是否存在（KV 缺失时恒 false → 走数据库层）
func (s *PointsService) idempotentHit(ctx context.Context, orderNo string, typ int32) bool {
	if s.kv == nil {
		return false
	}
	ok, err := s.kv.Exists(ctx, idempotentKey(orderNo, typ))
	if err != nil {
		// 查不到一律按"未命中"：宁可多走一次事务，也不能据此允许重复记账
		return false
	}
	return ok
}

// markIdempotent 事务成功后写幂等键（失败即放弃：键丢了还有数据库唯一键）
func (s *PointsService) markIdempotent(ctx context.Context, orderNo string, typ int32) {
	if s.kv == nil {
		return
	}
	if err := s.kv.Set(ctx, idempotentKey(orderNo, typ), "1", idempotentTTL); err != nil {
		s.logger.WarnContext(ctx, "points: idempotent key write failed (db unique key still guards)", logx.Err(err))
	}
}

// invalidate 删账户缓存（余额变了必须失效，否则读缓存会拿到旧余额）
func (s *PointsService) invalidate(ctx context.Context, userID int64) {
	if s.cache == nil {
		return
	}
	if err := s.cache.Delete(ctx, accountCacheKey(userID)); err != nil {
		s.logger.WarnContext(ctx, "points: cache invalidate failed", logx.Err(err))
	}
}

// traceparentOf 取当前链路的 W3C traceparent（随 outbox header 透传）
func (s *PointsService) traceparentOf(ctx context.Context) string {
	if !s.traceOn {
		return ""
	}
	carrier := propagation.MapCarrier(map[string]string{})
	launchx.Inject(ctx, carrier)
	return carrier.Get(events.TraceHeaderKey)
}

// evTraceID 事件体内嵌 trace_id（无活动 span 时为空）
func evTraceID(ctx context.Context) string {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		return sc.TraceID().String()
	}
	return ""
}

func accountCacheKey(userID int64) string {
	return fmt.Sprintf("mall:points:account:%d", userID)
}

func idempotentKey(orderNo string, typ int32) string {
	return fmt.Sprintf("mall:points:idem:%s:%d", orderNo, typ)
}

func toPbAccount(a *repository.Account) *pointspb.PointAccount {
	if a == nil {
		return nil
	}
	return &pointspb.PointAccount{
		UserId:      int64(a.UserID),
		Balance:     a.Balance,
		TotalEarned: a.TotalEarned,
		TotalSpent:  a.TotalSpent,
	}
}

// tracexStart 业务层埋点（保留一个内部别名，避免 service 包各处重复取 tracer）
var tracexStart = tracex.Start
