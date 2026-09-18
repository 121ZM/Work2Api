//go:build windows

// Command work2api-tray —— Work2Api 反代服务的系统托盘控制器。
//
// 常驻通知区域（右下角），右键菜单两项：
//
//	打开页面 —— 用默认浏览器打开面板 http://127.0.0.1:7865
//	关闭服务 —— 结束所有 work2api.exe 进程
//
// 实现口径与主程序一致：纯标准库 + syscall 直调 Win32，零第三方依赖。
//
// 构建：
//
//	go build -ldflags "-H=windowsgui" -o dist/work2api-tray.exe ./cmd/work2api-tray
//
// 结束托盘程序本身：任务管理器里结束 work2api-tray.exe
// （右键菜单刻意只有需求指定的两项，没有「退出」）。
//
// 排查：程序以 -H=windowsgui 构建，没有控制台。启动/菜单操作会追加写入
// exe 同目录的 tray.log；致命错误另外弹一个消息框（标题「Work2Api 托盘」）。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// ---------------- Win32 绑定 ----------------

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")

	procRegisterClassExW    = user32.NewProc("RegisterClassExW")
	procCreateWindowExW     = user32.NewProc("CreateWindowExW")
	procDefWindowProcW      = user32.NewProc("DefWindowProcW")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessageW    = user32.NewProc("DispatchMessageW")
	procPostQuitMessage     = user32.NewProc("PostQuitMessage")
	procPostMessageW        = user32.NewProc("PostMessageW")
	procLoadIconW           = user32.NewProc("LoadIconW")
	procCreateIconIndirect  = user32.NewProc("CreateIconIndirect")
	procCreatePopupMenu     = user32.NewProc("CreatePopupMenu")
	procAppendMenuW         = user32.NewProc("AppendMenuW")
	procTrackPopupMenu      = user32.NewProc("TrackPopupMenu")
	procDestroyMenu         = user32.NewProc("DestroyMenu")
	procGetCursorPos        = user32.NewProc("GetCursorPos")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procMessageBoxW         = user32.NewProc("MessageBoxW")

	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	procCreateMutexW     = kernel32.NewProc("CreateMutexW")
	procOpenProcess      = kernel32.NewProc("OpenProcess")
	procTerminateProcess = kernel32.NewProc("TerminateProcess")
	procCloseHandle      = kernel32.NewProc("CloseHandle")

	procCreateToolhelp32Snapshot = kernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32FirstW          = kernel32.NewProc("Process32FirstW")
	procProcess32NextW           = kernel32.NewProc("Process32NextW")

	procCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	procDeleteDC           = gdi32.NewProc("DeleteDC")
	procCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	procCreateBitmap       = gdi32.NewProc("CreateBitmap")

	procShellNotifyIconW = shell32.NewProc("Shell_NotifyIconW")
	procShellExecuteW    = shell32.NewProc("ShellExecuteW")
)

// ---------------- 常量 ----------------

const (
	// 窗口消息
	wmNull      = 0x0000
	wmDestroy   = 0x0002
	wmLButtonUp = 0x0202
	wmRButtonUp = 0x0205
	wmApp       = 0x8000
	wmTrayIcon  = wmApp + 1

	// Shell_NotifyIcon
	nimAdd     = 0x00000000
	nimDelete  = 0x00000002
	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004

	// 弹出菜单
	mfString       = 0x00000000
	tpmRightButton = 0x0002
	tpmNoNotify    = 0x0080
	tpmReturnCmd   = 0x0100

	// ShellExecute
	swShownormal = 1

	// 进程枚举 / 结束
	th32csSnapProcess = 0x00000002
	processTerminate  = 0x0001
	maxPath           = 260

	// GDI
	dibRGBColors = 0
	biRGB        = 0

	// 系统兜底图标
	idiApplication = 32512

	// 菜单命令
	cmdOpenPage    = 1001
	cmdStopService = 1002

	// MessageBox
	mbIconError = 0x00000010

	errAlreadyExists = 183
)

// 面板地址（与服务端 config.json 的 listen 保持一致）
const panelURL = "http://127.0.0.1:7865"

// 要结束的服务进程名
const serviceExe = "work2api.exe"

// 托盘提示文字
const trayTip = "Work2Api 反代服务"

// ---------------- 结构体 ----------------

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type msgT struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	ptX     int32
	ptY     int32
}

type pointT struct {
	x int32
	y int32
}

