#!/usr/bin/env bash
# ============================================================
# DTM 三段验收：Saga 正向 / Saga 补偿 / TCC 二阶段（tasks.md 4.3）
#
# 与 scripts/e2e.sh 的分工：
#   e2e.sh     = 原六进程复合链路断言（既有资产，本脚本不回退它）
#   dtm_e2e.sh = dtm 事务链专属三段。判据含「跨进程最终一致」与 dtm 侧事务状态，
#                这两样在 go test 里要么起不来 dtm、要么拿不到跨段现场；而
#                dtm_e2e_test.go 覆盖同样的判据时，失败的现场只能看 -v 日志。
#
# 用法：
#   bash examples/micro/scripts/dtm_e2e.sh                  # 自起六进程 + 三段断言
#   DTM_E2E_REUSE=1 bash examples/micro/scripts/dtm_e2e.sh  # 复用已在跑的进程，只做断言
#
# 前置：
#   export MYSQL_ROOT_PW=<mysql root 口令>   # 四库对账 + dtm_db.trans_global
#   export HUBX_DB_PW=<应用账号 mall_rw 口令> # 起六进程时注入 config.yaml 的 ${HUBX_DB_PW}
#   dtm server 起在 127.0.0.1:36789(http) / 127.0.0.1:36790(grpc)，存储库 dtm_db
#
# 退出码：0 = 三段全绿；2 = 前置缺失；1 = 某段判据失败（打印第几段）。
#
# 三条纪律（本脚本每处写法都是它们的直接后果）：
#
#  1. **变量后面紧跟中文必须写 ${name}**。macOS 自带 bash 3.2 不做多字节识别：
#     写成「$name 紧跟中文」会把变量名并进后面那些字节里 —— set -u 下报
#     "unbound variable" 且名字带乱码，不带 -u 则静默展开成空串。两种情况都不
#     指向真正的原因（e2e.sh 的文件头记着同一个坑）。
#  2. **没有 || true**。结论只能由退出码给出；把失败吞掉再往下跑，得到的是
#     「三段全绿」而底座根本没起 —— 在验收脚本里这是最贵的一种假通过。
#     需要容错的地方一律写成 if 显式判分支，且把容错本身也报出来。
#  3. **口令一律从环境取**，缺省即报错退出（码 2）。脚本进了仓库，写死在里面的
#     口令就等于公开；而「悄悄用默认口令连上」只会让真正去改口令的人发现改了
#     也没生效。
# ============================================================
set -euo pipefail

MICRO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO_BIN="${GO_BIN:-go}"
SCHEMA_MICRO_SQL="$MICRO_DIR/scripts/schema-micro.sql"
# 灌四库 DDL + 建应用账号的唯一入口（渲染占位 + 灌完用应用账号真连一次）。
# 本脚本不灌它，但要自己核查它灌出来的结果，见 require_base 里的 check。
SCHEMA_SH="$MICRO_DIR/../../examples/scripts/schema.sh"

GATEWAY="${MICRO_GATEWAY:-http://127.0.0.1:8080}"
JAEGER="${MICRO_JAEGER:-http://127.0.0.1:16686}"
DTM_HOST="${MICRO_DTM_HOST:-127.0.0.1}"
DTM_GRPC_PORT="${MICRO_DTM:-36790}"
DTM_DB="${MICRO_DTM_DB:-dtm_db}"
USER_ID="${MICRO_USER_ID:-1001}"
PRODUCT_ID="${MICRO_PRODUCT_ID:-1}"
QUANTITY="${MICRO_QUANTITY:-1}"
TOKEN="${MICRO_TOKEN-demo-token}"

# 终态集合。**不含 aborting**：它是 TCC Try 失败后的中间态（dtm 已决定回滚、
# 正在逆序 Cancel），当成终态会让判据在冻结量还没退干净时就通过 —— 实测报出
# 「Cancel 只撤了一半」，而那半截是根本没撤。
readonly FINAL_STATUS_RE='^(succeed|failed|rollbacked)$'

WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/micro-dtm-e2e.XXXXXX")"
STARTED=()
SEGMENT="前置"
FAILED=0

log()  { printf '\033[36m[dtm-e2e]\033[0m %s\n' "$*"; }
fail() { printf '\033[31m[dtm-e2e] FAIL\033[0m %s\n' "$*" >&2; }

