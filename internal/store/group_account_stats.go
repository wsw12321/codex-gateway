package store

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
)

type GroupUsagePeriodSummary struct {
	ID              string    `json:"id"`
	StartsAt        time.Time `json:"starts_at"`
	EndsAt          time.Time `json:"ends_at"`
	UsedUSD         string    `json:"used_usd"`
	GroupChargedUSD string    `json:"group_charged_usd"`
}

type GroupAccountStat struct {
	AccountID       string  `json:"account_id"`
	Provider        string  `json:"provider"`
	DisplayName     string  `json:"display_name,omitempty"`
	EmailMasked     string  `json:"email_masked,omitempty"`
	GroupChargedUSD string  `json:"group_charged_usd"`
	ActualCostUSD   string  `json:"actual_cost_usd"`
	Share           *string `json:"share"`
	DataComplete    bool    `json:"data_complete"`
}

func (s *Store) ListGroupUsagePeriods(ctx context.Context, groupID string, limit int) ([]GroupUsagePeriodSummary, error) {
	gid, err := canonicalGroupID(groupID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_groups WHERE id=$1)`, gid).Scan(&exists); err != nil {
		return nil, mapDBError("check group periods group", err)
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, `SELECT p.id,p.starts_at,p.ends_at,p.used_usd::text,
      COALESCE((SELECT sum(l.group_charged_usd) FROM billing_ledger_entries l WHERE l.group_id=p.group_id AND l.group_period_id=p.id AND l.entry_type='usage_charge'),0)::numeric(30,12)::text
      FROM group_usage_periods p WHERE p.group_id=$1 ORDER BY p.starts_at DESC,p.id DESC LIMIT $2`, gid, limit)
	if err != nil {
		return nil, mapDBError("list group usage periods", err)
	}
	defer rows.Close()
	result := make([]GroupUsagePeriodSummary, 0)
	for rows.Next() {
		var v GroupUsagePeriodSummary
		if err := rows.Scan(&v.ID, &v.StartsAt, &v.EndsAt, &v.UsedUSD, &v.GroupChargedUSD); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}

func parsePeriodIDs(ids []string) ([]string, error) {
	if len(ids) == 0 || len(ids) > 100 {
		return nil, fmt.Errorf("%w: select 1-100 periods", ErrInvalid)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		v, e := canonicalGroupID(strings.TrimSpace(id))
		if e != nil || seen[v] {
			return nil, fmt.Errorf("%w: invalid period id", ErrInvalid)
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out, nil
}

func (s *Store) GroupAccountStats(ctx context.Context, groupID string, periodIDs []string) ([]GroupAccountStat, error) {
	gid, err := canonicalGroupID(groupID)
	if err != nil {
		return nil, err
	}
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_groups WHERE id=$1)`, gid).Scan(&exists); err != nil {
		return nil, mapDBError("check group statistics group", err)
	}
	if !exists {
		return nil, ErrNotFound
	}
	periodIDs, err = parsePeriodIDs(periodIDs)
	if err != nil {
		return nil, err
	}
	encoded, _ := marshalStringArray(periodIDs)
	var incomplete bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM group_usage_periods p WHERE p.group_id=$1 AND p.id IN (SELECT value::uuid FROM jsonb_array_elements_text($2::jsonb)) AND p.used_usd>0 AND NOT EXISTS (SELECT 1 FROM billing_ledger_entries l WHERE l.group_period_id=p.id))`, gid, encoded).Scan(&incomplete); err != nil {
		return nil, mapDBError("check group account statistics completeness", err)
	}
	rows, err := s.db.QueryContext(ctx, `WITH selected AS (SELECT p.id,p.starts_at,p.ends_at FROM group_usage_periods p WHERE p.group_id=$1 AND p.id IN (SELECT value::uuid FROM jsonb_array_elements_text($2::jsonb))),
    nums AS (SELECT l.upstream_account_id account_id,COALESCE(sum(l.group_charged_usd),0)::numeric(30,12)::text charged FROM billing_ledger_entries l WHERE l.group_id=$1 AND l.group_period_id IN (SELECT id FROM selected) AND l.entry_type='usage_charge' GROUP BY l.upstream_account_id),
    dens AS (SELECT l.upstream_account_id account_id,COALESCE(sum(l.actual_cost_usd),0)::numeric(30,12)::text cost FROM billing_ledger_entries l WHERE l.entry_type='usage_charge' AND l.upstream_account_id IS NOT NULL AND EXISTS (SELECT 1 FROM selected sw WHERE COALESCE(l.usage_requested_at,l.created_at)>=sw.starts_at AND COALESCE(l.usage_requested_at,l.created_at)<sw.ends_at) GROUP BY l.upstream_account_id),
    combined AS (SELECT COALESCE(n.account_id,d.account_id) account_id,COALESCE(n.charged,'0') charged,COALESCE(d.cost,'0') cost FROM nums n FULL OUTER JOIN dens d ON d.account_id=n.account_id)
    SELECT c.account_id,a.provider,a.display_name,a.masked_email,c.charged,c.cost FROM combined c JOIN upstream_accounts a ON a.id=c.account_id ORDER BY a.provider,a.id`, gid, encoded)
	if err != nil {
		return nil, mapDBError("calculate group account statistics", err)
	}
	defer rows.Close()
	result := []GroupAccountStat{}
	for rows.Next() {
		var id, provider, displayName, emailMasked, charged, cost string
		if err := rows.Scan(&id, &provider, &displayName, &emailMasked, &charged, &cost); err != nil {
			return nil, err
		}
		v := GroupAccountStat{AccountID: id, Provider: provider, GroupChargedUSD: charged, ActualCostUSD: cost, DisplayName: displayName, EmailMasked: emailMasked, DataComplete: !incomplete}
		if r, ok := new(big.Rat).SetString(cost); ok && r.Sign() > 0 {
			n, _ := new(big.Rat).SetString(charged)
			q := new(big.Rat).Quo(n, r).FloatString(12)
			v.Share = &q
		}
		result = append(result, v)
	}
	return result, rows.Err()
}
