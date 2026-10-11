// Package srv 只承载冻结判据，不含业务代码。
//
// 为什么要有这个文件：`examples/micro/srv` 是拆分前的对照样例，冻结状态此前只
// 写在 `examples/scripts/ci.sh` 的一行注释里（`冻结的拆分前对照样例`）。注释不会
// 在任何一次门禁里被检查，于是"冻结"全靠人记得 —— 而记得的人迟早会换。
// 这里把两条最容易破的约定变成可执行判据：
//
//  1. README 顶部的冻结声明还在（被删 / 被改写就红）；
//  2. 本目录没有进任何部署清单（冻结样例不启动、不上线）。
package srv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFreezeNoticeDocumented 冻结声明必须写在 README 的开头。
//
// 只看开头 800 字节：声明的意义是"打开目录第一眼就能看见"，埋在文档中间
// 等于没有。
func TestFreezeNoticeDocumented(t *testing.T) {
	b, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("读 README.md: %v", err)
	}
	head := string(b)
	if len(head) > 800 {
		head = head[:800]
	}
	if !strings.Contains(head, "冻结") {
		t.Fatal("README.md 开头看不到「冻结」声明 —— 冻结状态一旦只存在于口头，" +
			"下一个人会把它当现行实现继续改（现行实现是 examples/micro/srv-*）")
	}
}

// TestFrozenSampleIsNotDeployed 冻结样例不得出现在任何部署清单里。
//
// 冻结 = 不启动、不上线。它留在仓里是为了对照，不是为了被部署；一旦进了
// 部署清单，就会有第二份「同域不同实现」在生产里跑，排查时先要分清是哪一份。
//
// 逐行跳过注释行：配置同源的说明性注释（"与 examples/micro/srv/config.yaml 同源"）
// 是合法引用，它不构成部署。
//
// 反向探针：在任一 deploy/*.yaml 里加一行 `image: example/micro/srv:v1`，本用例红。
func TestFrozenSampleIsNotDeployed(t *testing.T) {
	dirs, err := filepath.Glob("../../*/deploy")
	if err != nil || len(dirs) == 0 {
		t.Fatalf("未找到任何 deploy 目录（glob `../../*/deploy` = %v）—— "+
			"判据扫不到就不该假装通过", dirs)
	}
	checked := 0
	for _, dir := range dirs {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			checked++
			for i, line := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "#") {
					continue
				}
				if strings.Contains(line, "micro/srv") {
					t.Errorf("%s:%d 部署清单引用了冻结样例 micro/srv：%s",
						path, i+1, strings.TrimSpace(line))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("遍历 %s: %v", dir, err)
		}
	}
	if checked == 0 {
		t.Fatal("没有扫到任何 yaml —— 判据扫不到就不该假装通过")
	}
}
