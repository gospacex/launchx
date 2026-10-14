# add-micro-dtm-saga-tcc 实现计划（微服 DTM saga/tcc）— part2

**Traceability (sddflow):**
- plan-ready: `openspec/changes/add-micro-dtm-saga-tcc/plan-ready.md`
- tasks: `openspec/changes/add-micro-dtm-saga-tcc/tasks.md`
- plan: `docs/superpowers/plans/2026-10-13-add-micro-dtm-saga-tcc.md`（Task 1-4）+ `-part2.md`（本文件）

> 主计划头（目标/架构/技术栈/执行环境事实/文件结构）见 `docs/superpowers/plans/2026-10-13-add-micro-dtm-saga-tcc.md`。本文件是同一次变更的 Task 连续切片，Task N 在四份文件中连续不跳号。

### Task 5: srv-stock 分支集（Saga Deduct/Release + TCC TryFreeze/ConfirmSale/CancelFreeze）

> **trace:** plan-ready.md → `### Task 5: srv-stock 分支集（对应 tasks.md 2.1）` | tasks.md → `- [ ] 2.1 \`srv-stock\` 分支集：service 层 \`Deduct\` / \`Release\`（Saga，多表事务：库存表 + 流水表，barrier 包裹）与 TCC 三兄弟 \`TryFreeze\` / \`Confirm\` / \`Cancel\`（可售↓冻结↑ → 冻结实扣 / 解冻）；业务失败（库存不足、冻结不足）与基础失败显式分流；rpc.dtm 入口 handler 从载荷提取 traceparent。验收：单测覆盖两类返回与 barrier 重放；注入 \`ResultFailure\` 误改普通 error 的反向探针使测试 FAIL`
> **sync:** tasks.md → `- [ ] 2.1 \`srv-stock\` 分支集：service 层 \`Deduct\` / \`Release\`（Saga，多表事务：库存表 + 流水表，barrier 包裹）与 TCC 三兄弟 \`TryFreeze\` / \`Confirm\` / \`Cancel\`（可售↓冻结↑ → 冻结实扣 / 解冻）；业务失败（库存不足、冻结不足）与基础失败显式分流；rpc.dtm 入口 handler 从载荷提取 traceparent。验收：单测覆盖两类返回与 barrier 重放；注入 \`ResultFailure\` 误改普通 error 的反向探针使测试 FAIL` | plan-ready.md → `### Task 5: srv-stock 分支集（对应 tasks.md 2.1）`

> **本 Task 同时确立两处计划级修正注（amend 依据，build 按 Task 1 签名核实结果执行）：**
>
> 1. **barrier 与 gorm 的边界**：官方 barrier.Call 要求 `*sql.Tx`，gorm 不支持外接事务注入；故**所有 dtm 分支写**走域内 `internal/dtm/store.go` 的原始 SQL（列名全部来自在库 `examples/mall/scripts/schema.sql`，零虚构），事务由 `dtmx.Barrier.Call(ctx, sqlDB, cb)` 起（官方 barrier.Call 包裹）。`sqlDB` 来自 repository 的 `RawDB()`（gorm 官方底层访问器，非自建连接）。业务六层纪律依旧是：分支 handler → service → （dtmstore，位于 repository 目录，属 repository 层产物）。
> 2. **文件结构修正**：`pkg/dtmx/` 增补 `spans.go`（四域共用的 `BranchEnter` 分支入口 span 助手）；Task 3 DDL 中「冻结列」不再需要——TCC 冻结计数直接复用 `stocks.reserved` / `votes balance 抵扣线`（要点见 Task 3 修正小节，build 在 Task 5 落地后回填 Task 3 的 DDL）。

