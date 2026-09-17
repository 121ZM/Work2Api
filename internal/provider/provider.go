// Package provider 定义渠道标识、错误分类与 server / scheduler 依赖的最小上游接口。
//
// 与参考实现的关键差异：引入 **Channel = 渠道 + 版本** 的组合概念。
// region 是账号级属性，同一渠道的国内版与国际版是两条独立路由，
// 模型前缀显式区分（workbuddy-cn / workbuddy-global / trae-cn / trae-global），
// 同时保留不带版本的聚合别名（workbuddy / trae）用于跨版本自动选号。
package provider

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"work2api/internal/auth"
	"work2api/internal/region"
)

// Kind 渠道标识。
type Kind string

const (
	WorkBuddy Kind = "workbuddy"
	Trae      Kind = "traework"
)

func (k Kind) String() string { return string(k) }

// ErrCheckinUnsupported 该渠道 + 版本没有签到体系（国际版）。
//
// 调度器收到此错误时**必须跳过**：不计失败、不进入冷却、不查积分。
// 这是「国内版支持签到、国际版优雅跳过」的实现基础。
var ErrCheckinUnsupported = errors.New("该版本无签到体系")

// ErrAlreadyCheckedIn 表示今日已签到。
//
// 这是**正常状态，不是失败**。实测 WorkBuddy 国内版的签到接口把
// 「今天已签到」表达成 HTTP 400 + code=10001，与真实失败混在同一通道里；
// 若不区分，面板每天都会显示一条红色错误，调度器还会把好账号记成失败。
var ErrAlreadyCheckedIn = errors.New("今天已签到")

// ErrKind 错误分类，驱动 pool 冷却状态机。
type ErrKind int

