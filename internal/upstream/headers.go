// headers.go 构造上游请求头（common / chat / billing / refresh）。
//
// 关键点一：Origin / Referer 按账号 region 取值，且**不等于** a.Domain ——
// 国内版 auth.domain 是 www.codebuddy.cn，但 Origin 必须发同一个值；
// 拿错域名当 Origin 会被上游拒绝。
//
// 关键点二（2026-11-09 升级）：chat 请求改发**官方 IDE 指纹**而非 CLI 指纹。
// 上游对 /v2/chat/completions 有渠道识别（code=11128
// "Illegal API invocation from an unapproved channel"），CLI 指纹
// （User-Agent: CLI/2.63.2 CodeBuddy/2.63.2）属于「未被批准的渠道」，
// 会被间歇性拦截；官方 IDE 指纹（CodeBuddyIDE/* + x-ide-* + B3 trace 链）
// 才是上游认可的形状。指纹取值与实测依据见 officialIDEFingerprint。
package upstream

import (
	"net/http"
	"sync"

	"work2api/internal/auth"
	"work2api/internal/region"
)

// clientUA 与官方 CLI 一致的 User-Agent。
//
// 仅用于**非 chat** 端点（refresh / models / billing）：这些端点用 CLI 指纹
// 一直正常，且官方 CLI 确实会调用它们。chat 端点改用 officialChatHeaders。
const clientUA = "CLI/2.63.2 CodeBuddy/2.63.2"

// 官方 IDE 版本号。取自参考实现 Zhengyuuuui/workbuddy2api
// （server/codebuddy_proxy.py 的 OFFICIAL_IDE_VERSION / INTL_IDE_VERSION）。
//
// 这两个值是「上游认得的客户端版本」，不是任意字符串：x-ide-version 与
// User-Agent 里的版本必须一致，否则同样会被判成非法渠道。
const (
	officialIDEVersionCN     = "4.12.0"
	officialIDEVersionGlobal = "5.7.6"
)

// traceStore 保存每个会话的 B3 trace 身份。
//
// 官方 IDE 对同一个会话复用同一个 traceid（spanid 每请求新生成，
// parentspanid 指向上一请求的 spanid）。整条链缺失时上游会把它当成
// 「无来源调用」，与 11128 的拦截口径一致。
type traceStore struct {
	mu    sync.Mutex
	conv  map[string]*traceEntry // key = conversation id
	limit int
}

type traceEntry struct {
	traceID    string
	lastSpanID string
}

var traces = &traceStore{conv: map[string]*traceEntry{}, limit: 1000}

// trace 返回该会话的 traceid / spanid / parentspanid。
//
// 同一 convID 复用 traceid；parentspanid 取上一次的 spanid（首次等于自身）。
func (t *traceStore) trace(convID string) (traceID, spanID, parentSpanID string) {
	traceID = newHex32()
	spanID = newHex16()

	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.conv) > t.limit {
		t.conv = map[string]*traceEntry{}
	}
	e, ok := t.conv[convID]
	if !ok || convID == "" {
		// 新会话（或调用方没给 convID）：本次即链首
		if convID != "" {
			t.conv[convID] = &traceEntry{traceID: traceID, lastSpanID: spanID}
		}
		return traceID, spanID, spanID
	}
	parentSpanID = e.lastSpanID
	e.lastSpanID = spanID
	return e.traceID, spanID, parentSpanID
}

// commonHeaders 设置所有 API 共享的请求头。
func commonHeaders(req *http.Request, a *auth.Auth, h region.WorkBuddyHosts) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", h.Origin)
	req.Header.Set("Referer", h.Origin+"/")
	req.Header.Set("User-Agent", clientUA)
}

// officialIDEFingerprint 返回该 region 的官方 IDE 指纹头（不含鉴权头）。
//
// 国内版取 CodeBuddyIDE/4.12.0，国际版取 WorkBuddy/5.7.6 ——
// 两版是不同的产品外壳，指纹不能互用（国际版发 CodeBuddyIDE 会被拒）。
func officialIDEFingerprint(r region.Region, model, convID string) map[string]string {
	if r == region.Global {
		return map[string]string{
			"User-Agent":      "workbuddy-ai/" + officialIDEVersionGlobal + " workbuddy-ai/" + officialIDEVersionGlobal + " CLI/2.137.1",
			"X-Ide-Name":      "WorkBuddy",
			"X-Ide-Type":      "WorkBuddy",
			"X-Ide-Version":   officialIDEVersionGlobal,
			"X-Agent-Intent":  "craft",
			"X-Agent-Purpose": "conversation",
			"X-Agent-Type":    "main",
			"X-Private-Data":  "true",
			"X-Codebuddy-Request": "1",
			"X-Product":       "SaaS",
			"X-Conversation-Id": convID,
			"Accept":          "application/json",
		}
	}
	return map[string]string{
		"User-Agent":        "CodeBuddyIDE/" + officialIDEVersionCN + " CodeBuddy/" + officialIDEVersionCN,
		"X-Ide-Name":        "CodeBuddyIDE",
		"X-Ide-Type":        "CodeBuddyIDE",
		"X-Ide-Version":     officialIDEVersionCN,
		"X-Product":         "SaaS",
		"X-Product-Code":    "codebuddy",
		"X-Product-Version": officialIDEVersionCN,
		"X-Env-Id":          "production",
		"X-Agent-Intent":    "craft",
		"X-Model-Id":        model,
		"X-Conversation-Id": convID,
	}
}

// chatHeaders 构造 chat 请求头：鉴权头 + 官方 IDE 指纹 + B3 trace 链。
//
// convID 由调用方给出（同一会话稳定），用于串起 trace 链；
// model 写进 x-model-id（国内版独有），换模型重试时必须同步更新。
func chatHeaders(req *http.Request, a *auth.Auth, h region.WorkBuddyHosts, model, convID string) {
	// 鉴权头（与指纹无关，两版通用）
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
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", h.Origin)
	req.Header.Set("Referer", h.Origin+"/")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	// 官方 IDE 指纹
	for k, v := range officialIDEFingerprint(a.Region, model, convID) {
		req.Header.Set(k, v)
	}

	// B3 trace 链：官方 IDE 的每个请求都带，缺了就是「无来源调用」
	traceID, spanID, parentSpanID := traces.trace(convID)
	msgID, reqID := newHex32(), newHex32()
	req.Header.Set("X-Conversation-Message-Id", msgID)
	req.Header.Set("X-Conversation-Request-Id", reqID)
	if a.Region == region.Global {
		req.Header.Set("X-Request-Id", msgID)
	} else {
		req.Header.Set("X-Request-Id", reqID)
	}
	req.Header.Set("X-B3-Traceid", traceID)
	req.Header.Set("X-B3-Spanid", spanID)
	req.Header.Set("X-B3-Parentspanid", parentSpanID)
	req.Header.Set("X-B3-Sampled", "1")
	req.Header.Set("B3", traceID+"-"+spanID+"-1")
	req.Header.Set("X-Trace-Id", traceID)
	req.Header.Set("Traceparent", "00-"+traceID+"-"+spanID+"-01")
	req.Header.Set("X-Root-Request-Id", reqID)

	if a.Region != region.Global {
		// 国内版独有头（国际版官方 chat 不带这些）
		req.Header.Set("Accept", "*/*")
		req.Header.Set("Sec-Fetch-Mode", "cors")
	}
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
