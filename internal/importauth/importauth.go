// Package importauth 从本机已登录的客户端扫描并导入账号凭证。
//
// 单向导入：读客户端文件 → 归一化 → 写入自己的 authDir。
//
// **绝不回写客户端文件**，理由：
//  1. 客户端会原子覆写 .info，双向写会互相覆盖登录态；
//  2. refresh token 会轮换，两方各自刷新必有一方被吊销，把用户桌面端踢下线；
//  3. .info 含 sessionState / sso / allAccounts 等字段，round-trip 不了，写回即丢数据；
//  4. 跨进程写文件存在权限与锁风险。
//
// 代价：这是**快照语义** —— 客户端先刷新则我方副本失效，收到 session dead
// 后需重新导入。
//
// 实测结论（2026-09-17，本机）：
//   - workbuddy-desktop.info    → 国内版（auth.domain = www.workbuddy.cn）
//   - workbuddy-desktop-ai.info → 国际版（auth.domain = www.workbuddy.ai）
//   - accounts / allAccounts 只含账号**元信息，不含各自的 token**；只有顶层
//     auth + account 是当前生效的登录态。因此一个客户端文件只能导入一个账号。
//   - 同目录存在轮转备份 workbuddy-desktop.<ISO时间戳>.<pid>.<uuid>.info，
//     所以必须**精确匹配文件名，不要 glob**。
//   - TRAE 的 iCubeAuthInfo://* 是 TLV 二进制（base64 后以 dGMFEAAA 开头），
//     不是明文 JWT，无法解析 → TRAE 只能走自建 OAuth 登录。
package importauth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"work2api/internal/auth"
	"work2api/internal/region"
)

// Action 导入动作。
type Action string

const (
	ActionCreated  Action = "created"  // 新增
	ActionUpdated  Action = "updated"  // 覆盖已有
	ActionRejected Action = "rejected" // 校验失败，未写入
)

// Result 单个账号的导入结果。
type Result struct {
	Kind     string        `json:"kind"`
	Region   region.Region `json:"region"`
	UID      string        `json:"uid"`
	Nickname string        `json:"nickname,omitempty"`
	Domain   string        `json:"domain,omitempty"`
	Action   Action        `json:"action"`
	Path     string        `json:"path,omitempty"`
	Reason   string        `json:"reason,omitempty"`
}

// ScanResult 一次扫描的汇总。
type ScanResult struct {
	Candidates []*auth.Auth `json:"-"`
	Notes      []string     `json:"notes"`   // 扫描过程说明（未找到客户端、Trae 不支持等）
	Rejected   []Result     `json:"skipped"` // 校验失败项
}

// source 一个客户端凭证落点。
type source struct {
	Kind   string
	Region region.Region
	File   string // 精确文件名（不 glob）
}

// workBuddySources WorkBuddy 双版本的凭证文件名。
//
// 两个文件位于同一目录，文件名后缀区分版本；这是 region 的第一判定依据，
// 随后用 auth.domain 交叉校验。
var workBuddySources = []source{
	{Kind: auth.KindWorkBuddy, Region: region.CN, File: "workbuddy-desktop.info"},
	{Kind: auth.KindWorkBuddy, Region: region.Global, File: "workbuddy-desktop-ai.info"},
}

// workBuddyAuthDir 返回 WorkBuddy 系客户端共享的凭证目录。
func workBuddyAuthDir() string {
	switch runtime.GOOS {
	case "windows":
		if dir := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); dir != "" {
			return filepath.Join(dir, "CodeBuddyExtension", "Data", "Public", "auth")
		}
	case "darwin":
		// 未在 macOS 上实测；路径按 Electron 惯例推断。
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support", "CodeBuddyExtension", "Data", "Public", "auth")
		}
	}
	return ""
}

// infoFile 客户端 .info 文件的结构（只取需要的字段）。
type infoFile struct {
	Account struct {
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
		Uin      string `json:"uin"`
		Type     string `json:"type"`
	} `json:"account"`
	Auth struct {
		AccessToken      string `json:"accessToken"`
		RefreshToken     string `json:"refreshToken"`
		ExpiresAt        int64  `json:"expiresAt"`        // 毫秒
		RefreshExpiresAt int64  `json:"refreshExpiresAt"` // 毫秒
		Domain           string `json:"domain"`
	} `json:"auth"`
	AllAccounts []struct {
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
	} `json:"allAccounts"`
}

