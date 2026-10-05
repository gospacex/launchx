#!/usr/bin/env bash
# =============================================================================
# 网格案例（E4/E5、T10–T12）的部署与校验脚本
# -----------------------------------------------------------------------------
# 子命令：
#   check    只做前置与清单校验（不需要集群能跑起来）：工具/清单语法/地址假设
#   image    交叉编译 linux 二进制 → 构建镜像 → 载入 minikube
#   install  启动 minikube（若未启动）+ 安装 Istio（含 OTel tracing 配置）
#   deploy   apply 清单 → 等 rollout → 打一次请求
#   verify   取 Jaeger 证据（应用 span / sidecar span / 同一 trace）
#   status   打印 Pod / Service / sidecar 状态
#   down     删除 launchx-mesh 命名空间（保留集群与 Istio）
#
# 本机实测约束（别照通用文档抄）：
#   * PATH 里没有 /usr/local/bin，minikube/istioctl 都在那儿 → 脚本自己 export
#   * kubectl 未单独安装，一律走 `minikube kubectl --`
#   * docker CLI 不在 PATH，位于 /Applications/Docker.app/Contents/Resources/bin
#   * 宿主总内存 8GB → minikube 必须限 --cpus=2 --memory=4096，
#     且要先停掉与 launchx 无关的中间件，否则 Pod 反复 OOM/Evicted
# =============================================================================
set -euo pipefail

export PATH=/usr/local/bin:$PATH

DOCKER="${DOCKER_BIN:-/Applications/Docker.app/Contents/Resources/bin/docker}"
REPO_ROOT="${REPO_ROOT:-/Users/hyx/work/gowork/src/hubx}"
LAUNCHX_ROOT="${LAUNCHX_ROOT:-${REPO_ROOT}/launchx}"
MESH_DIR="${MESH_DIR:-${LAUNCHX_ROOT}/examples/mesh}"
IMAGE="${IMAGE:-launchx-demo:dev}"
NS="${NS:-launchx-mesh}"
JAEGER="${JAEGER:-http://127.0.0.1:16686}"

kubectl() { minikube kubectl -- "$@"; }

need() {
  command -v "$1" >/dev/null 2>&1 || { echo "ERROR: 缺少 $1（PATH=$PATH）" >&2; exit 1; }
}

cmd_check() {
  echo "== 前置检查 =="
  local rc=0
  for b in minikube istioctl; do
    if command -v "$b" >/dev/null 2>&1; then echo "  ✓ $b $(  $b version 2>/dev/null | head -1 )"
    else echo "  ✗ 缺 $b" >&2; rc=1; fi
  done
  [[ -x "$DOCKER" ]] && echo "  ✓ docker CLI" || { echo "  ✗ docker CLI 不可执行：$DOCKER" >&2; rc=1; }

  echo "-- 清单 dry-run（client 侧校验，不需要集群）--"
  if kubectl apply --dry-run=client -f "$MESH_DIR" -R >/dev/null 2>&1; then
    echo "  ✓ 清单语法通过"
  else
    echo "  ✗ 清单校验失败：" >&2
    kubectl apply --dry-run=client -f "$MESH_DIR" -R 2>&1 | tail -5 >&2
    rc=1
  fi

  echo "-- 地址假设自检（网格形态最易错的一处）--"
  local bad=0
  grep -n "127\.0\.0\.1" "$MESH_DIR"/configmap.yaml | grep -v "host.minikube.internal" \
    | grep -v "^[0-9]*: *#" > /tmp/mesh-bad-addrs.txt || true
  if [[ -s /tmp/mesh-bad-addrs.txt ]]; then
    echo "  ✗ configmap 里仍有裸 127.0.0.1（容器内不可达）：" >&2
    cat /tmp/mesh-bad-addrs.txt >&2
    bad=1; rc=1
  else
    echo "  ✓ 地址全部换算为 host.minikube.internal / 集群内 Service"
  fi
  (( rc == 0 )) && echo "== check 通过 ==" || echo "== check 失败 ==" >&2
  return "$rc"
}

cmd_image() {
  need minikube
  echo "== 交叉编译（宿主 $(uname -m) → linux/amd64）=="
  export PATH=/Users/hyx/sdk/go1.24.3/bin:$PATH
  mkdir -p "${LAUNCHX_ROOT}/examples/docker/bin"
  local svc
  for svc in gateway bff srv; do
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
      go build -C "$LAUNCHX_ROOT" -o "examples/docker/bin/demo-${svc}" "./examples/micro/${svc}/cmd"
    echo "  ✓ demo-${svc}"
  done

  echo "== 构建镜像（上下文 = examples/docker，内含刚编译的 bin/）=="
  # 直接 build 进 minikube 的 daemon（docker driver 下可用 `minikube docker-env`），
  # 比「宿主 build + minikube image load」快一个量级：省掉导出/导入 230MB。
  # 必须写在子 shell 里，别污染当前会话的 DOCKER_HOST。
  (
    eval "$(minikube docker-env)"
    docker build -f "${LAUNCHX_ROOT}/examples/docker/Dockerfile" -t "$IMAGE" \
      "${LAUNCHX_ROOT}/examples/docker"
    echo "-- consul 镜像（集群内健康检查源，必须集群内可达）--"
    docker pull swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/hashicorp/consul:1.22.4 || true
  )
  minikube image ls | grep -E "launchx-demo|consul" || true
  echo "== image 完成 =="
}

