// Package upstream 封装对 WorkBuddy（CodeBuddy）上游的全部 HTTP 调用
// （chat / billing / auth），以及错误分类（驱动 pool 冷却状态机）。
//
// region 维度：所有 base URL 按**账号所属版本**（auth.Auth.Region）选取，
// 国内版与国际版共用同一套代码路径，靠域名表区分。
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"work2api/internal/auth"
	"work2api/internal/provider"
	"work2api/internal/region"
)

// ErrKind 错误分类，pool 据此决定冷却时长。
type ErrKind = provider.ErrKind

const (
	ErrNone        = provider.ErrNone
	ErrHardCredit  = provider.ErrHardCredit
	ErrSoftRate    = provider.ErrSoftRate
	ErrSessionDead = provider.ErrSessionDead
	ErrNotFound    = provider.ErrNotFound
	ErrServer      = provider.ErrServer
	ErrClient      = provider.ErrClient
)

// Error 带分类的上游错误。
type Error = provider.Error

// hardMarkers 余额不足关键词（小写比较 + 中文原文比较双通道）。
var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

var sessionDeadMarkers = []string{"Offline user session not found", "12153"}

// Classify 按 HTTP 状态码 + body 判定错误类别。
func Classify(status int, body string) ErrKind {
	if status == http.StatusPaymentRequired {
		return ErrHardCredit
	}
	lower := strings.ToLower(body)
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrHardCredit
		}
	}
	for _, m := range sessionDeadMarkers {
		if strings.Contains(body, m) {
			return ErrSessionDead
		}
	}
	switch {
	case status == http.StatusTooManyRequests:
		return ErrSoftRate
	case status == http.StatusNotFound:
		return ErrNotFound
	case status >= 500:
		return ErrServer
	case status >= 400:
		return ErrClient
	}
	return ErrNone
}

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Client WorkBuddy 上游 HTTP 客户端。
type Client struct {
	HTTP *http.Client
	// BillingHTTP 供账单 / 签到接口使用（短超时，避免面板操作长时间假死）。
	// 为 nil 时回退到 HTTP。
	BillingHTTP *http.Client

	// Overrides 可选覆盖内置域名表（键 "cn" / "global"）。
	Overrides map[string]region.WorkBuddyHosts
}

// New 生产默认值。配置连接池减少 TLS 握手。
func New(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Client{
		HTTP:        &http.Client{Timeout: timeout, Transport: tr},
		BillingHTTP: &http.Client{Timeout: 30 * time.Second, Transport: tr},
	}
}

func (c *Client) billingClient() *http.Client {
	if c.BillingHTTP != nil {
		return c.BillingHTTP
	}
	return c.HTTP
}

// hosts 返回该账号所属版本的上游域名。
func (c *Client) hosts(a *auth.Auth) region.WorkBuddyHosts {
	r := region.CN
	if a != nil {
		r = a.Region
	}
	return region.WorkBuddy(r, c.Overrides)
}

func (c *Client) chatBase(a *auth.Auth) string    { return c.hosts(a).ChatBase }
func (c *Client) billingBase(a *auth.Auth) string { return c.hosts(a).BillingBase }

func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	return c.doJSONWith(c.HTTP, req)
}

func (c *Client) doJSONBilling(req *http.Request) (json.RawMessage, error) {
	return c.doJSONWith(c.billingClient(), req)
}

