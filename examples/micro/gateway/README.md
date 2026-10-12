# examples/micro/gateway —— 网关形态 demo（验收案例 E2 前半 / T6、T7）

订单域的流量入口，用来证明 launchx 的 **gateway 档** 是三段式成立的：

```go
a, err := launchx.Bootstrap("config.yaml")   // 1. 读配置 + 装配地基 + 按 mode 装配（本档不注册）
a.OnSetup(app.Setup)                          // 2. 业务装配（取引擎 + 注册中心句柄 → 路由表 + 转发器）
a.Run()                                       // 3. 起 HTTP 服务并托管退出
```

`cmd/main.go` **49 行**，只做这三件事：不开连接、不装配 exporter、不注册节点
（本档**不注册**，只有 bff/srv 由 launchx 按 mode 注册）。

## 1. 目录结构

```
examples/micro/gateway/
├── cmd/main.go                  # 49 行：Bootstrap → OnSetup → Run
├── app/setup.go                 # 装配层：Engine + Registry → 路由表 + 转发器 → 挂通配路由
├── internal/routing/routes.go   # 路由表：从 gateway.routes 加载，最长前缀匹配
├── internal/proxy/proxy.go      # 转发：上游解析 + 反向代理 + 状态码透传 + 转发 span
├── config.yaml                  # 唯一配置源（无任何地址硬编码）
└── README.md                    # 本文件
```

分层边界是**硬**的：

- `routing` 只回答「这个路径该转给哪个服务名」，不碰地址、不碰 HTTP；
- `proxy` 只回答「怎么把请求转出去」，不碰配置解析、不碰形态；
- 两个包内**都没有形态字面量与 `Mode()` 调用**（见 §5 第 ④ 条 grep）。

## 2. 转发行为

### 路由匹配

`gateway.routes` 是一张前缀表，按**最长前缀**匹配（表内按前缀长度降序排序）：

| 字段 | 含义 |
|---|---|
| `prefix` | 入站路径前缀，须以 `/` 开头 |
| `upstream` | 被调服务名（注册中心服务名 / `service.upstream.<name>` 的键后缀） |
| `strip` | 转发前是否剥掉前缀 |
| `name` | 决定 span 名 `gateway.proxy.<name>`，缺省取 `prefix` |

本 demo 只配了一条：`/api` → `demo-bff`，`strip: true`。

**前缀剥除**：`strip: true` 时剥掉前缀再转发 ——

```
GET /api/order/1001   →   demo-bff 收到 GET /order/1001
GET /api/             →   demo-bff 收到 GET /
```

剥完为空补 `/`。关掉 `strip`（改配置即可）则原样透传完整路径。

> hertz 的通配语义：`/api/*p` 匹配 `/api/...`，**不**匹配裸 `/api`
> （裸 `/api` 由 hertz 301 到 `/api/`）。E2 走的路径都带后缀，不受影响。

### 状态码

**原样透传**：上游 404 / 429 / 502 落到客户端就是同码，不改写、不统一成 500
——网关改写状态码会把上游的错误语义抹掉。

只有两种情形由网关自己产出 502，都带 JSON 错误体：

| 情形 | 响应 |
|---|---|
| 拿不到上游实例（发现与兜底都空） | `502 {"error":"proxy: no instance of upstream ..."}` |
| 上游不可达 / 超时 | `502 {"error":"proxy: dial upstream ..."}` |

### 上游地址解析

`proxy.Resolver` 两条路，**注册中心优先，配置直连兜底**：

1. 装配层从 `a.Registry()` 拿句柄，断言出发现能力（`coordx.Discover` 的同形
   子集 `Discovery(ctx, service)`）→ 取第一个健康实例的 `Addr:Port`；
2. 发现拿不到 → 读 `service.upstream.<被调服务名>`（`internal/mode/webx.go`
   里 launchx 自己用的同一个兜底键）；
3. 两条路都空 → 502（不静默换一个「猜出来的」上游）。

> **实测现状**：coordx 的 consul 驱动把注册与发现分成两个句柄
> （`coordx/consul/consul.go` 的 `registerImpl` 只有
> `Register/Deregister/HealthCheck/Close`，`Discovery` 在 `discoverImpl` 上），
> 而 `App.Registry()` 返回的是注册句柄。因此**第 1 步的断言当前必然失败**，
> 实际生效的是第 2 步的 `service.upstream.demo-bff`（`127.0.0.1:8081`）。
> 等 coordx 把注册/发现合成一个句柄，断言即自动生效、无需改本 demo —— 这也是
> 这里不做 `consul.NewClient` / `coordx.NewDiscover` 自建的原因（见 §5 第 ③ 条）。

