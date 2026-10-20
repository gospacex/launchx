package launchx

// 服务形态红线守卫：把两条原本写在文档里、由人手执行的 grep 命令固化成
// 自动判据。
//
// 为什么必须是自动判据而不是文档里的命令：这两条**都处于失效状态**，而且
// 失效的方式恰好互为反面 —— 一条永远通过，一条永远报错。
//
//   - 地址红线原先写成 BRE 的 `"127\.0\.0\.1\|localhost"`。`\|` 这个交替语法在
//     本机 grep（toybox 0.8.13）下**静默失效并返回 0 命中**，而它的判据是
//     「必须为空」—— 零命中恰好成立 ⇒ 有违规也被判成合规。实测：改用 `-E`
//     之后立刻吐出命中，其中 monolith/app/routes.go 的四处是上一轮新增
//     `loopbackOnly` 时带进来的，从那时起就没人发现过。
//   - 中间件红线扫 `examples/` 整树**含注释**：源码里 30 处命中全部是注释与
//     错误文案（"本包不 gorm.Open"、"instead of gorm.Open"），真调用一处都没
//     有。任何人跑一次就会认定这条坏了，从此连带着不看它 —— 恒定输出噪声的
//     判据比失效的判据更糟：它会把真正的新违规一起淹没掉。
//
// 两条失效都源于同一个原因：判据是文本 search，而注释与字符串同样是文本。
// 故这里用 go/parser 走 AST —— 注释不在 AST 里，字符串常量（BasicLit）与调用
// 表达式（CallExpr）在 AST 里天然分开，从根上不再是「命中注释」这一类假信号
// （本仓已因它反复踩坑，见 HANDLE-AUDIT.md 的搜索小节）。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// guardedServiceDirs 受本文件守卫的服务源码目录（相对 examples/ 的路径）。
//
// 为什么必须**逐个列举**而不是 `examples/` 整树：这条红线管的是「服务进程把
// 监听/注册地址、中间件连接写死在源码里」—— 写死 ⇒ 换个部署形态就得 fork 一份
// 改常量。`*/scripts/**`（本机排障工具）与 `*/e2e/**`（验收驱动）的默认值天生
// 就是回环地址，它们的地址是可覆盖的本机便利性，不是部署拓扑；扫进去会让判据
// 恒红，进而被当成噪声忽略 —— 那才是真正危险的结果。
//
// 逐个列举的缺点是「新增服务目录默认没人守」，故另有一条守卫
// TestEveryServiceDirIsGuarded 自动发现并强制登记（见其注释）。这条表自己
// 也因此补上了此前漏登记的 micro/srv（冻结对照样例，有真实 Go 代码）。
var guardedServiceDirs = []string{
	// micro
	"micro/bff", "micro/gateway", "micro/srv",
	"micro/srv-order", "micro/srv-points", "micro/srv-product", "micro/srv-stock",
	// mall
	"mall/srv-bff", "mall/srv-gateway",
	"mall/srv-order", "mall/srv-points", "mall/srv-product", "mall/srv-stock",
	// monolith
	"monolith/app", "monolith/cmd", "monolith/internal",
	// mesh 原生组（独立 module，根包 build 覆盖不到，这里与进行结构一样，是它在仓内的门禁）
	"mesh/apps",
}

// skipDirs 不算服务源码目录的一级目录名。
//
// deploy/docs/scripts/pkg/e2e 的性质：`pkg` 是跨示例的 demo 共享库（不建连接、
// 不起进程），`e2e` 与 `scripts` 是本机工具，`deploy`/`docs` 没有 Go 代码。
var skipDirs = map[string]bool{
	"deploy": true, "docs": true, "scripts": true, "pkg": true, "e2e": true,
}

