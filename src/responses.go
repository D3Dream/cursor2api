package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"cursor2api/internal/cursor"
)

// responseSession indexes the response id returned to a client. Responses
// clients normally send only previous_response_id plus function_call_output
// on the next request, so assistant-message fingerprinting is insufficient.
type responseSession struct {
	Conv     *Conversation
	NS       string
	Messages []ChatMessage // bounded to keep response-session memory predictable
	LastUsed time.Time
	// ForceCold is set when Cursor ends a continuation without producing any
	// usable event. The next request may still reference this response id, so
	// retain its bounded history but do not reuse the rejected checkpoint.
	ForceCold bool
}

const maxResponseHistoryMessages = 512

type responsesRequest struct {
	Model            string          `json:"model"`
	Input            json.RawMessage `json:"input"`
	Instructions     string          `json:"instructions"`
	Tools            []responseTool  `json:"tools"`
	Stream           bool            `json:"stream"`
	User             string          `json:"user"`
	PreviousResponse string          `json:"previous_response_id"`
}

type responseTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

func responseInputMessages(raw json.RawMessage, instructions string) ([]ChatMessage, error) {
	var v any
	if len(raw) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("input is required")
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("invalid input: %w", err)
	}
	var out []ChatMessage
	if instructions != "" {
		out = append(out, ChatMessage{Role: "system", Content: instructions})
	}
	appendItem := func(item map[string]any) {
		typ, _ := item["type"].(string)
		role, _ := item["role"].(string)
		if typ == "function_call_output" {
			callID, _ := item["call_id"].(string)
			out = append(out, ChatMessage{Role: "tool", ToolCallID: callID, Content: responseValueText(item["output"])})
			return
		}
		if typ == "function_call" {
			name, _ := item["name"].(string)
			args, _ := item["arguments"].(string)
			callID, _ := item["call_id"].(string)
			m := ChatMessage{Role: "assistant", Content: nil}
			var tc openAIToolCall
			tc.ID, tc.Type, tc.Function.Name, tc.Function.Arguments = callID, "function", name, args
			m.ToolCalls = []openAIToolCall{tc}
			out = append(out, m)
			return
		}
		if role == "" {
			role = "user"
		}
		content := item["content"]
		if content == nil {
			content = item["text"]
		}
		out = append(out, ChatMessage{Role: role, Content: responseContentValue(content)})
	}
	switch x := v.(type) {
	case string:
		out = append(out, ChatMessage{Role: "user", Content: x})
	case []any:
		for _, item := range x {
			if m, ok := item.(map[string]any); ok {
				appendItem(m)
			}
		}
	case map[string]any:
		appendItem(x)
	default:
		return nil, fmt.Errorf("input must be a string, object, or array")
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("input is required")
	}
	return out, nil
}

func responseContentValue(v any) any {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return v
}

func responseValueText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func responsesToChatRequest(req *responsesRequest, model, cursorMode string) (*ChatCompletionRequest, error) {
	messages, err := responseInputMessages(req.Input, req.Instructions)
	if err != nil {
		return nil, err
	}
	chat := &ChatCompletionRequest{Model: model, Messages: messages, Stream: req.Stream, User: req.User}
	for _, t := range req.Tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		var tool openAITool
		tool.Type = "function"
		tool.Function.Name, tool.Function.Description, tool.Function.Parameters = t.Name, t.Description, t.Parameters
		chat.Tools = append(chat.Tools, tool)
	}
	return chat, nil
}

func responseResults(messages []ChatMessage, pending []PendingTool) []pendingResult {
	var out []pendingResult
	for _, m := range messages {
		if m.Role != "tool" {
			continue
		}
		for _, p := range pending {
			if p.ToolUseID == m.ToolCallID {
				out = append(out, pendingResult{Tool: p, Text: messageText(m.Content)})
				break
			}
		}
	}
	return out
}

func (s *Server) responseSessionGet(id string) (responseSession, bool) {
	s.responsesMu.Lock()
	defer s.responsesMu.Unlock()
	if s.responses == nil {
		s.responses = make(map[string]responseSession)
	}
	v, ok := s.responses[id]
	if ok {
		v.LastUsed = time.Now()
		s.responses[id] = v
	}
	return v, ok
}