cleanup() {
  local rc=$?
  trap - EXIT
  if [ ${#STARTED[@]} -gt 0 ]; then
    log "清理 ${#STARTED[@]} 个本脚本拉起的进程"
    for pid in "${STARTED[@]}"; do kill "$pid" 2>/dev/null; done
  fi
  if [ "${rc}" -ne 0 ]; then
    fail "退出码 ${rc}；进程日志在 ${WORKDIR}/log/"
  else
    rm -rf "$WORKDIR"
  fi
  exit "${rc}"
}
trap cleanup EXIT

# assert_eq —— Want/Observed 分离，退出码即结论。
# 不相等时记红但**继续**（让三段都跑完再汇总）；相等才打印 PASS。
assert_eq() {
  local name="${1}" want="${2}" got="${3}"
  if [ "${want}" = "${got}" ]; then
    printf '  \033[32mPASS\033[0m  %s\n' "${name}"
    return 0
  fi
  printf '  \033[31mFAIL\033[0m  %s\n        Want:     %s\n        Observed: %s\n' \
    "${name}" "${want}" "${got}"
  FAILED=1
  return 0
}

segment() { SEGMENT="${1}"; printf '\n=== 段 %s ===\n' "${SEGMENT}"; }

# jget 从一段响应体里取字段；体不是合法 JSON 时返回空串而**不**中止脚本。
#
# 为什么必须这样：端点 404 时响应体是纯文本 "Not Found"，jq 会以退出码 4 挂掉，
# 而 set -e 会让整份验收在那一句上死掉 —— 报出来的结论变成「jq 解析失败」，
# 把真正的问题（路由没注册）藏了起来。
jget() {
  local body="${1}" expr="${2}" out
  out="$(printf '%s' "${body}" | jq -r "${expr}" 2>/dev/null)"
  if [ $? -ne 0 ]; then
    printf ''
    return 0
  fi
  printf '%s' "${out}"
  return 0
}

# wait_dtm_quiet 等 dtm 里没有未终结的全局事务。
#
# 四库对账全靠「与基线逐项比对」，而基线只在没有别人在动账时才成立：上一段跑剩的
# 事务在补偿途中落库会把基线打歪，判据报出一个与本段提交无关的差值。实测报成
# 「Cancel 只撤了一半」—— 而那一半是上一段留下的。假红比不判更坏。
wait_dtm_quiet() {
  local i n
  for ((i = 0; i < 60; i++)); do
    n="$(q "SELECT COUNT(*) FROM ${DTM_DB}.trans_global WHERE status NOT IN ('succeed','failed','rollbacked')")"
    if [ "${n}" -eq 0 ]; then return 0; fi
    sleep 1
  done
  fail "dtm 静默超时：仍有 ${n} 条未终结的全局事务（dtm server 可能没在推进）"
  return 1
}

port_open() { (exec 3<>"/dev/tcp/${1}/${2}") 2>/dev/null; }

wait_port() {
  local port="${1}" timeout="${2:-40}" i
  for ((i = 0; i < timeout; i++)); do
    if port_open 127.0.0.1 "${port}"; then return 0; fi
    sleep 1
  done
  return 1
}

# ---------------------------------------------------------------- MySQL
# mysql_exec 走本机 mysql 客户端。密码走 MYSQL_PWD 环境变量而不是 -p<pw>：
# 命令行参数在 ps 里对同机其他进程可见。
mysql_exec() {
  local pw="${MYSQL_ROOT_PW:?请设置 MYSQL_ROOT_PW（mysql root 口令）}"
  if ! command -v mysql >/dev/null 2>&1; then
    fail "本机没有 mysql 客户端，本脚本的四库对账需要它"
    return 1
  fi
  MYSQL_PWD="${pw}" mysql -h127.0.0.1 -P3306 -uroot -N -B "$@"
}

# q 查一个标量。查询失败即非零返回 —— 不吞，吞了就是把「没查到」读成「查到 0」。
q() { mysql_exec -e "${1}"; }

# ---------------------------------------------------------------- HTTP
# api_post <base> <path> <body> 打一次 POST，返回 "<HTTP码>\n<响应体>"。
# 码非 0 也算成功返回 —— 4xx/5xx 是判据要读的值，不是脚本的失败。
#
# base 是参数而不是写死 ${GATEWAY}：TCC 端点的基址可由 MICRO_TCC_BASE 覆盖
# （排障用），两段用同一个函数才不会各自漂一份 curl 参数。
api_post() {
  local base="${1}" path="${2}" body="${3}" out code payload
  out="$(curl -sS --noproxy '*' -m 30 -w '\n%{http_code}' -X POST "${base}${path}" \
    -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' -d "${body}")"
  code="$(printf '%s' "${out}" | tail -n 1)"
  payload="$(printf '%s' "${out}" | sed '$d')"
  printf '%s\n%s' "${code}" "${payload}"
}

# wait_trans_status 等一条全局事务到终态，打印终态名。超窗打印最后读到的值。
wait_trans_status() {
  local gid="${1}" window="${2}" st i
  for ((i = 0; i < window; i++)); do
    st="$(q "SELECT status FROM ${DTM_DB}.trans_global WHERE gid = '${gid}'")"
    if printf '%s' "${st}" | grep -Eq "${FINAL_STATUS_RE}"; then
      printf '%s' "${st}"
      return 0
    fi
    sleep 1
  done
  printf '%s' "${st:-none}"
  return 0
}

# ---------------------------------------------------------------- Jaeger
# jaeger_get 拿一条 trace 的 JSON；trace 不存在时返回空串（**不是**错误）。
jaeger_get() {
  local trace_id="${1}" body
  body="$(curl -sS --noproxy '*' -m 15 "${JAEGER}/api/traces/${trace_id}")"
  if printf '%s' "${body}" | jq -e '.data | length > 0' >/dev/null 2>&1; then
    printf '%s' "${body}"
    return 0
  fi
  printf ''
  return 0
}

# trace_ids_for_gid 列出带该 biz.dtm.gid 的 traceID（去重，一行一个）。
#
# 为什么不用 ?tags= 过滤：Jaeger 的 tag 检索依赖后端实现，本机 jaeger-query 1.63 +
# ES 上按 tags 过滤并不生效（internal/traceutil 的包注释记着同源的 lookback 不可信
# 问题）。故一律「按服务与时间窗拉回来、在客户端筛」，口径与 dtm_e2e_test.go 一致。
#
# 为什么多轮 + 最后才去重：OTLP 是分批抵达的，第一轮常常只有根 span 那一批；
# 而若每轮各自去重再累加，同一条 trace 会被数成好几条 —— 那正是「这批 trace
# 里恰好有一条」的判据最容易被自己弄红的方式。
trace_ids_for_gid() {
  local gid="${1}" rounds="${2:-8}" end_us since_us i raw all
  end_us=$(( $(date +%s) * 1000000 + 60000000 ))
  since_us=$(( end_us - 900 * 1000000 ))
  all=""
  for ((i = 0; i < rounds; i++)); do
    raw="$(curl -sS --noproxy '*' -m 20 \
      "${JAEGER}/api/traces?service=demo-bff&limit=100&start=${since_us}&end=${end_us}" \
      | jq -r --arg g "${gid}" '
          [ .data[]
            | select(any(.spans[]?; any(.tags[]?; .key == "biz.dtm.gid" and .value == $g)))
            | .traceID ] | .[]')"
    all="${all}
${raw}"
    sleep 1
  done
  printf '%s' "${all}" | awk 'NF' | sort -u
}

# spans_of 打印一条 trace 的 "服务|operation|gid|compensation" 四元组。
spans_of() {
  local body="${1}"
  printf '%s' "${body}" | jq -r '
    .data[0] as $t
    | $t.spans[]
    | [ ($t.processes[.processID].serviceName // "?"),
        .operationName,
        ((.tags // []) | map(select(.key=="biz.dtm.gid") | .value) | first // ""),
        ((.tags // []) | map(select(.key=="biz.compensation") | (.value|tostring)) | first // "") ]
    | join("|")'
}

# ---------------------------------------------------------------- 前置
require_prereq() {
  local -a pairs=("mysql:3306" "jaeger:16686" "dtm:${DTM_GRPC_PORT}" \
                  "product分支面:9041" "order分支面:9042" "stock分支面:9043" "points分支面:9044" \
                  "gateway:8080" "bff:8081")
  local missing=() name port pair
  for pair in "${pairs[@]}"; do
    name="${pair%%:*}"; port="${pair##*:}"
    if ! port_open 127.0.0.1 "${port}"; then missing+=("${name}(${port})"); fi
  done
  if [ ${#missing[@]} -ne 0 ]; then
    fail "缺失前置：${missing[*]}"
    log "起法：底座 docker compose -f /Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml up -d consul jaeger mysql redis kafka；"
    log "      dtm server 起在 ${DTM_HOST}:${DTM_GRPC_PORT}（存储库 ${DTM_DB}）；"
    log "      六进程用 bash examples/micro/scripts/e2e.sh 起，或 DTM_E2E_REUSE=1 复用已在跑的。"
    return 1
  fi
  if [ -z "${MYSQL_ROOT_PW:-}" ]; then
    fail "缺 MYSQL_ROOT_PW（四库对账与 ${DTM_DB}.trans_global 都要 root；口令一律从环境取）"
    return 1
  fi
  if [ -z "${HUBX_DB_PW:-}" ]; then
    fail "缺 HUBX_DB_PW（起六进程时要注入各域 config.yaml 的 \${HUBX_DB_PW}）"
    return 1
  fi
  log "前置就绪：${pairs[*]} / dtm ${DTM_HOST}:${DTM_GRPC_PORT}"
  return 0
}

seed() {
  # ① 四库基础 DDL **不**由本脚本灌，归 examples/scripts/schema.sh。
  #    那份 SQL 里应用账号的口令是 shell 占位：不先渲染就 `<` 进 mysql，
  #    口令会被设成字面量，而此刻六进程正在用它连库 —— 一次自伤，且症状
  #    要等到下次重连才暴露。所以这是"谁灌谁负责渲染"的活，本脚本不碰。
  #
  # ② 但"上游处理过了"这句话本身不能当作证据 —— 本脚本要求的四张基础表
  #    是用 **root** 查的（见 require_base），root 说什么都 PASS，它对
  #    "应用账号连不连得上"这个问题没有发言权。故这里先用应用账号 check 一次，
  #    把锅甩回正确的那一方：`bash examples/scripts/schema.sh check`。
  #    这一步的成本是一秒，替代的是一小时 1045 排障。
  if [ -x "$SCHEMA_SH" ]; then
    MYSQL_ROOT_PW="${MYSQL_ROOT_PW:-}" HUBX_DB_PW="${HUBX_DB_PW:-}" bash "$SCHEMA_SH" check || {
      fail "应用账号连不上库：先跑 bash examples/scripts/schema.sh apply 修回来"
      return 1
    }
  else
    fail "找不到 ${SCHEMA_SH}（四库与应用账号的唯一入口）"
    return 1
  fi
  local base_missing=0 t
  for t in stock_db.stocks product_db.product_sales points_db.point_accounts \
           order_db.orders order_db.outbox stock_db.barrier; do
    if [ "$(q "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = SUBSTRING_INDEX('${t}', '.', 1) AND table_name = SUBSTRING_INDEX('${t}', '.', -1)")" -eq 0 ]; then
      fail "表 ${t} 不存在：先跑 bash examples/micro/scripts/e2e.sh（四库 DDL + 应用账号），再跑本脚本"
      base_missing=1
    fi
  done
  if [ "${base_missing}" -ne 0 ]; then
    return 1
  fi

  log "种子：dtm 增量 DDL（barrier 表 / 冻结列 / 销量表 / dtm_gid，幂等）"
  mysql_exec <"${SCHEMA_MICRO_SQL}" >/dev/null
  # 积分账户：Saga 第四分支（points.Debit）的前置。缺它 lockAccount 直接判业务
  # 失败，于是每一笔下单都去补偿 —— 而症状长得像「Saga 坏了」。
  q "INSERT INTO points_db.point_accounts (user_id, balance, total_earned, total_spent)
     VALUES (${USER_ID}, 1000, 1000, 0)
     ON DUPLICATE KEY UPDATE balance = IF(balance < 100, 1000, balance)" >/dev/null
  log "积分账户就绪：user_id=${USER_ID} balance=$(q "SELECT balance FROM points_db.point_accounts WHERE user_id = ${USER_ID}")"
}

start_stack() {
  mkdir -p "${WORKDIR}/log" "${WORKDIR}/bin"
  local spec port rest dir bin
  export CGO_ENABLED=0
  for spec in "srv-product:9021" "srv-order:9022" \
              "srv-stock:9023" "srv-points:9024" \
              "bff:8081" "gateway:8080"; do
    dir="${spec%%:*}"; rest="${spec#*:}"; port="${rest##*:}"
    if port_open 127.0.0.1 "${port}"; then
      log "复用已在跑的 ${dir}（${port} 已被监听）"
      continue
    fi
    if [ "${DTM_E2E_REUSE:-0}" = "1" ]; then
      fail "${dir} 的端口 ${port} 没被监听，而 DTM_E2E_REUSE=1 要求复用现成进程"
      return 1
    fi
    bin="${WORKDIR}/bin/${dir}"
    ( cd "${MICRO_DIR}/${dir}" && "${GO_BIN}" build -o "${bin}" ./cmd ) >"${WORKDIR}/log/build-${dir}.log" 2>&1
    ( cd "${MICRO_DIR}/${dir}" && exec "${bin}" ) >"${WORKDIR}/log/${dir}.log" 2>&1 &
    STARTED+=("$!")
    if ! wait_port "${port}" 60; then
      fail "${dir} 在 60s 内没有监听 ${port}"
      return 1
    fi
    log "已起 ${dir}（${port}）"
  done
  return 0
}

# four_db 快照（与 dtm_e2e_test.go 的 fourDB 同一口径，逐项比而不是比一个总数）。
#
# **不含 outbox**：Saga 补偿后 outbox 表**不会**回到基线 —— CreateOrder 写下单事件，
# CancelOrder 把订单置 canceled，但不回收那一行事件（tasks.md 2.3 的口径是「作废补偿
# **不重复**落 outbox」，不是「把已落的那行删掉」）。把 outbox 塞进回基线判据，
# 会把这条既定口径报成一条缺陷；outbox 单列断言，且判据是「不新增第二行」。
four_db() {
  q "SELECT CONCAT_WS('|',
        (SELECT CONCAT(total,'/',available,'/',frozen) FROM stock_db.stocks WHERE product_id = ${PRODUCT_ID}),
        (SELECT COALESCE(sold,0) FROM product_db.product_sales WHERE product_id = ${PRODUCT_ID}),
        (SELECT CONCAT(balance,'/',frozen,'/',total_spent) FROM points_db.point_accounts WHERE user_id = ${USER_ID}))"
}

# outbox_rows 读 order_db.outbox 行数。
outbox_rows() { q 'SELECT COUNT(*) FROM order_db.outbox'; }

# wait_outbox_delta 等 outbox 行数相对 baseline 到达期望增量（默认 30s 窗口）。
#
# 为什么要轮询而不能读一次就判：这条链是 bff → dtm server → 四域分支面 → MySQL
# 六个进程，outbox 行落在 order.CreateOrder 的事务里，而 dtm 把全局事务置 succeed
# 与四域分支返回之间还有一段异步。读一次判增减，等于把「谁的时序」写进了判据 ——
# 实测偶发读到事务已 succeed 而 outbox 尚未可见。轮询把时序差异关在窗口里，
# 而窗口超时仍会报红（真有缺陷时它照样红，只是给了 30s 的机会）。
wait_outbox_delta() {
  local before="${1}" want_delta="${2}" window="${3:-30}" i now
  for ((i = 0; i < window; i++)); do
    now="$(outbox_rows)"
    if [ "$(( now - before ))" -eq "${want_delta}" ]; then
      printf '%s' "${now}"
      return 0
    fi
    sleep 1
  done
  printf '%s' "$(outbox_rows)"
  return 0
}

# ---------------------------------------------------------------- 主流程
# 前置 / 种子 / 起进程三步任何一步不成立都退出码 **2**，与「判据失败(1)」分开：
# 「底座没起」与「底座起了但判据没过」是两种结论，混成一个码就等于让排障先猜环境。
if ! require_prereq; then
  fail "前置缺失（退出码 2 = 环境未就绪，不是验收失败）"
  exit 2
fi
if ! seed; then
  fail "种子未完成（退出码 2）"
  exit 2
fi
if ! start_stack; then
  fail "六进程未全部就绪（退出码 2）"
  exit 2
fi

# ---------------------------------------------------------------- 段 1
# Saga 正向：一次 POST /api/order 在 Jaeger 上是**一条** trace，四库各扣一次。
#
# 反向探针：用一枚从未提交过的 gid 去检索，期望召回 0 条。
# 没有它，「按 gid 召回 1 条」这条判据可能是恒真的（例如 gid 取值写错导致
# 两边都查不到任何东西，而 0 条又被别处的过滤吃掉）。
segment "1/3 Saga 正向"

wait_dtm_quiet
BASE1="$(four_db)"
OUTBOX_BEFORE_1="$(outbox_rows)"
log "提交前四库：${BASE1}"

read_out="$(api_post "${GATEWAY}" /api/order \
  "{\"user_id\":${USER_ID},\"product_id\":${PRODUCT_ID},\"quantity\":${QUANTITY}}")"
CODE1="$(printf '%s' "${read_out}" | head -n 1)"
BODY1="$(printf '%s' "${read_out}" | sed -n '2,$p')"
assert_eq "POST /api/order 提交" "200" "${CODE1}"
GID1="$(jget "${BODY1}" '.dtm_gid // ""')"
ORDER1="$(jget "${BODY1}" '.order_no // ""')"
assert_eq "响应携带 dtm_gid（按业务号查链的唯一键）" "yes" "$([ -n "${GID1}" ] && printf yes || printf no)"
log "gid=${GID1} order_no=${ORDER1}"
assert_eq "dtm 侧全局事务终态" "succeed" "$(wait_trans_status "${GID1}" 40)"
AFTER1="$(four_db)"
log "提交后四库：${AFTER1}"
assert_eq "库存净减（total/available 各减 ${QUANTITY}，frozen 不动）" \
  "$(printf '%s' "${BASE1}" | cut -d'|' -f1 | awk -F/ -v q="${QUANTITY}" '{print $1-q"/"$2-q"/"$3}')" \
  "$(printf '%s' "${AFTER1}" | cut -d'|' -f1)"
assert_eq "销量 +${QUANTITY}" \
  "$(( $(printf '%s' "${BASE1}" | cut -d'|' -f2) + QUANTITY ))" \
  "$(printf '%s' "${AFTER1}" | cut -d'|' -f2)"
assert_eq "outbox +1（订单事件在 order.CreateOrder 落库分支里写）" \
  "$(( OUTBOX_BEFORE_1 + 1 ))" "$(wait_outbox_delta "${OUTBOX_BEFORE_1}" 1)"

TIDS1="$(trace_ids_for_gid "${GID1}" 8)"
NTRACE1="$(printf '%s\n' "${TIDS1}" | awk 'NF{n++} END{print n+0}')"
assert_eq "该 gid 在 Jaeger 上只对应 1 条 trace（补偿未发生，四域不另起根）" "1" "${NTRACE1}"
TRACE1="$(printf '%s\n' "${TIDS1}" | grep -m1 .)"
if [ -z "${TRACE1}" ]; then
  fail "没取到 trace —— 段 1 的 trace 判据无法执行"
else
  SPANS1="$(spans_of "$(jaeger_get "${TRACE1}")")"
  log "trace ${TRACE1} span 清单（rpc.dtm.*）："
  printf '%s\n' "${SPANS1}" | grep 'rpc\.dtm\.' | sed 's/^/    /'
  for want in rpc.dtm.stock.Deduct rpc.dtm.order.CreateOrder rpc.dtm.product.AddSales rpc.dtm.points.Debit; do
    assert_eq "正向 trace 含 ${want}" "yes" \
      "$(printf '%s\n' "${SPANS1}" | grep -q "|${want}|" && printf yes || printf no)"
  done
  assert_eq "正向 trace 含 service.* 分层 span" "yes" \
    "$(printf '%s\n' "${SPANS1}" | awk -F'|' '$2 ~ /^service\./ {found=1} END {print found ? "yes" : "no"}')"
  assert_eq "正向 trace 含 db.* 分层 span" "yes" \
    "$(printf '%s\n' "${SPANS1}" | awk -F'|' '$2 ~ /^db\./ {found=1} END {print found ? "yes" : "no"}')"
  assert_eq "根 span 恰 1 个" "1" \
    "$(printf '%s' "$(jaeger_get "${TRACE1}")" | jq '[.data[0] as $t | $t.spans[] | select((.references // []) | length == 0)] | length')"
fi
assert_eq "反向探针：伪造 gid 召回 0 条 trace" "0" \
  "$(trace_ids_for_gid 'deadbeefdeadbeefdeadbeef' 3 | awk 'NF{n++} END{print n+0}')"

# ---------------------------------------------------------------- 段 2
# Saga 补偿：fail_inject 点火最后一域 → dtm 逆序补偿 → 四库回基线，且补偿段与
# 正向段落在**同一条 traceID** 上（真机实测：dtm 按 step 共享载荷，补偿分支拿到
# 同一份 traceparent，故挂回正向那条链）。
#
# 反向探针：伪造 gid 的补偿段 span 数必须是 0。
segment "2/3 Saga 补偿"

wait_dtm_quiet
BASE2="$(four_db)"
OUTBOX_BEFORE_2="$(outbox_rows)"
log "注入前四库：${BASE2}，outbox=${OUTBOX_BEFORE_2} 行"
read_out="$(api_post "${GATEWAY}" /api/order \
  "{\"user_id\":${USER_ID},\"product_id\":${PRODUCT_ID},\"quantity\":${QUANTITY},\"fail_inject\":\"Debit\"}")"
CODE2="$(printf '%s' "${read_out}" | head -n 1)"
BODY2="$(printf '%s' "${read_out}" | sed -n '2,$p')"
assert_eq "POST /api/order(fail_inject=Debit) 提交" "200" "${CODE2}"
GID2="$(jget "${BODY2}" '.dtm_gid // ""')"
ORDER2="$(jget "${BODY2}" '.order_no // ""')"
log "gid=${GID2} order_no=${ORDER2}"
assert_eq "dtm 侧全局事务终态" "failed" "$(wait_trans_status "${GID2}" 60)"

# 四库回基线：dtm 的补偿是异步的，给它一段回补窗口再逐项比。
i=0; AFTER2=""
while [ "${i}" -lt 30 ]; do
  AFTER2="$(four_db)"
  if [ "${AFTER2}" = "${BASE2}" ]; then break; fi
  sleep 2; i=$(( i + 1 ))
done
log "补偿后四库：${AFTER2}"
assert_eq "四库账变逐项回基线（total/available/frozen、sold、balance/frozen/spent）" "${BASE2}" "${AFTER2}"
# Saga 补偿不回收到基线的只有 outbox：CreateOrder 写的那一行仍在，而 CancelOrder
# **不再写第二行**（tasks.md 2.3「作废补偿不重复落 outbox」）。这两条放一起才是口径。
assert_eq "作废补偿不重复落 outbox：只多 CreateOrder 写的那 1 行" \
  "$(( OUTBOX_BEFORE_2 + 1 ))" "$(wait_outbox_delta "${OUTBOX_BEFORE_2}" 1)"
assert_eq "被作废的那张单没有 order.completed 第二行" "1" \
  "$(q "SELECT COUNT(*) FROM order_db.outbox WHERE payload LIKE '%${ORDER2}%'")"

TIDS2="$(trace_ids_for_gid "${GID2}" 10)"
NTRACE2="$(printf '%s\n' "${TIDS2}" | awk 'NF{n++} END{print n+0}')"
assert_eq "补偿与正向同一条 traceID（gid 只召回 1 条 trace）" "1" "${NTRACE2}"
TRACE2="$(printf '%s\n' "${TIDS2}" | grep -m1 .)"
if [ -z "${TRACE2}" ]; then
  fail "没取到 trace —— 段 2 的补偿 span 判据无法执行"
else
  SPANS2="$(spans_of "$(jaeger_get "${TRACE2}")")"
  log "trace ${TRACE2} 的补偿段 span（comp=true）："
  printf '%s\n' "${SPANS2}" | awk -F'|' '$4 == "true" {print "    "$0}'
  for want in rpc.dtm.stock.Release rpc.dtm.order.CancelOrder rpc.dtm.product.ReduceSales rpc.dtm.points.Refund; do
    assert_eq "补偿段含 ${want}" "yes" \
      "$(printf '%s\n' "${SPANS2}" | grep -q "|${want}|" && printf yes || printf no)"
  done
  COMP_N="$(printf '%s\n' "${SPANS2}" | awk -F'|' '$4 == "true" {n++} END {print n+0}')"
  COMP_ALL_GID="$(printf '%s\n' "${SPANS2}" | awk -F'|' '$4 == "true" && $3 == gid {n++} END {print n+0}' gid="${GID2}")"
  assert_eq "每个补偿 span 都带 biz.compensation=true" "${COMP_N}" "${COMP_ALL_GID}"
  assert_eq "补偿 span 数 ≥ 4（四域逆序补偿）" "yes" \
    "$([ "${COMP_N}" -ge 4 ] && printf yes || printf no)"
  # 空补偿：被点名的 points.Debit 正向回滚，其补偿由 barrier 判为空补偿 —— 落点
  # 是 barrier 里一行 op='action' 而 reason='compensate'（补偿的 origin 检查插的）。
  NULLROW="$(q "SELECT COUNT(*) FROM points_db.barrier WHERE gid = '${GID2}' AND op = 'action' AND reason = 'compensate'")"
  LEDGERS="$(q "SELECT COUNT(*) FROM points_db.point_ledgers WHERE order_no = '${ORDER2}'")"
  assert_eq "空补偿指纹：barrier 有 action 行且 reason=compensate" "1" "${NULLROW}"
  assert_eq "空补偿不写业务库：该单积分流水 0 行" "0" "${LEDGERS}"
fi
assert_eq "反向探针：伪造 gid 的补偿段 span 数" "0" \
  "$(spans_of "$(jaeger_get deadbeefdeadbeefdeadbeefdeadbeef)" | awk -F'|' '$4 == "true" {n++} END {print n+0}')"

# ---------------------------------------------------------------- 段 3
# TCC 二阶段：Try 全绿 → Confirm（占位单转正式单，**此刻才**写 outbox）；
#            Try 中途失败 → Cancel（三域解冻，零脏数据）。
#
# 反向探针：注入失败那一笔的 outbox 增量必须恰为 0，而全绿那一笔恰为 1 ——
# 两条放在一起才证明「只在 Confirm 后出现」；单看全绿的 1 行，分不出是
# Confirm 写的还是 Try 写的。
segment "3/3 TCC 二阶段"

TCC_BASE="${MICRO_TCC_BASE:-${GATEWAY}}"

ROUTE_CODE="$(curl -sS --noproxy '*' -m 10 -o /dev/null -w '%{http_code}' -X POST "${TCC_BASE}/api/presale/lock" \
  -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' \
  -d '{"user_id":0,"product_id":0,"quantity":0}')"
if [ "${ROUTE_CODE}" = "404" ]; then
  fail "TCC 端点 ${TCC_BASE}/api/presale/lock 返回 404 —— 根因不在 dtm 链路上，而在 bff 的路由前缀："
  fail "  gateway 的路由表是 prefix: /api, strip: true，转发时把 /api 剥掉，bff 因此必须注册 /presale/lock；"
  fail "  而 examples/micro/bff/app/setup.go 注册的是 /api/presale/lock（同文件其余六条路由都不带 /api 前缀）。"
  fail "  排障对照：MICRO_TCC_BASE=http://127.0.0.1:8081 指到 bff 直连口，可验证 TCC 事务链本身是否健康。"
  FAILED=1
  printf '\n=== 汇总 ===\n'
  fail "段 3/3 TCC 二阶段 未执行（端点未注册）；末段：${SEGMENT}"
  exit 1
fi
assert_eq "TCC 端点已注册（零参请求应在参数校验处 400，不落任何数据）" "400" "${ROUTE_CODE}"

# --- 3a：Try 全绿 → Confirm ---
OUTBOX_BEFORE_3A="$(q 'SELECT COUNT(*) FROM order_db.outbox')"
read_out="$(api_post "${TCC_BASE}" /api/presale/lock "{\"user_id\":${USER_ID},\"product_id\":${PRODUCT_ID},\"quantity\":${QUANTITY}}")"
CODE3A="$(printf '%s' "${read_out}" | head -n 1)"
BODY3A="$(printf '%s' "${read_out}" | sed -n '2,$p')"
assert_eq "POST /api/presale/lock 全绿提交" "200" "${CODE3A}"
GID3A="$(jget "${BODY3A}" '.dtm_gid // ""')"
ORDER3A="$(jget "${BODY3A}" '.order_no // ""')"
log "全绿 gid=${GID3A} order_no=${ORDER3A}"
assert_eq "dtm 侧全局事务终态" "succeed" "$(wait_trans_status "${GID3A}" 60)"
STATUS3A="$(q "SELECT status FROM order_db.orders WHERE order_no = '${ORDER3A}' AND dtm_gid = '${GID3A}'")"
assert_eq "占位单终态" "paid" "${STATUS3A}"
assert_eq "order_status_log 出现 presale_locked → paid" "1" \
  "$(q "SELECT COUNT(*) FROM order_db.order_status_log
        WHERE order_id = (SELECT id FROM order_db.orders WHERE order_no = '${ORDER3A}')
          AND from_status = 'presale_locked' AND to_status = 'paid'")"
OUTBOX_AFTER_3A="$(q 'SELECT COUNT(*) FROM order_db.outbox')"
assert_eq "全绿：outbox 恰 +1（Confirm 写的）" \
  "$(( OUTBOX_BEFORE_3A + 1 ))" "${OUTBOX_AFTER_3A}"
assert_eq "Confirm 后冻结量归零（TCC 是冻结→实扣，不是永久冻结）" "0" \
  "$(q "SELECT frozen FROM stock_db.stocks WHERE product_id = ${PRODUCT_ID}")"

# --- 3b：TryPlace 注入 → Cancel ---
wait_dtm_quiet
BASE3B="$(four_db)"
OUTBOX_BEFORE_3B="$(outbox_rows)"
MINID3B="$(q "SELECT COALESCE(MAX(id),0) FROM ${DTM_DB}.trans_global WHERE trans_type = 'tcc'")"
read_out="$(api_post "${TCC_BASE}" /api/presale/lock \
  "{\"user_id\":${USER_ID},\"product_id\":${PRODUCT_ID},\"quantity\":${QUANTITY},\"fail_inject\":\"TryPlace\"}")"
CODE3B="$(printf '%s' "${read_out}" | head -n 1)"
BODY3B="$(printf '%s' "${read_out}" | sed -n '2,$p')"
assert_eq "POST /api/presale/lock(fail_inject=TryPlace) 非 200（Try 阶段就该失败）" "500" "${CODE3B}"
GID3B="$(q "SELECT gid FROM ${DTM_DB}.trans_global WHERE trans_type = 'tcc' AND id > ${MINID3B} ORDER BY id DESC LIMIT 1")"
log "注入笔 gid=${GID3B}（Try 失败时 bff 不回 gid，从 dtm_db 按自增 id 取）"
assert_eq "dtm 侧全局事务终态" "failed" "$(wait_trans_status "${GID3B}" 60)"
# stock 的 Try 真落过库 —— 否则「outbox 没多」是因为 Try 根本没跑。
assert_eq "反向控制：stock 的 Try 真落过库（barrier 有 try 行）" "1" \
  "$(q "SELECT COUNT(*) FROM stock_db.barrier WHERE gid = '${GID3B}' AND branch_id = '01' AND op = 'try' AND reason = 'try'")"

i=0; AFTER3B=""
while [ "${i}" -lt 30 ]; do
  AFTER3B="$(four_db)"
  if [ "${AFTER3B}" = "${BASE3B}" ]; then break; fi
  sleep 2; i=$(( i + 1 ))
done
log "Cancel 后四库：${AFTER3B}"
assert_eq "Cancel 后四库回基线（三域 frozen 归零、可售回原值）" "${BASE3B}" "${AFTER3B}"
# 0 是「不该变化」，故轮询只用于给退干净的留时间（下面的回基线循环已经覆盖），
# 这里直接读：多等也不会变，而多等会让失败来得更晚。
assert_eq "反向探针：Try 失败那一笔 outbox 增量为 0（反向控制：全绿那笔是 +1）" \
  "${OUTBOX_BEFORE_3B}" "$(outbox_rows)"
assert_eq "占位单零残留（无 presale_locked）" "0" \
  "$(q "SELECT COUNT(*) FROM order_db.orders WHERE status = 'presale_locked'")"
# 三域**各有自己的 barrier 表**（barrier 行必须与业务写同事务，故按域隔离，见
# schema-micro.sql）。只查 stock_db.barrier 只能数到 stock 那一个分支。
# order 域的 Cancel 是空补偿（TryPlace 从未落地），它同样落一行 op='cancel' ——
# 屏障记下了「这一步回滚过」，业务回调却被跳过，这正是空补偿的形状。
assert_eq "三域 Cancel 分支都发过（barrier 行：stock/points/order 各自的表）" "3" \
  "$(q "SELECT COUNT(DISTINCT branch_id) FROM (
          SELECT branch_id FROM stock_db.barrier  WHERE gid = '${GID3B}' AND op = 'cancel'
          UNION SELECT branch_id FROM points_db.barrier WHERE gid = '${GID3B}' AND op = 'cancel'
          UNION SELECT branch_id FROM order_db.barrier WHERE gid = '${GID3B}' AND op = 'cancel') x")"
assert_eq "未落地的 order 分支是空补偿：orders 表零占位单" "0" \
  "$(q "SELECT COUNT(*) FROM order_db.orders WHERE dtm_gid = '${GID3B}'")"

# ---------------------------------------------------------------- 汇总
printf '\n=== 汇总 ===\n'
if [ "${FAILED}" -eq 0 ]; then
  log "三段全绿（末段：${SEGMENT}）"
  exit 0
fi
fail "存在失败判据（末段：${SEGMENT}）"
exit 1