#!/usr/bin/env bash
# 全链路驱动脚本：gateway(8080) → bff(8081) → 四域服(9021-9024)
#
# 为什么是 curl 打网关而不是写 kitex 客户端：gateway 与 bff 都是 HTTP 面，
# 从网关进才叫「全链路」——少了 gateway.proxy 与 otelhttp CLIENT 那两跳，
# 证据里就只有 bff→srv 半条链。各域服是 kitex 私有协议，只能由 bff 代打。
#
# 用法：
#   bash examples/micro/scripts/chain.sh            # 跑全部路径
#   bash examples/micro/scripts/chain.sh --compensate   # 打补偿路径（fail_inject 点火，见下）
#
# ⚠️ --compensate 已改口径（2026-10-13，dtm Saga 上线）：下游单链是 bff 手动编排 +
#   手工 ReleaseStock 补偿；现在改由 dtm Saga 驱动、逆序补偿由 dtm 发起（见 README
#   §1bis）。故本模式不再「先停 srv-order」，改为由 fail_inject 点火——停服注入的是
#   **基础设施故障**，走的是 dtm 的重试路径，产不出补偿段。
#
# 前置：consul / jaeger / mysql / redis / kafka 已起，六个进程已起（见 README §5）。

set -u

GATEWAY="${GATEWAY:-http://127.0.0.1:8080}"
USER_ID="${USER_ID:-1001}"
PRODUCT_ID="${PRODUCT_ID:-1}"
# 边界鉴权凭据：bff 是唯一验凭据的地方，网关只透传 Authorization 头。
# 令牌由 bff 在启动时按 demo.auth_token 播种进缓存，故这里必须与那份配置一致。
# 想验"没凭据会被挡"时显式置空：DEMO_TOKEN="" bash ...
DEMO_TOKEN="${DEMO_TOKEN-demo-token}"
CURL=(curl -s --noproxy '*' -m 15)
if [ -n "$DEMO_TOKEN" ]; then
  CURL+=(-H "Authorization: Bearer $DEMO_TOKEN")
fi

step() { printf '\n=== %s ===\n' "$1"; }

get() { "${CURL[@]}" -w '\nHTTP %{http_code}\n' "$GATEWAY$1"; }

post() { "${CURL[@]}" -X POST -H 'Content-Type: application/json' -d "$2" -w '\nHTTP %{http_code}\n' "$GATEWAY$1"; }

if [ "${1:-}" = "--compensate" ]; then
  step "补偿路径：fail_inject 点火 points 分支 → dtm 逆序补偿（见 README §1bis）"
  post /api/order "{\"user_id\":$USER_ID,\"product_id\":$PRODUCT_ID,\"quantity\":1,\"fail_inject\":\"Debit\"}"
  exit 0
fi

step "① /api/ping —— 只到 bff，不触达下游（下游挂了也必须 200）"
get /api/ping

step "② /api/product/$PRODUCT_ID —— 读路径（首次缓存未命中：db 多表 JOIN + cache 回填）"
get "/api/product/$PRODUCT_ID?user_id=$USER_ID"

step "③ /api/product/$PRODUCT_ID —— 同一商品再读一次（缓存命中：应只见 db.redis.GET，无 db.query）"
get "/api/product/$PRODUCT_ID?user_id=$USER_ID"

step "④ /api/stock/$PRODUCT_ID —— 库存快照（2 表聚合）"
get "/api/stock/$PRODUCT_ID"

step "⑤ POST /api/order —— 复合编排：商品 → 库存 → 订单 → 积分 串进同一根 trace"
ORDER_JSON=$(post /api/order "{\"user_id\":$USER_ID,\"product_id\":$PRODUCT_ID,\"quantity\":1}")
echo "$ORDER_JSON"
ORDER_ID=$(printf '%s' "$ORDER_JSON" | sed -n 's/.*"order_id":\([0-9]*\).*/\1/p' | head -1)
echo "解析出 order_id=${ORDER_ID:-<未解析>}"

if [ -n "${ORDER_ID:-}" ]; then
  step "⑥ /api/order/$ORDER_ID —— 回读刚下的单（2 表：orders ⋈ order_items）"
  get "/api/order/$ORDER_ID?user_id=$USER_ID"
fi

step "⑦ /api/points/$USER_ID —— 积分账户（2 表聚合）"
get "/api/points/$USER_ID"

step "⑧ 不存在的商品 —— 失败路径（400/404，链路上应见到 domain 错误标记而非 RPC 错误）"
get "/api/product/99999999?user_id=$USER_ID"

printf '\n=== 完成 ===\n'
printf '取本次链路（gateway 根 span）：\n'
printf '  curl -s --noproxy "*" "http://127.0.0.1:16686/api/traces?service=demo-gateway&limit=20" > /tmp/micro-traces.json\n'
