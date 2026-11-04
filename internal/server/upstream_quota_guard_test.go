package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/store"
)

type fakeQuotaGuardRepository struct {
	mu        sync.Mutex
	claim     store.UpstreamQuotaCheckClaim
	claimErr  error
	claims    int
	claimed   chan struct{}
	state     store.UpstreamQuotaGuardState
	recordErr error
	records   []store.RecordUpstreamQuotaObservationParams
}

func (f *fakeQuotaGuardRepository) ClaimUpstreamQuotaCheck(_ context.Context, _ string, _ time.Time) (store.UpstreamQuotaCheckClaim, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims++
	if f.claimed != nil {
		f.claimed <- struct{}{}
	}
	return f.claim, f.claimErr
}

func (f *fakeQuotaGuardRepository) RecordUpstreamQuotaObservation(_ context.Context, params store.RecordUpstreamQuotaObservationParams) (store.UpstreamQuotaGuardState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, params)
	return f.state, f.recordErr
}

func TestRunUpstreamQuotaCheckFollowsClaimReadAndRecord(t *testing.T) {
	next := time.Now().UTC().Add(37 * time.Minute)
	reset := time.Now().UTC().Add(time.Hour)
	for _, test := range []struct {
		name       string
		repository *fakeQuotaGuardRepository
		readCode   string
		wantRead   bool
		wantRecord bool
		want       func(time.Time) bool
	}{
		{
			name:       "claim failure waits for the floor",
			repository: &fakeQuotaGuardRepository{claimErr: errors.New("database down")},
			want:       floorAfter,
		},
		{
			name:       "unguarded account waits for the floor",
			repository: &fakeQuotaGuardRepository{},
			want:       floorAfter,
		},
		{
			name:       "not due adopts the stored schedule",
			repository: &fakeQuotaGuardRepository{claim: store.UpstreamQuotaCheckClaim{Provider: "codex", NextCheckAt: next}},
			want:       func(got time.Time) bool { return got.Equal(next) },
		},
		{
			name:       "read failure keeps the state",
			repository: &fakeQuotaGuardRepository{claim: store.UpstreamQuotaCheckClaim{Provider: "codex", Claimed: true}},
			readCode:   "upstream_quota_timeout", wantRead: true,
			want: floorAfter,
		},
		{
			name: "claimed check records an automatic reading",
			repository: &fakeQuotaGuardRepository{claim: store.UpstreamQuotaCheckClaim{Provider: "anthropic", Claimed: true},
				state: store.UpstreamQuotaGuardState{NextCheckAt: &next}},
			wantRead: true, wantRecord: true,
			want: func(got time.Time) bool { return got.Equal(next) },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var readProvider string
			s := &Server{quotaGuardRepo: test.repository, quotaGuardRead: func(_ context.Context, provider, _ string) (store.UpstreamQuotaObservation, string) {
				readProvider = provider
				return store.UpstreamQuotaObservation{FiveHour: &store.UpstreamQuotaWindow{UsedPercent: 40, ResetAt: &reset}}, test.readCode
			}}
			got := s.runUpstreamQuotaCheck("0123456789abcdef")
			if !test.want(got) {
				t.Fatalf("next=%v", got)
			}
			if (readProvider != "") != test.wantRead || (len(test.repository.records) == 1) != test.wantRecord {
				t.Fatalf("read=%q records=%+v", readProvider, test.repository.records)
			}
			if test.wantRecord {
				record := test.repository.records[0]
				if !record.Automatic || record.Provider != "anthropic" || record.AccountID != "0123456789abcdef" ||
					record.Observation.FiveHour == nil || record.Observation.FiveHour.UsedPercent != 40 {
					t.Fatalf("record=%+v", record)
				}
			}
		})
	}
}

func floorAfter(got time.Time) bool {
	wait := time.Until(got)
	return wait > store.UpstreamQuotaGuardMinInterval-time.Minute && wait <= store.UpstreamQuotaGuardMinInterval
}

