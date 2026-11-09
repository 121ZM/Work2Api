package upstream

import "testing"

// TestIsTransientBlock11128 11128 必须被识别为「重发可恢复」。
//
// 定性依据（实测 2026-11-09）：200 次同请求只命中个位数，且上游响应体
// 自己写着 displayMsg.en = "Request blocked. Please send it again"。
// 这条判定驱动 handler 走「原账号重发」，而不是判死给客户端 ——
// 若这里漏判，用户看到的就是随机的 400。
func TestIsTransientBlock11128(t *testing.T) {
	body := `{"code":11128,"msg":"Illegal API invocation from an unapproved channel",` +
		`"requestId":"2408453e","retry_policy":{"retry":false},` +
		`"displayMsg":{"en":"Request blocked. Please send it again","zh":"请求被拦截，请重试"}}`
	if !IsTransientBlock(body) {
		t.Fatal("11128 应识别为瞬时拦截")
	}
}

// TestIsTransientBlockOtherCodes 其它错误码不能被误判成瞬时拦截。
//
// 误判的代价很大：真正的形态错误（11101/11102）会被无谓重发 3 次，
// 既拖慢响应又浪费额度。
func TestIsTransientBlockOtherCodes(t *testing.T) {
	cases := []string{
		`{"code":11101,"msg":"tool_choice type error"}`,
		`{"code":11102,"msg":"service info not found"}`,
		`{"code":12153,"msg":"refresh token is invalid"}`,
		`{"code":429,"msg":"rate limited"}`,
		``,
		`upstream 500 internal error`,
	}
	for _, c := range cases {
		if IsTransientBlock(c) {
			t.Fatalf("误判为瞬时拦截: %q", c)
		}
	}
}

// TestIsTransientBlockNilSafe 空/二进制内容不应 panic。
func TestIsTransientBlockNilSafe(t *testing.T) {
	_ = IsTransientBlock("")
	_ = IsTransientBlock("\x00\x01\x02")
}
