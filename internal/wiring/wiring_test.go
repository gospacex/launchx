package wiring

import (
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gospacex/launchx/internal/component"
	"github.com/gospacex/launchx/internal/mode"
)

// 本文件是 dependency-injection capability W1–W7 的可执行判据。
//
// 设计取向：能靠源码断言的就不起进程，必须起外部工具的（Wire CLI 重生成）就显式
// Skip 并说明原因 —— 「跑不了就跳过」和「跑不了就假装通过」是两回事，
// 前者会留下痕迹，后者会骗人。

// wantSDKs 本次要求纳入注册面的 15 个 SDK。注意**没有 hubx**：hubx 的 wire.go
// 引用了已不存在的 configx/hubx 包与 configx.ViperLoader 类型，且它已演进到
// 类型化注册表，故本次按裁决移出。
var wantSDKs = []string{
	"cachex", "configx", "coordx", "dbx", "kvx", "lockx", "logx", "mongodbx",
	"mqx", "objectx", "otelx", "rpcx", "searchx", "vectorx", "webx",
}

func newEnvForTest(traceOn map[string]bool) *component.Env {
	if traceOn == nil {
		traceOn = map[string]bool{}
	}
	return &component.Env{TraceOn: traceOn, Strict: map[string]bool{}}
}

func TestCatalogCoversFifteenSDKs(t *testing.T) {
	bySDK := CatalogBySDK()
	if len(bySDK) != len(Catalog) {
		t.Fatalf("Catalog 有重名 SDK：条目 %d 去重后 %d", len(Catalog), len(bySDK))
	}
	if len(Catalog) != len(wantSDKs) {
		t.Errorf("注册面条目数 = %d，应为 %d", len(Catalog), len(wantSDKs))
	}
	for _, sdk := range wantSDKs {
		e, ok := bySDK[sdk]
		if !ok {
			t.Errorf("SDK %s 未登记在册", sdk)
			continue
		}
		if !e.HasProvider {
			t.Errorf("SDK %s 登记了但没有 provider —— 注册面不完整", sdk)
		}
		if e.Category != CategoryFoundation && e.Note == "" && e.Builder == "" {
			t.Errorf("SDK %s 缺 Note：没写「为什么是这样」，后人会当成遗漏", sdk)
		}
	}
	if _, ok := bySDK["hubx"]; ok {
		t.Errorf("hubx 不应出现在本次注册面（已按裁决移出）")
	}
}

