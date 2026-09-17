// Package auth 解析账号凭证文件，提供 region 定值与 refresh 后的原子写回。
//
// 与参考实现的关键差异：**region 是账号级属性**（Auth.Region），加载时定值，
// 不做「按 region 过滤账号」——同一进程内 CN 与 Global 账号共存。
//
// 支持的磁盘形态：
//
//	嵌套形 {"auth":{...},"account":{...}}   —— 插件 OAuth 输出 / 本包写回格式
//	扁平形 {"accessToken":...,"uid":...}    —— 手工导入 / 面板粘贴
//
// 实测陷阱（务必保留处理）：
//  1. 客户端 .info 里 expiresAt / refreshExpiresAt 是**毫秒**，不归一会导致永不刷新
//  2. auth.domain 不等于 Origin/Referer（CN 的 domain 是 www.workbuddy.cn，
//     但 Origin 要发 www.codebuddy.cn），见 internal/region
package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"work2api/internal/region"
)

// 渠道标识（与 provider.Kind 取值一致，此处用字符串避免包循环依赖）。
const (
	KindWorkBuddy = "workbuddy"
	KindTrae      = "traework"
)

// epochMillisThreshold 秒级时间戳与毫秒级的分界。
// 1e11 秒 ≈ 公元 5138 年，真实秒级时间戳不可能超过；而毫秒级当前约 1.8e12。
const epochMillisThreshold = 1e11

// Auth 归一化后的账号凭证。
type Auth struct {
	// mu 串行化 RefreshToken 写与 SaveAtomic / JWT 读，防止并发读写 token。
	mu sync.RWMutex

	Kind   string        // workbuddy | traework
	Region region.Region // cn | global —— 账号级属性

	AccessToken  string
	RefreshToken string
	ExpiresAt    int64  // Unix **秒**（解析时已从毫秒归一）
	Domain       string // 上游返回的 domain，仅用于 region 判定与 X-Domain 头
	ApiHost      string // Trae: 登录时实际使用的 API host
	MachineID    string // Trae: x-machine-id
	DeviceID     string // Trae: x-device-id

	UID          string
	EnterpriseID string
	Nickname     string
	FilePath     string // 来源文件；refresh 后原子写回此处
}

// Lock / Unlock / RLock / RUnlock 供同进程其他包在改写字段期间加锁。
func (a *Auth) Lock()    { a.mu.Lock() }
func (a *Auth) Unlock()  { a.mu.Unlock() }
func (a *Auth) RLock()   { a.mu.RLock() }
func (a *Auth) RUnlock() { a.mu.RUnlock() }

// JWT 返回当前 access token 快照（Trae 侧也叫 Cloud-IDE-JWT）。
func (a *Auth) JWT() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.AccessToken
}

// NeedsRefreshLocked 是 NeedsRefresh 的持锁内部版本；调用方必须已持有读/写锁。
func (a *Auth) NeedsRefreshLocked(within time.Duration) bool {
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期 / 无 expiry）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.NeedsRefreshLocked(within)
}

// Key 返回账号在池内的唯一键（含 region，避免两区 UID 撞车）。
func (a *Auth) Key() string {
	return a.Kind + "|" + a.Region.String() + "|" + a.UID
}

// Label 返回面板展示用的简短标识。
func (a *Auth) Label() string {
	if a.Nickname != "" {
		return a.Nickname
	}
	if a.UID != "" {
		return a.UID
	}
	return filepath.Base(a.FilePath)
}

// RegionFromDomain 按 domain 后缀判定版本；无法判定时返回 CN（向后兼容）。
//
// 这是 region 的**交叉校验兜底**：正常情况下 Region 在加载时已由文件名或
// 显式字段定值，此函数用于校验两者是否一致，不一致则拒收（见 importauth）。
func RegionFromDomain(domain string) region.Region {
	d := strings.ToLower(strings.TrimSpace(domain))
	if d == "" {
		return region.CN
	}
	for _, suffix := range []string{".workbuddy.ai", ".codebuddy.ai", ".trae.ai", ".trae.com"} {
		if strings.HasSuffix(d, suffix) || d == strings.TrimPrefix(suffix, ".") {
			return region.Global
		}
	}
	return region.CN
}

// NormalizeEpoch 把可能为毫秒的时间戳归一为秒。
//
// 客户端 .info 的 expiresAt / refreshExpiresAt 实测为 13 位毫秒
// （如 1794791195362）。直接当作秒使用会让 NeedsRefresh 永远返回 false，
// token 过期后不再刷新。
func NormalizeEpoch(v int64) int64 {
	if v > epochMillisThreshold {
		return v / 1000
	}
	return v
}

