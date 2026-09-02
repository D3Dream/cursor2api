package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"cursor2api/internal/cursor"
)

// 回归：冷启动保存的会话 ID 曾为空串（Run 内部生成 ID 按值丢失），
// 导致续接时 conv.ID[:8] 切片越界 panic → 客户端 500 EOF。
func TestResumeConversationCarriesID(t *testing.T) {
	srv := &Server{
		cfg:           Config{CursorMode: "agent"},
		conversations: NewConversationStore(time.Minute),
	}

	// --- 第一轮：冷启动 ---
	req1 := &anthropicRequest{
		Model: "claude-test",
		Messages: []anthropicMessage{
			{Role: "user", Content: "hello"},
		},
	}
	opts1, _, conv1, ns1 := srv.planRun(req1, "cursor-model")
	if conv1 != nil {
		t.Fatalf("首轮不应匹配到会话, got %+v", conv1)
	}
	ensureConversationID(&opts1) // handler 在 Run 前补齐 ID
	if opts1.ConversationID == "" {
		t.Fatal("ensureConversationID 后 ConversationID 仍为空")
	}

	// finishTurn 保存会话（指纹 = 命名空间 + assistant 响应 blocks 哈希）
	assistantBlocks := []contentBlock{{Type: "text", Text: "hi there"}}
	srv.finishTurn(turnResult{}, opts1, nil, assistantBlocks, ns1)

	// --- 第二轮：带相同 assistant 响应，应续接 ---
	req2 := &anthropicRequest{
		Model: "claude-test",
		Messages: []anthropicMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: []any{map[string]any{"type": "text", "text": "hi there"}}},
			{Role: "user", Content: "how are you"},
		},
	}
	opts2, _, conv2, ns2 := srv.planRun(req2, "cursor-model")
	if ns1 != ns2 {
		t.Fatalf("同一会话历史应算出同一命名空间: %q != %q", ns1, ns2)
	}
	if conv2 == nil {
		t.Fatal("第二轮应匹配到已保存的会话")
	}
	if conv2.ID == "" {
		t.Fatal("续接会话 ID 为空——会在 handleMessages 日志处 panic")
	}
	if opts2.ConversationID != conv2.ID {
		t.Fatalf("续接应复用会话 ID: opts=%q conv=%q", opts2.ConversationID, conv2.ID)
	}
	if conv2.ID != opts1.ConversationID {
		t.Fatalf("续接 ID 应与首轮一致: first=%q second=%q", opts1.ConversationID, conv2.ID)
	}
}

