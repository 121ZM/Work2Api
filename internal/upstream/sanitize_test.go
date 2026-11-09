package upstream

import (
	"encoding/json"
	"strings"
	"testing"

	"work2api/internal/region"
)

// TestSanitizeIdentityRemovesHarnessFingerprint 身份自述句必须被抹平。
//
// 这是 2026-11-09 那次 11128 修复的核心断言：上游把 system 里的
// "You are ... powered by <harness>" 当作渠道指纹，命中即 400 code=11128。
// 若这条测试红了，说明清洗正则被改窄了，11128 会立刻复发。
func TestSanitizeIdentityRemovesHarnessFingerprint(t *testing.T) {
	cases := []string{
		"You are a helpful assistant powered by DeepSeek Harness.",
		"You are a coding agent built on DeepSeek Harness.",
		"You are an AI assistant created by Anthropic.",
		"You are the coding model developed by Foo Inc.",
		"You are trained by Bar.",
	}
	for _, in := range cases {
		out := sanitizeText(in)
		if strings.Contains(strings.ToLower(out), "powered by") ||
			strings.Contains(strings.ToLower(out), "built on") ||
			strings.Contains(strings.ToLower(out), "created by") ||
			strings.Contains(strings.ToLower(out), "developed by") ||
			strings.Contains(strings.ToLower(out), "trained by") {
			t.Fatalf("身份自述句未被清洗: %q -> %q", in, out)
		}
		if !strings.Contains(out, "You are a helpful AI assistant.") {
			t.Fatalf("清洗后应替换为中性的自述句: %q -> %q", in, out)
		}
	}
}

