package server

import (
	"strings"
	"testing"

	"github.com/wsw/codex-gateway/internal/config"
)

func TestExtractTopLevelModel(t *testing.T) {
	for _, test := range []struct {
		raw   string
		model string
	}{
		{`{"model":"gpt-5","input":"hello"}`, "gpt-5"},
		{`{"input":[{"text":"fake \\\"model\\\":\\\"evil\\\""}],"model":"gpt-5.1-codex"}`, "gpt-5.1-codex"},
		{` { "stream": true, "nested": {"model":"evil"}, "model" : "o3" }`, "o3"},
	} {
		got, err := extractTopLevelModel([]byte(test.raw))
		if err != nil || got != test.model {
			t.Fatalf("extract %s: got %q, %v", test.raw, got, err)
		}
	}
}

func TestPricingForModelRequiresExactCatalogEntry(t *testing.T) {
	pricing := config.UsagePricing{Models: map[string]config.ModelPricing{
		"gpt-exact": {InputUSDPerMillion: "1", CachedInputUSDPerMillion: "0.1", OutputUSDPerMillion: "10"},
	}}
	if _, ok := pricingForModel(pricing, "gpt-exact"); !ok {
		t.Fatal("exact pricing entry was not found")
	}
	for _, model := range []string{"GPT-EXACT", "gpt-exact-latest", " gpt-exact"} {
		if _, ok := pricingForModel(pricing, model); ok {
			t.Fatalf("non-exact model %q matched pricing catalog", model)
		}
	}
}

func TestRejectsNestedOrEscapedModel(t *testing.T) {
	for _, raw := range []string{
		`{"input":{"model":"evil"}}`,
		`{"model":"../../secret"}`,
		`{"mo\u0064el":"gpt-5"}`,
	} {
		if _, err := extractTopLevelModel([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestRecordedUpstreamModelRejectsMalformedValues(t *testing.T) {
	if got := recordedUpstreamModel("gpt-5.2-codex"); got != "gpt-5.2-codex" {
		t.Fatalf("valid upstream model = %q", got)
	}
	for _, model := range []string{"", " gpt-5.2-codex ", "../../model", strings.Repeat("a", 129)} {
		if got := recordedUpstreamModel(model); got != "" {
			t.Fatalf("malformed upstream model %q was recorded as %q", model, got)
		}
	}
}
