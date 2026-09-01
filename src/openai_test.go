package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 回归：工具调用轮的 assistant 消息 content 为 null，
// fmt.Sprint(nil) 会得到 "<nil>" 混进 prompt 和会话指纹。
func TestMessageTextNil(t *testing.T) {
	if got := messageText(nil); got != "" {
		t.Errorf("messageText(nil) = %q, want empty", got)
	}
	if got := messageText("hi"); got != "hi" {
		t.Errorf("messageText(string) = %q, want hi", got)
	}
	blocks := []any{
		map[string]any{"type": "text", "text": "a"},
		map[string]any{"type": "image", "url": "x"}, // 非 text 块跳过
		map[string]any{"type": "text", "text": "b"},
	}
	if got := messageText(blocks); got != "a\nb" {
		t.Errorf("messageText(blocks) = %q, want %q", got, "a\\nb")
	}
}

// OpenAI 续接尾部：tool 结果按 tool_call_id 匹配挂起调用走重放，
// 对不上号的和普通 user 文本拼成 prompt。
func TestBuildOpenAITail(t *testing.T) {
	pending := []PendingTool{
		{ToolUseID: "toolu_1", Name: "Read", Input: `{"path":"a.go"}`},
		{ToolUseID: "toolu_2", Name: "Write", Input: `{"path":"b.go"}`},
	}
	tail := []ChatMessage{
		{Role: "tool", ToolCallID: "toolu_2", Content: "wrote b.go"}, // 乱序：先回第二个
		{Role: "tool", ToolCallID: "toolu_1", Content: "file content"},
		{Role: "tool", ToolCallID: "toolu_9", Content: "orphan"}, // 无挂起 → 转文本
		{Role: "user", Content: "also check this"},               // 附带新指令
	}
	results, prompt := buildOpenAITail(tail, pending)
	if len(results) != 2 {
		t.Fatalf("应匹配 2 个挂起调用, got %d", len(results))
	}
	// 匹配与顺序无关，按 tool_call_id 对上号
	byID := map[string]string{}
	for _, r := range results {
		byID[r.Tool.ToolUseID] = r.Text
	}
	if byID["toolu_1"] != "file content" || byID["toolu_2"] != "wrote b.go" {
		t.Errorf("结果错配: %v", byID)
	}
	if prompt == "" || len(results) != 2 {
		t.Errorf("prompt 应包含 orphan 结果和新指令, got %q", prompt)
	}
}

// 空文本 + 有工具调用的指纹必须非空且互不相同（空指纹多会话互相覆盖的回归）。
func TestHashOpenAIAssistant(t *testing.T) {
	h1 := hashOpenAIAssistant("", []PendingTool{{ToolUseID: "t1", Name: "Read", Input: `{"path":"a"}`}})
	h2 := hashOpenAIAssistant("", []PendingTool{{ToolUseID: "t1", Name: "Read", Input: `{"path":"b"}`}})
	if h1 == h2 {
		t.Error("不同参数的工具调用指纹应不同")
	}
	// 参数 JSON 格式差异应归一化
	h3 := hashOpenAIAssistant("", []PendingTool{{ToolUseID: "t1", Name: "Read", Input: `{
		"path": "a"
	}`}})
	if h1 != h3 {
		t.Error("语义相同的参数 JSON 指纹应相同")
	}
	if hashOpenAIAssistant("text", nil) == hashOpenAIAssistant("", nil) {
		t.Error("有/无文本指纹应不同")
	}
}

