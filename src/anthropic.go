// Anthropic Messages API (/v1/messages) 翻译层。
// 把 Anthropic 请求映射到 cursor 直连协议，响应映射回 Anthropic 事件。
package main

import (
	"context"
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

// ---- 请求结构 ----

type anthropicRequest struct {
	Model     string             `json:"model"`
	System    any                `json:"system"`
	Messages  []anthropicMessage `json:"messages"`
	MaxTokens int                `json:"max_tokens"`
	Stream    bool               `json:"stream"`
	Tools     []anthropicTool    `json:"tools"`
	Metadata  *anthropicMetadata `json:"metadata"`
}

// anthropicMetadata Claude Code 会在 user_id 里带会话标识（…_session_xxx），
// 用于会话命名空间：不同终端会话以相同开场白启动时指纹不再碰撞。
type anthropicMetadata struct {
	UserID string `json:"user_id"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   any             `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// parseContent 把 string | []block 内容统一为 block 列表。
func parseContent(content any) []contentBlock {
	switch v := content.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []contentBlock{{Type: "text", Text: v}}
	case []any:
		var blocks []contentBlock
		for _, item := range v {
			b, err := json.Marshal(item)
			if err != nil {
				continue
			}
			var blk contentBlock
			if err := json.Unmarshal(b, &blk); err != nil {
				continue
			}
			blocks = append(blocks, blk)
		}
		return blocks
	}
	return nil
}

// systemText 提取 system 提示文本。
func systemText(system any) string {
	switch v := system.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

// toolResultText 提取 tool_result 的文本内容。
// 图片/文档块无法经 Cursor 协议传给模型，降级为占位文本——
// 静默丢弃会让模型以为用户什么都没发，按错误前提作答。
func toolResultText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					parts = append(parts, t)
					continue
				}
				if ty, _ := m["type"].(string); ty == "image" || ty == "document" {
					parts = append(parts, "["+ty+" omitted: not supported by upstream]")
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// ---- 会话续接 ----

// toolResultFor 在 tail blocks 中找挂起工具对应的结果。
type pendingResult struct {
	Tool  PendingTool
	Text  string
	IsErr bool
}

func anthropicNamespace(req *anthropicRequest) string {
	nsSeed := firstUserText(req.Messages)
	if req.Metadata != nil && req.Metadata.UserID != "" {
		nsSeed = req.Metadata.UserID + "|" + nsSeed
	}
	return hashText(nsSeed)
}

func anthropicRunOptions(req *anthropicRequest, model, cursorMode string) cursor.RunOptions {
	opts := cursor.RunOptions{Model: model, SystemPrompt: systemText(req.System)}
	if cursorMode == "ask" {
		opts.Mode = 2
	} else {
		opts.Mode = 1
	}
	for _, t := range req.Tools {
		opts.Tools = append(opts.Tools, cursor.ToolDef{
			Name:        "cc" + t.Name,
			Description: t.Description,
			InputSchema: string(t.InputSchema),
		})
	}
	return opts
}

func applyAnthropicColdHistory(req *anthropicRequest, opts *cursor.RunOptions) {
	if len(req.Messages) == 0 {
		if opts.Prompt == "" {
			opts.Prompt = "(continue)"
		}
		return
	}
	last := len(req.Messages) - 1
	for i, m := range req.Messages {
		blocks := parseContent(m.Content)
		isLastUser := i == last && m.Role == "user"
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if isLastUser {
					opts.Prompt += b.Text
				} else if m.Role == "user" {
					opts.History = append(opts.History, cursor.HistoryMessage{Role: "user", Text: b.Text})
				} else {
					opts.History = append(opts.History, cursor.HistoryMessage{Role: "assistant", Text: b.Text})
				}
			case "tool_use":
				opts.History = append(opts.History, cursor.HistoryMessage{
					Role: "assistant", ToolCallID: b.ID, ToolName: b.Name, ArgsJSON: string(b.Input),
				})
			case "tool_result":
				opts.History = append(opts.History, cursor.HistoryMessage{
					Role: "tool", ToolCallID: b.ToolUseID, Text: toolResultText(b.Content), IsError: b.IsError,
				})
			case "image", "document":
				role := "user"
				if m.Role == "assistant" {
					role = "assistant"
				}
				opts.History = append(opts.History, cursor.HistoryMessage{
					Role: role, Text: "[" + b.Type + " omitted: not supported by upstream]",
				})
			}
		}
	}
	if len(opts.History) > 0 {
		opts.Prompt = embedHistoryAsText(opts.History) + opts.Prompt
		opts.History = nil
	}
	if opts.Prompt == "" {
		opts.Prompt = "(continue)"
	}
	if last := req.Messages[len(req.Messages)-1]; last.Role == "assistant" {
		opts.Prompt = "(Continue exactly from where your last message left off. Do not repeat it.)\n" + opts.Prompt
	}
}

