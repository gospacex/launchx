package mode

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gospacex/configx"
	"github.com/gospacex/rpcx"
)

// holdersYAML 从内联 yaml 构造装配句柄（无需落 testdata 文件的用例用）。
func holdersYAML(t *testing.T, yaml string) *AppHolders {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := configx.ReadConfig(configx.ConfigOptions{ConfigFile: path, ConfigType: "yaml"})
	if err != nil {
		t.Fatalf("read inline config: %v", err)
	}
	return &AppHolders{Cfg: cfg, ServiceName: "mode-test"}
}

// clientOptionCount 取客户端选项工厂产出的选项条数。
func clientOptionCount(t *testing.T, h *AppHolders, svc string) int {
	t.Helper()
	v, err := h.Out.RPCClientOptions(svc)
	if err != nil {
		t.Fatalf("RPCClientOptions(%q): %v", svc, err)
	}
	return reflect.ValueOf(v).Len()
}

// TestRPCGovernanceAbsentIsAllOff 未配 xmiddleware.rpcx 段：三件套与指标全关。
//
// 这条是「加治理面不许改变既有行为」的守卫：条数必须与治理面引入之前一致
// （基础 2 条 + 直连目标 1 条 = 3，见 TestBuildOptionsAttachesCoordx 的口径）。
func TestRPCGovernanceAbsentIsAllOff(t *testing.T) {
	h := holders(t, "bff.yaml")
	if err := Assemble(h); err != nil {
		t.Fatalf("assemble: %v", err)
	}
	gov := h.Out.RPCGovernance
	if gov.Retry.Enabled || gov.CircuitBreaker.Enabled || gov.LoadBalancer != rpcx.LoadBalanceDefault {
		t.Fatalf("未配段时治理应全关，实得 %+v", gov)
	}
	if h.Out.MetricsEnabled {
		t.Fatal("未配 metrics 段时指标应为关")
	}
	if got := clientOptionCount(t, h, "downstream-svc"); got != 3 {
		t.Fatalf("客户端选项 %d 条，want 3（治理面不得有隐式开关）", got)
	}
}

// TestRPCGovernanceConfigLands 配置真的落到 kitex 客户端选项里。
//
// 条数拆解：基础 2（RPCTimeout/ConnectTimeout）+ 直连目标 1 + 三件套 3 + 指标 1 = 7。
// 少一条就说明某个开关没接上 —— 这类"配了不生效"不会报错，只能靠计数钉住。
func TestRPCGovernanceConfigLands(t *testing.T) {
	h := holders(t, "rpcgov.yaml")
	if err := Assemble(h); err != nil {
		t.Fatalf("assemble: %v", err)
	}

	gov := h.Out.RPCGovernance
	if !gov.Retry.Enabled || gov.Retry.MaxRetryTimes != 3 ||
		gov.Retry.MaxDurationMS != 400 || gov.Retry.BackoffMS != 20 {
		t.Fatalf("retry 未按配置解析：%+v", gov.Retry)
	}
	if !gov.CircuitBreaker.Enabled || gov.CircuitBreaker.ErrRate != 0.2 || gov.CircuitBreaker.MinSample != 30 {
		t.Fatalf("circuit_breaker 未按配置解析：%+v", gov.CircuitBreaker)
	}
	if gov.LoadBalancer != rpcx.LoadBalanceWeightedRoundRobin {
		t.Fatalf("load_balancer = %q", gov.LoadBalancer)
	}
	if !h.Out.MetricsEnabled {
		t.Fatal("metrics.enabled=true 未生效")
	}
	if h.Out.MetricsAddr != "127.0.0.1:19100" {
		t.Fatalf("metrics.addr = %q", h.Out.MetricsAddr)
	}
	if got := clientOptionCount(t, h, "downstream-svc"); got != 7 {
		t.Fatalf("客户端选项 %d 条，want 7（2 基础 + 1 目标 + 3 治理 + 1 指标）", got)
	}
}

// TestRPCGovernanceInvalidConfigFailsFast 非法取值在装配期即退，不做降级。
//
// 每一项都是"配错了也不报错"的类型（退回默认），因此必须在此拦住 ——
// 断言的不只是"报错"，还有错误信息里带上**键名**（否则几十行配置里找不出是哪一条）。
func TestRPCGovernanceInvalidConfigFailsFast(t *testing.T) {
	const tmpl = `
service: {name: bff-gov, mode: bff, addr: "127.0.0.1:18086"}
webx: {addr: "127.0.0.1:18086"}
trace: {enabled: false}
xmiddleware:
  rpcx:
__BODY__
`
	cases := []struct {
		name    string
		body    string
		wantErr []string
	}{
		{
			name:    "未知负载均衡器",
			body:    "    load_balancer: round_robin",
			wantErr: []string{"xmiddleware.rpcx.load_balancer", "round_robin", "weighted_round_robin"},
		},
		{
			name:    "重试次数越界",
			body:    "    retry: {enabled: true, max_attempts: 9}",
			wantErr: []string{"xmiddleware.rpcx.retry.max_attempts", "9", "5"},
		},
		{
			name:    "重试次数为负",
			body:    "    retry: {enabled: true, max_attempts: -1}",
			wantErr: []string{"xmiddleware.rpcx.retry.max_attempts", "-1"},
		},
		{
			name:    "错误率越界",
			body:    "    circuit_breaker: {enabled: true, error_rate: 1.5}",
			wantErr: []string{"xmiddleware.rpcx.circuit_breaker.error_rate", "1.5"},
		},
		{
			name:    "最小样本为负",
			body:    "    circuit_breaker: {enabled: true, min_sample: -3}",
			wantErr: []string{"xmiddleware.rpcx.circuit_breaker.min_sample", "-3"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, line := range strings.Split(c.body, "\n") {
				if !strings.HasPrefix(line, "    ") {
					t.Fatalf("用例内的缩进必须是 4 空格（xmiddleware.rpcx 之下）：%q", line)
				}
			}
			h := holdersYAML(t, strings.Replace(tmpl, "__BODY__", c.body, 1))
			err := Assemble(h)
			if err == nil {
				t.Fatalf("非法配置必须即退，实得 Out=%+v", h.Out)
			}
			for _, want := range c.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("错误信息缺 %q：%v", want, err)
				}
			}
		})
	}
}
