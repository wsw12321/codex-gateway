package config

import "sort"

const InternalGovernanceModel = "codex-auto-review"

// ManageableModelNames returns the priced models whose user access can be
// administered. The internal governance model deliberately retains its
// existing behavior and is not represented in model-access tables.
func (p UsagePricing) ManageableModelNames() []string {
	models := make([]string, 0, len(p.Models))
	for model := range p.Models {
		if model != InternalGovernanceModel {
			models = append(models, model)
		}
	}
	sort.Strings(models)
	return models
}

func (p UsagePricing) IsManageableModel(model string) bool {
	if model == InternalGovernanceModel {
		return false
	}
	_, ok := p.Models[model]
	return ok
}
