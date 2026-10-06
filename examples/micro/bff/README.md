# examples/micro/bff —— BFF 形态 demo（验收案例 E2 中段 / T6）

订单域的 BFF（Backends For Frontends），用来证明 launchx 的 **bff 档** 是三段式成立的：

```go
a, err := launchx.Bootstrap("config.yaml")   // 1. 读配置 + 装配地基 + 按 mode 装配 + 注册自身
a.OnSetup(app.Setup)                          // 2. 业务装配（取引擎 + 下游 RPC 客户端选项）
a.Run()                                       // 3. 起 HTTP 服务并托管退出
```

`cmd/main.go` **49 行**，只做这三件事：不开连接、不装配 exporter、不注册节点
（注册由 launchx 按 `service.mode=bff` 自己做）。

## 1. 目录结构

```
examples/micro/bff/
├── cmd/main.go                  # 49 行：Bootstrap → OnSetup → Run
├── app/setup.go                 # 装配层：Engine + RPCClientOptions → 下游客户端 → 挂路由
├── internal/handler/order.go    # HTTP 层：绑参 + 状态码映射，自开 handler.* span
├── internal/downstream/client.go # 业务 + 下游调用：自开 span，RPC client span 由 rpcx 产出
├── config.yaml                  # 唯一配置源（无任何地址硬编码）
└── README.md                    # 本文件
```

分层边界是**硬**的，因为它直接决定 T13 的分层断言：

- `handler` 是 hertz 唯一出现的地方，往下没有任何 HTTP 概念；
- `downstream` 是唯一持有 kitex 客户端的地方，往上没有任何 RPC 概念；
- `downstream` 的客户端**只能**由 launchx 产出的选项构造，自己不碰地址。

## 2. 三层 span 是怎么落的

单域读（GET /order/:id）：

```
GET /order/:id                     (SERVER, webx 的 otel 中间件开的根 span)
└── handler.GetOrder               handler 层，internal/handler/mall.go
    └── downstream.order.GetOrder  业务层，internal/downstream/clients.go
        └── client.demo-order.GetOrder
            ↓ traceparent 经 kitex 元数据注入
            demo-order  service.order.GetOrder → db.query
```

复合下单（POST /order，一次请求串起四服，全链路验收看这一条）：

```
POST /order                        (SERVER)
└── handler.PlaceOrder
    ├── downstream.product.GetProduct → client.demo-product.GetProduct → demo-product
    ├── downstream.stock.ReserveStock → client.demo-stock.ReserveStock → demo-stock
    ├── downstream.order.CreateOrder  → client.demo-order.CreateOrder  → demo-order
    └── downstream.points.Earn        → client.demo-points.Earn        → demo-points
    （失败补偿）handler.ReleaseStock.compensate → client.demo-stock.ReleaseStock
```

`client.<服>.<方法>` 由 rpcx 的 `ClientSpanMiddleware` 产出（经
`App.RPCClientOptions` 带下来的选项挂上），**本 demo 不再在它外面包一层 span**
——包了父子链就退化成两段，T6 的三段断言会挂。

## 3. 运行

### 前置

```bash
# 1) 中间件底座（hubx/dockers 统一供给：consul / jaeger；订单数据在 srv 侧连 mysql）
/Applications/Docker.app/Contents/Resources/bin/docker compose \
  -f /Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml up -d consul jaeger

# 2) 起下游四服：必须在 bff 之前就绪（bff 启动时不校验下游存活，首次调用才发现）
export HUBX_DB_PW=hubx123
for d in product order stock points; do
  (cd examples/micro/srv-$d && go run ./cmd &)
done
# 端口：demo-product 9021 / demo-order 9022 / demo-stock 9023 / demo-points 9024
```

> 四条域链路走的就是上面这四个 `srv-*`（demo-product / demo-order /
> demo-stock / demo-points）。拆分前的单域样例 `examples/micro/srv`
> （demo-srv，:9013）已随四域拆分移除。

### 启动

```bash
cd examples/micro/bff            # config.yaml 按相对路径解析，必须在此目录下启动
export PATH=/Users/hyx/sdk/go1.24.3/bin:$PATH
go run ./cmd
```

启动日志应出现 launchx 打出的 **`launchx: service registered`**（`service=demo-bff`，
实例 ID 形如 `demo-bff-127.0.0.1-8081`）；进程退出时按反序注销同一个实例。
demo 代码里**没有**任何 `consul.Register` / `Deregister` 调用。

consul 不在时按 A3 降级：一条 `register failed, running unregistered` 的 Warn，
进程**不退出**（要让「注册失败」变成启动失败，在 `config.yaml` 里设
`strict.registry: true`）。

> ⚠️ 本机 `127.0.0.1:8081` 已被 Docker Desktop 占用（监听 `*:8081`）。hertz 绑的
> 是具体地址 `127.0.0.1:8081`，两者可以共存，但若本机还有别的服务也抢这个端口，
> 先在 `config.yaml` 里换掉 `webx.addr`。

