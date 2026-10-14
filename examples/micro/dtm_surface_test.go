package micro

// dtm 分支面的**依赖面与产物落点**静态守卫（tasks.md 4.1a，specs/dtm-branch-facade
// 「四域服 SHALL 各新增轻量 gRPC 分支面且业务仍走六层链」/「新增 proto 不进 kitex_gen」）。
//
// 为什么这两条必须是静态守卫而不是动态断言：spec 把它们写成对**源码形态**的
// 要求 —— 「对分支面源码 grep 无 gorm.Open/dbx 直连调用点」与「dtm 分支 pb 生成
// 产物只落在 internal/dtm/proto/」。动态面（span 清单、四库对账）能证明「这一次
// 走了六层链」，证明不了「**永远不许**绕过」：某天有人在分支 handler 里加一句
// 自持连接的落库，真机验收照样全绿，因为那一行只在没被触发的路径上。
//
// 为什么用 go/parser 而不是文本 search：仓内已经因为文本 search 栽过一次 ——
// examples_addr_hygiene_test.go 的文件头记着，源码里 30 处 "本包不 gorm.Open"
// 全是注释，真调用零处，于是「按文档跑一次 grep」的人认定这条坏了，从此不再看它。
// 恒定输出噪声的判据比失效的判据更糟。AST 里注释不是节点，import 是节点，
// 从根上不存在「命中注释」这一类假信号。
//
// 本文件**只管形态**，不碰任何业务语义；失败语义的一致性归 dtm_failsem_test.go。

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// dtmDomains 四域。写死而不是从目录里发现：发现来的目录在「整个域被误删」时
// 会让守卫跟着少扫一个域而**照样判绿** —— 那正是这类守卫最坏的失败方式。
var dtmDomains = []string{"product", "order", "stock", "points"}

// branchForbiddenImports 分支面源码不许出现的 import 前缀。
//
// 为什么连 "gorm.io/driver" 一起禁：驱动是「自持连接」的第一步。先 import 了
// 驱动再没调用 gorm.Open 是死代码，但它是通往 gorm.Open 的半步；把半步也挡住，
// 绕开这条路就必须先删掉这个 import，于是绕开这件事在 diff 里是显式的。
var branchForbiddenImports = []struct {
	prefix string
	why    string
}{
	{"database/sql", "分支 handler 必须经 service → repository/dtmstore 落库，不得自持 *sql.DB"},
	{"gorm.io/gorm", "分支 handler 必须经 service → repository/dtmstore 落库，不得自持 *gorm.DB"},
	{"gorm.io/driver", "分支面不得引驱动：自持连接的第一步"},
}

