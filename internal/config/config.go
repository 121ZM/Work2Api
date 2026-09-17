// Package config 加载 JSON 配置 + 环境变量覆盖，并支持原子写回（Web 面板修改）。
//
// 与参考实现的关键差异：本包**不含全局 region 配置**。region 是账号级属性
// （见 internal/auth.Auth.Region），同一进程内 CN 与 Global 账号共存，
// 按账号所属版本自动选择上游域名。因此这里既没有 config.region，
// 也没有「按 region 过滤账号」的加载语义。
package config

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"work2api/internal/region"
)

// APIKeyPrefix 生成的随机 Key 的统一前缀，便于在日志/配置里一眼认出。
const APIKeyPrefix = "w2a_"

// GenerateAPIKey 用 crypto/rand 生成 128 位随机 Key。
//
// 为什么不留硬编码默认值：默认值一旦公开，任何扫到这个端口的人都能直接调用，
// 等于没有鉴权。首次运行自动生成并落盘，既省去手填，也不牺牲强度。
func GenerateAPIKey() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 在支持的平台上不会失败；真失败了也不能退回可预测的值。
		panic("config: 无法获取随机数：" + err.Error())
	}
	return APIKeyPrefix + hex.EncodeToString(buf)
}

// Listen 监听地址：Host 为空表示全部接口。
type Listen struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// Addr 返回 net.Listen 使用的地址串，如 "127.0.0.1:7865" / ":7865"。
func (l Listen) Addr() string {
	host, port := l.Host, l.Port
	if port <= 0 {
		port = 7865
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// UnmarshalJSON 兼容旧版字符串形式（":7865" / "127.0.0.1:9999" / "9999"）。
func (l *Listen) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		l2, err := ParseListen(s)
		if err != nil {
			return err
		}
		*l = l2
		return nil
	}
	var o struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	if err := json.Unmarshal(data, &o); err != nil {
		return err
	}
	l.Host, l.Port = o.Host, o.Port
	return nil
}

// ParseListen 解析 "host:port" / ":port" / "port" 三种形式。
func ParseListen(s string) (Listen, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Listen{}, fmt.Errorf("empty listen address")
	}
	host, portStr := s, ""
	if i := strings.LastIndex(s, ":"); i >= 0 {
		host, portStr = s[:i], s[i+1:]
	} else if n, err := strconv.Atoi(host); err == nil {
		return Listen{Port: n}, nil
	}
	port := 0
	if portStr != "" {
		n, err := strconv.Atoi(portStr)
		if err != nil || n <= 0 || n > 65535 {
			return Listen{}, fmt.Errorf("bad listen port %q", portStr)
		}
		port = n
	}
	return Listen{Host: host, Port: port}, nil
}

// Config 顶层配置。
type Config struct {
	Listen    Listen `json:"listen"`
	APIKey    string `json:"api_key"`    // 空 = 不鉴权（仅建议在监听 127.0.0.1 时）
	AuthDir   string `json:"auth_dir"`   // ./auths
	StateFile string `json:"state_file"` // ./data/state.json

	Cooldown struct {
		HardCredit  string `json:"hard_credit"`   // "12h" 余额不足长冷却
		SoftRate    string `json:"soft_rate"`     // "60s" 限流短冷却
		ErrThresh   int    `json:"err_threshold"` // 连续错误触发冷却的阈值
		ErrCooldown string `json:"err_cooldown"`  // "10m"
	} `json:"cooldown"`

	Schedule struct {
		CheckinTimes   []string `json:"checkin_times"`   // ["09:00","21:00"]，仅对国内版账号生效
		KeepaliveHours []int    `json:"keepalive_hours"` // [22] token 保活整点
	} `json:"schedule"`

	Upstream struct {
		TimeoutSeconds int `json:"timeout_seconds"` // 默认 120
	} `json:"upstream"`

	// Regions 可选覆盖内置域名表（键：cn / global）。留空即用 region 包的内置表。
	Regions struct {
		WorkBuddy map[string]region.WorkBuddyHosts `json:"workbuddy,omitempty"`
		Trae      map[string]region.TraeHosts      `json:"trae,omitempty"`
	} `json:"regions,omitempty"`

	// 解析后（不参与序列化）
	HardCreditDur  time.Duration `json:"-"`
	SoftRateDur    time.Duration `json:"-"`
	ErrCooldownDur time.Duration `json:"-"`
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen: Listen{Host: "127.0.0.1", Port: 7865},
		// 留空 = 由 Load 生成随机 Key 并落盘（见 EnsureAPIKey）。
		APIKey:    "",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
	}
	c.Cooldown.HardCredit = "12h"
	c.Cooldown.SoftRate = "60s"
	c.Cooldown.ErrThresh = 3
	c.Cooldown.ErrCooldown = "10m"
	c.Schedule.CheckinTimes = []string{"09:00", "21:00"}
	c.Schedule.KeepaliveHours = []int{22}
	c.Upstream.TimeoutSeconds = 120
	return c
}

