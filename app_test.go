package launchx

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gospacex/coordx"
	"github.com/gospacex/launchx/internal/component"
	"github.com/gospacex/launchx/internal/pipeline"
	"github.com/gospacex/logx"
)

// ======================= 测试替身（不连真底座） =======================

// fakeRegister 记录注册/注销调用的假注册中心。
//
// 断言「注册了什么、注销了什么」必须可观测，而真 consul 需要底座、
// 且会往本地注册中心写残留条目，故一律用替身。
type fakeRegister struct {
	mu     sync.Mutex
	regErr error

	registered    []coordx.ServiceInstance
	regServices   []string
	deregistered  []coordx.ServiceInstance
	deregServices []string
}

func (f *fakeRegister) Register(_ context.Context, service string, inst *coordx.ServiceInstance) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.regErr != nil {
		return f.regErr
	}
	f.regServices = append(f.regServices, service)
	f.registered = append(f.registered, *inst)
	return nil
}

func (f *fakeRegister) Deregister(_ context.Context, service string, inst *coordx.ServiceInstance) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deregServices = append(f.deregServices, service)
	f.deregistered = append(f.deregistered, *inst)
	return nil
}

func (f *fakeRegister) HealthCheck() error { return nil }
func (f *fakeRegister) Close() error       { return nil }

// counts 返回（注册次数，注销次数）。
func (f *fakeRegister) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.registered), len(f.deregistered)
}

// last 返回最后一次注册与注销的实例（未发生返回零值）。
func (f *fakeRegister) last() (reg coordx.ServiceInstance, dereg coordx.ServiceInstance) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n := len(f.registered); n > 0 {
		reg = f.registered[n-1]
	}
	if n := len(f.deregistered); n > 0 {
		dereg = f.deregistered[n-1]
	}
	return reg, dereg
}

// stubRegistry 用替身替换 registry 组件的构造器。
//
// component.Register 是组件层的公开扩展点：替换后 Bootstrap 走到
// component.mount 时拿到的就是替身句柄，无需任何底座。
func stubRegistry(t *testing.T, reg coordx.Register) {
	t.Helper()
	orig := component.Lookup("registry")
	component.Register("registry", func(env *component.Env) error {
		env.Registry = reg
		return nil
	})
	t.Cleanup(func() { component.Register("registry", orig) })
}

// fakeRPCServer 模拟 kitex server：Run() error / Stop() error
// （实测 kitex@v0.16.3/server/server.go:52-58）。
type fakeRPCServer struct {
	stopped bool
	ran     bool
}

func (f *fakeRPCServer) Run() error {
	f.ran = true
	return nil // 立刻返回，避免测试被监听阻塞
}

func (f *fakeRPCServer) Stop() error {
	f.stopped = true
	return nil
}

// warnCapture 捕获 Warn/Info 文案（内嵌真实日志器，其余级别转发给它）。
//
// 内嵌 nil 接口会在组件调用 InfoContext 时 panic，故必须嵌真实日志器。
type warnCapture struct {
	logx.Logger
	mu    sync.Mutex
	lines []string
}

func (w *warnCapture) Warn(msg string, fields ...logx.Field) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lines = append(w.lines, msg+" "+strings.Join(fieldTexts(fields), " "))
}

func (w *warnCapture) Info(msg string, fields ...logx.Field) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lines = append(w.lines, msg+" "+strings.Join(fieldTexts(fields), " "))
}

func (w *warnCapture) joined() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Join(w.lines, "\n")
}

// fieldTexts 把 logx.Field 拍成可匹配的文本（不依赖 logx 的格式化实现）。
func fieldTexts(fields []logx.Field) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, f.Key+"="+f.String)
	}
	return out
}

// ======================= 验收断言 =======================

// TestServerStopHookInvokedOnClose 断言服务端停机钩子真的接进了退出链：
// srv 档业务回存 RPC server → Run 起服 → Close 必须调到它的 Stop，
// 且 server.stop 早于 component.stop、早于 tracing.shutdown。
func TestServerStopHookInvokedOnClose(t *testing.T) {
	app, err := Bootstrap("testdata/srv.yaml")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	srv := &fakeRPCServer{}
	app.SetRPCServer(srv)

	if err := app.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := app.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !srv.stopped {
		t.Fatal("server stop hook was not invoked on close (serverStop 仍是死钩子)")
	}

	ev := app.Events()
	serverStop := ev.IndexOf(pipeline.StepServerStop)
	compStop := ev.IndexOf(pipeline.StepComponentStop)
	tracing := ev.IndexOf(pipeline.StepTracingShutdown)
	if serverStop < 0 || compStop < 0 || tracing < 0 {
		t.Fatalf("missing exit events, order = %v", ev.Order())
	}
	if serverStop >= compStop || serverStop >= tracing {
		t.Fatalf("server.stop must precede component.stop and tracing.shutdown: %v", ev.Order())
	}
}

