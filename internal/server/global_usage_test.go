package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/store"
)

func testUsagePricing() config.UsagePricing {
	return config.UsagePricing{
		CatalogAsOf: "2026-08-01",
		FXAsOf:      "2026-08-02",
		USDCNYRate:  "7.20",
		Models: map[string]config.ModelPricing{
			"priced": {
				InputUSDPerMillion:       "1",
				CachedInputUSDPerMillion: "0.1",
				OutputUSDPerMillion:      "10",
			},
		},
	}
}

func TestSummarizeGlobalUsageUsesLedgerAmountsAndReportsCoverage(t *testing.T) {
	pricing := testUsagePricing()
	rows := []store.GlobalUsageRow{
		{
			UserID: "user-a", Username: "alice", DisplayName: "Alice", Model: "priced",
			RequestCount: 2, InputTokens: 1_000_000, CachedInputTokens: 800_000,
			OutputTokens: 100_000, ReasoningTokens: 90_000, LedgerTokens: 1_100_000,
			ActualCostUSD: "1.28", ChargedUSD: "1.20", UncoveredUSD: "0.08",
		},
		{
			UserID: "user-a", Username: "alice", DisplayName: "Alice", Model: "unknown-z",
			RequestCount: 1, InputTokens: 50, OutputTokens: 50,
		},
		{
			UserID: "user-b", Username: "bob", DisplayName: "Bob", Model: "unknown-a",
			RequestCount: 1, InputTokens: 100, OutputTokens: 0,
		},
		{UserID: "user-c", Username: "carol", DisplayName: "Carol"},
	}

	response, err := summarizeGlobalUsage(rows, pricing)
	if err != nil {
		t.Fatal(err)
	}
	if response.Summary.ActiveUsers != 2 || response.Summary.TotalUsers != 3 {
		t.Fatalf("unexpected user counts: %+v", response.Summary)
	}
	usage := response.Summary.Usage
	if usage.Requests != 4 || usage.Tokens != 1_100_200 || usage.PricedTokens != 1_100_000 || usage.UnpricedTokens != 200 {
		t.Fatalf("unexpected total usage: %+v", usage)
	}
	if usage.EstimatedUSD != "1.280000" || usage.EstimatedCNY != "9.216000" {
		t.Fatalf("unexpected estimates: %+v", usage)
	}
	if response.Summary.PricingCoverage != "0.9998" {
		t.Fatalf("pricing coverage = %s", response.Summary.PricingCoverage)
	}
	if got := strings.Join(response.Pricing.UnpricedModels, ","); got != "unknown-a,unknown-z" {
		t.Fatalf("unpriced models = %q", got)
	}
	if len(response.Users) != 3 || response.Users[0].ID != "user-a" {
		t.Fatalf("users are not cost-descending: %+v", response.Users)
	}
	if response.Users[0].PricingStatus != "partial" || response.Users[1].PricingStatus != "unpriced" || response.Users[2].PricingStatus != "no_usage" {
		t.Fatalf("unexpected pricing statuses: %+v", response.Users)
	}
	if response.Users[2].Usage.EstimatedUSD != "0.000000" || response.Users[2].Usage.EstimatedCNY != "0.000000" {
		t.Fatalf("zero-usage estimate = %+v", response.Users[2].Usage)
	}
}

