//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"
)

// ── 服务生命周期 ────────────────────────────────────────────────────────
//
// 托盘是**唯一入口**：自己起来时顺手把服务拉起来，菜单里能起能停。
//
// 原来是拆成两处的 —— tools\start-work2api.vbs 负责开机拉起服务，托盘只
// 负责「打开页面 / 关闭服务」。两边互不知道对方存在，于是有个很别扭的洞：
// 托盘能把服务关掉，却没有能力把它起回来；服务被关之后只能重新登录才能恢复。
// 现在收成一处：vbs 只负责启动托盘，服务归托盘管。

var (
	procCreateProcessW = kernel32.NewProc("CreateProcessW")
	procCreateFileW    = kernel32.NewProc("CreateFileW")
)

const (
	// 菜单命令（接在 main.go 的 cmdOpenPage / cmdStopService 之后）
	cmdStartService = 1003

	// 菜单项状态
	mfGrayed    = 0x00000001
	mfSeparator = 0x00000800

	// CreateFile
	fileAppendData      = 0x00000004
	fileShareRead       = 0x00000001
	fileShareWrite      = 0x00000002
	openAlways          = 0x00000004
	fileAttributeNormal = 0x00000080

	// CreateProcess
	createNoWindow      = 0x08000000
	startfUseStdHandles = 0x00000100

	invalidHandleValue = ^uintptr(0)
)

// startupInfoW / processInformation 虽然只用到前几个字段，但**必须**按 Win32
// 的完整布局声明 —— CreateProcessW 按 cbSize 和固定偏移读，少一个字段整个
// 结构就错位了，而且是静默错位（调用返回成功，参数却乱七八糟）。
type startupInfoW struct {
	cb              uint32
	lpReserved      *uint16
	lpDesktop       *uint16
	lpTitle         *uint16
	dwX             uint32
	dwY             uint32
	dwXSize         uint32
	dwYSize         uint32
	dwXCountChars   uint32
	dwYCountChars   uint32
	dwFillAttribute uint32
	dwFlags         uint32
	wShowWindow     uint16
	cbReserved2     uint16
	lpReserved2     *byte
	hStdInput       uintptr
	hStdOutput      uintptr
	hStdErr         uintptr
}

// securityAttributes 对应 Win32 的 SECURITY_ATTRIBUTES。
// 唯一用到的字段是 bInheritHandle —— 见 openInheritable。
type securityAttributes struct {
	nLength              uint32
	lpSecurityDescriptor uintptr
	bInheritHandle       uint32
}

type processInformation struct {
	hProcess    uintptr
	hThread     uintptr
	dwProcessID uint32
	dwThreadID  uint32
}

// servicePaths 返回服务 exe、日志文件与**工作目录**。
//
// 工作目录必须是 dist 的上一级（仓库根）：服务是按相对路径读 config.json
// 和 auths/ 的，而托盘自己住在 dist/ 下 —— 工作目录给错，服务能起来但读不到
// 配置。原先这件事藏在 vbs 的 sh.CurrentDirectory 里，现在搬进代码。
func servicePaths() (svcExe, logPath, workDir string) {
	exeDir := filepath.Dir(exePath())
	return filepath.Join(exeDir, serviceExe),
		filepath.Join(exeDir, "serve.log"),
		filepath.Dir(exeDir)
}

// serviceRunning 判断服务是否已在跑。
func serviceRunning() bool {
	pids, err := findProcesses(serviceExe)
	return err == nil && len(pids) > 0
}

