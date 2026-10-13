# add-micro-dtm-saga-tcc 实现计划（微服 DTM saga/tcc）— part6

**Traceability (sddflow):**
- plan-ready: `openspec/changes/add-micro-dtm-saga-tcc/plan-ready.md`
- tasks: `openspec/changes/add-micro-dtm-saga-tcc/tasks.md`
- plan: `docs/superpowers/plans/2026-10-13-add-micro-dtm-saga-tcc.md`（Task 1-4）+ `-part2.md`（5）+ `-part3.md`（6-9）+ `-part4.md`（10-11）+ `-part5.md`（12-15）+ **本文件（16-17）**

> **amend 来源**：2026-10-13 用户决策「hubx 目录下创建 dtmx」→ 平级组件仓
> `github.com/gospacex/dtmx`（design.md **D8**、proposal `## Amendments`）。
> 主计划头见 `2026-10-13-add-micro-dtm-saga-tcc.md`。Task N 在六份文件中连续不跳号。

**本 Task 的前提事实（不得与之打架）：**

- `../dtmx` 的同级组件体例（`dbx`/`otelx`/`lockx`）：独立 git 仓、`module github.com/gospacex/<name>`、
  `interface.go` + `interface_test.go` + `README.md` + `.gitignore`；launchx 侧以
  `replace github.com/gospacex/<name> => ../<name>` 消费（见 launchx `go.mod:191-224`）。
- 迁移源：`examples/micro/pkg/dtmx/{result,barrier,payload}.go` + 三个 `_test.go`（已实现并全绿，
  commit `3cf3ccd`）。包名保持 `dtmx`，对外符号一个不改。
- 唯一必须改的实现细节：`ExtractParent` 现在调 `launchx.Extract`（commit `3cf3ccd`）。
  组件仓是**独立 module**，import `github.com/gospacex/launchx` 会成环
  （launchx 已 replace 指向 dtmx），必须换成 `github.com/gospacex/otelx/propagation`——
  它正是 launchx 的 `Inject`/`Extract` 背后的实现（`launchx/trace.go:26,34`），语义同源不变。

---

### Task 16: dtmx 组件仓骨架

> **trace:** plan-ready.md → `### Task 16: dtmx 组件仓骨架（对应 tasks.md 5.1）` | tasks.md → `- [ ] 5.1 组件仓骨架：新建 ` + '`../dtmx`' + `（` + '`module github.com/gospacex/dtmx`' + `）+ ` + '`git init`' + `；体例对齐 ` + '`dbx`' + `/` + '`otelx`' + `（` + '`interface.go`' + ` / ` + '`interface_test.go`' + ` / ` + '`README.md`' + ` / ` + '`.gitignore`' + ` / ` + '`go.mod`' + `+` + '`go.sum`' + `）；` + '`go.mod`' + ` 只依赖 ` + '`github.com/dtm-labs/client`' + ` 与 otel，**不 import launchx/configx/dbx**（纯库）。验收：` + '`cd ../dtmx && go build ./... && go test ./...`' + ` 全绿`
> **sync:** tasks.md → `- [ ] 5.1 组件仓骨架：新建 `../dtmx`（`module github.com/gospacex/dtmx`）+ `git init`；体例对齐 `dbx`/`otelx`（`interface.go` / `interface_test.go` / `README.md` / `.gitignore` / `go.mod`+`go.sum`）；`go.mod` 只依赖 `github.com/dtm-labs/client` 与 otel，**不 import launchx/configx/dbx**（纯库）。验收：`cd ../dtmx && go build ./... && go test ./...` 全绿` | plan-ready.md → `### Task 16: dtmx 组件仓骨架（对应 tasks.md 5.1）`

**Files:**
- 创建：`../dtmx/go.mod`、`../dtmx/go.sum`、`../dtmx/.gitignore`
- 创建：`../dtmx/interface.go`、`../dtmx/interface_test.go`、`../dtmx/README.md`

- [ ] **Step 16.1：建仓与 module 骨架**

```bash
cd /Users/hyx/work/gowork/src/hubx
mkdir -p dtmx && cd dtmx
git init -q
cat > go.mod <<'EOF'
module github.com/gospacex/dtmx

go 1.25.0

require (
	github.com/dtm-labs/client v1.18.7
	go.opentelemetry.io/otel v1.46.0
)
EOF
```

`go 1.25.0` 与同级 `otelx` 对齐（`otelx/go.mod:3`）。

- [ ] **Step 16.2：拉依赖并固化 go.sum**

