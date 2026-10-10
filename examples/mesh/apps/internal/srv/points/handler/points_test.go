package handler

import (
	"context"
	"errors"
	"testing"

	"github.com/gospacex/launchx/examples/mesh/apps/internal/srv/points/service"
	"github.com/gospacex/launchx/examples/mesh/apps/pkg/codes"
	pointspb "github.com/gospacex/launchx/examples/mesh/apps/pkg/kitex_gen/points"
)

// fakeSvc 结构性满足 handler 依赖的（未导出）窄接口。
type fakeSvc struct {
	view    *service.AccountView
	err     error
	calls   int
	gotUser uint64
}

func (f *fakeSvc) GetPoints(_ context.Context, userID uint64) (*service.AccountView, error) {
	f.calls++
	f.gotUser = userID
	if f.err != nil {
		return nil, f.err
	}
	return f.view, nil
}

func newHandler(svc *fakeSvc, traceOn bool) *PointsHandler { return New(svc, traceOn) }

// GetAccount：view → pb 的映射，含账户结构体。
func TestGetAccountMapsViewToResp(t *testing.T) {
	svc := &fakeSvc{view: &service.AccountView{UserID: 9, Balance: 250}}
	h := newHandler(svc, false)

	resp, err := h.GetAccount(context.Background(), &pointspb.GetAccountReq{UserId: 9})
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if svc.gotUser != 9 {
		t.Fatalf("svc.gotUser = %d, want 9", svc.gotUser)
	}
	if resp.UserId != 9 || resp.Account == nil || resp.Account.Balance != 250 {
		t.Fatalf("resp = %+v", resp)
	}
}

// 零值请求（UserId=0）不 panic，原样下传 user 0（由 service 决定语义）。
func TestGetAccountZeroUserIsForwarded(t *testing.T) {
	svc := &fakeSvc{view: &service.AccountView{UserID: 0, Balance: 0}}
	h := newHandler(svc, false)

	resp, err := h.GetAccount(context.Background(), &pointspb.GetAccountReq{})
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if resp.Account == nil || resp.Account.Balance != 0 {
		t.Fatalf("resp = %+v", resp)
	}
}

// service 报错：原样上抛（不返回半成品响应）。
func TestGetAccountPropagatesServiceError(t *testing.T) {
	h := newHandler(&fakeSvc{err: errors.New("db down")}, false)
	resp, err := h.GetAccount(context.Background(), &pointspb.GetAccountReq{UserId: 9})
	if err == nil {
		t.Fatal("service 报错应上抛")
	}
	if resp != nil {
		t.Fatalf("出错不应返回响应体，实际 %+v", resp)
	}
}

// 未接线方法：显式 Unimplemented（不是 panic，也不是空 resp,nil）——
// **Unimplemented 的唯一落点是 handler**。
//
// 判据是 respNil（具体类型的指针为空）而不是把 resp 装进 `any` 判 nil：
// 装进 any 的 typed nil 不等于 nil，会把「返回了类型化空指针」误判成有响应体。
func TestUnwiredMethodsReturnUnimplemented(t *testing.T) {
	h := newHandler(&fakeSvc{}, false)
	cases := []struct {
		name string
		call func() (respNil bool, err error)
	}{
		{"Earn", func() (bool, error) {
			resp, err := h.Earn(context.Background(), &pointspb.EarnReq{UserId: 1})
			return resp == nil, err
		}},
		{"Consume", func() (bool, error) {
			resp, err := h.Consume(context.Background(), &pointspb.ConsumeReq{UserId: 1})
			return resp == nil, err
		}},
		{"ListLedgers", func() (bool, error) {
			resp, err := h.ListLedgers(context.Background(), &pointspb.ListLedgersReq{UserId: 1})
			return resp == nil, err
		}},
		{"Report", func() (bool, error) {
			resp, err := h.Report(context.Background(), &pointspb.ReportReq{UserId: 1})
			return resp == nil, err
		}},
	}
	for _, tc := range cases {
		respNil, err := tc.call()
		if err == nil {
			t.Fatalf("%s 必须显式报错，不得空实现", tc.name)
		}
		if !respNil {
			t.Fatalf("%s 不应返回响应体", tc.name)
		}
		if c, _ := codes.From(err); c != codes.Unimplemented {
			t.Fatalf("%s 应回 Unimplemented，实际 %v (err=%v)", tc.name, c, err)
		}
	}
}

// 埋点开关不改变行为。
func TestGetAccountTraceFlagDoesNotChangeBehavior(t *testing.T) {
	for _, traceOn := range []bool{true, false} {
		svc := &fakeSvc{view: &service.AccountView{UserID: 9, Balance: 7}}
		h := newHandler(svc, traceOn)
		resp, err := h.GetAccount(context.Background(), &pointspb.GetAccountReq{UserId: 9})
		if err != nil || resp.Account == nil || resp.Account.Balance != 7 {
			t.Fatalf("traceOn=%v 行为异常: resp=%+v err=%v", traceOn, resp, err)
		}
	}
}
