// client.go TRAE SOLO 上游 HTTP 客户端。
//
// 所有 base URL 按**账号所属版本**（auth.Auth.Region）从 region 包查表，
// 国内版与国际版共用同一套路径与请求头构造。
package traework

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

var sessionDeadMarkers = []string{"login", "token 失效", "token invalid", "session", "unauthorized", "401"}

// Classify 按 HTTP 状态码 + body 判定错误类别。
func Classify(status int, body string) provider.ErrKind {
	lower := strings.ToLower(body)
	if strings.Contains(body, `"code":1005`) || (strings.Contains(body, "1005") && strings.Contains(lower, "plan")) {
		return provider.ErrHardCredit
	}
	if status == http.StatusUnauthorized {
		return provider.ErrSessionDead
	}
	switch {
	case status == http.StatusTooManyRequests:
		return provider.ErrSoftRate
	case status == http.StatusNotFound:
		return provider.ErrNotFound
	case status >= 500:
		return provider.ErrServer
	case status >= 400:
		return provider.ErrClient
	}
	return provider.ErrNone
}

// Client TRAE SOLO 上游 HTTP 客户端。
type Client struct {
	HTTP       *http.Client
	StreamHTTP *http.Client

	// Overrides 可选覆盖内置域名表（键 "cn" / "global"）。
	Overrides map[string]region.TraeHosts

	// CheckinRetryDelay 遇到 code=9074 限流后的重试等待；生产默认 8s。
	CheckinRetryDelay time.Duration
}

// New 生产默认值。
func New(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	tr := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: timeout,
	}
	return &Client{
		HTTP:              &http.Client{Timeout: timeout, Transport: tr},
		StreamHTTP:        &http.Client{Transport: tr},
		CheckinRetryDelay: 8 * time.Second,
	}
}

// hosts 返回该账号所属版本的上游域名。
func (c *Client) hosts(a *auth.Auth) region.TraeHosts {
	r := region.CN
	if a != nil {
		r = a.Region
	}
	return region.Trae(r, c.Overrides)
}

func (c *Client) agentBase(a *auth.Auth) string { return c.hosts(a).Agent }
func (c *Client) ugBase(a *auth.Auth) string    { return c.hosts(a).Ug }

// oauthBase 换 token 用的 host：优先账号登录时记录的 ApiHost，否则按 region 表。
func (c *Client) oauthBase(a *auth.Auth) string {
	if a != nil && strings.TrimSpace(a.ApiHost) != "" {
		return strings.TrimSpace(a.ApiHost)
	}
	return c.hosts(a).Ug
}

func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, &provider.Error{Kind: Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	return raw, nil
}

