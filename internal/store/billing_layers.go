package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// fundingLayer is one admission-time funding source and the accounts it may
// pay for: a group with its period and pool, or the personal layer.
type fundingLayer struct {
	position      int
	groupID       string
	groupPeriodID string
	allAccounts   bool
	accountIDs    []string
}

func (l fundingLayer) allocationGroup() UpstreamAllocationGroup {
	if l.allAccounts {
		return UpstreamAllocationGroup{GroupID: PersonalFundingLayerID, AccountIDs: []string{}, AllAccounts: true}
	}
	return UpstreamAllocationGroup{GroupID: l.groupID, AccountIDs: append([]string(nil), l.accountIDs...)}
}

func (l fundingLayer) contains(accountID string) bool {
	if l.allAccounts {
		return true
	}
	for _, id := range l.accountIDs {
		if id == accountID {
			return true
		}
	}
	return false
}

// fundingLayersTx orders the usable groups' pools by membership priority and
// appends the personal layer when personal funds exist. A group pool is cut to
// the accounts this user may use, and an empty pool drops its group, so the
// request falls through to the next source instead of failing at CPA.
func fundingLayersTx(ctx context.Context, tx *sql.Tx, params BillingReservationParams, groups []GroupSummary, personal bool) ([]fundingLayer, error) {
	provider := strings.ToLower(strings.TrimSpace(params.UpstreamProvider))
	if provider == "" {
		provider = UpstreamProviderCodex
	}
	layers := make([]fundingLayer, 0, len(groups)+1)
	for _, group := range groups {
		pool, err := snapshotGroupUpstreamAllocationGroupTx(ctx, tx, group.ID, provider)
		if err != nil {
			return nil, err
		}
		ids, err := userPermittedAccountsTx(ctx, tx, params.UserID, provider, pool.AccountIDs)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			continue
		}
		if len(ids) > MaxUpstreamAllocationCandidates {
			return nil, fmt.Errorf("%w: group account pool exceeds the forwarding limit", ErrInvalid)
		}
		layers = append(layers, fundingLayer{groupID: group.ID, groupPeriodID: group.PeriodID, accountIDs: ids})
	}
	// Personal funds are not narrowed to a pool; CPA's per-user eligibility
	// callback decides, exactly as for a request without group funding.
	if personal || params.BillingMode == BillingModeInternalZero {
		layers = append(layers, fundingLayer{allAccounts: true})
	}
	for i := range layers {
		layers[i].position = i + 1
	}
	return layers, nil
}

// userPermittedAccountsTx applies the same per-user rules as CPA's eligibility
// callback (shared or granted, and the user's account access mode).
func userPermittedAccountsTx(ctx context.Context, tx *sql.Tx, userID, provider string, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	// #nosec G202 -- the access predicate is fixed SQL over placeholder names; IDs, provider and user are bound parameters.
	rows, err := tx.QueryContext(ctx, `SELECT a.id FROM upstream_accounts a
		WHERE a.id = ANY($1::text[]) AND a.provider=$2
		  AND (a.access_mode='shared' OR EXISTS(SELECT 1 FROM upstream_account_users u WHERE u.upstream_account_id=a.id AND u.user_id=$3::uuid))
		  AND `+userUpstreamAccessPredicate("$3", "$2", "a.id")+`
		ORDER BY a.id`, ids, provider, userID)
	if err != nil {
		return nil, mapDBError("read funding layer accounts", err)
	}
	defer rows.Close()
	permitted := make([]string, 0, len(ids))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan funding layer account: %w", err)
		}
		permitted = append(permitted, id)
	}
	return permitted, rows.Err()
}

