package cursor

import (
	"encoding/binary"
	"io"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/dynamicpb"
)

func readAgentClientMessage(t *testing.T, reg interface {
	Unmarshal(string, []byte) (*dynamicpb.Message, error)
}, r io.Reader) *dynamicpb.Message {
	t.Helper()
	header := make([]byte, 5)
	if _, err := io.ReadFull(r, header); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, binary.BigEndian.Uint32(header[1:]))
	if _, err := io.ReadFull(r, payload); err != nil {
		t.Fatal(err)
	}
	msg, err := reg.Unmarshal("agent.v1.AgentClientMessage", payload)
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func TestHandleExecForwardsBuiltInWithoutLocalExecution(t *testing.T) {
	reg := testRegistry(t)
	esm, err := reg.New("agent.v1.ExecServerMessage")
	if err != nil {
		t.Fatal(err)
	}
	args := sub(esm, "shell_args")
	setStr(args, "command", "echo should-run-downstream")
	setStr(args, "working_directory", ".")

	r := &Run{
		reg:     reg,
		events:  make(chan Event, 1),
		closeCh: make(chan struct{}),
	}
	r.handleExec(esm)
	ev := <-r.events
	if ev.Kind != EventToolCall {
		t.Fatalf("event kind = %v, want EventToolCall", ev.Kind)
	}
	if ev.ToolName != "Bash" || ev.ExecName != "shell_args" {
		t.Fatalf("tool = %q exec = %q, want Bash/shell_args", ev.ToolName, ev.ExecName)
	}
	if !strings.Contains(ev.ToolArgsJSON, "should-run-downstream") {
		t.Fatalf("forwarded args missing command: %s", ev.ToolArgsJSON)
	}
}

func TestDownstreamToolArgsMapsFilePath(t *testing.T) {
	reg := testRegistry(t)
	args := func() *dynamicpb.Message {
		m, err := reg.New("agent.v1.ReadArgs")
		if err != nil {
			t.Fatal(err)
		}
		setStr(m, "path", "src/main.go")
		setStr(m, "tool_call_id", "cursor-internal-call-id")
		return m
	}()
	got := downstreamToolArgs("read_args", args)
	if !strings.Contains(got, `"file_path":"src/main.go"`) {
		t.Fatalf("mapped args = %s", got)
	}
	if strings.Contains(got, `"path"`) {
		t.Fatalf("Cursor path leaked instead of file_path: %s", got)
	}
	if strings.Contains(got, "tool_call_id") {
		t.Fatalf("Cursor internal tool_call_id leaked downstream: %s", got)
	}
}

func TestRespondExecReadSendsResultAndStreamClose(t *testing.T) {
	reg := testRegistry(t)
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	r := &Run{reg: reg, pw: writer}

	errCh := make(chan error, 1)
	go func() {
		errCh <- r.RespondExec("read_args", `{"file_path":"src/main.go"}`, 42, "exec-42", "package main", false)
	}()

	resultMsg := readAgentClientMessage(t, reg, reader)
	execMsg, ok := get(resultMsg, "exec_client_message")
	if !ok {
		t.Fatal("first frame is not exec_client_message")
	}
	if got := getUint(execMsg, "id"); got != 42 {
		t.Fatalf("result exec id = %d, want 42", got)
	}
	readResult, ok := get(execMsg, "read_result")
	if !ok {
		t.Fatal("first frame has no read_result")
	}
	success, ok := get(readResult, "success")
	if !ok || getStr(success, "content") != "package main" {
		t.Fatalf("read success content = %q", getStr(success, "content"))
	}

	closeMsg := readAgentClientMessage(t, reg, reader)
	control, ok := get(closeMsg, "exec_client_control_message")
	if !ok {
		t.Fatal("second frame is not exec_client_control_message")
	}
	streamClose, ok := get(control, "stream_close")
	if !ok || getUint(streamClose, "id") != 42 {
		t.Fatalf("stream_close id = %d, want 42", getUint(streamClose, "id"))
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}
