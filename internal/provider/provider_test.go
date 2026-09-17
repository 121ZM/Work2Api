package provider

import (
	"testing"

	"work2api/internal/region"
)

// TestModelIDSplitRoundTrip 是最关键的不变量：
// /v1/models 暴露的 ID 必须能被 SplitModel 解析回**同一个渠道**，
// 且剥掉前缀后剩下的就是上游真实模型名。
//
// 这条曾经是 bug：ModelID 用 Channel.String()（"workbuddy/cn"）拼出
// "workbuddy/cn/glm-5.2"，而别名表用的是连字符 "workbuddy-cn"，
// 于是客户端拿 /v1/models 的 ID 直接调用会路由到聚合别名、
// 并把 "cn/glm-5.2" 当成模型名转发上游。
func TestModelIDSplitRoundTrip(t *testing.T) {
	channels := []Channel{
		{Kind: WorkBuddy, Region: region.CN},
		{Kind: WorkBuddy, Region: region.Global},
		{Kind: Trae, Region: region.CN},
		{Kind: Trae, Region: region.Global},
	}
	for _, ch := range channels {
		for _, model := range []string{"glm-5.2", "deepseek-v4-pro", "gpt-5.5"} {
			id := ModelID(ch, model)
			prefix, name, ok := SplitModel(id)
			if !ok {
				t.Fatalf("SplitModel(%q) failed", id)
			}
			got, aggregated, okc := ParseChannel(prefix)
			if !okc {
				t.Fatalf("ParseChannel(%q) failed (from id %q)", prefix, id)
			}
			if aggregated {
				t.Fatalf("id %q resolved to aggregate alias, want concrete channel %s", id, ch)
			}
			if got != ch {
				t.Fatalf("id %q -> channel %s, want %s", id, got, ch)
			}
			if name != model {
				t.Fatalf("id %q -> model %q, want %q", id, name, model)
			}
		}
	}
}

// TestSplitModelLegacySlashForm 兼容形态：kind/region/model 仍能解析。
func TestSplitModelLegacySlashForm(t *testing.T) {
	for _, tc := range []struct {
		in     string
		prefix string
		name   string
	}{
		{"workbuddy/cn/glm-5.2", "workbuddy-cn", "glm-5.2"},
		{"workbuddy/global/gpt-5.5", "workbuddy-global", "gpt-5.5"},
		{"trae/cn/glm-5.2", "trae-cn", "glm-5.2"},
		{"traework/global/glm-5.2", "traework-global", "glm-5.2"},
	} {
		prefix, name, ok := SplitModel(tc.in)
		if !ok {
			t.Fatalf("SplitModel(%q) failed", tc.in)
		}
		if prefix != tc.prefix || name != tc.name {
			t.Fatalf("SplitModel(%q) = (%q,%q), want (%q,%q)", tc.in, prefix, name, tc.prefix, tc.name)
		}
	}
}

// TestSplitModelDoesNotEatUpstreamSlashNames 上游自身带斜杠的模型名不能被误拆。
//
// "openai/gpt-4" 的首段 "openai" 不是已知别名，必须原样当作
// 「前缀 openai + 模型 gpt-4」的普通两段式，由调用方判失败。
func TestSplitModelDoesNotEatUpstreamSlashNames(t *testing.T) {
	prefix, name, ok := SplitModel("openai/gpt-4")
	if !ok {
		t.Fatalf("two-segment name should still split")
	}
	if prefix != "openai" || name != "gpt-4" {
		t.Fatalf("got (%q,%q), want (openai, gpt-4)", prefix, name)
	}
	if _, _, okc := ParseChannel(prefix); okc {
		t.Fatal("openai should not be a known channel alias")
	}

	// 三段但第二段不是合法 region → 拒绝
	if _, _, ok := SplitModel("workbuddy/notaregion/glm-5.2"); ok {
		t.Fatal("three-segment with bad region should be rejected")
	}
	// 三段但首段不是聚合别名 → 拒绝
	if _, _, ok := SplitModel("openai/cn/gpt-4"); ok {
		t.Fatal("three-segment with non-alias head should be rejected")
	}
}

