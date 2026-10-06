package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/wsw/codex-gateway/internal/config"
)

func TestSetModelPriceRejectsInvalidInputBeforeTransaction(t *testing.T) {
	valid := SetModelPriceParams{BillingWriteParams: BillingWriteParams{OperationID: "00000000-0000-4000-8000-000000000001", ActorUserID: "00000000-0000-4000-8000-000000000002", Reason: "pricing regression"}, Model: "test-model", Action: "save", StructureID: strings.Repeat("a", 64), Price: &config.ModelPricing{InputUSDPerMillion: "1", CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "2"}}
	for name, mutate := range map[string]func(*SetModelPriceParams){
		"operation":     func(p *SetModelPriceParams) { p.OperationID = "" },
		"actor":         func(p *SetModelPriceParams) { p.ActorUserID = "" },
		"reason":        func(p *SetModelPriceParams) { p.Reason = "" },
		"model":         func(p *SetModelPriceParams) { p.Model = "" },
		"internal":      func(p *SetModelPriceParams) { p.Model = config.InternalGovernanceModel },
		"version":       func(p *SetModelPriceParams) { p.Version = -1 },
		"structure":     func(p *SetModelPriceParams) { p.StructureID = "" },
		"action":        func(p *SetModelPriceParams) { p.Action = "delete" },
		"missing-price": func(p *SetModelPriceParams) { p.Price = nil },
		"restore-price": func(p *SetModelPriceParams) { p.Action = "restore" },
		"precision": func(p *SetModelPriceParams) {
			copy := *p.Price
			copy.OutputUSDPerMillion = "0.0000000000001"
			p.Price = &copy
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := valid
			mutate(&p)
			if _, err := New(nil).SetModelPrice(context.Background(), p); !errors.Is(err, ErrInvalid) {
				t.Fatalf("got %v, want invalid", err)
			}
		})
	}
}

func TestModelPriceValueDetectsConflictAndCorruption(t *testing.T) {
	p := config.UsagePricing{SchemaVersion: 1, Models: map[string]config.ModelPricing{"model": {InputUSDPerMillion: "1", CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "3"}}}
	structure, _ := p.ModelPriceStructureID("model")
	override := config.ModelPricing{InputUSDPerMillion: "0", CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "9"}
	raw, _ := json.Marshal(override)
	v, err := modelPriceValue(p, "model", "2", storedModelPrice{price: raw, structureID: structure, version: 2})
	if err != nil || v.Source != "override" || v.EffectivePrice == nil || v.EffectivePrice.OutputUSDPerMillion != "9" || v.Multiplier != "2" || v.Version != 2 {
		t.Fatalf("override value %+v %v", v, err)
	}
	v, err = modelPriceValue(p, "model", "2", storedModelPrice{price: raw, structureID: "obsolete", version: 2})
	if err != nil || v.Source != "conflict" || !v.Conflict || v.EffectivePrice != nil || !v.Editable || v.StructureID != structure {
		t.Fatalf("conflict %+v %v", v, err)
	}
	v, err = modelPriceValue(p, "model", "2", storedModelPrice{structureID: "obsolete", version: 3})
	if err != nil || v.Source != "config" || v.Conflict || v.EffectivePrice == nil || v.Version != 3 {
		t.Fatalf("restored %+v %v", v, err)
	}
	if _, err := modelPriceValue(p, "model", "1", storedModelPrice{price: []byte(`{"input_usd_per_million":"invalid"}`), structureID: structure}); err == nil {
		t.Fatal("corrupt stored price silently defaulted")
	}
}
