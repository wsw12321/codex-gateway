package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Ownership lifecycle (migration 0052). A login or import claims its account
// when it registers its egress; deleting the credential releases the account;
// only the Owner transfers an account that still has a credential. Every
// change of owner_user_id goes through changeUpstreamOwnerTx, which names the
// reason the database trigger requires.

const (
	ownerChangeClaim    = "claim"
	ownerChangeTransfer = "transfer"
)

// UpstreamAccountOwnership is what handlers check before acting on an
// account directly in CPA, where no store query applies the owner scope.
type UpstreamAccountOwnership struct {
	Exists            bool
	Provider          string
	OwnerUserID       string
	AdminSuspended    bool
	CredentialRemoved bool
	// OwnerDeleted: a released account whose owner was permanently deleted.
	// It belongs to no one, the platform included.
	OwnerDeleted   bool
	CredentialKind string
}

// InScope reports whether a store scoped to owner (empty: platform) may manage
// the account. An account the Gateway has no row for counts as a platform
// account, as before ownership existed. A tenant never manages a released
// account, and nobody manages one whose owner was deleted.
func (o UpstreamAccountOwnership) InScope(owner string) bool {
	if !o.Exists {
		return owner == ""
	}
	if o.OwnerDeleted || o.OwnerUserID != owner {
		return false
	}
	return owner == "" || !o.CredentialRemoved
}

func (s *Store) UpstreamAccountOwnership(ctx context.Context, id string) (UpstreamAccountOwnership, error) {
	if !upstreamAccountIDPattern.MatchString(id) {
		return UpstreamAccountOwnership{}, fmt.Errorf("%w: invalid upstream account id", ErrInvalid)
	}
	var value UpstreamAccountOwnership
	err := s.db.QueryRowContext(ctx, `SELECT provider, COALESCE(owner_user_id::text,''), admin_suspended, owner_deleted_at IS NOT NULL,
		credential_removed_at IS NOT NULL, credential_kind FROM upstream_accounts WHERE id=$1`, id).Scan(
		&value.Provider, &value.OwnerUserID, &value.AdminSuspended, &value.OwnerDeleted, &value.CredentialRemoved, &value.CredentialKind)
	if errors.Is(err, sql.ErrNoRows) {
		return UpstreamAccountOwnership{}, nil
	}
	if err != nil {
		return UpstreamAccountOwnership{}, mapDBError("read upstream account ownership", err)
	}
	value.Exists = true
	return value, nil
}

// CountTenantAccounts counts the accounts owner holds a credential for, every
// provider and kind together.
func (s *Store) CountTenantAccounts(ctx context.Context, owner string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM upstream_accounts WHERE owner_user_id=$1::uuid AND credential_removed_at IS NULL`, owner).Scan(&count)
	return count, mapDBError("count tenant accounts", err)
}

// lockTenantProvidersTx serializes one tenant's account and node creation so
// two concurrent logins cannot both pass the limit.
func lockTenantProvidersTx(ctx context.Context, tx *sql.Tx, owner string) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('gateway.tenant_providers:' || $1, 0))`, owner)
	return mapDBError("lock tenant providers", err)
}

// checkTenantAccountLimitTx fails with ErrProviderLimitReached when owner
// already holds max_accounts_per_user accounts with a credential. A missing
// settings row allows none.
func checkTenantAccountLimitTx(ctx context.Context, tx *sql.Tx, owner string) error {
	if err := lockTenantProvidersTx(ctx, tx, owner); err != nil {
		return err
	}
	var limit, count int
	err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT max_accounts_per_user FROM marketplace_settings WHERE singleton), 0),
		(SELECT count(*) FROM upstream_accounts WHERE owner_user_id=$1::uuid AND credential_removed_at IS NULL)`, owner).Scan(&limit, &count)
	if err != nil {
		return mapDBError("count tenant accounts", err)
	}
	if count >= limit {
		return ErrProviderLimitReached
	}
	return nil
}

func checkTenantEgressLimitTx(ctx context.Context, tx *sql.Tx, owner string) error {
	if err := lockTenantProvidersTx(ctx, tx, owner); err != nil {
		return err
	}
	var limit, count int
	err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT max_egress_servers_per_user FROM marketplace_settings WHERE singleton), 0),
		(SELECT count(*) FROM egress_servers WHERE owner_user_id=$1::uuid)`, owner).Scan(&limit, &count)
	if err != nil {
		return mapDBError("count tenant egress nodes", err)
	}
	if count >= limit {
		return ErrProviderLimitReached
	}
	return nil
}

