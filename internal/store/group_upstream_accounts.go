package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

type GroupUpstreamAccountPolicy struct {
	GroupID    string    `json:"group_id"`
	Provider   string    `json:"provider"`
	Strategy   string    `json:"strategy"`
	Mode       string    `json:"mode,omitempty"`
	AccountIDs []string  `json:"account_ids"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type SetGroupUpstreamAccountsParams struct {
	BillingWriteParams
	GroupID    string
	Provider   string
	Strategy   string
	AccountIDs []string
}

func normalizeGroupUpstreamParams(p SetGroupUpstreamAccountsParams) (SetGroupUpstreamAccountsParams, error) {
	var err error
	p.GroupID, err = canonicalGroupID(strings.TrimSpace(p.GroupID))
	if err != nil {
		return p, err
	}
	p.Provider = strings.ToLower(strings.TrimSpace(p.Provider))
	switch p.Provider {
	case UpstreamProviderCodex, UpstreamProviderAntigravity, UpstreamProviderAnthropic:
	default:
		return p, fmt.Errorf("%w: invalid upstream provider", ErrInvalid)
	}
	p.Strategy = strings.ToLower(strings.TrimSpace(p.Strategy))
	if p.Strategy == "" {
		p.Strategy = "all"
	}
	if p.Strategy != "all" && p.Strategy != "selected" {
		return p, fmt.Errorf("%w: invalid group upstream strategy", ErrInvalid)
	}
	if p.Strategy == "all" && len(p.AccountIDs) != 0 {
		return p, fmt.Errorf("%w: all strategy cannot include accounts", ErrInvalid)
	}
	seen := map[string]bool{}
	for i, id := range p.AccountIDs {
		id, err = normalizeUpstreamAccountID(id)
		if err != nil || id == "" || seen[id] {
			return p, fmt.Errorf("%w: invalid or duplicate upstream account", ErrInvalid)
		}
		seen[id] = true
		p.AccountIDs[i] = id
	}
	sort.Strings(p.AccountIDs)
	if p.AccountIDs == nil {
		p.AccountIDs = []string{}
	}
	return p, nil
}

// SetGroupUpstreamAccounts replaces a group's provider-specific account scope.
// Missing policy rows mean all accounts; a selected policy with an empty list
// deliberately disables that provider for the group.
func (s *Store) SetGroupUpstreamAccounts(ctx context.Context, p SetGroupUpstreamAccountsParams) (GroupUpstreamAccountPolicy, error) {
	var result GroupUpstreamAccountPolicy
	var err error
	p, err = normalizeGroupUpstreamParams(p)
	if err != nil {
		return result, err
	}
	if err := validateBillingWrite(p.BillingWriteParams); err != nil {
		return result, err
	}
	p.At = normalizedBillingTime(p.At, s.now)
	if p.ActorUserID == "" {
		return result, fmt.Errorf("%w: actor required", ErrInvalid)
	}
	fingerprint := billingOperationFingerprint("group_upstream_accounts", p.GroupID, p.Provider, p.Strategy, strings.Join(p.AccountIDs, "\x00"), strings.TrimSpace(p.Reason))
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if err := lockGroupTx(ctx, tx, p.GroupID); err != nil {
			return err
		}
		var archived sql.NullTime
		if err := tx.QueryRowContext(ctx, `SELECT archived_at FROM user_groups WHERE id=$1`, p.GroupID).Scan(&archived); err != nil {
			return mapDBError("read group upstream status", err)
		}
		if archived.Valid {
			return fmt.Errorf("%w: group is archived", ErrConflict)
		}
		if len(p.AccountIDs) > 0 {
			encoded := marshalStringSliceJSON(p.AccountIDs)
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM upstream_accounts
				WHERE provider=$1 AND id IN (SELECT value FROM jsonb_array_elements_text($2::jsonb))`, p.Provider, encoded).Scan(&count); err != nil {
				return mapDBError("validate group upstream accounts", err)
			}
			if count != len(p.AccountIDs) {
				return fmt.Errorf("%w: upstream account not found for provider", ErrNotFound)
			}
		}
		inserted, err := tx.ExecContext(ctx, `INSERT INTO group_operations(operation_id,actor_user_id,action,request_fingerprint,created_at)
			VALUES($1,$2,'group_upstream_accounts',$3,$4) ON CONFLICT DO NOTHING`, p.OperationID, p.ActorUserID, fingerprint, p.At)
		if err != nil {
			return mapDBError("claim group upstream operation", err)
		}
		n, _ := inserted.RowsAffected()
		if n == 0 {
			var actor, action string
			var stored, encoded []byte
			if err := tx.QueryRowContext(ctx, `SELECT actor_user_id,action,request_fingerprint,response FROM group_operations WHERE operation_id=$1 FOR UPDATE`, p.OperationID).Scan(&actor, &action, &stored, &encoded); err != nil {
				return mapDBError("read group upstream operation", err)
			}
			if actor != p.ActorUserID || action != "group_upstream_accounts" || string(stored) != string(fingerprint) || len(encoded) == 0 {
				return fmt.Errorf("%w: group operation replay mismatch", ErrConflict)
			}
			return json.Unmarshal(encoded, &result)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO group_upstream_account_policies(group_id,provider,strategy,created_at,updated_at)
			VALUES($1,$2,$3,$4,$4) ON CONFLICT(group_id,provider)
			DO UPDATE SET strategy=EXCLUDED.strategy,updated_at=EXCLUDED.updated_at`, p.GroupID, p.Provider, p.Strategy, p.At); err != nil {
			return mapDBError("save group upstream policy", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM group_upstream_accounts WHERE group_id=$1 AND provider=$2`, p.GroupID, p.Provider); err != nil {
			return mapDBError("clear group upstream accounts", err)
		}
		if len(p.AccountIDs) > 0 {
			encoded := marshalStringSliceJSON(p.AccountIDs)
			if _, err := tx.ExecContext(ctx, `INSERT INTO group_upstream_accounts(group_id,provider,upstream_account_id,created_at,updated_at)
				SELECT $1,$2,value,$3,$3 FROM jsonb_array_elements_text($4::jsonb)`, p.GroupID, p.Provider, p.At, encoded); err != nil {
				return mapDBError("save group upstream accounts", err)
			}
		}
		var updated time.Time
		if err := tx.QueryRowContext(ctx, `SELECT updated_at FROM group_upstream_account_policies WHERE group_id=$1 AND provider=$2`, p.GroupID, p.Provider).Scan(&updated); err != nil {
			return mapDBError("read group upstream policy", err)
		}
		result = GroupUpstreamAccountPolicy{GroupID: p.GroupID, Provider: p.Provider, Strategy: p.Strategy, Mode: p.Strategy, AccountIDs: append([]string{}, p.AccountIDs...), UpdatedAt: updated}
		encoded, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE group_operations SET response=$2::jsonb WHERE operation_id=$1`, p.OperationID, string(encoded)); err != nil {
			return mapDBError("finish group upstream operation", err)
		}
		return appendBillingAuditTx(ctx, tx, p.BillingWriteParams, "group.upstream_accounts", "group", p.GroupID,
			map[string]any{"provider": p.Provider, "strategy": p.Strategy, "account_count": len(p.AccountIDs), "operation_id": p.OperationID})
	})
	return result, err
}

func marshalStringSliceJSON(values []string) string {
	b, _ := json.Marshal(values)
	return string(b)
}

func (s *Store) GetGroupUpstreamAccounts(ctx context.Context, groupID, provider string) (GroupUpstreamAccountPolicy, error) {
	gid, err := canonicalGroupID(strings.TrimSpace(groupID))
	if err != nil {
		return GroupUpstreamAccountPolicy{}, err
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	switch provider {
	case UpstreamProviderCodex, UpstreamProviderAntigravity, UpstreamProviderAnthropic:
	default:
		return GroupUpstreamAccountPolicy{}, fmt.Errorf("%w: invalid upstream provider", ErrInvalid)
	}
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_groups WHERE id=$1)`, gid).Scan(&exists); err != nil {
		return GroupUpstreamAccountPolicy{}, mapDBError("check group upstream group", err)
	}
	if !exists {
		return GroupUpstreamAccountPolicy{}, ErrNotFound
	}
	var result GroupUpstreamAccountPolicy
	err = s.db.QueryRowContext(ctx, `SELECT strategy,updated_at FROM group_upstream_account_policies WHERE group_id=$1 AND provider=$2`, gid, provider).Scan(&result.Strategy, &result.UpdatedAt)
	if err == sql.ErrNoRows {
		return GroupUpstreamAccountPolicy{GroupID: gid, Provider: provider, Strategy: "all", Mode: "all", AccountIDs: []string{}}, nil
	}
	if err != nil {
		return result, mapDBError("read group upstream policy", err)
	}
	result.GroupID, result.Provider, result.Mode, result.AccountIDs = gid, provider, result.Strategy, []string{}
	rows, err := s.db.QueryContext(ctx, `SELECT upstream_account_id FROM group_upstream_accounts
		WHERE group_id=$1 AND provider=$2 ORDER BY upstream_account_id`, gid, provider)
	if err != nil {
		return result, mapDBError("list group upstream accounts", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return result, err
		}
		result.AccountIDs = append(result.AccountIDs, id)
	}
	return result, rows.Err()
}

