#!/usr/bin/env bash
# ============================================================
# E2 · 微服调用链集成验收（gateway → bff → 四域服）
#
# 为什么不用 go test：这条验收的判据是「六个进程真的在同一条链上跑通」，
# 进程起停、consul 注册就绪、端口冲突处置都是 shell 的活。把它包进
# go test 只会让失败栈多一层壳，还拿不到各进程的 stdout/stderr 现场。
#
# 与 scripts/chain.sh 的分工：
#   chain.sh  = 打全 8 条路径，取证用（人看输出、看 Jaeger）
#   e2e.sh    = 自起自停 + 两条断言，机器判通过（退出码即结论）
#
# 用法：
#   bash examples/micro/scripts/e2e.sh            # 自起六进程（端口被占则报错退出）
#   E2E_REUSE=1 bash examples/micro/scripts/e2e.sh  # 复用已在跑的进程，只做断言
#
# 退出码：0 = 全部断言通过；非 0 = 前置 / 断言 / 进程提前退出。
#
# ⚠️ 已知本机冲突（踩过一次，报错指不到点子上）：bff 的 webx 口是 **8081**，
#   而 dockers/docker-compose.yml 里的 `etcdkeeper` 把宿主 8081 映射给了容器 8080。
#   两者同时存在时本脚本会报「bff 的端口 8081 已被占用」，但 `ps` 里找不到任何
#   自己的进程 —— 占用者是 **Docker Desktop 自身**（`lsof -nP -iTCP:8081 -sTCP:LISTEN`
#   显示 COMMAND=com.docke）。处置：`docker stop etcdkeeper`（跑完再 start 回来）。
#
# 踩坑记录（macOS bash 3.2）：**变量后面紧跟中文必须写成 ${name}**。
# bash 3.2 不做多字节识别：若写成「变量名紧跟中文且没加花括号」（ci.sh 的
# "shell 变量紧邻非 ASCII 检查" 那一步专门拦这种写法），变量名会被并进后面那些
# 字节里 —— `set -u` 下报 "unbound variable" 而名字带乱码，不带 -u 则静默展开成
# 空串，两种情况都不指向真正的原因。本脚本里所有紧跟非 ASCII 的引用一律用花括号。
# ============================================================
set -euo pipefail

MICRO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_ROOT="$(cd "$MICRO_DIR/../.." && pwd)"

GO_BIN="${GO_BIN:-$HOME/sdk/go1.24.3/bin/go}"
DOCKER_BIN="${DOCKER_BIN:-/Applications/Docker.app/Contents/Resources/bin/docker}"
COMPOSE_FILE="${COMPOSE_FILE:-/Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml}"
SCHEMA_SQL="$REPO_ROOT/examples/mall/scripts/schema.sql"
# micro 自有的 DDL 补丁：共用 schema.sql 是 mall 案例的资产，micro 侧的约束
#（库存预占流水唯一键）只加在自己这份里，见该文件头注释。
SCHEMA_MICRO_SQL="$MICRO_DIR/scripts/schema-micro.sql"

GATEWAY="${GATEWAY:-http://127.0.0.1:8080}"
USER_ID="${USER_ID:-1001}"
SEED_ORDER_ID="${SEED_ORDER_ID:-1001}"
# 指标采集口。bff 是 HTTP 档，/metrics 挂在自己的 webx 口（8081）上；
# srv-order 是 srv 档（没有 HTTP 面），采集口是配置里单开的 metrics.addr。
BFF_METRICS="${BFF_METRICS:-http://127.0.0.1:8081/metrics}"
SRV_ORDER_METRICS="${SRV_ORDER_METRICS:-http://127.0.0.1:9031/metrics}"
# 边界鉴权凭据：bff 是唯一验凭据的地方，gateway 只透传 Authorization 头。
# 令牌由 bff 在启动时按 demo.auth_token 播种进缓存，故必须与那份配置一致
#（与 scripts/chain.sh 同一口径）。bff 的 auth.require=true 时缺这个头必然 401。
DEMO_TOKEN="${DEMO_TOKEN-demo-token}"
AUTH=()
if [ -n "$DEMO_TOKEN" ]; then
  AUTH=(-H "Authorization: Bearer $DEMO_TOKEN")
