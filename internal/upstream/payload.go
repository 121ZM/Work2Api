// payload.go 改写发往上游的 chat 请求体。
//
// 四处改写是关键不变量，勿删：
//  1. 强制 stream:true —— 上游拒绝非流式请求
//  2. tool_choice 归一化 —— 上游该字段是 string 类型，对象形式会 400 code=11101
//  3. developer → system —— 上游对 role=developer 的消息一律命中内容过滤
//  4. 国际版补首条 system —— 国际版对首条非 system 的请求 400 code=11128
//     （实测：国内版无此要求，同一请求体国内版 200、国际版 400）
package upstream

import (
	"encoding/json"
	"strings"

	"work2api/internal/region"
)

// defaultSystemPrompt 国际版补位用的 system 消息内容。
//
// 上游只要求「首条是 system」，不校验内容；取最中性的表述，
// 避免给用户请求叠加任何倾向性指令。
const defaultSystemPrompt = "You are a helpful assistant."

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
	ensureLeadingSystem(obj, r)
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
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
