// 从 Cursor 拉取可用模型列表（agent.v1.AgentService/GetUsableModels unary）。
package cursor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/http2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"
)

// UsableModel 一个可用模型。
type UsableModel struct {
	ModelID     string
	DisplayName string
	Aliases     []string
}

// modelsHTTPClient 复用的 HTTP/2 客户端（避免每次 fetch 新建 Transport、
// 空闲连接占 fd 直到对端超时）。
var modelsHTTPClient = &http.Client{Transport: &http2.Transport{}, Timeout: 30 * time.Second}

// FetchUsableModels 调用 GetUsableModels 拉取账号可用模型。
func (c *Client) FetchUsableModels(ctx context.Context) ([]UsableModel, error) {
	reqMsg, err := c.reg.New("agent.v1.GetUsableModelsRequest")
	if err != nil {
		return nil, err
	}
	payload, err := proto.Marshal(reqMsg)
	if err != nil {
		return nil, err
	}

	hc := modelsHTTPClient
	doReq := func() (*http.Response, error) {
		token, err := c.tokens.Token()
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, "POST",
			c.endpoint+"/agent.v1.AgentService/GetUsableModels", bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("authorization", "Bearer "+token)
		req.Header.Set("content-type", "application/proto")
		req.Header.Set("connect-protocol-version", "1")
		req.Header.Set("user-agent", "connect-es/1.6.1")
		req.Header.Set("x-cursor-client-type", "cli")
		req.Header.Set("x-cursor-client-version", c.version)
		return hc.Do(req)
	}

	resp, err := doReq()
	if err == nil && resp.StatusCode == http.StatusUnauthorized && !c.tokens.FromEnv() {
		// token 过期：作废缓存，重取 token 透明重试一次（env token 无缓存可作废，不重试）
		resp.Body.Close()
		c.tokens.Invalidate()
		resp, err = doReq()
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		if resp.StatusCode == http.StatusUnauthorized {
			c.tokens.Invalidate()
			if c.tokens.FromEnv() {
				return nil, fmt.Errorf("GetUsableModels: %s (env token invalid/expired, update CURSOR_API_KEY/CURSOR_ACCESS_TOKEN)", resp.Status)
			}
		}
		return nil, fmt.Errorf("GetUsableModels: %s: %s", resp.Status, body)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsBodyBytes))
	if err != nil {
		return nil, err
	}
	msg, err := c.reg.Unmarshal("agent.v1.GetUsableModelsResponse", body)
	if err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}

	// 空列表也必须是已初始化切片：nil 会让缓存快路径永远判 miss，每次调用都打上游
	out := make([]UsableModel, 0)
	fd := msg.Descriptor().Fields().ByName("models")
	if fd == nil {
		return out, nil
	}
	list := msg.Get(fd).List()
	for i := 0; i < list.Len(); i++ {
		item, ok := list.Get(i).Message().Interface().(*dynamicpb.Message)
		if !ok {
			continue
		}
		um := UsableModel{
			ModelID:     getStr(item, "model_id"),
			DisplayName: getStr(item, "display_name"),
		}
		if um.DisplayName == "" {
			um.DisplayName = um.ModelID
		}
		if aFd := item.Descriptor().Fields().ByName("aliases"); aFd != nil {
			al := item.Get(aFd).List()
			for j := 0; j < al.Len(); j++ {
				um.Aliases = append(um.Aliases, al.Get(j).String())
			}
		}
		if um.ModelID != "" {
			out = append(out, um)
		}
	}
	return out, nil
}

// ModelCache 模型列表缓存（TTL 过期 + singleflight 刷新 + 失败负缓存）。
type ModelCache struct {
	mu       sync.RWMutex
	models   []UsableModel
	fetched  time.Time
	failedAt time.Time // 上次拉取失败时间（负缓存：故障期不每请求都打上游）
	ttl      time.Duration
	inflight chan struct{} // 非空表示有 fetch 进行中（并发请求合并到同一次拉取）
}

// maxModelsBodyBytes GetUsableModels 响应体上限（正常列表 KB 级）。
const maxModelsBodyBytes = 4 << 20

// modelNegTTL 拉取失败后的负缓存窗口：窗口内直接返回旧值，不重新发 RPC。
const modelNegTTL = time.Minute

// NewModelCache 创建模型缓存。
func NewModelCache(ttl time.Duration) *ModelCache {
	return &ModelCache{ttl: ttl}
}

// Peek 返回当前缓存（不触发拉取，用于热路径只读检查）。
// 过期视为无缓存（返回 nil）：可用性校验会放行，由下一次 Get 刷新——
// 不能用可能过期的列表拒绝已恢复可用的模型。
func (c *ModelCache) Peek() []UsableModel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.models == nil || time.Since(c.fetched) >= c.ttl {
		return nil
	}
	return c.models
}

// Usable 判断模型是否在可用列表（含别名）。
func Usable(models []UsableModel, id string) bool {
	for _, m := range models {
		if m.ModelID == id {
			return true
		}
		for _, a := range m.Aliases {
			if a == id {
				return true
			}
		}
	}
	return false
}

// Get 获取模型列表（过期自动刷新；并发过期合并为一次拉取）。
func (c *ModelCache) Get(ctx context.Context, client *Client) []UsableModel {
	c.mu.RLock()
	if c.models != nil && time.Since(c.fetched) < c.ttl {
		m := c.models
		c.mu.RUnlock()
		return m
	}
	c.mu.RUnlock()

	c.mu.Lock()
	// 双重检查：等锁期间可能已被别的 goroutine 刷新
	if c.models != nil && time.Since(c.fetched) < c.ttl {
		m := c.models
		c.mu.Unlock()
		return m
	}
	// 负缓存：故障期内不重复打上游（每个 /v1/models 请求都阻塞 30s 会放大故障）
	if !c.failedAt.IsZero() && time.Since(c.failedAt) < modelNegTTL {
		m := c.models
		c.mu.Unlock()
		return m
	}
	if c.inflight != nil {
		// 已有 fetch 进行中：等它结束再读缓存，不重复发 RPC
		ch := c.inflight
		c.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
		}
		c.mu.RLock()
		defer c.mu.RUnlock()
		return c.models
	}
	ch := make(chan struct{})
	c.inflight = ch
	c.mu.Unlock()

	// 不用调用方 ctx：第一个触发者的请求取消会把所有等待者的 fetch 一起中止。
	// fetch 是共享资源，生命周期独立于任何单个请求。
	fetchCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	models, err := client.FetchUsableModels(fetchCtx)
	cancel()

	c.mu.Lock()
	if err == nil {
		c.models = models
		c.fetched = time.Now()
		c.failedAt = time.Time{}
	} else {
		c.failedAt = time.Now()
	}
	out := c.models // 拉取失败用旧缓存兜底
	c.inflight = nil
	close(ch)
	c.mu.Unlock()
	return out
}