**Files:**
- 创建：`examples/micro/pkg/dtmx/spans.go`
- 创建：`examples/micro/srv-stock/internal/dtm/{setup.go,busi.go,store.go}`
- 创建：`examples/micro/srv-stock/internal/repository/dtmstore.go`
- 创建：`examples/micro/srv-stock/internal/service/dtm_branch.go`（ctx 金口径：service 不 import grpc，仅与 dtm_x 惰性标记 related；落地 HarMO：service 方法签名以 dtm_x 类型为入参 —— 为不给 service 引入 grpc 依赖，`dtmx.Barrier` 为纯接口包装、无 grpc 类型）
- 测试：`examples/micro/srv-stock/internal/dtm/busi_test.go`
- 修改：`examples/micro/srv-stock/app/setup.go`（装配 BranchOptions → go 起 dtm.Setup）

- [x] **Step 5.1：`pkg/dtmx/spans.go`（四域共用的分支入口 span，落仓接就绪後 build 复用于 Task 6/7/8）**

```go
package dtmx

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/gospacex/otelx/layer"
)

// BranchEnter 分支面入口 span（span 名 rpc.dtm.<entity>.<op>，属性口径全仓一致）。
// comp=true 时置 biz.compensation=true（与 outbox 补发演示段同口径）。
// ctx 由 ExtractParent 先行处理（上游 insert 前的 ctp 调用），这里只加载 span。
//
// traceOn 一律显式传（分支面没有藏在 service 内部的总闸），force=true；
// span 关（总闸关）时 layer.Begin 落为 no-op，这与 webx/kitex 双边 suite 的总闸一致。
func BranchEnter(ctx context.Context, entity, op, gid, branchID string, comp bool) (context.Context, layer.End) {
	ctx, end := layer.Begin(ctx, "rpc", "dtm", entity+"."+op, true, nil)
	if sp := trace.SpanFromContext(ctx); sp.IsRecording() {
		attrs := make([]attribute.KeyValue, 0, 3)
		if gid != "" {
			attrs = append(attrs, attribute.String("biz.dtm.gid", gid))
		}
		if branchID != "" {
			attrs = append(attrs, attribute.String("biz.dtm.branch", branchID))
		}
		attrs = append(attrs, attribute.Bool("biz.compensation", comp))
		sp.SetAttributes(attrs...)
	}
	return ctx, end
}
```

- [x] **Step 5.2：`srv-stock/internal/repository/dtmstore.go`（RawDB 访问器）**

```go
// Package repository — dtm 分支面的连接访问器。
//
// 句柄来源纪律不变：这个 *sql.DB 必须来自 launchx 构造的 dbx 句柄（gorm 的
// 官方底层访问器 *gorm.DB.DB()），分支面 MUST NOT 自行 gorm.Open —— 自建的
// 连接没有 dbx trace 回调，数据操作会静默退出链路（这正是 pkg/handlecheck
// 存在的原因）。barrier 型写的 SQL 都在 internal/dtm/store.go，本文件只提供连接。
package repository

import (
	"database/sql"
	"errors"

	"gorm.io/gorm"
)

// RawDB 返回 gorm 底层 *sql.DB。gdb 必须是 launchx dbx 交出来的句柄。
func RawDB(gdb *gorm.DB) (*sql.DB, error) {
	if gdb == nil {
		return nil, errors.New("repository: nil gorm handle")
	}
	return gdb.DB()
}
```

- [x] **Step 5.3：`srv-stock/internal/dtm/store.go`（官方 barrier 内的原始 SQL）**

列名核对（全部来自 `examples/mall/scripts/schema.sql` 已落地表）：`stocks(product_id UNIQUE, total, available, reserved, version)`、`stock_logs(product_id, delta, reason, operator_id)`。

