package main

import (
	"context"
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