## 3. 调用链：traceparent 必须显式注入

webx 的 otel 中间件**只做 Extract、不做 Inject**。所以转发前必须自己注入，
否则 gateway→bff 断成两根孤儿 trace（验收 T6/T7 的断链判据）。

```go
// internal/proxy/proxy.go
launchx.Inject(ctx, propagation.HeaderCarrier(req.Header))
```

走库侧入口而不是裸 `otel.GetTextMapPropagator().Inject`：注入口统一在 launchx，
传播器由 telemetry 步骤注册（`internal/telemetry/telemetry.go:56` 的
Composite{TraceContext, Baggage}），库侧换实现时本 demo 无需改动。
`launchx.Extract` 与 `launchx.PropagatorFields()` 一并导出，后者用于 T1 的字段断言。

span 层级（`gateway.proxy.*` 是 webx 入口根 span 的**子** span，不另起根）：

```
GET /api/order/1001              (SERVER, webx 的 otel 中间件开的根 span)
└── gateway.proxy.api            转发 span，internal/proxy/proxy.go
    └── HTTP GET                 出向 CLIENT span，otelhttp 包 http.Client 产出
        ↓ traceparent 经 HTTP 头注入
        demo-bff  GET /order/1001  →  handler.GetOrder → downstream... → client.demo-order...
```

出向那一层由 `otelhttp.NewTransport` 产出（`app/setup.go` 把 provider 交给
`proxy.New`，`proxy` 用它包 `http.DefaultTransport`）：它带来带 HTTP 语义属性的
CLIENT span（`http.method` / `http.url` / `http.status_code`），是整条链上唯一能
回答「转出这一跳花了多久、成没成」的位置 —— 只有 `gateway.proxy.*` 时，
Jaeger 上能看见"网关收到了请求"，看不见"上游那段到底怎样"。

otelhttp 内部也会注入 traceparent（用同一份 `launchx.Propagator()`），与上面那行
显式 `launchx.Inject` 是**重复而非冲突**：写的是同名字段、同一个 span context，
结果一致。两处都保留是刻意的 —— 显式那行让"续链发生在哪里"在代码里可读，
不必依赖 transport 的内部实现。

## 4. 运行

### 前置

```bash
# 1) 中间件底座（hubx/dockers 统一供给：consul / jaeger）
/Applications/Docker.app/Contents/Resources/bin/docker compose \
  -f /Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml up -d consul jaeger

# 2) 起上游（bff 必须在 127.0.0.1:8081 上就绪，否则 /api/* 一律 502）
cd examples/micro/bff && go run ./cmd
```

### 启动

```bash
cd examples/micro/gateway        # config.yaml 按相对路径解析，必须在此目录下启动
export PATH=/Users/hyx/sdk/go1.24.3/bin:$PATH
go run ./cmd
```

启动日志应出现 hertz 打出的 `/api/*p` 路由注册（GET/POST/PUT/… 各一条）
与 `HTTP server listening on address=127.0.0.1:8080`。

consul 不在时按 A3 降级：一条 Warn，进程**不退出**（要让它变成启动失败，
在 `config.yaml` 里设 `strict.registry: true`）。

> ⚠️ 本机 `127.0.0.1:8081` 已被 Docker Desktop 占用（监听 `*:8081`），
> 未起 bff 时 `/api/*` 会打到它上面（能看到 200/404 被原样透传，但不是 bff）。
> 若还有别的服务抢 `8080`，先在 `config.yaml` 里换掉 `webx.addr`。

