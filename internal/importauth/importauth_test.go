package importauth

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"work2api/internal/auth"
	"work2api/internal/region"
)

// writeInfo 在 dir 下写一个客户端 .info 文件，返回其路径。
// expiresAt 故意用**毫秒**，与真实客户端一致。
func writeInfo(t *testing.T, dir, name, domain, accessToken, uid string) string {
	t.Helper()
	doc := map[string]any{
		"account": map[string]any{"uid": uid, "nickname": "测试账号"},
		"auth": map[string]any{
			"accessToken":  accessToken,
			"refreshToken": "rt-" + uid,
			"expiresAt":    int64(1758000000000),
			"domain":       domain,
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func cnSource() source     { return workBuddySources[0] }
func globalSource() source { return workBuddySources[1] }

// ---- region 交叉校验（最关键的安全不变量）----

// TestLoadRejectsRegionMismatch 文件名判定与 auth.domain 判定冲突时必须拒收。
//
// 错导会把国内版账号发往国际版端点（或反之）—— 请求全 401，
// 而且账号会被反复打进冷却，排查起来很绕。
func TestLoadRejectsRegionMismatch(t *testing.T) {
	dir := t.TempDir()
	// 文件名说 CN，domain 却是国际版
	writeInfo(t, dir, "workbuddy-desktop.info", "www.workbuddy.ai", "tok", "u1")

	cand, rej := loadWorkBuddySource(dir, cnSource())

	if cand != nil {
		t.Error("region 冲突时必须拒收，不该产生候选账号")
	}
	if rej == nil {
		t.Fatal("region 冲突时应返回拒绝项")
	}
	if rej.Action != ActionRejected {
		t.Errorf("Action 应为 rejected，实际 %s", rej.Action)
	}
	if !strings.Contains(rej.Reason, "region 冲突") {
		t.Errorf("拒绝原因应说明 region 冲突，实际 %q", rej.Reason)
	}
}

// TestLoadAcceptsMatchingRegion 两版的正常组合都要能通过。
func TestLoadAcceptsMatchingRegion(t *testing.T) {
	cases := []struct {
		file   string
		domain string
		src    source
		want   region.Region
	}{
		{"workbuddy-desktop.info", "www.workbuddy.cn", cnSource(), region.CN},
		{"workbuddy-desktop-ai.info", "www.workbuddy.ai", globalSource(), region.Global},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		writeInfo(t, dir, tc.file, tc.domain, "tok-"+tc.want.String(), "u-"+tc.want.String())

		cand, rej := loadWorkBuddySource(dir, tc.src)
		if rej != nil {
			t.Fatalf("%s 不该被拒收：%s", tc.file, rej.Reason)
		}
		if cand == nil {
			t.Fatalf("%s 应产生候选", tc.file)
		}
		if cand.Region != tc.want {
			t.Errorf("region 应为 %s，实际 %s", tc.want, cand.Region)
		}
		if cand.Kind != auth.KindWorkBuddy {
			t.Errorf("kind 应为 %s，实际 %s", auth.KindWorkBuddy, cand.Kind)
		}
	}
}

// TestLoadNormalizesExpiresAt 客户端的 expiresAt 是 13 位毫秒，必须归一为秒。
func TestLoadNormalizesExpiresAt(t *testing.T) {
	dir := t.TempDir()
	writeInfo(t, dir, "workbuddy-desktop.info", "www.workbuddy.cn", "tok", "u1")

	cand, rej := loadWorkBuddySource(dir, cnSource())
	if rej != nil || cand == nil {
		t.Fatalf("前提不成立：rej=%v", rej)
	}
	if cand.ExpiresAt != 1758000000 {
		t.Errorf("expiresAt 应归一为秒 1758000000，实际 %d", cand.ExpiresAt)
	}
}

// ---- 文件名必须精确匹配 ----

// TestLoadIgnoresRotationBackups 客户端会在同目录留下轮转备份
// （workbuddy-desktop.<ISO时间戳>.<pid>.<uuid>.info）。只放备份时不该读到任何东西。
func TestLoadIgnoresRotationBackups(t *testing.T) {
	dir := t.TempDir()
	writeInfo(t, dir, "workbuddy-desktop.2026-09-17T10-00-00.1234.abcd1234.info",
		"www.workbuddy.cn", "stale-tok", "u-old")

	cand, rej := loadWorkBuddySource(dir, cnSource())

	if cand != nil {
		t.Error("轮转备份不该被读到（必须精确匹配文件名，不要 glob）")
	}
	if rej != nil {
		t.Error("轮转备份存在不算错误")
	}
}

// TestLoadMissingFileIsNotAnError 文件不存在返回 (nil, nil)，不是错误。
func TestLoadMissingFileIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	cand, rej := loadWorkBuddySource(dir, cnSource())
	if cand != nil || rej != nil {
		t.Errorf("文件不存在时应返回 (nil, nil)，实际 cand=%v rej=%v", cand, rej)
	}
}

// ---- 字段校验 ----

func TestLoadRejectsMissingFields(t *testing.T) {
	t.Run("缺 accessToken", func(t *testing.T) {
		dir := t.TempDir()
		writeInfo(t, dir, "workbuddy-desktop.info", "www.workbuddy.cn", "", "u1")
		cand, rej := loadWorkBuddySource(dir, cnSource())
		if cand != nil {
			t.Error("缺 accessToken 不该产生候选")
		}
		if rej == nil || !strings.Contains(rej.Reason, "accessToken") {
			t.Errorf("拒绝原因应提到 accessToken，实际 %v", rej)
		}
	})

	t.Run("缺 uid", func(t *testing.T) {
		dir := t.TempDir()
		writeInfo(t, dir, "workbuddy-desktop.info", "www.workbuddy.cn", "tok", "")
		cand, rej := loadWorkBuddySource(dir, cnSource())
		if cand != nil {
			t.Error("缺 uid 不该产生候选")
		}
		if rej == nil || !strings.Contains(rej.Reason, "uid") {
			t.Errorf("拒绝原因应提到 uid，实际 %v", rej)
		}
	})

	t.Run("非法 JSON", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "workbuddy-desktop.info"),
			[]byte("not json at all"), 0o600); err != nil {
			t.Fatal(err)
		}
		cand, rej := loadWorkBuddySource(dir, cnSource())
		if cand != nil {
			t.Error("非法 JSON 不该产生候选")
		}
		if rej == nil || !strings.Contains(rej.Reason, "解析失败") {
			t.Errorf("拒绝原因应说明解析失败，实际 %v", rej)
		}
	})
}

