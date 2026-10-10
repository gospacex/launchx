#!/usr/bin/env bash
# =============================================================================
# 网格案例（E4/E5、T10–T12）的部署与校验脚本
# -----------------------------------------------------------------------------
# 子命令：
#   check    只做前置与清单校验（不需要集群能跑起来）：工具/清单语法/地址假设
#   image    交叉编译 linux 二进制 → 构建镜像 → 载入 minikube（按 VARIANT 选二进制来源）
#   install  启动 minikube（若未启动）+ 安装 Istio（含 OTel tracing 配置）
#   deploy   apply 清单（shared + 选中的变体）→ 等 rollout → 打一次请求
#   verify   取 Jaeger 证据（应用 span / sidecar span / 同一 trace）
#   status   打印 Pod / Service / sidecar 状态
#   down     删除 launchx-mesh 命名空间（保留集群与 Istio）
#
# 环境变量：
#   VARIANT=micro|apps   选部署哪套（默认 apps）
#   IMAGE=<tag>          覆盖镜像 tag（默认随 VARIANT 定：launchx-demo:dev / launchx-mesh-apps:dev）
#   PROBE_BASE / PROBE_ROUTES / TRACE_SERVICE
#                         覆盖 verify 的入口前缀、路由矩阵（路径|方法|body）与判据服务名
#                         （默认随 VARIANT 定；矩阵文件在 scripts/route-matrix-<VARIANT>.txt）
#   PYTHON=<解释器>      指定跑 trace-audit.py 的解释器（默认 python3；需能 import requests）
#   E2E_REUSE=1          复用现成进程/集群（见根 README）
#   MESH_DIR / DEPLOY_DIR / NS / JAEGER  覆盖默认路径与地址
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
DEPLOY_DIR="${DEPLOY_DIR:-${MESH_DIR}/deploy}"
NS="${NS:-launchx-mesh}"
JAEGER="${JAEGER:-http://127.0.0.1:16686}"

# 变体选择（两套证据链并存，各自独立镜像 tag，别让它们互相污染）：
#   micro = 对照组   —— 复用 examples/micro 的二进制，论证「换形态不改代码」（C4/T8）
#   apps  = 网格原生组 —— examples/mesh/apps（独立 module），论证「为网格写的应用有多瘦」
VARIANT="${VARIANT:-apps}"
case "$VARIANT" in
  micro)
    IMAGE="${IMAGE:-launchx-demo:dev}"
    # 对照组入口 = 应用侧 gateway 进程（demo-gateway Service）。
    PROBE_BASE="${PROBE_BASE:-http://demo-gateway.${NS}.svc.cluster.local:8080}"
    # 对照组保持单路径：它的入口本来就只有 1 条路由，矩阵在它身上只有一行。
    PROBE_ROUTES=('/api/order/2?user_id=1|GET|')
    TRACE_SERVICE="${TRACE_SERVICE:-demo-gateway}"
    ;;
  apps)
    IMAGE="${IMAGE:-launchx-mesh-apps:dev}"
    # 原生组入口 = **Istio Gateway**（没有应用侧 gateway 进程，见 apps/README §3）。
    # 走 istio-ingressgateway 的 80：TrafficPolicy 由 deploy/apps/gateway.yaml 声明。
    PROBE_BASE="${PROBE_BASE:-http://istio-ingressgateway.istio-system.svc.cluster.local}"
    # 路由矩阵：与 mesh-entry-chain「bff 的路由表」逐条对应。
    # 改这里 MUST 同步改 scripts/route-matrix-apps.txt（两处必须逐条一致）。
    #
    # ⚠️ `/api/product/1` 出现**两次**是刻意的，不是手滑：矩阵里有一条 `+cache`
    # 行，判据是「前一次读回源（含 db.query / repository.*）、后一次读命中缓存
    # （这两个 span 都不出现）」—— 只有打两次才拿得到对比样本。打请求循环对
    # 连续重复的路径自动插入 2s 间隔（见 cmd_verify），确保回填已完成且两条
    # trace 的时间顺序可分辨。
    PROBE_ROUTES=(
      '/api/ping|GET|'
      '/api/order/2|GET|'
      '/api/order|POST|{"user_id":1,"items":[{"product_id":1,"name":"demo","quantity":1,"unit_price":100}]}'
      '/api/points/1|GET|'
      '/api/product/1|GET|'
      '/api/product/1|GET|'
      '/api/stock/1|GET|'
    )
    TRACE_SERVICE="${TRACE_SERVICE:-bff}"
    ;;
  *) echo "ERROR: VARIANT 只能是 micro | apps，收到 '$VARIANT'" >&2; exit 2 ;;
