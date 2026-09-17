package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"work2api/internal/region"
)

// TestNormalizeEpoch 覆盖实测数据：客户端 .info 的 expiresAt 是 13 位毫秒。
func TestNormalizeEpoch(t *testing.T) {
	cases := []struct {
		name string
		in   int64
		want int64
	}{
		{"毫秒（实测国内版）", 1794791195362, 1794791195},
		{"毫秒（实测国际版）", 1820653502014, 1820653502},
		{"已是秒", 1794791195, 1794791195},
		{"零值", 0, 0},
	}
	for _, c := range cases {
		if got := NormalizeEpoch(c.in); got != c.want {
			t.Errorf("%s: NormalizeEpoch(%d) = %d, want %d", c.name, c.in, got, c.want)
		}
	}
}

// TestRegionFromDomain 覆盖本机两个客户端实测 domain。
func TestRegionFromDomain(t *testing.T) {
	cases := map[string]region.Region{
		"www.workbuddy.cn": region.CN,
		"www.codebuddy.cn": region.CN,
		"www.workbuddy.ai": region.Global,
		"www.codebuddy.ai": region.Global,
		"www.trae.ai":      region.Global,
		"":                 region.CN,
		"unknown.example":  region.CN,
	}
	for domain, want := range cases {
		if got := RegionFromDomain(domain); got != want {
			t.Errorf("RegionFromDomain(%q) = %s, want %s", domain, got, want)
		}
	}
}

// TestParseNestedMillisAndRegion 验证嵌套形解析同时完成毫秒归一与 region 定值。
func TestParseNestedMillisAndRegion(t *testing.T) {
	raw := []byte(`{
	  "account": {"uid":"u-1","nickname":"Z"},
	  "auth": {"accessToken":"tok","refreshToken":"ref","expiresAt":1794791195362,
	           "domain":"www.workbuddy.cn"},
	  "accounts": [{"uid":"u-1"}],
	  "allAccounts": [{"uid":"u-1"},{"uid":"u-2"}]
	}`)
	a, err := Parse(raw, KindWorkBuddy)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.ExpiresAt != 1794791195 {
		t.Errorf("ExpiresAt = %d, want 1794791195（毫秒应归一为秒）", a.ExpiresAt)
	}
	if a.Region != region.CN {
		t.Errorf("Region = %s, want cn", a.Region)
	}
	if a.UID != "u-1" || a.Nickname != "Z" {
		t.Errorf("账号字段解析错误: uid=%q nickname=%q", a.UID, a.Nickname)
	}
	if a.Kind != KindWorkBuddy {
		t.Errorf("Kind = %q, want %q", a.Kind, KindWorkBuddy)
	}
}

// TestParseGlobalByDomain 未显式给 region 时按 domain 兜底为 global。
func TestParseGlobalByDomain(t *testing.T) {
	raw := []byte(`{"accessToken":"tok","uid":"u-2","domain":"www.workbuddy.ai"}`)
	a, err := Parse(raw, KindWorkBuddy)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.Region != region.Global {
		t.Errorf("Region = %s, want global", a.Region)
	}
}

// TestParseMissingToken 缺 accessToken 必须报错，不能静默产出空凭证。
func TestParseMissingToken(t *testing.T) {
	if _, err := Parse([]byte(`{"account":{"uid":"u"}}`), KindWorkBuddy); err == nil {
		t.Fatal("缺少 accessToken 时应报错")
	}
	if _, err := Parse(nil, KindWorkBuddy); err == nil {
		t.Fatal("空内容应报错")
	}
}

// TestSaveLoadRoundTrip 锁住「写回格式 == 读取格式」这一关键不变量。
// 若 SaveAtomicAs 与 Parse 失配，token 刷新后会写出一份自己读不回来的文件。
func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	orig := &Auth{
		Kind:         KindWorkBuddy,
		Region:       region.Global,
		AccessToken:  "access-token-value",
		RefreshToken: "refresh-token-value",
		ExpiresAt:    1820653502,
		Domain:       "www.workbuddy.ai",
		UID:          "u-rt",
		Nickname:     "rt",
		EnterpriseID: "ent-1",
	}
	path := filepath.Join(dir, FileName(orig.Kind, orig.Region, orig.UID))
	if err := orig.SaveAtomicAs(path); err != nil {
		t.Fatalf("SaveAtomicAs: %v", err)
	}

	got, err := LoadFile(path, KindWorkBuddy)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got.AccessToken != orig.AccessToken || got.RefreshToken != orig.RefreshToken {
		t.Error("token 往返丢失")
	}
	if got.ExpiresAt != orig.ExpiresAt {
		t.Errorf("ExpiresAt = %d, want %d（二次归一不应改变秒级值）", got.ExpiresAt, orig.ExpiresAt)
	}
	if got.Region != orig.Region {
		t.Errorf("Region = %s, want %s", got.Region, orig.Region)
	}
	if got.UID != orig.UID || got.Nickname != orig.Nickname || got.EnterpriseID != orig.EnterpriseID {
		t.Error("账号字段往返丢失")
	}
	if got.FilePath != path {
		t.Errorf("FilePath = %q, want %q", got.FilePath, path)
	}
	if got.Key() != "workbuddy|global|u-rt" {
		t.Errorf("Key() = %q, 应含 region 段以防两区 UID 撞车", got.Key())
	}
}

// TestLoadDirKeepsBothRegions 锁住核心决策：加载**不过滤 region**，两区共存。
func TestLoadDirKeepsBothRegions(t *testing.T) {
	dir := t.TempDir()
	mk := func(r region.Region, uid, domain string) {
		a := &Auth{
			Kind: KindWorkBuddy, Region: r, AccessToken: "t-" + uid,
			RefreshToken: "r", ExpiresAt: 1794791195, Domain: domain, UID: uid,
		}
		p := filepath.Join(dir, FileName(KindWorkBuddy, r, uid))
		if err := a.SaveAtomicAs(p); err != nil {
			t.Fatalf("SaveAtomicAs: %v", err)
		}
	}
	mk(region.CN, "uid-cn", "www.workbuddy.cn")
	mk(region.Global, "uid-global", "www.workbuddy.ai")

	// 干扰项：不应被当作凭证加载
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-cn-broken.json.tmp"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-cn-bad.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(got) != 2 {
		raw, _ := json.Marshal(names(got))
		t.Fatalf("加载到 %d 个账号，want 2（两区应共存）: %s", len(got), raw)
	}
	regions := map[region.Region]bool{}
	for _, a := range got {
		regions[a.Region] = true
	}
	if !regions[region.CN] || !regions[region.Global] {
		t.Fatalf("两区未同时加载: %v", regions)
	}
}

// TestNeedsRefresh 过期时间缺失时必须要求刷新。
func TestNeedsRefresh(t *testing.T) {
	fresh := &Auth{ExpiresAt: time.Now().Add(time.Hour).Unix()}
	if fresh.NeedsRefresh(10 * time.Minute) {
		t.Error("1 小时后过期不应要求刷新")
	}
	soon := &Auth{ExpiresAt: time.Now().Add(time.Minute).Unix()}
	if !soon.NeedsRefresh(10 * time.Minute) {
		t.Error("1 分钟后过期应要求刷新")
	}
	none := &Auth{}
	if !none.NeedsRefresh(10 * time.Minute) {
		t.Error("无过期时间应要求刷新")
	}
}

func names(list []*Auth) []string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.Kind+"|"+a.Region.String()+"|"+a.UID)
	}
	return out
}
