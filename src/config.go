// 读取并校验 config.json，填充默认值。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Config 服务运行配置。
type Config struct {
	Host             string            `json:"host"`
	Port             int               `json:"port"`
	APIKey           string            `json:"apiKey"`
	SessionTTLMs     int64             `json:"sessionTtlMs"`
	RequestTimeoutMs int64             `json:"requestTimeoutMs"`
	CursorEndpoint   string            `json:"cursorEndpoint"`
	CursorMode       string            `json:"cursorMode"`
	ClientVersion    string            `json:"clientVersion"`
	ModelMap         map[string]string `json:"modelMap"`
}

// LoadConfig 从文件加载配置并应用默认值。
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	// 未知字段硬拒绝：拼错字段名（如 "apikey"）会被静默忽略并回落到默认值——
	// apiKey 拼错意味着带着公开默认 key 运行却自以为已加固，必须启动即报。
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}

	if cfg.Host == "" {
		cfg.Host = "127.0.0.1" // 只绑回环；需要局域网/Docker 访问时显式设 "0.0.0.0"
	}
	if cfg.Port == 0 {
		cfg.Port = 3010
	}
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	if cfg.APIKey == "" {
		cfg.APIKey = "sk-cursor2api"
	}
	if cfg.SessionTTLMs == 0 {
		cfg.SessionTTLMs = 3_600_000
	}
	if cfg.RequestTimeoutMs == 0 {
		cfg.RequestTimeoutMs = 300_000
	}
	if cfg.CursorEndpoint == "" {
		cfg.CursorEndpoint = "https://agentn.global.api5.cursor.sh"
	}
	if cfg.ClientVersion == "" {
		cfg.ClientVersion = "cli-2026.07.23-e383d2b"
	}
	if cfg.CursorMode == "" {
		cfg.CursorMode = "agent" // agent=完整工具执行 ask=只读
	}
	if cfg.CursorMode != "agent" && cfg.CursorMode != "ask" {
		return Config{}, fmt.Errorf("invalid cursorMode %q: want \"agent\" or \"ask\"", cfg.CursorMode)
	}
	if cfg.Port < 0 || cfg.Port > 65535 {
		return Config{}, fmt.Errorf("invalid port %d", cfg.Port)
	}
	// 上限校验：值 * time.Millisecond 会溢出 int64 回绕成负数，
	// 负值校验在乘法前完成，溢出后拦不住——requestTimeoutMs 手滑多打几个零
	// （如 99999999999999）会回绕成负 duration，所有请求立即超时且无报错线索。
	// 24h 上限对"长 agent 任务"已足够宽裕。
	const maxTimeoutMs = 24 * 3600 * 1000
	if cfg.RequestTimeoutMs < 0 || cfg.RequestTimeoutMs > maxTimeoutMs {
		return Config{}, fmt.Errorf("invalid requestTimeoutMs %d: must be 0..%d", cfg.RequestTimeoutMs, maxTimeoutMs)
	}
	// sessionTtlMs：0=默认 1h；负值=永不过期（会话只靠指纹匹配，不过期无清理压力）。
	// 正值同样受溢出上限约束（回绕成负会被误读成"永不过期"，语义静默翻转）。
	if cfg.SessionTTLMs > maxTimeoutMs {
		return Config{}, fmt.Errorf("invalid sessionTtlMs %d: must be <= %d (negative = never expire)", cfg.SessionTTLMs, maxTimeoutMs)
	}
	if cfg.ModelMap == nil {
		// 默认无映射：未知模型名直通给 Cursor（内部 ID 可直接使用），
		// 需要别名/回退时在 config.json 里配置，如 {"claude-sonnet-4-5": "claude-sonnet-5", "*": "default"}
		cfg.ModelMap = map[string]string{}
	}

	return cfg, nil
}