type notifyIconDataW struct {
	cbSize           uint32
	hWnd             uintptr
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            uintptr
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uTimeout         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         [16]byte
	hBalloonIcon     uintptr
}

type processEntry32W struct {
	dwSize              uint32
	cntUsage            uint32
	th32ProcessID       uint32
	th32DefaultHeapID   uintptr
	th32ModuleID        uint32
	cntThreads          uint32
	th32ParentProcessID uint32
	pcPriClassBase      int32
	dwFlags             uint32
	szExeFile           [maxPath]uint16
}

type bitmapInfoHeader struct {
	biSize          uint32
	biWidth         int32
	biHeight        int32
	biPlanes        uint16
	biBitCount      uint16
	biCompression   uint32
	biSizeImage     uint32
	biXPelsPerMeter int32
	biYPelsPerMeter int32
	biClrUsed       uint32
	biClrImportant  uint32
}

type iconInfo struct {
	fIcon    int32
	xHotspot uint32
	yHotspot uint32
	hbmMask  uintptr
	hbmColor uintptr
}

// ---------------- 入口 ----------------

func main() {
	// 消息循环必须固定在同一个 OS 线程上，否则 GetMessage 会立刻返回 -1。
	runtime.LockOSThread()

	initLog()
	logf("托盘启动，exe=%s", exePath())

	// 单实例：双击两次不应该出现两个托盘图标。
	mutexName := utf16p("Work2ApiTray.SingleInstance")
	hMutex, _, err := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(mutexName)))
	if hMutex == 0 {
		logf("创建互斥体失败: %v", err)
		fatal("创建互斥体失败：" + err.Error())
		return
	}
	if errno, ok := err.(syscall.Errno); ok && uint32(errno) == errAlreadyExists {
		logf("已有托盘实例在运行，退出")
		return
	}

	// 托盘是唯一入口：起来就把服务带起来（已在跑则原样不动）。
	// 放在建窗口之前 —— 服务先通，用户点「打开页面」时页面才是活的。
	ensureService()

	hInst, _, _ := procGetModuleHandleW.Call(0)

	hIcon := makeTrayIcon()
	if hIcon == 0 {
		// 自绘失败就退回系统默认图标，宁可难看也不要没图标。
		logf("自绘图标失败，回退系统默认图标")
		hIcon, _, _ = procLoadIconW.Call(0, idiApplication)
	}

	className := utf16p("work2api-tray-wndclass")
	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		lpfnWndProc:   syscall.NewCallback(wndProc),
		hInstance:     hInst,
		hIcon:         hIcon,
		lpszClassName: className,
	}
	if ret, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); ret == 0 {
		logf("注册窗口类失败: %v", err)
		fatal("注册窗口类失败：" + err.Error())
		return
	}

	// 消息专用窗口：不可见、无样式，只用来收托盘回调。
	hwnd, _, err := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(utf16p(trayTip))),
		0, 0, 0, 0, 0,
		0, 0, hInst, 0,
	)
	if hwnd == 0 {
		logf("创建消息窗口失败: %v", err)
		fatal("创建消息窗口失败：" + err.Error())
		return
	}
	gHWND = hwnd

	if !addTrayIcon(hwnd, hIcon) {
		logf("Shell_NotifyIcon(NIM_ADD) 返回 0")
		fatal("添加托盘图标失败：Shell_NotifyIcon 返回 0")
		return
	}
	logf("托盘图标已挂载（hwnd=%d）", hwnd)

	var m msgT
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}

	logf("消息循环结束，退出")
	procCloseHandle.Call(hMutex)
}

// gHWND 供窗口过程回写（PostMessage 等）使用。
var gHWND uintptr

// ---------------- 窗口过程 ----------------

func wndProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case wmTrayIcon:
		// lParam 低字是鼠标消息
		switch uint32(lParam) & 0xffff {
		case wmRButtonUp, wmLButtonUp:
			showTrayMenu(hwnd)
		}
		return 0
	case wmDestroy:
		deleteTrayIcon(hwnd)
		procPostQuitMessage.Call(0)
		return 0
	}
	ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
	return ret
}

// ---------------- 托盘菜单 ----------------

