package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Plan provider scope modes (migration 0053).
const (
	PlanProviderScopeAll      = "all"
	PlanProviderScopeSelected = "selected"
)

// MaxPlanProviderScopeAccounts bounds a selected scope. Each provider's part
// becomes one funding layer, which CPA accepts with at most
// MaxUpstreamAllocationCandidates accounts.
const MaxPlanProviderScopeAccounts = 3 * MaxUpstreamAllocationCandidates

// PlanProviderScopeAccount is one listed account. Usable is false once the
// account no longer belongs to the plan's seller, is released or is
// suspended by the Owner: it stays listed but serves no subscription.
type PlanProviderScopeAccount struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Label    string `json:"label"`
	Usable   bool   `json:"usable"`
}

// PlanProviderScope is which accounts a plan's subscriptions pay for.
type PlanProviderScope struct {
	PlanID    string                     `json:"plan_id"`
	Mode      string                     `json:"mode"`
	Accounts  []PlanProviderScopeAccount `json:"accounts"`
	Revision  int64                      `json:"revision"`
	UpdatedAt time.Time                  `json:"updated_at"`
}

type SetPlanProviderScopeParams struct {
	PlanID string
	// SellerUserID is the plan's seller; empty for a platform plan. A plan of
	// another seller reads as not found.
	SellerUserID     string
	Mode             string
	AccountIDs       []string
	ExpectedRevision int64
	ActorUserID      string
	ActorSessionID   string
	RequestID        string
	SourceIP         string
	At               time.Time
}