func (s *Server) coldAnthropicRunOptions(req *anthropicRequest, model string) cursor.RunOptions {
	opts := anthropicRunOptions(req, model, s.cfg.CursorMode)
	applyAnthropicColdHistory(req, &opts)
	return opts
}

func (s *Server) planRun(req *anthropicRequest, model string) (cursor.RunOptions, []pendingResult, *Conversation, string) {
	opts := anthropicRunOptions(req, model, s.cfg.CursorMode)
	// 命名空间：首条 user 消息的指纹，隔离不同会话里相同的 assistant 响应；
	// 混入 metadata.user_id（Claude Code 的 session 标识），并行会话同开场白不再互串
	ns := anthropicNamespace(req)

	if len(req.Messages) == 0 {
		return opts, nil, nil, ns
	}

	// 从最新到最旧扫描 assistant 消息，命中第一个指纹匹配的会话。
	// 客户端重试/改写最后一条响应时，更早的 assistant 仍能救回会话状态。
	var conv *Conversation
	lastIdx := -1
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != "assistant" {
			continue
		}
		blocks := parseContent(req.Messages[i].Content)
		// Tool IDs are exact protocol identities. Prefer them over the full
		// assistant fingerprint, whose text can be normalized by streaming
		// clients and otherwise make us fall back to an older pending tool.
		for _, b := range blocks {
			if b.Type == "tool_use" && b.ID != "" {
				if c := s.conversations.FindByPendingToolID(ns, b.ID); c != nil {
					conv = c
					lastIdx = i
					break
				}
			}
		}
		if conv != nil {
			break
		}
		if c := s.conversations.FindByRespHash(ns + ":" + hashBlocks(blocks)); c != nil {
			conv = c
			lastIdx = i
			break
		}
	}

	if conv == nil {
		applyAnthropicColdHistory(req, &opts)
		return opts, nil, nil, ns
	}

	// 续接：复用 conversation id + checkpoint
	opts.ConversationID = conv.ID
	opts.State = conv.Checkpoint

	tail := tailUserBlocks(req.Messages, lastIdx)
	var results []pendingResult
	var textParts []string
	for _, b := range tail {
		switch b.Type {
		case "tool_result":
			// 匹配挂起的工具（按 tool_use_id 匹配；不匹配的作为文本附带）
			matched := false
			for _, pt := range conv.PendingTools {
				if pt.ToolUseID == b.ToolUseID {
					results = append(results, pendingResult{Tool: pt, Text: toolResultText(b.Content), IsErr: b.IsError})
					matched = true
					break
				}
			}
			if !matched {
				if debug {
					pendingIDs := make([]string, 0, len(conv.PendingTools))
					for _, pt := range conv.PendingTools {
						pendingIDs = append(pendingIDs, pt.ToolUseID+":"+pt.Name)
					}
					dlog("planRun: unmatched tool_result id=%q pending=%v", b.ToolUseID, pendingIDs)
				}
				// 找不到对应挂起调用：作为文本附带（模型可见）
				textParts = append(textParts, fmt.Sprintf("[tool result %s]: %s", b.ToolUseID, toolResultText(b.Content)))
			}
		case "text":
			textParts = append(textParts, b.Text)
		case "image", "document":
			textParts = append(textParts, "["+b.Type+" omitted: not supported by upstream]")
		}
	}

	if len(results) > 0 {
		// 有工具结果要提交：不发新消息，等服务端重放挂起调用后逐个应答。
		// 同批 tail 里的 user 文本不能丢（客户端常在 tool_result 后追加新指令）：
		// 附加到最后一个工具结果里，模型读到结果时可见。
		if len(textParts) > 0 {
			last := &results[len(results)-1]
			last.Text += "\n\n" + strings.Join(textParts, "\n")
		}
		return opts, results, conv, ns
	}

	opts.Prompt = strings.Join(textParts, "\n")
	if opts.Prompt == "" {
		opts.Prompt = "(continue)"
	}
	return opts, nil, conv, ns
}

