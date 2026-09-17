package traework

import "testing"

// 样本取自 traework/cn 真实账号的 web_user_ent_usage 返回（见 packExpireAt 注释）。
// 1791081100 = 2026-10-04 10:31:40（CST）。
const (
	sampleExpireTime = int64(1791081100)
)

// TestPackNamePrefersDisplayDesc 优先用上游的 display_desc。
//
// 原来一律拼「权益包 N」，面板上 13 行全是「权益包 1…13」，看不出是什么包。
func TestPackNamePrefersDisplayDesc(t *testing.T) {
	if got := packName("老用户福利", "用户福利", 1); got != "老用户福利" {
		t.Errorf("应优先 display_desc，实际 %q", got)
	}
	// display_desc 缺失时退回 group_name
	if got := packName("", "用户福利", 2); got != "用户福利" {
		t.Errorf("display_desc 缺失时应退回 group_name，实际 %q", got)
	}
	// 空白串等同缺失，不能当成名字用
	if got := packName("   ", "", 3); got != "权益包 3" {
		t.Errorf("两个都缺失时应退回序号，实际 %q", got)
	}
	if got := packName("", "", 7); got != "权益包 7" {
		t.Errorf("应退回「权益包 7」，实际 %q", got)
	}
}

// TestPackExpireAtPrefersExpireTime 优先用顶层 expire_time。
func TestPackExpireAtPrefersExpireTime(t *testing.T) {
	if got := packExpireAt(sampleExpireTime, sampleExpireTime); got != sampleExpireTime {
		t.Errorf("应取 expire_time %d，实际 %d", sampleExpireTime, got)
	}
}

// TestPackExpireAtFallsBackToEndTime 顶层缺失时回退 base.end_time。
func TestPackExpireAtFallsBackToEndTime(t *testing.T) {
	if got := packExpireAt(0, sampleExpireTime); got != sampleExpireTime {
		t.Errorf("应回退 end_time %d，实际 %d", sampleExpireTime, got)
	}
}

// TestPackExpireAtUnknownReturnsZero 两个都没有时返回 0，不猜。
func TestPackExpireAtUnknownReturnsZero(t *testing.T) {
	if got := packExpireAt(0, 0); got != 0 {
		t.Errorf("无到期字段时应返回 0，实际 %d", got)
	}
	if got := packExpireAt(-1, -1); got != 0 {
		t.Errorf("负值应视为缺失返回 0，实际 %d", got)
	}
}

// TestPackExpireAtBothFieldsAgree 锁住「两个字段表示同一时刻」这个实测前提。
//
// 上游同时给了 expire_time 和 entitlement_base_info.end_time，实测恒等。
// 哪天上游改了语义，这条会先红，而不是等用户发现到期日不对。
func TestPackExpireAtBothFieldsAgree(t *testing.T) {
	a := packExpireAt(sampleExpireTime, 0)
	b := packExpireAt(0, sampleExpireTime)
	if a != b {
		t.Errorf("两个字段应给同一时刻，expire_time=%d end_time=%d", a, b)
	}
}