func showTrayMenu(hwnd uintptr) {
	hMenu, _, _ := procCreatePopupMenu.Call()
	if hMenu == 0 {
		return
	}
	defer procDestroyMenu.Call(hMenu)

	procAppendMenuW.Call(hMenu, mfString, cmdOpenPage,
		uintptr(unsafe.Pointer(utf16p("打开页面"))))
	procAppendMenuW.Call(hMenu, mfSeparator, 0, 0)

	// 按服务的当前状态置灰 —— 托盘是唯一入口，菜单上就该直接看出
	// 「现在能不能起、能不能停」，而不是点完再弹个「本来就没启动」。
	running := serviceRunning()
	procAppendMenuW.Call(hMenu, menuFlags(mfString, !running), cmdStartService,
		uintptr(unsafe.Pointer(utf16p("启动服务"))))
	procAppendMenuW.Call(hMenu, menuFlags(mfString, running), cmdStopService,
		uintptr(unsafe.Pointer(utf16p("关闭服务"))))

	var pt pointT
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))

	// 不设前台窗口的话，点菜单外面菜单不会消失（MSDN 明确要求）。
	procSetForegroundWindow.Call(hwnd)

	cmd, _, _ := procTrackPopupMenu.Call(
		hMenu,
		tpmRightButton|tpmReturnCmd|tpmNoNotify,
		uintptr(pt.x), uintptr(pt.y),
		0, hwnd, 0,
	)

	// 菜单关闭后补一条消息，否则下一次弹出会卡住（MSDN 同款要求）。
	procPostMessageW.Call(hwnd, wmNull, 0, 0)

	switch cmd {
	case cmdOpenPage:
		openPanel()
	case cmdStartService:
		startServiceMenu()
	case cmdStopService:
		stopService()
	}
}

// openPanel 用系统默认浏览器打开控制台面板。
func openPanel() {
	logf("菜单：打开页面 → %s", panelURL)
	verb := utf16p("open")
	url := utf16p(panelURL)
	ret, _, _ := procShellExecuteW.Call(
		0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(url)),
		0, 0,
		swShownormal,
	)
	// ShellExecute 返回值 <= 32 表示失败。
	if ret <= 32 {
		logf("打开页面失败，ShellExecute 返回 %d", ret)
		fatal(fmt.Sprintf("打开页面失败（ShellExecute 返回 %d）：\n%s", ret, panelURL))
		return
	}
	logf("打开页面成功")
}

// stopService 结束所有 work2api.exe 进程。
func stopService() {
	logf("菜单：关闭服务")
	killed, err := killProcesses(serviceExe)
	if err != nil {
		logf("枚举进程失败: %v", err)
		fatal("关闭服务失败：" + err.Error())
		return
	}
	logf("关闭服务：结束了 %d 个 %s 进程", killed, serviceExe)
	if killed == 0 {
		fatal("没有找到正在运行的 " + serviceExe + "\n（服务可能本来就没启动）。")
	}
}

// killProcesses 结束所有名为 name 的进程，返回实际结束的数量。
// 名字是参数而不是写死 —— 测试要用假进程名验证，绝不能误伤真服务。
func killProcesses(name string) (int, error) {
	pids, err := findProcesses(name)
	if err != nil {
		return 0, err
	}
	killed := 0
	for _, pid := range pids {
		if terminateProcess(pid) {
			killed++
		}
	}
	return killed, nil
}

// findProcesses 返回所有可执行文件名等于 name 的进程 PID（大小写不敏感）。
func findProcesses(name string) ([]uint32, error) {
	snap, _, err := procCreateToolhelp32Snapshot.Call(th32csSnapProcess, 0)
	if snap == 0 || snap == ^uintptr(0) {
		return nil, fmt.Errorf("CreateToolhelp32Snapshot 失败: %w", err)
	}
	defer procCloseHandle.Call(snap)

	var pe processEntry32W
	pe.dwSize = uint32(unsafe.Sizeof(pe))

	var pids []uint32
	ok, _, _ := procProcess32FirstW.Call(snap, uintptr(unsafe.Pointer(&pe)))
	for ok != 0 {
		if strings.EqualFold(syscall.UTF16ToString(pe.szExeFile[:]), name) {
			pids = append(pids, pe.th32ProcessID)
		}
		ok, _, _ = procProcess32NextW.Call(snap, uintptr(unsafe.Pointer(&pe)))
	}
	return pids, nil
}

// terminateProcess 结束单个 PID，成功返回 true。
func terminateProcess(pid uint32) bool {
	h, _, _ := procOpenProcess.Call(processTerminate, 0, uintptr(pid))
	if h == 0 {
		return false
	}
	defer procCloseHandle.Call(h)
	ret, _, _ := procTerminateProcess.Call(h, 1)
	return ret != 0
}

