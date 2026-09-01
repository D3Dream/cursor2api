package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cursor2api/internal/cursor"
)

func TestLiveRunStoreSeparatesConcurrentConversations(t *testing.T) {
	store := NewLiveRunStore(time.Minute)
	r1 := &cursor.Run{}
	r2 := &cursor.Run{}
	if store.Put("conv-a", r1, nil, nil) == nil || store.Put("conv-b", r2, nil, nil) == nil {
		t.Fatal("Put returned nil")
	}
	if store.Get("conv-a") == store.Get("conv-b") {
		t.Fatal("different conversations share a live run")
	}
}

func TestReplayAnthropicPendingStream(t *testing.T) {
	rec := httptest.NewRecorder()
	replayAnthropicPending(rec, &anthropicRequest{Model: "test-model", Stream: true}, []PendingTool{{
		ToolUseID: "toolu_edit_1", Name: "Edit", Input: `{"file_path":"README.md"}`,
	}})
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"event: message_start", "toolu_edit_1", `"name":"Edit"`, "event: message_stop"} {
		if !strings.Contains(body, want) {
			t.Fatalf("replay stream missing %q: %s", want, body)
		}
	}
}

func TestLiveRunPendingToolsReturnsCopy(t *testing.T) {
	live := &liveRun{pending: []PendingTool{{ToolUseID: "toolu_1", Name: "Edit"}}}
	got := live.pendingTools()
	got[0].Name = "changed"
	if live.pending[0].Name != "Edit" {
		t.Fatal("pendingTools exposed live mutable state")
	}
}

func TestHandleMessagesReplaysPendingToolWithoutResult(t *testing.T) {
	const (
		convID  = "conv-permission"
		toolID  = "toolu_edit_permission"
		toolArg = `{"file_path":"README.md","old_string":"a","new_string":"b"}`
	)
	store := NewConversationStore(time.Minute)
	blocks := []contentBlock{{
		Type: "tool_use", ID: toolID, Name: "Edit", Input: json.RawMessage(toolArg),
	}}
	ns := hashText("please edit")
	store.Save(&Conversation{
		ID: convID, LastRespHash: ns + ":" + hashBlocks(blocks),
		PendingTools: []PendingTool{{ToolUseID: toolID, Name: "Edit", Input: toolArg}},
	})
	liveStore := NewLiveRunStore(time.Minute)
	liveStore.Put(convID, &cursor.Run{}, nil, []PendingTool{{ToolUseID: toolID, Name: "Edit", Input: toolArg}})
	srv := &Server{
		cfg:           Config{APIKey: "test-key", CursorMode: "agent", RequestTimeoutMs: 30_000},
		conversations: store, liveRuns: liveStore, models: cursor.NewModelCache(time.Minute),
	}
	body, err := json.Marshal(anthropicRequest{
		Model: "default", Stream: true,
		Messages: []anthropicMessage{
			{Role: "user", Content: "please edit"},
			{Role: "assistant", Content: []any{map[string]any{
				"type": "tool_use", "id": toolID, "name": "Edit",
				"input": map[string]any{"file_path": "README.md", "old_string": "a", "new_string": "b"},
			}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	req.Header.Set("x-api-key", "test-key")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), toolID) {
		t.Fatalf("replay did not preserve tool id: %s", rec.Body.String())
	}
	if liveStore.Get(convID) == nil {
		t.Fatal("permission replay removed the live run")
	}
}

func TestUpstreamContextOutlivesHTTPRequest(t *testing.T) {
	s := &Server{cfg: Config{SessionTTLMs: 0}}
	parent, cancelParent := context.WithCancel(context.Background())
	ctx, cancel := s.newUpstreamContext(parent)
	cancelParent()
	select {
	case <-ctx.Done():
		t.Fatal("upstream context inherited downstream cancellation")
	default:
	}
	cancel()
}
