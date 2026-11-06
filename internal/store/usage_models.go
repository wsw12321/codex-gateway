package store

import (
	"context"
	"fmt"
	"math/big"
	"strings"
)

// maxUsageModelSummaries bounds one breakdown; totals still cover every model.
const maxUsageModelSummaries = 200

// UsageModelSummary is one model's part of a usage window. ChargedUSD is the
// settled usage_charge amount, which already includes model multipliers.
type UsageModelSummary struct {
	Model             string
	RequestCount      int64
	ErrorCount        int64
	InProgressCount   int64
	InputTokens       int64
	CachedInputTokens int64
	OutputTokens      int64
	ChargedUSD        string
}

// UsageModelBreakdown groups the requests of one usage filter by model. The
// totals are exact sums over all models, including any beyond the row limit.
type UsageModelBreakdown struct {
	Models          []UsageModelSummary
	TotalRequests   int64
	TotalTokens     int64
	TotalChargedUSD string
	Truncated       bool
}

// SummarizeUsageByModel applies the same filter and ledger join as
// SummarizeUsageRequests, so the per-model rows add up to that summary.
// Rows are ordered by settled charge, then request count, then model name.
func (s *Store) SummarizeUsageByModel(ctx context.Context, filter UsageFilter) (UsageModelBreakdown, error) {
	where, args := usageFilterWhere(filter, "u")
	// #nosec G202 -- the WHERE fragments are fixed SQL with numbered placeholders; values are bound.
	query := `SELECT u.model,
		count(*)::bigint,
		count(*) FILTER (WHERE u.state IN ('failed', 'cancelled') OR u.http_status >= 400 OR u.error_code IS NOT NULL)::bigint,
		count(*) FILTER (WHERE u.state = 'in_progress')::bigint,
		COALESCE(sum(u.input_tokens), 0)::bigint,
		COALESCE(sum(u.cached_input_tokens), 0)::bigint,
		COALESCE(sum(u.output_tokens), 0)::bigint,
		COALESCE(sum(l.charged_usd), 0::numeric)::text
		FROM usage_requests u
		LEFT JOIN billing_ledger_entries l
			ON l.request_id = u.request_id AND l.entry_type = 'usage_charge'
		WHERE ` + strings.Join(where, " AND ") + `
		GROUP BY u.model
		ORDER BY COALESCE(sum(l.charged_usd), 0) DESC, count(*) DESC, u.model`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return UsageModelBreakdown{}, mapDBError("summarize usage by model", err)
	}
	defer rows.Close()
	result := UsageModelBreakdown{Models: []UsageModelSummary{}}
	total := new(big.Rat)
	for rows.Next() {
		var value UsageModelSummary
		if err := rows.Scan(&value.Model, &value.RequestCount, &value.ErrorCount, &value.InProgressCount,
			&value.InputTokens, &value.CachedInputTokens, &value.OutputTokens, &value.ChargedUSD); err != nil {
			return UsageModelBreakdown{}, fmt.Errorf("scan usage model summary: %w", err)
		}
		charged, ok := new(big.Rat).SetString(value.ChargedUSD)
		if !ok {
			return UsageModelBreakdown{}, fmt.Errorf("usage model summary charge %q is not a decimal", value.ChargedUSD)
		}
		total.Add(total, charged)
		result.TotalRequests += value.RequestCount
		result.TotalTokens += value.InputTokens + value.OutputTokens
		if len(result.Models) < maxUsageModelSummaries {
			result.Models = append(result.Models, value)
		} else {
			result.Truncated = true
		}
	}
	if err := rows.Err(); err != nil {
		return UsageModelBreakdown{}, mapDBError("summarize usage by model", err)
	}
	result.TotalChargedUSD = total.FloatString(12)
	return result, nil
}
