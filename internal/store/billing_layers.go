package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Funding layer kinds. Rule 4 has group layers and the personal layer; rule 5
// adds the user's own accounts (self-use) and one layer per subscription whose
// plan restricts the accounts it pays for.
const (
	FundingLayerGroup        = "group"
	FundingLayerSubscription = "subscription"
	FundingLayerSelf         = "self"
	FundingLayerPersonal     = "personal"
)

// SelfFundingLayerID identifies the self-use layer on the wire. Like the
// personal layer's nil UUID, no group or subscription can have it.
const SelfFundingLayerID = "00000000-0000-4000-8000-000000000002"

// MaxFundingLayers is the number of layers CPA accepts on one request.
const MaxFundingLayers = 64

// fundingLayer is one admission-time funding source and the accounts it may
// pay for: the user's own accounts, a group with its period and pool, a
// restricted subscription with its period and plan pool, or the personal
// layer.
type fundingLayer struct {
	position             int
	kind                 string
	groupID              string
	groupPeriodID        string
	subscriptionID       string
	subscriptionPeriodID string
	// tenant marks a layer whose accounts belong to members (self-use or a
	// seller plan). It is not persisted: only the forward path reads it.
	tenant      bool
	allAccounts bool
	accountIDs  []string
}

// layerKind tolerates layers built without a kind: those are rule 4 layers.
func (l fundingLayer) layerKind() string {
	switch {
	case l.kind != "":
		return l.kind
	case l.allAccounts:
		return FundingLayerPersonal
	default:
		return FundingLayerGroup
	}
}

func (l fundingLayer) layerID() string {
	switch l.layerKind() {
	case FundingLayerPersonal:
		return PersonalFundingLayerID
	case FundingLayerSelf:
		return SelfFundingLayerID
	case FundingLayerSubscription:
		return l.subscriptionID
	default:
		return l.groupID
	}
}

func (l fundingLayer) allocationGroup() UpstreamAllocationGroup {
	group := UpstreamAllocationGroup{GroupID: l.layerID(), Tenant: l.tenant}
	if l.allAccounts {
		group.AccountIDs, group.AllAccounts = []string{}, true
		return group
	}
	group.AccountIDs = append([]string(nil), l.accountIDs...)
	return group
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

// restrictedSubscription is a subscription whose plan restricts the accounts
// it pays for. Under rule 5 it is a layer of its own instead of a personal
// funding source.
type restrictedSubscription struct {
	source       BillingFundingSource
	planID       string
	sellerUserID string
}

// fundingLayersTx orders the layers of one admission: the user's own
// accounts, the usable groups' pools by membership priority, each restricted
// subscription by funding priority, then the personal layer when personal
// funds exist. A pool is cut to the accounts this user may use, and an empty
// pool drops its layer, so the request falls through to the next source
// instead of failing at CPA. Without self-use and restricted subscriptions
// this is exactly rule 4.
func fundingLayersTx(ctx context.Context, tx *sql.Tx, params BillingReservationParams, groups []GroupSummary, restricted []restrictedSubscription, selfPool []string, personal bool) ([]fundingLayer, error) {
	provider := upstreamProviderOrDefault(params.UpstreamProvider)
	layers := make([]fundingLayer, 0, len(groups)+len(restricted)+2)
	if len(selfPool) > 0 {
		if len(selfPool) > MaxUpstreamAllocationCandidates {
			return nil, fmt.Errorf("%w: own account pool exceeds the forwarding limit", ErrInvalid)
		}
		layers = append(layers, fundingLayer{kind: FundingLayerSelf, tenant: true, accountIDs: append([]string(nil), selfPool...)})
	}
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
		layers = append(layers, fundingLayer{kind: FundingLayerGroup, groupID: group.ID, groupPeriodID: group.PeriodID, accountIDs: ids})
	}
	for _, subscription := range restricted {
		ids, err := resolvePlanPoolTx(ctx, tx, subscription.planID, provider)
		if err != nil {
			return nil, err
		}
		if subscription.sellerUserID == "" {
			// A platform plan only narrows the platform accounts this user
			// may already use.
			if ids, err = userPermittedAccountsTx(ctx, tx, params.UserID, provider, ids); err != nil {
				return nil, err
			}
		}
		if len(ids) == 0 {
			continue
		}
		if len(ids) > MaxUpstreamAllocationCandidates {
			return nil, fmt.Errorf("%w: plan account pool exceeds the forwarding limit", ErrInvalid)
		}
		layers = append(layers, fundingLayer{kind: FundingLayerSubscription, subscriptionID: subscription.source.SubscriptionID,
			subscriptionPeriodID: subscription.source.PeriodID, tenant: subscription.sellerUserID != "", accountIDs: ids})
	}
	// Personal funds are not narrowed to a pool; CPA's per-user eligibility
	// callback decides, exactly as for a request without group funding.
	if personal || params.BillingMode == BillingModeInternalZero {
		layers = append(layers, fundingLayer{kind: FundingLayerPersonal, allAccounts: true})
	}
	if fundingRuleVersion(layers) == 5 && len(layers) > MaxFundingLayers {
		return nil, fmt.Errorf("%w: too many funding layers for one request", ErrInvalid)
	}
	for i := range layers {
		layers[i].position = i + 1
	}
	return layers, nil
}

