package server

import (
	"testing"

	"github.com/wsw/codex-gateway/internal/store"
)

func TestSummarizeUsageDoesNotDoubleCountCachedOrReasoning(t *testing.T) {
	status := 200
	ttft := int64(10)
	duration := int64(100)
	summary := summarizeUsage([]store.UsageRequest{
		{HTTPStatus: &status, State: "completed", InputTokens: 100, CachedInputTokens: 80, OutputTokens: 50, ReasoningTokens: 25, TTFTMillis: &ttft, DurationMillis: &duration},
	})
	if summary.Tokens != 150 {
		t.Fatalf("tokens were double-counted: %+v", summary)
	}
	if summary.CacheRate != .8 || summary.ErrorRate != 0 {
		t.Fatalf("unexpected rates: %+v", summary)
	}
}

func TestPercentile95NearestRank(t *testing.T) {
	values := make([]int64, 100)
	for i := range values {
		values[i] = int64(i + 1)
	}
	if got := percentile95(values); got != 95 {
		t.Fatalf("p95 = %d", got)
	}
}
