package cursor

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/types/dynamicpb"
)

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
		return m
	}()
	got := downstreamToolArgs("read_args", args)
	if !strings.Contains(got, `"file_path":"src/main.go"`) {
		t.Fatalf("mapped args = %s", got)
	}
	if strings.Contains(got, `"path"`) {
		t.Fatalf("Cursor path leaked instead of file_path: %s", got)
	}
}
