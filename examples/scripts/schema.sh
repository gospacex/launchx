#!/usr/bin/env bash
# ============================================================================
# examples/scripts/schema.sh —— 灌 examples/mall/scripts/schema.sql
#                               （四库 DDL + 应用账号 mall_rw）
#
# 为什么非要一个脚本：`docker exec -i mysql mysql -uroot -prootpass < schema.sql`
# 这一行为什么不灵 ——
#   那份 SQL 里应用账号的口令写成**占位** `${HUBX_DB_PW:-hubx123}`，而**重定向
#   不经过 shell 展开**：mysql 收到什么就执行什么，于是
#     CREATE USER 'mall_rw'@'%' IDENTIFIED BY '${HUBX_DB_PW:-hubx123}';
#   会被原样灌进去：账号建成功了、GRANT 也发了、SHOW GRANTS 一切正常，
#   唯一症状是应用侧带着「每天都在用的那个口令」来连，得到 ERROR 1045。
#   1045 只说「访问被拒」，一句不提灌库姿势，于是人开始怀疑网络、怀疑 host
#   通配符、怀疑 MySQL 8 的认证插件 —— 本机 2026-10-14 就是这么丢掉时间的。
#
# 所以本脚本做三件事：**渲染 → 灌 → 立刻用应用账号连一次**。第三步是关键：
# 它跳过「账号存在」「GRANT 正确」这些会误导人的绿灯，只问那个唯一重要的问题
# —— 灌库落下的口令，和 config.yaml 里 ${HUBX_DB_PW} 展开出来的口令，
#   是不是同一个值。
#
# 用法：
#   export MYSQL_ROOT_PW=<mysql root 口令>         # 必填
#   export HUBX_DB_PW=<应用账号 mall_rw 的口令>    # 必填，须与四服 config.yaml 同一个值
#   bash examples/scripts/schema.sh apply   # 渲染 + 灌 + 验证（幂等，可反复跑）
#   bash examples/scripts/schema.sh check   # 只验证：应用账号能不能连 + 四库在不在
#   bash examples/scripts/schema.sh render  # 只渲染到临时文件并打印路径（排障用）
#
# macOS bash 3.2 纪律：变量后面紧跟中文一律写成 ${name}（本文件已遵守）。
# ============================================================================
set -euo pipefail

EXAMPLES_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCHEMA_SQL="${SCHEMA_SQL:-$EXAMPLES_DIR/mall/scripts/schema.sql}"
APP_USER="${APP_USER:-mall_rw}"
DOCKER_BIN="${DOCKER_BIN:-/Applications/Docker.app/Contents/Resources/bin/docker}"
MYSQL_HOST="${MYSQL_HOST:-127.0.0.1}"
MYSQL_PORT="${MYSQL_PORT:-3306}"
MYSQL_CONTAINER="${MYSQL_CONTAINER:-mysql}"

