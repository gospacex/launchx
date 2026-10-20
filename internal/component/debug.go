package component

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"time"

	"github.com/gospacex/logx"
)

// debug 是观测侧伪组件：debug.enabled（yaml）或 APP_DEBUG=1（环境变量）任一为真时，
// 临时起一个仅监听 127.0.0.1:<port> 的 pprof 服务；生产默认关。
//
// 三条纪律与组件层其它成员一致：
//   - presence-based：都没开就静默跳过，不留监听、不留关闭钩子；
//   - 端口冲突 → 降级（Warn）+ 带病运行，不阻断启动、不返回 error；
//   - 退出钩子经 env.AddStop 注册到「组件停止相」（App.Close 的步骤 4），
//     该相在 logger.close（步骤 8）之前执行，满足「先于 logger 关闭」要求。
//
// 它不产出任何业务句柄，故不入 wire 图（wire 只管 type-identity 求解），由
// App.stepComponents 在图求解之后直接调用本 builder（见 buildDebug 注释）。
func init() { Register("debug", buildDebug) }

const (
	// debugConsequence 降级后果文案。debug 端点失效不影响业务请求，
	// 但运维会失去运行时诊断手段，必须说清。
	debugConsequence = "pprof/debug 端点不可用，运行时诊断能力缺失（不影响业务请求）"
	// debugDefaultPort 默认回环调试端口（仅回环：不暴露到容器外）。
	debugDefaultPort = 6060
	// debugStopTimeout 退出链里关停 debug server 的上限。
	debugStopTimeout = 3 * time.Second
)

// buildDebug 构造观测侧伪组件。
//
// 启用判定（两者任一为真即开）：
//  1. yaml `debug.enabled: true`；
//  2. 环境变量 `APP_DEBUG=1`。
//
// 当 yaml 显式写了 `debug.enabled`（无论真假），以 yaml 为准，环境变量不再覆盖——
// 这样「我在部署清单里关了」不会被宿主机的 `APP_DEBUG=1` 偷偷打开。
func buildDebug(env *Env) error {
	enabled := false
	explicit := false
	if env.Cfg != nil && env.Cfg.IsSet("debug.enabled") {
		enabled = env.Cfg.GetBool("debug.enabled")
		explicit = true
	}
	if !explicit && os.Getenv("APP_DEBUG") == "1" {
		enabled = true
	}
	if !enabled {
		return nil // 默认关：presence-based 静默跳过
	}

	port := debugDefaultPort
	if env.Cfg != nil && env.Cfg.IsSet("debug.port") {
		if p := env.Cfg.GetInt("debug.port"); p > 0 {
			port = p
		}
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// 端口冲突或绑定失败：降级带病运行，不阻断启动、不返回 error
		//（与组件层其它成员的 strict 裁决口径不同——debug 永远不阻断启动）。
		if env.Logger != nil {
			env.Logger.Warn("launchx: debug server unavailable, running without it",
				logx.String("component", "debug"),
				logx.Err(err),
				logx.String("consequence", debugConsequence),
				logx.String("hint", "free port "+addr+" or set debug.enabled: false"),
			)
		}
		return nil
	}

	mux := http.NewServeMux()
	// 标准 net/http/pprof handlers（索引页在 /debug/pprof/）。
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()

	// 经 AddStop 注册到组件停止相：App.Close 步骤 4（component.stop）先于
	// 步骤 8（logger.close）执行，故 debug server 一定在 logger 关之前关停。
	env.AddStop("debug", func() error {
		ctx, cancel := context.WithTimeout(context.Background(), debugStopTimeout)
		defer cancel()
		return srv.Shutdown(ctx)
	})
	return nil
}
