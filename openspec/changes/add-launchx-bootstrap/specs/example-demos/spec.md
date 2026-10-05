# example-demos delta

## ADDED Requirements

### Requirement: 三个自包含验证 demo

`examples/` 目录 SHALL 提供三个可复现的验证 demo：`monolith/`（单体，参考 `webx/examples` 分层与路由，订单存储经 launchx db 组件接真实 mysql）、`micro/`（微服，参考 `micro_apps` 精简的 `gateway → bff → srv` 完整调用链）、`mesh/`（服务网格，launchx 仓内自写最小 k8s + Istio 清单并适配微服 demo；`service_mesh/` 仓清单未交付，不作为 copy 源，仅其设计结论——业务零改动/sidecar 同 trace/地址段转换——作为编写依据）；三个 demo MUST 全部使用 `launchx.Bootstrap` 一行启动，不依赖参考仓的运行时代码；monolith 与 micro 的 e2e 底座 MUST 复用 `hubx/dockers/docker-compose.yml` 共享环境（consul / mysql / kafka / jaeger），不随仓自带 compose。

#### Scenario: 验收案例 E1 — 单体 demo 跑通

- **WHEN** 共享底座（`hubx/dockers` compose）就绪后按 `examples/monolith/README` 启动 demo
- **THEN** `Bootstrap().Run()` 起 webx 服务，订单读写经 launchx db 组件落真实 mysql，HTTP 请求经 Setup 挂载的分层（handler→logic→service→repository）返回预期响应，SIGTERM 正常退出

#### Scenario: 验收案例 E2 — 微服调用链跑通

- **WHEN** 共享底座（`hubx/dockers` compose，含 consul/mysql/kafka/jaeger）就绪后运行 `examples/micro/` 三服务
- **THEN** 请求经 `gateway → bff → srv` 完整调用链返回预期响应，srv 的 rpcx 服务与 bff 的下游客户端均经 consul 发现

### Requirement: 微服 demo 的 srv 为迁移实现

`examples/micro/` 中的 `srv` SHALL 由 `micro_apps/srv-order` 迁移而来：原 300 行 main.go 与 `infra/` 复制件替换为 `launchx.Bootstrap` 一行形态，原 e2e 下单链路（含 outbox 经 kafka 投递断言）MUST 全绿；kafka 复用共享底座（topic 自动创建，compose `KAFKA_CFG_AUTO_CREATE_TOPICS_ENABLE=true`）。

#### Scenario: 验收案例 E3 — srv-order 迁移回归

- **WHEN** 对迁移后的 srv 运行原 srv-order e2e（下单链路 + outbox 经 kafka 投递断言）
- **THEN** e2e 全绿（kafka 中可见 outbox 对应 topic 投递消息），且 `examples/micro/srv/cmd/main.go` 不超过 50 行

### Requirement: 网格 demo 清单自包含（launchx 仓内自写）

`examples/mesh/` SHALL 为 launchx 仓内自写的最小 k8s + Istio 清单（namespace / Deployment / Service / sidecar 注入 / bring-up 脚本），适配微服 demo 三服务；`service_mesh/` 仓清单未交付，MUST NOT 作为 copy 源，其设计结论（业务零改动、sidecar 与应用 span 同 trace、地址段转换）作为编写依据并在 README 注明来源；网格验收 SHALL 分两级：清单 + `kubectl apply --dry-run=client` 全绿为硬验收（自动化可复现），真实 minikube + Istio 部署验证 sidecar 与应用 span 汇入同一条 trace 为人工验证项（证据入 `examples/mesh/docs/verify.md`，close 阶段不硬阻塞）。

#### Scenario: 验收案例 E4 — 网格部署与同 trace 入链

- **WHEN** 执行 `examples/mesh/scripts/bring-up` 部署微服 demo 服务并发起调用
- **THEN** 服务以 sidecar 形态运行、业务代码零改动（Go 二进制与微服 demo 构建产物一致，仅配置副本地址段不同），jaeger 中可见应用 span 与 sidecar span 在同一条 trace（人工验证项，证据入 `examples/mesh/docs/verify.md`）

#### Scenario: mesh 清单 dry-run 硬验收

- **WHEN** 执行 `kubectl apply --dry-run=client -f examples/mesh/ -R` 与 `bash examples/mesh/scripts/bring-up.sh check`
- **THEN** 全绿（清单可解析、前置检查通过），该两级验收的第一级 MUST 在 build 内自动化通过

#### Scenario: 验收案例 E5 — trace 开关对比

- **WHEN** 分别以 `hubx.trace` 开关 OFF / ON 各发起一次调用（网格或微服环境均可）
- **THEN** OFF 不产生业务 span、ON 产生完整链路 span，对比可复现（人工验证项，证据入 `examples/mesh/docs/verify.md`）

### Requirement: 治理下沉边界（网格接管范围与反虚假陈述）

网格形态 SHALL 只接管与业务语义无关、且在当前传输协议下真正可用的治理能力：mTLS、基于服务身份的 L4 授权、连接级可观测，以及（可选）注册发现的平台化。配置中心 MUST NOT 下沉：Envoy 不参与配置分发，且 ConfigMap 不具备 Nacos 的长轮询热更新能力，替代即能力降级。注册发现若下沉，MUST 以**替换 `Registry` 实现**的方式达成（业务侧仍调用 `Register/Deregister`，实现退化为 k8s no-op），MUST NOT 通过删除业务侧注册调用来达成——后者会使微服与网格运行两份代码，直接违反「换形态不改业务代码」。

在 Kitex 私有 TCP 协议下（未声明 `appProtocol` 的端口由 Istio 按 TCP 处理），请求级重试 / 超时 / 熔断、L7 限流、JWT 用户态鉴权、按 path 或 header 的灰度切分 SHALL 继续由应用侧承担，网格侧 MUST NOT 重复开启（重试双侧开启会造成重试放大）。任何写入 `examples/mesh/docs/verify.md` 的「治理已由网格接管」结论 MUST 同时指出应用侧已撤除或已替换实现的具体位置；指不出 MUST NOT 作此表述。

#### Scenario: 治理下沉可追溯

- **WHEN** 核对网格形态的治理分工
- **THEN** 每一项声称已下沉的能力均可指出应用侧对应处已撤除或已换实现；未指出的能力仍留在应用侧且两侧未重复开启（尤指重试）

### Requirement: 验收案例标注对齐

每条验收案例 MUST 在 `specs/*/spec.md` 的 `#### Scenario:` 名称（A1–E5 编号）与 `tasks.md` 对应验证项之间一一标注对应，供 close 阶段核对。

#### Scenario: 案例可追溯

- **WHEN** 核对验收案例清单
- **THEN** A1–E5 每个编号均可在 spec Scenario 与 tasks.md 验证项中双向找到对应条目