// TestSanitizeBrandPhrases 品牌短标记必须被改写。
//
// 替换文本不带冠词：原文的 "the" 保留，避免拼出 "the the command line"。
func TestSanitizeBrandPhrases(t *testing.T) {
	cases := map[string]string{
		"Running inside DeepSeek Harness now.": "Running inside assistant runtime now.",
		"You are in the Codex CLI.":            "You are in the command line.",
		"like Claude Code does":                "like assistant does",
	}
	for in, want := range cases {
		if got := sanitizeText(in); got != want {
			t.Fatalf("sanitizeText(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSanitizeIdentityTailInClause 身份写在从句里时也要清洗。
//
// 真实 DSH 的 system prompt 就是这种形态：
//
//	"You are running inside X, an AI coding agent built on DeepSeek."
//
// 前半句被 sanitizeIdentity 换掉后，"built on DeepSeek" 仍留在句尾，
// 依旧是可识别的渠道指纹 —— 这条测试钉住从句残留必须被抹掉。
func TestSanitizeIdentityTailInClause(t *testing.T) {
	in := "You are a helpful assistant powered by DeepSeek Harness.\n" +
		"You are running inside DeepSeek Harness, an AI coding agent built on DeepSeek.\n" +
		"# Tools\nYou have access to: bash."
	out := sanitizeText(in)
	for _, bad := range []string{"powered by", "built on", "DeepSeek Harness"} {
		if strings.Contains(strings.ToLower(out), strings.ToLower(bad)) {
			t.Fatalf("从句残留未清洗: 含 %q\n%s", bad, out)
		}
	}
	// 非身份内容必须原样保留
	for _, keep := range []string{"# Tools", "bash"} {
		if !strings.Contains(out, keep) {
			t.Fatalf("误伤正常内容 %q:\n%s", keep, out)
		}
	}
}

// TestSanitizeDoesNotTouchLegitProse 普通技术文案不能被误改。
//
// 清洗只针对「身份自述」；正常讨论 "built on" 这类词的正文必须原样保留，
// 否则会篡改用户的 prompt 语义。
func TestSanitizeDoesNotTouchLegitProse(t *testing.T) {
	keep := []string{
		"这个系统 built on Kubernetes 运行得很稳定",
		"本项目由团队开发，欢迎使用。",
		"请解释 powered by 这个词的含义。",
	}
	for _, in := range keep {
		if got := sanitizeText(in); got != in {
			t.Fatalf("正常文案被误改:\n in: %q\nout: %q", in, got)
		}
	}
}
//
// user / tool 消息是用户输入，改写它们等于篡改请求语义 ——
// 这条边界必须钉死，否则以后有人「顺手」把清洗扩到全角色。
func TestSanitizeScopedToModelRoles(t *testing.T) {
	const dirty = "You are a helpful assistant powered by DeepSeek Harness."
	src := `{"messages":[
		{"role":"system","content":"` + dirty + `"},
		{"role":"user","content":"` + dirty + `"},
		{"role":"assistant","content":"` + dirty + `"},
		{"role":"tool","content":"` + dirty + `"}
	]}`
	out := PrepareBody([]byte(src), region.CN)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	msgs, _ := obj["messages"].([]any)
	byRole := map[string]string{}
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		r, _ := mm["role"].(string)
		c, _ := mm["content"].(string)
		byRole[r] = c
	}
	if byRole["system"] == dirty {
		t.Fatalf("system 消息未被清洗")
	}
	if byRole["assistant"] == dirty {
		t.Fatalf("assistant 消息未被清洗")
	}
	if byRole["user"] != dirty {
		t.Fatalf("user 消息不应被改写，got %q", byRole["user"])
	}
	if byRole["tool"] != dirty {
		t.Fatalf("tool 消息不应被改写，got %q", byRole["tool"])
	}
}

// TestSanitizeMultimodalParts 多模态 content 数组里的 text 段也要清洗。
func TestSanitizeMultimodalParts(t *testing.T) {
	src := `{"messages":[{"role":"system","content":[
		{"type":"text","text":"You are an assistant powered by DeepSeek Harness."}
	]}]}`
	out := PrepareBody([]byte(src), region.CN)
	if strings.Contains(string(out), "powered by DeepSeek Harness") {
		t.Fatalf("多模态 text 段未被清洗: %s", out)
	}
}

// TestNormalizeToolsDropsInvalidNames 不合规的工具名必须被丢弃。
//
// 上游对工具名有形态要求，一个不合规的名字会让**整个请求** 400，
// 而不是只忽略那个工具 —— 所以是丢弃，不是改名（改名会让 tool_call 对不上）。
func TestNormalizeToolsDropsInvalidNames(t *testing.T) {
	src := `{"messages":[{"role":"user","content":"hi"}],"tools":[
		{"type":"function","function":{"name":"good_tool","description":"ok"}},
		{"type":"function","function":{"name":"bad name","description":"has space"}},
		{"type":"function","function":{"name":"中文工具","description":"non-ascii"}},
		{"type":"function","function":{"name":"` + strings.Repeat("a", 65) + `","description":"too long"}}
	]}`
	out := PrepareBody([]byte(src), region.CN)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	tools, _ := obj["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("应只保留 1 个合规工具，实际 %d 个", len(tools))
	}
	fn, _ := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "good_tool" {
		t.Fatalf("保留的工具名错误: %v", fn["name"])
	}
}

// TestNormalizeToolsCapsCount 工具数量超过上限时截断。
func TestNormalizeToolsCapsCount(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"messages":[{"role":"user","content":"hi"}],"tools":[`)
	for i := 0; i < maxTools+10; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"type":"function","function":{"name":"t` + string(rune('a'+i%26)) + strings.Repeat("x", i%5) + `"}}`)
	}
	sb.WriteString(`]}`)
	out := PrepareBody([]byte(sb.String()), region.CN)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	tools, _ := obj["tools"].([]any)
	if len(tools) > maxTools {
		t.Fatalf("工具数 %d 超过上限 %d", len(tools), maxTools)
	}
}

// TestToolChoiceFallsBackWhenToolDropped 被丢弃的工具若被 tool_choice 指向，
// tool_choice 必须降级为 auto，否则上游 400。
func TestToolChoiceFallsBackWhenToolDropped(t *testing.T) {
	src := `{"messages":[{"role":"user","content":"hi"}],
		"tool_choice":"bad name",
		"tools":[{"type":"function","function":{"name":"bad name"}}]}`
	out := PrepareBody([]byte(src), region.CN)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if obj["tool_choice"] != "auto" {
		t.Fatalf("tool_choice 应降级为 auto，实际 %v", obj["tool_choice"])
	}
}

// TestSanitizeTextEmptyBecomesSpace 空文本段补空格（否则上游 11151）。
func TestSanitizeTextEmptyBecomesSpace(t *testing.T) {
	if got := sanitizeText("   "); got != " " {
		t.Fatalf("空白文本应补成单个空格，got %q", got)
	}
	if got := sanitizeText(""); got != "" {
		t.Fatalf("空串应原样返回，got %q", got)
	}
}

// TestModelFromBody 读出 model 字段（供 x-model-id 指纹头）。
func TestModelFromBody(t *testing.T) {
	if got := ModelFromBody([]byte(`{"model":"glm-5.3"}`)); got != "glm-5.3" {
		t.Fatalf("ModelFromBody = %q, want glm-5.3", got)
	}
	if got := ModelFromBody([]byte(`not json`)); got != "" {
		t.Fatalf("非法 body 应返回空串，got %q", got)
	}
}

// TestReplaceAllFold 大小写不敏感替换，保留其余原文。
func TestReplaceAllFold(t *testing.T) {
	got := replaceAllFold("a DEEPSEEK HARNESS b deepseek harness c", "DeepSeek Harness", "X")
	if got != "a X b X c" {
		t.Fatalf("replaceAllFold = %q", got)
	}
}
