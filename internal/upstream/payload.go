// payload.go 改写发往上游的 chat 请求体。
//
// 六处改写是关键不变量，勿删：
//  1. 强制 stream:true —— 上游拒绝非流式请求
//  2. tool_choice 归一化 —— 上游该字段是 string 类型，对象形式会 400 code=11101
//  3. developer → system —— 上游对 role=developer 的消息一律命中内容过滤
//  4. 国际版补首条 system —— 国际版对首条非 system 的请求 400 code=11128
//     （实测：国内版无此要求，同一请求体国内版 200、国际版 400）
//  5. **身份指纹清洗** —— 上游的渠道校验（code=11128
//     "Illegal API invocation from an unapproved channel"）把「客户端自述身份」
//     当作渠道标识：system 里出现 "You are ... powered by X" 这类句子时，
//     等于自报了一个未被批准的门户，请求被拦。详见 sanitizeIdentity。
//  6. 工具名 / 工具描述清洗与数量上限 —— 不合规的工具会拖垮整个请求。
package upstream

import (
	"encoding/json"
	"regexp"
	"strings"

	"work2api/internal/region"
)

// defaultSystemPrompt 国际版补位用的 system 消息内容。
//
// 上游只要求「首条是 system」，不校验内容；取最中性的表述，
// 避免给用户请求叠加任何倾向性指令。
const defaultSystemPrompt = "You are a helpful assistant."

// maxTools 上游接受的工具数量上限。
//
// 官方 IDE 一次最多带约 30-50 个工具；超出的部分上游不报错但也用不上，
// 而带上它们会让请求体显著变大、并可能触发渠道校验。
const maxTools = 30

// sanitizeIdentityRepl 身份自述句的替换文本。
//
// 与参考实现 hub_wb_proxy.py 的 SANITIZE_IDENTITY_REPLACEMENT 保持一致：
// 用一句最中性的自述覆盖原句，既消除门户指纹，又不改变「你是助手」的语义。
const sanitizeIdentityRepl = "You are a helpful AI assistant."

// sanitizeIdentityTail 兜底规则：句中任意位置的「powered/built/created by X」。
//
// sanitizeIdentity 只覆盖 "You are ... <谓词> ..." 这一种句式；实际 harness
// 的 system prompt 常把身份写在从句里，例如：
//
//	"You are running inside X, an AI coding agent built on DeepSeek."
//
// 前半句被 sanitizeIdentity 换掉后，"built on DeepSeek" 仍留在句尾，
// 依旧是可识别的渠道指纹。这条规则把「谓词 + 最多 40 字符 + 句号」整体抹掉。
//
// 作用域仍限制在 system / developer / assistant（见 sanitizeMessages），
// 因此不会动用户的正文。
var sanitizeIdentityTail = regexp.MustCompile(`(?i),?\s*` +
	`(?:an?\s+)?(?:[\w.-]+\s+){0,4}?` +
	`(?:powered by|built on|built by|created by|developed by|made by|trained by)` +
	`\s+[\w.\- ]{1,40}?\.`)

// sanitizeIdentity 匹配「You are <修饰语> powered/built/created by ...」这类
// 客户端自述句。
//
// 实测依据（2026-11-09，来自 ardeyouxipianyi/workbuddy2api-hub 的
// hub_wb_proxy.py:13341-13348）：上游把这类句子当**渠道指纹**，
// 命中即 400 code=11128。典型被拦的自述：
//
//	"You are a helpful assistant powered by DeepSeek Harness."
//	"You are a coding agent running in the Codex CLI"
//	"You are Claude Code"
//
// 正则刻意写得宽：修饰语允许 0-3 个词、可选的 agent/assistant/model 名词，
// 因为各家 harness 的自述句式差别很大，窄正则只会漏。
//
// 注意：Go 的 RE2 **不支持前瞻/后顾断言**（(?=...) 会 panic），
// 所以句尾用「非贪婪到句号」而不是「句号且后面是空白」来表达。
//
// 句尾必须是 `\.(?:\s|$)`，不能是裸 `\.`：模型名 "deepseek-v4.1-flash"
// 里的点会被当成句号，正则提前收尾，清洗出
// "You are a helpful AI assistant.1-flash model." 这种残留
// —— 实测过的真 bug，别改回去。
var sanitizeIdentity = regexp.MustCompile(`(?i)You are\s+` +
	`(?:an?\s+|the\s+)?(?:[\w.-]+\s+){0,3}?` +
	`(?:agent|assistant|model|AI|LLM)?[\s,]*` +
	`(?:powered by|built on|built by|created by|developed by|made by|trained by)` +
	`[^\n]*?\.(\s|$)`)

