// Package traework 封装 TRAE SOLO 免费通道上游协议。
//
// region 维度：国内版与国际版共用同一套路径与请求头，**只有域名与
// X-User-Region 取值不同**。国际版域名不是猜的 —— 来自本机
// %APPDATA%\TRAE SOLO\logs\ 的真实请求日志（见 internal/region 注释）。
package traework

import (
	"net/http"
	"regexp"

	"work2api/internal/auth"
	"work2api/internal/region"
)

// 跨版本共享的客户端标识常量。
const (
	ClientID       = "en1oxy7wnw8j9n"
	AppID          = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	IdeVersion     = "0.1.52"
	IdeVersionCode = "20260811"
	DeviceBrand    = "20Y5A002XX"
	OSVersion      = "Windows 10 Pro"
	PluginVersion  = "2.3.73734"
	Function       = "solo_work_lite"

	// DefaultConfigName 请求体未指定模型时的回退模型。
	DefaultConfigName = "glm-5.2"
)

// 上游路径（国内版 / 国际版一致，实测日志确认）。
const (
	EpChat          = "/api/agent/v3/llm_utils_chat"
	EpModels        = "/api/ide/v1/get_detail_param"
	EpExchange      = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	EpUserInfo      = "/cloudide/api/v3/trae/GetUserInfo"
	EpCheckinStatus = "/trae/api/v2/ug/checkin_credits/status"
	EpCheckinClaim  = "/trae/api/v2/ug/checkin_credits/claim"
	EpEntUsage      = "/trae/api/v2/pay/web_user_ent_usage"
	EpModelsPricing = "/api/remote/v1/models"
)

const clientUA = "Trae/" + IdeVersion

// usageChatCompletion 是 get_detail_param 里表示「用户可选用对话模型」的 usage 取值。
// 同字段的其它实测取值：custom_model（第三方代理，需额外授权）、summary（内部摘要配置）。
const usageChatCompletion = "chat_completion"

// traeInternalModelPat 匹配上游返回里「内部 agent 用的模型配置」。
//
// 依据不是猜的：上游 model_extra_config.v3_sub_agent_model_config_names 明确写着
// {"Explore":"explore_sub_agent_v2","browser_use":"browser_use_subagent"}。
// 实测同类命名的还有 computer_use_subagent / file_search_agent。
var traeInternalModelPat = regexp.MustCompile(`(?i)_sub_?agent|_agent$`)

// SOLOHeaders chat / 模型接口请求头。
func SOLOHeaders(req *http.Request, a *auth.Auth, h region.TraeHosts, stream bool) {
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("User-Agent", clientUA)
	at := a.JWT()
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+at)
	req.Header.Set("X-Cloudide-Token", at)
	req.Header.Set("X-Ide-Token", at)
	if a.UID != "" {
		req.Header.Set("X-Uid", a.UID)
	}
	req.Header.Set("X-App-Id", AppID)
	req.Header.Set("X-App-Version", "default")
	req.Header.Set("X-Ide-Version", IdeVersion)
	req.Header.Set("X-Ide-Version-Code", IdeVersionCode)
	req.Header.Set("X-App-Version-Code", IdeVersionCode)
	req.Header.Set("X-Ide-Version-Type", "stable")
	req.Header.Set("X-Device-Type", "windows")
	req.Header.Set("X-OS-Version", OSVersion)
	req.Header.Set("X-Device-Brand", DeviceBrand)
	req.Header.Set("Request-Traffic-Type", "prod")
	if a.MachineID != "" {
		req.Header.Set("X-Machine-Id", a.MachineID)
	}
	if a.DeviceID != "" {
		req.Header.Set("x-device-id", a.DeviceID)
	}
}

// UgHeaders 签到 / 积分接口请求头。
//
// 设备指纹头缺任一环节，上游会以 code=9074 拒绝；X-User-Region 必须与
// 账号所属版本一致（国内版 CN / 国际版 GLOBAL）。
func UgHeaders(req *http.Request, a *auth.Auth, h region.TraeHosts) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+a.JWT())
	req.Header.Set("X-User-Region", h.UserRegion)
	req.Header.Set("x-device-brand", DeviceBrand)
	req.Header.Set("x-device-type", "windows")
	req.Header.Set("x-os-version", OSVersion)
	req.Header.Set("x-app-version", IdeVersion)
	if a.DeviceID != "" {
		req.Header.Set("x-device-id", a.DeviceID) // 小写 key，值须为账号真实注册设备 ID
	}
}

// OAuthHeaders 登录 / 换 token 接口请求头（与 region 无关）。
func OAuthHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
}