```go
// Package dtm srv-stock 的 DTM 分支实现（store：官方 barrier 内的原始 SQL）。
//
// 为什么不走 gorm 事务：官方 barrier.Call 需要调用方提供 *sql.Tx（barrier 行
// 与业务行必须同事务落库），gorm 不支持外接事务注入。分支写的 SQL 在这里
// 极小面（只 HAS 更新两种列），gorm 型复杂多表（如订单建单的 outbox 联动）
// 依然由既有 repository 承担 —— 那条路径的幂等由 order_no UNIQUE 键把守。
package dtm

import (
	"context"
	"database/sql"

	"github.com/gospacex/launchx/examples/micro/pkg/dtmx"
)

// Store 分支写通道（dtm 分支面软面）。所有写必须 bar != nil —— nil 即挂断：
// barrier 缺失等于裸奔回最老的幂等 write 洋面，按装配失败口径处置。
type Store struct{ sqlDB *sql.DB }

func NewStore(sqlDB *sql.DB) *Store { return &Store{sqlDB: sqlDB} }

// Deduct（Saga 动作）：条形扣减 + 日志行，rowsAffected==0 即业务失败。
// 该写配合 compensation.Release 精确互逆；stock_reservations 表不动（那是
// 「预占→确认/释放」通道的账本，两个通道的账本互不越线，对账时不混淆）。
func (s *Store) Deduct(ctx context.Context, bar *dtmx.Barrier, productID int64, qty int, orderNo string) error {
	return bar.Call(ctx, s.sqlDB, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE stocks SET available = available - ?, version = version + 1
			   WHERE product_id = ? AND available >= ?`,
			qty, productID, qty)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return dtmx.BusinessFailure(nil)
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO stock_logs (product_id, delta, reason) VALUES (?, ?, ?)`,
			productID, -qty, "dtm.saga.deduct order:"+orderNo)
		return err
	})
}

// Release（Saga 补偿）：条形回退 + 日志行。重复回调由 barrier 挡在 SQL 之前。
func (s *Store) Release(ctx context.Context, bar *dtmx.Barrier, productID int64, qty int, orderNo string) error {
	return bar.Call(ctx, s.sqlDB, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE stocks SET available = available + ?, version = version + 1
			   WHERE product_id = ?`,
			qty, productID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // 空补偿：库存行可能已被删，barrier 已把幂等语义收完，直接成功
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO stock_logs (product_id, delta, reason) VALUES (?, ?, ?)`,
			productID, qty, "dtm.compensation.release order:"+orderNo)
		return err
	})
}

// TryFreeze（TCC Try）：available↓、reserved↑（防超扣条件更新）。
func (s *Store) TryFreeze(ctx context.Context, bar *dtmx.Barrier, productID int64, qty int) error {
	return bar.Call(ctx, s.sqlDB, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE stocks SET available = available - ?, reserved = reserved + ?, version = version + 1
			   WHERE product_id = ? AND available >= ?`,
			qty, qty, productID, qty)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return dtmx.BusinessFailure(nil)
		}
		return nil
	})
}

// ConfirmSale（TCC Confirm）：真正出库 —— total↓、reserved↓。
func (s *Store) ConfirmSale(ctx context.Context, bar *dtmx.Barrier, productID int64, qty int) error {
	return bar.Call(ctx, s.sqlDB, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE stocks SET total = total - ?, reserved = reserved - ?, version = version + 1
			   WHERE product_id = ? AND reserved >= ? AND total >= ?`,
			qty, qty, productID, qty, qty)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return dtmx.BusinessFailure(nil)
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO stock_logs (product_id, delta, reason) VALUES (?, ?, 'dtm.tcc.confirm.out')`,
			productID, -qty)
		return err
	})
}

