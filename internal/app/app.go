// Package app 装配配置、账号池、上游渠道、调度器与 HTTP 服务。
//
// 装配的核心是 **按 provider.Channel 分组**：每个「渠道 + 版本」组合
// （workbuddy/cn、workbuddy/global、trae/cn、trae/global）各持有独立的
// Pool / Upstream / Scheduler / 状态文件。国内版与国际版因此天然共存，
// 互不影响冷却与路由。
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"work2api/internal/auth"
	"work2api/internal/config"
	"work2api/internal/importauth"
	"work2api/internal/loginsvc"
	"work2api/internal/pool"
	"work2api/internal/provider"
	"work2api/internal/region"
	"work2api/internal/scheduler"
	"work2api/internal/server"
	"work2api/internal/traework"
	"work2api/internal/upstream"
	"work2api/web"
)

// App 应用实例。
type App struct {
	cfg      *config.Config
	runtimes map[provider.Channel]*server.Runtime
	pools    map[provider.Channel]*pool.Pool
	sched    *scheduler.Scheduler
	handler  *server.Handler
	logins   *loginsvc.Manager
}

// allChannels 全部渠道 + 版本组合。
func allChannels() []provider.Channel {
	return []provider.Channel{
		{Kind: provider.WorkBuddy, Region: region.CN},
		{Kind: provider.WorkBuddy, Region: region.Global},
		{Kind: provider.Trae, Region: region.CN},
		{Kind: provider.Trae, Region: region.Global},
	}
}

// New 装配应用。
func New(cfg *config.Config) (*App, error) {
	accounts, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		return nil, fmt.Errorf("加载凭证目录 %s: %w", cfg.AuthDir, err)
	}

	byChannel := map[provider.Channel][]*auth.Auth{}
	for _, acct := range accounts {
		ch := provider.Channel{Kind: provider.Kind(acct.Kind), Region: acct.Region}
		byChannel[ch] = append(byChannel[ch], acct)
	}

	pcfg := pool.Config{
		HardCredit:  cfg.HardCreditDur,
		SoftRate:    cfg.SoftRateDur,
		ErrThresh:   cfg.Cooldown.ErrThresh,
		ErrCooldown: cfg.ErrCooldownDur,
	}
	timeout := time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second

	a := &App{
		cfg:      cfg,
		runtimes: map[provider.Channel]*server.Runtime{},
		pools:    map[provider.Channel]*pool.Pool{},
	}
	// 登录会话管理器：中间状态与状态文件同目录，避免多一个配置项。
	a.logins = loginsvc.New(filepath.Dir(cfg.StateFile), cfg.AuthDir)
	a.logins.WBOverrides = cfg.Regions.WorkBuddy
	a.logins.TraeOverrides = cfg.Regions.Trae
	var targets []*scheduler.Target
	for _, ch := range allChannels() {
		p := pool.New(ch, pcfg)
		for _, acct := range byChannel[ch] {
			p.Add(acct)
		}
		statePath := channelStatePath(cfg.StateFile, ch)
		if err := p.LoadState(statePath); err != nil {
			log.Printf("加载 %s 状态失败: %v", ch, err)
		}
		up := newUpstream(ch, cfg, timeout)
		if up == nil {
			log.Printf("渠道 %-18s 暂不可用，已跳过", ch)
			continue
		}
		a.pools[ch] = p
		a.runtimes[ch] = &server.Runtime{
			Channel:      ch,
			Pool:         p,
			Upstream:     up,
			StaticModels: provider.StaticModels(ch),
		}
		targets = append(targets, &scheduler.Target{Channel: ch, Pool: p, Upstream: up})
		log.Printf("装配渠道 %-18s 账号=%d 签到=%v", ch, p.Len(), ch.CheckinSupported())
	}

	a.sched = scheduler.New(scheduler.Config{
		CheckinMinutes: mustMinutes(cfg.Schedule.CheckinTimes),
		KeepaliveHours: cfg.Schedule.KeepaliveHours,
	}, targets)

	a.handler = server.New(&server.Config{
		APIKey:    cfg.APIKey,
		Runtimes:  a.runtimes,
		AttachAPI: a.attachAPI,
	})
	return a, nil
}

// newUpstream 按渠道构造上游客户端。
func newUpstream(ch provider.Channel, cfg *config.Config, timeout time.Duration) provider.Upstream {
	switch ch.Kind {
	case provider.WorkBuddy:
		c := upstream.New(timeout)
		c.Overrides = cfg.Regions.WorkBuddy
		return c
	case provider.Trae:
		c := traework.New(timeout)
		c.Overrides = cfg.Regions.Trae
		return c
	default:
		return nil
	}
}

