package pool

import (
	"errors"
	"testing"
	"time"

	"work2api/internal/auth"
	"work2api/internal/provider"
	"work2api/internal/region"
)

func testChannel() provider.Channel {
	return provider.Channel{Kind: provider.WorkBuddy, Region: region.CN}
}

func testAuth(uid string) *auth.Auth {
	return &auth.Auth{Kind: "workbuddy", Region: region.CN, UID: uid}
}

// ---- better()：三段排序 ----

// TestBetterPrefersKnownRemain 第一段：已知余额优先于未知余额。
func TestBetterPrefersKnownRemain(t *testing.T) {
	known := &entry{hasRemain: true, remain: 10}
	unknown := &entry{}
	if !better(known, unknown) {
		t.Error("已知余额的账号应优先于未知余额的")
	}
	if better(unknown, known) {
		t.Error("未知余额的账号不该优先于已知余额的")
	}
}

// TestBetterTreatsZeroRemainAsKnown 记录一个**刻意**的行为：
// 「已知余额为 0」仍然优先于「未知余额」—— 未知不等于 0，不能当 0 处理。
//
// 若把 0 和未知混为一谈，选号会退化成「优先试那些余额不明的账号」，
// 与「先烧积分多的」这个目标相反。
func TestBetterTreatsZeroRemainAsKnown(t *testing.T) {
	zero := &entry{hasRemain: true, remain: 0}
	unknown := &entry{}
	if !better(zero, unknown) {
		t.Error("已知余额为 0 的账号应优先于余额未知的账号（未知 != 0）")
	}
}

// TestBetterPrefersLargerRemain 第二段：都有余额时，余额大的优先。
func TestBetterPrefersLargerRemain(t *testing.T) {
	rich := &entry{hasRemain: true, remain: 5000}
	poor := &entry{hasRemain: true, remain: 100}
	if !better(rich, poor) {
		t.Error("余额 5000 的账号应优先于余额 100 的")
	}
	if better(poor, rich) {
		t.Error("余额少的账号不该优先")
	}
}

// TestBetterFallsBackToLRU 第三段：余额相同（或都未知）时，最久未使用的优先。
func TestBetterFallsBackToLRU(t *testing.T) {
	now := time.Now()
	old := &entry{hasRemain: true, remain: 100, lastUsed: now.Add(-time.Hour)}
	fresh := &entry{hasRemain: true, remain: 100, lastUsed: now}
	if !better(old, fresh) {
		t.Error("余额相同时应优先最久未使用的账号")
	}
	if better(fresh, old) {
		t.Error("刚用过的账号不该优先")
	}
}

// TestBetterLRUAppliesWhenBothUnknown 余额都未知时也要走 LRU。
func TestBetterLRUAppliesWhenBothUnknown(t *testing.T) {
	now := time.Now()
	old := &entry{lastUsed: now.Add(-time.Hour)}
	fresh := &entry{lastUsed: now}
	if !better(old, fresh) {
		t.Error("余额都未知时应按最久未使用排序")
	}
}

// TestBetterNeitherWhenBothNeverUsed 两个都没用过时，谁都不比谁更优
// （结果取决于遍历顺序，属于可接受的非确定性）。
func TestBetterNeitherWhenBothNeverUsed(t *testing.T) {
	a := &entry{}
	b := &entry{}
	if better(a, b) || better(b, a) {
		t.Error("两个从未使用的账号之间不该有确定性的优劣，否则选号会总压同一个")
	}
}

// ---- Pick()：跳过不可用账号 ----

func TestPickSkipsDisabled(t *testing.T) {
	p := New(testChannel(), Config{})
	a1, a2 := testAuth("u1"), testAuth("u2")
	p.Add(a1)
	p.Add(a2)

	p.SetDisabled("u1", true, "manual")

	got, err := p.Pick()
	if err != nil {
		t.Fatalf("Pick 出错: %v", err)
	}
	if got.UID != "u2" {
		t.Errorf("应跳过停用的 u1，实际选到 %s", got.UID)
	}
}

