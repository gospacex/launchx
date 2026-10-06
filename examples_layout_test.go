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
// 它让「mesh 目录零 Go 代码」这条设计结论（见 examples/mesh/README.md）名存实亡。
//
// 这两个测试是从根包跑的（cwd = 仓根），所以能直接按相对路径走目录。

// TestMeshHasNoGoCode 断言网格形态只由部署清单构成，不含任何 Go 代码。
// 网格与微服共用同一份二进制（mode 包已显式拒绝 mode=mesh），清单目录里
// 出现 .go 就意味着有人又在往里塞第二份业务代码。
func TestMeshHasNoGoCode(t *testing.T) {
	root := filepath.Join("examples", "mesh")
	if _, err := os.Stat(root); err != nil {
		t.Skipf("examples/mesh 不存在，跳过：%v", err)
	}

	var goFiles []string
	var yamls int
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
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
		t.Fatalf("examples/mesh 必须零 Go 代码（与 micro 共用同一份二进制），发现 %d 个：%v",
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
