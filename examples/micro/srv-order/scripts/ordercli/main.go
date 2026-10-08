// Command ordercli 是 E3 回归的 RPC 驱动：直连 demo-order 发一次下单请求。
//
// 为什么不复刻 gateway → bff → srv 那条 HTTP 链：那样验出来的是 E2 的结论
// （聚合层能不能把单下出去），srv 自己能不能在**没有网关和聚合层**的时候
// 独立完成「下单 + 写 outbox」就看不出来了。E3 要的是域服务本身成立。
//
// 为什么直连地址而不经 consul 发现：注册中心的可用性是 E2 那一层的变量，
// 把它留在 E3 里会让「发现失败」冒充成「下单失败」。这里用 WithHostPorts
// 把发现环节从回归品里摘掉 —— 它应当被 E2 的 consul 就绪断言覆盖。
//
// 为什么允许直接 import kitex：本文件是**测试驱动用的小工具**（对标
// kafka-console-consumer），不是服务。服务侧的唯一 kitex 接触面仍是
// internal/rpc 那一层；这里只是把它当作远程调用的命令行入口用。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/cloudwego/kitex/client"

	"github.com/gospacex/launchx/examples/micro/pkg/auth"
	"github.com/gospacex/launchx/examples/micro/pkg/codes"
	"github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/order"
	orderrpc "github.com/gospacex/launchx/examples/micro/pkg/kitex_gen/order/order"
)

// envOr 读环境变量，缺失或非法时回落到 def。
func envOr(key string, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

// result 是本命令的输出契约：一行 JSON，脚本侧的 jq/grep 只对它负责。
type result struct {
	OK          bool   `json:"ok"`
	OrderID     int64  `json:"order_id"`
	OrderNo     string `json:"order_no"`
	TotalAmount int64  `json:"total_amount"`
	Code        int64  `json:"code"`
	Message     string `json:"message"`
}

func main() {
	addr := envOr("SRV_ORDER_ADDR", "127.0.0.1:9022")
	userID := envInt("ORDER_USER_ID", 1001)
	productID := envInt("ORDER_PRODUCT_ID", 1)
	quantity := int32(envInt("ORDER_QUANTITY", 1))
	unitPrice := envInt("ORDER_UNIT_PRICE", 9900)

	out := func(r result) {
		b, _ := json.Marshal(r)
		fmt.Println(string(b))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// 携带一份身份再发：srv 侧现在校验"边界层签发过的身份"（见 pkg/auth），
	// 裸调会被拒回 1004。
	//
	// 为什么这里能签：本工具是**回归驱动**，等价于一次"已经过边界鉴权"的调用，
	// 它验的是同一个判据（签发方 / 主体 / 时效），不是绕过鉴权 —— 绕过意味着
	// 不带身份也能通过，而那正是闸门挡住的情形。真实链路里签发方是 bff。
	ctx = auth.WithIdentity(ctx, auth.New(userID, userID, time.Now()))

	c, err := orderrpc.NewClient("demo-order", client.WithHostPorts(addr))
	if err != nil {
		out(result{OK: false, Message: "new client: " + err.Error()})
		os.Exit(2)
	}

	resp, err := c.CreateOrder(ctx, &order.CreateOrderReq{
		UserId: userID,
		Items: []*order.OrderItem{{
			ProductId: productID,
			Name:      envOr("ORDER_ITEM_NAME", "e3 regression item"),
			Quantity:  quantity,
			UnitPrice: unitPrice,
		}},
	})
	if err != nil {
		// 传输层失败：与下文「业务码非 0」分属两类，脚本据此区分中断点。
		out(result{OK: false, Message: "rpc error: " + err.Error()})
		os.Exit(3)
	}

	r := result{
		OrderID:     resp.GetOrderId(),
		OrderNo:     resp.GetOrderNo(),
		TotalAmount: resp.GetTotalAmount(),
		Code:        resp.GetCode(),
		Message:     resp.GetMessage(),
		OK:          resp.GetCode() == codes.CodeOK,
	}
	out(r)
	if !r.OK {
		os.Exit(4)
	}
}
