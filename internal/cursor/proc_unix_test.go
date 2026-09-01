//go:build !windows

package cursor

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// 平台 shell 调用：Unix 必须走 sh -c（与 Windows 的 cmd /C 分支对应），
// 且构造的命令真实可执行——硬编码错误解释器会让 agent 模式所有 shell 工具
// 以 "executable file not found" 静默失败（错误进 stderr，模型只能瞎猜）。
func TestShellCommandUnix(t *testing.T) {
	cmd := shellCommand("echo hello")
	if cmd.Path != "sh" && !strings.HasSuffix(cmd.Path, "/sh") {
		t.Fatalf("shellCommand Path = %q, want sh", cmd.Path)
	}
	if len(cmd.Args) != 3 || cmd.Args[1] != "-c" || cmd.Args[2] != "echo hello" {
		t.Fatalf("shellCommand Args = %v, want [sh -c 'echo hello']", cmd.Args)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("shellCommand 执行失败: %v", err)
	}
	if strings.TrimSpace(string(out)) != "hello" {
		t.Fatalf("输出 = %q, want hello", out)
	}
}

// 孙进程整组回收：`sh -c 'sleep 300 &'` 的后台孙进程继承 stdout 管道，
// 只杀 sh 会让 Wait 永久阻塞（管道写端未关）——killTree + WaitDelay 必须兜底。
func TestKillTreeReapsGrandchildren(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 300 & wait")
	setProcGroup(cmd)
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	gpid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("getpgid: %v", err)
	}
	if gpid != cmd.Process.Pid {
		t.Fatalf("子进程未自立进程组: pgid=%d pid=%d", gpid, cmd.Process.Pid)
	}
	killTree(cmd)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		// Wait 返回：整组已杀，管道已关
	case <-time.After(10 * time.Second):
		t.Fatal("Wait 未在 killTree 后返回（管道被孙进程楔住）")
	}
	// 进程组应已不存在（sleep 300 孙进程也被杀）
	if err := syscall.Kill(-cmd.Process.Pid, 0); err == nil {
		t.Fatal("孙进程仍存活（进程组未整组回收）")
	}
}