// TestW1WireIsBuildTimeOnly W1：wire 只在编译构建 wireinject 时出现，运行时不可见。
//
// 为什么不用 `go list -deps` 直接断言：Go 解析包依赖时会把**所有**构建配置
// （含被 build tag 排除的文件）一并计入，所以模块内只要有一处 import wire，
// 依赖闭包里就会出现它 —— 这不代表运行时真的把 wire 链了进去。真正能区分二者的
// 是链接产物的模块清单，见本用例第 3 段探针。
func TestW1WireIsBuildTimeOnly(t *testing.T) {
	// 1) 源码级：除 wire.go（wireinject）外，不得有别的文件 import wire。
	//
	// 用 parser 取真实 import 而不是 grep 字符串 —— 注释里提到 wire（比如解释
	// 为什么 ProviderSet 不能放在这里）会被 grep 误判成违规。
	var offenders []string
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读取包目录失败：%v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		importsWire, err := fileImportsWire(e.Name())
		if err != nil {
			t.Fatalf("解析 %s 失败：%v", e.Name(), err)
		}
		if importsWire && e.Name() != "wire.go" {
			offenders = append(offenders, e.Name())
		}
	}
	if len(offenders) > 0 {
		t.Errorf("以下文件在 wireinject 之外引入了 wire：%v\n"+
			"（若移到 wire.go，注意那里只允许放注入器：本 fork 的生成器会把该文件的\n"+
			" 全部声明拷进 wire_gen.go，ProviderSet 变量拷进去就等于运行时依赖 wire）",
			offenders)
	}

	// wire.go 必须有 build tag，否则它会被默认构建编译进去。
	b, err := os.ReadFile("wire.go")
	if err != nil {
		t.Fatalf("wire.go 不存在：%v", err)
	}
	if !strings.Contains(string(b), "//go:build wireinject") {
		t.Errorf("wire.go 缺少 //go:build wireinject —— 它会被默认构建编译")
	}

	// 2) 产物级：wire_gen.go 绝不能 import wire。
	genImportsWire, err := fileImportsWire("wire_gen.go")
	if err != nil {
		t.Fatalf("wire_gen.go 不存在：%v（忘了跑 wire gen？）", err)
	}
	if genImportsWire {
		t.Errorf("wire_gen.go 引入了 wire —— 生成产物必须零 wire 依赖")
	}

	// 3) 链接级探针：真实编译一个二进制并检查其模块清单。这是唯一能把
	//    「依赖闭包里有」与「运行时链进去了」区分开的证据。
	probeGo := os.Getenv("PROBE_GO")
	if probeGo == "" {
		t.Skip("PROBE_GO 未设置，跳过链接级探针（源码级与产物级断言已执行）")
	}
	bin := filepath.Join(t.TempDir(), "probe.test")
	if out, err := exec.Command(probeGo, "test", "-c", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("编译探针失败：%v\n%s", err, out)
	}
	vout, err := exec.Command(probeGo, "version", "-m", bin).Output()
	if err != nil {
		t.Fatalf("读取二进制模块清单失败：%v", err)
	}
	for _, line := range strings.Split(string(vout), "\n") {
		if strings.Contains(line, "goforj/wire") || strings.Contains(line, "google/wire") {
			t.Errorf("链接产物里出现了 wire 依赖：%s", strings.TrimSpace(line))
		}
	}
}

// TestW3TraceOnDoesNotGateConstruction W3 / D1 红线：trace.on 只控入链，不控构造。
//
// 历史背景：曾经把这个开关当成构造开关，结果是 producer 变 nil、outbox 静默堆积
// 到重试上限才放弃 —— 故障形态是「消息没发出去但没人报错」，极难定位。
func TestW3TraceOnDoesNotGateConstruction(t *testing.T) {
	real := component.Lookup("db")
	t.Cleanup(func() { component.Register("db", real) })
	component.Register("db", func(e *component.Env) error {
		e.DB = &struct{}{} // 哨兵：证明构造确实发生了
		return nil
	})

	off, err := ProvideDB(newEnvForTest(map[string]bool{"db": false}))
	if err != nil {
		t.Fatalf("ProvideDB 返回错误：%v", err)
	}
	if !off.Constructed() {
		t.Errorf("D1 回归：trace.on.db=false 时句柄仍应为非空（门控只控入链）")
	}
	if off.TraceEnabled() {
		t.Errorf("trace=false 时 TraceEnabled() 应为 false")
	}

	on, err := ProvideDB(newEnvForTest(map[string]bool{"db": true}))
	if err != nil {
		t.Fatalf("ProvideDB 返回错误：%v", err)
	}
	if !on.Constructed() || !on.TraceEnabled() {
		t.Errorf("trace=true 时应构造且允许入链：constructed=%v trace=%v",
			on.Constructed(), on.TraceEnabled())
	}
}

