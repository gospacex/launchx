// 本文件：单体 demo 的集成验收（验收案例 E1）与**共享真机底座**。
//
// 为什么自己拉进程而不是依赖外部常驻进程：
//   - 8080 端口冲突（微服 gateway 也占 8080）让"先手工起一个单体再跑用例"长期
//     处于"这次能不能跑看谁先占端口"的状态，mongodbx / searchx 两段因此一直
//     没能实测 —— 不是不想验，是每次都被端口挡回来；
//   - 降级用例要改配置（死地址、strict 开关），手工起进程改不了这些。
//
// 故这里按"用例自己 build → 自己挑空闲端口 → 自己起 → 自己 SIGTERM 收尾"来写，
// 端口冲突从根上消失，T2–T5 也能在同一跑里真机执行。
//
// TestMain 会为整个包拉起一个共享实例并写入 MONOLITH_BASE：既有 trace 用例
// （trace_e2e_test.go）因此自动变成真机用例，不再需要人工先起进程。
// 需要独立配置的用例（E1 生命周期、降级）各自再拉自己那份。
//
// 环境不具备时（无 go / 无 mysql / 无 docker）一律 Skip 并写明缺什么，
// 不 PASS —— 在这类依赖真机的验收里两者是不同结论。
package monolith

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ===== 环境常量 =====
//
// 库名刻意用 order_mono：examples/mall 的 schema.sql 在 order_db 里建的 orders
// 表没有 item / qty / amount 列，三个 demo 共用 order_db 时单体会直接
// "Unknown column 'item'"。这是 demo 之间的库名串扰，不是 launchx 缺陷，
// 但用例必须避开它才能稳定复现。
const (
	e2eDBName     = "order_mono"
	e2eDBPassword = "hubx123"
	e2eMySQLRoot  = "rootpass"
	e2eMySQLAddr  = "127.0.0.1:3306"
	e2eDockerBin  = "/Applications/Docker.app/Contents/Resources/bin/docker"
	e2eContainer  = "mysql"
)

var (
	buildOnce  sync.Once
	buildPath  string
	buildFail  error
	schemaOnce sync.Once
	schemaFail error
)

// =====================================================================
// 共享底座：go 定位 / 构建 / 建表 / 配置副本 / 进程
// =====================================================================

// demoDir 返回 examples/monolith 目录（go test 的 CWD 就是它）。
func demoDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(dir, "config.yaml")); err != nil {
		return "", fmt.Errorf("请在 examples/monolith 目录下跑本用例（%s 下没有 config.yaml）", dir)
	}
	return dir, nil
}

// goBinPath 定位 go 可执行文件：本机 go 常不在 PATH（装在 ~/sdk 下），
// LookPath 失败时回落到该路径。
func goBinPath() (string, error) {
	if p, err := exec.LookPath("go"); err == nil {
		return p, nil
	}
	p := filepath.Join(os.Getenv("HOME"), "sdk", "go1.24.3", "bin", "go")
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		return p, nil
	}
	return "", fmt.Errorf("PATH 里没有 go，%s 也不存在", p)
}

// buildBinary 构建单体二进制（整包只构建一次）。
func buildBinary() (string, error) {
	buildOnce.Do(func() {
		bin, err := goBinPath()
		if err != nil {
			buildFail = err
			return
		}
		dir, err := demoDir()
		if err != nil {
			buildFail = err
			return
		}
		out, err := os.MkdirTemp("", "monolith-e2e-build-")
		if err != nil {
			buildFail = err
			return
		}
		target := filepath.Join(out, "monolith")
		cmd := exec.Command(bin, "build", "-o", target, "./cmd")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if b, err := cmd.CombinedOutput(); err != nil {
			buildFail = fmt.Errorf("go build -o %s ./cmd 失败：%v\n%s", target, err, b)
			return
		}
		buildPath = target
	})
	return buildPath, buildFail
}