// TestDtmBranchFaceHasNoDirectDBImports 判据一：四域分支面（srv-*/internal/dtm/）
// 的**非测试**源码不得 import database/sql / gorm.io/gorm / gorm.io/driver。
//
// 理由（为什么这条红线真实存在，而不是洁癖）：分支 handler 一旦自己拿到 DB 句柄，
// 它就同时绕开了两样东西 ——
//  1. **service 层唯一的业务判断层**（库存不足/积分不足/账不平 → 业务失败 → dtm
//     立刻逆序补偿；绕开后这类失败会变成基础失败，被 dtm 按退避无限重试）；
//  2. **dtm 官方 barrier 包裹**（屏障行与业务写必须同事务；绕开后重复回调会
//     重复扣减，而当场没有任何报错 —— 账照样落，只是被记了两遍）。
//
// 这两种绕开 grep「有没有 gorm.Open(」是**看不出来**的（`db.Where(...)` 一样是落库），
// 只有 import 面看得见：句柄总得从某处来。
//
// 已实测会红的反向探针（改任一条，本用例必须报红并指名文件与行号）：
//
//	① 在 examples/micro/srv-stock/internal/dtm/ 下新建一个非测试 .go，
//	   import "database/sql" 与 "gorm.io/gorm" 并起一个接收 *gorm.DB 的函数
//	   （探针文件：srv-stock/internal/dtm/zz_probe_direct.go）。
//	   → 实测报红：「dtm 分支面出现 2 处直连 DB … srv-stock/internal/dtm/zz_probe_direct.go:8 import database/sql …」
//
//	② 把 srv-order/internal/dtm/busi_server.go 的 `import "encoding/json"`
//	   改成 `import _ "gorm.io/driver/mysql"`（只 import、不调用，判据仍须红）。
//	   → 已实测报红：判据看的是 import 面，不是调用面。
//
// 关于**测试替身目录**：srv-points/internal/dtmstub 与 srv-stock|order/internal/dbmock
// 是跨包复用的纯 Go 假 database/sql 驱动，必须 gorm.Open 一个内存驱动才跑得起来。
// 本守卫**不需要**为它们开口子 —— 它们根本不在作用域内：`dtmBranchFaceDirs` 用
// filepath.Glob("srv-*/internal/dtm") 取的是**精确目录名**，而 dtmstub / dbmock 是
// 与 internal/dtm 平级的另一个目录名，天然不匹配。写成前缀匹配就会把它们拖进来，
// 这里刻意不这么写。与之配套的「测试替身不得进生产源码」由仓级
// examples_addr_hygiene_test.go 的 TestTestDoublesAreNotImportedByProductionCode 守着。
func TestDtmBranchFaceHasNoDirectDBImports(t *testing.T) {
	dirs := dtmBranchFaceDirs(t)

	var bad []string
	scanned := 0
	for _, root := range dirs {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil // 见 TestDtmBranchFaceTestsOnlyTouchFakeDB —— 那条是这个洞的封口
			}
			scanned++

			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if perr != nil {
				// 解析失败不静默放过：那意味着这条守卫没在看它以为在看的文件。
				bad = append(bad, path+": 解析失败 "+perr.Error())
				return nil
			}
			for _, imp := range f.Imports {
				lit := strings.Trim(imp.Path.Value, `"`)
				for _, rule := range branchForbiddenImports {
					if strings.HasPrefix(lit, rule.prefix) {
						bad = append(bad, fmt.Sprintf("%s:%d  import %s（%s）",
							path, fset.Position(imp.Pos()).Line, lit, rule.why))
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("扫 %s 失败：%v", root, err)
		}
	}

	// 防误扫：路径基准错（测试不在 examples/micro 下跑）会让循环一条都不跑，
	// 守卫「静默通过」。四域各 2 个非测试文件 = 8 条下限，比它低就是没在看。
	if scanned < 8 {
		t.Fatalf("只扫到 %d 个分支面非测试 .go 文件（下限 8 = 四域各 2），疑似路径基准错误"+
			"（测试须在 examples/micro 下跑，cwd 目前疑似不对）：%v", scanned, dirs)
	}
	if len(bad) > 0 {
		t.Fatalf("dtm 分支面出现 %d 处直连 DB（扫过 %d 个文件）：分支 handler 自持句柄即绕开 "+
			"service 业务判断与 dtm barrier 包裹，而这种绕开只有 import 面看得见：\n  %s",
			len(bad), scanned, strings.Join(bad, "\n  "))
	}
	t.Logf("分支面依赖面干净：四域共扫过 %d 个非测试 .go 文件，无直连 database/sql / gorm", scanned)
}

// TestDtmBranchFaceTestsOnlyTouchFakeDB 封住上一条判据给 _test.go 开的那个洞。
//
// 上一条跳过 _test.go 的理由是「测试代码不会在真机上落库」—— 但这句话只在
// 它碰的是**假驱动**时成立。今天 srv-order / srv-stock 的 busi_server_test.go
// import gorm.io/gorm 是为了 `&gorm.DB{Config: &gorm.Config{ConnPool: db.SQL()}}`
// 把 dbmock 的内存句柄塞进 service；哪天有人顺手改成一���真 DSN，这条判据就
// 变成了「测试里连真库随便扣」，而判据全绿。
//
// 故把跳过收窄成一条可判的纪律：**分支面目录下的 _test.go 可以 import
// database/sql / gorm.io/gorm，但必须同时 import 测试替身包（dbmock / dtmstub），
// 且永远不许 import 真实驱动（gorm.io/driver/**）**。
//
// 已实测会红的反向探针：把 srv-order/internal/dtm/busi_server_test.go 里的
// "github.com/gospacex/launchx/examples/micro/srv-order/internal/dbmock" 删掉
// （保留 gorm 的 import）→ 本用例报红。
func TestDtmBranchFaceTestsOnlyTouchFakeDB(t *testing.T) {
	fakeDoubles := []string{"internal/dbmock", "internal/dtmstub"}
	realDrivers := []string{"gorm.io/driver", "github.com/go-sql-driver/mysql"}

	var bad []string
	checked := 0
	for _, root := range dtmBranchFaceDirs(t) {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, "_test.go") {
				return nil
			}
			checked++
			imports := importPathsOf(t, path)

			for _, drv := range realDrivers {
				if hasPrefixOf(imports, drv) {
					bad = append(bad, path+": import "+matched(imports, drv)+
						"（分支面测试只许用内存假驱动；连真库会让本目录的单测变成一个能改真数据的入口）")
				}
			}
			touchesDB := hasPrefixOf(imports, "database/sql") || hasPrefixOf(imports, "gorm.io/gorm")
			if !touchesDB {
				return nil
			}
			for _, fake := range fakeDoubles {
				// 用 Contains 而不是 HasPrefix：替身包的 import 路径带完整模块前缀
				// （…/srv-order/internal/dbmock），前缀匹配永远不命中 —— 一条恒假的
				// 判据比没有判据更糟，它会让人以为这条护栏在生效。
				if containsAny(imports, fake) {
					return nil // 假驱动在场：gorm 句柄连的是内存表，够格
				}
			}
			bad = append(bad, path+": import 了 database/sql / gorm.io/gorm 却没 import 测试替身"+
				"（dbmock / dtmstub）—— 这条 import 的去处不明，且一旦接上真驱动就绕过了"+
				"TestDtmBranchFaceHasNoDirectDBImports 对生产源码的红线")
			return nil
		})
		if err != nil {
			t.Fatalf("扫 %s 失败：%v", root, err)
		}
	}

	// 当前仓里只有 order / stock 两个域的 busi_server_test.go 碰 DB；这条下限只是
	// 「循环确实跑起来了」，不是「每个域都得有」。
	if checked < 2 {
		t.Fatalf("只扫到 %d 个分支面 _test.go（下限 2），疑似路径基准错误：%v", checked, dtmBranchFaceDirs(t))
	}
	if len(bad) > 0 {
		t.Fatalf("分支面测试文件的 DB 依赖不合规（%d 处）：\n  %s", len(bad), strings.Join(bad, "\n  "))
	}
	t.Logf("分支面测试只碰假驱动：扫过 %d 个 _test.go，真实驱动命中 0", checked)
}

