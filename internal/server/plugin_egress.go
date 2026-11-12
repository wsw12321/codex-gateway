package server

import (
	"context"
	"fmt"
	"time"

	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

// pluginEgressPushTimeout bounds pushing every binding of one node.
const pluginEgressPushTimeout = 10 * time.Second

// providerAccountClient returns the CPA client of an egress binding provider.
func (s *Server) providerAccountClient(provider string) *gatewayproxy.Client {
	switch provider {
	case store.UpstreamProviderAnthropic:
		return s.anthropic
	case store.UpstreamProviderAntigravity:
		return s.antigravity
	case store.UpstreamProviderCodex:
		return s.upstream
	}
	return nil
}

// pushPluginEgress applies changed bindings to CPA at once, in plugin mode.
// It is an optimisation, not the guarantee: the plugin checks each account's
// route with the Gateway on every request and never uses an account whose
// credential carries anything else, and a background pass in the plugin
// writes every credential's route at start-up and once a minute, which bounds
// how long CPA's own refreshes can use an old route. A failed push only means
// the account is skipped once while the plugin writes the route itself.
func (s *Server) pushPluginEgress(ctx context.Context, bindings []store.EgressBinding) {
	if !s.config.UsesCPAPlugin() || len(bindings) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, pluginEgressPushTimeout)
	defer cancel()
	for _, binding := range bindings {
		client := s.providerAccountClient(binding.Provider)
		if client == nil {
			continue
		}
		if err := client.SyncAccountEgress(ctx, binding.AccountID); err != nil && s.logger != nil {
			s.logger.Warn("plugin egress push failed", "provider", binding.Provider, "account", binding.AccountID, "error", fmt.Sprintf("%T", err))
		}
	}
}

// nodeBindings lists the bindings that route through node.
func nodeBindings(state store.EgressState, node string) []store.EgressBinding {
	var bound []store.EgressBinding
	for _, binding := range state.Bindings {
		if binding.NodeID == node {
			bound = append(bound, binding)
		}
	}
	return bound
}