func (s *Server) responseSessionPut(id string, value responseSession) {
	if id == "" || value.Conv == nil {
		return
	}
	s.responsesMu.Lock()
	defer s.responsesMu.Unlock()
	if s.responses == nil {
		s.responses = make(map[string]responseSession)
	}
	value.LastUsed = time.Now()
	if len(s.responses) >= maxConversations {
		var oldestID string
		var oldest time.Time
		for k := range s.responses {
			v := s.responses[k]
			if oldestID == "" || v.LastUsed.Before(oldest) {
				oldestID, oldest = k, v.LastUsed
			}
		}
		if oldestID != "" {
			delete(s.responses, oldestID)
		}
	}
	s.responses[id] = value
}

func (s *Server) responseSessionForceCold(id string) {
	if id == "" {
		return
	}
	s.responsesMu.Lock()
	defer s.responsesMu.Unlock()
	v, ok := s.responses[id]
	if !ok {
		return
	}
	v.ForceCold = true
	v.LastUsed = time.Now()
	s.responses[id] = v
}

func newResponseID() string { return "resp_" + randHex12() }

func responseHistory(base, current []ChatMessage, res turnResult) []ChatMessage {
	out := make([]ChatMessage, 0, len(base)+len(current)+1)
	out = append(out, base...)
	out = append(out, current...)
	if res.text.Len() > 0 || len(res.toolCalls) > 0 {
		m := ChatMessage{Role: "assistant"}
		if res.text.Len() > 0 {
			m.Content = res.text.String()
		}
		for _, tc := range res.toolCalls {
			var call openAIToolCall
			call.ID, call.Type = tc.ToolUseID, "function"
			call.Function.Name, call.Function.Arguments = tc.Name, tc.Input
			m.ToolCalls = append(m.ToolCalls, call)
		}
		out = append(out, m)
	}
	if len(out) > maxResponseHistoryMessages {
		// Keep leading system/developer instructions and the newest conversation
		// tail. This bounds memory while retaining the active turn context.
		prefix := 0
		for prefix < len(out) && (out[prefix].Role == "system" || out[prefix].Role == "developer") {
			prefix++
		}
		if prefix >= maxResponseHistoryMessages {
			return out[len(out)-maxResponseHistoryMessages:]
		}
		keepTail := maxResponseHistoryMessages - prefix
		if keepTail < 1 {
			keepTail = 1
		}
		if prefix+keepTail < len(out) {
			trimmed := make([]ChatMessage, 0, maxResponseHistoryMessages)
			trimmed = append(trimmed, out[:prefix]...)
			trimmed = append(trimmed, out[len(out)-keepTail:]...)
			out = trimmed
		}
	}
	return out
}

func responseOutput(id, model string, res turnResult, created int64) map[string]any {
	output := make([]any, 0, 1+len(res.toolCalls))
	if res.text.Len() > 0 {
		output = append(output, map[string]any{
			"type": "message", "id": "msg_" + randHex12(), "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": res.text.String(), "annotations": []any{}}},
		})
	}
	for _, tc := range res.toolCalls {
		output = append(output, map[string]any{
			"type": "function_call", "id": "fc_" + randHex12(), "call_id": tc.ToolUseID,
			"name": tc.Name, "arguments": tc.Input, "status": "completed",
		})
	}
	status := "completed"
	if len(res.toolCalls) > 0 {
		status = "completed"
	}
	return map[string]any{
		"id": id, "object": "response", "created_at": created, "status": status,
		"model": model, "output": output, "output_text": res.text.String(),
		"usage": map[string]any{"input_tokens": res.usage.InputTokens, "output_tokens": res.usage.OutputTokens, "total_tokens": res.usage.InputTokens + res.usage.OutputTokens},
	}
}

