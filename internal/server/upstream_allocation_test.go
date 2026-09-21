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
	"strings"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/store"
)

func allocationTestRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/internal/upstream-accounts/select", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer sidecar-test-secret")
	return r
}

func TestUpstreamAllocationAuthenticatesBeforeReadingCandidates(t *testing.T) {
	for _, authorization := range [][]string{nil, {""}, {"Bearer wrong"}, {"sidecar-test-secret"}, {"Bearer sidecar-test-secret", "Bearer sidecar-test-secret"}} {
		server := &Server{config: config.Config{SidecarToken: "sidecar-test-secret"}, mux: http.NewServeMux()}
		server.routes()
		r := allocationTestRequest(`{"account_ids":["0123456789abcdef"]}`)
		r.Header["Authorization"] = authorization
		w := httptest.NewRecorder()
		server.mux.ServeHTTP(w, r)
		if w.Code != 401 || !strings.Contains(w.Body.String(), "invalid_sidecar_token") || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	(&Server{}).selectUpstreamAccount(w, allocationTestRequest(`{}`))
	if w.Code != 401 {
		t.Fatalf("unconfigured secret status=%d", w.Code)
	}
}

func TestUpstreamAllocationRejectsNoncanonicalRequests(t *testing.T) {
	server := &Server{config: config.Config{SidecarToken: "sidecar-test-secret"}}
	tooMany := make([]string, store.MaxUpstreamAllocationCandidates+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("%016x", i)
	}
	tooManyJSON, _ := json.Marshal(map[string]any{"account_ids": tooMany, "user_id": "00000000-0000-0000-0000-000000000001"})
	for _, body := range []string{
		``, `{}`, `null`, `[]`, `{"user_id":"00000000-0000-0000-0000-000000000001","account_ids":null}`, `{"user_id":"00000000-0000-0000-0000-000000000001","account_ids":[]}`,
		`{"user_id":"00000000-0000-0000-0000-000000000001","account_ids":[""]}`, `{"user_id":"00000000-0000-0000-0000-000000000001","account_ids":[null]}`, `{"user_id":"00000000-0000-0000-0000-000000000001","account_ids":[1]}`,
		`{"user_id":"00000000-0000-0000-0000-000000000001","account_ids":["0123456789ABCDEF"]}`, `{"user_id":"00000000-0000-0000-0000-000000000001","account_ids":[" 0123456789abcdef"]}`,
		`{"user_id":"00000000-0000-0000-0000-000000000001","account_ids":["0123456789abcdef","0123456789abcdef"]}`,
		`{"Account_ids":["0123456789abcdef"]}`, `{"user_id":"00000000-0000-0000-0000-000000000001","account_ids":["0123456789abcdef"],"account_ids":[]}`,
		`{"user_id":"00000000-0000-0000-0000-000000000001","account_ids":["0123456789abcdef"],"token":"sensitive-canary"}`,
		`{"user_id":"00000000-0000-0000-0000-000000000001","account_ids":["0123456789abcdef"]}{}`, string(tooManyJSON),
	} {
		t.Run(body, func(t *testing.T) {
			w := httptest.NewRecorder()
			server.selectUpstreamAccount(w, allocationTestRequest(body))
			if w.Code != 400 || strings.Contains(w.Body.String(), "sensitive-canary") {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	for _, length := range []int64{-1, upstreamAllocationRequestBytes + 1} {
		r := allocationTestRequest(strings.Repeat(" ", upstreamAllocationRequestBytes+1))
		r.ContentLength = length
		w := httptest.NewRecorder()
		server.selectUpstreamAccount(w, r)
		if w.Code != 413 {
			t.Fatalf("oversize status=%d", w.Code)
		}
	}
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.Header.Del("Content-Type") },
		func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
		func(r *http.Request) { r.URL.RawQuery = "account_ids=sensitive-canary" },
	} {
		r := allocationTestRequest(`{"user_id":"00000000-0000-0000-0000-000000000001","account_ids":["0123456789abcdef"]}`)
		mutate(r)
		w := httptest.NewRecorder()
		server.selectUpstreamAccount(w, r)
		if w.Code != 400 {
			t.Fatalf("protocol status=%d", w.Code)
		}
	}
}

func TestUpstreamAllocationUsesOnlyDatabaseAndFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		weight int64
		fail   bool
		want   int
	}{
		{"new account default", 1, false, 200},
		{"single draining account", 0, false, 503},
		{"database failure", 1, true, 503},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			db := sql.OpenDB(statusTestConnector{conn: &statusTestConn{query: func(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
				calls++
				if !strings.Contains(query, "WITH candidates(id)") || len(args) != 4 || args[3].Value != "0123456789abcdef" || args[2].Value != "00000000-0000-0000-0000-000000000001" {
					t.Fatalf("unexpected allocation query: %s %+v", query, args)
				}
				if args[1].Value.(time.Time).Sub(args[0].Value.(time.Time)) != 24*time.Hour {
					t.Fatal("allocation query did not use rolling 24 hours")
				}
				if test.fail {
					return nil, errors.New("database sensitive-canary")
				}
				return &upstreamAuditRows{columns: []string{"id", "weight", "cost"}, values: []driver.Value{"0123456789abcdef", test.weight, "0"}}, nil
			}}})
			defer db.Close()
			server := &Server{config: config.Config{SidecarToken: "sidecar-test-secret"}, store: store.New(db)}
			w := httptest.NewRecorder()
			server.selectUpstreamAccount(w, allocationTestRequest(`{"account_ids":["0123456789abcdef"],"user_id":"00000000-0000-0000-0000-000000000001"}`))
			if w.Code != test.want || calls != 1 || strings.Contains(w.Body.String(), "sensitive-canary") {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
			}
			if test.want == 200 && strings.TrimSpace(w.Body.String()) != `{"account_id":"0123456789abcdef"}` {
				t.Fatalf("selection=%s", w.Body.String())
			}
		})
	}
}