// ensureSchema 建库建表（幂等，整包只跑一次）。
func ensureSchema() error {
	schemaOnce.Do(func() {
		if !portOpen(e2eMySQLAddr) {
			schemaFail = fmt.Errorf("mysql %s 不可达", e2eMySQLAddr)
			return
		}
		dir, err := demoDir()
		if err != nil {
			schemaFail = err
			return
		}
		cmd := exec.Command("bash", "scripts/bring-up.sh", "up")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"MYSQL_DB="+e2eDBName,
			"HUBX_DB_PW="+e2eDBPassword,
			"DOCKER_BIN="+dockerBin(),
		)
		if b, err := cmd.CombinedOutput(); err != nil {
			schemaFail = fmt.Errorf("scripts/bring-up.sh up 失败：%v\n%s", err, b)
			return
		}
	})
	return schemaFail
}

// dockerBin 优先用环境指定的 docker CLI（本机 Docker 不在 PATH）。
func dockerBin() string {
	if v := strings.TrimSpace(os.Getenv("DOCKER_BIN")); v != "" {
		return v
	}
	return e2eDockerBin
}

func portOpen(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 700*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// cfgEdit 一次配置改写。old=="" 表示在文件尾追加 new（用于新增段落）。
//
// 为什么要求 old 必须命中：配置改了名而用例还在替换旧串时，静默替换会变成
// "什么都没改"，用例会拿默认配置跑出一个看上去很绿的结论 —— 比失败危险得多。
type cfgEdit struct{ old, new string }

// writeConfigAt 生成一份配置副本：端口改到空闲端口，库名改到 order_mono。
func writeConfigAt(dir string, port int, edits ...cfgEdit) (string, error) {
	root, err := demoDir()
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(filepath.Join(root, "config.yaml"))
	if err != nil {
		return "", err
	}
	cfg := string(raw)
	all := append([]cfgEdit{
		{old: "127.0.0.1:8080", new: fmt.Sprintf("127.0.0.1:%d", port)},
		{old: "/order_db?", new: "/" + e2eDBName + "?"},
	}, edits...)
	for _, e := range all {
		if e.old == "" {
			cfg += "\n" + e.new
			continue
		}
		if !strings.Contains(cfg, e.old) {
			return "", fmt.Errorf("config.yaml 里找不到 %q：配置改名了，改判据而不是改配置", e.old)
		}
		cfg = strings.ReplaceAll(cfg, e.old, e.new)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o600); err != nil {
		return "", err
	}
	return dir, nil
}

// logBuffer 收集子进程输出（进程可能在并发写，故加锁）。
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// instance 一个由用例拉起的单体进程。
type instance struct {
	name  string
	port  int
	base  string
	dir   string
	cmd   *exec.Cmd
	logs  *logBuffer
	state chan struct{} // 进程退出后关闭
	code  int
	once  sync.Once
}

// launchRaw 拉起进程但不等待就绪（降级用例要观察"能不能起来"，不能先等死）。
func launchRaw(name string, edits ...cfgEdit) (*instance, error) {
	if err := ensureSchema(); err != nil {
		return nil, err
	}
	bin, err := buildBinary()
	if err != nil {
		return nil, err
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "monolith-e2e-run-")
	if err != nil {
		return nil, err
	}
	if _, err := writeConfigAt(dir, port, edits...); err != nil {
		return nil, err
	}
	logs := &logBuffer{}
	cmd := exec.Command(bin)
	cmd.Dir = dir // main.go 按相对路径读 config.yaml，故 CWD 必须是配置所在目录
	cmd.Env = append(os.Environ(), "HUBX_DB_PW="+e2eDBPassword)
	cmd.Stdout = logs
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动单体失败：%w", err)
	}
	in := &instance{
		name: name, port: port, base: fmt.Sprintf("http://127.0.0.1:%d", port),
		dir: dir, cmd: cmd, logs: logs, state: make(chan struct{}),
	}
	go func() {
		_ = cmd.Wait()
		in.code = cmd.ProcessState.ExitCode()
		close(in.state)
	}()
	return in, nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	p := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return p, nil
}