// startService 拉起服务；已经在跑就什么都不做。
//
// 幂等是必须的（vbs 里踩过同一个坑）：端口被占时 Go 进程只打一行日志、
// **仍然继续运行**，于是留下一个占着内存却永不接流量的幽灵进程。
//
// 全程不经过 shell：直接 CreateProcessW，日志文件用可继承句柄喂给子进程。
// 早先的版本写的是 `cmd.exe /c ... >> serve.log 2>&1`，借 shell 换重定向 ——
// 短，但把功能押在「允许拉起 cmd.exe」上，而本机沙箱是把 cmd 当 LOLBin 拦的
// （wscript 已经被拦过一次）。多写 40 行 Win32，换掉这个假设，值。
func startService() error {
	if serviceRunning() {
		logf("启动服务：%s 已在运行，跳过", serviceExe)
		return nil
	}
	svcExe, logPath, workDir := servicePaths()
	if _, err := os.Stat(svcExe); err != nil {
		return fmt.Errorf("找不到服务程序：%w", err)
	}

	hLog, err := openInheritable(logPath)
	if err != nil {
		return err
	}
	defer procCloseHandle.Call(hLog)

	var si startupInfoW
	si.cb = uint32(unsafe.Sizeof(si))
	si.dwFlags = startfUseStdHandles
	si.hStdOutput = hLog
	si.hStdErr = hLog

	// 命令行必须带引号：路径含空格时 CreateProcessW 会按空格切分。
	cmdline := utf16p(`"` + svcExe + `" serve`)
	dir := utf16p(workDir)

	var pi processInformation
	ret, _, callErr := procCreateProcessW.Call(
		0,
		uintptr(unsafe.Pointer(cmdline)),
		0, 0,
		1, // bInheritHandles=TRUE：把上面的日志句柄传下去
		createNoWindow,
		0,
		uintptr(unsafe.Pointer(dir)),
		uintptr(unsafe.Pointer(&si)),
		uintptr(unsafe.Pointer(&pi)),
	)
	runtime.KeepAlive(cmdline)
	runtime.KeepAlive(dir)
	if ret == 0 {
		return fmt.Errorf("CreateProcessW 失败：%w", callErr)
	}
	procCloseHandle.Call(pi.hThread)
	procCloseHandle.Call(pi.hProcess)
	logf("启动服务：%s serve（pid=%d，工作目录 %s，日志 %s）",
		svcExe, pi.dwProcessID, workDir, logPath)
	return nil
}

// openInheritable 打开（不存在则创建）日志文件，返回**可被子进程继承**的句柄。
//
// 关键在 SECURITY_ATTRIBUTES.bInheritHandle：CreateFileW 默认给的句柄不可继承，
// 直接塞进 hStdOutput 的话子进程拿到的是无效句柄，服务一启动就写日志失败 ——
// 而且它只是静默丢日志，不会报错，很难查。这一步没有捷径：
// os.OpenFile 拿不到可继承句柄。
func openInheritable(path string) (uintptr, error) {
	p := utf16p(path)
	sa := securityAttributes{
		nLength:        uint32(unsafe.Sizeof(securityAttributes{})),
		bInheritHandle: 1,
	}
	h, _, err := procCreateFileW.Call(
		uintptr(unsafe.Pointer(p)),
		fileAppendData,
		fileShareRead|fileShareWrite,
		uintptr(unsafe.Pointer(&sa)),
		openAlways,
		fileAttributeNormal,
		0,
	)
	runtime.KeepAlive(p)
	if h == invalidHandleValue {
		return 0, fmt.Errorf("打开日志 %s 失败：%w", path, err)
	}
	return h, nil
}

// ensureService 供托盘启动时调用。失败只记日志，**不拦住托盘** ——
// 托盘起不来就彻底没辙了，服务起不来至少还能从菜单再点一次。
func ensureService() {
	if err := startService(); err != nil {
		logf("自动启动服务失败: %v", err)
	}
}

// startServiceMenu 是菜单里的「启动服务」。这里失败要弹框：
// 用户点了没动静会以为是自己没点准。
func startServiceMenu() {
	logf("菜单：启动服务")
	if err := startService(); err != nil {
		logf("启动服务失败: %v", err)
		fatal("启动服务失败：" + err.Error())
	}
}

// menuFlags 在 enabled=false 时补上 MF_GRAYED。
// 置灰项点不动，所以 TrackPopupMenu 不会把它的 cmd 返回来。
func menuFlags(base uintptr, enabled bool) uintptr {
	if !enabled {
		return base | mfGrayed
	}
	return base
}
