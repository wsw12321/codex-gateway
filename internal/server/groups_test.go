package server

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/store"
)

const groupHandlerTestID = "22b7a99d-3b76-4c60-8e9e-4dd8459554ed"
const groupHandlerOperationID = "1159cfe8-e43d-47f1-a2f9-b74b74d3fdec"
const groupHandlerMemberID = "1d305460-13c0-43cb-a98c-558ce8bc3d24"

type fakeGroupRepository struct {
	err     error
	groups  []store.GroupSummary
	group   store.Group
	calls   int
	readID  string
	put     *store.PutGroupParams
	members *store.SetGroupMembersParams
	archive *store.ArchiveGroupParams
}

func (f *fakeGroupRepository) ListGroups(context.Context) ([]store.GroupSummary, error) {
	f.calls++
	return f.groups, f.err
}
func (f *fakeGroupRepository) GetGroup(_ context.Context, id string) (store.Group, error) {
	f.calls++
	f.readID = id
	return f.group, f.err
}
func (f *fakeGroupRepository) PutGroup(_ context.Context, p store.PutGroupParams) (store.Group, error) {
	f.calls++
	f.put = &p
	return f.group, f.err
}
func (f *fakeGroupRepository) SetGroupMembers(_ context.Context, p store.SetGroupMembersParams) (store.Group, error) {
	f.calls++
	f.members = &p
	return f.group, f.err
}
func (f *fakeGroupRepository) ArchiveGroup(_ context.Context, p store.ArchiveGroupParams) (store.Group, error) {
	f.calls++
	f.archive = &p
	return f.group, f.err
}

type groupHandlerEndpoint struct {
	name, method, path string
	body               map[string]any
}

func groupHandlerWriteEndpoints() []groupHandlerEndpoint {
	groupBody := func() map[string]any {
		return map[string]any{"operation_id": groupHandlerOperationID, "reason": " approved budget ", "name": "Research", "limit_usd": "12.345678", "period": "custom", "custom_days": 4, "starts_at": "2026-10-01T01:02:03Z"}
	}
	return []groupHandlerEndpoint{
		{"create", http.MethodPost, "/admin/groups", groupBody()},
		{"update", http.MethodPut, "/admin/groups/" + groupHandlerTestID, groupBody()},
		{"members", http.MethodPut, "/admin/groups/" + groupHandlerTestID + "/members", map[string]any{"operation_id": groupHandlerOperationID, "reason": " approved budget ", "action": "add", "user_ids": []string{groupHandlerMemberID}}},
		{"archive", http.MethodDelete, "/admin/groups/" + groupHandlerTestID, map[string]any{"operation_id": groupHandlerOperationID, "reason": " approved budget "}},
	}
}

func groupHandlerRequest(t *testing.T, e groupHandlerEndpoint) *http.Request {
	t.Helper()
	var body []byte
	if e.body != nil {
		var err error
		body, err = json.Marshal(e.body)
		if err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(e.method, e.path, strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://gateway.example")
	return r
}

func TestGroupRoutesRequireOwnerSessionOriginAndRecentVerification(t *testing.T) {
	endpoints := append(groupHandlerWriteEndpoints(), groupHandlerEndpoint{name: "list", method: http.MethodGet, path: "/admin/groups"}, groupHandlerEndpoint{name: "detail", method: http.MethodGet, path: "/admin/groups/" + groupHandlerTestID})
	for _, e := range endpoints {
		t.Run(e.name, func(t *testing.T) {
			type routeCase struct {
				name, role, origin, site, code string
				session, verified              bool
				age                            time.Duration
				status                         int
			}
			cases := []routeCase{
				{name: "unauthenticated", origin: "https://gateway.example", code: "session_required", status: 401},
				{name: "member", role: store.UserRoleMember, origin: "https://gateway.example", session: true, verified: true, code: "owner_required", status: 403},
			}
			if e.method != http.MethodGet {
				cases = append(cases,
					routeCase{name: "foreign origin", origin: "https://foreign.example", code: "invalid_origin", status: 403},
					routeCase{name: "cross site", origin: "https://gateway.example", site: "cross-site", code: "cross_site_request", status: 403},
					routeCase{name: "owner unverified", role: store.UserRoleOwner, origin: "https://gateway.example", session: true, code: "recent_identity_verification_required", status: 403},
					routeCase{name: "owner verification expired", role: store.UserRoleOwner, origin: "https://gateway.example", session: true, verified: true, age: 6 * time.Minute, code: "recent_identity_verification_required", status: 403},
				)
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					var verified *time.Time
					if tc.verified {
						at := time.Now().Add(-tc.age)
						verified = &at
					}
					s, _ := newBillingSourceTestServer(t, tc.role, verified)
					repo := &fakeGroupRepository{}
					s.groupRepo = repo
					r := groupHandlerRequest(t, e)
					r.Header.Set("Origin", tc.origin)
					r.Header.Set("Sec-Fetch-Site", tc.site)
					if tc.session {
						addBillingSourceTestSession(t, r)
					}
					w := httptest.NewRecorder()
					s.Handler().ServeHTTP(w, r)
					if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) || repo.calls != 0 {
						t.Fatalf("status=%d body=%s calls=%d", w.Code, w.Body.String(), repo.calls)
					}
					if w.Header().Get("Cache-Control") != "no-store" {
						t.Fatal("group response may be cached")
					}
				})
			}
		})
	}
}