// exited 报告进程是否已退出。
func (in *instance) exited() bool {
	select {
	case <-in.state:
		return true
	default:
		return false
	}
}

// waitReady 等进程起来：判据是 GET /orders 返回订单数组，而不是端口能连
// （端口能连只能说明有个东西在听，可能是别的进程）。
func (in *instance) waitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if in.exited() {
			return fmt.Errorf("单体进程提前退出（code=%d）：\n%s", in.code, tail(in.logs.String(), 40))
		}
		body, code, err := in.try(http.MethodGet, "/orders", "")
		if err == nil && code == http.StatusOK {
			var list []map[string]any
			if json.Unmarshal([]byte(body), &list) == nil {
				return nil
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("%s 内没就绪（最后状态见下）：\n%s", timeout, tail(in.logs.String(), 40))
}

// waitExit 等进程自己退出（strict 即退用例用），返回退出码。
func (in *instance) waitExit(timeout time.Duration) (int, error) {
	select {
	case <-in.state:
		return in.code, nil
	case <-time.After(timeout):
		// SIGKILL 之前先要一份 goroutine 栈：直接杀掉就等于永远不知道它卡在哪。
		// Go 运行时收到 SIGQUIT 会把所有 goroutine 的栈打到 stderr（这里就是
		// in.logs），等一小会儿让这条输出走完管道。
		_ = in.cmd.Process.Signal(syscall.SIGQUIT)
		time.Sleep(1500 * time.Millisecond)
		stack := tail(in.logs.String(), 60)
		_ = in.cmd.Process.Signal(syscall.SIGKILL)
		return 0, fmt.Errorf("%s 内进程没有退出（退出链挂住了？下面是退出前的栈）：\n%s",
			timeout, stack)
	}
}

// do 发一次 HTTP 请求；传输层失败直接返回 error（判"进程不在了"用）。
func (in *instance) do(method, path, body string) (string, int, error) {
	var rdr *bytes.Reader
	if body == "" {
		rdr = bytes.NewReader(nil)
	} else {
		rdr = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, in.base+path, rdr)
	if err != nil {
		return "", 0, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	cli := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := cli.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return buf.String(), resp.StatusCode, nil
}

// try 同 do，但用于轮询（错误由调用方决定是否重试）。
func (in *instance) try(method, path, body string) (string, int, error) {
	return in.do(method, path, body)
}

// signalAndWait 发信号并等进程退出，返回退出码。
func (in *instance) signalAndWait(sig syscall.Signal, timeout time.Duration) (int, error) {
	if in.exited() {
		return in.code, nil
	}
	if err := in.cmd.Process.Signal(sig); err != nil {
		return -1, err
	}
	select {
	case <-in.state:
		return in.code, nil
	case <-time.After(timeout):
		_ = in.cmd.Process.Signal(syscall.SIGKILL)
		return -1, fmt.Errorf("发 %v 后 %s 内没有退出", sig, timeout)
	}
}

// stop 兜底收尾：先 SIGTERM，超时再 SIGKILL。
func (in *instance) stop() {
	in.once.Do(func() {
		if in.exited() {
			return
		}
		_ = in.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-in.state:
		case <-time.After(10 * time.Second):
			_ = in.cmd.Process.Signal(syscall.SIGKILL)
		}
	})
	if in.dir != "" {
		_ = os.RemoveAll(in.dir)
	}
}

// tail 取最后 n 行（报错时给全文会淹没原因）。
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// =====================================================================
// *testing.T 包装层：环境不具备 → Skip；进程行为不符 → Fatal
// =====================================================================

// launch 拉起一个单体实例并等到就绪，注册 t.Cleanup 收尾。
func launch(t *testing.T, name string, edits ...cfgEdit) *instance {
	t.Helper()
	if err := ensureSchema(); err != nil {
		t.Skipf("库表未就绪，跳过：%v", err)
	}
	if _, err := buildBinary(); err != nil {
		t.Skipf("构建单体二进制失败，跳过：%v", err)
	}
	in, err := launchRaw(name, edits...)
	if err != nil {
		t.Fatalf("拉起单体 %s 失败：%v", name, err)
	}
	t.Cleanup(in.stop)
	if err := in.waitReady(90 * time.Second); err != nil {
		in.stop()
		t.Fatalf("单体 %s 未就绪：%v", name, err)
	}
	t.Logf("单体 %s 就绪于 %s", name, in.base)
	return in
}

// get/post 语义化封装：状态码不符即 Fatal（进程级异常不是验收失败，但要停）。
func mustStatus(t *testing.T, in *instance, method, path, body string, want int) string {
	t.Helper()
	got, code, err := in.do(method, path, body)
	if err != nil {
		t.Fatalf("%s %s 请求失败：%v\n%s", method, path, err, tail(in.logs.String(), 20))
	}
	if code != want {
		t.Fatalf("%s %s → HTTP %d（want %d）：%s\n%s", method, path, code, want,
			truncate(got, 200), tail(in.logs.String(), 20))
	}
	return got
}

// fieldString 取响应 JSON 里的字符串字段。
func fieldString(t *testing.T, body, key string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("响应不是 JSON 对象（%s）：%s", truncate(body, 160), err)
	}
	v, _ := m[key].(string)
	return v
}

