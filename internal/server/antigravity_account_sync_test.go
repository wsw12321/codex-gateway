package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
)

type antigravitySyncTransport func(*http.Request) (*http.Response, error)

func (transport antigravitySyncTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestTrySyncAntigravityAccountsDoesNotWaitForBusyLock(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	base, _ := url.Parse("http://antigravity.test")
	s := &Server{antigravity: gatewayproxy.NewWithHTTPClient(base, "bridge-token", &http.Client{
		Transport: antigravitySyncTransport(func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			if request.URL.Path != "/internal/upstream-accounts" || request.Header.Get("Authorization") != "Bearer bridge-token" {
				t.Error("account synchronization used an incorrect endpoint or credentials")
			}
			if deadline, ok := request.Context().Deadline(); !ok || time.Until(deadline) > upstreamAccountSyncTimeout {
				t.Error("account synchronization omitted the internal timeout")
			}
			return &http.Response{StatusCode: http.StatusServiceUnavailable,
				Header: http.Header{"Content-Type": {"application/json"}},
				Body:   io.NopCloser(strings.NewReader(`{"error":{"code":"upstream_unavailable"}}`))}, nil
		}),
	})}
	s.upstreamAccountSyncMu.Lock()
	locked := true
	defer func() {
		if locked {
			s.upstreamAccountSyncMu.Unlock()
		}
	}()
	result := make(chan error, 1)
	go func() { result <- s.trySyncAntigravityAccounts(context.Background()) }()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("busy account synchronization unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		s.upstreamAccountSyncMu.Unlock()
		locked = false
		<-result
		t.Fatal("catalog account synchronization waited for the busy lock")
	}
	if calls.Load() != 0 {
		t.Fatal("busy synchronization contacted the bridge")
	}
	s.upstreamAccountSyncMu.Unlock()
	locked = false
	// A released lock reaches the real client failure path. Both entrypoints
	// must release the lock on failure so the next synchronization can proceed.
	for _, synchronize := range []func(context.Context) error{s.trySyncAntigravityAccounts, s.syncAntigravityAccounts, s.trySyncAntigravityAccounts} {
		err := synchronize(context.Background())
		var upstreamError *gatewayproxy.InternalAPIError
		if !errors.As(err, &upstreamError) || upstreamError.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("released synchronization error = %v; want HTTP 503", err)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("bridge calls = %d, want 3", calls.Load())
	}
}

func TestTrySyncAntigravityAccountsPreservesContextDeadline(t *testing.T) {
	t.Parallel()
	base, _ := url.Parse("http://antigravity.test")
	s := &Server{antigravity: gatewayproxy.NewWithHTTPClient(base, "bridge-token", &http.Client{
		Transport: antigravitySyncTransport(func(request *http.Request) (*http.Response, error) {
			<-request.Context().Done()
			return nil, request.Context().Err()
		}),
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := s.trySyncAntigravityAccounts(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("synchronization error = %v; want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("synchronization ignored caller deadline: %s", elapsed)
	}
	if !s.upstreamAccountSyncMu.TryLock() {
		t.Fatal("synchronization retained the lock after cancellation")
	}
	s.upstreamAccountSyncMu.Unlock()
}