func TestSummarizeGlobalUsageReconcilesRoundedUserAmounts(t *testing.T) {
	pricing := testUsagePricing()
	pricing.USDCNYRate = "1"
	rows := []store.GlobalUsageRow{
		{UserID: "b", Username: "b", DisplayName: "B", Model: "priced", RequestCount: 1, InputTokens: 1, LedgerTokens: 1, ActualCostUSD: "0.0000005", ChargedUSD: "0.0000005", UncoveredUSD: "0"},
		{UserID: "a", Username: "a", DisplayName: "A", Model: "priced", RequestCount: 1, InputTokens: 1, LedgerTokens: 1, ActualCostUSD: "0.0000005", ChargedUSD: "0.0000005", UncoveredUSD: "0"},
	}

	response, err := summarizeGlobalUsage(rows, pricing)
	if err != nil {
		t.Fatal(err)
	}
	if response.Users[0].ID != "a" || response.Users[1].ID != "b" {
		t.Fatalf("equal-cost ordering is unstable: %+v", response.Users)
	}
	for _, user := range response.Users {
		if user.Usage.EstimatedUSD != "0.000001" {
			t.Fatalf("rounded user estimate = %s", user.Usage.EstimatedUSD)
		}
	}
	if response.Summary.Usage.EstimatedUSD != "0.000002" {
		t.Fatalf("summary estimate does not reconcile: %s", response.Summary.Usage.EstimatedUSD)
	}
	if response.Summary.Usage.EstimatedCNY != "0.000002" ||
		response.Summary.Usage.Requests != 2 || response.Summary.Usage.Tokens != 2 {
		t.Fatalf("summary does not reconcile with user rows: %+v", response.Summary.Usage)
	}
}

func TestSummarizeGlobalUsageIgnoresMutableModelPrices(t *testing.T) {
	rows := []store.GlobalUsageRow{{
		UserID: "user", Username: "user", DisplayName: "User", Model: "priced",
		RequestCount: 1, InputTokens: 100, OutputTokens: 10, LedgerTokens: 110,
		ActualCostUSD: "0.25", ChargedUSD: "0.2", UncoveredUSD: "0.05",
	}}
	first := testUsagePricing()
	second := testUsagePricing()
	second.Models["priced"] = config.ModelPricing{
		InputUSDPerMillion: "999", CachedInputUSDPerMillion: "999", OutputUSDPerMillion: "999",
	}
	a, err := summarizeGlobalUsage(rows, first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := summarizeGlobalUsage(rows, second)
	if err != nil {
		t.Fatal(err)
	}
	if a.Summary.Usage.ActualCostUSD != "0.250000" || a.Summary.Usage != b.Summary.Usage {
		t.Fatalf("mutable catalog changed ledger report: a=%+v b=%+v", a.Summary.Usage, b.Summary.Usage)
	}
}

func TestSummarizeGlobalUsageRejectsTokenTotalOverflow(t *testing.T) {
	const maximumInt64 = int64(^uint64(0) >> 1)
	pricing := testUsagePricing()
	if _, err := summarizeGlobalUsage([]store.GlobalUsageRow{{
		UserID: "user", Username: "user", DisplayName: "User", Model: "priced",
		RequestCount: 1, InputTokens: maximumInt64, OutputTokens: 1,
	}}, pricing); err == nil {
		t.Fatal("expected token total overflow to fail closed")
	}
}

func TestParseDecimalRejectsNonDecimalRationals(t *testing.T) {
	for _, value := range []string{"", "1/2", "1e3", "+1", "-1", ".1", "1.", "1..0"} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseDecimal(value); err == nil {
				t.Fatalf("accepted %q", value)
			}
		})
	}
}