const (
	ErrNone        ErrKind = iota // 成功
	ErrHardCredit                 // 余额 / 权益不足 → 长冷却
	ErrSoftRate                   // 429 软限流 → 短冷却
	ErrSessionDead                // 登录态失效 → 禁用
	ErrNotFound                   // 404 上游偶发 → 短冷却，不累计 errCount
	ErrServer                     // 5xx 上游故障
	ErrClient                     // 其他 4xx / 业务错误
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
	default:
		return "none"
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// Channel 渠道 + 版本的组合，如 workbuddy/cn。
type Channel struct {
	Kind   Kind
	Region region.Region
}

func (c Channel) String() string { return c.Kind.String() + "/" + c.Region.String() }

// Key 返回用作 map 键 / 日志字段的字符串，形如 "workbuddy|cn"。
//
// 注意不要退回成只用 Kind 作键 —— 那样国内版与国际版会互相覆盖。
func (c Channel) Key() string { return c.Kind.String() + "|" + c.Region.String() }

// CheckinSupported 报告该渠道 + 版本是否有签到体系。
func (c Channel) CheckinSupported() bool { return region.CheckinSupported(c.Kind.String(), c.Region) }

// prefixAliases 前缀 → 渠道 + 版本的映射。
//
// 键为模型名前缀（小写），Region 为空表示聚合别名（跨版本自动选号）。
var prefixAliases = map[string]Channel{
	"workbuddy":        {Kind: WorkBuddy},
	"codebuddy":        {Kind: WorkBuddy},
	"workbuddy-cn":     {Kind: WorkBuddy, Region: region.CN},
	"workbuddy-global": {Kind: WorkBuddy, Region: region.Global},
	"trae":             {Kind: Trae},
	"traework":         {Kind: Trae},
	"trae-cn":          {Kind: Trae, Region: region.CN},
	"trae-global":      {Kind: Trae, Region: region.Global},
}

// ParseChannel 解析模型名前缀。
//
// 第二个返回值 aggregated 为 true 时表示用的是不带版本的聚合别名，
// 调用方需在候选渠道中按优先级自动选号。
func ParseChannel(prefix string) (ch Channel, aggregated bool, ok bool) {
	ch, ok = prefixAliases[strings.ToLower(strings.TrimSpace(prefix))]
	if !ok {
		return Channel{}, false, false
	}
	return ch, ch.Region == "", true
}

// Prefix 返回该渠道类型在模型名里的规范前缀。
func (k Kind) Prefix() string {
	switch k {
	case WorkBuddy:
		return "workbuddy"
	case Trae:
		return "trae"
	default:
		return string(k)
	}
}

// Prefix 返回该渠道的规范模型前缀，与 prefixAliases 的键保持一致。
//
// 形如 "workbuddy-cn" / "trae-global"。**必须**与 ModelID 用的是同一个
// 来源，否则 /v1/models 暴露的 ID 会解析不回本渠道。
func (c Channel) Prefix() string {
	if c.Region.Valid() {
		return c.Kind.Prefix() + "-" + c.Region.String()
	}
	return c.Kind.Prefix()
}

// SplitModel 拆分模型名，返回 (前缀, 上游真实模型名, ok)。
//
// 接受两种形态：
//
//	"workbuddy-cn/glm-5.2"    规范形态，/v1/models 暴露的就是这个
//	"workbuddy/cn/glm-5.2"    兼容形态，容忍按 Channel.String() 拼出来的旧 ID
//
// 第二形态只在「首段是聚合别名 + 第二段是合法 region」时成立，
// 因此不会误吞上游本身带斜杠的模型名（如 "openai/gpt-4"）。
func SplitModel(model string) (prefix, name string, ok bool) {
	parts := strings.SplitN(strings.TrimSpace(model), "/", 3)
	if len(parts) < 2 {
		return "", "", false
	}
	first := strings.TrimSpace(parts[0])
	if len(parts) == 3 {
		_, aggregated, okc := ParseChannel(first)
		if !okc || !aggregated {
			return "", "", false
		}
		r, okr := region.ParseStrict(parts[1])
		if !okr {
			return "", "", false
		}
		rest := strings.TrimSpace(parts[2])
		if rest == "" {
			return "", "", false
		}
		return first + "-" + r.String(), rest, true
	}
	rest := strings.TrimSpace(parts[1])
	if rest == "" {
		return "", "", false
	}
	return first, rest, true
}

// ModelID 拼回对外暴露的模型 ID，如 "workbuddy-cn/glm-5.2"。
func ModelID(ch Channel, model string) string { return ch.Prefix() + "/" + model }

// ModelInfo 模型信息。
type ModelInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextWindow int64  `json:"context_window,omitempty"`
	MaxTokens     int64  `json:"max_tokens,omitempty"`
}

// ResourceItem 积分明细条目。
type ResourceItem struct {
	Name   string `json:"name"`
	Total  int64  `json:"total"`
	Used   int64  `json:"used"`
	Remain int64  `json:"remain"`
	// ExpireAt 该权益包的到期时间（Unix 秒），0 表示上游未给出。
	// 取值口径见 upstream.expireAt 的注释（实测三个候选字段，只 DeductionEndTime 恒有值）。
	ExpireAt int64 `json:"expire_at,omitempty"`
}

// Upstream 是 server / scheduler 依赖的最小上游能力集合。
type Upstream interface {
	// RefreshToken 刷新 access token；调用方负责 SaveAtomic 落盘。
	RefreshToken(a *auth.Auth) error
	// ChatStream 发起对话，返回上游 SSE body 流（调用方负责 Close）。
	// 非 2xx 时 rc 为 nil、respBody 为上游响应体、err 为 nil。
	ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error)
	FetchModels(a *auth.Auth) ([]ModelInfo, error)
	UserResource(a *auth.Auth) (int64, error)
	UserResourceDetail(a *auth.Auth) (int64, []ResourceItem, error)
	// DailyCheckin 执行签到。无签到体系的版本返回 ErrCheckinUnsupported。
	DailyCheckin(a *auth.Auth) error
	Classify(status int, body string) ErrKind
	// Stream 把上游 SSE 流转换 / 透传到下游 ResponseWriter。
	Stream(w http.ResponseWriter, r io.Reader) error
	// Aggregate 把上游 SSE 流聚合成一个 OpenAI 非流式响应对象。
	Aggregate(r io.Reader) (map[string]any, error)
}
