// Package pool 管理一组**同渠道同版本**的账号：选号、冷却、熔断、状态持久化。
//
// 隔离策略：每个 provider.Channel（如 workbuddy/cn 与 workbuddy/global）
// 持有独立的 Pool 实例。冷却与熔断本就是「池」级语义，按渠道 + 版本分池后，
// 国内版被限流不会波及国际版，也无需在池内部再区分 region。
package pool

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"work2api/internal/auth"
	"work2api/internal/provider"
	"work2api/internal/region"
)

// ErrNoAccount 池内没有可用账号（全部冷却 / 禁用 / 为空）。
var ErrNoAccount = errors.New("no available account in pool")

// Config 冷却参数。
type Config struct {
	HardCredit  time.Duration // 余额不足 → 长冷却
	SoftRate    time.Duration // 限流 → 短冷却
	ErrThresh   int           // 连续错误触发冷却的阈值
	ErrCooldown time.Duration // 熔断退避时长
}

// State 账号状态快照（供 /status 与 Web 面板）。
type State struct {
	Kind             string        `json:"kind"`
	Region           region.Region `json:"region"`
	UID              string        `json:"uid"`
	Nickname         string        `json:"nickname,omitempty"`
	Remain           int64         `json:"remain"`
	HasRemain        bool          `json:"has_remain"`
	Disabled         bool          `json:"disabled"`
	DisabledReason   string        `json:"disabled_reason,omitempty"`
	CooldownUntil    int64         `json:"cooldown_until,omitempty"`
	CooldownReason   string        `json:"cooldown_reason,omitempty"`
	ErrCount         int           `json:"err_count"`
	LastErr          string        `json:"last_err,omitempty"`
	ExpiresAt        int64         `json:"expires_at,omitempty"`
	CheckinSupported bool          `json:"checkin_supported"`
	LastCheckinAt    int64         `json:"last_checkin_at,omitempty"`
	LastCheckinMsg   string        `json:"last_checkin_msg,omitempty"`
}

// Cooldown 报告快照当前是否处于冷却中。
func (s State) Cooling() bool { return s.CooldownUntil > time.Now().Unix() }

type entry struct {
	auth *auth.Auth

	disabled       bool
	disabledReason string

	cooldownUntil  time.Time
	cooldownReason string

	errCount  int
	lastErr   string
	lastUsed  time.Time
	remain    int64
	hasRemain bool

	lastCheckinAt  time.Time
	lastCheckinMsg string
}

// Pool 一组账号。
type Pool struct {
	channel provider.Channel
	cfg     Config

	mu      sync.Mutex
	entries map[string]*entry // key = auth.Key()
}

// New 创建某渠道 + 版本的账号池。
func New(ch provider.Channel, cfg Config) *Pool {
	if cfg.ErrThresh <= 0 {
		cfg.ErrThresh = 3
	}
	if cfg.HardCredit <= 0 {
		cfg.HardCredit = 12 * time.Hour
	}
	if cfg.SoftRate <= 0 {
		cfg.SoftRate = 60 * time.Second
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 10 * time.Minute
	}
	return &Pool{channel: ch, cfg: cfg, entries: map[string]*entry{}}
}

// Channel 返回本池对应的渠道 + 版本。
func (p *Pool) Channel() provider.Channel { return p.channel }

// Add 加入账号（同 UID 覆盖）。
func (p *Pool) Add(a *auth.Auth) {
	if a == nil || a.UID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	key := a.Key()
	if e, ok := p.entries[key]; ok {
		e.auth = a
		return
	}
	p.entries[key] = &entry{auth: a}
}

// Auth 按 UID 取回账号对象。
//
// 注意：本方法**不看账号状态** —— 停用与冷却中的账号同样会被返回。
// 需要「可用才返回」的语义请用 UsableAuth。
func (p *Pool) Auth(uid string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.auth.UID == uid {
			return e.auth
		}
	}
	return nil
}