func TestParseGlobalUsageQuery(t *testing.T) {
	now := time.Date(2026, time.August, 11, 12, 34, 56, 0, time.FixedZone("test", 8*60*60))

	t.Run("default current UTC month", func(t *testing.T) {
		query, err := parseGlobalUsageQuery(now, url.Values{})
		if err != nil {
			t.Fatal(err)
		}
		if got := query.From.Format(time.RFC3339); got != "2026-08-01T00:00:00Z" {
			t.Fatalf("from = %s", got)
		}
		if got := query.Until.Format(time.RFC3339); got != "2026-08-11T04:34:56Z" {
			t.Fatalf("until = %s", got)
		}
		if got := query.LiveFrom.Format(time.RFC3339); got != "2026-07-01T00:00:00Z" {
			t.Fatalf("live from = %s", got)
		}
	})

	t.Run("browser RFC3339 boundaries", func(t *testing.T) {
		query, err := parseGlobalUsageQuery(now, url.Values{
			"from":  {"2026-08-01T00:00:00+08:00"},
			"until": {"2026-08-11T00:00:00+08:00"},
			"model": {"gpt-exact"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if query.Model != "gpt-exact" || query.From.Hour() != 16 || query.From.Day() != 31 {
			t.Fatalf("unexpected browser-local interval: %+v", query)
		}
	})

	t.Run("ninety browser days across DST", func(t *testing.T) {
		query, err := parseGlobalUsageQuery(now, url.Values{
			"from":  {"2026-08-03T00:00:00-04:00"},
			"until": {"2026-11-01T00:00:00-05:00"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := query.Until.Sub(query.From); got != 90*24*time.Hour+time.Hour {
			t.Fatalf("DST interval = %s", got)
		}
	})

	t.Run("all history", func(t *testing.T) {
		query, err := parseGlobalUsageQuery(now, url.Values{"all": {"true"}})
		if err != nil {
			t.Fatal(err)
		}
		if !query.All {
			t.Fatal("all was not enabled")
		}
	})
}

func TestParseGlobalUsageQueryRejectsInvalidFilters(t *testing.T) {
	now := time.Date(2026, time.August, 11, 0, 0, 0, 0, time.UTC)
	tests := map[string]url.Values{
		"invalid all":         {"all": {"yes"}},
		"duplicate all":       {"all": {"true", "false"}},
		"all with interval":   {"all": {"true"}, "from": {"2026-01-01"}},
		"reversed":            {"from": {"2026-08-02"}, "until": {"2026-08-01"}},
		"over ninety days":    {"from": {"2026-05-01"}, "until": {"2026-07-30"}},
		"ninety one DST days": {"from": {"2026-08-02T00:00:00-04:00"}, "until": {"2026-11-01T00:00:00-05:00"}},
		"adversarial offsets": {"from": {"2026-08-03T00:00:00+14:00"}, "until": {"2026-11-01T00:00:00-12:00"}},
		"non-midnight DST":    {"from": {"2026-08-03T01:00:00-04:00"}, "until": {"2026-11-01T01:00:00-05:00"}},
		"invalid date":        {"from": {"not-a-date"}},
		"trimmed model":       {"model": {" gpt-exact"}},
		"model with slash":    {"model": {"openai/gpt-exact"}},
		"oversized model":     {"model": {strings.Repeat("m", 129)}},
		"duplicate model":     {"model": {"a", "b"}},
		"duplicate from":      {"from": {"2026-08-01", "2026-08-02"}},
		"duplicate until":     {"until": {"2026-08-10", "2026-08-11"}},
	}
	for name, values := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseGlobalUsageQuery(now, values); err == nil {
				t.Fatalf("accepted invalid filter: %v", values)
			}
		})
	}
}

func TestOwnerOnlyRejectsMemberForGlobalUsage(t *testing.T) {
	server := &Server{}
	called := false
	handler := server.ownerOnly(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	request := httptest.NewRequest(http.MethodGet, "/admin/usage/global", nil)
	request = request.WithContext(context.WithValue(
		request.Context(), userContextKey, store.User{ID: "member", Role: store.UserRoleMember},
	))
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d", response.Code)
	}
	if called {
		t.Fatal("member reached owner handler")
	}
	if !strings.Contains(response.Body.String(), "owner_required") {
		t.Fatalf("unexpected response: %s", response.Body.String())
	}
}

func TestGlobalUsageEmptyArraysSerializeAsArrays(t *testing.T) {
	response, err := summarizeGlobalUsage(nil, testUsagePricing())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if !strings.Contains(text, `"users":[]`) || !strings.Contains(text, `"unpriced_models":[]`) {
		t.Fatalf("empty collections are not arrays: %s", text)
	}
}
