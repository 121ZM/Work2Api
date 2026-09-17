// headers.go 构造四类上游请求头（common / chat / billing / refresh）。
//
// 关键点：Origin / Referer 按账号 region 取值，且**不等于** a.Domain ——
// 国内版 auth.domain 是 www.workbuddy.cn，但 Origin 必须发 www.codebuddy.cn。
// 拿 a.Domain 当 Origin 会被上游拒绝。
package upstream

import (
	"net/http"

	"work2api/internal/auth"
	"work2api/internal/region"
)

// clientUA 与官方 CLI 一致的 User-Agent。
const clientUA = "CLI/2.63.2 CodeBuddy/2.63.2"

// commonHeaders 设置所有 API 共享的请求头。
func commonHeaders(req *http.Request, a *auth.Auth, h region.WorkBuddyHosts) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", h.Origin)
	req.Header.Set("Referer", h.Origin+"/")
	req.Header.Set("User-Agent", clientUA)
}

// chatHeaders 在 common 之上加 chat 专属账号头。
// 缺省字段用 X-No-* 约定（与官方 CLI 一致）。
func chatHeaders(req *http.Request, a *auth.Auth, h region.WorkBuddyHosts) {
	commonHeaders(req, a, h)
	if a.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	// 安全红线：绝不在 chat 请求里携带 X-Refresh-Token。
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	} else {
		req.Header.Set("X-No-Department-Info", "1")
	}
	req.Header.Set("X-Product", "SaaS")
}

// billingHeaders 计费 / 积分 / 签到接口请求头。
func billingHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	}
}

// refreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
func refreshHeaders(req *http.Request, a *auth.Auth, h region.WorkBuddyHosts) {
	commonHeaders(req, a, h)
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	req.Header.Set("X-Auth-Refresh-Source", refreshSource)
}

// refreshSource X-Auth-Refresh-Source 的取值。
//
// 官方客户端（codebuddy.js）发的是 "plugin"，而参考实现发的是 "workbuddy"。
// 两者都被上游接受过，但无法从静态代码断定哪个是权威值 ——
// 见计划第 4 节待确认项 #2，首次刷新若被拒需改这里。
const refreshSource = "workbuddy"