// mysqlScalar 经 docker exec 直查 mysql，拿到"库里到底是什么"。
//
// 为什么 HTTP 读回不算落库证据：订单有 cachex 读穿缓存，进程内也可能有态，
// 只有绕过应用直接查库才能把"写进了 mysql"和"读的是缓存"分开 —— 这两者在
// Jaeger 上长得一模一样。
func mysqlScalar(t *testing.T, sql string) string {
	t.Helper()
	out, err := mysqlQuery(sql)
	if err != nil {
		t.Skipf("无法直查 mysql，跳过库内核对：%v", err)
	}
	return strings.TrimSpace(out)
}

func mysqlQuery(sql string) (string, error) {
	bin := dockerBin()
	if fi, err := os.Stat(bin); err != nil || fi.IsDir() {
		return "", fmt.Errorf("docker CLI 不可用（%s）：DOCKER_BIN 可覆盖", bin)
	}
	cmd := exec.Command(bin, "exec", "-e", "MYSQL_QUERY", "-e", "MYSQL_PWD",
		e2eContainer, "sh", "-c", `exec mysql -uroot -N -B -e "$MYSQL_QUERY"`)
	cmd.Env = append(os.Environ(), "MYSQL_QUERY="+sql, "MYSQL_PWD="+e2eMySQLRoot)
	b, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v：%s", err, bytes.TrimSpace(b))
	}
	return string(b), nil
}

// =====================================================================
// TestMain：为整包拉起一个共享实例，让既有 trace 用例自动真机化
// =====================================================================

var sharedInstance *instance

func TestMain(m *testing.M) {
	code := func() int {
		// 外部已经起了（比如手工在 8080 跑着）就直接用，不重复拉。
		if os.Getenv("MONOLITH_BASE") != "" {
			return m.Run()
		}
		in, err := launchRaw("shared")
		if err != nil {
			fmt.Fprintf(os.Stderr, "monolith: 共享实例未拉起（%v），真机用例将 Skip\n", err)
			return m.Run()
		}
		if err := in.waitReady(90 * time.Second); err != nil {
			fmt.Fprintf(os.Stderr, "monolith: 共享实例未就绪（%v），真机用例将 Skip\n", err)
			in.stop()
			return m.Run()
		}
		sharedInstance = in
		os.Setenv("MONOLITH_BASE", in.base)
		fmt.Fprintf(os.Stderr, "monolith: 共享实例就绪 %s（本跑的 trace 用例均为真机）\n", in.base)
		defer in.stop()
		return m.Run()
	}()
	if sharedInstance != nil {
		_ = os.RemoveAll(sharedInstance.dir)
	}
	os.Exit(code)
}

