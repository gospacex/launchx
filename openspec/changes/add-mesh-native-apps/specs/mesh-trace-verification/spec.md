# mesh-trace-verification delta

## ADDED Requirements

### Requirement: T13 —— apps 组的网格入链判据

验证 SHALL 断言：apps 组端到端请求产生的**同一 trace** 内同时存在应用 span 与
sidecar span，且 sidecar span 真是 Envoy 产出的。

判据 MUST 复用 `examples/scripts/trace-audit.py --mesh` 的三条既有规则，不得另写一套：

- **M1** 同一 trace 内同时存在应用 span 与 sidecar span（服务名以 `.<ns>` 结尾）；
- **M2** sidecar span 带 tag `component=proxy` **且** 有 `upstream_cluster`
  （只按服务名后缀判不可靠，同名服务能伪造）；
- **M3** sidecar 至少覆盖 2 跳（inbound + outbound）。

T13 的断言范围 SHALL 是**路由矩阵**（见 `mesh-entry-chain`），MUST NOT 以「任意一条
业务 trace 含 sidecar span」为通过条件：初稿的「sidecar 入链比例 ≥ 1 条」只证明
**存在**一条入链路径，证明不了入口的每条路由都通到了对应域的业务逻辑。

#### Scenario: apps 组 T13 通过

- **WHEN** `VARIANT=apps bring-up.sh verify`（入口经 Istio Gateway 打到 bff）
- **THEN** 输出 `结论：PASS`，且路由矩阵**每一行**同时满足：
  入口 2xx、目标域 `service.<域>.*` span 在同一 trace、该 trace 含 sidecar span；
  判据覆盖的跳包含 `inbound|…|` 与 `outbound|…|`

#### Scenario: 判据不得放宽以凑通过

- **WHEN** 若 apps 组的 sidecar span 数为 0，或矩阵里有任一行缺目标域 service span
- **THEN** verify MUST 报 FAIL 并非零退出；MUST NOT 通过去掉 M2/M3、
  或把矩阵降级成「任一行为真即 PASS」的方式让结论变绿

### Requirement: bff→srv 这一跳永远没有 sidecar span（显式记录）

验证报告 MUST 显式记录：`bff→srv` 的 RPC 跳走 Kitex 默认 TTHeader/TCP，
Istio 对该端口按 `tcp_proxy` 处理 —— Envoy 只看得见连接、看不见请求，
因此该跳**不会有 sidecar span**，其熔断/重试/超时也交不出去。

这条 MUST 作为「协议事实」而非「配置漏项」写入 `apps/README.md` 与
`docs/verify.md`，使 T13 的断言范围（只认 `ingress→bff`）有据可依。

#### Scenario: 报告不得声称「治理已全部下沉」

- **WHEN** 阅读 `docs/verify.md` 的结论段
- **THEN** 准确表述为「网格本轮提供 mTLS + 连接级可观测 + HTTP 跳的 L7 治理」；
  RPC 跳与企业级治理（业务限流、幂等、事务、配置分发）明确标注仍在应用侧

### Requirement: 证据必须落盘且可复跑

`examples/mesh/docs/verify.md` SHALL 承载 T10（micro 对照组）与 T13（apps 原生组）
两组证据，每组 MUST 含：

- 验收时间与版本口径（minikube / kubectl / istioctl / Jaeger / 宿主资源限制）；
- 判据的可执行命令原文（`trace-audit.py … --mesh`）；
- 实测原始输出（含 trace-id、span 总数、sidecar 跳列表、服务名清单）；
- 结论行（PASS/FAIL）与 FAIL 时的原因分析。

#### Scenario: 证据可被第三方复跑

- **WHEN** 另一人按 `verify.md` 里的命令原文在本机执行
- **THEN** 得到同量级的结论（span 数与跳列表可能不同，但 PASS/FAIL 一致）
