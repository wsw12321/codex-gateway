package server

import (
	"context"
	"time"

	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

// maxEmptySubscriptionLayerAlerts bounds the alert writes one request makes.
// A buyer rarely holds more restricted subscriptions than this.
const maxEmptySubscriptionLayerAlerts = 8

// emptySubscriptionLayerAlerts describes, for the Owner, each paid
// subscription that admission dropped because its plan had no account for
// the request: the buyer has allowance left for the model, yet the seller (or
// the Owner, for a platform plan) offers nothing to serve it. One alert per
// subscription and protocol coalesces repeated requests.
func emptySubscriptionLayerAlerts(userID, requestID, provider, model string, layers []store.EmptySubscriptionLayer) []store.CreateAlertParams {
	if len(layers) > maxEmptySubscriptionLayerAlerts {
		layers = layers[:maxEmptySubscriptionLayerAlerts]
	}
	alerts := make([]store.CreateAlertParams, 0, len(layers))
	for _, layer := range layers {
		title := "买家已付费，但卖家套餐没有可用的上游账号"
		seller := layer.SellerUserID
		if seller == "" {
			title = "已付费的平台套餐没有可用的上游账号"
		}
		alerts = append(alerts, store.CreateAlertParams{
			Type: "subscription_layer_empty", Severity: "warning", UserID: userID, RequestID: requestID,
			DedupeKey: "subscription_layer_empty:" + layer.SubscriptionID + ":" + provider,
			Title:     title,
			Details: map[string]any{
				"subscription_id": layer.SubscriptionID, "plan_id": layer.PlanID,
				"seller_user_id": seller, "provider": provider, "model": model,
			},
		})
	}
	return alerts
}

func (s *Server) createEmptySubscriptionLayerAlerts(ctx context.Context, userID, requestID, provider, model string, layers []store.EmptySubscriptionLayer) {
	if len(layers) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	for _, alert := range emptySubscriptionLayerAlerts(userID, requestID, provider, model, layers) {
		_, _ = s.store.CreateAlert(ctx, alert)
	}
}

// allocationLayerKind tolerates layers built without a kind: those are rule 4
// layers, a group or the personal layer.
func allocationLayerKind(group gatewayproxy.AllocationGroup) string {
	switch {
	case group.Kind != "":
		return group.Kind
	case group.AllAccounts:
		return store.FundingLayerPersonal
	default:
		return store.FundingLayerGroup
	}
}

// attributionMissingAlert describes how settlement pays for a request whose
// serving account is unknown, when that is a guess the Owner must see.
//
// Rule 4 (groups and the personal layer) pays from the first layer, which is
// a guess only when there is more than one. Rule 5 (self-use or restricted
// subscription layers present) never assumes self-use: the first group,
// restricted subscription or personal layer pays, and with none of those the
// cost is left uncovered. With a self-use layer the request may have been
// free, so that is always reported; a single paying layer is not a guess.
func attributionMissingAlert(userID, requestID string, groups []gatewayproxy.AllocationGroup) (store.CreateAlertParams, bool) {
	kinds := make([]string, 0, len(groups))
	rule5, self := false, false
	fallback := ""
	for _, group := range groups {
		kind := allocationLayerKind(group)
		kinds = append(kinds, kind)
		switch kind {
		case store.FundingLayerSelf:
			rule5, self = true, true
			continue
		case store.FundingLayerSubscription:
			rule5 = true
		}
		if fallback == "" {
			fallback = kind
		}
	}
	if !rule5 {
		if len(groups) < 2 {
			return store.CreateAlertParams{}, false
		}
		return store.CreateAlertParams{
			Type: "billing_attribution_missing", Severity: "warning", UserID: userID, RequestID: requestID,
			DedupeKey: "billing_attribution_missing", Title: "上游账号归因缺失，已按第一扣费来源结算",
			Details: map[string]any{"funding_rule": 4, "layer_kinds": kinds, "fallback_layer": "first"},
		}, true
	}
	if !self && len(groups) < 2 {
		return store.CreateAlertParams{}, false
	}
	title := "上游账号归因缺失，"
	if self {
		title += "不按自用免费，"
	}
	switch fallback {
	case store.FundingLayerGroup:
		title += "已按第一个群组层结算"
	case store.FundingLayerSubscription:
		title += "已按第一个受限订阅层结算"
	case store.FundingLayerPersonal:
		title += "已按个人订阅与余额结算"
	default:
		fallback = "none"
		title += "没有可结算的扣费层，费用记为未覆盖"
	}
	return store.CreateAlertParams{
		Type: "billing_attribution_missing", Severity: "warning", UserID: userID, RequestID: requestID,
		DedupeKey: "billing_attribution_missing:rule5:" + fallback, Title: title,
		Details: map[string]any{"funding_rule": 5, "layer_kinds": kinds, "fallback_layer": fallback},
	}, true
}

func (s *Server) createAttributionMissingAlert(ctx context.Context, userID, requestID string, groups []gatewayproxy.AllocationGroup) {
	if alert, ok := attributionMissingAlert(userID, requestID, groups); ok {
		_, _ = s.store.CreateAlert(ctx, alert)
	}
}