// upstreamProviderOrDefault normalizes a reservation's provider; empty is Codex.
func upstreamProviderOrDefault(provider string) string {
	if provider = strings.ToLower(strings.TrimSpace(provider)); provider == "" {
		return UpstreamProviderCodex
	}
	return provider
}

// fundingRuleVersion is 5 when a layer needs it, else 4.
func fundingRuleVersion(layers []fundingLayer) int {
	for _, layer := range layers {
		if kind := layer.layerKind(); kind == FundingLayerSelf || kind == FundingLayerSubscription {
			return 5
		}
	}
	return 4
}

// planScopeSchemaTx reports whether migration 0053 is applied. Upgrade tests
// admit requests on a replayed older schema before migrating forward, like
// the other schema checks of the admission path.
func planScopeSchemaTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	var present bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass('billing_plan_provider_scopes') IS NOT NULL`).Scan(&present); err != nil {
		return false, mapDBError("check plan scope schema", err)
	}
	return present, nil
}

// marketplaceEnabledTx reads the marketplace switch for tenant features. A
// missing row reads as disabled: self-use and seller plans fail closed.
func marketplaceEnabledTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	var enabled bool
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT enabled FROM marketplace_settings WHERE singleton), false)`).Scan(&enabled); err != nil {
		return false, mapDBError("read marketplace switch", err)
	}
	return enabled, nil
}

// selfUsePoolTx lists the user's own accounts of provider that may serve the
// user free of charge: self-use is on, and the account is neither suspended
// by the Owner nor released.
func selfUsePoolTx(ctx context.Context, tx *sql.Tx, userID, provider string) ([]string, error) {
	return queryAccountIDsTx(ctx, tx, "read own account pool", `SELECT a.id FROM upstream_accounts a
		WHERE a.owner_user_id=$1::uuid AND a.provider=$2 AND NOT a.admin_suspended AND a.credential_removed_at IS NULL
		  AND COALESCE((SELECT b.self_use_enabled FROM billing_accounts b WHERE b.user_id=$1::uuid), false)
		ORDER BY a.id`, userID, provider)
}

// resolvePlanPoolTx resolves a plan's provider scope as it is now: every
// account of its seller, or the listed accounts the seller still owns. An
// account suspended by the Owner or released serves no plan, and a seller
// that is no longer active serves nothing.
func resolvePlanPoolTx(ctx context.Context, tx *sql.Tx, planID, provider string) ([]string, error) {
	return queryAccountIDsTx(ctx, tx, "resolve plan account pool", `SELECT a.id FROM billing_plans p
		LEFT JOIN billing_plan_provider_scopes sc ON sc.plan_id=p.id
		JOIN upstream_accounts a ON a.provider=$2 AND a.owner_user_id IS NOT DISTINCT FROM p.seller_user_id
		WHERE p.id=$1::uuid AND NOT a.admin_suspended AND a.credential_removed_at IS NULL
		  AND (p.seller_user_id IS NULL OR EXISTS(SELECT 1 FROM users u WHERE u.id=p.seller_user_id AND u.status='active'))
		  AND (COALESCE(sc.mode,'all')='all' OR EXISTS(
			SELECT 1 FROM billing_plan_provider_accounts pa WHERE pa.plan_id=p.id AND pa.account_id=a.id))
		ORDER BY a.id`, planID, provider)
}