// UsableAuth 返回该 UID 的账号，**前提是它当前可用**（未停用且不在冷却中）；
// 否则返回 nil。
//
// 存在的理由：粘性路由不能只判断「账号还在不在池里」，还要判断「它是否健康」。
// 早期写法是 `Pool.Auth(uid) != nil && Pool.Healthy() > 0`，其中 Healthy() 返回的是
// **池级**健康账号数、与这个账号毫无关系 —— 于是已停用 / 已进冷却的粘性账号
// 会被一直复用，直到某次请求失败才被动换号。
func (p *Pool) UsableAuth(uid string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.find(uid)
	if e == nil || e.disabled || time.Now().Before(e.cooldownUntil) {
		return nil
	}
	return e.auth
}

// Remove 移除账号。
func (p *Pool) Remove(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, e := range p.entries {
		if e.auth.UID == uid {
			delete(p.entries, k)
			return true
		}
	}
	return false
}

// SetDisabled 停用 / 启用账号。
func (p *Pool) SetDisabled(uid string, disabled bool, reason string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.find(uid)
	if e == nil {
		return false
	}
	e.disabled = disabled
	e.disabledReason = reason
	if !disabled {
		e.errCount = 0
		e.cooldownUntil = time.Time{}
		e.cooldownReason = ""
	}
	return true
}

// Pick 选一个健康账号：跳过禁用与冷却中的账号，按可用积分降序，
// 同分时优先最久未使用的（避免总压一个账号）。
func (p *Pool) Pick() (*auth.Auth, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	var best *entry
	for _, e := range p.entries {
		if e.disabled || now.Before(e.cooldownUntil) {
			continue
		}
		if best == nil || better(e, best) {
			best = e
		}
	}
	if best == nil {
		return nil, ErrNoAccount
	}
	best.lastUsed = now
	return best.auth, nil
}

// better 报告 a 是否比 b 更该被选中。
func better(a, b *entry) bool {
	if a.hasRemain != b.hasRemain {
		return a.hasRemain // 已知余额的优先
	}
	if a.hasRemain && a.remain != b.remain {
		return a.remain > b.remain
	}
	return a.lastUsed.Before(b.lastUsed)
}

// NoteSuccess 记录一次成功调用。
func (p *Pool) NoteSuccess(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.find(a.UID); e != nil {
		e.errCount = 0
	}
}

// NoteError 按错误类别推进冷却状态机。
func (p *Pool) NoteError(a *auth.Auth, kind provider.ErrKind, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.find(a.UID)
	if e == nil {
		return
	}
	e.lastErr = msg
	now := time.Now()
	switch kind {
	case provider.ErrHardCredit:
		e.cooldownUntil, e.cooldownReason = now.Add(p.cfg.HardCredit), "hard_credit"
		log.Printf("pool cooldown channel=%s uid=%s kind=hard_credit until=%s",
			p.channel, a.UID, e.cooldownUntil.Format(time.RFC3339))
	case provider.ErrSoftRate:
		e.cooldownUntil, e.cooldownReason = now.Add(p.cfg.SoftRate), "soft_rate"
	case provider.ErrNotFound:
		// 上游偶发 404：短冷却但**不累计** errCount，避免误熔断
		e.cooldownUntil, e.cooldownReason = now.Add(p.cfg.SoftRate), "not_found"
	case provider.ErrSessionDead:
		e.disabled, e.disabledReason = true, "session_dead"
		log.Printf("pool disable channel=%s uid=%s reason=session_dead", p.channel, a.UID)
	case provider.ErrServer, provider.ErrClient:
		e.errCount++
		if e.errCount >= p.cfg.ErrThresh {
			e.cooldownUntil, e.cooldownReason = now.Add(p.cfg.ErrCooldown), "err_threshold"
			e.errCount = 0
		}
	}
}

// NoteRemain 记录查询到的可用积分。
func (p *Pool) NoteRemain(a *auth.Auth, remain int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.find(a.UID); e != nil {
		e.remain, e.hasRemain = remain, true
	}
}

// MarkCheckin 记录签到结果。
func (p *Pool) MarkCheckin(a *auth.Auth, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.find(a.UID); e != nil {
		e.lastCheckinAt, e.lastCheckinMsg = time.Now(), msg
	}
}

