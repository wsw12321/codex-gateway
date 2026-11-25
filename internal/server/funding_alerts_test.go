package server

import (
	"reflect"
	"testing"

	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

func TestAttributionMissingAlert(t *testing.T) {
	t.Parallel()
	group := gatewayproxy.AllocationGroup{GroupID: "g", AccountIDs: []string{"a"}, Kind: store.FundingLayerGroup}
	legacyGroup := gatewayproxy.AllocationGroup{GroupID: "g", AccountIDs: []string{"a"}}
	personal := gatewayproxy.AllocationGroup{GroupID: gatewayproxy.PersonalLayerID, AllAccounts: true, Kind: store.FundingLayerPersonal}
	self := gatewayproxy.AllocationGroup{GroupID: store.SelfFundingLayerID, AccountIDs: []string{"own"}, Tenant: true, Kind: store.FundingLayerSelf}
	subscription := gatewayproxy.AllocationGroup{GroupID: "sub", AccountIDs: []string{"sold"}, Tenant: true, Kind: store.FundingLayerSubscription}
	for _, tc := range []struct {
		name     string
		groups   []gatewayproxy.AllocationGroup
		alert    bool
		dedupe   string
		title    string
		fallback string
	}{
		{name: "single personal layer", groups: []gatewayproxy.AllocationGroup{personal}},
		{name: "single rule 4 group", groups: []gatewayproxy.AllocationGroup{group}},
		{name: "single restricted subscription", groups: []gatewayproxy.AllocationGroup{subscription}},
		{name: "rule 4 layers keep the original alert", groups: []gatewayproxy.AllocationGroup{group, personal}, alert: true,
			dedupe: "billing_attribution_missing", title: "上游账号归因缺失，已按第一扣费来源结算", fallback: "first"},
		{name: "layers without a kind are rule 4", groups: []gatewayproxy.AllocationGroup{legacyGroup, {GroupID: gatewayproxy.PersonalLayerID, AllAccounts: true}}, alert: true,
			dedupe: "billing_attribution_missing", title: "上游账号归因缺失，已按第一扣费来源结算", fallback: "first"},
		{name: "self-use alone leaves the cost uncovered", groups: []gatewayproxy.AllocationGroup{self}, alert: true,
			dedupe: "billing_attribution_missing:rule5:none", title: "上游账号归因缺失，不按自用免费，没有可结算的扣费层，费用记为未覆盖", fallback: "none"},
		{name: "self-use is skipped for the first group", groups: []gatewayproxy.AllocationGroup{self, group, subscription, personal}, alert: true,
			dedupe: "billing_attribution_missing:rule5:group", title: "上游账号归因缺失，不按自用免费，已按第一个群组层结算", fallback: store.FundingLayerGroup},
		{name: "a subscription pays before personal funds", groups: []gatewayproxy.AllocationGroup{subscription, personal}, alert: true,
			dedupe: "billing_attribution_missing:rule5:subscription", title: "上游账号归因缺失，已按第一个受限订阅层结算", fallback: store.FundingLayerSubscription},
		{name: "self-use then personal funds", groups: []gatewayproxy.AllocationGroup{self, personal}, alert: true,
			dedupe: "billing_attribution_missing:rule5:personal", title: "上游账号归因缺失，不按自用免费，已按个人订阅与余额结算", fallback: store.FundingLayerPersonal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			alert, ok := attributionMissingAlert("user", "request", tc.groups)
			if ok != tc.alert {
				t.Fatalf("alert = %v, want %v", ok, tc.alert)
			}
			if !ok {
				return
			}
			if alert.Type != "billing_attribution_missing" || alert.DedupeKey != tc.dedupe || alert.Title != tc.title ||
				alert.UserID != "user" || alert.RequestID != "request" || alert.Details["fallback_layer"] != tc.fallback {
				t.Fatalf("alert = %+v", alert)
			}
			kinds, _ := alert.Details["layer_kinds"].([]string)
			if len(kinds) != len(tc.groups) {
				t.Fatalf("layer kinds = %v", alert.Details["layer_kinds"])
			}
		})
	}
}

func TestEmptySubscriptionLayerAlerts(t *testing.T) {
	t.Parallel()
	layers := []store.EmptySubscriptionLayer{
		{SubscriptionID: "s1", PlanID: "p1", SellerUserID: "seller"},
		{SubscriptionID: "s2", PlanID: "p2"},
	}
	alerts := emptySubscriptionLayerAlerts("buyer", "request", "anthropic", "claude-x", layers)
	if len(alerts) != 2 {
		t.Fatalf("alerts = %+v", alerts)
	}
	first := alerts[0]
	if first.Type != "subscription_layer_empty" || first.Severity != "warning" || first.UserID != "buyer" ||
		first.RequestID != "request" || first.DedupeKey != "subscription_layer_empty:s1:anthropic" ||
		first.Title != "买家已付费，但卖家套餐没有可用的上游账号" {
		t.Fatalf("seller alert = %+v", first)
	}
	want := map[string]any{"subscription_id": "s1", "plan_id": "p1", "seller_user_id": "seller", "provider": "anthropic", "model": "claude-x"}
	if !reflect.DeepEqual(first.Details, want) {
		t.Fatalf("details = %+v", first.Details)
	}
	if alerts[1].Title != "已付费的平台套餐没有可用的上游账号" || alerts[1].Details["seller_user_id"] != "" {
		t.Fatalf("platform alert = %+v", alerts[1])
	}
	many := make([]store.EmptySubscriptionLayer, maxEmptySubscriptionLayerAlerts+3)
	if got := emptySubscriptionLayerAlerts("buyer", "request", "codex", "m", many); len(got) != maxEmptySubscriptionLayerAlerts {
		t.Fatalf("alerts per request = %d", len(got))
	}
	if got := emptySubscriptionLayerAlerts("buyer", "request", "codex", "m", nil); len(got) != 0 {
		t.Fatalf("no layers produced %d alerts", len(got))
	}
}
