// Command work2api 把 WorkBuddy（腾讯 CodeBuddy）与 TRAE SOLO 的上游 API
// 反代成 OpenAI 兼容接口。
//
// 用法：
//
//	work2api serve  [-config config.json]                      启动反代服务（默认）
//	work2api login  -kind <workbuddy|trae> -region <cn|global> 交互式登录新账号
package main

import (
	"context"
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
	"work2api/internal/loginsvc"
	"work2api/internal/provider"
	"work2api/internal/region"
)

// version 由构建时注入：-ldflags "-X main.version=<ver>"，未注入时为 dev。
//
// 用注入而不是写死在源码里，是为了让「源码是什么版本」和「二进制是什么版本」
// 只有一个真相来源 —— 打包脚本把 git rev 与日期拼成版本号塞进来。
// 即便忘了注入，`go version -m work2api.exe` 仍能读回 vcs.revision，
// 所以这里不是唯一的溯源手段，只是给人看的那一个。
var version = "dev"

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "login":
			os.Exit(cmdLogin(args[1:]))
		case "serve", "run":
			os.Exit(cmdServe(args[1:]))
		case "version", "-v", "--version":
			printVersion()
			return
		case "help", "-h", "--help":
			usage()
			return
		}
	}
	os.Exit(cmdServe(args))
}

func printVersion() {
	fmt.Printf("work2api %s\n", version)
	fmt.Println("完整构建信息（含 commit）: go version -m <本程序>")
}

func usage() {
	fmt.Fprint(os.Stderr, `work2api —— WorkBuddy / TRAE SOLO 反代为 OpenAI 兼容接口

用法：
  work2api serve   [-config config.json]                        启动反代服务（默认）
  work2api login   -kind <workbuddy|trae> -region <cn|global>   交互式登录新账号
                   [-config config.json] [-timeout 5m]
  work2api version                                              打印版本号

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

	// 与 serve 同规则：登录产物（凭证文件）也必须落到同一个数据根，
	// 否则双击 bin\work2api.exe login 会把账号写进 bin\auths\，
	// 而服务从包根读 auths\ —— 登录「成功」但服务看不见这个账号。
	enterDataRoot(*cfgPath != "")

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

// enterDataRoot 把进程工作目录切到「数据根」，让「数据落在哪」不再取决于启动方式。
//
// 背景：配置里的路径都是相对 cwd 解析的，于是同一个发布包能长出两份互不相干的
// config.json（api_key 不同 → 已配置的客户端全部 401，而且毫无报错）：
//
//	托盘启动（工作目录 = 安装根）→ <安装根>\config.json
//	双击 bin\work2api.exe        → <安装根>\bin\config.json
//
// 只在**没有显式指定 -config** 时切换。显式指定配置文件的人是在自己挑目录，
// 此时动 cwd 会改变他配置里相对路径的含义 —— 那不是修 bug，是改语义。
func enterDataRoot(explicitConfig bool) {
	if explicitConfig {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return // 拿不到自身路径就什么都不动，退化成旧行为
	}
	enterDataRootFor(exe)
}

// enterDataRootFor 是可测的核心：exePath 由调用方注入，返回切换后的目录（未切换为空）。
func enterDataRootFor(exePath string) string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	root := config.DataRoot(exePath, cwd)
	if root == "" || root == cwd {
		return ""
	}
	if err := os.Chdir(root); err != nil {
		fmt.Fprintf(os.Stderr, "警告：无法切换到数据根 %s（继续用 %s）：%v\n", root, cwd, err)
		return ""
	}
	// 打一行，让这条静默生效的规则可被审计 —— 它以前正是静默错的。
	fmt.Fprintf(os.Stderr, "工作目录 %s（数据根）\n", root)
	return root
}

func cmdServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "配置文件路径（默认 ./config.json）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	enterDataRoot(*cfgPath != "")
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
