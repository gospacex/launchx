# examples/micro/srv —— 微服形态 demo（验收案例 E3）

> ⚠️ **冻结（frozen）**：拆分前的对照样例，用来留住"launchx 化之前长什么样"。
> 不新增能力、不修缺陷，只保「编译得过 + 单测过得去」；现行实现是
> `examples/micro/srv-*`（四域）。
>
> 冻结不是口头约定：`frozen_test.go` 钉住两件事 —— 本文件的冻结声明被删会红，
> 本目录出现在任何部署清单里也会红（它不启动、不上线）。

订单域领域服务，用来证明 launchx 的**微服形态（srv）**是三段式成立的：

```go
a, err := launchx.Bootstrap("config.yaml")   // 1. 读配置 + 装配地基 + 按 mode 装配
a.OnSetup(app.Setup)                          // 2. 业务装配（取句柄、建 kitex server）
a.Run()                                       // 3. 起 RPC 服务并托管退出
```

`cmd/main.go` **49 行**，只做这三件事：不开数据库连接、不装配 exporter、
不注册节点（注册由 launchx 按 `service.mode=srv` 自己做）。

## 1. 目录结构

```
examples/micro/srv/
├── cmd/main.go                   # 49 行：Bootstrap → OnSetup → Run
├── app/setup.go                  # 装配层：取 DB/KV/MQ 句柄 → 三层 → SetRPCServer
├── internal/rpc/order.go         # RPC 桩：限流门控 + 分层 span（tracex.Start）
├── internal/service/order.go     # 业务层：校验 + 编排 + outbox 事件体
│   └── outbox.go                 # 事务发件箱补发器（3s 轮询、重试上限转放弃）
├── internal/repository/order.go  # 数据层：JOIN/事务/outbox 状态迁移
├── internal/middleware/ratelimit.go  # kvx 固定窗口限流（原子 Incr + fail-open）
├── internal/tracex/tracex.go     # 极小埋点包（替换 micro_apps/infra/telemetry）
├── pkg/kitex_gen/                # kitex 生成桩（order/product/pay/user/coupon）
├── pkg/{codes,events}/           # 跨服务错误码与事件体（纯数据）
├── config.yaml                   # 唯一配置源（无任何地址硬编码）
└── README.md                     # 本文件
```

### `internal/tracex` 为什么存在

原 `srv-order` 的 `internal/**` 只用到 `micro_apps/infra/telemetry` 的两个能力：
`Start(ctx, layer, entity, op)` 与 `Tracer()`。那份实现会自建并 `InstallGlobal`
一个全局 TracerProvider，而 launchx 的 telemetry 步骤已经注册好全局 provider 与
传播器，业务不该再抢一次全局位。故本 demo 自建 30 行的 `internal/tracex`：
签名与 span 命名约定（`"<layer>.<entity>.<op>"`）逐字一致，**业务代码零改动，
只换 import 路径**。

## 2. 运行

### 前置

```bash
# 1) 中间件底座（hubx/dockers 统一供给：mysql / consul / kafka / redis / jaeger）
/Applications/Docker.app/Contents/Resources/bin/docker compose \
  -f /Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml up -d mysql consul kafka redis jaeger

# 2) 库密码（config.yaml 的 DSN 用 ${HUBX_DB_PW} 展开）
export HUBX_DB_PW=hubx123

# 3) 建表：orders / order_items / outbox / order_status_log / refund_records
mysql -h127.0.0.1 -uroot -p order_db < /Users/hyx/work/gowork/src/hubx/micro_apps/pkg/seedsql/order_db.sql
```

### 启动

```bash
cd examples/micro/srv            # config.yaml 按相对路径解析，必须在此目录下启动
export PATH=/Users/hyx/sdk/go1.24.3/bin:$PATH
export HUBX_DB_PW=hubx123
go run ./cmd
```

