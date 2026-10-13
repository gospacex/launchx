package dtmx

import (
	"strings"
	"testing"

	"github.com/dtm-labs/client/dtmcli"
	dtmimp "github.com/dtm-labs/client/dtmcli/dtmimp"
)

// TestBarrierFromCtxRejectsMissingGrpcMeta 是本文件最要紧的一条。
//
// dtm 的子事务屏障靠请求里携带的 gid/branch_id/op 建立唯一行；拿不到这些
// （metadata 缺失、请求不是 dtm server 发的）时**必须报错**。若这里返回
// (nil, nil) 让调用点继续，分支就会在没有屏障保护的情况下落库：重复回调
// 重复扣减、补偿早于正向到达时凭空写出脏数据，且当场没有任何迹象。
func TestBarrierFromCtxRejectsMissingGrpcMeta(t *testing.T) {
	b, err := BarrierFromCtx(t.Context())
	if err == nil {
		t.Fatalf("无 dtm grpc metadata 时 BarrierFromCtx 应报错，实际 err=nil、barrier=%v —— 分支面会无保护落库", b)
	}
	if b != nil {
		t.Fatalf("报错时 barrier 必须为 nil，实际 %v", b)
	}
	if !strings.Contains(err.Error(), "barrier") {
		t.Fatalf("错误文案须点明 barrier 便于 dtm 侧排障，实际：%v", err)
	}
}

// TestSetBarrierTableNameDefaultsToPerDomain 锁定「按域隔离」这一裁决：
// barrier 表名必须是 barrier（落在各域业务库内），而不是官方那个跨库的
// dtm_barrier.barrier —— 后者会让 barrier 行与业务写分属不同库，
// CallWithDB 在业务连接的事务里插不进这张表。
//
// 反向探针：把默认值改回 "dtm_barrier.barrier"，本用例必红。
func TestSetBarrierTableNameDefaultsToPerDomain(t *testing.T) {
	if got := tableNameOrDefault(); got != "barrier" {
		t.Fatalf("barrier 表名默认应为 barrier（各域业务库内），实际 %q", got)
	}
}

// TestSetBarrierTableNamePropagatesToDtmCli 保证设置真正落到 dtm 官方全局变量上：
// dtmimp.BarrierTableName 是进程级全局量，不设就是官方默认 dtm_barrier.barrier。
func TestSetBarrierTableNamePropagatesToDtmCli(t *testing.T) {
	prev := dtmimp.BarrierTableName
	t.Cleanup(func() { SetBarrierTableName(prev) })

	SetBarrierTableName("barrier")
	if dtmimp.BarrierTableName != "barrier" {
		t.Fatalf("SetBarrierTableName 未落到 dtm 官方全局变量，实际 %q", dtmimp.BarrierTableName)
	}
}

// TestBarrierTypeWrapsOfficialOne 保证我们对外的类型真的包着官方 *BranchBarrier：
// 若内部换成自造 barrier，空补偿/重复回调/悬挂三条守卫全部失效 —— 而这三条
// 只有真机（真 MySQL + 真 dtm）才判得出来，单测必须先把"包的是官方的"钉死。
func TestBarrierTypeWrapsOfficialOne(t *testing.T) {
	var _ *dtmcli.BranchBarrier // 编译期即证明类型可达
	b := &Barrier{inner: &dtmcli.BranchBarrier{}}
	if b.inner == nil {
		t.Fatal("Barrier.inner 必须持有官方 BranchBarrier")
	}
}
