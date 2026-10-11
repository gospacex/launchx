package app

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/route"

	"github.com/gospacex/launchx/examples/monolith/internal/handler"
)

// noopAuth 鉴权中间件的替身：只放行。
//
// 用替身是为了把「auth 已装配」这件事与它的实现（缓存里的令牌校验）解耦 ——
// 本文件要判的是路由表的裁决口径，不是令牌校验本身。
func noopAuth(_ context.Context, c *app.RequestContext) { c.Next(context.Background()) }

// TestLoopbackOnly 逐地址核对「是否只对本机可见」的判定。
//
// 这个函数此前只存在于 repository.List / service.List 的注释里，代码里零命中 ——
// 注释承诺了一个不存在的闸门。判据先钉住判定表本身，再钉住它的调用点。
//
// 反向探针：把 loopbackOnly 改成恒 true，TestRegisterRoutes* 两条立刻红。
func TestLoopbackOnly(t *testing.T) {
	cases := []struct {
		addr string
		want bool
		why  string
	}{
		{"127.0.0.1:8080", true, "IPv4 回环"},
		{"127.0.0.2:8080", true, "127/8 整段都是回环，不能只认 .1"},
		{"[::1]:8080", true, "IPv6 回环"},
		{"localhost:8080", true, "名字形式只认 localhost"},
		{"0.0.0.0:8080", false, "通配 = 监听所有网卡，最典型的暴露姿势"},
		{"192.168.1.5:8080", false, "局域网地址"},
		{":8080", false, "空 host：收紧，不猜一个宽松答案"},
		{"", false, "空串"},
		{"127.0.0.1", false, "缺端口：解析不出即按非回环处理"},
		{"db.internal:8080", false, "非 localhost 的名字形式（要 DNS 才能定性，猜不得）"},
	}
	for _, tc := range cases {
		if got := loopbackOnly(tc.addr); got != tc.want {
			t.Errorf("loopbackOnly(%q) = %v, want %v（%s）", tc.addr, got, tc.want, tc.why)
		}
	}
}

// TestRegisterRoutesRejectsNonLoopback 对外监听 + 路由表里还有免鉴权路径 ⇒ 必须报错。
//
// 背景：路由表里 8 条免鉴权路径含 POST/PUT/DELETE /orders。绑到网卡地址时放它们
// 过去，等于把下单 / 改单 / 删单裸奔出去，而这个示例的注释一直声称「已被
// RegisterRoutes 限制为只能绑回环地址」—— 那条限制此前根本不存在。
//
// 反向探针：把 RegisterRoutes 开头那块裁决删掉，本用例立刻红。
func TestRegisterRoutesRejectsNonLoopback(t *testing.T) {
	e := route.NewEngine(config.NewOptions(nil))
	err := RegisterRoutes(e, handler.New(nil, false), noopAuth, "0.0.0.0:8080")
	if err == nil {
		t.Fatal("对外监听 + 免鉴权路径（含 POST/PUT/DELETE /orders）必须报错并中止装配；" +
			"放过等于任何人都能下单 / 改单 / 删单")
	}
	// 错误信息要能自解释：下一次撞上的人不该被迫去读源码才知道怎么改。
	for _, kw := range []string{"127.0.0.1", "Secure"} {
		if !strings.Contains(err.Error(), kw) {
			t.Errorf("错误信息缺少修法提示 %q，实际：%v", kw, err)
		}
	}
}

// TestRegisterRoutesRejectsNonLoopbackWithoutAuth 对外监听但鉴权没装配 ⇒ 必须报错。
//
// 与上一条是两种独立缺口：那一条是「路由表里就有免鉴权路径」，这一条是「连
// /secure 都注册不上（缓存降级 / 令牌为空）」。后者更隐蔽 —— 看起来配了鉴权，
// 实际 auth 为 nil 时注册循环会整条跳过 /secure，服务起来得干干净净、一条
// 受保护的路径都没有。
//
// 回环下这条不生效：那是本示例既定的降级姿态（旁路边车挂了不该带走主流程）。
func TestRegisterRoutesRejectsNonLoopbackWithoutAuth(t *testing.T) {
	e := route.NewEngine(config.NewOptions(nil))
	if err := RegisterRoutes(e, handler.New(nil, false), nil, "0.0.0.0:8080"); err == nil {
		t.Fatal("对外监听 + 鉴权未装配（缓存降级 / demo.auth_token 为空）必须报错")
	}
}