func (s *Server) saveResponseSession(id string, res turnResult, opts cursor.RunOptions, conv *Conversation, ns string, history []ChatMessage) {
	s.saveOpenAIConversation(res, opts, conv, ns)
	ck := res.lastCk
	if ck == nil && conv != nil {
		ck = conv.Checkpoint
	}
	s.responseSessionPut(id, responseSession{
		Conv: &Conversation{ID: opts.ConversationID, Checkpoint: ck, PendingTools: res.toolCalls},
		NS:   ns, Messages: responseHistory(nil, history, res),
	})
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(r) {
		writeError(w, http.StatusUnauthorized, "invalid api key", "invalid_request_error")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var req responsesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var mbErr *http.MaxBytesError
		if errors.As(err, &mbErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large", "invalid_request_error")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid json body", "invalid_request_error")
		return
	}
	model := s.mapModel(req.Model)
	if err := s.checkModelUsable(model); err != nil {
		writeError(w, http.StatusNotFound, err.Error(), "invalid_request_error")
		return
	}
	chatReq, err := responsesToChatRequest(&req, model, s.cfg.CursorMode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	opts, err := openAIToRunOptions(chatReq, model, s.cfg.CursorMode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	nsSeed := req.User + "|" + firstOpenAIUserText(chatReq.Messages)
	if sessionID := strings.TrimSpace(r.Header.Get("X-Agent-Session-ID")); sessionID != "" {
		nsSeed = sessionID + "|" + nsSeed
	}
	ns := hashText(nsSeed)
	var conv *Conversation
	var results []pendingResult
	var baseHistory []ChatMessage
	coldOpts := opts
	if req.PreviousResponse != "" {
		session, ok := s.responseSessionGet(req.PreviousResponse)
		if !ok || session.Conv == nil {
			writeError(w, http.StatusNotFound, "previous_response_id was not found or has expired", "invalid_request_error")
			return
		}
		conv, ns = session.Conv, session.NS
		baseHistory = append(baseHistory, session.Messages...)
		fullMessages := append(append([]ChatMessage(nil), baseHistory...), chatReq.Messages...)
		coldChatReq := *chatReq
		coldChatReq.Messages = fullMessages
		rebuilt, rebuildErr := openAIToRunOptions(&coldChatReq, model, s.cfg.CursorMode)
		if rebuildErr == nil {
			coldOpts = rebuilt
			if len(chatReq.Messages) == 0 || chatReq.Messages[len(chatReq.Messages)-1].Role != "user" {
				coldOpts.Prompt = "(continue)"
			}
		}
		if session.ForceCold {
			if rebuildErr != nil {
				writeError(w, http.StatusBadRequest, rebuildErr.Error(), "invalid_request_error")
				return
			}
			dlog("responses: previous response %s has a rejected checkpoint; rebuild cold", req.PreviousResponse)
			opts = coldOpts
			conv = nil
		} else {
			results = responseResults(chatReq.Messages, conv.PendingTools)
			opts.ConversationID, opts.State, opts.History = conv.ID, conv.Checkpoint, nil
			if len(results) > 0 {
				opts.Prompt = ""
			}
		}
	}
	ensureConversationID(&opts)
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.cfg.RequestTimeoutMs)*time.Millisecond)
	defer cancel()
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
	rebuildFromFullHistory := func(reason string) {
		dlog("responses: live continuation failed (%v); rebuild from full response history", reason)
		if live != nil && conv != nil {
			s.liveStore().Remove(conv.ID, live)
		}
		if conv != nil {
			s.conversations.DeleteConversation(ns, conv.ID)
		}
		opts = coldOpts
		ensureConversationID(&opts)
		conv, live, run, results = nil, nil, nil, nil
	}
	if conv != nil {
		live = s.liveStore().Get(conv.ID)
		if live != nil {
			if err := live.respond(ctx, results); err != nil {
				rebuildFromFullHistory(err.Error())
			} else {
				run = live.currentRun()
				if run == nil {
					rebuildFromFullHistory("live Cursor run is no longer available")
				} else {
					results = nil
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
			writeError(w, http.StatusBadGateway, err.Error(), "api_error")
			return
		}
	}
	id := newResponseID()
	var res turnResult
	if req.Stream {
		res = s.streamResponses(ctx, w, run, &req, opts, conv, results, ns, id)
	} else {
		res = s.runTurn(ctx, run, results, nil)
		if res.err == nil && res.text.Len() == 0 && len(res.toolCalls) == 0 {
			res.err = errEmptyTurn(opts.Model)
		}
		if res.err != nil {
			writeError(w, http.StatusBadGateway, res.err.Error(), "api_error")
		} else {
			if err := writeJSON(w, http.StatusOK, responseOutput(id, req.Model, res, time.Now().Unix())); err != nil {
				res.err = err
			}
		}
	}
	keep := res.err == nil && len(res.toolCalls) > 0
	if keep {
		if live == nil {
			s.liveStore().Put(opts.ConversationID, run, runCancel, res.toolCalls)
		} else {
			live.updatePending(res.toolCalls)
		}
		s.saveResponseSession(id, res, opts, conv, ns, append(append([]ChatMessage(nil), baseHistory...), chatReq.Messages...))
	} else {
		if live != nil {
			s.liveStore().Remove(opts.ConversationID, live)
			if conv != nil && res.err != nil {
				s.conversations.DeleteConversation(ns, conv.ID)
			}
		} else {
			if runCancel != nil {
				runCancel()
			}
			run.Close()
		}
		if res.err == nil {
			s.saveResponseSession(id, res, opts, conv, ns, append(append([]ChatMessage(nil), baseHistory...), chatReq.Messages...))
		}
		if conv != nil && isEmptyTurnError(res.err) {
			// Preserve the previous response's bounded message history, but make
			// its next continuation rebuild without the rejected checkpoint.
			s.conversations.DeleteConversation(ns, conv.ID)
			s.responseSessionForceCold(req.PreviousResponse)
		}
	}
}

func (s *Server) streamResponses(ctx context.Context, w http.ResponseWriter, run *cursor.Run, req *responsesRequest, opts cursor.RunOptions, conv *Conversation, results []pendingResult, ns, id string) turnResult {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	var wmu sync.Mutex
	send := func(event string, payload any) bool {
		b, err := json.Marshal(payload)
		if err != nil {
			return false
		}
		wmu.Lock()
		defer wmu.Unlock()
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Second))
		if _, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	stopPing := make(chan struct{})
	pingDone := make(chan struct{})
	defer func() {
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
				wmu.Lock()
				_, _ = fmt.Fprint(w, ": keep-alive\n\n")
				if flusher != nil {
					flusher.Flush()
				}
				wmu.Unlock()
			}
		}
	}()
	created := time.Now().Unix()
	send("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": id, "object": "response", "created_at": created, "status": "in_progress", "model": req.Model, "output": []any{}}})
	toolIndex := 0
	res := s.runTurn(ctx, run, results, func(ev cursor.Event) bool {
		switch ev.Kind {
		case cursor.EventText:
			return send("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "delta": ev.Text, "response_id": id, "output_index": 0, "content_index": 0})
		case cursor.EventToolCall:
			itemID := "fc_" + randHex12()
			idx := toolIndex
			toolIndex++
			return send("response.output_item.added", map[string]any{"type": "response.output_item.added", "response_id": id, "output_index": idx, "item": map[string]any{"type": "function_call", "id": itemID, "call_id": ev.ToolCallID, "name": ev.ToolName, "arguments": "", "status": "in_progress"}}) &&
				send("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "delta": ev.ToolArgsJSON, "response_id": id, "item_id": itemID, "output_index": idx}) &&
				send("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "arguments": ev.ToolArgsJSON, "response_id": id, "item_id": itemID, "output_index": idx}) &&
				send("response.output_item.done", map[string]any{"type": "response.output_item.done", "response_id": id, "output_index": idx, "item": map[string]any{"type": "function_call", "id": itemID, "call_id": ev.ToolCallID, "name": ev.ToolName, "arguments": ev.ToolArgsJSON, "status": "completed"}})
		}
		return true
	})
	if res.err == nil && res.text.Len() == 0 && len(res.toolCalls) == 0 {
		res.err = errEmptyTurn(opts.Model)
	}
	if res.err == nil {
		send("response.completed", map[string]any{"type": "response.completed", "response": responseOutput(id, req.Model, res, created)})
	} else {
		send("error", map[string]any{"type": "error", "error": map[string]any{"message": res.err.Error(), "type": "api_error"}})
	}
	return res
}