// TestServerStopStaysNilWhenNeverStarted 断言 Run 从未起服时停机钩子保持 nil：
// 后台型服务（纯消费者）没有服务端，退出链不该对没监听过的句柄做停机。
func TestServerStopStaysNilWhenNeverStarted(t *testing.T) {
	app, err := Bootstrap("testdata/srv.yaml")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	srv := &fakeRPCServer{}
	app.SetRPCServer(srv) // 只回存，从不 Run

	if err := app.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if srv.ran {
		t.Fatal("sanity: the fake server must not run when Run() is never called")
	}
	if srv.stopped {
		t.Fatal("serverStop must stay nil until the server is actually started")
	}
}

// TestRegistryDeregisteredOnClose 断言 bff 档注册自身、退出时注销同一实例（A3）：
// 注销用的 instance 必须与注册时完全一致（addr/port/ID），
// 否则注销作用在另一个实例上，注册中心会残留脏条目。
func TestRegistryDeregisteredOnClose(t *testing.T) {
	reg := &fakeRegister{}
	stubRegistry(t, reg)

	app, err := Bootstrap("testdata/bff.yaml")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if app.Mode() != ModeBFF {
		t.Fatalf("mode = %q, want %q", app.Mode(), ModeBFF)
	}
	if n, _ := reg.counts(); n != 1 {
		t.Fatalf("bff must register itself once, got %d", n)
	}
	registered, _ := reg.last()
	if registered.Addr != "127.0.0.1" || registered.Port != 8081 {
		t.Fatalf("registered addr = %s:%d, want 127.0.0.1:8081 (地址必须来自配置)",
			registered.Addr, registered.Port)
	}
	if registered.ID == "" {
		t.Fatal("registered instance must carry an ID (注销按 ID 定位)")
	}

	if err := app.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	regN, deregN := reg.counts()
	if deregN != 1 {
		t.Fatalf("deregister calls = %d, want 1 (registered %d)", deregN, regN)
	}
	_, deregistered := reg.last()
	if deregistered.ID != registered.ID || deregistered.Addr != registered.Addr || deregistered.Port != registered.Port {
		t.Fatalf("deregistered instance %+v != registered instance %+v", deregistered, registered)
	}
	// 注销必须早于调用链冲刷（否则注销的 span 丢在缓冲里）
	ev := app.Events()
	if ev.IndexOf(pipeline.StepRegistryDeregister) >= ev.IndexOf(pipeline.StepTracingShutdown) {
		t.Fatalf("deregister must precede tracing.shutdown: %v", ev.Order())
	}
}

// TestSRVRegistersItself 断言 srv 档同样注册自身（四档里只有 bff/srv 注册）。
func TestSRVRegistersItself(t *testing.T) {
	reg := &fakeRegister{}
	stubRegistry(t, reg)

	app, err := Bootstrap("testdata/srv.yaml")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if n, _ := reg.counts(); n != 1 {
		t.Fatalf("srv must register itself once, got %d", n)
	}
	if err := app.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, deregN := reg.counts(); deregN != 1 {
		t.Fatalf("srv deregister calls = %d, want 1", deregN)
	}
}

// TestMonolithDoesNotRegister 断言单体既不注册也不注销，且退出时无 deregister 日志。
//
// 这是验收陷阱第 11 条的代码化：单体打出 deregister 说明 mode 装配串档
// （monolith 走了 bff/srv 分支），链路看似正常但装配矩阵已错。
func TestMonolithDoesNotRegister(t *testing.T) {
	reg := &fakeRegister{}
	stubRegistry(t, reg)

	app, err := Bootstrap("testdata/monolith.yaml")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if n, _ := reg.counts(); n != 0 {
		t.Fatalf("monolith must not register, got %d register calls", n)
	}

	cap := &warnCapture{Logger: app.Logger()}
	app.logger = cap
	if err := app.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, deregN := reg.counts(); deregN != 0 {
		t.Fatalf("monolith must not deregister, got %d calls", deregN)
	}
	if strings.Contains(cap.joined(), "deregister") || strings.Contains(cap.joined(), "register") {
		t.Fatalf("monolith must emit no register-related log, got: %q", cap.joined())
	}
}

// TestGatewayDoesNotRegister 断言网关档同样不注册（网关是流量入口，没人发现它）。
func TestGatewayDoesNotRegister(t *testing.T) {
	reg := &fakeRegister{}
	stubRegistry(t, reg)

	app, err := Bootstrap("testdata/gateway.yaml")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	defer func() { _ = app.Close() }()
	if n, _ := reg.counts(); n != 0 {
		t.Fatalf("gateway must not register, got %d register calls", n)
	}
}