// TestDtmBarrierWriteChannelLivesInRepositoryLayer 判据一的**配对正向判据**：
// 分支落库的唯一通道是各域 internal/repository/dtmstore.go，而它确实拿着
// *sql.DB 与 dtmx.Barrier。
//
// 为什么要这条：上一条守卫（「分支面不许 import database/sql」）的合法解释有两条 ——
// 「落库真的经 service→repository」与「**这个功能压根还没实现**」。两条都让它判绿。
// 少一条正向判据，守卫就会在实现被整体删掉时依然全绿，那是把「禁止绕过」写成
// 「不许存在」的典型失真。
//
// **这里同时钉住一处计划口径的偏差**：plan part5 Step 12.1 把 allowed 集合写成
// 「srv-*/internal/dtm/ 下除 store.go 以外的 .go 文件」，而实际形态是 ——
// internal/dtm/ 下**根本没有** store.go，barrier 写通道落在
// internal/repository/dtmstore.go（四域皆然），它在守卫作用域之外。
// 故真实守卫不需要任何 allowed 集合；把计划里那份 allowed 照抄进代码，会得到一个
// 指向不存在文件的空豁免，看起来有豁免、实则永远命中不到。
func TestDtmBarrierWriteChannelLivesInRepositoryLayer(t *testing.T) {
	need := map[string]string{
		"database/sql":             "barrier 回调签名就是 func(ctx, *sql.Tx)",
		"github.com/gospacex/dtmx": "屏障与业务写的同事务包裹由 dtmx.Barrier.Call 提供",
		"gorm.io/gorm":             "RawDB 从 launchx 的 dbx gorm 句柄取 *sql.DB，不另开连接",
	}
	for _, dom := range dtmDomains {
		path := filepath.Join("srv-"+dom, "internal", "repository", "dtmstore.go")
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s 不存在：分支落库通道消失，上面那条「分支面不许 import database/sql」"+
				"会变成「不许存在」的失真判据（%v）", path, err)
		}
		imports := importPathsOf(t, path)
		for want := range need {
			if !hasPrefixOf(imports, want) {
				t.Errorf("%s 未 import %s（%s）", path, want, need[want])
			}
		}
	}
	t.Logf("四域 barrier 写通道都在 repository 层：internal/repository/dtmstore.go")
}