// PlanProviderScopes returns the scope of every plan of a seller that is not
// deleted, keyed by plan ID. sellerUserID empty means platform plans. A plan
// without a scope row has the default scope (all, revision 0).
func (s *Store) PlanProviderScopes(ctx context.Context, sellerUserID string) (map[string]PlanProviderScope, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.id::text, COALESCE(sc.mode,'all'), COALESCE(sc.revision,0),
			COALESCE(sc.updated_at, p.created_at)
		FROM billing_plans p LEFT JOIN billing_plan_provider_scopes sc ON sc.plan_id=p.id
		WHERE p.deleted_at IS NULL AND p.seller_user_id IS NOT DISTINCT FROM $1::uuid`, valueOrNil(sellerUserID))
	if err != nil {
		return nil, mapDBError("list plan provider scopes", err)
	}
	scopes := make(map[string]PlanProviderScope)
	for rows.Next() {
		var scope PlanProviderScope
		if err := rows.Scan(&scope.PlanID, &scope.Mode, &scope.Revision, &scope.UpdatedAt); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan plan provider scope: %w", err)
		}
		scope.Accounts = []PlanProviderScopeAccount{}
		scopes[scope.PlanID] = scope
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	accounts, err := s.db.QueryContext(ctx, `SELECT pa.plan_id::text, a.id, a.provider,
			COALESCE(NULLIF(a.alias,''), NULLIF(a.display_name,''), NULLIF(a.masked_email,''), a.id),
			a.owner_user_id IS NOT DISTINCT FROM p.seller_user_id AND NOT a.admin_suspended AND a.credential_removed_at IS NULL
		FROM billing_plan_provider_accounts pa
		JOIN billing_plans p ON p.id=pa.plan_id
		JOIN upstream_accounts a ON a.id=pa.account_id
		WHERE p.deleted_at IS NULL AND p.seller_user_id IS NOT DISTINCT FROM $1::uuid
		ORDER BY pa.plan_id, a.provider, a.id`, valueOrNil(sellerUserID))
	if err != nil {
		return nil, mapDBError("list plan provider scope accounts", err)
	}
	defer accounts.Close()
	for accounts.Next() {
		var planID string
		var account PlanProviderScopeAccount
		if err := accounts.Scan(&planID, &account.ID, &account.Provider, &account.Label, &account.Usable); err != nil {
			return nil, fmt.Errorf("scan plan provider scope account: %w", err)
		}
		if scope, ok := scopes[planID]; ok {
			scope.Accounts = append(scope.Accounts, account)
			scopes[planID] = scope
		}
	}
	return scopes, accounts.Err()
}

func normalizePlanProviderScope(params *SetPlanProviderScopeParams) error {
	params.Mode = strings.TrimSpace(params.Mode)
	if !validUUIDText(params.PlanID) || strings.TrimSpace(params.ActorUserID) == "" || params.ExpectedRevision < 0 {
		return fmt.Errorf("%w: invalid plan provider scope request", ErrInvalid)
	}
	if params.SellerUserID != "" && !validUUIDText(params.SellerUserID) {
		return fmt.Errorf("%w: invalid plan seller", ErrInvalid)
	}
	seen := make(map[string]bool, len(params.AccountIDs))
	for _, id := range params.AccountIDs {
		if !upstreamAccountIDPattern.MatchString(id) || seen[id] {
			return fmt.Errorf("%w: invalid or duplicate scope account", ErrInvalid)
		}
		seen[id] = true
	}
	switch params.Mode {
	case PlanProviderScopeAll:
		if len(params.AccountIDs) != 0 {
			return fmt.Errorf("%w: an unrestricted scope lists no accounts", ErrInvalid)
		}
	case PlanProviderScopeSelected:
		if len(params.AccountIDs) == 0 || len(params.AccountIDs) > MaxPlanProviderScopeAccounts {
			return fmt.Errorf("%w: a selected scope lists 1 to %d accounts", ErrInvalid, MaxPlanProviderScopeAccounts)
		}
	default:
		return fmt.Errorf("%w: invalid plan provider scope mode", ErrInvalid)
	}
	sort.Strings(params.AccountIDs)
	return nil
}

// SetPlanProviderScope replaces a plan's provider scope. It does not touch the
// plan's version or its renewal bindings: the next admission of every
// subscription bought from the plan resolves its accounts from the new scope.
// Every listed account must belong to the seller and still hold a credential;
// each provider may list at most MaxUpstreamAllocationCandidates accounts.
func (s *Store) SetPlanProviderScope(ctx context.Context, params SetPlanProviderScopeParams) (PlanProviderScope, error) {
	if err := normalizePlanProviderScope(&params); err != nil {
		return PlanProviderScope{}, err
	}
	if params.At.IsZero() {
		params.At = s.now().UTC()
	} else {
		params.At = params.At.UTC()
	}
	var previousMode string
	var revision int64
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		var deleted sql.NullTime
		var seller sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT deleted_at, seller_user_id::text FROM billing_plans WHERE id=$1::uuid FOR SHARE`, params.PlanID).Scan(&deleted, &seller)
		if errors.Is(err, sql.ErrNoRows) || err == nil && (deleted.Valid || seller.String != params.SellerUserID) {
			return ErrNotFound
		}
		if err != nil {
			return mapDBError("lock plan for provider scope", err)
		}
		// A plan created after migration 0053 has no scope row until its first
		// change; it reads as the default scope at revision 0.
		created, err := tx.ExecContext(ctx, `INSERT INTO billing_plan_provider_scopes(plan_id,mode,revision,updated_at)
			VALUES ($1::uuid,'all',1,$2) ON CONFLICT (plan_id) DO NOTHING`, params.PlanID, params.At)
		if err != nil {
			return mapDBError("create plan provider scope", err)
		}
		if err := tx.QueryRowContext(ctx, `SELECT mode, revision FROM billing_plan_provider_scopes WHERE plan_id=$1::uuid FOR UPDATE`,
			params.PlanID).Scan(&previousMode, &revision); err != nil {
			return mapDBError("lock plan provider scope", err)
		}
		current := revision
		if inserted, err := created.RowsAffected(); err == nil && inserted == 1 {
			current = 0
		}
		if params.ExpectedRevision != current {
			return fmt.Errorf("plan provider scope changed: %w", ErrConflict)
		}
		if len(params.AccountIDs) > 0 {
			rows, err := tx.QueryContext(ctx, `SELECT provider, count(*) FROM upstream_accounts
				WHERE id = ANY($1::text[]) AND owner_user_id IS NOT DISTINCT FROM $2::uuid AND credential_removed_at IS NULL
				GROUP BY provider`, params.AccountIDs, valueOrNil(params.SellerUserID))
			if err != nil {
				return mapDBError("check plan provider scope accounts", err)
			}
			total := 0
			for rows.Next() {
				var provider string
				var count int
				if err := rows.Scan(&provider, &count); err != nil {
					_ = rows.Close()
					return fmt.Errorf("scan plan provider scope accounts: %w", err)
				}
				if count > MaxUpstreamAllocationCandidates {
					_ = rows.Close()
					return fmt.Errorf("%w: a scope lists at most %d accounts per provider", ErrInvalid, MaxUpstreamAllocationCandidates)
				}
				total += count
			}
			if err := rows.Close(); err != nil {
				return err
			}
			if total != len(params.AccountIDs) {
				return fmt.Errorf("%w: every scope account must belong to the plan's seller", ErrInvalid)
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM billing_plan_provider_accounts WHERE plan_id=$1::uuid`, params.PlanID); err != nil {
			return mapDBError("clear plan provider scope accounts", err)
		}
		if len(params.AccountIDs) > 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO billing_plan_provider_accounts(plan_id,account_id)
				SELECT $1::uuid, unnest($2::text[])`, params.PlanID, params.AccountIDs); err != nil {
				return mapDBError("save plan provider scope accounts", err)
			}
		}
		if err := tx.QueryRowContext(ctx, `UPDATE billing_plan_provider_scopes
			SET mode=$2, revision=revision+1, updated_at=$3, updated_by_user_id=$4::uuid
			WHERE plan_id=$1::uuid RETURNING revision`, params.PlanID, params.Mode, params.At, params.ActorUserID).Scan(&revision); err != nil {
			return mapDBError("save plan provider scope", err)
		}
		return appendBillingAuditTx(ctx, tx, BillingWriteParams{ActorUserID: params.ActorUserID, ActorSessionID: params.ActorSessionID,
			RequestID: params.RequestID, SourceIP: params.SourceIP, At: params.At},
			"billing.plan_provider_scope_changed", "billing_plan", params.PlanID, map[string]any{
				"previous_mode": previousMode, "mode": params.Mode, "account_count": len(params.AccountIDs), "revision": revision,
			})
	})
	if err != nil {
		return PlanProviderScope{}, err
	}
	scopes, err := s.PlanProviderScopes(ctx, params.SellerUserID)
	if err != nil {
		return PlanProviderScope{}, err
	}
	scope, ok := scopes[params.PlanID]
	if !ok {
		return PlanProviderScope{}, ErrNotFound
	}
	return scope, nil
}