// ---- 响应 ----

type anthropicUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

type anthropicResponse struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Content      []contentBlock `json:"content"`
	Model        string         `json:"model"`
	StopReason   string         `json:"stop_reason"`
	StopSequence any            `json:"stop_sequence"`
	Usage        anthropicUsage `json:"usage"`
}

// replayAnthropicPending returns the same tool_use blocks while a downstream
// client is still waiting for permission and has not produced tool_result yet.
// The live Cursor stream remains paused and is resumed only by a later request
// carrying the results.
func replayAnthropicPending(w http.ResponseWriter, req *anthropicRequest, pending []PendingTool) {
	content := make([]contentBlock, 0, len(pending))
	for _, p := range pending {
		content = append(content, contentBlock{
			Type: "tool_use", ID: p.ToolUseID, Name: p.Name, Input: json.RawMessage(p.Input),
		})
	}
	if !req.Stream {
		_ = writeJSON(w, http.StatusOK, anthropicResponse{
			ID: newMessageID(), Type: "message", Role: "assistant", Content: content,
			Model: req.Model, StopReason: "tool_use", Usage: anthropicUsage{},
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)
	send := func(event string, payload any) bool {
		data, err := json.Marshal(payload)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	msgID := newMessageID()
	if !send("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": msgID, "type": "message", "role": "assistant", "content": []any{},
			"model": req.Model, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	}) {
		return
	}
	for i, p := range pending {
		if !send("content_block_start", map[string]any{
			"type": "content_block_start", "index": i,
			"content_block": map[string]any{
				"type": "tool_use", "id": p.ToolUseID, "name": p.Name, "input": map[string]any{},
			},
		}) || !send("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": i,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": p.Input},
		}) || !send("content_block_stop", map[string]any{"type": "content_block_stop", "index": i}) {
			return
		}
	}
	if !send("message_delta", map[string]any{
		"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use", "stop_sequence": nil},
		"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
	}) {
		return
	}
	send("message_stop", map[string]any{"type": "message_stop"})
}

// turnResult 一次 run 的收集结果。
type turnResult struct {
	text       strings.Builder
	toolCalls  []PendingTool
	blocks     []contentBlock // 保序的内容块（text/tool_use 交错），与客户端所见一致
	usage      anthropicUsage
	stopReason string
	lastCk     *cursor.Checkpoint
	err        error
}

// appendText 追加文本到保序块列（连续文本合并进同一 block）。
func (res *turnResult) appendText(t string) {
	res.text.WriteString(t)
	if n := len(res.blocks); n > 0 && res.blocks[n-1].Type == "text" {
		res.blocks[n-1].Text += t
		return
	}
	res.blocks = append(res.blocks, contentBlock{Type: "text", Text: t})
}

// appendToolCall 登记新工具调用（保序块 + 挂起列表），返回生成的 toolUseID。
func (res *turnResult) appendToolCall(ev cursor.Event) PendingTool {
	tc := PendingTool{
		ToolUseID: newToolUseID(), Name: ev.ToolName, Input: normalizeJSON(ev.ToolArgsJSON),
		ExecName: ev.ExecName, ExecID: ev.ExecID, ExecIDStr: ev.ExecIDStr,
	}
	res.toolCalls = append(res.toolCalls, tc)
	res.blocks = append(res.blocks, contentBlock{
		Type: "tool_use", ID: tc.ToolUseID, Name: tc.Name, Input: json.RawMessage(tc.Input),
	})
	res.stopReason = "tool_use"
	return tc
}

// stallTimeout 无任何服务端事件的最大等待（上游杀 run 不给错误帧时的兜底）。
const stallTimeout = 120 * time.Second