// channelStatePath 派生渠道专属状态文件，如 ./data/state-workbuddy-cn.json。
func channelStatePath(base string, ch provider.Channel) string {
	if strings.TrimSpace(base) == "" {
		return ""
	}
	dir, file := filepath.Split(base)
	ext := filepath.Ext(file)
	name := strings.TrimSuffix(file, ext)
	if name == "" {
		name = "state"
	}
	if ext == "" {
		ext = ".json"
	}
	return filepath.Join(dir, fmt.Sprintf("%s-%s-%s%s", name, ch.Kind, ch.Region, ext))
}

func mustMinutes(times []string) []int {
	mins, err := config.ParseClockTimes(times)
	if err != nil {
		return []int{9 * 60, 21 * 60}
	}
	return mins
}

// Handler 返回 HTTP 处理器。
func (a *App) Handler() http.Handler { return a.handler }

// Scheduler 返回调度器（供管理 API 触发立即签到）。
func (a *App) Scheduler() *scheduler.Scheduler { return a.sched }

// Pool 按渠道取账号池。
func (a *App) Pool(ch provider.Channel) *pool.Pool { return a.pools[ch] }

// Run 启动 HTTP 服务与调度循环，阻塞直到 ctx 取消。
func (a *App) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              a.cfg.Listen.Addr(),
		Handler:           a.handler,
		ReadHeaderTimeout: 15 * time.Second,
	}

	go a.sched.Run(ctx)
	go a.runModelRefresh(ctx)

	errc := make(chan error, 1)
	go func() {
		log.Printf("work2api 监听 %s（API Key %s）", a.cfg.Listen.Addr(), maskKey(a.cfg.APIKey))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		a.saveAllStates()
		return nil
	}
}

// saveAllStates 落盘全部渠道运行态。
func (a *App) saveAllStates() {
	for ch, p := range a.pools {
		if err := p.SaveState(channelStatePath(a.cfg.StateFile, ch)); err != nil {
			log.Printf("保存 %s 状态失败: %v", ch, err)
		}
	}
}

func maskKey(k string) string {
	if k == "" {
		return "(未设置，不鉴权)"
	}
	if len(k) <= 4 {
		return "****"
	}
	return k[:2] + strings.Repeat("*", len(k)-2)
}

// ---- 管理 API ----

// modelRefreshInterval 上游模型表的刷新周期。
const modelRefreshInterval = 30 * time.Minute

// refreshModels 从上游拉取各渠道的实时模型表。
//
// 为什么必须做：静态兜底表和真实权限差得很远 —— 实测 TRAE cn 静态表只有 2 个
// 模型（真实 13 个可用的对话模型），而 WorkBuddy cn 静态表列了 37 个
// （账号实际只有 16 个权限）。只读静态表，模型列表必然是错的。
func (a *App) refreshModels() {
	for ch, rt := range a.runtimes {
		if rt.Pool.Len() == 0 {
			continue // 没账号就不拉，面板也不该暴露该渠道的模型
		}
		acct := pickForModels(rt.Pool)
		if acct == nil {
			rt.SetModels(nil, time.Now(), errors.New("无可用账号（全部禁用或冷却中）"))
			continue
		}
		list, err := rt.Upstream.FetchModels(acct)
		if err != nil {
			log.Printf("拉取 %s 模型失败: %v", ch, err)
		}
		rt.SetModels(list, time.Now(), err)
		live, n, _, errMsg := rt.ModelsInfo()
		src := "静态兜底"
		if live {
			src = "上游实时"
		}
		log.Printf("渠道 %-18s 模型=%d 来源=%s %s", ch, n, src, errMsg)
	}
}

// pickForModels 挑一个健康账号用于拉模型。
//
// 刻意不用 Pool.Pick()：那会推进粘性 / 轮转计数，为了一次只读查询
// 打乱正常请求的选号顺序不划算。
//
// 可用性判断统一走 UsableAuth，别再手写 `Disabled || Cooling()` ——
// 那种重复判断迟早会和 pool 内部的语义漂移（粘性路由就踩过这个坑）。
func pickForModels(p *pool.Pool) *auth.Auth {
	for _, st := range p.List() {
		if acct := p.UsableAuth(st.UID); acct != nil {
			return acct
		}
	}
	return nil
}

