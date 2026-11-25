package gatewayplugin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// testSelfLayer is the Gateway's self-use layer ID.
const testSelfLayer = "00000000-0000-4000-8000-000000000002"

// A member's account is eligible only in the layer that pays for it: here a
// is the user's own (self-use) account, which the personal layer must never
// offer, and b and c are platform accounts.
func TestLayerEligibilityKeepsMemberAccountsInTheirLayer(t *testing.T) {
	f := newFixture(t, 3)
	a, b, c := f.ids["a"], f.ids["b"], f.ids["c"]
	f.alloc.layerPermitted = map[string]map[string]bool{testSelfLayer: {a: true}}
	f.alloc.permitted = map[string]map[string]bool{testUser: {b: true, c: true}}
	f.alloc.prefer = []string{b}
	groups := groupsHeader(groupLayer(testSelfLayer, a), personalLayer)
	if name := f.mustPick(pickOptions{groups: groups}); name != "a" {
		t.Fatalf("picked %q, want the own account a", name)
	}
	if len(f.alloc.layerCalls) != 1 || len(f.alloc.layerCalls[0]) != 2 {
		t.Fatalf("layered eligibility calls = %+v", f.alloc.layerCalls)
	}
	if call := f.alloc.layerCalls[0]; call[0].GroupID != testSelfLayer || len(call[0].AccountIDs) != 1 || call[0].AccountIDs[0] != a ||
		call[1].GroupID != personalLayerID || len(call[1].AccountIDs) != 3 {
		t.Fatalf("offered layers = %+v", call)
	}
	if f.alloc.selectLayers[0] != testSelfLayer {
		t.Fatalf("selected in layer %s, want the self-use layer", f.alloc.selectLayers[0])
	}
	// Once a is exhausted the personal layer pays, and a is not offered there.
	f.alloc.exhausted[a] = true
	if name := f.mustPick(pickOptions{groups: groups}); name != "b" {
		t.Fatalf("picked %q, want the platform account b", name)
	}
	last := f.alloc.selectCalls[len(f.alloc.selectCalls)-1]
	for _, id := range last {
		if id == a {
			t.Fatal("the personal layer offered the member's own account")
		}
	}
	if f.alloc.selectLayers[len(f.alloc.selectLayers)-1] != personalLayerID {
		t.Fatal("the platform account was not selected in the personal layer")
	}
}

// A conversation bound to an account that is no longer eligible in any layer
// of the request is unbound and allocated again.
func TestBindingRequiresEligibilityInSomeLayer(t *testing.T) {
	f := newFixture(t, 2)
	a, b := f.ids["a"], f.ids["b"]
	f.alloc.layerPermitted = map[string]map[string]bool{testSelfLayer: {a: true}}
	f.alloc.permitted = map[string]map[string]bool{testUser: {b: true}}
	groups := groupsHeader(groupLayer(testSelfLayer, a), personalLayer)
	if name := f.mustPick(pickOptions{session: "codex:s1", groups: groups}); name != "a" {
		t.Fatalf("picked %q, want a", name)
	}
	if name := f.mustPick(pickOptions{session: "codex:s1", groups: groups}); name != "a" {
		t.Fatalf("binding not reused: %q", name)
	}
	// Self-use turned off: a is eligible nowhere, so the binding is left.
	f.alloc.layerPermitted[testSelfLayer] = map[string]bool{}
	if name := f.mustPick(pickOptions{session: "codex:s1", groups: groups}); name != "b" {
		t.Fatalf("picked %q after the own account lost eligibility, want b", name)
	}
}

// Without layers the plugin keeps the original one-list protocol.
func TestUnlayeredRequestsKeepTheListProtocol(t *testing.T) {
	f := newFixture(t, 2)
	f.mustPick(pickOptions{})
	if len(f.alloc.layerCalls) != 0 || len(f.alloc.selectLayers) != 0 {
		t.Fatal("an unlayered request used the layered protocol")
	}
}

