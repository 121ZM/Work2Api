package region

import "testing"

// ---- 内置域名表：锁住实测事实，防止被「顺手统一」改坏 ----

// TestWorkBuddyCNBillingDiffersFromChatBase CN 版的 chat 端点与计费/签到端点
// **不是同一个域名**（实测分别发往 copilot.tencent.com 与 www.codebuddy.cn）。
// 看起来「不整洁」，但是真的 —— 别为了对称把它们改成同一个。
func TestWorkBuddyCNBillingDiffersFromChatBase(t *testing.T) {
	h := WorkBuddy(CN, nil)
	if h.ChatBase == h.BillingBase {
		t.Errorf("CN 版 ChatBase 与 BillingBase 不该相同，当前都是 %s", h.ChatBase)
	}
	if h.ChatBase != "https://copilot.tencent.com" {
		t.Errorf("CN ChatBase 实测为 https://copilot.tencent.com，实际 %s", h.ChatBase)
	}
	if h.BillingBase != "https://www.codebuddy.cn" {
		t.Errorf("CN BillingBase 实测为 https://www.codebuddy.cn，实际 %s", h.BillingBase)
	}
}

// TestWorkBuddyGlobalUsesSingleHost Global 版三个域名一致（与 CN 不同）。
func TestWorkBuddyGlobalUsesSingleHost(t *testing.T) {
	h := WorkBuddy(Global, nil)
	if h.ChatBase != h.BillingBase || h.ChatBase != h.Origin {
		t.Errorf("Global 版三个域名实测一致，实际 chat=%s billing=%s origin=%s",
			h.ChatBase, h.BillingBase, h.Origin)
	}
}

// TestPlatformIsCLIForBothRegions auth/state 的 platform 参数两版**都是 CLI**，
// 版本区分靠 base URL —— 按直觉改成不同值会让登录流程报废。
func TestPlatformIsCLIForBothRegions(t *testing.T) {
	for _, r := range []Region{CN, Global} {
		if h := WorkBuddy(r, nil); h.Platform != "CLI" {
			t.Errorf("%s 版 Platform 应为 CLI（实测两版相同），实际 %q", r, h.Platform)
		}
	}
}

// TestAuthIDDiffersByRegion 两版的客户端账号库 ID 不同，对应 .info 文件名前缀。
func TestAuthIDDiffersByRegion(t *testing.T) {
	cn, gl := WorkBuddy(CN, nil), WorkBuddy(Global, nil)
	if cn.AuthID == gl.AuthID {
		t.Errorf("两版 AuthID 不该相同，都是 %s", cn.AuthID)
	}
	if cn.AuthID != "workbuddy-desktop" {
		t.Errorf("CN AuthID 实测为 workbuddy-desktop，实际 %s", cn.AuthID)
	}
	if gl.AuthID != "workbuddy-desktop-ai" {
		t.Errorf("Global AuthID 实测为 workbuddy-desktop-ai，实际 %s", gl.AuthID)
	}
}

// TestTraeHostsDifferByRegion TRAE 两版三个域名都不同。
func TestTraeHostsDifferByRegion(t *testing.T) {
	cn, gl := Trae(CN, nil), Trae(Global, nil)
	if cn.Agent == gl.Agent {
		t.Errorf("两版 Agent 域名不该相同：%s", cn.Agent)
	}
	if cn.Ug == gl.Ug {
		t.Errorf("两版 Ug 域名不该相同：%s", cn.Ug)
	}
	if cn.Console == gl.Console {
		t.Errorf("两版 Console 域名不该相同：%s", cn.Console)
	}
}

// TestTraeUserRegion UG 接口的 X-User-Region 取值两版不同。
func TestTraeUserRegion(t *testing.T) {
	if got := Trae(CN, nil).UserRegion; got != "CN" {
		t.Errorf("CN UserRegion 应为 CN，实际 %q", got)
	}
	if got := Trae(Global, nil).UserRegion; got != "GLOBAL" {
		t.Errorf("Global UserRegion 应为 GLOBAL，实际 %q", got)
	}
}

// ---- CheckinSupported：签到能力位 ----

// TestCheckinSupported 只有「已知渠道 + 国内版」为 true。
//
// 这个能力位驱动 scheduler 跳过国际版账号，且不计失败、不进入冷却 ——
// 若这里返回 true，国际版账号每天都会被记一次失败。
func TestCheckinSupported(t *testing.T) {
	cases := []struct {
		kind string
		r    Region
		want bool
	}{
		{"workbuddy", CN, true},
		{"workbuddy", Global, false},
		{"traework", CN, true},
		{"traework", Global, false},
		{"trae", CN, true},
		{"trae", Global, false},
		{"WorkBuddy", CN, true},
		{"  workbuddy  ", CN, true},
		{"unknown", CN, false},
		{"", CN, false},
		{"", Global, false},
	}
	for _, tc := range cases {
		if got := CheckinSupported(tc.kind, tc.r); got != tc.want {
			t.Errorf("CheckinSupported(%q, %s) = %v，期望 %v", tc.kind, tc.r, got, tc.want)
		}
	}
}