```bash
cd /Users/hyx/work/gowork/src/hubx/dtmx
go mod tidy 2>&1 | tail -3
go list -m all | head -5
```

预期：退出 0，`go.sum` 生成。**若 `go mod tidy` 报 "no Go files"**，说明源码还没移过来 ——
这是预期的，下一个 Step 会移；`go mod tidy` 留到 Step 17.1 之后再跑一次。

- [ ] **Step 16.3：写 `.gitignore`（对齐 otelx 体例）**

```gitignore
# 编译产物
*.exe
*.test
*.out

# IDE
.idea/
.vscode/

# 本地环境（口令、DSN 一律不入库）
.env
*.local.yaml
```

- [ ] **Step 16.4：写 `interface_test.go`（先 RED）**

组件仓的 `interface_test.go` 在本仓的职责是**契约守卫**：钉死「dtmx 不得依赖 launchx」与
「失败哨兵必须是 dtm 官方的」。这两条用 import 图直接判。

```go
package dtmx

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dtmx 是**独立组件仓**，不是 launchx 的子包。它被 launchx 以
// `replace => ../dtmx` 消费 —— 一旦反向 import launchx 就成环，
// MVS 直接解析失败；更糟的是能解析但把示例/装配面拖进组件。
//
// 为什么用 AST 而不是 grep：本判据要求「零命中」，而本仓的注释里会大量出现
// "launchx" 字样（说明为什么不能依赖它），grep 会把它们算成命中 —— 恒红的判据
// 很快会被整片忽略掉。
func TestDtmxDoesNotDependOnLaunchx(t *testing.T) {
	forbidden := []string{
		"github.com/gospacex/launchx",   // 反向依赖会与 replace 成环
		"github.com/gospacex/configx",   // 纯库不读配置中心
		"github.com/gospacex/dbx",       // 句柄由调用方注入
		"github.com/cloudwego/",          // RPC/HTTP 框架属调用方
		"gorm.io/gorm",                   // ORM 属调用方
	}
	var bad []string
	scanned := 0
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != "." && strings.HasPrefix(filepath.Base(path), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		scanned++
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			t.Errorf("解析 %s 失败：%v", path, perr)
			return nil
		}
		for _, imp := range f.Imports {
			lit := strings.Trim(imp.Path.Value, `"`)
			for _, prefix := range forbidden {
				if strings.HasPrefix(lit, prefix) {
					bad = append(bad, path+"  import "+lit)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk 失败：%v", err)
	}
	if scanned == 0 {
		t.Fatal("一个 .go 都没扫到，路径基准错误（测试 cwd 应为 dtmx 仓根）")
	}
	if len(bad) > 0 {
		t.Fatalf("dtmx 出现 %d 处禁用依赖，组件仓必须保持独立：\n  %s", len(bad), strings.Join(bad, "\n  "))
	}
	t.Logf("dtmx 依赖面干净：扫过 %d 个 .go 文件", scanned)
}
```

- [ ] **Step 16.5：运行守卫，预期 RED（源码还没移进来 → 一个文件没扫到）**

运行：`cd /Users/hyx/work/gowork/src/hubx/dtmx && go test ./... -v`
预期：FAIL，报 `一个 .go 都没扫到`。Task 17 移入源码后转绿。

- [ ] **Step 16.6：Commit**

```bash
cd /Users/hyx/work/gowork/src/hubx/dtmx
git add -A && git commit -q -m "chore(dtmx): 组件仓骨架（module github.com/gospacex/dtmx）"
```

- [ ] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 对应行同步）

---

### Task 17: 三件套迁移与接线

> **trace:** plan-ready.md → `### Task 17: 三件套迁移与接线（对应 tasks.md 5.2 / 5.3 / 5.4）` | tasks.md → `- [ ] 5.2 迁移三件套：把 ` + '`examples/micro/pkg/dtmx/{result,barrier,payload}.go`' + ` 及其 ` + '`_test.go`' + ` 移入 ` + '`../dtmx`' + `，包名保持 ` + '`dtmx`' + `；` + '`ExtractParent`' + ` 原依赖 ` + '`launchx.Extract`' + ` 需改为 ` + '`otelx/propagation`' + `（与 launchx 的 Inject/Extract 同源实现，语义不变）。验收：` + '`../dtmx`' + ` 内测试全绿且与迁移前用例数一致`
> **sync:** tasks.md → `- [ ] 5.2 迁移三件套：把 `examples/micro/pkg/dtmx/{result,barrier,payload}.go` 及其 `_test.go` 移入 `../dtmx`，包名保持 `dtmx`；`ExtractParent` 原依赖 `launchx.Extract` 需改为 `otelx/propagation`（与 launchx 的 Inject/Extract 同源实现，语义不变）。验收：`../dtmx` 内测试全绿且与迁移前用例数一致` | plan-ready.md → `### Task 17: 三件套迁移与接线（对应 tasks.md 5.2 / 5.3 / 5.4）`

