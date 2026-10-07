package handlesrc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/gorm"
)

// monolithRoot 测试 cwd 是本包目录，往回两级即 examples/monolith。
const monolithRoot = "../.."

// TestNoSelfBuiltStorageClient 源码级守卫：案例里不得出现自己构造存储客户端的代码。
// 为什么走 AST 而不是字符串 grep：这些判据（尤其 "gorm.Open"）会以字符串形式
// 出现在错误提示与注释里（本仓已有 handlecheck 那份就是这样），grep 会把释义
// 当成违规。parse 成 AST 后只认真实的函数调用，天然忽略注释与字面量。
//
// 它守的是「将来有人图省事自己开一个」这条回归路径 —— handlesrc.Verify 只能在
// 句柄已经握在手里时判断它的状态，判断不了它是从哪来的（除 dbx 那条能验回调）。
// 两者互补：这条守来源，那条守入链。
func TestNoSelfBuiltStorageClient(t *testing.T) {
	forbidden := map[string]string{
		"gorm.Open":               "MySQL/SQLite 连接须经 App.DB()",
		"redis.NewClient":         "redis 客户端须经 App.Cache() / App.KV()",
		"mongo.Connect":           "mongo 客户端须经 App.Mongo()",
		"elasticsearch.NewClient": "ES 客户端须经 App.Search()",
		"sql.Open":                "裸 database/sql 句柄不入链，须经 App.DB()",
		"kafka.NewWriter":         "kafka 生产者须经 App.MQ()",
		"kafka.NewReader":         "kafka 消费者须经 App.MQConsumer()",
		"minio.New":               "对象存储须经 objectx 注册后再取句柄",
		"ristretto.NewCache":      "本地缓存须经 cachex 注册后再取句柄",
	}

	var bad []string
	scanned := 0

	// 先把相对目录换成绝对路径：相对路径 "../.." 的 filepath.Base 是 ".."，
	// 会被下面"跳过隐藏目录"的判据当成隐藏目录整个 SkipDir —— 那样 walked
	// 一次都不跑，守卫会假装通过（第一次写就是这么哑火的）。
	root, err := filepath.Abs(monolithRoot)
	if err != nil {
		t.Fatalf("解析 %s 失败：%v", monolithRoot, err)
	}

	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && strings.HasPrefix(filepath.Base(path), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		scanned++

		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Errorf("解析 %s 失败：%v", path, perr)
			return nil
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
			key := ident.Name + "." + sel.Sel.Name
			if reason, hit := forbidden[key]; hit {
				bad = append(bad, path+":"+itoa(fset.Position(call.Pos()).Line)+
					" 调用 "+key+"（"+reason+"）")
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s 失败：%v", monolithRoot, err)
	}

	// 防误删：路径写错时上面的循环一次都不跑，守卫会假装通过。
	if scanned < 10 {
		t.Fatalf("只扫到 %d 个 .go 文件，疑似路径基准错误（cwd 应为本包目录）", scanned)
	}
	if len(bad) > 0 {
		t.Fatalf("案例源码出现 %d 处自建存储客户端（共扫 %d 个文件）：\n  %s",
			len(bad), scanned, strings.Join(bad, "\n  "))
	}
	t.Logf("存储句柄来源干净：扫过 %d 个 .go 文件，无自建客户端调用", scanned)
}

// TestEveryStorageHandleComesFromApp 反向断言：六个句柄必须在装配层经 App.*() 取过。
//
// 为什么不能只有上一条：上一条只证明「没有乱开」，证明不了「该取的都取了」——
// 有人删掉 a.Search() 那行、让索引能力悄悄消失，上一条照样全绿。
func TestEveryStorageHandleComesFromApp(t *testing.T) {
	setup := filepath.Join(monolithRoot, "app", "setup.go")
	src, err := os.ReadFile(setup)
	if err != nil {
		t.Fatalf("读 %s 失败：%v", setup, err)
	}
	text := string(src)

	// 左侧是必须出现的取用表达式，右侧是该句柄对应的组件。
	required := map[string]string{
		"a.DB()":     "db",
		"a.Cache()":  "cache",
		"a.KV()":     "kv",
		"a.MQ()":     "mq",
		"a.Search()": "search",
		"a.Mongo()":  "mongodb",
	}
	for expr, comp := range required {
		if !strings.Contains(text, expr) {
			t.Errorf("%s 里没有 %s：%s 组件不会被装配，其能力会无声消失", setup, expr, comp)
		}
	}
}

// TestVerifyDBXRejectsBareHandle 变异验证：裸 gorm 句柄必须被拒。
//
// 这是整包判据能否成立的底线 —— 若它对随便一个句柄都返回 nil，装配期的那道
// 检查就成了摆设。正向用例（真 dbx 句柄通过）由 examples/monolith 的 E2E 覆盖：
// 服务起来时 trace.on.db=true，通过即代表回调齐全。
func TestVerifyDBXRejectsBareHandle(t *testing.T) {
	if err := VerifyDBX(nil); err == nil {
		t.Error("VerifyDBX(nil) 返回 nil：nil 句柄必须被拒")
	}
	if err := VerifyDBX(&gorm.DB{}); err == nil {
		t.Error("VerifyDBX(&gorm.DB{}) 返回 nil：没有 dbx trace 回调的裸句柄必须被拒")
	} else if !strings.Contains(err.Error(), "dbx") {
		t.Errorf("错误信息里看不出与 dbx 的关系：%v", err)
	}
}

// TestDivergedOnlyFlagsSilentLoss 锁住背离的判定口径。
//
// 只有「句柄在 + 要求入链 + 却不产 span」才需要失败。"没配段"和"本就没打算入链"
// 都是正常的运行姿态，把它们算成问题会让人为了消掉告警去加 component 段或改
// 开关 —— 那才是真把配置改坏。
func TestDivergedOnlyFlagsSilentLoss(t *testing.T) {
	cases := []struct {
		name string
		c    Check
		want bool
	}{
		{"配了段要求入链却不产span", Check{Present: true, Want: true, Observed: false}, true},
		{"配了段且真的入链", Check{Present: true, Want: true, Observed: true}, false},
		{"没配段", Check{Present: false, Want: true, Observed: false}, false},
		{"配了段但本就不要求入链", Check{Present: true, Want: false, Observed: false}, false},
	}
	for _, tc := range cases {
		if got := tc.c.Diverged(); got != tc.want {
			t.Errorf("%s：Diverged()=%v，want %v（%s）", tc.name, got, tc.want, tc.c)
		}
	}
}

// 本文件刻意不引入 strconv：保持守卫自身零额外依赖。
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