func TestColdAnthropicRunOptionsReplaysToolPair(t *testing.T) {
	srv := &Server{cfg: Config{CursorMode: "agent"}}
	req := &anthropicRequest{
		System: "be concise",
		Tools: []anthropicTool{{
			Name: "Read", Description: "read a file", InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
		Messages: []anthropicMessage{
			{Role: "user", Content: "inspect config"},
			{Role: "assistant", Content: []any{map[string]any{
				"type": "tool_use", "id": "toolu_1", "name": "Read",
				"input": map[string]any{"file_path": "config.json"},
			}}},
			{Role: "user", Content: []any{map[string]any{
				"type": "tool_result", "tool_use_id": "toolu_1", "content": "file contents",
			}}},
		},
	}

	opts := srv.coldAnthropicRunOptions(req, "cursor-model")
	if opts.ConversationID != "" || opts.State != nil {
		t.Fatalf("cold fallback must not reuse state: id=%q state=%v", opts.ConversationID, opts.State)
	}
	if len(opts.Tools) != 1 || opts.Tools[0].Name != "ccRead" {
		t.Fatalf("unexpected tools: %#v", opts.Tools)
	}
	for _, want := range []string{"toolu_1", "Read", "config.json", "file contents"} {
		if !strings.Contains(opts.Prompt, want) {
			t.Fatalf("cold prompt missing %q: %s", want, opts.Prompt)
		}
	}
}

func TestDeleteConversationOnlyInvalidatesExactNamespace(t *testing.T) {
	store := NewConversationStore(time.Minute)
	const conversationID = "conv-fallback"
	store.Save(&Conversation{
		ID: conversationID, LastRespHash: "ns-a:assistant-a",
		PendingTools: []PendingTool{{ToolUseID: "toolu-old", Name: "Edit"}},
	})
	store.Save(&Conversation{ID: conversationID, LastRespHash: "ns-b:assistant-b"})

	if store.FindByPendingToolID("ns-a", "toolu-old") == nil {
		t.Fatal("test setup did not index the old pending tool")
	}
	store.DeleteConversation("ns-a", conversationID)
	if store.FindByRespHash("ns-a:assistant-a") != nil {
		t.Fatal("old namespace state survived fallback invalidation")
	}
	if store.FindByPendingToolID("ns-a", "toolu-old") != nil {
		t.Fatal("old pending-tool state survived fallback invalidation")
	}
	if store.FindByRespHash("ns-b:assistant-b") == nil {
		t.Fatal("fallback invalidation removed another namespace")
	}
}

func TestApplyAnthropicColdHistoryHandlesEmptyMessages(t *testing.T) {
	opts := cursor.RunOptions{}
	applyAnthropicColdHistory(&anthropicRequest{}, &opts)
	if opts.Prompt != "(continue)" {
		t.Fatalf("empty cold history prompt = %q, want continue sentinel", opts.Prompt)
	}
}

// 回归：不同会话产出相同 assistant 文本（"好的"/"Done."）时，
// 命名空间必须隔离指纹，否则会错拿别的会话的 checkpoint 续跑。
func TestConversationNamespaceIsolation(t *testing.T) {
	srv := &Server{
		cfg:           Config{CursorMode: "agent"},
		conversations: NewConversationStore(time.Minute),
	}

	optsA, _, _, nsA := srv.planRun(&anthropicRequest{
		Model:    "m",
		Messages: []anthropicMessage{{Role: "user", Content: "project A question"}},
	}, "m")
	ensureConversationID(&optsA)
	srv.finishTurn(turnResult{}, optsA, nil, []contentBlock{{Type: "text", Text: "好的"}}, nsA)

	// 会话 B：首条 user 不同，assistant 响应文本完全相同 → 不应匹配到 A
	_, _, convB, _ := srv.planRun(&anthropicRequest{
		Model: "m",
		Messages: []anthropicMessage{
			{Role: "user", Content: "project B question"},
			{Role: "assistant", Content: []any{map[string]any{"type": "text", "text": "好的"}}},
			{Role: "user", Content: "next"},
		},
	}, "m")
	if convB != nil {
		t.Fatal("相同 assistant 文本但不同命名空间，不应匹配到别的会话")
	}
}

// 续接匹配向前扫描：客户端改写/重试最后一条 assistant 时，
// 更早的 assistant 指纹仍能命中会话，救回服务端会话状态（分叉续接）。
func TestPlanRunMatchesEarlierAssistant(t *testing.T) {
	srv := &Server{
		cfg:           Config{CursorMode: "agent"},
		conversations: NewConversationStore(time.Minute),
	}
	opts1, _, _, ns1 := srv.planRun(&anthropicRequest{
		Model:    "m",
		Messages: []anthropicMessage{{Role: "user", Content: "hello"}},
	}, "m")
	ensureConversationID(&opts1)
	srv.finishTurn(turnResult{}, opts1, nil, []contentBlock{{Type: "text", Text: "reply A"}}, ns1)

	// 历史里 reply A 之后又出现一条没入库的 assistant（客户端改写），最后才是新 user
	_, _, conv, _ := srv.planRun(&anthropicRequest{
		Model: "m",
		Messages: []anthropicMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: []any{map[string]any{"type": "text", "text": "reply A"}}},
			{Role: "assistant", Content: []any{map[string]any{"type": "text", "text": "rewritten reply"}}},
			{Role: "user", Content: "next"},
		},
	}, "m")
	if conv == nil {
		t.Fatal("最后一条 assistant 不匹配时应向前扫描命中 reply A 的会话")
	}
	if conv.ID != opts1.ConversationID {
		t.Fatalf("命中会话 ID = %q, want %q", conv.ID, opts1.ConversationID)
	}
}

