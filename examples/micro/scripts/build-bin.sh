#!/usr/bin/env bash
# =============================================================================
# 交叉编译微服六个进程到 examples/docker/bin/
# -----------------------------------------------------------------------------
# 产物名与 k8s 清单（deploy/micro.yaml）里的 command 一一对应：
#   demo-gateway demo-bff demo-product demo-order demo-stock demo-points
#
# 为什么不在容器里 go build：镜像构建上下文得是 hubx 仓库根（replace 指向
# ../webx 等兄弟 SDK），那个目录下挂着 mongodbx/data、objectx/data 等数据目录，
# tar 一遍极慢；而且容器内 build 会重新拉依赖，构建时间与失败面都不可控。
#
# 用法：
#   bash examples/micro/scripts/build-bin.sh
#   GOOS=linux GOARCH=amd64 bash examples/micro/scripts/build-bin.sh   # 默认
#
# 架构写错的表现是容器内 `exec format error`：宿主是 Apple Silicon 时别照
# uname -m 猜，minikube(docker driver) 通常是 amd64。
# =============================================================================
set -uo pipefail

GO_BIN="${GO_BIN:-go}"
GOOS="${GOOS:-linux}"
GOARCH="${GOARCH:-amd64}"
MICRO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${OUT:-$(cd "$MICRO_DIR/../docker" && pwd)/bin}"

# 产物名刻意带 demo- 前缀：它是**服务名**（Jaeger 的 service、consul 的注册名），
# 与目录名（srv-order 等）不是一回事。两者同名会让人以为"改目录名就能改服务名"。
SPECS=(
  "gateway:demo-gateway"
  "bff:demo-bff"
  "srv-product:demo-product"
  "srv-order:demo-order"
  "srv-stock:demo-stock"
  "srv-points:demo-points"
)

mkdir -p "$OUT"
fail=0
for spec in "${SPECS[@]}"; do
  dir=${spec%%:*}
  name=${spec##*:}
  printf 'build %-12s → %s/%s\n' "$dir" "$OUT" "$name"
  if ! (cd "$MICRO_DIR/$dir" && CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
    "$GO_BIN" build -trimpath -o "$OUT/$name" ./cmd); then
    echo "FAIL $dir" >&2
    fail=1
  fi
done

if [ "$fail" -ne 0 ]; then
  echo "编译失败：$OUT 里的产物不完整，不要拿去打镜像" >&2
  exit 1
fi
echo "产物就绪：$OUT"
ls -1 "$OUT"
