package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// apiKeyRe 期望形态：w2a_ + 32 位小写十六进制（128 位随机）。
var apiKeyRe = regexp.MustCompile(`^w2a_[0-9a-f]{32}$`)

func TestGenerateAPIKeyFormat(t *testing.T) {
	k := GenerateAPIKey()
	if !apiKeyRe.MatchString(k) {
		t.Fatalf("GenerateAPIKey() = %q，期望 w2a_ + 32 位十六进制", k)
	}
}

func TestGenerateAPIKeyIsRandom(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		k := GenerateAPIKey()
		if seen[k] {
			t.Fatalf("GenerateAPIKey() 出现重复值 %q，随机性存疑", k)
		}
		seen[k] = true
	}
}

// 首次运行：文件里 api_key 为空 → 生成随机 Key，并**写回文件**。
// 这是本次改动的核心保证 —— 不落盘的话每次重启都换 Key，已配置的客户端全部 401。
func TestLoadGeneratesAndPersistsAPIKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"api_key":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("W2A_API_KEY", "")

	first, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !apiKeyRe.MatchString(first.APIKey) {
		t.Fatalf("生成的 Key = %q，形态不对", first.APIKey)
	}

	// 文件里必须已经写上了同一个 Key
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk struct {
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("写回的配置不是合法 JSON: %v", err)
	}
	if onDisk.APIKey != first.APIKey {
		t.Fatalf("文件里的 key=%q，内存里=%q，未落盘", onDisk.APIKey, first.APIKey)
	}

	// 再次加载必须拿到同一个 Key（跨重启稳定）
	second, err := Load(path)
	if err != nil {
		t.Fatalf("二次 Load: %v", err)
	}
	if second.APIKey != first.APIKey {
		t.Fatalf("重启后 Key 变了：%q → %q", first.APIKey, second.APIKey)
	}
}

// 文件里已有 Key → 原样保留，不重新生成。
func TestLoadKeepsExistingAPIKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	const want = "w2a_00000000000000000000000000000000"
	if err := os.WriteFile(path, []byte(`{"api_key":"`+want+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("W2A_API_KEY", "")

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.APIKey != want {
		t.Fatalf("APIKey = %q，期望保留原值 %q", c.APIKey, want)
	}
}

// W2A_API_KEY 优先级高于文件，且**不能**被写进文件覆盖掉用户配置。
func TestEnvOverridesFileAndIsNotPersisted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	const fileKey = "w2a_11111111111111111111111111111111"
	const envKey = "w2a_22222222222222222222222222222222"
	if err := os.WriteFile(path, []byte(`{"api_key":"`+fileKey+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("W2A_API_KEY", envKey)

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.APIKey != envKey {
		t.Fatalf("APIKey = %q，期望环境变量值 %q", c.APIKey, envKey)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), fileKey) {
		t.Fatalf("文件里的 key 被环境变量覆盖写掉了：%s", raw)
	}
}

// path 为空（无配置文件）时也要给出可用的随机 Key，只是不落盘。
func TestLoadWithoutFileStillGeneratesKey(t *testing.T) {
	t.Setenv("W2A_API_KEY", "")
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if !apiKeyRe.MatchString(c.APIKey) {
		t.Fatalf("APIKey = %q，期望随机生成的 Key", c.APIKey)
	}
}

// 默认配置不再带硬编码 Key —— 硬编码默认值等于没有鉴权。
func TestDefaultHasNoHardcodedKey(t *testing.T) {
	if k := Default().APIKey; k != "" {
		t.Fatalf("Default().APIKey = %q，期望为空（由 Load 生成）", k)
	}
}

// 示例配置里的 _comment_* 说明字段不应导致解析失败。
func TestUnknownFieldsAreIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"_comment_api_key":"留空即自动生成","api_key":"","listen":{"host":"127.0.0.1","port":7865}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("W2A_API_KEY", "")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load 应忽略未知字段，却报错: %v", err)
	}
	if c.Listen.Port != 7865 {
		t.Fatalf("Listen.Port = %d，期望 7865", c.Listen.Port)
	}
	if !apiKeyRe.MatchString(c.APIKey) {
		t.Fatalf("APIKey = %q，期望自动生成", c.APIKey)
	}
}
