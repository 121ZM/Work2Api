// authcode.go TRAE SOLO 登录所需的 PKCE / AuthCode 交换 / 用户信息。
//
// 与参考实现的差异：候选 origin **按账号所属版本**（auth.Auth.Region）取，
// 国内版回退 api.trae.cn，国际版回退 growsg-normal.trae.ai —— 国际版域名
// 来自本机 %APPDATA%\TRAE SOLO\logs\ 的真实请求日志，参考实现完全没有。
package traework

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"work2api/internal/auth"
	"work2api/internal/region"
)

// EpAuthCodeExchange AuthCode 交换端点（首次交换无签名）。
// 与 EpExchange 路径不同：EpExchange 是 refresh 续期，此处是 PKCE 首登。
const EpAuthCodeExchange = "/trae/api/v3/oauth/ExchangeToken"

// DeviceInfo 设备指纹（AuthCode 交换必需，官方客户端 bb() 注入）。
type DeviceInfo struct {
	DeviceID        string `json:"DeviceID"`
	MachineID       string `json:"MachineID"`
	PlatformCode    string `json:"PlatformCode"`
	DeviceType      string `json:"DeviceType"`
	DeviceName      string `json:"DeviceName"`
	DeviceModel     string `json:"DeviceModel"`
	ClientVersion   string `json:"ClientVersion"`
	DevicePublicKey string `json:"DevicePublicKey"`
	DeviceBrand     string `json:"DeviceBrand"`
	DeviceCPU       string `json:"DeviceCPU"`
	OSInfo          string `json:"OSInfo"`
	OSVersion       string `json:"OSVersion"`
}

// AuthCodeResult AuthCode 交换响应。
type AuthCodeResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
	Host         string // 交换成功的 origin（后续 API 用）
}

// GenPKCE 生成 PKCE code_verifier + code_challenge（S256）。
// verifier 必须与登录 URL 配对保存，回调后交换 token 时使用。
func GenPKCE() (verifier, challenge string) {
	b := make([]byte, 48)
	_, _ = rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return
}

// ExchangeAuthCode 用 AuthCode + CodeVerifier + 设备公钥交换 token。
//
// 依次尝试候选 origin：账号记录的 ApiHost → 所属版本的 UG 域 → 对侧域兜底。
// 首登时 ApiHost 为空，因此实际以 region 表为准。
func (c *Client) ExchangeAuthCode(a *auth.Auth, authCode, codeVerifier string) (*AuthCodeResult, error) {
	if strings.TrimSpace(authCode) == "" {
		return nil, fmt.Errorf("no authCode")
	}
	if strings.TrimSpace(codeVerifier) == "" {
		return nil, fmt.Errorf("no codeVerifier")
	}
	pubPEM, err := devicePublicKeyPEM()
	if err != nil {
		return nil, fmt.Errorf("device key: %w", err)
	}
	di := DeviceInfo{
		DeviceID:        a.DeviceID,
		MachineID:       a.MachineID,
		PlatformCode:    "SOLO_PC", // 对齐抓包 GetPCAuthCode 的 PlatformCode
		DeviceType:      "PC",
		DeviceName:      deviceDisplayName(),
		DeviceModel:     DeviceBrand,
		ClientVersion:   IdeVersion,
		DevicePublicKey: pubPEM,
		DeviceBrand:     DeviceBrand,
		OSInfo:          "windows",
		OSVersion:       OSVersion,
	}
	body, _ := json.Marshal(map[string]any{
		"ClientID":     ClientID,
		"AuthCode":     authCode,
		"CodeVerifier": codeVerifier,
		"DeviceInfo":   di,
		"IDEVersion":   IdeVersion,
	})

	var lastErr string
	for _, origin := range c.authCodeOrigins(a) {
		req, err := http.NewRequest(http.MethodPost, origin+EpAuthCodeExchange, bytes.NewReader(body))
		if err != nil {
			lastErr = err.Error()
			continue
		}
		OAuthHeaders(req)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = origin + " => " + err.Error()
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			lastErr = fmt.Sprintf("%s => HTTP %d %s", origin, resp.StatusCode, truncate(string(data), 160))
			continue
		}
		res, err := parseAuthCodeResult(data)
		if err != nil {
			lastErr = origin + " => " + err.Error()
			continue
		}
		res.Host = origin
		log.Printf("traework authcode exchange success region=%s host=%s token_len=%d refresh=%t",
			a.Region, origin, len(res.AccessToken), res.RefreshToken != "")
		return res, nil
	}
	return nil, fmt.Errorf("AuthCode ExchangeToken failed: %s", lastErr)
}