func accountInPlatformUseTx(ctx context.Context, tx *sql.Tx, accountID string) (bool, error) {
	var used bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM group_upstream_accounts WHERE upstream_account_id=$1)
		OR EXISTS(SELECT 1 FROM upstream_account_users WHERE upstream_account_id=$1)
		OR EXISTS(SELECT 1 FROM user_upstream_access_accounts WHERE upstream_account_id=$1)`, accountID).Scan(&used)
	return used, mapDBError("check platform account use", err)
}

// changeUpstreamOwnerTx moves one locked account to newOwner (empty: the
// platform). The caller holds the egress_state lock. A binding to a node of
// another owner falls back to nothing: it is reset to the global egress at a
// new revision, which the caller pushes to the plugin. It reports that
// binding, or nil when the binding did not change.
func changeUpstreamOwnerTx(ctx context.Context, tx *sql.Tx, accountID, provider, previousOwner, newOwner, reason string, actor EgressActor) (*EgressBinding, error) {
	if newOwner != "" {
		used, err := accountInPlatformUseTx(ctx, tx, accountID)
		if err != nil {
			return nil, err
		}
		if used {
			return nil, ErrAccountInPlatformUse
		}
	}
	var foreignNode bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM upstream_account_egress b JOIN egress_servers n ON n.id=b.node_id
		WHERE b.account_id=$1 AND n.owner_user_id IS DISTINCT FROM $2::uuid)`, accountID, valueOrNil(newOwner)).Scan(&foreignNode); err != nil {
		return nil, mapDBError("check account egress owner", err)
	}
	var binding *EgressBinding
	if foreignNode {
		version, err := nextEgressVersion(ctx, tx)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE upstream_account_egress SET node_id=NULL, revision=$2 WHERE account_id=$1`, accountID, version); err != nil {
			return nil, mapDBError("reset account egress", err)
		}
		binding = &EgressBinding{AccountID: accountID, Provider: provider, Revision: version}
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('gateway.upstream_owner_change', $1, true)`, reason); err != nil {
		return nil, mapDBError("mark owner change", err)
	}
	// A claimed account whose previous owner was deleted has an owner again.
	if _, err := tx.ExecContext(ctx, `UPDATE upstream_accounts SET owner_user_id=$2::uuid, owner_deleted_at=NULL WHERE id=$1`, accountID, valueOrNil(newOwner)); err != nil {
		return nil, mapDBError("change upstream account owner", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('gateway.upstream_owner_change', '', true)`); err != nil {
		return nil, mapDBError("clear owner change", err)
	}
	metadata, err := marshalSafeMetadata(map[string]any{
		"provider": provider, "reason": reason, "previous_owner_user_id": previousOwner,
		"owner_user_id": newOwner, "egress_reset": binding != nil,
	})
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_events
		(occurred_at, actor_user_id, actor_session_id, event_type, severity, success,
			source_ip, subject_type, subject_id, request_id, metadata)
		VALUES (now(),$1::uuid,$2::uuid,'upstream_account.owner_changed','warning',true,
			$3::inet,'upstream_account',$4,$5,$6::jsonb)`,
		valueOrNil(actor.UserID), valueOrNil(actor.SessionID), valueOrNil(actor.SourceIP),
		accountID, valueOrNil(actor.RequestID), metadata)
	if err != nil {
		return nil, mapDBError("audit owner change", err)
	}
	return binding, nil
}

