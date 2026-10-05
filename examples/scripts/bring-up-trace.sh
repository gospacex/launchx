#!/usr/bin/env bash
# launchx 端到端调用链验收底座（tasks 0.3）
#
# 用途：拉起 / 校验 / 停止 Jaeger、MySQL、Kafka、Elasticsearch 四个容器，
#       供 trace-verification（T1–T9）与既有 E1–E3 验收复用。
#
# 环境前提（已实测）：
#   - Docker Desktop 的 CLI 不在 PATH，位于
#     /Applications/Docker.app/Contents/Resources/bin/docker
#   - 容器统一接入 dockers_devnet 网络（与 hubx/dockers/docker-compose.yml 一致）
#
# 用法：
#   ./bring-up-trace.sh start    启动/新建缺失容器，并等待就绪
#   ./bring-up-trace.sh check    探测全部端口与服务健康，全绿退出 0
#   ./bring-up-trace.sh status   打印容器与端口状态（不做断言）
#   ./bring-up-trace.sh stop     停止上述容器（不删除，保留数据）

set -euo pipefail

DOCKER="${DOCKER_BIN:-/Applications/Docker.app/Contents/Resources/bin/docker}"
NET="${DOCKER_NET:-dockers_devnet}"

ES_IMAGE="${ES_IMAGE:-swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/elasticsearch:7.6.2}"

# 已由其它项目复用的在跑底座，此处只做就绪提示，不负责启停
readonly REUSED="consul:8500 mongo1:27018 keydb:6380 memcached:11211"

need_docker() {
  if [[ ! -x "$DOCKER" ]]; then
    echo "ERROR: docker CLI 不可执行：$DOCKER" >&2
    echo "       Docker Desktop 的 CLI 不在 PATH，请用 DOCKER_BIN=<绝对路径> 指定。" >&2
    exit 1
  fi
  "$DOCKER" info >/dev/null 2>&1 || { echo "ERROR: docker daemon 未运行" >&2; exit 1; }
}

port_open() { nc -z -G2 127.0.0.1 "$1" >/dev/null 2>&1; }

wait_port() { # <port> <label> <seconds>
  local port="$1" label="$2" limit="${3:-60}" i=0
  while (( i < limit )); do
    port_open "$port" && { echo "  ✓ $label ($port) 就绪"; return 0; }
    sleep 2; i=$((i + 2))
  done
  echo "  ✗ $label ($port) 超时未就绪" >&2; return 1
}

ensure_running() { # <container>
  local name="$1"
  if "$DOCKER" inspect "$name" >/dev/null 2>&1; then
    local st; st="$("$DOCKER" inspect -f '{{.State.Running}}' "$name")"
    if [[ "$st" == "true" ]]; then echo "  = $name 已在运行"; else
      echo "  → 启动已存在的 $name"; "$DOCKER" start "$name" >/dev/null
    fi
  else
    echo "  ! 容器 $name 不存在，需手工创建（见 README）" >&2; return 1
  fi
}

ensure_es() {
  if "$DOCKER" inspect elasticsearch >/dev/null 2>&1; then
    ensure_running elasticsearch; return
  fi
  echo "  + 新建 elasticsearch（本机原无此容器）"
  "$DOCKER" run -d --name elasticsearch --network "$NET" \
    -p 9200:9200 -p 9300:9300 \
    -e "discovery.type=single-node" \
    -e "ES_JAVA_OPTS=-Xms512m -Xmx512m" \
    "$ES_IMAGE" >/dev/null
}

cmd_start() {
  need_docker
  echo "== 启动 trace 验收底座 =="
  ensure_running jaeger
  ensure_running mysql
  ensure_running kafka
  ensure_es
  echo "-- 等待就绪 --"
  wait_port 16686 "Jaeger UI"      60
  wait_port 4318  "Jaeger OTLP/HTTP" 60
  wait_port 3306  "MySQL"          90
  wait_port 9092  "Kafka"          90
  wait_port 9200  "Elasticsearch"  120
  echo "-- 复用底座（应已在跑）：$REUSED --"
  echo "完成。下一步：$0 check"
}

cmd_check() {
  need_docker
  local rc=0
  echo "== 底座健康检查 =="
  check() { # <port> <label>
    if port_open "$1"; then echo "  ✓ $2 ($1)"; else echo "  ✗ $2 ($1) 不可达" >&2; rc=1; fi
  }
  check 16686 "Jaeger UI"
  check 4318  "Jaeger OTLP/HTTP"
  check 3306  "MySQL"
  check 9092  "Kafka"
  check 9200  "Elasticsearch"

  # Jaeger 的 OTLP 接收必须开启，否则 span 静默丢弃
  local enabled
  enabled="$("$DOCKER" inspect jaeger --format '{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null \
    | grep -c 'COLLECTOR_OTLP_ENABLED=true' || true)"
  if [[ "${enabled:-0}" != "0" ]]; then echo "  ✓ Jaeger COLLECTOR_OTLP_ENABLED=true"
  else echo "  ✗ Jaeger 未开启 OTLP 接收，span 会被静默丢弃" >&2; rc=1; fi

  # Elasticsearch 需 green/yellow 才算可写入
  local health
  health="$(curl -s --max-time 8 http://127.0.0.1:9200/_cluster/health \
    | grep -o '"status":"[a-z]*"' | head -1 || true)"
  if [[ "$health" == '"status":"green"' || "$health" == '"status":"yellow"' ]]; then
    echo "  ✓ Elasticsearch cluster ${health}"
  else echo "  ✗ Elasticsearch 集群状态异常：${health:-无响应}" >&2; rc=1; fi

  (( rc == 0 )) && echo "== check 全绿 ==" || echo "== check 失败 ==" >&2
  return "$rc"
}

cmd_status() {
  need_docker
  "$DOCKER" ps -a --filter name=jaeger --filter name=mysql --filter name=kafka \
    --filter name=elasticsearch --filter name=consul --filter name=mongo1 \
    --format '{{.Names}}\t{{.Status}}' | sed 's/\t/ | /'
}

cmd_stop() {
  need_docker
  echo "== 停止 trace 验收底座（保留数据）=="
  for c in elasticsearch kafka mysql jaeger; do
    "$DOCKER" inspect "$c" >/dev/null 2>&1 && { echo "  - $c"; "$DOCKER" stop "$c" >/dev/null; }
  done
}

case "${1:-status}" in
  start)  cmd_start ;;
  check)  cmd_check ;;
  status) cmd_status ;;
  stop)   cmd_stop ;;
  *) echo "用法: $0 {start|check|status|stop}" >&2; exit 2 ;;
esac
