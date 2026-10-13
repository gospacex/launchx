// Package service srv-points 业务编排层（积分是"钱"，幂等优先）。
//
// 在分层链上的位置：handler → **service** → repository → model。
//
// 边界纪律：本层**不 import pb、不 import kitex**。出入参一律走 model 的
// 领域 DTO（*Cmd / *Query / *Result），pb 与领域模型之间的转换归 handler。
// 否则 pb 加一个字段就会顺着 import 渗进业务规则，协议演进与业务演进被绑死。
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
// 业务失败用 codes.Error 向上抛（带码），由 handler 统一翻译成响应体
// Code/Message；本层自己不认识响应体长什么样。
//
// 句柄来源纪律：本层不 import gorm/redis/kafka，也不用全局单例 ——
// DB / Cache / KV / Lock / MQ 全部经 repository 与构造参数注入，
// 换形态（srv → monolith）与换驱动都不改业务代码。
package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gospacex/cachex"
	"github.com/gospacex/kvx"
	"github.com/gospacex/lockx"
	"github.com/gospacex/logx"
	"github.com/gospacex/otelx/propagation"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/gospacex/cachex/loadthrough"
	"github.com/gospacex/launchx/examples/mall/pkg/codes"
	"github.com/gospacex/launchx/examples/mall/pkg/events"
	"github.com/gospacex/launchx/examples/mall/srv-points/internal/model"
	"github.com/gospacex/launchx/examples/mall/srv-points/internal/repository"
	"github.com/gospacex/otelx/layer"
)

// pointsLockTTL 入账/消费互斥的持锁上限。
//
// 取 5s（远大于单事务耗时）而非配置化：这是"防并发改余额互相覆盖"的上限，
// 不是地址、也不是业务阈值。与 lockx 配置里的 10s TTL 形成双保险 ——
// 进程在解锁前崩溃时，锁也会在 TTL 内自动过期，不会永久锁死该用户。
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

// Repo 暴露仓储（pkg/handlecheck 断言 / 对账用）
func (s *PointsService) Repo() *repository.PointsRepository { return s.repo }

// begin 开 service 层 span，返回「收 error 再收尾」的闭包。
//
// 不做 quiet 豁免：service 返回非 nil error 意味着这次业务操作没完成
// （参数非法 / 写库失败 / 下游失败），都该在 Jaeger 上看得见 —— 与单体
// 示例 s.end 的口径一致。预期内的业务拒绝由 handler 翻成响应体 Code，
// 不进 error，因此不会把正常的「商品不存在」标成故障。
func (s *PointsService) begin(ctx context.Context, op string) (context.Context, layer.End) {
	return layer.Begin(ctx, "service", "points", op, s.traceOn, nil)
}

// GetAccount 查账户（cachex 优先，miss 回源 DB 并回填）
//
// 回源走的是 point_accounts ⋈ point_ledgers 的联表聚合投影，缓存里存的
// 也就是这份投影 —— 缓存值与回源值必须是同一个结构，否则"命中缓存就没有
// 流水笔数"这种半截响应会反复出现。
//
// 读缓存走 loadthrough.Fetch（含空值哨兵）：查没开过户的 user 会回填一条短期
// 负缓存，否则"批量请求不存在的用户"能把整层缓存旁路掉，压力直接落到 DB。
// 首次入账会经 apply → invalidate 把这条负缓存删掉（开户即该键转为存在）。
func (s *PointsService) GetAccount(ctx context.Context, q model.GetAccountQuery) (out *model.AccountDetail, err error) {
	ctx, end := s.begin(ctx, "GetAccount")
	defer func() { end(err) }()

	if q.UserID <= 0 {
		return nil, codes.New(codes.CodeUnauthorized, "user required")
	}
	key := accountCacheKey(q.UserID)

	a, err := loadthrough.Fetch(ctx, s.cache, s.cacheOpt(), key, func(ctx context.Context) (*model.AccountDetail, error) {
		return s.repo.GetAccount(ctx, q.UserID)
	})
	if err != nil {
		// *loadthrough.NotFound 会 Unwrap 到 repository.ErrNotFound，
		// 因此 mapRepoErr 的 errors.Is 分支照样认得出 —— 命中负缓存与
		// 回源 miss 走同一条翻译路径，不会给出两种不同的错误码。
		return nil, s.mapRepoErr(ctx, err)
	}
	return a, nil
}

