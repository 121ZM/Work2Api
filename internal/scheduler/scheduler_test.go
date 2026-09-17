package scheduler

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"work2api/internal/auth"
	"work2api/internal/pool"
	"work2api/internal/provider"
	"work2api/internal/region"
)

// fakeUpstream 只实现签到与查积分，其余方法空实现。
type fakeUpstream struct {
	checkinErr   error
	checkinCalls int
	remain       int64
	remainErr    error
}

func (f *fakeUpstream) RefreshToken(*auth.Auth) error { return nil }
func (f *fakeUpstream) ChatStream(*auth.Auth, []byte) (io.ReadCloser, int, []byte, error) {
	return nil, 0, nil, nil
}
func (f *fakeUpstream) FetchModels(*auth.Auth) ([]provider.ModelInfo, error) { return nil, nil }
func (f *fakeUpstream) UserResource(*auth.Auth) (int64, error)               { return f.remain, f.remainErr }
func (f *fakeUpstream) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return f.remain, nil, f.remainErr
}
func (f *fakeUpstream) DailyCheckin(*auth.Auth) error {
	f.checkinCalls++
	return f.checkinErr
}
func (f *fakeUpstream) Classify(int, string) provider.ErrKind { return provider.ErrNone }
func (f *fakeUpstream) Stream(http.ResponseWriter, io.Reader) error {
	return nil
}
func (f *fakeUpstream) Aggregate(io.Reader) (map[string]any, error) { return nil, nil }

func newTarget(ch provider.Channel, up provider.Upstream, uids ...string) (*Target, *pool.Pool) {
	p := pool.New(ch, pool.Config{})
	for _, uid := range uids {
		p.Add(&auth.Auth{Kind: ch.Kind.String(), Region: ch.Region, UID: uid})
	}
	return &Target{Channel: ch, Pool: p, Upstream: up}, p
}

func cnChannel() provider.Channel {
	return provider.Channel{Kind: provider.WorkBuddy, Region: region.CN}
}

func globalChannel() provider.Channel {
	return provider.Channel{Kind: provider.WorkBuddy, Region: region.Global}
}

// TestCheckinAlreadyCheckedInIsSuccess 「今天已签到」是**正常状态，不是失败**。
//
// 上游把它表达成 HTTP 400 + code=10001，与真实失败共用同一通道。
// 若当成失败：面板每天显示一条红色错误，好账号还会被记进冷却。
func TestCheckinAlreadyCheckedInIsSuccess(t *testing.T) {
	up := &fakeUpstream{checkinErr: provider.ErrAlreadyCheckedIn, remain: 6247}
	tgt, p := newTarget(cnChannel(), up, "u1")
	s := &Scheduler{}

	res := s.checkinOne(tgt, "u1")

	if !res.OK {
		t.Errorf("「今天已签到」应记为成功，实际 OK=%v msg=%q", res.OK, res.Msg)
	}
	if res.Skipped {
		t.Error("「今天已签到」不该被标为 skipped（那是「该版本无签到体系」的语义）")
	}
	if !strings.Contains(res.Msg, "已签到") {
		t.Errorf("提示语应说明已签到，实际 %q", res.Msg)
	}
	if res.Remain != 6247 {
		t.Errorf("已签到路径也应刷新积分，期望 6247，实际 %d", res.Remain)
	}

	// 关键：不能进冷却、不能累计错误
	st := p.List()[0]
	if st.ErrCount != 0 {
		t.Errorf("「今天已签到」不该累计 err_count，实际 %d", st.ErrCount)
	}
	if st.Cooling() {
		t.Error("「今天已签到」不该让账号进入冷却")
	}
	if !st.HasRemain || st.Remain != 6247 {
		t.Errorf("积分应被写入池，实际 hasRemain=%v remain=%d", st.HasRemain, st.Remain)
	}
}

// TestCheckinUnsupportedIsSkipped 国际版哨兵错误 → 跳过，且不算失败。
func TestCheckinUnsupportedIsSkipped(t *testing.T) {
	up := &fakeUpstream{checkinErr: provider.ErrCheckinUnsupported}
	tgt, p := newTarget(cnChannel(), up, "u1")
	s := &Scheduler{}

	res := s.checkinOne(tgt, "u1")

	if !res.Skipped {
		t.Errorf("ErrCheckinUnsupported 应标为 skipped，实际 %+v", res)
	}
	if res.OK {
		t.Error("跳过的签到不该记为成功")
	}
	if st := p.List()[0]; st.Cooling() || st.ErrCount != 0 {
		t.Errorf("跳过不该产生冷却或错误计数，实际 cooling=%v err_count=%d", st.Cooling(), st.ErrCount)
	}
}

