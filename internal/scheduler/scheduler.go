// Package scheduler 定时任务：每日签到 + token 保活。
//
// 关键语义：**签到只对国内版账号执行**。
// 国际版（海外发行版）没有积分签到体系，调度器遇到这类渠道时直接跳过 ——
// 不计失败、不进入冷却、不查积分。判定依据是 Channel.CheckinSupported()，
// 而非配置开关，这样新增渠道时不会漏掉。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"work2api/internal/auth"
	"work2api/internal/pool"
	"work2api/internal/provider"
	"work2api/internal/region"
)

// Target 一个渠道 + 版本的调度目标。
type Target struct {
	Channel  provider.Channel
	Pool     *pool.Pool
	Upstream provider.Upstream
}

// Config 调度配置。
type Config struct {
	CheckinMinutes []int // 当天分钟数（0..1439），如 9*60、21*60
	KeepaliveHours []int // token 保活整点
}

// CheckinResult 单账号签到结果。
type CheckinResult struct {
	Channel string        `json:"channel"`
	Region  region.Region `json:"region"`
	UID     string        `json:"uid"`
	OK      bool          `json:"ok"`
	Skipped bool          `json:"skipped,omitempty"` // 该版本无签到体系
	Msg     string        `json:"msg"`
	Remain  int64         `json:"remain,omitempty"`
}

// Scheduler 定时任务调度器。
type Scheduler struct {
	mu      sync.Mutex
	cfg     Config
	targets []*Target

	ranCheckin   map[string]map[int]bool // day → 已执行的签到分钟集合
	ranKeepalive map[string]map[int]bool // day → 已执行的保活整点集合

	onCheckin func([]CheckinResult) // 结果观察器（Web 面板推送）
}

// New 创建调度器。
func New(cfg Config, targets []*Target) *Scheduler {
	return &Scheduler{
		cfg:          cfg,
		targets:      targets,
		ranCheckin:   map[string]map[int]bool{},
		ranKeepalive: map[string]map[int]bool{},
	}
}

// SetCheckinObserver 设置签到结果观察器。
func (s *Scheduler) SetCheckinObserver(fn func([]CheckinResult)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onCheckin = fn
}

// CheckinMinutes 当前签到时间（当天分钟数）。
func (s *Scheduler) CheckinMinutes() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int{}, s.cfg.CheckinMinutes...)
}

// SetCheckinMinutes 运行时更新签到时间。
func (s *Scheduler) SetCheckinMinutes(minutes []int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.CheckinMinutes = append([]int{}, minutes...)
}

// Run 阻塞运行调度循环，直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.tick(now)
		}
	}
}

func (s *Scheduler) tick(now time.Time) {
	day := now.Format("2006-01-02")
	minute := now.Hour()*60 + now.Minute()

	s.mu.Lock()
	due := false
	for _, m := range s.cfg.CheckinMinutes {
		if m != minute {
			continue
		}
		if s.ranCheckin[day] == nil {
			s.ranCheckin[day] = map[int]bool{}
		}
		if !s.ranCheckin[day][m] {
			s.ranCheckin[day][m] = true
			due = true
		}
		break
	}
	keepalive := false
	if now.Minute() == 0 {
		for _, h := range s.cfg.KeepaliveHours {
			if h != now.Hour() {
				continue
			}
			if s.ranKeepalive[day] == nil {
				s.ranKeepalive[day] = map[int]bool{}
			}
			if !s.ranKeepalive[day][h] {
				s.ranKeepalive[day][h] = true
				keepalive = true
			}
			break
		}
	}
	// 清理非当天的记录，避免 map 无限增长
	for d := range s.ranCheckin {
		if d != day {
			delete(s.ranCheckin, d)
		}
	}
	for d := range s.ranKeepalive {
		if d != day {
			delete(s.ranKeepalive, d)
		}
	}
	s.mu.Unlock()

	if due {
		go s.RunCheckinNow()
	}
	if keepalive {
		go s.RunKeepaliveNow()
	}
}

