#!/usr/bin/env bash
# ============================================================
# 边界鉴权的**反向探针**（gateway → bff 这一段）
#
# 为什么单列一个脚本：e2e.sh 只验「带对凭据能通」，那是**正向**判据 ——
# 一个把鉴权装上去却始终放行的实现照样能过（中间件没挂上、或 Engine.Use
# 晚于路由注册，表现都是「谁都能访问」）。鉴权这种东西**正向通过不算数**，
# 必须有一条断言「没凭据会被挡」，否则上线就是裸奔而门禁还是绿的。
#
# 与 e2e.sh 的分工：e2e.sh 打全链（六进程 + 指标），本脚本只起最小三进程
# （srv-order + bff + gateway），专测边界那一段的四种入参。
#
# 用法：
#   bash examples/micro/scripts/auth-probe.sh
#
# 退出码：0 = 四条断言全过；非 0 = 至少一条没挡住 / 挡错了。
#
# ⚠️ 与 e2e.sh 同一个本机冲突：bff 的 8081 被 dockers/docker-compose.yml 里的
#   `etcdkeeper`（容器 8080 → 宿主 8081）占着。本脚本会临时 stop 它，
#   并在退出时**无论成败都 start 回来**（trap 里做，不靠正常流程走到最后一行）。
#
# 踩坑记录（macOS bash 3.2）：**变量后面紧跟中文必须写成 ${name}** ——
# 同 e2e.sh 的约定，ci.sh 有一步静态检查专门扫这个。
# ============================================================
set -uo pipefail

MICRO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

GO_BIN="${GO_BIN:-$HOME/sdk/go1.24.3/bin/go}"
DOCKER_BIN="${DOCKER_BIN:-/Applications/Docker.app/Contents/Resources/bin/docker}"
GATEWAY="${GATEWAY:-http://127.0.0.1:8080}"
DEMO_TOKEN="${DEMO_TOKEN-demo-token}"
SEED_ORDER_ID="${SEED_ORDER_ID:-1001}"
USER_ID="${USER_ID:-1001}"

WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/auth-probe.XXXXXX")"
readonly WORKDIR
mkdir -p "$WORKDIR/log" "$WORKDIR/bin"

STARTED=()
ETCDKEEPER_STOPPED=0

log()  { printf '\033[36m[auth]\033[0m %s\n' "$*"; }
fail() { printf '\033[31m[auth] FAIL\033[0m %s\n' "$*" >&2; }