// List 返回全部账号状态快照（按 region 再按积分排序，便于面板展示）。
func (p *Pool) List() []State {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]State, 0, len(p.entries))
	for _, e := range p.entries {
		st := State{
			Kind:             p.channel.Kind.String(),
			Region:           p.channel.Region,
			UID:              e.auth.UID,
			Nickname:         e.auth.Nickname,
			Remain:           e.remain,
			HasRemain:        e.hasRemain,
			Disabled:         e.disabled,
			DisabledReason:   e.disabledReason,
			ErrCount:         e.errCount,
			LastErr:          e.lastErr,
			ExpiresAt:        e.auth.ExpiresAt,
			CheckinSupported: p.channel.CheckinSupported(),
			LastCheckinMsg:   e.lastCheckinMsg,
		}
		if !e.cooldownUntil.IsZero() {
			st.CooldownUntil, st.CooldownReason = e.cooldownUntil.Unix(), e.cooldownReason
		}
		if !e.lastCheckinAt.IsZero() {
			st.LastCheckinAt = e.lastCheckinAt.Unix()
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Region != out[j].Region {
			return out[i].Region < out[j].Region
		}
		return out[i].Remain > out[j].Remain
	})
	return out
}

// UIDs 返回池内全部账号 UID（含禁用项，供批量签到遍历）。
func (p *Pool) UIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.entries))
	for _, e := range p.entries {
		out = append(out, e.auth.UID)
	}
	sort.Strings(out)
	return out
}

// Healthy 报告池内是否有可用账号。
func (p *Pool) Healthy() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	n := 0
	for _, e := range p.entries {
		if !e.disabled && !now.Before(e.cooldownUntil) {
			n++
		}
	}
	return n
}

// Len 返回池内账号总数。
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

func (p *Pool) find(uid string) *entry {
	for _, e := range p.entries {
		if e.auth.UID == uid {
			return e
		}
	}
	return nil
}

// ---- 持久化 ----

// persisted 落盘结构（只存运行态，不存 token —— token 在 authDir 里）。
type persisted struct {
	Channel string            `json:"channel"`
	Items   map[string]record `json:"items"`
}

type record struct {
	Disabled       bool   `json:"disabled,omitempty"`
	DisabledReason string `json:"disabled_reason,omitempty"`
	CooldownUntil  int64  `json:"cooldown_until,omitempty"`
	CooldownReason string `json:"cooldown_reason,omitempty"`
	Remain         int64  `json:"remain"`
	HasRemain      bool   `json:"has_remain,omitempty"`
	LastCheckinAt  int64  `json:"last_checkin_at,omitempty"`
	LastCheckinMsg string `json:"last_checkin_msg,omitempty"`
}

// SaveState 把运行态写入 path（原子写）。path 为空则跳过。
func (p *Pool) SaveState(path string) error {
	if path == "" {
		return nil
	}
	p.mu.Lock()
	items := make(map[string]record, len(p.entries))
	for k, e := range p.entries {
		r := record{
			Disabled: e.disabled, DisabledReason: e.disabledReason,
			Remain: e.remain, HasRemain: e.hasRemain,
			LastCheckinMsg: e.lastCheckinMsg,
		}
		if !e.cooldownUntil.IsZero() {
			r.CooldownUntil, r.CooldownReason = e.cooldownUntil.Unix(), e.cooldownReason
		}
		if !e.lastCheckinAt.IsZero() {
			r.LastCheckinAt = e.lastCheckinAt.Unix()
		}
		items[k] = r
	}
	ch := p.channel.Key()
	p.mu.Unlock()

	raw, err := json.MarshalIndent(persisted{Channel: ch, Items: items}, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadState 从 path 恢复运行态。文件不存在不算错误。
func (p *Pool) LoadState(path string) error {
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var doc persisted
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("state parse: %w", err)
	}
	if doc.Channel != "" && doc.Channel != p.channel.Key() {
		return nil // 别的渠道的状态文件
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, r := range doc.Items {
		e, ok := p.entries[k]
		if !ok {
			continue
		}
		e.disabled, e.disabledReason = r.Disabled, r.DisabledReason
		e.remain, e.hasRemain = r.Remain, r.HasRemain
		e.lastCheckinMsg = r.LastCheckinMsg
		if r.CooldownUntil > 0 {
			e.cooldownUntil, e.cooldownReason = time.Unix(r.CooldownUntil, 0), r.CooldownReason
		}
		if r.LastCheckinAt > 0 {
			e.lastCheckinAt = time.Unix(r.LastCheckinAt, 0)
		}
	}
	return nil
}
