# examples/monolith —— 单体形态 demo（验收案例 E1 / T5 / T13）

一个最小可跑的订单服务，用来证明 launchx 的**单体形态**是三段式成立的：

```go
a, err := launchx.Bootstrap("config.yaml")   // 1. 读配置 + 装配地基
a.OnSetup(app.Setup)                          // 2. 业务装配（取句柄、挂路由）
a.Run()                                       // 3. 起服务并托管退出
```

`cmd/main.go` **50 行**，只做这三件事：不开数据库连接、不装配 exporter、不注册节点。

## 1. 目录结构

```
examples/monolith/
├── cmd/main.go                  # 50 行：Bootstrap → OnSetup → Run
├── app/setup.go                 # 装配层：取 DB/引擎句柄 → 构造三层 → 挂路由
│   └── routes.go                # 唯一路由注册源
├── internal/handler/order.go    # HTTP 层：绑参 + 状态码，自开 handler.* span
├── internal/service/order.go    # 业务层：校验 + 编排，自开 service.* span
├── internal/repository/order.go # 数据层：单表/连表/事务，自开 repository.* span
├── internal/model/order.go      # orders / order_items / users 三表模型
├── config.yaml                  # 唯一配置源（无任何地址硬编码）
├── scripts/bring-up.sh          # 建表 + 授权 + 种子（幂等）
└── README.md                    # 本文件
```

分层边界是**硬**的，因为它直接决定 T13 的分层断言：

- `service` 不碰 `*gorm.DB`，`repository` 不做参数校验与业务决策；
- `handler` 是 hertz 唯一出现的地方，往下没有任何 HTTP 概念。

## 2. 运行

### 前置

```bash
# 1) 中间件底座（hubx/dockers 统一供给）
docker compose -f /Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml up -d mysql jaeger

# 2) 库密码（config.yaml 的 DSN 用 ${HUBX_DB_PW} 展开）
export HUBX_DB_PW=hubx123

# 3) 建表 + 授权 + 种子用户（幂等，可反复执行）
cd examples/monolith && ./scripts/bring-up.sh && ./scripts/bring-up.sh check
# 期望输出：OK tables:orders order_items users db:order_db users:2
```

### 启动

```bash
cd examples/monolith            # config.yaml 按相对路径解析，必须在此目录下启动
export PATH=/Users/hyx/sdk/go1.24.3/bin:$PATH
export HUBX_DB_PW=hubx123
go run ./cmd
```

启动日志应出现 `service.mode=monolith`，且**不出现任何注册/deregister 日志**（单体不注册，验收陷阱第 11 条）。

## 3. 四个接口

| 方法 | 路径 | 说明 | T13 落点 |
|---|---|---|---|
| POST | `/orders` | 建单：事务写 orders + order_items | 事务两条 INSERT 各自成 span |
| GET | `/orders?limit=20` | 单表列表 | SELECT |
| GET | `/orders/:order_no` | 单表单条 | SELECT |
| GET | `/orders/:order_no/join` | **连表**：orders JOIN users | JOIN 语句单独成 span |
| PUT | `/orders/:order_no` | 改状态（会回读一次） | UPDATE + SELECT |
| DELETE | `/orders/:order_no` | 删主子数据 | DELETE × 2 |

```bash
NO=$(curl -s -XPOST localhost:8080/orders -H 'Content-Type: application/json' \
     -d '{"user_id":1,"item":"e2e","qty":2,"unit_price":9.9}' | jq -r .order_no)

curl -s "localhost:8080/orders?limit=5"        | jq 'length'      # >= 1
curl -s "localhost:8080/orders/$NO"            | jq .status       # "created"
curl -s "localhost:8080/orders/$NO/join"       | jq .user_name    # "alice"（JOIN 生效）
curl -s -XPUT "localhost:8080/orders/$NO" -H 'Content-Type: application/json' \
     -d '{"status":"paid"}'                    | jq .status       # "paid"
curl -s -XDELETE "localhost:8080/orders/$NO"   | jq .deleted      # true
```

## 4. 验收指引

按 `examples/ACCEPTANCE.md` 跑，本 demo 对应三段：

| 验收 | 章节 | 一句话判据 |
|---|---|---|
| **E1** 单体跑通 | §3 | 下单后 mysql 真有行；SIGTERM 后 20s 内退出 |
| **T5** 树形态 | §5 | 根 span 恰好 1 个、树深 ≥ 3、无孤儿 |
| **T13** 调用链完整性 | §9b | 三层各 ≥1 span、JOIN 单独成 span、四类 CRUD 齐全 |

一条命令取出本 demo 的 trace：

```bash
curl -s 'http://127.0.0.1:16686/api/traces?service=demo-monolith&limit=1' > /tmp/monolith-trace.json
```

### T13 反向断言

```bash
# config.yaml 里把 trace.on.db 改成 false，重启后重跑 T13 检查 —— 必须失败。
# 改之前之后都通过，说明完整性检查是空壳。
```

## 5. 本 demo 刻意不做的事

| 不做 | 原因 |
|---|---|
| 不注册（无 coordx/consul 段） | 单体形态不注册；出现 deregister 即串档（陷阱 11） |
| 不用 MQ | 单体形态不需要；微服案例才涉及 outbox→kafka |
| 代码内不出现 `127.0.0.1` / `localhost` | 网格要复用同一份二进制，地址必须全部配置化（陷阱 14） |
| 不做进程内 AutoMigrate | 表结构由 `scripts/bring-up.sh` 显式准备，避免隐式依赖 |
| 不自建 DB 连接 | DB 句柄只能经 `a.DB().(*gorm.DB)` 取 |

## 6. 与微服 / 网格的关系

本 demo 的业务三层（handler/service/repository）**不含任何形态分支**
（注意：模块路径本身含 `monolith`，故判据要排除 import 行）：

```bash
# 业务三层不得出现 mode 判定或形态取值字面量（import 路径含 monolith 不算）
grep -rnE '"monolith"|"bff"|"gateway"|Mode\(\)|service\.mode' --include=*.go internal/   # 必须为空
```

换形态只改 `config.yaml` 的 `service.mode`；网格形态复用**同一份二进制**，
差异只在配置副本与部署清单。
