package store

import (
	"reflect"
	"testing"
)

func TestBillingFundingOrder(t *testing.T) {
	sources := []BillingFundingSource{
		{SourceType: "cash", Priority: 20, RemainingUSD: "9007199254740993"},
		{SourceType: "subscription", SubscriptionID: "b", Priority: 30, PeriodDays: 1, RemainingUSD: "1"},
		{SourceType: "subscription", SubscriptionID: "a", Priority: 30, PeriodDays: 1, RemainingUSD: "1.000000000000"},
		{SourceType: "subscription", SubscriptionID: "small", Priority: 20, PeriodDays: 7, RemainingUSD: "9007199254740992.000000000001"},
		{SourceType: "subscription", SubscriptionID: "large", Priority: 20, PeriodDays: 7, RemainingUSD: "9007199254740992.000000000002"},
		{SourceType: "subscription", SubscriptionID: "long", Priority: 20, PeriodDays: 31, RemainingUSD: "0.000000000001"},
		{SourceType: "subscription", SubscriptionID: "first", Priority: 1, PeriodDays: 1, RemainingUSD: "1"},
	}
	if err := sortBillingFundingSources(sources); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, source := range sources {
		ids = append(ids, source.SubscriptionID)
	}
	if want := []string{"first", "long", "large", "small", "", "a", "b"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("funding order = %v, want %v", ids, want)
	}
	for _, priority := range []int{-1, 0, 101} {
		if err := sortBillingFundingSources([]BillingFundingSource{{Priority: priority, RemainingUSD: "1"}}); err == nil {
			t.Fatalf("accepted priority %d", priority)
		}
	}
}

func TestBillingSettlementUsesVersionedAdmissionOrder(t *testing.T) {
	day, week, month := "day", "week", "month"
	cutoff := int64(7)
	for _, version := range []int{1, 2} {
		sources, err := billingSettlementSources(BillingReservation{FundingRuleVersion: version,
			DayPeriodID: &day, WeekPeriodID: &week, MonthPeriodID: &month, CashLotCutoff: &cutoff})
		if err != nil {
			t.Fatal(err)
		}
		var types []string
		for _, source := range sources {
			types = append(types, source.SourceType)
		}
		if !reflect.DeepEqual(types, []string{"day", "week", "month", "cash"}) {
			t.Fatalf("legacy order changed: %v", types)
		}
	}
	frozen := []BillingFundingSource{{SourceType: "cash", CashLotCutoff: 7, Priority: 100},
		{SourceType: "subscription", SubscriptionID: "sub", PeriodID: "period", Priority: 1}}
	got, err := billingSettlementSources(BillingReservation{FundingRuleVersion: 3, OrderedFundingSources: frozen})
	if err != nil || !reflect.DeepEqual(got, frozen) {
		t.Fatalf("snapshot must never be sorted again: %v %v", got, err)
	}
	for _, bad := range [][]BillingFundingSource{
		{{SourceType: "unknown"}}, {{SourceType: "cash"}}, {{SourceType: "subscription", PeriodID: "p"}},
		{{SourceType: "cash", CashLotCutoff: 1}, {SourceType: "cash", CashLotCutoff: 2}},
	} {
		if _, err := billingSettlementSources(BillingReservation{FundingRuleVersion: 3, OrderedFundingSources: bad}); err == nil {
			t.Fatalf("accepted corrupt snapshot: %+v", bad)
		}
	}
}
