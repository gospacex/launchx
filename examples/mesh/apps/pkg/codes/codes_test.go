package codes

import (
	"errors"
	"strings"
	"testing"
)

// 根因哨兵：模拟 gorm.ErrRecordNotFound 这类需要穿透到调用方的底层错误。
var errSentinel = errors.New("record not found")

func TestErrorUnwrapPenetratesToCause(t *testing.T) {
	be := Wrap(OrderNotFound, "order not found", errSentinel)
	if !errors.Is(be, errSentinel) {
		t.Fatalf("errors.Is 未穿透到 Cause: %#v", be)
	}
	var got *Error
	if !errors.As(be, &got) || got.Code != OrderNotFound {
		t.Fatalf("errors.As 未取回业务错误: %#v", be)
	}
}

func TestErrorWithoutCause(t *testing.T) {
	be := New(InvalidArgument, "bad request")
	if be.Unwrap() != nil {
		t.Fatalf("无 cause 时 Unwrap 应为 nil")
	}
	if got, want := be.Error(), "[10001] bad request"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestCodeString(t *testing.T) {
	cases := []struct {
		c    Code
		want string
	}{
		{OK, "code=0"},
		{OrderNotFound, "code=20404"},
		{StockNotEnough, "code=50300"},
		{Unimplemented, "code=10501"},
		{Unauthorized, "code=10401"},
		{RateLimited, "code=10429"},
	}
	for _, tc := range cases {
		if got := tc.c.String(); got != tc.want {
			t.Fatalf("Code(%d).String() = %q, want %q", int32(tc.c), got, tc.want)
		}
	}
}

// 带 Cause 的格式化分支：Error() 必须是 `[code] msg: cause` 三段。
func TestErrorWithCauseFormatting(t *testing.T) {
	be := Wrap(OrderCreateFail, "create failed", errSentinel)
	got := be.Error()
	for _, want := range []string{"[20500]", "create failed", errSentinel.Error()} {
		if !strings.Contains(got, want) {
			t.Fatalf("Error() = %q, want 含 %q（三段式）", got, want)
		}
	}
}

func TestFrom(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantC   Code
		wantMsg string
	}{
		{"nil 归 OK", nil, OK, ""},
		{"业务错误取自身码", New(StockNotEnough, "out of stock"), StockNotEnough, "out of stock"},
		{"非业务错误一律 Internal", errSentinel, Internal, errSentinel.Error()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotC, gotMsg := From(tc.err)
			if gotC != tc.wantC || gotMsg != tc.wantMsg {
				t.Fatalf("From() = (%d, %q), want (%d, %q)", gotC, gotMsg, tc.wantC, tc.wantMsg)
			}
		})
	}
}