// 冷启动转换：history 文本嵌入 + 工具消息进历史 + prompt 兜底。
func TestOpenAIToRunOptions(t *testing.T) {
	req := &ChatCompletionRequest{
		Model: "m",
		Messages: []ChatMessage{
			{Role: "system", Content: "be brief"},
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: nil, ToolCalls: mkOpenAIToolCall("c1", "Read", `{"path":"a.go"}`)},
			{Role: "tool", ToolCallID: "c1", Content: "file body"},
			{Role: "user", Content: "now what"},
		},
	}
	opts, err := openAIToRunOptions(req, "m", "agent")
	if err != nil {
		t.Fatal(err)
	}
	if opts.Mode != 1 {
		t.Errorf("agent 模式 Mode = %d, want 1", opts.Mode)
	}
	if opts.SystemPrompt != "be brief" {
		t.Errorf("SystemPrompt = %q", opts.SystemPrompt)
	}
	// prompt 应含嵌入历史 + 最后一条 user 文本
	if opts.Prompt == "now what" {
		t.Error("冷启动应把历史文本嵌入 prompt")
	}
	if len(opts.History) != 0 {
		t.Error("历史应已文本化，History 字段置空")
	}
	if !containsAll(opts.Prompt, "<conversation_history>", "user: hi", "assistant called tool Read", "tool result [c1]: file body", "now what") {
		t.Errorf("prompt 缺历史片段:\n%s", opts.Prompt)
	}

	// ask 模式
	opts, _ = openAIToRunOptions(req, "m", "ask")
	if opts.Mode != 2 {
		t.Errorf("ask 模式 Mode = %d, want 2", opts.Mode)
	}

	// developer role（OpenAI 新规范的 system 更名）必须进 SystemPrompt，
	// 静默丢弃会让模型行为与预期不符且无任何线索
	reqDev := &ChatCompletionRequest{Messages: []ChatMessage{
		{Role: "developer", Content: "be terse"},
		{Role: "user", Content: "hi"},
	}}
	optsDev, err := openAIToRunOptions(reqDev, "m", "agent")
	if err != nil {
		t.Fatal(err)
	}
	if optsDev.SystemPrompt != "be terse" {
		t.Errorf("developer role 未进 SystemPrompt: %q", optsDev.SystemPrompt)
	}
}

// writeJSON 必须把底层写错误返回给调用方：
// 非流式路径据此决定"客户端没收到响应就不存会话指纹"，
// 吞掉错误会让入库指纹成为丢失的续接锚点。
type failWriter struct{ header http.Header }

func (f *failWriter) Header() http.Header { return f.header }
func (f *failWriter) WriteHeader(int)     {}
func (f *failWriter) Write([]byte) (int, error) {
	return 0, errors.New("broken pipe")
}

func TestWriteJSONReportsError(t *testing.T) {
	if err := writeJSON(&failWriter{header: http.Header{}}, 200, map[string]string{"a": "b"}); err == nil {
		t.Fatal("写失败应返回错误")
	}
	if err := writeJSON(httptest.NewRecorder(), 200, map[string]string{"a": "b"}); err != nil {
		t.Fatalf("正常写入不应报错: %v", err)
	}
}

// OpenAI 续接同样向前扫描：最后一条 assistant 不匹配时命中更早的指纹。
func TestFindOpenAIConversationScansBack(t *testing.T) {
	store := NewConversationStore(time.Minute)
	ns := hashText("hello")
	store.Save(&Conversation{
		ID:           "conv-1",
		LastRespHash: ns + ":" + hashOpenAIAssistant("reply A", nil),
	})
	msgs := []ChatMessage{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "reply A"},
		{Role: "assistant", Content: "rewritten"},
		{Role: "user", Content: "next"},
	}
	conv, idx := findOpenAIConversation(msgs, store, ns)
	if conv == nil || conv.ID != "conv-1" {
		t.Fatalf("应命中 conv-1: %+v", conv)
	}
	if idx != 1 {
		t.Fatalf("idx = %d, want 1（reply A 的位置，tail 从其后开始）", idx)
	}
	if conv, idx = findOpenAIConversation(msgs, store, "other-ns"); conv != nil || idx != -1 {
		t.Fatalf("不同命名空间不应命中: %+v idx=%d", conv, idx)
	}
}

// mapModel：精确映射 > 通配回退 > 直通；auto/空 → default。
func TestMapModel(t *testing.T) {
	srv := &Server{cfg: Config{ModelMap: map[string]string{
		"claude-x": "cursor-y",
		"*":        "default",
	}}}
	cases := map[string]string{
		"":         "default",  // 空 → default
		"auto":     "default",  // auto 别名 → default
		"claude-x": "cursor-y", // 精确映射
		"unknown":  "default",  // 通配回退
	}
	for in, want := range cases {
		if got := srv.mapModel(in); got != want {
			t.Errorf("mapModel(%q) = %q, want %q", in, got, want)
		}
	}
	// 无通配时未知模型直通
	srv2 := &Server{cfg: Config{ModelMap: map[string]string{}}}
	if got := srv2.mapModel("gpt-x"); got != "gpt-x" {
		t.Errorf("无通配 mapModel(gpt-x) = %q, want 直通 gpt-x", got)
	}
}

func mkOpenAIToolCall(id, name, args string) []openAIToolCall {
	var c openAIToolCall
	c.ID = id
	c.Type = "function"
	c.Function.Name = name
	c.Function.Arguments = args
	return []openAIToolCall{c}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