// GroupUpstreamAccountIDs resolves a provider policy. A missing policy means
// all current provider accounts (the caller still applies live eligibility).
func (s *Store) GroupUpstreamAccountIDs(ctx context.Context, groupID, provider string) ([]string, bool, error) {
	policy, err := s.GetGroupUpstreamAccounts(ctx, groupID, provider)
	if err != nil {
		return nil, false, err
	}
	if policy.Strategy == "all" {
		return nil, true, nil
	}
	return append([]string{}, policy.AccountIDs...), false, nil
}

// Naming aliases used by HTTP and sidecar integrations.
type SetGroupUpstreamAccountPolicyParams = SetGroupUpstreamAccountsParams

func (s *Store) SetGroupUpstreamAccountPolicy(ctx context.Context, p SetGroupUpstreamAccountPolicyParams) (GroupUpstreamAccountPolicy, error) {
	return s.SetGroupUpstreamAccounts(ctx, p)
}

type UpstreamAllocationGroup struct {
	GroupID    string   `json:"group_id"`
	AccountIDs []string `json:"account_ids"`
}

// ListGroupUpstreamAllocationGroup resolves one already-admitted group. The
// caller uses the immutable billing reservation's group ID so a membership
// or priority edit after admission cannot silently switch the account pool.
func (s *Store) ListGroupUpstreamAllocationGroup(ctx context.Context, groupID, provider string) (UpstreamAllocationGroup, error) {
	gid, err := canonicalGroupID(strings.TrimSpace(groupID))
	if err != nil {
		return UpstreamAllocationGroup{}, err
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider != UpstreamProviderCodex && provider != UpstreamProviderAntigravity && provider != UpstreamProviderAnthropic {
		return UpstreamAllocationGroup{}, fmt.Errorf("%w: invalid upstream provider", ErrInvalid)
	}
	policy, err := s.GetGroupUpstreamAccounts(ctx, gid, provider)
	if err != nil {
		return UpstreamAllocationGroup{}, err
	}
	accountIDs := append([]string{}, policy.AccountIDs...)
	if policy.Strategy == "all" {
		accounts, err := s.ListUpstreamAccounts(ctx)
		if err != nil {
			return UpstreamAllocationGroup{}, err
		}
		accountIDs = make([]string, 0, len(accounts))
		for _, account := range accounts {
			accountIDs = append(accountIDs, account.ID)
		}
	}
	return UpstreamAllocationGroup{GroupID: gid, AccountIDs: accountIDs}, nil
}

// ListUserUpstreamAllocationGroups returns the membership ordered account-pool
// layers used by a request. A missing policy means all current provider
// accounts; an explicit selected-empty policy remains an empty layer.
func (s *Store) ListUserUpstreamAllocationGroups(ctx context.Context, userID, provider string, models ...string) ([]UpstreamAllocationGroup, error) {
	uid, err := canonicalGroupID(strings.TrimSpace(userID))
	if err != nil {
		return nil, err
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider != UpstreamProviderCodex && provider != UpstreamProviderAntigravity && provider != UpstreamProviderAnthropic {
		return nil, fmt.Errorf("%w: invalid upstream provider", ErrInvalid)
	}
	model := ""
	if len(models) > 0 {
		model = models[0]
	}
	rows, err := s.db.QueryContext(ctx, `SELECT gm.group_id FROM group_memberships gm JOIN user_groups g ON g.id=gm.group_id WHERE gm.user_id=$1 AND g.archived_at IS NULL AND ($2='' OR g.model_ids ? $2) ORDER BY gm.priority,gm.group_id`, uid, model)
	ids := []string{}
	legacyFallback := false
	if err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "group_memberships") {
			return nil, mapDBError("list user allocation groups", err)
		}
		legacyFallback = true
	} else {
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	if legacyFallback {
		var legacy sql.NullString
		if e := s.db.QueryRowContext(ctx, `SELECT group_id FROM billing_accounts WHERE user_id=$1`, uid).Scan(&legacy); e != nil {
			return nil, mapDBError("read legacy allocation group", e)
		}
		if legacy.Valid {
			ids = append(ids, legacy.String)
		}
	}
	result := make([]UpstreamAllocationGroup, 0, len(ids))
	for _, gid := range ids {
		policy, err := s.GetGroupUpstreamAccounts(ctx, gid, provider)
		if err != nil {
			return nil, err
		}
		accountIDs := append([]string{}, policy.AccountIDs...)
		if policy.Strategy == "all" {
			ar, err := s.ListUpstreamAccounts(ctx)
			if err != nil {
				return nil, err
			}
			accountIDs = make([]string, 0, len(ar))
			for _, a := range ar {
				accountIDs = append(accountIDs, a.ID)
			}
		}
		result = append(result, UpstreamAllocationGroup{GroupID: gid, AccountIDs: accountIDs})
	}
	return result, nil
}