// runModelRefresh 启动时先拉一次，之后按周期刷新，随 ctx 退出。
func (a *App) runModelRefresh(ctx context.Context) {
	a.refreshModels()
	t := time.NewTicker(modelRefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.refreshModels()
		}
	}
}

func (a *App) attachAPI(mux *http.ServeMux, protect func(http.HandlerFunc) http.HandlerFunc) {
	// 管理 API：全部经 API Key 校验（面板会把它存到 localStorage）。
	mux.HandleFunc("GET /api/state", protect(a.apiState))
	mux.HandleFunc("POST /api/account/checkin", protect(a.apiCheckin))
	mux.HandleFunc("POST /api/account/refresh", protect(a.apiRefresh))
	mux.HandleFunc("POST /api/account/resource", protect(a.apiResource))
	mux.HandleFunc("POST /api/import/local", protect(a.apiImportLocal))
	mux.HandleFunc("POST /api/login/start", protect(a.apiLoginStart))
	mux.HandleFunc("GET /api/login/poll", protect(a.apiLoginPoll))
	mux.HandleFunc("POST /api/login/cancel", protect(a.apiLoginCancel))
	mux.HandleFunc("POST /api/models/refresh", protect(a.apiModelsRefresh))

	// 静态面板：路径本身不鉴权，但会把 Key 注入页面（仅回环请求），
	// 浏览器因此不用手填；外部访问拿到的页面里 Key 为空。
	mux.Handle("GET /", web.Handler(a.cfg.APIKey))
}

func (a *App) apiState(w http.ResponseWriter, r *http.Request) {
	type chState struct {
		Channel          string       `json:"channel"`
		Kind             string       `json:"kind"`
		Region           string       `json:"region"`
		CheckinSupported bool         `json:"checkin_supported"`
		Healthy          int          `json:"healthy"`
		Total            int          `json:"total"`
		Accounts         []pool.State `json:"accounts"`
		// 模型表来源：上游实时 or 静态兜底，面板要如实展示（兜底表可能不准）
		ModelsLive  bool   `json:"models_live"`
		ModelsCount int    `json:"models_count"`
		ModelsAt    int64  `json:"models_at,omitempty"`
		ModelsErr   string `json:"models_err,omitempty"`
	}
	out := make([]chState, 0, len(a.runtimes))
	for _, ch := range allChannels() {
		p, ok := a.pools[ch]
		if !ok {
			continue
		}
		var live bool
		var mCount int
		var mAt time.Time
		var mErr string
		if rt := a.runtimes[ch]; rt != nil {
			live, mCount, mAt, mErr = rt.ModelsInfo()
		}
		st := chState{
			Channel:          ch.String(),
			Kind:             ch.Kind.String(),
			Region:           ch.Region.String(),
			CheckinSupported: ch.CheckinSupported(),
			Healthy:          p.Healthy(),
			Total:            p.Len(),
			Accounts:         p.List(),
			ModelsLive:       live,
			ModelsCount:      mCount,
			ModelsErr:        mErr,
		}
		if !mAt.IsZero() {
			st.ModelsAt = mAt.Unix()
		}
		out = append(out, st)
	}
	writeAPI(w, http.StatusOK, map[string]any{
		"listen":        a.cfg.Listen.Addr(),
		"auth_dir":      absPath(a.cfg.AuthDir),
		"checkin_times": a.cfg.Schedule.CheckinTimes,
		"channels":      out,
	})
}

// apiModelsRefresh 立即重新拉取各渠道的上游模型表。
func (a *App) apiModelsRefresh(w http.ResponseWriter, r *http.Request) {
	a.refreshModels()
	out := map[string]any{}
	for ch, rt := range a.runtimes {
		live, n, at, errMsg := rt.ModelsInfo()
		item := map[string]any{"live": live, "count": n, "error": errMsg}
		if !at.IsZero() {
			item["at"] = at.Unix()
		}
		out[ch.String()] = item
	}
	writeAPI(w, http.StatusOK, map[string]any{"channels": out})
}

// apiCheckin 立即执行签到。channel 参数为空则全部渠道。
func (a *App) apiCheckin(w http.ResponseWriter, r *http.Request) {
	results := a.sched.RunCheckinNow()
	a.saveAllStates()
	writeAPI(w, http.StatusOK, map[string]any{"results": results})
}

