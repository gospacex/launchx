package launchx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件钉住「micro 与 mesh 不耦合」这条结构约束。
//
// 背景：examples/mesh 曾被人拷进一份 examples/micro 的旧快照（mesh/micro/**），
// 那份快照反过来 import examples/micro/**/internal —— Go 的 internal 规则只允许
// 同目录树内引用，于是 `go build ./...` 直接挂掉，只能靠显式包列表绕。更糟的是
// 它让「mesh 的清单目录零 Go 代码」这条设计结论（见 examples/mesh/README.md）名存实亡。
//
// 这两个测试是从根包跑的（cwd = 仓根），所以能直接按相对路径走目录。

// TestMeshGoCodeOnlyUnderApps 断言 examples/mesh 的 Go 代码只允许出现在 apps/ 子树。
//
// 口径变更（2026-10-07，add-mesh-native-apps）：本目录原先是「零 Go 代码」，
// 论证的是 C4「换形态不改代码」（micro 与 mesh 共用同一份二进制）。加入 apps/
// 原生组之后，口径细化为两条**并存**的证据链：
//
//	examples/mesh/deploy/**   → 仍零 Go 代码（清单目录不许塞实现）
//	examples/mesh/apps/**     → 网格原生应用，独立 module，允许 Go 代码
//
// 守卫继续挡住「随手往清单目录扔 main.go」这类结构腐化：全目录 walk 一遍，
// 只在 apps/ 处 SkipDir。apps/ 是独立 module，根包 `go build ./...` 覆盖不到它，
// 所以这条 walk 也是它在结构层面唯一的门禁。
func TestMeshGoCodeOnlyUnderApps(t *testing.T) {
	root := filepath.Join("examples", "mesh")
	if _, err := os.Stat(root); err != nil {
		t.Skipf("examples/mesh 不存在，跳过：%v", err)
	}

	allowed := filepath.Join(root, "apps")
	if fi, err := os.Stat(allowed); err != nil || !fi.IsDir() {
		t.Fatalf("examples/mesh/apps 必须存在（网格原生应用的独立 module）：%v", err)
	}

	var goFiles []string
	var yamls int
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if path == allowed {
				return filepath.SkipDir // apps/ 整棵子树放行
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".go":
			goFiles = append(goFiles, path)
		case ".yaml", ".yml":
			yamls++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk examples/mesh: %v", err)
	}

	if len(goFiles) > 0 {
		t.Fatalf("examples/mesh 的 Go 代码只允许在 apps/ 下，deploy 等清单目录必须零代码，发现 %d 个：%v",
			len(goFiles), goFiles)
	}
	if yamls == 0 {
		t.Fatal("examples/mesh 里没有任何 yaml —— 目录被清空了？")
	}
}

// TestMicroNotCoupledToMesh 断言微服示例不依赖网格示例，反之亦然。
// 双向都查：micro 的 import 里不许出现 examples/mesh；
// examples/micro 子树里也不许再嵌一个叫 mesh 的目录。
func TestMicroNotCoupledToMesh(t *testing.T) {
	const meshImport = "examples/mesh"

	root := filepath.Join("examples", "micro")
	if _, err := os.Stat(root); err != nil {
		t.Skipf("examples/micro 不存在，跳过：%v", err)
	}

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// 目录层：不允许再嵌 mesh 目录（历史上那份快照就叫 mesh/micro）
		if info.IsDir() && info.Name() == "mesh" {
			t.Errorf("examples/micro 下不允许嵌套 mesh 目录（发现 %s）—— 网格只应有部署清单", path)
			return nil
		}
		if info.IsDir() || filepath.Ext(path) != ".go" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), meshImport) {
			t.Errorf("%s 引用了 %s —— micro 与 mesh 必须解耦", path, meshImport)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk examples/micro: %v", err)
	}
}
