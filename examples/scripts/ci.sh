#!/usr/bin/env bash
# =============================================================================
# examples 的门禁脚本：gofmt → go build → go vet → go test
# -----------------------------------------------------------------------------
# 为什么单独一个脚本而不是把四条命令写进 workflow：门禁顺序是**有依赖的** ——
# 格式不过就别编译，编译不过就别静态检查，静态检查不过就别跑测试。写在 YAML 里
# 会被人加一句 `|| true` 让某一步静默失效，而这类失效正是门禁存在的意义所在。
# 本地与 CI 跑同一份脚本，才不会出现"CI 红了但本地绿了"。
#
# 用法：
#   bash examples/scripts/ci.sh                # 全量（monolith + micro + mall）
#   bash examples/scripts/ci.sh micro          # 只跑一个示例
# 默认开 cgo：examples/micro/srv（冻结的拆分前对照样例）的仓储与 outbox 用例
# 走 go-sqlite3，它在 CGO_ENABLED=0 下是编译期 stub，一律报
# "Binary was compiled with 'CGO_ENABLED=0'"。关掉 cgo 会让这批用例**全红**，
# 而它们其实是好的 —— 那等于逼着人把门禁改成 skip。
# 真正需要关 cgo 的场景（交叉编译产物）不跑测试，用 build 脚本单独走。
#
# 前置：go.mod 的 replace 指向 ../<SDK> 兄弟目录，故必须在 hubx 工作区内跑
# （CI 由 examples/scripts/fetch-sdks.sh 把这些兄弟仓克隆到 ../）。
#
# 刻意不定义 set -e：四步都要跑完再看总账，中途退出会让人以为只错了一处。
#
# 踩坑记录（macOS bash 3.2）：**变量后面紧跟中文必须写成 ${name}**。
# bash 3.2 不做多字节识别：若写成「`$scope` 紧跟中文且没加花括号」（下面第 78 行
# 那道检查专门拦这种写法），变量名会被并进后面那些字节里 —— `set -u` 下报
# "unbound variable" 而名字带乱码，不带 -u 则静默展开成空串，两种情况都不指向
# 真正的原因。本脚本里所有紧跟非 ASCII 的引用一律用花括号（同 e2e.sh 的约定）。
# =============================================================================
set -uo pipefail

GO_BIN="${GO_BIN:-go}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

# 目标包：按示例划分，谁改了什么就跑谁。
# 不用关联数组：macOS 自带的 bash 是 3.2，declare -A 在那里不可用。
pkgs_for() {
  case "$1" in
    monolith) echo "./examples/monolith/..." ;;
    micro)    echo "./examples/micro/..." ;;
    mall)     echo "./examples/mall/..." ;;
    pkg)      echo "./examples/pkg/..." ;;
    mesh)     echo "" ;; # 独立模块，走下面的 extra_modules
    all)      echo "./examples/monolith/... ./examples/micro/... ./examples/mall/... ./examples/pkg/..." ;;
    *)        echo "" ;;
  esac
}

# 独立模块清单：这些目录自带 go.mod，主模块的 ./... **覆盖不到**。
#
# 不在这里列出来就等于"门禁绿了，而那个模块根本没被编译过" ——
# examples/mesh/apps 正是这种情况（独立 go.mod + replace => ../../..），
# 早期踩过一次：有人把 micro 的旧快照拷进 mesh 并反向 import micro/**/internal，
# 违反 internal 规则直接把主模块的 go build ./... 搞挂，而 mesh 自己从没被门禁扫过。
extra_modules() {
  case "$1" in
    all|mesh) echo "examples/mesh/apps" ;;
    *)        echo "" ;;
  esac
}

scope="${1:-all}"
# 判可用性要**同时**看两处：mesh 的包列表按设计是空的（它只有独立模块，
# 走 extra_modules）。只判 pkgs_for 会把 mesh 挡在门外 —— 而那正是本脚本
# 最需要覆盖的模块（主模块的 ./... 扫不到它），却又在校验里自相矛盾地
# 把它列在"可选"里。早期这个 bug 让 `ci.sh mesh` 永远跑不成。
if [ -z "$(pkgs_for "$scope")" ] && [ -z "$(extra_modules "$scope")" ]; then
  echo "未知示例：${scope}（可选：monolith / micro / mall / pkg / mesh / all）" >&2
  exit 2
fi

# shellcheck disable=SC2086 # 这里就是要按空白拆词
pkgs=($(pkgs_for "$scope"))

fail=0
step() { printf '\n=== %s ===\n' "$1"; }