## 4. 接口

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/ping` | 自响应 `{"service":"demo-bff"}`，**不触达下游**（下游挂掉时仍须 200） |
| GET | `/product/:id?user_id=N` | 商品域 `demo-product` → `GetProduct` |
| GET | `/stock/:product_id` | 库存域 `demo-stock` → `GetStock` |
| GET | `/order/:id?user_id=N` | 订单域 `demo-order` → `GetOrder` |
| GET | `/points/:user_id` | 积分域 `demo-points` → `GetAccount` |
| POST | `/order` | **复合下单**：商品 → 库存预占 → 建单 → 积分入账，失败补偿释放预占 |

`user_id` 是可选查询参数（下游按它做归属校验，缺省 0）。

状态码映射只有三种确定语义，其余一律 502 —— 猜出来的状态码比 502 更难排查：

| 情况 | 状态码 |
|---|---|
| `:id` 不是整数 | 400 |
| 下游返回 `CodeInvalidParam` | 400 |
| 下游返回 `CodeNotFound` / `CodeOrderNotFound` | 404 |
| 下游返回其它领域错误码 | 502 |
| 下游不可达 / 发现不到实例 / 超时 | 502 |

```bash
curl -s --noproxy '*' http://127.0.0.1:8081/ping                    # {"service":"bff"}
curl -s --noproxy '*' http://127.0.0.1:8081/order/1001              # 含 order_id / order_no / items
curl -s --noproxy '*' 'http://127.0.0.1:8081/order/1001?user_id=1'
```

> 本机 shell 有 `http_proxy`，不经 `--noproxy '*'` 会把请求发给代理拿到 404。

健康端点：webx 注册的是 `/healthz/live` 与 `/healthz/ready`（不是裸 `/healthz`，
裸路径返回 404）。这是 webx 侧的现有行为，与 monolith demo 一致。

srv 未起时的实测（降级，不是失败）：

```json
{"error":"no more instances to retry: last error: %!w(<nil>)"}
HTTP 502
```

## 5. E2 / T6 验收指引

```bash
export PATH=/Users/hyx/sdk/go1.24.3/bin:$PATH
cd /Users/hyx/work/gowork/src/hubx/launchx

# ① main.go ≤ 50 行
wc -l examples/micro/bff/cmd/main.go                       # 49

# ② 地址全部配置化（换部署形态只改配置，不重新编译）
grep -rn "127\.0\.0\.1\|localhost" --include=*.go examples/micro/bff/   # 必须为空

# ③ 不自建客户端：下游 RPC 客户端只能用 launchx 给的选项构造
grep -rnE "gorm\.Open|redis\.NewClient|kafka\.(NewWriter|NewReader)|consul\.NewClient" \
     --include=*.go examples/micro/bff/                    # 必须为空

# ④ 业务层不含形态分支（/ping 回显的 "bff" 取自 App.Mode()，写在装配层）
grep -rnE '"monolith"|"bff"|"gateway"|Mode\(\)' --include=*.go examples/micro/bff/internal/   # 必须为空

# ⑤ 下游发现生效 + 三段链路（完整链路在 Task 19 跑，本 Task 只要求编译通过 + 可启动）
for s in demo-product demo-stock demo-order demo-points; do
  printf '%s ' "$s"; curl -s "http://127.0.0.1:8500/v1/health/service/$s?passing=1" | jq 'length'   # ≥ 1
done
curl -s 'http://127.0.0.1:8500/v1/health/service/demo-bff?passing=1' | jq 'length'   # ≥ 1
```

一条命令取出本 demo 的 trace：

```bash
curl -s 'http://127.0.0.1:16686/api/traces?service=demo-bff&limit=1' > /tmp/bff-trace.json
```

### T13 反向断言

```bash
# config.yaml 里把 trace.enabled 改成 false，重启后重跑分层检查：
# handler.* / downstream.* / client.demo-{product,stock,order,points}.* 三层 span 必须全消失。
# 改之前之后都通过，说明完整性检查是空壳。
```

## 6. 本 demo 刻意不做的事

| 不做 | 原因 |
|---|---|
| 不注册（代码里没有 `Register`/`Deregister`） | bff 档的注册由 launchx 按 mode 自己做，demo 侧不重复盖一层 |
| 不自建 consul / redis / kafka / gorm 客户端 | 句柄与选项一律经 `a.Engine()` / `a.RPCClientOptions()` 取 |
| 不覆盖 rpcx 产出的 client span | 再包一层 span 会把三段父子链压成两段（T6 判据） |
| 不在代码里写 `127.0.0.1` | 换部署形态只改配置，地址必须全部配置化 |
| 不建数据层（无 sqlx/mqx 段） | BFF 不做数据落地，订单数据的唯一入口是下游四个域服 |
| 不复制第二份 kitex_gen | 直接 import `examples/micro/pkg/kitex_gen`（同 module，四域共用） |
| 不写死 `/ping` 回显的形态名 | 取 `App.Mode()`，业务分层里不出现形态字面量（C4） |

## 7. 形态无关

`internal/**` 内**不含任何形态分支**（见 §5 第 ④ 条 grep）。换形态只改
`config.yaml` 的 `service.mode`；换部署方式（宿主进程 / 容器 / 集群）都不需要
重新编译 —— 注册地址可由 `service.self.addr`（`${DEMO_BFF_SELF_ADDR}`，
未设则回落 `service.addr`）覆盖，代码侧无感知。