func (c *Client) doJSONWith(client *http.Client, req *http.Request) (json.RawMessage, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, &Error{Kind: Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return env.Data, nil
}

// RefreshToken 刷新 access token；成功时更新 a 的字段（缺省值保留旧值），
// 调用方负责 SaveAtomic。全程持 a 锁，防止并发 SaveAtomic 读到半更新 token。
func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	oldRefresh := a.RefreshToken
	log.Printf("workbuddy refresh start uid=%s region=%s", a.UID, a.Region)
	if strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	req, err := http.NewRequest(http.MethodPost, c.chatBase(a)+"/v2/plugin/auth/token/refresh", nil)
	if err != nil {
		return err
	}
	refreshHeaders(req, a, c.hosts(a))
	data, err := c.doJSON(req)
	if err != nil {
		log.Printf("workbuddy refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("refresh_failed: no accessToken in response — re-login required")
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		a.Domain = tok.Domain
	}
	// 响应缺 expiresIn 时保留旧过期时间，避免刷新风暴。
	if tok.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	log.Printf("workbuddy refresh success uid=%s refresh_rotated=%t expires_at=%d",
		a.UID, a.RefreshToken != oldRefresh, a.ExpiresAt)
	return nil
}

// ChatStream 发起对话并返回原始 SSE body 流（调用方负责 Close）。
//
// 非 2xx 时 rc 为 nil、respBody 为上游响应体、err 为 nil
// （供调用方 Classify(status, string(body)) 后重试别的账号）；
// 只有传输层失败才返回 err。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	req, err := http.NewRequest(http.MethodPost, c.chatBase(a)+"/v2/chat/completions", bytes.NewReader(PrepareBody(body, a.Region)))
	if err != nil {
		return nil, 0, nil, err
	}
	chatHeaders(req, a, c.hosts(a))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		log.Printf("chat_stream uid=%s: transport error: %v", a.UID, err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		log.Printf("chat_stream uid=%s region=%s: upstream %d %s body=%s",
			a.UID, a.Region, resp.StatusCode, Classify(resp.StatusCode, string(raw)), truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

// FetchModels 调上游动态模型接口。
// 字段名与上游实际返回对齐：maxInputTokens（非 contextWindow）、maxOutputTokens（非 maxTokens）。
func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	h := c.hosts(a)
	req, err := http.NewRequest(http.MethodGet, h.ChatBase+"/console/enterprises/personal/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", h.Origin)
	req.Header.Set("Referer", h.Origin+"/")
	req.Header.Set("User-Agent", clientUA)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models api status %d%s", resp.StatusCode, briefBody(raw))
	}
	type modelEntry struct {
		ID              string `json:"id"`
		Name            string `json:"name"`
		MaxInputTokens  int64  `json:"maxInputTokens"`
		MaxOutputTokens int64  `json:"maxOutputTokens"`
		Disabled        bool   `json:"disabled"`
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []modelEntry `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("models api code=%d", env.Code)
	}
	var cliIDs []string
	for _, ag := range env.Data.Agents {
		if ag.Name == "cli" {
			cliIDs = ag.Models
			break
		}
	}
	if len(cliIDs) == 0 {
		return nil, fmt.Errorf("no cli agent models found")
	}
	dynMap := make(map[string]modelEntry, len(env.Data.Models))
	for _, m := range env.Data.Models {
		dynMap[m.ID] = m
	}
	out := make([]provider.ModelInfo, 0, len(cliIDs))
	for _, id := range cliIDs {
		m, ok := dynMap[id]
		if !ok || m.Disabled {
			continue
		}
		out = append(out, provider.ModelInfo{
			ID:            m.ID,
			Name:          m.Name,
			ContextWindow: m.MaxInputTokens,
			MaxTokens:     m.MaxOutputTokens,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	return out, nil
}

// resourceBody 构造积分查询请求体。
func resourceBody() []byte {
	now := time.Now()
	raw, _ := json.Marshal(map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	})
	return raw
}

type resourceAccount struct {
	PackageName         string `json:"PackageName"`
	CapacitySize        int64  `json:"CapacitySize"`
	CapacityRemain      int64  `json:"CapacityRemain"`
	CapacityUsed        int64  `json:"CapacityUsed"`
	CycleCapacitySize   int64  `json:"CycleCapacitySize"`
	CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
	CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
	// ---- 到期相关（实测结论见 expireAt）----
	DeductionEndTime int64  `json:"DeductionEndTime"` // UTC 毫秒时间戳，恒有值
	CycleEndTime     string `json:"CycleEndTime"`     // "2006-01-02 15:04:05"，北京时间
	ExpiredTime      string `json:"ExpiredTime"`      // 时有时无；有值即为已过期
}

type resourceResp struct {
	Response struct {
		Data struct {
			Accounts []resourceAccount `json:"Accounts"`
		} `json:"Data"`
	} `json:"Response"`
}

// fetchResource 拉取积分明细原始数据。
func (c *Client) fetchResource(a *auth.Auth) ([]resourceAccount, error) {
	req, err := http.NewRequest(http.MethodPost, c.billingBase(a)+"/v2/billing/meter/get-user-resource", bytes.NewReader(resourceBody()))
	if err != nil {
		return nil, err
	}
	billingHeaders(req, a)
	data, err := c.doJSONBilling(req)
	if err != nil {
		return nil, err
	}
	var resp resourceResp
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("resource parse: %w", err)
	}
	return resp.Response.Data.Accounts, nil
}

// pickRemain 按套餐字段形态挑出可用余额。
func pickRemain(acct resourceAccount) (total, used, remain int64) {
	switch {
	case acct.CycleCapacitySize > 0:
		total, used, remain = acct.CycleCapacitySize, acct.CycleCapacityUsed, acct.CycleCapacityRemain
	case acct.CycleCapacityRemain > 0 || acct.CycleCapacityUsed > 0:
		total, used, remain = acct.CycleCapacityRemain+acct.CycleCapacityUsed, acct.CycleCapacityUsed, acct.CycleCapacityRemain
	default:
		total, used, remain = acct.CapacitySize, acct.CapacityUsed, acct.CapacityRemain
	}
	if remain < 0 {
		remain = 0
	}
	return total, used, remain
}

// UserResource 查询账号当前可花费积分余额（所有套餐聚合，负值钳 0）。
func (c *Client) UserResource(a *auth.Auth) (int64, error) {
	accounts, err := c.fetchResource(a)
	if err != nil {
		return 0, err
	}
	var remain int64
	for _, acct := range accounts {
		_, _, r := pickRemain(acct)
		remain += r
	}
	return remain, nil
}

// cstZone 上游 CycleEndTime 用的是北京时间（UTC+8）。
var cstZone = time.FixedZone("CST", 8*3600)

// expireAt 取权益包的到期时间（Unix 秒），取不到返回 0。
//
// 实测依据（workbuddy/cn 真实账号，get-user-resource 返回 24 条权益包）：
//
//	DeductionEndTime  毫秒时间戳，24/24 条都有值
//	CycleEndTime      "2006-01-02 15:04:05" 北京时间字符串，24/24 条都有值
//	ExpiredTime       时有时无；有值的那些恰好都是已过期（剩余为 0）的包
//
// 且同一包的两个字段**换算后完全相等**，例如
//
//	DeductionEndTime = 1792195250000 (ms) → 1792195250 s → 2026-10-17T00:00:50Z
//	CycleEndTime     = "2026-10-17 08:00:50" (CST) → 同一时刻
//
// 所以以 DeductionEndTime 为准（纯数值、无时区解析风险），
// 缺失时才回退解析 CycleEndTime；两者都没有返回 0，面板显示「未知」而不是猜一个。
func expireAt(acct resourceAccount) int64 {
	if acct.DeductionEndTime > 0 {
		return acct.DeductionEndTime / 1000
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", acct.CycleEndTime, cstZone); err == nil {
		return t.Unix()
	}
	return 0
}

// UserResourceDetail 查询账号积分明细（所有套餐条目）。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	accounts, err := c.fetchResource(a)
	if err != nil {
		return 0, nil, err
	}
	var total int64
	items := make([]provider.ResourceItem, 0, len(accounts))
	for _, acct := range accounts {
		t, used, remain := pickRemain(acct)
		// 额度为 0 的权益包跳过：它对合计没有贡献（total 为 0 时 remain 必为 0），
		// 留在列表里只是噪音。实测 workbuddy/cn 当前没有这种条目，
		// 但 traework/cn 有，两个渠道口径保持一致。
		if t <= 0 {
			continue
		}
		total += remain
		items = append(items, provider.ResourceItem{
			Name: acct.PackageName, Total: t, Used: used, Remain: remain,
			ExpireAt: expireAt(acct),
		})
	}
	return total, items, nil
}

// DailyCheckin 执行每日签到。
//
// **国际版没有签到体系**，直接返回 provider.ErrCheckinUnsupported，
// 调用方（scheduler / 面板）据此跳过，不计失败、不进入冷却、不查积分。
//
// 今日已签到时返回 provider.ErrAlreadyCheckedIn —— 同样是正常状态，
// 不是失败。实测国内版把「今天已签到」表达成 HTTP 400 + code=10001。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	if !region.CheckinSupported(provider.WorkBuddy.String(), a.Region) {
		return provider.ErrCheckinUnsupported
	}
	log.Printf("workbuddy checkin start uid=%s region=%s", a.UID, a.Region)
	req, err := http.NewRequest(http.MethodPost, c.billingBase(a)+"/v2/billing/meter/daily-checkin", bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	billingHeaders(req, a)
	if _, err := c.doJSONBilling(req); err != nil {
		if isAlreadyCheckedIn(err) {
			log.Printf("workbuddy checkin already-done uid=%s", a.UID)
			return provider.ErrAlreadyCheckedIn
		}
		log.Printf("workbuddy checkin failed uid=%s err=%v", a.UID, err)
		return err
	}
	log.Printf("workbuddy checkin success uid=%s", a.UID)
	return nil
}

// checkinDoneCodes 上游「今日已签到」的业务码。
//
// 10001 为实测命中值（2026-09-17，国内版 daily-checkin 返回
// HTTP 400 + {"code":10001,"msg":"今天已签到，请明天再来"}）。
// 10002/10003 来自抓包记录，一并视为已签到。
var checkinDoneCodes = []string{`"code":10001`, `"code":10002`, `"code":10003`}

// checkinDoneMarkers 兜底文案匹配（上游换码但文案未变时仍能识别）。
var checkinDoneMarkers = []string{"已签到", "already checked", "already signed"}

// isAlreadyCheckedIn 判定错误是否只是「今天已签到」。
func isAlreadyCheckedIn(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, code := range checkinDoneCodes {
		if strings.Contains(msg, code) {
			return true
		}
	}
	lower := strings.ToLower(msg)
	for _, m := range checkinDoneMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// Classify 实现 provider.Upstream。
func (c *Client) Classify(status int, body string) provider.ErrKind { return Classify(status, body) }

// Stream 实现 provider.Upstream（上游已是 OpenAI SSE，直接透传）。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader) error { return Stream(w, r) }

// Aggregate 实现 provider.Upstream。
func (c *Client) Aggregate(r io.Reader) (map[string]any, error) { return Aggregate(r) }

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// briefBody 把上游错误响应压成一句可读的说明。
//
// 网关/反代异常时返回的是整页 HTML，直接塞进 error 里会把面板刷满标签 ——
// 实测 /console/enterprises/personal/models 对国际版账号就返回 500 + HTML。
func briefBody(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "<") {
		return "（上游返回 HTML 而非 JSON，通常是网关错误）"
	}
	return ": " + truncate(s, 120)
}
