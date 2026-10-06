#!/usr/bin/env bash
# ============================================================
# E3 · srv-order 回归：下单链路 + outbox→kafka 投递 + main.go ≤ 50 行
#
# 目标服务变更说明：`examples/micro/srv`（拆分前的单域旧样例 demo-srv）已随
# 四域拆分删除，E3 现在落到 `examples/micro/srv-order`（demo-order，9022）。
#
# 三条断言：
#   ① 下单：经 kitex 直连 demo-order 下单一笔 → 业务码 0 + 订单落 order_db
#   ② 投递：outbox Runner 补发后，topic order.completed.v1 能收到该单事件
#   ③ 行数：cmd/main.go ≤ 50 行（"main 只做 Bootstrap+OnSetup+Run" 的守门线）
#
# 用法：
#   bash examples/micro/srv-order/scripts/run-e2e.sh
#   E3_REUSE=1 bash examples/micro/srv-order/scripts/run-e2e.sh   # 复用已起的 srv-order
#
# 退出码：0 = 三条断言均通过；非 0 = 前置缺失 / 断言失败。
#
# 踩坑记录（macOS bash 3.2）：紧跟中文的变量必须写成 ${var}。详见
# examples/micro/scripts/e2e.sh 头部注释。
# ============================================================
set -euo pipefail

SRV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_ROOT="$(cd "$SRV_DIR/../../.." && pwd)"

GO_BIN="${GO_BIN:-$HOME/sdk/go1.24.3/bin/go}"
DOCKER_BIN="${DOCKER_BIN:-/Applications/Docker.app/Contents/Resources/bin/docker}"
TOPICS_SH="$REPO_ROOT/examples/mall/scripts/topics.sh"
MAIN_GO="$SRV_DIR/cmd/main.go"
MAX_MAIN_LINES="${MAX_MAIN_LINES:-50}"

SRV_ADDR="${SRV_ADDR:-127.0.0.1:9022}"
SRV_PORT="${SRV_ADDR##*:}"
SERVICE_NAME="${SERVICE_NAME:-demo-order}"
TOPIC="${TOPIC:-order.completed.v1}"
KAFKA_CONTAINER="${KAFKA_CONTAINER:-kafka}"
KT="/opt/bitnami/kafka/bin"

# 把 docker 所在目录放进 PATH：它不在默认 PATH 里，而 chain.sh 之外的协作脚本
# （topics.sh 等）里都是裸写 docker 的。放在这里而不是每处调用前补一次。
case ":$PATH:" in
  *":$(dirname "$DOCKER_BIN"):"*) ;;
  *) PATH="$(dirname "$DOCKER_BIN"):$PATH"; export PATH ;;
esac

WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/srv-order-e2e.XXXXXX")"
readonly WORKDIR

SRV_PID=""

log()  { printf '\033[36m[e3]\033[0m %s\n' "$*"; }
fail() { printf '\033[31m[e3] FAIL\033[0m %s\n' "$*" >&2; }

