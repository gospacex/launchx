#!/usr/bin/env bash
# 单体 demo 的库表准备：建表 + 种子用户（幂等，可反复执行）。
#
# 用法（在 examples/monolith 目录下）：
#   export HUBX_DB_PW=hubx123        # 业务账号密码，须与 config.yaml 的 ${HUBX_DB_PW} 一致
#   ./scripts/bring-up.sh            # 默认：建表 + 授权 + 种子
#   ./scripts/bring-up.sh check      # 只探连通性与表是否齐全
#
# 环境前提：
#   - hubx/dockers/docker-compose.yml 起的 mysql 容器已在 127.0.0.1:3306 监听；
#   - 本机通常没有 mysql 客户端，故默认走 docker exec（有客户端时优先用客户端）。
# 说明：demo 不做进程内 AutoMigrate——库表由本脚本显式准备，
#       避免"服务起来了但表结构来源不明"这种隐式依赖。

set -euo pipefail

DOCKER="${DOCKER_BIN:-/Applications/Docker.app/Contents/Resources/bin/docker}"
CONTAINER="${MYSQL_CONTAINER:-mysql}"
DB_NAME="${MYSQL_DB:-order_db}"
ROOT_PW="${MYSQL_ROOT_PW:-rootpass}"          # compose 的 MYSQL_ROOT_PASSWORD
APP_USER="${MYSQL_APP_USER:-order_rw}"
APP_PW="${HUBX_DB_PW:-hubx123}"
MYSQL_HOST="${MYSQL_HOST:-127.0.0.1}"
MYSQL_PORT="${MYSQL_PORT:-3306}"

readonly TABLES="orders order_items users"

# mysql_client 执行一条 SQL：本机有 mysql 客户端就直连，否则经 docker exec。
mysql_client() { # <sql>
  local sql="$1"
  if command -v mysql >/dev/null 2>&1; then
    MYSQL_PWD="$ROOT_PW" mysql -h"$MYSQL_HOST" -P"$MYSQL_PORT" -uroot -N -B -e "$sql"
    return
  fi
  if [[ ! -x "$DOCKER" ]]; then
    echo "ERROR: 本机无 mysql 客户端，且 docker CLI 不可执行：$DOCKER" >&2
    echo "       请用 DOCKER_BIN=<绝对路径> 指定，或安装 mysql 客户端。" >&2
    exit 1
  fi
  MYSQL_QUERY="$sql" MYSQL_PWD="$ROOT_PW" "$DOCKER" exec -e MYSQL_QUERY -e MYSQL_PWD \
    "$CONTAINER" sh -c 'exec mysql -uroot -N -B -e "$MYSQL_QUERY"'
}

DDL="CREATE DATABASE IF NOT EXISTS ${DB_NAME} CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
USE ${DB_NAME};
CREATE TABLE IF NOT EXISTS orders (
  id         BIGINT AUTO_INCREMENT PRIMARY KEY,
  order_no   VARCHAR(64)   NOT NULL,
  user_id    BIGINT        NOT NULL DEFAULT 0,
  item       VARCHAR(128)  NOT NULL,
  qty        INT           NOT NULL DEFAULT 0,
  amount     DECIMAL(10,2) NOT NULL DEFAULT 0,
  status     VARCHAR(32)   NOT NULL DEFAULT 'created',
  created_at DATETIME(3)   DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3)   DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_orders_order_no (order_no),
  KEY idx_orders_user_id (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
CREATE TABLE IF NOT EXISTS order_items (
  id       BIGINT AUTO_INCREMENT PRIMARY KEY,
  order_no VARCHAR(64)   NOT NULL,
  sku      VARCHAR(64)   NOT NULL,
  qty      INT           NOT NULL DEFAULT 0,
  price    DECIMAL(10,2) NOT NULL DEFAULT 0,
  subtotal DECIMAL(10,2) NOT NULL DEFAULT 0,
  KEY idx_order_items_order_no (order_no)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
CREATE TABLE IF NOT EXISTS users (
  id    BIGINT PRIMARY KEY,
  name  VARCHAR(64) NOT NULL,
  level VARCHAR(16) NOT NULL DEFAULT 'normal'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
CREATE USER IF NOT EXISTS '${APP_USER}'@'%' IDENTIFIED BY '${APP_PW}';
GRANT SELECT, INSERT, UPDATE, DELETE ON ${DB_NAME}.* TO '${APP_USER}'@'%';
FLUSH PRIVILEGES;
INSERT IGNORE INTO users (id, name, level) VALUES (1, 'alice', 'gold'), (2, 'bob', 'normal');"

check() {
  mysql_client "SELECT TABLE_NAME FROM information_schema.tables WHERE table_schema='${DB_NAME}';" \
    > /tmp/monolith-tables.txt
  local missing=""
  local t
  for t in $TABLES; do
    grep -qx "$t" /tmp/monolith-tables.txt || missing="$missing $t"
  done
  if [[ -n "$missing" ]]; then
    echo "MISSING TABLES:$missing —— 先执行 $(basename "$0")" >&2
    return 1
  fi
  local n
  n=$(mysql_client "SELECT COUNT(*) FROM ${DB_NAME}.users;")
  echo "OK tables:$TABLES db:${DB_NAME} users:$n"
}

case "${1:-up}" in
  check)
    check
    ;;
  up|"")
    mysql_client "$DDL"
    echo "DDL + grant + seed done"
    check
    ;;
  *)
    echo "usage: $0 [up|check]" >&2
    exit 2
    ;;
esac
