// Package server 暴露 OpenAI 兼容端点与管理 API。
//
// 路由核心：模型名前缀决定 Channel（渠道 + 版本），例如
//
//	workbuddy-cn/<model>      → 国内版 WorkBuddy 账号池
//	workbuddy-global/<model>  → 国际版 WorkBuddy 账号池
//	trae-cn/<model>           → 国内版 TRAE SOLO 账号池
//	trae-global/<model>       → 国际版 TRAE SOLO 账号池
//	workbuddy/<model>         → 聚合别名，跨版本自动选号（国内版优先）
//
// 每个 Channel 持有独立的 pool 与 upstream，国内版被限流不会波及国际版。
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"work2api/internal/auth"
	"work2api/internal/pool"
	"work2api/internal/provider"
)

const (
	maxBodyBytes = 8 << 20 // 8MB
	maxAttempts  = 4       // 单次请求最多尝试的账号数
	// refreshSkew token 剩余有效期低于此值时提前刷新。
	refreshSkew = 10 * time.Minute
	// stickyMaxReqs 同一账号连续成功多少次后主动轮换（提升上游缓存命中，又不至于长期压一个号）。
	stickyMaxReqs = 50
)

// Runtime 一个渠道 + 版本的运行时。
type Runtime struct {
	Channel      provider.Channel
	Pool         *pool.Pool
	Upstream     provider.Upstream
	StaticModels []provider.ModelInfo // 上游拉取失败时的兜底模型表

	// 上游实时模型表。StaticModels 只是兜底 —— 它既可能少（TRAE 只写了两三个），
	// 也可能多（WorkBuddy 静态表列了账号未必有权限的模型），必须以上游为准。
	modelsMu  sync.RWMutex
	models    []provider.ModelInfo
	modelsAt  time.Time
	modelsErr string
}

// SetModels 记录一次上游模型拉取结果。
//
// list 为空（拉取失败）时**保留上一次的结果**，只更新错误信息 ——
// 上游偶尔抽风不该让面板的模型列表忽然空掉或退回兜底表。
func (rt *Runtime) SetModels(list []provider.ModelInfo, at time.Time, err error) {
	rt.modelsMu.Lock()
	defer rt.modelsMu.Unlock()
	if len(list) > 0 {
		rt.models = list
		rt.modelsAt = at
		rt.modelsErr = ""
		return
	}
	if err != nil {
		rt.modelsErr = err.Error()
	}
}

// Models 返回当前应对外暴露的模型表，以及它是否来自上游实时结果。
func (rt *Runtime) Models() ([]provider.ModelInfo, bool) {
	rt.modelsMu.RLock()
	defer rt.modelsMu.RUnlock()
	if len(rt.models) > 0 {
		return rt.models, true
	}
	return rt.StaticModels, false
}

// ModelsInfo 返回模型表来源信息（供面板展示）。
func (rt *Runtime) ModelsInfo() (live bool, count int, at time.Time, errMsg string) {
	rt.modelsMu.RLock()
	defer rt.modelsMu.RUnlock()
	if len(rt.models) > 0 {
		return true, len(rt.models), rt.modelsAt, rt.modelsErr
	}
	return false, len(rt.StaticModels), rt.modelsAt, rt.modelsErr
}

// Config Handler 配置。
type Config struct {
	APIKey   string
	Runtimes map[provider.Channel]*Runtime
	// AttachAPI 可选挂载额外路由（管理 API / 静态面板）。
	//
	// protect 是 API Key 校验包装器，管理类路由应当用它包一层 ——
	// 面板路由（静态资源）不需要，但改状态 / 触发登录的路由必须。
	AttachAPI func(mux *http.ServeMux, protect func(http.HandlerFunc) http.HandlerFunc)
}

// Handler HTTP 处理器。
type Handler struct {
	cfg *Config
	mux *http.ServeMux

	stickyMu sync.RWMutex
	sticky   map[string]*stickyEntry // channel.Key() → 粘性记录
}

