//go:build !windows

package cursor

import (
	"os/exec"
	"syscall"
)

// setProcGroup 让子进程自立进程组，使 killTree 能整组强杀（含后台孙进程）。
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// shellCommand 构造平台 shell 调用（Unix: sh -c）。
func shellCommand(command string) *exec.Cmd {
	return exec.Command("sh", "-c", command)
}

// killTree 强杀整个进程组。退化为单进程 Kill（组可能已不存在）。
// 只杀 cmd.Process 杀不掉继承了 stdout/stderr 管道的后台孙进程，
// Wait 会一直等管道关闭，超时与 Close 回收双双失效。
func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
