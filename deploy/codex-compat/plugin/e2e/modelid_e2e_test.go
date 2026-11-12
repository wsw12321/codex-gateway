//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestModelIdentification checks the plugin's side of model identification
// against a real CPA process: the protocol, the models CPA registered for one
// credential, and that a probe is an ordinary non-streaming Responses request
// which CPA serves on the one account the Gateway pins and never moves to
// another when that account fails.
func TestModelIdentification(t *testing.T) {
	h := newHarness(t)
	a, b := h.addAccount("a", nil), h.addAccount("b", nil)
	h.start()
	base := "/v0/management/gateway/model-identification"

	status, body := h.management(http.MethodGet, base+"/capabilities", managementKey, "")
	if status != http.StatusOK || body != `{"protocol":"model_identification_cpa_v1"}` {
		t.Fatalf("capabilities: %d %s", status, body)
	}
	status, body = h.management(http.MethodGet, base+"/accounts/models?id="+a.id, managementKey, "")
	var listing struct {
		AccountID string   `json:"account_id"`
		Models    []string `json:"models"`
	}
	if status != http.StatusOK || json.Unmarshal([]byte(body), &listing) != nil || listing.AccountID != a.id || !contains(listing.Models, model) {
		t.Fatalf("models: %d %s", status, body)
	}
	if status, _ = h.management(http.MethodGet, base+"/accounts/models?id=0123456789abcdef", managementKey, ""); status != http.StatusNotFound {
		t.Fatalf("unknown account: %d", status)
	}
	if status, _ = h.management(http.MethodGet, base+"/capabilities", internalKey, ""); status != http.StatusUnauthorized {
		t.Fatalf("the data-plane key reached the route: %d", status)
	}

	// The Gateway answers a probe identity's callbacks with the pinned
	// account alone; the fake Gateway does the same through its permissions.
	const probeUser = "33333333-3333-4333-8333-333333333333"
	h.gateway.mu.Lock()
	h.gateway.permitted[probeUser] = map[string]bool{a.id: true}
	h.gateway.mu.Unlock()
	probe := call{user: probeUser, body: `{"model":"` + model + `","input":"which model are you?","max_output_tokens":16384,"stream":false}`}

	h.resetObservations()
	got := h.do(probe)
	var answer struct {
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if got.status != http.StatusOK || json.Unmarshal([]byte(got.body), &answer) != nil || answer.Status != "completed" ||
		!strings.Contains(got.body, `"output_text"`) || !strings.Contains(got.body, "hello") {
		t.Fatalf("probe: %d %s", got.status, got.body)
	}
	if served := h.served(); len(served) != 1 || served[0] != "a" {
		t.Fatalf("the probe was served by %v", served)
	}
	if reports := h.gateway.reportsFor(got.token); len(reports) == 0 || reports[len(reports)-1].AccountID != a.id || reports[len(reports)-1].UserID != probeUser {
		t.Fatalf("attribution %+v", reports)
	}

	// The pinned account fails: the probe fails, b is never tried.
	h.provider.set(a.token, "e500")
	h.resetObservations()
	if got = h.do(probe); got.status < 500 {
		t.Fatalf("a failing pinned account answered %d: %s", got.status, got.body)
	}
	for _, name := range h.served() {
		if name != "a" {
			t.Fatalf("the probe moved to %s", name)
		}
	}
	_ = b
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
