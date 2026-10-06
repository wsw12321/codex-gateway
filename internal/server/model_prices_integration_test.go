//go:build integration

package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/wsw/codex-gateway/internal/store"
)

func TestModelPricesPostgresHTTPIntegration(t *testing.T) {
	f := newInvitationHTTPFixture(t)
	f.s.config.UsagePricing = modelPriceTestCatalog(t)
	const collection = "/admin/billing/model-prices"
	const endpoint = collection + "/gpt-6-astra"
	readModel := func() store.ModelPrice {
		t.Helper()
		response := f.send(t, "GET", collection, nil, f.cookie, 200)
		var result struct {
			Models []store.ModelPrice `json:"models"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		for _, row := range result.Models {
			if row.Model == "gpt-6-astra" {
				return row
			}
		}
		t.Fatal("configured model absent from GET response")
		return store.ModelPrice{}
	}
	f.send(t, "GET", collection, nil, "", 401)
	initial := readModel()
	if initial.Version != 0 || initial.Source != "config" || initial.EffectivePrice == nil || !initial.Editable || initial.Conflict {
		t.Fatalf("initial row: %+v", initial)
	}
	body := modelPriceTestBody(t, f.s)
	body["operation_id"] = uuid.NewString()
	price := modelPriceTestCatalog(t).Models["gpt-6-astra"]
	price.ServiceTiers["fast"].Long.InputUSDPerMillion = "3.125"
	body["price"] = price
	first := f.send(t, "PUT", endpoint, body, f.cookie, 200).Body.String()
	if replay := f.send(t, "PUT", endpoint, body, f.cookie, 200).Body.String(); replay != first {
		t.Fatalf("replay changed response: %s != %s", replay, first)
	}
	saved := readModel()
	if saved.Version != 1 || saved.Source != "override" || saved.EffectivePrice.ServiceTiers["fast"].Long.InputUSDPerMillion != "3.125" || saved.UpdatedAt == nil {
		t.Fatalf("override not visible: %+v", saved)
	}
	stale := modelPriceTestBody(t, f.s)
	stale["operation_id"] = uuid.NewString()
	if response := f.send(t, "PUT", endpoint, stale, f.cookie, 409); !strings.Contains(response.Body.String(), "model_price_conflict") {
		t.Fatalf("missing concurrency conflict: %s", response.Body)
	}
	body["reason"] = "different operation with same ID"
	f.send(t, "PUT", endpoint, body, f.cookie, 409)
	body["reason"] = "update base prices"
	restore := modelPriceTestBody(t, f.s)
	restore["operation_id"], restore["action"], restore["version"] = uuid.NewString(), "restore", 1
	delete(restore, "price")
	f.send(t, "PUT", endpoint, restore, f.cookie, 200)
	restored := readModel()
	if restored.Version != 2 || restored.Source != "config" || restored.EffectivePrice.ServiceTiers["fast"].Long.InputUSDPerMillion != "1.25" {
		t.Fatalf("restore failed to retain version or defaults: %+v", restored)
	}
	if replay := f.send(t, "PUT", endpoint, body, f.cookie, 200).Body.String(); replay != first {
		t.Fatalf("restored state changed original operation response: %s != %s", replay, first)
	}
	second := modelPriceTestBody(t, f.s)
	second["operation_id"], second["version"], second["price"] = uuid.NewString(), 2, price
	f.send(t, "PUT", endpoint, second, f.cookie, 200)
	deployed := f.s.config.UsagePricing.Models["gpt-6-astra"]
	deployed.LongContextThresholdTokens++
	f.s.config.UsagePricing.Models["gpt-6-astra"] = deployed
	conflict := readModel()
	if conflict.Version != 3 || conflict.Source != "conflict" || !conflict.Conflict || conflict.EffectivePrice != nil || conflict.StructureID == initial.StructureID {
		t.Fatalf("deployment conflict not surfaced: %+v", conflict)
	}
	if replay := f.send(t, "PUT", endpoint, body, f.cookie, 200).Body.String(); replay != first {
		t.Fatalf("deployment changed original operation response: %s != %s", replay, first)
	}
	second["operation_id"], second["version"] = uuid.NewString(), 3
	f.send(t, "PUT", endpoint, second, f.cookie, 409)
	restore["operation_id"], restore["version"], restore["structure_id"] = uuid.NewString(), 3, conflict.StructureID
	f.send(t, "PUT", endpoint, restore, f.cookie, 200)
	recovered := readModel()
	if recovered.Version != 4 || recovered.Conflict || recovered.Source != "config" || recovered.EffectivePrice.LongContextThresholdTokens != deployed.LongContextThresholdTokens {
		t.Fatalf("deployment restore failed: %+v", recovered)
	}
	delete(f.s.config.UsagePricing.Models, "gpt-6-astra")
	if replay := f.send(t, "PUT", endpoint, body, f.cookie, 200).Body.String(); replay != first {
		t.Fatalf("model removal changed original operation response: %s != %s", replay, first)
	}
	for _, action := range []string{"save", "restore"} {
		newWrite := make(map[string]any, len(body))
		for key, value := range body {
			newWrite[key] = value
		}
		newWrite["operation_id"], newWrite["action"], newWrite["version"] = uuid.NewString(), action, 4
		if action == "restore" {
			delete(newWrite, "price")
		}
		if response := f.send(t, "PUT", endpoint, newWrite, f.cookie, 404); !strings.Contains(response.Body.String(), "model_price_not_editable") {
			t.Fatalf("new %s for removed model was not rejected: %s", action, response.Body)
		}
		var artifacts int
		if err := f.s.store.DB().QueryRowContext(context.Background(), `SELECT
			(SELECT count(*) FROM billing_operations WHERE operation_id=$1)+
			(SELECT count(*) FROM billing_ledger_entries WHERE operation_id=$1)+
			(SELECT count(*) FROM audit_events WHERE metadata->>'operation_id'=$1::text)`, newWrite["operation_id"]).Scan(&artifacts); err != nil || artifacts != 0 {
			t.Fatalf("absent-model %s left %d artifacts: %v", action, artifacts, err)
		}
	}
	var operations, ledger, audits int
	err := f.s.store.DB().QueryRowContext(context.Background(), `SELECT
		(SELECT count(*) FROM billing_operations WHERE operation_type='model_price'),
		(SELECT count(*) FROM billing_ledger_entries WHERE entry_type='model_price'),
		(SELECT count(*) FROM audit_events WHERE event_type='billing.model_price_updated')`).Scan(&operations, &ledger, &audits)
	if err != nil || operations != 4 || ledger != 4 || audits != 4 {
		t.Fatalf("retries/conflicts changed durable operations: operations=%d ledger=%d audits=%d err=%v", operations, ledger, audits, err)
	}
}
