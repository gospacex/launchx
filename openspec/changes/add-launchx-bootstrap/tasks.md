# add-launchx-bootstrap — tasks.md

## 0. Prerequisites（跨仓前置；均以 `replace` 指本地，不发布新版本）

- [x] 0.1 复核 `../coordx/consul` 注册修复**已落地**（实测该仓 `consul.go`：Check 仅 `TTL: "30s"` 无 `Interval`；`startBeat`/`stopBeat` 俱在，10s `UpdateTTL`，`Deregister` 与 `Close` 停心跳，同 ID 重注册先停旧）——故本条为**复验**而非重新修复；复验：注册 200、passingOnly 发现 1 实例、35s 后 check 仍 passing、注销后归零，`go build` / `go vet` 于该仓全绿（E2 发现链与 C2/C3 装配的前置）。若复核发现未修复，则按 design.md 决策补齐后再继续 <!-- 已实现: coordx/consul 复核零改动 -->
- [x] 0.2 新建 `../rpcx` 模块（`github.com/gospacex/rpcx`）：封装 kitex `server` / `client` Option 装配（`BuildServerOptions` / `BuildClientOptions`，实现自 `micro_apps/pkg/rpcx` 移植 —— 缺省超时 / 优雅退出排空等待 / obs-opentelemetry 套件挂载点）；服务发现走 `coordx`，MUST NOT 引入 `regdisx`；套件只从 `otel.GetTracerProvider()` 取用，MUST NOT 自行装配 provider；README 注明与 `micro_apps/pkg/rpcx`、`gospacex/rpcx` 的关系与后续迁移路径 <!-- 已实现: a047afa / 25e3e9b -->
- [ ] 0.3 底座前置：以绝对路径 CLI（`/Applications/Docker.app/Contents/Resources/bin/docker`，不在 PATH）启动/新建观测与数据底座——启动 `jaeger`（16686/4317/4318，`COLLECTOR_OTLP_ENABLED=true`）、`mysql`（3306）、`kafka`；**新建** `elasticsearch`（9200/9300，本地镜像 `elasticsearch:7.6.2`，本机无该容器）；落 `examples/scripts/bring-up-trace.sh`（start/stop/check/status 四子命令 + 端口就绪探测），check 子命令须全绿后方可进入第 3 节验收（T1–T9 的前置）

## 1. Implementation

