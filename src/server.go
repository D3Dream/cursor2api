// HTTP 路由与请求处理：健康检查、模型列表、OpenAI 对话、Anthropic 对话。
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"cursor2api/internal/cursor"
)

// maxRequestBody 请求体上限（Claude Code 大对话历史可达数 MB，32MB 足够宽裕）。
const maxRequestBody = 32 << 20

// Server API 服务主体。
type Server struct {
	cfg           Config
	tokens        *cursor.TokenProvider
	cursor        *cursor.Client
	conversations *ConversationStore
	liveRuns      *LiveRunStore
	liveMu        sync.Mutex
	models        *cursor.ModelCache
	responsesMu   sync.Mutex
	responses     map[string]responseSession
	startedAt     time.Time // 进程启动时间（/v1/models 的稳定 created）
}

// NewServer 创建服务实例。
func NewServer(cfg Config) *Server {
	tokens := cursor.NewTokenProvider()
	return &Server{
		cfg:    cfg,
		tokens: tokens,
		cursor: cursor.NewClient(
			globalRegistry,
			tokens,
			cfg.CursorEndpoint,
			cfg.ClientVersion,
		),
		conversations: NewConversationStore(time.Duration(cfg.SessionTTLMs) * time.Millisecond),
		liveRuns:      NewLiveRunStore(time.Duration(cfg.SessionTTLMs) * time.Millisecond),
		models:        cursor.NewModelCache(10 * time.Minute),
		responses:     make(map[string]responseSession),
		startedAt:     time.Now(),
	}
}

// Handler 返回带 CORS 的 HTTP 处理器。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("POST /v1/responses", s.handleResponses)
	mux.HandleFunc("POST /v1/messages", s.handleMessages)
	mux.HandleFunc("POST /v1/messages/count_tokens", s.handleCountTokens)
	return withRecover(withPrivateNetworkCORS(withCORS(mux)))
}

// withRecover panic 兜底：schema 缺字段等 panic 返回 JSON 500，
// 而不是让 net/http 默认恢复后给客户端一个无解释的 EOF。
func withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic serving %s %s: %v", r.Method, r.URL.Path, rec)
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("internal error: %v", rec), "api_error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// Warmup 启动时预热（检查 token 可用性 + 拉取模型列表）。
func (s *Server) Warmup(ctx context.Context) {
	if _, err := s.tokens.Token(); err != nil {
		log.Printf("cursor: token 暂不可用（/health 将报 no_token）: %v", err)
	}
	models := s.models.Get(ctx, s.cursor)
	if len(models) > 0 {
		log.Printf("cursor: %d usable models", len(models))
	}
}

func (s *Server) authorize(r *http.Request) bool {
	// 空配置 key 永不可达（LoadConfig 强制默认值）；纵深防御：
	// 绕过 LoadConfig 的构造（测试/嵌入）不能留下空 token 直接过认证的后门
	if s.cfg.APIKey == "" {
		return false
	}
	// 恒定时间比较：服务可绑非回环地址，避免时序侧信道逐字节探测 key
	keyEq := func(got string) bool {
		return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(s.cfg.APIKey)) == 1
	}
	// Anthropic 风格：x-api-key 头
	if key := r.Header.Get("x-api-key"); key != "" {
		return keyEq(key)
	}
	// OpenAI 风格：Authorization: Bearer
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return false
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return false
	}
	return keyEq(auth[len(prefix):])
}

// mapModel 按配置把请求模型名映射为 Cursor 模型 ID。
func (s *Server) mapModel(requested string) string {
	// auto 是 CLI 的别名，服务端只认 default（实测 auto 会被静默终止）
	if requested == "" || requested == "auto" {
		requested = "default"
	}
	if m, ok := s.cfg.ModelMap[requested]; ok {
		return m
	}
	// "*" 通配回退；未配置时未知模型直通（Cursor 侧模型名）
	if m, ok := s.cfg.ModelMap["*"]; ok {
		return m
	}
	return requested
}

