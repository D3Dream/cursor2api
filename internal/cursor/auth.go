// Package cursor 直连 Cursor agent.v1.AgentService 的 Connect-RPC 客户端。
package cursor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// TokenProvider 获取访问令牌（带缓存与过期刷新）。
type TokenProvider struct {
	mu      sync.Mutex
	cached  string
	expires time.Time
}

// NewTokenProvider 创建令牌提供者。
func NewTokenProvider() *TokenProvider {
	return &TokenProvider{}
}

// Token 返回可用的访问令牌。
// 优先 CURSOR_API_KEY / CURSOR_ACCESS_TOKEN 环境变量，
// 否则读 macOS Keychain（account: cursor-user, service: cursor-access-token）。
func (p *TokenProvider) Token() (string, error) {
	if k := os.Getenv("CURSOR_API_KEY"); k != "" {
		return k, nil
	}
	if k := os.Getenv("CURSOR_ACCESS_TOKEN"); k != "" {
		return k, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cached != "" && time.Now().Before(p.expires) {
		return p.cached, nil
	}
	// security(1) 仅 macOS 存在：其他平台走 keychain 必然报
	// "executable file not found"，把排查方向误导到"没登录"——
	// 直接说明只能用环境变量。
	if runtime.GOOS != "darwin" {
		return "", fmt.Errorf("cursor token: keychain 仅 macOS 可用，请设置 CURSOR_API_KEY / CURSOR_ACCESS_TOKEN 环境变量")
	}
	// 超时兜底：macOS keychain ACL 弹窗会让 security 永远等用户点击，
	// 无超时会挂住 token mutex、饿死所有请求
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "security", "find-generic-password",
		"-a", "cursor-user", "-s", "cursor-access-token", "-w").Output()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("read cursor token from keychain: timed out after 10s (keychain unlock prompt?)")
	}
	if err != nil {
		// ExitError 带 stderr（"could not be found in keychain" 等），
		// 丢了它排查方向会从"没登录"误导到"命令不存在"
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("read cursor token from keychain: %w: %s (run `agent login` first, or set CURSOR_API_KEY)", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("read cursor token from keychain: %w (run `agent login` first, or set CURSOR_API_KEY)", err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", fmt.Errorf("empty cursor token in keychain")
	}
	p.cached = token
	p.expires = time.Now().Add(5 * time.Minute)
	return token, nil
}

// FromEnv 报告 token 是否来自环境变量。
// env token 不走缓存，Invalidate 对它无效：401 重试必然再 401，
// 错误信息需要指出这一点，否则排查方向被误导到"刷新失败"上。
func (p *TokenProvider) FromEnv() bool {
	return os.Getenv("CURSOR_API_KEY") != "" || os.Getenv("CURSOR_ACCESS_TOKEN") != ""
}

// Invalidate 丢弃缓存的令牌（401 后强制重读；对 env token 无效，见 FromEnv）。
func (p *TokenProvider) Invalidate() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cached = ""
	p.expires = time.Time{}
}
