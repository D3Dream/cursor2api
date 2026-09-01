package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigCursorMode(t *testing.T) {
	write := func(body string) string {
		p := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// 缺省 → agent
	cfg, err := LoadConfig(write(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CursorMode != "agent" {
		t.Fatalf("default cursorMode = %q, want agent", cfg.CursorMode)
	}
	// 缺省 host → 只绑回环
	if cfg.Host != "127.0.0.1" {
		t.Fatalf("default host = %q, want 127.0.0.1", cfg.Host)
	}

	// ask 合法
	cfg, err = LoadConfig(write(`{"cursorMode":"ask"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CursorMode != "ask" {
		t.Fatalf("cursorMode = %q, want ask", cfg.CursorMode)
	}

	// 非法值（如大小写拼错）必须报错而不是静默落到 agent
	if _, err = LoadConfig(write(`{"cursorMode":"Agent"}`)); err == nil {
		t.Fatal("invalid cursorMode should error")
	}

	// 负数超时/端口必须报错（负数会让请求立即超时、端口直接 listen 失败）
	if _, err = LoadConfig(write(`{"requestTimeoutMs":-1}`)); err == nil {
		t.Fatal("negative requestTimeoutMs should error")
	}
	// sessionTtlMs 负值 = 永不过期（合法语义，不是错误）
	cfg, err = LoadConfig(write(`{"sessionTtlMs":-1}`))
	if err != nil {
		t.Fatalf("negative sessionTtlMs should be allowed (never expire): %v", err)
	}
	if cfg.SessionTTLMs != -1 {
		t.Fatalf("sessionTtlMs = %d, want -1", cfg.SessionTTLMs)
	}
	if _, err = LoadConfig(write(`{"port":99999}`)); err == nil {
		t.Fatal("out-of-range port should error")
	}
}

// 极大超时值会在 * time.Millisecond 时溢出 int64 回绕成负数：
// 负值校验在乘法前完成，溢出拦不住——requestTimeoutMs 手滑多打几个零，
// 所有请求会立即超时且无任何报错线索；sessionTtlMs 回绕成负会被
// 误读成"永不过期"（语义静默翻转）。必须在乘法前加上限。
func TestLoadConfigTimeoutOverflow(t *testing.T) {
	write := func(body string) string {
		p := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// 1e14 ms * 1e6 = 1e20 > int64 max (≈9.2e18)，乘法必溢出
	if _, err := LoadConfig(write(`{"requestTimeoutMs":99999999999999}`)); err == nil {
		t.Fatal("overflowing requestTimeoutMs should error (would wrap to negative duration)")
	}
	if _, err := LoadConfig(write(`{"sessionTtlMs":99999999999999}`)); err == nil {
		t.Fatal("overflowing sessionTtlMs should error (would wrap to negative = never-expire)")
	}
	// 边界内正常通过：24h 超时 + 永不过期 TTL
	cfg, err := LoadConfig(write(`{"requestTimeoutMs":86400000,"sessionTtlMs":-1}`))
	if err != nil {
		t.Fatalf("boundary values should parse: %v", err)
	}
	if cfg.RequestTimeoutMs != 86400000 {
		t.Fatalf("requestTimeoutMs = %d", cfg.RequestTimeoutMs)
	}
}

// 未知字段必须硬拒绝：apiKey 拼错（如 "apikey"）静默忽略意味着
// 带着公开默认 key 运行却自以为已加固。
func TestLoadConfigUnknownField(t *testing.T) {
	write := func(body string) string {
		p := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, err := LoadConfig(write(`{"api_key":"my-secret"}`)); err == nil {
		t.Fatal("unknown field api_key should error (likely apiKey typo)")
	}
	if _, err := LoadConfig(write(`{"apiKey":"my-secret"}`)); err != nil {
		t.Fatalf("valid apiKey should parse: %v", err)
	}
	// encoding/json 大小写不敏感："apikey" 能匹配到 apiKey（合法，不算 typo 漏网）
	cfg, err := LoadConfig(write(`{"apikey":"my-secret"}`))
	if err != nil {
		t.Fatalf("case-insensitive match should parse: %v", err)
	}
	if cfg.APIKey != "my-secret" {
		t.Fatalf("APIKey = %q, want my-secret", cfg.APIKey)
	}
}

// APIKey 首尾空白必须 trim：配置文件里的 "sk-x " 会让合法请求永远 401。
func TestLoadConfigAPIKeyTrim(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"apiKey":"  sk-real-key  "}`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey != "sk-real-key" {
		t.Fatalf("APIKey = %q, want trimmed sk-real-key", cfg.APIKey)
	}
}
