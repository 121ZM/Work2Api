// Command work2api 把 WorkBuddy（腾讯 CodeBuddy）与 TRAE SOLO 的上游 API
// 反代成 OpenAI 兼容接口。
//
// 用法：
//
//	work2api serve  [-config config.json]                      启动反代服务（默认）
//	work2api import [-config config.json] [-dry-run] [-json]   扫描并导入本机客户端账号
//	work2api login  -kind <workbuddy|trae> -region <cn|global> 交互式登录新账号
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"work2api/internal/app"
	"work2api/internal/config"
	"work2api/internal/importauth"
	"work2api/internal/loginsvc"
	"work2api/internal/provider"
	"work2api/internal/region"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "import":
			os.Exit(cmdImport(args[1:]))
		case "login":
			os.Exit(cmdLogin(args[1:]))
		case "serve", "run":
			os.Exit(cmdServe(args[1:]))
		case "help", "-h", "--help":
			usage()
			return
		}
	}
	os.Exit(cmdServe(args))
}

func usage() {
	fmt.Fprint(os.Stderr, `work2api —— WorkBuddy / TRAE SOLO 反代为 OpenAI 兼容接口

用法：
  work2api serve  [-config config.json]                        启动反代服务（默认）
  work2api import [-config config.json] [-dry-run] [-json]     扫描并导入本机客户端账号
  work2api login  -kind <workbuddy|trae> -region <cn|global>   交互式登录新账号
                  [-config config.json] [-timeout 5m]

环境变量覆盖：W2A_LISTEN / W2A_API_KEY / W2A_AUTH_DIR / W2A_STATE_FILE /
              W2A_HARD_CREDIT / W2A_SOFT_RATE / W2A_ERR_THRESHOLD /
              W2A_ERR_COOLDOWN / W2A_TIMEOUT_SECONDS
`)
}

// loadConfig 读取配置。
//
// 路径为空时按 ./config.json → ./config.local.json 顺序探测；
// 都不存在就落到 ./config.json 并**创建**它 —— 首次运行要在这里生成随机
// api_key，不落盘的话每次重启都会换 Key，已配置的客户端会全部 401。
func loadConfig(path string) (*config.Config, error) {
	if strings.TrimSpace(path) == "" {
		for _, cand := range []string{"config.json", "config.local.json"} {
			if _, err := os.Stat(cand); err == nil {
				path = cand
				break
			}
		}
		if path == "" {
			path = "config.json"
		}
	}
	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		cfg, err := config.Load("")
		if err != nil {
			return nil, err
		}
		if err := config.Save(cfg, path); err != nil {
			return nil, fmt.Errorf("写入默认配置 %s：%w", path, err)
		}
		return cfg, nil
	}
	return config.Load(path)
}

func cmdImport(args []string) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "配置文件路径（默认 ./config.json）")
	authDir := fs.String("auth-dir", "", "凭证输出目录（覆盖配置文件）")
	dryRun := fs.Bool("dry-run", false, "只扫描不写入")
	asJSON := fs.Bool("json", false, "以 JSON 输出")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败：%v\n", err)
		return 1
	}
	if *authDir != "" {
		cfg.AuthDir = *authDir
	}

	scan := importauth.Scan()

	var results []importauth.Result
	if len(scan.Candidates) > 0 && !*dryRun {
		results, err = importauth.Import(scan.Candidates, cfg.AuthDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "导入失败：%v\n", err)
			return 1
		}
	}
	results = append(results, scan.Rejected...)

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		_ = enc.Encode(map[string]any{
			"auth_dir":   absOrSelf(cfg.AuthDir),
			"dry_run":    *dryRun,
			"candidates": len(scan.Candidates),
			"results":    results,
			"notes":      scan.Notes,
		})
		return 0
	}

	fmt.Printf("凭证目录：%s\n", absOrSelf(cfg.AuthDir))
	if *dryRun {
		fmt.Println("模式：只扫描（-dry-run），未写入任何文件")
	}
	fmt.Println()

	if len(scan.Candidates) == 0 {
		fmt.Println("未发现可导入的账号。")
	} else {
		fmt.Printf("发现 %d 个账号：\n", len(scan.Candidates))
		for _, c := range scan.Candidates {
			exp := "无过期时间"
			if c.ExpiresAt > 0 {
				exp = fmt.Sprintf("expiresAt=%d", c.ExpiresAt)
			}
			fmt.Printf("  [%-6s] %-10s %-20s %s\n", c.Region, c.Kind, c.Label(), exp)
		}
	}

	if len(results) > 0 {
		fmt.Println()
		for _, r := range results {
			line := fmt.Sprintf("  %-9s %-6s %-10s %s", r.Action, r.Region, r.Kind, r.UID)
			if r.Nickname != "" {
				line += " (" + r.Nickname + ")"
			}
			if r.Reason != "" {
				line += " —— " + r.Reason
			}
			fmt.Println(line)
		}
	}

	if len(scan.Notes) > 0 {
		fmt.Println()
		for _, n := range scan.Notes {
			fmt.Println("提示：" + n)
		}
	}
	return 0
}