fi

WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/micro-e2e.XXXXXX")"
readonly WORKDIR

STARTED=()          # 由本脚本拉起的 PID（清理只杀这些，绝不动别人的进程）

log()  { printf '\033[36m[e2e]\033[0m %s\n' "$*"; }
fail() { printf '\033[31m[e2e] FAIL\033[0m %s\n' "$*" >&2; }

cleanup() {
  local rc=$?
  trap - EXIT
  if [ ${#STARTED[@]} -gt 0 ]; then
    log "清理 ${#STARTED[@]} 个本脚本拉起的进程"
    for pid in "${STARTED[@]}"; do kill "$pid" 2>/dev/null || true; done
    wait "${STARTED[@]}" 2>/dev/null || true
  fi
  if [ "${rc}" -ne 0 ]; then
    fail "退出码 ${rc}；进程日志在 $WORKDIR/log/"
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

# port_open —— 用 bash 的 /dev/tcp，不依赖 nc / lsof（nc 在部分镜像里没有，
# lsof 在受限沙箱里不可用）。
port_open() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }

wait_port() {
  local port=$1 timeout=${2:-30} i
  for ((i = 0; i < timeout; i++)); do
    if port_open "$port"; then return 0; fi
    sleep 1
  done
  return 1
}

http_ok() { curl -sf --noproxy '*' -m 10 "$1" >/dev/null 2>&1; }

# ------------------------------------------------------------
# 步骤 0：底座前置（consul / jaeger / mysql / redis / kafka）
# ------------------------------------------------------------
require_base() {
  local -a pairs=("consul:8500" "jaeger:16686" "jaeger-otlp:4318" "mysql:3306" "redis:6379" "kafka:9092")
  local missing=() name port
  for pair in "${pairs[@]}"; do
    name=${pair%%:*}; port=${pair##*:}
    port_open "$port" || missing+=("$name($port)")
  done
  if [ ${#missing[@]} -eq 0 ]; then
    log "底座就绪：consul 8500 / jaeger 16686+4318 / mysql 3306 / redis 6379 / kafka 9092"
    return 0
  fi
  log "缺失底座：${missing[*]} —— 尝试拉起（幂等）"
  if [ ! -x "${DOCKER_BIN}" ]; then
    fail "缺少底座且 docker 不可用（${DOCKER_BIN}）"
    return 1
  fi
  # shellcheck disable=SC2086
  "${DOCKER_BIN}" compose -f "$COMPOSE_FILE" up -d consul kafka jaeger mysql redis >&2
  for pair in "${pairs[@]}"; do
    port=${pair##*:}; name=${pair%%:*}
    if wait_port "$port" 60; then
      log "  $name($port) 已就绪"
    else
      fail "  $name($port) 超时未就绪"
      return 1
    fi
  done
}

# ------------------------------------------------------------
# 步骤 1：种子数据（四库 DDL + outbox 同构表 + id=1001 的订单）
#
# 为什么断言要依赖一条真实订单行：GET /order/:id 走的是
# order ⋈ order_items 的双表查询 + 缓存回填，比 /ping 多触达三层
# （bff → order 域 → mysql）。只断言 /ping 等于只验了 bff 活着。
# ------------------------------------------------------------
# 口令一律从环境取、缺省即报错，不写 `-pxxx` 明文、也不给弱口令默认值：
# 脚本进了仓库，写死在里面的口令就等于公开，而"连不上库"必须显式失败 ——
# 悄悄用默认口令连上，只会让真正去改口令的人发现改了也没生效。
mysql_exec() {
  local pw="${MYSQL_ROOT_PW:?请设置 MYSQL_ROOT_PW（mysql root 口令）}"
  if command -v mysql >/dev/null 2>&1; then
    MYSQL_PWD="$pw" mysql -h127.0.0.1 -P3306 -uroot "$@"
  elif [ -x "${DOCKER_BIN}" ]; then
    MYSQL_PWD="$pw" "${DOCKER_BIN}" exec -i -e MYSQL_PWD mysql mysql -uroot "$@"
  else
    fail "没有 mysql 客户端也没有 docker，无法灌种子数据"
    return 1
  fi
}

seed() {
  if [ ! -f "$SCHEMA_SQL" ]; then
    fail "找不到 schema.sql：$SCHEMA_SQL"
    return 1
  fi
  log "灌种子：$(basename "$SCHEMA_SQL")（四库 DDL + outbox 同构表，幂等）"
  mysql_exec <"$SCHEMA_SQL" >/dev/null

  # micro 自有的 DDL 补丁：库存预占流水的 (order_no, product_id) 唯一键。
  # 不并进上面的共用 schema.sql —— 那是 mall 案例的资产，且唯一键属于 micro
  # 侧这次的幂等修复。缺了它，「重放一次 POST /order」会多冻结一份库存。
  if [ -f "$SCHEMA_MICRO_SQL" ]; then
    log "灌种子补丁：$(basename "$SCHEMA_MICRO_SQL")（micro 自有 DDL，幂等）"
    mysql_exec <"$SCHEMA_MICRO_SQL" >/dev/null
  else
    fail "找不到 micro 的 DDL 补丁：$SCHEMA_MICRO_SQL"
    return 1
  fi

  # 幂等种子行：反复重跑不会撞 uk_order_no，也不会让主键漂移。
  log "灌种子订单：order_db.orders id=$SEED_ORDER_ID user_id=$USER_ID"
  mysql_exec <<SQL
INSERT INTO order_db.orders (id, user_id, order_no, status, total_amount, paid_amount)
VALUES ($SEED_ORDER_ID, $USER_ID, 'E2E-SEED-$SEED_ORDER_ID', 'created', 19900, 0)
ON DUPLICATE KEY UPDATE user_id = VALUES(user_id), status = 'created';
-- items 没有可用于 ON DUPLICATE KEY 的唯一键（idx_items_order 只是普通索引），
-- 光靠 INSERT 重跑会累加明细行；这里先删后插，重跑 N 次仍是 1 条明细。
DELETE FROM order_db.order_items WHERE order_id = $SEED_ORDER_ID;
INSERT INTO order_db.order_items (order_id, product_id, product_name, quantity, unit_price)
VALUES ($SEED_ORDER_ID, 1, 'e2e seed item', 1, 19900);
SQL

  # 直连灌库绕过了写路径，也就绕过了「先落库再失效缓存」那一步 —— 下一次读会命中
  # 5 分钟 TTL 里的旧缓存值（实测：库里 1 条明细，接口返回 3 条）。必须显式 DEL，
  # 否则 retry 一次脚本就可能对着陈旧数据做判断。delete 失败不致命：读链路不见它，
  # 只是这一轮读到旧值，TTL 到期自愈。
  "$DOCKER_BIN" exec redis redis-cli -n 0 DEL "mall:order:$USER_ID:$SEED_ORDER_ID" >/dev/null 2>&1 \
    || log "（跳过）订单缓存未失效：本轮可能读到旧值，等 TTL 或重跑即可"
}

# ------------------------------------------------------------
# 步骤 2：六进程
#
# 每个进程必须 curl 到自己的目录下启动 —— config.yaml 按 CWD 解析
# （main.go 里 configPath = "config.yaml"，相对路径）。
# ------------------------------------------------------------
start_proc() {
  local dir=$1 name=$2 bin=$3 port=$4
  if port_open "$port"; then
    if [ "${E2E_REUSE:-0}" = "1" ]; then
      log "复用已在跑的 ${name}（端口 ${port} 已被监听）"
      return 0
    fi
    fail "$name 的端口 $port 已被占用；停掉它或设 E2E_REUSE=1 复用现成的进程"
    return 1
  fi
  log "启动 ${name}（cwd=$(basename "${dir}")，等待 ${port}）"
  ( cd "$dir" && exec "$bin" ) >"$WORKDIR/log/$name.log" 2>&1 &
  local pid=$!
  STARTED+=("$pid")
  if ! wait_port "$port" 40; then
    fail "$name 在 40s 内没有监听 $port"
    tail -n 20 "$WORKDIR/log/$name.log" >&2 || true
    return 1
  fi
  # 进程可能监听了端口又立刻崩掉，补一次存活检查。
  sleep 0.3
  if ! kill -0 "$pid" 2>/dev/null; then
    fail "$name 进程已退出"
    tail -n 20 "$WORKDIR/log/$name.log" >&2 || true
    return 1
  fi
  return 0
}

consul_has() {
  local svc=$1 timeout=${2:-30} i
  for ((i = 0; i < timeout; i++)); do
    local body
    body="$(curl -s --noproxy '*' -m 5 "http://127.0.0.1:8500/v1/health/service/$svc?passing" || true)"
    if [ -n "$body" ] && [ "$body" != "[]" ]; then return 0; fi
    sleep 1
  done
  return 1
}

# ------------------------------------------------------------
# 步骤 3：断言
# ------------------------------------------------------------
assert_ping() {
  local body
  body="$(curl -sf --noproxy '*' -m 10 "$GATEWAY/api/ping" || true)"
  # curl -sf 静默吞掉非 2xx 与连接错误，必须有返回值才谈得上比较响应内容。
  if [ -z "$body" ]; then
    fail "GET /api/ping 无响应体（网关不可达，或返回非 2xx 被 -f 吞掉）"
    return 1
  fi
  printf '%s\n' "$body"
  case "$body" in
    *'"service":"demo-bff"'*) ;;
    *) fail "/api/ping 响应不含 \"service\":\"demo-bff\""; return 1 ;;
  esac
}

# 注意这里带了两样东西，缺一都过不去，且失败长得不一样：
#   - `?user_id=`：order 域要求携带身份，防跨用户读单（见 docs/verify-chain.md §10：
#     曾经把「缺 user_id」渲染成 502 造成过误诊）；
#   - `Authorization: Bearer <demo.auth_token>`：bff 的 `auth.require=true` 时
#     缺这个头会在**边界**就 401（`missing bearer token`），根本到不了 order 域。
# 断言的目标（链路真的打到 srv-order 的 db 层）没有变，变的只是入参。
assert_order() {
  local body
  # `${AUTH[@]+"${AUTH[@]}"}` 而不是 `"${AUTH[@]}"`：bash 3.2 下 `set -u` 遇到
  # 空数组的 `"${A[@]}"` 会报 unbound variable。这层壳让 DEMO_TOKEN="" 也能跑
  #（那条路径是刻意留的：验「没凭据会被挡」）。
  body="$(curl -sf --noproxy '*' -m 15 ${AUTH[@]+"${AUTH[@]}"} \
    "$GATEWAY/api/order/$SEED_ORDER_ID?user_id=$USER_ID" || true)"
  if [ -z "$body" ]; then
    fail "GET /api/order/$SEED_ORDER_ID 无响应体（网关不可达，或返回非 2xx 被 -f 吞掉）"
    return 1
  fi
  printf '%s\n' "$body"
  case "$body" in
    *'"order_id":'*"$SEED_ORDER_ID"*) ;;
    *) fail "/api/order/$SEED_ORDER_ID 响应不含 order_id=$SEED_ORDER_ID"; return 1 ;;
  esac
}

# assert_metrics 指标采集口真的可读（本轮新增）。
#
# 判据分三层，缺一不算过 —— 只断言「HTTP 200」是不够的：一个返回空 JSON 的
# 假端点同样 200（ACCEPTANCE §15 第 7 条明确把这种凑数行为判 FAIL）。
#   1. 有响应体（-f 没吞掉）；
#   2. 是 Prometheus 文本格式（含 `# HELP`，不是随手写的 JSON）；
#   3. 里面出现**该进程自己的**指标名 —— HTTP 档要有 webx_requests_total，
#      RPC 档要有 rpcx_requests_total（只看 200 会被"空壳端点"骗过）。
assert_metrics() {
  local label=$1 url=$2 want=$3 body
  body="$(curl -sf --noproxy '*' -m 10 "$url" || true)"
  if [ -z "${body}" ]; then
    fail "${label}: ${url} 无响应（-f 吞掉了非 2xx，或采集口没起）"
    return 1
  fi
  case "${body}" in
    *'# HELP'*) ;;
    *) fail "${label}: ${url} 不是 Prometheus 文本格式（缺 # HELP）"; return 1 ;;
  esac
  case "${body}" in
    *"${want}"*) ;;
    *) fail "${label}: ${url} 里找不到 ${want}（端点活着但没接上仪表）"; return 1 ;;
  esac
  printf '  ✓ %s：含 %s（%s 行）\n' "${label}" "${want}" \
    "$(printf '%s' "${body}" | wc -l | tr -d ' ')"
}

