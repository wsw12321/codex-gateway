package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/store"
)

type fakeInformationRepository struct {
	calls    int
	err      error
	cutoff   time.Time
	create   store.InformationCleanupParams
	deletion store.DeleteInformationUsersParams
	job      store.InformationCleanupJob
}

func (f *fakeInformationRepository) PreviewInformationCleanup(_ context.Context, cutoff time.Time) (store.InformationCleanupReport, error) {
	f.calls++
	f.cutoff = cutoff
	return store.InformationCleanupReport{Cutoff: cutoff}, f.err
}
func (f *fakeInformationRepository) CreateInformationCleanupJob(_ context.Context, p store.InformationCleanupParams) (store.InformationCleanupJob, error) {
	f.calls++
	f.create = p
	return f.job, f.err
}
func (f *fakeInformationRepository) GetInformationCleanupJob(context.Context, string) (store.InformationCleanupJob, error) {
	f.calls++
	return f.job, f.err
}
func (f *fakeInformationRepository) LatestInformationCleanupJob(context.Context) (store.InformationCleanupJob, error) {
	f.calls++
	return f.job, f.err
}
func (f *fakeInformationRepository) InformationCleanedBefore(context.Context) (*time.Time, error) {
	f.calls++
	return nil, f.err
}
func (f *fakeInformationRepository) ListDeletableInformationUsers(context.Context, string, int, int) ([]store.DeletableUser, error) {
	f.calls++
	return []store.DeletableUser{}, f.err
}
func (f *fakeInformationRepository) DeleteInformationUsers(_ context.Context, p store.DeleteInformationUsersParams) (store.DeleteInformationUsersResult, error) {
	f.calls++
	f.deletion = p
	return store.DeleteInformationUsersResult{DeletedCount: len(p.UserIDs), UserIDs: p.UserIDs}, f.err
}

func TestInformationRoutesRequireOwnerAndWriteVerification(t *testing.T) {
	endpoints := []groupHandlerEndpoint{
		{name: "overview", method: "GET", path: "/admin/information"},
		{name: "preview", method: "POST", path: "/admin/information/preview", body: map[string]any{"retention_days": 90}},
		{name: "create", method: "POST", path: "/admin/information/jobs", body: map[string]any{"retention_days": 90, "operation_id": groupHandlerOperationID, "cutoff": "2020-01-01T00:00:00Z"}},
		{name: "status", method: "GET", path: "/admin/information/jobs/" + groupHandlerOperationID},
		{name: "candidates", method: "GET", path: "/admin/information/deletable-users"},
		{name: "delete", method: "POST", path: "/admin/information/users/delete", body: map[string]any{"operation_id": groupHandlerOperationID, "user_ids": []string{groupHandlerMemberID}}},
	}
	for _, endpoint := range endpoints {
		for _, scenario := range []string{"unauthenticated", "member", "foreign_origin", "unverified"} {
			if endpoint.method == "GET" && (scenario == "foreign_origin" || scenario == "unverified") {
				continue
			}
			if endpoint.name == "preview" && scenario == "unverified" {
				continue
			}
			t.Run(endpoint.name+"/"+scenario, func(t *testing.T) {
				verified := time.Now()
				var at *time.Time = &verified
				if scenario == "unverified" {
					at = nil
				}
				role := store.UserRoleOwner
				if scenario == "member" {
					role = store.UserRoleMember
				}
				s, _ := newBillingSourceTestServer(t, role, at)
				repo := &fakeInformationRepository{}
				s.informationRepo = repo
				r := groupHandlerRequest(t, endpoint)
				if scenario != "unauthenticated" {
					addBillingSourceTestSession(t, r)
				}
				if scenario == "foreign_origin" {
					r.Header.Set("Origin", "https://attacker.example")
				}
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, r)
				status := 403
				if scenario == "unauthenticated" {
					status = 401
				}
				if w.Code != status || repo.calls != 0 {
					t.Fatalf("status=%d calls=%d body=%s", w.Code, repo.calls, w.Body.String())
				}
			})
		}
	}
}

func TestInformationPreviewDefaultsAndConfirmedCutoff(t *testing.T) {
	verified := time.Now()
	s, _ := newBillingSourceTestServer(t, store.UserRoleOwner, &verified)
	repo := &fakeInformationRepository{}
	s.informationRepo = repo
	request := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Origin", "https://gateway.example")
		r.Header.Set("Content-Type", "application/json")
		addBillingSourceTestSession(t, r)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	w := request("/admin/information/preview", `{}`)
	want, _ := store.InformationCutoff(time.Now(), 90)
	if w.Code != 200 || !repo.cutoff.Equal(want) || !strings.Contains(w.Body.String(), `"retention_days":90`) {
		t.Fatalf("default preview: %d %s", w.Code, w.Body.String())
	}
	for _, body := range []string{`{"retention_days":0}`, `{"retention_days":-1}`, `{"retention_days":1.5}`, `{"retention_days":"90"}`} {
		w = request("/admin/information/preview", body)
		if w.Code != 400 {
			t.Fatalf("invalid preview %s: %d", body, w.Code)
		}
	}
	confirmed := want.AddDate(0, 0, -1).Format(time.RFC3339)
	w = request("/admin/information/jobs", `{"retention_days":90,"cutoff":"`+confirmed+`","operation_id":"`+groupHandlerOperationID+`"}`)
	if w.Code != 202 || repo.create.Cutoff.Format(time.RFC3339) != confirmed || repo.create.ActorUserID == "" || repo.create.ActorSessionID == "" {
		t.Fatalf("confirmed preview: %d %s %+v", w.Code, w.Body.String(), repo.create)
	}
	w = request("/admin/information/jobs", `{"retention_days":90,"cutoff":"`+time.Now().Add(24*time.Hour).Format(time.RFC3339)+`","operation_id":"`+groupHandlerOperationID+`"}`)
	if w.Code != 400 {
		t.Fatalf("future cutoff: %d", w.Code)
	}
}

func TestInformationUserConflictDetailsAndCleanedBillingError(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest("POST", "/admin/information/users/delete", nil)
	w := httptest.NewRecorder()
	s.informationStoreError(w, r, &store.UserDeletionBlockedError{Blockers: []store.UserDeletionBlocker{{UserID: groupHandlerMemberID, Reasons: []string{"balance", "ledger"}}}})
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"blockers"`) || !strings.Contains(w.Body.String(), `"balance"`) {
		t.Fatalf("conflict details: %s", w.Body.String())
	}
	w = httptest.NewRecorder()
	s.billingStoreError(w, r, "replay", store.ErrBillingOperationCleaned)
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"billing_operation_cleaned"`) {
		t.Fatalf("cleaned operation: %s", w.Body.String())
	}
}