func TestNoteUpstreamQuotaUseTouchesDatabaseOnlyWhenDue(t *testing.T) {
	repository := &fakeQuotaGuardRepository{
		claim:   store.UpstreamQuotaCheckClaim{Provider: "codex", NextCheckAt: time.Now().Add(30 * time.Minute)},
		claimed: make(chan struct{}, 4),
	}
	s := &Server{quotaGuardRepo: repository}
	s.noteUpstreamQuotaUse("0123456789abcdef")
	select {
	case <-repository.claimed:
	case <-time.After(5 * time.Second):
		t.Fatal("first request did not claim a check")
	}
	scheduler := s.upstreamQuotaGuardScheduler()
	deadline := time.Now().Add(5 * time.Second)
	for {
		scheduler.mu.Lock()
		entry := scheduler.entries["0123456789abcdef"]
		scheduler.mu.Unlock()
		if !entry.running && !entry.next.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("check did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	for range 5 {
		s.noteUpstreamQuotaUse("0123456789abcdef")
	}
	s.noteUpstreamQuotaUse("not-an-account")
	time.Sleep(20 * time.Millisecond)
	repository.mu.Lock()
	claims := repository.claims
	repository.mu.Unlock()
	if claims != 1 {
		t.Fatalf("claims=%d; requests before the next check must not reach the database", claims)
	}
	// Without a repository the guard is off.
	(&Server{}).noteUpstreamQuotaUse("0123456789abcdef")
}

func TestUpstreamQuotaGuardSchedulerBoundsConcurrentChecks(t *testing.T) {
	scheduler := newUpstreamQuotaGuardScheduler()
	now := time.Now()
	ids := []string{"0000000000000001", "0000000000000002", "0000000000000003", "0000000000000004", "0000000000000005"}
	for _, id := range ids[:upstreamQuotaGuardConcurrency] {
		if !scheduler.begin(id, now) {
			t.Fatalf("slot for %s refused", id)
		}
	}
	if scheduler.begin(ids[4], now) {
		t.Fatal("a fifth concurrent check started")
	}
	for _, id := range ids[:upstreamQuotaGuardConcurrency] {
		scheduler.finish(id, now.Add(time.Hour))
	}
	if scheduler.begin(ids[0], now) {
		t.Fatal("account checked before its next time")
	}
	scheduler.reschedule(ids[0], now.Add(-time.Second))
	if !scheduler.begin(ids[0], now) || scheduler.begin(ids[0], now) {
		t.Fatal("rescheduled account not due, or started twice")
	}
	scheduler.reschedule(ids[0], now.Add(time.Hour))
	scheduler.finish(ids[0], now.Add(-time.Second))
	if !scheduler.begin(ids[0], now) {
		t.Fatal("reschedule overrode a running check")
	}
	scheduler.finish(ids[0], now.Add(time.Hour))
	scheduler.forget()
	if !scheduler.begin(ids[1], now) {
		t.Fatal("forget kept a schedule")
	}
}

func TestUpstreamQuotaObservationKeepsReportedWindows(t *testing.T) {
	observed := time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)
	reset := observed.Add(time.Hour)
	used := 61
	observation := upstreamQuotaObservation(accountUsage{UpdatedAt: &observed,
		FiveHour: &accountUsageWindow{UsedPercent: &used, ResetAt: &reset}, SevenDay: &accountUsageWindow{ResetAt: &reset}}, observed.Add(time.Hour))
	if !observation.ObservedAt.Equal(observed) || observation.FiveHour == nil || observation.FiveHour.UsedPercent != 61 ||
		!observation.FiveHour.ResetAt.Equal(reset) || observation.SevenDay != nil {
		t.Fatalf("observation=%+v", observation)
	}
	now := observed.Add(2 * time.Hour)
	if got := upstreamQuotaObservation(accountUsage{}, now); !got.ObservedAt.Equal(now) || got.FiveHour != nil || got.SevenDay != nil {
		t.Fatalf("empty observation=%+v", got)
	}
}

func TestRecordUpstreamQuotaReadingIsNotAutomatic(t *testing.T) {
	next := time.Now().Add(25 * time.Minute)
	repository := &fakeQuotaGuardRepository{state: store.UpstreamQuotaGuardState{NextCheckAt: &next}}
	s := &Server{quotaGuardRepo: repository}
	used := 12
	s.recordUpstreamQuotaReading(context.Background(), store.UpstreamProviderCodex, "0123456789abcdef", accountUsage{FiveHour: &accountUsageWindow{UsedPercent: &used}})
	if len(repository.records) != 1 || repository.records[0].Automatic || repository.records[0].Observation.FiveHour.UsedPercent != 12 {
		t.Fatalf("records=%+v", repository.records)
	}
	if entry := s.upstreamQuotaGuardScheduler().entries["0123456789abcdef"]; !entry.next.Equal(next) {
		t.Fatalf("schedule=%+v", entry)
	}
	(&Server{}).recordUpstreamQuotaReading(context.Background(), store.UpstreamProviderCodex, "0123456789abcdef", accountUsage{})
}

