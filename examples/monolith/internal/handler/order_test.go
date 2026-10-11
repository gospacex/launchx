package handler

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/gospacex/launchx/examples/monolith/internal/service"
)

// TestLimitOfBounds 钉住列表接口的条数边界。
//
// 反向探针：把 limitOf 里的上界判定删掉（原样返回 n），本用例立刻红
// （`?limit=10000000` 那条期望 200）。它不是风格问题：limit 来自 query 且这两个
// 接口不在鉴权分组下，无上界等于把"拉爆内存"的旋钮交给任意调用方。
func TestLimitOfBounds(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"缺省（空串）→ 默认值", "", limitDefault},
		{"非数字 → 默认值", "abc", limitDefault},
		{"零 → 默认值（0 在仓储侧是不限）", "0", limitDefault},
		{"负数 → 默认值", "-5", limitDefault},
		{"正常值 → 原样", "50", 50},
		{"刚好等于上界 → 原样", "200", limitMax},
		{"超上界 → 截断", "10000000", limitMax},
		{"带空格 → 仍能解析", " 30 ", 30},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := limitOf(tc.raw); got != tc.want {
				t.Fatalf("limitOf(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}

// TestFailureOfMapsDomainErrors 钉住「状态码与对外文案同一张表」这条约束。
//
// 这两者曾经分开判：statusOf 只回状态码、文案由调用方取 err.Error()，于是 unknown
// 分支上出现「状态码说内部错误、响应体却把驱动原文摊出去」的中间态。分开判迟早
// 会再漂 —— 表里绑死，就分不开了。
func TestFailureOfMapsDomainErrors(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		status  int
		wantMsg string
	}{
		{"入参不合法 → 400（领域文案原文可见）", service.ErrInvalidInput, http.StatusBadRequest, service.ErrInvalidInput.Error()},
		{"未找到 → 404", service.ErrNotFound, http.StatusNotFound, service.ErrNotFound.Error()},
		{"组件缺席 → 501 且不给细节", service.ErrComponentUnavailable, http.StatusNotImplemented, "service temporarily unavailable"},
		{"未知错误 → 500 且只说 internal error", errors.New("dial tcp <db-host>:<port>: connect: connection refused"), http.StatusInternalServerError, "internal error"},
		// 包装过的领域错误仍按领域判 —— 状态码是关键：包装不改变"这是 404"这个事实。
		// 文案取 err.Error() 时会带上包装层的前缀（"service: get order\n..."），
		// 那条前缀是层名，不是内部细节，故这里只断言它**包含**领域文案。
		{"包装过的领域错误 → 仍按领域判", wrapf(service.ErrNotFound), http.StatusNotFound, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, msg := failureOf(tc.err)
			if status != tc.status {
				t.Fatalf("status = %d, want %d", status, tc.status)
			}
			if tc.wantMsg == "" {
				if !strings.Contains(msg, service.ErrNotFound.Error()) {
					t.Fatalf("msg = %q, want 包含领域文案 %q", msg, service.ErrNotFound.Error())
				}
				return
			}
			if msg != tc.wantMsg {
				t.Fatalf("msg = %q, want %q", msg, tc.wantMsg)
			}
		})
	}
}

// TestFailureOfLeaksNoInternalDetail unknown 分支不得把原始错误文本回给调用方。
//
// 这是 CWE-209 的可执行判据：库地址、表结构、依赖拓扑都出现在驱动原文里。
// 反向探针：把 default 分支改成 `return 500, err.Error()`，本用例立刻红。
func TestFailureOfLeaksNoInternalDetail(t *testing.T) {
	internal := errors.New("Unknown column 'item' in 'field list'")
	_, msg := failureOf(internal)
	if msg == internal.Error() || msg == "" {
		t.Fatalf("对外文案 = %q —— 不得回吐内部错误原文", msg)
	}
}

func wrapf(err error) error { return errors.Join(errors.New("service: get order"), err) }