// =====================================================================
// E1：启动 → HTTP 读写落 mysql → SIGTERM 优雅退出
// =====================================================================

// TestE1MonolithServesMySQLThenExitsGracefully 验收案例 E1。
//
// 三段判据各自对应一个"不看就发现不了"的失效形态：
//   - 读写落库：只判 HTTP 状态码会把"写进缓存/进程内态"也算通过；
//   - mysql 直查：把上面那种误判堵死；
//   - SIGTERM 退出码 + 端口释放：退出码 0 但端口还占着，说明 server.stop 没关监听，
//     下一次启动会 address already in use，而测试当时是绿的。
func TestE1MonolithServesMySQLThenExitsGracefully(t *testing.T) {
	in := launch(t, "e1")

	// ① 健康检查面：没挂业务中间件，起没起来看它最直接。
	//
	// 路径是 /healthz/live 与 /healthz/ready（webx 用 health_prefix 作前缀再拼
	// live/ready，裸 /healthz 没有处理器）—— 只探裸路径会拿到 404，
	// 然后把"服务没起来"和"探错了路径"混成同一个现象。
	for _, p := range []string{"/healthz/live", "/healthz/ready"} {
		mustStatus(t, in, http.MethodGet, p, "", http.StatusOK)
	}

	// ② 下单（写）：事务写 orders + order_items。
	item := fmt.Sprintf("e1-%d", time.Now().UnixNano())
	body := mustStatus(t, in, http.MethodPost, "/orders",
		fmt.Sprintf(`{"user_id":1,"item":%q,"sku":"SKU-E1","qty":2,"unit_price":12.5}`, item),
		http.StatusCreated)
	orderNo := fieldString(t, body, "order_no")
	if orderNo == "" {
		t.Fatalf("下单响应里没有 order_no：%s", truncate(body, 200))
	}
	t.Logf("下单 ✓ order_no=%s item=%s", orderNo, item)

	// ③ 读回（读）：字段对得上才算"写进去的和我以为的一样"。
	got := mustStatus(t, in, http.MethodGet, "/orders/"+orderNo, "", http.StatusOK)
	var order map[string]any
	if err := json.Unmarshal([]byte(got), &order); err != nil {
		t.Fatalf("订单响应解析失败：%v", err)
	}
	if order["item"] != item {
		t.Errorf("读回 item=%v，want %q", order["item"], item)
	}
	if order["status"] != "created" {
		t.Errorf("新建订单 status=%v，want created", order["status"])
	}

	// ④ 更新：PUT 后再读一次，确认写路径与读路径都通。
	mustStatus(t, in, http.MethodPut, "/orders/"+orderNo, `{"status":"paid"}`, http.StatusOK)
	got = mustStatus(t, in, http.MethodGet, "/orders/"+orderNo, "", http.StatusOK)
	_ = json.Unmarshal([]byte(got), &order)
	if order["status"] != "paid" {
		t.Errorf("更新后 status=%v，want paid", order["status"])
	}

	// ⑤ 库内核对：绕过应用直查，避免"读到的是缓存"被当成落库。
	inDB := mysqlScalar(t, fmt.Sprintf("SELECT status FROM %s.orders WHERE order_no='%s'", e2eDBName, orderNo))
	if inDB != "paid" {
		t.Errorf("mysql 里 status=%q，want paid —— HTTP 读到 paid 而库里不是，说明读的是缓存/进程内态", inDB)
	}
	t.Logf("落库 ✓ %s.orders 里该单 status=%s", e2eDBName, inDB)

	// ⑥ SIGTERM 优雅退出：退出码 0 + 日志里有信号行。
	code, err := in.signalAndWait(syscall.SIGTERM, 20*time.Second)
	if err != nil {
		t.Fatalf("SIGTERM 后进程没有退出：%v\n%s", err, tail(in.logs.String(), 30))
	}
	if code != 0 {
		t.Errorf("SIGTERM 退出码 = %d，want 0（退出链上有一环失败就会变成非 0）\n%s",
			code, tail(in.logs.String(), 30))
	}
	if !strings.Contains(in.logs.String(), "shutdown: signal terminated received") {
		t.Errorf("日志里没有收到信号的记录：\n%s", tail(in.logs.String(), 20))
	}

	// ⑦ 端口释放：进程没了但端口还连得上 = server.stop 没真关监听。
	if portOpen(fmt.Sprintf("127.0.0.1:%d", in.port)) {
		t.Errorf("进程已退出但 127.0.0.1:%d 仍可连：server.stop 没关掉监听，重启会 address already in use", in.port)
	}
	t.Logf("退出 ✓ code=0，端口 %d 已释放", in.port)
}

