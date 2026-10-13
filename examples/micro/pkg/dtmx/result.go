// Package dtmx micro 示例的 DTM 公共件：失败语义、traceparent 续链、官方 barrier 包装。
//
// 三件事的共同点是「都不在本包里造轮子」：
//   - 失败哨兵用 dtm 官方的 dtmcli.ErrFailure，不自造同义错误；
//   - traceparent 编解码走 launchx.Inject/Extract，与仓内唯一续链入口同源；
//   - 幂等守卫包 dtm 官方的 dtmcli.BranchBarrier，空补偿/悬挂/重放由官方语义裁决。
//
// 自造其中任何一件都能让代码「看起来对」，但在真机上是静默丢账。
package dtmx

import (
	"errors"

	"github.com/dtm-labs/client/dtmcli"
)

// BusinessFailure 把一个业务性错误标记为「dtm 不该重试」。
//
// dtm server 只认官方哨兵 dtmcli.ErrFailure（本包 = dtmimp.ErrFailure =
// errors.New("FAILURE")）。包它而不是自造一个同文案错误，原因在
// TestBusinessFailureCarriesOfficialSentinel 里：自造哨兵 server 认不出，
// 业务失败（积分不足 / 库存不足）会被当基础失败**无限重试**。
//
// 典型调用点（域服 service 分支方法）：
//
//	if err := repo.DeductTx(...); err != nil {
//	    return dtmx.BusinessFailure(err)   // 业务失败：不重试，dtm 立即逆序补偿
//	}
//	return err                            // 基础失败：原样上抛，dtm 按退避重试
func BusinessFailure(err error) error {
	if err == nil {
		return nil
	}
	return errors.Join(dtmcli.ErrFailure, err)
}

// IsBusinessFailure 判一个错误是否被标记为业务失败。
//
// 只能配合 errors.Is 使用（BusinessFailure 用的是 errors.Join，包装多层后
// 直接 == 比较会漏判）。
func IsBusinessFailure(err error) bool {
	return err != nil && errors.Is(err, dtmcli.ErrFailure)
}