func TestGatewayClientLayerContract(t *testing.T) {
	var path, answer string
	status := http.StatusOK
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	}))
	defer server.Close()
	client := NewGatewayClient(server.URL, testInternalKey, "egress-allowlist:3129", time.Second)
	ctx := context.Background()
	const a, b = "0123456789abcdef", "fedcba9876543210"
	layers := []LayerCandidates{{GroupID: testSelfLayer, AccountIDs: []string{a}}, {GroupID: personalLayerID, AccountIDs: []string{b, a}}}

	answer = `{"groups":[{"group_id":"` + testSelfLayer + `","accounts":[{"id":"` + a + `","concurrent_limit":2}]},` +
		`{"group_id":"` + personalLayerID + `","accounts":[{"id":"` + b + `","concurrent_limit":1}]}]}`
	got, err := client.EligibleLayers(ctx, ProviderCodex, testUser, layers)
	if err != nil || len(got) != 2 || len(got[0]) != 1 || got[0][0].ConcurrentLimit != 2 || len(got[1]) != 1 || got[1][0].ID != b {
		t.Fatalf("eligibility = %+v, %v", got, err)
	}
	wantBody := `{"user_id":"` + testUser + `","groups":[{"group_id":"` + testSelfLayer + `","account_ids":["` + a + `"]},` +
		`{"group_id":"` + personalLayerID + `","account_ids":["` + a + `","` + b + `"]}]}`
	if path != "/internal/upstream-accounts/eligible" || body != wantBody {
		t.Fatalf("request %s %s", path, body)
	}
	for name, response := range map[string]string{
		"layers reordered":   `{"groups":[{"group_id":"` + personalLayerID + `","accounts":[]},{"group_id":"` + testSelfLayer + `","accounts":[]}]}`,
		"a layer missing":    `{"groups":[{"group_id":"` + testSelfLayer + `","accounts":[]}]}`,
		"account not asked":  `{"groups":[{"group_id":"` + testSelfLayer + `","accounts":[{"id":"` + b + `","concurrent_limit":1}]},{"group_id":"` + personalLayerID + `","accounts":[]}]}`,
		"zero limit":         `{"groups":[{"group_id":"` + testSelfLayer + `","accounts":[{"id":"` + a + `","concurrent_limit":0}]},{"group_id":"` + personalLayerID + `","accounts":[]}]}`,
		"unknown field":      `{"groups":[],"accounts":[]}`,
		"list protocol only": `{"accounts":[]}`,
	} {
		answer = response
		if _, err := client.EligibleLayers(ctx, ProviderCodex, testUser, layers); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if _, err := client.EligibleLayers(ctx, ProviderCodex, testUser, []LayerCandidates{{GroupID: "not-a-uuid"}}); err == nil {
		t.Fatal("an invalid layer ID was sent")
	}

	answer = `{"account_id":"` + a + `"}`
	if selected, err := client.SelectLayer(ctx, ProviderCodex, testUser, layers[0]); err != nil || selected != a {
		t.Fatalf("selection = %q, %v", selected, err)
	}
	if body != `{"user_id":"`+testUser+`","groups":[{"group_id":"`+testSelfLayer+`","account_ids":["`+a+`"]}]}` {
		t.Fatalf("selection request %s", body)
	}
	answer = `{"account_id":"` + b + `"}`
	if _, err := client.SelectLayer(ctx, ProviderCodex, testUser, layers[0]); err == nil {
		t.Fatal("an account outside the layer was accepted")
	}
	status, answer = http.StatusTooManyRequests, `{}`
	if _, err := client.SelectLayer(ctx, ProviderCodex, testUser, layers[0]); err != ErrConcurrency {
		t.Fatalf("exhausted layer = %v", err)
	}
	if _, err := client.SelectLayer(ctx, ProviderCodex, testUser, LayerCandidates{GroupID: testSelfLayer}); err != ErrUnavailable {
		t.Fatalf("empty layer selection = %v", err)
	}
}
