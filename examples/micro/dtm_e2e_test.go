// Package micro 的 DTM 事务链真机验收（tasks.md 4.2 / plan part5 Task 13）。
//
// 前置（底座 + dtm + 六进程 + 四个分支面）：
//
//	docker compose -f /Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml up -d consul jaeger mysql redis kafka
//	dtm server 起在 127.0.0.1:36789(http) / 127.0.0.1:36790(grpc)，存储库 dtm_db
//	export MYSQL_ROOT_PW=<mysql root 口令>   # 四库对账 + dtm_db.trans_global 都要它（见 mysqlConn）
//	export HUBX_DB_PW=<应用账号 mall_rw 口令> # 业务账号，四库读写（配置里的 ${HUBX_DB_PW}）
//	bash examples/micro/scripts/dtm_e2e.sh   # 种子 + 起六进程
//
// 底座 / dtm / 六进程任一缺失时全部真机用例 **Skip**，不是 PASS —— 在依赖真机的
// 验收里「跳过」和「通过」是两种结论，混同就等于给自己开了一张永久通行证。
//
// 三条已钉死的口径（本文件的判据全部按它们写，写错就是把已实测的事实当猜测）：
//
//  1. **补偿段与正向同一条 traceID**，不是新根。dtm 按 step 共享载荷（Saga 的
//     compensate 与 action 同取 BinPayloads[i]；TCC 的 cancel 与 confirm 是同一份
//     TransBranch 的副本），两侧拿到同一份 traceparent，故补偿分支面 Extract
//     挂回的是**正向那条 trace**。design.md D4（2026-10-13 真机实测修正）。
//     本文件任何一条判据都不写「补偿是新根」。
//  2. **TCC 的 Confirm 由 dtm 自动驱动**，bff 只发三个 Try；outbox 行**只在
//     Confirm 后出现**（srv-order DtmTryPlace 的签名里根本没有 OutboxFill）。
//  3. **失败走 gRPC status 不是响应体**（D7a）：业务失败 Aborted、基础失败
//     Internal，dtm server 从不解析分支响应体。
//
// 为什么这些判据写在这个包：四域分支面在 srv-<域>/internal/dtm/ 下，Go 的
// internal 规则禁止 examples/micro import 它们（dtm_failsem_test.go 的文件头
// 记着同一个约束）。能跨进程观察到的面只有 HTTP（经网关）、MySQL、Jaeger、
// dtm server 的 gRPC —— 本文件的判据就只从这四个面上取。
package micro

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gospacex/launchx/internal/traceutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	// 仅为把 "mysql" 这个 database/sql 驱动注册进来：断言要真查四库与 dtm_db，
	// 而 go.mod 里 go-sql-driver 是 indirect，直接 import 它会把一条间接依赖
	// 提成直接依赖（go.mod churn）。gorm.io/driver/mysql 是既有直接依赖，且它
	// 在 init 里 import 了 go-sql-driver，副作用注册同名驱动。
	_ "gorm.io/driver/mysql"
)

// ---------- 真机前置 ----------

// requireDtm 探 dtm server 的 gRPC 口。不可达即 Skip。
//
// 本文件每条判据都建立在「dtm 真在推进事务」之上：dtm 没起时让它们红，就是把
// 环境问题伪装成验收失败。
func requireDtm(t *testing.T) {
	t.Helper()
	addr := envOrDefault("MICRO_DTM", "127.0.0.1:36790")
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Skipf("dtm server 不可达（%s）：%v（起法：dtm 起在 36789/36790，"+
			"存储库 dtm_db，见 examples/micro/README.md 的 DTM 段）", addr, err)
	}
	_ = conn.Close()
}

// branchFacePorts 四域分支面端口（README 端口表）。
var branchFacePorts = map[string]int{
	"product": 9041,
	"order":   9042,
	"stock":   9043,
	"points":  9044,
}

