// dtm 分支面的进程级装配：起 grpc-go listener。
//
// 单独成文件是为了让 busi_server.go 只剩「协议转换」这一件事 —— 那边一个 span
// 属性写错，症状是 Jaeger 上链路错位；这边的地址/生命周期写错，症状是分支 404
// 或进程退出时 goroutine 泄漏。两者混在一处，改一处时容易顺手动到另一处的判据。
package dtm

import (
	"errors"
	"fmt"
	"net"

	"google.golang.org/grpc"

	"github.com/gospacex/dtmx"
	"github.com/gospacex/launchx"
	"github.com/gospacex/launchx/examples/micro/srv-order/internal/dtm/protos"
	"github.com/gospacex/launchx/examples/micro/srv-order/internal/service"
	"github.com/gospacex/logx"
)

// Setup 起分支面 listener（装配期调用）。
//
// 地址一律从 config 读（dtm.branch.listen_addr），Go 代码里零硬编码 —— 换部署
// 形态就得 fork 一份改常量的事在本仓是红线（examples_addr_hygiene_test.go 的
// TestServiceSourceHasNoHardcodedLoopbackAddr）。**不注册 consul**：分支面不是
// 对外服务入口，dtm server 按配置直连；注册进去只会让运维多看到一个没有健康检查
// 的裸 gRPC 端点。
//
// barrier 表名在此设（而不是留给 dtmx 的默认值）是为了让「这张表落在哪个库」
// 成为装配期的一句显式声明：官方默认 dtm_barrier.barrier 是**独立库**，与本域
// 业务库分属两处，CallWithDB 在业务连接的事务里根本插不进那张表 —— 屏障会静默失效。
func Setup(a *launchx.App, svc *service.OrderService) error {
	cfg := a.Config()
	addr := cfg.GetString("dtm.branch.listen_addr")
	if addr == "" {
		// 不给默认端口：四域各一个端口（9041-9044），猜错就是本进程占着别的域的
		// 端口，症状是「另一个域的分支全连不上」，排查方向会被带偏很远。
		return errors.New("srv-order: dtm.branch.listen_addr 未配置（分支面没有地址可监听；" +
			"四域端口约定为 9041-9044，订单域是 9042）")
	}
	dtmx.SetBarrierTableName(barrierTableName)

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("srv-order: dtm 分支面监听 %s 失败: %w", addr, err)
	}

	srv := grpc.NewServer()
	protos.RegisterOrderBusiServer(srv, NewBusiServer(svc, cfg.GetBool("trace.enabled"), a.Logger()))
	go func() {
		if serveErr := srv.Serve(lis); serveErr != nil {
			a.Logger().Error("srv-order: dtm 分支面退出", logx.Err(serveErr))
		}
	}()

	a.Logger().Info("dtm branch face listening",
		logx.String("addr", lis.Addr().String()),
		logx.String("service", orderBusiServiceName),
		logx.String("barrier_table", barrierTableName),
		logx.String("callback_url", "grpc://"+lis.Addr().String()+"/"+orderBusiServiceName+"/CreateOrder"),
		logx.String("hint", "dtm server 按 URL 回调；方法名必须与 busi.proto 一致"))

	a.OnStopNamed("dtm-branch-graceful", func() error {
		srv.GracefulStop()
		return nil
	})
	return nil
}

// barrierTableName 本域 barrier 表名。
//
// 表名不是环境变量而是 schema 契约（examples/micro/scripts/schema-micro.sql 逐字
// 抄自官方 sqls/dtmcli.barrier.mysql.sql，唯一差异是落在各域业务库内而非独立库），
// 故留在代码里；放进 config 只会多一个「配错了就静默插错表」的面。
const barrierTableName = "barrier"
