package server

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAnthropicAccountSyncWaitIsCancellable(t *testing.T) {
	s := &Server{}
	s.upstreamAccountSyncMu.Lock()
	defer s.upstreamAccountSyncMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- s.syncAnthropicAccounts(ctx) }()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("sync wait = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled request waited for unrelated account sync to unlock")
	}
}

func TestAnthropicAccountSyncAlreadyCancelledAvoidsIOAndUnlocks(t *testing.T) {
	s := &Server{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.syncAnthropicAccounts(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled sync = %v", err)
	}
	if !s.upstreamAccountSyncMu.TryLock() {
		t.Fatal("cancelled sync retained lock")
	}
	s.upstreamAccountSyncMu.Unlock()
}