func TestGroupWriteRoutesRejectMissingIdempotencyAndReason(t *testing.T) {
	for _, e := range groupHandlerWriteEndpoints() {
		t.Run(e.name, func(t *testing.T) {
			for _, tc := range []struct {
				name, key string
				value     any
				remove    bool
			}{
				{"missing operation", "operation_id", nil, true}, {"malformed operation", "operation_id", "not-a-uuid", false},
				{"missing reason", "reason", nil, true}, {"blank reason", "reason", " \t ", false}, {"long reason", "reason", strings.Repeat("理", 501), false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					now := time.Now()
					s, _ := newBillingSourceTestServer(t, store.UserRoleOwner, &now)
					repo := &fakeGroupRepository{}
					s.groupRepo = repo
					body := make(map[string]any, len(e.body))
					for k, v := range e.body {
						body[k] = v
					}
					if tc.remove {
						delete(body, tc.key)
					} else {
						body[tc.key] = tc.value
					}
					copy := e
					copy.body = body
					r := groupHandlerRequest(t, copy)
					addBillingSourceTestSession(t, r)
					w := httptest.NewRecorder()
					s.Handler().ServeHTTP(w, r)
					if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_group_operation") || repo.calls != 0 {
						t.Fatalf("status=%d body=%s calls=%d", w.Code, w.Body.String(), repo.calls)
					}
				})
			}
		})
	}
}

func TestGroupWriteRoutesCarryAuthenticatedAttribution(t *testing.T) {
	for _, e := range groupHandlerWriteEndpoints() {
		t.Run(e.name, func(t *testing.T) {
			now := time.Now()
			s, _ := newBillingSourceTestServer(t, store.UserRoleOwner, &now)
			repo := &fakeGroupRepository{group: store.Group{GroupSummary: store.GroupSummary{ID: groupHandlerTestID, Name: "Research"}, Members: []store.GroupMember{}}}
			s.groupRepo = repo
			r := groupHandlerRequest(t, e)
			r.RemoteAddr = "192.0.2.30:4000"
			r.Header.Set("X-Forwarded-For", "203.0.113.77")
			addBillingSourceTestSession(t, r)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != 200 || repo.calls != 1 {
				t.Fatalf("status=%d body=%s calls=%d", w.Code, w.Body.String(), repo.calls)
			}
			var write store.BillingWriteParams
			switch e.name {
			case "create", "update":
				if repo.put == nil {
					t.Fatal("put not called")
				}
				p := repo.put
				write = p.BillingWriteParams
				wantID := groupHandlerTestID
				if e.name == "create" {
					wantID = ""
				}
				wantStart, _ := time.Parse(time.RFC3339, "2026-10-01T01:02:03Z")
				if p.GroupID != wantID || p.Name != "Research" || p.LimitUSD != "12.345678" || p.Period != "custom" || p.CustomDays != 4 || p.StartsAt == nil || !p.StartsAt.Equal(wantStart) {
					t.Fatalf("put params: %+v", p)
				}
			case "members":
				if repo.members == nil {
					t.Fatal("member write not called")
				}
				p := repo.members
				write = p.BillingWriteParams
				if p.GroupID != groupHandlerTestID || p.Action != "add" || !reflect.DeepEqual(p.UserIDs, []string{groupHandlerMemberID}) {
					t.Fatalf("member params: %+v", p)
				}
			case "archive":
				if repo.archive == nil {
					t.Fatal("archive not called")
				}
				write = repo.archive.BillingWriteParams
				if repo.archive.GroupID != groupHandlerTestID {
					t.Fatalf("archive params: %+v", repo.archive)
				}
			}
			if write.OperationID != groupHandlerOperationID || write.Reason != "approved budget" || write.ActorUserID != "self-1" || write.ActorSessionID != "session-1" || write.RequestID == "" || write.RequestID != w.Header().Get(httpx.RequestIDHeader) || write.SourceIP != "192.0.2.30" || write.At.Before(now) || write.At.After(time.Now()) {
				t.Fatalf("audit attribution: %+v", write)
			}
		})
	}
}

