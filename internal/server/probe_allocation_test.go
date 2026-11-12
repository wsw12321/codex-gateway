package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wsw/codex-gateway/internal/config"
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
)

// A model identification probe's allocation callbacks are answered with its
// pinned account alone, whatever the Owner's own permissions, and never with
// an account CPA did not offer.
func TestProbeAllocationAnswersOnlyThePinnedAccount(t *testing.T) {
	const pinned, other = "0123456789abcdef", "fedcba9876543210"
	ledger := gatewayproxy.NewAllocationLedger()
	identity, release := ledger.PinProbe(pinned)
	server := &Server{config: config.Config{SidecarToken: "sidecar-test-secret"}, allocations: ledger,
		probeAccountLimit: func(_ context.Context, _ *http.Request, id string) (int64, error) {
			if id != pinned {
				t.Errorf("limit read for %s", id)
			}
			return 3, nil
		}}
	call := func(path, body string) (int, string) {
		r := allocationTestRequest(body)
		r.URL.Path = path
		w := httptest.NewRecorder()
		if strings.HasSuffix(path, "/eligible") {
			server.eligibleUpstreamAccounts(w, r)
		} else {
			server.selectUpstreamAccount(w, r)
		}
		return w.Code, strings.TrimSpace(w.Body.String())
	}
	user := `"user_id":"` + identity + `"`
	for _, test := range []struct {
		path, body, want string
		status           int
	}{
		{"/internal/upstream-accounts/eligible", `{"account_ids":["` + other + `","` + pinned + `"],` + user + `}`, `{"accounts":[{"concurrent_limit":3,"id":"` + pinned + `"}]}`, 200},
		{"/internal/upstream-accounts/eligible", `{"account_ids":["` + other + `"],` + user + `}`, `{"accounts":[]}`, 200},
		{"/internal/upstream-accounts/select", `{"account_ids":["` + other + `","` + pinned + `"],` + user + `}`, `{"account_id":"` + pinned + `"}`, 200},
		{"/internal/upstream-accounts/select", `{"account_ids":["` + other + `"],` + user + `}`, "upstream_concurrency_exceeded", 429},
	} {
		status, body := call(test.path, test.body)
		if status != test.status || !strings.Contains(body, test.want) {
			t.Fatalf("%s %s: %d %s", test.path, test.body, status, body)
		}
	}

	// Only the pinned account may be reported for the probe.
	report := func(account string) int {
		r := httptest.NewRequest(http.MethodPost, "/internal/upstream-accounts/attempt", strings.NewReader(
			`{"request_token":"`+strings.Repeat("A", 43)+`",`+user+`,"account_id":"`+account+`"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer sidecar-test-secret")
		w := httptest.NewRecorder()
		server.recordUpstreamAttempt(w, r)
		return w.Code
	}
	if status := report(other); status != http.StatusForbidden {
		t.Fatalf("another account was accepted for a probe: %d", status)
	}
	// The pinned account passes the permission check; this dispatch is not
	// open, so the ledger refuses it as unknown rather than as forbidden.
	if status := report(pinned); status != http.StatusConflict {
		t.Fatalf("pinned account report: %d", status)
	}

	// Once the probe ends, the identity is an ordinary (unknown) user again
	// and never reaches the probe path.
	release()
	if _, ok := ledger.ProbeAccount(identity); ok {
		t.Fatal("the pin outlived its probe")
	}
}
