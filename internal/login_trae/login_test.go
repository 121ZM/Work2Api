package login_trae

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"work2api/internal/auth"
	"work2api/internal/region"
	"work2api/internal/traework"
)

// TestParseCallbackRefreshToken 标准回调：refreshToken 直给。
func TestParseCallbackRefreshToken(t *testing.T) {
	got := ParseCallback("http://127.0.0.1:9999/authorize?refreshToken=rt-abc&host=https://api.trae.cn")
	if got.RefreshToken != "rt-abc" {
		t.Fatalf("refreshToken = %q, want rt-abc", got.RefreshToken)
	}
	if got.AuthCode != "" {
		t.Fatalf("authCode should be empty, got %q", got.AuthCode)
	}
}

// TestParseCallbackUserJwt 无 refreshToken 时回退 userJwt.RefreshToken。
func TestParseCallbackUserJwt(t *testing.T) {
	jwt := `{"RefreshToken":"rt-from-jwt","Token":"at-from-jwt"}`
	raw := "http://127.0.0.1:1/authorize?userJwt=" + url.QueryEscape(jwt)
	got := ParseCallback(raw)
	if got.RefreshToken != "rt-from-jwt" {
		t.Fatalf("refreshToken = %q, want rt-from-jwt", got.RefreshToken)
	}
}

// TestParseCallbackUserJwtTokenOnly 只有 Token 时作为 accessToken 兜底。
func TestParseCallbackUserJwtTokenOnly(t *testing.T) {
	jwt := `{"Token":"at-only"}`
	raw := "http://127.0.0.1:1/authorize?userJwt=" + url.QueryEscape(jwt)
	got := ParseCallback(raw)
	if got.AccessToken != "at-only" {
		t.Fatalf("accessToken = %q, want at-only", got.AccessToken)
	}
	if got.RefreshToken != "" {
		t.Fatalf("refreshToken should be empty, got %q", got.RefreshToken)
	}
}

// TestParseCallbackAuthCode PKCE 新流程：authCodeInfo 为 JSON 对象。
func TestParseCallbackAuthCode(t *testing.T) {
	info := `{"AuthCode":"ac-123","ExpireAt":1794791195}`
	raw := "http://127.0.0.1:1/authorize?authCodeInfo=" + url.QueryEscape(info)
	got := ParseCallback(raw)
	if got.AuthCode != "ac-123" {
		t.Fatalf("authCode = %q, want ac-123", got.AuthCode)
	}
}

// TestParseCallbackAuthCodeNestedResult authCodeInfo 里嵌 Result 容器。
func TestParseCallbackAuthCodeNestedResult(t *testing.T) {
	info := `{"Result":{"AuthCode":"ac-nested"}}`
	got := ParseCallback("http://127.0.0.1:1/authorize?authCodeInfo=" + url.QueryEscape(info))
	if got.AuthCode != "ac-nested" {
		t.Fatalf("authCode = %q, want ac-nested", got.AuthCode)
	}
}

// TestParseCallbackAuthCodeBare 非 JSON 的 authCodeInfo 视为 code 本身。
func TestParseCallbackAuthCodeBare(t *testing.T) {
	got := ParseCallback("http://127.0.0.1:1/authorize?authCodeInfo=ac-bare")
	if got.AuthCode != "ac-bare" {
		t.Fatalf("authCode = %q, want ac-bare", got.AuthCode)
	}
}

// TestParseCallbackEmpty 空输入不 panic。
func TestParseCallbackEmpty(t *testing.T) {
	if got := ParseCallback(""); got != (CallbackInfo{}) {
		t.Fatalf("empty input should yield zero value, got %+v", got)
	}
	if got := ParseCallback("not a url at all \x7f"); got != (CallbackInfo{}) {
		t.Fatalf("malformed input should yield zero value, got %+v", got)
	}
}

// TestGenPKCE PKCE 的 verifier / challenge 必须配对且每次不同。
func TestGenPKCE(t *testing.T) {
	v1, c1 := traework.GenPKCE()
	v2, c2 := traework.GenPKCE()
	if v1 == "" || c1 == "" {
		t.Fatal("GenPKCE returned empty")
	}
	if v1 == v2 || c1 == c2 {
		t.Fatal("GenPKCE should not repeat across calls")
	}
	// challenge 必须是 verifier 的 S256，长度固定 43（32 字节 base64url 无填充）
	if len(c1) != 43 {
		t.Fatalf("challenge len = %d, want 43", len(c1))
	}
	if len(v1) != 64 { // 48 字节 base64url 无填充
		t.Fatalf("verifier len = %d, want 64", len(v1))
	}
}

// TestDomainFor region → domain 映射必须让 auth.RegionFromDomain 反解回同一 region。
// 这是「两区账号共存」不被写错的关键闭环。
func TestDomainForRegionRoundTrip(t *testing.T) {
	for _, r := range []region.Region{region.CN, region.Global} {
		d := domainFor(r)
		if got := auth.RegionFromDomain(d); got != r {
			t.Fatalf("domainFor(%s)=%q, RegionFromDomain -> %s (round trip broken)", r, d, got)
		}
	}
}

// TestStateRoundTrip state 落盘 / 读取后 region 与 PKCE verifier 不丢。
func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trae-state.json")
	in := state{
		Region:       region.Global.String(),
		MachineID:    "m" + "0",
		DeviceID:     "123456789012345",
		CodeVerifier: "verifier-xyz",
	}
	if err := writeState(path, in); err != nil {
		t.Fatalf("writeState: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var out state
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if out.Region != region.Global.String() || out.CodeVerifier != "verifier-xyz" {
		t.Fatalf("state round trip lost fields: %+v", out)
	}
	// Poll 前必须能判定 region，否则会用错域名
	if region.Parse(out.Region) != region.Global {
		t.Fatalf("region.Parse(%q) != Global", out.Region)
	}
}

// TestRandNumericID 设备 ID 必须是 15 位纯数字（对齐官方客户端格式）。
func TestRandNumericID(t *testing.T) {
	for i := 0; i < 50; i++ {
		id := randNumericID()
		if len(id) != 15 {
			t.Fatalf("randNumericID len = %d (%q), want 15", len(id), id)
		}
		for _, ch := range id {
			if ch < '0' || ch > '9' {
				t.Fatalf("randNumericID %q contains non-digit %q", id, ch)
			}
		}
	}
}

// TestNewUUIDFormat login_trace_id 必须是 36 字符 v4 UUID。
func TestNewUUIDFormat(t *testing.T) {
	u := newUUID()
	if len(u) != 36 {
		t.Fatalf("newUUID len = %d (%q), want 36", len(u), u)
	}
	if u[14] != '4' {
		t.Fatalf("newUUID version nibble = %q, want 4", u[14])
	}
	if u[8] != '-' || u[13] != '-' || u[18] != '-' || u[23] != '-' {
		t.Fatalf("newUUID dashes misplaced: %q", u)
	}
}
