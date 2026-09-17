package upstream

import (
	"testing"
)

// 样本取自 workbuddy/cn 真实账号的 get-user-resource 返回（见 expireAt 注释）。
// 期望值 1792195250 是用 Python 独立算出来的：
//
//	1792195250000 ms → 1792195250 s → 2026-10-17T00:00:50Z → 2026-10-17 08:00:50 CST
const (
	sampleDeductionEndMS = 1792195250000
	sampleCycleEndStr    = "2026-10-17 08:00:50"
	sampleWantUnix       = 1792195250
)

// TestExpireAtPrefersDeductionEndTime 优先用毫秒戳。
func TestExpireAtPrefersDeductionEndTime(t *testing.T) {
	got := expireAt(resourceAccount{
		DeductionEndTime: sampleDeductionEndMS,
		CycleEndTime:     sampleCycleEndStr,
	})
	if got != sampleWantUnix {
		t.Errorf("应取 DeductionEndTime 换算值 %d，实际 %d", sampleWantUnix, got)
	}
}

// TestExpireAtFallsBackToCycleEndTime 缺毫秒戳时回退解析 CycleEndTime。
//
// 这条真正锁的是**时区**：CycleEndTime 是北京时间。若有人「顺手简化」成
// time.Parse（按 UTC 解析），结果会差 28800 秒（8 小时），到期日可能在跨日时错一天。
func TestExpireAtFallsBackToCycleEndTime(t *testing.T) {
	got := expireAt(resourceAccount{CycleEndTime: sampleCycleEndStr})
	if got != sampleWantUnix {
		t.Errorf("按 CST 解析应得 %d，实际 %d（差 %d 秒，若是 28800 则是按 UTC 解析了）",
			sampleWantUnix, got, got-sampleWantUnix)
	}
}

// TestExpireAtBothFormsAgree 两种表示法必须给出同一时刻。
//
// 上游同时给了数值和字符串两种到期时间，实测它们表示同一时刻。
// 这条断言把这个前提钉住 —— 哪天上游改了语义，这里会先红。
func TestExpireAtBothFormsAgree(t *testing.T) {
	a := expireAt(resourceAccount{DeductionEndTime: sampleDeductionEndMS})
	b := expireAt(resourceAccount{CycleEndTime: sampleCycleEndStr})
	if a != b {
		t.Errorf("两种表示法应给同一时刻，毫秒戳 %d vs 字符串 %d", a, b)
	}
}

// TestExpireAtIgnoresExpiredTime 锁住「刻意不用 ExpiredTime」这个决定。
//
// ExpiredTime 实测时有时无，且**有值的那些都是已过期的包**，拿它当到期时间
// 会把「已过期」和「未提供」混为一谈。所以只有它、没有别的字段时必须返回 0。
func TestExpireAtIgnoresExpiredTime(t *testing.T) {
	if got := expireAt(resourceAccount{ExpiredTime: "2026-07-04 08:23:03"}); got != 0 {
		t.Errorf("只有 ExpiredTime 时应返回 0（不采用该字段），实际 %d", got)
	}
}

// TestExpireAtUnknownReturnsZero 两个字段都缺时返回 0，不猜一个假时间。
func TestExpireAtUnknownReturnsZero(t *testing.T) {
	if got := expireAt(resourceAccount{}); got != 0 {
		t.Errorf("无任何到期字段时应返回 0，实际 %d", got)
	}
	// 无法解析的字符串同样返回 0，而不是 panic 或负值
	if got := expireAt(resourceAccount{CycleEndTime: "不是时间"}); got != 0 {
		t.Errorf("无法解析时应返回 0，实际 %d", got)
	}
	// 负的毫秒戳（上游异常）不该产出负数时间
	if got := expireAt(resourceAccount{DeductionEndTime: -1}); got != 0 {
		t.Errorf("负毫秒戳应回退（此处无兜底字段）返回 0，实际 %d", got)
	}
}

// TestKeepResourceItem 锁住过滤判据：只看「有没有剩余」。
//
// 「已用完」那条是本次改动的核心 —— 旧判据只挡 total<=0，
// 于是 workbuddy/cn 的 12 条「0 / 100」全都漏到了面板上。
func TestKeepResourceItem(t *testing.T) {
	cases := []struct {
		name          string
		total, remain int64
		want          bool
	}{
		{"满额未动", 100, 100, true},
		{"用了大半", 100, 73, true},
		{"只剩 1 点", 100, 1, true},
		{"已用完（本次改动要挡的就是它）", 100, 0, false},
		{"额度本身为 0", 0, 0, false},
		{"上游给负数", -1, -1, false},
		{"异常：total=0 却有剩余（不该出现，但判据也不放行）", 0, 5, false},
	}
	for _, c := range cases {
		if got := keepResourceItem(c.total, c.remain); got != c.want {
			t.Errorf("%s: keepResourceItem(%d, %d) = %v，期望 %v",
				c.name, c.total, c.remain, got, c.want)
		}
	}
}

// TestKeepResourceItemOnRealShape 用 workbuddy/cn 实测的 24 条形态跑一遍过滤。
//
// 样本逐条照抄 /api/account/resource 的真实返回（2026-09-17 实测），
// 所以合计必须等于接口给出的 total=1929 —— 这同时校验了样本没抄错。
//
// 断言的是过滤的**正当性**：被滤掉的条目必须对合计零贡献，否则过滤会算错余额。
// 若判据退化成「只判 total>0」（旧逻辑），kept 会变成 24 而不是 12，这里先红。
func TestKeepResourceItemOnRealShape(t *testing.T) {
	rows := [][3]int64{{500, 500, 0}, {5000, 5000, 0}, {1000, 93, 906}, {50, 0, 50}}
	for i := 0; i < 10; i++ {
		rows = append(rows, [3]int64{100, 100, 0})
	}
	rows = append(rows, [3]int64{100, 26, 73})
	for i := 0; i < 9; i++ {
		rows = append(rows, [3]int64{100, 0, 100})
	}
	if len(rows) != 24 {
		t.Fatalf("样本应为实测的 24 条，实际 %d", len(rows))
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
			t.Errorf("把还有剩余 %d 的条目滤掉了（total=%d）—— 过滤会少算余额", r[2], r[0])
		}
	}
	if sumAll != 1929 {
		t.Fatalf("样本合计应为实测的 1929，实际 %d（样本抄错了）", sumAll)
	}
	if kept != 12 || dropped != 12 {
		t.Errorf("实测样本应保留 12 条、滤掉 12 条，实际保留 %d、滤掉 %d", kept, dropped)
	}
	if sumKept != sumAll {
		t.Errorf("过滤改变了合计：%d → %d", sumAll, sumKept)
	}
}