// sanitizeBrandPhrases 已知会被当作「非批准渠道」的品牌自述片段。
//
// 这些是整句指纹正则抓不到、但同样会触发拦截的短标记。
// 键为待匹配片段（大小写不敏感），值为替换文本。
//
// 替换文本刻意**不带冠词**（不用 "the command line"）：原文常写成
// "in the Codex CLI"，带冠词会拼出 "the the command line"。
// 冠词由原文保留，替换只换核心名词短语。
var sanitizeBrandPhrases = []struct{ from, to string }{
	{"DeepSeek Harness", "assistant runtime"},
	{"Codex CLI", "command line"},
	{"Claude Code", "assistant"},
	{"Claude Agent SDK", "assistant runtime"},
	{"x-anthropic-billing-header", ""},
	{"cc_entrypoint=", ""},
}

// toolNameRE 上游接受的工具名形态。
//
// 不合规的名字（含空格、中文、超长）会让整个请求 400，而不是只忽略那个工具。
var toolNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// PrepareBody 单 pass 改写；无法解析时原样返回。
func PrepareBody(src []byte, r region.Region) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	obj["stream"] = true
	normalizeToolChoice(obj)
	normalizeRoles(obj)
	sanitizeMessages(obj)
	normalizeTools(obj)
	ensureLeadingSystem(obj, r)
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// ModelFromBody 读出请求体里的 model 字段（供 x-model-id 等指纹头使用）。
//
// 解析失败返回空串 —— 指纹头缺失不会让请求失败，但发错值会。
func ModelFromBody(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	return obj.Model
}

// sanitizeMessages 对 system / developer / assistant 三类消息做身份指纹清洗。
//
// **作用域是刻意的**：user 与 tool 消息是用户数据，改写它们等于篡改用户输入。
// 只有「模型自述」类的角色才承载渠道指纹。
func sanitizeMessages(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := mm["role"].(string)
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "system", "developer", "assistant":
		default:
			continue
		}
		if s, ok := mm["content"].(string); ok {
			mm["content"] = sanitizeText(s)
			continue
		}
		// 多模态消息：content 是 [{type:"text",text:"..."}] 数组
		if parts, ok := mm["content"].([]any); ok {
			for _, p := range parts {
				pp, ok := p.(map[string]any)
				if !ok {
					continue
				}
				if t, ok := pp["text"].(string); ok {
					pp["text"] = sanitizeText(t)
				}
			}
		}
	}
}

// sanitizeText 清洗一段文本里的身份指纹。
func sanitizeText(s string) string {
	if s == "" {
		return s
	}
	out := sanitizeIdentity.ReplaceAllString(s, sanitizeIdentityRepl)
	out = sanitizeIdentityTail.ReplaceAllString(out, ".")
	for _, p := range sanitizeBrandPhrases {
		if strings.Contains(strings.ToLower(out), strings.ToLower(p.from)) {
			out = replaceAllFold(out, p.from, p.to)
		}
	}
	// 空文本段会让上游 11151，补一个空格
	if strings.TrimSpace(out) == "" && s != "" {
		return " "
	}
	return out
}