**Files:**
- 移动：`examples/micro/pkg/dtmx/{result,barrier,payload}.go` + `*_test.go` → `../dtmx/`
- 修改：`../dtmx/payload.go`（`ExtractParent` 换 otelx）、`../dtmx/payload_test.go`（同）
- 修改：`go.mod`、`go.sum`（launchx 侧加 replace + require）
- 修改：srv-stock / srv-points 下 15 个导入方的 import 路径
- 删除：`examples/micro/pkg/dtmx/`

- [ ] **Step 17.1：迁移文件**

```bash
cd /Users/hyx/work/gowork/src/hubx
git -C launchx mv examples/micro/pkg/dtmx/result.go examples/micro/pkg/dtmx/barrier.go \
                 examples/micro/pkg/dtmx/payload.go \
                 examples/micro/pkg/dtmx/result_test.go examples/micro/pkg/dtmx/barrier_test.go \
                 examples/micro/pkg/dtmx/payload_test.go dtmx/
ls dtmx/ && ls examples/micro/pkg/ | grep dtmx || echo "✓ 源目录已空"
```

预期：`../dtmx` 下 6 个 .go 文件；`examples/micro/pkg/` 下不再有 `dtmx`。

- [ ] **Step 17.2：改 `ExtractParent` 的依赖来源**

`../dtmx/payload.go` 中把

```go
import "github.com/gospacex/launchx"
...
	return launchx.Extract(ctx, carrierOf(tp))
```

改为（语义等价：launchx.Extract 背后就是 otelx/propagation.Extract，见 `launchx/trace.go:34`）：

```go
import "github.com/gospacex/otelx/propagation"
...
	return propagation.Extract(ctx, carrierOf(tp))
```

同时把包注释里「与仓内唯一续链入口 launchx.Inject/Extract 同源」一句改为
「与 launchx 的 Inject/Extract 同源（launchx/trace.go 即转调本包）」——
组件仓不该让 import 它的库反过来成为它的依赖说明。

`../dtmx/payload_test.go` 里 `launchx.Extract` / `launchx.Inject` 同样换成
`propagation.Extract` / `propagation.Inject`，并把 `telemetry.Init(...)` 那段去掉
—— `internal/telemetry` 是 launchx 的内部包，组件仓 test 不能 import 它。
改为直接注册传播器：

```go
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
```

- [ ] **Step 17.3：组件仓独立跑测试**

```bash
cd /Users/hyx/work/gowork/src/hubx/dtmx
go mod tidy
gofmt -l .          # 必须为空
go build ./...
go test ./... -v -count=1 2>&1 | grep -E "^(--- PASS|--- FAIL|ok|FAIL)"
```

预期：9 个用例全 PASS（迁移前 `3cf3ccd` 的用例数），含 Task 12 会复用的
`TestBarrierFromCtxRejectsMissingGrpcMeta` / `TestSetBarrierTableNameDefaultsToPerDomain` /
`TestPayloadRoundTripCarriesTraceparentAndFailInject`。

- [ ] **Step 17.4：launchx 接线**

```bash
cd /Users/hyx/work/gowork/src/hubx/launchx
go mod edit -replace github.com/gospacex/dtmx=../dtmx
go get github.com/gospacex/dtmx 2>&1 | tail -2
go build ./... 2>&1 | head -20
```

预期：此时**会报 15 处 `no required module provides package github.com/gospacex/dtmx` 之外的问题**
—— 正常，因为导入方还没改路径，下一步改。

- [ ] **Step 17.5：改全部导入方**

```bash
cd /Users/hyx/work/gowork/src/hubx/launchx
grep -rl 'examples/micro/pkg/dtmx' --include="*.go" . | while read -r f; do
  sed -i '' 's|github.com/gospacex/launchx/examples/micro/pkg/dtmx|github.com/gospacex/dtmx|g' "$f"
  echo "patched: $f"
done
grep -r 'examples/micro/pkg/dtmx' --include="*.go" . && echo "❌ 仍有残留" || echo "✓ 无残留引用"
```