// cacheOpt 本域读缓存的统一参数（正缓存 TTL 与空值哨兵只在这一处定义）。
//
// 为什么要抽：8 处读缓存若各写一份 Options，改负缓存策略就要改 8 遍，
// 漏改的那处不会报错，只会静默少一层保护 —— 这正是缓存穿透留下的方式。
func (s *PointsService) cacheOpt() loadthrough.Options {
	return loadthrough.Options{
		Label: "points",
		TTL:   accountCacheTTL,
		NotFound: func(err error) bool {
			return errors.Is(err, repository.ErrNotFound)
		},
		Logger: s.logger,
	}
}

// Earn 入账：幂等键 → 用户锁 → 2 表事务 + outbox → 缓存失效
func (s *PointsService) Earn(ctx context.Context, cmd model.EarnCmd) (out *model.ChangeResult, err error) {
	ctx, end := s.begin(ctx, "Earn")
	defer func() { end(err) }()

	if cmd.UserID <= 0 || cmd.Amount <= 0 || cmd.OrderNo == "" {
		return nil, codes.New(codes.CodePointsAmountInvalid, "user_id/amount/order_no invalid")
	}
	return s.apply(ctx, cmd.UserID, cmd.OrderNo, model.TypeEarn, cmd.Amount, cmd.Remark)
}

// Consume 消费：幂等键 → 用户锁 → 2 表事务 + outbox → 缓存失效
func (s *PointsService) Consume(ctx context.Context, cmd model.ConsumeCmd) (out *model.ChangeResult, err error) {
	ctx, end := s.begin(ctx, "Consume")
	defer func() { end(err) }()

	if cmd.UserID <= 0 || cmd.Amount <= 0 || cmd.OrderNo == "" {
		return nil, codes.New(codes.CodePointsAmountInvalid, "user_id/amount/order_no invalid")
	}
	return s.apply(ctx, cmd.UserID, cmd.OrderNo, model.TypeConsume, cmd.Amount, cmd.Remark)
}

// ListLedgers 流水列表（point_ledgers ⋈ point_accounts，带出账户余额）
//
// 不走缓存：流水要求实时，缓存只会造成对账偏差 —— 用户刚消费完就来看流水，
// 少一条比慢一点严重得多。
func (s *PointsService) ListLedgers(ctx context.Context, q model.ListLedgersQuery) (out []model.LedgerDetail, err error) {
	ctx, end := s.begin(ctx, "ListLedgers")
	defer func() { end(err) }()

	if q.UserID <= 0 {
		return nil, codes.New(codes.CodeUnauthorized, "user required")
	}
	rows, err := s.repo.ListLedgers(ctx, q)
	if err != nil {
		return nil, codes.Wrap(codes.CodeInternal, "list points ledgers failed", err)
	}
	return rows, nil
}

// Report 2 表 JOIN 报表
func (s *PointsService) Report(ctx context.Context, q model.PointsReportQuery) (out []model.ReportRow, err error) {
	ctx, end := s.begin(ctx, "Report")
	defer func() { end(err) }()

	rows, err := s.repo.Report(ctx, q)
	if err != nil {
		return nil, codes.Wrap(codes.CodeInternal, "report points failed", err)
	}
	return rows, nil
}

// ---- 内部 ----