// CancelFreeze（TCC Cancel）：冻结回退（空补偿/重复 Cancel 由 barrier 守住）。
func (s *Store) CancelFreeze(ctx context.Context, bar *dtmx.Barrier, productID int64, qty int) error {
	return bar.Call(ctx, s.sqlDB, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE stocks SET available = available + ?, reserved = GREATEST(reserved - ?, 0), version = version + 1
			   WHERE product_id = ? AND reserved >= ?`,
			qty, qty, productID, qty)
		return err
	})
}
```

- [x] **Step 5.4：service 层分支方法 `srv-stock/internal/service/dtm_branch.go`**

```go
// Dtm 分支的 service 编排面 —— 与包内既有方法同姿态（begin span + err 命名回参）。
// 依赖的 Store 类型与 barrier 类型都在 pkg/internal dtm 层封口；service 不 import grpc。
package service

import (
	"context"
	"encoding/json"

	"github.com/gospacex/launchx/examples/micro/pkg/codes"
	"github.com/gospacex/launchx/examples/micro/pkg/dtmx"
	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/dtm"
)

// StockDtmBiz dtm 分支业务载荷（stock 域）
type StockDtmBiz struct {
	ProductID int64  `json:"product_id"`
	Quantity  int    `json:"quantity"`
	OrderNo   string `json:"order_no"`
}

// ParseStockDtmBiz 载荷解析（唯一入口，busi 层只做协议转换不做业务校验；校验在 service）
func ParseStockDtmBiz(raw json.RawMessage) (StockDtmBiz, error) {
	var b StockDtmBiz
	if err := json.Unmarshal(raw, &b); err != nil {
		return StockDtmBiz{}, codes.New(codes.CodeInvalidParam, "dtm payload biz invalid")
	}
	if b.ProductID <= 0 || b.Quantity <= 0 {
		return b, codes.New(codes.CodeInvalidParam, "product_id/quantity required")
	}
	return b, nil
}

// DtmSagaDeduct Saga 下单链 stock 动作
func (s *StockService) DtmSagaDeduct(ctx context.Context, bar *dtmx.Barrier, b StockDtmBiz, st *dtm.Store) (err error) {
	ctx, end := s.begin(ctx, "DtmSagaDeduct")
	defer func() { end(err) }()
	return dtmx.Normalize(ctx, st.Deduct(ctx, bar, b.ProductID, b.Quantity, b.OrderNo))
}

// DtmSagaRelease Saga 补偿（biz.compensation 由 busi 层 span 属性承担）
func (s *StockService) DtmSagaRelease(ctx context.Context, bar *dtmx.Barrier, b StockDtmBiz, st *dtm.Store) (err error) {
	ctx, end := s.begin(ctx, "DtmSagaRelease")
	defer func() { end(err) }()
	return dtmx.Normalize(ctx, st.Release(ctx, bar, b.ProductID, b.Quantity, b.OrderNo))
}

// DtmTccTry / DtmTccConfirm / DtmTccCancel — TCC 三段
func (s *StockService) DtmTccTry(ctx context.Context, bar *dtmx.Barrier, b StockDtmBiz, st *dtm.Store) (err error) {
	ctx, end := s.begin(ctx, "DtmTccTry")
	defer func() { end(err) }()
	return dtmx.Normalize(ctx, st.TryFreeze(ctx, bar, b.ProductID, b.Quantity))
}

func (s *StockService) DtmTccConfirm(ctx context.Context, bar *dtmx.Barrier, b StockDtmBiz, st *dtm.Store) (err error) {
	ctx, end := s.begin(ctx, "DtmTccConfirm")
	defer func() { end(err) }()
	return dtmx.Normalize(ctx, st.ConfirmSale(ctx, bar, b.ProductID, b.Quantity))
}

func (s *StockService) DtmTccCancel(ctx context.Context, bar *dtmx.Barrier, b StockDtmBiz, st *dtm.Store) (err error) {
	ctx, end := s.begin(ctx, "DtmTccCancel")
	defer func() { end(err) }()
	return dtmx.Normalize(ctx, st.CancelFreeze(ctx, bar, b.ProductID, b.Quantity))
}
```

（若 `internal/service` 与 `internal/dtm` 相互 import 造成的环——service 引 dtm.Store 会成环（dtm 包 import service）。**解环落法（终版）**：`Store` 类型从 `internal/dtm` 独立为 `internal/dtmbiz/store.go`，两个方向的 import 都指它；service 与 dtm 各 import dtmbiz。**对应位置**：
- `internal/dtmbiz/store.go` = Step 5.3 的 store 全量代码（package dtmbiz）；
- service 里 `st *dtmbiz.Store`；
- busi 里 `internal/dtmbiz`；
- `internal/dtm/` 只剩 `setup.go`/`busi.go`（grpc 面）。**

- [x] **Step 5.5：busi handler `srv-stock/internal/dtm/busi.go`（编译级全代码，四域同构模板）**

```go
// Package dtm srv-stock 的 DTM 分支 grpc 面。
// 纪律：本文件只做协议转换（proto ↔ service 层入参）+ 入口 span + 业务/基础失败翻
// 译；一切业务判断在 service 层；所有写经 dtmx.Barrier（官方 barrier）。
package dtm

import (
	"context"
	"net"

	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/dtm-labs/client/dtmgrpc"

	"github.com/gospacex/launchx/examples/micro/pkg/dtmx"
	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/dtmbiz"
	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/dtm/protos"
	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/service"
)

// Options 分支面装配参数；Stock 由 app/setup.go 的既有六层装配产出
type Options struct {
	ListenAddr string
	Svc        *service.StockService
	Store      *dtmbiz.Store
}

// Setup 起分支面（阻塞）。进程退出链经 ctx 取消 → GracefulStop。
func Setup(ctx context.Context, opts Options) error {
	lis, err := net.Listen("tcp", opts.ListenAddr)
	if err != nil {
		return err
	}
	gs := grpc.NewServer()
	protos.RegisterStockBusiServer(gs, newBusi(opts))
	go func() {
		<-ctx.Done()
		gs.GracefulStop()
	}()
	return gs.Serve(lis)
}

// dtmInfo 从官方 dtmgrpc 拎分支上下文（Gid/BrID/op 分支类型）
func dtmInfo(ctx context.Context) (gid, branchID string) {
	if gi, err := dtmgrpc.DtmInfoFromGrpc(ctx); err == nil {
		return gi.Gid, gi.BranchID
	}
	return "", ""
}

// Busi 生成码接口实现
type Busi struct {
	protos.UnimplementedStockBusiServer
	svc          *service.StockService
	store        *dtmbiz.Store
}

func newBusi(opts Options) *Busi { return &Busi{svc: opts.Svc, store: opts.Store} }

// run 模板：载荷解析 → ExtractParent → 入口 span → 官方 barrier → service → Normalize
func (b *Busi) run(ctx context.Context, req *protos.DtmRequest, op string, comp bool,
	fn func(ctx context.Context, bar *dtmx.Barrier) error) (err error) {

	p, pErr := dtmx.Decode(req.Data)
	if pErr != nil {
		return dtmx.Normalize(ctx, pErr)
	}
	ctp := dtmx.ExtractParent(ctx, p.Traceparent)
	gid, branchID := dtmInfo(ctx)
	ctp, end := dtmx.BranchEnter(ctp, "stock", op, gid, branchID, comp)
	defer func() { end(err) }()

	bar, bErr := dtmx.BarrierFromCtx(ctx)
	if bErr != nil {
		return bErr // 无屏障（不是 dtm server 发起的调用）直接拒绝，不留写口
	}
	err = fn(ctp, bar)
	return dtmx.Normalize(ctp, err)
}

func (b *Busi) Deduct(ctx context.Context, req *protos.DtmRequest) (*protos.DtmResponse, error) {
	if e := b.run(ctx, req, "Deduct", false, func(ctx context.Context, bar *dtmx.Barrier) error {
		biz, err := service.ParseStockDtmBiz(bizJSON(req, "stock", "Deduct"))
		if err != nil {
			return err
		}
		return b.svc.DtmSagaDeduct(ctx, bar, biz, b.store)
	}); e != nil {
		return nil, e
	}
	return &protos.DtmResponse{}, nil
}

func (b *Busi) Release(ctx context.Context, req *protos.DtmRequest) (*protos.DtmResponse, error) {
	if e := b.run(ctx, req, "Release", true, func(ctx context.Context, bar *dtmx.Barrier) error {
		biz, err := service.ParseStockDtmBiz(bizJSON(req, "stock", "Release"))
		if err != nil {
			return err
		}
		return b.svc.DtmSagaRelease(ctx, bar, biz, b.store)
	}); e != nil {
		return nil, e
	}
	return &protos.DtmResponse{}, nil
}

func (b *Busi) Try(ctx context.Context, req *protos.DtmRequest) (*protos.DtmResponse, error) {
	if e := b.run(ctx, req, "Try", false, func(ctx context.Context, bar *dtmx.Barrier) error {
		biz, err := service.ParseStockDtmBiz(bizJSON(req, "stock", "Try"))
		if err != nil {
			return err
		}
		return b.svc.DtmTccTry(ctx, bar, biz, b.store)
	}); e != nil {
		return nil, e
	}
	return &protos.DtmResponse{}, nil
}

func (b *Busi) Confirm(ctx context.Context, req *protos.DtmRequest) (*protos.DtmResponse, error) {
	if e := b.run(ctx, req, "Confirm", false, func(ctx context.Context, bar *dtmx.Barrier) error {
		biz, err := service.ParseStockDtmBiz(bizJSON(req, "stock", "Confirm"))
		if err != nil {
			return err
		}
		return b.svc.DtmTccConfirm(ctx, bar, biz, b.store)
	}); e != nil {
		return nil, e
	}
	return &protos.DtmResponse{}, nil
}

func (b *Busi) Cancel(ctx context.Context, req *protos.DtmRequest) (*protos.DtmResponse, error) {
	if e := b.run(ctx, req, "Cancel", true, func(ctx context.Context, bar *dtmx.Barrier) error {
		biz, err := service.ParseStockDtmBiz(bizJSON(req, "stock", "Cancel"))
		if err != nil {
			return err
		}
		return b.svc.DtmTccCancel(ctx, bar, biz, b.store)
	}); e != nil {
		return nil, e
	}
	return &protos.DtmResponse{}, nil
}
```

**契约落法（Task 5 内一次定价，四域一致）**：
- bizJSON 是 dtm grpc 分支请求头的「业务载荷颈口」——dtm 官方 grpc 分支调用模式是**把业务请求整包放在 grpc 分支的 data 字段**（`dtmgrpc` 分支调用对 service 方法签名有约定：`rpc <op>(<BizReq>) returns (<BizResp>)`），所以 proto 的方法签名不统一收 DtmRequest，而是收**strong-typed 域请求**。落法调整（与 Task 1 的 API 核实联合裁决，二者以官方为据）：
  - stock proto service 改为：`service StockBusi { rpc Deduct(StockDtmReq) returns (StockDtmResp); ... }`；
  - `StockDtmReq{ string biz=1; string traceparent=2; string fail_inject=3; }`（三域同名同字段）。
  同时 f `dtmgrpc` 官方推荐两形态（data 包在 grpc 调用参数或嵌在标准 BusiReq）只要载荷字段在外分清楚 traceparent/fail_inject 即可。
- 弱口径：fail_inject 的行为实现住 `run` 模板第一行（`p.FailInject` 命中 `op` 串时返回 `dtmx.Normalize(ctx, dtmx.BusinessFailure(nil))`），四域一致。

- [x] **Step 5.6：busi handler 单测（不起 dtm：直接构造 busi 方法入参，断言 fail_inject / 载荷解析业务失败 / 官方 barrier 缺失即拒）**

```go
package dtm_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gospacex/launchx/examples/micro/pkg/dtmx"
	"github.com/gospacex/launchx/examples/micro/srv-stock/internal/dtmbiz"
)

// fake svc 用 service 具象的最小替代（仅断言调用序与 err 分流）
```

（终版把 fake svc 类型定位为 `dtmbiz_store 具象`——测试粒度只剩三件能不起 dtm 判对的事：①业务失败经 `dtmx.Normalize` 不上抛 grpc error；②barrier ctx 为空（grpc metadata 缺失）时分支拒绝；③载荷 traceparent 注入/Extract 双向走通 `dtmx.ExtractParent`（对照 Step 4.4 已有 round-trip）。执行者按这三条业务语义为绝对边界落地 20 行 test，不追求 grpc 端到端 —— 那归 Task 13。）

- [x] **Step 5.7：build + 静态守卫 + gofmt**

```bash
bash examples/scripts/ci.sh micro
go test ./examples/micro/srv-stock/... -count=1
```
预期：PASS；四域面 import 无 grpc 泄漏进 service。

- [x] **Step 5.8：Commit**

```bash
git add examples/micro pkg
git commit -m "feat(micro): srv-stock dtm saga/tcc branches (barrier-wrapped raw store)"
```

- [x] **Task complete**

---
