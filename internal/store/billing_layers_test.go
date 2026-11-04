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