// TestSplitModelRejectsBadInput 空串 / 缺模型名 / 只有前缀。
func TestSplitModelRejectsBadInput(t *testing.T) {
	for _, in := range []string{"", "/", "glm-5.2", "workbuddy-cn/", "workbuddy/", "workbuddy/cn/", "  "} {
		if _, _, ok := SplitModel(in); ok {
			t.Fatalf("SplitModel(%q) should fail", in)
		}
	}
}

// TestChannelPrefixCanonical 规范前缀必须与别名表键一致。
func TestChannelPrefixCanonical(t *testing.T) {
	for _, tc := range []struct {
		ch   Channel
		want string
	}{
		{Channel{Kind: WorkBuddy, Region: region.CN}, "workbuddy-cn"},
		{Channel{Kind: WorkBuddy, Region: region.Global}, "workbuddy-global"},
		{Channel{Kind: Trae, Region: region.CN}, "trae-cn"},
		{Channel{Kind: Trae, Region: region.Global}, "trae-global"},
		{Channel{Kind: WorkBuddy}, "workbuddy"},
		{Channel{Kind: Trae}, "trae"},
	} {
		if got := tc.ch.Prefix(); got != tc.want {
			t.Fatalf("%s.Prefix() = %q, want %q", tc.ch, got, tc.want)
		}
		// 规范前缀必须能被别名表解析
		if _, _, ok := ParseChannel(tc.ch.Prefix()); !ok {
			t.Fatalf("ParseChannel(%q) failed — canonical prefix not in alias table", tc.ch.Prefix())
		}
	}
}

// TestCheckinSupported 验收点 2 的能力位：只有国内版有签到体系。
func TestCheckinSupported(t *testing.T) {
	for _, tc := range []struct {
		ch   Channel
		want bool
	}{
		{Channel{Kind: WorkBuddy, Region: region.CN}, true},
		{Channel{Kind: WorkBuddy, Region: region.Global}, false},
		{Channel{Kind: Trae, Region: region.CN}, true},
		{Channel{Kind: Trae, Region: region.Global}, false},
	} {
		if got := tc.ch.CheckinSupported(); got != tc.want {
			t.Fatalf("%s.CheckinSupported() = %v, want %v", tc.ch, got, tc.want)
		}
	}
}

// TestChannelKeyIncludesRegion 分组键必须含 region，否则两区账号会串池。
func TestChannelKeyIncludesRegion(t *testing.T) {
	cn := Channel{Kind: WorkBuddy, Region: region.CN}
	gl := Channel{Kind: WorkBuddy, Region: region.Global}
	if cn.Key() == gl.Key() {
		t.Fatalf("CN and Global share key %q — pools would collide", cn.Key())
	}
	if cn.String() == gl.String() {
		t.Fatalf("CN and Global share String() %q", cn.String())
	}
}

// TestWorkBuddyGlobalStaticModelsAreCallable 防止「从 product.json 重新生成静态表」
// 把已实测不可用的模型又带回来。
//
// 背景：global 的模型接口稳定返回 500，所以这张静态表就是该渠道对外的真实清单。
// 下面这些 ID 在客户端 product.json 里存在，但服务端没注册对应的 service info
// （真实调用返回 11102），列出去客户端一调就 400。详见 staticmodels.go 的
// 「global 表实测结论」。
func TestWorkBuddyGlobalStaticModelsAreCallable(t *testing.T) {
	gl := Channel{Kind: WorkBuddy, Region: region.Global}
	list := StaticModels(gl)

	have := make(map[string]bool, len(list))
	for _, m := range list {
		have[m.ID] = true
	}

	// 实测返回 11102「model service info not found」，必须不在表里。
	absent := []string{
		"default-model-lite", // 别名指向 codewise-default-cw-api-3
		"gpt-5.1-codex",
		"gpt-5.1-codex-mini",
		"gemini-3.1-flash-lite",
		"gemini-3.0-flash",
		"gemini-2.5-pro",   // 别名指向 gemini-2.5-pro-us-central1
		"gemini-2.5-flash", // 别名指向 gemini-2.5-flash-us-central1
		"deepseek-v3-2-volc",
	}
	for _, id := range absent {
		if have[id] {
			t.Errorf("global 静态表含已实测不存在的模型 %q（调用返回 11102），应剔除", id)
		}
	}

	// 实测返回 200（或模型特有的 429），必须保留。
	present := []string{
		"default-model", "fast-model", "balanced-model", "primary-model",
		"deep-model", "gpt-5.5", "gpt-5.4", "gpt-5.3-codex",
		"gemini-3.1-pro", "gemini-3.5-flash", "kimi-k2.5", "glm-5.0",
	}
	for _, id := range present {
		if !have[id] {
			t.Errorf("global 静态表缺了已实测可用的模型 %q", id)
		}
	}

	if len(list) != 12 {
		t.Errorf("global 静态表 %d 个，期望 12（11 个实测 200 + glm-5.0）", len(list))
	}
}