type stickyEntry struct {
	uid      string
	reqCount int
}

// New 构造 Handler 并注册路由。
func New(cfg *Config) *Handler {
	if cfg.Runtimes == nil {
		cfg.Runtimes = map[provider.Channel]*Runtime{}
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), sticky: map[string]*stickyEntry{}}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	if cfg.AttachAPI != nil {
		cfg.AttachAPI(h.mux, h.withAuth)
	}
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

// Protect 暴露 API Key 校验包装器，供外部挂载管理路由时使用。
func (h *Handler) Protect(next http.HandlerFunc) http.HandlerFunc { return h.withAuth(next) }

// withAuth 校验 Authorization: Bearer <api_key>；api_key 为空则不鉴权。
func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.APIKey != "" {
			token := bearerToken(r)
			if token != h.cfg.APIKey {
				writeError(w, http.StatusUnauthorized, "invalid api key", "invalid_request_error")
				return
			}
		}
		next(w, r)
	}
}

func bearerToken(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("Authorization")); v != "" {
		if rest, ok := strings.CutPrefix(v, "Bearer "); ok {
			return strings.TrimSpace(rest)
		}
		return v
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

// ---- /v1/models ----

func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	out := make([]modelEntry, 0, 64)
	now := time.Now().Unix()
	for _, ch := range h.channels() {
		rt := h.cfg.Runtimes[ch]
		if rt.Pool.Len() == 0 {
			continue // 没有该版本的账号就不暴露其模型，避免客户端选到必然失败的 ID
		}
		// 上游实时表优先，静态表只作兜底（见 Runtime.Models）
		models, _ := rt.Models()
		for _, m := range models {
			out = append(out, modelEntry{
				ID:      provider.ModelID(ch, m.ID),
				Object:  "model",
				Created: now,
				OwnedBy: ch.String(),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

// ---- /v1/chat/completions ----

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error(), "invalid_request_error")
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error")
		return
	}
	model, _ := req["model"].(string)
	stream, _ := req["stream"].(bool)

	ch, modelName, err := h.resolveChannel(model)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	rt := h.cfg.Runtimes[ch]
	if rt == nil || rt.Pool.Len() == 0 {
		writeError(w, http.StatusServiceUnavailable,
			fmt.Sprintf("渠道 %s 没有可用账号", ch), "invalid_request_error")
		return
	}

	upstreamBody := rewriteModel(body, modelName)

	var lastMsg string
	var lastKind provider.ErrKind
	for attempt := 0; attempt < maxAttempts; attempt++ {
		acct := h.pick(rt)
		if acct == nil {
			lastMsg = "该渠道账号全部处于冷却或停用状态"
			lastKind = provider.ErrSoftRate
			break
		}
		if err := h.ensureFresh(rt, acct); err != nil {
			rt.Pool.NoteError(acct, provider.ErrSessionDead, err.Error())
			lastMsg, lastKind = err.Error(), provider.ErrSessionDead
			continue
		}

		rc, status, respBody, err := rt.Upstream.ChatStream(acct, upstreamBody)
		if err != nil {
			rt.Pool.NoteError(acct, provider.ErrServer, err.Error())
			lastMsg, lastKind = err.Error(), provider.ErrServer
			continue
		}
		if rc == nil {
			// 上游返回了非 2xx：分类 → 记冷却 → 换账号重试
			kind := rt.Upstream.Classify(status, string(respBody))
			log.Printf("chat channel=%s uid=%s attempt=%d upstream_status=%d kind=%s",
				ch, acct.UID, attempt+1, status, kind)

			// 请求形态类错误（如 code=11128 "first message is not system prompt"、
			// code=11101 tool_choice 类型错）：换账号也是同一个结果，
			// 既不该重试，也不该把账号拖进冷却 —— 直接透传给客户端。
			if kind == provider.ErrClient {
				h.clearSticky(ch)
				writeError(w, status, upstreamMsg(respBody, status), "invalid_request_error")
				return
			}

			rt.Pool.NoteError(acct, kind, string(respBody))
			h.clearSticky(ch)
			lastKind, lastMsg = kind, fmt.Sprintf("上游 %d: %s", status, truncate(string(respBody), 300))
			continue
		}

		rt.Pool.NoteSuccess(acct)
		h.noteSticky(ch, acct.UID)
		log.Printf("chat channel=%s uid=%s stream=%t attempt=%d", ch, acct.UID, stream, attempt+1)

		if stream {
			_ = rt.Upstream.Stream(w, rc)
		} else {
			resp, aerr := rt.Upstream.Aggregate(rc)
			_ = rc.Close()
			if aerr != nil {
				writeError(w, http.StatusBadGateway, "聚合上游响应失败: "+aerr.Error(), "api_error")
				return
			}
			resp["model"] = model // 回填客户端请求的完整模型 ID
			writeJSON(w, http.StatusOK, resp)
		}
		_ = rc.Close()
		return
	}

	status := http.StatusBadGateway
	if lastKind == provider.ErrSoftRate || lastKind == provider.ErrHardCredit {
		status = http.StatusTooManyRequests
	}
	writeError(w, status, lastMsg, "api_error")
}

// resolveChannel 解析模型名前缀。
//
// 显式前缀直接命中；聚合别名（workbuddy / trae）在候选版本中挑一个
// 有健康账号的，国内版优先。
func (h *Handler) resolveChannel(model string) (provider.Channel, string, error) {
	prefix, name, ok := provider.SplitModel(model)
	if !ok {
		return provider.Channel{}, "", fmt.Errorf(
			"模型 ID 必须带渠道前缀，形如 workbuddy-cn/<model> / workbuddy-global/<model> / trae-cn/<model>；收到 %q", model)
	}
	ch, aggregated, ok := provider.ParseChannel(prefix)
	if !ok {
		return provider.Channel{}, "", fmt.Errorf("未知渠道前缀 %q", prefix)
	}
	if !aggregated {
		return ch, name, nil
	}
	for _, cand := range h.candidates(ch.Kind) {
		if rt := h.cfg.Runtimes[cand]; rt != nil && rt.Pool.Healthy() > 0 {
			return cand, name, nil
		}
	}
	// 全都没健康账号时退回第一个存在的版本，让它走正常的重试与报错路径
	for _, cand := range h.candidates(ch.Kind) {
		if rt := h.cfg.Runtimes[cand]; rt != nil && rt.Pool.Len() > 0 {
			return cand, name, nil
		}
	}
	return provider.Channel{}, "", fmt.Errorf("渠道 %s 没有任何可用账号", ch.Kind)
}

// candidates 返回某渠道的全部版本，国内版优先。
func (h *Handler) candidates(kind provider.Kind) []provider.Channel {
	out := []provider.Channel{{Kind: kind, Region: "cn"}, {Kind: kind, Region: "global"}}
	if _, ok := h.cfg.Runtimes[out[0]]; !ok {
		// 配置里没有 cn 时只返回实际存在的
		out = out[1:]
	}
	return out
}

// channels 返回全部已配置渠道，按稳定顺序排列。
func (h *Handler) channels() []provider.Channel {
	out := make([]provider.Channel, 0, len(h.cfg.Runtimes))
	for ch := range h.cfg.Runtimes {
		out = append(out, ch)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

// ---- 账号选择与粘性路由 ----

func (h *Handler) pick(rt *Runtime) *auth.Auth {
	key := rt.Channel.Key()

	h.stickyMu.RLock()
	st := h.sticky[key]
	h.stickyMu.RUnlock()

	if st != nil && st.reqCount < stickyMaxReqs {
		// 粘性账号必须**自身**可用才复用。早期写的是
		// `Pool.Auth(uid) != nil && Pool.Healthy() > 0` —— Healthy() 是池级计数、
		// 与这个账号无关，导致已停用 / 已进冷却的粘性账号被继续选中，
		// 要等它失败一次（clearSticky）才换号。
		if a := rt.Pool.UsableAuth(st.uid); a != nil {
			return a
		}
	}
	a, err := rt.Pool.Pick()
	if err != nil {
		return nil
	}
	h.stickyMu.Lock()
	h.sticky[key] = &stickyEntry{uid: a.UID}
	h.stickyMu.Unlock()
	return a
}

func (h *Handler) noteSticky(ch provider.Channel, uid string) {
	h.stickyMu.Lock()
	defer h.stickyMu.Unlock()
	st := h.sticky[ch.Key()]
	if st == nil || st.uid != uid {
		h.sticky[ch.Key()] = &stickyEntry{uid: uid, reqCount: 1}
		return
	}
	st.reqCount++
	if st.reqCount >= stickyMaxReqs {
		delete(h.sticky, ch.Key()) // 达上限后下次重新选号
	}
}

func (h *Handler) clearSticky(ch provider.Channel) {
	h.stickyMu.Lock()
	delete(h.sticky, ch.Key())
	h.stickyMu.Unlock()
}

// ensureFresh 必要时刷新 token 并落盘。
func (h *Handler) ensureFresh(rt *Runtime, a *auth.Auth) error {
	if !a.NeedsRefresh(refreshSkew) {
		return nil
	}
	log.Printf("refresh on demand channel=%s uid=%s", rt.Channel, a.UID)
	if err := rt.Upstream.RefreshToken(a); err != nil {
		return err
	}
	if err := a.SaveAtomic(); err != nil {
		log.Printf("refresh save failed uid=%s err=%v", a.UID, err)
	}
	return nil
}

// ---- /status 与 /healthz ----

type channelStatus struct {
	Channel          string       `json:"channel"`
	Kind             string       `json:"kind"`
	Region           string       `json:"region"`
	Healthy          int          `json:"healthy"`
	Total            int          `json:"total"`
	CheckinSupported bool         `json:"checkin_supported"`
	Accounts         []pool.State `json:"accounts"`
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	out := make([]channelStatus, 0, len(h.cfg.Runtimes))
	for _, ch := range h.channels() {
		rt := h.cfg.Runtimes[ch]
		out = append(out, channelStatus{
			Channel:          ch.String(),
			Kind:             ch.Kind.String(),
			Region:           ch.Region.String(),
			Healthy:          rt.Pool.Healthy(),
			Total:            rt.Pool.Len(),
			CheckinSupported: ch.CheckinSupported(),
			Accounts:         rt.Pool.List(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": out})
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	healthy, total := 0, 0
	for _, ch := range h.channels() {
		rt := h.cfg.Runtimes[ch]
		healthy += rt.Pool.Healthy()
		total += rt.Pool.Len()
	}
	status := http.StatusOK
	if healthy == 0 {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{
		"healthy": healthy, "total": total, "service": "work2api",
	})
}

// ---- 辅助 ----

// rewriteModel 把请求体里的 model 换成上游裸模型名（去掉渠道前缀）。
func rewriteModel(body []byte, name string) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	obj["model"] = name
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg, typ string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": typ},
	})
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// upstreamMsg 从上游错误信封里抽出可读信息，失败时回退到截断原文。
//
// 上游信封形如 {"code":11128,"msg":"first message is not system prompt",...}。
// 直接回原文会让客户端看到一大坨 JSON，抽出 code + msg 更好定位问题。
func upstreamMsg(body []byte, status int) string {
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Code != 0 {
		msg := strings.TrimSpace(env.Msg)
		if msg == "" {
			msg = "上游拒绝了该请求"
		}
		return fmt.Sprintf("上游 %d code=%d: %s", status, env.Code, msg)
	}
	return fmt.Sprintf("上游 %d: %s", status, truncate(string(body), 300))
}