// TestW4PresenceIsRuntimeAdjudication W4 反向断言：presence 由运行时裁决，不进图。
//
// 同一个 provider（同一张图），两个 Env 只差「段有没有配」，产出不同 —— 这证明
// 「有没有」是运行时按 Env 决定的，而不是靠切换不同注入器变体表达。一旦有人试图
// 把 presence 搬进图，就需要 2^10 个变体，走不通；这条断言守的就是这个边界。
//
// presence 未命中的真实形态：builder 自己发现没配，直接 return nil（既不设产物
// 也不报错）。下面的 fake 就是照这个形态写的。
func TestW4PresenceIsRuntimeAdjudication(t *testing.T) {
	real := component.Lookup("db")
	t.Cleanup(func() { component.Register("db", real) })

	// 命中：builder 设了产物。
	component.Register("db", func(e *component.Env) error {
		e.DB = &struct{}{}
		return nil
	})
	hit, err := ProvideDB(newEnvForTest(nil))
	if err != nil {
		t.Fatalf("ProvideDB 返回错误：%v", err)
	}
	if !hit.Constructed() {
		t.Errorf("presence 命中时应构造出句柄")
	}

	// 未命中：builder 什么都不做。
	component.Register("db", func(e *component.Env) error { return nil })
	miss, err := ProvideDB(newEnvForTest(nil))
	if err != nil {
		t.Fatalf("presence 未命中不应报错，应静默降级为零值：%v", err)
	}
	if miss.Constructed() {
		t.Errorf("presence 未命中时句柄应为空")
	}
}

// TestW4StrictControlsFailurePosture D2 红线：strict 决定失败是降级还是返回 error。
//
// 同一处构造失败，strict 缺省 → provider 返回零值不报错（启动继续）；
// strict.db=true → provider 必须把错误交出去，交由 Bootstrap 走完整反序退出链。
func TestW4StrictControlsFailurePosture(t *testing.T) {
	real := component.Lookup("db")
	t.Cleanup(func() { component.Register("db", real) })
	component.Register("db", func(e *component.Env) error {
		return ErrFoundationNotReady // 借用现有 error 做哨兵
	})

	degradeEnv := newEnvForTest(nil)
	h, err := ProvideDB(degradeEnv)
	if err != nil {
		t.Errorf("strict 缺省应降级（不返回错误），实际 err=%v", err)
	}
	if h.Constructed() {
		t.Errorf("降级时句柄应为空")
	}

	strictEnv := newEnvForTest(nil)
	strictEnv.Strict = map[string]bool{"db": true}
	if _, err := ProvideDB(strictEnv); err == nil {
		t.Errorf("strict.db=true 时必须返回错误")
	}
}

// TestW5NoGormInLibrarySource W5：库源码不得直接依赖具体驱动。
//
// 注意断言落在**源码**而非 go.mod：go.mod 里 gorm 是 direct，因为 examples/**
// 与库同属一个 module，demo 的 repository 必须用 gorm。go.mod 层面体现不出
// 「库不要用」，所以真相只能在源码层面守住。
func TestW5NoGormInLibrarySource(t *testing.T) {
	b, err := os.ReadFile("../component/db.go")
	if err != nil {
		t.Skipf("读不到 ../component/db.go：%v", err)
	}
	if strings.Contains(string(b), "gorm.io/gorm") {
		t.Errorf("component 层直接 import 了 gorm —— 库不得绑定具体 ORM（DB 句柄刻意保持 any）")
	}
}

// TestProvideModeReadySkipsWhenAlreadyAssembled 形态装配只做一次。
//
// 为什么值得单测：pipeline 的顺序是 mode.assemble → component.mount，而图里
// 形态产物排在可选组件之后，所以本图被求解时 mode 已经装过一遍了。若这里再装
// 一次，Out 会换成一批新产物（新的 webx 引擎、新的 kitex 选项），而 App 在
// mode 步骤已经把旧的 out.Webx 交给了 a.engine —— 症状是"引擎起来了，路由却挂
// 在另一个引擎上"，且不报任何错。这种错在 Jaeger 上表现为"链路全绿但没有业务
// span"，极难反查。
//
// 判据设计成不需要真配置：Out.Mode 非空时 Cfg 给 nil 也必须通过 —— 因为一旦
// 真去 Assemble，nil Cfg 必然报错。这就把"跳过"钉死了。
func TestProvideModeReadySkipsWhenAlreadyAssembled(t *testing.T) {
	h := &mode.AppHolders{Out: mode.Out{Mode: string(mode.ModeMonolith), Webx: "engine-sentinel"}}
	if _, err := ProvideModeReady(h); err != nil {
		t.Fatalf("已装配时应跳过 Assemble 并直接返回，实际 err=%v", err)
	}
	if h.Out.Webx != "engine-sentinel" {
		t.Errorf("Out.Webx 被换掉了（%v）—— 说明 Assemble 又跑了一次，"+
			"App 上已交出去的引擎句柄会变成孤儿", h.Out.Webx)
	}
}