main() {
  mkdir -p "$WORKDIR/log" "$WORKDIR/bin"

  require_base
  seed

  # 应用账号口令（注入 config.yaml 的 ${HUBX_DB_PW} 占位）：从环境取、缺省即报错。
  # 给默认值 hubx123 的旧写法会让"口令没配"这件事静默过去 —— 连上的是哪个库
  # 全凭运气，排障时看不出来。
  export HUBX_DB_PW="${HUBX_DB_PW:?请设置 HUBX_DB_PW（应用账号 mall_rw 的口令）}"
  export CGO_ENABLED=0

  # 四个域服：90xx 是 kitex RPC 端口；业务数据都在各自私有库里。
  local spec name port dir bin
  for spec in "srv-product:demo-product:9021" "srv-order:demo-order:9022" \
              "srv-stock:demo-stock:9023" "srv-points:demo-points:9024" \
              "bff:demo-bff:8081" "gateway:demo-gateway:8080"; do
    dir=${spec%%:*}; rest=${spec#*:}; name=${rest%%:*}; port=${rest##*:}
    bin="$WORKDIR/bin/$dir"
    log "编译 $dir → $bin"
    ( cd "$MICRO_DIR/$dir" && "$GO_BIN" build -o "$bin" ./cmd ) >"$WORKDIR/log/build-$dir.log" 2>&1 \
      || { fail "$dir 编译失败"; tail -n 20 "$WORKDIR/log/build-$dir.log" >&2; return 1; }
    start_proc "$MICRO_DIR/$dir" "$name" "$bin" "$port"
  done

  # gateway 只做服务发现、不注册自己（部署形态决定的：它没有业务端点
  # 需要被别的服务发现），故就绪判据是 HTTP 面能答，而不是 consul 里有条目。
  for name in demo-product demo-order demo-stock demo-points demo-bff; do
    if consul_has "$name" 40; then
      log "consul 已发现 ${name}（passing）"
    else
      fail "consul 40s 内未见 $name 的健康实例"
      return 1
    fi
  done

  local i
  for ((i = 0; i < 30; i++)); do
    http_ok "$GATEWAY/api/ping" && break
    sleep 1
  done

  log "断言 ①：GET /api/ping"
  assert_ping
  log "断言 ②：GET /api/order/$SEED_ORDER_ID?user_id=$USER_ID"
  assert_order

  # ③ 放在业务之后：计数器只在**发生过调用**之后才会被导出（零观测的 counter
  # 不出现在导出里），所以顺序反了会得到一个必然失败的断言。
  log "断言 ③：指标采集口（HTTP 档 bff / RPC 档 srv-order）"
  assert_metrics "bff(HTTP 档, /metrics 挂 webx 口)" "$BFF_METRICS" "webx_requests_total"
  assert_metrics "bff(出向 RPC, rpc_role=client)" "$BFF_METRICS" "rpcx_requests_total"
  assert_metrics "srv-order(RPC 档, 独立采集口)" "$SRV_ORDER_METRICS" "rpcx_requests_total"

  printf '\n'
  log "E2 通过：gateway → bff → 四域服 调用链 + 指标采集口（退出码 0）"
}

main "$@"
