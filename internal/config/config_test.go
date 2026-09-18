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

// ── 数据根 ──────────────────────────────────────────────────────────────
//
// 这组测试锁的是「同一个发布包不该长出两份 config.json」——
// 那个缺陷是静的：落在 bin\ 的那份有自己的 api_key，已配置的客户端全部 401，
// 而日志里一个错都不打。判据只依赖 (exePath, cwd) 两个入参，所以能纯函数测。

// 每个 marker 单独都要能认出来；目录形态与文件形态都算。
func TestLooksLikeDataRootMarkers(t *testing.T) {
	files := []string{"config.json", "config.local.json", "config.example.json", "go.mod"}
	dirs := []string{"auths", "data"}

	for _, name := range files {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if !LooksLikeDataRoot(dir) {
			t.Errorf("含 %s 的目录应被识别为数据根", name)
		}
	}
	for _, name := range dirs {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if !LooksLikeDataRoot(dir) {
			t.Errorf("含 %s 目录的应被识别为数据根", name)
		}
	}
}

// 反例：空目录、不存在的目录、空串都不能算数据根 ——
// 否则「从任意目录启动」都会被误判成数据根，等于没修。
func TestLooksLikeDataRootRejects(t *testing.T) {
	if LooksLikeDataRoot("") {
		t.Error("空串不该是数据根")
	}
	if LooksLikeDataRoot(filepath.Join(t.TempDir(), "不存在")) {
		t.Error("不存在的目录不该是数据根")
	}
	if LooksLikeDataRoot(t.TempDir()) {
		t.Error("空目录不该是数据根")
	}
}

// 回归：发布包布局。exe 在 <pkg>\bin\，从 bin\ 双击启动（cwd = bin\）。
// 数据根必须是包根，否则 config.json 落进 bin\，与托盘启动时写的
// <pkg>\config.json 变成两份、两个 api_key。
func TestDataRootPackageLayout(t *testing.T) {
	pkg := t.TempDir()
	bin := filepath.Join(pkg, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "config.example.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := DataRoot(filepath.Join(bin, "work2api.exe"), bin); got != pkg {
		t.Fatalf("发布包布局：DataRoot = %q，期望包根 %q", got, pkg)
	}
	// 托盘启动时 cwd 已经是包根 → 原样返回（这条保证现有用法不变）
	if got := DataRoot(filepath.Join(bin, "work2api.exe"), pkg); got != pkg {
		t.Fatalf("cwd 已是包根时 DataRoot = %q，期望 %q", got, pkg)
	}
}

// 回归：dev 仓库布局。exe 在 <repo>\dist\，无论 cwd 是仓库根还是 dist\，
// 数据根都必须是仓库根 —— config.json / auths / data 都在那儿。
func TestDataRootDevLayout(t *testing.T) {
	repo := t.TempDir()
	dist := filepath.Join(repo, "dist")
	if err := os.MkdirAll(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x"), 0o644); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dist, "work2api.exe")

	if got := DataRoot(exe, repo); got != repo {
		t.Fatalf("cwd=仓库根：DataRoot = %q，期望 %q", got, repo)
	}
	if got := DataRoot(exe, dist); got != repo {
		t.Fatalf("cwd=dist：DataRoot = %q，期望 %q", got, repo)
	}
}

// cwd 自己像个数据根就优先用它 —— 用户自己挑了个目录放 config.json 在那里启动，
// 不能被 exe 的位置「纠正」走。这条是「零行为变化」的兜底。
func TestDataRootPrefersCwdOverExe(t *testing.T) {
	userDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(userDir, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	otherPkg := t.TempDir()
	if err := os.WriteFile(filepath.Join(otherPkg, "go.mod"), []byte("module y"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := DataRoot(filepath.Join(otherPkg, "bin", "work2api.exe"), userDir)
	if got != userDir {
		t.Fatalf("DataRoot = %q，期望优先返回 cwd %q", got, userDir)
	}
}

// 单个 exe 被拷到别处（周围什么都没有）：数据落在 exe 旁边，
// 而不是莫名写进它的父目录 —— 也不该 panic 或返回空。
func TestDataRootFallsBackToExeDir(t *testing.T) {
	lonely := t.TempDir()
	exe := filepath.Join(lonely, "work2api.exe")

	if got := DataRoot(exe, lonely); got != lonely {
		t.Fatalf("孤立 exe：DataRoot = %q，期望 exe 所在目录 %q", got, lonely)
	}
	// 拿不到 exe 路径时退化成 cwd，不能返回空
	if got := DataRoot("", lonely); got != lonely {
		t.Fatalf("exePath 为空：DataRoot = %q，期望 %q", got, lonely)
	}
}