// TestWorkBuddyCNStaticModelsAreCallable 同上的 cn 版。
//
// 37 个原始条目逐模型实测：10 个可用且在实时表、14 个可用但不在实时表、13 个 11102。
// 这里重点锁住两件事：① 13 个未注册的不能再回来；
// ② **14 个「不在实时表但可用」的不能被误删** —— 那只是当前账号的权限视图，
// 按实时表裁剪静态表会把「换个账号就有权限」的模型一起删掉。
func TestWorkBuddyCNStaticModelsAreCallable(t *testing.T) {
	cn := Channel{Kind: WorkBuddy, Region: region.CN}
	list := StaticModels(cn)

	have := make(map[string]bool, len(list))
	for _, m := range list {
		have[m.ID] = true
	}

	// 实测返回 11102，服务端未注册，必须不在表里。
	absent := []string{
		"deepseek-v3-1-volc", "deepseek-v3-1", "deepseek-r1-0528", "minimax-m2.5",
		"glm-5.0", "glm-4.7", "glm-4.6", "glm-4.6v", "kimi-k2-thinking",
		"kimi-k2-instruct-taiji", "completion-gf", "default-1.1", "default-1.2",
	}
	for _, id := range absent {
		if have[id] {
			t.Errorf("cn 静态表含已实测不存在的模型 %q（调用返回 11102），应剔除", id)
		}
	}

	// 实测可调用，必须保留 —— 含 14 个不在当前账号实时列表里的。
	present := []string{
		"default", "auto", "deepseek-v4-pro", "deepseek-v4-flash",
		"deepseek-v3-2-volc", "deepseek-v3-1-lkeap", "deepseek-v3-0324-lkeap",
		"deepseek-v3-0324", "deepseek-r1-0528-lkeap", "minimax-m2.7", "minimax-m3",
		"glm-5.2", "glm-5.1", "glm-5.0-turbo", "glm-5v-turbo",
		"kimi-k3-1", "kimi-k2.7", "kimi-k2.6", "kimi-k2.5",
		"hy3", "hy3-preview", "hunyuan-chat",
		"hunyuan-2.0-thinking", "hunyuan-2.0-instruct",
	}
	for _, id := range present {
		if !have[id] {
			t.Errorf("cn 静态表缺了已实测可用的模型 %q", id)
		}
	}

	if len(list) != 24 {
		t.Errorf("cn 静态表 %d 个，期望 24（10 在实时表 + 14 仅账号无权限）", len(list))
	}
}

// TestStaticModelRegistryDiffersByRegion 锁住「同一个 ID 在两区的注册状态可以相反」。
//
// 实测：deepseek-v3-2-volc 在 cn 可调用、在 global 返回 11102；
// glm-5.0 反过来（cn 11102、global 仅 429）。所以两区表**不能合并、不能共用黑名单**。
// 如果有人图省事把两张表「统一」了，这个测试会红。
func TestStaticModelRegistryDiffersByRegion(t *testing.T) {
	cn := StaticModels(Channel{Kind: WorkBuddy, Region: region.CN})
	gl := StaticModels(Channel{Kind: WorkBuddy, Region: region.Global})

	has := func(list []ModelInfo, id string) bool {
		for _, m := range list {
			if m.ID == id {
				return true
			}
		}
		return false
	}

	if !has(cn, "deepseek-v3-2-volc") {
		t.Error("deepseek-v3-2-volc 在 cn 实测可用，不该被删")
	}
	if has(gl, "deepseek-v3-2-volc") {
		t.Error("deepseek-v3-2-volc 在 global 实测返回 11102，不该出现在 global 表")
	}
	if has(cn, "glm-5.0") {
		t.Error("glm-5.0 在 cn 实测返回 11102，不该出现在 cn 表")
	}
	if !has(gl, "glm-5.0") {
		t.Error("glm-5.0 在 global 实测存在（仅 429），不该被删")
	}
}
