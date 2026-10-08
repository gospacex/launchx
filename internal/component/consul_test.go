package component

import (
	"context"
	"strings"
	"testing"

	"github.com/gospacex/coordx"
)

// fakeRegister 只用注册能力的最小句柄（测试桩）。
type fakeRegister struct{}

func (f *fakeRegister) Register(context.Context, string, *coordx.ServiceInstance) error   { return nil }
func (f *fakeRegister) Deregister(context.Context, string, *coordx.ServiceInstance) error { return nil }
func (f *fakeRegister) HealthCheck() error                                                { return nil }
func (f *fakeRegister) Close() error                                                      { return nil }

// regOnlyYAML 的 driver 由用例在运行期替换为「只注册、不发现」的驱动名。
const regOnlyYAML = `
service: {name: srv-order, mode: srv}
xmiddleware:
  coordx:
    driver: __DRIVER__
    addr: "127.0.0.1:8500"
`

// registerOnlyDriver 注册一个只有注册能力、**没有发现能力**的驱动。
//
// coordx 的 registerCreators / discoverCreators 是两张独立的表，因此这个组合
// 是能真实构造出来的（不是硬凑）：注册成功、发现构件器建不出来。
func registerOnlyDriver(t *testing.T) string {
	t.Helper()
	driver := "reg-only-" + strings.ReplaceAll(t.Name(), "/", "-")
	coordx.RegisterCreator(driver, func(*coordx.Config) (coordx.Register, error) {
		return &fakeRegister{}, nil
	})
	// 刻意不调 RegisterDiscoverCreator。
	return driver
}

// TestRegistryDiscoverFailureIsNotSilent 断言发现侧失败与注册侧**同口径**。
//
// 历史行为（2026-10-08 前）：发现构件器建不出来时只打一条 Warn 后 return nil，
// **绕过 strict.registry** —— 于是「配了注册中心但发现侧建不出来」在生产里
// 只表现为多一行日志，全部出向调用退化成静态地址、没有兜底时表现为 502，
// 而排查方向还会被「注册是成功的」误导。本用例对旧实现是红的（strict 分支）。
func TestRegistryDiscoverFailureIsNotSilent(t *testing.T) {
	driver := registerOnlyDriver(t)
	yaml := strings.Replace(regOnlyYAML, "__DRIVER__", driver, 1)

	t.Run("缺省：降级为 Warn（带发现侧后果文案），注册句柄仍在", func(t *testing.T) {
		env, w := newTestEnv(t, yaml)
		if err := Lookup("registry")(env); err != nil {
			t.Fatalf("缺省姿态应降级而不是即退：%v", err)
		}
		if env.Registry == nil {
			t.Fatal("注册本身没失败，注册句柄必须建出来")
		}
		if env.Discover != nil {
			t.Fatal("发现句柄建不出来时必须保持零值（降级），不得留半成品")
		}
		if !strings.Contains(w.joined(), ConsequenceDiscovery) {
			t.Fatalf("降级告警必须带**发现侧**后果文案（否则与注册失败混为一谈），实得：\n%s", w.joined())
		}
		if !strings.Contains(w.joined(), "component=registry") {
			t.Fatalf("降级告警必须点名组件名（strict 键就是它），实得：\n%s", w.joined())
		}
	})

	t.Run("strict.registry=true：即退", func(t *testing.T) {
		env, _ := newTestEnv(t, yaml)
		env.Strict["registry"] = true
		err := Lookup("registry")(env)
		if err == nil {
			t.Fatal("strict.registry=true 时发现侧失败必须即退（旧实现只告警，本断言即为回归网）")
		}
		if !strings.Contains(err.Error(), "init registry") {
			t.Fatalf("即退错误必须带 init <name> 上下文，实得：%v", err)
		}
	})
}