// claimRegistrationAccountTx makes sure the account row exists and belongs to
// the registration's owner (tenant, or the platform when empty) before a
// login or import commits. The caller holds the egress_state lock and a ROW
// EXCLUSIVE lock on upstream_accounts.
//
//   - A new account is created for the registration's owner.
//   - A released account (credential removed) is claimed.
//   - An account of the same owner is a re-authorization.
//   - A platform row that metadata synchronization created while this login
//     was running, and that nothing platform-only uses yet, is claimed.
//   - Anything else belongs to someone else: ErrAccountOwnedElsewhere.
func claimRegistrationAccountTx(ctx context.Context, tx *sql.Tx, registration EgressRegistration, accountID string) (bool, error) {
	owner := registration.TenantUserID
	actor := EgressActor{UserID: registration.ActorUserID, SessionID: registration.ActorSessionID}
	var provider, currentOwner, kind string
	var removed bool
	var createdAt time.Time
	err := tx.QueryRowContext(ctx, `SELECT provider, COALESCE(owner_user_id::text,''), credential_removed_at IS NOT NULL,
		credential_kind, created_at FROM upstream_accounts WHERE id=$1 FOR UPDATE`, accountID).Scan(
		&provider, &currentOwner, &removed, &kind, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		if owner != "" {
			if err := checkTenantAccountLimitTx(ctx, tx, owner); err != nil {
				return false, err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO upstream_accounts(id,masked_email,plan,status,provider,created_at,updated_at,owner_user_id)
			VALUES($1,'','unknown','unavailable',$2,now(),now(),$3::uuid)`, accountID, registration.Provider, valueOrNil(owner))
		return false, mapDBError("ensure registration account", err)
	}
	if err != nil {
		return false, mapDBError("find registration account", err)
	}
	if provider != registration.Provider {
		return false, ErrNotFound
	}
	if kind != "oauth" {
		return false, ErrConflict
	}
	switch {
	case removed:
		// A released account is not counted, so reviving it takes a slot.
		if owner != "" {
			if err := checkTenantAccountLimitTx(ctx, tx, owner); err != nil {
				return false, err
			}
		}
		if currentOwner != owner {
			if _, err := changeUpstreamOwnerTx(ctx, tx, accountID, provider, currentOwner, owner, ownerChangeClaim, actor); err != nil {
				return false, err
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE upstream_accounts SET credential_removed_at=NULL, owner_deleted_at=NULL WHERE id=$1`, accountID)
		return false, mapDBError("restore released account", err)
	case currentOwner == owner:
		// Metadata sync can discover a new platform credential between OAuth
		// start and prepare. Only a separate registration makes that a relogin.
		if owner == "" && !createdAt.Before(registration.CreatedAt) {
			var registered bool
			err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM egress_registrations
				WHERE account_id=$1 AND id<>$2::uuid AND state IN ('prepared','committed'))`, accountID, registration.ID).Scan(&registered)
			return registered, mapDBError("check previous credential registration", err)
		}
		return true, nil
	case currentOwner == "" && owner != "" && !createdAt.Before(registration.CreatedAt):
		var claimable bool
		if err := tx.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM upstream_account_egress WHERE account_id=$1)
			AND NOT EXISTS(SELECT 1 FROM egress_registrations WHERE account_id=$1 AND state='committed')
			AND NOT EXISTS(SELECT 1 FROM group_upstream_accounts WHERE upstream_account_id=$1)
			AND NOT EXISTS(SELECT 1 FROM upstream_account_users WHERE upstream_account_id=$1)
			AND NOT EXISTS(SELECT 1 FROM user_upstream_access_accounts WHERE upstream_account_id=$1)`, accountID).Scan(&claimable); err != nil {
			return false, mapDBError("check claimable account", err)
		}
		if !claimable {
			return false, ErrAccountOwnedElsewhere
		}
		if err := checkTenantAccountLimitTx(ctx, tx, owner); err != nil {
			return false, err
		}
		_, err := changeUpstreamOwnerTx(ctx, tx, accountID, provider, currentOwner, owner, ownerChangeClaim, actor)
		return false, err
	default:
		return false, ErrAccountOwnedElsewhere
	}
}

