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
		{"single draining account", 0, false, 429},
		{"database failure", 1, true, 503},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			db := sql.OpenDB(statusTestConnector{conn: &statusTestConn{query: func(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
				calls++
				if !strings.Contains(query, "WITH candidates(id)") || len(args) != 5 || args[3].Value != "0123456789abcdef" || args[2].Value != "00000000-0000-0000-0000-000000000001" || args[4].Value != store.UpstreamProviderCodex {
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
			if test.want == 429 && !strings.Contains(w.Body.String(), "upstream_concurrency_exceeded") {
				t.Fatalf("unavailable account did not return the concurrency error: %s", w.Body.String())
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

func concurrentLimitTestRequest(body string) *http.Request {
	r := statusTestRequest(body)
	r.URL.Path = "/admin/upstream-accounts/0123456789abcdef/concurrent-limit"
	return r
}

func TestUpstreamConcurrentLimitRejectsInvalidRequests(t *testing.T) {
	for _, body := range []string{
		``, `{}`, `null`, `[]`, `{"concurrent_limit":null}`, `{"concurrent_limit":0}`,
		`{"concurrent_limit":-1}`, `{"concurrent_limit":2147483648}`, `{"concurrent_limit":1.1}`,
		`{"concurrent_limit":1.0}`, `{"concurrent_limit":1e1}`, `{"concurrent_limit":"2"}`,
		`{"concurrent_limit":true}`, `{"Concurrent_limit":2}`, `{"concurrent_limit":1,"concurrent_limit":2}`,
		`{"concurrent_limit":2,"token":"sensitive-canary"}`, `{"concurrent_limit":2}{}`,
	} {
		t.Run(body, func(t *testing.T) {
			w := httptest.NewRecorder()
			(&Server{}).setUpstreamAccountConcurrentLimit(w, concurrentLimitTestRequest(body))
			if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "sensitive-canary") {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.Header.Del("Content-Type") },
		func(r *http.Request) { r.URL.RawQuery = "concurrent_limit=2" },
	} {
		r := concurrentLimitTestRequest(`{"concurrent_limit":2}`)
		mutate(r)
		w := httptest.NewRecorder()
		(&Server{}).setUpstreamAccountConcurrentLimit(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("protocol status=%d", w.Code)
		}
	}
	r := concurrentLimitTestRequest(`{"concurrent_limit":2}`)
	r.SetPathValue("id", "INVALID")
	w := httptest.NewRecorder()
	(&Server{}).setUpstreamAccountConcurrentLimit(w, r)
	if w.Code != http.StatusNotFound {
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
					return &upstreamAuditRows{columns: make([]string, 11), values: []driver.Value{"0123456789abcdef", "", "u***@example.com", "plus", "available", now, now, now, weight, int64(1), ""}}, nil
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

func aliasTestRequest(body string) *http.Request {
	r := statusTestRequest(body)
	r.URL.Path = "/admin/upstream-accounts/0123456789abcdef/alias"
	return r
}

func TestUpstreamAliasRejectsInvalidRequests(t *testing.T) {
	for _, body := range []string{
		``, `{}`, `null`, `[]`, `{"alias":null}`, `{"alias":1}`, `{"alias":true}`, `{"Alias":"a"}`,
		`{"alias":"a","alias":"b"}`, `{"alias":"a","token":"sensitive-canary"}`, `{"alias":"a"}{}`,
		`{"alias":"sensitive-canary@example.com"}`, `{"alias":"line\nbreak"}`, `{"alias":"‮evil"}`,
		`{"alias":"` + strings.Repeat("号", 41) + `"}`,
	} {
		t.Run(body, func(t *testing.T) {
			w := httptest.NewRecorder()
			(&Server{}).setUpstreamAccountAlias(w, aliasTestRequest(body))
			if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "sensitive-canary") {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.Header.Del("Content-Type") },
		func(r *http.Request) { r.URL.RawQuery = "alias=a" },
	} {
		r := aliasTestRequest(`{"alias":"a"}`)
		mutate(r)
		w := httptest.NewRecorder()
		(&Server{}).setUpstreamAccountAlias(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("protocol status=%d", w.Code)
		}
	}
	w := httptest.NewRecorder()
	(&Server{}).setUpstreamAccountAlias(w, aliasTestRequest(`{"alias":"`+strings.Repeat("x", 1100)+`"}`))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status=%d", w.Code)
	}
	r := aliasTestRequest(`{"alias":"a"}`)
	r.SetPathValue("id", "INVALID")
	w = httptest.NewRecorder()
	(&Server{}).setUpstreamAccountAlias(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("id status=%d", w.Code)
	}
}

func TestUpstreamAliasSavesNormalizedValueAndHidesDatabaseFailure(t *testing.T) {
	for _, test := range []struct{ body, want string }{
		{`{"alias":"  主力 Plus  "}`, "主力 Plus"},
		{`{"alias":""}`, ""},
	} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/failure=%v", test.body, fail), func(t *testing.T) {
				now := time.Now().UTC()
				var written driver.Value
				db := sql.OpenDB(statusTestConnector{conn: &statusTestConn{query: func(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
					if fail {
						return nil, errors.New("database sensitive-canary")
					}
					if strings.Contains(query, "FOR UPDATE") {
						return &upstreamAuditRows{columns: []string{"alias"}, values: []driver.Value{"旧别名"}}, nil
					}
					if !strings.Contains(query, "SET alias = $2") {
						t.Fatalf("unexpected write query %s %+v", query, args)
					}
					written = args[1].Value
					return &upstreamAuditRows{columns: make([]string, 11), values: []driver.Value{"0123456789abcdef", "", "u***@example.com", "plus", "available", now, now, now, int64(1), int64(1), written}}, nil
				}}})
				defer db.Close()
				server := &Server{store: store.New(db), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
				w := httptest.NewRecorder()
				server.setUpstreamAccountAlias(w, aliasTestRequest(test.body))
				if fail {
					if w.Code != 500 || strings.Contains(w.Body.String(), "sensitive-canary") {
						t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
					}
					return
				}
				var result map[string]any
				if json.Unmarshal(w.Body.Bytes(), &result) != nil || w.Code != 200 || written != test.want ||
					result["id"] != "0123456789abcdef" || result["alias"] != test.want || len(result) != 2 {
					t.Fatalf("status=%d written=%q body=%s", w.Code, written, w.Body.String())
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

func TestUpstreamEligibilityReturnsPerAccountConcurrentLimits(t *testing.T) {
	db := sql.OpenDB(statusTestConnector{conn: &statusTestConn{query: func(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
		if !strings.Contains(query, "COALESCE(a.concurrent_limit, 1)") || len(args) != 3 || args[2].Value != store.UpstreamProviderCodex {
			return nil, fmt.Errorf("unexpected eligibility query: %s %+v", query, args)
		}
		return &upstreamAuditRows{columns: []string{"id", "concurrent_limit"}, values: []driver.Value{"0123456789abcdef", int64(3)}}, nil
	}}})
	defer db.Close()
	server := &Server{config: config.Config{SidecarToken: "sidecar-test-secret"}, store: store.New(db)}
	w := httptest.NewRecorder()
	server.eligibleUpstreamAccounts(w, allocationTestRequest(`{"account_ids":["0123456789abcdef"],"user_id":"00000000-0000-0000-0000-000000000001"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var result struct {
		Accounts []struct {
			ID              string `json:"id"`
			ConcurrentLimit int    `json:"concurrent_limit"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Accounts) != 1 || result.Accounts[0].ID != "0123456789abcdef" || result.Accounts[0].ConcurrentLimit != 3 {
		t.Fatalf("eligibility response=%s", w.Body.String())
	}
}

func TestGroupUpstreamAllocationStaysWithinCurrentCandidates(t *testing.T) {
	const first = "0123456789abcdef"
	const second = "fedcba9876543210"
	const outside = "aaaabbbbccccdddd"
	for _, provider := range []string{store.UpstreamProviderCodex, store.UpstreamProviderAntigravity, store.UpstreamProviderAnthropic} {
		for _, test := range []struct {
			name       string
			groups     [][]string
			candidates any
			omitIDs    bool
			denied     string
			zeroWeight string
			wantID     string
			queries    []string
		}{
			{name: "empty group", groups: [][]string{{}}, candidates: []string{outside}},
			{name: "null group", groups: [][]string{nil}, candidates: []string{outside}},
			{name: "user cannot use group account", groups: [][]string{{first}}, candidates: []string{first, outside}, denied: first, queries: []string{"eligible:" + first}},
			{name: "group account draining", groups: [][]string{{first}}, candidates: []string{first, outside}, zeroWeight: first, queries: []string{"eligible:" + first, "select:" + first}},
			{name: "group account missing from live candidates", groups: [][]string{{first}}, candidates: []string{outside}},
			{name: "group and live candidate intersection", groups: [][]string{{first, second}}, candidates: []string{second, outside}, wantID: second, queries: []string{"eligible:" + second, "select:" + second}},
			{name: "authorized selection", groups: [][]string{{first}}, candidates: []string{first, outside}, wantID: first, queries: []string{"eligible:" + first, "select:" + first}},
			{name: "groups only protocol", groups: [][]string{{first}}, omitIDs: true, wantID: first, queries: []string{"eligible:" + first, "select:" + first}},
			{name: "explicit empty live candidates", groups: [][]string{{first}}, candidates: []string{}},
			{name: "explicit null live candidates", groups: [][]string{{first}}, candidates: nil},
			{name: "next authorized group", groups: [][]string{{first}, {second}}, candidates: []string{first, second, outside}, denied: first, wantID: second, queries: []string{"eligible:" + first, "eligible:" + second, "select:" + second}},
		} {
			t.Run(provider+"/"+test.name, func(t *testing.T) {
				var queries []string
				s := groupAllocationTestServer(t, provider, test.denied, test.zeroWeight, &queries)
				body := groupAllocationTestBody(t, test.groups, test.candidates, test.omitIDs)
				r := allocationTestRequest(body)
				r.URL.Path = groupAllocationTestPath(provider, "select")
				w := httptest.NewRecorder()
				s.selectUpstreamAccount(w, r)
				if test.wantID == "" {
					if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "upstream_concurrency_exceeded") {
						t.Fatalf("unavailable group selected an account: status=%d body=%s", w.Code, w.Body)
					}
				} else if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"account_id":"`+test.wantID+`"}` {
					t.Fatalf("selection status=%d body=%s; want %s", w.Code, w.Body, test.wantID)
				}
				if !reflect.DeepEqual(queries, test.queries) {
					t.Fatalf("candidate queries=%v; want %v", queries, test.queries)
				}
			})
		}
	}
}

func TestGroupUpstreamEligibilityStaysWithinCurrentCandidates(t *testing.T) {
	const first = "0123456789abcdef"
	const second = "fedcba9876543210"
	const outside = "aaaabbbbccccdddd"
	for _, provider := range []string{store.UpstreamProviderCodex, store.UpstreamProviderAntigravity, store.UpstreamProviderAnthropic} {
		for _, test := range []struct {
			name       string
			groups     [][]string
			candidates any
			omitIDs    bool
			denied     string
			wantIDs    [][]string
			queries    []string
		}{
			{name: "empty group", groups: [][]string{{}}, candidates: []string{outside}, wantIDs: [][]string{{}}},
			{name: "null group", groups: [][]string{nil}, candidates: []string{outside}, wantIDs: [][]string{{}}},
			{name: "disjoint candidates", groups: [][]string{{first}}, candidates: []string{outside}, wantIDs: [][]string{{}}},
			{name: "intersection", groups: [][]string{{first, second}}, candidates: []string{second, outside}, wantIDs: [][]string{{second}}, queries: []string{"eligible:" + second}},
			{name: "user denied", groups: [][]string{{first}}, candidates: []string{first, outside}, denied: first, wantIDs: [][]string{{}}, queries: []string{"eligible:" + first}},
			{name: "groups only protocol", groups: [][]string{{first}}, omitIDs: true, wantIDs: [][]string{{first}}, queries: []string{"eligible:" + first}},
			{name: "explicit empty candidates", groups: [][]string{{first}}, candidates: []string{}, wantIDs: [][]string{{}}},
			{name: "explicit null candidates", groups: [][]string{{first}}, candidates: nil, wantIDs: [][]string{{}}},
			{name: "each group intersects candidates", groups: [][]string{{first}, {second}}, candidates: []string{second, outside}, wantIDs: [][]string{{}, {second}}, queries: []string{"eligible:" + second}},
		} {
			t.Run(provider+"/"+test.name, func(t *testing.T) {
				var queries []string
				s := groupAllocationTestServer(t, provider, test.denied, "", &queries)
				r := allocationTestRequest(groupAllocationTestBody(t, test.groups, test.candidates, test.omitIDs))
				r.URL.Path = groupAllocationTestPath(provider, "eligible")
				w := httptest.NewRecorder()
				s.eligibleUpstreamAccounts(w, r)
				var result struct {
					Groups []struct {
						GroupID  string                `json:"group_id"`
						Accounts []struct{ ID string } `json:"accounts"`
					} `json:"groups"`
				}
				if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Groups) != len(test.groups) {
					t.Fatalf("eligibility status=%d body=%s", w.Code, w.Body)
				}
				for i, group := range result.Groups {
					gotIDs := make([]string, 0, len(group.Accounts))
					for _, account := range group.Accounts {
						gotIDs = append(gotIDs, account.ID)
					}
					if group.GroupID != fmt.Sprintf("00000000-0000-0000-0000-%012d", i+2) || group.Accounts == nil || !reflect.DeepEqual(gotIDs, test.wantIDs[i]) {
						t.Fatalf("group %d eligibility=%+v; want %v", i, group, test.wantIDs[i])
					}
				}
				if !reflect.DeepEqual(queries, test.queries) {
					t.Fatalf("candidate queries=%v; want %v", queries, test.queries)
				}
			})
		}
	}
}

func groupAllocationTestBody(t *testing.T, pools [][]string, candidates any, omitIDs bool) string {
	t.Helper()
	groups := make([]map[string]any, 0, len(pools))
	for i, ids := range pools {
		groups = append(groups, map[string]any{"group_id": fmt.Sprintf("00000000-0000-0000-0000-%012d", i+2), "account_ids": ids})
	}
	request := map[string]any{"user_id": "00000000-0000-0000-0000-000000000001", "groups": groups}
	if !omitIDs {
		request["account_ids"] = candidates
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func groupAllocationTestPath(provider, action string) string {
	namespace := "upstream-accounts"
	if provider != store.UpstreamProviderCodex {
		namespace = provider + "-accounts"
	}
	return "/internal/" + namespace + "/" + action
}

func groupAllocationTestServer(t *testing.T, provider, denied, zeroWeight string, queries *[]string) *Server {
	t.Helper()
	db := sql.OpenDB(statusTestConnector{conn: &statusTestConn{query: func(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
		if args[len(args)-1].Value != provider {
			t.Fatalf("provider=%v; want %s", args[len(args)-1].Value, provider)
		}
		selection := strings.Contains(query, "COALESCE(a.allocation_weight, 1)")
		start, label := 1, "eligible:"
		columns := []string{"id", "concurrent_limit"}
		if selection {
			start, label = 3, "select:"
			columns = []string{"id", "weight", "cost"}
		}
		var ids []string
		var rows [][]driver.Value
		for _, arg := range args[start : len(args)-1] {
			id := arg.Value.(string)
			ids = append(ids, id)
			if id == denied {
				continue
			}
			if selection {
				weight := int64(1)
				if id == zeroWeight {
					weight = 0
				}
				rows = append(rows, []driver.Value{id, weight, "0"})
			} else {
				rows = append(rows, []driver.Value{id, int64(3)})
			}
		}
		*queries = append(*queries, label+strings.Join(ids, ","))
		return &responsesWebSocketTestRows{columns: columns, values: rows}, nil
	}}})
	t.Cleanup(func() { _ = db.Close() })
	return &Server{store: store.New(db), config: config.Config{SidecarToken: "sidecar-test-secret", AntigravityBridgeToken: "sidecar-test-secret"}}
}