// cmdLogin 交互式登录：发起 → 打印授权 URL → 轮询直到完成或超时。
//
// 不自动打开浏览器：打开动作交给用户，避免命令行的隐式外部副作用。
func cmdLogin(args []string) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "配置文件路径（默认 ./config.json）")
	kindRaw := fs.String("kind", "", "渠道：workbuddy | trae（必填）")
	regionRaw := fs.String("region", "", "版本：cn | global（必填）")
	authDir := fs.String("auth-dir", "", "凭证输出目录（覆盖配置文件）")
	timeout := fs.Duration("timeout", 5*time.Minute, "等待授权完成的超时时间")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	kind, reg, err := normalizeKindRegion(*kindRaw, *regionRaw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 2
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败：%v\n", err)
		return 1
	}
	if *authDir != "" {
		cfg.AuthDir = *authDir
	}

	mgr := loginsvc.New(filepath.Dir(cfg.StateFile), cfg.AuthDir)
	mgr.WBOverrides = cfg.Regions.WorkBuddy
	mgr.TraeOverrides = cfg.Regions.Trae

	sess, err := mgr.Start(kind, reg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "发起登录失败：%v\n", err)
		return 1
	}

	fmt.Printf("登录 %s · %s\n\n", kind, regionLabel(reg))
	fmt.Printf("  在浏览器打开：\n    %s\n\n", sess.AuthURL)
	fmt.Printf("  凭证将写入：%s\n", absOrSelf(cfg.AuthDir))
	fmt.Printf("  等待授权完成（最多 %s，Ctrl+C 取消）…\n", timeout.String())

	deadline := time.Now().Add(*timeout)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		out, err := mgr.Poll(kind, reg)
		if err != nil {
			if errors.Is(err, loginsvc.ErrNoSession) {
				fmt.Fprintln(os.Stderr, "\n会话已失效，请重新发起。")
				return 1
			}
			fmt.Fprintf(os.Stderr, "\n登录失败：%v\n", err)
			return 1
		}
		if out == nil {
			continue // 尚未完成
		}
		fmt.Printf("\n登录成功\n")
		fmt.Printf("  账号    %s\n", out.UID)
		if out.Nick != "" {
			fmt.Printf("  昵称    %s\n", out.Nick)
		}
		fmt.Printf("  版本    %s（domain=%s）\n", regionLabel(out.Region), out.Domain)
		fmt.Printf("  已写入  %s\n", absOrSelf(out.Path))
		return 0
	}

	mgr.Cancel(kind, reg)
	fmt.Fprintf(os.Stderr, "\n等待超时（%s），已取消本次登录。\n", timeout.String())
	return 1
}

// normalizeKindRegion 归一化并校验 CLI 传入的渠道 / 版本。
//
// 与 HTTP 侧同样严格：region 未知值**不**回退 CN，直接报错 ——
// 写错区会让账号落到另一套账号库里。
func normalizeKindRegion(kindRaw, regionRaw string) (provider.Kind, region.Region, error) {
	var kind provider.Kind
	switch strings.ToLower(strings.TrimSpace(kindRaw)) {
	case "workbuddy", "codebuddy":
		kind = provider.WorkBuddy
	case "trae", "traework":
		kind = provider.Trae
	case "":
		return "", "", errors.New("缺少 -kind（workbuddy | trae）")
	default:
		return "", "", fmt.Errorf("未知 -kind %q（workbuddy | trae）", kindRaw)
	}

	var reg region.Region
	switch {
	case strings.TrimSpace(regionRaw) == "":
		return "", "", errors.New("缺少 -region（cn | global）")
	default:
		var ok bool
		reg, ok = region.ParseStrict(regionRaw)
		if !ok {
			return "", "", fmt.Errorf("未知 -region %q（cn | global）", regionRaw)
		}
	}
	return kind, reg, nil
}

func regionLabel(r region.Region) string {
	if r == region.Global {
		return "国际版"
	}
	return "国内版"
}

func cmdServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "配置文件路径（默认 ./config.json）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败：%v\n", err)
		return 1
	}

	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[work2api] ")

	a, err := app.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "装配失败：%v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := a.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "运行失败：%v\n", err)
		return 1
	}
	return 0
}

func absOrSelf(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}
