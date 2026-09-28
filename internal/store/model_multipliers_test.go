package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSetModelMultiplierRejectsInvalidInputBeforeTransaction(t *testing.T) {
	valid := SetModelMultiplierParams{
		BillingWriteParams: BillingWriteParams{
			OperationID: "00000000-0000-4000-8000-000000000001",
			ActorUserID: "00000000-0000-4000-8000-000000000002",
			Reason:      "test setting validation",
		},
		Model: "test-model", Multiplier: "1",
	}
	for _, tc := range []struct {
		name   string
		change func(*SetModelMultiplierParams)
	}{
		{"missing-operation", func(p *SetModelMultiplierParams) { p.OperationID = "" }},
		{"missing-actor", func(p *SetModelMultiplierParams) { p.ActorUserID = "" }},
		{"missing-reason", func(p *SetModelMultiplierParams) { p.Reason = " \n" }},
		{"long-reason", func(p *SetModelMultiplierParams) { p.Reason = strings.Repeat("x", 501) }},
		{"empty-model", func(p *SetModelMultiplierParams) { p.Model = "" }},
		{"long-model", func(p *SetModelMultiplierParams) { p.Model = strings.Repeat("x", 129) }},
		{"fixed-internal-model", func(p *SetModelMultiplierParams) { p.Model = "codex-auto-review" }},
		{"zero", func(p *SetModelMultiplierParams) { p.Multiplier = "0" }},
		{"negative", func(p *SetModelMultiplierParams) { p.Multiplier = "-0.5" }},
		{"exponent", func(p *SetModelMultiplierParams) { p.Multiplier = "1e2" }},
		{"non-finite", func(p *SetModelMultiplierParams) { p.Multiplier = "NaN" }},
		{"overflow", func(p *SetModelMultiplierParams) { p.Multiplier = "1000000000000000000" }},
		{"excess-scale", func(p *SetModelMultiplierParams) { p.Multiplier = "0.0000000000001" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := valid
			tc.change(&params)
			// A nil database makes any unintended transaction observable.
			_, err := New(nil).SetModelMultiplier(context.Background(), params)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("SetModelMultiplier error = %v, want ErrInvalid", err)
			}
		})
	}
}