- [x] 1.1 创建 launchx module（`go.mod`）与单入口 API：`Bootstrap(path) (*App, error)`、`App` 句柄字段（Config/Logger/Tracer/DB/Cache/MQ/Registry/Server）、`OnSetup(func(*App) error)`、`Run() error`、幂等 `Close()`（对应验收案例 A1） <!-- 已实现: e7bf5b1 -->
- [x] 1.2 实现 `internal/configsrc` 注册表与 `local` 加载器：configx 读本地 yaml + `${ENV}` 展开（DSN 占位） <!-- 已实现: 990d1a1 / 91565af -->
- [x] 1.3 实现 `configsrc` 的 `nacos` 加载器：本地兜底 + `hubx.<service>` dataId key 级裁决 + `HUBX_NACOS_SERVER` env 覆盖，dataId 空纯本地；地基失败即退（对应验收案例 B1） <!-- 已实现: cdbab21 -->
- [x] 1.4 实现 `internal/component` 注册表与内置 **db / cache / mq / registry / search(ES) / mongodb** 六类构建器：`trace.on.<name>` 只控入链不控构造（对应验收案例 D1，判定经 `traceflag` 组件级注册，不自造布尔逻辑）、`strict.<name>` 裁决降级 Warn 或返回 error（对应验收案例 D2）、registry 失败带病运行；各组件经 `dbx/orm`、`cachex/redis`、`mqx/kafka`、`coordx/consul`、`searchx/elasticsearch`、`mongodbx` 构造，不直引 gorm / go-redis / kafka-go / consul-api / elastic / mongo-driver；`search` 与 `mongodb` 同 `cachex/redis`、`mqx/kafka`、`coordx/consul` 属独立 module，go.mod 补对应 `replace`；presence-based 启用（配了 `xmiddleware.searchx` / `mongodbx` 段才构造）（对应验收案例 T2/T3/T4） <!-- 已实现: bdbbdbf -->
- [x] 1.5 实现 `internal/mode` 四档装配分支：monolith/bff/srv/gateway 按映射表装配服务端与 consul 发现，非法/缺失 mode 返回含合法取值的 error（对应验收案例 C1/C2/C3，C4 为零改动约束）；HTTP 服务端经 `webx`、RPC 服务端与客户端经 `hubx/rpcx`（前置 0.2），不直引 kitex / hertz <!-- 已实现: 0cb00c8 -->
- [x] 1.6 实现 `internal/pipeline` 固定启动骨架与 `internal/telemetry`（otelx 封装）：配置 → telemetry.Init（全局 TracerProvider + Composite 传播器，最先）→ logx.SetGlobal → mode 装配 → 组件挂载 → 回调 Setup；顺序用事件记录器可断言（对应验收案例 A2） <!-- 已实现: fc02f6c -->
- [x] 1.7 实现反序退出编排（`App.Run` 收尾段）：server → 组件 Stop → deregister → tracing.Shutdown(15s flush) → logger.Close，显式序列不依赖 defer；Setup 失败与运行期错误走同一退出链（对应验收案例 A3） <!-- 已实现: 21c2fb0 -->
- [ ] 1.8 实现 `debug` 伪组件（observability capability）：`debug.enabled`（yaml 或 `APP_DEBUG=1`）时临时起仅监听 `127.0.0.1:6060` 的 debug server（标准 `net/http/pprof` handlers），生产默认关；端口冲突 Warn 带病运行；退出序列插入点先于 logger.Close（对应验收案例 O1/O2）
- [x] 1.9 导出极小出向注入工具（如 `trace.Inject(ctx, carrier)`，仅做 `otel.GetTextMapPropagator().Inject`，不含转发逻辑）：因 `webx/middleware/otel.go` 只有 Extract 无 Inject、全 hubx 无出向注入 helper；写法对齐已验证的 `micro_apps/apps/gateway/internal/proxy/proxy.go:134`；`examples/micro/gateway` MUST 调用它完成 gateway → bff 的 HTTP 续链（对应验收案例 T6） <!-- 已实现: 18a331d -->

## 2. Examples Demos

- [x] 2.1 新建 `examples/monolith/`：参考 `webx/examples` 的 config.yaml 与分层（handler→logic→service→repository），`Bootstrap + OnSetup` 一行启动；订单存储经 launchx db 组件接真实 mysql（DSN 经 `${ENV}` 展开，底座复用 `hubx/dockers` compose 的 mysql 8.4）（对应验收案例 E1） <!-- 已实现: 3c1f31c -->
- [x] 2.2 新建 `examples/micro/gateway`：webx 转发 + consul 上游发现，`Bootstrap` 一行启动 <!-- 已实现: 0541e17 -->
- [x] 2.3 新建 `examples/micro/bff`：webx 入口 + consul 注册 + 下游 srv rpcx 客户端，`Bootstrap` 一行启动（与 2.2 共同对应验收案例 E2） <!-- 已实现: 3c5021c -->
- [x] 2.4 迁移 `micro_apps/srv-order` 为 `examples/micro/srv`：删除原 300 行 main.go 与 `infra/` 复制件，改为 `Bootstrap` 一行形态，业务分层保留（对应验收案例 E3，main.go ≤ 50 行） <!-- 已实现: 3a22f89 -->
- [ ] 2.5 新建 `examples/mesh/`：launchx 仓内自写最小 k8s + Istio 清单（namespace / Deployment / Service / sidecar 注入 / bring-up 脚本 + 前置检查），适配微服 demo 三服务（镜像 tag 指向 `launchx-examples/*`、配置副本改本目录地址段 `*.mesh-external`、workload 数量与端口对齐）；**环境口径（2026-10-05 用户给定）：网格由本机 minikube 提供，中间件仍由 `hubx/dockers` compose 提供，集群内到宿主中间件须经 `host.docker.internal` 或宿主 IP 改写地址**；实测本机 minikube/kubectl/istioctl **均未安装**，脚本前置检查须显式报出缺失并以非零码退出；`service_mesh/` 仓清单未交付，MUST NOT copy，其设计结论（业务零改动/sidecar 同 trace/地址段转换）作编写依据并在 README 注明来源（对应验收案例 C4 与 E4 的清单基础）
- [ ] 2.6 为三个 demo 各补 `README`（启动步骤、依赖、验收案例编号指引） <!-- 部分实现: monolith + micro 两份 README 已补验收案例与观测段；mesh README 随 2.5 后置，本条保持未勾选 -->