func TestPlanRunPrefersLatestPendingToolIDOverOlderFingerprint(t *testing.T) {
	srv := &Server{
		cfg: Config{CursorMode: "agent"}, conversations: NewConversationStore(time.Minute),
	}
	ns := hashText("chain tools")
	write := PendingTool{ToolUseID: "toolu_write", Name: "Write", Input: `{"file_path":"a"}`}
	edit := PendingTool{ToolUseID: "toolu_edit", Name: "Edit", Input: `{"file_path":"a","old_string":"x","new_string":"y"}`}
	srv.conversations.Save(&Conversation{
		ID: "conv-chain", LastRespHash: ns + ":" + hashBlocks([]contentBlock{{
			Type: "tool_use", ID: write.ToolUseID, Name: write.Name, Input: json.RawMessage(write.Input),
		}}), PendingTools: []PendingTool{write},
	})
	// The saved text intentionally differs from what the client sends below.
	// Full-response hashing therefore cannot match the latest Edit state.
	srv.conversations.Save(&Conversation{
		ID: "conv-chain", LastRespHash: ns + ":" + hashBlocks([]contentBlock{
			{Type: "text", Text: "server streamed text"},
			{Type: "tool_use", ID: edit.ToolUseID, Name: edit.Name, Input: json.RawMessage(edit.Input)},
		}), PendingTools: []PendingTool{edit},
	})
	req := &anthropicRequest{Messages: []anthropicMessage{
		{Role: "user", Content: "chain tools"},
		{Role: "assistant", Content: []any{map[string]any{
			"type": "tool_use", "id": write.ToolUseID, "name": write.Name,
			"input": map[string]any{"file_path": "a"},
		}}},
		{Role: "user", Content: []any{map[string]any{
			"type": "tool_result", "tool_use_id": write.ToolUseID, "content": "denied", "is_error": true,
		}}},
		{Role: "assistant", Content: []any{
			map[string]any{"type": "text", "text": "client normalized text"},
			map[string]any{
				"type": "tool_use", "id": edit.ToolUseID, "name": edit.Name,
				"input": map[string]any{"file_path": "a", "old_string": "x", "new_string": "y"},
			},
		}},
		{Role: "user", Content: []any{map[string]any{
			"type": "tool_result", "tool_use_id": edit.ToolUseID, "content": "edited",
		}}},
	}}
	_, results, conv, _ := srv.planRun(req, "default")
	if conv == nil || conv.PendingTools[0].ToolUseID != edit.ToolUseID {
		t.Fatalf("matched conversation = %+v, want latest Edit state", conv)
	}
	if len(results) != 1 || results[0].Tool.ToolUseID != edit.ToolUseID {
		t.Fatalf("results = %+v, want Edit result", results)
	}
}

// 防御：空 ID 的会话不允许入库（污染数据会让下一轮续接直接 panic）。
func TestFinishTurnSkipsEmptyID(t *testing.T) {
	srv := &Server{
		cfg:           Config{CursorMode: "agent"},
		conversations: NewConversationStore(time.Minute),
	}
	blocks := []contentBlock{{Type: "text", Text: "some reply"}}
	srv.finishTurn(turnResult{}, cursor.RunOptions{}, nil, blocks, "ns")
	if got := srv.conversations.FindByRespHash("ns:" + hashBlocks(blocks)); got != nil {
		t.Fatalf("空 ID 会话不应入库, got %+v", got)
	}
}

// 会话存储必须有上限：每个会话携带 Checkpoint（含 blobs），无上限时
// 大量会话能撑爆内存。超限按 LastUsed 淘汰最久未用（LRU）。
func TestConversationStoreLRUEviction(t *testing.T) {
	s := NewConversationStore(time.Minute)
	for i := 0; i < maxConversations; i++ {
		s.Save(&Conversation{ID: "c", LastRespHash: "h" + string(rune(i))})
	}
	// 触碰 h\x00（最早的一个），让它不再是"最久未用"
	first := "h" + string(rune(0))
	second := "h" + string(rune(1))
	if s.FindByRespHash(first) == nil {
		t.Fatal("填充后首个会话应存在")
	}
	// 再入一个 → 超限淘汰：被淘汰的是第二老的（首个刚被触碰过）
	s.Save(&Conversation{ID: "c-new", LastRespHash: "h-new"})
	if got := s.FindByRespHash("h-new"); got == nil {
		t.Fatal("新会话未入库")
	}
	if got := s.FindByRespHash(first); got == nil {
		t.Fatal("刚被触碰的会话不应被淘汰")
	}
	if got := s.FindByRespHash(second); got != nil {
		t.Fatal("最久未用的会话应被淘汰")
	}
	s.mu.Lock()
	n := len(s.byHash)
	s.mu.Unlock()
	if n > maxConversations {
		t.Fatalf("存储超限: %d > %d", n, maxConversations)
	}
}