// emptyTurnError marks an upstream run that ended without any usable event.
// Besides making the client-facing error useful, the concrete type lets the
// handlers invalidate a possibly stale local checkpoint without deleting a
// conversation for unrelated request failures.
type emptyTurnError struct {
	model string
}

func (e emptyTurnError) Error() string {
	return fmt.Sprintf("cursor: empty response (model %q unavailable on this account, or run rejected upstream)", e.model)
}

func errEmptyTurn(model string) error {
	return emptyTurnError{model: model}
}

func isEmptyTurnError(err error) bool {
	var target emptyTurnError
	return errors.As(err, &target)
}

// errClientWrite 客户端连接写入失败（断连）。
// 必须置 err：否则上层会把半截响应当成功结果存入会话指纹，污染会话链。
var errClientWrite = errors.New("cursor: client write failed (disconnected)")

const toolCallCollectionWindow = 1500 * time.Millisecond

// matchReplay 判断服务端发来的工具调用是否为挂起调用的重放。
// 在 results[startIdx:] 里搜（剥 cc 前缀、忽略大小写）。
// 两级匹配：
//  1. 名字+参数双中 → 唯一确定（同名并行调用乱序重放时防止结果错配——
//     两个 Read 不同 path，只按名字会把 A 的结果交给 B）
//  2. 名字唯一命中 → 降级按名字匹配（服务端可能改写参数）
//     多个同名候选但参数都对不上 → 不猜，返回 -1 当新调用处理
//     （宁可让客户端重新执行，不可把结果张冠李戴）
func matchReplay(results []pendingResult, startIdx int, toolName, argsJSON string) int {
	name := strings.TrimPrefix(toolName, "cc")
	want := normalizeJSON(argsJSON)
	found := -1
	ambiguous := false
	for i := startIdx; i < len(results); i++ {
		if !strings.EqualFold(name, results[i].Tool.Name) {
			continue
		}
		if want != "" && results[i].Tool.Input != "" && results[i].Tool.Input == want {
			return i // 名字+参数双中
		}
		if found == -1 {
			found = i
		} else {
			ambiguous = true
		}
	}
	if ambiguous {
		return -1
	}
	return found
}