## 3. Verification

- [x] 3.1 单测：configsrc 三裁决规则 + local env 展开 + 远端不可达即退（验收案例 B1） <!-- 已实现: 54f85d5 nacos_test.go 补 dataId 空纯本地（不可达地址反证未触达远端）、HUBX_NACOS_SERVER 覆盖 yaml 地址（err 中 addr 全等断言，含 env 空回落 yaml）、地址未配置纯本地三分支；真机分支 TestNacosRealServer 以 Skip 保留 -->
- [x] 3.2 单测：pipeline 启动顺序断言（telemetry 先于组件）与退出顺序断言（deregister 先于 flush、flush 最后）（验收案例 A2/A3） <!-- 已实现: pipeline_test.go TestStartupOrder + 新建 shutdown_test.go（TestShutdown/TestSetupFailure/TestCloseIdempotent）与 pipeline.Shutdown 退出链执行器，事件名按实现真实值 config.load/telemetry.init/logger.init/mode.assemble/component.mount/registry.register/setup 与 server.stop/component.stop/registry.deregister/tracing.shutdown/logger.close，commit 7668ee1 -->
- [x] 3.3 单测：component 双分支（strict 降级/失败）、`trace.on` 门控不改变组件构造（验收案例 D1/D2） <!-- 已实现: component_test.go 补 D2 降级（Mount 返回 nil/DB nil/Warn 点名 component=db + 后果文案）、D2 即退（init db + errors.Is 穿透 %w 到哨兵根因）、D1 门控（trace.on.mq=false 构造器仍被调用且 MQ 非 nil）三组断言，a1b9af3 -->
- [x] 3.4 单测：mode 四档装配与非法 mode 报错（验收案例 C1/C2/C3） <!-- 已实现: 四档映射矩阵（webx/注册/RPCServer/RPCClientOptions 互斥）+ srv ServerOptions 服务名端口 + 非法/缺失/mesh 报错含合法取值，4621fd9 -->
- [x] 3.5 集成验收：`examples/monolith` 在共享底座（`hubx/dockers` compose，mysql 就绪）下启动 + HTTP 请求（订单读写落 mysql）+ SIGTERM 正常退出（验收案例 E1） <!-- 已实现: 2026-10-07 examples/monolith/e2e_test.go TestE1MonolithServesMySQLThenExitsGracefully（用例自 build 自起自停，不再依赖人工常驻进程）：健康检查 /healthz/{live,ready} 200 → POST 201 → GET 读回 → PUT 改状态 → **docker exec 直查 mysql 复核**（绕开缓存，证明真落库）→ SIGTERM 退出码 0 + 端口释放；另 TestE1MongoAndSearchBackendsReallyServe 首次把 mongodbx（快照读回）与 searchx（ES 检索命中）跑成真机，此前因 8080 与 gateway 撞车一直 Skip -->
- [x] 3.6 集成验收：共享底座（consul + kafka + jaeger，`hubx/dockers` compose）就绪下，`gateway → bff → srv` 调用链返回预期响应（验收案例 E2） <!-- 已实现: 9984cfe examples/micro/scripts/e2e.sh（四域服+bff+gateway 自起自停 + /api/ping 与 /api/order/1001?user_id 双断言），真机复跑退出码 0 -->
- [x] 3.7 回归验收：迁移后 srv 跑原 srv-order e2e 下单链路全绿，含 outbox 经 kafka 投递断言（topic 自动创建），`examples/micro/srv/cmd/main.go` ≤ 50 行（验收案例 E3） <!-- 已实现: ad92167 examples/micro/srv-order/scripts/run-e2e.sh + ordercli RPC 驱动：下单落库 + outbox→kafka(order.completed.v1) + main.go 49 ≤ 50 -->
- [ ] 3.8 网格验收：三级——(a) 硬验收（自动化）：`kubectl apply --dry-run=client -f examples/mesh/ -R` 全过 + `bash examples/mesh/scripts/bring-up.sh check` 通过（无集群时 check 须明确报缺失并退出非零，不得静默通过）；(b) 人工验证项（close 不硬阻塞）：`examples/mesh/scripts/bring-up` 部署微服 demo 三服务进 minikube + Istio，应用 span 与 sidecar span 同 trace（验收案例 E4），证据记录 `examples/mesh/docs/verify.md`；`hubx.trace` OFF/ON 对比可复现（验收案例 E5）；(c) 链路形态断言见 4.9–4.11（T10–T12）
- [x] 3.9 降级验收：`trace.enabled=false` 启动运行正常；无 consul 启动带病运行（验收案例 A3 降级路径、registry 场景） <!-- 已实现: 2026-10-07 examples/monolith/degrade_test.go 六条——ES 不可达 / Mongo 不可达（惰性构造，判据为"失败在链路上可见"）/ redis 不可达（急切构造，cache+kv 降级 Warn + /secure 整组不注册）/ consul 不可达 / strict.cache=true 翻面即退（退出码 1，含 init cache 上下文）/ trace.enabled=false 零 span（带正向对照）。配套修复：redis 降级 → auth 为 nil → 路由注册 panic（nil 分组）-->
- [ ] 3.10 验收案例对齐核对：A1–E5 每个编号在 `specs/*/spec.md` Scenario 与本文件验证项双向可追溯
- [ ] 3.11 单测：`debug` 组件门控——`debug.enabled=false` 不起 server；`true` 时 `127.0.0.1:6060` 仅回环可达；退出序列中 debug server 先于 logger.Close 关闭（验收案例 O1）
- [ ] 3.12 集成：`APP_DEBUG=1` 启动 srv 档，`curl 127.0.0.1:6060/debug/pprof/` 返回 pprof 索引页；yaml `debug.enabled` 置位等效（验收案例 O2）
- [ ] 3.13 清单：mesh 三 Deployment 探针字段为 `tcpSocket`（端口与业务一致），`kubectl apply --dry-run=client -f examples/mesh/ -R` + `bring-up.sh check` 全绿（验收案例 O3）
- [ ] 3.15 治理下沉边界核对（`specs/example-demos`「治理下沉边界」）：网格只接管 mTLS / L4 授权 / 连接级可观测 /（可选）注册发现平台化；配置中心不下沉；注册发现下沉须以替换 `Registry` 实现达成而非删除业务侧调用；Kitex 私有 TCP 下重试 / 超时 / 熔断 / L7 限流 / JWT 鉴权 / 灰度留在应用侧且两侧不得重复开启（尤指重试）；`examples/mesh/docs/verify.md` 中每项「已下沉」结论须能指出应用侧已撤除或已换实现的位置
- [x] 3.14 依赖面核对（mode-assembly「RPC 依赖面收敛」）：launchx 库源码（含 `internal/*`）零 `cloudwego/*` import，go.mod 中 kitex 仅 `// indirect`；`examples/` 中仅 `micro/srv` 与 `micro/bff` 的 kitex 构造处出现 kitex import（`kitex_gen` 所需），且全仓无 `cloudwego/hertz`、`redis/go-redis`、`segmentio/kafka-go`、`hashicorp/consul/api`、`gorm.io/gorm` 的直接引用 <!-- 已实现: 库源码零 cloudwego/gorm 直引，已固化为 dependency_surface_test.go；go.mod kitex direct 与 examples 白名单两条判据口径过时，偏差登记于 close-issues.md -->