// Parse 解析凭证文件内容为 Auth。kind 由调用方按文件名前缀给定。
func Parse(raw []byte, kind string) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}

	var a Auth
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				AccessToken      string `json:"accessToken"`
				RefreshToken     string `json:"refreshToken"`
				ExpiresAt        int64  `json:"expiresAt"`
				RefreshExpiresAt int64  `json:"refreshExpiresAt"`
				Domain           string `json:"domain"`
				ApiHost          string `json:"apiHost"`
				MachineID        string `json:"machineId"`
				DeviceID         string `json:"deviceId"`
				Region           string `json:"region"`
			} `json:"auth"`
			Account struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
			} `json:"account"`
			// 本包写回时把 region 也放在顶层，便于人工查看
			Region string `json:"region"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  n.Auth.AccessToken,
			RefreshToken: n.Auth.RefreshToken,
			ExpiresAt:    NormalizeEpoch(n.Auth.ExpiresAt),
			Domain:       n.Auth.Domain,
			ApiHost:      n.Auth.ApiHost,
			MachineID:    n.Auth.MachineID,
			DeviceID:     n.Auth.DeviceID,
			UID:          n.Account.UID,
			EnterpriseID: n.Account.EnterpriseID,
			Nickname:     n.Account.Nickname,
		}
		if r := firstNonEmpty(n.Auth.Region, n.Region); r != "" {
			a.Region = region.Parse(r)
		}
	} else {
		var f struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
			ApiHost      string `json:"apiHost"`
			MachineID    string `json:"machineId"`
			DeviceID     string `json:"deviceId"`
			Region       string `json:"region"`
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  f.AccessToken,
			RefreshToken: f.RefreshToken,
			ExpiresAt:    NormalizeEpoch(f.ExpiresAt),
			Domain:       f.Domain,
			ApiHost:      f.ApiHost,
			MachineID:    f.MachineID,
			DeviceID:     f.DeviceID,
			UID:          f.UID,
			EnterpriseID: f.EnterpriseID,
			Nickname:     f.Nickname,
		}
		if f.Region != "" {
			a.Region = region.Parse(f.Region)
		}
	}

	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	// region 未显式给出时按 domain 兜底
	if !a.Region.Valid() {
		a.Region = RegionFromDomain(a.Domain)
	}
	a.Kind = kind
	return &a, nil
}

// SaveAtomic 以嵌套形原子写回 FilePath（tmp + rename），并保留 region 字段。
func (a *Auth) SaveAtomic() error {
	return a.SaveAtomicAs(a.FilePath)
}

// SaveAtomicAs 以嵌套形原子写入指定路径，**不改动** a.FilePath。
// 供导入流程使用：把扫描到的凭证落到自己的 authDir，而不触碰客户端文件。
func (a *Auth) SaveAtomicAs(path string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveAtomicLocked(path)
}

func (a *Auth) saveAtomicLocked(path string) error {
	if path == "" {
		return fmt.Errorf("no target path")
	}
	doc := map[string]any{
		"region": a.Region.String(),
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt, // 秒（内部统一单位）
			"domain":       a.Domain,
			"apiHost":      a.ApiHost,
			"machineId":    a.MachineID,
			"deviceId":     a.DeviceID,
			"region":       a.Region.String(),
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// FileName 返回该账号在 authDir 下的规范文件名。
// 含 region 段，避免两区同 UID 互相覆盖。
func FileName(kind string, r region.Region, uid string) string {
	return fmt.Sprintf("%s-%s-%s.json", kind, r.String(), uid)
}

// LoadDir 扫描 dir 下所有渠道的凭证文件。
//
// 与参考实现不同：**不过滤 region**，全部加载，region 由每个文件自身决定。
// 解析失败的文件静默跳过（启动日志由调用方统计）。
func LoadDir(dir string) ([]*Auth, error) {
	var out []*Auth
	for _, spec := range []struct {
		glob string
		kind string
	}{
		{"workbuddy*.json", KindWorkBuddy},
		{"trae*.json", KindTrae},
	} {
		files, err := filepath.Glob(filepath.Join(dir, spec.glob))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if strings.HasSuffix(f, ".tmp") {
				continue
			}
			a, err := LoadFile(f, spec.kind)
			if err != nil {
				continue
			}
			out = append(out, a)
		}
	}
	return out, nil
}

// LoadFile 读取并解析单个凭证文件，同时设置 Kind / FilePath。
func LoadFile(path, kind string) (*Auth, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	a, err := Parse(raw, kind)
	if err != nil {
		return nil, err
	}
	a.FilePath = path
	return a, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
