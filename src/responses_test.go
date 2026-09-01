package main

import (
	"encoding/json"
	"testing"

	"cursor2api/internal/cursor"
)

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
