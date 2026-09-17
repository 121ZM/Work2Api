// Package login_trae TRAE SOLO OAuth 登录（PKCE S256 + 本地一次性回调）。
//
// 与参考实现的差异：**region 参数化**。参考实现把 ConsoleHost 写死为
// www.trae.cn（`login_trae/login.go:118`），国际版无法登录。
// 此处登录页 host 由 region.TraeHosts.Console 传入。
//
// 本地导入不可行（实测）：客户端 storage.json 里的 `iCubeAuthInfo://*` 是
// TLV 二进制（base64 解码后以 dGMFEAAA 开头），非明文 JWT —— TRAE 只能走 OAuth。
package login_trae

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"work2api/internal/auth"
	"work2api/internal/region"
	"work2api/internal/traework"
)

// ErrPending 回调尚未到达。
var ErrPending = errors.New("login pending")

// Result 登录成功后的凭证与设备指纹。
type Result struct {
	Region       region.Region
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
	Domain       string
	ApiHost      string
	MachineID    string
	DeviceID     string
	UID          string
	EnterpriseID string
	Nickname     string
}

// state 登录会话状态（落盘，Poll 时读取）。
type state struct {
	Region       string `json:"region"`
	MachineID    string `json:"machineId"`
	DeviceID     string `json:"deviceId"`
	RefreshToken string `json:"refreshToken,omitempty"`
	AccessToken  string `json:"accessToken,omitempty"`  // userJwt 兜底路径（无 refreshToken 时）
	AuthCode     string `json:"authCode,omitempty"`     // PKCE 新流程：回调 authCodeInfo.AuthCode
	CodeVerifier string `json:"codeVerifier,omitempty"` // 与登录 URL 配对的 PKCE verifier，必须保存
	Host         string `json:"host,omitempty"`
	Err          string `json:"err,omitempty"`
}

// NewClient 登录流程用的 HTTP 客户端。
func NewClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

// Start 启动本地一次性回调监听，返回 TRAE 授权 URL。
//
// 监听 127.0.0.1:0（随机端口），5 分钟无回调自动关闭。
func Start(client *http.Client, r region.Region, h region.TraeHosts, statePath string) (string, error) {
	machineID := randHex(32)    // 真实客户端 64 位 hex（32 字节）
	deviceID := randNumericID() // 真实客户端 15 位数字设备 ID
	codeVerifier, codeChallenge := traework.GenPKCE()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := ln.Addr().String()
	callback := "http://" + addr + "/authorize"
	st := state{Region: r.String(), MachineID: machineID, DeviceID: deviceID, CodeVerifier: codeVerifier}
	if err := writeState(statePath, st); err != nil {
		_ = ln.Close()
		return "", err
	}

	srv := &http.Server{ReadHeaderTimeout: 15 * time.Second}
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// 支持 GET（query）与 POST（JSON/form body）两种回调：
		// 新版授权页登录成功后 redirect=1 走 POST，body 带 refreshToken。
		if req.Method == http.MethodPost {
			if body, _ := io.ReadAll(io.LimitReader(req.Body, 64<<10)); len(body) > 0 {
				var m map[string]any
				if err := json.Unmarshal(body, &m); err == nil {
					q := req.URL.Query()
					for k, v := range m {
						if s, ok := v.(string); ok && q.Get(k) == "" {
							q.Set(k, s)
						}
					}
					req.URL.RawQuery = q.Encode()
				}
			}
		}
		q := req.URL.Query()
		st.Host = q.Get("host")
		if st.Host == "" {
			st.Host = h.Ug
		}
		info := ParseCallback(req.URL.String())
		st.RefreshToken = info.RefreshToken
		st.AccessToken = info.AccessToken
		st.AuthCode = info.AuthCode
		if st.RefreshToken == "" && st.AccessToken == "" && st.AuthCode == "" {
			st.Err = "missing refreshToken in callback"
		}
		_ = writeState(statePath, st)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body style='font-family:sans-serif;padding:24px'>TRAE 登录已完成，可以关闭此页面。</body></html>"))
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		}()
	})
	go func() { _ = srv.Serve(ln) }()
	go func() {
		time.Sleep(5 * time.Minute)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	u, _ := url.Parse(strings.TrimRight(h.Console, "/") + "/authorization")
	v := u.Query()
	v.Set("login_version", "1")
	v.Set("auth_from", "solo")
	v.Set("login_channel", "native_ide")
	v.Set("plugin_version", traework.PluginVersion)
	v.Set("auth_type", "local")
	v.Set("client_id", traework.ClientID)
	v.Set("redirect", "0")
	v.Set("login_trace_id", newUUID())
	v.Set("auth_callback_url", callback)
	v.Set("machine_id", machineID)
	v.Set("device_id", deviceID)
	v.Set("x_device_id", deviceID)
	v.Set("x_machine_id", machineID)
	v.Set("x_device_brand", traework.DeviceBrand)
	v.Set("x_device_type", "windows")
	v.Set("x_os_version", traework.OSVersion)
	v.Set("x_env", "")
	v.Set("x_app_version", traework.IdeVersion)
	v.Set("x_app_type", "stable")
	v.Set("code_challenge", codeChallenge)
	v.Set("code_challenge_method", "S256")
	v.Set("hide_saas_login", "true")
	v.Set("channel_name", "common")
	v.Set("click_id", "TRAE SOLOSetup-stable-"+traework.PluginVersion)
	u.RawQuery = v.Encode()
	_ = client
	return u.String(), nil
}

