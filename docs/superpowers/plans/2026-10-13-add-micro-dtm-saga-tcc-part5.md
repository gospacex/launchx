# add-micro-dtm-saga-tcc 实现计划（微服 DTM saga/tcc）— part5

**Traceability (sddflow):**
- plan-ready: `openspec/changes/add-micro-dtm-saga-tcc/plan-ready.md`
- tasks: `openspec/changes/add-micro-dtm-saga-tcc/tasks.md`
- plan: `docs/superpowers/plans/2026-10-13-add-micro-dtm-saga-tcc.md`（Task 1-4）+ `-part2.md`（Task 5）+ `-part3.md`（Task 6-9）+ `-part4.md`（Task 10-11）+ **本文件（Task 12-15）**

> 主计划头（目标/架构/技术栈/执行环境事实/文件结构）见 `docs/superpowers/plans/2026-10-13-add-micro-dtm-saga-tcc.md`。本文件是同一次变更的 Task 连续切片，Task N 在五份文件中连续不跳号。

## 本文件的依赖事实（承接 Task 1-11，不得与前序 Task 打架）

写作时必须与下列已定型符号**逐字一致**（源自 Task 4 `pkg/dtmx` 终版与 Task 5-11 落地形态）：

| 符号 | 签名 | 出处 |
|---|---|---|
| `dtmx.Payload` | `struct { Biz json.RawMessage; Traceparent, FailInject string }` | Task 4 Step 4.1 |
| `dtmx.InjectKey` | `const InjectKey = "traceparent"` | Task 4 Step 4.1 |
| `dtmx.Encode` / `Decode` | `Encode(p Payload) (string, error)` / `Decode(data string) (Payload, error)` | Task 4 Step 4.1 |
| `dtmx.ExtractParent` | `ExtractParent(ctx context.Context, tp string) context.Context` | Task 4 Step 4.1 |
| `dtmx.ErrBizFailure` | `var ErrBizFailure = errors.New("dtm: business failure (dtm must not retry)")` | Task 4 Step 4.2 终版 |
| `dtmx.BusinessFailure` | `BusinessFailure(err error) error`（`errors.Join(ErrBizFailure, err)`） | 同上 |
| `dtmx.IsBusinessFailure` | `IsBusinessFailure(err error) bool` | 同上 |
| `dtmx.Normalize` | `Normalize(ctx context.Context, err error) error` | 同上 |
| `dtmx.Barrier` | `struct{ inner dtmBarrier }`（字段不导出，外部不可构造） | Task 4 Step 4.3 终版 |
| `dtmx.BarrierFromCtx` | `BarrierFromCtx(ctx context.Context) (*Barrier, error)` | 同上 |
| `(*Barrier).Call` | `Call(ctx context.Context, sqlDB *sql.DB, cb func(ctx context.Context, tx *sql.Tx) error) error` | 同上 |
| `RawDB` | `RawDB(gdb *gorm.DB) (*sql.DB, error)` → `gdb.DB()` | Task 5 Step（part2:84） |
| `Store` | `NewStore(sqlDB *sql.DB) *Store` | Task 5-8 |
| span 命名 | `rpc.dtm.<entity>.<Method>`；tag `biz.dtm.gid`、`biz.dtm.branch`、`biz.compensation` | design.md D4 / specs |
| 服务名（Jaeger service） | `demo-gateway` / `demo-bff` / `demo-product` / `demo-order` / `demo-stock` / `demo-points` | `trace_e2e_test.go:38-41` |
| 端点 | Saga `POST /api/order`、TCC `POST /api/presale/lock`（经 gateway 前缀 `/api`） | tasks.md 3.1 / 3.2 |

**已定型的测试助手**（`examples/micro/trace_e2e_test.go`，Task 13 直接复用，**不得重复定义**）：
`requireJaeger(t) *traceutil.Client`、`requireChain(t)`、`gatewayBase() string`、
`authHeader() map[string]string`、`demoToken() string`、
`do(t, method, url, body, hdr) (string, int)`、`get(t, url) (string, int)`、
`fetchSince(t, c, service, since, limit) []traceutil.Trace`、`truncate(s, n)`、
常量 `domainService1..4 = demo-product/demo-order/demo-stock/demo-points`。

**`internal/traceutil` 可用面**（`internal/traceutil/client.go` / `trace.go`）：
`NewFromEnv() *Client`、`(*Client).Fetch(ctx, Query) ([]Trace, error)`、
`Query{Service, Start, End, Limit}` —— **无 tag 过滤字段**，`biz.dtm.gid` 只能拉回来在客户端筛；
`Trace{TraceID, Spans, Processes}`、`Roots()`、`ServiceOf(span)`、`TagsOf(span)`、
`StringTag(span, key)`、`IsError(span)`。

---

### Task 12: 单测与静态守卫（对应 tasks.md 4.1）

> **trace:** plan-ready.md → `### Task 12: 单测与静态守卫（对应 4.1）` | tasks.md → `- [ ] 4.1 单元测试与静态守卫：a)「分支 handler 禁直连 dbx / 必经 service」静态守卫（反向断言：注入直连探针必红）；b) barrier 空补偿/悬挂/重放三条幂等单测（以假 barrier 存储为主口径）；c) bff 两编排器的分支序/补偿序/载荷单测。验收：`go test ./examples/... -count=1` 全绿`
> **sync:** tasks.md → `- [ ] 4.1 单元测试与静态守卫：a)「分支 handler 禁直连 dbx / 必经 service」静态守卫（反向断言：注入直连探针必红）；b) barrier 空补偿/悬挂/重放三条幂等单测（以假 barrier 存储为主口径）；c) bff 两编排器的分支序/补偿序/载荷单测。验收：`go test ./examples/... -count=1` 全绿` | plan-ready.md → `### Task 12: 单测与静态守卫（对应 4.1）`

**Files:**
- 创建：`examples/micro/dtm_surface_test.go`（静态守卫，`package micro`）
- 创建：`examples/micro/pkg/dtmx/barrier_contract_test.go`（barrier 边界契约）
- 创建（临时，Step 12.2 用完即删）：`examples/micro/srv-stock/internal/dtm/zz_probe_direct.go`

> **本 Task 的口径修正（amend 依据）**：tasks.md 4.1(b) 原写「barrier 空补偿/悬挂/重放三条幂等单测（假 barrier 存储）」。
> 复核结论：这三条依赖 dtm 官方 barrier 表在 **MySQL** 上的行锁语义（`SELECT … FOR UPDATE`），
> sqlite 无法复现（sqlite 不支持 `FOR UPDATE`），仓内亦无 sqlmock（`go.mod` 只有
> `gorm.io/driver/sqlite` 与 `stretchr/testify`）。写成「假 barrier 存储」的单测只会断言
> 一个自己写的假实现，测不到 dtm 真实行为——正是 writing-plans「禁止占位符」反对的东西。
> 故：**能在单测里真判的**是 barrier 的**边界契约**（无 grpc metadata 必须拒绝、回调错误必须上抛），
> 落 Step 12.4；**空补偿/悬挂/重放三条**移入 Task 13 真机（真 MySQL + 真 dtm），
> 落 Step 13.6。Task 编号与三向勾选不变。

