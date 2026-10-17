package proxy

import (
	"fmt"
	"os"
	"testing"

	"github.com/wsw/codex-gateway/internal/billing"
	"github.com/wsw/codex-gateway/internal/config"
)

func TestHaiku55UsageCountsEveryInputCategoryForLongContext(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/pricing-v2.example.json")
	if err != nil {
		t.Fatal(err)
	}
	pricing, err := config.ParseUsagePricing(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	raw, _, ok, err := pricing.ModelSnapshot("claude-haiku-5-5")
	if err != nil || !ok {
		t.Fatalf("snapshot: ok=%t err=%v", ok, err)
	}
	snapshot, err := config.ParsePricingSnapshot(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                       string
		ordinary, read, five, hour int64
		input                      int64
		context, cost              string
	}{
		{"at threshold", 40_000, 10_000, 30_000, 20_000, 100_000, config.ContextClassShort, "0.015850000000"},
		{"ordinary crosses threshold", 40_001, 10_000, 30_000, 20_000, 100_001, config.ContextClassLong, "0.079250500000"},
		{"cache read crosses threshold", 40_000, 10_001, 30_000, 20_000, 100_001, config.ContextClassLong, "0.079250050000"},
		{"five minute write crosses threshold", 40_000, 10_000, 30_001, 20_000, 100_001, config.ContextClassLong, "0.079250625000"},
		{"one hour write crosses threshold", 40_000, 10_000, 30_000, 20_001, 100_001, config.ContextClassLong, "0.079251000000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"input_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,"cache_creation":{"ephemeral_5m_input_tokens":%d,"ephemeral_1h_input_tokens":%d},"output_tokens":8000}`,
				tc.ordinary, tc.read, tc.five+tc.hour, tc.five, tc.hour)
			usage, err := anthropicUsage([]byte(body), Usage{})
			if err != nil || usage.InputTokens != tc.input || !usage.CacheWriteTTLPresent {
				t.Fatalf("Haiku 5.5 usage = %+v, %v", usage, err)
			}
			decision, err := snapshot.Select("standard", usage.InputTokens)
			if err != nil || decision.ContextClass != tc.context || decision.FallbackReason != "" {
				t.Fatalf("Haiku 5.5 decision = %+v, %v", decision, err)
			}
			cost, err := billing.CalculateCostV2TTLWithMultiplier(usage.InputTokens, usage.CachedTokens, usage.CacheWriteTokens,
				usage.CacheWrite5mTokens, usage.CacheWrite1hTokens, usage.OutputTokens, usage.CacheWriteTTLPresent,
				decision.InputUSDPerMillion, decision.CachedInputUSDPerMillion, decision.CacheWrite5mUSDPerMillion,
				decision.CacheWrite1hUSDPerMillion, decision.OutputUSDPerMillion, "1")
			if err != nil || cost != tc.cost {
				t.Fatalf("Haiku 5.5 cost = %q, %v; want %q", cost, err, tc.cost)
			}
		})
	}
}
