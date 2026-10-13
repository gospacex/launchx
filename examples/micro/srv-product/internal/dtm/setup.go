// 分支面的装配：起一个 grpc-go listener，地址读 config，不注册进 consul。
//
// 三条纪律与理由：
//   - **地址只来自 config**（dtm.branch.listen_addr）：写死等于换个部署形态就得
//     改代码（仓根那条 TestServiceSourceHasNoHardcodedLoopbackAddr 守的就是它）。
//   - **不注册 consul**：分支面只被 dtm server 按 URL 回调，不接受业务调用。
//     注册进注册中心的后果是 gateway 之类的调用方会照着服务名调它、拿到
//     Unimplemented，把「分支面没起来」变成「一堆下游报 500」。
//   - **进程级全局量在装配期设一次**：barrier 表名（applyProcessGlobals）。
//
// 单独成文件是为了让 busi_server.go 只剩「协议转换」这一件事 —— 那边一个 span
// 属性写错，症状是 Jaeger 上链路错位；这边的地址/生命周期写错，症状是分支 404
// 或进程退出时 goroutine 泄漏。两者混在一处，改一处时容易顺手动到另一处的判据。
package dtm

import (
	"context"
	"fmt"
	"net"

	"github.com/gospacex/logx"
	"google.golang.org/grpc"

	"github.com/gospacex/dtmx"
	productbusi "github.com/gospacex/launchx/examples/micro/srv-product/internal/dtm/protos"
)

// ListenAddrKey 分支面监听地址的配置键。
//
// 键名定成常量而不是在两处各写一个字面量：config.yaml 与 Go 代码各写一遍时，
// 改键只会让其中一边读到空串，而空串上 net.Listen 监听的是**所有网卡** ——
// 一台机器上的任何人都能调分支面。故监听前判空并报错（见 Start）。
const ListenAddrKey = "dtm.branch.listen_addr"

// barrierTable 本域 barrier 表名（见 examples/micro/scripts/schema-micro.sql
// 的 product_db 段）。
//
// 必须是 "barrier"（落在 product_db 内）而不是 dtm 官方的
// "dtm_barrier.barrier"：barrier 行由 BranchBarrier.CallWithDB 在**业务连接的
// 事务内**插入，跨库就插不进这个事务 —— 屏障形同虚设，而分支照样「成功」。
//
// 表名不是环境变量而是 schema 契约（逐字抄自官方 sqls/dtmcli.barrier.mysql.sql，
// 唯一差异是落在本域业务库内而非独立库），故留在代码里；放进 config 只会多一个
// 「配错了就静默插错表」的面。
const barrierTable = "barrier"

// applyProcessGlobals 设进程级全局量。装配期调一次即可。
func applyProcessGlobals() { dtmx.SetBarrierTableName(barrierTable) }

// Server 分支面 listener 的运行句柄。
type Server struct {
	grpc   *grpc.Server
	lis    net.Listener
	logger logx.Logger
}

// Start 起分支面 listener（非阻塞），返回运行句柄。
//
// addr 必须来自 config（ListenAddrKey）。这里不读 config 本身：读配置是装配层
// 的职责（app.Setup 手里才有 cfg），本包只负责「按给定地址把服务起起来」——
// 于是这段逻辑能被单测用 127.0.0.1:0 起一个真 listener 验真连接，而不是
// 只对着源码推断它能起来。
func Start(addr string, svc BranchService, traceOn bool, logger logx.Logger) (*Server, error) {
	if logger == nil {
		logger = logx.GetGlobal()
	}
	if addr == "" {
		// 宁可不启动：空串会被 net.Listen 当成「监听所有网卡」，一个配置拼写
		// 错误就把分支面暴露到整台机器上。
		return nil, errListenAddrEmpty
	}
	if svc == nil {
		return nil, errNotWired
	}
	applyProcessGlobals()

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	gs := grpc.NewServer()
	productbusi.RegisterProductBusiServer(gs, NewBusiServer(svc, traceOn, logger))

	s := &Server{grpc: gs, lis: lis, logger: logger}
	go func() {
		// Serve 返回非 nil error 时只可能来自 Stop/GracefulStop，那属于预期；
		// 真正的启动失败已经在 net.Listen 那一层返回过了。
		if err := gs.Serve(lis); err != nil {
			logger.Warn("product: dtm branch grpc server stopped", logx.Err(err))
		}
	}()
	logger.Info("dtm branch face listening",
		logx.String("addr", lis.Addr().String()),
		logx.String("service", productbusi.ProductBusi_ServiceDesc.ServiceName),
		logx.String("barrier_table", barrierTable),
		logx.String("registered_to_registry", "false"))
	return s, nil
}

// Addr 实际监听地址（配置里写 :0 时给的是随机端口，单测据此拨号）。
func (s *Server) Addr() string { return s.lis.Addr().String() }

// Stop 优雅停机：先摘流量（GracefulStop 等在途请求做完），再关 listener。
func (s *Server) Stop(ctx context.Context) error {
	if s == nil || s.grpc == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		s.grpc.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		// 停不下来的分支调用不能无限期占着 listener：dtm 会一直重试它，
		// 端口被占住的话整个事务的补偿链全挂在上面。硬停。
		s.grpc.Stop()
		return ctx.Err()
	}
}

// errListenAddrEmpty 配置缺地址（消息里带键名，免得排障时不知道查哪一项）。
var errListenAddrEmpty = fmt.Errorf(
	"dtm: 分支面监听地址为空（检查 config.yaml 的 %s）—— 空串会被 net.Listen 当成「监听所有网卡」", ListenAddrKey)
