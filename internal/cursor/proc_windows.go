//go:build windows

package cursor

import "os/exec"

// setProcGroup Windows 无 POSIX 进程组，占位（配合 WaitDelay 兜底回收）。
func setProcGroup(cmd *exec.Cmd) {}

// shellCommand Windows 下用 cmd /C（无 POSIX sh，硬编码 sh 会报 executable not found）。
func shellCommand(command string) *exec.Cmd {
	return exec.Command("cmd", "/C", command)
}

// killTree Windows 下退化单进程 Kill（taskkill /T 需额外调外部命令，保持简单）。
func killTree(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