// TestRegisterRoutesAllowsLoopback 回环监听是 demo 的既定形态，不该被上面的裁决挡住。
//
// 这条是反向语义：约束收紧的目的不是"起不来"，而是"只有在本机时才放行者几条
// 验收入口"。没有这条，把裁决写成一刀切拒绝也会绿。
func TestRegisterRoutesAllowsLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"} {
		e := route.NewEngine(config.NewOptions(nil))
		if err := RegisterRoutes(e, handler.New(nil, false), noopAuth, addr); err != nil {
			t.Errorf("RegisterRoutes(%q) 在回环下不该报错（E1 / T13 的验收入口靠它）: %v", addr, err)
		}
	}
}

// TestSecureRoutesAreNotDoublePrefixed 鉴权路径必须真的挂在 /secure/orders 上。
//
// 背景：`e.Group("/secure", auth)` 自带前缀，而表里那条 Entry 的 Pattern 也写
// 了 /secure/orders；照抄注册出来的实际路径是 **/secure/secure/orders**（hertz
// 启动日志里能看到），也就是：这条受保护的路径谁也访问不到，/secure/orders
// 一直是 404。鉴权在本示例里等于挂在了空气上 —— 而 T13 的判据只验「/secure
// 分组存在 / 无凭据 401」，从不真的请求它，所以一直绿。
//
// 修法只能是注册时摘前缀（不能改表里的 Pattern）：route 层按 c.FullPath() 查表，
// FullPath 是注册后的真实模板，改表会让每笔鉴权请求都记成 route.order.Unregistered。
//
// 反向探针：去掉 routes.go 里的 strings.TrimPrefix，本用例立刻红。
func TestSecureRoutesAreNotDoublePrefixed(t *testing.T) {
	e := route.NewEngine(config.NewOptions(nil))
	if err := RegisterRoutes(e, handler.New(nil, false), noopAuth, "127.0.0.1:8080"); err != nil {
		t.Fatalf("回环下注册失败: %v", err)
	}
	var paths []string
	for _, r := range e.Routes() {
		paths = append(paths, r.Method+" "+r.Path)
	}
	want := "GET /secure/orders"
	found := false
	for _, p := range paths {
		if p == want {
			found = true
		}
		if strings.Contains(p, "/secure/secure/") {
			t.Fatalf("注册出重复前缀的路由 %q —— 鉴权路径挂在了访问不到的地址上，/secure/orders 会一直 404", p)
		}
	}
	if !found {
		t.Fatalf("未注册 %s，实际：%v", want, paths)
	}
}

// TestSetupReadsListenAddrFromConfig 监听地址必须来自配置，不得写字面量。
//
// 动态用例钉住的是 RegisterRoutes 的裁决逻辑，钉不住「调用方传了什么」——
// 传一个写死的 "127.0.0.1:8080" 同样能让所有动态用例全绿，而约束等于被架空
// （真正监听在 0.0.0.0 上，判据却看的是字面量）。这类"接线本身"只能源码级断言，
// 与 TestSetupMountsRouteLayer 同形。
//
// 反向探针：把 setup.go 里的 cfg.GetString("webx.addr") 换成常量，本用例立刻红。
func TestSetupReadsListenAddrFromConfig(t *testing.T) {
	src, err := os.ReadFile("setup.go")
	if err != nil {
		t.Fatalf("读 setup.go: %v", err)
	}
	if !strings.Contains(string(src), `cfg.GetString("webx.addr")`) {
		t.Fatal(`setup.go 必须从配置读监听地址（cfg.GetString("webx.addr")）—— ` +
			"写死回环字面量会让「对外监听不得有免鉴权路径」这条裁决形同虚设")
	}
}