// SubscriptionProviderScope describes, for the subscriber, which accounts a
// subscription pays for now. Restricted is true when it forms its own funding
// layer; AccountCounts are the accounts usable now, per provider.
type SubscriptionProviderScope struct {
	PlanID        string         `json:"plan_id"`
	Mode          string         `json:"mode"`
	Seller        bool           `json:"seller"`
	Restricted    bool           `json:"restricted"`
	AccountCounts map[string]int `json:"account_counts"`
}

// SubscriptionProviderScopes returns the provider scope of each of the user's
// subscriptions bought from a plan, keyed by subscription ID.
func (s *Store) SubscriptionProviderScopes(ctx context.Context, userID string) (map[string]SubscriptionProviderScope, error) {
	if !validUUIDText(userID) {
		return nil, fmt.Errorf("%w: invalid user", ErrInvalid)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT s.id::text, p.id::text, COALESCE(sc.mode,'all'), p.seller_user_id IS NOT NULL,
			p.seller_user_id IS NOT NULL OR (`+marketplaceTenantSQL+` AND COALESCE(sc.mode,'all')='selected')
		FROM billing_subscriptions s
		JOIN billing_plans p ON p.id=s.source_plan_id
		LEFT JOIN billing_plan_provider_scopes sc ON sc.plan_id=p.id
		WHERE s.user_id=$1::uuid`, userID)
	if err != nil {
		return nil, mapDBError("list subscription provider scopes", err)
	}
	scopes := make(map[string]SubscriptionProviderScope)
	for rows.Next() {
		var id string
		var scope SubscriptionProviderScope
		if err := rows.Scan(&id, &scope.PlanID, &scope.Mode, &scope.Seller, &scope.Restricted); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan subscription provider scope: %w", err)
		}
		scope.AccountCounts = map[string]int{}
		scopes[id] = scope
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for id, scope := range scopes {
		if !scope.Restricted {
			continue
		}
		for _, provider := range []string{UpstreamProviderCodex, UpstreamProviderAnthropic, UpstreamProviderAntigravity} {
			var count int
			err := s.withTx(ctx, &sql.TxOptions{ReadOnly: true}, func(tx *sql.Tx) error {
				ids, err := resolvePlanPoolTx(ctx, tx, scope.PlanID, provider)
				count = len(ids)
				return err
			})
			if err != nil {
				return nil, err
			}
			if count > 0 {
				scope.AccountCounts[provider] = count
			}
		}
		scopes[id] = scope
	}
	return scopes, nil
}

// SelfUseEnabled reports the user's self-use switch.
func (s *Store) SelfUseEnabled(ctx context.Context, userID string) (bool, error) {
	var enabled bool
	err := s.db.QueryRowContext(ctx, `SELECT self_use_enabled FROM billing_accounts WHERE user_id=$1::uuid`, userID).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	return enabled, mapDBError("read self-use switch", err)
}

type SetSelfUseParams struct {
	UserID         string
	Enabled        bool
	ActorSessionID string
	RequestID      string
	SourceIP       string
	At             time.Time
}

// SetSelfUse turns the user's self-use layer on or off. It takes the billing
// account lock admission takes, so a concurrent admission sees either state;
// requests already admitted keep their layers.
func (s *Store) SetSelfUse(ctx context.Context, params SetSelfUseParams) (bool, error) {
	if !validUUIDText(params.UserID) {
		return false, fmt.Errorf("%w: invalid user", ErrInvalid)
	}
	if params.At.IsZero() {
		params.At = s.now().UTC()
	} else {
		params.At = params.At.UTC()
	}
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if err := lockBillingAccountTx(ctx, tx, params.UserID); err != nil {
			return err
		}
		var previous bool
		if err := tx.QueryRowContext(ctx, `SELECT self_use_enabled FROM billing_accounts WHERE user_id=$1::uuid`, params.UserID).Scan(&previous); err != nil {
			return mapDBError("read self-use switch", err)
		}
		if previous == params.Enabled {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE billing_accounts SET self_use_enabled=$2, updated_at=$3 WHERE user_id=$1::uuid`,
			params.UserID, params.Enabled, params.At); err != nil {
			return mapDBError("save self-use switch", err)
		}
		return appendBillingAuditTx(ctx, tx, BillingWriteParams{ActorUserID: params.UserID, ActorSessionID: params.ActorSessionID,
			RequestID: params.RequestID, SourceIP: params.SourceIP, At: params.At},
			"billing.self_use_changed", "user", params.UserID, map[string]any{"enabled": params.Enabled})
	})
	return params.Enabled, err
}

// PlanScopeCandidates lists the accounts a seller may put in a plan scope:
// every account the seller owns that still holds a credential, on every
// provider. sellerUserID empty means platform accounts. Usable is false while
// the Owner suspends the account.
func (s *Store) PlanScopeCandidates(ctx context.Context, sellerUserID string) ([]PlanProviderScopeAccount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.id, a.provider,
			COALESCE(NULLIF(a.alias,''), NULLIF(a.display_name,''), NULLIF(a.masked_email,''), a.id), NOT a.admin_suspended
		FROM upstream_accounts a
		WHERE a.owner_user_id IS NOT DISTINCT FROM $1::uuid AND a.credential_removed_at IS NULL
		ORDER BY a.provider, a.id`, valueOrNil(sellerUserID))
	if err != nil {
		return nil, mapDBError("list plan scope candidates", err)
	}
	defer rows.Close()
	result := make([]PlanProviderScopeAccount, 0)
	for rows.Next() {
		var account PlanProviderScopeAccount
		if err := rows.Scan(&account.ID, &account.Provider, &account.Label, &account.Usable); err != nil {
			return nil, fmt.Errorf("scan plan scope candidate: %w", err)
		}
		result = append(result, account)
	}
	return result, rows.Err()
}