cleanup() {
  local rc=$?
  trap - EXIT
  for pid in "${STARTED[@]:-}"; do
    [ -n "$pid" ] && kill "$pid" 2>/dev/null
  done
  wait 2>/dev/null || true
  # 借来的端口必须还回去：放在 trap 里，不依赖脚本能正常走到最后一行。
  if [ "$ETCDKEEPER_STOPPED" = "1" ]; then
    "${DOCKER_BIN}" start etcdkeeper >/dev/null 2>&1 \
      && log "已恢复 etcdkeeper" \
      || fail "etcdkeeper 没能 start 回来，请手动 docker start etcdkeeper"
  fi
  if [ "${rc}" -ne 0 ]; then
    fail "退出码 ${rc}；进程日志在 ${WORKDIR}/log/"
    for f in "$WORKDIR"/log/*.log; do
      [ -f "$f" ] || continue
      printf -- '--- %s ---\n' "$f"
      tail -n 15 "$f" || true
    done
  else
    rm -rf "$WORKDIR"
  fi
  exit "${rc}"
}
trap cleanup EXIT

port_open() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
wait_port() {
  local port=$1 timeout=${2:-40} i
  for ((i = 0; i < timeout; i++)); do
    port_open "$port" && return 0
    sleep 1
  done
  return 1
}

# 腾 8081：占用者是 Docker Desktop 的端口转发（etcdkeeper），`ps` 里看不到。
if port_open 8081; then
  log "8081 已被占用 —— 临时停掉 etcdkeeper（退出时自动恢复）"
  if "${DOCKER_BIN}" stop etcdkeeper >/dev/null 2>&1; then
    ETCDKEEPER_STOPPED=1
    sleep 1
  else
    fail "8081 被占且 etcdkeeper 停不掉；请先排查占用者"
    exit 1
  fi
fi

export HUBX_DB_PW="${HUBX_DB_PW:-hubx123}"
export CGO_ENABLED=0

# 最小三进程：只测 gateway → bff 这一跳的边界，不需要四个域服全起。
for spec in "srv-order:9022" "bff:8081" "gateway:8080"; do
  dir=${spec%%:*}; port=${spec##*:}
  log "编译并启动 ${dir}（等待 ${port}）"
  ( cd "$MICRO_DIR/$dir" && "$GO_BIN" build -o "$WORKDIR/bin/$dir" ./cmd ) \
    >"$WORKDIR/log/build-$dir.log" 2>&1 \
    || { fail "$dir 编译失败"; tail -n 20 "$WORKDIR/log/build-$dir.log" >&2; exit 1; }
  ( cd "$MICRO_DIR/$dir" && exec "$WORKDIR/bin/$dir" ) >"$WORKDIR/log/$dir.log" 2>&1 &
  STARTED+=($!)
  if ! wait_port "$port" 40; then
    fail "$dir 在 40s 内没有监听 $port"
    tail -n 20 "$WORKDIR/log/$dir.log" >&2
    exit 1
  fi
done
# gateway 靠 consul 发现 bff，注册有延迟；等链路就绪而不是等端口。
sleep 3

# 只取 HTTP 码：鉴权判据是「挡没挡住」，响应体只用于失败时给人看。
status_of() {
  curl -s --noproxy '*' -m 10 -o "$WORKDIR/body.txt" -w '%{http_code}' "$@"
}

ORDER_URL="$GATEWAY/api/order/$SEED_ORDER_ID?user_id=$USER_ID"
rc=0

log "断言 ①：无 Authorization 头 —— 必须在边界被挡（401）"
c1=$(status_of "$ORDER_URL")
printf '  HTTP %s  %s\n' "$c1" "$(head -c 120 "$WORKDIR/body.txt")"
[ "$c1" = "401" ] || { fail "无凭据应 401，实际 ${c1}（鉴权没生效 = 裸奔）"; rc=1; }

log "断言 ②：错令牌 —— 必须被挡（401）"
c2=$(status_of -H "Authorization: Bearer wrong-token-on-purpose" "$ORDER_URL")
printf '  HTTP %s  %s\n' "$c2" "$(head -c 120 "$WORKDIR/body.txt")"
[ "$c2" = "401" ] || { fail "错令牌应 401，实际 ${c2}"; rc=1; }

log "断言 ③：正确令牌 —— 必须放行（200）"
c3=$(status_of -H "Authorization: Bearer $DEMO_TOKEN" "$ORDER_URL")
printf '  HTTP %s  %s\n' "$c3" "$(head -c 120 "$WORKDIR/body.txt")"
[ "$c3" = "200" ] || { fail "正确令牌应 200，实际 ${c3}（鉴权挡过头了）"; rc=1; }

# ④ 是刻意的例外：探针路径要凭据的话，凭据存储抖一下就会让 K8s 把健康的
#    Pod 重启掉 —— 那是把「存储抖动」放大成「实例重启」，必须免鉴权。
log "断言 ④：/api/ping 在 authSkipPaths 里 —— 必须免鉴权（200）"
c4=$(status_of "$GATEWAY/api/ping")
printf '  HTTP %s  %s\n' "$c4" "$(head -c 120 "$WORKDIR/body.txt")"
[ "$c4" = "200" ] || { fail "探针路径应免鉴权 200，实际 ${c4}"; rc=1; }

printf '\n'
if [ "$rc" -ne 0 ]; then
  fail "鉴权反向探针 FAIL"
  exit 1
fi
log "鉴权反向探针 PASS：无凭据/错令牌被挡，正确令牌放行，探针路径免鉴权"
