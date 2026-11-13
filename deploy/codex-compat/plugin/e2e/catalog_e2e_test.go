//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"
)

// reviewedAntigravityModels is the Gateway's Antigravity catalog: the only
// models it prices, authorizes and attributes to an Antigravity account.
var reviewedAntigravityModels = []string{
	"gemini-3-flash", "gemini-3.1-flash-lite", "gemini-3.5-flash-lite", "gemini-3.8-flash-high", "gemini-4-argon-high",
}

// TestAntigravityModelCatalog checks that the oauth-excluded-models rule of
// the deployment confines an Antigravity account to the reviewed models. The
// catalog is the one this CPA binary embeds, so a CPA upgrade that adds an
// Antigravity model fails here until the model is reviewed or excluded.
func TestAntigravityModelCatalog(t *testing.T) {
	h := newHarness(t)
	h.addAccount("a", nil)
	c := h.addClaudeAccount("c", "0c1a0de000000001")
	h.start()

	// Everything an Antigravity account could register: the provider's static
	// catalog, narrowed by what Google reports for the account.
	status, body := h.management(http.MethodGet, "/v0/management/model-definitions/antigravity", managementKey, "")
	catalog := modelIDs(t, body)
	if status != http.StatusOK || len(catalog) == 0 {
		t.Fatalf("static catalog: %d %s", status, body)
	}
	for _, reviewed := range reviewedAntigravityModels {
		if !contains(catalog, reviewed) {
			t.Fatalf("CPA no longer serves the reviewed model %s: %v", reviewed, catalog)
		}
	}
	// Without another provider's model in the catalog the rule would be
	// untested; the shared one is also in CPA's Claude catalog.
	const shared = "claude-sonnet-4-6"
	if !contains(catalog, shared) || len(catalog) <= len(reviewedAntigravityModels) {
		t.Fatalf("the catalog has nothing to exclude: %v", catalog)
	}

	// Google reports the whole catalog for the account, and identifiers the
	// Gateway retired or that belong to other providers.
	h.stop()
	g := h.addAntigravityAccount("g", "0a71a61a00000001")
	h.provider.mu.Lock()
	h.provider.discovered = append(append([]string{}, catalog...),
		"gemini-pro-agent", "gemini-3.1-pro-low", "gemini-3.6-flash-high", "gemini-3.7-flash", "gemini-4-argon", "claude-opus-5-5", model)
	h.provider.mu.Unlock()
	h.start()

	listing := "/v0/management/auth-files/models?name=" + h.files["g"]
	var registered []string
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if status, body = h.management(http.MethodGet, listing, managementKey, ""); status == http.StatusOK {
			if registered = modelIDs(t, body); len(registered) > 0 {
				break
			}
		}
	}
	sort.Strings(registered)
	if strings.Join(registered, ",") != strings.Join(reviewedAntigravityModels, ",") {
		t.Fatalf("the Antigravity account registered %v, want %v", registered, reviewedAntigravityModels)
	}

	// CPA discovered the models itself, with the account's token, through the
	// route written into the credential.
	discoveries := 0
	for _, seen := range h.provider.seen() {
		if seen.Host != antigravityHost {
			continue
		}
		discoveries++
		if seen.Via != "global" || seen.Bearer != g.token || seen.Path != "/v1internal:fetchAvailableModels" {
			t.Fatalf("model discovery: %+v", seen)
		}
	}
	if discoveries == 0 {
		t.Fatal("CPA did not discover the account's models")
	}

	// The data plane lists the reviewed models and none of the excluded ones
	// that no other provider serves.
	status, body = h.management(http.MethodGet, "/v1/models", internalKey, "")
	listed := modelIDs(t, body)
	if status != http.StatusOK {
		t.Fatalf("model listing: %d %s", status, body)
	}
	for _, reviewed := range reviewedAntigravityModels {
		if !contains(listed, reviewed) {
			t.Fatalf("%s is not listed: %v", reviewed, listed)
		}
	}
	status, body = h.management(http.MethodGet, "/v0/management/auth-files/models?name="+h.files["c"], managementKey, "")
	claude := modelIDs(t, body)
	if status != http.StatusOK || !contains(claude, shared) {
		t.Fatalf("the Claude account's models: %d %s", status, body)
	}
	for _, id := range catalog {
		if !contains(reviewedAntigravityModels, id) && !contains(claude, id) && contains(listed, id) {
			t.Fatalf("the excluded model %s is listed", id)
		}
	}

	// A Claude model the Antigravity catalog also names has only Claude
	// candidates: the Claude account serves it through the Anthropic callbacks.
	h.resetObservations()
	r := h.do(call{path: "/v1/messages", body: `{"model":"` + shared + `","max_tokens":16,"stream":false,"messages":[{"role":"user","content":"hi"}]}`})
	if r.status != http.StatusOK {
		t.Fatalf("shared Claude model: %d %s", r.status, r.body)
	}
	for _, seen := range h.provider.seen() {
		if seen.Host == antigravityHost {
			continue
		}
		if seen.Host != "api.anthropic.com" || seen.Bearer != c.token {
			t.Fatalf("the shared Claude model reached %+v", seen)
		}
	}
	h.gateway.mu.Lock()
	actions, namespaces := append([]string(nil), h.gateway.calls...), append([]string(nil), h.gateway.namespaces...)
	h.gateway.mu.Unlock()
	for i, namespace := range namespaces {
		// Routes are resolved through one endpoint for every provider.
		if actions[i] != "resolve-egress" && namespace != "anthropic-accounts" {
			t.Fatalf("allocation used the %s callbacks", namespace)
		}
	}
	if reports := h.gateway.reportsFor(r.token); len(reports) == 0 || reports[len(reports)-1].AccountID != c.id {
		t.Fatalf("attribution %+v", reports)
	}
}

// modelIDs decodes the identifiers of a model listing: {"models":[{"id":..}]}
// from the management API or {"data":[{"id":..}]} from the data plane.
func modelIDs(t *testing.T, body string) []string {
	t.Helper()
	var listing struct {
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(body), &listing) != nil {
		return nil
	}
	ids := make([]string, 0, len(listing.Models)+len(listing.Data))
	for _, entry := range append(listing.Models, listing.Data...) {
		ids = append(ids, entry.ID)
	}
	return ids
}