// TestDtmBranchArtifactsStayOutOfKitexGen 判据二：dtm 分支的生成产物只落在
// srv-*/internal/dtm/protos/ 与 bff/internal/dtm/protos/，pkg/kitex_gen 不得有。
//
// 为什么这条要单独立判据而不是「grep 一下 kitex_gen」：kitex_gen 是 kitex 的
// 生成物，**由 IDL 决定**，而 dtm 分支走 grpc-go。两者混在一处时 kitex 工具会
// 按错误假设处理这个目录（把 grpc-go 的生成码当 thrift 产物再生成一遍），
// 而症状是「重新 gen 之后 dtm 分支的 pb 没了」—— 一次没人跑 gen 的操作把它抹掉。
//
// 已实测会红的反向探针：把 srv-stock/internal/dtm/protos/busi.pb.go 复制一份到
// pkg/kitex_gen/stock/dtm_busi.pb.go（内容原样，一个字不改）→ 本用例报红。
func TestDtmBranchArtifactsStayOutOfKitexGen(t *testing.T) {
	const kitexGen = "pkg/kitex_gen"
	// 判据不写成「文件名含 dtm」：生成物是按 IDL 命名的，改一次 IDL 名字就跟着变，
	// 判据会跟着实现一起漂。改判 proto package —— 那是**协议身份**，改它等于换协议。
	const branchProtoPackage = "dtmbranch."

	if _, err := os.Stat(kitexGen); err != nil {
		t.Fatalf("找不到 %s（测试须在 examples/micro 下跑）：%v", kitexGen, err)
	}

	var offenders []string
	files := 0
	err := filepath.WalkDir(kitexGen, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// 目录名同样判：pb 生成物先建包目录，目录在即产物在即。
			if strings.Contains(d.Name(), "dtm") {
				offenders = append(offenders, path+"（目录名含 dtm：dtm 分支包不该出现在 kitex_gen）")
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		files++
		if strings.Contains(path, "dtm") {
			offenders = append(offenders, path+"（文件名含 dtm）")
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(raw), branchProtoPackage) {
			offenders = append(offenders, path+"（内容含 dtm 分支 proto package "+branchProtoPackage+"）")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫 %s 失败：%v", kitexGen, err)
	}
	// 防误扫：kitex_gen 现在有 17 个 .go；一条都不扫到同样是「静默通过」。
	if files < 10 {
		t.Fatalf("只扫到 %d 个 kitex_gen .go（下限 10），疑似路径基准错误", files)
	}
	if len(offenders) > 0 {
		t.Fatalf("dtm 分支产物混进 kitex_gen（%d 处）：该面走 grpc-go，与 kitex 传输面互不渗透，"+
			"混在一处会让 kitex 的 gen 按错误假设处理这个目录：\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}

	// 正向对偶：产物确实在 internal/dtm/protos/。没有这条，上面那条会退化成
	// 「dtm 分支的 pb 压根不存在也算过」。
	for _, dom := range dtmDomains {
		dir := filepath.Join("srv-"+dom, "internal", "dtm", "protos")
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Errorf("%s 不存在：dtm 分支 pb 的唯一落点缺失（%v）", dir, err)
			continue
		}
		names := map[string]bool{}
		for _, e := range entries {
			names[e.Name()] = true
		}
		for _, want := range []string{"busi.proto", "busi.pb.go", "busi_grpc.pb.go"} {
			if !names[want] {
				t.Errorf("%s 缺 %s", dir, want)
			}
		}
	}
	t.Logf("dtm 分支产物落点正确：四域 internal/dtm/protos/ 齐备，pkg/kitex_gen（%d 个 .go）零命中", files)
}

// ---------- 助手 ----------

// dtmBranchFaceDirs 取四域分支面目录，并断言四域都在。
//
// 用 filepath.Glob("srv-*/internal/dtm") 而不是遍历 srv-*/internal：一来精确
// （dtmstub / dbmock 与 internal/dtm 平级，天然不匹配），二来少一个域会在这里
// 直接炸出来，而不是让守卫悄悄少扫一个域。
func dtmBranchFaceDirs(t *testing.T) []string {
	t.Helper()
	roots, err := filepath.Glob(filepath.Join("srv-*", "internal", "dtm"))
	if err != nil {
		t.Fatalf("定位分支面目录失败：%v", err)
	}
	if len(roots) != len(dtmDomains) {
		sort.Strings(roots)
		t.Fatalf("期望 %d 个 srv-*/internal/dtm 分支面目录，实际 %d 个：%v"+
			"（四域分支面少了一个会让本文件的所有守卫少扫一个域）",
			len(dtmDomains), len(roots), roots)
	}
	sort.Strings(roots)
	return roots
}

// importPathsOf 读一个 .go 文件的 import 路径列表。
func importPathsOf(t *testing.T, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("解析 %s 失败：%v（守卫失效 —— 它没在看这个文件）", path, err)
	}
	out := make([]string, 0, len(f.Imports))
	for _, imp := range f.Imports {
		out = append(out, strings.Trim(imp.Path.Value, `"`))
	}
	return out
}

func hasPrefixOf(imports []string, prefix string) bool {
	for _, lit := range imports {
		if strings.HasPrefix(lit, prefix) {
			return true
		}
	}
	return false
}

// containsAny 判是否有任一 import 路径含 needle（替身包这类「路径中段」用它）。
func containsAny(imports []string, needle string) bool {
	for _, lit := range imports {
		if strings.Contains(lit, needle) {
			return true
		}
	}
	return false
}

// matched 返回第一条命中 prefix 的 import 路径（给报错文案用）。
func matched(imports []string, prefix string) string {
	for _, lit := range imports {
		if strings.HasPrefix(lit, prefix) {
			return lit
		}
	}
	return prefix
}
