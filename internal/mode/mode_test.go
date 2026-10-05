package mode

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gospacex/configx"
)

// validValuesRe 错误信息必须给出的合法取值串。
//
// 四档必须全列且以 | 分隔：只列一部分会让调用方靠猜补档，
// 是"配错又蒙对"的典型来源。
var validValuesRe = regexp.MustCompile(`monolith\|bff\|srv\|gateway`)

// holders 按 testdata 下的配置文件构造装配句柄（不经任何真底座）。
func holders(t *testing.T, file string) *AppHolders {
	t.Helper()
	cfg, err := configx.ReadConfig(configx.ConfigOptions{
		ConfigFile: filepath.Join("testdata", file),
		ConfigType: "yaml",
	})
	if err != nil {
		t.Fatalf("read config %s: %v", file, err)
	}
	return &AppHolders{Cfg: cfg, ServiceName: "mode-test"}
}

// TestIllegalMode 断言非法形态即退，且错误列出全部合法取值（C1）。
func TestIllegalMode(t *testing.T) {
	h := holders(t, "illegal.yaml")
	err := Assemble(h)
	if err == nil {
		t.Fatalf("illegal mode must be rejected, out = %+v", h.Out)
	}
	if !validValuesRe.MatchString(err.Error()) {
		t.Fatalf("error must list valid values, got: %v", err)
	}
}

// TestMissingMode 断言缺失形态即退（C2）：不允许默默默认成 monolith。
func TestMissingMode(t *testing.T) {
	h := holders(t, "nomode.yaml")
	err := Assemble(h)
	if err == nil {
		t.Fatalf("missing mode must be rejected, out = %+v", h.Out)
	}
	if !validValuesRe.MatchString(err.Error()) {
		t.Fatalf("error must list valid values, got: %v", err)
	}
}

// TestMeshRejected 断言 mesh 被显式拒绝（C4）：网格不是独立档，
// 与微服走同一份代码，差异只在部署清单。错误里必须点名 mesh 并给出替代档。
func TestMeshRejected(t *testing.T) {
	h := holders(t, "mesh.yaml")
	err := Assemble(h)
	if err == nil {
		t.Fatalf("mesh must be rejected, out = %+v", h.Out)
	}
	if !strings.Contains(err.Error(), "mesh") {
		t.Fatalf("error must name mesh, got: %v", err)
	}
	if !validValuesRe.MatchString(err.Error()) {
		t.Fatalf("error must list valid values, got: %v", err)
	}
}

// TestWebxSectionOptional 断言 webx 段是 presence-based：没配也能装配
// （缺省由 FillDefaults 补齐，监听地址回落到 service.addr）。
// 反例：直接把整段丢给 ReadConfigFromConfigx 会报 "key webx not found"，
// 于是"不配 webx 段"变成启动失败——配置缺省不该有这种杀伤力。
func TestWebxSectionOptional(t *testing.T) {
	h := holders(t, "plain.yaml")
	if err := Assemble(h); err != nil {
		t.Fatalf("assemble without webx section: %v", err)
	}
	if h.Out.Webx == nil {
		t.Fatal("webx engine must be assembled from defaults")
	}
}

// TestModeAssemblyMatrix 断言四档映射表（C3）：
//
//	monolith → webx 构建 + 不注册
//	bff      → webx + 注册 + RPCClientOptions 工厂
//	srv      → 不建 webx + RPCServerOptions + 注册
//	gateway  → webx + 不注册（上游发现经 RPCClientOptions 走 coordx resolver）
func TestModeAssemblyMatrix(t *testing.T) {
	cases := []struct {
		file           string
		mode           string
		wantWebx       bool
		wantRegistered bool
		wantRPCServer  bool
		wantRPCClient  bool
	}{
		{file: "monolith.yaml", mode: "monolith", wantWebx: true, wantRegistered: false, wantRPCServer: false, wantRPCClient: false},
		{file: "bff.yaml", mode: "bff", wantWebx: true, wantRegistered: true, wantRPCServer: false, wantRPCClient: true},
		{file: "srv.yaml", mode: "srv", wantWebx: false, wantRegistered: true, wantRPCServer: true, wantRPCClient: false},
		{file: "gateway.yaml", mode: "gateway", wantWebx: true, wantRegistered: false, wantRPCServer: false, wantRPCClient: true},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			h := holders(t, tc.file)
			if err := Assemble(h); err != nil {
				t.Fatalf("assemble %s: %v", tc.file, err)
			}
			if h.Out.Mode != tc.mode {
				t.Fatalf("Out.Mode = %q, want %q", h.Out.Mode, tc.mode)
			}
			if (h.Out.Webx != nil) != tc.wantWebx {
				t.Fatalf("webx built = %v, want %v", h.Out.Webx != nil, tc.wantWebx)
			}
			if h.Out.Registered != tc.wantRegistered {
				t.Fatalf("Registered = %v, want %v", h.Out.Registered, tc.wantRegistered)
			}
			if (h.Out.RPCServer != nil) != tc.wantRPCServer {
				t.Fatalf("RPCServer built = %v, want %v", h.Out.RPCServer != nil, tc.wantRPCServer)
			}
			if (h.Out.RPCClientOptions != nil) != tc.wantRPCClient {
				t.Fatalf("RPCClientOptions built = %v, want %v", h.Out.RPCClientOptions != nil, tc.wantRPCClient)
			}
			if !tc.wantRPCClient {
				return
			}
			// 工厂按被调服务名产出客户端选项；无注册中心段时走直连兜底，不连真底座。
			opts, err := h.Out.RPCClientOptions("downstream-svc")
			if err != nil {
				t.Fatalf("RPCClientOptions: %v", err)
			}
			if opts == nil {
				t.Fatal("RPCClientOptions returned nil options")
			}
			if _, err := h.Out.RPCClientOptions(""); err == nil {
				t.Fatal("RPCClientOptions must reject empty service name")
			}
		})
	}
}
