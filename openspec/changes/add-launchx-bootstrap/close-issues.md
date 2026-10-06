# close-issues — 后置项（本轮不验收，回补开新 change）

本文件是 sddflow close 阶段消费的登记文档，**不是实现文件**。下列条目在本轮
`add-launchx-bootstrap` 内**占位不实现**，勾选本文件条目不代表对应 tasks.md /
plan-ready.md 条目已完成 —— 那两处保持未勾选，由开新 change 时回补。

## 一、原计划的既定后置项

- [ ] **1.8 debug 伪组件（O1/O2）**：spec 依据 `specs/observability/spec.md`
      （Requirement「Pprof 走 debug 伪组件」+ Scenario O1/O2）；
      插入点：`internal/component/debug.go`（builder + server + Stop，
      `127.0.0.1:6060` 仅回环，端口冲突 Warn 带病运行）、`internal/pipeline`
      退出链（先于 `logger.close`）、`App.Debug` 句柄字段
- [ ] **2.5 examples/mesh 清单（C4 部署部分 / E4 清单基础）**：自写 k8s + Istio
      最小清单（namespace / 3 Deployment + Service / sidecar 注入 /
      `bring-up.sh` + `check` 前置检查），**MUST NOT copy `service_mesh/`**
      （该清单未交付）；设计结论参考（业务零改动 / 同 trace /
      地址段转换 `*.mesh-external`），README 注明来源
- [ ] **3.8 E4/E5 网格两级验收**：(a) `kubectl apply --dry-run=client -f examples/mesh/ -R`
      + `bring-up.sh check` 全绿；(b) minikube + Istio 真实部署、应用 span 与
      sidecar span 同 trace（E4）、`hubx.trace` OFF/ON 对比（E5），
      证据 `examples/mesh/docs/verify.md`，close 不硬阻塞
- [ ] **3.11 debug 门控单测（O1）**：false 不起 server / true 仅回环可达 /
      退出先于 `logger.close`（事件记录器断言）
- [ ] **3.12 `APP_DEBUG=1` srv pprof 可达（O2）**：`curl 127.0.0.1:6060/debug/pprof/`
      索引页；yaml 置位等效
- [ ] **3.13 mesh 三 Deployment tcpSocket 探针（O3）**：liveness/readiness 为
      tcpSocket 打业务端口（无 HTTP 端点假设，语义就绪由 consul 承载；
      Istio sidecar 补充探测），dry-run + bring-up check 全绿
- [ ] **3.10 后置案例对齐核对**（C4 部署部分 / E4–E5 / O1–O3）
      （A1–A3、B1、C1–C3 纯代码部分、D1–D2、E1–E3 已随本轮 Task 12–19 核对）
- [ ] **B1 真机 nacos 分支**：当前 `t.Skip` 形态，nacos 底座就绪后解除
      （落点 `internal/configsrc/nacos_test.go:TestNacosRealServer`）
- [ ] **examples/mesh/README.md**（随 2.5）

## 二、本轮执行中新发现的偏差（3.14 依赖面核对）

Task 24 实测三条判据，两条与现状冲突，**判据口径过时而非代码违规**：

- [ ] **3.14-a `go.mod` 中 kitex 仅 `// indirect` —— 当前不满足，结构上无法满足**
      `examples/micro/pkg/kitex_gen/**` 直引 `github.com/cloudwego/kitex`，
      而 examples 与库**同属一个 module**，Go 因此把 kitex 记为 direct。
      要满足该判据须把 `examples/**` 拆成独立 module（会改变 `go build ./...`
      的一次性构建语义与本地 replace 开发方式），超出 3.14「纯核对」范围。
      **现状**：库源码（`internal/**` + 根包）零 cloudwego import —— 这条才是
      该判据真正想守的东西，已由 `dependency_surface_test.go` 固化。

- [ ] **3.14-b 「全仓无 `cloudwego/hertz` / `gorm.io/gorm` 直引」—— 口径过时**
      实测 hertz 11 处、gorm 24 处命中，**全部落在 `examples/**`**：
      demo 的 handler 签名必须拿 hertz 的 `*app.RequestContext`，
      repository 必须用 gorm，这是 demo 应用的角色决定的。
      该判据写于 2026-10-05，当时 demo 结构不同。
      **已收窄为**：库源码（含 `internal/**`）零直引 —— 已测试固化；
      examples 侧保持开放。

- [x] **3.14-c 库源码零 `cloudwego` import —— 已通过并固化**
      守卫测试 `dependency_surface_test.go:TestLibrarySourceHasNoDirectMiddlewareImports`
      扫 37 个库源码 `.go`，禁用前缀：cloudwego/\*、gorm.io/gorm、
      redis/go-redis、segmentio/kafka-go、hashicorp/consul/api。
      已做变异验证：注入 `import _ "gorm.io/gorm"` 即 FAIL 并点名文件行号。

## 三、与本轮其它决策的交叉影响

- [ ] **`examples/micro/srv` 已删除**（网格清单部署的 demo-srv，拆分前单域旧样例，
      含 3 个单测）。受影响的既有条目：tasks.md 3.7（E3 回归）、4.9–4.11
      （网格 T10–T12）中「srv」一词现指四域服之一，回补时须重新选定目标服务；
      `examples/mesh/**` 清单里的 srv 段指向的是已不存在的 `demo-srv`，
      **网格若要继续可跑，须先把该段改指四域服之一**。
- [ ] **注册实例 ID 撞名连坐**：`stepRegister` 用固定 ID `<服务名>-<host>-<port>`，
      同机同端口的旧进程退出时会用同一 ID 把新实例条目一起摘掉
      （现象：进程在监听、consul 里 `[]`、日志却打过 `service registered`）。
      coordx 的 TTL 心跳机制本身无问题。根治需给实例 ID 加唯一后缀
      （多实例滚动发布下必现）。
