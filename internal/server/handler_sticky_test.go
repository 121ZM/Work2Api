package server

import (
	"testing"

	"work2api/internal/auth"
	"work2api/internal/pool"
	"work2api/internal/provider"
	"work2api/internal/region"
)

// newStickyTestHandler 造一个最小可用的 Handler + Runtime（两个账号）。
func newStickyTestHandler(t *testing.T) (*Handler, *Runtime, provider.Channel) {
	t.Helper()
	ch := provider.Channel{Kind: provider.WorkBuddy, Region: region.CN}
	p := pool.New(ch, pool.Config{})
	p.Add(&auth.Auth{Kind: "workbuddy", Region: region.CN, UID: "uid-1"})
	p.Add(&auth.Auth{Kind: "workbuddy", Region: region.CN, UID: "uid-2"})
	rt := &Runtime{Channel: ch, Pool: p}
	h := &Handler{sticky: map[string]*stickyEntry{}}
	return h, rt, ch
}

// TestPickReusesDisabledStickyAccount 检查「粘性账号被停用后是否还会被继续使用」。
//
// handler.pick 的粘性分支条件是：
//
//	if a := rt.Pool.Auth(st.uid); a != nil && rt.Pool.Healthy() > 0 { return a }
//
// Pool.Auth 只按 UID 查表、**不看该账号是否已停用/冷却**；
// Pool.Healthy() 返回的是**池级**健康账号数，与粘性账号本身无关。
// 所以只要池里还有别的健康账号，停用的粘性账号就会被继续选中。
func TestPickReusesDisabledStickyAccount(t *testing.T) {
	h, rt, ch := newStickyTestHandler(t)

	first := h.pick(rt)
	if first == nil {
		t.Fatal("首次选号返回 nil，测试前提不成立")
	}
	h.noteSticky(ch, first.UID) // 建立粘性

	// 面板「停用」这个账号
	if !rt.Pool.SetDisabled(first.UID, true, "manual") {
		t.Fatal("SetDisabled 失败，测试前提不成立")
	}
	if rt.Pool.Healthy() == 0 {
		t.Fatal("池里应还剩一个健康账号，测试前提不成立")
	}

	second := h.pick(rt)
	if second == nil {
		t.Fatal("二次选号返回 nil —— 池里明明还有健康账号")
	}
	if second.UID == first.UID {
		t.Errorf("已停用的粘性账号 uid=%s 仍被选中；应改选池内另一个健康账号", second.UID)
	}
}

// TestPickReusesCoolingStickyAccount 同上，但用「冷却」代替「停用」。
func TestPickReusesCoolingStickyAccount(t *testing.T) {
	h, rt, ch := newStickyTestHandler(t)

	first := h.pick(rt)
	if first == nil {
		t.Fatal("首次选号返回 nil")
	}
	h.noteSticky(ch, first.UID)

	// 让这个账号进入长时间冷却（模拟积分不足）
	rt.Pool.NoteError(first, provider.ErrHardCredit, "insufficient credit")
	if rt.Pool.Healthy() == 0 {
		t.Fatal("池里应还剩一个健康账号")
	}

	second := h.pick(rt)
	if second == nil {
		t.Fatal("二次选号返回 nil")
	}
	if second.UID == first.UID {
		t.Errorf("处于冷却中的粘性账号 uid=%s 仍被选中；应改选健康账号", second.UID)
	}
}