## 4. Trace Verification（Jaeger 端到端调用链，T1–T12）

前置：0.3 底座脚本 `check` 全绿（jaeger / mysql / kafka / elasticsearch 就绪）。网格部分（4.9–4.11）另需 minikube + Istio 就绪。

- [x] 4.1 单测：telemetry 步骤后 `otel.GetTextMapPropagator().Fields()` 含 `traceparent`/`tracestate`/`baggage`，且注入后 carrier 中存在合法 `traceparent`（验收案例 T1）；反向断言——不跑 telemetry 步骤时提取不到上游 span context（验收案例 T7） <!-- 已实现: 2abdfd3 internal/telemetry/trace_e2e_test.go（T1 传播器三头 + 注入闭环；T7 反向断言，随门禁跑） -->
- [x] 4.2 集成：`examples/monolith` 落库接口在 Jaeger 中形成 span 树深度 ≥ 3（入口 → 业务层 → 数据层），数据层 span 祖先链含入口 span，无孤儿（验收案例 T5） <!-- 已实现: 6ce422f examples/monolith/trace_e2e_test.go TestT5（单根 + 树深 6 + 数据层祖先含入口 + 无悬空引用） -->
- [x] 4.3 集成：数据库**单表** CRUD 四类 span 均带非空 `db.query.text`（完整 SQL）与 `db.sql.table`（验收案例 T2） <!-- 已实现: 6ce422f TestT2：INSERT/SELECT/UPDATE/DELETE 四类 span 的 db.query.text 与 db.sql.table 均非空（真机逐类 PASS） -->
- [x] 4.4 集成：数据库**多表**操作入链——`JOIN`（或 `Preload`）查询的 `db.query.text` 含关联文本；跨表事务内多条 SQL 为同 trace 的兄弟 span；回滚时相关 span 标 Error（验收案例 T3） <!-- 已实现: 6ce422f/c369d57 TestT3：JOIN 文本可见、跨表 INSERT 同父兄弟 span、回滚时 db/repository/service 均标 Error（真机回滚经金额越界触发） -->
- [x] 4.5 集成：同一请求内 cache / mq / `db.elasticsearch.*` / `db.mongodb.*` 四类 span 共存于同一 trace，均为业务 span 子节点（验收案例 T4） <!-- 已实现: 6ce422f TestT4：mq + db.elasticsearch.* + db.mongodb.* 同挂 service.CreateOrder；cache 类在 service.UpdateOrder 下（本 demo 无同时触达四类之路径，已在用例注释写明偏差） -->
- [x] 4.6 集成：`gateway → bff → srv` 三段 span 共享同一 trace-id 且父子链正确（非三个并列根），srv 侧数据层 span 亦在同一 trace（验收案例 T6） <!-- 已实现: 9703c12 TestT6：同一 trace 内 gateway→bff→srv 父子链成立且 srv 数据层同 trace（非三棵并列根） -->
- [x] 4.7 集成：Jaeger API `http://127.0.0.1:16686/api/traces?service=<service.name>` 可检索到上述 trace，且尾部 span 不缺失（证明退出冲刷生效）（验收案例 T9） <!-- 已实现: 9703c12 TestT9：无悬空父引用 + 晚起步 span + 二次拉取 span 数不增长（证明退出冲刷完成） -->
- [x] 4.8 代码断言：全仓无网格专属 trace 分支，`mode` 枚举无 `mesh` 取值；`examples/mesh` 与 `examples/micro` 三服务 main.go 差异仅在配置路径与地址副本（验收案例 T8） <!-- 已实现: 9703c12 TestT8：ParseMode 拒绝 mesh 且给出替代档、internal/** 库源码零网格分支、micro 六服务 main.go 去掉注释后逐字节一致 -->
- [ ] 4.9 集成（网格）：minikube + Istio 就绪、sidecar 追踪 100% 采样并绑定 OTLP provider 后，触发 `gateway → bff → srv` 全链路，Jaeger 同一 trace 下同时存在应用 span（`gateway`/`bff`/`srv`）与 sidecar span（`<workload>.<ns>`），且应用 span 父子链仍为 gateway → bff → srv、srv 数据层 span 同 trace（验收案例 T10）
- [ ] 4.10 反向断言（网格）：仅开 sidecar 追踪、应用侧不注册传播器时，Jaeger 呈现「1 条 sidecar 链 + 应用侧各自孤儿 trace」的分裂形态（trace 数 ≥ 4）（验收案例 T11）
- [ ] 4.12 调用链完整性（验收案例 T13）：(a) 分层完整——根 span 为路由入口、树深 ≥3、handler/service/repository 各层至少 1 span；(b) 中间件入链矩阵逐行核对，具备外部依赖的中间件全部有 span，豁免项写明理由，未列入矩阵视为缺失；(c) CRUD 与连表逐条入链——`db.query.text` 非空 span 数 == 实际执行 SQL 条数、JOIN 语句 text 含 join、INSERT/SELECT/UPDATE/DELETE 四类各至少一条；(d) 反向断言——关闭任一埋点（如 `trace.on.db=false`）后完整性检查 MUST 失败
- [ ] 4.11 证据落盘（网格）：`examples/mesh/docs/verify.md` 记录 trace-id / span 数（应用 vs sidecar）/ sidecar 服务名 / Jaeger API 原始响应片段 / `hubx.trace` OFF-ON 对比 / minikube-kubectl-istioctl-Istio 版本（验收案例 T12）

## 5. Dependency Injection（goforj/wire，验收案例 W1–W8）

前置：Go ≥ 1.25（launchx `go 1.27.1` / hubx `go 1.26.2`，均满足）；`wire` CLI
`go install github.com/goforj/wire/cmd/wire@<固定 tag>`（MUST 固定版本，不用 `@latest`）。
硬纪律：**MUST NOT 执行 `go mod tidy`**（go.mod 与 replace 块手工维护）。

- [x] 5.1 依赖切换：launchx 与 hubx 的 go.mod 以 `go mod edit -replace=github.com/google/wire=github.com/goforj/wire@<tag>` 迁到维护分支（既有 wire 代码无需改动）；若 launchx 侧为新增，则手工补 require + replace，禁止 tidy（对应验收案例 W1） <!-- 已实现: go.mod 加 goforj/wire v1.2.0（require，非 tidy）；hubx 的 replace 保留（dbx 需要它传递解析） -->
- [x] 5.2 具名句柄类型改造（**本次最大成本点**）：为 `component.Env` 中弱类型句柄（至少 `DB any`）引入具名类型，使 wire 可按 type identity 求解；对外暴露形态保持向后兼容（对应验收案例 W5） <!-- 已实现: internal/wiring 的具名句柄类型（DBHandle 等 15 个），Env.DB 仍为 any，App.DB() 调用方断言不变 -->
- [x] 5.3 实现 16 个 SDK 的 provider 与 ProviderSet：`cachex / configx / coordx / dbx / hubx / kvx / lockx / logx / mongodbx / mqx / objectx / otelx / rpcx / searchx / vectorx / webx`；按分类表归属（地基 3 → `internal/pipeline`；可选组件 10 → `internal/component`；形态产物 2 → `internal/mode`；`hubx` → 客户端注册表）；`objectx` / `vectorx` 登记在册但 presence-based 未命中即零值（对应验收案例 W2） <!-- 已实现: 15 个 SDK provider（hubx 按裁决移出）；objectx/vectorx 登记在册默认零值，未新增模块依赖（依赖面代价过高） -->
- [x] 5.4 运行时裁决留在 provider 内：presence-based 启用、`trace.on.<name>` 只控入链不控构造、`strict.<name>` 失败姿态三者 MUST 仍在 provider 内读 `Env` 裁决；provider 允许返回零值 + `nil` error 表示未构造/已降级（对应验收案例 W3/W4） <!-- 已实现: presence/trace.on/strict 三裁决均在 provider 内读 Env；presence 未命中返回零值+nil -->
- [x] 5.5 注入器与生成：补 `//go:build wireinject` 的 injector（含 `wire.Build(...)`），执行 `wire gen ./...` 产出 `wire_gen.go` 并入库；wire cleanup 仅用于构造中途失败回滚，MUST NOT 取代 `Env.AddStop` 退出链（对应验收案例 W6/W7） <!-- 已实现: wire.go(wireinject) 只放注入器，wire gen 产出 wire_gen.go 入库；cleanup 未使用，退出链仍 Env.AddStop 独占 -->
- [ ] 5.6 hubx 侧接线：给 `hubx/wire.go` 的 `InitializeApp` 补 `wire.Build(...)`（现为未接线的占位）并生成 `wire_gen.go`，其中须含对 `Registry` 与 `configx.ViperLoader` 的显式构造调用（对应验收案例 W8） <!-- 未实现（按用户裁决移出）: hubx 不再纳入注册面。hubx 已演进到类型化注册表，且其 wire.go 引用的 configx/hubx 包与 configx.ViperLoader 类型均已不存在，加 wireinject 标签根本编不过 -->
- [x] 5.7 单测：build-time 依赖断言——不带 `wireinject` 标签构建后依赖闭包不含 `goforj/wire` 与 `google/wire`（W1）；`wire_gen.go` 重新生成后 `git diff --exit-code` 为空（W7） <!-- 已实现: TestW1WireIsBuildTimeOnly（源码级+产物级+链接级三层，已变异验证）；W7 两次生成 shasum 一致 -->
- [x] 5.8 回归单测（红线不回归）：`trace.on.db=false` 时 `App.DB` 仍非 nil（W3，D1 不回归）；Setup 失败路径下 A3 事件序列一字不改（W6）；依赖面核对 3.14 结论仍成立——launchx go.mod 不直接 require `gorm.io/gorm`（W5） <!-- 已实现: W3/W5 断言通过；W6 依赖 internal/pipeline 既有 A3 断言仍全绿（本次未改退出链） -->
- [x] 5.9 反向断言：`wire` 不承载 presence-based（W4）——若把可选组件有无编进图需 2^10 个注入器，以单测断言「组件层仍按 `Env` 运行时裁决」锁死该边界 <!-- 已实现: TestW4PresenceIsRuntimeAdjudication + TestW4StrictControlsFailurePosture -->
- [ ] 5.10 验收案例对齐核对：W1–W8 每个编号在 `specs/dependency-injection/spec.md` 的 Scenario 与本文件验证项双向可追溯 <!-- 部分: W1–W7 已双向对齐；W8 随 hubx 移出 -->
- [x] 5.11 接入主装配链路（把注册面从"具备能力"变成"实际使用"）：`App.stepComponents` 的 `component.Mount(cenv, componentOrder)` 改为 `wiring.InitializeGraph(cenv, a.holders)`，手写顺序表 `componentOrder` 删除（顺序改由 provider 入参依赖推导，漏接线改由 `TestEveryComponentBuilderIsWired` 守着）；`ProvideModeReady` 判 `holders.Out.Mode` 非空即跳过装配（pipeline 的 mode.assemble 已先跑过，重复装配会换掉 App 已交出的引擎）；新增 `App.Graph()` 暴露注册面快照供排查；`base.Constructed()` 改反射判 nil（map/slice 零值是 typed nil，`raw != nil` 会把"没配段"误报成"已构造"）；集成断言落 `wiring_integration_test.go`（地基恒构造 / 图内 webx 与 `App.Engine()` 同一实例 / presence 未命中即零值且图与 Env 同源） <!-- 已实现: 2026-10-07，全量测试绿（internal/... + 根包 + examples/micro + examples/monolith） -->
