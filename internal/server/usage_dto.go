package server

import (
	"context"
	"time"

	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

// usageRequestDTO is an explicit allowlist. Adding a store field never exposes
// upstream credentials or other internal metadata to a personal usage response.
type usageRequestDTO struct {
	ID                      int64
	RequestID               string
	UserID                  string
	DeviceID                string
	APIKeyID                string
	KeyPrefix               string
	ProjectID               *string
	Model                   string
	RequestedModel          *string
	RequestedServiceTier    *string
	ActualServiceTier       *string
	Endpoint                string
	State                   string
	HTTPStatus              *int
	ErrorCode               *string
	RequestedAt             time.Time
	FirstTokenAt            *time.Time
	CompletedAt             *time.Time
	TTFTMillis              *int64
	DurationMillis          *int64
	InputTokens             int64
	CachedInputTokens       int64
	CacheWriteTokens        int64
	CacheWriteTokensPresent bool
	CacheWrite5mTokens      int64
	CacheWrite1hTokens      int64
	CacheWriteTTLPresent    bool
	OutputTokens            int64
	ReasoningTokens         int64
	RequestBytes            int64
	ResponseBytes           int64
	UpstreamRequestID       *string
	PricingRuleVersion      int
	PricingServiceTier      *string
	ContextClass            *string
	PricingFallbackReason   *string
}

// ownerUsageRequestDTO names the serving account for the Owner only. The alias
// is Gateway-owned and, like the masked email, never reaches a personal view.
type ownerUsageRequestDTO struct {
	usageRequestDTO
	UpstreamAccountID   *string `json:"upstream_account_id"`
	UpstreamAlias       *string `json:"upstream_alias"`
	UpstreamDisplayName *string `json:"upstream_display_name"`
	UpstreamMaskedEmail *string `json:"upstream_masked_email"`
}

// usageAccountIdentity is the display metadata of one attributed account.
type usageAccountIdentity struct {
	Alias       string
	DisplayName string
	MaskedEmail string
}

func personalUsageDTO(row store.UsageRequest) usageRequestDTO {
	return usageRequestDTO{
		ID:                      row.ID,
		RequestID:               row.RequestID,
		UserID:                  row.UserID,
		DeviceID:                row.DeviceID,
		APIKeyID:                row.APIKeyID,
		KeyPrefix:               row.KeyPrefix,
		ProjectID:               row.ProjectID,
		Model:                   row.Model,
		RequestedModel:          row.RequestedModel,
		RequestedServiceTier:    row.RequestedServiceTier,
		ActualServiceTier:       row.ActualServiceTier,
		Endpoint:                row.Endpoint,
		State:                   row.State,
		HTTPStatus:              row.HTTPStatus,
		ErrorCode:               row.ErrorCode,
		RequestedAt:             row.RequestedAt,
		FirstTokenAt:            row.FirstTokenAt,
		CompletedAt:             row.CompletedAt,
		TTFTMillis:              row.TTFTMillis,
		DurationMillis:          row.DurationMillis,
		InputTokens:             row.InputTokens,
		CachedInputTokens:       row.CachedInputTokens,
		CacheWriteTokens:        row.CacheWriteTokens,
		CacheWriteTokensPresent: row.CacheWriteTokensPresent,
		CacheWrite5mTokens:      row.CacheWrite5mTokens, CacheWrite1hTokens: row.CacheWrite1hTokens, CacheWriteTTLPresent: row.CacheWriteTTLPresent,
		OutputTokens:          row.OutputTokens,
		ReasoningTokens:       row.ReasoningTokens,
		RequestBytes:          row.RequestBytes,
		ResponseBytes:         row.ResponseBytes,
		UpstreamRequestID:     row.UpstreamRequestID,
		PricingRuleVersion:    row.PricingRuleVersion,
		PricingServiceTier:    row.PricingServiceTier,
		ContextClass:          row.ContextClass,
		PricingFallbackReason: row.PricingFallbackReason,
	}
}

func (s *Server) usageAccountMetadata(ctx context.Context, rows []store.UsageRequest, owner bool) (map[string]usageAccountIdentity, error) {
	result := make(map[string]usageAccountIdentity)
	if !owner {
		return result, nil
	}
	hasAttribution := false
	for _, row := range rows {
		if row.UpstreamAccountID != nil && validUpstreamAccountID(*row.UpstreamAccountID) {
			hasAttribution = true
			break
		}
	}
	if !hasAttribution {
		return result, nil
	}
	for _, provider := range []string{store.UpstreamProviderCodex, store.UpstreamProviderAntigravity, store.UpstreamProviderAnthropic} {
		accounts, err := s.store.WithUpstreamProvider(provider).ListUpstreamAccounts(ctx)
		if err != nil {
			return nil, err
		}
		for _, account := range accounts {
			result[account.ID] = usageAccountIdentity{Alias: account.Alias, DisplayName: account.DisplayName, MaskedEmail: account.MaskedEmail}
		}
	}
	s.completeUsageAccountMetadata(ctx, rows, result)
	return result, nil
}

// Usage can be the first admin page opened after account discovery. Fill only
// missing display metadata without modifying account settings or inventing an
// attribution for old requests. A sidecar failure preserves the durable ID.
// The alias is Gateway-owned, so the sidecar never supplies or replaces it.
func (s *Server) completeUsageAccountMetadata(ctx context.Context, rows []store.UsageRequest, accounts map[string]usageAccountIdentity) {
	missing := make(map[string]bool)
	for _, row := range rows {
		if row.UpstreamAccountID != nil && validUpstreamAccountID(*row.UpstreamAccountID) && accounts[*row.UpstreamAccountID].MaskedEmail == "" {
			missing[*row.UpstreamAccountID] = true
		}
	}
	if len(missing) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, upstreamAccountSyncTimeout)
	defer cancel()
	for _, client := range []*gatewayproxy.Client{s.upstream, s.antigravity, s.anthropic} {
		if client == nil || len(missing) == 0 {
			continue
		}
		listed, err := client.ListUpstreamAccounts(ctx)
		if err != nil {
			continue
		}
		for _, account := range listed {
			if missing[account.ID] {
				identity := accounts[account.ID]
				identity.MaskedEmail = account.MaskedEmail
				if identity.DisplayName == "" {
					identity.DisplayName = account.DisplayName
				}
				accounts[account.ID] = identity
				delete(missing, account.ID)
			}
		}
	}
}

func usageResponseDTO(rows []store.UsageRequest, owner bool, accounts map[string]usageAccountIdentity) any {
	if !owner {
		result := make([]usageRequestDTO, 0, len(rows))
		for _, row := range rows {
			result = append(result, personalUsageDTO(row))
		}
		return result
	}
	result := make([]ownerUsageRequestDTO, 0, len(rows))
	for _, row := range rows {
		dto := ownerUsageRequestDTO{usageRequestDTO: personalUsageDTO(row)}
		if row.UpstreamAccountID != nil && validUpstreamAccountID(*row.UpstreamAccountID) {
			id := *row.UpstreamAccountID
			dto.UpstreamAccountID = &id
			identity := accounts[id]
			dto.UpstreamAlias = nonEmptyString(identity.Alias)
			dto.UpstreamDisplayName = nonEmptyString(identity.DisplayName)
			dto.UpstreamMaskedEmail = nonEmptyString(identity.MaskedEmail)
		}
		result = append(result, dto)
	}
	return result
}

// nonEmptyString keeps an unknown value null in JSON instead of "".
func nonEmptyString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