// apiRefresh 刷新指定账号或全部账号的 token 与积分。
func (a *App) apiRefresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Channel string `json:"channel"`
		UID     string `json:"uid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	type item struct {
		Channel string `json:"channel"`
		UID     string `json:"uid"`
		OK      bool   `json:"ok"`
		Remain  int64  `json:"remain,omitempty"`
		Msg     string `json:"msg,omitempty"`
	}
	var out []item
	for _, ch := range allChannels() {
		if req.Channel != "" && req.Channel != ch.String() {
			continue
		}
		p, ok := a.pools[ch]
		rt := a.runtimes[ch]
		if !ok || rt == nil {
			continue
		}
		for _, uid := range p.UIDs() {
			if req.UID != "" && req.UID != uid {
				continue
			}
			acct := p.Auth(uid)
			if acct == nil {
				continue
			}
			it := item{Channel: ch.String(), UID: uid}
			if err := rt.Upstream.RefreshToken(acct); err != nil {
				it.Msg = err.Error()
				out = append(out, it)
				continue
			}
			if err := acct.SaveAtomic(); err != nil {
				it.Msg = "刷新后落盘失败: " + err.Error()
				out = append(out, it)
				continue
			}
			if remain, err := rt.Upstream.UserResource(acct); err == nil {
				p.NoteRemain(acct, remain)
				it.Remain = remain
			}
			it.OK = true
			out = append(out, it)
		}
	}
	a.saveAllStates()
	writeAPI(w, http.StatusOK, map[string]any{"results": out})
}

// apiResource 查询单个账号的积分明细（每个权益包的额度 / 已用 / 剩余）。
//
// 面板展开账号详情时按需调用 —— 不放进 /api/state，避免每次轮询都打一遍上游。
func (a *App) apiResource(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Channel string `json:"channel"`
		UID     string `json:"uid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPI(w, http.StatusBadRequest, map[string]any{"error": "请求体解析失败: " + err.Error()})
		return
	}
	ch, ok := findChannel(req.Channel)
	if !ok {
		writeAPI(w, http.StatusBadRequest, map[string]any{"error": "未知渠道 " + req.Channel})
		return
	}
	rt := a.runtimes[ch]
	p := a.pools[ch]
	if rt == nil || p == nil {
		writeAPI(w, http.StatusServiceUnavailable, map[string]any{"error": "该渠道未装配"})
		return
	}
	acct := p.Auth(req.UID)
	if acct == nil {
		writeAPI(w, http.StatusNotFound, map[string]any{"error": "账号不存在: " + req.UID})
		return
	}
	total, items, err := rt.Upstream.UserResourceDetail(acct)
	if err != nil {
		writeAPI(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	p.NoteRemain(acct, total)
	a.saveAllStates()
	writeAPI(w, http.StatusOK, map[string]any{
		"channel": ch.String(),
		"uid":     req.UID,
		"total":   total,
		"items":   items,
	})
}

// findChannel 按 "workbuddy/cn" 形态的字符串找回渠道。
func findChannel(s string) (provider.Channel, bool) {
	for _, ch := range allChannels() {
		if ch.String() == s {
			return ch, true
		}
	}
	return provider.Channel{}, false
}

// apiImportLocal 扫描本机客户端凭证并导入。
func (a *App) apiImportLocal(w http.ResponseWriter, r *http.Request) {
	dryRun := r.URL.Query().Get("dry_run") == "1"
	scan := importauth.Scan()
	var results []importauth.Result
	if !dryRun && len(scan.Candidates) > 0 {
		var err error
		results, err = importauth.Import(scan.Candidates, a.cfg.AuthDir)
		if err != nil {
			writeAPI(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		// 把新导入的账号热加载进池
		a.reloadAccounts()
	}
	results = append(results, scan.Rejected...)
	writeAPI(w, http.StatusOK, map[string]any{
		"dry_run":    dryRun,
		"candidates": len(scan.Candidates),
		"results":    results,
		"notes":      scan.Notes,
	})
}

// reloadAccounts 重新扫描 authDir，把新增账号加入对应池（幂等）。
func (a *App) reloadAccounts() {
	accounts, err := auth.LoadDir(a.cfg.AuthDir)
	if err != nil {
		log.Printf("重新加载凭证失败: %v", err)
		return
	}
	for _, acct := range accounts {
		ch := provider.Channel{Kind: provider.Kind(acct.Kind), Region: acct.Region}
		if p, ok := a.pools[ch]; ok {
			p.Add(acct)
		}
	}
}

// ---- 登录 API ----

// apiLoginStart 发起交互式登录，返回可打开的授权 URL。
//
// 同渠道已有在途会话时复用既有 URL（幂等），避免重复点击产生多个 state。
func (a *App) apiLoginStart(w http.ResponseWriter, r *http.Request) {
	kind, reg, err := parseChannelQuery(r, true)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	s, err := a.logins.Start(kind, reg)
	if err != nil {
		writeAPI(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeAPI(w, http.StatusOK, map[string]any{
		"key":        s.Key(),
		"kind":       s.Kind.String(),
		"region":     s.Region.String(),
		"auth_url":   s.AuthURL,
		"created_at": s.CreatedAt.Format(time.RFC3339),
	})
}

// apiLoginPoll 轮询登录状态。
//
// 返回体三种形态：
//
//	{"pending":true}                        —— 浏览器还没走完授权
//	{"done":true,...}                       —— 成功，凭证已落盘并热加载进池
//	{"error":"..."} + 4xx/5xx               —— 失败，会话已作废
func (a *App) apiLoginPoll(w http.ResponseWriter, r *http.Request) {
	kind, reg, err := parseChannelQuery(r, false)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	out, err := a.logins.Poll(kind, reg)
	if err != nil {
		if errors.Is(err, loginsvc.ErrNoSession) {
			writeAPI(w, http.StatusNotFound, map[string]any{"error": "没有在途登录会话"})
			return
		}
		writeAPI(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	if out == nil {
		writeAPI(w, http.StatusOK, map[string]any{"pending": true})
		return
	}
	a.reloadAccounts()
	// 顺手拉一次余额，让面板立刻看到数字
	remain := int64(0)
	hasRemain := false
	ch := provider.Channel{Kind: out.Kind, Region: out.Region}
	if p, ok := a.pools[ch]; ok {
		if acct := p.Auth(out.UID); acct != nil {
			if rt := a.runtimes[ch]; rt != nil {
				if v, rerr := rt.Upstream.UserResource(acct); rerr == nil {
					p.NoteRemain(acct, v)
					remain, hasRemain = v, true
				}
			}
		}
	}
	a.saveAllStates()
	writeAPI(w, http.StatusOK, map[string]any{
		"done":       true,
		"kind":       out.Kind.String(),
		"region":     out.Region.String(),
		"uid":        out.UID,
		"nickname":   out.Nick,
		"domain":     out.Domain,
		"path":       out.Path,
		"remain":     remain,
		"has_remain": hasRemain,
	})
}

// apiLoginCancel 放弃在途登录会话。
func (a *App) apiLoginCancel(w http.ResponseWriter, r *http.Request) {
	kind, reg, err := parseChannelQuery(r, true)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	a.logins.Cancel(kind, reg)
	writeAPI(w, http.StatusOK, map[string]any{"ok": true})
}

// parseChannelQuery 从 query 或 JSON body 解析 kind + region。
//
// requireRegion 为 true 时 region 必须显式给出：登录是「往哪个账号库加号」的
// 动作，猜错会写到另一区，所以宁可报错也不默认。
func parseChannelQuery(r *http.Request, requireRegion bool) (provider.Kind, region.Region, error) {
	q := r.URL.Query()
	kindRaw := strings.TrimSpace(q.Get("kind"))
	regionRaw := strings.TrimSpace(q.Get("region"))
	if kindRaw == "" && regionRaw == "" && r.Body != nil {
		var body struct {
			Kind   string `json:"kind"`
			Region string `json:"region"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		kindRaw, regionRaw = strings.TrimSpace(body.Kind), strings.TrimSpace(body.Region)
	}
	if kindRaw == "" {
		return "", "", errors.New("缺少 kind（workbuddy | trae）")
	}
	if regionRaw == "" {
		if requireRegion {
			return "", "", errors.New("缺少 region（cn | global）")
		}
		regionRaw = region.CN.String()
	}
	switch strings.ToLower(kindRaw) {
	case "workbuddy", "codebuddy":
		kindRaw = string(provider.WorkBuddy)
	case "trae", "traework":
		kindRaw = string(provider.Trae)
	default:
		return "", "", fmt.Errorf("未知 kind %q", kindRaw)
	}
	// region.ParseStrict 拒绝未知值，不回退 CN ——
	// 否则 "glboal" 这类拼写错误会静默写到国内区。
	reg, ok := region.ParseStrict(regionRaw)
	if !ok {
		return "", "", fmt.Errorf("未知 region %q（cn | global）", regionRaw)
	}
	return provider.Kind(kindRaw), reg, nil
}

func writeAPI(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func absPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}
