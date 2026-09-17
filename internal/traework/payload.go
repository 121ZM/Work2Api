// payload.go 把 OpenAI chat/completions 请求体改写为 TRAE SOLO 的
// llm_utils_chat 请求体。
//
// 改写项（实测必要）：
//  1. 强制 stream:true，并注入 function=Function
//  2. content 字符串 → [{type:text,text:...}]
//  3. tool_calls → function_call（SOLO 专属字段名），并剔除 name 为空的项
//  4. tools[].function.parameters 对象 → 字符串
//  5. developer → system，config_name/model 归一
package traework

import (
	"encoding/json"
	"strings"
)

// PrepareBody 单 pass 改写；无法解析时原样返回。
func PrepareBody(src []byte) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	obj["stream"] = true
	obj["function"] = Function

	if msgs, ok := obj["messages"].([]any); ok {
		for _, mi := range msgs {
			m, ok := mi.(map[string]any)
			if !ok {
				continue
			}
			content, present := m["content"]
			role, _ := m["role"].(string)
			// SOLO 上游只认 system/assistant/user/tool，不认 developer。
			if role == "developer" {
				m["role"] = "system"
				role = "system"
			}
			if role == "assistant" {
				normalizeAssistantToolCalls(m)
			}
			if !present || content == nil {
				continue
			}
			if s, ok := content.(string); ok {
				m["content"] = []any{map[string]any{"type": "text", "text": s}}
			}
		}
	}

	model, _ := obj["model"].(string)
	if model = strings.TrimSpace(model); model == "" {
		model = DefaultConfigName
	}
	obj["config_name"] = model
	obj["model"] = model
	normalizeToolChoice(obj)
	normalizeTools(obj)

	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// normalizeAssistantToolCalls 把 assistant 消息的 tool_calls[].function 改成
// SOLO 认的 function_call，并丢掉 name 为空的条目（上游会因此报错）。
func normalizeAssistantToolCalls(m map[string]any) {
	tcs, ok := m["tool_calls"].([]any)
	if !ok {
		return
	}
	kept := make([]any, 0, len(tcs))
	for _, tci := range tcs {
		tc, ok := tci.(map[string]any)
		if !ok {
			continue
		}
		if fn, ok := tc["function"].(map[string]any); ok {
			tc["function_call"] = fn
			delete(tc, "function")
		}
		if fc, ok := tc["function_call"].(map[string]any); ok {
			name, _ := fc["name"].(string)
			if strings.TrimSpace(name) == "" {
				continue
			}
		}
		kept = append(kept, tc)
	}
	if len(kept) == 0 {
		delete(m, "tool_calls")
		return
	}
	m["tool_calls"] = kept
}

// normalizeToolChoice 按 SOLO 上游期望（string 类型）改写 tool_choice。
func normalizeToolChoice(obj map[string]any) {
	suppress := func() { delete(obj, "tools"); delete(obj, "functions") }
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

// normalizeTools 把 tools[].function.parameters 的对象形式序列化为字符串
// （SOLO 上游期望字符串）。
func normalizeTools(obj map[string]any) {
	raw, present := obj["tools"]
	if !present {
		return
	}
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return
	}
	out := make([]any, 0, len(list))
	for _, item := range list {
		t, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := t["function"].(map[string]any)
		if !ok {
			continue
		}
		if params, ok := fn["parameters"]; ok {
			if paramsMap, isMap := params.(map[string]any); isMap {
				if s, err := json.Marshal(paramsMap); err == nil {
					fn["parameters"] = string(s)
				}
			}
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		delete(obj, "tools")
		return
	}
	obj["tools"] = out
}
