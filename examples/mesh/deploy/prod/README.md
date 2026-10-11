# deploy/prod —— 生产形态清单（教学集群**不 apply**）

本目录装的是「要上线时必须补、但教学集群跑不了或不必跑」的那部分。
`bring-up.sh deploy` 只 apply `shared/` 与变体目录（micro/ 或 apps/），
永远不会碰这里 —— 这些文件是给生产部署做底稿，不是给 minikube 用的。

| 文件 | 内容 | 为什么教学集群不跑 |
|---|---|---|
| `networkpolicy.yaml` | K8s 层东西向隔离：bff 只收 ingressgateway，srv 只收 bff | minikube 默认 CNI（kindnet/bridge）**不执行** NetworkPolicy，apply 了也是摆设；生产需 Calico/Cilium 等可执行 CNI |
| `podmonitor.yaml` | prometheus-operator 的 PodMonitor ×5（bff 8081 + 四域 9031-9034） | 集群没装 prometheus-operator，CRD 不存在，apply 直接报错 |
| `prometheusrule.yaml` | 告警底稿：入口 5xx、目标下线、限流触发 | 同上，依赖 prometheus-operator |
| `hpa-bff.yaml` | bff 按 CPU 的 HPA（2→4） | minikube 未开 metrics-server addon，HPA 取不到指标会停摆 |

不在本目录、但上线同样必须做的事（已就近落实或登记）：

1. **入口 TLS**：生产 Gateway 加 443 server（simple TLS，证书来自 cert-manager），
   80 改 redirect。教学集群保持明文 80 —— route-matrix 与 e2e 都按 http 断言。
2. **采样率**：`shared/40-telemetry.yaml` 的 100% 是教学防假阴性需要；生产按入口
   流量比例下调（如 1~10%），并对 bff 用 workload 级 Telemetry 单独保留高采样。
3. **Istio 形态**：demo profile 换 default，istiod 双副本 + PodAntiAffinity。
4. **中间件**：host-middleware 门面整体替换为集群内或托管实例（含鉴权与 TLS），
   配置面只需改 `20-host-middleware.yaml` 一处 Endpoints。
5. **镜像**：`:dev` 换不可变 tag（git sha），仓库侧接扫描与 SBOM。
