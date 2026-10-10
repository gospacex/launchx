// Package main mall 聚合层（bff 档）入口：Bootstrap → OnSetup → Run。
//
// 业务装配全在 app.Setup 里：本文件不开连接、不装配 exporter、不注册节点。
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/gospacex/launchx"
	"github.com/gospacex/launchx/examples/mall/srv-bff/app"
)

// configPath 配置文件路径（相对 CWD 解析，故须在 examples/mall/srv-bff 目录下启动）。
const configPath = "config.yaml"

func main() {
	a, err := launchx.Bootstrap(configPath)
	if err != nil {
		log.Fatalf("bootstrap: %v", err)
	}
	a.OnSetup(app.Setup)

	// Run 阻塞监听故放协程；信号到来时显式走 App.Close 退出链
	// （server.stop → ... → logger.close），否则最后一批 span 来不及冲刷。
	runErr := make(chan error, 1)
	go func() { runErr <- a.Run() }()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-stop:
		log.Printf("shutdown: signal %s received", sig)
		if err := a.Close(); err != nil {
			log.Printf("close: %v", err)
			os.Exit(1)
		}
	case err := <-runErr:
		if err != nil {
			log.Printf("run: %v", err)
			_ = a.Close()
			os.Exit(1)
		}
		_ = a.Close()
	}
}