// authCodeOrigins 候选交换 origin：账号 ApiHost → 本版本 UG 域 → 对侧域兜底。
//
// 国际版把 CN 域放进兜底列表是有意为之：若某天 trae.ai 域名变更，
// 至少还能命中一次已知可用的旧域，避免完全无路可走。
func (c *Client) authCodeOrigins(a *auth.Auth) []string {
	h := c.hosts(a)
	candidates := []string{
		strings.TrimSpace(a.ApiHost),
		h.Ug,
		region.DefaultTrae[region.CN].Ug,
	}
	if h.Ug == region.DefaultTrae[region.CN].Ug {
		candidates = append(candidates, region.DefaultTrae[region.Global].Ug)
	}
	var out []string
	for _, cand := range candidates {
		cand = strings.TrimRight(strings.TrimSpace(cand), "/")
		if cand == "" || containsString(out, cand) {
			continue
		}
		out = append(out, cand)
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// parseAuthCodeResult 解析交换响应；token 可能在 Result / result / data 任一容器内。
func parseAuthCodeResult(data []byte) (*AuthCodeResult, error) {
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("authcode exchange parse: %w", err)
	}
	res := &AuthCodeResult{}
	for _, container := range []any{v["Result"], v["result"], v["data"], v} {
		m, ok := container.(map[string]any)
		if !ok {
			continue
		}
		res.AccessToken = firstNonEmptyStr(m, "AccessToken", "accessToken", "access_token", "Token", "token")
		if res.AccessToken != "" {
			res.RefreshToken = firstNonEmptyStr(m, "RefreshToken", "refreshToken", "refresh_token")
			res.ExpiresAt = normalizeExpiresAt(firstInt64(m, "TokenExpireAt", "tokenExpireAt", "ExpiresAt", "expiresAt", "expiredAt"))
			break
		}
	}
	if res.AccessToken == "" {
		return nil, fmt.Errorf("response missing token")
	}
	return res, nil
}

func firstNonEmptyStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func firstInt64(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int64(v)
		case json.Number:
			if n, err := v.Int64(); err == nil {
				return n
			}
		}
	}
	return 0
}

// devicePublicKeyPEM 生成一次性 ECDSA P256 公私钥对，返回公钥 PEM。
// 官方客户端持私钥用于后续鉴权；反代只需完成本次交换。
func devicePublicKeyPEM() (string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

// deviceDisplayName 取主机名作设备名（官方客户端取 USER/USERNAME/HOSTNAME）。
func deviceDisplayName() string {
	if v := os.Getenv("USERNAME"); v != "" {
		return v
	}
	if v := os.Getenv("USER"); v != "" {
		return v
	}
	return "PC"
}

// GetUserInfo 拉取 uid / 昵称 / 企业 ID。
func (c *Client) GetUserInfo(a *auth.Auth) (uid, nickname, enterpriseID string, err error) {
	host := strings.TrimRight(strings.TrimSpace(a.ApiHost), "/")
	if host == "" {
		host = c.oauthBase(a)
	}
	body, _ := json.Marshal(map[string]any{"ReqSource": "IDE", "IDEVersion": IdeVersion})
	req, err := http.NewRequest(http.MethodPost, host+EpUserInfo, bytes.NewReader(body))
	if err != nil {
		return "", "", "", err
	}
	OAuthHeaders(req)
	req.Header.Set("X-Cloudide-Token", a.JWT())
	data, err := c.doJSON(req)
	if err != nil {
		return "", "", "", err
	}
	var resp struct {
		Result struct {
			UserID       string `json:"UserID"`
			ScreenName   string `json:"ScreenName"`
			EnterpriseID string `json:"EnterpriseID"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", "", "", fmt.Errorf("userinfo parse: %w", err)
	}
	return resp.Result.UserID, resp.Result.ScreenName, resp.Result.EnterpriseID, nil
}
