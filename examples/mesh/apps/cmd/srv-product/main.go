// Package main 商品域（srv 档）入口：Bootstrap → OnSetup → Run。
//
// 本文件不开连接、不装配 exporter、不注册节点 —— 那些都在 launchx 与
// app.Setup 里。它与本目录另外四个 cmd/main.go 逐字相同（除 import 路径与
// configPath），这正是「网格形态不需要为形态改代码」的直接证据。
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/gospacex/launchx"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/product/app"
)

// configPath 配置文件路径，相对 **CWD** 解析。
//
// CWD 契约（两处必须一致，故这里不用绝对路径）：
//   - 本机起服：在 apps/ 根目录跑 `go run ./cmd/srv-product`；
//   - 容器内：镜像 WORKDIR=/app，ConfigMap 经 subPath 挂到 /app/conf/srv-product.yaml。
const configPath = "conf/srv-product.yaml"

func main() {
	a, err := launchx.Bootstrap(configPath)
	if err != nil {
		log.Fatalf("bootstrap: %v", err)
	}
	a.OnSetup(app.Setup)

	// Run 阻塞监听故放协程；信号到来时显式走 App.Close 退出链
	// （server.stop → … → logger.close），否则最后一批 span 来不及冲刷。
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
