// OpenAI Chat Completions 请求/响应格式转换（直连 cursor 后端）。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"cursor2api/internal/cursor"
)

type ModelInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ChatCompletionRequest OpenAI 兼容对话请求体。
type ChatCompletionRequest struct {
	Model         string              `json:"model"`
	Messages      []ChatMessage       `json:"messages"`
	Stream        bool                `json:"stream"`
	User          string              `json:"user"`
	Tools         []openAITool        `json:"tools"`
	StreamOptions *openAIStreamOption `json:"stream_options"`
}

// openAITool OpenAI 格式的工具声明。
type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// openAIStreamOption 流式选项（include_usage 时末 chunk 带 usage）。
type openAIStreamOption struct {
	IncludeUsage bool `json:"include_usage"`
}

type ChatMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content"`
	ToolCallID string           `json:"tool_call_id,omitempty"` // role=tool 时携带
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`   // role=assistant 时携带
}

// openAIToolCall OpenAI 格式的工具调用。
// Index 仅流式 chunk 需要（OpenAI 流式规范要求每个 tool_call 带 index）。
type openAIToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAIError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

type errorResponse struct {
	Error openAIError `json:"error"`
}

type modelsListResponse struct {
	Object string      `json:"object"`
	Data   []modelItem `json:"data"`
}

type modelItem struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type chatCompletion struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []choice `json:"choices"`
	Usage   *usage   `json:"usage,omitempty"`
}

type choice struct {
	Index        int     `json:"index"`
	Message      *msg    `json:"message,omitempty"`
	Delta        *delta  `json:"delta,omitempty"`
	FinishReason *string `json:"finish_reason"`
}

type msg struct {
	Role      string           `json:"role"`
	Content   any              `json:"content"` // 工具调用轮为 null（OpenAI 规范），否则为文本
	ToolCalls []openAIToolCall `json:"tool_calls,omitempty"`
}

type delta struct {
	Role      string           `json:"role,omitempty"`
	Content   string           `json:"content,omitempty"`
	ToolCalls []openAIToolCall `json:"tool_calls,omitempty"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// writeJSON 写出 JSON 响应。返回 Encode 错误：调用方需要据此判断
// "客户端是否真的收到了完整响应"（写失败时不应保存会话指纹——
// 客户端历史里没有这条 assistant，入库的指纹会成为丢失的续接锚点）。
func writeJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message, errType string) {
	writeJSON(w, status, errorResponse{
		Error: openAIError{
			Message: message,
			Type:    errType,
		},
	})
}

// messageText 提取 OpenAI 消息文本。
// image_url 块降级为占位文本（协议无法传图，静默丢弃会让模型按错误前提作答）。
func messageText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if block["type"] == "text" || block["type"] == "input_text" || block["type"] == "output_text" {
				if text, ok := block["text"].(string); ok {
					parts = append(parts, text)
				}
				continue
			}
			if block["type"] == "image_url" {
				parts = append(parts, "[image omitted: not supported by upstream]")
			}
		}
		return strings.Join(parts, "\n")
	default:
		if content == nil {
			// 工具调用轮的 assistant 消息 content 常为 null，
			// fmt.Sprint(nil) 会得到 "<nil>" 混进 prompt
			return ""
		}
		return fmt.Sprint(content)
	}
}

// openAIToRunOptions 把 OpenAI 消息映射为 run 参数（冷启动：全量历史文本嵌入 + 最后 user 文本）。
// cursorMode 与 Anthropic 路径共用同一配置（agent=完整工具执行 ask=只读）。
func openAIToRunOptions(req *ChatCompletionRequest, model, cursorMode string) (cursor.RunOptions, error) {
	opts := cursor.RunOptions{Model: model}
	if cursorMode == "ask" {
		opts.Mode = 2
	} else {
		opts.Mode = 1
	}
	if len(req.Messages) == 0 {
		return opts, fmt.Errorf("messages is required")
	}
	for _, t := range req.Tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		// 统一加 cc 前缀（与 Anthropic 路径同策）：避免与 Cursor 内置工具撞名
		opts.Tools = append(opts.Tools, cursor.ToolDef{
			Name:        "cc" + t.Function.Name,
			Description: t.Function.Description,
			InputSchema: string(t.Function.Parameters),
		})
	}

	// 最后一条 user 消息作为 prompt，其余全部进历史
	lastUser := -1
	for i, m := range req.Messages {
		if m.Role == "user" {
			lastUser = i
		}
	}
	for i, m := range req.Messages {
		text := messageText(m.Content)
		switch m.Role {
		case "system", "developer":
			// developer 是 OpenAI 新规范里 system 的更名，同等对待
			if text == "" {
				continue
			}
			if opts.SystemPrompt != "" {
				opts.SystemPrompt += "\n\n"
			}
			opts.SystemPrompt += text
		case "user":
			if i == lastUser && text != "" {
				opts.Prompt = text
			} else if text != "" {
				opts.History = append(opts.History, cursor.HistoryMessage{Role: "user", Text: text})
			}
		case "assistant":
			if text != "" {
				opts.History = append(opts.History, cursor.HistoryMessage{Role: "assistant", Text: text})
			}
			for _, tc := range m.ToolCalls {
				opts.History = append(opts.History, cursor.HistoryMessage{
					Role: "assistant", ToolCallID: tc.ID, ToolName: tc.Function.Name, ArgsJSON: tc.Function.Arguments,
				})
			}
		case "tool":
			// 工具结果消息：进历史（续接场景由 buildOpenAITail 走重放应答，不到这里）
			opts.History = append(opts.History, cursor.HistoryMessage{
				Role: "tool", ToolCallID: m.ToolCallID, Text: text,
			})
		}
	}
	// 历史以文本形式嵌入 prompt（conversation_history 字段会被服务端忽略，实测）
	if len(opts.History) > 0 {
		opts.Prompt = embedHistoryAsText(opts.History) + opts.Prompt
		opts.History = nil
	}
	if opts.Prompt == "" {
		opts.Prompt = "(continue)"
	}
	return opts, nil
}

