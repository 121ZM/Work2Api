package app

import (
	"path/filepath"
	"testing"

	"work2api/internal/auth"
	"work2api/internal/region"
)

// writeCred 在 dir 下落一份凭证文件，返回路径。
func writeCred(t *testing.T, dir, name, uid string, r region.Region) string {
	t.Helper()
	a := &auth.Auth{
		Kind:        auth.KindWorkBuddy,
		Region:      r,
		AccessToken: "tok-" + uid,
		UID:         uid,
		Nickname:    uid,
	}
	fp := filepath.Join(dir, name)
	if err := a.SaveAtomicAs(fp); err != nil {
		t.Fatalf("写凭证 %s: %v", fp, err)
	}
	return fp
}

// TestSiblingCredFilesFindsSameAccount 锁住删除账号时的「同账号多份凭证」处理。
//
// 场景：同一个账号在 auths/ 下有两份文件（登录写一份、手工导入又放一份，
// 文件名不同但 UID 相同）。只删 FilePath 那一份的话，重启后账号会复活。
func TestSiblingCredFilesFindsSameAccount(t *testing.T) {
	dir := t.TempDir()
	primary := writeCred(t, dir, "workbuddy-cn-u1.json", "u1", region.CN)
	dup := writeCred(t, dir, "workbuddy-backup.json", "u1", region.CN)
	other := writeCred(t, dir, "workbuddy-cn-u2.json", "u2", region.CN)

	acct, err := auth.LoadFile(primary, auth.KindWorkBuddy)
	if err != nil {
		t.Fatalf("加载主凭证: %v", err)
	}
	got := siblingCredFiles(dir, acct, []string{primary})

	if len(got) != 1 || filepath.Base(got[0]) != filepath.Base(dup) {
		t.Fatalf("应只找到同账号的另一份凭证 %s，实际 %v", filepath.Base(dup), got)
	}
	for _, f := range got {
		if filepath.Clean(f) == filepath.Clean(other) {
			t.Fatal("误把别的账号当成同账号 —— 会删掉无关凭证")
		}
	}
}

// TestSiblingCredFilesIgnoresOtherRegion 同 UID 但不同版本不能算同一个账号：
// 两区账号库互相独立，删国内版不该动国际版。
func TestSiblingCredFilesIgnoresOtherRegion(t *testing.T) {
	dir := t.TempDir()
	primary := writeCred(t, dir, "workbuddy-cn-u1.json", "u1", region.CN)
	writeCred(t, dir, "workbuddy-global-u1.json", "u1", region.Global)

	acct, err := auth.LoadFile(primary, auth.KindWorkBuddy)
	if err != nil {
		t.Fatalf("加载主凭证: %v", err)
	}
	if got := siblingCredFiles(dir, acct, []string{primary}); len(got) != 0 {
		t.Fatalf("不该跨版本匹配，实际找到 %v", got)
	}
}

// TestSiblingCredFilesEmptyDir 目录为空 / 不存在时返回空，不 panic。
func TestSiblingCredFilesEmptyDir(t *testing.T) {
	acct := &auth.Auth{Kind: auth.KindWorkBuddy, Region: region.CN, UID: "u1"}
	if got := siblingCredFiles(filepath.Join(t.TempDir(), "nope"), acct, nil); len(got) != 0 {
		t.Fatalf("目录不存在时应返回空，实际 %v", got)
	}
	if got := siblingCredFiles("", acct, nil); len(got) != 0 {
		t.Fatalf("空目录参数应返回空，实际 %v", got)
	}
}

// TestSiblingCredFilesSkipsExcluded 已经算进删除列表的路径不重复返回。
func TestSiblingCredFilesSkipsExcluded(t *testing.T) {
	dir := t.TempDir()
	primary := writeCred(t, dir, "workbuddy-cn-u1.json", "u1", region.CN)

	acct, err := auth.LoadFile(primary, auth.KindWorkBuddy)
	if err != nil {
		t.Fatalf("加载主凭证: %v", err)
	}
	// 先确认这份文件确实能被扫到（排除列表为空）
	if got := siblingCredFiles(dir, acct, nil); len(got) != 1 {
		t.Fatalf("排除列表为空时应扫到自身，实际 %v", got)
	}
	if got := siblingCredFiles(dir, acct, []string{primary}); len(got) != 0 {
		t.Fatalf("排除后不该再返回，实际 %v", got)
	}
	// 排除列表里的路径与扫描结果大小写/分隔符不同也要能对上（filepath.Clean 归一）
	if got := siblingCredFiles(dir, acct, []string{filepath.Join(dir, ".", filepath.Base(primary))}); len(got) != 0 {
		t.Fatalf("路径未归一导致重复返回 %v", got)
	}
}
