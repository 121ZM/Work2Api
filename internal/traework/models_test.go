package traework

import (
	"strings"
	"testing"

	"work2api/internal/provider"
)

func boolPtr(v bool) *bool { return &v }

// mk 构造一条上游模型配置。usage 传空表示上游没给该字段。
func mk(name, usage, display string, invisible *bool, custom bool) modelConfig {
	c := modelConfig{ConfigName: name, Usage: usage, Invisible: invisible}
	c.DisplayConfig.DisplayName = display
	c.DisplayConfig.IsCustomModel = custom
	return c
}

func ids(list []provider.ModelInfo) []string {
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, m.ID)
	}
	return out
}

func TestPickUserModelsExcludesNonChatUsage(t *testing.T) {
	list := []modelConfig{
		mk("glm-5.2", "chat_completion", "GLM-5.2", nil, false),
		mk("custom_model_claude", "custom_model", "Claude", nil, true),
		mk("summary", "summary", "", nil, false),
	}
	out, skipped := pickUserModels(list)
	got := ids(out)
	if len(got) != 1 || got[0] != "glm-5.2" {
		t.Fatalf("got %v，期望只剩 glm-5.2", got)
	}
	if skipped != 2 {
		t.Fatalf("skipped = %d，期望 2", skipped)
	}
}

// 上游有时不给 usage 字段，此时不能因为「usage 为空」就丢弃。
func TestPickUserModelsKeepsMissingUsage(t *testing.T) {
	out, _ := pickUserModels([]modelConfig{
		mk("glm-5.2", "", "GLM-5.2", nil, false),
	})
	if len(out) != 1 {
		t.Fatalf("usage 缺失时不应被丢弃，got %v", ids(out))
	}
}

func TestPickUserModelsExcludesNoDisplayName(t *testing.T) {
	out, _ := pickUserModels([]modelConfig{
		mk("glm-5.2", "chat_completion", "GLM-5.2", nil, false),
		mk("sagitta", "chat_completion", "-", nil, false),
		mk("aquila", "chat_completion", "", nil, false),
	})
	got := ids(out)
	if len(got) != 1 || got[0] != "glm-5.2" {
		t.Fatalf("got %v，期望只剩 glm-5.2", got)
	}
}

func TestPickUserModelsExcludesInternalAgents(t *testing.T) {
	out, _ := pickUserModels([]modelConfig{
		mk("glm-5.2", "chat_completion", "GLM-5.2", nil, false),
		mk("browser_use_subagent", "chat_completion", "browser_use_subagent", boolPtr(true), false),
		mk("computer_use_subagent", "chat_completion", "computer_use_subagent", boolPtr(true), false),
		mk("explore_sub_agent_v2", "chat_completion", "", boolPtr(true), false),
		mk("file_search_agent", "chat_completion", "search_agent_qwen_fast", boolPtr(true), false),
	})
	got := ids(out)
	if len(got) != 1 || got[0] != "glm-5.2" {
		t.Fatalf("内部 agent 配置应被过滤，got %v", got)
	}
}

// 关键回归：invisible 不等于「不可用」，绝不能拿它当过滤条件。
//
// 实测 traework/global 账号的全部 chat_completion 模型都是 Beta + invisible，
// 一旦按 invisible 过滤，该渠道会一个模型都不剩、退回错误的静态兜底表。
func TestPickUserModelsKeepsInvisibleModels(t *testing.T) {
	list := []modelConfig{
		mk("gpt-5.4", "chat_completion", "GPT-5.4", boolPtr(true), false),
		mk("gpt-5.2", "chat_completion", "GPT-5.2", boolPtr(true), false),
	}
	out, _ := pickUserModels(list)
	got := ids(out)
	if len(got) != 2 {
		t.Fatalf("invisible 的对话模型必须保留，got %v", got)
	}
}

// 隐藏别名（被「正式版」取代的旧 id）也要保留 —— 它们仍可调用。
func TestPickUserModelsKeepsHiddenAliases(t *testing.T) {
	list := []modelConfig{
		mk("glm-5", "chat_completion", "GLM-5", boolPtr(true), false),
		mk("DeepSeek-V4-Pro", "chat_completion", "DeepSeek-V4-Pro", boolPtr(true), false),
		mk("glm-5.3", "chat_completion", "GLM-5.3", boolPtr(false), false),
	}
	out, _ := pickUserModels(list)
	if len(out) != 3 {
		t.Fatalf("隐藏别名不应被丢弃，got %v", ids(out))
	}
}

func TestPickUserModelsDedups(t *testing.T) {
	out, _ := pickUserModels([]modelConfig{
		mk("glm-5.2", "chat_completion", "GLM-5.2", nil, false),
		mk("glm-5.2", "chat_completion", "GLM-5.2", nil, false),
	})
	if len(out) != 1 {
		t.Fatalf("同名配置应去重，got %v", ids(out))
	}
}

