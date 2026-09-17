package provider

// WorkBuddy 静态模型表。
//
// 来源（实测）：
//   - CN:     E:\JJWork\DEV\WorkBuddy\resources\app.asar.unpacked\cli\product.json（客户端 5.5.6）
//   - Global: E:\Program Files\WorkBuddyAI\resources\app.asar.unpacked\cli\product.json（客户端 5.5.2）
//
// 只保留可用于 chat completions 的模型 —— 已剔除纯图像 / 视频 / 代码补全类
// （这些在 product.json 里 maxInputTokens = 0，如 hunyuan-image-*、kling-v3-*、codewise-*）。
//
// 运行时若上游可拉取（FetchModels 的 /console/enterprises/personal/models），
// 以动态结果为准，本表仅作兜底。
//
// # cn 表实测结论（逐模型真实调用，非推断）
//
// 37 个原始条目逐个发最小 chat 请求，并与上游实时列表（16 个）交叉对照：
//
//	可用 且在上游实时表 .... 10 个（保留）
//	可用 但不在实时表 ...... 14 个（保留）—— 模型存在，只是该账号无权限
//	不存在 11102 ........... 13 个（剔除）
//
// 剔除的 13 个（product.json 里有、服务端 service info 未注册）：
//
//	deepseek-v3-1-volc  deepseek-v3-1  deepseek-r1-0528  minimax-m2.5
//	glm-5.0  glm-4.7  glm-4.6  glm-4.6v  kimi-k2-thinking
//	kimi-k2-instruct-taiji  completion-gf  default-1.1  default-1.2
//
// 「不在实时表」**不能**作为剔除依据 —— 那只是当前账号的权限视图。
// 实测有 14 个模型该账号调不通、但服务端确实注册着（换个账号就可能有权限），
// 全部保留。这与「宁可多暴露也不要少暴露」的原则一致。
//
// 注意：黑名单**必须按 region 独立**，不能跨区共用 —— 实测同一个 ID 在两区
// 的注册状态可以完全相反：
//
//	deepseek-v3-2-volc  在 cn 可用，在 global 不存在（11102）
//	glm-5.0             在 cn 不存在（11102），在 global 存在（仅 429）
//
// # global 表实测结论（逐模型真实调用，非推断）
//
// global 的模型接口**稳定返回 500**（详见 upstream.briefBody 注释），因此这张表
// 实际就是该渠道对外的模型清单 —— 列一个调不通的模型，比列表短更糟。
// 对 region=global 的真实账号逐个发最小 chat 请求，20 个原始条目里：
//
//	可用 200 ................ 11 个（已全部保留）
//	不存在 11102 ............  8 个（已全部剔除）
//	模型特有 429 ............  1 个（glm-5.0，保留）
//
// 被剔除的 8 个（product.json 里有、服务端 service info 未注册）：
//
//	default-model-lite   → 别名指向 codewise-default-cw-api-3（不存在）
//	gpt-5.1-codex        gpt-5.1-codex-mini
//	gemini-3.1-flash-lite  gemini-3.0-flash
//	gemini-2.5-pro       → 别名指向 gemini-2.5-pro-us-central1（不存在）
//	gemini-2.5-flash     → 别名指向 gemini-2.5-flash-us-central1（不存在）
//	deepseek-v3-2-volc
//
// glm-5.0 保留：它返回的是 429 而非 11102，且**同一时刻**同账号的其他模型返回 200
// （已做对照），故属于「存在但该模型单独限流/无配额」，不是账号级限流、也不是不存在。
var workBuddyStaticModels = map[Channel][]ModelInfo{
	{Kind: WorkBuddy, Region: "cn"}: {
		// 本表经过**逐模型实测**筛选（见文件头「cn 表实测结论」）：
		// 剔除 13 个服务端未注册（11102）的模型；保留 24 个，
		// 其中 14 个虽不在当前账号的上游实时列表里，但实测可调用（换账号可能有权限）。
		{ID: "default", Name: "Default", ContextWindow: 200000, MaxTokens: 24000},
		{ID: "auto", Name: "Auto", ContextWindow: 168000, MaxTokens: 32000},
		{ID: "deepseek-v4-pro", Name: "Deepseek-V4-Pro", ContextWindow: 1000000, MaxTokens: 50000},
		{ID: "deepseek-v4-flash", Name: "Deepseek-V4-Flash", ContextWindow: 1000000, MaxTokens: 50000},
		{ID: "deepseek-v3-2-volc", Name: "DeepSeek-V3.2", ContextWindow: 96000, MaxTokens: 32000},
		{ID: "deepseek-v3-1-lkeap", Name: "DeepSeek-V3-1", ContextWindow: 96000, MaxTokens: 32000},
		{ID: "deepseek-v3-0324-lkeap", Name: "DeepSeek-V3-0324", ContextWindow: 112000, MaxTokens: 16000},
		{ID: "deepseek-v3-0324", Name: "deepseek-v3", ContextWindow: 96000, MaxTokens: 8192},
		{ID: "deepseek-r1-0528-lkeap", Name: "DeepSeek-R1-0528", ContextWindow: 96000, MaxTokens: 16000},
		{ID: "minimax-m2.7", Name: "MiniMax-M2.7", ContextWindow: 200000, MaxTokens: 48000},
		{ID: "minimax-m3", Name: "MiniMax-M3", ContextWindow: 512000, MaxTokens: 128000},
		{ID: "glm-5.2", Name: "GLM-5.2", ContextWindow: 1000000, MaxTokens: 48000},
		{ID: "glm-5.1", Name: "GLM-5.1", ContextWindow: 200000, MaxTokens: 48000},
		{ID: "glm-5.0-turbo", Name: "GLM-5.0-Turbo", ContextWindow: 200000, MaxTokens: 48000},
		{ID: "glm-5v-turbo", Name: "GLM-5v-Turbo", ContextWindow: 200000, MaxTokens: 38000},
		{ID: "kimi-k3-1", Name: "Kimi-K3", ContextWindow: 1000000, MaxTokens: 32000},
		{ID: "kimi-k2.7", Name: "Kimi-K2.7-Code", ContextWindow: 256000, MaxTokens: 32000},
		{ID: "kimi-k2.6", Name: "Kimi-K2.6", ContextWindow: 256000, MaxTokens: 32000},
		{ID: "kimi-k2.5", Name: "Kimi-K2.5", ContextWindow: 256000, MaxTokens: 32000},
		{ID: "hy3", Name: "Hy3", ContextWindow: 192000, MaxTokens: 64000},
		{ID: "hy3-preview", Name: "Hy3 preview", ContextWindow: 192000, MaxTokens: 64000},
		{ID: "hunyuan-chat", Name: "Hunyuan-Turbos", ContextWindow: 128000, MaxTokens: 8192},
		{ID: "hunyuan-2.0-thinking", Name: "Hunyuan-2.0-Thinking", ContextWindow: 128000, MaxTokens: 24000},
		{ID: "hunyuan-2.0-instruct", Name: "Hunyuan-2.0-Instruct", ContextWindow: 128000, MaxTokens: 16000},
	},
	// Global 表经过**逐模型实测**筛选（见文件头「global 表实测结论」）：
	// 只保留在该 region 真实账号上返回 200 的模型，以及仅因限流（429）未定论的 glm-5.0。
	{Kind: WorkBuddy, Region: "global"}: {
		{ID: "default-model", Name: "Default", ContextWindow: 176000, MaxTokens: 24000},
		{ID: "fast-model", Name: "Fast", ContextWindow: 200000, MaxTokens: 32000},
		{ID: "balanced-model", Name: "Balanced", ContextWindow: 256000, MaxTokens: 32000},
		{ID: "primary-model", Name: "Primary", ContextWindow: 272000, MaxTokens: 72000},
		{ID: "deep-model", Name: "Deep", ContextWindow: 176000, MaxTokens: 24000},
		{ID: "gpt-5.5", Name: "GPT-5.5", ContextWindow: 1000000, MaxTokens: 72000},
		{ID: "gpt-5.4", Name: "GPT-5.4", ContextWindow: 272000, MaxTokens: 128000},
		{ID: "gpt-5.3-codex", Name: "GPT-5.3-Codex", ContextWindow: 272000, MaxTokens: 128000},
		{ID: "gemini-3.1-pro", Name: "Gemini-3.1-Pro", ContextWindow: 400000, MaxTokens: 64000},
		{ID: "gemini-3.5-flash", Name: "Gemini-3.5-Flash", ContextWindow: 1000000, MaxTokens: 65536},
		{ID: "glm-5.0", Name: "GLM-5.0", ContextWindow: 200000, MaxTokens: 48000},
		{ID: "kimi-k2.5", Name: "Kimi-K2.5", ContextWindow: 164000, MaxTokens: 32000},
	},
}

// TraeStaticModels TRAE SOLO 静态兜底模型表。
// 实际以 /api/ide/v1/get_detail_param 动态拉取为准。
var traeStaticModels = map[Channel][]ModelInfo{
	{Kind: Trae, Region: "cn"}: {
		{ID: "glm-5.2", Name: "GLM-5.2", ContextWindow: 1000000, MaxTokens: 48000},
		{ID: "DeepSeek-V4-Pro", Name: "DeepSeek-V4-Pro", ContextWindow: 1000000, MaxTokens: 50000},
	},
	{Kind: Trae, Region: "global"}: {
		{ID: "glm-5.2", Name: "GLM-5.2", ContextWindow: 1000000, MaxTokens: 48000},
	},
}

// StaticModels 返回某渠道 + 版本的静态兜底模型表。
func StaticModels(ch Channel) []ModelInfo {
	switch ch.Kind {
	case WorkBuddy:
		return workBuddyStaticModels[Channel{Kind: WorkBuddy, Region: ch.Region}]
	case Trae:
		return traeStaticModels[Channel{Kind: Trae, Region: ch.Region}]
	default:
		return nil
	}
}
