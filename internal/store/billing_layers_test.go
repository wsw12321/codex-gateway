package store

import (
	"reflect"
	"testing"
)

func TestSettlementLayerCandidatesFollowServingAccount(t *testing.T) {
	first := fundingLayer{position: 1, groupID: "g1", groupPeriodID: "p1", accountIDs: []string{"aaaaaaaaaaaaaaaa"}}
	second := fundingLayer{position: 2, groupID: "g2", groupPeriodID: "p2", accountIDs: []string{"bbbbbbbbbbbbbbbb", "aaaaaaaaaaaaaaaa"}}
	personal := fundingLayer{position: 3, allAccounts: true}
	for _, test := range []struct {
		name       string
		layers     []fundingLayer
		account    string
		groups     []fundingLayer
		personal   bool
		attributed bool
	}{
		{"account in both groups keeps billing order", []fundingLayer{first, second, personal}, "aaaaaaaaaaaaaaaa", []fundingLayer{first, second}, true, true},
		{"account only in the second group", []fundingLayer{first, second, personal}, "bbbbbbbbbbbbbbbb", []fundingLayer{second}, true, true},
		{"account outside groups is personal", []fundingLayer{first, second, personal}, "cccccccccccccccc", nil, true, true},
		{"missing attribution falls back to the first group", []fundingLayer{first, second, personal}, "", []fundingLayer{first}, false, false},
		{"foreign account without personal layer", []fundingLayer{first, second}, "cccccccccccccccc", []fundingLayer{first}, false, false},
		{"missing attribution with personal only", []fundingLayer{personal}, "", nil, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			groups, personal, attributed := settlementLayerCandidates(test.layers, test.account)
			if !reflect.DeepEqual(groups, test.groups) || personal != test.personal || attributed != test.attributed {
				t.Fatalf("groups=%+v personal=%v attributed=%v", groups, personal, attributed)
			}
		})
	}
}

func TestFundingLayerAllocationGroupsAreIndependent(t *testing.T) {
	layer := fundingLayer{groupID: "00000000-0000-0000-0000-000000000002", accountIDs: []string{"aaaaaaaaaaaaaaaa"}}
	group := layer.allocationGroup()
	group.AccountIDs[0] = "ffffffffffffffff"
	if layer.accountIDs[0] != "aaaaaaaaaaaaaaaa" {
		t.Fatal("forwarded layer shares the admission snapshot")
	}
	if personal := (fundingLayer{allAccounts: true}).allocationGroup(); personal.GroupID != PersonalFundingLayerID || !personal.AllAccounts || len(personal.AccountIDs) != 0 {
		t.Fatalf("personal layer = %+v", personal)
	}
}

func TestRule5SettlementCandidates(t *testing.T) {
	own := fundingLayer{position: 1, kind: FundingLayerSelf, accountIDs: []string{"aaaaaaaaaaaaaaaa"}}
	group := fundingLayer{position: 2, kind: FundingLayerGroup, groupID: "g1", groupPeriodID: "p1", accountIDs: []string{"bbbbbbbbbbbbbbbb"}}
	plan := fundingLayer{position: 3, kind: FundingLayerSubscription, subscriptionID: "s1", subscriptionPeriodID: "sp1", accountIDs: []string{"bbbbbbbbbbbbbbbb", "cccccccccccccccc"}}
	seller := fundingLayer{position: 4, kind: FundingLayerSubscription, subscriptionID: "s2", subscriptionPeriodID: "sp2", accountIDs: []string{"dddddddddddddddd"}}
	personal := fundingLayer{position: 5, kind: FundingLayerPersonal, allAccounts: true}
	all := []fundingLayer{own, group, plan, seller, personal}
	for _, test := range []struct {
		name    string
		layers  []fundingLayer
		account string
		tenant  bool
		want    rule5Settlement
	}{
		{"own account is self-use", all, "aaaaaaaaaaaaaaaa", true, rule5Settlement{selfUse: true}},
		{"platform account flows group, plan, personal", all, "bbbbbbbbbbbbbbbb", false,
			rule5Settlement{groups: []fundingLayer{group}, subscriptions: []fundingLayer{plan}, personal: true}},
		{"plan-only platform account", all, "cccccccccccccccc", false, rule5Settlement{subscriptions: []fundingLayer{plan}, personal: true}},
		{"seller account never reaches personal", all, "dddddddddddddddd", true, rule5Settlement{subscriptions: []fundingLayer{seller}}},
		{"unknown member account pays nothing", all, "eeeeeeeeeeeeeeee", true, rule5Settlement{}},
		{"missing attribution skips self-use", all, "", false, rule5Settlement{groups: []fundingLayer{group}, personal: true}},
		{"missing attribution with a plan first", []fundingLayer{own, plan, personal}, "", false, rule5Settlement{subscriptions: []fundingLayer{plan}}},
		{"missing attribution with self-use only", []fundingLayer{own}, "", false, rule5Settlement{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := rule5SettlementCandidates(test.layers, test.account, test.tenant); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("got %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestFundingRuleVersionAndLayerIDs(t *testing.T) {
	group := fundingLayer{groupID: "g1", accountIDs: []string{"aaaaaaaaaaaaaaaa"}}
	personal := fundingLayer{allAccounts: true}
	if fundingRuleVersion([]fundingLayer{group, personal}) != 4 {
		t.Fatal("groups and personal funds are rule 4")
	}
	own := fundingLayer{kind: FundingLayerSelf, tenant: true, accountIDs: []string{"bbbbbbbbbbbbbbbb"}}
	plan := fundingLayer{kind: FundingLayerSubscription, subscriptionID: "11111111-1111-4111-8111-111111111111", accountIDs: []string{"cccccccccccccccc"}}
	if fundingRuleVersion([]fundingLayer{own, personal}) != 5 || fundingRuleVersion([]fundingLayer{plan}) != 5 {
		t.Fatal("self-use and plan layers are rule 5")
	}
	if got := own.allocationGroup(); got.GroupID != SelfFundingLayerID || !got.Tenant || got.AllAccounts {
		t.Fatalf("self layer = %+v", got)
	}
	if got := plan.allocationGroup(); got.GroupID != plan.subscriptionID || got.Tenant {
		t.Fatalf("plan layer = %+v", got)
	}
}