// ReleaseUpstreamAccount records that the account's credential was deleted:
// it leaves every funding path, its egress binding returns to the global
// egress, and the next login of the same upstream account may claim it. The
// row stays, with its last owner, for usage history.
func (s *Store) ReleaseUpstreamAccount(ctx context.Context, id string, actor EgressActor) error {
	if !s.validUpstreamProvider() || !upstreamAccountIDPattern.MatchString(id) {
		return ErrInvalid
	}
	return s.withTx(ctx, nil, func(tx *sql.Tx) error {
		var lockedVersion int64
		if err := tx.QueryRowContext(ctx, `SELECT version FROM egress_state WHERE singleton FOR UPDATE`).Scan(&lockedVersion); err != nil {
			return mapDBError("lock egress state", err)
		}
		if _, err := tx.ExecContext(ctx, `LOCK TABLE upstream_accounts IN ROW EXCLUSIVE MODE`); err != nil {
			return mapDBError("lock upstream account release", err)
		}
		var owner string
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(owner_user_id::text,'') FROM upstream_accounts
			WHERE id=$1 AND provider=$2 AND `+accountScopeSQL("", "$3")+` FOR UPDATE`,
			id, s.upstreamProviderName(), s.upstreamOwnerArg()).Scan(&owner); err != nil {
			return mapDBError("find released account", err)
		}
		var bound bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM upstream_account_egress WHERE account_id=$1 AND node_id IS NOT NULL)`, id).Scan(&bound); err != nil {
			return mapDBError("read released account egress", err)
		}
		if bound {
			version, err := nextEgressVersion(ctx, tx)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE upstream_account_egress SET node_id=NULL, revision=$2 WHERE account_id=$1`, id, version); err != nil {
				return mapDBError("reset released account egress", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE upstream_accounts SET credential_removed_at=COALESCE(credential_removed_at, now()) WHERE id=$1`, id); err != nil {
			return mapDBError("release upstream account", err)
		}
		metadata, err := marshalSafeMetadata(map[string]any{"provider": s.upstreamProviderName(), "owner_user_id": owner, "egress_reset": bound})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO audit_events
			(occurred_at, actor_user_id, actor_session_id, event_type, severity, success,
				source_ip, subject_type, subject_id, request_id, metadata)
			VALUES (now(),$1::uuid,$2::uuid,'upstream_account.released','info',true,
				$3::inet,'upstream_account',$4,$5,$6::jsonb)`,
			valueOrNil(actor.UserID), valueOrNil(actor.SessionID), valueOrNil(actor.SourceIP),
			id, valueOrNil(actor.RequestID), metadata)
		return mapDBError("audit upstream account release", err)
	})
}

type TransferUpstreamAccountParams struct {
	AccountID string
	// NewOwnerUserID is an active member, or empty for the platform.
	NewOwnerUserID string
	// ExpectedOwnerUserID must match the current owner (empty: platform), so
	// a stale page cannot move an account that changed hands meanwhile.
	ExpectedOwnerUserID string
	Actor               EgressActor
}

// TransferUpstreamAccountOwner is the Owner's ownership change. It reports
// the reset egress binding, if the old owner's node had to be dropped.
func (s *Store) TransferUpstreamAccountOwner(ctx context.Context, params TransferUpstreamAccountParams) (*EgressBinding, error) {
	params.NewOwnerUserID = strings.TrimSpace(params.NewOwnerUserID)
	params.ExpectedOwnerUserID = strings.TrimSpace(params.ExpectedOwnerUserID)
	if !s.validUpstreamProvider() || !upstreamAccountIDPattern.MatchString(params.AccountID) || params.Actor.UserID == "" {
		return nil, ErrInvalid
	}
	for _, id := range []string{params.NewOwnerUserID, params.ExpectedOwnerUserID} {
		if id == "" {
			continue
		}
		if parsed, err := uuid.Parse(id); err != nil || parsed.String() != id {
			return nil, ErrInvalid
		}
	}
	var binding *EgressBinding
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		var lockedVersion int64
		if err := tx.QueryRowContext(ctx, `SELECT version FROM egress_state WHERE singleton FOR UPDATE`).Scan(&lockedVersion); err != nil {
			return mapDBError("lock egress state", err)
		}
		if params.NewOwnerUserID != "" {
			var role, status string
			if err := tx.QueryRowContext(ctx, `SELECT role, status FROM users WHERE id=$1::uuid`, params.NewOwnerUserID).Scan(&role, &status); err != nil {
				return mapDBError("find new account owner", err)
			}
			if role != UserRoleMember || status != StatusActive {
				return fmt.Errorf("%w: new owner must be an active member", ErrInvalid)
			}
		}
		if _, err := tx.ExecContext(ctx, `LOCK TABLE upstream_accounts IN ROW EXCLUSIVE MODE`); err != nil {
			return mapDBError("lock upstream account transfer", err)
		}
		var owner string
		var removed bool
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(owner_user_id::text,''), credential_removed_at IS NOT NULL
			FROM upstream_accounts WHERE id=$1 AND provider=$2 FOR UPDATE`, params.AccountID, s.upstreamProviderName()).Scan(&owner, &removed); err != nil {
			return mapDBError("find transferred account", err)
		}
		if removed {
			return fmt.Errorf("%w: released account", ErrConflict)
		}
		if owner != params.ExpectedOwnerUserID {
			return fmt.Errorf("%w: account owner changed", ErrConflict)
		}
		if owner == params.NewOwnerUserID {
			return nil
		}
		var err error
		binding, err = changeUpstreamOwnerTx(ctx, tx, params.AccountID, s.upstreamProviderName(), owner, params.NewOwnerUserID, ownerChangeTransfer, params.Actor)
		return err
	})
	if err != nil {
		return nil, err
	}
	return binding, nil
}

