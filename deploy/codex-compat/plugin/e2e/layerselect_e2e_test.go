//go:build e2e

package e2e

import (
	"net/http"
	"strings"
	"testing"
)

// selfLayer is the Gateway's self-use funding layer.
const selfLayer = "00000000-0000-4000-8000-000000000002"

// TestLayerSelect: with funding layers, eligibility is asked per layer, so a
// member's own account serves the request only through the layer that pays
// for it, and the personal layer never reaches it.
func TestLayerSelect(t *testing.T) {
	h := newHarness(t)
	own, platform := h.addAccount("own", nil), h.addAccount("platform", nil)
	h.start()
	h.gateway.permitted[userAlice] = map[string]bool{platform.id: true}
	h.gateway.layerPermitted[selfLayer] = map[string]bool{own.id: true}
	scope := groups(groupLayer(selfLayer, own.id), personalLayer)

	t.Run("the own account serves through the self-use layer", func(t *testing.T) {
		h.resetObservations()
		h.gateway.prefer = []string{platform.id, own.id}
		got := h.do(call{groups: scope})
		if got.status != http.StatusOK || !strings.Contains(got.body, "response.completed") {
			t.Fatalf("status %d: %s", got.status, got.body)
		}
		if served := h.served(); len(served) != 1 || served[0] != "own" {
			t.Fatalf("provider saw %v, want the own account first", served)
		}
		if reports := h.gateway.reportsFor(got.token); len(reports) != 1 || reports[0].AccountID != own.id {
			t.Fatalf("unexpected attribution: %+v", reports)
		}
	})

	t.Run("without self-use the personal layer serves platform accounts only", func(t *testing.T) {
		h.resetObservations()
		h.gateway.layerPermitted[selfLayer] = map[string]bool{}
		defer func() { h.gateway.layerPermitted[selfLayer] = map[string]bool{own.id: true} }()
		h.gateway.prefer = []string{own.id, platform.id}
		for i := 0; i < 3; i++ {
			if got := h.do(call{groups: scope}); got.status != http.StatusOK {
				t.Fatalf("status %d: %s", got.status, got.body)
			}
		}
		for _, name := range h.served() {
			if name != "platform" {
				t.Fatalf("the personal layer reached %s", name)
			}
		}
	})
}
