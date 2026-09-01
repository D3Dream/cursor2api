package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"cursor2api/internal/cursor"
)

// liveRun is a Cursor bidi stream paused at one or more downstream tool calls.
// Its lifetime is independent from the HTTP request that exposed the calls.
type liveRun struct {
	mu       sync.Mutex
	run      *cursor.Run
	cancel   context.CancelFunc
	convID   string
	pending  []PendingTool
	lastUsed time.Time
	closed   bool
}

func (l *liveRun) respond(results []pendingResult) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.run == nil || l.closed {
		return fmt.Errorf("live run is closed")
	}
	if len(results) == 0 {
		return fmt.Errorf("no tool results supplied for live run")
	}
	byID := make(map[string]PendingTool, len(l.pending))
	for _, p := range l.pending {
		byID[p.ToolUseID] = p
	}
	seen := make(map[string]bool, len(results))
	for _, result := range results {
		p, ok := byID[result.Tool.ToolUseID]
		if !ok {
			return fmt.Errorf("tool result %q does not belong to live run", result.Tool.ToolUseID)
		}
		seen[p.ToolUseID] = true
	}
	if len(seen) != len(byID) || len(results) != len(byID) {
		return fmt.Errorf("live run requires all %d pending tool results, got %d", len(byID), len(seen))
	}
	for _, result := range results {
		p := byID[result.Tool.ToolUseID]
		var err error
		if p.ExecName == "" || p.ExecName == "mcp_args" {
			err = l.run.RespondTool(p.ExecID, p.ExecIDStr, result.Text, result.IsErr)
		} else {
			err = l.run.RespondExec(p.ExecName, p.Input, p.ExecID, p.ExecIDStr, result.Text, result.IsErr)
		}
		if err != nil {
			return err
		}
	}
	l.lastUsed = time.Now()
	return nil
}

func (l *liveRun) updatePending(pending []PendingTool) {
	l.mu.Lock()
	l.pending = append([]PendingTool(nil), pending...)
	l.lastUsed = time.Now()
	l.mu.Unlock()
}

func (l *liveRun) pendingTools() []PendingTool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	return append([]PendingTool(nil), l.pending...)
}

func (l *liveRun) currentRun() *cursor.Run {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	return l.run
}

func (l *liveRun) close() {
	l.mu.Lock()
	if l.cancel != nil {
		l.cancel()
		l.cancel = nil
	}
	if l.run != nil {
		l.run.Close()
	}
	l.closed = true
	l.mu.Unlock()
}

// LiveRunStore owns all upstream runs that are waiting for downstream tools.
// Conversation locking prevents two requests from consuming one run at once;
// the store lock only protects the map and TTL/LRU bookkeeping.
type LiveRunStore struct {
	mu   sync.Mutex
	byID map[string]*liveRun
	ttl  time.Duration
}

const maxLiveRuns = 256

func NewLiveRunStore(ttl time.Duration) *LiveRunStore {
	return &LiveRunStore{byID: make(map[string]*liveRun), ttl: ttl}
}

func (s *Server) newUpstreamContext(parent context.Context) (context.Context, context.CancelFunc) {
	// A downstream HTTP request may finish immediately after a tool_use frame.
	// Do not inherit its cancellation into the Cursor bidi stream.
	base := context.WithoutCancel(parent)
	if s.cfg.SessionTTLMs > 0 {
		return context.WithTimeout(base, time.Duration(s.cfg.SessionTTLMs)*time.Millisecond)
	}
	return context.WithCancel(base)
}

func (s *Server) liveStore() *LiveRunStore {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if s.liveRuns == nil {
		s.liveRuns = NewLiveRunStore(time.Duration(s.cfg.SessionTTLMs) * time.Millisecond)
	}
	return s.liveRuns
}

func (s *LiveRunStore) Get(convID string) *liveRun {
	if convID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeLocked()
	l := s.byID[convID]
	if l != nil {
		l.mu.Lock()
		l.lastUsed = time.Now()
		l.mu.Unlock()
	}
	return l
}

func (s *LiveRunStore) Put(convID string, run *cursor.Run, cancel context.CancelFunc, pending []PendingTool) *liveRun {
	if convID == "" || run == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeLocked()
	if old := s.byID[convID]; old != nil && old.run != run {
		old.close()
	}
	l := &liveRun{
		run: run, cancel: cancel, convID: convID,
		pending: append([]PendingTool(nil), pending...), lastUsed: time.Now(),
	}
	s.byID[convID] = l
	for len(s.byID) > maxLiveRuns {
		var oldestID string
		var oldest time.Time
		for id, item := range s.byID {
			if id == convID {
				continue
			}
			item.mu.Lock()
			used := item.lastUsed
			item.mu.Unlock()
			if oldestID == "" || used.Before(oldest) {
				oldestID, oldest = id, used
			}
		}
		if oldestID == "" {
			break
		}
		old := s.byID[oldestID]
		delete(s.byID, oldestID)
		old.close()
	}
	return l
}

func (s *LiveRunStore) Remove(convID string, expected *liveRun) {
	if convID == "" {
		return
	}
	s.mu.Lock()
	if current := s.byID[convID]; current != nil && (expected == nil || current == expected) {
		delete(s.byID, convID)
		current.close()
	}
	s.mu.Unlock()
}

func (s *LiveRunStore) purgeLocked() {
	if s.ttl <= 0 {
		return
	}
	now := time.Now()
	for id, l := range s.byID {
		l.mu.Lock()
		expired := now.Sub(l.lastUsed) >= s.ttl
		l.mu.Unlock()
		if expired {
			delete(s.byID, id)
			l.close()
		}
	}
}