// TestE1StorageHandlesVerifiedInProcess 把手柄核对从"启动日志里看到了"升级成断言。
//
// 为什么值得单列一条：handlesrc 那六道核对是 fail-fast 的 —— 背离就起不来。这带来
// 一个反向风险：**它一旦没跑到，进程照样正常起来**，而且没有任何痕迹。上面几条
// 用例判的是"服务能不能用"，判不出"装配期那道检查有没有执行"（它可能压根没被
// 调用，也可能因为 Present=false 被整段跳过）。所以这里直接对着进程日志断言。
func TestE1StorageHandlesVerifiedInProcess(t *testing.T) {
	requireApp(t)
	if sharedInstance == nil {
		t.Skip("共享实例不是本跑拉起的（MONOLITH_BASE 指向外部进程），取不到它的启动日志")
	}

	var lines []string
	for _, l := range strings.Split(sharedInstance.logs.String(), "\n") {
		if strings.Contains(l, "storage handle verified") {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		t.Fatalf("进程日志里没有 storage handle verified 记录：装配期的句柄核对没跑\n%s",
			tail(sharedInstance.logs.String(), 30))
	}

	// 六个必须逐个出现且 joins_trace=true。用宽松正则而不是拆字段：logx 的落地
	// 形态（key=value 还是 JSON、冒号后有没有空格）随部署而变，把断言绑在编码上
	// 会让换个 encoder 就红，故用宽松正则匹配字段名与值。
	compRe := func(comp string) *regexp.Regexp {
		return regexp.MustCompile(`component"?[=:]"?\s*"?` + comp + `\b`)
	}
	for _, comp := range []string{"db", "cache", "kv", "mq", "search", "mongodb"} {
		var line string
		for _, l := range lines {
			if compRe(comp).MatchString(l) {
				line = l
				break
			}
		}
		if line == "" {
			t.Errorf("进程日志里没有 %s 的核对记录（配了段却没参与核对 = 该组件的入链能力无人看守）", comp)
			continue
		}
		if !regexp.MustCompile(`joins_trace"?[=:]"?\s*"?true`).MatchString(line) {
			t.Errorf("%s 的核对结果是 joins_trace=false：trace.on.%s 要求入链，经它执行的操作却不会有 span\n%s",
				comp, comp, truncate(line, 200))
			continue
		}
		t.Logf("%s ✓ %s", comp, truncate(line, 160))
	}
}

// TestE1MongoAndSearchBackendsReallyServe 把 mongodbx / searchx 从"配了段"推到"真的读写过"。
//
// 这两段长期没能实测，原因是单体的 8080 与微服 gateway 撞车，每次真机验证
// 都被端口挡回来（T4 因此一直是 Skip）。本包改由 TestMain 挑空闲端口拉进程后，
// 这两条路径第一次能被自动执行。
func TestE1MongoAndSearchBackendsReallyServe(t *testing.T) {
	requireApp(t)

	item := fmt.Sprintf("es-mongo-%d", time.Now().UnixNano())
	body := mustStatusShared(t, http.MethodPost, "/orders",
		fmt.Sprintf(`{"user_id":1,"item":%q,"sku":"SKU-EM","qty":1,"unit_price":3.5}`, item),
		http.StatusCreated)
	orderNo := fieldString(t, body, "order_no")
	if orderNo == "" {
		t.Fatalf("下单响应里没有 order_no：%s", truncate(body, 200))
	}

	// ① mongodbx：下单时写了一份快照，这里按订单号读回。
	//
	// 为什么不等 ES 那样轮询：Mongo 写入是同步的，读不到就是没写，重试只会把
	// "快照没落库"掩盖成"多等一会儿就好了"。
	snap, code := request(t, http.MethodGet, appBase()+"/orders/"+orderNo+"/snapshot", "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /orders/%s/snapshot → HTTP %d：%s（mongodbx 未真正读写）",
			orderNo, code, truncate(snap, 200))
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(snap), &doc); err != nil {
		t.Fatalf("快照响应不是 JSON 对象：%s", truncate(snap, 200))
	}
	if doc["order_no"] != orderNo {
		t.Errorf("快照 order_no=%v，want %s", doc["order_no"], orderNo)
	}
	t.Logf("mongodbx ✓ 快照读回 order_no=%s item=%v", orderNo, doc["item"])

	// ② searchx：ES 默认 1s refresh，索引写入不是立刻可见，故给一个短轮询窗口。
	// 轮询上限刻意写死 15s —— 超过这个时间还没命中就是没写进去，不是"再等等"。
	var hits []map[string]any
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		body, code := request(t, http.MethodGet,
			appBase()+"/orders/search?q="+url.QueryEscape(item), "", nil)
		if code != http.StatusOK {
			t.Fatalf("GET /orders/search → HTTP %d：%s", code, truncate(body, 200))
		}
		_ = json.Unmarshal([]byte(body), &hits)
		if containsOrderNo(hits, orderNo) {
			break
		}
		time.Sleep(time.Second)
	}
	if !containsOrderNo(hits, orderNo) {
		t.Fatalf("ES 检索 15s 内没命中订单 %s（hits=%d）：searchx 未真正读写索引",
			orderNo, len(hits))
	}
	t.Logf("searchx ✓ ES 检索命中订单 %s（hits=%d）", orderNo, len(hits))
}