func TestPickSkipsCooling(t *testing.T) {
	p := New(testChannel(), Config{})
	a1, a2 := testAuth("u1"), testAuth("u2")
	p.Add(a1)
	p.Add(a2)

	p.NoteError(a1, provider.ErrHardCredit, "insufficient credit")

	got, err := p.Pick()
	if err != nil {
		t.Fatalf("Pick 出错: %v", err)
	}
	if got.UID != "u2" {
		t.Errorf("应跳过冷却中的 u1，实际选到 %s", got.UID)
	}
}

func TestPickNoAccountWhenAllUnavailable(t *testing.T) {
	p := New(testChannel(), Config{})
	a1 := testAuth("u1")
	p.Add(a1)
	p.SetDisabled("u1", true, "manual")

	if _, err := p.Pick(); !errors.Is(err, ErrNoAccount) {
		t.Errorf("全部不可用时应返回 ErrNoAccount，实际 %v", err)
	}
}

func TestPickEmptyPool(t *testing.T) {
	p := New(testChannel(), Config{})
	if _, err := p.Pick(); !errors.Is(err, ErrNoAccount) {
		t.Errorf("空池应返回 ErrNoAccount，实际 %v", err)
	}
}

// TestPickPrefersAccountWithMoreRemain 端到端：通过 NoteRemain 设置余额后，
// 选号应命中积分多的那个。
func TestPickPrefersAccountWithMoreRemain(t *testing.T) {
	p := New(testChannel(), Config{})
	a1, a2 := testAuth("u1"), testAuth("u2")
	p.Add(a1)
	p.Add(a2)

	p.NoteRemain(a1, 100)
	p.NoteRemain(a2, 5000)

	got, err := p.Pick()
	if err != nil {
		t.Fatalf("Pick 出错: %v", err)
	}
	if got.UID != "u2" {
		t.Errorf("应选积分多的 u2（5000 > 100），实际选到 %s", got.UID)
	}
}

// ---- UsableAuth()：粘性路由依赖的可用性判断 ----

func TestUsableAuthRejectsUnavailable(t *testing.T) {
	p := New(testChannel(), Config{})
	a1, a2 := testAuth("u1"), testAuth("u2")
	p.Add(a1)
	p.Add(a2)

	if got := p.UsableAuth("u1"); got == nil {
		t.Fatal("健康账号 UsableAuth 应返回账号")
	}

	p.SetDisabled("u1", true, "manual")
	if got := p.UsableAuth("u1"); got != nil {
		t.Error("已停用账号 UsableAuth 应返回 nil")
	}

	p.NoteError(a2, provider.ErrHardCredit, "insufficient credit")
	if got := p.UsableAuth("u2"); got != nil {
		t.Error("冷却中账号 UsableAuth 应返回 nil")
	}

	if got := p.UsableAuth("no-such-uid"); got != nil {
		t.Error("不存在的 UID 应返回 nil")
	}
}

// TestAuthIgnoresState 锁住 Auth 与 UsableAuth 的语义差异：
// Auth 只按 UID 取对象、**不看状态**，维护类操作（签到 / 保活 / 手动刷新）
// 依赖这个语义 —— 积分不足的账号恰恰最需要签到拿新积分。
func TestAuthIgnoresState(t *testing.T) {
	p := New(testChannel(), Config{})
	a1 := testAuth("u1")
	p.Add(a1)

	p.SetDisabled("u1", true, "manual")
	if got := p.Auth("u1"); got == nil {
		t.Error("Auth 不该受停用状态影响 —— 签到/保活需要能拿到停用账号")
	}
	p.NoteError(a1, provider.ErrHardCredit, "x")
	if got := p.Auth("u1"); got == nil {
		t.Error("Auth 不该受冷却状态影响")
	}
}

// TestDisabledAccountNotCountedHealthy 停用账号不计入健康数。
func TestDisabledAccountNotCountedHealthy(t *testing.T) {
	p := New(testChannel(), Config{})
	p.Add(testAuth("u1"))
	p.Add(testAuth("u2"))

	if n := p.Healthy(); n != 2 {
		t.Fatalf("初始健康数应为 2，实际 %d", n)
	}
	p.SetDisabled("u1", true, "manual")
	if n := p.Healthy(); n != 1 {
		t.Errorf("停用一个后健康数应为 1，实际 %d", n)
	}
}