// 用实测的 traework/cn 上游结构（39 条）做端到端过滤，期望 19 个可用模型。
func TestPickUserModelsRealisticCN(t *testing.T) {
	list := []modelConfig{
		mk("Doubao-Seed-Evolving", "chat_completion", "Seed-Evolving", nil, false),
		mk("Doubao-Seed-2.1-Pro", "chat_completion", "Seed-2.1-Pro-0915", nil, false),
		mk("seed-code-pro-0430", "chat_completion", "Doubao-Seed-2.1-Pro", boolPtr(true), false),
		mk("Doubao-Seed-2.1-Turbo", "chat_completion", "Seed-2.1-Turbo", nil, false),
		mk("computer_use_subagent", "chat_completion", "computer_use_subagent", boolPtr(true), false),
		mk("Doubao-Seed-2.0-Code", "chat_completion", "Doubao-Seed-2.1-Turbo", boolPtr(true), false),
		mk("browser_use_subagent", "chat_completion", "", boolPtr(true), false),
		mk("glm-5.3", "chat_completion", "GLM-5.3", nil, false),
		mk("glm-5.2", "chat_completion", "GLM-5.2", nil, false),
		mk("glm-5-turbo", "chat_completion", "GLM-5-Turbo", boolPtr(true), false),
		mk("glm-5", "chat_completion", "GLM-5", boolPtr(true), false),
		mk("DeepSeek-V4-Flash-Official", "chat_completion", "DeepSeek-V4-Flash 正式版", nil, false),
		mk("DeepSeek-V4-Flash", "chat_completion", "DeepSeek-V4-Flash", boolPtr(true), false),
		mk("DeepSeek-V4-Pro-Official", "chat_completion", "DeepSeek-V4-Pro 正式版", nil, false),
		mk("DeepSeek-V4-Pro", "chat_completion", "DeepSeek-V4-Pro", boolPtr(true), false),
		mk("kimi-k3", "chat_completion", "Kimi-K3", nil, false),
		mk("kimi-k2.7-code", "chat_completion", "Kimi-K2.7-Code", nil, false),
		mk("kimi-k2.6", "chat_completion", "Kimi-K2.6", nil, false),
		mk("minimax-m3", "chat_completion", "MiniMax-M3", nil, false),
		mk("qwen3.8-max", "chat_completion", "Qwen3.8-Max", nil, false),
		mk("qwen-3.7-plus", "chat_completion", "Qwen3.7-Plus", nil, false),
		mk("sagitta", "chat_completion", "-", boolPtr(true), false),
		mk("aquila", "chat_completion", "-", boolPtr(true), false),
		mk("file_search_agent", "chat_completion", "search_agent_qwen_fast", boolPtr(true), false),
		mk("explore_sub_agent_v2", "chat_completion", "", boolPtr(true), false),
		mk("summary", "summary", "", nil, false),
		mk("custom_model_gemini", "custom_model", "Gemini-3.1-Pro-Preview", nil, false),
	}
	out, skipped := pickUserModels(list)
	if len(out) != 19 {
		t.Fatalf("got %d 个（%v），期望 19 个", len(out), ids(out))
	}
	if skipped != 8 {
		t.Fatalf("skipped = %d，期望 8", skipped)
	}
	// 被过滤掉的必须确实不在结果里
	for _, bad := range []string{
		"summary", "custom_model_gemini", "sagitta", "aquila",
		"file_search_agent", "explore_sub_agent_v2",
		"browser_use_subagent", "computer_use_subagent",
	} {
		for _, id := range ids(out) {
			if id == bad {
				t.Fatalf("%s 不应出现在结果里", bad)
			}
		}
	}
	// 隐藏别名必须保留
	for _, want := range []string{"glm-5", "DeepSeek-V4-Pro", "seed-code-pro-0430"} {
		if !strings.Contains(strings.Join(ids(out), ","), want) {
			t.Fatalf("%s 应保留（invisible 不代表不可用）", want)
		}
	}
}

// 实测的 traework/global 上游结构：13 条里只有 2 条 chat_completion，且都是 invisible。
// 过滤后必须剩 2 个 —— 若按 invisible 过滤就会得到 0 个并退回错误的静态兜底表。
func TestPickUserModelsRealisticGlobal(t *testing.T) {
	list := []modelConfig{
		mk("gpt-5.4", "chat_completion", "GPT-5.4", boolPtr(true), false),
		mk("gpt-5.2", "chat_completion", "GPT-5.2", boolPtr(true), false),
		mk("custom_model_placeholder", "custom_model", "", nil, false),
		mk("custom_model_1M_text", "custom_model", "", nil, false),
		mk("custom_model_1M", "custom_model", "", nil, false),
		mk("custom_model_kimi", "custom_model", "", nil, false),
		mk("custom_model_gemini", "custom_model", "Gemini-3.1-Pro-Preview", boolPtr(false), false),
		mk("custom_model_claude", "custom_model", "", nil, false),
		mk("custom_model_gpt-5", "custom_model", "", nil, false),
		mk("custom_model_no-fc", "custom_model", "", nil, false),
		mk("custom_model_deepseek_chat", "custom_model", "", nil, false),
		mk("custom_model_deepseek_reasoner", "custom_model", "", nil, false),
		mk("custom_model_deepseek_v4", "custom_model", "", nil, false),
	}
	out, _ := pickUserModels(list)
	got := ids(out)
	if len(got) != 2 {
		t.Fatalf("got %v，期望 2 个（gpt-5.4 / gpt-5.2）", got)
	}
}