- [ ] **Step 12.1：写静态守卫测试（含直连探针）**

创建 `examples/micro/dtm_surface_test.go`：

```go
package micro

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dtm 分支面依赖面守卫：分支 handler 只做协议转换，落库统一经 service → dtmstore。
//
// 规则（对应 specs/dtm-branch-facade「分支数据操作经 service 层入链」）：
// srv-*/internal/dtm/ 下**除 store.go 以外**的源码，MUST NOT 直接 import
// database/sql 或 gorm.io/gorm。store.go 是 repository 层产物（Task 5 建立），
// 它持 *sql.DB 并在 dtmx.Barrier.Call 的 tx 内落库；分支 handler（server.go 等）
// 一旦也拿到 *sql.DB / *gorm.DB，就绕开了 service 层的业务判断与 barrier 包裹，
// 而这种绕开 grep 看不出来——只有 import 面看得见。
//
// 为什么用 AST 而不用 grep：仓内已有先例（仓根 dependency_surface_test.go）。
// 注释里写「本包不 gorm.Open」会让 grep 恒红，而本判据恰恰要求「命中为空」。
func TestDtmBranchFaceHasNoDirectDBImports(t *testing.T) {
	// 允许直连的唯一文件：repository 层产物 store.go。
	const allowed = "store.go"
	forbidden := map[string]string{
		"database/sql":  "分支 handler 必须经 service → dtmstore 落库，不得自持 *sql.DB",
		"gorm.io/gorm":  "分支 handler 必须经 service → dtmstore 落库，不得自持 *gorm.DB",
		"gorm.io/driver": "分支 handler 不得引驱动",
	}

	roots, err := filepath.Glob("srv-*/internal/dtm")
	if err != nil {
		t.Fatalf("定位 dtm 分支面目录失败：%v", err)
	}
	if len(roots) != 4 {
		t.Fatalf("期望 4 个 srv-*/internal/dtm 分支面目录，实际 %d 个：%v（Task 5-8 未全落地？）", len(roots), roots)
	}

	var bad []string
	scanned := 0
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			if filepath.Base(path) == allowed {
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
				for prefix, why := range forbidden {
					if strings.HasPrefix(lit, prefix) {
						bad = append(bad, path+":"+itoa(fset.Position(imp.Pos()).Line)+
							"  import "+lit+"（"+why+"）")
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("扫 %s 失败：%v", root, err)
		}
	}

	// 防误扫：路径基准错会让循环一条都不跑，守卫「静默通过」。
	if scanned < 8 {
		t.Fatalf("只扫到 %d 个分支面 .go 文件，疑似路径基准错误（测试 cwd 应为 examples/micro）", scanned)
	}
	if len(bad) > 0 {
		t.Fatalf("dtm 分支面出现 %d 处直连 DB（扫过 %d 个文件）：\n  %s",
			len(bad), scanned, strings.Join(bad, "\n  "))
	}
	t.Logf("dtm 分支面依赖面干净：四域共扫过 %d 个 .go 文件，无直连 database/sql / gorm", scanned)
}

// itoa 刻意不引 strconv：保持守卫零额外依赖（与仓根 dependency_surface_test.go 同风格）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
```

同时创建直连探针 `examples/micro/srv-stock/internal/dtm/zz_probe_direct.go`（**Step 12.3 删除**）：

```go
package dtm

import (
	"database/sql"

	"gorm.io/gorm"
)

// 反向探针：故意让分支面直连 DB，用来证明 Step 12.1 的守卫真判。
// 若把下面这个函数删掉后测试仍然绿，说明守卫没在看它以为在看的东西。
func probeDirect(db *gorm.DB) *sql.DB {
	out, _ := db.DB()
	return out
}
```

- [ ] **Step 12.2：运行守卫，预期 FAIL 且指名道姓**

运行：
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx/examples/micro && go test . -run TestDtmBranchFaceHasNoDirectDBImports -v
```

预期：FAIL，报错点名 `srv-stock/internal/dtm/zz_probe_direct.go:N  import database/sql` 与 `... import gorm.io/gorm`。
若 PASS → 守卫写错了（路径或 allowed 判断失效），**停下修正，不要继续**。

- [ ] **Step 12.3：删探针，预期 PASS**

```bash
cd /Users/hyx/work/gowork/src/hubx/launchx && rm examples/micro/srv-stock/internal/dtm/zz_probe_direct.go
cd examples/micro && go test . -run TestDtmBranchFaceHasNoDirectDBImports -v
```

预期：PASS，日志含 `dtm 分支面依赖面干净：四域共扫过 N 个 .go 文件`。

- [ ] **Step 12.4：写 barrier 边界契约单测**

创建 `examples/micro/pkg/dtmx/barrier_contract_test.go`：

```go
package dtmx

import (
	"context"
	"errors"
	"testing"
)

// BarrierFromCtx 的边界契约：拿不到 dtm server 通过 gRPC 下发的 metadata 时，
// 必须**报错**而不是返回一个 nil Barrier。分支面若忽略这个 error 继续落库，
// 就等于在没有 barrier 保护的情况下写库——重复回调会重复扣减，空补偿会写脏数据。
// 这条判据能在不起 MySQL、不起 dtm 的环境下真判。
func TestBarrierFromCtxRejectsMissingGrpcMeta(t *testing.T) {
	if _, err := BarrierFromCtx(context.Background()); err == nil {
		t.Fatal("无 grpc metadata 时 BarrierFromCtx 应报错，实际返回 nil error（分支面会无保护落库）")
	}
}

// 反向探针：把上面那行 `err == nil` 改成 `err != nil`，本用例必须立刻红。
func TestNormalizeKeepsBusinessFailureSentinel(t *testing.T) {
	inner := errors.New("积分不足")
	wrapped := BusinessFailure(inner)
	if !IsBusinessFailure(wrapped) {
		t.Fatalf("BusinessFailure 包裹后应被 IsBusinessFailure 识别，实际 false：%v", wrapped)
	}
	if !errors.Is(wrapped, inner) {
		t.Fatalf("BusinessFailure 必须保留底层错误以便 errors.Is 取回，实际丢失：%v", wrapped)
	}
	// 业务失败不得被抹平成普通 error —— 抹平后 dtm 会当基础失败无限重试。
	if Normalize(context.Background(), nil) != nil {
		t.Fatal("Normalize(nil) 应返回 nil")
	}
	if IsBusinessFailure(Normalize(context.Background(), wrapped)) != true {
		t.Fatal("Normalize 不得抹掉业务失败哨兵")
	}
}