func TestGroupReadAndStoreErrorResponses(t *testing.T) {
	endpoints := append(groupHandlerWriteEndpoints(), groupHandlerEndpoint{name: "list", method: http.MethodGet, path: "/admin/groups"}, groupHandlerEndpoint{name: "detail", method: http.MethodGet, path: "/admin/groups/" + groupHandlerTestID})
	for _, e := range endpoints {
		for _, tc := range []struct {
			err    error
			status int
			code   string
		}{
			{fmt.Errorf("wrapped: %w", store.ErrInvalid), 400, "invalid_group_operation"}, {store.ErrNotFound, 404, "group_resource_not_found"}, {store.ErrConflict, 409, "group_operation_conflict"}, {errors.New("private database detail"), 500, "internal_error"},
		} {
			t.Run(e.name+"/"+tc.code, func(t *testing.T) {
				now := time.Now()
				s, _ := newBillingSourceTestServer(t, store.UserRoleOwner, &now)
				repo := &fakeGroupRepository{err: tc.err}
				s.groupRepo = repo
				r := groupHandlerRequest(t, e)
				addBillingSourceTestSession(t, r)
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, r)
				if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) || strings.Contains(w.Body.String(), "private database detail") || repo.calls != 1 {
					t.Fatalf("status=%d body=%s calls=%d", w.Code, w.Body.String(), repo.calls)
				}
			})
		}
	}
	now := time.Now()
	s, _ := newBillingSourceTestServer(t, store.UserRoleOwner, &now)
	repo := &fakeGroupRepository{}
	s.groupRepo = repo
	r := groupHandlerRequest(t, groupHandlerEndpoint{method: http.MethodGet, path: "/admin/groups"})
	addBillingSourceTestSession(t, r)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"groups":[]`) {
		t.Fatalf("empty list: %d %s", w.Code, w.Body.String())
	}
	repo.group = store.Group{GroupSummary: store.GroupSummary{ID: groupHandlerTestID}, Members: []store.GroupMember{{UserID: groupHandlerMemberID, UsedUSD: "0.25"}}}
	r = groupHandlerRequest(t, groupHandlerEndpoint{method: http.MethodGet, path: "/admin/groups/" + groupHandlerTestID})
	addBillingSourceTestSession(t, r)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 || repo.readID != groupHandlerTestID || !strings.Contains(w.Body.String(), groupHandlerMemberID) {
		t.Fatalf("detail: %d %s", w.Code, w.Body.String())
	}
}

func TestGroupRoutesRejectMalformedResourceAndMemberRequests(t *testing.T) {
	for _, e := range append(groupHandlerWriteEndpoints()[1:], groupHandlerEndpoint{name: "detail", method: http.MethodGet, path: "/admin/groups/" + groupHandlerTestID}) {
		t.Run(e.name, func(t *testing.T) {
			now := time.Now()
			s, _ := newBillingSourceTestServer(t, store.UserRoleOwner, &now)
			repo := &fakeGroupRepository{}
			s.groupRepo = repo
			e.path = strings.ReplaceAll(e.path, groupHandlerTestID, "not-a-uuid")
			r := groupHandlerRequest(t, e)
			addBillingSourceTestSession(t, r)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != 404 || repo.calls != 0 {
				t.Fatalf("invalid ID status=%d calls=%d", w.Code, repo.calls)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		action string
		users  any
	}{
		{"missing users", "add", nil}, {"empty users", "add", []string{}}, {"invalid action", "replace", []string{groupHandlerMemberID}}, {"oversized member selection", "add", make([]string, 5001)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			s, _ := newBillingSourceTestServer(t, store.UserRoleOwner, &now)
			repo := &fakeGroupRepository{}
			s.groupRepo = repo
			e := groupHandlerWriteEndpoints()[2]
			e.body["action"] = tc.action
			e.body["user_ids"] = tc.users
			r := groupHandlerRequest(t, e)
			addBillingSourceTestSession(t, r)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != 400 || repo.calls != 0 {
				t.Fatalf("invalid members status=%d calls=%d", w.Code, repo.calls)
			}
		})
	}
	for _, e := range groupHandlerWriteEndpoints() {
		t.Run(e.name+"/actor override", func(t *testing.T) {
			now := time.Now()
			s, _ := newBillingSourceTestServer(t, store.UserRoleOwner, &now)
			repo := &fakeGroupRepository{}
			s.groupRepo = repo
			e.body["actor_user_id"] = "forged-actor"
			r := groupHandlerRequest(t, e)
			addBillingSourceTestSession(t, r)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != 400 || repo.calls != 0 {
				t.Fatalf("actor override status=%d calls=%d", w.Code, repo.calls)
			}
		})
	}
}

func TestBillingStateResponseIncludesOnlyGroupSummary(t *testing.T) {
	g := store.GroupSummary{ID: groupHandlerTestID, Name: "Research", LimitUSD: "10.000000000000", UsedUSD: "2.000000000000", RemainingUSD: "8.000000000000", Period: "month", MemberCount: 2}
	encoded, err := json.Marshal(billingStateResponse(store.BillingState{UserID: "self", Group: &g}, 10, 0))
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Group map[string]any `json:"group"`
	}
	if err := json.Unmarshal(encoded, &response); err != nil {
		t.Fatal(err)
	}
	if response.Group["id"] != g.ID || response.Group["used_usd"] != g.UsedUSD || response.Group["remaining_usd"] != g.RemainingUSD || response.Group["period"] != "month" {
		t.Fatalf("group summary: %s", encoded)
	}
	for _, key := range []string{"members", "user_ids", "member_usage", "ledger"} {
		if _, ok := response.Group[key]; ok {
			t.Fatalf("personal response exposed %s", key)
		}
	}
	encoded, err = json.Marshal(billingStateResponse(store.BillingState{}, 10, 0))
	if err != nil || !strings.Contains(string(encoded), `"group":null`) {
		t.Fatalf("ungrouped response: %s %v", encoded, err)
	}
}

type groupAdmissionErrorConnector struct {
	err    error
	begins *int
}

func (c groupAdmissionErrorConnector) Connect(context.Context) (driver.Conn, error) {
	return groupAdmissionErrorConn{c}, nil
}
func (groupAdmissionErrorConnector) Driver() driver.Driver { return upstreamAuditDriver{} }

type groupAdmissionErrorConn struct{ groupAdmissionErrorConnector }

func (groupAdmissionErrorConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected statement")
}
func (groupAdmissionErrorConn) Close() error                { return nil }
func (c groupAdmissionErrorConn) Begin() (driver.Tx, error) { *c.begins++; return nil, c.err }

func TestGroupQuotaHTTPResponseHasStableCodeAndRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name            string
		retry           time.Duration
		notStarted      bool
		header, message string
	}{
		{"cap", 1500 * time.Millisecond, false, "2", "群组额度已耗尽"}, {"future", 3500 * time.Millisecond, true, "4", "群组额度周期尚未开始"}, {"minimum retry", 0, false, "1", "群组额度已耗尽"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			begins := 0
			db := sql.OpenDB(groupAdmissionErrorConnector{err: &store.GroupQuotaExceededError{GroupID: groupHandlerTestID, RetryAfter: tc.retry, NotStarted: tc.notStarted}, begins: &begins})
			t.Cleanup(func() { _ = db.Close() })
			s := &Server{store: store.New(db), logger: slog.New(slog.NewTextHandler(io.Discard, nil)), config: config.Config{BodyLimit: 1024, UsagePricing: config.UsagePricing{SchemaVersion: 1, Models: map[string]config.ModelPricing{"gpt-test": {InputUSDPerMillion: "1", CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "0"}}}}}
			r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"test"}`))
			r = r.WithContext(context.WithValue(r.Context(), apiKeyContextKey, store.APIKey{ID: "key", UserID: "user", DeviceID: "device"}))
			w := httptest.NewRecorder()
			httpx.RequestContext(nil)(http.HandlerFunc(s.proxyResponses)).ServeHTTP(w, r)
			var body httpx.ErrorBody
			err := json.Unmarshal(w.Body.Bytes(), &body)
			if err != nil || w.Code != 429 || w.Header().Get("Retry-After") != tc.header || body.Error.Type != "insufficient_quota" || body.Error.Code != "group_quota_exceeded" || !strings.Contains(body.Error.Message, tc.message) || begins != 1 {
				t.Fatalf("status=%d retry=%s body=%s begins=%d", w.Code, w.Header().Get("Retry-After"), w.Body.String(), begins)
			}
			if strings.Contains(w.Body.String(), groupHandlerTestID) {
				t.Fatal("quota response leaked internal group ID")
			}
		})
	}
}
