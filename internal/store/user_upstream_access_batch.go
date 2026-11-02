package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	UserUpstreamAccessBatchEnable  = "enable"
	UserUpstreamAccessBatchDisable = "disable"
	UserUpstreamAccessBatchAll     = "all"

	maxUserUpstreamAccessBatchUsers    = 1000
	maxUserUpstreamAccessBatchAccounts = 1000
)

// SetUserUpstreamAccessBatchParams applies one action to every selected user.
// Enable and disable name accounts and derive their providers; all names the
// providers to restore to the default scope.
type SetUserUpstreamAccessBatchParams struct {
	UserIDs        []string
	Action         string
	AccountIDs     []string
	Providers      []string
	Reason         string
	ActorUserID    string
	ActorSessionID string
	RequestID      string
	SourceIP       string
	At             time.Time
}

// UserUpstreamAccessBatchResult lists the resulting scope of every targeted
// user and provider, whether or not the batch changed it.
type UserUpstreamAccessBatchResult struct {
	Access         []UserUpstreamAccess `json:"access"`
	TargetCount    int                  `json:"target_count"`
	ChangedCount   int                  `json:"changed_count"`
	ConvertedCount int                  `json:"converted_count"`
}

// sortedUniqueStrings copies and sorts values, rejecting duplicates and
// values that fail valid.
func sortedUniqueStrings(values []string, valid func(string) bool) ([]string, bool) {
	sorted := append([]string{}, values...)
	sort.Strings(sorted)
	for i, value := range sorted {
		if !valid(value) || i > 0 && sorted[i-1] == value {
			return nil, false
		}
	}
	return sorted, true
}

func normalizeUserUpstreamAccessBatch(params SetUserUpstreamAccessBatchParams) (SetUserUpstreamAccessBatchParams, error) {
	invalid := fmt.Errorf("%w: invalid user upstream access batch", ErrInvalid)
	params.Reason = strings.TrimSpace(params.Reason)
	if strings.TrimSpace(params.ActorUserID) == "" || params.Reason == "" || len([]rune(params.Reason)) > 500 ||
		len(params.UserIDs) == 0 || len(params.UserIDs) > maxUserUpstreamAccessBatchUsers ||
		len(params.AccountIDs) > maxUserUpstreamAccessBatchAccounts {
		return params, invalid
	}
	switch params.Action {
	case UserUpstreamAccessBatchEnable, UserUpstreamAccessBatchDisable:
		if len(params.AccountIDs) == 0 || len(params.Providers) != 0 {
			return params, invalid
		}
	case UserUpstreamAccessBatchAll:
		if len(params.AccountIDs) != 0 || len(params.Providers) == 0 {
			return params, invalid
		}
	default:
		return params, invalid
	}
	var ok bool
	if params.UserIDs, ok = sortedUniqueStrings(params.UserIDs, func(id string) bool {
		parsed, err := uuid.Parse(id)
		return err == nil && parsed.String() == id
	}); !ok {
		return params, invalid
	}
	if params.AccountIDs, ok = sortedUniqueStrings(params.AccountIDs, upstreamAccountIDPattern.MatchString); !ok {
		return params, invalid
	}
	if params.Providers, ok = sortedUniqueStrings(params.Providers, isUpstreamProvider); !ok {
		return params, invalid
	}
	return params, nil
}

// nextUserUpstreamAccess applies a batch action to one user's provider scope.
// selected holds the batch accounts of this provider; registered lists every
// account of the provider and replaces all when an account is disabled.
func nextUserUpstreamAccess(action string, previous UserUpstreamAccess, selected map[string]bool, registered []string) UserUpstreamAccess {
	next := UserUpstreamAccess{UserID: previous.UserID, Provider: previous.Provider, Mode: "selected", AccountIDs: []string{}}
	switch action {
	case UserUpstreamAccessBatchAll:
		next.Mode = "all"
	case UserUpstreamAccessBatchEnable:
		if previous.Mode == "all" {
			next.Mode = "all"
			break
		}
		next.AccountIDs = append(next.AccountIDs, previous.AccountIDs...)
		for id := range selected {
			if !slices.Contains(previous.AccountIDs, id) {
				next.AccountIDs = append(next.AccountIDs, id)
			}
		}
	case UserUpstreamAccessBatchDisable:
		base := previous.AccountIDs
		if previous.Mode == "all" {
			base = registered
		}
		for _, id := range base {
			if !selected[id] {
				next.AccountIDs = append(next.AccountIDs, id)
			}
		}
	}
	sort.Strings(next.AccountIDs)
	return next
}