预期：15 个文件被改；最后一行打印 `✓ 无残留引用`。

- [ ] **Step 17.6：验证迁移没有打破既有行为**

```bash
cd /Users/hyx/work/gowork/src/hubx/launchx
gofmt -l examples/ ; echo "(空=干净)"
CGO_ENABLED=0 go build ./... ; echo "build exit=$?"
CGO_ENABLED=0 go vet ./examples/micro/... 2>&1 | head -5
CGO_ENABLED=0 go test ./examples/micro/... -count=1 2>&1 | grep -E "^(ok|FAIL|---)" | head -20
```

预期：build exit=0；srv-stock 与 srv-points 的既有测试（Task 5/6 产物）仍全绿 ——
它们引用 dtmx 的方式没变，只是路径换了。

- [ ] **Step 17.7：写 `../dtmx/README.md`**

```markdown
# dtmx

dtm（distributed transaction manager）的 Go 客户端公共件。三件套，各自解决一件
「搞错了会静默丢账」的事：

| 件 | 解决什么 | 搞错的后果 |
|---|---|---|
| `result.go` | 业务失败标记 | 自造哨兵 → dtm server 认不出 → 业务失败（积分不足）被当基础失败**无限重试** |
| `payload.go` | traceparent 编解码与续链 | 续链入口与注入端不同源 → 每个服务都有 span 却不共链；补偿段须走新根 + `biz.dtm.gid` 关联 |
| `barrier.go` | dtm 官方 `BranchBarrier` 包装 | 不套 barrier → 空补偿/重复回调/悬挂三条守卫全失效 |

## 硬约束

1. **不得自造失败哨兵。** `BusinessFailure` 包的是官方 `dtmcli.ErrFailure`，
   `IsBusinessFailure` 用 `errors.Is` 判。自造一个同文案错误 dtm server 认不出。
2. **barrier 表按域隔离。** 默认表名 `"barrier"`，落在**各域业务库内**，
   不是官方那个跨库的 `dtm_barrier.barrier` —— `BranchBarrier.CallWithDB` 是在
   **业务连接的事务内**插 barrier 行的，跨库就插不进这个事务，屏障等于没有。
   每个域服在装配期显式调 `SetBarrierTableName("barrier")`（该变量是进程级全局量）。
3. **不得 import launchx / configx / dbx。** 本仓是独立组件仓，被 launchx 以
   `replace => ../dtmx` 消费，反向 import 会成环。由
   `TestDtmxDoesNotDependOnLaunchx` 守卫。
4. **`ExtractParent` 空 traceparent 原样放行。** dtm 的异步补偿/重启续推不带
   traceparent，这类回调按「新根 trace + `biz.dtm.gid` 关联」处理，
   凭空造同 ID 孤儿 span 是自欺。

## 分支寻址

Saga 的 action/compensate、TCC 的 try/confirm/cancel 都是 URL 字符串，形态
`<host:port>/<proto-service>/<method>`（无 scheme 前缀），由 dtm server 拆解后拨号。
本包**不生成**这些 URL —— host 段属调用方环境，由它从自己的配置拼入。

## 用法

```go
dtmx.SetBarrierTableName("barrier")            // 装配期，每进程一次

ctx = dtmx.ExtractParent(ctx, payload.Traceparent) // 分支面入口
bar, err := dtmx.BarrierFromCtx(ctx)            // 拿不到必须上抛
if err != nil { return err }                     // 忽略 = 无保护落库

err = bar.Call(ctx, rawDB, func(ctx context.Context, tx *sql.Tx) error {
    // 只准用 tx 落库；换 gorm 或另开连接会脱离这个事务
    return store.DeductTx(ctx, tx, ...)
})
if err != nil {
    if dtmx.IsBusinessFailure(err) { /* 库存不足等，dtm 不重试 */ }
    return err // 基础失败，dtm 按退避重试
}
```
```

- [ ] **Step 17.8：Commit（两个仓分别提交）**

```bash
cd /Users/hyx/work/gowork/src/hubx/dtmx
git add -A && git commit -q -m "feat(dtmx): 三件套落地（官方失败哨兵 / traceparent 续链 / 官方 barrier 包装）"

cd /Users/hyx/work/gowork/src/hubx/launchx
git add go.mod go.sum examples/ && git commit -q -m "refactor(examples/micro): dtmx 改用 hubx 平级组件仓 github.com/gospacex/dtmx"
```

- [ ] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 对应行同步）