// Poll 检查回调是否已写入 state，完成后 ExchangeToken + GetUserInfo。
func Poll(client *http.Client, overrides map[string]region.TraeHosts, statePath string) (Result, error) {
	raw, err := os.ReadFile(statePath)
	if err != nil {
		return Result{}, err
	}
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		return Result{}, err
	}
	r := region.Parse(st.Region)
	h := region.Trae(r, overrides)
	if st.Err != "" {
		return Result{}, errors.New(st.Err)
	}
	if st.RefreshToken == "" && st.AccessToken == "" && st.AuthCode == "" {
		return Result{}, ErrPending
	}
	if st.Host == "" {
		st.Host = h.Ug
	}

	c := traework.New(0)
	c.HTTP = client
	a := &auth.Auth{
		Kind:         auth.KindTrae,
		Region:       r,
		RefreshToken: st.RefreshToken,
		AccessToken:  st.AccessToken,
		ApiHost:      st.Host,
		MachineID:    st.MachineID,
		DeviceID:     st.DeviceID,
		Domain:       domainFor(r),
	}
	switch {
	case st.AuthCode != "":
		// PKCE 新流程：AuthCode + codeVerifier + 设备公钥交换
		res, err := c.ExchangeAuthCode(a, st.AuthCode, st.CodeVerifier)
		if err != nil {
			return Result{}, err
		}
		a.AccessToken = res.AccessToken
		a.RefreshToken = res.RefreshToken
		a.ExpiresAt = res.ExpiresAt
		if res.Host != "" {
			a.ApiHost = res.Host
			st.Host = res.Host
		}
	case st.RefreshToken != "":
		// 标准路径：ExchangeToken 换 access token
		if err := c.RefreshToken(a); err != nil {
			return Result{}, err
		}
	default:
		// 兜底路径：回调只给了 userJwt.Token，无 refreshToken（本轮可用，续期会失败）
		if a.AccessToken == "" {
			return Result{}, errors.New("no token in callback")
		}
	}

	uid, nick, ent, err := c.GetUserInfo(a)
	if err != nil {
		return Result{}, err
	}
	_ = os.Remove(statePath)
	return Result{
		Region: r, AccessToken: a.AccessToken, RefreshToken: a.RefreshToken, ExpiresAt: a.ExpiresAt,
		Domain: domainFor(r), ApiHost: st.Host, MachineID: st.MachineID, DeviceID: st.DeviceID,
		UID: uid, EnterpriseID: ent, Nickname: nick,
	}, nil
}

// domainFor 返回该版本 TRAE 的规范 domain。
//
// 用固定值而非回调 host：回调 host 是内部域名（coresg-normal.trae.ai），
// 落盘成 domain 会让 region 判定依赖子串巧合，不如显式给定。
func domainFor(r region.Region) string {
	if r == region.Global {
		return "trae.ai"
	}
	return "trae.cn"
}