// runTurn 执行一次 run，向 streamFn 回调事件（nil 表示非流式收集）。
// results 非空时：服务端重放挂起调用 → 按名字匹配 RespondTool（乱序安全）。
func (s *Server) runTurn(ctx context.Context, run *cursor.Run, results []pendingResult, onEvent func(ev cursor.Event) bool) turnResult {
	var res turnResult
	res.stopReason = "end_turn"
	replayIdx := 0
	sawTurnEnd := false

	events := run.Events()
	var idleTimer *time.Timer
	var idleCh <-chan time.Time
	shortIdle := false // true=工具调用后的并行收集窗口；false=全局停滞兜底
	resetIdle := func(d time.Duration) {
		if idleTimer != nil {
			idleTimer.Stop()
		}
		idleTimer = time.NewTimer(d)
		idleCh = idleTimer.C
	}
	resetIdle(stallTimeout)

	// handleEvent 处理单个事件，返回 true 表示本轮结束。
	handleEvent := func(ev cursor.Event, ok bool) bool {
		if !ok {
			dlog("runTurn: events channel closed, text=%dB tools=%d", res.text.Len(), len(res.toolCalls))
			return true
		}
		switch ev.Kind {
		case cursor.EventText:
			shortIdle = false
			resetIdle(stallTimeout)
			res.appendText(ev.Text)
			if onEvent != nil && !onEvent(ev) {
				res.err = errClientWrite
				dlog("runTurn: client gone (onEvent false), text=%dB", res.text.Len())
				return true
			}
		case cursor.EventThinking:
			shortIdle = false
			resetIdle(stallTimeout)
			if onEvent != nil && !onEvent(ev) {
				res.err = errClientWrite
				dlog("runTurn: client gone (onEvent false, thinking)")
				return true
			}
		case cursor.EventToolCall:
			// 重放场景：服务端重新发起挂起调用 → 提交结果。
			// 乱序安全：名字+参数两级匹配（只查头部/只按名字会错配同名调用）
			if idx := matchReplay(results, replayIdx, ev.ToolName, ev.ToolArgsJSON); idx >= 0 {
				r0 := results[idx]
				// 已匹配的交换到 [0:replayIdx) 区间，保持未匹配区间紧凑
				results[replayIdx], results[idx] = results[idx], results[replayIdx]
				replayIdx++
				if r0.Tool.ExecName == "" || r0.Tool.ExecName == "mcp_args" {
					if err := run.RespondTool(ev.ExecID, ev.ExecIDStr, r0.Text, r0.IsErr); err != nil {
						res.err = fmt.Errorf("cursor: replay tool result: %w", err)
						return true
					}
				} else {
					if err := run.RespondExec(r0.Tool.ExecName, r0.Tool.Input, ev.ExecID, ev.ExecIDStr, r0.Text, r0.IsErr); err != nil {
						res.err = fmt.Errorf("cursor: replay tool result: %w", err)
						return true
					}
				}
				resetIdle(stallTimeout)
				return false
			}
			if replayIdx < len(results) {
				dlog("runTurn: no replay match for %s (%d pending), treat as new call", ev.ToolName, len(results)-replayIdx)
			}
			// 新工具调用：收集，等客户端执行（剥掉 cc 前缀）。
			// 事件副本回填生成的 toolu_ id/剥前缀名，流式层可即时发块（保序）
			if ev.ExecName == "mcp_args" {
				ev.ToolName = strings.TrimPrefix(ev.ToolName, "cc")
			}
			tc := res.appendToolCall(ev)
			ev.ToolCallID = tc.ToolUseID
			ev.ToolName = tc.Name
			ev.ToolArgsJSON = tc.Input
			if onEvent != nil && !onEvent(ev) {
				res.err = errClientWrite
				dlog("runTurn: client gone (onEvent false, toolcall)")
				return true
			}
			// 收集并行工具调用：短暂等待更多事件
			shortIdle = true
			resetIdle(toolCallCollectionWindow)
		case cursor.EventTurnEnd:
			shortIdle = false
			sawTurnEnd = true
			resetIdle(stallTimeout)
			res.usage.InputTokens = ev.Usage.InputTokens
			res.usage.OutputTokens = ev.Usage.OutputTokens
		case cursor.EventCheckpoint:
			res.lastCk = ev.Checkpoint
			// Cursor normally sends a checkpoint immediately after read_args and
			// other tool calls. Keep the short collection window active so the
			// tool_use response reaches the downstream agent instead of waiting
			// for the global 2-minute stall timeout.
			if shortIdle {
				resetIdle(toolCallCollectionWindow)
			} else {
				resetIdle(stallTimeout)
			}
		case cursor.EventError:
			res.err = ev.Err
			log.Printf("[anthropic] run error: %v", ev.Err)
			return true
		case cursor.EventDone:
			// 观测：正常流程上游先发 turn_ended 再关流；没见到 turn_ended 的
			// EventDone 可能是帧边界恰好对齐的截断（无法与干净结束区分），
			// 留日志便于排查"回答看起来被切断"类问题
			dlog("runTurn: EventDone, text=%dB tools=%d sawTurnEnd=%v", res.text.Len(), len(res.toolCalls), sawTurnEnd)
			return true
		}
		return false
	}

	for {
		select {
		case ev, ok := <-events:
			if handleEvent(ev, ok) {
				return res
			}
		case <-idleCh:
			if idleTimer != nil {
				idleTimer.Stop()
			}
			// select 在 idle 与 events 同时就绪时随机选择：先非阻塞排空缓冲事件，
			// 避免丢弃刚到达的并行工具调用
			select {
			case ev, ok := <-events:
				if handleEvent(ev, ok) {
					return res
				}
				continue
			default:
			}
			if shortIdle {
				// 工具调用后静默超时：认为本轮调用已收集完
				dlog("runTurn: idle timeout after toolcall, tools=%d", len(res.toolCalls))
				return res
			}
			// 全局停滞：上游杀了 run（安全围栏/模型不可用）但没发任何结束帧
			res.err = fmt.Errorf("cursor: run stalled, no server events for %s (upstream killed the run without an error frame)", stallTimeout)
			log.Printf("[anthropic] %v", res.err)
			return res
		case <-ctx.Done():
			// 客户端断连 / 请求超时：立即回收，避免僵尸 run 泄漏心跳
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				res.err = fmt.Errorf("cursor: request timed out after %dms", s.cfg.RequestTimeoutMs)
			} else {
				res.err = ctx.Err() // 客户端主动断连
			}
			dlog("runTurn: ctx done: %v", ctx.Err())
			return res
		}
	}
}