// TestRegistryStrictFailsBootstrap 断言 strict.registry=true + 注册失败即退，
// 且错误链保留原始注册错误（不是降级）。
func TestRegistryStrictFailsBootstrap(t *testing.T) {
	boom := errors.New("consul unreachable")
	reg := &fakeRegister{regErr: boom}
	stubRegistry(t, reg)

	app, err := Bootstrap("testdata/strict-registry.yaml")
	if err == nil {
		_ = app.Close()
		t.Fatal("strict.registry=true with a failing register must fail bootstrap")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("error must wrap the register error, got: %v", err)
	}
}

// TestRegistryFailureDegradesByDefault 是上一条的对称面（陷阱 4：只测一种不算过）：
// strict 缺省 false 时注册失败只降级——Bootstrap 成功、不记实例、退出时不注销。
func TestRegistryFailureDegradesByDefault(t *testing.T) {
	reg := &fakeRegister{regErr: errors.New("consul unreachable")}
	stubRegistry(t, reg)

	app, err := Bootstrap("testdata/bff.yaml")
	if err != nil {
		t.Fatalf("strict unset must degrade, not fail: %v", err)
	}
	cap := &warnCapture{Logger: app.Logger()}
	app.logger = cap
	if err := app.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	regN, deregN := reg.counts()
	if regN != 0 {
		t.Fatalf("failed register must not be recorded, got %d", regN)
	}
	if deregN != 0 {
		t.Fatalf("degraded (unregistered) app must not deregister, got %d", deregN)
	}
}

// TestRegisterFailDegradesByDefault 断言注册失败的降级分支带后果文案。
//
// 单独在单元层断言是因为降级发生在 Bootstrap 的注册步骤内（此时 App 的日志器
// 刚由骨架装好，测试插不进去），后果文案又是陷阱 4 的判据——降级不打 Warn
// 就等于「服务不被发现」被静默吞掉。
func TestRegisterFailDegradesByDefault(t *testing.T) {
	cap := &warnCapture{}
	app := &App{serviceName: "degrade-svc", logger: cap}

	if err := app.registerFail(errors.New("consul unreachable")); err != nil {
		t.Fatalf("strict unset must degrade, not fail: %v", err)
	}
	if !strings.Contains(cap.joined(), "不被发现") {
		t.Fatalf("degraded register must warn with consequence, got: %q", cap.joined())
	}
	if !strings.Contains(cap.joined(), "strict.registry") {
		t.Fatalf("degraded register must hint strict.registry, got: %q", cap.joined())
	}
	cap.mu.Lock()
	warned := len(cap.lines) > 0
	cap.mu.Unlock()
	if !warned {
		t.Fatal("degraded register must warn")
	}
}

// TestRegisterFailStrictReturnsError 是上一条的对称面：strict 置真时返回带 %w 的 error。
func TestRegisterFailStrictReturnsError(t *testing.T) {
	boom := errors.New("consul unreachable")
	app := &App{
		serviceName: "strict-svc",
		comp:        &component.Env{Strict: map[string]bool{"registry": true}},
	}
	err := app.registerFail(boom)
	if err == nil {
		t.Fatal("strict.registry=true must fail fast")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("error must wrap the register error, got: %v", err)
	}
}

// TestSelfAddrFromConfigWithEnv 断言自身注册地址来自配置且支持 ${ENV} 展开（P3）：
// 仓库里不得出现硬编码地址，否则网格复用二进制时必然 fork 一份改常量。
func TestSelfAddrFromConfigWithEnv(t *testing.T) {
	t.Setenv("LX_TEST_SELF_ADDR", "10.0.0.7:9099")
	reg := &fakeRegister{}
	stubRegistry(t, reg)

	app, err := Bootstrap("testdata/selfaddr.yaml")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	defer func() { _ = app.Close() }()

	registered, _ := reg.last()
	if registered.Addr != "10.0.0.7" || registered.Port != 9099 {
		t.Fatalf("registered = %s:%d, want 10.0.0.7:9099 (service.self.addr 的 ${ENV} 未生效)",
			registered.Addr, registered.Port)
	}
	if os.Getenv("LX_TEST_SELF_ADDR") == "" {
		t.Fatal("sanity: env must be set")
	}
}

// ======================= 退出链有界性（P0：进程必须退得出来） =======================

