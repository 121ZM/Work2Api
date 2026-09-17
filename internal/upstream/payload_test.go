package upstream

import (
	"encoding/json"
	"testing"

	"work2api/internal/region"
)

func mustObj(t *testing.T, raw string) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		t.Fatalf("unmarshal %q: %v", raw, err)
	}
	return obj
}

func roles(t *testing.T, obj map[string]any) []string {
	t.Helper()
	msgs, _ := obj["messages"].([]any)
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		r, _ := mm["role"].(string)
		out = append(out, r)
	}
	return out
}

// TestEnsureLeadingSystemGlobalPrepends 国际版缺 system 时必须补一条。
func TestEnsureLeadingSystemGlobalPrepends(t *testing.T) {
	obj := mustObj(t, `{"messages":[{"role":"user","content":"hi"}]}`)
	ensureLeadingSystem(obj, region.Global)
	got := roles(t, obj)
	want := []string{"system", "user"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("roles = %v, want %v", got, want)
	}
	// 原有消息内容不能被改动
	msgs, _ := obj["messages"].([]any)
	last, _ := msgs[1].(map[string]any)
	if last["content"] != "hi" {
		t.Fatalf("original message mutated: %+v", last)
	}
}

// TestEnsureLeadingSystemGlobalKeepsExisting CN 不补，已有 system 也不重复补。
func TestEnsureLeadingSystemNoop(t *testing.T) {
	// 已有 system → 原样
	obj := mustObj(t, `{"messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}]}`)
	ensureLeadingSystem(obj, region.Global)
	if got := roles(t, obj); len(got) != 2 || got[0] != "system" {
		t.Fatalf("should not prepend when system already first, roles=%v", got)
	}

	// 国内版 → 不动
	obj = mustObj(t, `{"messages":[{"role":"user","content":"hi"}]}`)
	ensureLeadingSystem(obj, region.CN)
	if got := roles(t, obj); len(got) != 1 || got[0] != "user" {
		t.Fatalf("CN should not be modified, roles=%v", got)
	}
}

// TestEnsureLeadingSystemRoleCaseInsensitive role 大小写不应导致重复补位。
func TestEnsureLeadingSystemRoleCaseInsensitive(t *testing.T) {
	obj := mustObj(t, `{"messages":[{"role":"System","content":"s"},{"role":"user","content":"hi"}]}`)
	ensureLeadingSystem(obj, region.Global)
	if got := roles(t, obj); len(got) != 2 {
		t.Fatalf("role case should be tolerated, roles=%v", got)
	}
}

// TestEnsureLeadingSystemEmptyMessages 空 messages 也要补（上游同样要求首条为 system）。
func TestEnsureLeadingSystemEmptyMessages(t *testing.T) {
	obj := mustObj(t, `{"messages":[]}`)
	ensureLeadingSystem(obj, region.Global)
	if got := roles(t, obj); len(got) != 1 || got[0] != "system" {
		t.Fatalf("empty messages should get a system message, roles=%v", got)
	}
}

// TestPrepareBodyKeepsInvariants 四处不变量同时生效且互不干扰。
//
// 注意这里的顺序效应：normalizeRoles 先把 developer 改写成 system，
// 于是首条消息**已经**满足国际版要求，ensureLeadingSystem 便不再补位 ——
// 结果是 2 条而不是 3 条。这是正确行为（少一次无谓注入），
// 顺序反过来的话就会多出一条冗余 system 消息。
func TestPrepareBodyKeepsInvariants(t *testing.T) {
	src := `{
		"model":"glm-5.2",
		"stream":false,
		"tool_choice":{"type":"function","function":{"name":"get_weather"}},
		"messages":[{"role":"developer","content":"be nice"},{"role":"user","content":"hi"}]
	}`
	out := PrepareBody([]byte(src), region.Global)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if obj["stream"] != true {
		t.Fatalf("stream should be forced true, got %v", obj["stream"])
	}
	if obj["tool_choice"] != "get_weather" {
		t.Fatalf("tool_choice should collapse to name, got %v", obj["tool_choice"])
	}
	got := roles(t, obj)
	want := []string{"system", "user"} // developer→system 已满足首条 system，无需再补
	if len(got) != len(want) {
		t.Fatalf("roles = %v, want %v（多出的 system 说明补位与改写顺序反了）", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("roles = %v, want %v", got, want)
		}
	}
	// 首条必须是 system —— 这是国际版的硬要求
	if got[0] != "system" {
		t.Fatalf("first role = %q, global requires system", got[0])
	}
}

// TestPrepareBodyGlobalFirstMessageIsAlwaysSystem 国际版输出必须首条为 system。
//
// 覆盖所有可能的首条角色：user / developer / assistant / 空。
func TestPrepareBodyGlobalFirstMessageIsAlwaysSystem(t *testing.T) {
	for _, first := range []string{"user", "developer", "assistant", "tool"} {
		src := `{"model":"glm-5.2","messages":[{"role":"` + first + `","content":"x"}]}`
		out := PrepareBody([]byte(src), region.Global)
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("output not JSON: %v", err)
		}
		got := roles(t, obj)
		if len(got) == 0 || got[0] != "system" {
			t.Fatalf("first role %q -> roles %v, global requires leading system", first, got)
		}
	}
}

// TestPrepareBodyCNNoSystemInjection 国内版不得注入 system ——
// 实测国内版接受首条 user，注入会改变用户请求语义。
func TestPrepareBodyCNNoSystemInjection(t *testing.T) {
	src := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	out := PrepareBody([]byte(src), region.CN)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if got := roles(t, obj); len(got) != 1 || got[0] != "user" {
		t.Fatalf("CN must not inject system, roles=%v", got)
	}
}

// TestPrepareBodyMalformedPassthrough 无法解析的 body 原样返回，不 panic。
func TestPrepareBodyMalformedPassthrough(t *testing.T) {
	for _, in := range []string{"", "not json", "[1,2,3]"} {
		got := PrepareBody([]byte(in), region.Global)
		if string(got) != in {
			t.Fatalf("PrepareBody(%q) = %q, want passthrough", in, string(got))
		}
	}
}