func TestUpstreamQuotaGuardViewDefaultsToAvailable(t *testing.T) {
	view := upstreamQuotaGuardView(store.UpstreamQuotaGuardState{}, false)
	if view.Status != "available" || view.Source != "auto" || view.NextCheckAt != nil || view.RestoreAt != nil {
		t.Fatalf("view=%+v", view)
	}
	checked := time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)
	view = upstreamQuotaGuardView(store.UpstreamQuotaGuardState{Status: "available", Source: "manual", AutoCheckedAt: &checked}, true)
	if view.NextCheckAt == nil || !view.NextCheckAt.Equal(checked.Add(10*time.Minute)) {
		t.Fatalf("view=%+v", view)
	}
}

func TestUpstreamQuotaGuardHandlersRejectInvalidRequests(t *testing.T) {
	s := &Server{}
	for _, test := range []struct {
		name, path, contentType, body, code string
		status                              int
		handler                             func(http.ResponseWriter, *http.Request)
	}{
		{name: "account id", path: "/admin/upstream-accounts/XYZ/quota-guard", body: `{"limited":true}`, code: "upstream_account_not_found", status: 404, handler: s.setUpstreamAccountQuotaGuard},
		{name: "content type", path: "/admin/upstream-accounts/0123456789abcdef/quota-guard", contentType: "text/plain", body: `{"limited":true}`, code: "invalid_upstream_quota_guard_request", status: 400, handler: s.setUpstreamAccountQuotaGuard},
		{name: "string", path: "/admin/upstream-accounts/0123456789abcdef/quota-guard", body: `{"limited":"yes"}`, code: "invalid_json", status: 400, handler: s.setUpstreamAccountQuotaGuard},
		{name: "extra field", path: "/admin/upstream-accounts/0123456789abcdef/quota-guard", body: `{"limited":true,"until":1}`, code: "invalid_json", status: 400, handler: s.setUpstreamAccountQuotaGuard},
		{name: "null", path: "/admin/upstream-accounts/0123456789abcdef/quota-guard", body: `{"limited":null}`, code: "invalid_upstream_quota_guard_status", status: 400, handler: s.setUpstreamAccountQuotaGuard},
		{name: "percent above range", path: "/admin/upstream-quota-guard", body: `{"safety_percent":51}`, code: "invalid_upstream_quota_guard_percent", status: 400, handler: s.setUpstreamQuotaGuardSettings},
		{name: "negative percent", path: "/admin/upstream-quota-guard", body: `{"safety_percent":-1}`, code: "invalid_upstream_quota_guard_percent", status: 400, handler: s.setUpstreamQuotaGuardSettings},
		{name: "fractional percent", path: "/admin/upstream-quota-guard", body: `{"safety_percent":5.5}`, code: "invalid_json", status: 400, handler: s.setUpstreamQuotaGuardSettings},
		{name: "missing percent", path: "/admin/upstream-quota-guard", body: `{"safety_percent":null}`, code: "invalid_upstream_quota_guard_percent", status: 400, handler: s.setUpstreamQuotaGuardSettings},
		{name: "query", path: "/admin/upstream-quota-guard?x=1", body: `{"safety_percent":5}`, code: "invalid_upstream_quota_guard_request", status: 400, handler: s.setUpstreamQuotaGuardSettings},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPut, test.path, strings.NewReader(test.body))
			contentType := test.contentType
			if contentType == "" {
				contentType = "application/json"
			}
			r.Header.Set("Content-Type", contentType)
			if strings.Contains(test.path, "/upstream-accounts/") {
				r.SetPathValue("id", strings.Split(test.path, "/")[3])
			}
			w := httptest.NewRecorder()
			test.handler(w, r)
			if w.Code != test.status || !strings.Contains(w.Body.String(), test.code) {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}