// handleMessages Anthropic /v1/messages 入口。
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(r) {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid api key")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var req anthropicRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var mbErr *http.MaxBytesError
		if errors.As(err, &mbErr) {
			writeAnthropicError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body too large")
			return
		}
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid json body")
		return
	}
	if len(req.Messages) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "messages is required")
		return
	}
	model := s.mapModel(req.Model)
	if err := s.checkModelUsable(model); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if sessionID := strings.TrimSpace(r.Header.Get("X-Agent-Session-ID")); sessionID != "" {
		// Optional caller-provided isolation for multiple downstream agents that
		// omit Anthropic metadata.user_id.
		if req.Metadata == nil {
			req.Metadata = &anthropicMetadata{}
		}
		req.Metadata.UserID = sessionID + "|" + req.Metadata.UserID
	}
	opts, results, conv, ns := s.planRun(&req, model)
	ensureConversationID(&opts)
	convInfo := "cold-start"
	if conv != nil {
		convInfo = "continue conv=" + shortConvID(conv.ID)
	}
	dlog("messages: model=%s stream=%v msgs=%d %s prompt=%dB history=%d pendingResults=%d",
		model, req.Stream, len(req.Messages), convInfo, len(opts.Prompt), len(opts.History), len(results))
	start := time.Now()

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.cfg.RequestTimeoutMs)*time.Millisecond)
	defer cancel()

	// 会话在飞互斥：客户端超时重试会与仍在运行的首个请求并发复用同一
	// conversation_id + checkpoint，并发推进服务端会话状态导致互相污染
	if conv != nil {
		release := s.conversations.LockConv(conv.ID, ctx)
		if release == nil {
			writeAnthropicError(w, http.StatusConflict, "api_error", "conversation is busy with a previous request")
			return
		}
		defer release()
	}

	var run *cursor.Run
	var runCancel context.CancelFunc
	var live *liveRun
	var err error
	liveStore := s.liveStore()
	rebuildFromFullHistory := func(reason string) {
		dlog("messages: live continuation failed (%v); rebuild from full client history", reason)
		if live != nil && conv != nil {
			liveStore.Remove(conv.ID, live)
		}
		if conv != nil {
			s.conversations.DeleteConversation(ns, conv.ID)
		}
		opts = s.coldAnthropicRunOptions(&req, model)
		ensureConversationID(&opts)
		conv, live, run, results = nil, nil, nil, nil
	}
	if conv != nil {
		live = liveStore.Get(conv.ID)
		if live != nil {
			if len(results) == 0 {
				pending := live.pendingTools()
				if len(pending) == 0 {
					rebuildFromFullHistory("live run has no pending tools")
				} else {
					dlog("messages: replay %d pending tools while awaiting permission/results", len(pending))
					replayAnthropicPending(w, &req, pending)
					return
				}
			}
			if live != nil {
				if err := live.respond(ctx, results); err != nil {
					rebuildFromFullHistory(err.Error())
				} else {
					run = live.currentRun()
					if run == nil {
						rebuildFromFullHistory("live Cursor run is no longer available")
					} else {
						// The live stream is already paused at these execs; they were just
						// answered directly. Do not let runTurn replay them a second time.
						results = nil
					}
				}
			}
		}
	}
	if run == nil {
		upstreamCtx, cancelRun := s.newUpstreamContext(r.Context())
		runCancel = cancelRun
		run, err = s.cursor.Run(upstreamCtx, opts)
		if err != nil {
			cancelRun()
			log.Printf("[anthropic] run start error: %v", err)
			writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
			return
		}
	}
	defer func() { dlog("messages: done in %s", time.Since(start).Round(time.Millisecond)) }()

	var res turnResult
	var keep bool
	if req.Stream {
		res, keep = s.streamAnthropic(ctx, w, run, &req, results, opts, conv, ns)
	} else {
		res, keep = s.collectAnthropic(ctx, w, run, &req, results, opts, conv, ns)
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
		// Any upstream failure while continuing a saved conversation can leave
		// its checkpoint unusable. Forget that checkpoint so the next client
		// retry with the same history starts cold. A fresh conversation has no
		// cached state to invalidate.
		if conv != nil && res.err != nil {
			s.conversations.DeleteConversation(ns, conv.ID)
		}
	}
}

