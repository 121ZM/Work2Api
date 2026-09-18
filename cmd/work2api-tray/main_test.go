//go:build windows

package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 测试用的假进程名 —— 绝不能是 work2api.exe，否则会误杀真服务。
const fakeExeName = "w2atray-killtest.exe"

// startFakeProcess 复制一个系统 exe 并改名启动，
// 用来验证「按进程名结束」这条路径真的作用在目标进程上。
func startFakeProcess(t *testing.T) {
	t.Helper()
	src := filepath.Join(os.Getenv("SystemRoot"), "System32", "ping.exe")
	if _, err := os.Stat(src); err != nil {
		t.Skipf("找不到可用于测试的系统进程 %s: %v", src, err)
	}
	dst := filepath.Join(t.TempDir(), fakeExeName)
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("复制测试进程失败: %v", err)
	}
	cmd := exec.Command(dst, "-n", "60", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动测试进程失败: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
}

func waitForProcess(t *testing.T, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pids, err := findProcesses(name); err == nil && len(pids) > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%v 内没等到进程 %s 出现", timeout, name)
}

func TestFindProcessesFindsFakeProcess(t *testing.T) {
	startFakeProcess(t)
	waitForProcess(t, fakeExeName, 5*time.Second)

	pids, err := findProcesses(fakeExeName)
	if err != nil {
		t.Fatalf("findProcesses 出错: %v", err)
	}
	if len(pids) != 1 {
		t.Fatalf("期望找到 1 个 %s，实际 %d 个", fakeExeName, len(pids))
	}
}

func TestKillProcessesTerminatesFakeProcess(t *testing.T) {
	startFakeProcess(t)
	waitForProcess(t, fakeExeName, 5*time.Second)

	killed, err := killProcesses(fakeExeName)
	if err != nil {
		t.Fatalf("killProcesses 出错: %v", err)
	}
	if killed != 1 {
		t.Fatalf("期望结束 1 个进程，实际 %d", killed)
	}

	// 再调一次必须是 0 —— 证明进程是真的没了，而不是第一次只是枚举没找到。
	time.Sleep(200 * time.Millisecond)
	again, err := killProcesses(fakeExeName)
	if err != nil {
		t.Fatalf("killProcesses 出错: %v", err)
	}
	if again != 0 {
		t.Fatalf("第二次调用仍报告结束了 %d 个进程", again)
	}
}

// 这条盯的是「不会误杀」：名字不匹配的进程必须原封不动。
func TestKillProcessesLeavesOtherNamesAlone(t *testing.T) {
	startFakeProcess(t)
	waitForProcess(t, fakeExeName, 5*time.Second)

	killed, err := killProcesses("definitely-not-running.exe")
	if err != nil {
		t.Fatalf("killProcesses 出错: %v", err)
	}
	if killed != 0 {
		t.Fatalf("不该结束任何进程，实际 %d", killed)
	}

	pids, err := findProcesses(fakeExeName)
	if err != nil {
		t.Fatalf("findProcesses 出错: %v", err)
	}
	if len(pids) != 1 {
		t.Fatalf("测试进程被误杀了：期望仍在运行，实际找到 %d 个", len(pids))
	}
}

func TestFillWNullTerminates(t *testing.T) {
	buf := make([]uint16, 8)
	for i := range buf {
		buf[i] = 0xFFFF
	}
	fillW(buf, "ab")
	if buf[0] != 'a' || buf[1] != 'b' || buf[2] != 0 {
		t.Fatalf("短串写入错误: %v", buf[:4])
	}

	// 超长串：截断后最后一位必须是 NUL，
	// 否则 Shell_NotifyIcon 会越过缓冲区读下去。
	long := strings.Repeat("x", 64)
	buf2 := make([]uint16, 8)
	fillW(buf2, long)
	if buf2[len(buf2)-1] != 0 {
		t.Fatalf("超长串未以 NUL 结尾: %v", buf2)
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
