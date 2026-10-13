package dtmx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"github.com/dtm-labs/client/dtmcli"
	"github.com/dtm-labs/client/dtmgrpc"
	"go.opentelemetry.io/otel/propagation"
)

func carrierOf(tp string) propagation.MapCarrier {
	return propagation.MapCarrier{InjectKey: tp}
}

// Barrier 包装 dtm 官方的子事务屏障。
//
// 字段不导出：官方 Barrier 由 dtm server 的回调上下文构造，外部凭空造一个
// 只会得到「没有 barrier 保护的假分支」。
type Barrier struct {
	inner *dtmcli.BranchBarrier
}

// barrierTableName 进程内生效的 barrier 表名。
//
// 默认 "barrier"：四域各在自己的业务库里建同名 barrier 表（见
// examples/micro/scripts/schema-micro.sql）。**不要**改回官方的
// "dtm_barrier.barrier" —— 那是独立库，而 CallWithDB 是在**业务连接的事务内**
// 插 barrier 行，跨库就插不进这个事务，屏障等于没有。
//
// 该名字最终落到 dtmimp.BarrierTableName（进程级全局量）。四域是四个独立
// 进程，各设各的，互不影响。
var (
	barrierTableMu   sync.RWMutex
	barrierTableName = defaultBarrierTable
)

const defaultBarrierTable = "barrier"

// SetBarrierTableName 设置 barrier 表名（装配期调用）。
func SetBarrierTableName(name string) {
	barrierTableMu.Lock()
	defer barrierTableMu.Unlock()
	barrierTableName = name
	dtmcli.SetBarrierTableName(name)
}

func tableNameOrDefault() string {
	barrierTableMu.RLock()
	defer barrierTableMu.RUnlock()
	if barrierTableName == "" {
		return defaultBarrierTable
	}
	return barrierTableName
}

// BarrierFromCtx 从 dtm server 的分支回调上下文取出官方屏障。
//
// 取不到必须报错：没有 barrier 就没有空补偿 / 重复回调 / 悬挂三条守卫，
// 分支会在无保护状态下落库，而当场没有任何迹象。调用点必须显式处理这个
// error，不允许忽略。
func BarrierFromCtx(ctx context.Context) (*Barrier, error) {
	inner, err := dtmgrpc.BarrierFromGrpc(ctx)
	if err != nil {
		return nil, fmt.Errorf("dtmx: barrier from grpc ctx（请求非 dtm server 回调？）: %w", err)
	}
	if inner == nil {
		return nil, errors.New("dtmx: barrier unavailable（请求缺少 dtm grpc metadata）")
	}
	return &Barrier{inner: inner}, nil
}

// Call 在 barrier 保护下执行业务写。
//
// 落到官方 `BranchBarrier.CallWithDB(db, cb)`：它内部 db.Begin() 起事务，
// 先插 barrier 行，再调 cb —— 屏障行与业务写在**同一个事务**里，因此
// 「补偿已到但正向没落库」这类乱序由官方语义直接判空补偿，业务侧零处理。
//
// db 必须是本域业务库的连接（见 repository.RawDB，取自 launchx 的 dbx 句柄）。
// cb 里只准用 tx 落库：换成 gorm 或另开连接会脱离这个事务，屏障形同虚设。
func (b *Barrier) Call(ctx context.Context, db *sql.DB, cb func(ctx context.Context, tx *sql.Tx) error) error {
	if b == nil || b.inner == nil {
		return errors.New("dtmx: barrier unavailable（nil Barrier）")
	}
	if db == nil {
		return errors.New("dtmx: barrier call 需要业务库连接（*sql.DB），nil 会让屏障静默失效")
	}
	if cb == nil {
		return errors.New("dtmx: barrier call 需要业务回调")
	}
	return b.inner.CallWithDB(db, func(tx *sql.Tx) error {
		return cb(ctx, tx)
	})
}
