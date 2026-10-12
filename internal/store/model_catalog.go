package store

import (
	"context"
	"fmt"
)

// ListUsageModels preserves historical model choices after catalog edits and
// history aggregation, including costs retained only in the ledger. An empty
// user ID is reserved for the owner global view.
func (s *Store) ListUsageModels(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT model FROM (
  SELECT DISTINCT model FROM usage_requests WHERE ($1 = '' OR user_id::text = $1)
  UNION SELECT DISTINCT model FROM usage_daily WHERE ($1 = '' OR user_id::text = $1)
  UNION SELECT DISTINCT model FROM usage_monthly WHERE ($1 = '' OR user_id::text = $1)
  UNION SELECT DISTINCT COALESCE(actual_model, model, '') AS model
  FROM billing_ledger_entries WHERE entry_type = 'usage_charge' AND ($1 = '' OR user_id::text = $1)
 ) candidates WHERE model <> '' ORDER BY model`, userID)
	if err != nil {
		return nil, fmt.Errorf("list historical usage models: %w", err)
	}
	defer rows.Close()
	values := make([]string, 0)
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			return nil, fmt.Errorf("scan historical usage model: %w", err)
		}
		values = append(values, model)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read historical usage models: %w", err)
	}
	return values, nil
}