// Scan 扫描本机所有已知客户端凭证落点。
//
// 返回可导入的候选账号；校验失败项放进 Rejected，过程说明放进 Notes。
// 本函数只读，不写任何文件。
func Scan() *ScanResult {
	res := &ScanResult{}

	dir := workBuddyAuthDir()
	if dir == "" {
		res.Notes = append(res.Notes, fmt.Sprintf("当前平台（%s）未支持自动定位 WorkBuddy 凭证目录", runtime.GOOS))
	} else if _, err := os.Stat(dir); err != nil {
		res.Notes = append(res.Notes, fmt.Sprintf("WorkBuddy 凭证目录不存在：%s", dir))
	} else {
		for _, s := range workBuddySources {
			cand, rej := loadWorkBuddySource(dir, s)
			if rej != nil {
				res.Rejected = append(res.Rejected, *rej)
				continue
			}
			if cand == nil {
				res.Notes = append(res.Notes, fmt.Sprintf("未找到 %s（%s）", s.File, s.Region))
				continue
			}
			res.Candidates = append(res.Candidates, cand)
		}
	}

	// TRAE 明确不支持导入：凭证是 TLV 二进制而非明文 JWT。
	res.Notes = append(res.Notes,
		"TRAE SOLO 的凭证（iCubeAuthInfo://*）是 TLV 二进制，无法解析，请使用 OAuth 登录添加")

	return res
}

// loadWorkBuddySource 读取单个 .info 文件。
// 返回 (候选, 拒绝项)，两者互斥；都为空表示文件不存在。
func loadWorkBuddySource(dir string, s source) (*auth.Auth, *Result) {
	path := filepath.Join(dir, s.File)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil // 文件不存在，不算错误
	}
	reject := func(reason string) *Result {
		return &Result{Kind: s.Kind, Region: s.Region, Action: ActionRejected, Path: path, Reason: reason}
	}

	var info infoFile
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, reject("解析失败（可能被客户端加密）：" + err.Error())
	}
	if strings.TrimSpace(info.Auth.AccessToken) == "" {
		return nil, reject("缺少 accessToken")
	}
	if strings.TrimSpace(info.Account.UID) == "" {
		return nil, reject("缺少 account.uid，无法生成稳定文件名")
	}

	// region 交叉校验：文件名判定 vs auth.domain 判定。
	// 不一致一律拒收 —— 错导会把国内版账号发往国际版端点（或反之）。
	if domain := strings.TrimSpace(info.Auth.Domain); domain != "" {
		if fromDomain := auth.RegionFromDomain(domain); fromDomain != s.Region {
			r := reject(fmt.Sprintf("region 冲突：文件名判定 %s，但 auth.domain=%q 判定 %s", s.Region, domain, fromDomain))
			r.UID, r.Nickname, r.Domain = info.Account.UID, info.Account.Nickname, domain
			return nil, r
		}
	}

	a := &auth.Auth{
		Kind:         s.Kind,
		Region:       s.Region,
		AccessToken:  info.Auth.AccessToken,
		RefreshToken: info.Auth.RefreshToken,
		ExpiresAt:    auth.NormalizeEpoch(info.Auth.ExpiresAt), // 毫秒 → 秒
		Domain:       info.Auth.Domain,
		UID:          info.Account.UID,
		Nickname:     info.Account.Nickname,
		FilePath:     path, // 仅作来源记录，导入时会被改写为目标路径
	}
	return a, nil
}

// Import 把候选账号写入 authDir。已存在同名文件则覆盖（快照刷新语义）。
//
// 返回每个账号的落地结果。写入使用原子写（tmp + rename）。
func Import(cands []*auth.Auth, authDir string) ([]Result, error) {
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建 authDir 失败: %w", err)
	}
	out := make([]Result, 0, len(cands))
	for _, c := range cands {
		target := filepath.Join(authDir, auth.FileName(c.Kind, c.Region, c.UID))
		_, statErr := os.Stat(target)
		action := ActionCreated
		if statErr == nil {
			action = ActionUpdated
		}

		// 写到我们自己的文件；绝不触碰客户端文件。
		// 注意：c.FilePath 仍指向客户端路径，此处用 SaveAtomicAs 显式指定目标。
		if err := c.SaveAtomicAs(target); err != nil {
			out = append(out, Result{
				Kind: c.Kind, Region: c.Region, UID: c.UID, Nickname: c.Nickname,
				Domain: c.Domain, Action: ActionRejected, Path: target,
				Reason: "写入失败：" + err.Error(),
			})
			continue
		}
		out = append(out, Result{
			Kind: c.Kind, Region: c.Region, UID: c.UID, Nickname: c.Nickname,
			Domain: c.Domain, Action: action, Path: target,
		})
	}
	return out, nil
}