type SetUpstreamAccountSuspendedParams struct {
	AccountID string
	Suspended bool
	Actor     EgressActor
}

// SetUpstreamAccountSuspended is the Owner's moderation hold on a tenant
// account. Platform accounts are switched off in CPA instead.
func (s *Store) SetUpstreamAccountSuspended(ctx context.Context, params SetUpstreamAccountSuspendedParams) (TenantUpstreamAccount, error) {
	if !upstreamAccountIDPattern.MatchString(params.AccountID) || params.Actor.UserID == "" {
		return TenantUpstreamAccount{}, ErrInvalid
	}
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `LOCK TABLE upstream_accounts IN ROW EXCLUSIVE MODE`); err != nil {
			return mapDBError("lock upstream account suspension", err)
		}
		var previous bool
		var owner string
		if err := tx.QueryRowContext(ctx, `SELECT admin_suspended, owner_user_id::text FROM upstream_accounts
			WHERE id=$1 AND owner_user_id IS NOT NULL AND credential_removed_at IS NULL FOR UPDATE`, params.AccountID).Scan(&previous, &owner); err != nil {
			return mapDBError("find suspended account", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE upstream_accounts SET admin_suspended=$2 WHERE id=$1`, params.AccountID, params.Suspended); err != nil {
			return mapDBError("suspend upstream account", err)
		}
		event := "upstream_account.unsuspended"
		if params.Suspended {
			event = "upstream_account.suspended"
		}
		metadata, err := marshalSafeMetadata(map[string]any{"owner_user_id": owner, "previous_suspended": previous, "suspended": params.Suspended})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO audit_events
			(occurred_at, actor_user_id, actor_session_id, event_type, severity, success,
				source_ip, subject_type, subject_id, request_id, metadata)
			VALUES (now(),$1::uuid,$2::uuid,$3,'warning',true,$4::inet,'upstream_account',$5,$6,$7::jsonb)`,
			params.Actor.UserID, valueOrNil(params.Actor.SessionID), event, valueOrNil(params.Actor.SourceIP),
			params.AccountID, valueOrNil(params.Actor.RequestID), metadata)
		return mapDBError("audit upstream account suspension", err)
	})
	if err != nil {
		return TenantUpstreamAccount{}, err
	}
	accounts, err := s.listTenantUpstreamAccounts(ctx, params.AccountID)
	if err != nil {
		return TenantUpstreamAccount{}, err
	}
	if len(accounts) != 1 {
		return TenantUpstreamAccount{}, ErrNotFound
	}
	return accounts[0], nil
}

// TenantUpstreamAccount is the Owner's view of one tenant account.
type TenantUpstreamAccount struct {
	ID               string     `json:"id"`
	Provider         string     `json:"provider"`
	CredentialKind   string     `json:"credential_kind"`
	OwnerUserID      string     `json:"owner_user_id"`
	OwnerUsername    string     `json:"owner_username"`
	OwnerDisplayName string     `json:"owner_display_name"`
	DisplayName      string     `json:"display_name"`
	Alias            string     `json:"alias"`
	MaskedEmail      string     `json:"email_masked"`
	Plan             string     `json:"plan"`
	Status           string     `json:"status"`
	AdminSuspended   bool       `json:"admin_suspended"`
	EgressServerID   string     `json:"egress_server_id"`
	EgressServerName string     `json:"egress_server_name"`
	CreatedAt        time.Time  `json:"created_at"`
	LastSyncedAt     *time.Time `json:"last_synced_at"`
	ConcurrentLimit  int        `json:"concurrent_limit"`
	AllocationWeight int        `json:"allocation_weight"`
	OwnerStatus      string     `json:"owner_status"`
}

// ListTenantUpstreamAccounts returns every tenant account that still has a
// credential, for the Owner.
func (s *Store) ListTenantUpstreamAccounts(ctx context.Context) ([]TenantUpstreamAccount, error) {
	return s.listTenantUpstreamAccounts(ctx, "")
}