step "shell 变量紧邻非 ASCII 检查（macOS bash 3.2 会把它吞进变量名）"
# 为什么值得单独一步：这不是风格问题，是**静默错误**。`echo "端口 $p 被占用"`
# 这种写法一旦漏写花括号、让变量名与后面的中文之间没有空格，bash 3.2 里
# 带 set -u 时报 "unbound variable" 而名字还带乱码，两种都不指向真正原因。
# 只扫被花括号保护的写法以外的裸引用。
#
# ⚠️ 这里曾经用 `[^[:space:][:print:]]` 判"非 ASCII"，那条正则**恒不命中**：
# POSIX 的 `[[:print:]]` 把多字节汉字也判为可打印（`printf '（x\n' | grep -cE
# '[^[:print:]]'` == 0，`LC_ALL=C` 下同样 0），于是这一步永远打印 OK —— 一道
# 恒绿的闸门比没有闸门更危险。判"非 ASCII"必须用字节范围 `[^ -~]`。
bad_shell=$(grep -rnE '\$[A-Za-z_][A-Za-z0-9_]*[^ -~]' --include='*.sh' examples/ 2>/dev/null || true)
if [ -n "$bad_shell" ]; then
  echo "以下脚本里变量后紧跟非 ASCII，必须写成 \${name} 形式：" >&2
  printf '%s\n' "$bad_shell" >&2
  fail=1
else
  echo "OK"
fi

step "gofmt（只报不改：自动修会把"忘了格式化"变成一次静默的格式提交）"
# gofmt 的目录清单要**连独立模块一起收**：只扫主模块包的话，`ci.sh mesh`
# 会对一个空目录列表调 gofmt —— 而 `gofmt -l` 无参时是**从 stdin 读**，
# 表现为脚本无声挂住（不是报错，更难查）。两处都堵上：
#   1. 目录列表并入 extra_modules；
#   2. 列表为空时直接跳过，不给 gofmt 裸奔的机会。
dirs=()
for p in ${pkgs[@]+"${pkgs[@]}"}; do dirs+=("${p%/...}"); done
while read -r m; do
  [ -n "$m" ] && dirs+=("$ROOT/$m")
done <<<"$(extra_modules "$scope")"
if [ ${#dirs[@]} -eq 0 ]; then
  echo "OK（本范围没有可格式化的目录）"
else
  # 先看 gofmt 自己的退出码：本脚本刻意不加 `set -e`（失败要聚合成一条报告），
  # 命令替换失败时变量为空，若只判空就会把"gofmt 执行失败"读成"全部已格式化"。
  unformatted=$(gofmt -l "${dirs[@]}")
  grc=$?
  if [ "$grc" -ne 0 ]; then
    echo "gofmt 执行失败（退出码 ${grc}），门禁无法判定，按失败处理" >&2
    fail=1
  elif [ -n "$unformatted" ]; then
    echo "以下文件未 gofmt：" >&2
    printf '%s\n' "$unformatted" >&2
    fail=1
  else
    echo "OK"
  fi
fi

for p in ${pkgs[@]+"${pkgs[@]}"}; do
  step "go build $p"
  if ! CGO_ENABLED="${CGO_ENABLED:-1}" "$GO_BIN" build "$p"; then fail=1; fi

  step "go vet $p"
  if ! CGO_ENABLED="${CGO_ENABLED:-1}" "$GO_BIN" vet "$p"; then fail=1; fi

  step "go test $p"
  # 不加 -count=1：允许命中缓存，让"没改的包"在 CI 上也秒过。
  # 需要强制重跑时先 `go clean -testcache`。
  if ! CGO_ENABLED="${CGO_ENABLED:-1}" "$GO_BIN" test "$p"; then fail=1; fi
done

# 独立模块：进目录跑，路径相对主模块不生效。
read -r -a modules <<<"$(extra_modules "$scope")"
for m in "${modules[@]:-}"; do
  [ -z "$m" ] && continue
  step "go build + vet + test（独立模块 ${m}）"
  ( cd "$ROOT/$m" \
    && CGO_ENABLED="${CGO_ENABLED:-1}" "$GO_BIN" build ./... \
    && CGO_ENABLED="${CGO_ENABLED:-1}" "$GO_BIN" vet ./... \
    && CGO_ENABLED="${CGO_ENABLED:-1}" "$GO_BIN" test ./... ) || fail=1
done

printf '\n'
if [ "$fail" -ne 0 ]; then
  echo "门禁 FAIL" >&2
  exit 1
fi
echo "门禁 PASS"