// TestEveryServiceDirIsGuarded 断言每个服务目录都已登记进 guardedServiceDirs。
//
// 存在的理由：显式清单的优点是作用域精确，缺点是漏一个目录就永远没人守 —— 而
// 且这种漏是**静默**的，不会有任何信号。这里按命名规则自动发现服务目录
// （`examples/{micro,mall,monolith}` 下的一级子目录 + `mesh/apps`），凡发现而
// 未登记的即 FAIL：新增服务的人必须显式表态"要不要接受这条红线"，不能靠默认值。
func TestEveryServiceDirIsGuarded(t *testing.T) {
	guarded := map[string]bool{}
	for _, d := range guardedServiceDirs {
		guarded[d] = true
	}

	var missing []string
	for _, example := range []string{"micro", "mall", "monolith"} {
		entries, err := os.ReadDir(filepath.Join("examples", example))
		if err != nil {
			t.Fatalf("读 examples/%s 失败：%v", example, err)
		}
		for _, e := range entries {
			if !e.IsDir() || skipDirs[e.Name()] {
				continue
			}
			// 隐藏目录（.omc / .git / .workbuddy …）是工具产物，不是服务源码：
			// 把它们算进来只会让这条守卫因为与业务无关的原因恒红，
			// 而恒红的守卫最终会被整体忽略 —— 那才是真正的损失。
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			rel := example + "/" + e.Name()
			if !guarded[rel] {
				missing = append(missing, rel)
			}
		}
	}
	if guarded["mesh/apps"] != true {
		missing = append(missing, "mesh/apps（清单缺失）")
	}
	if len(missing) > 0 {
		t.Fatalf("发现未登记的服务目录，请加进 guardedServiceDirs 并在注释里说明它属不属于红线范围：\n  %s",
			strings.Join(missing, "\n  "))
	}
}

// testDoubleDirs 是**测试替身**目录：跨包复用的纯 Go 假 database/sql 驱动
// （内存库 + barrier 判重 + 事务快照回滚），供 repository / service / dtm 三个
// 包的 _test.go 共用。
//
// 为什么它们不进红线扫描：它们必须 gorm.Open 一个内存驱动才跑得起来，
// 而「服务源码不得自建连接」这条守的正是**真实**中间件连接。把这两种 gorm.Open
// 混在一起判，红线就会因为两个合理的例外而失真 —— 而失真的判据最终会被整体忽略。
//
// **跳过是有代价的**：目录里的真代码会一并逃出红线。因此配套的反向守卫是
// TestTestDoublesAreNotImportedByProductionCode —— 非测试文件一律不许 import
// 这几个包。跳过 + 反向守卫成对，才等于没开洞。
var testDoubleDirs = map[string]bool{
	"dtmstub": true,
	"dbmock":  true,
}

// shouldSkipSourceDir 判哪些子目录不进红线扫描。
//
// kitex_gen 是生成码（量大、且它由 IDL 决定，改临时区无意义）；testdata 是
// 测试数据；scripts 是**本机排障与验收工具**（如 micro/srv-order/scripts/
// ordercli），它的地址是可 env 覆盖的本机便利性、而不是部署拓扑 —— 把它算进
// 来会让红线恒红，进而被整体忽略（详见文件头两条失真的判据）。
func shouldSkipSourceDir(name string) bool {
	switch name {
	case "kitex_gen", "testdata", "scripts":
		return true
	}
	return testDoubleDirs[name]
}