// RefreshToken 刷新 access token；调用方负责 SaveAtomic。
func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	oldRefresh := a.RefreshToken
	log.Printf("traework refresh start uid=%s region=%s", a.UID, a.Region)
	if strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	body, _ := json.Marshal(map[string]any{
		"ClientID":     ClientID,
		"RefreshToken": a.RefreshToken,
		"ClientSecret": "-",
		"UserID":       "",
	})
	req, err := http.NewRequest(http.MethodPost, c.oauthBase(a)+EpExchange, bytes.NewReader(body))
	if err != nil {
		return err
	}
	OAuthHeaders(req)
	data, err := c.doJSON(req)
	if err != nil {
		log.Printf("traework refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	var resp struct {
		Result struct {
			Token               string `json:"Token"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
			RefreshToken        string `json:"RefreshToken"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("exchange parse: %w", err)
	}
	if resp.Result.Token == "" {
		return fmt.Errorf("refresh_failed: no token in response — re-login required")
	}
	a.AccessToken = resp.Result.Token
	if resp.Result.RefreshToken != "" {
		a.RefreshToken = resp.Result.RefreshToken
	}
	switch {
	case resp.Result.TokenExpireAt > 0:
		a.ExpiresAt = normalizeExpiresAt(resp.Result.TokenExpireAt)
	case resp.Result.TokenExpireDuration > 0:
		d := time.Duration(resp.Result.TokenExpireDuration)
		if resp.Result.TokenExpireDuration > 1e9 { // 上游通常是毫秒
			d *= time.Millisecond
		} else {
			d *= time.Second
		}
		a.ExpiresAt = time.Now().Add(d).Unix()
	}
	log.Printf("traework refresh success uid=%s refresh_rotated=%t expires_at=%d",
		a.UID, a.RefreshToken != oldRefresh, a.ExpiresAt)
	return nil
}

// normalizeExpiresAt 把可能为毫秒的时间戳归一为秒。
func normalizeExpiresAt(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

// ChatStream 发起对话并返回 SOLO 事件流（调用方负责 Close）。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	req, err := http.NewRequest(http.MethodPost, c.agentBase(a)+EpChat, bytes.NewReader(PrepareBody(body)))
	if err != nil {
		return nil, 0, nil, err
	}
	SOLOHeaders(req, a, c.hosts(a), true)
	hc := c.HTTP
	if c.StreamHTTP != nil {
		hc = c.StreamHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		log.Printf("traework chat_stream uid=%s: transport error: %v", a.UID, err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		log.Printf("traework chat_stream uid=%s region=%s: upstream %d %s body=%s",
			a.UID, a.Region, resp.StatusCode, Classify(resp.StatusCode, string(raw)), truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

// FetchModels 拉取模型列表。
//
// 上游 llm_utils_chat 强制 stream=true，所有模型均为流式模式；
// 非流式请求由本地 Aggregate() 缓冲事件流后聚合。
func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	body, _ := json.Marshal(map[string]any{
		"function": Function, "config_names": nil, "need_prompt": false,
		"current_config_info": nil, "poly_prompt": true, "mode_type": nil, "agent_type": nil,
	})
	req, err := http.NewRequest(http.MethodPost, c.agentBase(a)+EpModels, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	SOLOHeaders(req, a, c.hosts(a), false)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		ConfigInfoList []modelConfig `json:"config_info_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	out, skipped := pickUserModels(resp.ConfigInfoList)
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	log.Printf("traework models: %d 个可用（过滤 %d 个非用户模型）", len(out), skipped)
	return out, nil
}

// modelConfig 上游 get_detail_param 返回的单条模型配置（只保留用得到的字段）。
type modelConfig struct {
	ConfigName string `json:"config_name"`
	// Usage 是上游给出的**用途判别字段**，实测取值：
	//   chat_completion —— 用户可选用的对话模型
	//   custom_model    —— 第三方代理模型（需额外授权）
	//   summary         —— 内部摘要配置
	Usage string `json:"usage"`
	// Invisible 对应 is_invisible_to_user：官方客户端模型选择器里不显示。
	// 实测被标为 true 的既有内部子 agent（browser_use_subagent /
	// explore_sub_agent_v2 / file_search_agent），也有被「正式版」取代的隐藏别名
	// （glm-5、DeepSeek-V4-Pro）。用指针以区分「字段缺失」与「显式 false」。
	Invisible     *bool `json:"is_invisible_to_user"`
	DisplayConfig struct {
		DisplayName   string `json:"display_name"`
		IsCustomModel bool   `json:"is_custom_model"`
	} `json:"display_config"`
}

// pickUserModels 从上游配置列表里挑出可以暴露给用户的模型，返回 (模型表, 过滤条数)。
//
// 过滤规则三条，都有实测依据：
//
//  1. usage 必须是 chat_completion —— 排除 custom_model（第三方代理，需额外授权）
//     与 summary（内部摘要配置）。
//  2. 必须有真实的显示名 —— 显示名为空或 "-" 的（实测 sagitta / aquila）不是
//     给用户选的模型。
//  3. config_name 不能是内部 agent 配置 —— 依据是上游自己的
//     model_extra_config.v3_sub_agent_model_config_names 明确把
//     explore_sub_agent_v2 / browser_use_subagent 标为子 agent 用的模型配置。
//
// **刻意不按 is_invisible_to_user 过滤**：实测 traework/global 账号的全部
// chat_completion 模型（gpt-5.4 / gpt-5.2，Beta）都被标为 invisible，
// 按它过滤会让该渠道一个模型都不剩，反而退回错误的静态兜底表。
// invisible 只表示「不在客户端选择器里默认展示」，不代表不可调用。
func pickUserModels(list []modelConfig) ([]provider.ModelInfo, int) {
	seen := make(map[string]bool, len(list))
	out := make([]provider.ModelInfo, 0, len(list))
	skipped := 0
	for _, cfg := range list {
		name := strings.TrimSpace(cfg.ConfigName)
		if name == "" || seen[name] {
			continue
		}
		usage := strings.TrimSpace(cfg.Usage)
		display := strings.TrimSpace(cfg.DisplayConfig.DisplayName)
		switch {
		case usage != "" && usage != usageChatCompletion:
			skipped++
		case cfg.DisplayConfig.IsCustomModel || strings.HasPrefix(name, "custom_model_"):
			skipped++
		case display == "" || display == "-":
			log.Printf("traework skip 无显示名: %s (usage=%q)", name, usage)
			skipped++
		case traeInternalModelPat.MatchString(name):
			log.Printf("traework skip 内部 agent 配置: %s", name)
			skipped++
		default:
			seen[name] = true
			out = append(out, provider.ModelInfo{ID: name, Name: display})
		}
	}
	return out, skipped
}

// CheckinStatus 查询签到状态。
func (c *Client) CheckinStatus(a *auth.Auth) (checkedIn bool, credits int64, enable bool, err error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBase(a)+EpCheckinStatus, bytes.NewReader([]byte("{}")))
	if err != nil {
		return false, 0, false, err
	}
	UgHeaders(req, a, c.hosts(a))
	data, err := c.doJSON(req)
	if err != nil {
		log.Printf("traework checkin status failed uid=%s err=%v", a.UID, err)
		return false, 0, false, err
	}
	var resp struct {
		CheckedIn bool   `json:"checked_in"`
		Credits   int64  `json:"credits"`
		Enable    bool   `json:"enable"`
		Code      int    `json:"code"`
		Message   string `json:"message"`
		Msg       string `json:"msg"`
		Success   *bool  `json:"success"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, 0, false, fmt.Errorf("checkin status parse: %w", err)
	}
	if resp.Code != 0 {
		return false, 0, false, fmt.Errorf("checkin status code=%d msg=%s", resp.Code, checkinMsg(resp.Message, resp.Msg))
	}
	if resp.Success != nil && !*resp.Success {
		return false, 0, false, fmt.Errorf("checkin status failed: %s", checkinMsg(resp.Message, resp.Msg))
	}
	log.Printf("traework checkin status uid=%s checked_in=%t credits=%d enable=%t",
		a.UID, resp.CheckedIn, resp.Credits, resp.Enable)
	return resp.CheckedIn, resp.Credits, resp.Enable, nil
}

// CheckinClaim 领取签到奖励；遇到 code=9074 限流延迟重试一次。
func (c *Client) CheckinClaim(a *auth.Auth) error {
	log.Printf("traework checkin claim start uid=%s", a.UID)
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequest(http.MethodPost, c.ugBase(a)+EpCheckinClaim, bytes.NewReader([]byte("{}")))
		if err != nil {
			return err
		}
		UgHeaders(req, a, c.hosts(a))
		data, err := c.doJSON(req)
		if err != nil {
			log.Printf("traework checkin claim failed uid=%s err=%v", a.UID, err)
			return err
		}
		code, msg, success, err := parseCheckinResponse(data)
		if err != nil {
			return err
		}
		if code == 9074 && attempt == 0 {
			if delay := c.CheckinRetryDelay; delay > 0 {
				log.Printf("traework checkin claim rate-limited uid=%s code=9074 retry_after=%s", a.UID, delay)
				time.Sleep(delay)
			}
			continue
		}
		if code != 0 {
			err := fmt.Errorf("checkin claim code=%d msg=%s", code, msg)
			log.Printf("traework checkin claim failed uid=%s err=%v", a.UID, err)
			return err
		}
		if success != nil && !*success {
			err := fmt.Errorf("checkin claim failed: %s", msg)
			log.Printf("traework checkin claim failed uid=%s err=%v", a.UID, err)
			return err
		}
		log.Printf("traework checkin claim response uid=%s code=%d msg=%s", a.UID, code, msg)
		return nil // 9095 等业务无害响应交给后置 status 验证最终状态
	}
	return fmt.Errorf("checkin claim rate limited: code=9074")
}

// DailyCheckin 执行签到：status → claim → 复核 status。
//
// 国际版无签到体系时返回 provider.ErrCheckinUnsupported，
// 调度器据此跳过且不计失败、不进入冷却。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	if !region.CheckinSupported(provider.Trae.String(), a.Region) {
		return provider.ErrCheckinUnsupported
	}
	log.Printf("traework checkin start uid=%s region=%s", a.UID, a.Region)
	checked, _, enable, err := c.CheckinStatus(a)
	if err != nil {
		return err
	}
	if checked {
		return provider.ErrAlreadyCheckedIn
	}
	if !enable {
		return fmt.Errorf("签到未开放")
	}
	if err := c.CheckinClaim(a); err != nil {
		return err
	}
	checked, _, _, err = c.CheckinStatus(a)
	if err != nil {
		return fmt.Errorf("checkin verification: %w", err)
	}
	if !checked {
		return fmt.Errorf("签到复核未通过（checked_in=false）")
	}
	log.Printf("traework checkin verified uid=%s", a.UID)
	return nil
}

func parseCheckinResponse(data []byte) (code int, msg string, success *bool, err error) {
	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
		Success *bool  `json:"success"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, "", nil, fmt.Errorf("checkin response parse: %w", err)
	}
	return resp.Code, checkinMsg(resp.Message, resp.Msg), resp.Success, nil
}

func checkinMsg(message, msg string) string {
	if strings.TrimSpace(message) != "" {
		return strings.TrimSpace(message)
	}
	return strings.TrimSpace(msg)
}

// UserResource 查询可用积分（剩余 = Σ(credits_limit - credits_amount)）。
func (c *Client) UserResource(a *auth.Auth) (int64, error) {
	_, items, err := c.UserResourceDetail(a)
	if err != nil {
		return 0, err
	}
	var remain int64
	for _, it := range items {
		remain += it.Remain
	}
	return remain, nil
}

// packName 取权益包的显示名。
//
// 原来一律用「权益包 N」，面板上 13 行全是「权益包 1…13」，完全看不出是什么包。
// 上游其实给了 display_desc（"老用户福利" / "签到奖励" / "每月登录赠送" / "免费"…），
// 优先用它；再退回 group_name，最后才用序号。
func packName(displayDesc, groupName string, idx int) string {
	if s := strings.TrimSpace(displayDesc); s != "" {
		return s
	}
	if s := strings.TrimSpace(groupName); s != "" {
		return s
	}
	return fmt.Sprintf("权益包 %d", idx)
}

// keepResourceItem 判断一条权益包是否值得展示在积分明细里。
//
// 判据是「有没有剩余」：limit<=0 表示这个包本身没有额度（实测有过一条
// credits_limit=0 的「免费」包），remain<=0 表示已经用完。两者对合计都是
// 零贡献，留在面板上只是噪音 —— 实测 traework/cn 的第二个账号 33 条里
// 有 25 条 remain=0，面板上就是连续 25 行「0 / 200」。
//
// 与 internal/upstream 的同名函数保持同一口径。两个包互不依赖，所以各留一份；
// 改这里时那边也要改。
//
// 判据只看剩余、**不看到期时间**：到期时间已经在明细里单独显示（过期染红），
// 不在这里重复判断，避免同一件事有两个口径。
func keepResourceItem(limit, remain int64) bool {
	return limit > 0 && remain > 0
}

// packExpireAt 取权益包到期时间（Unix 秒），取不到返回 0。
//
// 实测（traework/cn 真实账号，13 条权益包）：expire_time 与
// entitlement_base_info.end_time **都有值且恒等**（如 1791081100
// = 2026-10-04 10:31:40），所以用前者、后者兜底。
func packExpireAt(expireTime, endTime int64) int64 {
	if expireTime > 0 {
		return expireTime
	}
	if endTime > 0 {
		return endTime
	}
	return 0
}

// UserResourceDetail 查询积分明细（每个权益包一条）。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBase(a)+EpEntUsage, bytes.NewReader([]byte(`{"require_usage":true}`)))
	if err != nil {
		return 0, nil, err
	}
	UgHeaders(req, a, c.hosts(a))
	data, err := c.doJSON(req)
	if err != nil {
		return 0, nil, err
	}
	var resp struct {
		UserEntitlementPackList []struct {
			DisplayDesc string `json:"display_desc"`
			GroupName   string `json:"group_name"`
			ExpireTime  int64  `json:"expire_time"`
			Base        struct {
				EndTime int64 `json:"end_time"`
				Quota   struct {
					CreditsLimit float64 `json:"credits_limit"`
				} `json:"quota"`
			} `json:"entitlement_base_info"`
			Usage struct {
				CreditsAmount float64 `json:"credits_amount"`
			} `json:"usage"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, nil, fmt.Errorf("ent usage parse: %w", err)
	}
	var total int64
	items := make([]provider.ResourceItem, 0, len(resp.UserEntitlementPackList))
	for i, p := range resp.UserEntitlementPackList {
		limit := p.Base.Quota.CreditsLimit
		used := p.Usage.CreditsAmount
		remain := int64(limit - used)
		if remain < 0 {
			remain = 0
		}
		if !keepResourceItem(int64(limit), remain) {
			continue
		}
		total += remain
		items = append(items, provider.ResourceItem{
			Name:     packName(p.DisplayDesc, p.GroupName, i+1),
			Total:    int64(limit),
			Used:     int64(used),
			Remain:   remain,
			ExpireAt: packExpireAt(p.ExpireTime, p.Base.EndTime),
		})
	}
	return total, items, nil
}

// Classify 实现 provider.Upstream。
func (c *Client) Classify(status int, body string) provider.ErrKind { return Classify(status, body) }

// Stream 实现 provider.Upstream（SOLO 事件流 → OpenAI SSE）。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader) error { return Stream(w, r) }

// Aggregate 实现 provider.Upstream（SOLO 事件流聚合）。
func (c *Client) Aggregate(r io.Reader) (map[string]any, error) { return Aggregate(r) }

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
