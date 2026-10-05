#!/usr/bin/env bash
# =============================================================================
# order_db 的**唯一 DDL 真源** —— monolith 与 micro/srv 共用同一个库
# -----------------------------------------------------------------------------
# 背景（这是踩过的坑，别退回原样）：monolith/scripts/bring-up.sh 与 micro/srv
# 曾各带一份 DDL，两边都写 CREATE TABLE IF NOT EXISTS —— 于是「谁先跑谁赢」，
# 后跑的一方必然撞上 `Unknown column` 而 502。两份 DDL 打架是环境级 bug，
# 不是业务 bug，故收敛到本文件：
#
#   * 列取**并集**：monolith 的 item/qty/amount 与 srv 的 total_amount/
#     paid_amount/pay_order_no 同时存在，两边业务代码一行都不用改；
#   * 追加列一律带 DEFAULT（NOT NULL 无默认值会让另一方的 INSERT 直接失败）；
#   * 对**已存在**的表做幂等补列（MySQL 8.0 没有 ADD COLUMN IF NOT EXISTS，
#     故先查 information_schema 再决定是否 ALTER）。
#
# 用法（在 examples 目录下）：
#   export HUBX_DB_PW=hubx123
#   ./scripts/order_db.sh up      # 建库建表 + 补列 + 授权 + 种子（幂等）
#   ./scripts/order_db.sh check   # 只核对：库/表/列/账号是否齐
#
# 本机通常没有 mysql 客户端，默认经 docker exec 走 compose 的 mysql 容器。
# =============================================================================

set -euo pipefail

DOCKER="${DOCKER_BIN:-/Applications/Docker.app/Contents/Resources/bin/docker}"
CONTAINER="${MYSQL_CONTAINER:-mysql}"
DB_NAME="${MYSQL_DB:-order_db}"
ROOT_PW="${MYSQL_ROOT_PW:-rootpass}"
APP_USER="${MYSQL_APP_USER:-order_rw}"
APP_PW="${HUBX_DB_PW:-hubx123}"

# mysql -N -B -e "<sql>"：只出数据行，便于取值判断
mysql_client() {
  if command -v mysql >/dev/null 2>&1; then
    MYSQL_PWD="$ROOT_PW" mysql -uroot -N -B -e "$1"
    return
  fi
  MYSQL_QUERY="$1" MYSQL_PWD="$ROOT_PW" "$DOCKER" exec -e MYSQL_QUERY -e MYSQL_PWD \
    "$CONTAINER" sh -c 'exec mysql -uroot -N -B -e "$MYSQL_QUERY"'
}

# mysql_db "<sql>"：带库上下文的执行（表名前缀会污染反引号写法，故用 USE）
mysql_db() { mysql_client "USE ${DB_NAME}; $1"; }

# --- 表列清单：并集，只增不减 ------------------------------------------------
# 格式：表名 | 列名:定义 ...
declare -a TABLES=(
  "orders|id:BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY|user_id:BIGINT UNSIGNED NOT NULL DEFAULT 0|order_no:VARCHAR(64) NOT NULL DEFAULT ''|status:VARCHAR(32) NOT NULL DEFAULT 'created'|item:VARCHAR(128) NOT NULL DEFAULT ''|qty:INT NOT NULL DEFAULT 0|amount:DECIMAL(10,2) NOT NULL DEFAULT 0|total_amount:BIGINT NOT NULL DEFAULT 0|paid_amount:BIGINT NOT NULL DEFAULT 0|pay_order_no:VARCHAR(64) NOT NULL DEFAULT ''|created_at:DATETIME(3) DEFAULT CURRENT_TIMESTAMP(3)|updated_at:DATETIME(3) DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3)"
  "order_items|id:BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY|order_id:BIGINT UNSIGNED NOT NULL DEFAULT 0|order_no:VARCHAR(64) NOT NULL DEFAULT ''|product_id:BIGINT UNSIGNED NOT NULL DEFAULT 0|product_name:VARCHAR(128) NOT NULL DEFAULT ''|quantity:INT NOT NULL DEFAULT 0|unit_price:BIGINT NOT NULL DEFAULT 0|sku:VARCHAR(64) NOT NULL DEFAULT ''|qty:INT NOT NULL DEFAULT 0|price:DECIMAL(10,2) NOT NULL DEFAULT 0|subtotal:DECIMAL(10,2) NOT NULL DEFAULT 0"
  "order_status_log|id:BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY|order_id:BIGINT UNSIGNED NOT NULL DEFAULT 0|from_status:VARCHAR(16) NOT NULL DEFAULT ''|to_status:VARCHAR(16) NOT NULL DEFAULT ''|remark:VARCHAR(128) NOT NULL DEFAULT ''|created_at:DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP"
  "refund_records|id:BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY|order_id:BIGINT UNSIGNED NOT NULL DEFAULT 0|user_id:BIGINT UNSIGNED NOT NULL DEFAULT 0|amount:BIGINT NOT NULL DEFAULT 0|status:VARCHAR(16) NOT NULL DEFAULT 'pending'|reason:VARCHAR(128) NOT NULL DEFAULT ''|created_at:DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP|updated_at:DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP"
  "outbox|id:BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY|topic:VARCHAR(64) NOT NULL DEFAULT ''|event_type:VARCHAR(64) NOT NULL DEFAULT ''|payload:TEXT NULL|traceparent:VARCHAR(128) NOT NULL DEFAULT ''|status:TINYINT NOT NULL DEFAULT 0|retry_count:INT NOT NULL DEFAULT 0|created_at:DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP|updated_at:DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP"
  "users|id:BIGINT NOT NULL PRIMARY KEY|name:VARCHAR(64) NOT NULL DEFAULT ''|level:VARCHAR(16) NOT NULL DEFAULT 'normal'"
)

