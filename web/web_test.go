package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsLoopback(t *testing.T) {
	cases := []struct {
		remote string
		want   bool
	}{
		{"127.0.0.1:54321", true},
		{"127.0.0.1", true},
		{"[::1]:54321", true},
		{"::1", true},
		{"192.168.1.20:54321", false},
		{"10.0.0.7:80", false},
		{"198.18.0.1:80", false},
		{"0.0.0.0:80", false},
		{"", false},
		{"not-an-ip:80", false},
	}
	for _, c := range cases {
		if got := isLoopback(c.remote); got != c.want {
			t.Errorf("isLoopback(%q) = %v，期望 %v", c.remote, got, c.want)
		}
	}
}

// 占位符必须被整体替换成一个合法的 JS 字符串字面量，且页面里不再残留占位符。
func TestRenderKeyReplacesPlaceholder(t *testing.T) {
	out := string(renderKey("w2a_deadbeef"))
	if strings.Contains(out, keyPlaceholder) {
		t.Fatalf("渲染后仍残留占位符 %s", keyPlaceholder)
	}
	if !strings.Contains(out, `var W2A_KEY = "w2a_deadbeef";`) {
		t.Fatalf("未找到注入后的赋值语句")
	}
}

// 空 Key（非本机访问）也要替换成空字符串字面量，而不是留下占位符。
func TestRenderKeyEmptyBecomesEmptyLiteral(t *testing.T) {
	out := string(renderKey(""))
	if strings.Contains(out, keyPlaceholder) {
		t.Fatalf("空 Key 时仍残留占位符")
	}
	if !strings.Contains(out, `var W2A_KEY = "";`) {
		t.Fatalf("空 Key 未被替换成空字符串字面量")
	}
}

// 用户可以把 Key 设成含引号/反斜杠的值，注入结果必须仍是合法 JS 字符串。
// 否则面板脚本会被直接破坏（语法错误 → 整页白屏）。
func TestRenderKeyEscapesHostileKey(t *testing.T) {
	hostile := `a"b\c` + "\n" + `d`
	out := string(renderKey(hostile))

	const prefix = `var W2A_KEY = `
	i := strings.Index(out, prefix)
	if i < 0 {
		t.Fatal("未找到 W2A_KEY 赋值")
	}
	rest := out[i+len(prefix):]
	end := strings.Index(rest, ";\n")
	if end < 0 {
		t.Fatal("未找到赋值语句结尾")
	}
	literal := rest[:end]

	// 用 JSON 解析来验证「这是一个转义正确的字符串字面量」
	var got string
	if err := json.Unmarshal([]byte(literal), &got); err != nil {
		t.Fatalf("注入的字面量 %s 不是合法转义字符串: %v", literal, err)
	}
	if got != hostile {
		t.Fatalf("转义后还原得到 %q，期望 %q", got, hostile)
	}
}

// 回环请求拿到 Key，非回环请求拿不到 —— 这是「监听 0.0.0.0 时不外泄」的保证。
func TestHandlerInjectsKeyOnlyForLoopback(t *testing.T) {
	const key = "w2a_0123456789abcdef0123456789abcdef"
	h := Handler(key)

	call := func(remote string) string {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Body.String()
	}

	local := call("127.0.0.1:40000")
	if !strings.Contains(local, `var W2A_KEY = "`+key+`";`) {
		t.Fatalf("回环请求未注入 Key")
	}

	remote := call("192.168.1.20:40000")
	if strings.Contains(remote, key) {
		t.Fatalf("非回环请求拿到了 Key —— 会把 Key 泄露给局域网/公网")
	}
	if !strings.Contains(remote, `var W2A_KEY = "";`) {
		t.Fatalf("非回环请求应拿到空 Key")
	}
}