esac

kubectl() { minikube kubectl -- "$@"; }

need() {
  # 变量名后紧跟中文必须写成 ${1}：macOS bash 3.2 不做多字节识别，
  # `$1（PATH` 会被当成变量名 `$1\xef\xbc\x88...`，静默展开成空串。
  command -v "$1" >/dev/null 2>&1 || { echo "ERROR: 缺少 ${1}（PATH=${PATH}）" >&2; exit 1; }
}

# yaml_syntax_check <dir> —— 无集群时的清单兜底校验：纯 YAML 解析，不碰 API。
# 找一个带 pyyaml 的解释器；找不到就明说跳过（**不当失败**：没有 pyyaml 是本机
# 环境问题，不是清单问题）。要更严格的 schema 校验，那是集群起来之后的事。
yaml_syntax_check() {
  local dir="$1" py=""
  for c in ${PYTHON:-} python3 python; do
    command -v "$c" >/dev/null 2>&1 || continue
    if "$c" -c 'import yaml' >/dev/null 2>&1; then py="$c"; break; fi
  done
  if [[ -z "$py" ]]; then
    echo "  ! 本机无带 pyyaml 的 python，跳过 YAML 语法解析（集群起来后 kubectl 校验仍会生效）"
    return 0
  fi
  "$py" - "$dir" <<'PY'
import sys, pathlib
try:
    import yaml
except ImportError:
    sys.exit(0)
bad, n = [], 0
for p in sorted(pathlib.Path(sys.argv[1]).rglob("*.yaml")):
    try:
        n += len([d for d in yaml.safe_load_all(p.read_text()) if d])
    except Exception as e:  # noqa: BLE001 —— 校验脚本，任何解析异常都算清单问题
        bad.append(f"  BAD {p}: {e}")
for b in bad:
    print(b, file=sys.stderr)
print(f"  {'✓' if not bad else '✗'} YAML 语法：{n} 个文档解析，{len(bad)} 个失败")
sys.exit(1 if bad else 0)
PY
}