// TestCloseNeverHangsOnBlockedStopHook 退出链每一环都有界：某一环卡死时 Close
// 必须在该环的上限内返回，而不是跟着一起挂住。
//
// 为什么值得单列一条：这个缺陷的形态是「进程永远收不到 SIGTERM」——K8s 优雅期
// 耗尽后直接 SIGKILL，而进程自己还以为在正常收尾。现实触发者（2026-10-07 实测）
// 是 kafka-go：Reader 的 GroupID 非空即起 ConsumerGroup 协程，它阻塞在 joinGroup
// 的网络读上且不设读 deadline，于是 Reader.Close 永久等待。这类驱动级行为
// 修不完，只能由退出链统一兜住。
func TestCloseNeverHangsOnBlockedStopHook(t *testing.T) {
	app, err := Bootstrap("testdata/srv.yaml")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	blocked := make(chan struct{})
	app.OnStopNamed("blocked-for-test", func() error {
		<-blocked // 永不返回：模拟驱动 Close 卡死
		return nil
	})

	limit := app.stopTimeout()
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- app.Close() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("卡住的一环不应让 Close 返回错误（超时是已处理的降级，不是失败）：%v", err)
		}
	case <-time.After(limit + 5*time.Second):
		t.Fatalf("Close 跟着卡住的回调一起挂住了（%s 内没有返回）——退出链又变回无界了",
			limit+5*time.Second)
	}
	if elapsed := time.Since(start); elapsed > limit+5*time.Second {
		t.Fatalf("Close 耗时 %s，超过上限 %s", elapsed, limit)
	}

	// 超时必须留痕：否则线上只会看到「进程退出了」，看不出有一环被放弃。
	if app.Events().IndexOf("stop.timeout.blocked-for-test") < 0 {
		t.Fatalf("事件流里没有 stop.timeout 记录，卡住的一环被静默吞掉了：%v", app.Events().Order())
	}
}

// TestStopHookErrorStillAggregated 确认「超时豁免」没有把真实失败一起豁免掉：
// 回调自己报的 error 照样进聚合，只是超时不算。
func TestStopHookErrorStillAggregated(t *testing.T) {
	app, err := Bootstrap("testdata/srv.yaml")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	boom := errors.New("stop failed on purpose")
	app.OnStopNamed("failing-for-test", func() error { return boom })

	if err := app.Close(); err == nil {
		t.Fatal("回调返回的 error 必须进聚合，got nil")
	} else if !errors.Is(err, boom) {
		t.Fatalf("聚合结果丢失原始 error：%v", err)
	}
	if app.Events().IndexOf("stop.timeout.failing-for-test") >= 0 {
		t.Fatal("返回 error 的回调不应被记为超时")
	}
}

// TestCloseGoesThroughPipelineShutdown 断言退出链的骨架真的由 pipeline.Shutdown
// 执行，而不是 App.Close 里另写一份循环。
//
// 为什么值得单列一条：两份退出链并存时，改了 pipeline.Shutdown（幂等 / 事件
// 记录 / 错误聚合）的人会以为退出链跟着改了，实际没走那份代码 —— 死代码比
// 重复代码更危险，它看起来像被维护着。判据取**运行时**证据而不是 grep：
// Shutdown 给每一步的错误加 `pipeline step %q` 前缀，这个前缀只可能来自它。
//
// 顺带钉住退出链的**完整顺序契约**（此前只断言了 server.stop 与 deregister
// 的先后两条，中间漏掉 metrics.listener / component.stop 等环都能溜过去）。
func TestCloseGoesThroughPipelineShutdown(t *testing.T) {
	app, err := Bootstrap("testdata/srv.yaml")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	boom := errors.New("stop failed on purpose")
	app.OnStopNamed("failing-for-test", func() error { return boom })

	closeErr := app.Close()
	if closeErr == nil {
		t.Fatal("回调返回的 error 必须进聚合，got nil")
	}
	if !errors.Is(closeErr, boom) {
		t.Fatalf("聚合结果丢失原始 error：%v", closeErr)
	}
	if !strings.Contains(closeErr.Error(), "pipeline step") {
		t.Fatalf("退出链没有走 pipeline.Shutdown（错误里缺少它独有的 `pipeline step` 前缀）：%v", closeErr)
	}

	// 顺序契约：每一步都必须留下事件，且严格按下面的次序。
	want := []string{
		pipeline.StepSetupStop,
		pipeline.StepServerStop,
		pipeline.StepMetricsListener,
		pipeline.StepComponentStop,
		pipeline.StepRegistryDeregister,
		pipeline.StepTracingShutdown,
		pipeline.StepMetricsShutdown,
		pipeline.StepLoggerClose,
	}
	ev := app.Events()
	prev := -1
	for _, name := range want {
		at := ev.IndexOf(name)
		if at < 0 {
			t.Fatalf("退出链缺一步 %q，实际只记录了：%v", name, ev.Order())
		}
		if at <= prev {
			t.Fatalf("退出链顺序错乱：%q(%d) 不晚于前一步(%d)，实际 %v", name, at, prev, ev.Order())
		}
		prev = at
	}
}
