package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsLoopbackOrigin(t *testing.T) {
	for _, o := range []string{
		"http://localhost:3000", "http://127.0.0.1:8080", "http://[::1]:9000",
		"https://localhost", "http://app.localhost:5173",
	} {
		if !isLoopbackOrigin(o) {
			t.Errorf("isLoopbackOrigin(%q) = false, want true", o)
		}
	}
	for _, o := range []string{
		"https://evil.com", "http://127.0.0.1.evil.com", "https://localhost.evil.com",
		"", "not-a-url", "http://169.254.169.254",
	} {
		if isLoopbackOrigin(o) {
			t.Errorf("isLoopbackOrigin(%q) = true, want false", o)
		}
	}
}

// 公网 Origin 不得拿到 CORS 反射/PNA 放行（默认 key 下浏览器路径 = RCE 缺口）；
// 回环 Origin（本机浏览器 UI）保持可用。
func TestCORSOriginPolicy(t *testing.T) {
	next := withPrivateNetworkCORS(withCORS(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }),
	))

	// 公网 Origin：预检不得反射 Origin、不得放行 PNA
	req := httptest.NewRequest("OPTIONS", "http://127.0.0.1:3010/v1/messages", nil)
	req.Header.Set("Origin", "https://evil.com")
	req.Header.Set("Access-Control-Request-Private-Network", "true")
	rec := httptest.NewRecorder()
	next.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got == "https://evil.com" {
		t.Fatal("公网 Origin 被反射，浏览器预检会通过")
	}
	if got := rec.Header().Get("Access-Control-Allow-Private-Network"); got == "true" {
		t.Fatal("公网 Origin 的 PNA 被放行")
	}
	// 任意分支都要带 Vary: Origin——同一 URL 对不同 Origin 返回不同 ACAO，
	// 缺 Vary 时共享缓存会把一种响应回给另一种请求
	if got := rec.Header().Get("Vary"); got != "Origin" {
		t.Fatalf("公网 Origin 分支缺 Vary: Origin, got %q", got)
	}

	// 无 Origin（CLI/curl）：ACAO=* 且同样带 Vary
	req = httptest.NewRequest("GET", "http://127.0.0.1:3010/health", nil)
	rec = httptest.NewRecorder()
	next.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("无 Origin 应 ACAO=*, got %q", got)
	}
	if got := rec.Header().Get("Vary"); got != "Origin" {
		t.Fatalf("无 Origin 分支缺 Vary: Origin, got %q", got)
	}

	// 回环 Origin：正常反射 + PNA
	req = httptest.NewRequest("OPTIONS", "http://127.0.0.1:3010/v1/messages", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Private-Network", "true")
	rec = httptest.NewRecorder()
	next.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Fatalf("回环 Origin 未反射: %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Private-Network"); got != "true" {
		t.Fatal("回环 Origin 的 PNA 未放行")
	}
}
