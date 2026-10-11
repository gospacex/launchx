#!/usr/bin/env bash
# =============================================================================
# 把 go.mod 里 replace 指向的兄弟 SDK 仓克隆到 ../
# -----------------------------------------------------------------------------
# 为什么需要这一步：launchx 的 go.mod 用 replace 把 github.com/gospacex/<sdk>
# 指到 ../<sdk>（本地工作区多仓并行开发）。这套 replace 在**宿主工作区**里跑得
# 很顺，但到了 CI 上 ../<sdk> 是不存在的 —— MVS 会去访问真实的 github 仓库，
# 拿到的是未包含本次改动的上游版本，于是"CI 编译的是另一份代码"。
# 那种绿比红危险得多。
#
# 用法：
#   bash examples/scripts/fetch-sdks.sh            # 克隆全部
#   GOSPACEX_REF=main bash examples/scripts/fetch-sdks.sh
#
# 幂等：已存在的目录只 fetch，不重新 clone。
# =============================================================================
set -uo pipefail

ORG="${GOSPACEX_ORG:-gospacex}"
REF="${GOSPACEX_REF:-main}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# 清单与 go.mod 的 replace 段一一对应。
# 刻写成显式列表而不是从 go.mod 解析：解析出来的清单会随 replace 的增删漂移，
# 而"哪个仓是必须的"这件事应当由人确认过 —— 解析脚本漏一个，CI 会以
# "某个包找不到"的形式挂在离原因很远的地方。
SDK_REPOS=(
  cachex configx coordx dbx kvx lockx logx mqx otelx rpcx searchx
  traceflag webx mongodbx hubx
)
# 嵌套子模块：路径形如 `cachex/redis`，对应**父仓 cachex 内的子目录**（子 module），
# GitHub 上并没有 `gospacex/redis` 这个仓库。故这里只登记路径，clone 时连同父仓
# 一起取回 —— clone_one 幂等，多个子路径共享同一个父仓时只会真 clone 一次。
SDK_NESTED=(
  cachex/memcached cachex/redis
  coordx/apollo coordx/consul coordx/etcd coordx/k8s_configmap coordx/nacos
  kvx/redis lockx/redis
  mqx/kafka mqx/nsq mqx/pulsar mqx/rabbitmq mqx/rocketmq
  searchx/elasticsearch
)

clone_one() {
  local rel="$1" url="$2"
  local dest="$ROOT/../$rel"
  if [ -d "$dest/.git" ]; then
    printf 'skip  %s（已存在，如需更新请手动 pull）\n' "$rel"
    return 0
  fi
  printf 'clone %s\n' "$rel"
  mkdir -p "$(dirname "$dest")"
  if ! git clone --depth 1 --branch "$REF" "$url" "$dest"; then
    printf 'FAIL  %s\n' "$rel" >&2
    return 1
  fi
}

fail=0
for r in "${SDK_REPOS[@]}"; do
  clone_one "$r" "https://github.com/$ORG/$r.git" || fail=1
done
# 嵌套子模块取**第一段**（父仓名）来拼 URL：`path` 里的第二段是父仓内的子目录，
# 拼进 URL 会得到 `.../gospacex/cachex/redis.git` 这种必然 404 的地址。
for r in "${SDK_NESTED[@]}"; do
  repo="${r%%/*}"
  clone_one "$repo" "https://github.com/$ORG/$repo.git" || fail=1
done

if [ "$fail" -ne 0 ]; then
  echo "部分 SDK 克隆失败 —— 不要继续编译：拿到的会是上游旧版本" >&2
  exit 1
fi
echo "SDK 就绪"