cmd_check() {
  echo "== 前置检查 =="
  local rc=0
  for b in minikube istioctl; do
    if command -v "$b" >/dev/null 2>&1; then
      # istioctl version 默认 --remote=true，会去连 istiod；无集群时**挂起**而不是报错，
      # 让 check 卡死在这里。前置检查只要客户端版本，必须显式关掉远端查询。
      local varg=""
      [[ "$b" == "istioctl" ]] && varg="--remote=false"
      echo "  ✓ $b $(  $b version $varg 2>/dev/null | head -1 )"
    else echo "  ✗ 缺 $b" >&2; rc=1; fi
  done
  [[ -x "$DOCKER" ]] && echo "  ✓ docker CLI" || { echo "  ✗ docker CLI 不可执行：$DOCKER" >&2; rc=1; }

  echo "-- 清单 dry-run（client 侧校验）--"
  # 目标**逐个目录列举**，不用 `-R -f "$DEPLOY_DIR"`：后者会把
  # `deploy/istio/operator.yaml` 一起扫进去，而它是 `istioctl install -f` 的入参
  # （kind: IstioOperator），不是 kubectl 对象 → 集群一启动就报
  #   no matches for kind "IstioOperator" in version "install.istio.io/v1alpha1"
  # 集群没起时反而看不出来（kubectl 连不上会整体降级成 YAML 解析），
  # 于是这条**只在集群健康时**才炸的错误最容易在「刚装完集群」时误报成「清单写错了」。
  # 现在按实际会被 apply 的范围校验：shared + micro + apps（跳过 istio/）。
  local dry_args=()
  local var_dir
  for var_dir in "${DEPLOY_DIR}"/shared "${DEPLOY_DIR}"/micro "${DEPLOY_DIR}"/apps; do
    [[ -d "$var_dir" ]] && dry_args+=(-f "$var_dir")
  done
  local dryout dryrc
  dryout="$(kubectl apply --dry-run=client -R "${dry_args[@]}" 2>&1)" && dryrc=0 || dryrc=$?
  if (( dryrc == 0 )); then
    echo "  ✓ 清单语法通过（kubectl --dry-run=client）"
  elif grep -qiE "connection to the server|connection refused|no such host|Unable to connect" <<<"$dryout"; then
    # kubectl apply 即使 --dry-run=client 也要先做一次 discovery 才能把 kind 映射到
    # 资源，集群没起时这里必然连不上。但 check 的契约是「不需要集群能跑起来」——
    # 此时降级为纯 YAML 语法解析，**不判失败**：否则「minikube 没启动」会被误报成
    # 「清单写错了」，排障方向直接跑偏。
    echo "  ! 集群未就绪（kubectl 无法 discovery），降级为 YAML 语法解析"
    yaml_syntax_check "$DEPLOY_DIR" || rc=1
  else
    echo "  ✗ 清单校验失败：" >&2
    tail -5 <<<"$dryout" >&2
    rc=1
  fi

  echo "-- 变体目录存在性 --"
  if [[ -d "${DEPLOY_DIR}/${VARIANT}" ]]; then
    echo "  ✓ VARIANT=${VARIANT} 的清单目录存在"
  else
    echo "  ✗ VARIANT=${VARIANT} 的清单目录不存在：${DEPLOY_DIR}/${VARIANT}" >&2
    rc=1
  fi

  echo "-- 地址假设自检（网格形态最易错的一处）--"
  local bad=0
  grep -rn "127\.0\.0\.1" "$DEPLOY_DIR" --include="*configmap*.yaml" 2>/dev/null \
    | grep -v "host.minikube.internal" \
    | grep -v "^[^:]*:[0-9]*: *#" > /tmp/mesh-bad-addrs.txt || true
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
  export PATH="${GO_BIN_DIR:-/Users/hyx/sdk/go1.24.3/bin}:$PATH"

  if [[ "$VARIANT" == "apps" ]]; then
    # apps 是独立 module，二进制与镜像都由它自己的 Makefile 产出
    # （根包 go build ./... 覆盖不到它，这里也不能省）。
    echo "== 交叉编译 apps 五进程（独立 module）=="
    make -C "${MESH_DIR}/apps" image IMAGE="$IMAGE"
    return
  fi

  echo "== 交叉编译（宿主 $(uname -m) → linux/amd64）=="
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
    istioctl install -y -f "${DEPLOY_DIR}/istio/operator.yaml"
  fi

  echo "== 确保命名空间带 sidecar 注入标签 =="
  kubectl apply -f "${DEPLOY_DIR}/shared/00-namespace.yaml"

  echo "== 下发 Telemetry（采样率 100%）=="
  # 不 apply 本文件的后果：Istio 默认采样率极低，Jaeger 里 sidecar span 时有时无，
  # T10 表现为假阴性（README 排查表里「Jaeger 里看不到 sidecar span」那条）。
  kubectl apply -f "${DEPLOY_DIR}/shared/40-telemetry.yaml"

  echo "== install 完成 =="
  kubectl get pods -n istio-system
}

cmd_deploy() {
  echo "== apply 共用清单（shared/：命名空间 / consul / 宿主中间件门面 / Telemetry）=="
  # 只 apply shared（**不**含 deploy/istio/operator.yaml —— 那是 istioctl install 的入参，
  # 拿 kubectl apply 会触发 istiod 重配，副作用不在本脚本的可控范围内）。
  kubectl apply -R -f "${DEPLOY_DIR}/shared"

  echo "== apply 变体清单（VARIANT=${VARIANT}）=="
  kubectl apply -R -f "${DEPLOY_DIR}/${VARIANT}"

  echo "== 等待 rollout（每个 Pod 应有 2/2 容器：app + istio-proxy）=="
  kubectl -n "$NS" wait --for=condition=available --timeout=180s deployment --all
  cmd_status
}

cmd_status() {
  echo "== Pods =="
  kubectl -n "$NS" get pods -o wide
  echo "== Services =="
  kubectl -n "$NS" get svc
}