启动后应看到 launchx 打出 **注册成功** 的日志（`service=demo-srv`，实例 ID 形如
`demo-srv-127.0.0.1-9013`）；进程退出时会按反序注销同一个实例。
demo 代码里**没有**任何 `consul.Register` / `Deregister` 调用。

端口与地址全在 `config.yaml`：`service.addr` 是 kitex 监听地址，
`service.self.addr` 是注册进 consul 的自身地址（支持 `${ENV}`，未设则回落到
`service.addr`；网格形态设 `DEMO_SRV_SELF_ADDR` 即可复用同一份二进制）。

## 3. 四个 RPC 方法

| 方法 | 说明 | 链路落点 |
|---|---|---|
| `CreateOrder` | 下单：本地事务 orders + order_items + outbox | `service.order.CreateOrder` + db span |
| `GetOrder` | 查单个订单 + 明细（2 表 JOIN） | JOIN 单独成 span |
| `Report` | 4 表 JOIN 报表（COUNT(DISTINCT) 抵消行放大） | 聚合 SELECT |
| `ListOrders` | 只读订单流水 | SELECT |

本 demo 未接商品域客户端（`ProductClient` 为 nil）：srv 档不产出下游客户端选项
（那是 bff/gateway 档的事），故下单走 service 层既有的「无预占」分支，
总额按明细计算，启动时会打一条 Warn 说明。跨服务调用由 bff demo（Task 10）验证。

## 4. E3 验收指引

```bash
export PATH=/Users/hyx/sdk/go1.24.3/bin:$PATH
cd /Users/hyx/work/gowork/src/hubx/launchx

# ① main.go ≤ 50 行（E3 硬断言）
wc -l examples/micro/srv/cmd/main.go                       # 49

# ② 无 micro_apps / infra 残留（业务代码只换 import，不再依赖原仓内部包）
grep -rn "micro_apps" --include=*.go examples/micro/srv/   # 必须为空

# ③ 地址全部配置化（陷阱 14：网格要复用同一份二进制）
grep -rn "127\.0\.0\.1\|localhost" --include=*.go examples/micro/srv/   # 必须为空
#    地址只允许出现在 config.yaml 里

# ④ 不自建客户端（句柄一律经 a.DB() / a.MQ() / launchx 注册）
grep -rnE "gorm\.Open|redis\.NewClient|kafka\.(NewWriter|NewReader)|consul\.NewClient" \
     --include=*.go examples/micro/srv/                    # 必须为空

# ⑤ 迁移进来的单测全绿
GOFLAGS=-mod=mod go test ./examples/micro/srv/...
```

一条命令取出本 demo 的 trace：

```bash
curl -s 'http://127.0.0.1:16686/api/traces?service=demo-srv&limit=1' > /tmp/srv-trace.json
```

## 5. 本 demo 刻意不做的事

| 不做 | 原因 |
|---|---|
| 不写 `cmd/srv-order/main.go` 那 336 行装配 | 装配是 launchx 的职责（Bootstrap 六步骨架） |
| 不复制 `micro_apps/infra/*` | telemetry → launchx + `internal/tracex`；drivers → `a.DB()/Cache()/MQ()`；consul → launchx 按 mode 注册 |
| 不自建 DB / MQ / 注册客户端 | 句柄只能经 launchx 取（依赖面红线） |
| 不建 webx 入口 | srv 档只有 RPC 服务端；HTTP 入口是 gateway/bff 的事 |
| 不接商品域下游客户端 | srv 档不产出下游客户端选项；bff demo 才做跨服务调用 |

## 6. 与单体 / 网格的关系

`internal/{rpc,service,repository}` 内**不含任何形态分支**：

```bash
grep -rnE '"monolith"|"bff"|"gateway"|Mode\(\)|service\.mode' --include=*.go \
     examples/micro/srv/internal/     # 必须为空（import 路径不算）
```

换形态只改 `config.yaml` 的 `service.mode`；网格形态复用**同一份二进制**，
差异只在配置副本（`service.self.addr`）与部署清单。