// 防御：日志用短 ID 对空串/短串不 panic。
func TestShortConvID(t *testing.T) {
	cases := map[string]string{
		"":                    "-",
		"abc":                 "abc",
		"abcdefgh":            "abcdefgh",
		"abcdefghi0123456789": "abcdefgh",
	}
	for in, want := range cases {
		if got := shortConvID(in); got != want {
			t.Errorf("shortConvID(%q) = %q, want %q", in, got, want)
		}
	}
}

// 重放匹配必须乱序安全：服务端重放顺序 ≠ 客户端提交顺序时，
// 只查头部会把重放误判成新调用 → 客户端重复执行有副作用的工具。
// 同名并行调用必须靠参数区分，否则结果张冠李戴。
func TestMatchReplay(t *testing.T) {
	mk := func(names ...string) []pendingResult {
		out := make([]pendingResult, len(names))
		for i, n := range names {
			out[i] = pendingResult{Tool: PendingTool{Name: n}}
		}
		return out
	}
	mkIn := func(name, input string) pendingResult {
		return pendingResult{Tool: PendingTool{Name: name, Input: normalizeJSON(input)}}
	}

	cases := []struct {
		desc     string
		results  []pendingResult
		startIdx int
		toolName string
		args     string
		want     int
	}{
		{"顺序匹配", mk("Read", "Write"), 0, "ccRead", "", 0},
		{"乱序匹配（核心回归）", mk("Read", "Write"), 0, "ccWrite", "", 1},
		{"新调用不匹配", mk("Read"), 0, "ccBash", "", -1},
		{"忽略大小写", mk("Read"), 0, "ccread", "", 0},
		{"无 cc 前缀也能匹配", mk("Read"), 0, "Read", "", 0},
		{"从 startIdx 起搜（已匹配的跳过）", mk("Read", "Write", "Read"), 1, "ccRead", "", 2},
		{"startIdx 越界", mk("Read"), 1, "ccRead", "", -1},
		{"空 results", nil, 0, "ccRead", "", -1},
		// 同名并行调用：参数是唯一的区分手段
		{"同名不同参数按参数匹配", []pendingResult{mkIn("Read", `{"path":"a.go"}`), mkIn("Read", `{"path":"b.go"}`)}, 0, "ccRead", `{"path":"b.go"}`, 1},
		{"同名参数 JSON 格式差异归一化", []pendingResult{mkIn("Read", `{"a":1,"b":2}`), mkIn("Read", `{"path":"b.go"}`)}, 0, "ccRead", `{"b":2,"a":1}`, 0},
		{"同名多候选参数都不匹配→当新调用（宁重复执行不错配）", []pendingResult{mkIn("Read", `{"path":"a.go"}`), mkIn("Read", `{"path":"b.go"}`)}, 0, "ccRead", `{"path":"c.go"}`, -1},
		{"唯一同名参数不匹配→降级按名字（服务端可能改写参数）", []pendingResult{mkIn("Read", `{"path":"a.go"}`)}, 0, "ccRead", `{"path":"z.go"}`, 0},
		{"已匹配过的同名不再参配", []pendingResult{mkIn("Read", `{"path":"a.go"}`), mkIn("Read", `{"path":"a.go"}`)}, 1, "ccRead", `{"path":"a.go"}`, 1},
	}
	for _, c := range cases {
		if got := matchReplay(c.results, c.startIdx, c.toolName, c.args); got != c.want {
			t.Errorf("%s: matchReplay(%v, %d, %q, %q) = %d, want %d",
				c.desc, c.results, c.startIdx, c.toolName, c.args, got, c.want)
		}
	}
}
