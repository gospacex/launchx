// Package app 装配层：把 launchx 交出的句柄接成
//
//	rpc → route(auth>ratelimit) → handler → service → repository → model
//
// 这条**入向**链，并额外起一条**消费链**
//
//	MQConsumer → events.UnmarshalOrderCompleted → service.ConsumeOrderCompleted
//	           → repository.AddBalance / AddLog
//
// 把 kitex 服务端回存给 launchx 启动与停止。
//
// 与 order 域装配的三处差异：
//  1. 句柄面更窄：只用 db + kv（route 限流计数）+ mqConsumer，没有 cache / lock / mq producer；
//  2. 多一段消费循环的启停（本域是 order.completed.v1 的消费方）；
//  3. 入向 RPC 面只读（IDL 里只有 GetAccount 接线）：**但限流照设** —— 读路径同样打 DB，
//     且该跳走 Kitex TTHeader/TCP，Envoy 只按 tcp_proxy 处理、拿不到 L7 限流，
//     route 的 middleware 是本跳唯一的治理手段（见 route 包注释）。
//     写路径走 MQ 消费、不经 route，是另一条独立的入口。
package app

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/cloudwego/kitex/server"
	"github.com/gospacex/logx"
	"github.com/gospacex/mqx"
	"gorm.io/gorm"

	"github.com/gospacex/launchx"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/points/handler"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/points/middleware"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/points/repository"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/points/route"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/points/rpc"
	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/points/service"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/events"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/handles"
	pointsrpc "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/points/points"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/rpcwire"
)

// consumeDrainTimeout 是业务 OnStop 里等待消费循环退出的**上界**。
//
// 为什么要有界而不是无限等：退出链是「业务 OnStop（逆序）→ server.stop →
// component.stop」，组件停 DB / MQ 时若那条在飞的消息仍在 repo.AddBalance，
// 就会打到已关闭的连接上。故取消后要等一下，让本轮处理收尾；但消费循环可能
// 卡在下游 IO 上——无限等会把整个进程的退出拖住，比丢一条消息严重得多。
// 上界到点即记 Warn 继续往下走（进程必须能退）。
const consumeDrainTimeout = 3 * time.Second

// Setup 注册给 App.OnSetup。
//
// 顺序即纪律：先取句柄 → 再自检 → 后装配。反过来（先装配再自检）判据就失效了：
// 句柄被业务包过一层之后，自检看到的已不是 launchx 交出的那个实例。
func Setup(a *launchx.App) error {
	// ---- 1. 取句柄 ----
	db, ok := a.DB().(*gorm.DB)
	if !ok || db == nil {
		return errors.New("srv-points: db handle unavailable (check xmiddleware.sqlx + strict.db)")
	}
	opts, ok := a.RPCServerOptions().([]server.Option)
	if !ok {
		return errors.New("srv-points: rpc server options unavailable (check service.mode=srv)")
	}
	// 业务码要过进程边界：kitex 默认只装 metainfo handler，业务码会被降级成
	// 一段字符串（客户端取不回码）。漏掉的症状是「服务照常起、链路照常通，
	// bff 却把每个业务拒绝都当 502」—— 不是可选优化项，见 rpcwire 包注释。
	opts = append(opts, rpcwire.ServerOptions()...)
	cfg := a.Config()
	// 实例名必须等于 service.name（spec mesh-app-handles）：它同时是 kv 实例的键，
	// 写成字面量会在改名后悄悄取到 nil 句柄。
	name := cfg.GetString("service.name")
	logger := a.Logger()
	// traceOn 是分层埋点的总闸：一次取出，分别交给 repository / service / handler / rpc。
	traceOn := cfg.GetBool("trace.enabled")

	// ---- 2. 句柄自检：来源合规 + 入链有效 ----
	// db 是硬前提（strict.db: true 已在配置里声明），缺了必须装配期即退。
	if err := handles.VerifyOrError(a, "db"); err != nil {
		return errors.New("srv-points: " + err.Error())
	}
	for _, c := range handles.Verify(a) {
		if !c.Present {
			continue
		}
		logger.Info("handle verified",
			logx.String("component", c.Component), logx.String("backend", c.Backend),
			logx.String("joins_trace", strconv.FormatBool(c.Observed)), logx.String("probe", c.Probe))
	}

	// ---- 3. 分层装配：自下而上 ----
	repo := repository.New(db, traceOn)
	svc := service.New(repo, traceOn)
	guard := route.New(middleware.New(a.KV()[name], logger), logger)

	// ---- 4. 建服务端并回存 ----
	// rpc/ 实现 points.Points 全部方法，并在委派 handler 前走 route.Guard
	// （身份闸门 + 按主体限流；被拒回 codes.RateLimited，bff 映射 429）。
	// kitex 按接口签名约束入参，少一个方法编译期即失败。
	srv := pointsrpc.NewServer(rpc.New(handler.New(svc, traceOn), guard, traceOn), opts...)
	a.SetRPCServer(srv)

	// ---- 5. 消费循环：本域作为 order.completed.v1 的消费方 ----
	//
	// 消费循环的起停归业务决策（launchx 不替业务起循环，见 App.MQConsumer 注释：
	// 何时开始消费、并发几路、失败要不要重投都是业务的事）。
	// 这里取最小可用形态：单路消费 + 退出时取消 + 坏载荷/确定性失败跳过。
	//
	// 跨 Task 契约（与 Task 8 本地 conf / Task 11 K8s ConfigMap 对齐）：
	// consumer.topic MUST 等于 events.TopicOrderCompleted（"order.completed.v1"）。
	// 键名是 consumer.topic 而不是 producer.topic —— mqx/kafka 现状是两侧都读
	// 这一个键。写错的症状是「消息发了没人收」，两侧日志都不报错。
	consumer := a.MQConsumer()
	if consumer == nil {
		// 消费不到消息不会立刻被看见，静默跳过的后果是队列无声地一直涨，故必须 Error。
		logger.Error("srv-points: mq consumer unavailable, points will never grow",
			logx.String("consequence", "积分流水表不会新增"),
			logx.String("probe", "检查 xmiddleware.mqx 段与 consumer.topic"))
	} else {
		done := make(chan struct{})
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			defer close(done)
			// Consume(ctx, handler) 只有两个参数 —— topic 不是入参，它在构造消费端时
			// 从 consumer.topic 取（见 apps/pkg/events 的键位口径注释）。
			if err := consumer.Consume(ctx, consumeHandler(svc, logger)); err != nil && ctx.Err() == nil {
				logger.Error("srv-points: consume loop exited", logx.Err(err))
			}
		}()
		// 退出链：先取消消费循环并**有界**等它退出（让在飞的那条消息能收尾），
		// 再让组件停 DB / MQ。上界存在的理由见 consumeDrainTimeout：消费循环可能
		// 卡在下游 IO 上，无限等会把整个进程的退出拖住。
		a.OnStop(func() error {
			cancel()
			select {
			case <-done:
			case <-time.After(consumeDrainTimeout):
				logger.Warn("srv-points: consume loop did not stop in time, proceeding to stop components",
					logx.String("timeout", consumeDrainTimeout.String()))
			}
			return nil
		})
	}

	logger.Info("layer chain assembled",
		logx.String("service", name),
		logx.String("chain", "rpc>route(auth>ratelimit)>handler>service>repository>model"),
		logx.String("consumer", "order.completed.v1 -> service.ConsumeOrderCompleted"))
	return nil
}

