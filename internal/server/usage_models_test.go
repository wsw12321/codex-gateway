package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/store"
)

type fakeUsageModelRepository struct {
	filters []store.UsageFilter
	result  store.UsageModelBreakdown
	err     error
}

func (f *fakeUsageModelRepository) SummarizeUsageByModel(_ context.Context, filter store.UsageFilter) (store.UsageModelBreakdown, error) {
	f.filters = append(f.filters, filter)
	return f.result, f.err
}

func usageModelsRequest(t *testing.T, path string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	addBillingSourceTestSession(t, r)
	return r
}

func TestUsageModelsDefaultsToLast24HoursForTheSessionUser(t *testing.T) {
	s, _ := newBillingSourceTestServer(t, store.UserRoleMember, nil)
	repo := &fakeUsageModelRepository{result: store.UsageModelBreakdown{
		Models: []store.UsageModelSummary{
			{Model: "gpt-5.6", RequestCount: 3, ErrorCount: 1, InputTokens: 100, CachedInputTokens: 40, OutputTokens: 20, ChargedUSD: "1.250000000000"},
			{Model: "claude-sonnet-5", RequestCount: 1, InProgressCount: 1, ChargedUSD: "0"},
		},
		TotalRequests: 4, TotalTokens: 120, TotalChargedUSD: "1.250000000000",
	}}
	s.usageModelRepo = repo

	unauthenticated := httptest.NewRecorder()
	s.Handler().ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/admin/usage/models", nil))
	if unauthenticated.Code != http.StatusUnauthorized || len(repo.filters) != 0 {
		t.Fatalf("unauthenticated: status=%d reads=%d", unauthenticated.Code, len(repo.filters))
	}

	before := time.Now().UTC()
	w := httptest.NewRecorder()
	// A member's user_id is ignored: the summary is always their own.
	s.Handler().ServeHTTP(w, usageModelsRequest(t, "/admin/usage/models?user_id=someone-else"))
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d headers=%v body=%s", w.Code, w.Header(), w.Body.String())
	}
	if len(repo.filters) != 1 {
		t.Fatalf("reads=%d", len(repo.filters))
	}
	filter := repo.filters[0]
	if filter.UserID != "self-1" || filter.From == nil || filter.Until == nil || filter.Until.Sub(*filter.From) != 24*time.Hour ||
		filter.Until.Before(before) || filter.Until.After(time.Now().UTC().Add(2*time.Second)) {
		t.Fatalf("default filter = %+v", filter)
	}
	var body usageModelsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.From.Equal(*filter.From) || !body.Until.Equal(*filter.Until) || body.GeneratedAt.IsZero() || body.Truncated {
		t.Fatalf("window = %+v", body)
	}
	if body.Total != (usageModelTotal{Requests: 4, Tokens: 120, ChargedUSD: "1.250000000000"}) {
		t.Fatalf("total = %+v", body.Total)
	}
	want := []usageModelRow{
		{Model: "gpt-5.6", Requests: 3, Errors: 1, InputTokens: 100, CachedInputTokens: 40, OutputTokens: 20, Tokens: 120, ChargedUSD: "1.250000000000"},
		{Model: "claude-sonnet-5", Requests: 1, InProgress: 1, ChargedUSD: "0"},
	}
	if len(body.Models) != len(want) || body.Models[0] != want[0] || body.Models[1] != want[1] {
		t.Fatalf("models = %+v", body.Models)
	}
}

func TestUsageModelsHonoursFiltersAndOwnerScope(t *testing.T) {
	s, _ := newBillingSourceTestServer(t, store.UserRoleOwner, nil)
	repo := &fakeUsageModelRepository{result: store.UsageModelBreakdown{Models: []store.UsageModelSummary{}, TotalChargedUSD: "0.000000000000"}}
	s.usageModelRepo = repo

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, usageModelsRequest(t, "/admin/usage/models?user_id=member-2&from=2026-11-01T00:00:00Z&until=2026-11-03T00:00:00Z&model=gpt-5.6"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"models":[]`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	filter := repo.filters[0]
	if filter.UserID != "member-2" || filter.Model != "gpt-5.6" ||
		!filter.From.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) || !filter.Until.Equal(time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("filter = %+v", filter)
	}

	invalid := httptest.NewRecorder()
	s.Handler().ServeHTTP(invalid, usageModelsRequest(t, "/admin/usage/models?from=2026-11-03T00:00:00Z&until=2026-11-01T00:00:00Z"))
	if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), "invalid_usage_filter") || len(repo.filters) != 1 {
		t.Fatalf("invalid window: status=%d body=%s", invalid.Code, invalid.Body.String())
	}

	repo.err = errors.New("private database detail")
	failed := httptest.NewRecorder()
	s.Handler().ServeHTTP(failed, usageModelsRequest(t, "/admin/usage/models"))
	if failed.Code != http.StatusInternalServerError || strings.Contains(failed.Body.String(), "private database detail") {
		t.Fatalf("store failure: status=%d body=%s", failed.Code, failed.Body.String())
	}
}