// TestTestDoublesAreNotImportedByProductionCode 是 shouldSkipSourceDir 跳过
// testDoubleDirs 的**反向守卫**：这些目录里的东西必须只在测试里被用到。
//
// 为什么必须有它：跳过目录等于给该目录开了一张空白通行证 —— 之后谁往
// testDoubleDirs 里放一段真业务代码，它都不会被任何红线扫到。判据把这条
// 通行证收窄成「可以被 import，但只能被 _test.go import」。
func TestTestDoublesAreNotImportedByProductionCode(t *testing.T) {
	forbidden := map[string]bool{}
	for name := range testDoubleDirs {
		forbidden[name] = true
	}

	var bad []string
	for _, dir := range guardedServiceDirs {
		root := filepath.Join("examples", dir)
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if shouldSkipSourceDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			// 只有 _test.go 被允许 import 测试替身；其余一律不许。
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel := filepath.ToSlash(path)
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if perr != nil {
				t.Errorf("解析 %s 失败：%v（守卫失效）", rel, perr)
				return nil
			}
			for _, imp := range f.Imports {
				lit := strings.Trim(imp.Path.Value, `"`)
				for seg := range forbidden {
					if strings.Contains(lit, "/"+seg) || strings.HasSuffix(lit, "/"+seg) {
						bad = append(bad, rel+": "+lit)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("扫 %s 失败：%v", root, err)
		}
	}
	if len(bad) > 0 {
		t.Fatalf("生产源码 import 了测试替身（%d 处）——测试替身会随二进制出厂：\n  %s",
			len(bad), strings.Join(bad, "\n  "))
	}
	t.Logf("测试替身未泄漏进生产源码：%v", testDoubleDirs)
}

// loopbackAddrPattern 回环相关的地址字面量。
//
// 只判回环而不判任意 IP：硬编码 `10.0.0.7:3306` 当然也该反对，但那要先把
// "什么算环境地址"讲清楚；回环这条是**无争议**的 —— 它在任何非本机部署形态下
// 都必然是错的。判据宁窄勿宽：宽的判据一旦恒红就会被整体忽略。
var loopbackAddrPattern = regexp.MustCompile(`127\.0\.0\.1|localhost|\[?::1\]?`)

// addrExemption 一处「命中了模式但不是违规」的豁免。
type addrExemption struct {
	rel    string // 相对仓根的路径（`/` 分隔）
	needle string // 该行里的唯一子串，用于自证豁免仍在起作用
	reason string // 为什么它不是违规；写不出理由就说明它其实是违规
}

// addrExemptions 豁免清单。
//
// **每条都会被检查是不是还在真地豁免着某个命中**（见 TestServiceSourceHasNo
// HardcodedLoopbackAddr 末尾的陈旧检查）。理由是豁免清单会自然膨胀：留着一条
// 已经用不上的豁免毫无代价，但等到有人照抄它去豁免第 N 处时，没人能分辨哪条
// 是历史包袱。让它自证，过期就红。
var addrExemptions = []addrExemption{
	{
		rel:    "examples/monolith/app/routes.go",
		needle: `return host == "localhost"`,
		reason: "loopbackOnly 的判定本体 —— 「什么是回环」的定义，它必须知道回环长什么样",
	},
	{
		rel:    "examples/monolith/app/routes.go",
		needle: "修法：绑回环（webx.addr: 127.0.0.1:PORT），或修好鉴权依赖",
		reason: "报错文案里的修复建议，不是连接地址；把它改成占位符会让最需要可读结论的时刻变得没用",
	},
	{
		rel:    "examples/monolith/app/routes.go",
		needle: "修法：绑回环（webx.addr: 127.0.0.1:PORT），或把这些 Entry 改成 Secure: true",
		reason: "同上，同一处分支的另一条文案",
	},
}

// TestServiceSourceHasNoHardcodedLoopbackAddr 断言服务源码里没有硬编码的回环地址。
//
// 用 AST 而不是文本 grep：字符串字面量在 AST 里是 BasicLit，注释不进 AST，
// 因此"注释里提到 127.0.0.1"这类此前提到的 noise 不会再产生命中。
func TestServiceSourceHasNoHardcodedLoopbackAddr(t *testing.T) {
	var violations []string
	hit := map[string]bool{} // rel|needle → 该豁免是否被用到
	scanned := 0

	for _, dir := range guardedServiceDirs {
		err := filepath.WalkDir(filepath.Join("examples", dir), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel := filepath.ToSlash(path)
			if d.IsDir() {
				if shouldSkipSourceDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			scanned++

			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				// 解析失败不该静默放过：那意味着这条守卫没在看它以为自己在看的文件。
				t.Errorf("解析 %s 失败：%v（守卫失效）", rel, err)
				return nil
			}
			// 命中行要给得出上下文，故记住每个违规的行号后再回文件取文本。
			lines := readLines(t, path)
			violate := func(pos token.Pos) {
				line := fset.Position(pos).Line
				if line <= 0 || line > len(lines) {
					t.Errorf("守卫自身错误：%s 第 %d 行取不到源码", rel, line)
					return
				}
				text := strings.TrimSpace(lines[line-1])
				if ex, ok := matchExemption(rel, text); ok {
					hit[rel+"|"+ex.needle] = true
					return
				}
				violations = append(violations, rel+":"+itoa(line)+"  "+text)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				raw, err := unquoteGoString(lit.Value)
				if err != nil {
					raw = lit.Value
				}
				if loopbackAddrPattern.MatchString(raw) {
					violate(lit.Pos())
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk examples/%s 失败：%v（守卫失效）", dir, err)
		}
	}

	// 防误删：整张目录表失效时上面的循环一条都不跑，守卫会"静默通过"。
	if scanned < 20 {
		t.Fatalf("只扫到 %d 个 .go 文件，疑似路径基准错误（测试 cwd 应为仓根）", scanned)
	}

	for _, ex := range addrExemptions {
		if !hit[ex.rel+"|"+ex.needle] {
			stillThere := lineExists(ex.rel, ex.needle)
			if stillThere {
				t.Errorf("陈旧豁免：%s 里的 %q 已经不再产生命中（也许它被改成了注释？）—— 该条不再有必要，请删除它",
					ex.rel, ex.needle)
			} else {
				t.Errorf("陈旧豁免：%s 里找不到 %q —— 代码改过了，豁免指向的行已不存在，请删除它",
					ex.rel, ex.needle)
			}
		}
	}

	if len(violations) > 0 {
		t.Fatalf("服务源码出现 %d 处硬编码回环地址（共扫 %d 个文件）：\n  %s\n\n"+
			"若某处确属「命中模式但不是违规」，加进 addrExemptions 并写出理由 —— "+
			"写不出理由通常说明它其实是违规。",
			len(violations), scanned, strings.Join(violations, "\n  "))
	}
	t.Logf("地址红线干净：扫过 %d 个 .go 文件（服务目录 %d 个），无硬编码回环地址", scanned, len(guardedServiceDirs))
}

// middlewareSetupCall 禁用的「自己动手建连接」调用。
//
// 为什么禁：这些客户端一旦被服务代码直接 new 出来，它就绕开了 App 装配出来的
// 那一份 —— 不共享连接池、不共享 otel 插件、不共享退出链。典型症状是「业务分层
// span 齐全，存储 span 一条没有」，而代码看上去毫无异常。
//
// key 是 import path（库源码那条守卫用的是同样的口径），value 是禁用的构造函数集合。
var middlewareSetupCall = map[string][]string{
	"gorm.io/gorm":                        {"Open"},
	"github.com/redis/go-redis":           {"NewClient"},
	"github.com/redis/go-redis/v9":        {"NewClient"},
	"github.com/segmentio/kafka-go":       {"NewWriter", "NewReader"},
	"github.com/hashicorp/consul/api":     {"NewClient"},
	"go.mongodb.org/mongo-driver/mongo":   {"Connect"},
	"github.com/elastic/go-elasticsearch": {"NewClient"},
}

// TestServiceSourceHasNoDirectMiddlewareSetup 断言服务源码不自己新建中间件连接。
//
// 只看 **CallExpr 且 selector 的包名能对应到一个禁用的 import**：这是本文件与
// 手写 grep 的关键差别 —— grep 版本会把注释里那 30 处"我们不做 X"也当成命中，
// 而 CallExpr 只有在真有调用表达式时才存在。
//
// 包名取自 import 列表而非字符串拼接的文本，故起别名（`g "gorm.io/gorm"`）也躲不掉。
func TestServiceSourceHasNoDirectMiddlewareSetup(t *testing.T) {
	var violations []string
	scanned := 0

	for _, dir := range guardedServiceDirs {
		err := filepath.WalkDir(filepath.Join("examples", dir), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel := filepath.ToSlash(path)
			if d.IsDir() {
				if shouldSkipSourceDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			scanned++

			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Errorf("解析 %s 失败：%v（守卫失效）", rel, err)
				return nil
			}
			// importPath 的别名/默认包名 → import 路径。没有对应别名的命名（如 dot
			// import）保守跳过：这项守卫的价值在于抓住常见写法，不必为覆盖所有
			// 语言边角而牺牲可读性。
			pkgOf := map[string]string{}
			for _, imp := range f.Imports {
				p := strings.Trim(imp.Path.Value, `"`)
				name := importBase(p)
				if imp.Name != nil && imp.Name.Name != "" && imp.Name.Name != "." {
					name = imp.Name.Name
				}
				pkgOf[name] = p
			}

			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				ident, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				path, ok := pkgOf[ident.Name]
				if !ok {
					return true
				}
				for _, fn := range middlewareSetupCall[path] {
					if sel.Sel.Name == fn {
						violations = append(violations, rel+":"+itoa(fset.Position(call.Pos()).Line)+
							"  "+ident.Name+"."+fn+"（"+path+"）—— 服务源码不得自建连接，取 App.DB()/Cache()/MQ() 装配好的那一份")
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk examples/%s 失败：%v（守卫失效）", dir, err)
		}
	}

	if scanned < 20 {
		t.Fatalf("只扫到 %d 个 .go 文件，疑似路径基准错误（测试 cwd 应为仓根）", scanned)
	}
	if len(violations) > 0 {
		t.Fatalf("服务源码出现 %d 处自建中间件连接（共扫 %d 个文件）：\n  %s",
			len(violations), scanned, strings.Join(violations, "\n  "))
	}
	t.Logf("中间件红线干净：扫过 %d 个 .go 文件，服务源码不自建连接", scanned)
}

// TestAcceptanceDocsDoNotUseBrokenGrepForm 断言验收文档里没有已知会失效的 grep 写法。
//
// 上面两条是把「判据内容」自动化；这一条管的是**判据的形态** —— 修完之后最可能
// 发生的事，是有人照着旧段落把坏写法又抄回去。两种形态已被实测证伪：
//
//   - BRE 的 `\|` 交替：`grep -rn "127\.0\.0\.1\|localhost"` 在本机 grep
//     （toybox 0.8.13）下静默返回 0 命中。这类判据的通过条件恰好是「输出为空」，
//     于是失效直接表现为**永远通过**。必须写成 `-rnE`。
//   - 无引号的 `--include=*.go`：zsh 下报 "no matches found"，bash 下会被当前目录
//     里的 .go 文件 glob 展开成 `--include=某个具体文件`，两种情况都不报错地
//     改变了判据的语义。必须写成 `--include='*.go'`。
//
// 只扫 examples/**：验收文档里的命令是要人手执行的，写错就有后果；docs/** 下的
// 评审报告会引用失效形态作为问题描述（那是证据本身），禁掉它只会让报告写不成。
func TestAcceptanceDocsDoNotUseBrokenGrepForm(t *testing.T) {
	var violations []string
	scanned := 0

	err := filepath.WalkDir("examples", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".md" {
			return nil
		}
		scanned++
		b, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("读 %s 失败：%v", path, err)
			return nil
		}
		for i, line := range strings.Split(string(b), "\n") {
			rel := filepath.ToSlash(path) + ":" + itoa(i+1)
			if strings.Contains(line, "grep -") && strings.Contains(line, `\|`) {
				violations = append(violations, rel+"  grep 用了 BRE 的 `\\|` 交替（本机静默 zero 命中）→ 改成 -E")
			}
			if strings.Contains(line, "--include=*") {
				violations = append(violations, rel+"  --include 的通配符没加引号（会被 shell 展开）→ 写成 --include='*.go'")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk examples 失败：%v（守卫失效）", err)
	}
	if scanned < 10 {
		t.Fatalf("只扫到 %d 个 .md 文件，疑似路径基准错误（测试 cwd 应为仓根）", scanned)
	}
	if len(violations) > 0 {
		t.Fatalf("验收文档出现 %d 处已知会失效的 grep 写法（共扫 %d 个 .md）：\n  %s",
			len(violations), scanned, strings.Join(violations, "\n  "))
	}
	t.Logf("验收文档形态干净：扫过 %d 个 .md 文件", scanned)
}

// importBase 从 import 路径推包名：`gorm.io/gorm` → `gorm`。
//
// 末尾是 `vN`（语义化版本目录）时要再往上取一段：`github.com/redis/go-redis/v9`
// 的包名是 `redis` 而不是 `v9`。
func importBase(p string) string {
	base := p[strings.LastIndex(p, "/")+1:]
	if regexp.MustCompile(`^v\d+$`).MatchString(base) {
		p = p[:strings.LastIndex(p, "/")]
		base = p[strings.LastIndex(p, "/")+1:]
	}
	return base
}

func matchExemption(rel, text string) (addrExemption, bool) {
	for _, ex := range addrExemptions {
		if ex.rel == rel && strings.Contains(text, ex.needle) {
			return ex, true
		}
	}
	return addrExemption{}, false
}

func lineExists(rel, needle string) bool {
	b, err := os.ReadFile(rel)
	if err != nil {
		return false
	}
	return strings.Contains(string(b), needle)
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败：%v", path, err)
	}
	return strings.Split(string(b), "\n")
}

// unquoteGoString 去掉 Go 字符串字面量的引号。
//
// 刻意只用 strconv 之外的最小处理：判据自身保持零额外依赖是这个文件的一贯选择
// （见 dependency_surface_test.go 的 itoa）。解释型字符串已经足够这里使用。
func unquoteGoString(s string) (string, error) {
	if len(s) < 2 {
		return s, os.ErrInvalid
	}
	q := s[0]
	if q != '"' && q != '`' {
		return s, os.ErrInvalid
	}
	body := s[1 : len(s)-1]
	if q == '`' {
		return body, nil
	}
	return strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\n`, "\n", `\t`, "\t").Replace(body), nil
}