log() { printf '  %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; }

# ------------------------------------------------------------
# 口令一律从环境取、缺省即报错。理由和 examples/micro/scripts/e2e.sh 一致：
# 脚本进了仓库，写死在里面的口令等于公开；而「口令没配」必须显式失败 ——
# 悄悄用一个默认口令连上，只会让真正去改口令的人发现改了也没生效。
# MALL_RW_PW 是旧名，仍然兼容（优先级高于 HUBX_DB_PW）。
# ------------------------------------------------------------
resolve_app_pw() {
  local pw="${MALL_RW_PW:-}"
  if [ -z "$pw" ]; then
    pw="${HUBX_DB_PW:?请设置 HUBX_DB_PW（应用账号 ${APP_USER} 的口令，须与四服 config.yaml 的 DSN 占位同一个值）}"
  fi
  printf '%s' "$pw"
}

# mysql 客户端：宿主有就用宿主（走 TCP，不走 socket —— 库在 docker 里，
# 宿主上没有那个 socket 路径），没有就回落容器内的客户端。
mysql_root() {
  local pw="${MYSQL_ROOT_PW:?请设置 MYSQL_ROOT_PW（mysql root 口令）}"
  if command -v mysql >/dev/null 2>&1; then
    MYSQL_PWD="$pw" mysql -h"$MYSQL_HOST" -P"$MYSQL_PORT" -uroot "$@"
  elif [ -x "${DOCKER_BIN}" ]; then
    MYSQL_PWD="$pw" "${DOCKER_BIN}" exec -i -e MYSQL_PWD "$MYSQL_CONTAINER" \
      mysql -uroot "$@"
  else
    fail "没有 mysql 客户端也没有 docker（${DOCKER_BIN}）"
    return 1
  fi
}

# ------------------------------------------------------------
# 渲染：把 ${XXX} / ${XXX:-default} 占位换成真口令。
#   sed 的替换值里 `&` 代表「整段匹配」、`\` 会吞掉后一个字符，口令里万一有
#   它们必须转义；分隔符取 `|`，免得口令里的 `/`（base64 类口令很常见）
#   提前结束表达式。
# 渲染完**断言没有残留占位** —— 这一条比什么都重要：新增占位忘了加替换规则，
# 就会有人在毫不知情的情况下把字面量口令灌进库里。
# ------------------------------------------------------------
render() {
  local src="$1" dst="$2" pw escaped
  pw="$(resolve_app_pw)"
  escaped="$(printf '%s' "$pw" | sed 's/[&\\]/\\&/g')"
  sed -e "s|\\\${HUBX_DB_PW:-[^}]*}|${escaped}|g" \
      -e "s|\\\${MALL_RW_PW:-[^}]*}|${escaped}|g" \
      -e "s|\\\${HUBX_DB_PW}|${escaped}|g" \
      -e "s|\\\${MALL_RW_PW}|${escaped}|g" \
      "$src" >"$dst"
  if grep -q '\${' "$dst"; then
    fail "$(basename "$src") 渲染后仍有未展开的占位符（口令会被设成字面量）"
    grep -n '\${' "$dst" | head -5 >&2
    return 1
  fi
}

# ------------------------------------------------------------
# 用**应用账号**而不是 root 连一次。
# 这里问的是「灌库落下的口令」与「config.yaml 展开出的口令」是否同一个值；
# 用 root 连永远 PASS，而 root 根本不是应用连库用的账号 —— 它对这个问题
# 没有任何发言权。
# ------------------------------------------------------------
check() {
  local pw out
  pw="$(resolve_app_pw)"
  if command -v mysql >/dev/null 2>&1; then
    out="$(MYSQL_PWD="$pw" mysql -h"$MYSQL_HOST" -P"$MYSQL_PORT" -u"$APP_USER" \
      -N -B -e "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name IN ('product_db','order_db','stock_db','points_db')" 2>&1)" || true
  elif [ -x "${DOCKER_BIN}" ]; then
    out="$(MYSQL_PWD="$pw" "${DOCKER_BIN}" exec -e MYSQL_PWD "$MYSQL_CONTAINER" \
      mysql -u"$APP_USER" -h"$MYSQL_HOST" -N -B \
      -e "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name IN ('product_db','order_db','stock_db','points_db')" 2>&1)" || true
  else
    fail "没有 mysql 客户端也没有 docker"
    return 1
  fi
  if [ "$out" != "4" ]; then
    fail "应用账号 ${APP_USER} 连库失败或看不到四库（期望 4，实际：${out}）"
    log "  这一步失败=灌库落下的口令与本次 HUBX_DB_PW 不是同一个值。"
    log "  修法：docker exec ${MYSQL_CONTAINER} mysql -uroot -p<root 口令> \\"
    log "        -e \"ALTER USER '${APP_USER}'@'%' IDENTIFIED BY '<你的口令>'\""
    return 1
  fi
  log "应用账号 ${APP_USER} 连通，四库齐备"
  return 0
}

apply() {
  local rendered
  if [ ! -f "$SCHEMA_SQL" ]; then
    fail "找不到 schema.sql：$SCHEMA_SQL"
    return 1
  fi
  rendered="$(mktemp "${TMPDIR:-/tmp}/schema-rendered.XXXXXX")"
  if ! render "$SCHEMA_SQL" "$rendered"; then
    rm -f "$rendered"
    return 1
  fi
  log "灌 $(basename "$SCHEMA_SQL")（已渲染真口令，幂等）"
  mysql_root <"$rendered" >/dev/null
  rm -f "$rendered"
  check
}

render_only() {
  local rendered
  rendered="$(mktemp "${TMPDIR:-/tmp}/schema-rendered.XXXXXX")"
  render "$SCHEMA_SQL" "$rendered" || { rm -f "$rendered"; return 1; }
  printf '%s\n' "$rendered"
}

case "${1:-}" in
  apply) apply ;;
  check) check ;;
  render) render_only ;;
  *)
    printf '用法：bash %s {apply|check|render}\n' "${BASH_SOURCE[0]}" >&2
    exit 2
    ;;
esac