// ListUserUpstreamAccessRules returns every explicit scope of non-pending
// users across providers. Users without a rule use all accounts.
func (s *Store) ListUserUpstreamAccessRules(ctx context.Context) ([]UserUpstreamAccess, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.user_id::text,a.provider,a.mode,
		COALESCE((SELECT jsonb_agg(m.upstream_account_id ORDER BY m.upstream_account_id)
			FROM user_upstream_access_accounts m WHERE m.user_id=a.user_id AND m.provider=a.provider),'[]'::jsonb)::text
		FROM user_upstream_access a JOIN users u ON u.id=a.user_id
		WHERE u.status <> 'pending'
		ORDER BY a.user_id,a.provider`)
	if err != nil {
		return nil, mapDBError("list user upstream access", err)
	}
	defer rows.Close()
	rules := []UserUpstreamAccess{}
	for rows.Next() {
		var rule UserUpstreamAccess
		var accountIDs string
		if err := rows.Scan(&rule.UserID, &rule.Provider, &rule.Mode, &accountIDs); err != nil {
			return nil, fmt.Errorf("scan user upstream access: %w", err)
		}
		if err := json.Unmarshal([]byte(accountIDs), &rule.AccountIDs); err != nil {
			return nil, fmt.Errorf("decode user upstream accounts: %w", err)
		}
		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, mapDBError("read user upstream access", err)
	}
	return rules, nil
}

// SetUserUpstreamAccessBatch applies one action to many users in a single
// transaction, writing and auditing only the scopes it changes. Every account
// of the involved providers is locked before the users, so member inserts
// never wait on an account after a user lock is held.
func (s *Store) SetUserUpstreamAccessBatch(ctx context.Context, params SetUserUpstreamAccessBatchParams) (UserUpstreamAccessBatchResult, error) {
	params, err := normalizeUserUpstreamAccessBatch(params)
	if err != nil {
		return UserUpstreamAccessBatchResult{}, err
	}
	if params.At.IsZero() {
		params.At = s.now().UTC()
	} else {
		params.At = params.At.UTC()
	}
	encodedUsers, _ := json.Marshal(params.UserIDs)
	write := userUpstreamAccessWrite{
		Reason: params.Reason, ActorUserID: params.ActorUserID, ActorSessionID: params.ActorSessionID,
		RequestID: params.RequestID, SourceIP: params.SourceIP, At: params.At,
	}
	result := UserUpstreamAccessBatchResult{Access: []UserUpstreamAccess{}}
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		// Metadata synchronization takes the stronger table lock first.
		if _, err := tx.ExecContext(ctx, `LOCK TABLE upstream_accounts IN ROW EXCLUSIVE MODE`); err != nil {
			return mapDBError("lock user upstream access batch", err)
		}
		providers := params.Providers
		selected := map[string]map[string]bool{}
		registered := map[string][]string{}
		if params.Action != UserUpstreamAccessBatchAll {
			encodedAccounts, _ := json.Marshal(params.AccountIDs)
			rows, err := tx.QueryContext(ctx, `SELECT id,provider FROM upstream_accounts
				WHERE provider IN (SELECT provider FROM upstream_accounts
					WHERE id IN (SELECT value FROM jsonb_array_elements_text($1::jsonb)))
				ORDER BY id FOR UPDATE`, string(encodedAccounts))
			if err != nil {
				return mapDBError("lock user upstream access batch accounts", err)
			}
			for rows.Next() {
				var id, provider string
				if err := rows.Scan(&id, &provider); err != nil {
					_ = rows.Close()
					return fmt.Errorf("scan user upstream access batch account: %w", err)
				}
				registered[provider] = append(registered[provider], id)
				if _, found := slices.BinarySearch(params.AccountIDs, id); found {
					if selected[provider] == nil {
						selected[provider] = map[string]bool{}
					}
					selected[provider][id] = true
				}
			}
			if err := rows.Close(); err != nil {
				return mapDBError("close user upstream access batch accounts", err)
			}
			if err := rows.Err(); err != nil {
				return mapDBError("read user upstream access batch accounts", err)
			}
			count := 0
			for provider, ids := range selected {
				count += len(ids)
				providers = append(providers, provider)
			}
			if count != len(params.AccountIDs) {
				return fmt.Errorf("%w: upstream account does not exist", ErrInvalid)
			}
			sort.Strings(providers)
		}
		// NO KEY UPDATE serializes status changes and deletion while letting
		// concurrent admin edits audit their Owner FK with KEY SHARE.
		rows, err := tx.QueryContext(ctx, `SELECT id FROM users WHERE status <> 'pending'
			AND id IN (SELECT value::uuid FROM jsonb_array_elements_text($1::jsonb)) ORDER BY id FOR NO KEY UPDATE`, string(encodedUsers))
		if err != nil {
			return mapDBError("lock user upstream access batch users", err)
		}
		users := 0
		for rows.Next() {
			users++
		}
		if err := rows.Close(); err != nil {
			return mapDBError("close user upstream access batch users", err)
		}
		if err := rows.Err(); err != nil {
			return mapDBError("read user upstream access batch users", err)
		}
		if users != len(params.UserIDs) {
			return fmt.Errorf("%w: user does not exist or is awaiting approval", ErrConflict)
		}
		encodedProviders, _ := json.Marshal(providers)
		previousRows, err := tx.QueryContext(ctx, `SELECT a.user_id::text,a.provider,a.mode,
			COALESCE((SELECT jsonb_agg(m.upstream_account_id ORDER BY m.upstream_account_id)
				FROM user_upstream_access_accounts m WHERE m.user_id=a.user_id AND m.provider=a.provider),'[]'::jsonb)::text
			FROM user_upstream_access a
			WHERE a.user_id IN (SELECT value::uuid FROM jsonb_array_elements_text($1::jsonb))
			AND a.provider IN (SELECT value FROM jsonb_array_elements_text($2::jsonb))`, string(encodedUsers), string(encodedProviders))
		if err != nil {
			return mapDBError("read previous user upstream access batch", err)
		}
		previous := map[[2]string]UserUpstreamAccess{}
		for previousRows.Next() {
			var rule UserUpstreamAccess
			var accountIDs string
			if err := previousRows.Scan(&rule.UserID, &rule.Provider, &rule.Mode, &accountIDs); err != nil {
				_ = previousRows.Close()
				return fmt.Errorf("scan previous user upstream access batch: %w", err)
			}
			if err := json.Unmarshal([]byte(accountIDs), &rule.AccountIDs); err != nil {
				_ = previousRows.Close()
				return fmt.Errorf("decode previous user upstream access batch: %w", err)
			}
			previous[[2]string{rule.UserID, rule.Provider}] = rule
		}
		if err := previousRows.Close(); err != nil {
			return mapDBError("close previous user upstream access batch", err)
		}
		if err := previousRows.Err(); err != nil {
			return mapDBError("read previous user upstream access batch", err)
		}
		extra := map[string]any{"batch_action": params.Action}
		for _, userID := range params.UserIDs {
			for _, provider := range providers {
				current, ok := previous[[2]string{userID, provider}]
				if !ok {
					current = UserUpstreamAccess{UserID: userID, Provider: provider, Mode: "all", AccountIDs: []string{}}
				}
				next := nextUserUpstreamAccess(params.Action, current, selected[provider], registered[provider])
				result.TargetCount++
				if next.Mode == current.Mode && slices.Equal(next.AccountIDs, current.AccountIDs) {
					result.Access = append(result.Access, current)
					continue
				}
				if err := replaceUserUpstreamAccessTx(ctx, tx, write, current, next, extra); err != nil {
					return err
				}
				result.ChangedCount++
				if current.Mode == "all" && next.Mode == "selected" {
					result.ConvertedCount++
				}
				result.Access = append(result.Access, next)
			}
		}
		return nil
	})
	if err != nil {
		return UserUpstreamAccessBatchResult{}, err
	}
	return result, nil
}
