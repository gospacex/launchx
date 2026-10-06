package mode

import (
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/gospacex/configx"
	"github.com/gospacex/rpcx"
	"github.com/gospacex/webx"
)

// kitex 选项切片的类型名。
//
// 断言动态类型而非 import kitex：launchx 源码（含 internal 与测试）零
// cloudwego/* 依赖（验收 3.14），kitex 只经 rpcx 引入，故用类型名比对。
const (
	clientOptionSlice = "[]client.Option"
	serverOptionSlice = "[]server.Option"
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
	// mesh 追加说明：点出替代档，否则调用方只知道"不行"不知道"改用什么"。
	if !strings.Contains(err.Error(), "srv/gateway/bff") {
		t.Fatalf("mesh error must point at replacement modes, got: %v", err)
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
		// targetSvc/targetAddr 被调服务名与其直连兜底目标（配在 service.upstream 下）。
		targetSvc  string
		targetAddr string
	}{
		{file: "monolith.yaml", mode: "monolith", wantWebx: true, wantRegistered: false, wantRPCServer: false, wantRPCClient: false},
		{file: "bff.yaml", mode: "bff", wantWebx: true, wantRegistered: true, wantRPCServer: false, wantRPCClient: true,
			targetSvc: "downstream-svc", targetAddr: "127.0.0.1:19013"},
		{file: "srv.yaml", mode: "srv", wantWebx: false, wantRegistered: true, wantRPCServer: true, wantRPCClient: false},
		{file: "gateway.yaml", mode: "gateway", wantWebx: true, wantRegistered: false, wantRPCServer: false, wantRPCClient: true,
			targetSvc: "downstream-svc", targetAddr: "127.0.0.1:18081"},
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
			if tc.wantWebx {
				// HTTP 入口必须是真的 webx 引擎（*webx.App），业务 Setup 与网关
				// 转发路由就挂在这个引擎上；给个别的东西等于路由无处可挂。
				if _, ok := h.Out.Webx.(*webx.App); !ok {
					t.Fatalf("Webx = %T, want *webx.App", h.Out.Webx)
				}
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
			opts, err := h.Out.RPCClientOptions(tc.targetSvc)
			if err != nil {
				t.Fatalf("RPCClientOptions(%q): %v", tc.targetSvc, err)
			}
			if opts == nil {
				t.Fatal("RPCClientOptions returned nil options")
			}
			if got := typeName(opts); got != clientOptionSlice {
				t.Fatalf("RPCClientOptions returned %s, want %s", got, clientOptionSlice)
			}
			// Target 解析路径存在：service.upstream.<svc> 必须真的流进 kitex 选项，
			// 否则"配了上游地址却没生效"会变成静默连不上的故障。
			wantLen := len(rpcx.BuildClientOptions(rpcx.ClientOptions{
				ServiceName: h.ServiceName, Target: tc.targetAddr,
			}))
			if got := reflect.ValueOf(opts).Len(); got != wantLen {
				t.Fatalf("client options len = %d, want %d (target %q not wired?)", got, wantLen, tc.targetAddr)
			}
			// 未配 upstream 的被调方：没有 Target 可解析，选项数必须少于带目标时。
			bare, err := h.Out.RPCClientOptions("no-target-svc")
			if err != nil {
				t.Fatalf("RPCClientOptions(no-target-svc): %v", err)
			}
			if got := reflect.ValueOf(bare).Len(); got >= wantLen {
				t.Fatalf("client options len without target = %d, want < %d", got, wantLen)
			}
			if _, err := h.Out.RPCClientOptions(""); err == nil {
				t.Fatal("RPCClientOptions must reject empty service name")
			}
		})
	}
}

// TestSRVServerOptions 断言 srv 档的 rpcx 服务端选项含服务名与监听端口（C3）。
//
// 这两项是注册中心里能被找到的全部依据：服务名错了注册不到、端口错了
// 注册成功但连不上，都属于"启动看不出来、调用时才炸"的静默故障。
func TestSRVServerOptions(t *testing.T) {
	h := holders(t, "srv.yaml")
	if err := Assemble(h); err != nil {
		t.Fatalf("assemble srv.yaml: %v", err)
	}
	so := ServerOptions(h)
	if so.ServiceName != h.ServiceName {
		t.Fatalf("ServerOptions.ServiceName = %q, want %q", so.ServiceName, h.ServiceName)
	}
	if want := "127.0.0.1:19013"; so.Addr != want {
		t.Fatalf("ServerOptions.Addr = %q, want %q", so.Addr, want)
	}
	if so.TraceEnabled {
		t.Fatal("ServerOptions.TraceEnabled = true, want false (testdata trace.enabled: false)")
	}
	// Out.RPCServer 必须与 ServerOptions 同源：业务 Setup 拿它构造 kitex server。
	if got := typeName(h.Out.RPCServer); got != serverOptionSlice {
		t.Fatalf("Out.RPCServer = %s, want %s", got, serverOptionSlice)
	}
	if got, want := reflect.ValueOf(h.Out.RPCServer).Len(), len(rpcx.BuildServerOptions(so)); got != want {
		t.Fatalf("Out.RPCServer len = %d, want %d", got, want)
	}
}

// typeName 取选项产物的动态类型名（nil 安全）。
func typeName(v any) string {
	if v == nil {
		return "<nil>"
	}
	return reflect.TypeOf(v).String()
}