// finishTurn 保存会话状态。
func (s *Server) finishTurn(res turnResult, opts cursor.RunOptions, conv *Conversation, content []contentBlock, ns string) {
	ck := res.lastCk
	if ck == nil && conv != nil {
		// 沿用旧 checkpoint 的前提：服务端按 conversation_id 累积会话状态，
		// checkpoint 是状态引用而非全量快照（e2e 实测成立）。
		// 若上游改成全量快照语义，这里会丢本轮 turn，需要重新验证。
		ck = conv.Checkpoint
	}
	convID := opts.ConversationID
	if convID == "" {
		// 空 ID 会话入库后，下一轮续接会拿到空 conv.ID，直接污染会话链
		dlog("finishTurn: skip save, empty conversation id")
		return
	}
	s.conversations.Save(&Conversation{
		ID:           convID,
		Checkpoint:   ck,
		LastRespHash: ns + ":" + hashBlocks(content),
		PendingTools: res.toolCalls,
	})
}

// collectAnthropic 非流式。
func (s *Server) collectAnthropic(ctx context.Context, w http.ResponseWriter, run *cursor.Run, req *anthropicRequest, results []pendingResult, opts cursor.RunOptions, conv *Conversation, ns string) (turnResult, bool) {
	res := s.runTurn(ctx, run, results, nil)
	if res.err == nil && res.text.Len() == 0 && len(res.toolCalls) == 0 {
		res.err = errEmptyTurn(opts.Model)
	}
	if res.err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", res.err.Error())
		return res, false
	}
	content := res.blocks
	if len(content) == 0 {
		content = []contentBlock{{Type: "text", Text: ""}}
	}

	// 先写响应、成功后才入库（与流式路径同策）：客户端没收到完整响应时
	// 存下的指纹指向客户端历史中不存在的 assistant，续接锚点丢失
	if err := writeJSON(w, http.StatusOK, anthropicResponse{
		ID:           newMessageID(),
		Type:         "message",
		Role:         "assistant",
		Content:      content,
		Model:        req.Model,
		StopReason:   res.stopReason,
		StopSequence: nil,
		Usage:        res.usage,
	}); err != nil {
		dlog("messages: response write failed (%v), skip save", err)
		return res, false
	}
	s.finishTurn(res, opts, conv, content, ns)
	return res, len(res.toolCalls) > 0
}

