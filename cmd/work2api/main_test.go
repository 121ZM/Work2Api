package main

import (
	"os"
	"path/filepath"
	"testing"
)

// sameDir 用 os.SameFile 比较，避开 Windows 短路径（DEV~1）与大小写差异 ——
// 直接比字符串会在某些 %TEMP% 配置下假失败。
func sameDir(t *testing.T, a, b string) bool {
	t.Helper()
	fa, err := os.Stat(a)
	if err != nil {
		t.Fatalf("stat %s: %v", a, err)
	}
	fb, err := os.Stat(b)
	if err != nil {
		t.Fatalf("stat %s: %v", b, err)
	}
	return os.SameFile(fa, fb)
}

// 发布包布局 + 从 bin\ 启动（双击）→ 进程必须切到包根。
//
// 这是那个静默缺陷的回归测试：不切的话 config.json 落进 bin\，
// 而托盘启动时写的是 <包根>\config.json —— 两份配置、两个 api_key。
func TestEnterDataRootForSwitchesToPackageRoot(t *testing.T) {
	pkg := t.TempDir()
	bin := filepath.Join(pkg, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "config.example.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Chdir(bin)
	got := enterDataRootFor(filepath.Join(bin, "work2api.exe"))
	if got == "" {
		t.Fatal("从 bin\\ 启动时应切到数据根，却什么都没做")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if !sameDir(t, cwd, pkg) {
		t.Fatalf("切换后 cwd = %q，期望包根 %q", cwd, pkg)
	}
}

// 已经在数据根里 → 不动（返回空），保证 dev 里 `cd <repo> && dist\work2api.exe serve`
// 这类现有用法一字不变。
func TestEnterDataRootForNoopWhenAlreadyRoot(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Chdir(repo)
	if got := enterDataRootFor(filepath.Join(repo, "dist", "work2api.exe")); got != "" {
		t.Fatalf("cwd 已是数据根时不该切换，却返回 %q", got)
	}
	cwd, _ := os.Getwd()
	if !sameDir(t, cwd, repo) {
		t.Fatalf("cwd 被改动了：%q", cwd)
	}
}

// 显式指定 -config 时不切 cwd：那会改变他配置里相对路径的含义 —— 是改语义，不是修 bug。
func TestEnterDataRootRespectsExplicitConfig(t *testing.T) {
	pkg := t.TempDir()
	bin := filepath.Join(pkg, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "config.example.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Chdir(bin)
	enterDataRoot(true) // 显式指定了 -config
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if !sameDir(t, cwd, bin) {
		t.Fatalf("显式 -config 时 cwd 不该被改，实际 = %q，期望 %q", cwd, bin)
	}
}
