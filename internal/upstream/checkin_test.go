package upstream

import (
	"errors"
	"fmt"
	"testing"
)

// TestIsAlreadyCheckedIn 上游把「今天已签到」表达成 HTTP 400 + code=10001，
// 与真实失败共用同一条错误通道 —— 必须能区分开，否则面板天天报红、
// 调度器还会把好账号记成失败。
func TestIsAlreadyCheckedIn(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{
			"实测报文 code=10001",
			&Error{Kind: ErrClient, Status: 400,
				Msg: `{"code":10001,"msg":"今天已签到，请明天再来","requestId":"daca36f2"}`},
			true,
		},
		{"code=10002", fmt.Errorf(`upstream http 400: {"code":10002,"msg":"x"}`), true},
		{"code=10003", fmt.Errorf(`upstream http 400: {"code":10003,"msg":"x"}`), true},
		{"仅文案已签到", errors.New("今天已签到，请明天再来"), true},
		{"英文文案", errors.New("Already checked in today"), true},
		{"真实失败：积分不足", &Error{Kind: ErrHardCredit, Status: 402, Msg: `{"code":1005,"msg":"积分不足"}`}, false},
		{"真实失败：限流", &Error{Kind: ErrSoftRate, Status: 429, Msg: `{"code":9074,"msg":"too many requests"}`}, false},
		{"真实失败：会话失效", errors.New("Offline user session not found"), false},
		{"空错误文本", errors.New(""), false},
	}
	for _, tc := range cases {
		if got := isAlreadyCheckedIn(tc.err); got != tc.want {
			t.Fatalf("%s: isAlreadyCheckedIn(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// TestIsAlreadyCheckedInDoesNotSwallowRealErrors 防回归：
// 判定必须精确到 code / 文案，不能因为报文里含 "100" 之类子串就误判。
func TestIsAlreadyCheckedInDoesNotSwallowRealErrors(t *testing.T) {
	for _, msg := range []string{
		`{"code":11001,"msg":"first message is not system prompt"}`,
		`{"code":10010,"msg":"参数错误"}`,
		`{"code":1000,"msg":"ok"}`,
	} {
		if isAlreadyCheckedIn(errors.New(msg)) {
			t.Fatalf("false positive on %s", msg)
		}
	}
}