# mesh_curl <url> [method] [body] —— 从集群内发起一次请求。
#
# 为什么不用 `kubectl run --image=curlimages/curl`（原先的写法）：
# 本机到 Docker Hub 不通（`registry-1.docker.io` 连接超时），该镜像**永远拉不下来**
# → 探针 Pod 停在 ImagePullBackOff → `kubectl run --rm` 等不到就绪，
# verify 以一个和真实原因毫无关系的 "timed out waiting for the condition" 失败。
# 改用集群内**已经在跑的** istio-proxy 容器（proxyv2 自带 curl 8.5）：它就在 Pod
# 网络里，请求天然经过 outbound sidecar，判据 M1/M3 需要的跳数一条不少。
mesh_curl() {
  local url="$1" method="${2:-GET}" body="${3:-}" pod want=""
  # 方法/body 只在非 GET 时带 -X；GET 带 body 是 curl 的错误用法，
  # 而 -X GET 又会关掉 curl 对 -d 的自动方法推断，故分两路拼。
  local -a args=(-s -m 20 -w '\nHTTP=%{http_code}\n')
  if [[ "$method" != "GET" ]]; then
    args+=(-X "$method")
    [[ -n "$body" ]] && args+=(-H 'Content-Type: application/json' -d "$body")
  fi
  for pod in $(kubectl -n "$NS" get pods -o name 2>/dev/null | sed 's|pod/||'); do
    # initContainers 也要查：k8s ≥1.29 的**原生 sidecar**形态下 istio-proxy 落在
    # spec.initContainers（本机集群重建到 v1.33.1 后实测如此；v1.28 时代在 containers），
    # 只查 containers 会误判「没有注入」并回退宿主直连，全部请求 502。
    if [[ "$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.spec.containers[*].name} {.spec.initContainers[*].name}' 2>/dev/null)" == *istio-proxy* ]]; then
      want="$pod"; break
    fi
  done
  if [[ -z "$want" ]]; then
    echo "  ! $NS 下没有带 istio-proxy 的 Pod（未注入？）——回退到宿主直连" >&2
    curl "${args[@]}" "$url"
    return
  fi
  echo "  (探针：$want / istio-proxy)"
  kubectl -n "$NS" exec "$want" -c istio-proxy -- \
    curl "${args[@]}" "$url"
}

cmd_verify() {
  echo "== 逐路由打请求（${VARIANT} 组，共 ${#PROBE_ROUTES[@]} 条）=="
  local line path method body prev_path=""
  for line in "${PROBE_ROUTES[@]}"; do
    IFS='|' read -r path method body <<<"$line"
    # 与上一条同路径 = 缓存命中判据的「第二次读」：间隔 2s，让第一次的回填
    # （service 层 fill 是同步的，这里只是留足余量）先落定，同时让两条 trace
    # 的 startTime 在 Jaeger 里可分辨（+cache 行按时间取最后两条做对比）。
    if [[ "$path" == "$prev_path" ]]; then
      echo "-- ${method} ${PROBE_BASE}${path}  （重复：第二次读，判缓存命中）"
      sleep 2
    else
      echo "-- ${method} ${PROBE_BASE}${path}"
    fi
    mesh_curl "${PROBE_BASE}${path}" "$method" "$body"
    prev_path="$path"
  done
  echo
  # 比初稿的 12s 多 3s：POST /order 的消费 span 是**异步**落盘的
  # （order → kafka → points 消费），flush 完之前 Jaeger 里查不到，
  # 矩阵会报「窗口内没有该路由的 trace」这种与真实原因无关的 FAIL。
  echo "== 等 Jaeger flush（batch 约 5s）+ 等异步链消费完成 =="
  sleep 15
  echo "== Jaeger services（应出现应用服务与 <workload>.${NS} 两类）=="
  curl -s --noproxy '*' "${JAEGER}/api/services"
  echo
  echo
  echo "== T13 判据：路由矩阵（入口 → bff → 目标域业务层 + sidecar）=="
  # 矩阵判据（--route-matrix）比「有 sidecar span 即 PASS」严一档：逐路由断言
  # 该 trace 内出现**目标域**的 service.<域>.* span —— 挡住「六条路由全接到
  # order 域」这类接线错误。判据的来龙去脉见 docs/verify.md §T13。
  # 矩阵文件按变体落盘，与上面的 PROBE_ROUTES 逐条对应。
  "${PYTHON:-python3}" "${LAUNCHX_ROOT}/examples/scripts/trace-audit.py" \
    --base "$JAEGER" --service "$TRACE_SERVICE" --lookback 3m --mesh \
    --route-matrix "${MESH_DIR}/scripts/route-matrix-${VARIANT}.txt" || true
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
  *) echo "用法: VARIANT={micro|apps} $0 {check|image|install|deploy|status|verify|down}" >&2; exit 2 ;;
esac
