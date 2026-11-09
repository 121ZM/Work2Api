package upstream

import (
	"encoding/json"
	"strings"
	"testing"

	"work2api/internal/region"
)

// TestSanitizeFileReferenceBreaksContiguousMatch 文件引用段必须被打断。
//
// 这是 2026-11-09 那次 11128 的直接根因（实测 4/4 命中）：
// DSH 的 system prompt 里这两句连排，上游渠道校验就判 400：
//
//	Tokens prefixed with @ are paths the user explicitly referenced.
//	Relative paths resolve from the workspace root; ...
//
// 只把 `paths the user` 补成 `paths that the user` 后，7623 字的完整
// system 从 4/4 被拦变为 0/4 通过。匹配的是**整段连续文本**，
// 不是单个词 —— 逐句删词拆不出来，所以只能这样加词。
//
// 若这条测试红了，说明规则被改窄或删了，11128 会立刻复发。
func TestSanitizeFileReferenceBreaksContiguousMatch(t *testing.T) {
	in := "Tokens prefixed with @ are paths the user explicitly referenced. " +
		"Relative paths resolve from the workspace root; absolute paths " +
		"identify files or directories on the host."
	out := sanitizeText(in)
	if strings.Contains(out, "are paths the user explicitly referenced") {
		t.Fatalf("文件引用段未被改写（11128 会复发）:\n%s", out)
	}
	if !strings.Contains(out, "are paths that the user explicitly referenced") {
		t.Fatalf("改写后应保留原句并补上 that:\n%s", out)
	}
	// 其余内容必须一字不改
	if !strings.Contains(out, "Relative paths resolve from the workspace root") {
		t.Fatalf("误伤后一句:\n%s", out)
	}
}

// TestSanitizeFileReferenceIdempotent 重复清洗不能反复加词。
func TestSanitizeFileReferenceIdempotent(t *testing.T) {
	in := "Tokens prefixed with @ are paths the user explicitly referenced."
	once := sanitizeText(in)
	twice := sanitizeText(once)
	if once != twice {
		t.Fatalf("清洗不幂等:\n once=%q\n twice=%q", once, twice)
	}
	if strings.Contains(once, "that that") {
		t.Fatalf("重复加词: %q", once)
	}
}

// TestSanitizeDSHGUI 只调句尾语序，一个词都不删。
func TestSanitizeDSHGUI(t *testing.T) {
	in := "You are interacting with the user through the DeepSeek Harness Web GUI " +
		"whose URL the runtime context names."
	out := sanitizeText(in)
	if strings.Contains(out, "whose URL the runtime context names") {
		t.Fatalf("GUI 描述句未被改写:\n%s", out)
	}
	if !strings.Contains(out, "whose URL is named in the runtime context") {
		t.Fatalf("改写后语义应保持:\n%s", out)
	}
}

// TestSanitizeBrandReducesHarness 品牌词缩减为 DeepSeek。
//
// 顺序依赖：GUI 规则要认 `DeepSeek Harness` 全名，所以品牌缩减必须排在后面。
func TestSanitizeBrandReducesHarness(t *testing.T) {
	out := sanitizeText("Running inside DeepSeek Harness now.")
	if strings.Contains(out, "DeepSeek Harness") {
		t.Fatalf("品牌词未被缩减: %q", out)
	}
	if !strings.Contains(out, "DeepSeek") {
		t.Fatalf("应保留 DeepSeek: %q", out)
	}
}

// TestSanitizeBrandKeepsIdentifiers 路径与标识符不能被改坏。
//
// hub 的实测教训：用 `[ \t_-]*` 会把 `deepseek-harness`、
// `/opt/deepseek-harness/app` 一起改掉，所以只认空格/制表符。
func TestSanitizeBrandKeepsIdentifiers(t *testing.T) {
	for _, in := range []string{
		"see deepseek-harness/app",
		"at /opt/deepseek-harness/app",
		"pkg deepseek_harness",
	} {
		if got := sanitizeText(in); got != in {
			t.Fatalf("标识符被误改: %q -> %q", in, got)
		}
	}
}

// TestSanitizeIdentityRemovesHarnessFingerprint 身份自述句必须被抹平。
func TestSanitizeIdentityRemovesHarnessFingerprint(t *testing.T) {
	cases := []string{
		"You are an AI agent powered by DeepSeek Harness.",
		"You are a helpful assistant powered by DeepSeek Harness.",
		"You are a coding agent built on DeepSeek Harness.",
		"You are an AI assistant created by Anthropic.",
		"You are the coding model developed by Foo Inc.",
	}
	for _, in := range cases {
		out := sanitizeText(in)
		low := strings.ToLower(out)
		for _, bad := range []string{"powered by", "built on", "created by", "developed by"} {
			if strings.Contains(low, bad) {
				t.Fatalf("身份自述句未被清洗: %q -> %q", in, out)
			}
		}
		if !strings.Contains(out, "You are a helpful AI assistant.") {
			t.Fatalf("清洗后应替换为中性的自述句: %q -> %q", in, out)
		}
	}
}

// TestSanitizeIdentityKeepsTrailingSpace 句尾空白必须原样带回。
//
// 丢了空白会把两句话黏成一句（实测过的真 bug）。
func TestSanitizeIdentityKeepsTrailingSpace(t *testing.T) {
	in := "You are an AI agent powered by X.\nNext line."
	out := sanitizeText(in)
	if !strings.Contains(out, "You are a helpful AI assistant.\nNext line.") {
		t.Fatalf("句尾换行被吞: %q", out)
	}
}

// TestSanitizeCodexAndClaudeIntro Codex / Claude Code 自述首段整句中性化。
func TestSanitizeCodexAndClaudeIntro(t *testing.T) {
	codex := "You are a coding agent running in the Codex CLI tool. Follow instructions."
	if out := sanitizeText(codex); strings.Contains(out, "Codex CLI") {
		t.Fatalf("Codex 自述句未中性化: %q", out)
	}
	claude := "You are Claude Code, Anthropic's official CLI for Claude."
	if out := sanitizeText(claude); strings.Contains(out, "Claude Code") {
		t.Fatalf("Claude Code 自述句未中性化: %q", out)
	}
}

// TestSanitizeRewritesLiterals 字面量改写，含把错误码本身当指纹的情况。
func TestSanitizeRewritesLiterals(t *testing.T) {
	out := sanitizeText("报错 code 11128，见 Main branch (you will usually use this for PRs)")
	if strings.Contains(out, "11128") {
		t.Fatalf("错误码字面量未改写: %q", out)
	}
	if strings.Contains(out, "Main branch (") {
		t.Fatalf("Main branch 未改写: %q", out)
	}
	if !strings.Contains(out, "11-128") {
		t.Fatalf("应改写为 11-128: %q", out)
	}
}

// TestSanitizeScopedToModelRoles 只清洗「模型自述」角色，不动用户数据。
func TestSanitizeScopedToModelRoles(t *testing.T) {
	const dirty = "Tokens prefixed with @ are paths the user explicitly referenced."
	src := `{"messages":[
		{"role":"system","content":"` + dirty + `"},
		{"role":"user","content":"` + dirty + `"},
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
		{"type":"text","text":"Tokens prefixed with @ are paths the user explicitly referenced."}
	]}]}`
	out := PrepareBody([]byte(src), region.CN)
	if strings.Contains(string(out), "are paths the user explicitly referenced") {
		t.Fatalf("多模态 text 段未被清洗: %s", out)
	}
}

// TestNormalizeToolsDropsInvalidNames 不合规的工具名必须被丢弃。
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