func (s *Store) listTenantUpstreamAccounts(ctx context.Context, id string) ([]TenantUpstreamAccount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.id, a.provider, a.credential_kind, a.owner_user_id::text, u.username, u.display_name,
		a.display_name, a.alias, a.masked_email, a.plan, a.status, a.admin_suspended,
		COALESCE(b.node_id::text,''), COALESCE(n.name,''), a.created_at, a.last_synced_at,
		a.concurrent_limit, a.allocation_weight, u.status
		FROM upstream_accounts a JOIN users u ON u.id=a.owner_user_id
		LEFT JOIN upstream_account_egress b ON b.account_id=a.id
		LEFT JOIN egress_servers n ON n.id=b.node_id
		WHERE a.owner_user_id IS NOT NULL AND a.credential_removed_at IS NULL AND ($1='' OR a.id=$1)
		ORDER BY u.username, a.provider, a.id`, id)
	if err != nil {
		return nil, mapDBError("list tenant accounts", err)
	}
	defer rows.Close()
	result := make([]TenantUpstreamAccount, 0)
	for rows.Next() {
		var value TenantUpstreamAccount
		var lastSynced sql.NullTime
		if err := rows.Scan(&value.ID, &value.Provider, &value.CredentialKind, &value.OwnerUserID, &value.OwnerUsername,
			&value.OwnerDisplayName, &value.DisplayName, &value.Alias, &value.MaskedEmail, &value.Plan, &value.Status,
			&value.AdminSuspended, &value.EgressServerID, &value.EgressServerName, &value.CreatedAt, &lastSynced,
			&value.ConcurrentLimit, &value.AllocationWeight, &value.OwnerStatus); err != nil {
			return nil, mapDBError("read tenant account", err)
		}
		if lastSynced.Valid {
			value.LastSyncedAt = &lastSynced.Time
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

// TenantEgressServer is the Owner's view of one tenant node, without secrets.
type TenantEgressServer struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Protocol      string    `json:"protocol"`
	Server        string    `json:"server"`
	Port          int       `json:"port"`
	OwnerUserID   string    `json:"owner_user_id"`
	OwnerUsername string    `json:"owner_username"`
	BoundAccounts int       `json:"bound_accounts"`
	CreatedAt     time.Time `json:"created_at"`
	// OwnerStatus is the member's user status: a disabled member cannot
	// remove the node, so the Owner does.
	OwnerStatus string `json:"owner_status"`
}

func (s *Store) ListTenantEgressServers(ctx context.Context) ([]TenantEgressServer, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT n.id::text, n.name, n.protocol, n.server, n.port, n.owner_user_id::text, u.username,
		(SELECT count(*) FROM upstream_account_egress b WHERE b.node_id=n.id), n.created_at, u.status
		FROM egress_servers n JOIN users u ON u.id=n.owner_user_id ORDER BY u.username, n.name, n.id`)
	if err != nil {
		return nil, mapDBError("list tenant egress nodes", err)
	}
	defer rows.Close()
	result := make([]TenantEgressServer, 0)
	for rows.Next() {
		var value TenantEgressServer
		if err := rows.Scan(&value.ID, &value.Name, &value.Protocol, &value.Server, &value.Port, &value.OwnerUserID,
			&value.OwnerUsername, &value.BoundAccounts, &value.CreatedAt, &value.OwnerStatus); err != nil {
			return nil, mapDBError("read tenant egress node", err)
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

// DeleteTenantEgressServer is the Owner's removal of a member's egress node,
// for example before permanently deleting a disabled member, who can no
// longer sign in to remove it. The rules are the member's own: a node that an
// account is bound to, or that a recent login chose, is refused (ErrConflict).
// A platform node answers ErrNotFound; the Owner manages those on the egress
// page.
func (s *Store) DeleteTenantEgressServer(ctx context.Context, id string, actor EgressActor) error {
	if parsed, err := uuid.Parse(id); err != nil || parsed.String() != id {
		return fmt.Errorf("%w: invalid node id", ErrInvalid)
	}
	// A node's owner never changes (trigger egress_servers_owner_immutable),
	// so it is read once outside the deletion.
	var owner sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT owner_user_id::text FROM egress_servers WHERE id=$1::uuid`, id).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !owner.Valid {
		return ErrNotFound
	}
	if err != nil {
		return mapDBError("find tenant egress node", err)
	}
	return s.WithUpstreamOwner(owner.String).DeleteEgressServer(ctx, id, actor)
}
