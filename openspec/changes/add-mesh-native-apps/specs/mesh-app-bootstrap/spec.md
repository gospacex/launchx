# mesh-app-bootstrap delta

## ADDED Requirements

### Requirement: 五个进程统一以 launchx.Bootstrap 装配

`examples/mesh/apps` 的服务 SHALL 全部经 `launchx.Bootstrap(configPath)` 单入口启动，
形态为 **5 个进程**：`bff` + `srv-{order,points,product,stock}`；MUST NOT 存在 `gateway`
进程（入口转发由 Istio `Gateway` + `VirtualService` 承担）。

每个 `cmd/<svc>/main.go` 的业务装配 MUST 只做两件事：

1. `launchx.Bootstrap(configPath)`；
2. `a.OnSetup(<svc>/internal/<layer>/app.Setup)`。

`main.go` MUST NOT 出现形态分支（不出现 `monolith` / `bff` / `srv` / `gateway` 任何取值）、
MUST NOT 自建连接、MUST NOT 装配 exporter、MUST NOT 注册节点；行数 MUST ≤ 30 行（含注释与 package 行）。

#### Scenario: 入口形态与 micro 一致

- **WHEN** 对比 `apps/cmd/bff/main.go` 与 `examples/micro/bff/cmd/main.go`
- **THEN** 两者结构一致（Bootstrap → OnSetup → Run 协程 + 信号处理 → Close 退出链），
  差异仅 import 路径与 `configPath`；不存在任何 mesh 专属分支

#### Scenario: apps 下不得出现 gateway

- **WHEN** 检查 `examples/mesh/apps`
- **THEN** 不存在 `cmd/gateway/`、`conf/gateway.yaml`、`internal/gateway/**`；
  `examples/micro/gateway/**` 保持不动（它是「换形态不改代码」的对照证据）

### Requirement: apps 只用 launchx 已有的两档装配

apps 的进程 MUST 只使用 launchx 现有的 `bff` 与 `srv` 两档，MUST NOT 引入
`mode=mesh` 第五档。`service.mode` 取值的校验口径不变：非法值 MUST 返回含
`monolith|bff|srv|gateway` 列表的 error。

同时 apps MUST NOT 使用 `gateway` 档 —— 该档的「HTTP 转发 + 上游发现」在网格形态下
由 Istio 承担，继续使用它等于把 562 行入口代码重写一遍。

#### Scenario: 非法 mode 仍然即退

- **WHEN** apps 的任一 `conf/*.yaml` 把 `service.mode` 写成 `mesh` 或其他非枚举值
- **THEN** `Bootstrap` 返回 error 并列出四档合法取值，进程不启动

### Requirement: apps 侧零服务注册

apps 的配置 MUST NOT 出现 `xmiddleware.coordx` 段；服务发现 MUST 完全由
K8s Service + Istio EDS 承担，业务侧不注册、不发现。

因此 apps 的 `srv` 档 MUST NOT 依赖 `service.self.addr` 完成注册（该键在 apps 下
没有任何消费者），`bff` 也 MUST NOT 期望 `App.Registry()` 返回非空。
为保留「同一份代码换环境可跑」的性质，`service.self.addr` MAY 仍出现在配置里，
但 MUST NOT 是启动成败的前提。

#### Scenario: 不配 coordx 段仍能启动并服务

- **WHEN** apps 的 `srv-order` 以不含 `xmiddleware.coordx` 的配置启动
- **THEN** 进程正常起服、正常收 RPC、正常响应；日志中**不出现**任何
  `register` / `deregister` 事件（未注册态必须彻底静默）

### Requirement: 独立 module 与构建入口

`examples/mesh/apps` SHALL 是独立 Go module，`module` 名为
`github.com/gospacex/launchx/examples/mesh/apps`，并以 `replace` 指向仓库根
（`github.com/gospacex/launchx => ../../..`）。

因独立 module，仓库根的 `go build ./...` **覆盖不到**本目录，故：

- `apps/Makefile` MUST 提供 `build` / `image` / `test` / `tidy` 四个目标；
- `build` MUST 以 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64` 交叉编译出 5 个二进制；
- `bring-up.sh image` 在 `VARIANT=apps` 时 MUST 委托给该 Makefile，
  MUST NOT 由脚本自己拼 `go build` 命令。

#### Scenario: 不写 gateway 也能编出全部二进制

- **WHEN** 执行 `make -C examples/mesh/apps build`
- **THEN** 产出 `bff`、`srv-order`、`srv-points`、`srv-product`、`srv-stock` 五个
  linux/amd64 二进制（**不含** `gateway`），任一缺失即失败

### Requirement: 退出链与优雅停机不在 apps 侧重写

apps MUST NOT 自行实现信号处理之外的退出编排：`SIGINT`/`SIGTERM` 到达后
MUST 调用 `App.Close()` 并让其按
`业务 OnStop(逆序) → server.stop → component.stop → registry.deregister → tracing.shutdown → logger.close`
执行，MUST NOT 用 `defer` 或自行调 `os.Exit` 跳过该链。

`srv` 档的 kitex 服务端 MUST 经 `App.SetRPCServer` 回存后交给 launchx 启动与停止，
apps MUST NOT 自己调 `server.Run()` / `server.Stop()`。

#### Scenario: 停机不丢最后一批 span

- **WHEN** 向 `srv-order` 发送 SIGTERM
- **THEN** 退出码为 0，且 Jaeger 中该进程退出前产生的 span 完整（尾部无缺失）；
  apps 代码中不出现第二次注册信号处理或 `os.Exit(0)` 绕过退出链
