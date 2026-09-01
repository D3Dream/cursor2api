package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// normalizeJSON 空输入必须归一为 "{}"：
// json.RawMessage("") 会让整体 Marshal 失败（200 空响应体）。
func TestNormalizeJSONEmpty(t *testing.T) {
	if got := normalizeJSON(""); got != "{}" {
		t.Fatalf("normalizeJSON(\"\") = %q, want {}", got)
	}
	if got := normalizeJSON("  "); got != "{}" {
		t.Fatalf("normalizeJSON(blank) = %q, want {}", got)
	}
	// key 排序归一
	if got := normalizeJSON(`{"b":1,"a":2}`); got != `{"a":2,"b":1}` {
		t.Fatalf("normalizeJSON order = %q", got)
	}
	// 非法 JSON 原样返回
	if got := normalizeJSON(`{bad`); got != `{bad` {
		t.Fatalf("normalizeJSON(invalid) = %q, want passthrough", got)
	}
}

// LockConv 互斥：第二个请求必须等到第一个释放；ctx 取消则放弃。
func TestLockConvMutualExclusion(t *testing.T) {
	s := NewConversationStore(time.Minute)
	ctx := context.Background()

	release1 := s.LockConv("conv-1", ctx)
	if release1 == nil {
		t.Fatal("first LockConv = nil")
	}

	// 第二个：应在 release1 后获得
	got := make(chan func(), 1)
	go func() {
		got <- s.LockConv("conv-1", ctx)
	}()
	select {
	case <-got:
		t.Fatal("second LockConv 未等待在飞的第一个请求")
	case <-time.After(50 * time.Millisecond):
	}
	release1()
	select {
	case r2 := <-got:
		if r2 == nil {
			t.Fatal("second LockConv after release = nil")
		}
		r2()
	case <-time.After(time.Second):
		t.Fatal("second LockConv 未在释放后获得锁")
	}

	// ctx 取消：返回 nil
	release3 := s.LockConv("conv-2", ctx)
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	if r := s.LockConv("conv-2", cancelCtx); r != nil {
		t.Fatal("cancelled ctx LockConv != nil")
	}
	release3()

	// 空 id 是合法 no-op
	if r := s.LockConv("", ctx); r == nil {
		t.Fatal("empty id LockConv = nil")
	}
}

// 无文本首消息（纯 tool_result/图片开场）必须用结构指纹，
// 否则所有这类会话共享 hashText("") 命名空间互相碰撞。
func TestFirstUserTextStructuredFallback(t *testing.T) {
	// 纯 tool_result 开场：两个不同 tool_use_id 不得同 ns
	m1 := []anthropicMessage{{Role: "user", Content: []any{
		map[string]any{"type": "tool_result", "tool_use_id": "toolu_aaa", "content": "ok"},
	}}}
	m2 := []anthropicMessage{{Role: "user", Content: []any{
		map[string]any{"type": "tool_result", "tool_use_id": "toolu_bbb", "content": "ok"},
	}}}
	t1, t2 := firstUserText(m1), firstUserText(m2)
	if t1 == "" || t2 == "" {
		t.Fatalf("结构化指纹为空: %q / %q", t1, t2)
	}
	if t1 == t2 {
		t.Fatal("不同 tool_use_id 的纯 tool_result 开场指纹碰撞")
	}
	// 稳定：同一消息两次计算一致
	if firstUserText(m1) != t1 {
		t.Fatal("结构指纹不稳定")
	}
	// 文本优先：有文本时不走结构指纹
	m3 := []anthropicMessage{{Role: "user", Content: "hello"}}
	if firstUserText(m3) != "hello" {
		t.Fatalf("文本消息 firstUserText = %q", firstUserText(m3))
	}
}

// 图片块必须降级为占位文本而不是静默消失（冷启动路径经 embedHistoryAsText 进入 prompt）。
func TestImageBlockDegradesToPlaceholder(t *testing.T) {
	s := &Server{cfg: Config{CursorMode: "ask"}, conversations: NewConversationStore(time.Minute)}
	req := &anthropicRequest{
		Messages: []anthropicMessage{
			{Role: "user", Content: []any{
				map[string]any{"type": "image", "source": map[string]any{"data": "..."}},
			}},
			{Role: "user", Content: "what is this?"},
		},
	}
	opts, _, _, _ := s.planRun(req, "default")
	if !strings.Contains(opts.Prompt, "[image omitted") {
		t.Fatalf("prompt 缺图片占位符: %q", opts.Prompt)
	}
	if !strings.Contains(opts.Prompt, "what is this?") {
		t.Fatalf("prompt 缺用户文本: %q", opts.Prompt)
	}
}