// consumeHandler 产出一条消息处理闭包：mqx 消息 → 领域事件 → service。
//
// 抽成独立函数（而不是内联在 Setup 的 go 块里）是为了能在单测里用假
// mqx.Consumer 直接驱动整条链 —— app.Setup 依赖 *launchx.App，外部包构造不出来。
//
// 错误分三类，分界见 nonRetryable：
//  1. 坏载荷（events.UnmarshalOrderCompleted 报错：非法 JSON / order_id 为 0）
//     → 返回 nil，不重投（重投只会无限循环）；
//  2. 确定性业务失败（nonRetryable 为真）→ 记 Warn 后返回 nil（等价死信）；
//  3. 可恢复失败 → 原样返回 error，交给驱动按策略重投。
//
// 契约上「能过 Unmarshal 的都可处理」并不成立：total_amount ≤ 0 能过 Unmarshal，
// 却被 service 判为 InvalidArgument（见 service.ConsumeOrderCompleted）。
func consumeHandler(svc *service.PointsService, logger logx.Logger) func(context.Context, *mqx.Message) error {
	return func(c context.Context, msg *mqx.Message) error {
		e, err := events.UnmarshalOrderCompleted(msg.Value)
		if err != nil {
			logger.Warn("srv-points: bad event payload, skipped", logx.Err(err))
			return nil // 坏载荷不重投：重投只会无限循环
		}
		if err := svc.ConsumeOrderCompleted(c, e); err != nil {
			if nonRetryable(err) {
				// 确定性失败：重投只会把消息在队列里转圈，故记 Warn 后吞掉（等价于死信）。
				logger.Warn("srv-points: event not processable, dropped",
					logx.Err(err), logx.String("order_id", strconv.FormatUint(e.OrderID, 10)))
				return nil
			}
			return err // 可恢复失败：交给驱动按策略重投
		}
		return nil
	}
}

// nonRetryable 判断这个错误重投是否有意义。
//
// 只有**由载荷本身决定、重投一万次也是同一个结果**的错误才归此类；范围刻意收窄 ——
// 把可恢复的错误误判成不可重试会静默丢消息，比多投几次严重得多。
// 目前的唯一成员是 InvalidArgument（载荷里的字段不可用，例如 total_amount <= 0）。
func nonRetryable(err error) bool {
	c, _ := codes.From(err)
	return c == codes.InvalidArgument
}