// streamAnthropic 流式 SSE。
func (s *Server) streamAnthropic(ctx context.Context, w http.ResponseWriter, run *cursor.Run, req *anthropicRequest, results []pendingResult, opts cursor.RunOptions, conv *Conversation, ns string) (turnResult, bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)

	// 所有 send 共用一把锁：ping goroutine 与主流程会并发写 w。
	// 每次写入前重设写超时：僵尸 TCP 连接（合盖/NAT 静默掉线）会让阻塞写
	// 楔住 handler 与 wmu，连带 defer run.Close() 不执行、上游 run 心跳泄漏。
	var wmu sync.Mutex
	send := func(event string, v any) bool {
		data, err := json.Marshal(v)
		if err != nil {
			return false
		}
		line := fmt.Sprintf("event: %s\ndata: %s\n\n", event, data)
		wmu.Lock()
		defer wmu.Unlock()
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Second))
		if _, err := fmt.Fprint(w, line); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}

	msgID := newMessageID()
	send("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": msgID, "type": "message", "role": "assistant",
			"content": []any{}, "model": req.Model,
			"stop_reason": nil, "stop_sequence": nil,
			// 上游协议不提供 input token 计数，恒 0（turn_ended 后 message_delta 补实际值）
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})

	blockIndex := 0
	textOpen := false
	// 注：thinking 内容被整体丢弃（缺 Anthropic 必需的 signature），
	// 不存在 thinking block，关闭逻辑只需处理 text。
	closeText := func() {
		if textOpen {
			send("content_block_stop", map[string]any{"type": "content_block_stop", "index": blockIndex})
			blockIndex++
			textOpen = false
		}
	}

	// ping 保活：内置工具执行期间可能长时间无事件
	stopPing := make(chan struct{})
	pingDone := make(chan struct{})
	defer func() {
		// 必须等 ping goroutine 退出再返回：close(stopPing) 与 tick 同时就绪时
		// select 可能选中 tick，handler 返回后写 ResponseWriter 是数据竞争
		//（HTTP/1 keep-alive 下字节会污染该连接上的下一个响应）
		close(stopPing)
		<-pingDone
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
				send("ping", map[string]any{"type": "ping"})
			}
		}
	}()

	// 流式回调：文本直接转发；工具调用即时发块（保序：上游顺序 text→tool→text
	// 不再压扁成"合并 text + 尾部工具块"，块序即叙述序）
	onEvent := func(ev cursor.Event) bool {
		switch ev.Kind {
		case cursor.EventThinking:
			// Cursor 的 thinking 没有 Anthropic 必需的 signature 字段，转发会导致
			// SDK 校验失败（Execution error）。丢弃思考内容，仅保留正文。
			return true
		case cursor.EventText:
			if !textOpen {
				send("content_block_start", map[string]any{
					"type": "content_block_start", "index": blockIndex,
					"content_block": map[string]any{"type": "text", "text": ""},
				})
				textOpen = true
			}
			return send("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": blockIndex,
				"delta": map[string]any{"type": "text_delta", "text": ev.Text},
			})
		case cursor.EventToolCall:
			// ev 已被 runTurn 回填 toolu_ id / 剥前缀名 / 规范化 input
			closeText()
			if !send("content_block_start", map[string]any{
				"type": "content_block_start", "index": blockIndex,
				"content_block": map[string]any{
					"type": "tool_use", "id": ev.ToolCallID, "name": ev.ToolName, "input": map[string]any{},
				},
			}) {
				return false
			}
			if !send("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": blockIndex,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": ev.ToolArgsJSON},
			}) {
				return false
			}
			if !send("content_block_stop", map[string]any{"type": "content_block_stop", "index": blockIndex}) {
				return false
			}
			blockIndex++
			return true
		}
		return true
	}

	res := s.runTurn(ctx, run, results, onEvent)
	closeText()

	if res.err == nil && res.text.Len() == 0 && len(res.toolCalls) == 0 {
		res.err = errEmptyTurn(opts.Model)
	}
	if res.err != nil {
		// 协议内错误事件：客户端能显示具体原因，而不是无声"无回复"
		log.Printf("[anthropic] stream error: %v", res.err)
		send("error", map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "api_error",
				"message": res.err.Error(),
			},
		})
		return res, false
	}

	// 会话指纹用保序块结构（与发给客户端的一致，续接指纹才能命中）
	content := res.blocks
	if len(content) == 0 {
		content = []contentBlock{{Type: "text", Text: ""}}
	}

	// 最终帧先于入库：客户端没收到完整响应就断连时不存指纹，
	// 否则客户端历史里没有这条 assistant，下一轮续接锚点丢失
	okDelta := send("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": res.stopReason, "stop_sequence": nil},
		"usage": map[string]any{
			"input_tokens":  res.usage.InputTokens,
			"output_tokens": res.usage.OutputTokens,
		},
	})
	okStop := send("message_stop", map[string]any{"type": "message_stop"})
	if okDelta && okStop {
		s.finishTurn(res, opts, conv, content, ns)
		return res, len(res.toolCalls) > 0
	} else {
		dlog("messages: client gone before final frames, skip save")
	}
	return res, false
}

// writeAnthropicError Anthropic 格式错误响应。
func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": message,
		},
	})
}
