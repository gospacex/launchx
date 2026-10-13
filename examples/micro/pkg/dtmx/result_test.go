package dtmx

import (
	"errors"
	"testing"

	"github.com/dtm-labs/client/dtmcli"
)

// TestBusinessFailureCarriesOfficialSentinel 是本文件存在的核心理由：
// dtm server 只认**官方**哨兵 dtmcli.ErrFailure。自造一个同义错误（哪怕文案
// 一模一样）是认不出来的 —— 后果不是"报错信息不好看"，而是业务失败被当基础失败
// **无限重试**：积分不足的单会被 dtm 反复回调，而它永远不足。
func TestBusinessFailureCarriesOfficialSentinel(t *testing.T) {
	inner := errors.New("积分不足")
	got := BusinessFailure(inner)

	if !errors.Is(got, dtmcli.ErrFailure) {
		t.Fatalf("BusinessFailure 未携带官方哨兵 dtmcli.ErrFailure，dtm server 认不出 → 业务失败会被无限重试。got=%v", got)
	}
	if !errors.Is(got, inner) {
		t.Fatalf("BusinessFailure 必须保留底层错误供 errors.Is 取回，实际丢失：%v", got)
	}
	if IsBusinessFailure(inner) {
		t.Fatal("普通 error 不得被判为业务失败（否则基础失败不再重试，故障会被吞）")
	}
	if !IsBusinessFailure(got) {
		t.Fatal("IsBusinessFailure 应识别包裹后的业务失败")
	}
}

// TestBusinessFailureOfNil 不炸：调用点常是 `BusinessFailure(mapErr(err))`，
// err 为 nil 时不该 panic（panic 会把整个分支面进程带下去）。
func TestBusinessFailureOfNil(t *testing.T) {
	if got := BusinessFailure(nil); got != nil {
		t.Fatalf("BusinessFailure(nil) 应返回 nil，实际 %v", got)
	}
	if IsBusinessFailure(nil) {
		t.Fatal("IsBusinessFailure(nil) 应为 false")
	}
}

// 反向探针：把 BusinessFailure 里的 errors.Join(dtmcli.ErrFailure, err) 改成只返回 err，
// TestBusinessFailureCarriesOfficialSentinel 必红（errors.Is 哨兵失败）。