// SaveAuth 落盘到 authDir，返回文件路径。
func SaveAuth(authDir string, res Result) (string, error) {
	if res.UID == "" {
		return "", fmt.Errorf("missing uid in result")
	}
	r := res.Region
	if !r.Valid() {
		r = auth.RegionFromDomain(res.Domain)
	}
	a := &auth.Auth{
		Kind:         auth.KindTrae,
		Region:       r,
		AccessToken:  res.AccessToken,
		RefreshToken: res.RefreshToken,
		ExpiresAt:    res.ExpiresAt,
		Domain:       res.Domain,
		ApiHost:      res.ApiHost,
		MachineID:    res.MachineID,
		DeviceID:     res.DeviceID,
		UID:          res.UID,
		EnterpriseID: res.EnterpriseID,
		Nickname:     res.Nickname,
	}
	_ = os.MkdirAll(authDir, 0o755)
	fp := filepath.Join(authDir, auth.FileName(auth.KindTrae, r, res.UID))
	if err := a.SaveAtomicAs(fp); err != nil {
		return "", err
	}
	return fp, nil
}

func writeState(path string, st state) error {
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	raw, _ := json.MarshalIndent(st, "", "  ")
	return os.WriteFile(path, raw, 0o600)
}

// CallbackInfo 回调解析结果（登录流程内部使用，不外泄）。
type CallbackInfo struct {
	RefreshToken string
	AccessToken  string
	AuthCode     string // PKCE 新流程：authCodeInfo.AuthCode
}

// ParseCallback 解析 TRAE 登录回调链接，提取凭证字段。
//
// 回调形如：
//
//	http://127.0.0.1:PORT/authorize?refreshToken=...&userInfo={...}&userJwt={...}
//	或新流程：?authCodeInfo={AuthCode,...}&host=...&userInfo={...}
//
// 优先级：refreshToken → userJwt.RefreshToken → userJwt.Token → authCodeInfo.AuthCode。
func ParseCallback(rawURL string) CallbackInfo {
	info := CallbackInfo{}
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return info
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return info
	}
	q := u.Query()
	info.RefreshToken = q.Get("refreshToken")
	if info.RefreshToken != "" {
		return info
	}
	userJwt := parseJSONParam(q.Get("userJwt"))
	info.RefreshToken = getString(userJwt, "RefreshToken")
	if info.RefreshToken != "" {
		return info
	}
	info.AccessToken = getString(userJwt, "Token")
	if info.AccessToken != "" {
		return info
	}
	if raw := q.Get("authCodeInfo"); raw != "" {
		info.AuthCode = authCodeFromInfo(raw)
	}
	return info
}

// authCodeFromInfo 从 authCodeInfo（原始 code 或 JSON 对象）提取 AuthCode。
func authCodeFromInfo(raw string) string {
	raw = strings.TrimSpace(raw)
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) == nil {
		for _, container := range []any{m, m["Result"], m["result"]} {
			mm, ok := container.(map[string]any)
			if !ok {
				continue
			}
			if s := getString(mm, "AuthCode"); s != "" {
				return s
			}
		}
		return ""
	}
	return raw // 非 JSON：视为 code 本身
}

// parseJSONParam 解回调里 URL 编码的 JSON 参数（再容错解一层 percent-encoding）。
func parseJSONParam(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	candidates := []string{raw}
	if uq, err := url.QueryUnescape(raw); err == nil && uq != raw {
		candidates = append(candidates, uq)
	}
	for _, c := range candidates {
		var obj map[string]any
		if json.Unmarshal([]byte(c), &obj) == nil && obj != nil {
			return obj
		}
	}
	return nil
}

func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

func randHex(n int) string { b := make([]byte, n); _, _ = rand.Read(b); return hex.EncodeToString(b) }

// randNumericID 生成 15 位数字 ID，对齐官方客户端 device_id 格式（纯数字）。
func randNumericID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	n := uint64(b[0])<<24 | uint64(b[1])<<16 | uint64(b[2])<<8 | uint64(b[3])
	return fmt.Sprintf("%015d", n%900000000000000+100000000000000)
}

// newUUID 生成 v4 格式 UUID（真实客户端 login_trace_id 形如 0f9b52e9-a63f-48e5-9bcf-888c822a5a8e）。
func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