func recordFundingLayersTx(ctx context.Context, tx *sql.Tx, reservation BillingReservation, layers []fundingLayer) error {
	for _, layer := range layers {
		var group, period any
		accounts := []string{}
		if !layer.allAccounts {
			group, period, accounts = layer.groupID, layer.groupPeriodID, layer.accountIDs
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO billing_reservation_layers
			(request_id,position,user_id,group_id,group_period_id,all_accounts,account_ids)
			VALUES ($1,$2,$3,$4,$5,$6,$7::text[])`, reservation.RequestID, layer.position, reservation.UserID,
			group, period, layer.allAccounts, accounts); err != nil {
			return mapDBError("record billing funding layer", err)
		}
	}
	return nil
}

func readFundingLayersTx(ctx context.Context, tx *sql.Tx, requestID string) ([]fundingLayer, error) {
	rows, err := tx.QueryContext(ctx, `SELECT position, COALESCE(group_id::text,''), COALESCE(group_period_id::text,''),
		all_accounts, array_to_string(account_ids, ',')
		FROM billing_reservation_layers WHERE request_id=$1 ORDER BY position`, requestID)
	if err != nil {
		return nil, mapDBError("read billing funding layers", err)
	}
	defer rows.Close()
	var layers []fundingLayer
	for rows.Next() {
		var layer fundingLayer
		var accounts string
		if err := rows.Scan(&layer.position, &layer.groupID, &layer.groupPeriodID, &layer.allAccounts, &accounts); err != nil {
			return nil, fmt.Errorf("scan billing funding layer: %w", err)
		}
		if accounts != "" {
			layer.accountIDs = strings.Split(accounts, ",")
		}
		layers = append(layers, layer)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(layers) == 0 {
		return nil, fmt.Errorf("rule 4 reservation without funding layers: %w", ErrConflict)
	}
	return layers, nil
}

// settlementLayerCandidates returns the group layers, in billing order, that
// may pay for accountID. attributed is false when the account is missing or
// outside every layer; then the first layer pays, as before rule 4.
func settlementLayerCandidates(layers []fundingLayer, accountID string) (groups []fundingLayer, personal, attributed bool) {
	for _, layer := range layers {
		if accountID == "" || !layer.contains(accountID) {
			continue
		}
		if layer.allAccounts {
			personal = true
			continue
		}
		groups = append(groups, layer)
	}
	if len(groups) > 0 || personal {
		return groups, personal, true
	}
	if len(layers) > 0 && !layers[0].allAccounts {
		return layers[:1], false, false
	}
	return nil, len(layers) > 0, false
}

// bindSettlementLayerTx records which layer pays: the first candidate group
// that still has period and member funds, else the first candidate group (the
// existing split then sends the remainder to personal sources), else the
// personal layer. Candidate groups must already be locked by the caller.
func bindSettlementLayerTx(ctx context.Context, tx *sql.Tx, reservation *BillingReservation, candidates []fundingLayer, personal bool) error {
	var chosen *fundingLayer
	for i := range candidates {
		var available string
		var memberLimit sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT GREATEST(limit_usd-used_usd,0)::numeric(30,12)::text,
			member_limit_usd::text FROM group_usage_periods WHERE id=$1 AND group_id=$2`,
			candidates[i].groupPeriodID, candidates[i].groupID).Scan(&available, &memberLimit); err != nil {
			return mapDBError("read candidate group funding", err)
		}
		if !billingPositive(available) {
			continue
		}
		memberRemaining, err := groupMemberRemainingTx(ctx, tx, candidates[i].groupPeriodID, reservation.UserID, nullableString(memberLimit))
		if err != nil {
			return err
		}
		if memberRemaining == nil || billingPositive(*memberRemaining) {
			chosen = &candidates[i]
			break
		}
	}
	if chosen == nil && len(candidates) > 0 {
		chosen = &candidates[0]
	}
	if chosen == nil && !personal {
		return fmt.Errorf("rule 4 settlement without a paying layer: %w", ErrConflict)
	}
	var group, period any
	reservation.GroupID, reservation.GroupPeriodID = nil, nil
	if chosen != nil {
		groupID, periodID := chosen.groupID, chosen.groupPeriodID
		group, period = groupID, periodID
		reservation.GroupID, reservation.GroupPeriodID = &groupID, &periodID
	}
	result, err := tx.ExecContext(ctx, `UPDATE billing_reservations SET group_id=$2, group_period_id=$3
		WHERE request_id=$1 AND state='reserved'`, reservation.RequestID, group, period)
	if err != nil {
		return mapDBError("bind settlement funding layer", err)
	}
	return requireAffected("bind settlement funding layer", result)
}
