package config

import (
	"reflect"
	"testing"
)

func TestManageableModelNamesExcludesInternalGovernanceModel(t *testing.T) {
	pricing := UsagePricing{Models: map[string]ModelPricing{
		"gpt-z":                 {},
		InternalGovernanceModel: {},
		"gpt-a":                 {},
	}}
	if got, want := pricing.ManageableModelNames(), []string{"gpt-a", "gpt-z"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ManageableModelNames() = %#v, want %#v", got, want)
	}
	if pricing.IsManageableModel(InternalGovernanceModel) {
		t.Fatal("internal governance model is manageable")
	}
	if !pricing.IsManageableModel("gpt-a") || pricing.IsManageableModel("gpt-missing") {
		t.Fatal("manageable model membership is incorrect")
	}
}
