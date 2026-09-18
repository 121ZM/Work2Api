//go:build windows

package main

import (
	"path/filepath"
	"testing"
	"unsafe"
)

// 只给测试用：确认日志句柄真的带上了可继承标志。
var procGetHandleInformation = kernel32.NewProc("GetHandleInformation")

const handleFlagInherit = 0x00000001

// 这条盯的是最阴的一个坑：CreateFileW 默认给**不可继承**的句柄，直接塞进
// startupInfoW.hStdOutput 的话调用照样成功、进程照样起来，但子进程拿到的是
// 无效 stdout —— 服务日志会静默消失，不报任何错。
func TestOpenInheritableProducesInheritableHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	h, err := openInheritable(path)
	if err != nil {
		t.Fatalf("openInheritable 出错: %v", err)
	}
	defer procCloseHandle.Call(h)

	var flags uint32
	if ret, _, e := procGetHandleInformation.Call(h, uintptr(unsafe.Pointer(&flags))); ret == 0 {
		t.Fatalf("GetHandleInformation 失败: %v", e)
	}
	if flags&handleFlagInherit == 0 {
		t.Fatal("句柄不可继承 —— 子进程会拿到无效的 stdout，服务日志会静默丢失")
	}
}

// 这两条不变量原来是靠 vbs 的 sh.CurrentDirectory 撑着的，搬进代码后必须钉住：
//
//  1. 服务 exe 和日志都在托盘自己所在的目录（dist）；
//  2. **工作目录是 dist 的上一级** —— 服务按相对路径读 config.json 和 auths/，
//     工作目录给错它照样能起来，只是读不到配置，症状是「渠道一个都没有」。
func TestServicePathsInvariants(t *testing.T) {
	svcExe, logPath, workDir := servicePaths()
	exeDir := filepath.Dir(exePath())

	if got := filepath.Dir(svcExe); got != exeDir {
		t.Errorf("服务 exe 应在托盘所在目录 %s，实际 %s", exeDir, got)
	}
	if got := filepath.Base(svcExe); got != serviceExe {
		t.Errorf("服务 exe 名应为 %s，实际 %s", serviceExe, got)
	}
	if got := filepath.Dir(logPath); got != exeDir {
		t.Errorf("日志应写在托盘所在目录 %s，实际 %s", exeDir, got)
	}
	if workDir != filepath.Dir(exeDir) {
		t.Fatalf("工作目录必须是 %s（dist 的上一级），实际 %s —— "+
			"服务会起来但读不到 config.json", filepath.Dir(exeDir), workDir)
	}
}