column_exists() { # <table> <column>
  local n
  n="$(mysql_client "SELECT COUNT(*) FROM information_schema.columns WHERE table_schema='${DB_NAME}' AND table_name='$1' AND column_name='$2';")"
  [[ "$n" != "0" ]]
}

table_exists() { # <table>
  local n
  n="$(mysql_client "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='${DB_NAME}' AND table_name='$1';")"
  [[ "$n" != "0" ]]
}

cmd_up() {
  echo "== order_db up =="
  mysql_client "CREATE DATABASE IF NOT EXISTS ${DB_NAME} CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;"

  local entry table spec col def created=0 altered=0
  for entry in "${TABLES[@]}"; do
    table="${entry%%|*}"
    if ! table_exists "$table"; then
      # 建表：拿到全部列定义（并集），一次到位
      local cols_sql="" first=1
      IFS='|' read -r -a parts <<< "$entry"
      for spec in "${parts[@]:1}"; do
        col="${spec%%:*}"; def="${spec#*:}"
        [[ $first -eq 1 ]] && first=0 || cols_sql+=", "
        cols_sql+="\`${col}\` ${def}"
      done
      mysql_db "CREATE TABLE \`${table}\` (${cols_sql}) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;"
      echo "  + 建表 ${table}"
      created=$((created + 1))
      continue
    fi
    # 已存在：只补缺列（幂等），不动已有列与数据
    IFS='|' read -r -a parts <<< "$entry"
    for spec in "${parts[@]:1}"; do
      col="${spec%%:*}"; def="${spec#*:}"
      case "$def" in *AUTO_INCREMENT*|*PRIMARY\ KEY*) continue ;; esac
      if ! column_exists "$table" "$col"; then
        mysql_db "ALTER TABLE \`${table}\` ADD COLUMN \`${col}\` ${def};"
        echo "  ~ ${table}.${col} 补列"
        altered=$((altered + 1))
      fi
    done
  done

  # 兼容旧表：item 原为 NOT NULL 无默认值，会让 srv 侧 INSERT 失败
  if column_exists orders item; then
    mysql_db "ALTER TABLE orders MODIFY COLUMN item VARCHAR(128) NOT NULL DEFAULT '';" || true
  fi

  mysql_db "CREATE USER IF NOT EXISTS '${APP_USER}'@'%' IDENTIFIED BY '${APP_PW}';
GRANT SELECT, INSERT, UPDATE, DELETE ON ${DB_NAME}.* TO '${APP_USER}'@'%';
FLUSH PRIVILEGES;
INSERT IGNORE INTO users (id, name, level) VALUES (1, 'alice', 'gold'), (2, 'bob', 'normal');"
  echo "  = 建表 ${created} 张 / 补列 ${altered} 处 / 授权与种子已就绪"
  cmd_check
}

cmd_check() {
  echo "== order_db check =="
  local rc=0 entry table spec col
  for entry in "${TABLES[@]}"; do
    table="${entry%%|*}"
    if ! table_exists "$table"; then
      echo "  ✗ 缺表 ${table}" >&2; rc=1; continue
    fi
    IFS='|' read -r -a parts <<< "$entry"
    for spec in "${parts[@]:1}"; do
      col="${spec%%:*}"
      if ! column_exists "$table" "$col"; then
        echo "  ✗ 缺列 ${table}.${col}" >&2; rc=1
      fi
    done
  done
  local n
  n="$(mysql_client "SELECT COUNT(*) FROM ${DB_NAME}.users;")"
  if (( rc == 0 )); then
    echo "  ✓ 6 表列全齐（orders/order_items/order_status_log/refund_records/outbox/users），users=${n}"
  fi
  return "$rc"
}

case "${1:-up}" in
  up)    cmd_up ;;
  check) cmd_check ;;
  *) echo "用法: $0 {up|check}" >&2; exit 2 ;;
esac