// ---- 覆盖合并 ----

// TestOverridesDoNotBlankDefaults 覆盖里的**空字段不能把默认值抹掉**。
// 这是配置项「留空即用内置表」的实现基础。
func TestOverridesDoNotBlankDefaults(t *testing.T) {
	base := WorkBuddy(CN, nil)
	got := WorkBuddy(CN, map[string]WorkBuddyHosts{
		"cn": {ChatBase: "https://example.test"},
	})
	if got.ChatBase != "https://example.test" {
		t.Errorf("非空覆盖应生效，实际 %s", got.ChatBase)
	}
	if got.BillingBase != base.BillingBase {
		t.Errorf("空字段不该覆盖默认 BillingBase：期望 %s，实际 %s", base.BillingBase, got.BillingBase)
	}
	if got.Origin != base.Origin {
		t.Errorf("空字段不该覆盖默认 Origin：期望 %s，实际 %s", base.Origin, got.Origin)
	}
	if got.AuthID != base.AuthID {
		t.Errorf("空字段不该覆盖默认 AuthID：期望 %s，实际 %s", base.AuthID, got.AuthID)
	}
	if got.Platform != base.Platform {
		t.Errorf("空字段不该覆盖默认 Platform：期望 %s，实际 %s", base.Platform, got.Platform)
	}
}

// TestOverridesOnlyApplyToMatchingRegion 覆盖只对指定 region 生效。
func TestOverridesOnlyApplyToMatchingRegion(t *testing.T) {
	ov := map[string]WorkBuddyHosts{"cn": {ChatBase: "https://cn.test"}}
	if got := WorkBuddy(Global, ov); got.ChatBase == "https://cn.test" {
		t.Error("cn 的覆盖不该影响 global")
	}
	if got := WorkBuddy(CN, ov); got.ChatBase != "https://cn.test" {
		t.Error("cn 的覆盖没生效")
	}
}

// TestTraeOverrides 覆盖合并对 TRAE 同样成立。
func TestTraeOverrides(t *testing.T) {
	base := Trae(CN, nil)
	got := Trae(CN, map[string]TraeHosts{"cn": {Agent: "https://agent.test"}})
	if got.Agent != "https://agent.test" {
		t.Errorf("非空覆盖应生效，实际 %s", got.Agent)
	}
	if got.Ug != base.Ug {
		t.Errorf("空字段不该覆盖默认 Ug：期望 %s，实际 %s", base.Ug, got.Ug)
	}
	if got.UserRegion != base.UserRegion {
		t.Errorf("空字段不该覆盖默认 UserRegion")
	}
}

// ---- 解析与归一化 ----

// TestParseStrictAliases 别名、大小写、空白都要归一。
func TestParseStrictAliases(t *testing.T) {
	yes := map[string]Region{
		"cn": CN, "CN": CN, " China ": CN, "mainland": CN,
		"global": Global, "GLOBAL": Global, "intl": Global,
		"international": Global, "ai": Global,
		"oversea": Global, "overseas": Global,
	}
	for in, want := range yes {
		got, ok := ParseStrict(in)
		if !ok || got != want {
			t.Errorf("ParseStrict(%q) = (%v, %v)，期望 (%v, true)", in, got, ok, want)
		}
	}

	for _, in := range []string{"", "usa", "jp", "cn-", "globall"} {
		if got, ok := ParseStrict(in); ok {
			t.Errorf("ParseStrict(%q) 应返回 ok=false，实际 (%v, true)", in, got)
		}
	}
}

// TestParseFallsBackToCN Parse 对未知值静默回退 CN（文档已说明的刻意行为）。
func TestParseFallsBackToCN(t *testing.T) {
	if got := Parse("typo"); got != CN {
		t.Errorf("Parse 未知值应回退 CN，实际 %v", got)
	}
	if got := Parse("global"); got != Global {
		t.Errorf("Parse 已知值不该被回退，实际 %v", got)
	}
}

// TestNormalizeTreatsUnknownAsCN 记录一个**刻意但危险**的行为：
// 未知 Region 一律按 CN 处理。
//
// 所以所有「写入 / 落盘」路径必须先用 ParseStrict 校验 ——
// 否则一个拼写错误会被静默写进另一套账号库，且很难发现。
func TestNormalizeTreatsUnknownAsCN(t *testing.T) {
	got := WorkBuddy(Region("typo"), nil)
	if got != DefaultWorkBuddy[CN] {
		t.Error("未知 Region 应回退到 CN 域名表（刻意行为；调用方需自行用 ParseStrict 校验）")
	}
}

// TestRegionValid
func TestRegionValid(t *testing.T) {
	if !CN.Valid() || !Global.Valid() {
		t.Error("CN / Global 应有效")
	}
	if Region("").Valid() || Region("typo").Valid() {
		t.Error("未知 Region 不该有效")
	}
	if CN.String() != "cn" || Global.String() != "global" {
		t.Error("String() 应返回原始小写标识")
	}
}