func weightTestRequest(body string) *http.Request {
	r := statusTestRequest(body)
	r.URL.Path = "/admin/upstream-accounts/0123456789abcdef/allocation-weight"
	return r
}

func TestUpstreamWeightRejectsInvalidRequests(t *testing.T) {
	for _, body := range []string{
		``, `{}`, `null`, `[]`, `{"weight":null}`, `{"weight":-1}`, `{"weight":2147483648}`,
		`{"weight":1.1}`, `{"weight":1.0}`, `{"weight":1e1}`, `{"weight":"20"}`, `{"weight":true}`,
		`{"Weight":20}`, `{"weight":0,"weight":20}`, `{"weight":0,"\u0077eight":20}`,
		`{"weight":20,"token":"sensitive-canary"}`, `{"weight":20}{}`,
	} {
		t.Run(body, func(t *testing.T) {
			w := httptest.NewRecorder()
			(&Server{}).setUpstreamAccountAllocationWeight(w, weightTestRequest(body))
			if w.Code != 400 || strings.Contains(w.Body.String(), "sensitive-canary") {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	for _, length := range []int64{-1, upstreamWeightRequestBytes + 1} {
		r := weightTestRequest(strings.Repeat(" ", upstreamWeightRequestBytes+1))
		r.ContentLength = length
		w := httptest.NewRecorder()
		(&Server{}).setUpstreamAccountAllocationWeight(w, r)
		if w.Code != 413 {
			t.Fatalf("oversize status=%d", w.Code)
		}
	}
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.Header.Del("Content-Type") },
		func(r *http.Request) { r.URL.RawQuery = "weight=20" },
	} {
		r := weightTestRequest(`{"weight":20}`)
		mutate(r)
		w := httptest.NewRecorder()
		(&Server{}).setUpstreamAccountAllocationWeight(w, r)
		if w.Code != 400 {
			t.Fatalf("protocol status=%d", w.Code)
		}
	}
	r := weightTestRequest(`{"weight":20}`)
	r.SetPathValue("id", "INVALID")
	w := httptest.NewRecorder()
	(&Server{}).setUpstreamAccountAllocationWeight(w, r)
	if w.Code != 404 {
		t.Fatalf("id status=%d", w.Code)
	}
}

func TestUpstreamWeightReturnsConfirmedValueAndDatabaseFailure(t *testing.T) {
	for _, weight := range []int64{0, 20, store.MaxUpstreamAllocationWeight} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/failure=%v", weight, fail), func(t *testing.T) {
				now := time.Now().UTC()
				db := sql.OpenDB(statusTestConnector{conn: &statusTestConn{query: func(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
					if fail {
						return nil, errors.New("database sensitive-canary")
					}
					if strings.Contains(query, "FOR UPDATE") {
						return &upstreamAuditRows{columns: []string{"allocation_weight"}, values: []driver.Value{int64(1)}}, nil
					}
					if !strings.Contains(query, "UPDATE upstream_accounts") || args[1].Value != weight {
						t.Fatalf("unexpected write query %s %+v", query, args)
					}
					return &upstreamAuditRows{columns: make([]string, 8), values: []driver.Value{"0123456789abcdef", "u***@example.com", "plus", "available", now, now, now, weight}}, nil
				}}})
				defer db.Close()
				server := &Server{store: store.New(db), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
				w := httptest.NewRecorder()
				server.setUpstreamAccountAllocationWeight(w, weightTestRequest(fmt.Sprintf(`{"weight":%d}`, weight)))
				if fail {
					if w.Code != 500 || strings.Contains(w.Body.String(), "sensitive-canary") {
						t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
					}
				} else {
					var result map[string]any
					if json.Unmarshal(w.Body.Bytes(), &result) != nil || w.Code != 200 || result["id"] != "0123456789abcdef" || result["allocation_weight"] != float64(weight) || len(result) != 2 {
						t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
					}
				}
			})
		}
	}
}

func TestUpstreamEligibilityRequiresAuthenticatedCanonicalUser(t *testing.T) {
	server := &Server{config: config.Config{SidecarToken: "sidecar-test-secret"}}
	for _, body := range []string{
		`{"account_ids":["0123456789abcdef"]}`,
		`{"account_ids":["0123456789abcdef"],"user_id":null}`,
		`{"account_ids":["0123456789abcdef"],"user_id":"bad-user"}`,
		`{"account_ids":["0123456789abcdef"],"user_id":"00000000-0000-0000-0000-000000000001","user_id":"00000000-0000-0000-0000-000000000002"}`,
	} {
		for _, handler := range []http.HandlerFunc{server.selectUpstreamAccount, server.eligibleUpstreamAccounts} {
			w := httptest.NewRecorder()
			handler(w, allocationTestRequest(body))
			if w.Code != 400 {
				t.Fatalf("invalid identity accepted: %d %s", w.Code, w.Body.String())
			}
		}
	}
	request := allocationTestRequest(`{}`)
	request.Header.Del("Authorization")
	w := httptest.NewRecorder()
	server.eligibleUpstreamAccounts(w, request)
	if w.Code != 401 {
		t.Fatal("eligibility endpoint accepted unauthenticated request")
	}
}