### 两个端点

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/healthz/live`、`/healthz/ready` | webx 自带健康探针（裸 `/healthz` 是 404） |
| ANY | `/api/*` | 转发到 `demo-bff` |

```bash
curl -s --noproxy '*' http://127.0.0.1:8080/healthz/live              # {"status":"alive"}
curl -s --noproxy '*' -w '\nHTTP %{http_code}\n' http://127.0.0.1:8080/api/order/1001
curl -s --noproxy '*' -w '\nHTTP %{http_code}\n' 'http://127.0.0.1:8080/api/order/1001?user_id=1'
```

> 本机 shell 有 `http_proxy`，不加 `--noproxy '*'` 会把请求发给代理拿到 404。

## 5. E2 / T6 / T7 验收指引

```bash
export PATH=/Users/hyx/sdk/go1.24.3/bin:$PATH
cd /Users/hyx/work/gowork/src/hubx/launchx

# ① main.go ≤ 50 行
wc -l examples/micro/gateway/cmd/main.go                       # 49

# ② 地址全部配置化（换部署形态只改配置，不重新编译）
go test . -run 'TestServiceSourceHasNoHardcodedLoopbackAddr' -count=1   # PASS = 无硬编码回环地址
#    （原手写 grep 用 BRE 的 `\|`，在本机 grep 下静默零命中，而「必须为空」恰恰由零命中成立）

# ③ 不自建客户端：注册中心句柄只能经 a.Registry() 取
grep -rnE "gorm\.Open|redis\.NewClient|kafka\.(NewWriter|NewReader)|consul\.NewClient|coordx\.New" \
     --include='*.go' examples/micro/gateway/                  # 必须为空

# ④ 业务分层不含形态分支与 mode 分支
grep -rnE '"monolith"|"bff"|"gateway"|Mode\(\)' --include='*.go' examples/micro/gateway/internal/  # 必须为空

# ⑤ 本档不注册：代码里不应有任何 Register/Deregister 调用
grep -rnE "Register\(|Deregister\(" --include='*.go' examples/micro/gateway/   # 必须为空

# ⑥ 上游发现生效（完整链路在 Task 19 跑，本 Task 只要求编译通过 + 可启动）
curl -s 'http://127.0.0.1:8500/v1/health/service/demo-bff?passing=1' | jq 'length'   # ≥ 1
```

单进程冒烟（bff 不在时 `/api/*` 应返回 502，不要求链路真通）：

```
$ curl -s --noproxy '*' -w '\nHTTP %{http_code}\n' http://127.0.0.1:8080/api/order/1001
{"error":"proxy: dial upstream 127.0.0.1:1: Get \"http://127.0.0.1:1/order/1001\": dial tcp 127.0.0.1:1: connect: connection refused"}
HTTP 502
```

（`127.0.0.1:1` 是把 `service.upstream.demo-bff` 临时改成一个不可达地址的结果；
改成空串则是另一条 502：`no instance of upstream "demo-bff"`。）

一条命令取出本 demo 的 trace：

```bash
curl -s 'http://127.0.0.1:16686/api/traces?service=demo-gateway&limit=1' > /tmp/gateway-trace.json
```

### T13 反向断言

```bash
# config.yaml 里把 trace.enabled 改成 false，重启后重跑：
# gateway.proxy.* span 必须消失。改之前之后都通过，说明完整性检查是空壳。
```

## 6. 本 demo 刻意不做的事

| 不做 | 原因 |
|---|---|
| 不注册（代码里没有 `Register`/`Deregister`） | gateway 是流量入口，注册它只会让注册中心多一批无人调用的条目 |
| 不自建 `consul.NewClient` / `coordx.NewDiscover` | 发现句柄一律经 `a.Registry()` 取，拿不到发现能力就走配置兜底 |
| 不改写上游状态码 | 404/429/502 原样透传，改写会把上游错误语义抹掉 |
| 不在代码里写 `127.0.0.1` | 换部署形态只改配置，地址必须全部配置化 |
| 不覆盖 webx 的入口根 span | `gateway.proxy.*` 只做子 span，另起根就把链路断成两根 |
| 不省略 traceparent 注入 | webx 中间件只有 Extract，不注入就会断链（T6/T7） |
| 不建数据层、不接 MQ（无 sqlx/mqx 段） | 网关不落数据 |
| 不写形态字面量进 `internal/**` | 换形态只改 `config.yaml` 的 `service.mode`（C4） |

## 7. 形态无关

`internal/**` 内**不含任何形态分支**（见 §5 第 ④ 条 grep）。本 demo 只服务于
gateway 一档：换形态只改 `config.yaml` 的 `service.mode`，业务代码与部署形态
解耦 —— 换部署方式（宿主进程 / 容器 / 集群）都不需要重新编译。