// checkModelUsable 缓存非空时校验模型可用性，不可用则给出可用列表的错误信息。
// default/auto 是服务端别名，不参与校验；缓存为空（拉取失败）时放行。
// 缓存过期（Peek=nil）时触发后台刷新——否则聊天路径上预检过期后永久失效。
func (s *Server) checkModelUsable(model string) error {
	if model == "" || model == "default" || model == "auto" {
		return nil
	}
	avail := s.models.Peek()
	if avail == nil {
		// 过期/从未拉取：异步刷新（singleflight 合并，不会放大请求），本次放行
		go s.models.Get(context.Background(), s.cursor)
		return nil
	}
	if len(avail) == 0 || cursor.Usable(avail, model) {
		return nil
	}
	ids := make([]string, 0, len(avail))
	for _, m := range avail {
		ids = append(ids, m.ModelID)
	}
	return fmt.Errorf("model %q is not usable on this Cursor account right now; available: %s", model, strings.Join(ids, ", "))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	authStatus := "token_ok"
	if _, err := s.tokens.Token(); err != nil {
		authStatus = "no_token"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"auth":     authStatus,
		"endpoint": s.cfg.CursorEndpoint,
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(r) {
		writeError(w, http.StatusUnauthorized, "invalid api key", "invalid_request_error")
		return
	}

	// created 固定为进程启动时间：每请求刷新的 created 对把它当版本号的客户端是噪声
	created := s.startedAt.Unix()
	items := []modelItem{
		{ID: "auto", Object: "model", Created: created, OwnedBy: "cursor"},
		// default 是 mapModel 的实际落点别名（auto/空都会映射到它），不列出会让
		// 严格客户端认为列表与可请求集合不一致
		{ID: "default", Object: "model", Created: created, OwnedBy: "cursor"},
	}
	seen := map[string]bool{"auto": true, "default": true}
	add := func(id string) {
		// 上游列表可能自带 auto/default/重复别名，去重（严格客户端对重复 id 报错）
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		items = append(items, modelItem{ID: id, Object: "model", Created: created, OwnedBy: "cursor"})
	}
	for _, m := range s.models.Get(r.Context(), s.cursor) {
		add(m.ModelID)
		for _, a := range m.Aliases {
			add(a)
		}
	}
	writeJSON(w, http.StatusOK, modelsListResponse{Object: "list", Data: items})
}

// handleCountTokens 粗略的 token 估算（Claude Code 会调用）。
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(r) {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid api key")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var body struct {
		Messages []anthropicMessage `json:"messages"`
		System   any                `json:"system"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		var mbErr *http.MaxBytesError
		if errors.As(err, &mbErr) {
			writeAnthropicError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body too large")
			return
		}
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid json body")
		return
	}
	// 粗估：4 字符 ≈ 1 token
	chars := len(systemText(body.System))
	for _, m := range body.Messages {
		for _, b := range parseContent(m.Content) {
			// tool_result 的文本在 Content（any）里，漏算会让长会话估算差一个数量级
			chars += len(b.Text) + len(b.Input) + len(toolResultText(b.Content))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": chars/4 + 1})
}

func newCompletionID() string {
	return "chatcmpl-" + randHex12()
}

func randHex12() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// findOpenAIConversation 从最新到最旧扫描 assistant 消息，
// 返回第一个指纹命中的会话及其消息索引（tail 从 idx+1 起）。
// 客户端重试/改写最后一条响应时，更早的 assistant 仍能救回会话状态。
func findOpenAIConversation(messages []ChatMessage, store *ConversationStore, ns string) (*Conversation, int) {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "assistant" {
			continue
		}
		fp := hashOpenAIAssistant(messageText(messages[i].Content), incomingToolCalls(messages[i]))
		if conv := store.FindByRespHash(ns + ":" + fp); conv != nil {
			return conv, i
		}
	}
	return nil, -1
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(r) {
		writeError(w, http.StatusUnauthorized, "invalid api key", "invalid_request_error")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var req ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var mbErr *http.MaxBytesError
		if errors.As(err, &mbErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large", "invalid_request_error")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid json body", "invalid_request_error")
		return
	}

	opts, err := openAIToRunOptions(&req, s.mapModel(req.Model), s.cfg.CursorMode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	if err := s.checkModelUsable(opts.Model); err != nil {
		// OpenAI 语义：模型不可用是 404 + code=model_not_found（客户端按 code 做降级决策）
		code := "model_not_found"
		writeJSON(w, http.StatusNotFound, errorResponse{
			Error: openAIError{Message: err.Error(), Type: "invalid_request_error", Code: &code},
		})
		return
	}

	// 会话续接：命名空间 + assistant 响应（含工具调用）的指纹匹配。
	// 混入 OpenAI user 字段（客户端用户/会话标识），不同客户端同开场白不再互串
	nsSeed := firstOpenAIUserText(req.Messages)
	if req.User != "" {
		nsSeed = req.User + "|" + nsSeed
	}
	if sessionID := strings.TrimSpace(r.Header.Get("X-Agent-Session-ID")); sessionID != "" {
		nsSeed = sessionID + "|" + nsSeed
	}
	ns := hashText(nsSeed)
	var results []pendingResult
	conv, lastIdx := findOpenAIConversation(req.Messages, s.conversations, ns)
	if conv != nil {
		// 续接：tool 结果走重放应答，其余增量作为新 prompt，历史靠 checkpoint 重放
		var prompt string
		results, prompt = buildOpenAITail(req.Messages[lastIdx+1:], conv.PendingTools)
		opts.ConversationID = conv.ID
		opts.State = conv.Checkpoint
		opts.History = nil
		if len(results) > 0 {
			opts.Prompt = "" // 重放应答模式：不发新消息
			// 同批 tail 里的 user 文本不能丢：附加到最后一个工具结果里
			if prompt != "" {
				last := &results[len(results)-1]
				last.Text += "\n\n" + prompt
			}
		} else {
			opts.Prompt = prompt
			if opts.Prompt == "" {
				opts.Prompt = "(continue)"
			}
		}
	}
	ensureConversationID(&opts)

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.cfg.RequestTimeoutMs)*time.Millisecond)
	defer cancel()

	// 会话在飞互斥（同 Anthropic 路径）：重试与慢请求并发复用同一 checkpoint 会互相污染
	if conv != nil {
		release := s.conversations.LockConv(conv.ID, ctx)
		if release == nil {
			writeError(w, http.StatusConflict, "conversation is busy with a previous request", "api_error")
			return
		}
		defer release()
	}

	var run *cursor.Run
	var runCancel context.CancelFunc
	var live *liveRun
	liveStore := s.liveStore()
	if conv != nil {
		live = liveStore.Get(conv.ID)
		if live != nil {
			if err := live.respond(results); err != nil {
				writeError(w, http.StatusConflict, err.Error(), "api_error")
				return
			}
			run = live.currentRun()
			if run == nil {
				writeError(w, http.StatusConflict, "live Cursor run is no longer available", "api_error")
				return
			}
			// Results were submitted directly to the paused bidi stream.
			results = nil
		}
	}
	if run == nil {
		upstreamCtx, cancelRun := s.newUpstreamContext(r.Context())
		runCancel = cancelRun
		run, err = s.cursor.Run(upstreamCtx, opts)
		if err != nil {
			cancelRun()
			writeError(w, http.StatusBadGateway, err.Error(), "api_error")
			return
		}
	}

	var res turnResult
	var keep bool
	if req.Stream {
		res, keep = s.handleOpenAIStream(ctx, w, run, &req, opts, conv, results, ns)
	} else {
		res, keep = s.handleOpenAINonStream(ctx, w, run, &req, opts, conv, results, ns)
	}
	if keep {
		if live == nil {
			live = liveStore.Put(opts.ConversationID, run, runCancel, res.toolCalls)
		} else {
			live.updatePending(res.toolCalls)
		}
	} else {
		if live != nil {
			liveStore.Remove(opts.ConversationID, live)
		} else {
			if runCancel != nil {
				runCancel()
			}
			run.Close()
		}
	}
}

func (s *Server) handleOpenAINonStream(ctx context.Context, w http.ResponseWriter, run *cursor.Run, req *ChatCompletionRequest, opts cursor.RunOptions, conv *Conversation, results []pendingResult, ns string) (turnResult, bool) {
	res := s.runTurn(ctx, run, results, nil)
	if res.err == nil && res.text.Len() == 0 && len(res.toolCalls) == 0 {
		res.err = errEmptyTurn(opts.Model)
	}
	if res.err != nil {
		writeError(w, http.StatusBadGateway, res.err.Error(), "api_error")
		return res, false
	}
	var u *usage
	if res.usage.InputTokens > 0 || res.usage.OutputTokens > 0 {
		u = &usage{
			PromptTokens:     int(res.usage.InputTokens),
			CompletionTokens: int(res.usage.OutputTokens),
			TotalTokens:      int(res.usage.InputTokens + res.usage.OutputTokens),
		}
	} else {
		// 恒带 usage：部分严格客户端对缺失字段报解析错
		u = &usage{}
	}
	// 先写响应、成功后才存指纹（与 Anthropic 流式路径同策）：
	// 客户端没收到完整响应时入库的指纹是丢失的续接锚点
	if err := writeJSON(w, http.StatusOK, buildCompletion(newCompletionID(), req.Model, res.text.String(), res.toolCalls, u, time.Now().Unix())); err != nil {
		dlog("chat/completions: response write failed (%v), skip save", err)
		return res, false
	}
	s.saveOpenAIConversation(res, opts, conv, ns)
	return res, len(res.toolCalls) > 0
}

func (s *Server) handleOpenAIStream(ctx context.Context, w http.ResponseWriter, run *cursor.Run, req *ChatCompletionRequest, opts cursor.RunOptions, conv *Conversation, results []pendingResult, ns string) (turnResult, bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	created := time.Now().Unix() // 同一 completion 的全部 chunk 共享同一 created
	id := newCompletionID()

	// 所有写共用一把锁 + 写超时：保活 goroutine 与主流程并发写 w；
	// 僵尸连接阻塞写会楔住 handler（defer run.Close() 不执行，上游 run 泄漏）
	var wmu sync.Mutex
	writeRaw := func(line string) bool {
		wmu.Lock()
		defer wmu.Unlock()
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Second))
		if _, err := fmt.Fprint(w, line); err != nil {
			return false
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return true
	}
	writeJSONSSE := func(payload any) bool {
		data, err := json.Marshal(payload)
		if err != nil {
			return false
		}
		return writeRaw(fmt.Sprintf("data: %s\n\n", data))
	}

	// 保活：内置工具执行期间可能长时间无事件（上游停滞兜底 120s），
	// 中间代理（nginx 默认 60s）会掐断静默连接。SSE 注释行客户端忽略，最安全。
	stopPing := make(chan struct{})
	pingDone := make(chan struct{})
	defer func() {
		close(stopPing)
		<-pingDone // 等保活 goroutine 退出再返回（避免 handler 返回后写 ResponseWriter 的竞态）
	}()
	go func() {
		defer close(pingDone)
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopPing:
				return
			case <-t.C:
				writeRaw(": keep-alive\n\n")
			}
		}
	}()

	// OpenAI 流式规范：首 chunk 只带 role，严格客户端会校验首帧
	roleChunk := buildChunk(id, req.Model, "", false, created)
	roleChunk.Choices[0].Delta.Role = "assistant"
	_ = writeJSONSSE(roleChunk)
	res := s.runTurn(ctx, run, results, func(ev cursor.Event) bool {
		if ev.Kind == cursor.EventText {
			return writeJSONSSE(buildChunk(id, req.Model, ev.Text, false, created))
		}
		return true
	})
	if res.err == nil && res.text.Len() == 0 && len(res.toolCalls) == 0 {
		res.err = errEmptyTurn(opts.Model)
	}
	if res.err != nil {
		_ = writeJSONSSE(errorResponse{
			Error: openAIError{Message: res.err.Error(), Type: "api_error"},
		})
		// 错误帧后也要发 [DONE]：部分客户端（LangChain 系）靠它判定流结束，
		// 收不到会挂到自己超时
		writeRaw("data: [DONE]\n\n")
		return res, false
	}
	// 最终帧全部写成功才入库（对齐 Anthropic 流式路径）：客户端在最终帧前
	// 断连时存下的指纹，指向客户端历史里不存在的 assistant，续接锚点丢失
	okTail := true
	if len(res.toolCalls) > 0 {
		okTail = writeJSONSSE(buildToolCallsChunk(id, req.Model, res.toolCalls, created)) &&
			writeJSONSSE(buildFinishChunk(id, req.Model, "tool_calls", created))
	} else {
		okTail = writeJSONSSE(buildChunk(id, req.Model, "", true, created))
	}
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		okTail = writeJSONSSE(buildUsageChunk(id, req.Model, &usage{
			PromptTokens:     int(res.usage.InputTokens),
			CompletionTokens: int(res.usage.OutputTokens),
			TotalTokens:      int(res.usage.InputTokens + res.usage.OutputTokens),
		}, created)) && okTail
	}
	okTail = writeRaw("data: [DONE]\n\n") && okTail
	if okTail {
		s.saveOpenAIConversation(res, opts, conv, ns)
		return res, len(res.toolCalls) > 0
	} else {
		dlog("chat/completions: client gone before final frames, skip save")
	}
	return res, false
}

// saveOpenAIConversation 保存 OpenAI 会话（指纹 = 命名空间 + 响应文本/工具调用哈希）。
func (s *Server) saveOpenAIConversation(res turnResult, opts cursor.RunOptions, conv *Conversation, ns string) {
	ck := res.lastCk
	if ck == nil && conv != nil {
		ck = conv.Checkpoint
	}
	if opts.ConversationID == "" {
		dlog("saveOpenAIConversation: skip save, empty conversation id")
		return
	}
	if res.text.Len() == 0 && len(res.toolCalls) == 0 {
		// 空响应的指纹是定值，入库后多个空响应会话互相覆盖
		dlog("saveOpenAIConversation: skip save, empty turn")
		return
	}
	s.conversations.Save(&Conversation{
		ID:           opts.ConversationID,
		Checkpoint:   ck,
		LastRespHash: ns + ":" + hashOpenAIAssistant(res.text.String(), res.toolCalls),
		PendingTools: res.toolCalls,
	})
}

// hashText 简单文本指纹。
func hashText(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:32]
}

func newMessageID() string {
	return "msg_" + randHex12()
}

func newToolUseID() string {
	return "toolu_" + randHex12()
}
