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

// sseEvent is one parsed `event:`/`data:` SSE frame.
type sseEvent struct {
	name string
	data map[string]any
}

// parseSSE splits an SSE body into ordered event frames, skipping comments
// (keep-alive lines beginning with ":").
func parseSSE(t *testing.T, body string) []sseEvent {
	t.Helper()
	var out []sseEvent
	for _, block := range strings.Split(body, "\n\n") {
		var name, data string
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if name == "" || data == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			t.Fatalf("event %q has invalid data json: %v\n%s", name, err, data)
		}
		out = append(out, sseEvent{name: name, data: m})
	}
	return out
}

func newStreamTestServer() *Server {
	return &Server{
		cfg:           Config{APIKey: "test-key", RequestTimeoutMs: 5000},
		conversations: NewConversationStore(time.Minute),
		liveRuns:      NewLiveRunStore(time.Minute),
		models:        cursor.NewModelCache(time.Minute),
		responses:     make(map[string]responseSession),
	}
}

// TestStreamResponsesTextLifecycle guards the Codex-critical SSE contract: a
// plain-text reply must be wrapped in a message item that is announced
// (output_item.added + content_part.added) before any output_text.delta, every
// text delta must carry item_id + output_index, and the item must be closed
// (output_text.done + content_part.done + output_item.done) before
// response.completed. Without this envelope Codex silently drops the text and
// the client shows no reply.
func TestStreamResponsesTextLifecycle(t *testing.T) {
	srv := newStreamTestServer()
	run := cursor.NewScriptedRun([]cursor.Event{
		{Kind: cursor.EventText, Text: "Hello, "},
		{Kind: cursor.EventText, Text: "world"},
		{Kind: cursor.EventTurnEnd, Usage: &cursor.Usage{InputTokens: 3, OutputTokens: 2}},
		{Kind: cursor.EventDone},
	})
	rec := httptest.NewRecorder()
	req := &responsesRequest{Model: "default", Stream: true}
	res := srv.streamResponses(context.Background(), rec, run, req, cursor.RunOptions{Model: "default"}, nil, nil, "ns", "resp_test")
	if res.err != nil {
		t.Fatalf("unexpected turn error: %v", res.err)
	}
	events := parseSSE(t, rec.Body.String())

	// Ordering invariants.
	idxOf := func(name string) int {
		for i, e := range events {
			if e.name == name {
				return i
			}
		}
		return -1
	}
	itemAdded := idxOf("response.output_item.added")
	partAdded := idxOf("response.content_part.added")
	firstDelta := idxOf("response.output_text.delta")
	itemDone := idxOf("response.output_item.done")
	completed := idxOf("response.completed")
	for name, i := range map[string]int{
		"output_item.added": itemAdded, "content_part.added": partAdded,
		"output_text.delta": firstDelta, "output_item.done": itemDone,
		"response.completed": completed,
	} {
		if i < 0 {
			t.Fatalf("missing event %q; got %v", name, eventNames(events))
		}
	}
	if !(itemAdded < partAdded && partAdded < firstDelta && firstDelta < itemDone && itemDone < completed) {
		t.Fatalf("events out of order: %v", eventNames(events))
	}

	// Every text delta must carry item_id and output_index.
	var itemID string
	var deltas []string
	for _, e := range events {
		if e.name != "response.output_text.delta" {
			continue
		}
		id, _ := e.data["item_id"].(string)
		if id == "" {
			t.Fatalf("output_text.delta missing item_id: %#v", e.data)
		}
		if _, ok := e.data["output_index"]; !ok {
			t.Fatalf("output_text.delta missing output_index: %#v", e.data)
		}
		if itemID == "" {
			itemID = id
		} else if id != itemID {
			t.Fatalf("text deltas span multiple item_ids: %q vs %q", itemID, id)
		}
		deltas = append(deltas, e.data["delta"].(string))
	}
	if got := strings.Join(deltas, ""); got != "Hello, world" {
		t.Fatalf("reassembled text = %q, want %q", got, "Hello, world")
	}

	// sequence_number must be present and strictly increasing.
	prev := -1
	for _, e := range events {
		sn, ok := e.data["sequence_number"].(float64)
		if !ok {
			t.Fatalf("event %q missing sequence_number", e.name)
		}
		if int(sn) <= prev {
			t.Fatalf("sequence_number not increasing at %q: %v after %d", e.name, sn, prev)
		}
		prev = int(sn)
	}
}

func eventNames(events []sseEvent) []string {
	names := make([]string, len(events))
	for i, e := range events {
		names[i] = e.name
	}
	return names
}

func TestResponsesUnknownPreviousResponseReturnsNotFound(t *testing.T) {
	srv := &Server{
		cfg:           Config{APIKey: "test-key"},
		conversations: NewConversationStore(time.Minute),
		liveRuns:      NewLiveRunStore(time.Minute),
		models:        cursor.NewModelCache(time.Minute),
		responses:     make(map[string]responseSession),
	}
	body := []byte(`{"model":"default","input":"continue","previous_response_id":"resp_missing"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["error"] == nil {
		t.Fatalf("missing error body: %v", got)
	}
}

func TestResponseInputMessages(t *testing.T) {
	raw := json.RawMessage(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"read file"}]},{"type":"function_call","call_id":"call_1","name":"Read","arguments":"{\"file_path\":\"a.txt\"}"},{"type":"function_call_output","call_id":"call_1","output":"hello"}]`)
	msgs, err := responseInputMessages(raw, "be precise")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 || msgs[0].Role != "system" || messageText(msgs[1].Content) != "read file" {
		t.Fatalf("unexpected messages: %#v", msgs)
	}
	if len(msgs[2].ToolCalls) != 1 || msgs[2].ToolCalls[0].ID != "call_1" {
		t.Fatalf("missing function call: %#v", msgs[2])
	}
	if msgs[3].Role != "tool" || msgs[3].ToolCallID != "call_1" || messageText(msgs[3].Content) != "hello" {
		t.Fatalf("missing function output: %#v", msgs[3])
	}
}

func TestResponseOutputFunctionCall(t *testing.T) {
	res := turnResult{}
	res.appendToolCall(cursor.Event{ToolName: "Read", ToolArgsJSON: `{"file_path":"a.txt"}`})
	out := responseOutput("resp_1", "m", res, 1)
	items, ok := out["output"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("unexpected output: %#v", out)
	}
	item := items[0].(map[string]any)
	if item["type"] != "function_call" || item["name"] != "Read" {
		t.Fatalf("unexpected function call item: %#v", item)
	}
}
