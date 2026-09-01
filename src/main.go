// 程序入口：加载配置、加载协议 schema、启动 HTTP 服务、处理优雅退出。
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"cursor2api/internal/schema"
)

// globalRegistry 协议描述符注册表（从提取的 FDS 加载）。
var globalRegistry *schema.Registry

// debug 诊断日志开关（CURSOR2API_DEBUG=1）。
var debug = os.Getenv("CURSOR2API_DEBUG") != ""

func dlog(format string, args ...any) {
	if debug {
		log.Printf("[api] "+format, args...)
	}
}

func main() {
	configPath := "config.json"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	schemaPath := os.Getenv("CURSOR_SCHEMA")
	if schemaPath == "" {
		schemaPath = "schema/cursor_fds.json"
	}
	globalRegistry, err = schema.Load(schemaPath)
	if err != nil {
		log.Fatalf("schema: %v", err)
	}
	// 关键字段自检：fdOf 缺字段时在请求中途 panic，提前到启动阶段暴露
	if err := globalRegistry.Validate(); err != nil {
		log.Fatalf("schema: %v", err)
	}

	srv := NewServer(cfg)
	// 异步预热：token 检查 + 模型拉取走外网（30s 超时），同步等会拖慢启动
	go srv.Warmup(context.Background())

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		// body 读取总时限：已认证客户端逐字节 drip 32MB body 会无限占用 goroutine。
		// 120s 对正常大历史上传足够宽裕（SSE 出站不受此限）。
		ReadTimeout: 120 * time.Second,
		// 不设 WriteTimeout：SSE 流式响应是长连接，写了会误杀正常流。
		//（出站侧的僵尸连接由 handler 内的 30s 写超时兜底）
		IdleTimeout: 120 * time.Second, // keep-alive 空闲连接回收
	}

	loopback := cfg.Host == "127.0.0.1" || cfg.Host == "localhost" || cfg.Host == "::1"
	if cfg.APIKey == "sk-cursor2api" {
		if !loopback {
			// 默认 key 是公开的，agent 模式下 API key 可驱动下游 Agent 的
			// shell/文件工具；绑定非回环仍会把高权限工具入口公开到网络，硬拒绝
			log.Fatalf("拒绝启动：默认 API key 不得绑定非回环地址 %s（请在 config.json 设置强 apiKey）", cfg.Host)
		}
		log.Print("warning: 使用默认 API key，建议在 config.json 里换一个")
	}
	if !loopback {
		log.Printf("warning: 绑定非回环地址 %s，服务将暴露到网络，请确认已设置强 API key", cfg.Host)
	}

	go func() {
		// 先 bind 再打日志：ListenAndServe 失败时"listening"字样是误导
		// （bind 冲突的重启循环里日志会反复打印 listening + 失败成对出现）
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			log.Fatalf("server: listen %s: %v", addr, err)
		}
		log.Printf("cursor2api listening on http://%s/v1", addr)
		log.Printf("health: http://%s/health", addr)
		if err := httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		// 长连接（SSE）10s 内排不完：如实记录，进程仍会退出
		log.Printf("server: graceful shutdown incomplete: %v", err)
	}
}