cmd_install() {
  need minikube
  need istioctl
  if ! minikube status >/dev/null 2>&1; then
    echo "== 启动 minikube（8GB 宿主：必须限 2C4G）=="
    minikube start --driver=docker --cpus=2 --memory=4096
  else
    echo "  = minikube 已在运行"
  fi
  minikube update-context >/dev/null 2>&1 || true   # 否则 kubeconfig 端口可能 stale

  if kubectl get ns istio-system >/dev/null 2>&1; then
    echo "  = istio-system 已存在，跳过安装"
  else
    echo "== 安装 Istio（tracing provider = opentelemetry → W3C traceparent）=="
    # 关键：provider 必须是 opentelemetry。选 zipkin 会走 B3 头（x-b3-traceid），
    # 与 launchx 的 W3C Composite{TraceContext,Baggage} 不互通 → sidecar span
    # 和应用 span 分成两条 trace，T10 必然失败。
    #
    # 用 operator 文件而非 --set：`defaultConfig.tracing` 在 1.30 是**数组**，
    # 再写 `--set meshConfig.defaultConfig.tracing.sampling=100`（map 语义）
    # 会让 istioctl 在 Map.SetPath 里直接 panic（实测）。
    istioctl install -y -f "${MESH_DIR}/istio-operator.yaml"
  fi

  echo "== 确保命名空间带 sidecar 注入标签 =="
  kubectl apply -f "${MESH_DIR}/namespace.yaml"
  echo "== install 完成 =="
  kubectl get pods -n istio-system
}

cmd_deploy() {
  echo "== apply 清单 =="
  kubectl apply -f "${MESH_DIR}/namespace.yaml"
  kubectl apply -f "${MESH_DIR}/configmap.yaml"
  kubectl apply -f "${MESH_DIR}/consul.yaml"
  kubectl apply -f "${MESH_DIR}/srv.yaml"
  kubectl apply -f "${MESH_DIR}/bff.yaml"
  kubectl apply -f "${MESH_DIR}/gateway.yaml"

  echo "== 等待 rollout（每个 Pod 应有 2/2 容器：app + istio-proxy）=="
  kubectl -n "$NS" rollout status deploy/consul --timeout=180s
  kubectl -n "$NS" rollout status deploy/demo-srv --timeout=180s
  kubectl -n "$NS" rollout status deploy/demo-bff --timeout=180s
  kubectl -n "$NS" rollout status deploy/demo-gateway --timeout=180s
  cmd_status
}

cmd_status() {
  echo "== Pods =="
  kubectl -n "$NS" get pods -o wide
  echo "== Services =="
  kubectl -n "$NS" get svc
}

cmd_verify() {
  echo "== 从集群内发起一次全链路请求（gateway → bff → srv）=="
  kubectl -n "$NS" run mesh-probe --rm -i --restart=Never --image=curlimages/curl:8.10.1 \
    --command -- curl -s -w '\nHTTP=%{http_code}\n' \
    "http://demo-gateway.${NS}.svc.cluster.local:8080/api/order/2?user_id=1" || true
  echo
  echo "== 等 Jaeger flush（batch 约 5s）=="
  sleep 12
  echo "== Jaeger services（应出现 demo-* 与 *.${NS} 两类）=="
  curl -s --noproxy '*' "${JAEGER}/api/services"
  echo
  echo
  echo "== T10 判据：同一 trace 内同时有应用 span 与 sidecar span =="
  # --mesh 追加 M1/M2/M3：sidecar span 存在、带 component=proxy+upstream_cluster、
  # 且覆盖 >= 2 跳。判据的来龙去脉见 docs/verify.md §1、§4。
  # lookback 必须与上面这次请求的时间窗匹配（Jaeger 的 ?lookback= 参数实测无效，
  # 脚本已改为自算 start/end，别再往回传 lookback）。
  python3 "${LAUNCHX_ROOT}/examples/scripts/trace-audit.py" \
    --base "$JAEGER" --service demo-gateway --lookback 2m --mesh || true
}

cmd_down() {
  kubectl delete ns "$NS" --ignore-not-found
  echo "== 命名空间已删除（集群与 Istio 保留）=="
}

case "${1:-check}" in
  check)   cmd_check ;;
  image)   cmd_image ;;
  install) cmd_install ;;
  deploy)  cmd_deploy ;;
  status)  cmd_status ;;
  verify)  cmd_verify ;;
  down)    cmd_down ;;
  *) echo "用法: $0 {check|image|install|deploy|status|verify|down}" >&2; exit 2 ;;
esac