// apply 入账/消费的公共编排：幂等 → 加锁 → 2 表事务 → 写幂等键 → 缓存失效。
//
// 抽成一个函数而不是两套复制粘贴：两条路径的差别只有 type，
// 复制一份的代价是"将来只改一边"（比如给消费加了风控却漏了入账）。
func (s *PointsService) apply(ctx context.Context, userID uint64, orderNo string,
	typ int32, amount int64, remark string) (*model.ChangeResult, error) {

	// 第一层幂等（kvx）：挡在加锁之前，命中即返回，且**不再查库**
	if s.idempotentHit(ctx, orderNo, typ) {
		return nil, codes.New(codes.CodePointsDuplicated, "duplicated points change, ignored")
	}

	unlock, deny := s.withUserLock(ctx, userID)
	if deny != nil {
		return nil, deny
	}
	defer unlock()

	tp := propagation.TraceparentIf(ctx, s.traceOn)
	out, err := s.repo.ChangeTx(ctx, repository.ChangeInput{
		UserID:  userID,
		OrderNo: orderNo,
		Type:    typ,
		Amount:  amount,
		Remark:  remark,
		OutboxFill: func(ledgerID uint64, balanceAfter int64) (payload, traceparent string) {
			ev := events.PointsChanged{
				UserID:     int64(userID),
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
				// 序列化失败不阻断记账：空 payload 时 Runner 直接放弃该行
				s.logger.WarnContext(ctx, "points event marshal failed", logx.Err(mErr))
				return "", tp
			}
			return string(b), tp
		},
	})
	if err != nil {
		return nil, s.mapRepoErr(ctx, err)
	}
	// 成功后才写幂等键：失败路径不写，否则一次失败会把后续重试全挡掉
	s.markIdempotent(ctx, orderNo, typ)
	s.invalidate(ctx, userID)
	if span := trace.SpanFromContext(ctx); span != nil && span.IsRecording() {
		span.SetAttributes(attribute.Int64("points.ledger_id", int64(out.LedgerID)))
	}
	return &model.ChangeResult{LedgerID: out.LedgerID, BalanceAfter: out.BalanceAfter}, nil
}

// withUserLock 取用户维度互斥锁，返回解锁闭包。
//
// 锁器为 nil（组件未装配 / 构造降级）时**显式 fail-open 并留 Warn**：
// 后果是"同一用户并发入账/消费可能出现余额竞态"，必须在日志里说清 ——
// 积分是钱，静默降级的事后追责无从下手。返回非 nil error 表示拒绝本次请求。
func (s *PointsService) withUserLock(ctx context.Context, userID uint64) (func(), *codes.Error) {
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
		// 取锁失败（含重试耗尽）≠ 业务失败：明确告知调用方可重试
		return noop, codes.Wrap(codes.CodeInternal, "concurrent points change in progress, retry later", err)
	}
	return func() {
		// 解锁必须用请求 ctx：传 Background 会让 release span 失去父节点，
		// 每次解锁在 Jaeger 上另起一根孤儿单 span trace（见 order 域同款注释）。
		if uErr := lk.Unlock(ctx); uErr != nil {
			s.logger.WarnContext(ctx, "points: unlock failed (lock will expire by ttl)", logx.Err(uErr))
		}
	}, nil
}

// mapRepoErr 把持久层哨兵错误翻译成业务码并上抛。
//
// 翻译集中一处是刻意的：5xxx 各码的语义差异（没开户 vs 不够扣 vs 重复记账）
// 一旦散落到各个方法里，必然出现"某条路径把 5001 报成 5002"。
func (s *PointsService) mapRepoErr(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, repository.ErrDuplicated):
		return codes.Wrap(codes.CodePointsDuplicated, "duplicated points change, ignored", err)
	case errors.Is(err, repository.ErrNotFound):
		return codes.Wrap(codes.CodePointsAccountNF, "points account not found", err)
	case errors.Is(err, repository.ErrInsufficient):
		return codes.Wrap(codes.CodePointsInsufficient, "points insufficient", err)
	default:
		s.logger.WarnContext(ctx, "points: repository error", logx.Err(err))
		return codes.Wrap(codes.CodePointsWriteFailed, "points change failed", err)
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
func (s *PointsService) invalidate(ctx context.Context, userID uint64) {
	if s.cache == nil {
		return
	}
	if err := s.cache.Delete(ctx, accountCacheKey(userID)); err != nil {
		s.logger.WarnContext(ctx, "points: cache invalidate failed", logx.Err(err))
	}
}

// evTraceID 事件体内嵌 trace_id（无活动 span 时为空）
func evTraceID(ctx context.Context) string {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		return sc.TraceID().String()
	}
	return ""
}

func accountCacheKey(userID uint64) string {
	return fmt.Sprintf("mall:points:account:%d", userID)
}

func idempotentKey(orderNo string, typ int32) string {
	return fmt.Sprintf("mall:points:idem:%s:%d", orderNo, typ)
}
