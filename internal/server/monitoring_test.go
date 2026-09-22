package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/store"
)

type fakeMonitoringRepository struct {
	snapshot store.MonitoringSnapshot
	err      error
	calls    int
}

func (f *fakeMonitoringRepository) Monitoring(context.Context) (store.MonitoringSnapshot, error) {
	f.calls++
	return f.snapshot, f.err
}

func TestMonitoringJSONReturnsIndependentWindowsAndActiveAttribution(t *testing.T) {
	sampledAt := time.Date(2026, time.September, 23, 8, 9, 10, 0, time.UTC)
	completedAt := sampledAt.Add(-time.Minute)
	active := store.MonitoringRequest{
		RequestID: "request-monitoring-active", RequestedAt: sampledAt.Add(-time.Second),
		UserID: "user-active", Username: "alice", DisplayName: "张三",
		Model: "gpt-6-astra", State: "in_progress",
	}
	failure := store.MonitoringRequest{
		RequestID: "request-monitoring-failed", RequestedAt: sampledAt.Add(-2 * time.Minute),
		CompletedAt: &completedAt, UserID: "user-failed", Username: "bob", DisplayName: "Bob",
		Model: "gpt-6-sol", State: "failed",
	}
	repository := &fakeMonitoringRepository{snapshot: store.MonitoringSnapshot{
		SampledAt: sampledAt, InProgress: []store.MonitoringRequest{active},
		Recent: []store.MonitoringRequest{active}, Failures: []store.MonitoringRequest{failure},
	}}
	server := &Server{
		monitoringRepo: repository,
		activeAttributions: map[string]activeRequestAttribution{
			active.RequestID: {AccountID: "0123456789abcdef", UpdatedAt: sampledAt},
		},
	}
	response := httptest.NewRecorder()
	server.monitoringJSON(response, httptest.NewRequest(http.MethodGet, "/admin/monitoring", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var document monitoringResponse
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if repository.calls != 1 || !document.SampledAt.Equal(sampledAt) || len(document.InProgress) != 1 ||
		len(document.Recent) != 1 || len(document.Failures) != 1 {
		t.Fatalf("response = %#v, calls = %d", document, repository.calls)
	}
	for _, row := range []monitoringRequestDTO{document.InProgress[0], document.Recent[0]} {
		if row.UpstreamAccountID == nil || *row.UpstreamAccountID != "0123456789abcdef" {
			t.Fatalf("active attribution missing: %#v", row)
		}
	}
	if document.Failures[0].UpstreamAccountID != nil {
		t.Fatalf("terminal attribution was changed: %#v", document.Failures[0])
	}
}

func TestActiveAttributionLifecycleRejectsInvalidValuesAndPrunesExpiredEntries(t *testing.T) {
	server := &Server{activeAttributions: make(map[string]activeRequestAttribution)}
	server.rememberActiveAttribution("request-a", "0123456789abcdef")
	server.rememberActiveAttribution("request-b", "not-an-account")
	if got := server.activeAttributionSnapshot(time.Now()); len(got) != 1 || got["request-a"] != "0123456789abcdef" {
		t.Fatalf("active attributions = %#v", got)
	}

	server.activeAttributionsMu.Lock()
	server.activeAttributions["expired"] = activeRequestAttribution{
		AccountID: "fedcba9876543210", UpdatedAt: time.Now().Add(-activeAttributionTTL - time.Second),
	}
	server.activeAttributionsMu.Unlock()
	if got := server.activeAttributionSnapshot(time.Now()); len(got) != 1 {
		t.Fatalf("expired attribution was retained: %#v", got)
	}
	server.clearActiveAttribution("request-a")
	if got := server.activeAttributionSnapshot(time.Now()); len(got) != 0 {
		t.Fatalf("cleared attribution was retained: %#v", got)
	}
}
