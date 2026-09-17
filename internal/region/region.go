// Package region 定义各渠道在「国内版 / 国际版」下的上游域名表。
//
// region 是账号级属性（见 internal/auth.Auth.Region），不是全局配置 ——
// 同一进程内可同时挂载 CN 与 Global 账号，按账号所属版本选择域名。
//
// 域名来源（实测）：
//   - WorkBuddy CN:      E:\JJWork\DEV\WorkBuddy\...\cli\product.json
//   - WorkBuddy Global:  E:\Program Files\WorkBuddyAI\...\cli\product.json
//   - TRAE SOLO CN:      internal/traework 常量 + 本机 api.trae.cn 请求日志
//   - TRAE SOLO Global:  %APPDATA%\TRAE SOLO\logs\ 真实请求日志
package region

import "strings"

// Region 版本标识。
type Region string

const (
	CN     Region = "cn"     // 国内版
	Global Region = "global" // 国际版
)

func (r Region) String() string { return string(r) }

// Valid 报告是否为已知版本。
func (r Region) Valid() bool { return r == CN || r == Global }

// Parse 归一化外部输入（大小写、空白、常见别名）。未知值一律按 CN 处理。
//
// 需要「拒绝未知值」的场景请用 ParseStrict —— 静默回退 CN 会把拼写错误
// 变成「写进另一套账号库」，很难发现。
func Parse(s string) Region {
	if r, ok := ParseStrict(s); ok {
		return r
	}
	return CN
}

// ParseStrict 与 Parse 相同，但未知值返回 ok=false。
func ParseStrict(s string) (Region, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "cn", "china", "mainland":
		return CN, true
	case "global", "intl", "international", "ai", "oversea", "overseas":
		return Global, true
	default:
		return CN, false
	}
}

// WorkBuddyHosts WorkBuddy（CodeBuddy）某版本的上游域名。
//
// 三个易混的 "platform" 字段（实测自两版 product.json）：
//
//	Platform  auth/state?platform= 查询参数   → 两版**都是 "CLI"**（顶层 platform）
//	AuthID    客户端账号库 ID                → workbuddy-desktop / workbuddy-desktop-ai
//	Product   authentication.attributes.platform → workbuddy / workbuddy-ai
//
// 也就是说：**auth/state 的版本区分靠 base URL，不靠 platform 参数**。
type WorkBuddyHosts struct {
	ChatBase    string `json:"chat_base"`    // chat / plugin 主端点
	BillingBase string `json:"billing_base"` // 计费 / 积分 / 签到
	Origin      string `json:"origin"`       // Origin / Referer（注意不等于 auth.domain）
	Platform    string `json:"platform"`     // auth/state 的 platform 参数（两版均为 CLI）
	AuthID      string `json:"auth_id"`      // 客户端账号库 ID，对应本机 .info 文件名前缀
	Product     string `json:"product"`      // authentication.attributes.platform
}

// TraeHosts TRAE SOLO 某版本的上游域名。
type TraeHosts struct {
	Agent      string `json:"agent"`       // chat / 模型列表 / 定价
	Ug         string `json:"ug"`          // OAuth / 签到 / 积分
	Console    string `json:"console"`     // 登录授权页
	UserRegion string `json:"user_region"` // UG 接口的 X-User-Region
}

// DefaultWorkBuddy WorkBuddy 内置域名表。
var DefaultWorkBuddy = map[Region]WorkBuddyHosts{
	CN: {
		ChatBase:    "https://copilot.tencent.com",
		BillingBase: "https://www.codebuddy.cn",
		Origin:      "https://www.codebuddy.cn",
		Platform:    "CLI",
		AuthID:      "workbuddy-desktop",
		Product:     "workbuddy",
	},
	Global: {
		ChatBase:    "https://www.workbuddy.ai",
		BillingBase: "https://www.workbuddy.ai",
		Origin:      "https://www.workbuddy.ai",
		Platform:    "CLI",
		AuthID:      "workbuddy-desktop-ai",
		Product:     "workbuddy-ai",
	},
}

// DefaultTrae TRAE SOLO 内置域名表。
//
// Global 的 AgentHost 取自本机日志中 /api/ide/v1 与 /api/remote/v1 的宿主；
// UgHost 取自 /trae/api/v3 与 /cloudide/api/v3 的宿主。
var DefaultTrae = map[Region]TraeHosts{
	CN: {
		Agent:      "https://trae-api-cn.mchost.guru",
		Ug:         "https://api.trae.cn",
		Console:    "https://www.trae.cn",
		UserRegion: "CN",
	},
	Global: {
		Agent:      "https://coresg-normal.trae.ai",
		Ug:         "https://growsg-normal.trae.ai",
		Console:    "https://www.trae.ai",
		UserRegion: "GLOBAL",
	},
}

// WorkBuddy 返回指定版本的 WorkBuddy 域名；overrides 非空项优先。
func WorkBuddy(r Region, overrides map[string]WorkBuddyHosts) WorkBuddyHosts {
	h := DefaultWorkBuddy[normalize(r)]
	if o, ok := overrides[string(normalize(r))]; ok {
		h = mergeWorkBuddy(h, o)
	}
	return h
}

// Trae 返回指定版本的 TRAE 域名；overrides 非空项优先。
func Trae(r Region, overrides map[string]TraeHosts) TraeHosts {
	h := DefaultTrae[normalize(r)]
	if o, ok := overrides[string(normalize(r))]; ok {
		h = mergeTrae(h, o)
	}
	return h
}

// CheckinSupported 报告该渠道 + 版本是否有签到体系。
//
// 只有国内版有积分签到活动；国际版（海外发行版）不提供签到入口。
// 该能力位驱动 scheduler 跳过国际版账号，且不计失败、不进入冷却。
func CheckinSupported(kind string, r Region) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "workbuddy", "traework", "trae":
		return normalize(r) == CN
	default:
		return false
	}
}

func normalize(r Region) Region {
	if r == Global {
		return Global
	}
	return CN
}

func mergeWorkBuddy(base, o WorkBuddyHosts) WorkBuddyHosts {
	if o.ChatBase != "" {
		base.ChatBase = o.ChatBase
	}
	if o.BillingBase != "" {
		base.BillingBase = o.BillingBase
	}
	if o.Origin != "" {
		base.Origin = o.Origin
	}
	if o.Platform != "" {
		base.Platform = o.Platform
	}
	if o.AuthID != "" {
		base.AuthID = o.AuthID
	}
	if o.Product != "" {
		base.Product = o.Product
	}
	return base
}

func mergeTrae(base, o TraeHosts) TraeHosts {
	if o.Agent != "" {
		base.Agent = o.Agent
	}
	if o.Ug != "" {
		base.Ug = o.Ug
	}
	if o.Console != "" {
		base.Console = o.Console
	}
	if o.UserRegion != "" {
		base.UserRegion = o.UserRegion
	}
	return base
}
