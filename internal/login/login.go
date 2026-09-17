// Package login WorkBuddy（CodeBuddy）OAuth 登录。
//
// 无 PKCE：设备流由服务端签发 state，浏览器完成授权后回轮询取 token。
//
// 与参考实现的关键差异：**region 参数化**。参考实现把 CN 的 base URL 与
// Origin 写死在包级常量里（`login/login.go:21-29`），国际版无法登录。
// 此处全部经 region.WorkBuddyHosts 传入。
//
// 实测澄清（两版 product.json）：`auth/state?platform=` 两版**都是 "CLI"**，
// 版本区分靠 **base URL**，不靠 platform 参数。`workbuddy` / `workbuddy-ai`
// 是 authentication.attributes.platform，另一个字段。
package login

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"work2api/internal/auth"
	"work2api/internal/region"
)

const clientUA = "CLI/2.63.2 CodeBuddy/2.63.2"

// ErrPending 登录尚未完成（业务 code 非 0，浏览器还没走完授权）。
var ErrPending = errors.New("login pending")

// Result 登录成功后的凭证与账号信息。
type Result struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
	Domain       string
	UID          string
	EnterpriseID string
	Nickname     string
}

// NewClient 每个登录流程独立的 cookie jar（多账号登录互不串会话）。
func NewClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Timeout: 30 * time.Second, Jar: jar}
}

// stateFile 登录会话状态落盘结构。
//
// 额外记录 region：Poll 时按它选回 base URL，避免调用方重复传参。
type stateFile struct {
	State  string `json:"state"`
	Region string `json:"region"`
}

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// commonHeaders 与官方 CLI 一致的请求头；Origin / Referer 按 region 取。
func commonHeaders(req *http.Request, h region.WorkBuddyHosts) {
	origin := strings.TrimRight(h.Origin, "/")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", clientUA)
}

// authStateURL / loginAccountURL / authTokenURL 三个上游端点。
//
// 版本区分靠这里的 base URL，不靠 platform 参数（两版都是 "CLI"）。
func authStateURL(h region.WorkBuddyHosts) string {
	return strings.TrimRight(h.ChatBase, "/") + "/v2/plugin/auth/state?platform=" + url.QueryEscape(h.Platform)
}

func loginAccountURL(h region.WorkBuddyHosts, state string) string {
	return strings.TrimRight(h.ChatBase, "/") + "/v2/plugin/login/account?state=" + url.QueryEscape(state)
}

func authTokenURL(h region.WorkBuddyHosts, state string) string {
	return strings.TrimRight(h.ChatBase, "/") + "/v2/plugin/auth/token?state=" + url.QueryEscape(state)
}

// doJSON 发请求并解 {code,msg,data} 信封；code!=0 → error。
func doJSON(client *http.Client, method, fullURL string, h region.WorkBuddyHosts, extra func(*http.Request), body io.Reader) (json.RawMessage, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, err
	}
	commonHeaders(req, h)
	if extra != nil {
		extra(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream http %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))
	}
	return env.Data, nil
}

// Start 发起登录流程：POST auth/state 拿 state + 授权 URL，state 落盘后返回 URL。
func Start(client *http.Client, r region.Region, h region.WorkBuddyHosts, statePath string) (string, error) {
	data, err := doJSON(client, http.MethodPost, authStateURL(h), h, nil, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", fmt.Errorf("auth state failed: %w", err)
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		return "", fmt.Errorf("auth state: missing state or authUrl")
	}
	raw, _ := json.Marshal(stateFile{State: st.State, Region: r.String()})
	if dir := filepath.Dir(statePath); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		return "", fmt.Errorf("write state: %w", err)
	}
	return st.AuthURL, nil
}

