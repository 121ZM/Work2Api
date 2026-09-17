// Package loginsvc 管理交互式登录会话。
//
// 每个渠道（kind + region）同时只允许一个在途会话：登录本身是单用户交互动作，
// 同渠道并发登录没有意义，且会让轮询结果串台。
//
// 职责边界：本包只做「发起 → 轮询 → 落盘」，不碰账号池热加载 ——
// 落盘后由调用方（app）决定何时 reload。
package loginsvc

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"work2api/internal/auth"
	"work2api/internal/login"
	"work2api/internal/login_trae"
	"work2api/internal/provider"
	"work2api/internal/region"
)

// ErrNoSession 该渠道没有在途登录会话。
var ErrNoSession = errors.New("no pending login session")

// Session 一次在途登录。
type Session struct {
	Kind      provider.Kind
	Region    region.Region
	AuthURL   string
	StatePath string
	CreatedAt time.Time
}

// Key 会话键。
func (s *Session) Key() string { return string(s.Kind) + "|" + s.Region.String() }

// Outcome 轮询成功后的结果。
type Outcome struct {
	Kind   provider.Kind
	Region region.Region
	UID    string
	Nick   string
	Domain string
	Path   string // 落盘文件路径
}

// Manager 登录会话管理器。
type Manager struct {
	mu       sync.Mutex
	stateDir string
	authDir  string
	sessions map[string]*Session // key: kind|region

	WBOverrides   map[string]region.WorkBuddyHosts
	TraeOverrides map[string]region.TraeHosts

	// ClientFactory 可注入，测试时替换；默认每会话独立 cookie jar。
	ClientFactory func() *http.Client
}

// New 创建管理器。stateDir 存放登录中间状态（state 文件）。
func New(stateDir, authDir string) *Manager {
	return &Manager{
		stateDir: stateDir,
		authDir:  authDir,
		sessions: make(map[string]*Session),
	}
}

// Start 发起登录，返回可直接打开的授权 URL。
//
// 同一渠道已有在途会话时**复用**（返回既有 URL），避免用户重复点击产生
// 多个 state 互相覆盖。
func (m *Manager) Start(kind provider.Kind, r region.Region) (*Session, error) {
	if !r.Valid() {
		return nil, fmt.Errorf("invalid region %q", r)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	key := string(kind) + "|" + r.String()
	if s, ok := m.sessions[key]; ok {
		return s, nil
	}
	if err := os.MkdirAll(m.stateDir, 0o755); err != nil {
		return nil, err
	}
	statePath := filepath.Join(m.stateDir, fmt.Sprintf("login-%s-%s.json", kind, r))

	var (
		authURL string
		err     error
	)
	switch kind {
	case provider.WorkBuddy:
		h := region.WorkBuddy(r, m.WBOverrides)
		client := m.newClient()
		authURL, err = login.Start(client, r, h, statePath)
		if err != nil {
			return nil, err
		}
		// 跟随一次重定向，让用户直接打开最终地址
		if resolved, rerr := login.ResolveAuthURL(client, h, authURL); rerr == nil && resolved != "" {
			authURL = resolved
		}
	case provider.Trae:
		h := region.Trae(r, m.TraeOverrides)
		authURL, err = login_trae.Start(m.newTraeClient(), r, h, statePath)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown kind %q", kind)
	}

	s := &Session{Kind: kind, Region: r, AuthURL: authURL, StatePath: statePath, CreatedAt: time.Now()}
	m.sessions[key] = s
	return s, nil
}

// Poll 轮询指定渠道的登录状态。
//
// 返回 (nil, nil) 表示尚未完成；成功时落盘并移除会话；
// 失败（含 state 文件不存在）时移除会话，避免用户卡在坏会话上。
func (m *Manager) Poll(kind provider.Kind, r region.Region) (*Outcome, error) {
	m.mu.Lock()
	key := string(kind) + "|" + r.String()
	s, ok := m.sessions[key]
	m.mu.Unlock()
	if !ok {
		return nil, ErrNoSession
	}

	var (
		out *Outcome
		err error
	)
	switch kind {
	case provider.WorkBuddy:
		var res login.Result
		res, err = login.Poll(m.newClient(), m.WBOverrides, s.StatePath)
		if err == nil {
			var fp string
			if fp, err = login.SaveAuth(m.authDir, res); err == nil {
				out = &Outcome{Kind: kind, Region: auth.RegionFromDomain(res.Domain), UID: res.UID, Nick: res.Nickname, Domain: res.Domain, Path: fp}
			}
		}
	case provider.Trae:
		var res login_trae.Result
		res, err = login_trae.Poll(m.newTraeClient(), m.TraeOverrides, s.StatePath)
		if err == nil {
			var fp string
			if fp, err = login_trae.SaveAuth(m.authDir, res); err == nil {
				out = &Outcome{Kind: kind, Region: res.Region, UID: res.UID, Nick: res.Nickname, Domain: res.Domain, Path: fp}
			}
		}
	default:
		return nil, fmt.Errorf("unknown kind %q", kind)
	}

	if err != nil {
		if isPending(err) {
			return nil, nil // 尚未完成，保留会话
		}
		m.drop(key)
		return nil, err
	}
	m.drop(key)
	return out, nil
}

// Cancel 放弃在途会话（不删客户端文件，仅清本地中间态）。
func (m *Manager) Cancel(kind provider.Kind, r region.Region) {
	m.drop(string(kind) + "|" + r.String())
}

// Pending 列出所有在途会话（面板展示用）。
func (m *Manager) Pending() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	return out
}

func (m *Manager) drop(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[key]; ok {
		_ = os.Remove(s.StatePath)
		delete(m.sessions, key)
	}
}

// newClient 每个登录流程独立的 cookie jar（多账号登录互不串会话）。
func (m *Manager) newClient() *http.Client {
	if m.ClientFactory != nil {
		return m.ClientFactory()
	}
	return login.NewClient()
}

func (m *Manager) newTraeClient() *http.Client {
	return login_trae.NewClient()
}

// isPending 判定错误是否为「尚未完成」。
//
// 两个登录包的 ErrPending 都是 sentinel，直接 errors.Is 即可 ——
// 这里额外容忍 state 文件被外部删除导致的 read 错误，视为会话失效。
func isPending(err error) bool {
	return errors.Is(err, login.ErrPending) || errors.Is(err, login_trae.ErrPending)
}
