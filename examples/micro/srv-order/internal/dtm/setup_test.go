// 分支面装配的守卫：地址必须来自 config，且四域端口约定里订单域那一格没被占错。
//
// 为什么读配置文件而不是直接测 Setup：Setup 需要一个完整装配好的 launchx.App
// （dbx / cachex / kvx / lockx / coordx 五个组件 + kitex 选项），为「读一个键」
// 起这么一套夹具不划算；而「这个键在不在、配的是什么」恰恰是本文件要钉的东西 ——
// 它配错时的症状是 dtm server 回调 404，而本进程日志里一片正常。
package dtm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// configPath 本域配置文件（从 internal/dtm 往上两级是 srv-order 根）。
const configPath = "../../config.yaml"

// wantListenAddr 订单域分支面地址。
//
// 取值口径：四域端口约定 9041-9044（商品/订单/库存/积分），订单域是 9042。
// 它写在测试里是刻意的：config.yaml 与本测试必须同时改 —— 只改一边时，
// 要么这条红，要么编排侧（bff 里的 dtm 提交代码）按旧端口回调而 404。
const wantListenAddr = "127.0.0.1:9042"

// TestConfigCarriesBranchListenAddr config.yaml 必须带 dtm.branch.listen_addr。
func TestConfigCarriesBranchListenAddr(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(configPath))
	if err != nil {
		t.Fatalf("读 %s: %v", configPath, err)
	}
	lines := strings.Split(string(raw), "\n")

	// 只找 dtm 段下的 branch.listen_addr，不做 YAML 解析：仓内已有 yaml 依赖，
	// 但这一条要判的是「键在正确的段里」，逐行走反而更直接（YAML 解析器不会
	// 告诉你它是在 coordx 段底下读到的那个同名键）。
	inDtm, inBranch := false, false
	var got string
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		indent := len(ln) - len(strings.TrimLeft(ln, " "))
		switch {
		case trimmed == "" || strings.HasPrefix(trimmed, "#"):
			continue
		case indent == 0:
			inDtm = strings.HasPrefix(trimmed, "dtm:")
			inBranch = false
		case indent == 2 && strings.HasPrefix(trimmed, "branch:"):
			inBranch = inDtm
		case indent == 4 && strings.HasPrefix(trimmed, "listen_addr:"):
			if inBranch {
				got = strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "listen_addr:")), `"'`)
			}
		}
	}
	if got == "" {
		t.Fatalf("config.yaml 缺 dtm.branch.listen_addr —— 分支面没有地址可监听，dtm server 回调必然 404")
	}
	if got != wantListenAddr {
		t.Fatalf("dtm.branch.listen_addr = %q, want %q（编排侧按这个端口回调）", got, wantListenAddr)
	}
}

// TestNoHardcodedListenAddrInGoSource Go 源码里不许出现分支面监听地址。
//
// 与仓级守卫（examples_addr_hygiene_test.go 的 TestServiceSourceHasNoHardcodedLoopbackAddr）
// 同一件事，但落在这里是因为**这条域刚加了一条新的入口**：装配代码写顺手就会把
// 端口抄进 Go 文件，而仓级那条扫的是全 examples/、报错信息离改动点很远。
//
// 扫 **非测试**源码（与仓级守卫同口径）：测试文件必须能写出期望值，而把期望值
// 拆成几段拼起来只为躲一条文本判据，是把判据变得比它要防的东西更难读。
func TestNoHardcodedListenAddrInGoSource(t *testing.T) {
	needle := wantListenAddr
	err := filepath.WalkDir("../..", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		// 只判字符串字面量那一行：注释里说明「端口是 9042」是允许的（它不构成
		// 部署拓扑），出现在代码里才是。
		for i, ln := range strings.Split(string(b), "\n") {
			if !strings.Contains(ln, needle) {
				continue
			}
			trimmed := strings.TrimSpace(ln)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
				continue
			}
			rel, _ := filepath.Rel("../..", path)
			t.Errorf("%s:%d 把监听地址写进了代码：%s", rel, i+1, trimmed)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描本域源码失败（守卫失效）：%v", err)
	}
}
