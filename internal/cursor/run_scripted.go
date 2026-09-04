package cursor

// NewScriptedRun builds a Run whose Events() channel replays the provided
// events and then closes. It performs no network I/O and is intended for tests
// that exercise the SSE translation layers (Anthropic / OpenAI / Responses)
// without a live upstream. RespondTool/RespondExec are inert on such a run.
func NewScriptedRun(events []Event) *Run {
	ch := make(chan Event, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return &Run{
		events:   ch,
		closeCh:  make(chan struct{}),
		scripted: true,
		blobs:    make(map[string][]byte),
		execSem:  make(chan struct{}, maxConcurrentExec),
	}
}