func queryAccountIDsTx(ctx context.Context, tx *sql.Tx, operation, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError(operation, err)
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan %s: %w", operation, err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
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
		  AND `+platformAccountSQL("a")+`
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

// recordFundingLayersTx stores the admission layers. planScopes is false on a
// schema replayed from before migration 0053 (upgrade tests), where every
// layer is a rule 4 group or personal layer.
func recordFundingLayersTx(ctx context.Context, tx *sql.Tx, reservation BillingReservation, layers []fundingLayer, planScopes bool) error {
	for _, layer := range layers {
		var group, period, subscription, subscriptionPeriod any
		accounts := []string{}
		if !layer.allAccounts {
			accounts = layer.accountIDs
		}
		switch layer.layerKind() {
		case FundingLayerGroup:
			group, period = layer.groupID, layer.groupPeriodID
		case FundingLayerSubscription:
			subscription, subscriptionPeriod = layer.subscriptionID, layer.subscriptionPeriodID
		}
		var err error
		if planScopes {
			_, err = tx.ExecContext(ctx, `INSERT INTO billing_reservation_layers
				(request_id,position,user_id,kind,group_id,group_period_id,subscription_id,subscription_period_id,all_accounts,account_ids)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::text[])`, reservation.RequestID, layer.position, reservation.UserID,
				layer.layerKind(), group, period, subscription, subscriptionPeriod, layer.allAccounts, accounts)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO billing_reservation_layers
				(request_id,position,user_id,group_id,group_period_id,all_accounts,account_ids)
				VALUES ($1,$2,$3,$4,$5,$6,$7::text[])`, reservation.RequestID, layer.position, reservation.UserID,
				group, period, layer.allAccounts, accounts)
		}
		if err != nil {
			return mapDBError("record billing funding layer", err)
		}
	}
	return nil
}

// readFundingLayersTx reads a reservation's layers. planScopes is false on a
// schema replayed from before migration 0053 (upgrade tests): every layer is
// then a rule 4 group or personal layer.
func readFundingLayersTx(ctx context.Context, tx *sql.Tx, requestID string, planScopes bool) ([]fundingLayer, error) {
	kindColumns := `CASE WHEN group_id IS NULL THEN 'personal' ELSE 'group' END, '', ''`
	if planScopes {
		kindColumns = `kind, COALESCE(subscription_id::text,''), COALESCE(subscription_period_id::text,'')`
	}
	// #nosec G202 -- the kind columns are fixed SQL; the request ID is a bound parameter.
	rows, err := tx.QueryContext(ctx, `SELECT position, `+kindColumns+`, COALESCE(group_id::text,''), COALESCE(group_period_id::text,''),
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
		if err := rows.Scan(&layer.position, &layer.kind, &layer.subscriptionID, &layer.subscriptionPeriodID,
			&layer.groupID, &layer.groupPeriodID, &layer.allAccounts, &accounts); err != nil {
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
		return nil, fmt.Errorf("layered reservation without funding layers: %w", ErrConflict)
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

// rule5Settlement is who pays for one rule 5 request.
type rule5Settlement struct {
	// selfUse: one of the user's own accounts served it; nothing is charged.
	selfUse bool
	// groups are the candidate group layers in billing order; at most one
	// of them is bound and charged, as under rule 4.
	groups []fundingLayer
	// subscriptions are the restricted subscription layers, in billing order,
	// that contain the serving account.
	subscriptions []fundingLayer
	// personal: personal funds may pay. Never for a member's account.
	personal bool
}

// rule5SettlementCandidates decides which layers may pay for accountID. A
// cost a layer cannot cover flows only to later layers that also contain the
// account; what none covers is left uncovered. tenant reports that the
// account is a member's, so the personal layer never contains it: a seller's
// usage never reaches the buyer's cash. Without attribution, self-use is
// never assumed: the first other layer pays, as under rule 4. A member's
// account outside every layer pays nothing.
func rule5SettlementCandidates(layers []fundingLayer, accountID string, tenant bool) rule5Settlement {
	var result rule5Settlement
	attributed := false
	if accountID != "" {
		for _, layer := range layers {
			kind := layer.layerKind()
			if kind == FundingLayerPersonal {
				if !tenant {
					result.personal, attributed = true, true
				}
				continue
			}
			if !layer.contains(accountID) {
				continue
			}
			attributed = true
			switch kind {
			case FundingLayerSelf:
				return rule5Settlement{selfUse: true}
			case FundingLayerGroup:
				result.groups = append(result.groups, layer)
			case FundingLayerSubscription:
				result.subscriptions = append(result.subscriptions, layer)
			}
		}
	}
	if attributed {
		return result
	}
	if accountID != "" && tenant {
		// A member's account outside every layer was never admitted for this
		// request: nothing may pay for it.
		return rule5Settlement{}
	}
	for _, layer := range layers {
		switch layer.layerKind() {
		case FundingLayerGroup:
			personal := false
			for _, other := range layers {
				personal = personal || other.layerKind() == FundingLayerPersonal
			}
			return rule5Settlement{groups: []fundingLayer{layer}, personal: personal}
		case FundingLayerSubscription:
			return rule5Settlement{subscriptions: []fundingLayer{layer}}
		case FundingLayerPersonal:
			return rule5Settlement{personal: true}
		}
	}
	return rule5Settlement{}
}

// servingAccountIsTenantTx reports whether a served account belongs to a
// member, judged both by its admission layers and by its current owner. An
// account without a row is treated as a member's: rule 5 admissions happen
// while the marketplace is on, when an unregistered account serves nobody.
func servingAccountIsTenantTx(ctx context.Context, tx *sql.Tx, layers []fundingLayer, accountID string) (bool, error) {
	if accountID == "" {
		return false, nil
	}
	for _, layer := range layers {
		if layer.layerKind() == FundingLayerSelf && layer.contains(accountID) {
			return true, nil
		}
	}
	var platform bool
	err := tx.QueryRowContext(ctx, `SELECT owner_user_id IS NULL FROM upstream_accounts WHERE id=$1`, accountID).Scan(&platform)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, mapDBError("read serving account owner", err)
	}
	return !platform, nil
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
