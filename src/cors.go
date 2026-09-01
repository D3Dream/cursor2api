// CORS 跨域中间件，支持浏览器与本机 Private Network Access。
package main

import (
	"net/http"
	"net/url"
	"strings"
)

// isLoopbackOrigin 判断 Origin 是否指向本机（本机浏览器 UI 等）。
// 只有回环 Origin 才会被反射/PNA 放行：公网网页经 PNA 访问 loopback 时
// 拿不到 CORS 授权，浏览器会在预检阶段拦截——这是默认 API key 下
// 防"任意网页驱动本机 agent"的关键闸口（CLI/curl 等无 Origin 客户端不受影响）。
func isLoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1" ||
		strings.HasSuffix(host, ".localhost")
}

// withCORS 为所有路由添加跨域响应头（仅反射回环 Origin）。
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		// Vary 无条件：同一 URL 对"无 Origin / 回环 Origin / 公网 Origin"返回
		// 不同的 ACAO，共享缓存缺 Vary 会把一种响应回给另一种请求
		w.Header().Set("Vary", "Origin")
		if origin != "" && isLoopbackOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		} else if origin == "" {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		// 非回环 Origin：不下发 Allow-Origin，浏览器预检/读取将被拦截。

		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Expose-Headers", "X-Cursor-Session-Id, Content-Type")
		w.Header().Set("Access-Control-Max-Age", "86400")

		if requested := r.Header.Get("Access-Control-Request-Headers"); requested != "" {
			w.Header().Set("Access-Control-Allow-Headers", requested)
		} else {
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, X-Requested-With")
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// withPrivateNetworkCORS 支持 Chrome 从公网页面访问 localhost 的预检。
// 只对回环 Origin 放行——公网 Origin 的 PNA 预检拿不到授权头，浏览器拦截。
func withPrivateNetworkCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Access-Control-Request-Private-Network"), "true") &&
			isLoopbackOrigin(r.Header.Get("Origin")) {
			w.Header().Set("Access-Control-Allow-Private-Network", "true")
		}
		next.ServeHTTP(w, r)
	})
}