// ---- 导入：单向 + 覆盖语义 ----

// TestImportDoesNotTouchSourceFile 导入**绝不能**改写客户端源文件。
//
// 这是本包最核心的不变量：客户端会原子覆写 .info，双向写会互相覆盖登录态，
// 且 refresh token 轮换会让某一方被吊销（把用户桌面端踢下线）。
func TestImportDoesNotTouchSourceFile(t *testing.T) {
	srcDir := t.TempDir()
	srcPath := writeInfo(t, srcDir, "workbuddy-desktop.info", "www.workbuddy.cn", "tok", "u1")
	before, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}

	cand, rej := loadWorkBuddySource(srcDir, cnSource())
	if rej != nil || cand == nil {
		t.Fatalf("前提不成立：rej=%v", rej)
	}

	dstDir := filepath.Join(t.TempDir(), "auths")
	results, err := Import([]*auth.Auth{cand}, dstDir)
	if err != nil {
		t.Fatalf("Import 出错: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("应有 1 条结果，实际 %d", len(results))
	}
	if results[0].Action != ActionCreated {
		t.Errorf("首次导入应为 created，实际 %s", results[0].Action)
	}

	after, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("导入改写了客户端源文件 —— 违反单向导入不变量")
	}

	if _, err := os.Stat(results[0].Path); err != nil {
		t.Errorf("目标文件应已写入 %s：%v", results[0].Path, err)
	}
}

