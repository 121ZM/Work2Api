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

// TestKeepResourceItem 锁住过滤判据：只看「有没有剩余」。
//
// 「已用完」那条是本次改动的核心 —— 旧判据只挡 limit<=0，
// 于是「0 / 200」这种用完的包会一直留在面板上。
func TestKeepResourceItem(t *testing.T) {
	cases := []struct {
		name          string
		limit, remain int64
		want          bool
	}{
		{"满额未动", 200, 200, true},
		{"用掉一部分", 200, 150, true},
		{"只剩 1 点", 200, 1, true},
		{"已用完（本次改动要挡的就是它）", 200, 0, false},
		{"额度本身为 0 的「免费」包", 0, 0, false},
		{"上游给负数", -1, -1, false},
	}
	for _, c := range cases {
		if got := keepResourceItem(c.limit, c.remain); got != c.want {
			t.Errorf("%s: keepResourceItem(%d, %d) = %v，期望 %v",
				c.name, c.limit, c.remain, got, c.want)
		}
	}
}

// TestKeepResourceItemOnRealShape 用 traework/cn 实测的 33 条形态跑一遍过滤。
//
// 样本照抄 web_user_ent_usage 的真实分布（2026-09-17 实测，账号 288519977712979），
// 所以合计必须等于接口给出的 total=1200 —— 这同时校验了样本没抄错。
//
// 断言的是过滤的**正当性**：被滤掉的条目必须对合计零贡献，否则过滤会算错余额。
// 若判据退化成「只判 limit>0」（旧逻辑），kept 会变成 33 而不是 8，这里先红。
func TestKeepResourceItemOnRealShape(t *testing.T) {
	rows := [][3]int64{}
	for i := 0; i < 2; i++ {
		rows = append(rows, [3]int64{1000, 1000, 0})
	}
	for i := 0; i < 3; i++ {
		rows = append(rows, [3]int64{500, 500, 0})
	}
	for i := 0; i < 14; i++ {
		rows = append(rows, [3]int64{200, 200, 0})
	}
	for i := 0; i < 6; i++ {
		rows = append(rows, [3]int64{150, 150, 0})
	}
	for i := 0; i < 8; i++ {
		rows = append(rows, [3]int64{150, 0, 150})
	}
	if len(rows) != 33 {
		t.Fatalf("样本应为实测的 33 条，实际 %d", len(rows))
	}

	var sumAll, sumKept int64
	kept, dropped := 0, 0
	for _, r := range rows {
		sumAll += r[2]
		if keepResourceItem(r[0], r[2]) {
			kept++
			sumKept += r[2]
			continue
		}
		dropped++
		if r[2] > 0 {
			t.Errorf("把还有剩余 %d 的条目滤掉了（limit=%d）—— 过滤会少算余额", r[2], r[0])
		}
	}
	if sumAll != 1200 {
		t.Fatalf("样本合计应为实测的 1200，实际 %d（样本抄错了）", sumAll)
	}
	if kept != 8 || dropped != 25 {
		t.Errorf("实测样本应保留 8 条、滤掉 25 条，实际保留 %d、滤掉 %d", kept, dropped)
	}
	if sumKept != sumAll {
		t.Errorf("过滤改变了合计：%d → %d", sumAll, sumKept)
	}
}