// replaceAllFold 大小写不敏感的全量替换（保留原文其余部分不变）。
func replaceAllFold(s, old, new string) string {
	if old == "" {
		return s
	}
	lowerS, lowerOld := strings.ToLower(s), strings.ToLower(old)
	var b strings.Builder
	i := 0
	for {
		j := strings.Index(lowerS[i:], lowerOld)
		if j < 0 {
			b.WriteString(s[i:])
			break
		}
		b.WriteString(s[i : i+j])
		b.WriteString(new)
		i += j + len(old)
	}
	return b.String()
}

// normalizeTools 清洗工具定义：丢弃不合规的名字、清洗描述、限制数量。
//
// 工具名不合规会让**整个请求** 400（不是忽略那一个工具），
// 所以这里是丢弃而非改名 —— 改名会让模型的 tool_call 对不上号。
func normalizeTools(obj map[string]any) {
	tools, ok := obj["tools"].([]any)
	if !ok {
		return
	}
	kept := make([]any, 0, len(tools))
	for _, t := range tools {
		tt, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tt["function"].(map[string]any)
		if !ok {
			kept = append(kept, tt)
			continue
		}
		name, _ := fn["name"].(string)
		if !toolNameRE.MatchString(name) {
			continue // 不合规 → 丢弃
		}
		if d, ok := fn["description"].(string); ok {
			fn["description"] = sanitizeText(d)
		}
		kept = append(kept, tt)
	}
	if len(kept) > maxTools {
		kept = kept[:maxTools]
	}
	obj["tools"] = kept
	// tool_choice 指向被丢弃的工具时，上游会 400；降级为 auto
	if tc, ok := obj["tool_choice"].(string); ok && tc != "auto" && tc != "required" && tc != "none" {
		found := false
		for _, t := range kept {
			tt, _ := t.(map[string]any)
			fn, _ := tt["function"].(map[string]any)
			if n, _ := fn["name"].(string); n == tc {
				found = true
				break
			}
		}
		if !found {
			obj["tool_choice"] = "auto"
		}
	}
}


// ensureLeadingSystem 保证国际版请求的首条消息是 system。
//
// 实测契约：国际版对首条非 system 的请求返回
//
//	{"code":11128,"msg":"first message is not system prompt"}
//
// 国内版无此要求。缺失时补一条中性 system 消息 —— 这是让「同一份
// OpenAI 请求体在两区都可用」的最小干预。
func ensureLeadingSystem(obj map[string]any, r region.Region) {
	if r != region.Global {
		return
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	if len(msgs) > 0 {
		if first, ok := msgs[0].(map[string]any); ok {
			if role, _ := first["role"].(string); strings.EqualFold(strings.TrimSpace(role), "system") {
				return
			}
		}
	}
	obj["messages"] = append([]any{
		map[string]any{"role": "system", "content": defaultSystemPrompt},
	}, msgs...)
}

// normalizeRoles 将 OpenAI 的 developer 角色改写为 system。
//
// 上游对 role=developer 的消息一律命中内容过滤（finish_reason=content_filter，
// 返回「检测到敏感内容」），而相同内容用 system 角色则完全正常。
func normalizeRoles(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if r, _ := mm["role"].(string); r == "developer" {
			mm["role"] = "system"
		}
	}
}

// normalizeToolChoice 按上游 Go struct（string 类型）改写 OpenAI tool_choice。
//
//   - "none"                                      → 删 tool_choice + 删 tools/functions
//   - {"type":"none"}                             → 同上
//   - {"type":"auto"/"required"}                  → 字符串 "auto"/"required"
//   - {"type":"function","function":{"name":"x"}} → 字符串 "x"
//   - 其他对象 / 非标量                            → 删 tool_choice
func normalizeToolChoice(obj map[string]any) {
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		switch strings.ToLower(strings.TrimSpace(typ)) {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = strings.ToLower(strings.TrimSpace(typ))
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}