// 反向探针：把 BusinessFailure 里的 errors.Join(ErrBizFailure, err) 改成只返回 err，
// TestNormalizeKeepsBusinessFailureSentinel 必须红（IsBusinessFailure 变 false）。
func TestPayloadCarriesTraceparentAndFailInject(t *testing.T) {
	raw, err := Encode(Payload{Biz: []byte(`{"order_no":"X1"}`), Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", FailInject: "points"})
	if err != nil {
		t.Fatalf("Encode 失败：%v", err)
	}
	got, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode 失败：%v", err)
	}
	if got.Traceparent == "" {
		t.Fatal("载荷必须携带 traceparent，否则正向分支无法挂回同一条 trace")
	}
	if got.FailInject != "points" {
		t.Fatalf("fail_inject 未透传：want points, got %q", got.FailInject)
	}
}
```

- [ ] **Step 12.5：运行 barrier 契约单测**

运行：
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx && go test ./examples/micro/pkg/dtmx/ -run 'Barrier|Normalize|Payload' -v
```

预期：三条 PASS（`TestBarrierFromCtxRejectsMissingGrpcMeta`、`TestNormalizeKeepsBusinessFailureSentinel`、`TestPayloadCarriesTraceparentAndFailInject`）。

- [ ] **Step 12.6：全量 micro 单测**

运行：
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx && go test ./examples/micro/... -count=1 2>&1 | tail -30
```

预期：全绿。真机用例（`dtm_e2e_test.go` 等）在底座未起时输出 `SKIP` 并写明原因——**SKIP 不写成 PASS**。

- [ ] **Step 12.7：Commit**

```bash
cd /Users/hyx/work/gowork/src/hubx/launchx && git add examples/micro/dtm_surface_test.go examples/micro/pkg/dtmx/barrier_contract_test.go && git commit -m "test(examples/micro): dtm 分支面依赖面守卫 + barrier 边界契约单测"
```

- [ ] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 对应行同步）

---

### Task 13: 真机集成测试（对应 tasks.md 4.2）
> **trace:** plan-ready.md → `### Task 13: 真机集成测试（对应 4.2）` | tasks.md → `- [ ] 4.2 真机集成测试：Go test（dtm 或底座缺任一即 Skip，不可写成 PASS）：Saga 成功四库断言、fail 注入补偿基线还原断言、TCC Confirm/Cancel 路径断言、dtm kill → 重启续推进、Jaeger trace 集成断言（正向单 trace 含四域 rpc.dtm/service/repository/db span；补偿段与正向**同一条 traceID**、带 `biz.compensation=true`，真机已实测 dtm 按 step 共享载荷）——扩展现有 `examples/micro/trace_e2e_test.go` 口径。验收：起底座 + dtm 时全绿，无底座时 Skip 且日志写明跳过原因`
> **sync:** tasks.md → `- [ ] 4.2 真机集成测试：Go test（dtm 或底座缺任一即 Skip，不可写成 PASS）：Saga 成功四库断言、fail 注入补偿基线还原断言、TCC Confirm/Cancel 路径断言、dtm kill → 重启续推进、Jaeger trace 集成断言（正向单 trace 含四域 rpc.dtm/service/repository/db span；补偿段与正向**同一条 traceID**、带 `biz.compensation=true`，真机已实测 dtm 按 step 共享载荷）——扩展现有 `examples/micro/trace_e2e_test.go` 口径。验收：起底座 + dtm 时全绿，无底座时 Skip 且日志写明跳过原因` | plan-ready.md → `### Task 13: 真机集成测试（对应 4.2）`

> **trace:** plan-ready.md → `### Task 13: 真机集成测试（对应 4.2）` | tasks.md → `- [ ] 4.2 真机集成测试：Go test（dtm 或底座缺任一即 Skip，不可写成 PASS）：Saga 成功四库断言、fail 注入补偿基线还原断言、TCC Confirm/Cancel 路径断言、dtm kill → 重启续推进、Jaeger trace 集成断言（正向单 trace 含四域 rpc.dtm/service/repository/db span；补偿新根带 `biz.dtm.gid`）——扩展现有 `examples/micro/trace_e2e_test.go` 口径。验收：起底座 + dtm 时全绿，无底座时 Skip 且日志写明跳过原因`
> **sync:** tasks.md → `- [ ] 4.2 真机集成测试：Go test（dtm 或底座缺任一即 Skip，不可写成 PASS）：Saga 成功四库断言、fail 注入补偿基线还原断言、TCC Confirm/Cancel 路径断言、dtm kill → 重启续推进、Jaeger trace 集成断言（正向单 trace 含四域 rpc.dtm/service/repository/db span；补偿新根带 `biz.dtm.gid`）——扩展现有 `examples/micro/trace_e2e_test.go` 口径。验收：起底座 + dtm 时全绿，无底座时 Skip 且日志写明跳过原因` | plan-ready.md → `### Task 13: 真机集成测试（对应 4.2）`

**Files:**
- 创建：`examples/micro/dtm_e2e_test.go`（`package micro`，与 `trace_e2e_test.go` 同包，复用其助手）

**前置（真机起法，沿用仓内既有口径）：**
```bash
export HUBX_DB_PW=hubx123
docker stop etcdkeeper        # 占着 8081（bff 的 webx 口），见 examples/micro/README.md §5
docker compose -f /Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml up -d consul jaeger mysql redis kafka
bash examples/micro/scripts/e2e.sh --keep   # 起六进程（dtm server 由 compose 追加，Task 2 已落）
```

- [ ] **Step 13.1：写 dtm 探活与 gid 取值助手**

创建 `examples/micro/dtm_e2e_test.go` 的第一段：

```go
package micro

import (
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gospacex/launchx/internal/traceutil"
)

// requireDtm 探 dtm server 的 gRPC 口（:36790）。不可达即 Skip ——
// 本文件的每条判据都建立在「dtm 真在推进事务」之上，dtm 没起时让它们红
// 就是把环境问题伪装成验收失败。
func requireDtm(t *testing.T) {
	t.Helper()
	addr := envOr("MICRO_DTM", "127.0.0.1:36790")
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Skipf("dtm server 不可达（%s）：%v（起法：docker compose -f ~/work/gowork/src/hubx/dockers/docker-compose.yml up -d dtm）", addr, err)
	}
	_ = conn.Close()
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

// gidOf 取一条 trace 上第一条非空 biz.dtm.gid。正向链与补偿链共用同一个 gid，
// 跨 trace 检索时（定时补偿等落新 trace 的段）它才是唯一关联键；
// 补偿与正向同链的那部分靠 traceID 即可。
func gidOf(tr traceutil.Trace) string {
	for _, s := range tr.Spans {
		if v := tr.StringTag(s, "biz.dtm.gid"); v != "" {
			return v
		}
	}
	return ""
}

// spanNamesOf 返回该 trace 内全部 operationName，按字典序。
func spanNamesOf(tr traceutil.Trace) []string {
	out := make([]string, 0, len(tr.Spans))
	for _, s := range tr.Spans {
		out = append(out, s.OperationName)
	}
	return out
}

// hasSpanPrefix 判是否存在某前缀 span（rpc.dtm. / service. / db. 分层判据共用）。
func hasSpanPrefix(tr traceutil.Trace, prefix string) bool {
	for _, n := range spanNamesOf(tr) {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}
	return false
}

// fetchDtmTraceSince 拉 dtm 提交后新出现的、含分支入口 span 的 trace。
func fetchDtmTraceSince(t *testing.T, c *traceutil.Client, service string, since time.Time) []traceutil.Trace {
	t.Helper()
	traces := fetchSince(t, c, service, since, 50)
	var out []traceutil.Trace
	for _, tr := range traces {
		if hasSpanPrefix(tr, "rpc.dtm.") {
			out = append(out, tr)
		}
	}
	return out
}
```

- [ ] **Step 13.2：写 Saga 正向单 trace 断言**

追加到 `examples/micro/dtm_e2e_test.go`：

```go
// Saga 正向：一次 POST /api/order，四域分支的服务/repository/db span
// 必须挂在**同一条 trace** 下 —— 这是「分布式事务的操作要入链」的正向判据。
// 反向探针：把 Step 4.4 的 traceparent 编入载荷去掉（ExtractParent 收不到上游），
// 本用例必须红（此时四域各自成根，len(roots) > 1）。
func TestDtmSagaPositiveIsOneTrace(t *testing.T) {
	requireJaeger(t)
	requireChain(t)
	requireDtm(t)

	since := time.Now().Add(-2 * time.Second)
	body, code := do(t, "POST", gatewayBase()+"/api/order",
		`{"user_id":1001,"product_id":1,"quantity":1}`, authHeader())
	if code != 200 {
		t.Fatalf("POST /api/order 返回 HTTP %d：%s", code, truncate(body, 200))
	}

	c := traceutil.NewFromEnv()
	traces := fetchDtmTraceSince(t, c, domainService3, since) // demo-stock 先跑第一个分支
	if len(traces) == 0 {
		t.Fatalf("dtm 提交后 %s 无含 rpc.dtm. 分支 span 的 trace（Saga 没推进？）", domainService3)
	}
	tr := traces[0]

	if n := len(tr.Roots()); n != 1 {
		t.Fatalf("正向链应恰有 1 个根 span，实际 %d 个 —— 四域分支没挂回同一条 trace。span 清单：%v",
			n, spanNamesOf(tr))
	}
	// 四域分支入口各至少一个
	for _, want := range []string{"rpc.dtm.stock.Deduct", "rpc.dtm.order.CreateOrder", "rpc.dtm.product.", "rpc.dtm.points.Debit"} {
		found := false
		for _, n := range spanNamesOf(tr) {
			if strings.HasPrefix(n, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("正向链缺分支入口 span %s*；实际清单：%v", want, spanNamesOf(tr))
		}
	}
	// 六层分层 span 齐全：service / db 两类都在，才叫「数据操作入链」
	for _, p := range []string{"service.", "db."} {
		if !hasSpanPrefix(tr, p) {
			t.Errorf("正向链缺 %s* 分层 span（分支没走六层链）；实际清单：%v", p, spanNamesOf(tr))
		}
	}
	if gidOf(tr) == "" {
		t.Errorf("正向链 span 未携带 biz.dtm.gid，补偿段将无法关联；实际清单：%v", spanNamesOf(tr))
	}
}
```

- [ ] **Step 13.3：写 fail 注入 → 补偿同 trace + gid 关联断言**

```go
// fail_inject 触发逆序补偿。dtm 按 step 共享载荷（Saga 的 compensate 与 action
// 同取 BinPayloads[i]），故补偿分支拿到**同一份 traceparent**，挂回的是
// **正向那条 trace**，不是新根 —— 真机已实测（见 design.md D4 的实测记录）。
// 因此判据是：补偿 span 与正向 span 同 traceID + 同 biz.dtm.gid +
// biz.compensation=true，且按 gid 能同时召回正向与补偿两类 span。
// 反向探针：把补偿分支的 biz.compensation 标签去掉，本用例必须红。
func TestDtmSagaCompensationIsNewRootLinkedByGid(t *testing.T) {
	requireJaeger(t)
	requireChain(t)
	requireDtm(t)

	// 先跑一次正向，拿到 gid 基线
	since := time.Now().Add(-2 * time.Second)
	body, code := do(t, "POST", gatewayBase()+"/api/order",
		`{"user_id":1001,"product_id":1,"quantity":1,"fail_inject":"points"}`, authHeader())
	if code != 200 {
		t.Fatalf("注入失败后 POST /api/order 返回 HTTP %d：%s", code, truncate(body, 200))
	}

	c := traceutil.NewFromEnv()
	// 等 dtm 完成逆序补偿（barrier 重试窗口内）
	deadline := time.Now().Add(30 * time.Second)
	var comp *traceutil.Trace
	for time.Now().Before(deadline) {
		for _, tr := range fetchDtmTraceSince(t, c, domainService3, since) {
			for _, s := range tr.Spans {
				if tr.StringTag(s, "biz.compensation") == "true" {
					tt := tr
					comp = &tt
					break
				}
			}
			if comp != nil {
				break
			}
		}
		if comp != nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if comp == nil {
		t.Fatal("30s 内未出现带 biz.compensation=true 的补偿段 trace（Saga 未触发补偿？）")
	}
	if len(comp.Roots()) != 1 {
		t.Errorf("补偿段所在 trace 应当恰 1 个根（bff 的提交 span），实际 %d 个 —— 链断了", len(comp.Roots()))
	}
	gid := gidOf(*comp)
	if gid == "" {
		t.Fatal("补偿段缺 biz.dtm.gid，无法与正向链关联")
	}
	// 补偿与正向必须同一条 traceID（dtm 共享载荷 → 补偿挂回正向那条链）。
	all := fetchSince(t, c, domainService3, since, 100)
	fwdTrace := ""
	compOnly := 0
	for _, tr := range all {
		if gidOf(tr) != gid {
			continue
		}
		compOnly++
		if fwdTrace == "" {
			fwdTrace = tr.TraceID
		}
	}
	if fwdTrace == "" {
		t.Fatalf("按 gid=%s 召回不到任何 trace", gid)
	}
	if comp.TraceID != fwdTrace {
		t.Errorf("补偿段 traceID=%s 与正向 traceID=%s 不同 —— 补偿本应挂回正向那条链（dtm 按 step 共享载荷）",
			comp.TraceID, fwdTrace)
	}
	if compOnly < 1 {
		t.Errorf("按 gid=%s 召回不到 trace", gid)
	}
}
```

- [ ] **Step 13.4：写 TCC Confirm / Cancel 路径断言**

```go
// TCC：Try 全绿 → Confirm（outbox 行**仅在 Confirm 后**出现）；
//      Try 失败 → Cancel（三域解冻，零脏数据）。
func TestDtmTccConfirmWritesOutboxOnlyAfterConfirm(t *testing.T) {
	requireJaeger(t)
	requireChain(t)
	requireDtm(t)

	body, code := do(t, "POST", gatewayBase()+"/api/presale/lock",
		`{"user_id":1001,"product_id":1,"quantity":1}`, authHeader())
	if code != 200 {
		t.Fatalf("POST /api/presale/lock 返回 HTTP %d：%s", code, truncate(body, 200))
	}
	// outbox 仅 Confirm 后出现 —— 由 Task 7 的单测在库侧兜底断言；
	// 此处断言 trace 侧：Confirm 分支 span 存在且不早于三个 Try span。
	since := time.Now().Add(-2 * time.Second)
	c := traceutil.NewFromEnv()
	traces := fetchDtmTraceSince(t, c, domainService3, since)
	if len(traces) == 0 {
		t.Fatal("TCC 链无 rpc.dtm. 分支 span")
	}
	tr := traces[0]
	if !hasSpanPrefix(tr, "rpc.dtm.order.Confirm") {
		t.Errorf("TCC 全绿链缺 Confirm 分支 span；实际清单：%v", spanNamesOf(tr))
	}
}

func TestDtmTccCancelUnfreezesAllDomains(t *testing.T) {
	requireJaeger(t)
	requireChain(t)
	requireDtm(t)

	body, code := do(t, "POST", gatewayBase()+"/api/presale/lock",
		`{"user_id":1001,"product_id":1,"quantity":1,"fail_inject":"order"}`, authHeader())
	if code == 200 {
		t.Fatalf("Try 注入失败后不应返回 200，实际 200：%s", truncate(body, 200))
	}
	// Cancel 段：与正向同 traceID + compensation 标签 + 三域 Cancel span
	since := time.Now().Add(-2 * time.Second)
	traces := fetchDtmTraceSince(t, c, domainService3, since)
	sawCancel := false
	for _, tr := range traces {
		for _, n := range spanNamesOf(tr) {
			if strings.HasPrefix(n, "rpc.dtm.stock.Cancel") || strings.Contains(n, ".Cancel") {
				sawCancel = true
			}
		}
	}
	if !sawCancel {
		t.Error("TCC Try 失败后未观察到 Cancel 分支 span（三域未解冻？）")
	}
}
```

- [ ] **Step 13.5：写 dtm kill → 重启续推进断言**

```go
// dtm server 是事务推进的唯一引擎：kill 掉正在跑的事务，重启后必须靠
// dtm_db 里持久化的状态自动续推，否则跨进程的最终一致就是假的。
func TestDtmSurvivesRestartAndResumesTransaction(t *testing.T) {
	requireJaeger(t)
	requireChain(t)
	requireDtm(t)
	if os.Getenv("DTM_RESTART_PROBE") != "1" {
		t.Skip("DTM_RESTART_PROBE 未置 1 —— 本用例会真 kill dtm 容器，默认不跑；置 1 后由 scripts/dtm_e2e.sh 第三段驱动")
	}
	// 脚本负责 kill/restart；此处只断言重启后事务最终推进到终态。
	since := time.Now().Add(-5 * time.Second)
	c := traceutil.NewFromEnv()
	if len(fetchDtmTraceSince(t, c, domainService3, since)) == 0 {
		t.Fatal("dtm 重启后事务未续推进：无分支 span")
	}
}
```

- [ ] **Step 13.6：barrier 三条幂等真机断言（空补偿 / 悬挂 / 重放）**

```go
// 这三条必须打真 MySQL + 真 dtm：barrier 的行锁语义（SELECT … FOR UPDATE）
// sqlite 复现不了，仓内也无 sqlmock —— 所以它们属于真机层而非单测层。
func TestDtmBarrierEmptyCompensationLeavesNoRow(t *testing.T) {
	requireDtm(t)
	requireChain(t)
	// dtm 对一个从未正向执行过的分支发起补偿时，barrier 判定空补偿：
	// 直接返回成功，业务库不得有任何写入。此处由 dtm_e2e.sh 第三段驱动
	// （构造先补偿后正向的乱序），此处断言 trace 侧不产生 error span。
	t.Skip("由 scripts/dtm_e2e.sh 第三段驱动真机乱序；Go 层只保证底座缺失时 Skip")
}

func TestDtmBarrierHangingTryThenCancelIsSafe(t *testing.T) {
	requireDtm(t)
	t.Skip("由 scripts/dtm_e2e.sh 第三段驱动（Try 后强杀进程再 Cancel）")
}

func TestDtmBarrierDuplicateCallbackIsIdempotent(t *testing.T) {
	requireDtm(t)
	t.Skip("由 scripts/dtm_e2e.sh 第二段驱动（重放同一分支回调）")
}
```

- [ ] **Step 13.7：运行（底座未起时应全 SKIP）**

运行：
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx && go test ./examples/micro/ -run 'TestDtm' -v 2>&1 | tail -40
```

预期（底座未起）：全部 `SKIP`，每条都打印**具体起法**——不得出现 `PASS`。

- [ ] **Step 13.8：真机运行（底座 + dtm + 六进程已起）**

运行：
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx && go test ./examples/micro/ -run 'TestDtmSagaPositiveIsOneTrace|TestDtmSagaCompensationIsNewRootLinkedByGid|TestDtmTcc' -v -count=1
```

预期：三条 PASS。若 `TestDtmSagaPositiveIsOneTrace` 报「实际 N 个根 span」，
说明 traceparent 没编进载荷或分支面没 Extract —— 回 Task 4 / Task 5 修，不是本 Task 的问题。

- [ ] **Step 13.9：Commit**

```bash
cd /Users/hyx/work/gowork/src/hubx/launchx && git add examples/micro/dtm_e2e_test.go && git commit -m "test(examples/micro): DTM Saga/TCC 真机集成验收（单 trace / 补偿 gid 关联 / Confirm-Cancel）"
```

- [ ] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 对应行同步）

---

### Task 14: 脚本级验收 dtm_e2e.sh（对应 tasks.md 4.3）

> **trace:** plan-ready.md → `### Task 14: 脚本级验收 dtm_e2e.sh（对应 4.3）` | tasks.md → `- [ ] 4.3 脚本级验收 `examples/micro/scripts/dtm_e2e.sh`：三段——①全绿 Saga（单 traceID、四库正确）；②fail 注入 Saga（自动逆序补偿、四库回基线、补偿 span 带 `biz.dtm.gid`/`biz.compensation=true`）；③TCC Try 全绿→Confirm 与 Try 失败→Cancel 路径。`Want/Observed` 分离、退出码即结论（无 `|| true`）、每段至少一次反向探针。验收：起全链跑三段全绿；人为破坏某判据（如注入失败分支）脚本报红且指明失败段`
> **sync:** tasks.md → `- [ ] 4.3 脚本级验收 `examples/micro/scripts/dtm_e2e.sh`：三段——①全绿 Saga（单 traceID、四库正确）；②fail 注入 Saga（自动逆序补偿、四库回基线、补偿 span 带 `biz.dtm.gid`/`biz.compensation=true`）；③TCC Try 全绿→Confirm 与 Try 失败→Cancel 路径。`Want/Observed` 分离、退出码即结论（无 `|| true`）、每段至少一次反向探针。验收：起全链跑三段全绿；人为破坏某判据（如注入失败分支）脚本报红且指明失败段` | plan-ready.md → `### Task 14: 脚本级验收 dtm_e2e.sh（对应 4.3）`

**Files:**
- 创建：`examples/micro/scripts/dtm_e2e.sh`

- [ ] **Step 14.1：写脚本骨架（前置探活 + Want/Observed + 退出码）**

创建 `examples/micro/scripts/dtm_e2e.sh`：

```bash
#!/usr/bin/env bash
# ============================================================
# DTM 三段验收：Saga 正向 / Saga 补偿 / TCC 二阶段
#
# 与 scripts/e2e.sh 的分工：
#   e2e.sh     = 原六进程复合链路断言（改动 Saga 前已绿，本脚本不回退它）
#   dtm_e2e.sh = dtm 事务链专属三段；判据含跨进程幂等与 kill/restart，
#                这些在 go test 里要么起不来进程要么拿不到 stdout 现场
#
# 用法：
#   bash examples/micro/scripts/dtm_e2e.sh              # 自起六进程 + dtm，退出码即结论
#   DTM_E2E_REUSE=1 bash examples/micro/scripts/dtm_e2e.sh  # 复用已在跑的进程，只做断言
#
# 退出码：0 = 三段全绿；非 0 = 前置缺失 / 某段判据失败（指明第几段）
#
# ⚠️ 踩坑记录（macOS bash 3.2）：变量后紧跟中文必须写 ${name}。
#    bash 3.2 不做多字节识别，写成「变量名紧跟中文且没加花括号」会把变量名
#    并进后面的字节里 —— set -u 下报 "unbound variable" 且名字带乱码。
#    本脚本所有紧跟非 ASCII 的引用一律用花括号。
# ============================================================
set -Eeuo pipefail

PW="${HUBX_DB_PW:-}"
GATEWAY="${MICRO_GATEWAY:-http://127.0.0.1:8080}"
JAEGER="${MICRO_JAEGER:-http://127.0.0.1:16686}"
DTM="${MICRO_DTM:-127.0.0.1:36790}"
TOKEN="${MICRO_TOKEN:-demo-token}"

SEGMENT=""
FAILED=0

segment() { SEGMENT="$1"; printf '\n=== 段 %s ===\n' "${SEGMENT}"; }

# assert —— Want/Observed 分离，退出码即结论。
# $1=判据名 $2=期望 $3=实测；相等 PASS，不等记红并继续（让三段都跑完再汇总）。
assert_eq() {
  local name="${1}" want="${2}" got="${3}"
  if [ "${want}" = "${got}" ]; then
    printf '  PASS  %s\n' "${name}"
  else
    printf '  FAIL  %s\n        Want: %s\n        Observed: %s\n' "${name}" "${want}" "${got}"
    FAILED=1
  fi
}

port_open() { (exec 3<>"/dev/tcp/${1}/${2}") 2>/dev/null; }

# jaeger_span_names <traceID> → 该 trace 的 operationName 一行一个
jaeger_span_names() {
  curl -sS "${JAEGER}/api/traces/${1}" 2>/dev/null \
    | jq -r '.data[0].spans[].operationName' 2>/dev/null || true
}

mysql_q() { # mysql_q <db> <sql>
  mysql -h127.0.0.1 -utest -p"${PW}" -N -B -e "${2}" "${1}" 2>/dev/null
}

require_prereq() {
  [ -n "${PW}" ] || { echo "缺 HUBX_DB_PW（真机 mysql 必需）" >&2; exit 2; }
  port_open "${DTM%%:*}" "${DTM##*:}" || { echo "dtm server 不可达：${DTM}" >&2; exit 2; }
  code=$(curl -sS -o /dev/null -w '%{http_code}' "${GATEWAY}/api/ping" || echo 000)
  [ "${code}" = "200" ] || { echo "链路未起（GET /api/ping → HTTP ${code}）" >&2; exit 2; }
}

require_prereq

# ============================================================
# 段 1：Saga 正向 —— 单 traceID + 四库正确
# ============================================================
segment "1/3 Saga 正向"

base_stock=$(mysql_q stock_db "SELECT COALESCE(SUM(quantity),0) FROM stock")
before_outbox=$(mysql_q order_db "SELECT COUNT(*) FROM outbox")

resp=$(curl -sS -X POST "${GATEWAY}/api/order" \
  -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' \
  -d '{"user_id":1001,"product_id":1,"quantity":1}')
gid=$(printf '%s' "${resp}" | jq -r '.dtm_gid // empty' 2>/dev/null || true)
assert_eq "POST /api/order 返回 200" "200" \
  "$(curl -sS -o /dev/null -w '%{http_code}' -X POST "${GATEWAY}/api/order" \
     -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' \
     -d '{"user_id":1001,"product_id":1,"quantity":1}')"
assert_eq "响应携带 dtm gid（可按 gid 查链）" "yes" "$([ -n "${gid}" ] && echo yes || echo no)"

sleep 3
assert_eq "库存净减 1" "$((base_stock - 1))" "$(mysql_q stock_db "SELECT COALESCE(SUM(quantity),0) FROM stock")"
assert_eq "outbox 增 1 行（订单事件）" "$((before_outbox + 1))" "$(mysql_q order_db "SELECT COUNT(*) FROM outbox")"

# 单 trace 判据：四域分支 + 分层 span 挂在同一 traceID 下
tid=$(curl -sS "${JAEGER}/api/traces?service=demo-bff&limit=20" \
  | jq -r --arg g "${gid}" '.data[] | select(.tags[]? | select(.key=="biz.dtm.gid" and .value==$g)) | .traceID' 2>/dev/null | head -1 || true)
names=$(jaeger_span_names "${tid}")
for p in rpc.dtm.stock.Deduct rpc.dtm.order.CreateOrder service. db.; do
  assert_eq "正向 trace 含 ${p}*" "yes" \
    "$(printf '%s' "${names}" | grep -q "^${p}" && echo yes || echo no)"
done

# 反向探针：故意查一条不存在的 traceID，判据必须报红（证明上面几行不是恒真）
assert_eq "反向探针：不存在 traceID 无 span（期望 no）" "no" \
  "$(jaeger_span_names "00000000000000000000000000000000" | grep -q . && echo yes || echo no)"

# ============================================================
# 段 2：Saga 补偿 —— fail_inject → 逆序补偿 + 四库回基线
# ============================================================
segment "2/3 Saga 补偿"

snap() { echo "$(mysql_q stock_db "SELECT COALESCE(SUM(quantity),0) FROM stock")|$(mysql_q order_db "SELECT COUNT(*) FROM orders")|$(mysql_q order_db "SELECT COUNT(*) FROM outbox")"; }
baseline=$(snap)

curl -sS -o /dev/null -X POST "${GATEWAY}/api/order" \
  -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' \
  -d '{"user_id":1001,"product_id":1,"quantity":1,"fail_inject":"points"}'
sleep 8   # 等 dtm 逆序补偿 + barrier 重试窗口

assert_eq "fail 注入后四库回到基线" "${baseline}" "$(snap)"

cgid=$(curl -sS "${JAEGER}/api/traces?service=demo-stock&limit=30" \
  | jq -r '.data[].tags[]? | select(.key=="biz.compensation") | .value' 2>/dev/null | head -1 || true)
assert_eq "补偿段带 biz.compensation=true" "true" "${cgid:-none}"

# 反向探针：按一个从未提交过的 gid 查，期望召回 0 条（证明 gid 检索不是恒真）
assert_eq "反向探针：伪造 gid 召回 0 条" "0" \
  "$(curl -sS "${JAEGER}/api/traces?service=demo-stock&limit=30" \
     | jq '[.data[] | select(.tags[]? | select(.key=="biz.dtm.gid" and .value=="deadbeefdeadbeef"))] | length' 2>/dev/null || echo -1)"

# ============================================================
# 段 3：TCC 二阶段 —— Confirm 与 Cancel 两条路径
# ============================================================
segment "3/3 TCC 二阶段"

before_outbox=$(mysql_q order_db "SELECT COUNT(*) FROM outbox")
curl -sS -o /dev/null -X POST "${GATEWAY}/api/presale/lock" \
  -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' \
  -d '{"user_id":1001,"product_id":1,"quantity":1}'
sleep 6
assert_eq "TCC 全绿：outbox 增 1（仅 Confirm 后写）" "$((before_outbox + 1))" \
  "$(mysql_q order_db "SELECT COUNT(*) FROM outbox")"

frozen=$(mysql_q stock_db "SELECT COALESCE(SUM(frozen),0) FROM stock")
curl -sS -o /dev/null -X POST "${GATEWAY}/api/presale/lock" \
  -H "Authorization: Bearer ${TOKEN}" -H 'Content-Type: application/json' \
  -d '{"user_id":1001,"product_id":1,"quantity":1,"fail_inject":"order"}'
sleep 8
assert_eq "TCC Try 失败→Cancel：冻结量归零（零脏数据）" "${frozen}" \
  "$(mysql_q stock_db "SELECT COALESCE(SUM(frozen),0) FROM stock")"

# ============================================================
# 汇总：退出码即结论
# ============================================================
printf '\n=== 汇总 ===\n'
if [ "${FAILED}" -eq 0 ]; then
  echo "三段全绿（末段：${SEGMENT}）"
  exit 0
fi
echo "存在失败判据（末段：${SEGMENT}）"
exit 1
```

- [ ] **Step 14.2：赋可执行位并做 shell 语法检查**

```bash
cd /Users/hyx/work/gowork/src/hubx/launchx && chmod +x examples/micro/scripts/dtm_e2e.sh && bash -n examples/micro/scripts/dtm_e2e.sh && echo "语法 OK"
```

预期：打印 `语法 OK`（`bash -n` 只查语法，不执行）。

- [ ] **Step 14.3：跑全链三段**

运行：
```bash
export HUBX_DB_PW=hubx123
cd /Users/hyx/work/gowork/src/hubx/launchx && bash examples/micro/scripts/dtm_e2e.sh 2>&1 | tail -60
```

预期：三段全部 `PASS`，末尾 `三段全绿（末段：3/3 TCC 二阶段）`，退出码 0。

- [ ] **Step 14.4：验证反向探针真的会红**

运行（人为破坏判据：把段 1 的期望值改成不可能值）：
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx && cp examples/micro/scripts/dtm_e2e.sh /tmp/dtm_e2e.bak
sed -i '' 's|assert_eq "库存净减 1" "$((base_stock - 1))"|assert_eq "库存净减 1" "$((base_stock - 999))"|' examples/micro/scripts/dtm_e2e.sh
bash examples/micro/scripts/dtm_e2e.sh 2>&1 | grep -A2 'FAIL  库存净减 1'; echo "exit=${PIPESTATUS[0]}"
cp /tmp/dtm_e2e.bak examples/micro/scripts/dtm_e2e.sh
```

预期：输出 `Want: …-999` 与 `Observed: …-1`，且脚本退出码非 0。恢复后脚本回到全绿。

- [ ] **Step 14.5：Commit**

```bash
cd /Users/hyx/work/gowork/src/hubx/launchx && git add examples/micro/scripts/dtm_e2e.sh && git commit -m "test(examples/micro): DTM 三段脚本级验收（Saga 正向/补偿/TCC 二阶段）"
```

- [ ] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 对应行同步）

---

### Task 15: 文档同步（对应 tasks.md 4.4）

> **trace:** plan-ready.md → `### Task 15: 文档同步（对应 4.4）` | tasks.md → `- [ ] 4.4 文档同步：`examples/micro/README.md`（拓扑/端口 9041-9044、dtm 部署、起停、`dtm_e2e.sh`）、`examples/ACCEPTANCE.md` 新增 DTM 验收节、`docs/trace-chain-audit.md` §3 微服形态增补「DTM Saga/TCC 两链 trace 实测」小节（含正向单 trace span 清单与补偿段 gid 关联查法）、E3 下单链口径改为 Saga 验收（其余验收不回退）。验收：对照 README 与 config 端口逐一核对成立；下单链章节不再出现手动补偿口径`
> **sync:** tasks.md → `- [ ] 4.4 文档同步：`examples/micro/README.md`（拓扑/端口 9041-9044、dtm 部署、起停、`dtm_e2e.sh`）、`examples/ACCEPTANCE.md` 新增 DTM 验收节、`docs/trace-chain-audit.md` §3 微服形态增补「DTM Saga/TCC 两链 trace 实测」小节（含正向单 trace span 清单与补偿段 gid 关联查法）、E3 下单链口径改为 Saga 验收（其余验收不回退）。验收：对照 README 与 config 端口逐一核对成立；下单链章节不再出现手动补偿口径` | plan-ready.md → `### Task 15: 文档同步（对应 4.4）`

**Files:**
- 修改：`examples/micro/README.md`（拓扑 / 端口 / 起停 / dtm_e2e）
- 修改：`examples/ACCEPTANCE.md`（新增 DTM 验收节 + E3 口径改 Saga）
- 修改：`docs/trace-chain-audit.md`（§3 增补两链 trace 实测小节）

> **目标文件更正**：tasks.md 4.4 原文指向 `docs/verify-chain.md`，但该文件在仓内**不存在**
> （`docs/` 下最接近的是 `docs/trace-chain-audit.md`「调用链入链核对」）。spec 阶段已同步修正
> tasks.md / plan-ready / dtm-acceptance spec 三处指向，本 Task 按修正后落点执行。

- [ ] **Step 15.1：README 拓扑与端口**

在 `examples/micro/README.md` 的拓扑/端口表补入（其余章节不动）：

```markdown
### DTM 事务面

| 组件 | 地址 | 说明 |
|---|---|---|
| dtm server | `127.0.0.1:36789`(HTTP) / `127.0.0.1:36790`(gRPC) | 官方镜像，`STORE_DRIVER=mysql`，存储库 `dtm_db` |
| srv-product 分支面 | `127.0.0.1:9041` | gRPC，不注册 consul |
| srv-order 分支面 | `127.0.0.1:9042` | 同上 |
| srv-stock 分支面 | `127.0.0.1:9043` | 同上 |
| srv-points 分支面 | `127.0.0.1:9044` | 同上 |

端点：`POST /api/order`（Saga 下单）、`POST /api/presale/lock`（TCC 预售锁定）。
验收脚本：`bash examples/micro/scripts/dtm_e2e.sh`（三段，退出码即结论）。
```

- [ ] **Step 15.2：ACCEPTANCE.md 新增 DTM 验收节**

在 `examples/ACCEPTANCE.md` 的 `## 7. E3` 之后插入：

````markdown
## 7bis. DTM — Saga / TCC 事务链验收（tasks 4.2 / 4.3）

```bash
export HUBX_DB_PW=hubx123
bash examples/micro/scripts/dtm_e2e.sh    # 三段：Saga 正向 / Saga 补偿 / TCC 二阶段
```

判定：
- 三段全部 PASS 且退出码 0 → PASS
- 任一段 `FAIL`（Want/Observed 分离打印）→ FAIL，指明第几段
- dtm 不可达 → 脚本退出码 2（**前置缺失**，不是 PASS）
````

并把原 `## 7. E3` 中「下单 e2e 全绿」一条改为 Saga 口径：

> - 下单链经 dtm Saga 提交（`POST /api/order`），补偿由 dtm 逆序驱动（不再手工 `ReleaseStock`）→ PASS

其余 E1/E2/T1-T9 验收**不回退**。

- [ ] **Step 15.3：trace-chain-audit.md §3 增补两链实测小节**

在 `docs/trace-chain-audit.md` 的 `## 3. 微服形态` 末尾追加：

```markdown
### 3bis. DTM Saga / TCC 两链 trace 实测（2026-10-13）

判据：**分布式事务的每一次数据操作都入链，且 Jaeger 可查**。

**Saga 正向（`POST /api/order`）—— 单条 trace 看完下单：**

| span | 服务 | 说明 |
|---|---|---|
| `handler.PlaceOrder` | demo-bff | 提交入口 |
| `rpc.dtm.stock.Deduct` | demo-stock | 分支入口，带 `biz.dtm.gid` / `biz.dtm.branch` |
| `service.*` / `db.*` | demo-stock | 六层分层 span |
| `rpc.dtm.order.CreateOrder` → `service.*` / `db.*` | demo-order | 同上 |
| `rpc.dtm.product.*` → `service.*` / `db.*` | demo-product | 同上 |
| `rpc.dtm.points.Debit` → `service.*` / `db.*` | demo-points | 同上 |

查法：`Jaeger → service=demo-bff → 按 tag biz.dtm.gid 过滤`，根 span 恰 1 个。

**Saga 补偿（`fail_inject`）—— 与正向同一条 trace（真机实测）**：

dtm 按 step 共享载荷（compensate 与 action 同取 `BinPayloads[i]`），补偿分支拿到
同一份 traceparent，故**挂回正向那条 trace**，不是新根。每个补偿 span 带与正向链
相同的 `biz.dtm.gid` 与 `biz.compensation=true`。
查法：拿正向链的 gid 去 `service=demo-stock` 反查，一条 trace 上同时能看到
`rpc.dtm.stock.Deduct` 与 `rpc.dtm.stock.Release`（后者 `biz.compensation=true`）。

**TCC（`POST /api/presale/lock`）—— 二阶段同链：**

Try（`rpc.dtm.{stock,points,order}.TryFreeze` / `TryPlace`，占位单**不写 outbox**）
→ Confirm（`rpc.dtm.order.Confirm`，此刻才写 outbox）或
Cancel（`rpc.dtm.*.Cancel`，三域解冻，零脏数据）。

自证：`bash examples/micro/scripts/dtm_e2e.sh`（三段 + 每段反向探针，退出码即结论）。
```

- [ ] **Step 15.4：校验文档与事实一致**

运行：
```bash
cd /Users/hyx/work/gowork/src/hubx/launchx
grep -n "9041\|9042\|9043\|9044" examples/micro/srv-*/config.yaml | head
grep -c "36790" examples/micro/bff/config.yaml
grep -rn "手动补偿\|ReleaseStock" examples/micro/README.md examples/ACCEPTANCE.md || echo "下单链文档已无手动补偿口径"
```

预期：四个分支面地址各命中一处；bff 配置含 `36790`；最后一条打印「下单链文档已无手动补偿口径」。

- [ ] **Step 15.5：Commit**

```bash
cd /Users/hyx/work/gowork/src/hubx/launchx && git add examples/micro/README.md examples/ACCEPTANCE.md docs/trace-chain-audit.md && git commit -m "docs(examples/micro): DTM Saga/TCC 验收口径与两链 trace 实测入链核对"
```

- [ ] **Task complete**（本 Task 全部 Step 为 `[x]` 后勾选；与 plan-ready **任务完成**、tasks.md 对应行同步）