cleanup() {
  local rc=$?
  trap - EXIT
  if [ -n "$SRV_PID" ]; then
    log "停止 srv-order（pid ${SRV_PID}）—— 走正常退出以便退出链冲刷最后一批 span"
    kill "$SRV_PID" 2>/dev/null || true
    wait "$SRV_PID" 2>/dev/null || true
  fi
  if [ "$rc" -ne 0 ]; then
    fail "退出码 ${rc}；日志在 ${WORKDIR}/log/"
    for f in "$WORKDIR"/log/*.log; do
      [ -f "$f" ] || continue
      printf -- '--- %s ---\n' "$f"
      tail -n 15 "$f" || true
    done
  else
    rm -rf "$WORKDIR"
  fi
  exit "$rc"
}
trap cleanup EXIT

port_open() { (exec 3<>"/dev/tcp/${1%%:*}/${1##*:}") 2>/dev/null; }

wait_port() {
  local target=$1 timeout=${2:-30} i
  for ((i = 0; i < timeout; i++)); do
    if port_open "$target"; then return 0; fi
    sleep 1
  done
  return 1
}

require_base() {
  local pair name target
  local missing=()
  for pair in "consul:127.0.0.1:8500" "mysql:127.0.0.1:3306" \
              "redis:127.0.0.1:6379" "kafka:127.0.0.1:9092"; do
    name=${pair%%:*}; target=${pair#*:}
    port_open "$target" || missing+=("${name}(${target})")
  done
  if [ ${#missing[@]} -gt 0 ]; then
    fail "缺少前置底座：${missing[*]}"
    return 1
  fi
  log "底座就绪：consul / mysql / redis / kafka"
}

# kafka_exec —— Kafka 工具在容器里，本机没有 kafka-topics.sh。
kafka_exec() {
  "$DOCKER_BIN" exec "$KAFKA_CONTAINER" bash -c "$KT/$1"
}

# ------------------------------------------------------------
# 前置 ①：topic 必须先存在
#
# 缺 topic 时 outbox 事件会**静默 abandon** —— 生产者报 Unknown Topic Or
# Partition，Runner 重试到上限就置 status=2，表面上"下单成功"，实际事件永远
# 没出域。所以这个前置不是洁癖，是让断言 ② 有可能失败得明明白白。
# ------------------------------------------------------------
ensure_topic() {
  log "前置：确保 topic ${TOPIC} 存在（缺 topic 会让 outbox 事件静默 abandon）"
  if [ ! -f "$TOPICS_SH" ]; then
    fail "找不到 topics.sh：${TOPICS_SH}"
    return 1
  fi
  DOCKER_BIN="$DOCKER_BIN" KAFKA_CONTAINER="$KAFKA_CONTAINER" bash "$TOPICS_SH" >/dev/null
  local list
  list="$(kafka_exec "kafka-topics.sh --bootstrap-server ${KAFKA_CONTAINER}:9092 --list" 2>/dev/null || true)"
  case "$list" in
    *"$TOPIC"*) log "topic ${TOPIC} 已就绪" ;;
    *)
      fail "topic ${TOPIC} 建不出来（topic 列表：$(printf '%s' "$list" | tr '\n' ' ')）"
      return 1
      ;;
  esac
}

mysql_exec() {
  "$DOCKER_BIN" exec -i mysql mysql -uroot -prootpass "$@" 2>/dev/null
}

# ------------------------------------------------------------
# 前置 ②：起 srv-order
# ------------------------------------------------------------
start_srv() {
  local bin="$WORKDIR/bin/srv-order"
  mkdir -p "$WORKDIR/bin" "$WORKDIR/log"

  if port_open "$SRV_ADDR"; then
    if [ "${E3_REUSE:-0}" != "1" ]; then
      fail "${SRV_ADDR} 已被占用；停掉它或设 E3_REUSE=1 复用"
      return 1
    fi
    log "复用已在跑的 ${SERVICE_NAME}（${SRV_ADDR}）"
    return 0
  fi

  log "编译 srv-order → ${bin}"
  ( cd "$SRV_DIR" && CGO_ENABLED=0 "$GO_BIN" build -o "$bin" ./cmd ) \
    >"$WORKDIR/log/build.log" 2>&1 \
    || { fail "编译失败"; tail -n 20 "$WORKDIR/log/build.log" >&2; return 1; }

  log "启动 srv-order（cwd=srv-order）"
  ( cd "$SRV_DIR" && HUBX_DB_PW="${HUBX_DB_PW:-hubx123}" exec "$bin" ) \
    >"$WORKDIR/log/srv-order.log" 2>&1 &
  SRV_PID=$!

  wait_port "$SRV_ADDR" 40 || { fail "${SRV_ADDR} 40s 内未监听"; tail -n 20 "$WORKDIR/log/srv-order.log" >&2; return 1; }

  local i
  for ((i = 0; i < 40; i++)); do
    local body
    body="$(curl -s --noproxy '*' -m 5 "http://127.0.0.1:8500/v1/health/service/${SERVICE_NAME}?passing" || true)"
    if [ -n "$body" ] && [ "$body" != "[]" ]; then
      log "consul 已发现 ${SERVICE_NAME}（passing）"
      return 0
    fi
    sleep 1
  done
  fail "consul 40s 内未见 ${SERVICE_NAME} 的健康实例"
  return 1
}

# ------------------------------------------------------------
# 断言 ①：下单 + 落库
# ------------------------------------------------------------
place_order() {
  local bin="$WORKDIR/bin/ordercli"
  log "编译 RPC 驱动 ordercli"
  ( cd "$SRV_DIR" && CGO_ENABLED=0 "$GO_BIN" build -o "$bin" ./scripts/ordercli ) \
    >"$WORKDIR/log/build-cli.log" 2>&1 \
    || { fail "ordercli 编译失败"; tail -n 20 "$WORKDIR/log/build-cli.log" >&2; return 1; }

  log "断言 ①a：经 kitex 直连 ${SRV_ADDR} 下单"
  ORDER_JSON="$(SRV_ORDER_ADDR="$SRV_ADDR" "$bin")"
  printf '%s\n' "$ORDER_JSON"
  case "$ORDER_JSON" in
    *'"ok":true'*) ;;
    *) fail "下单失败（RPC 或业务码非 0）"; return 1 ;;
  esac
  ORDER_NO="$(printf '%s' "$ORDER_JSON" | sed -n 's/.*"order_no":"\([^"]*\)".*/\1/p')"
  if [ -z "$ORDER_NO" ]; then
    fail "响应里没有 order_no，无法做后续投递断言"
    return 1
  fi
  log "下单一号：${ORDER_NO}"

  log "断言 ①b：订单落 order_db（orders ⋈ order_items 都有行）"
  local row
  row="$(mysql_exec -e "SELECT COUNT(*) FROM order_db.orders WHERE order_no='${ORDER_NO}';" | tail -1)"
  if [ "$row" != "1" ]; then
    fail "order_db.orders 中 order_no=${ORDER_NO} 的行数应为 1，实际 ${row}"
    return 1
  fi
  row="$(mysql_exec -e "SELECT COUNT(*) FROM order_db.order_items i JOIN order_db.orders o ON o.id=i.order_id WHERE o.order_no='${ORDER_NO}';" | tail -1)"
  if [ "$row" -lt 1 ] 2>/dev/null; then
    fail "order_items 中没有该单的明细行（实际 ${row}）"
    return 1
  fi
  log "落库确认：orders 1 行 + order_items ${row} 行"
}

