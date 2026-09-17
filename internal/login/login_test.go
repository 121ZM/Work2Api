package login

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"work2api/internal/auth"
	"work2api/internal/region"
)

// ---- SaveAuth：region 必须由 domain 判定 ----

// TestSaveAuthRegionComesFromDomain 锁住一条关键不变量：
// **region 取 domain 后缀判定，不由调用方指定**。
//
// 这是「两区账号共存」不被写错的关键 —— 若 region 由调用方传入（或写死），
// 国际版账号可能被写进国内版的账号库，之后请求会被发往错误端点。
func TestSaveAuthRegionComesFromDomain(t *testing.T) {
	cases := []struct {
		domain     string
		wantRegion region.Region
		wantFile   string
	}{
		{"www.workbuddy.ai", region.Global, "workbuddy-global-uid-1.json"},
		{"www.workbuddy.cn", region.CN, "workbuddy-cn-uid-1.json"},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		fp, err := SaveAuth(dir, Result{
			AccessToken:  "at",
			RefreshToken: "rt",
			ExpiresIn:    3600,
			Domain:       tc.domain,
			UID:          "uid-1",
			Nickname:     "测试",
		})
		if err != nil {
			t.Fatalf("domain=%s SaveAuth 出错: %v", tc.domain, err)
		}
		if got := filepath.Base(fp); got != tc.wantFile {
			t.Errorf("domain=%s 应落到 %s，实际 %s", tc.domain, tc.wantFile, got)
		}

		// 读回落盘内容，确认 region 与 domain 一致
		a, err := auth.LoadFile(fp, auth.KindWorkBuddy)
		if err != nil {
			t.Fatalf("读回失败: %v", err)
		}
		if a.Region != tc.wantRegion {
			t.Errorf("domain=%s 落盘的 region 应为 %s，实际 %s", tc.domain, tc.wantRegion, a.Region)
		}
	}
}

// TestSaveAuthRequiresUID 缺 uid 必须报错 —— 没有 uid 就生成不出稳定文件名。
func TestSaveAuthRequiresUID(t *testing.T) {
	if _, err := SaveAuth(t.TempDir(), Result{Domain: "www.workbuddy.cn"}); err == nil {
		t.Error("缺 uid 时应报错")
	}
}

// TestSaveAuthComputesExpiryFromExpiresIn expiresIn（秒）应换算成绝对过期时间。
func TestSaveAuthComputesExpiryFromExpiresIn(t *testing.T) {
	dir := t.TempDir()
	before := time.Now().Unix()

	fp, err := SaveAuth(dir, Result{
		AccessToken: "at", ExpiresIn: 3600,
		Domain: "www.workbuddy.cn", UID: "uid-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	a, err := auth.LoadFile(fp, auth.KindWorkBuddy)
	if err != nil {
		t.Fatal(err)
	}
	want := before + 3600
	if a.ExpiresAt < want-5 || a.ExpiresAt > want+5 {
		t.Errorf("expiresAt 应约为 %d（now+3600），实际 %d", want, a.ExpiresAt)
	}
	if a.AccessToken != "at" {
		t.Errorf("accessToken 未落盘正确，实际 %q", a.AccessToken)
	}
	if a.UID != "uid-1" {
		t.Errorf("uid 未落盘正确，实际 %q", a.UID)
	}
}

// TestSaveAuthZeroExpiresInLeavesExpiryZero expiresIn 缺省时不编造过期时间。
func TestSaveAuthZeroExpiresInLeavesExpiryZero(t *testing.T) {
	fp, err := SaveAuth(t.TempDir(), Result{
		AccessToken: "at", Domain: "www.workbuddy.cn", UID: "uid-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := auth.LoadFile(fp, auth.KindWorkBuddy)
	if err != nil {
		t.Fatal(err)
	}
	if a.ExpiresAt != 0 {
		t.Errorf("未提供 expiresIn 时不该编造过期时间，实际 %d", a.ExpiresAt)
	}
}

// ---- ResolveAuthURL：重定向链 ----

func cnHosts() region.WorkBuddyHosts { return region.WorkBuddy(region.CN, nil) }

// TestResolveAuthURLFollowsRedirectChain 手动跟随重定向链，返回最终 URL。
func TestResolveAuthURLFollowsRedirectChain(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/hop1", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/hop1", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/final", http.StatusFound)
	})
	mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := ResolveAuthURL(NewClient(), cnHosts(), srv.URL+"/start")
	if err != nil {
		t.Fatalf("ResolveAuthURL 出错: %v", err)
	}
	if want := srv.URL + "/final"; got != want {
		t.Errorf("应跟到最终地址 %s，实际 %s", want, got)
	}
}

// TestResolveAuthURLResolvesRelativeLocation 相对 Location（不带前导 /）必须
// 相对**当前**地址解析，而不是当成根路径。
//
// 这是最容易写错的一种重定向：手写字符串拼接会把 "hop2" 拼成 "/hop2" 之外的
// 东西，或丢失中间的路径层级。
func TestResolveAuthURLResolvesRelativeLocation(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/a/b/start", func(w http.ResponseWriter, r *http.Request) {
		// 相对当前目录：应解析为 /a/b/next
		w.Header().Set("Location", "next")
		w.WriteHeader(http.StatusFound)
	})
	mux.HandleFunc("/a/b/next", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := ResolveAuthURL(NewClient(), cnHosts(), srv.URL+"/a/b/start")
	if err != nil {
		t.Fatalf("ResolveAuthURL 出错: %v", err)
	}
	if want := srv.URL + "/a/b/next"; got != want {
		t.Errorf("相对 Location 应相对当前路径解析：期望 %s，实际 %s", want, got)
	}
}

// TestResolveAuthURLReturnsImmediatelyWhenNoRedirect 无 Location 时直接返回原地址。
func TestResolveAuthURLReturnsImmediatelyWhenNoRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	got, err := ResolveAuthURL(NewClient(), cnHosts(), srv.URL+"/plain")
	if err != nil {
		t.Fatal(err)
	}
	if want := srv.URL + "/plain"; got != want {
		t.Errorf("无重定向时应原样返回 %s，实际 %s", want, got)
	}
}

// TestResolveAuthURLStopsAfterMaxHops 无限重定向不能挂住 —— 必须在跳数上限内返回。
func TestResolveAuthURLStopsAfterMaxHops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/loop")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	done := make(chan struct{})
	var got string
	var err error
	go func() {
		got, err = ResolveAuthURL(NewClient(), cnHosts(), srv.URL+"/loop")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("无限重定向应在跳数上限内返回，而不是挂住")
	}
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if want := srv.URL + "/loop"; got != want {
		t.Errorf("应在跳数上限后返回当前地址 %s，实际 %s", want, got)
	}
}