// TestImportSecondTimeIsUpdate 重复导入是快照刷新语义 → updated。
func TestImportSecondTimeIsUpdate(t *testing.T) {
	srcDir := t.TempDir()
	writeInfo(t, srcDir, "workbuddy-desktop.info", "www.workbuddy.cn", "tok", "u1")
	cand, rej := loadWorkBuddySource(srcDir, cnSource())
	if rej != nil || cand == nil {
		t.Fatalf("前提不成立：rej=%v", rej)
	}

	dstDir := filepath.Join(t.TempDir(), "auths")
	if _, err := Import([]*auth.Auth{cand}, dstDir); err != nil {
		t.Fatal(err)
	}
	results, err := Import([]*auth.Auth{cand}, dstDir)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Action != ActionUpdated {
		t.Errorf("二次导入应为 updated，实际 %s", results[0].Action)
	}
}

// TestImportTargetPathFollowsFileName 目标路径遵循 <kind>-<region>-<uid>.json。
func TestImportTargetPathFollowsFileName(t *testing.T) {
	srcDir := t.TempDir()
	writeInfo(t, srcDir, "workbuddy-desktop.info", "www.workbuddy.cn", "tok", "uid-xyz")
	cand, _ := loadWorkBuddySource(srcDir, cnSource())
	if cand == nil {
		t.Fatal("前提不成立")
	}

	dstDir := filepath.Join(t.TempDir(), "auths")
	results, err := Import([]*auth.Auth{cand}, dstDir)
	if err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(dstDir, "workbuddy-cn-uid-xyz.json")
	if results[0].Path != want {
		t.Errorf("目标路径应为 %s，实际 %s", want, results[0].Path)
	}
}

// ---- Scan：双版本共存 ----

// TestScanFindsBothRegions 两个客户端文件位于同一目录、文件名后缀区分版本，
// 应各自被正确识别为对应 region。
func TestScanFindsBothRegions(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "CodeBuddyExtension", "Data", "Public", "auth")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeInfo(t, dir, "workbuddy-desktop.info", "www.workbuddy.cn", "tok-cn", "u-cn")
	writeInfo(t, dir, "workbuddy-desktop-ai.info", "www.workbuddy.ai", "tok-gl", "u-gl")
	t.Setenv("LOCALAPPDATA", root)

	res := Scan()

	if len(res.Candidates) != 2 {
		t.Fatalf("应找到 2 个候选，实际 %d（notes=%v rejected=%v）",
			len(res.Candidates), res.Notes, res.Rejected)
	}
	got := map[region.Region]string{}
	for _, c := range res.Candidates {
		got[c.Region] = c.UID
	}
	if got[region.CN] != "u-cn" {
		t.Errorf("CN 候选应为 u-cn，实际 %q", got[region.CN])
	}
	if got[region.Global] != "u-gl" {
		t.Errorf("Global 候选应为 u-gl，实际 %q", got[region.Global])
	}
}

// TestScanAlwaysNotesTraeUnsupported TRAE 无法导入这件事必须始终出现在说明里。
func TestScanAlwaysNotesTraeUnsupported(t *testing.T) {
	t.Setenv("LOCALAPPDATA", filepath.Join(t.TempDir(), "no-such-dir"))

	res := Scan()

	found := false
	for _, n := range res.Notes {
		if strings.Contains(n, "TRAE") {
			found = true
		}
	}
	if !found {
		t.Errorf("应始终提示 TRAE 不支持导入，实际 notes=%v", res.Notes)
	}
	if len(res.Candidates) != 0 {
		t.Errorf("目录不存在时不该有候选，实际 %d", len(res.Candidates))
	}
}