// Load 从文件读，再用 W2A_* 环境变量覆盖。path 为空则直接用默认值。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(c)
	if err := c.Normalize(); err != nil {
		return nil, err
	}
	// Key 为空说明既没写在文件里、也没被 W2A_API_KEY 覆盖 —— 生成一个并落盘，
	// 保证「一次生成、长期稳定」：重启换 Key 会让已配置的客户端全部 401。
	if c.APIKey == "" {
		c.APIKey = GenerateAPIKey()
		if path != "" {
			if err := Save(c, path); err != nil {
				return nil, fmt.Errorf("persist generated api_key: %w", err)
			}
		}
	}
	return c, nil
}

// Save 以 0600 原子写回配置。
func Save(c *Config, path string) error {
	raw, err := json.MarshalIndent(c, "", "  ")
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

// ParseClockTimes 将 HH:MM 列表转换为当天分钟数（0..1439），去重并升序。
func ParseClockTimes(values []string) ([]int, error) {
	seen := map[int]bool{}
	out := make([]int, 0, len(values))
	for _, v := range values {
		parts := strings.Split(strings.TrimSpace(v), ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid time %q", v)
		}
		h, errH := strconv.Atoi(parts[0])
		m, errM := strconv.Atoi(parts[1])
		if errH != nil || errM != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			return nil, fmt.Errorf("invalid time %q", v)
		}
		if minute := h*60 + m; !seen[minute] {
			seen[minute] = true
			out = append(out, minute)
		}
	}
	sort.Ints(out)
	return out, nil
}

// FormatClockTimes 将当天分钟数格式化为排序后的 HH:MM 列表。
func FormatClockTimes(minutes []int) []string {
	out := make([]int, 0, len(minutes))
	seen := map[int]bool{}
	for _, m := range minutes {
		if m >= 0 && m < 24*60 && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	sort.Ints(out)
	formatted := make([]string, 0, len(out))
	for _, m := range out {
		formatted = append(formatted, fmt.Sprintf("%02d:%02d", m/60, m%60))
	}
	return formatted
}

func applyEnv(c *Config) {
	if v := os.Getenv("W2A_LISTEN"); v != "" {
		if l, err := ParseListen(v); err == nil {
			c.Listen = l
		}
	}
	if v := os.Getenv("W2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("W2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("W2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("W2A_HARD_CREDIT"); v != "" {
		c.Cooldown.HardCredit = v
	}
	if v := os.Getenv("W2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("W2A_ERR_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Cooldown.ErrThresh = n
		}
	}
	if v := os.Getenv("W2A_ERR_COOLDOWN"); v != "" {
		c.Cooldown.ErrCooldown = v
	}
	if v := os.Getenv("W2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
}

// Normalize 校验并补齐默认值。
func (c *Config) Normalize() error {
	var err error
	if c.HardCreditDur, err = time.ParseDuration(c.Cooldown.HardCredit); err != nil {
		return fmt.Errorf("cooldown.hard_credit: %w", err)
	}
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	if c.ErrCooldownDur, err = time.ParseDuration(c.Cooldown.ErrCooldown); err != nil {
		return fmt.Errorf("cooldown.err_cooldown: %w", err)
	}
	if c.Cooldown.ErrThresh <= 0 {
		c.Cooldown.ErrThresh = 3
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	if c.Listen.Port <= 0 {
		c.Listen.Port = 7865
	}
	if strings.TrimSpace(c.AuthDir) == "" {
		c.AuthDir = "./auths"
	}
	if strings.TrimSpace(c.StateFile) == "" {
		c.StateFile = "./data/state.json"
	}
	mins, err := ParseClockTimes(c.Schedule.CheckinTimes)
	if err != nil {
		return fmt.Errorf("schedule.checkin_times: %w", err)
	}
	c.Schedule.CheckinTimes = FormatClockTimes(mins)
	if len(c.Schedule.KeepaliveHours) == 0 {
		c.Schedule.KeepaliveHours = []int{22}
	}
	return nil
}
