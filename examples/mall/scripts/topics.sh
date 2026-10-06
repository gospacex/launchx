#!/usr/bin/env bash
# ============================================================
# mall 示例 · 四个事件 topic 的建Topic脚本
#
# 为什么需要：compose 的 kafka 虽然开了 KAFKA_CFG_AUTO_CREATE_TOPICS_ENABLE，
# 但自动建 Topic 与首次 Send 是**竞态**的 —— 实测 sarama 生产者第一次
# 投递就撞 "Unknown Topic Or Partition"，等自动建好再重试才成功。
# 而 outbox Runner 的重试上限是 10 次 × 3s，攒够就 abandon，事件从此
# 卡在 status=0（表面上"下单成功"，实际事件永远没出域）。
#
# 用法（本机 compose 的 kafka 容器，bitnami 镜像自带 kafka-topics.sh）：
#   bash examples/mall/scripts/topics.sh
#   bash examples/mall/scripts/topics.sh --list
# ============================================================
set -euo pipefail

CONTAINER="${KAFKA_CONTAINER:-kafka}"
BOOTSTRAP="${KAFKA_BOOTSTRAP:-localhost:9092}"
KT="/opt/bitnami/kafka/bin/kafka-topics.sh"

TOPICS=(
  product.created.v1
  order.completed.v1
  stock.reserved.v1
  points.changed.v1
)

if [[ "${1:-}" == "--list" ]]; then
  docker exec "$CONTAINER" bash -c "$KT --bootstrap-server $BOOTSTRAP --list"
  exit 0
fi

for t in "${TOPICS[@]}"; do
  docker exec "$CONTAINER" bash -c \
    "$KT --bootstrap-server $BOOTSTRAP --create --if-not-exists --topic $t --partitions 1 --replication-factor 1" \
    >/dev/null
  echo "topic ready: $t"
done

echo "--- 当前 topic ---"
docker exec "$CONTAINER" bash -c "$KT --bootstrap-server $BOOTSTRAP --list"
