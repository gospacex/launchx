package launchx

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 3.14 依赖面收敛守卫（mode-assembly「RPC 依赖面收敛」的可执行判据）
//
// 规则：launchx 是**库**，库源码（含 internal/**）不得直引任何具体中间件 /
// 框架客户端，一律经 hubx 的 *x SDK 取用。examples/** 是 demo 应用，业务
// handler 必须拿 hertz 的 *app.RequestContext、repository 必须用 gorm，
// 因此不在守卫范围内 —— 这不是放水，是两者的角色本就不同。
//
// 为什么要有这条测试而不是靠手工 grep：3.14 原本是一条写在计划里的 grep
// 命令，没人跑就等于没约束。而且光看 go.mod 看不出问题 —— examples 本来就
// 直引 gorm/hertz，这些依赖在 go.mod 里一直是 direct，internal/ 里偷偷加一个
// gorm import 在依赖层面不会有任何变化，但库就从此被绑死在某个 ORM 上。
func TestLibrarySourceHasNoDirectMiddlewareImports(t *testing.T) {
	forbidden := []string{
		"github.com/cloudwego/",           // kitex / hertz：RPC 与 HTTP 框架，库只经 rpcx / webx 取用
		"gorm.io/gorm",                    // ORM：库只经 dbx 取用，Env.DB 刻意保持 any
		"github.com/redis/go-redis",       // redis 客户端：库只经 cachex / kvx / lockx 取用
		"github.com/segmentio/kafka-go",   // kafka 客户端：库只经 mqx 取用
		"github.com/hashicorp/consul/api", // consul 客户端：库只经 coordx 取用
	}

	var bad []string
	scanned := 0

	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// 只扫本仓源码；跳过隐藏目录（.git 等）与 demo 应用
		if d.IsDir() {
			if path != "." && strings.HasPrefix(filepath.Base(path), ".") {
				return filepath.SkipDir
			}
			if path == "examples" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		scanned++

		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			// 解析失败不该静默放过：那意味着这条守卫根本没在看你以为它在看的文件
			t.Errorf("解析 %s 失败：%v", path, err)
			return nil
		}
		for _, imp := range f.Imports {
			lit := strings.Trim(imp.Path.Value, `"`)
			for _, prefix := range forbidden {
				if strings.HasPrefix(lit, prefix) {
					bad = append(bad, formatViolation(path, fset.Position(imp.Pos()).Line, lit, prefix))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk 仓库失败：%v", err)
	}

	// 防误删：目录被清空时上面的循环一条都不会跑，守卫会“静默通过”
	if scanned < 10 {
		t.Fatalf("只扫到 %d 个 .go 文件，疑似路径基准错误（测试 cwd 应为仓根）", scanned)
	}
	if len(bad) > 0 {
		t.Fatalf("库源码出现 %d 处禁用的直接依赖（共扫 %d 个文件）：\n  %s",
			len(bad), scanned, strings.Join(bad, "\n  "))
	}
	t.Logf("库源码依赖面干净：扫过 %d 个 .go 文件，无禁用直接依赖", scanned)
}

func formatViolation(path string, line int, lit, prefix string) string {
	return path + ":" + itoa(line) + "  import " + strconvQuote(lit) + "（库源码不得直引 " + prefix + "）"
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

func strconvQuote(s string) string { return `"` + s + `"` }
