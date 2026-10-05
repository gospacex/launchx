# observability delta

## ADDED Requirements

### Requirement: metrics 全走 otel 推送，零新增业务 HTTP 端点

系统 SHALL 复用 otelx 通道推送 metrics（底座 jaeger/collector），四档 mode（monolith/bff/srv/gateway）统一；MUST NOT 为观测目的新增业务 HTTP 端点；`srv` 档（无 webx HTTP server）亦 MUST 可被观测。metrics 推送失败 MUST NOT 影响主流程。

#### Scenario: 验收案例 O2 — srv 档 pprof 可达（debug 组件联动）

- **WHEN** `APP_DEBUG=1` 启动 `srv` 档
- **THEN** `curl 127.0.0.1:6060/debug/pprof/` 返回 pprof 索引页；业务 rpcx 端口不受影响

#### Scenario: metrics 推送失败不影响主流程

- **WHEN** otelx collector 不可达
- **THEN** 服务正常启动与处理请求，仅记录 Warn（与 registry 降级策略一致）

### Requirement: Pprof 走 debug 伪组件

组件注册表 SHALL 内置 `debug` 伪组件：`debug.enabled`（yaml 置位或 `APP_DEBUG=1`）时临时起**仅监听 `127.0.0.1:6060`** 的 debug server（标准 `net/http/pprof` handlers）；生产默认关；端口冲突 SHALL Warn 带病运行（不即退，与 registry 降级一致）；退出时 debug server MUST 先于 `logger.Close` 关闭（插入反序退出链）。

#### Scenario: 验收案例 O1 — debug 组件门控

- **WHEN** `debug.enabled=false`
- **THEN** 不起任何 debug server，`127.0.0.1:6060` 不可达
- **WHEN** `debug.enabled=true`（yaml 或 `APP_DEBUG=1` 任一置位）
- **THEN** `127.0.0.1:6060` 仅回环可达（外网不可达），退出序列中 debug server 先于 `logger.Close` 关闭（可经事件记录器断言）

### Requirement: k8s 探针 = TCP + consul 双层

`examples/mesh/` 清单 SHALL 将 liveness/readiness 探针写为 `tcpSocket` 打业务端口（端口存活层），MUST NOT 假设任何 HTTP 端点；语义就绪由 consul 健康检查承载（业务层）；Istio sidecar 侧补充探测并在 README 注明。

#### Scenario: 验收案例 O3 — 清单探针字段

- **WHEN** `kubectl apply --dry-run=client -f examples/mesh/ -R` 与 `bring-up.sh check`
- **THEN** 全绿；三个 Deployment 的 liveness/readiness 均为 `tcpSocket` 且端口与业务端口一致
