package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// SetGroupAccountUsageVisibilityParams toggles whether members may read the
// usage windows of the accounts in the group's selected account pools.
type SetGroupAccountUsageVisibilityParams struct {
	BillingWriteParams
	GroupID string
	Enabled bool
}

// SetGroupAccountUsageVisibility is an idempotent group write. The resulting
// state is part of the action, so replaying an operation ID with the opposite
// value is a conflict rather than a silent no-op.
func (s *Store) SetGroupAccountUsageVisibility(ctx context.Context, params SetGroupAccountUsageVisibilityParams) (Group, error) {
	var err error
	params.GroupID, err = canonicalGroupID(strings.TrimSpace(params.GroupID))
	if err != nil {
		return Group{}, err
	}
	params.At = normalizedBillingTime(params.At, s.now)
	action := "account_usage.disable"
	if params.Enabled {
		action = "account_usage.enable"
	}
	return s.groupWrite(ctx, params.BillingWriteParams, action, []string{params.GroupID}, func(tx *sql.Tx) (Group, error) {
		if err := lockGroupTx(ctx, tx, params.GroupID); err != nil {
			return Group{}, err
		}
		g, err := rollGroupTx(ctx, tx, params.GroupID, params.At)
		if err != nil {
			return Group{}, err
		}
		if g.ArchivedAt != nil {
			return Group{}, fmt.Errorf("%w: group is archived", ErrConflict)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE user_groups SET account_usage_visible=$2,updated_at=$3 WHERE id=$1`, params.GroupID, params.Enabled, params.At); err != nil {
			return Group{}, mapDBError("update group account usage visibility", err)
		}
		return groupDetailTx(ctx, tx, params.GroupID)
	})
}

// GroupAccountUsageTarget is one account whose usage windows a member may read
// through one of their groups.
type GroupAccountUsageTarget struct {
	GroupID   string
	GroupName string
	Provider  string
	AccountID string
	Alias     string
}

// ListGroupAccountUsageTargets resolves the accounts a member may inspect, in
// membership priority order. Only explicitly selected pools qualify, so an
// "all accounts" pool never exposes the provider inventory. Archived and
// expired groups are excluded, as is Antigravity, which has no five-hour or
// weekly window.
func (s *Store) ListGroupAccountUsageTargets(ctx context.Context, userID string) ([]GroupAccountUsageTarget, error) {
	uid, err := canonicalGroupID(strings.TrimSpace(userID))
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT g.id,g.name,ga.provider,a.id,a.alias
		FROM group_memberships gm
		JOIN user_groups g ON g.id=gm.group_id
		JOIN group_upstream_account_policies p ON p.group_id=g.id AND p.strategy='selected'
		JOIN group_upstream_accounts ga ON ga.group_id=p.group_id AND ga.provider=p.provider
		JOIN upstream_accounts a ON a.id=ga.upstream_account_id AND a.provider=ga.provider
		WHERE gm.user_id=$1 AND g.account_usage_visible AND g.archived_at IS NULL
			AND (g.expires_at IS NULL OR g.expires_at>$2) AND p.provider IN ($3,$4)
		ORDER BY gm.priority,g.id,CASE p.provider WHEN $3 THEN 0 ELSE 1 END,a.id`,
		uid, s.now().UTC(), UpstreamProviderCodex, UpstreamProviderAnthropic)
	if err != nil {
		return nil, mapDBError("list group account usage accounts", err)
	}
	defer rows.Close()
	result := []GroupAccountUsageTarget{}
	for rows.Next() {
		var value GroupAccountUsageTarget
		if err := rows.Scan(&value.GroupID, &value.GroupName, &value.Provider, &value.AccountID, &value.Alias); err != nil {
			return nil, fmt.Errorf("scan group account usage account: %w", err)
		}
		result = append(result, value)
	}
	return result, rows.Err()
}