// TestCheckinSuccessRefreshesRemain 签到成功要顺带刷新积分。
func TestCheckinSuccessRefreshesRemain(t *testing.T) {
	up := &fakeUpstream{remain: 1200}
	tgt, p := newTarget(cnChannel(), up, "u1")
	s := &Scheduler{}

	res := s.checkinOne(tgt, "u1")

	if !res.OK {
		t.Errorf("签到成功应 OK=true，实际 msg=%q", res.Msg)
	}
	if res.Remain != 1200 {
		t.Errorf("应回填积分 1200，实际 %d", res.Remain)
	}
	if st := p.List()[0]; st.Remain != 1200 || !st.HasRemain {
		t.Errorf("积分应写入池，实际 %d hasRemain=%v", st.Remain, st.HasRemain)
	}
}

// TestCheckinSuccessWithoutRemainStillOK 签到成功但查积分失败时，仍算成功。
func TestCheckinSuccessWithoutRemainStillOK(t *testing.T) {
	up := &fakeUpstream{remainErr: errors.New("billing api down")}
	tgt, _ := newTarget(cnChannel(), up, "u1")
	s := &Scheduler{}

	res := s.checkinOne(tgt, "u1")

	if !res.OK {
		t.Error("签到本身成功时，查积分失败不该把结果标成失败")
	}
}

// TestCheckinRealFailureDoesNotCooldown 签到真实失败只记录、**不进冷却**。
//
// 这是刻意设计：签到失败（网络抖动等）不该影响对话路由的选号。
func TestCheckinRealFailureDoesNotCooldown(t *testing.T) {
	up := &fakeUpstream{checkinErr: errors.New("network glitch")}
	tgt, p := newTarget(cnChannel(), up, "u1")
	s := &Scheduler{}

	res := s.checkinOne(tgt, "u1")

	if res.OK {
		t.Error("真实失败不该标为成功")
	}
	st := p.List()[0]
	if st.Cooling() {
		t.Error("签到失败不该让账号进入冷却（否则会波及对话路由）")
	}
	if st.ErrCount != 0 {
		t.Errorf("签到失败不该累计 err_count，实际 %d", st.ErrCount)
	}
}

// TestCheckinOneMissingAccount 账号不存在时不应 panic。
func TestCheckinOneMissingAccount(t *testing.T) {
	up := &fakeUpstream{}
	tgt, _ := newTarget(cnChannel(), up, "u1")
	s := &Scheduler{}

	res := s.checkinOne(tgt, "no-such-uid")

	if res.OK || res.Skipped {
		t.Errorf("不存在的账号应返回失败且不跳过，实际 %+v", res)
	}
	if up.checkinCalls != 0 {
		t.Error("账号不存在时不该发起上游请求")
	}
}

// TestRunCheckinNowSkipsGlobalChannel 国际版渠道**整池跳过**，
// 且一次上游签到请求都不发。
func TestRunCheckinNowSkipsGlobalChannel(t *testing.T) {
	up := &fakeUpstream{}
	tgt, _ := newTarget(globalChannel(), up, "g1", "g2")
	s := New(Config{}, []*Target{tgt})

	results := s.RunCheckinNow()

	if len(results) != 2 {
		t.Fatalf("两个账号应各产生一条结果，实际 %d", len(results))
	}
	for _, r := range results {
		if !r.Skipped {
			t.Errorf("国际版账号 %s 应被跳过，实际 %+v", r.UID, r)
		}
		if r.OK {
			t.Errorf("跳过的结果不该 OK=true：%+v", r)
		}
	}
	if up.checkinCalls != 0 {
		t.Errorf("国际版不该发起任何签到请求，实际 %d 次", up.checkinCalls)
	}
}

// TestRunCheckinNowRunsForCN 国内版渠道正常逐个签到。
func TestRunCheckinNowRunsForCN(t *testing.T) {
	up := &fakeUpstream{remain: 100}
	tgt, _ := newTarget(cnChannel(), up, "c1", "c2")
	s := New(Config{}, []*Target{tgt})

	results := s.RunCheckinNow()

	if len(results) != 2 {
		t.Fatalf("两个账号应各产生一条结果，实际 %d", len(results))
	}
	if up.checkinCalls != 2 {
		t.Errorf("国内版两个账号应各签到一次，实际 %d 次", up.checkinCalls)
	}
	for _, r := range results {
		if !r.OK || r.Skipped {
			t.Errorf("国内版签到应成功，实际 %+v", r)
		}
	}
}