// TestProvideModeReadyAssemblesWhenOutEmpty 反向断言：Out 为空时必须真去装配。
//
// 没有这条，"跳过"就可能退化成"什么都不做也算过"——那时 mode 装配被整个绕开，
// 服务会以「没有形态」的状态继续启动，比重复装配更糟。
func TestProvideModeReadyAssemblesWhenOutEmpty(t *testing.T) {
	if _, err := ProvideModeReady(&mode.AppHolders{}); err == nil {
		t.Error("Out.Mode 为空时必须执行 Assemble（nil 配置必然报错），实际返回 nil error")
	}
	if _, err := ProvideModeReady(nil); err == nil {
		t.Error("holders 为 nil 时必须报错")
	}
}

// TestEveryComponentBuilderIsWired 组件层每注册一个 builder，图里就得有归属。
//
// componentOrder 那张手写顺序表删掉之后，"加组件别忘接线"这件事就只剩这个断言
// 守着了 —— 漏掉的表现是：yaml 里配了段、builder 也在，但句柄恒为零值，
// 因为图里根本没人调它。
func TestEveryComponentBuilderIsWired(t *testing.T) {
	wired := map[string]bool{}
	for _, e := range Catalog {
		if e.Builder != "" {
			wired[e.Builder] = true
		}
	}
	for _, name := range component.Names() {
		if !wired[name] {
			t.Errorf("组件 builder %q 已注册却没接进 wire 图 —— 去 wiring.go 的 Catalog 补一条 Entry", name)
		}
	}
}

// TestConstructedRejectsTypedNil 零值句柄不得报成"已构造"。
//
// 背景见 base.Constructed 的注释：Env.Cache / KV / Mongo 是 map、slice 类型的
// 命名类型，零值装进 `any` 之后 `raw != nil` 恒为真。这里用一个同构的 map 零值
// 复现该形态，避免为了写这条测试去 import cachex（那会无谓地加一条依赖边）。
//
// 这条断言是 wire 接进主链路后才有的：在那之前 env 上这些字段恒为零值，
// 谁也没真的问过"它构造了没有"。
func TestConstructedRejectsTypedNil(t *testing.T) {
	var zero map[string]int // 与 cachex.Instances 同构的零值

	cases := []struct {
		name string
		raw  any
		want bool
	}{
		{"nil 接口", nil, false},
		{"typed nil map", zero, false},
		{"typed nil slice", []string(nil), false},
		{"typed nil pointer", (*int)(nil), false},
		{"非空 map", map[string]int{"a": 1}, true},
		{"非空字符串", "db-sentinel", true},
	}
	for _, c := range cases {
		if got := (base{name: "probe", raw: c.raw}).Constructed(); got != c.want {
			t.Errorf("%s: Constructed() = %v, want %v", c.name, got, c.want)
		}
	}
}

// fileImportsWire 用 go/parser 判断文件是否真的 import 了 wire。
//
// 不用 grep 字符串：注释里解释「为什么 ProviderSet 不能放在 wire.go」时也会提到
// wire，那不是违规，却是 grep 区分不出来的。
func fileImportsWire(path string) (bool, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
	if err != nil {
		return false, err
	}
	for _, imp := range f.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		if p == "github.com/goforj/wire" || p == "github.com/google/wire" {
			return true, nil
		}
	}
	return false, nil
}