# ------------------------------------------------------------
# 断言 ②：outbox → kafka
#
# outbox 的补发是**轮询**的（3s 一轮），不是同步投递，所以必须带超时地等。
# 每轮只读一小段窗口，命中即返，避免在 CLI 里干等满 30s。
# ------------------------------------------------------------
await_kafka_event() {
  local deadline=$((SECONDS + 45)) out
  log "断言 ②：等待 topic ${TOPIC} 出现 order_no=${ORDER_NO}（outbox 轮询补发，上限 45s）"
  while [ "$SECONDS" -lt "$deadline" ]; do
    out="$(kafka_exec "kafka-console-consumer.sh --bootstrap-server ${KAFKA_CONTAINER}:9092 --topic ${TOPIC} --from-beginning --timeout-ms 8000" 2>/dev/null || true)"
    if printf '%s' "$out" | grep -qF "$ORDER_NO"; then
      printf '%s\n' "$out" | grep -F "$ORDER_NO" | tail -1
      log "投递确认：topic ${TOPIC} 已收到该单事件"
      return 0
    fi
    sleep 2
  done
  fail "45s 内 topic ${TOPIC} 未出现 order_no=${ORDER_NO}（检查 outbox 行 status 是否被 abandon）"
  printf 'outbox 行状态：\n'
  mysql_exec -e "SELECT id, topic, status, retry_count FROM order_db.outbox WHERE payload LIKE '%${ORDER_NO}%'\\G" || true
  return 1
}

# ------------------------------------------------------------
# 断言 ③：main.go 行数
#
# 为什么单独守这一条：main.go 一旦开始堆业务（开连接、拼 exporter、
# 注册节点），"形态无关 + 装配集中" 这两条纪律就没有落脚点了，而它不会
# 让任何单测变红 —— 只能靠行数这条硬闸门。
# ------------------------------------------------------------
assert_main_lines() {
  local n
  n="$(wc -l <"$MAIN_GO" | tr -d ' ')"
  log "断言 ③：${MAIN_GO} 行数 ${n} ≤ ${MAX_MAIN_LINES}"
  if [ "$n" -gt "$MAX_MAIN_LINES" ]; then
    fail "main.go 已 ${n} 行，超过 ${MAX_MAIN_LINES} 行：业务逻辑应下沉到 app.Setup"
    return 1
  fi
}

main() {
  require_base
  ensure_topic
  start_srv
  place_order
  await_kafka_event
  assert_main_lines
  printf '\n'
  log "E3 通过：下单落库 + outbox 经 kafka 投递 + main.go ≤ ${MAX_MAIN_LINES} 行"
}

main "$@"