// ResolveAuthURL 手动跟随登录页重定向链，返回最终 URL。
//
// 上游会把 copilot.tencent.com/login 301 到 {Origin}/login/...，
// 浏览器直接打开最终地址可跳过中间跳转。
func ResolveAuthURL(client *http.Client, h region.WorkBuddyHosts, rawURL string) (string, error) {
	noFollow := &http.Client{
		Timeout:   client.Timeout,
		Jar:       client.Jar,
		Transport: client.Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // 自己读 Location
		},
	}
	current := rawURL
	for hop := 0; hop < 5; hop++ {
		req, err := http.NewRequest(http.MethodGet, current, nil)
		if err != nil {
			return "", err
		}
		commonHeaders(req, h)
		resp, err := noFollow.Do(req)
		if err != nil {
			return "", err
		}
		loc := resp.Header.Get("Location")
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if loc == "" {
			return current, nil
		}
		ref, err := url.Parse(loc)
		if err != nil {
			return "", err
		}
		base, err := url.Parse(current)
		if err != nil {
			return "", err
		}
		current = base.ResolveReference(ref).String()
	}
	return current, nil
}

// Poll 单次轮询登录状态；未完成返回 ErrPending；成功返回凭证并删除 state 文件。
//
// region 从 state 文件读取，因此调用方只需传 hosts 覆盖表（可为 nil）。
func Poll(client *http.Client, overrides map[string]region.WorkBuddyHosts, statePath string) (Result, error) {
	raw, err := os.ReadFile(statePath)
	if err != nil {
		return Result{}, fmt.Errorf("read state: %w", err)
	}
	var ls stateFile
	if err := json.Unmarshal(raw, &ls); err != nil || ls.State == "" {
		return Result{}, fmt.Errorf("parse state: %w", err)
	}
	h := region.WorkBuddy(region.Parse(ls.Region), overrides)

	// auth/token 是权威登录状态端点：pending 时业务 code 非 0（如 "login ing"）
	tokRaw, errTok := doJSON(client, http.MethodGet, authTokenURL(h, ls.State), h, nil, nil)
	if errTok != nil {
		if isPending(errTok) {
			return Result{}, ErrPending
		}
		return Result{}, errTok
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		return Result{}, ErrPending
	}

	// login/account 拿 uid / nickname（需带 Bearer）
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	withBearer := func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	}
	if acctRaw, errAcct := doJSON(client, http.MethodGet, loginAccountURL(h, ls.State), h, withBearer, nil); errAcct == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	}
	_ = os.Remove(statePath)
	return Result{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresIn:    tok.ExpiresIn,
		Domain:       tok.Domain,
		UID:          acct.UID,
		EnterpriseID: acct.EnterpriseID,
		Nickname:     acct.Nickname,
	}, nil
}

// isPending 业务错误码（如 "login ing"）视为尚未完成。
func isPending(err error) bool {
	s := err.Error()
	return (strings.Contains(s, "code=") || strings.Contains(s, "login")) &&
		!strings.Contains(s, "http ") && !strings.Contains(s, "parse")
}

// SaveAuth 落盘到 authDir，返回文件路径。
//
// region 取 domain 后缀判定（**不**由调用方指定），保证文件里的 region
// 与上游签发的 domain 一致 —— 这是「两区账号共存」不被写错的关键。
func SaveAuth(authDir string, res Result) (string, error) {
	if res.UID == "" {
		return "", fmt.Errorf("missing uid in result")
	}
	r := auth.RegionFromDomain(res.Domain)
	expiresAt := int64(0)
	if res.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(res.ExpiresIn) * time.Second).Unix()
	}
	a := &auth.Auth{
		Kind:         auth.KindWorkBuddy,
		Region:       r,
		AccessToken:  res.AccessToken,
		RefreshToken: res.RefreshToken,
		ExpiresAt:    expiresAt,
		Domain:       res.Domain,
		UID:          res.UID,
		EnterpriseID: res.EnterpriseID,
		Nickname:     res.Nickname,
	}
	_ = os.MkdirAll(authDir, 0o755)
	fp := filepath.Join(authDir, auth.FileName(auth.KindWorkBuddy, r, res.UID))
	if err := a.SaveAtomicAs(fp); err != nil {
		return "", err
	}
	return fp, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