// TestE1SharedInstanceIsOurs 反向断言：MONOLITH_BASE 必须指着本 demo。
//
// 为什么单独有一条：8080 上常驻着微服 gateway，若 MONOLITH_BASE 误指向它，
// 上面几条会因为"请求也返回了 2xx"而全部误绿 —— 这种绿比红危险。
// 指纹取 /orders/search：只有单体有这个端点（gateway 只会 404/502）。
func TestE1SharedInstanceIsOurs(t *testing.T) {
	requireApp(t)
	body, code := request(t, http.MethodGet, appBase()+"/orders/search?q=e1-ownership", "", nil)
	if code != http.StatusOK {
		t.Fatalf("%s 上没有可用的 /orders/search（HTTP %d）：MONOLITH_BASE 指的不是单体 demo",
			appBase(), code)
	}
	var hits []map[string]any
	if err := json.Unmarshal([]byte(body), &hits); err != nil {
		t.Fatalf("%s 的 /orders/search 返回的不是数组（%s）：那不是本 demo", appBase(), truncate(body, 160))
	}
}

// mustStatusShared 对共享实例发请求并断言状态码。
func mustStatusShared(t *testing.T, method, path, body string, status int) string {
	t.Helper()
	got, code := request(t, method, appBase()+path, body, nil)
	if code != status {
		t.Fatalf("%s %s → HTTP %d（want %d）：%s", method, path, code, status, truncate(got, 200))
	}
	return got
}

// containsOrderNo 判断 ES 命中列表里有没有指定订单号。
func containsOrderNo(hits []map[string]any, orderNo string) bool {
	for _, h := range hits {
		if v, ok := h["order_no"]; ok && fmt.Sprint(v) == orderNo {
			return true
		}
	}
	return false
}
