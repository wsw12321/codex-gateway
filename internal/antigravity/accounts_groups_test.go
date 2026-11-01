package antigravity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const accountGroupID = "22222222-2222-4222-8222-222222222222"

func accountGroupHeader(t *testing.T, ids ...string) string {
	t.Helper()
	encoded, err := json.Marshal([]allocationGroup{{GroupID: accountGroupID, AccountIDs: ids}})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func groupManagedRequest(server http.Handler, groups []string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(accountRequestBody))
	r.Header.Set("Authorization", "Bearer "+testBridgeToken)
	r.Header.Set("X-Codex-Gateway-User", accountUserID)
	r.Header["X-Codex-Gateway-Groups"] = groups
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	return w
}

func testGroupSelector(t *testing.T) *httptest.Server {
	t.Helper()
	selector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			IDs    []string          `json:"account_ids"`
			Groups []allocationGroup `json:"groups"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "invalid request", 400)
			return
		}
		rows := func(ids []string) []map[string]any {
			accounts := []map[string]any{}
			for _, id := range ids {
				if !containsString(request.IDs, id) {
					t.Errorf("group callback includes an unavailable attempt candidate: %s", id)
				}
				accounts = append(accounts, map[string]any{"id": id, "concurrent_limit": 2})
			}
			return accounts
		}
		if strings.HasSuffix(r.URL.Path, "/eligible") {
			if len(request.Groups) == 0 {
				writeJSON(w, 200, map[string]any{"accounts": rows(request.IDs)})
				return
			}
			groups := []map[string]any{}
			for _, group := range request.Groups {
				groups = append(groups, map[string]any{"group_id": group.GroupID, "accounts": rows(group.AccountIDs)})
			}
			writeJSON(w, 200, map[string]any{"groups": groups})
			return
		}
		if len(request.IDs) == 0 {
			t.Error("empty selection candidates")
			writeFailure(w, accountBusyFailure())
			return
		}
		writeJSON(w, 200, map[string]string{"account_id": request.IDs[0]})
	}))
	t.Cleanup(selector.Close)
	return selector
}

func TestAccountGroupHeaderFailsClosed(t *testing.T) {
	valid := accountGroupHeader(t, accountID("default"))
	for name, values := range map[string][]string{
		"empty":            {""},
		"oversized":        {strings.Repeat(" ", 8193)},
		"duplicate header": {valid, valid},
		"null":             {"null"},
		"empty layers":     {"[]"},
		"wrong fields":     {`[{"Group_id":"` + accountGroupID + `","Account_ids":[]}]`},
		"missing accounts": {`[{"group_id":"` + accountGroupID + `"}]`},
		"duplicate field":  {`[{"group_id":"` + accountGroupID + `","account_ids":[],"account_ids":[]}]`},
		"invalid account":  {accountGroupHeader(t, "INVALID")},
		"repeated account": {accountGroupHeader(t, accountID("default"), accountID("default"))},
	} {
		t.Run(name, func(t *testing.T) {
			executor := &accountExecutor{}
			selector := testGroupSelector(t)
			server := accountServer(t, selector.URL, executor)
			response := groupManagedRequest(server, values)
			if response.Code != 503 || executor.runs.Load() != 0 {
				t.Fatalf("invalid group scope executed: status=%d runs=%d", response.Code, executor.runs.Load())
			}
		})
	}
}

func TestAccountGroupPoolRestrictsRetry(t *testing.T) {
	for _, includeBackup := range []bool{false, true} {
		t.Run(map[bool]string{false: "no global fallback", true: "retry within pool"}[includeBackup], func(t *testing.T) {
			selector := testGroupSelector(t)
			first := &accountExecutor{failure: &Failure{429, "synthetic_failure", "Provider failure"}}
			second := &accountExecutor{}
			server := accountServer(t, selector.URL, first, second)
			ids := []string{accountID("default")}
			if includeBackup {
				ids = append(ids, accountID("work"))
			}
			response := groupManagedRequest(server, []string{accountGroupHeader(t, ids...)})
			wantStatus, wantSecond := 429, int64(0)
			if includeBackup {
				wantStatus, wantSecond = 200, 1
			}
			if response.Code != wantStatus || first.runs.Load() != 1 || second.runs.Load() != wantSecond {
				t.Fatalf("status=%d first=%d second=%d body=%s", response.Code, first.runs.Load(), second.runs.Load(), response.Body)
			}
		})
	}
	selector := testGroupSelector(t)
	executor := &accountExecutor{}
	server := accountServer(t, selector.URL, executor)
	response := groupManagedRequest(server, []string{accountGroupHeader(t)})
	if response.Code != 429 || executor.runs.Load() != 0 {
		t.Fatalf("empty group scope executed: status=%d runs=%d", response.Code, executor.runs.Load())
	}
}

func TestAccountGroupPoolRechecksActiveConversation(t *testing.T) {
	selector := testGroupSelector(t)
	first, second := newConversationAccountExecutor(), &accountExecutor{}
	server := accountServer(t, selector.URL, first, second)
	scope := strings.Repeat("a", 43)
	active := startAccountConversation(t, server, first, accountConversationA, scope, false)
	r := newAccountConversationRequest(context.Background(), accountConversationA, scope, false)
	r.Header.Set("X-Codex-Gateway-Groups", accountGroupHeader(t, accountID("work")))
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("X-Codex-Upstream-Account") != accountID("work") || second.runs.Load() != 1 {
		t.Fatalf("active conversation escaped new group pool: status=%d body=%s", w.Code, w.Body)
	}
	active.finish <- nil
	if response := active.wait(t); response.Code != 200 {
		t.Fatalf("original accepted request failed: status=%d body=%s", response.Code, response.Body)
	}
}

func TestAccountGroupEligibilityRejectsUnscopedOrForeignAccounts(t *testing.T) {
	for _, reply := range []string{
		`{"accounts":[{"id":"` + accountID("default") + `","concurrent_limit":2}]}`,
		`{"groups":[{"group_id":"` + accountGroupID + `","accounts":[{"id":"` + accountID("work") + `","concurrent_limit":2}]}]}`,
	} {
		t.Run(reply, func(t *testing.T) {
			selector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(reply))
			}))
			defer selector.Close()
			first, second := &accountExecutor{}, &accountExecutor{}
			server := accountServer(t, selector.URL, first, second)
			response := groupManagedRequest(server, []string{accountGroupHeader(t, accountID("default"))})
			if response.Code != 503 || first.runs.Load() != 0 || second.runs.Load() != 0 {
				t.Fatalf("invalid scope response accepted: status=%d first=%d second=%d", response.Code, first.runs.Load(), second.runs.Load())
			}
		})
	}
}