// requireDtmChain 断言「六进程 + 四个分支面」都在跑；任一缺失即 Skip。
//
// 为什么单独一条而不是逐个测试里探活：六个进程是**同一批**前置，分散探活会
// 让同一份缺失在六处报六种不同的错，而「缺哪一个、怎么起」这个答案只该有一处。
func requireDtmChain(t *testing.T) {
	t.Helper()
	requireJaeger(t) // 复用 trace_e2e_test.go：Jaeger 不可达即 Skip 并写明起法
	requireChain(t)  // 六进程 HTTP 面活着（/api/ping 回 demo-bff）
	requireDtm(t)

	for _, dom := range dtmDomains {
		addr := fmt.Sprintf("127.0.0.1:%d", branchFacePorts[dom])
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Skipf("srv-%s 的 dtm 分支面没在监听（%s）：%v（起法：起六进程，分支面由各域 "+
				"app/setup.go 的 dtm.Setup 装配，地址取自 dtm.branch.listen_addr）", dom, addr, err)
		}
		_ = conn.Close()
	}

	db := mysqlConn(t)
	defer func() { _ = db.Close() }()

	// 积分账户是 Saga 第四分支（points.Debit）的前置：lockAccount 查不到账户即
	// 业务失败，整条 Saga 每次都去补偿。这不是「环境缺件」而是「种子里没这条
	// 数据」，两者混为一谈会让人去查 dtm 而真正的原因在种子里，故分开写。
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM points_db.point_accounts WHERE user_id = ?`,
		testUserID).Scan(&n); err != nil {
		t.Fatalf("查 points_db 失败：%v", err)
	}
	if n == 0 {
		t.Skipf("points_db.point_accounts 里没有 user_id=%d 的账户 —— Saga 第四分支必然业务失败，"+
			"四库对账会一直红。灌法：INSERT INTO points_db.point_accounts (user_id, balance, "+
			"total_earned, total_spent) VALUES (%d, 1000, 1000, 0);（或跑 scripts/dtm_e2e.sh 的种子段）",
			testUserID, testUserID)
	}

	// ② 余额垫底。测试机会被反复扣：points.Debit 的 cost 是成交额 1%（9 分），
	//    扣到 0 之后每一笔下单都变成业务失败，整条 Saga 每次都去补偿 ——
	//    症状长得像「Saga 补偿坏了」，而真因是账户见底。
	//
	//    为什么是补足而不是 Skip：Skip 会让「跑第二遍」变成一件不确定的事（上一遍
	//    恰好把余额扣穿了，这遍就 Skip），验收结果取决于历史。补足只加不减，
	//    且本文件所有判据都是**相对基线**的差值，补多少都不影响结论 ——
	//    所以它**照常打印**，不藏在静默里。
	if bal := scalar(t, db, `SELECT balance FROM points_db.point_accounts WHERE user_id = ?`, testUserID); bal < 200 {
		if _, err := db.Exec(`UPDATE points_db.point_accounts SET balance = 1000 WHERE user_id = ?`, testUserID); err != nil {
			t.Fatalf("把 user_id=%d 的积分余额垫到 1000 失败：%v", testUserID, err)
		}
		t.Logf("积分账户余额 %d < 200，已垫到 1000（测试机被反复扣；所有判据都是相对基线的差值，不受影响）", bal)
	}

	// 静默期：dtm 里没有未终结的全局事务。四库对账全靠「与基线逐项比对」，而
	// 基线只在没有别人在动账时才成立 —— 上一轮跑剩的事务在取消途中落库，会把
	// 基线打歪，于是判据报出一个与本次提交无关的差值。
	//
	// 这不是洁癖：实测不静默时，Cancel 用例会拿到「stock frozen 基线=1、
	// 取消后=0」这种差值，然后归因成「Cancel 只撤了一半」—— 而那 1 是上一轮
	// 留下的。假红比不判更坏。
	waitUntil(t, 60*time.Second, "dtm 静默（无未终结的全局事务）", func() bool {
		return scalar(t, db, `SELECT COUNT(*) FROM dtm_db.trans_global
			WHERE status NOT IN ('succeed','failed','rollbacked')`) == 0
	})
}

// testUserID / testProductID / testQty 测试用的业务入参。
const (
	testUserID    = 1001
	testProductID = 1
	testQty       = 1
)

// tccBase TCC 端点的基址。
//
// 口径：tasks.md 3.2 与 plan part5 的依赖事实表都写「TCC `POST /api/presale/lock`
// （经 gateway 前缀 `/api`）」，故默认走网关 —— 那是**对外契约**，绕开它就等于
// 把验收判在一条没人走的路上。
//
// MICRO_TCC_BASE 只作**排障用**（把基址指到 bff 直连口），用来把「路由不通」与
// 「TCC 事务链本身坏了」这两件事分开。它不是让判据变绿的旁路：默认路径该红就红。
func tccBase() string {
	if v := strings.TrimSpace(os.Getenv("MICRO_TCC_BASE")); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return gatewayBase()
}

// requireTccRouteReachable 先探 TCC 端点是否注册；未注册（404）时把根因点名，
// 不让三个 TCC 用例各自报一遍「HTTP 404」。
//
// 这条**不是 Skip**：底座与链路都在跑，端点 404 是验收失败，把它记成 Skip
// 就是把「对外路由不通」伪装成「环境没起」。
//
// 探针必须**不落任何数据**：发一份 user_id/product_id/quantity 全 0 的请求，
// bff 在 handler 入口的参数校验处就 400 返回，根本走不到 GetProduct 预检与
// Submit。
//
// 这一条是踩出来的：初版拿一份**正常**请求探路由，于是每个 TCC 用例在正式提交
// 之前都先偷偷提交了一笔真的全局事务（真冻结、真 Confirm、真写 outbox），而
// 后面所有的「outbox 未新增」「三域 frozen 回基线」判据都被它自己顶高了 ——
// 判据红了却归因成「outbox 在 Confirm 之前就写了」。**一个会改数据的探针不是探针。**
func requireTccRouteReachable(t *testing.T) string {
	t.Helper()
	base := tccBase()
	body, code := do(t, "POST", base+"/api/presale/lock",
		`{"user_id":0,"product_id":0,"quantity":0}`, authHeader())
	if code == 404 {
		t.Fatalf("TCC 端点 %s/api/presale/lock 返回 404（响应体：%s）\n"+
			"  根因不在 dtm 链路上，而在 bff 的路由前缀：gateway 的路由表是 `prefix: /api, strip: true`，"+
			"转发时会把 /api 剥掉，bff 因此必须注册 /presale/lock；而 examples/micro/bff/app/setup.go "+
			"注册的是 /api/presale/lock（同文件其余六条路由都不带 /api 前缀）。"+
			" 排障对照：把 MICRO_TCC_BASE 指到 bff 直连口（http://127.0.0.1:8081）可验证 TCC 事务链本身是否健康。",
			base, truncate(body, 120))
	}
	if code != 400 {
		t.Fatalf("TCC 路由探针返回 HTTP %d，want 400（零参请求应在参数校验处被拒）。"+
			"返回别的东西说明 handler 的入参校验没了，这份探针就不再是「零副作用」的探针：%s",
			code, truncate(body, 120))
	}
	return base
}

// ---------- MySQL ----------

// mysqlConn 取一条到本机 MySQL 的连接。
//
// 为什么用 root（MYSQL_ROOT_PW）而不是业务账号 mall_rw（HUBX_DB_PW）：barrier 表
// 在四库内、而 `dtm_db.trans_global` 是 dtm server 的库 —— 判据要回答「这条全局
// 事务在 dtm 侧到哪一步了」，业务账号按 schema.sql 的授权只够四库，够不到 dtm_db。
// 口令一律从环境取，缺省即 Skip，绝不写默认值：悄悄用某个口令连上，只会让真正
// 去改口令的人发现改了也没生效。
func mysqlConn(t *testing.T) *sql.DB {
	t.Helper()
	pw := strings.TrimSpace(os.Getenv("MYSQL_ROOT_PW"))
	if pw == "" {
		t.Skip("环境变量 MYSQL_ROOT_PW 未设置，跳过四库对账（口令一律从环境取：脚本与仓内都不写明文）")
	}
	dsn := fmt.Sprintf("root:%s@tcp(%s)/?parseTime=true&timeout=5s&readTimeout=10s",
		pw, envOrDefault("MICRO_MYSQL", "127.0.0.1:3306"))
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("打开 MySQL 失败：%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Skipf("MySQL 不可达：%v（起法：docker compose -f "+
			"/Users/hyx/work/gowork/src/hubx/dockers/docker-compose.yml up -d mysql）", err)
	}
	return db
}

// scalar 查一个单值标量。
func scalar(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var v sql.NullInt64
	if err := db.QueryRow(query, args...).Scan(&v); err != nil {
		t.Fatalf("查询失败 %s（args=%v）：%v", query, args, err)
	}
	if !v.Valid {
		t.Fatalf("查询 %s（args=%v）返回 NULL —— 判据要的是确定值，NULL 说明取数口径错了", query, args)
	}
	return v.Int64
}

// text 查一个字符串标量（无行时返回 ""）。
func text(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	var v sql.NullString
	if err := db.QueryRow(query, args...).Scan(&v); err != nil {
		t.Fatalf("查询失败 %s（args=%v）：%v", query, args, err)
	}
	return v.String
}

// fourDB 四库对账快照。
//
// 选这几列的理由：每一条都对应一个域的账变落点，且成对出现（Deduct 减 total 与
// available、Release 两个都加回来；TryFreeze 只动 available↔frozen、Confirm 再
// 减 total）。只挑一列去判「回基线」会把「减了 available 没减 total」这种
// 账面漂移放过去。
type fourDB struct {
	stockTotal, stockAvailable, stockFrozen int64
	sold                                    int64
	ptsBalance, ptsFrozen, ptsTotalSpent    int64
	orders, outbox                          int64
}

func (s fourDB) String() string {
	return fmt.Sprintf("stock{total=%d available=%d frozen=%d} sold=%d points{balance=%d frozen=%d spent=%d} orders=%d outbox=%d",
		s.stockTotal, s.stockAvailable, s.stockFrozen, s.sold,
		s.ptsBalance, s.ptsFrozen, s.ptsTotalSpent, s.orders, s.outbox)
}

func snapshotFourDB(t *testing.T, db *sql.DB) fourDB {
	t.Helper()
	return fourDB{
		stockTotal:     scalar(t, db, `SELECT total FROM stock_db.stocks WHERE product_id = ?`, testProductID),
		stockAvailable: scalar(t, db, `SELECT available FROM stock_db.stocks WHERE product_id = ?`, testProductID),
		stockFrozen:    scalar(t, db, `SELECT frozen FROM stock_db.stocks WHERE product_id = ?`, testProductID),
		sold:           scalar(t, db, `SELECT COALESCE((SELECT sold FROM product_db.product_sales WHERE product_id = ?), 0)`, testProductID),
		ptsBalance:     scalar(t, db, `SELECT balance FROM points_db.point_accounts WHERE user_id = ?`, testUserID),
		ptsFrozen:      scalar(t, db, `SELECT frozen FROM points_db.point_accounts WHERE user_id = ?`, testUserID),
		ptsTotalSpent:  scalar(t, db, `SELECT total_spent FROM points_db.point_accounts WHERE user_id = ?`, testUserID),
		orders:         scalar(t, db, `SELECT COUNT(*) FROM order_db.orders`),
		outbox:         scalar(t, db, `SELECT COUNT(*) FROM order_db.outbox`),
	}
}

// orderSubmitResp POST /api/order（与 /api/presale/lock）的提交响应。
type orderSubmitResp struct {
	Code    int    `json:"code"`
	DTMGID  string `json:"dtm_gid"`
	OrderNo string `json:"order_no"`
	Message string `json:"message"`
	Error   string `json:"error"`
}

func submitOrder(t *testing.T, failInject string) orderSubmitResp {
	t.Helper()
	payload := fmt.Sprintf(`{"user_id":%d,"product_id":%d,"quantity":%d`, testUserID, testProductID, testQty)
	if failInject != "" {
		payload += fmt.Sprintf(`,"fail_inject":%q`, failInject)
	}
	payload += "}"
	body, code := do(t, "POST", gatewayBase()+"/api/order", payload, authHeader())
	if code != 200 {
		t.Fatalf("POST /api/order 返回 HTTP %d：%s（链路异常，不是验收失败）", code, truncate(body, 200))
	}
	var out orderSubmitResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("解析 /api/order 响应失败：%v（响应体：%s）", err, truncate(body, 200))
	}
	if out.DTMGID == "" {
		t.Fatalf("提交响应里没有 dtm_gid：%s —— 没有 gid 就无法按业务号把四域 span 归到一笔事务上", truncate(body, 200))
	}
	return out
}

func submitPresale(t *testing.T, failInject string) (int, orderSubmitResp) {
	t.Helper()
	payload := fmt.Sprintf(`{"user_id":%d,"product_id":%d,"quantity":%d`, testUserID, testProductID, testQty)
	if failInject != "" {
		payload += fmt.Sprintf(`,"fail_inject":%q`, failInject)
	}
	payload += "}"
	body, code := do(t, "POST", tccBase()+"/api/presale/lock", payload, authHeader())
	var out orderSubmitResp
	_ = json.Unmarshal([]byte(body), &out)
	return code, out
}

// ---------- dtm 侧事务状态 ----------

// waitTransStatus 等一条全局事务在 dtm_db 里到达终态，返回终态。
func waitTransStatus(t *testing.T, db *sql.DB, gid string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		last = text(t, db, `SELECT status FROM dtm_db.trans_global WHERE gid = ?`, gid)
		if isFinalTransStatus(last) {
			return last
		}
		time.Sleep(time.Second)
	}
	return last
}

// isFinalTransStatus 判全局事务是否已到终态。
//
// 注意 "aborting" **不在**其中：它是 TCC Try 失败后的中间态（dtm 已决定回滚、
// 正在逆序 Cancel 各分支），把它当终态会让判据在 Cancel 还没落库时就通过 —— 而
// 「三域 frozen 归零」恰恰要在那之后才成立。实测 TCC 注入失败后先 aborting、
// 数秒后才 failed。
func isFinalTransStatus(s string) bool {
	switch s {
	case "succeed", "failed", "rollbacked":
		return true
	}
	return false
}

// maxTransID 取某类型全局事务当前的最大自增 id（提交前取一次）。
func maxTransID(t *testing.T, db *sql.DB, transType string) int64 {
	t.Helper()
	return scalar(t, db, `SELECT COALESCE(MAX(id), 0) FROM dtm_db.trans_global WHERE trans_type = ?`, transType)
}

// latestTransGID 取 afterID 之后最新一条某类型全局事务的 gid。
//
// 为什么需要它：TCC 的 Try 阶段失败时 `dtmgrpc.TccGlobalTransaction` 会把 Abort
// 上抛，bff 返回 5xx 且**不带 gid**（响应体只有错误文案）。而「Try 失败后 dtm 到底
// 推进到哪一步」正是 Cancel 用例的核心判据，只能从 dtm 侧捞回来。
//
// 为什么用自增 id 而不用时间窗：时间窗会把「上一轮跑剩的事务」一起捞进来（用例
// 串行跑，两轮之间可能只隔几百毫秒），于是这条 Cancel 用例断言的其实是上一条事务
// 的状态 —— 而且它**照样会绿**，因为上一条也是 failed。
func latestTransGID(t *testing.T, db *sql.DB, transType string, afterID int64) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		gid := text(t, db, `SELECT gid FROM dtm_db.trans_global
			WHERE trans_type = ? AND id > ? ORDER BY id DESC LIMIT 1`, transType, afterID)
		if gid != "" {
			return gid
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("dtm_db 里 20s 内没有 id>%d 的 %s 类型新全局事务 —— 事务根本没提交出去", afterID, transType)
	return ""
}

// ---------- trace 取值 ----------

// gidOf 取一条 trace 上第一条非空 biz.dtm.gid。
func gidOf(tr traceutil.Trace) string {
	for _, s := range tr.Spans {
		if v := tr.StringTag(s, "biz.dtm.gid"); v != "" {
			return v
		}
	}
	return ""
}

// spanNames 返回该 trace 内全部 operationName，按字典序。
func spanNames(tr traceutil.Trace) []string {
	out := make([]string, 0, len(tr.Spans))
	for _, s := range tr.Spans {
		out = append(out, s.OperationName)
	}
	sort.Strings(out)
	return out
}

// hasSpanPrefix 判是否存在某前缀 span（rpc.dtm. / service. / db. 分层判据共用）。
func hasSpanPrefix(tr traceutil.Trace, prefix string) bool {
	for _, s := range tr.Spans {
		if strings.HasPrefix(s.OperationName, prefix) {
			return true
		}
	}
	return false
}

// branchSpans 取 trace 内全部 rpc.dtm.* 分支入口 span（按名字排序）。
func branchSpans(tr traceutil.Trace) []traceutil.Span {
	var out []traceutil.Span
	for _, s := range tr.Spans {
		if strings.HasPrefix(s.OperationName, "rpc.dtm.") {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].OperationName < out[j].OperationName
	})
	return out
}

// compensationSpans 取 trace 内带 biz.compensation=true 的 span。
func compensationSpans(tr traceutil.Trace) []traceutil.Span {
	var out []traceutil.Span
	for _, s := range tr.Spans {
		if tr.StringTag(s, "biz.compensation") == "true" {
			out = append(out, s)
		}
	}
	return out
}

// waitTraceByGID 在 window 内轮询，直到拿到「含指定 gid 的分支 span」的 trace。
//
// mustHave 非空时，还要求这些 operationName 全部出现才返回 —— 少了这个条件，
// 「见到第一个补偿 span 就往下判」会在 dtm 还在逆序取消时提前放行：dtm 的二阶段
// 是从后往前推的，order 的 Cancel 先落地、points 与 stock 的 Cancel 随后，实测
// 在只见到两段 Cancel 时就去查库，冻结量当然还没退干净。
//
// 为什么按 gid 找而不是取最新一条：同一时刻 examples/mall 共用四库的 outbox 表
// 也会往 Jaeger 写 trace，取到的可能根本不是这一笔。gid 是唯一能把「这次提交」
// 与「那批 trace」对齐的键。
func waitTraceByGID(t *testing.T, c *traceutil.Client, gid string, wantCompensation bool, mustHave []string, window time.Duration) traceutil.Trace {
	t.Helper()
	deadline := time.Now().Add(window)
	for {
		for _, tr := range fetchSince(t, c, domainService3, time.Now().Add(-2*time.Minute), 100) {
			if gidOf(tr) != gid {
				continue
			}
			spans := branchSpans(tr)
			if len(spans) == 0 {
				continue
			}
			if (len(compensationSpans(tr)) > 0) != wantCompensation {
				continue
			}
			if !hasAllSpans(tr, mustHave) {
				continue
			}
			return stableTrace(t, c, tr)
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Second)
	}
	kind := "正向段"
	if wantCompensation {
		kind = "补偿段"
	}
	t.Fatalf("%s 内没找到 gid=%s 的%s trace（要求含 span %v）—— dtm 没推进到那一步，或四域分支面没起",
		window, gid, kind, mustHave)
	return traceutil.Trace{}
}

func hasAllSpans(tr traceutil.Trace, names []string) bool {
	have := map[string]bool{}
	for _, s := range tr.Spans {
		have[s.OperationName] = true
	}
	for _, want := range names {
		if !have[want] {
			return false
		}
	}
	return true
}

// sagaForwardBranches Saga 四域正向分支入口 span 名。
var sagaForwardBranches = []string{
	"rpc.dtm.stock.Deduct", "rpc.dtm.order.CreateOrder",
	"rpc.dtm.product.AddSales", "rpc.dtm.points.Debit",
}

// sagaCompensateBranches Saga 四域补偿分支入口 span 名。
var sagaCompensateBranches = []string{
	"rpc.dtm.stock.Release", "rpc.dtm.order.CancelOrder",
	"rpc.dtm.product.ReduceSales", "rpc.dtm.points.Refund",
}

// tccCancelBranches TCC 三域 Cancel 分支入口 span 名。
var tccCancelBranches = []string{
	"rpc.dtm.stock.CancelFreeze", "rpc.dtm.points.CancelFreeze", "rpc.dtm.order.CancelFreeze",
}

// =============================================================================
// Task 13-①：Saga 正向 —— 一次 POST /api/order 在 Jaeger 上是**一条** trace
// =============================================================================

// TestDtmSagaPositiveIsOneTrace Saga 正向：bff 提交 span + 四域 rpc.dtm.* +
// 各域 service.* / repository.* 必须在**同一条 trace** 下，且根 span 恰 1。
//
// 这条是本文件最容易被「假通过」的断言：四个域服各自都能在 Jaeger 上点开、各自
// 都有 span，看起来完全正常。所以断的不是「各服务都能查到 trace」，而是
// **同一条 trace 内**四段都在，且四个分支入口都是 bff 提交 span 的子节点 ——
// 各生各的根时，四个分支会变成四棵并列的树，最后一条判据立刻不成立。
//
// 反向探针（改任一条本用例必须红）：
//
//	① 去掉 saga.go 里编进载荷的 traceparent（busi_branch 载荷的 traceparent 置空）
//	   → 分支面 ExtractParent 拿不到父，四域各自成根 → 「根 span 应恰有 1 个，实际 4 个」。
//	② 把四域之一（say srv-points）的 dtm.ExtractParent 调用删掉
//	   → 同上，且该域的 service.* span 挂到一棵无根的树上。
func TestDtmSagaPositiveIsOneTrace(t *testing.T) {
	c := requireJaeger(t)
	requireDtmChain(t)

	db := mysqlConn(t)
	defer func() { _ = db.Close() }()

	base := snapshotFourDB(t, db)
	cost := pointsCostOf(t, db)
	res := submitOrder(t, "")
	t.Logf("驱动：POST /api/order → gid=%s order_no=%s；基线 %s", res.DTMGID, res.OrderNo, base)

	if st := waitTransStatus(t, db, res.DTMGID, 40*time.Second); st != "succeed" {
		t.Fatalf("dtm 侧全局事务 %s 终态 = %q，want succeed（提交成功但四分支没走通）", res.DTMGID, st)
	}

	// 四库账变各扣一次。这不是「顺带看一眼」：单 trace 只证明入链，
	// 不证明这笔账真落了 —— 入链与落库是两条独立的失效路径。
	waitUntil(t, 20*time.Second, "Saga 四分支账变落库", func() bool {
		got := snapshotFourDB(t, db)
		return got.stockTotal == base.stockTotal-testQty &&
			got.stockAvailable == base.stockAvailable-testQty &&
			got.sold == base.sold+testQty &&
			got.ptsBalance == base.ptsBalance-cost &&
			got.ptsTotalSpent == base.ptsTotalSpent+cost
	})
	got := snapshotFourDB(t, db)
	t.Logf("正向账变：stock total %d→%d available %d→%d；sold %d→%d；points balance %d→%d spent %d→%d",
		base.stockTotal, got.stockTotal, base.stockAvailable, got.stockAvailable,
		base.sold, got.sold, base.ptsBalance, got.ptsBalance, base.ptsTotalSpent, got.ptsTotalSpent)

	tr := waitTraceByGID(t, c, res.DTMGID, false, sagaForwardBranches, 40*time.Second)
	t.Logf("取到 trace %s：span=%d 服务=%v", tr.TraceID, len(tr.Spans), tr.Services())

	// ① 单根。
	roots := tr.Roots()
	if len(roots) != 1 {
		t.Fatalf("根 span 应恰有 1 个，实际 %d 个：%v —— 四域分支没挂回同一条 trace"+
			"（根因通常是 traceparent 没编进载荷，或分支面没 Extract）", len(roots), spanNames(tr))
	}
	if got := tr.ServiceOf(roots[0]); got != gatewayService {
		t.Fatalf("根 span 所属服务 = %s，want %s（入口必须是网关）", got, gatewayService)
	}

	// ② 四域分支入口齐全（域 × 方法名逐条点名，不看前缀）。
	branches := branchSpans(tr)
	have := map[string]bool{}
	for _, s := range branches {
		have[s.OperationName] = true
	}
	for _, want := range sagaForwardBranches {
		if !have[want] {
			t.Errorf("正向链缺分支入口 span %s；实际分支清单：%v", want, opNames(tr, branches))
		}
	}
	if len(branches) != 4 {
		t.Errorf("正向链应恰有 4 个 rpc.dtm.* 分支入口，实际 %d 个：%v", len(branches), spanNames(tr))
	}

	// ③ 每个分支入口都带同一个非空 gid —— gid 是补偿段与正向段唯一的跨段关联键。
	for _, s := range branches {
		if g := tr.StringTag(s, "biz.dtm.gid"); g != res.DTMGID {
			t.Errorf("%s 的 biz.dtm.gid = %q，want %s", s.OperationName, g, res.DTMGID)
		}
	}

	// ④ 四个分支入口都是 bff 提交 span 的**子节点**（不是并列的根）。
	// 这条比「四域都在同一条 trace」更强：traceID 相同也可能是因为被谁硬塞进去的，
	// 而父子关系才说明它们真的挂在那次提交底下。
	submitSpan, ok := spanWithTagOnService(tr, "biz.dtm.gid", res.DTMGID, bffService)
	if !ok {
		t.Fatalf("trace %s 里找不到 %s 上带 biz.dtm.gid=%s 的 span（连 bff 提交 span 都没有）",
			tr.TraceID, bffService, res.DTMGID)
	}
	for _, s := range branches {
		found := false
		for _, a := range tr.AncestorsOf(s) {
			if a.SpanID == submitSpan.SpanID {
				found = true
			}
		}
		if !found {
			t.Errorf("%s 不是 bff 提交 span(%s) 的后代：它的祖先链里没有它 —— "+
				"分支是各自起的根，只是碰巧 traceID 相同", s.OperationName, submitSpan.SpanID)
		}
	}

	// ⑤ 六层分层 span 齐全：service / repository 两类都在，才叫「走六层链」。
	for _, p := range []string{"service.", "repository."} {
		if !hasSpanPrefix(tr, p) {
			t.Errorf("正向链缺 %s* 分层 span（分支没走六层链）；实际清单：%v", p, spanNames(tr))
		}
	}
	if !hasSpanPrefix(tr, "db.") {
		t.Errorf("正向链缺 db.* span；实际清单：%v", spanNames(tr))
	}
	t.Logf("正向链判定通过：1 根 / 4 分支入口 / gid 一致 / 分层 span 齐 / 终态 succeed（trace %s）", tr.TraceID)
}

// pointsCostOf 复算 bff 给本单算的积分口径（handler/mall.go 的 pointsReward：
// 成交额的 1%，分→积分，下限 1）。
//
// 为什么要复算而不是写死 9：单价与 1% 口径都是实现细节，写死会在口径调整时变成
// 一条假红，而假红比没有判据更坏 —— 它会让人开始习惯红。
func pointsCostOf(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	price := scalar(t, db, `SELECT price FROM product_db.products WHERE id = ?`, testProductID)
	cost := price * testQty / 100
	if cost < 1 {
		cost = 1
	}
	return cost
}

// =============================================================================
// Task 13-②：Saga 补偿 —— 与正向**同一条 traceID**，四库回基线
// =============================================================================

// TestDtmSagaCompensationJoinsForwardTrace fail_inject 点火逆序补偿后：
//
//	① 补偿 span 与正向 span 同 traceID（**不是新根**）；
//	② 每个补偿 span 带 biz.compensation=true 与与正向相同的 biz.dtm.gid；
//	③ 四库账回到基线；
//	④ dtm 侧终态是 failed。
//
// 口径来源（design.md D4，2026-10-13 真机实测修正）：dtm 按 step 共享载荷 ——
// Saga 的 GenBranches() 对每个 step 生成 compensate 与 action 两条分支，二者都取
// BinData: t.BinPayloads[i]。所以补偿分支拿到的**是同一份** traceparent，
// ExtractParent 挂回的是正向那条 trace。这比「补偿另起一根、靠 gid 关联」更好：
// 一笔 saga 的正向与回滚在一棵树里，Jaeger 上不必跨 trace 拼接。
//
// 本用例任何一条判据都不写「补偿是新根」—— 那是被实测推翻的旧口径。
//
// 反向探针：把 srv-stock/internal/dtm/busi_server.go 里 Release 分支的
// biz.compensation 属性去掉（或把 layer.Attrs 整行删掉）→ 「补偿 span 未带
// biz.compensation=true」报红；把 dtm 回调载荷的 traceparent 置空 → 补偿变成
// 新根 → 「补偿段与正向段不在同一条 traceID」报红。
func TestDtmSagaCompensationJoinsForwardTrace(t *testing.T) {
	c := requireJaeger(t)
	requireDtmChain(t)

	db := mysqlConn(t)
	defer func() { _ = db.Close() }()

	base := snapshotFourDB(t, db)
	res := submitOrder(t, "Debit") // Debit 是第四个分支：前三个已落库，补偿才真的有事可做
	t.Logf("驱动：POST /api/order(fail_inject=Debit) → gid=%s order_no=%s；基线 %s",
		res.DTMGID, res.OrderNo, base)

	if st := waitTransStatus(t, db, res.DTMGID, 60*time.Second); st != "failed" {
		t.Fatalf("dtm 侧全局事务 %s 终态 = %q，want failed（注入点名的分支返业务失败后 dtm 应转补偿并置失败）",
			res.DTMGID, st)
	}

	tr := waitTraceByGID(t, c, res.DTMGID, true, sagaCompensateBranches, 60*time.Second)
	fwd := branchSpans(tr)
	comp := compensationSpans(tr)
	t.Logf("trace %s：正向分支 %d 个、补偿段 span %d 个", tr.TraceID, len(fwd), len(comp))

	// ① 正向三段都真的落过库（否则「四库回基线」是因为什么都没做，不是补偿的功劳）。
	fwdNames := map[string]bool{}
	for _, s := range fwd {
		fwdNames[s.OperationName] = true
	}
	for _, want := range []string{"rpc.dtm.stock.Deduct", "rpc.dtm.order.CreateOrder", "rpc.dtm.product.AddSales"} {
		if !fwdNames[want] {
			t.Fatalf("正向段缺 %s —— 注入点在最后一步，前三步本应都已落库；"+
				"缺它则「四库回基线」是因为什么都没做，证明不了补偿", want)
		}
	}

	// ② 补偿段：四域的逆序分支各一个，都带 biz.compensation=true 与同一个 gid。
	if len(comp) < 3 {
		t.Fatalf("补偿段 span 只有 %d 个（%v），want ≥3 —— 前三步都落过库，逆序补偿至少该回三段",
			len(comp), opNames(tr, comp))
	}
	compNames := map[string]bool{}
	for _, s := range comp {
		name := s.OperationName
		compNames[name] = true
		if g := tr.StringTag(s, "biz.dtm.gid"); g != res.DTMGID {
			t.Errorf("补偿 span %s 的 biz.dtm.gid = %q，want %s（跨段关联键不一致就查不回这一笔）",
				name, g, res.DTMGID)
		}
	}
	for _, want := range []string{"rpc.dtm.stock.Release", "rpc.dtm.order.CancelOrder", "rpc.dtm.product.ReduceSales"} {
		if !compNames[want] {
			t.Errorf("缺补偿分支 %s；实际补偿清单：%v", want, opNames(tr, comp))
		}
	}

	// ③ **同一条 traceID**：补偿段与正向段在同一个 Trace 对象里，天然同 ID。
	//    显式再断一次「不是新根」：把 gid 反查到 Jaeger 上只应召回这一条。
	all := fetchSince(t, c, domainService3, time.Now().Add(-2*time.Minute), 200)
	byGID := map[string]int{}
	for _, other := range all {
		if gidOf(other) == res.DTMGID {
			byGID[other.TraceID]++
		}
	}
	if len(byGID) != 1 {
		t.Errorf("gid=%s 在 Jaeger 上召回了 %d 条 trace（%v），want 1 —— 补偿落到了新 trace 上；"+
			"真机实测 dtm 按 step 共享载荷，补偿分支拿的是同一份 traceparent，应当挂回正向那条",
			res.DTMGID, len(byGID), keysOf(byGID))
	}
	if got := len(tr.Roots()); got != 1 {
		t.Errorf("含补偿段的 trace 应恰有 1 个根，实际 %d 个 —— 补偿段成了新根", got)
	}
	// 补偿 span 的父必须在同 trace 内真实存在（spec：MUST NOT 伪造同 ID 孤儿 span）。
	if dangling := tr.DanglingRefs(); len(dangling) > 0 {
		t.Fatalf("trace %s 有 %d 条悬空父引用：%v —— 补偿段被塞成了孤儿 span",
			tr.TraceID, len(dangling), firstN(dangling, 5))
	}

	// ④ 四库回基线：账逐项对，不比「大概没变」。
	deadline := time.Now().Add(30 * time.Second)
	for {
		got := snapshotFourDB(t, db)
		if got.stockTotal == base.stockTotal && got.stockAvailable == base.stockAvailable &&
			got.sold == base.sold && got.ptsBalance == base.ptsBalance &&
			got.ptsTotalSpent == base.ptsTotalSpent {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("四库未回基线：\n  Want(补偿前) %s\n  Observed(补偿后) %s\n"+
				"  逆序补偿没走完或回补量不对（Deduct 是 total∓n/available∓n 的严格逆运算，"+
				"少回一个量就是账漂了）", base, got)
		}
		time.Sleep(2 * time.Second)
	}
	t.Logf("四库回基线通过：%s", base)
	t.Logf("补偿与正向同 traceID 通过：gid=%s 只对应 trace %s", res.DTMGID, tr.TraceID)
}

// =============================================================================
// Task 13-③：TCC —— outbox 行只在 Confirm 后出现
// =============================================================================

// TestDtmTccOutboxOnlyAfterConfirm 两半合成一条判据：
//
//	上半（**反向控制**，全绿之外的另一半）：fail_inject=TryPlace 让第三个 Try 失败
//	  → dtm Abort 并逆序 Cancel。此时 stock/points 的 Try **真落过库**（barrier 有
//	  try 行、stock_logs 有 freeze 流水），而 order_db 里**一行 outbox 都没有**。
//	  这才是「只在 Confirm 后出现」的真实证据：存在一个「Try 已完成、outbox 为空」
//	  的真机状态。若只跑全绿再断言 outbox 有 1 行，那条判据恒真 —— 它分不出
//	  「Confirm 写了 outbox」和「Try 就写了 outbox」。
//	下半：全绿提交 → 占位单 presale_locked → paid，order_status_log 出现该迁移，
//	  且 outbox 恰好一行。
//
// 反向探针：把 srv-order 的 DtmConfirmFreeze 里的 OutboxFill 去掉（即在 Confirm
// 才写的 outbox 挪到 TryPlace）→ 上半报红（「Try 已完成却出现了 outbox 行」）；
// 反过来把 DtmTryPlace 加上 OutboxFill → 同样报红。
func TestDtmTccOutboxOnlyAfterConfirm(t *testing.T) {
	requireDtmChain(t)
	requireTccRouteReachable(t)

	db := mysqlConn(t)
	defer func() { _ = db.Close() }()

	// ---------- 上半：Try 完成、Confirm 从未发生 ⇒ outbox 必须为零 ----------
	//
	// 锚点用「提交前后的 outbox 行数」而不是 order_no：Try 失败时 dtmgrpc 把 Abort
	// 上抛，bff 返回 5xx 且**不带 order_no**（响应体只有错误文案），拿不到单号就
	// 没法按单查 outbox —— 而「一个 outbox 新行都没多」本来就是更强的判据。
	outboxBefore := scalar(t, db, `SELECT COUNT(*) FROM order_db.outbox`)
	ordersBefore := scalar(t, db, `SELECT COUNT(*) FROM order_db.orders`)
	minID := maxTransID(t, db, "tcc")
	code, resp := submitPresale(t, "TryPlace")
	if code == 200 {
		t.Fatalf("Try 注入失败后 presale 不应返回 200，实际 200：%s", truncate(resp.Error, 200))
	}
	gid := latestTransGID(t, db, "tcc", minID)
	if st := waitTransStatus(t, db, gid, 60*time.Second); st != "failed" {
		t.Fatalf("注入失败后全局事务 %s 终态 = %q，want failed", gid, st)
	}
	// stock 的 Try 确实落过库（barrier 有 reason='try' 的 try 行）—— 否则
	//「outbox 为零」是因为 Try 没跑，判据就成了一句废话。
	barrierTry := scalar(t, db, `SELECT COUNT(*) FROM stock_db.barrier
		WHERE gid = ? AND op = 'try' AND reason = 'try'`, gid)
	if barrierTry == 0 {
		t.Fatalf("gid=%s 的 stock barrier 表里没有 try(reason='try') 行 —— Try 没落过库，"+
			"「outbox 为零」证明不了任何东西", gid)
	}
	outboxAfterTry := scalar(t, db, `SELECT COUNT(*) FROM order_db.outbox`)
	if outboxAfterTry != outboxBefore {
		t.Fatalf("Try 阶段就多出了 %d 行 outbox（%d→%d）—— 「outbox 只在 Confirm 后出现」被打破",
			outboxAfterTry-outboxBefore, outboxBefore, outboxAfterTry)
	}
	if got := scalar(t, db, `SELECT COUNT(*) FROM order_db.orders`); got != ordersBefore {
		t.Fatalf("Try 失败后 orders 表多出 %d 张单（%d→%d）—— 占位单没被 Cancel 撤掉",
			got-ordersBefore, ordersBefore, got)
	}
	t.Logf("反向控制：gid=%s Try 真落库（stock barrier try 行 %d 条）、dtm 置 failed，"+
		"而 outbox 行数 %d 未变、orders 行数 %d 未变 ✓", gid, barrierTry, outboxAfterTry, ordersBefore)

	// ---------- 下半：全绿 ⇒ Confirm ⇒ outbox 恰好一行 + presale_locked→paid ----------
	code2, ok := submitPresale(t, "")
	if code2 != 200 {
		t.Fatalf("POST /api/presale/lock 返回 HTTP %d：%s（链路异常，不是验收失败）", code2, truncate(ok.Error, 200))
	}
	if st := waitTransStatus(t, db, ok.DTMGID, 60*time.Second); st != "succeed" {
		t.Fatalf("全绿 TCC gid=%s 终态 = %q，want succeed", ok.DTMGID, st)
	}

	status := text(t, db, `SELECT status FROM order_db.orders WHERE order_no = ? AND dtm_gid = ?`,
		ok.OrderNo, ok.DTMGID)
	if status != "paid" {
		t.Fatalf("占位单终态 = %q，want paid（TCC Confirm 之后应转正式单）", status)
	}
	transition := scalar(t, db, `SELECT COUNT(*) FROM order_db.order_status_log
		WHERE order_id = (SELECT id FROM order_db.orders WHERE order_no = ? AND dtm_gid = ?)
		  AND from_status = 'presale_locked' AND to_status = 'paid'`, ok.OrderNo, ok.DTMGID)
	if transition != 1 {
		t.Fatalf("order_status_log 里 presale_locked → paid 出现 %d 次，want 恰 1 次", transition)
	}
	outbox := scalar(t, db, `SELECT COUNT(*) FROM order_db.outbox WHERE payload LIKE ?`, "%"+ok.OrderNo+"%")
	if outbox != 1 {
		t.Fatalf("Confirm 之后 outbox 行数 = %d，want 恰 1（gid=%s order_no=%s）", outbox, ok.DTMGID, ok.OrderNo)
	}
	// 时序：outbox 行不得早于「转正式单」那一步 —— 两者同事务提交，判据取
	// created_at 的先后。DATETIME 秒级精度下两者相等即满足「不早于」。
	orderPaidAt := text(t, db, `SELECT DATE_FORMAT(created_at, '%Y-%m-%d %H:%i:%s')
		FROM order_db.order_status_log
		WHERE order_id = (SELECT id FROM order_db.orders WHERE order_no = ? AND dtm_gid = ?)
		  AND to_status = 'paid'`, ok.OrderNo, ok.DTMGID)
	outboxAt := text(t, db, `SELECT DATE_FORMAT(created_at, '%Y-%m-%d %H:%i:%s')
		FROM order_db.outbox WHERE payload LIKE ? ORDER BY id LIMIT 1`, "%"+ok.OrderNo+"%")
	if outboxAt < orderPaidAt {
		t.Fatalf("outbox 行(%s) 早于「转正式单」(%s) —— outbox 被写在了 Confirm 之前", outboxAt, orderPaidAt)
	}
	t.Logf("全绿 TCC 通过：gid=%s order_no=%s 占位单 presale_locked→paid，outbox 恰 1 行（%s，not before %s）",
		ok.DTMGID, ok.OrderNo, outboxAt, orderPaidAt)
}

// =============================================================================
// Task 13-④：TCC Cancel —— 三域 frozen 全归零、占位单残留 0
// =============================================================================

// TestDtmTccCancelLeavesNoDirtyData TCC Try 中途失败 → dtm 逆序 Cancel：
// 三域冻结量全归零、订单域无 presale_locked 残留、outbox 无新增。
//
// 「frozen 全归零」是 D7b 三态不变式的验收面：Try 让 available→frozen，Cancel
// 把它原样退回。只判「available 回到基线」会放过「frozen 留了一截」—— 而 frozen
// 不参与可售判定，账面看着正常，实际已经锁死了一批货。
//
// 反向探针：把 srv-stock 的 dtmCancelFreezeSQL 去掉 frozen 列的更新
// （只加 available）→ 本用例报红（frozen 残留）。
func TestDtmTccCancelLeavesNoDirtyData(t *testing.T) {
	c := requireJaeger(t)
	requireDtmChain(t)
	requireTccRouteReachable(t)

	db := mysqlConn(t)
	defer func() { _ = db.Close() }()

	base := snapshotFourDB(t, db)
	minID := maxTransID(t, db, "tcc")
	code, resp := submitPresale(t, "TryPlace")
	if code == 200 {
		t.Fatalf("Try 注入失败后不应返回 200，实际 200：%s", truncate(resp.Error, 200))
	}
	gid := latestTransGID(t, db, "tcc", minID)
	if st := waitTransStatus(t, db, gid, 60*time.Second); st != "failed" {
		t.Fatalf("dtm 侧全局事务 %s 终态 = %q，want failed", gid, st)
	}

	// 三域 Cancel 分支 span 都在（biz.compensation=true，与 Try 同一 gid）。
	tr := waitTraceByGID(t, c, gid, true, tccCancelBranches, 60*time.Second)
	compNames := map[string]bool{}
	for _, s := range compensationSpans(tr) {
		compNames[s.OperationName] = true
		if g := tr.StringTag(s, "biz.dtm.gid"); g != gid {
			t.Errorf("Cancel span %s 的 gid = %q，want %s", s.OperationName, g, gid)
		}
	}
	for _, want := range []string{
		"rpc.dtm.stock.CancelFreeze", "rpc.dtm.points.CancelFreeze", "rpc.dtm.order.CancelFreeze",
	} {
		if !compNames[want] {
			t.Errorf("缺 Cancel 分支 span %s；实际：%v", want, opNames(tr, compensationSpans(tr)))
		}
	}
	t.Logf("Cancel 段 span 齐（同 trace %s）：%v", tr.TraceID, opNames(tr, compensationSpans(tr)))

	// 三域 frozen 归零 + 可售回基线 + 零占位单 + 零新事件。
	deadline := time.Now().Add(30 * time.Second)
	for {
		got := snapshotFourDB(t, db)
		locked := scalar(t, db, `SELECT COUNT(*) FROM order_db.orders WHERE status = 'presale_locked'`)
		if got.stockFrozen == base.stockFrozen && got.ptsFrozen == base.ptsFrozen &&
			got.stockAvailable == base.stockAvailable && got.ptsBalance == base.ptsBalance &&
			locked == 0 && got.outbox == base.outbox {
			t.Logf("Cancel 后零脏数据：stock frozen=%d available=%d，points frozen=%d balance=%d，"+
				"presale_locked 残留=%d，outbox %d→%d（基线 %s）",
				got.stockFrozen, got.stockAvailable, got.ptsFrozen, got.ptsBalance,
				locked, base.outbox, got.outbox, base)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("TCC Cancel 后仍有脏数据：\n  Want(提交前) %s\n  Observed(取消后) %s\n"+
				"  presale_locked 残留 %d 条，outbox 基线 %d → 实际 %d",
				base, got, locked, base.outbox, got.outbox)
		}
		time.Sleep(2 * time.Second)
	}
}

// =============================================================================
// Task 13-⑤：dtm 官方 barrier 的三条幂等守卫（真 MySQL + 真 dtm）
//
// 这三条被 amend 从 tasks.md 4.1(b)（单测）移到真机层，理由是它们依赖
// dtm 官方 barrier 在 **MySQL** 上的行锁与 insert-ignore 语义：sqlite 不支持
// FOR UPDATE，仓内也没有 sqlmock，写成「假 barrier 存储」的单测只会断言一个
// 自己写的假实现，测不到 dtm 的真实行为。
//
// 三条判据都取 **barrier 表 reason 列与 op 列不相等**这一指纹：官方
// BranchBarrier.Call 在补偿/取消时先用 originOp(action/try) 做一次 origin 插入，
// 那次插入的 reason 记的是**当前 op**（compensate/cancel）。于是：
//
//	reason != op 的行 = 「补偿的 origin 检查插进去的」= 正向从未落地 = 空补偿。
// =============================================================================

// TestDtmBarrierNullCompensationWritesNothing 空补偿：barrier 判定正向从未到达，
// 于是只记屏障、不调业务回调，业务库零写入。
//
// 真机构造：Saga 第四分支 points.Debit 被 fail_inject 点名 → 该步正向回滚，
// barrier 的 action 行随之回滚 → dtm 随后发起 points.Refund 时，barrier 的
// origin 检查插 action 行成功（originAffected>0）⇒ 判定空补偿 ⇒ 跳过 Refund 的
// 业务回调。落点：points_db.barrier 里有一行 op='action' 而 reason='compensate'，
// 且 point_ledgers 里该 order_no 零行、账户余额分毫未动。
//
// 反向探针：把 srv-points 的 repository/dtmstore.go 的 Refund 从 bar.Call 的
// 回调里挪到回调之外（bar.Call 之外直接改余额）→ 空补偿就真的写了账 →
// 本用例报红（point_ledgers 出现该 order_no 的行）。
func TestDtmBarrierNullCompensationWritesNothing(t *testing.T) {
	requireDtmChain(t)

	db := mysqlConn(t)
	defer func() { _ = db.Close() }()

	base := snapshotFourDB(t, db)
	res := submitOrder(t, "Debit")
	if st := waitTransStatus(t, db, res.DTMGID, 60*time.Second); st != "failed" {
		t.Fatalf("注入失败后 gid=%s 终态 = %q，want failed", res.DTMGID, st)
	}
	// 等补偿段落库。
	waitUntil(t, 30*time.Second, "points barrier 里出现 compensate 行", func() bool {
		return scalar(t, db, `SELECT COUNT(*) FROM points_db.barrier
			WHERE gid = ? AND op = 'compensate'`, res.DTMGID) > 0
	})

	// ① 空补偿指纹：action 行的 reason 是 compensate ⇒ 这行是补偿的 origin 检查插的。
	nullRows := scalar(t, db, `SELECT COUNT(*) FROM points_db.barrier
		WHERE gid = ? AND op = 'action' AND reason = 'compensate'`, res.DTMGID)
	if nullRows == 0 {
		t.Fatalf("gid=%s 在 points_db.barrier 里没有 reason='compensate' 的 action 行 —— "+
			"要么正向落过库（那就不是空补偿），要么补偿根本没来。本用例要判的是前者为假时"+
			"业务库必须零写入；这条判据缺失说明 barrier 行为与预期不符", res.DTMGID)
	}

	// ② 业务库零写入：积分流水无该单任何一行（debit 失败回滚 + refund 被 barrier 跳过）。
	ledgers := scalar(t, db, `SELECT COUNT(*) FROM points_db.point_ledgers WHERE order_no = ?`, res.OrderNo)
	if ledgers != 0 {
		t.Fatalf("空补偿却写出了 %d 行积分流水（order_no=%s）—— barrier 的空补偿判定失效了",
			ledgers, res.OrderNo)
	}

	// ③ 账户余额分毫未动（被点名的正是这一域的分支）。
	got := snapshotFourDB(t, db)
	if got.ptsBalance != base.ptsBalance || got.ptsTotalSpent != base.ptsTotalSpent ||
		got.ptsFrozen != base.ptsFrozen {
		t.Fatalf("空补偿却改了积分账户：balance %d→%d、total_spent %d→%d、frozen %d→%d",
			base.ptsBalance, got.ptsBalance, base.ptsTotalSpent, got.ptsTotalSpent,
			base.ptsFrozen, got.ptsFrozen)
	}
	t.Logf("空补偿判定通过：gid=%s points barrier 空补偿指纹 %d 行、积分流水 0 行、账户 %d/%d/%d 未动",
		res.DTMGID, nullRows, got.ptsBalance, got.ptsFrozen, got.ptsTotalSpent)
}

// TestDtmBarrierHangingTryIsUnwoundByCancel 悬挂：Try 已在部分分支落库、进程
// 没走到 Confirm，dtm 逆序 Cancel 把它们全撤回去，零脏数据。
//
// 真机构造：TCC 的第三个 Try（order.TryPlace）被 fail_inject 点名。此时前两个
// Try（stock/points 的 TryFreeze）**已经真冻结**，dtm Abort 后逆序 Cancel：
//   - stock / points：Cancel 业务真跑了（barrier 有 try 行 + cancel 行，frozen 回零）；
//   - order：TryPlace 从未落地 ⇒ 它的 Cancel 是**空补偿**（barrier 有 reason='cancel'
//     的 try 行，且 orders 表里一张占位单都没有）。
//
// 两者合起来就是悬挂守卫的完整面：已冻结的全撤、未冻结的不写。
//
// 反向探针：把 srv-stock 的 dtmCancelFreezeSQL 的 frozen 减法去掉 → 本用例报红。
func TestDtmBarrierHangingTryIsUnwoundByCancel(t *testing.T) {
	requireDtmChain(t)
	requireTccRouteReachable(t)

	db := mysqlConn(t)
	defer func() { _ = db.Close() }()

	base := snapshotFourDB(t, db)
	minID := maxTransID(t, db, "tcc")
	code, resp := submitPresale(t, "TryPlace")
	if code == 200 {
		t.Fatalf("Try 注入失败后不应返回 200，实际 200：%s", truncate(resp.Error, 200))
	}
	gid := latestTransGID(t, db, "tcc", minID)
	if st := waitTransStatus(t, db, gid, 60*time.Second); st != "failed" {
		t.Fatalf("dtm 侧全局事务 %s 终态 = %q，want failed", gid, st)
	}

	// ① 两个已冻结的域：try 行与 cancel 行都在（真冻结 + 真回退）。
	for _, tc := range []struct{ db, branch string }{
		{"stock_db", "01"}, {"points_db", "02"},
	} {
		tryRows := scalar(t, db, fmt.Sprintf(
			`SELECT COUNT(*) FROM %s.barrier WHERE gid = ? AND branch_id = ? AND op = 'try' AND reason = 'try'`, tc.db),
			gid, tc.branch)
		cancelRows := scalar(t, db, fmt.Sprintf(
			`SELECT COUNT(*) FROM %s.barrier WHERE gid = ? AND branch_id = ? AND op = 'cancel'`, tc.db),
			gid, tc.branch)
		if tryRows != 1 || cancelRows != 1 {
			t.Errorf("%s 分支 %s 的 barrier：try(reason=try) %d 行、cancel %d 行，want 各 1 —— "+
				"悬挂回退要么没发起、要么被判成空补偿（真冻结过的分支不该是空补偿）",
				tc.db, tc.branch, tryRows, cancelRows)
		}
	}

	// ② 从未落地的 order 分支：Cancel 是空补偿，且 orders 表零占位单。
	nullTry := scalar(t, db, `SELECT COUNT(*) FROM order_db.barrier
		WHERE gid = ? AND branch_id = '03' AND op = 'try' AND reason = 'cancel'`, gid)
	if nullTry != 1 {
		t.Errorf("order 分支没有 reason='cancel' 的 try 行（%d 行）—— 该分支的 Cancel "+
			"不是空补偿，order 的 Try 竟落过库", nullTry)
	}
	placeholders := scalar(t, db, `SELECT COUNT(*) FROM order_db.orders WHERE dtm_gid = ?`, gid)
	if placeholders != 0 {
		t.Fatalf("悬挂回退后仍残留 %d 张占位单（gid=%s）", placeholders, gid)
	}

	// ③ 三域冻结量归零 + 可售回基线。
	waitUntil(t, 30*time.Second, "三域 frozen 归零", func() bool {
		got := snapshotFourDB(t, db)
		return got.stockFrozen == base.stockFrozen && got.ptsFrozen == base.ptsFrozen &&
			got.stockAvailable == base.stockAvailable && got.ptsBalance == base.ptsBalance
	})
	got := snapshotFourDB(t, db)
	t.Logf("悬挂回退通过：gid=%s 已冻结两域真回退（try+cancel 各一行）、未落地一域空补偿（占位单 0）、"+
		"三域 frozen=%d/%d 回基线", gid, got.stockFrozen, got.ptsFrozen)
}

// TestDtmBarrierReplayIsIdempotent 重放：dtm 因重试对同一分支发起第二次回调，
// barrier 判重，业务库零重复写入。
//
// 真机构造（不采用「杀掉 dtm 再等它重投」那种侵入手法）：直接向 srv-stock 的分支面
// 重发 dtm server 会发的那个回调 —— 同一份 gRPC metadata（gid / branch_id / op /
// trans_type）+ 同一份 biz 载荷。官方 BranchBarrier 每次回调的 barrier_id 都从
// "01" 起，所以「重试同一分支」在协议上就是**逐字相同**的一次回调；第二发必须被
// insert-ignore 的唯一键 (gid, branch_id, op, barrier_id) 判成重复：
// 返回成功，而 available/total 不再动、流水不增行、屏障行数不变。
//
// 判据里带一条**反向控制**（同一条用例内）：换一个从未提交过的 gid 发同一份载荷
// —— 那是「另一个屏障」，barrier 必须放行业务，于是库存真扣。控制的存在是为了
// 排除「分支面来者不拒、一律返回成功」这种让前半恒真的假通过。控制用真实业务
// 扣减，故收尾再发一次 Release 原样还回去，不给机器留脏数据。
//
// 反向探针：把 srv-stock 的 repository/dtmstore.go 里 DtmDeduct 的
// dtmx.Barrier 包裹去掉（用自有连接直接落库）→ 本用例报红（第二发又扣了一次）。
func TestDtmBarrierReplayIsIdempotent(t *testing.T) {
	requireDtmChain(t)

	db := mysqlConn(t)
	defer func() { _ = db.Close() }()

	// 前置：先跑一笔真 Saga，拿到一个 dtm 确实提交过的 gid。barrier 判重的
	// 前提是该 (gid, branch_id, op, barrier_id) 已经存在过 —— 拿一个从未提交过的
	// gid 去"重放"，那不叫重放，那叫首次执行（正是下面反向控制那一半）。
	res := submitOrder(t, "")
	if st := waitTransStatus(t, db, res.DTMGID, 40*time.Second); st != "succeed" {
		t.Fatalf("前置：正向 Saga gid=%s 终态 = %q，want succeed", res.DTMGID, st)
	}
	availBefore := scalar(t, db, `SELECT available FROM stock_db.stocks WHERE product_id = ?`, testProductID)
	logsBefore := scalar(t, db, `SELECT COUNT(*) FROM stock_db.stock_logs WHERE reason = ?`,
		"dtm saga deduct "+res.OrderNo)
	barBefore := scalar(t, db, `SELECT COUNT(*) FROM stock_db.barrier
		WHERE gid = ? AND branch_id = '01' AND op = 'action'`, res.DTMGID)
	if barBefore != 1 {
		t.Fatalf("前置：gid=%s 的 stock action 屏障行 %d 条，want 1 —— 正向分支没有落屏障，"+
			"「重放」无从谈起", res.DTMGID, barBefore)
	}

	biz := fmt.Sprintf(`{"order_no":%q,"product_id":%d,"quantity":%d,"user_id":%d}`,
		res.OrderNo, testProductID, testQty, testUserID)

	// ---------- 前半：真重放（同 gid 同 branch 同 op 再发一次）----------
	code, msg := callStockBranch(t, "Deduct", res.DTMGID, "01", "action", "saga", biz)
	if code != "OK" {
		t.Fatalf("重放的 Deduct 返回 gRPC status=%s（%s）—— barrier 判重时应当直接判成功", code, msg)
	}
	availAfter := scalar(t, db, `SELECT available FROM stock_db.stocks WHERE product_id = ?`, testProductID)
	logsAfter := scalar(t, db, `SELECT COUNT(*) FROM stock_db.stock_logs WHERE reason = ?`,
		"dtm saga deduct "+res.OrderNo)
	barAfter := scalar(t, db, `SELECT COUNT(*) FROM stock_db.barrier
		WHERE gid = ? AND branch_id = '01' AND op = 'action'`, res.DTMGID)
	if availAfter != availBefore {
		t.Fatalf("重放又扣了一次库存：available %d → %d（重复回调重复扣减）", availBefore, availAfter)
	}
	if logsAfter != logsBefore {
		t.Fatalf("重放又记了一笔流水：%d → %d 行", logsBefore, logsAfter)
	}
	if barAfter != barBefore {
		t.Fatalf("重放多插了屏障行：%d → %d", barBefore, barAfter)
	}
	t.Logf("前半（真重放，gid=%s）：判重通过 —— 可用库存 %d 不变、流水 %d 行不变、屏障 %d 行不变",
		res.DTMGID, availBefore, logsAfter, barAfter)

	// ---------- 反向控制：换一个从未提交过的 gid，barrier 必须放行 ----------
	probeGID := res.DTMGID + "PROBE"
	probeBiz := fmt.Sprintf(`{"order_no":%q,"product_id":%d,"quantity":%d,"user_id":%d}`,
		"e2e-replay-probe", testProductID, testQty, testUserID)
	if code, msg := callStockBranch(t, "Deduct", probeGID, "01", "action", "saga", probeBiz); code != "OK" {
		t.Fatalf("反向控制：新 gid=%s 的 Deduct 返回 status=%s（%s），应当放行", probeGID, code, msg)
	}
	waitUntil(t, 20*time.Second, "反向控制：新 gid 的回调真扣了库存", func() bool {
		return scalar(t, db, `SELECT available FROM stock_db.stocks WHERE product_id = ?`,
			testProductID) == availBefore-testQty
	})
	t.Logf("反向控制通过：新 gid=%s 的同款回调真扣了库存（%d→%d）—— 说明前半的判重确实"+
		"来自 barrier 的唯一键，而不是分支面来者不拒", probeGID, availBefore, availBefore-testQty)

	// ---------- 收尾：把反向控制那一笔原样还回去，别给机器留脏数据 ----------
	if code, msg := callStockBranch(t, "Release", probeGID, "01", "compensate", "saga", probeBiz); code != "OK" {
		t.Fatalf("收尾：Release 返回 status=%s（%s）", code, msg)
	}
	waitUntil(t, 20*time.Second, "收尾：探针扣减已回补", func() bool {
		return scalar(t, db, `SELECT available FROM stock_db.stocks WHERE product_id = ?`,
			testProductID) == availBefore
	})
	t.Logf("探针已回补：可用库存回到 %d", availBefore)
}

// callStockBranch 直接向 srv-stock 的 dtm 分支面发一次回调，返回 gRPC status 的
// 码名（成功为 "OK"）与错误文案。
//
// 为什么返回 status 而不是响应体：D7a —— dtm 回调用 raw codec，dtm server 从不
// 解析分支响应体，只看 status。这里若按响应体的 code/message 判成败，就把一条
// 「body 写着 code=0 而 status 是 Aborted」的分支读成成功。
//
// 为什么手搓 pb 而不 import 各域的 protos 包：Go 的 internal 规则禁止
// examples/micro import srv-stock/internal/dtm/protos（dtm_failsem_test.go 的文件头
// 记着同一个约束）。手搓一份**运行时构造**的描述符是本包唯一能打真分支面的办法，
// 而它与真实 IDL 的一致性由 assertBranchProtoMatchesIDL 守着 —— 字段名或字段号
// 一改，那条守卫先红，绝不会静默发一个空载荷过去。
func callStockBranch(t *testing.T, method, gid, branchID, op, transType, biz string) (code, msg string) {
	t.Helper()
	fd := stockBranchFileDescriptor(t)
	md := fd.Messages().ByName("StockBusiReq")
	req := dynamicpb.NewMessage(md)
	req.Set(md.Fields().ByName("biz"), protoreflect.ValueOfString(biz))
	req.Set(md.Fields().ByName("traceparent"), protoreflect.ValueOfString(""))
	req.Set(md.Fields().ByName("fail_inject"), protoreflect.ValueOfString(""))

	addr := fmt.Sprintf("127.0.0.1:%d", branchFacePorts["stock"])
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// 官方 dtmgimp.TransBaseFromGrpc 从 "dtm-" 前缀的 metadata 里取这四项；
	// 少一项 BarrierFrom 就报错、屏障随之失效 —— 而那正是本用例要验的东西的前提。
	//
	// 注意 barrier_id **不在** metadata 里：官方 BranchBarrier 每次回调由进程内
	// 计数器从 0 自增到 "01"，dtm 重试同一分支时会重新拿到 01。所以「同一个
	// 屏障的重放」天然就是「同 gid + 同 branch_id + 同 op + 同 trans_type 再发一次」，
	// 而「另一个屏障」只能用**另一个 gid** 来构造。
	ctx = metadata.AppendToOutgoingContext(ctx,
		"dtm-trans_type", transType,
		"dtm-gid", gid,
		"dtm-branch_id", branchID,
		"dtm-op", op,
	)
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("连分支面 %s 失败：%v", addr, err)
	}
	defer func() { _ = conn.Close() }()

	respDesc := fd.Messages().ByName("StockBusiResp")
	resp := dynamicpb.NewMessage(respDesc)
	if err := conn.Invoke(ctx, "/dtmbranch.stock.StockBusi/"+method, req, resp); err != nil {
		return status.Code(err).String(), err.Error()
	}
	return status.Code(nil).String(), resp.Get(respDesc.Fields().ByName("message")).String()
}

// protoFieldRe 抽 message 段里的字段名与字段号。
//
// (?m) 不是可省的：Go 的 regexp 默认 `^` 只匹配**整段文本的开头**（不逐行），
// 少了它一个字段只会在它恰好是块内第一行时被抽到 —— 实测 StockBusiReq 一条都没
// 抽到、StockBusiResp 只抽到 code，于是这条一致性守卫会对着一份几乎空的字段表
// 说「字段少了一个」，报错信息指向错误的方向。
var protoFieldRe = regexp.MustCompile(`(?m)^\s*(?:repeated\s+)?[A-Za-z0-9_.]+\s+([A-Za-z0-9_]+)\s*=\s*(\d+)\s*;`)

// assertBranchProtoMatchesIDL 手搓描述符与真 IDL 的一致性守卫。
//
// 没有它，这份手搓的 busi.proto 就是一个会随 IDL 漂移的影子：IDL 改了字段名或
// 字段号，探针照发不误，只是发出去的东西已经不是业务真正收的东西了 —— 而判据
// 仍然全绿。这条守卫把「影子」钉死在 IDL 上。
func assertBranchProtoMatchesIDL(t *testing.T, fd protoreflect.FileDescriptor) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("srv-stock", "internal", "dtm", "protos", "busi.proto"))
	if err != nil {
		t.Fatalf("读 srv-stock/internal/dtm/protos/busi.proto 失败：%v", err)
	}
	// 取 message 段：message 之后、下一个顶层 message 之前。
	blocks := map[string]string{}
	var cur string
	var buf strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "message ") {
			if cur != "" {
				blocks[cur] = buf.String()
			}
			cur = strings.Fields(trimmed)[1]
			buf.Reset()
			continue
		}
		if strings.HasPrefix(trimmed, "service ") {
			break
		}
		if cur != "" {
			buf.WriteString(line + "\n")
		}
	}
	if cur != "" {
		blocks[cur] = buf.String()
	}

	for _, name := range []string{"StockBusiReq", "StockBusiResp"} {
		md := fd.Messages().ByName(protoreflect.Name(name))
		if md == nil {
			t.Fatalf("手搓描述符里没有 message %s", name)
		}
		want := map[string]int32{}
		for _, m := range protoFieldRe.FindAllStringSubmatch(blocks[name], -1) {
			n, _ := strconv.Atoi(m[2])
			want[m[1]] = int32(n)
		}
		if md.Fields().Len() != len(want) {
			t.Errorf("%s 字段数：手搓 %d、IDL %d（%v）", name, md.Fields().Len(), len(want), want)
		}
		for f := 0; f < md.Fields().Len(); f++ {
			field := md.Fields().Get(f)
			num, ok := want[string(field.Name())]
			if !ok {
				t.Errorf("%s.%s 只在手搓描述符里，IDL 没有", name, field.Name())
				continue
			}
			if int32(field.Number()) != num {
				t.Errorf("%s.%s 字段号：手搓 %d、IDL %d", name, field.Name(), field.Number(), num)
			}
		}
	}
	// 分支方法名是 URL 的一段，改了 IDL 就得改这里，也得改 bff 的编排常量。
	if fd.Services().Len() != 1 {
		t.Fatalf("手搓描述符里有 %d 个 service，want 1", fd.Services().Len())
	}
	for _, want := range []string{"Deduct", "Release", "TryFreeze", "ConfirmFreeze", "CancelFreeze"} {
		if fd.Services().Get(0).Methods().ByName(protoreflect.Name(want)) == nil {
			t.Errorf("手搓描述符的 StockBusi 缺方法 %s", want)
		}
	}
}

func stockBranchFileDescriptor(t *testing.T) protoreflect.FileDescriptor {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("busi.proto"),
		Package: proto.String("dtmbranch.stock"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("StockBusiReq"), Field: []*descriptorpb.FieldDescriptorProto{
				strFieldOf("biz", 1),
				strFieldOf("traceparent", 2),
				strFieldOf("fail_inject", 3),
			}},
			{Name: proto.String("StockBusiResp"), Field: []*descriptorpb.FieldDescriptorProto{
				int64FieldOf("code", 1),
				strFieldOf("message", 2),
			}},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: proto.String("StockBusi"),
			Method: []*descriptorpb.MethodDescriptorProto{
				methodOf("Deduct"), methodOf("Release"), methodOf("TryFreeze"),
				methodOf("ConfirmFreeze"), methodOf("CancelFreeze"),
			},
		}},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("构造分支 pb 描述符失败：%v", err)
	}
	assertBranchProtoMatchesIDL(t, fd)
	return fd
}

func strFieldOf(name string, num int32) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name: proto.String(name), Number: proto.Int32(num),
		Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:  descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
	}
}

func int64FieldOf(name string, num int32) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name: proto.String(name), Number: proto.Int32(num),
		Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:  descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
	}
}

func methodOf(name string) *descriptorpb.MethodDescriptorProto {
	return &descriptorpb.MethodDescriptorProto{
		Name:       proto.String(name),
		InputType:  proto.String(".dtmbranch.stock.StockBusiReq"),
		OutputType: proto.String(".dtmbranch.stock.StockBusiResp"),
	}
}

// ---------- 助手 ----------

// spanWithTagOnService 取「某服务上带指定 tag 值的第一个 span」。
//
// 为什么不按 span 返回顺序取第一个带 gid 的：分支入口 span 也带 gid，而 Jaeger
// 返回的 span 顺序不保证谁先谁后 —— 实测第一个命中的是 demo-points 的分支入口，
// 于是「bff 提交 span」取错了人，父子判据整条失真。
func spanWithTagOnService(tr traceutil.Trace, key, want, service string) (traceutil.Span, bool) {
	for _, s := range tr.Spans {
		if tr.ServiceOf(s) == service && tr.StringTag(s, key) == want {
			return s, true
		}
	}
	return traceutil.Span{}, false
}

// opNamesOf 该 trace 内满足 pred 的 span 名单（服务:操作名）。
func opNamesOf(tr traceutil.Trace, spans []traceutil.Span) []string {
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		out = append(out, tr.ServiceOf(s)+":"+s.OperationName)
	}
	return out
}

func keysOf(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func envOrDefault(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func waitUntil(t *testing.T, window time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(window)
	for {
		if ok() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s：%s 内不成立", what, window)
		}
		time.Sleep(time.Second)
	}
}