// pendingToOpenAIToolCalls 把收集到的挂起工具调用转为 OpenAI 格式。
// withIndex 用于流式 chunk（OpenAI 流式规范要求 index）。
func pendingToOpenAIToolCalls(tcs []PendingTool, withIndex bool) []openAIToolCall {
	out := make([]openAIToolCall, 0, len(tcs))
	for i, tc := range tcs {
		var c openAIToolCall
		if withIndex {
			idx := i
			c.Index = &idx
		}
		c.ID = tc.ToolUseID
		c.Type = "function"
		c.Function.Name = tc.Name
		c.Function.Arguments = tc.Input
		out = append(out, c)
	}
	return out
}

// incomingToolCalls 把客户端回传的 assistant 工具调用规范化（用于指纹匹配）。
func incomingToolCalls(m ChatMessage) []PendingTool {
	var out []PendingTool
	for _, tc := range m.ToolCalls {
		out = append(out, PendingTool{
			ToolUseID: tc.ID,
			Name:      tc.Function.Name,
			Input:     normalizeJSON(tc.Function.Arguments),
		})
	}
	return out
}

// hashOpenAIAssistant OpenAI assistant 响应指纹（文本 + 工具调用）。
func hashOpenAIAssistant(text string, tcs []PendingTool) string {
	h := sha256.New()
	h.Write([]byte(text))
	for _, tc := range tcs {
		fmt.Fprintf(h, "\x00%s\x00%s\x00%s\x00", tc.ToolUseID, tc.Name, normalizeJSON(tc.Input))
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// firstOpenAIUserText 提取首条 user 消息文本（会话命名空间，同 firstUserText）。
func firstOpenAIUserText(messages []ChatMessage) string {
	for _, m := range messages {
		if m.Role == "user" {
			if t := messageText(m.Content); t != "" {
				return t
			}
		}
	}
	return ""
}

// buildOpenAITail 续接时提取增量：匹配挂起调用的 tool 结果走重放应答，
// 其余（user 文本、对不上号的 tool 结果）拼成 prompt 文本。
func buildOpenAITail(tail []ChatMessage, pending []PendingTool) ([]pendingResult, string) {
	var results []pendingResult
	var textParts []string
	for _, m := range tail {
		switch m.Role {
		case "tool":
			matched := false
			for _, pt := range pending {
				if pt.ToolUseID == m.ToolCallID {
					results = append(results, pendingResult{Tool: pt, Text: messageText(m.Content)})
					matched = true
					break
				}
			}
			if !matched {
				textParts = append(textParts, fmt.Sprintf("[tool result %s]: %s", m.ToolCallID, messageText(m.Content)))
			}
		case "user":
			if t := messageText(m.Content); t != "" {
				textParts = append(textParts, t)
			}
		}
	}
	return results, strings.Join(textParts, "\n")
}

func buildCompletion(id, model, text string, toolCalls []PendingTool, u *usage, created int64) chatCompletion {
	finish := "stop"
	if len(toolCalls) > 0 {
		finish = "tool_calls"
	}
	m := &msg{Role: "assistant", Content: text}
	if len(toolCalls) > 0 {
		m.ToolCalls = pendingToOpenAIToolCalls(toolCalls, false)
		if text == "" {
			// OpenAI 规范：纯工具调用轮 content 为 null（"" 会让严格 schema 客户端报错）
			m.Content = nil
		}
	}
	resp := chatCompletion{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []choice{{
			Index:        0,
			Message:      m,
			FinishReason: &finish,
		}},
	}
	resp.Usage = u
	return resp
}

func buildChunk(id, model, deltaText string, done bool, created int64) chatCompletion {
	chunk := chatCompletion{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []choice{{
			Index: 0,
			Delta: &delta{},
		}},
	}
	if done {
		reason := "stop"
		chunk.Choices[0].FinishReason = &reason
	} else if deltaText != "" {
		chunk.Choices[0].Delta.Content = deltaText
	}
	return chunk
}

// buildToolCallsChunk 流式工具调用 chunk（delta.tool_calls，finish 单独发）。
func buildToolCallsChunk(id, model string, toolCalls []PendingTool, created int64) chatCompletion {
	return chatCompletion{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []choice{{
			Index: 0,
			Delta: &delta{
				ToolCalls: pendingToOpenAIToolCalls(toolCalls, true),
			},
		}},
	}
}

// buildFinishChunk 带指定 finish_reason 的结束 chunk（如 tool_calls）。
func buildFinishChunk(id, model, reason string, created int64) chatCompletion {
	return chatCompletion{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []choice{{
			Index:        0,
			Delta:        &delta{},
			FinishReason: &reason,
		}},
	}
}

// buildUsageChunk stream_options.include_usage 的末 usage chunk（choices 为空数组）。
func buildUsageChunk(id, model string, u *usage, created int64) chatCompletion {
	return chatCompletion{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []choice{},
		Usage:   u,
	}
}

// writeSSE 输出 OpenAI 兼容的 SSE 数据行。
// 注意：流式热路径（handleOpenAIStream）使用带锁+写超时的内联写入，不用此函数；
// 它保留给未来低并发场景，以及测试。
func writeSSE(w http.ResponseWriter, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}