// ---------------- 托盘图标 ----------------

func addTrayIcon(hwnd, hIcon uintptr) bool {
	var nid notifyIconDataW
	nid.cbSize = uint32(unsafe.Sizeof(nid))
	nid.hWnd = hwnd
	nid.uID = 1
	nid.uFlags = nifMessage | nifIcon | nifTip
	nid.uCallbackMessage = wmTrayIcon
	nid.hIcon = hIcon
	fillW(nid.szTip[:], trayTip)

	ret, _, _ := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
	return ret != 0
}

func deleteTrayIcon(hwnd uintptr) {
	var nid notifyIconDataW
	nid.cbSize = uint32(unsafe.Sizeof(nid))
	nid.hWnd = hwnd
	nid.uID = 1
	procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
}

// makeTrayIcon 把 renderLogo 画好的图逐像素搬进 32bpp DIB，省掉外部 .ico 文件。
// 失败返回 0，调用方退回系统图标。
func makeTrayIcon() uintptr {
	const size = 32
	img := renderLogo(size)

	hdc, _, _ := procCreateCompatibleDC.Call(0)
	if hdc == 0 {
		return 0
	}
	defer procDeleteDC.Call(hdc)

	bi := bitmapInfoHeader{
		biSize:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		biWidth:       size,
		biHeight:      -size, // 负值 = 自上而下，省得算行序
		biPlanes:      1,
		biBitCount:    32,
		biCompression: biRGB,
	}
	var bits unsafe.Pointer
	hbmColor, _, _ := procCreateDIBSection.Call(
		hdc,
		uintptr(unsafe.Pointer(&bi)),
		dibRGBColors,
		uintptr(unsafe.Pointer(&bits)),
		0, 0,
	)
	if hbmColor == 0 || bits == nil {
		return 0
	}

	// DIB 是 BGRA，image.NRGBA 是 RGBA，逐像素换序。
	// 两边都是非预乘 alpha，所以 alpha 直接搬。
	px := unsafe.Slice((*byte)(bits), size*size*4)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			c := img.NRGBAAt(x, y)
			i := (y*size + x) * 4
			px[i+0] = c.B
			px[i+1] = c.G
			px[i+2] = c.R
			px[i+3] = c.A
		}
	}

	// 1bpp AND mask：全 0 = 不透明区域交给 color 位图的 alpha 决定。
	maskBits := make([]byte, size*4)
	hbmMask, _, _ := procCreateBitmap.Call(
		size, size, 1, 1,
		uintptr(unsafe.Pointer(&maskBits[0])),
	)
	if hbmMask == 0 {
		return 0
	}
	runtime.KeepAlive(maskBits)

	ii := iconInfo{
		fIcon:    1,
		hbmMask:  hbmMask,
		hbmColor: hbmColor,
	}
	hIcon, _, _ := procCreateIconIndirect.Call(uintptr(unsafe.Pointer(&ii)))
	return hIcon
}

// ---------------- 日志 ----------------

var logFile *os.File

func exePath() string {
	p, err := os.Executable()
	if err != nil {
		return "?"
	}
	return p
}

func initLog() {
	p, err := os.Executable()
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(filepath.Dir(p), "tray.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	logFile = f
}

func logf(format string, args ...any) {
	if logFile == nil {
		return
	}
	fmt.Fprintf(logFile, "%s %s\r\n",
		time.Now().Format("2006-01-02 15:04:05"),
		fmt.Sprintf(format, args...))
}

// ---------------- 小工具 ----------------

func utf16p(s string) *uint16 {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		panic(err)
	}
	return p
}

// fillW 把 Go 字符串写进定长 UTF-16 缓冲，并保证以 NUL 结尾。
func fillW(dst []uint16, s string) {
	u := syscall.StringToUTF16(s)
	n := copy(dst, u)
	if n < len(dst) {
		dst[n] = 0
		return
	}
	dst[len(dst)-1] = 0
}

// fatal 用消息框报错 —— 程序以 -H=windowsgui 构建，没有控制台可以看 panic。
func fatal(msg string) {
	procMessageBoxW.Call(
		0,
		uintptr(unsafe.Pointer(utf16p(msg))),
		uintptr(unsafe.Pointer(utf16p("Work2Api 托盘"))),
		mbIconError,
	)
}