// RunCheckinNow 立即对所有渠道执行签到（国内版真签到，国际版跳过）。
func (s *Scheduler) RunCheckinNow() []CheckinResult {
	results := make([]CheckinResult, 0, 8)
	for _, t := range s.targets {
		if !t.Channel.CheckinSupported() {
			// 国际版无签到体系：跳过全部账号，且不产生任何错误 / 冷却副作用
			for _, uid := range t.Pool.UIDs() {
				msg := "该版本无签到体系，已跳过"
				if a := t.Pool.Auth(uid); a != nil {
					t.Pool.MarkCheckin(a, msg)
				}
				results = append(results, CheckinResult{
					Channel: t.Channel.String(), Region: t.Channel.Region,
					UID: uid, OK: false, Skipped: true, Msg: msg,
				})
			}
			log.Printf("checkin skip channel=%s reason=no_checkin_system accounts=%d",
				t.Channel, t.Pool.Len())
			continue
		}
		uids := t.Pool.UIDs()
		log.Printf("checkin start channel=%s accounts=%d", t.Channel, len(uids))
		for _, uid := range uids {
			r := s.checkinOne(t, uid)
			results = append(results, r)
			log.Printf("checkin result channel=%s uid=%s ok=%t skipped=%t msg=%s",
				t.Channel, uid, r.OK, r.Skipped, r.Msg)
		}
	}
	s.notify(results)
	return results
}

// checkinOne 单账号签到。注意：跳过路径**不调用** Pool.NoteError。
func (s *Scheduler) checkinOne(t *Target, uid string) CheckinResult {
	res := CheckinResult{Channel: t.Channel.String(), Region: t.Channel.Region, UID: uid}
	a := t.Pool.Auth(uid)
	if a == nil {
		res.Msg = "账号不存在"
		return res
	}
	err := t.Upstream.DailyCheckin(a)
	switch {
	case err == nil:
		// 签到成功顺带刷新余额
		if remain, rerr := t.Upstream.UserResource(a); rerr == nil {
			t.Pool.NoteRemain(a, remain)
			res.Remain, res.OK = remain, true
		} else {
			res.OK = true
		}
		res.Msg = "签到成功"
		if res.OK {
			res.Msg = fmt.Sprintf("签到成功，可用积分 %d", res.Remain)
		}
		t.Pool.MarkCheckin(a, res.Msg)
	case errors.Is(err, provider.ErrAlreadyCheckedIn):
		// 今日已签到 —— 正常状态，不是失败：不记错误、不进冷却，
		// 但仍然刷一次余额，让面板上的积分保持新鲜。
		res.OK = true
		res.Msg = "今天已签到"
		if remain, rerr := t.Upstream.UserResource(a); rerr == nil {
			t.Pool.NoteRemain(a, remain)
			res.Remain = remain
			res.Msg = fmt.Sprintf("今天已签到，可用积分 %d", remain)
		}
		t.Pool.MarkCheckin(a, res.Msg)
	case errors.Is(err, provider.ErrCheckinUnsupported):
		// 双保险：upstream 侧也做了能力位检查
		res.Skipped = true
		res.Msg = "该版本无签到体系，已跳过"
		t.Pool.MarkCheckin(a, res.Msg)
	default:
		res.Msg = err.Error()
		t.Pool.MarkCheckin(a, res.Msg)
	}
	return res
}

// RunKeepaliveNow 立即刷新所有账号 token。
func (s *Scheduler) RunKeepaliveNow() {
	for _, t := range s.targets {
		for _, uid := range t.Pool.UIDs() {
			a := t.Pool.Auth(uid)
			if a == nil {
				continue
			}
			if err := s.keepaliveOne(t, a); err != nil {
				log.Printf("keepalive failed channel=%s uid=%s err=%v", t.Channel, uid, err)
			}
		}
	}
}

func (s *Scheduler) keepaliveOne(t *Target, a *auth.Auth) error {
	if err := t.Upstream.RefreshToken(a); err != nil {
		return err
	}
	if err := a.SaveAtomic(); err != nil {
		return fmt.Errorf("save after refresh: %w", err)
	}
	log.Printf("keepalive ok channel=%s uid=%s expires_at=%d", t.Channel, a.UID, a.ExpiresAt)
	return nil
}

func (s *Scheduler) notify(results []CheckinResult) {
	s.mu.Lock()
	fn := s.onCheckin
	s.mu.Unlock()
	if fn != nil {
		fn(results)
	}
}
