package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// Layer eligibility kinds. A layer is judged by what its ID names for this
// user right now, never by what the caller claims it is.
const (
	// LayerEligibilityPlatform: group, personal and unknown layers. Only
	// platform accounts the user may use, exactly as without layers.
	LayerEligibilityPlatform = "platform"
	// LayerEligibilitySelf: the user's own accounts while self-use is on.
	LayerEligibilitySelf = "self"
	// LayerEligibilitySubscription: the accounts the subscription's plan
	// scope names now.
	LayerEligibilitySubscription = "subscription"
	// LayerEligibilityClosed: a subscription of the user that can no longer
	// pay. Its layer has no account.
	LayerEligibilityClosed = "closed"
)

// UpstreamLayerEligibility is one funding layer's accounts the user may use
// right now, with their concurrency limits.
type UpstreamLayerEligibility struct {
	GroupID  string
	Kind     string
	Accounts []UpstreamAccountEligibility
}

type layerSubscription struct {
	active bool
	planID string
	seller string
	mode   string
}

// LayerEligibleAccounts checks each layer's candidates against the layer's
// current rules, for this store's provider:
//
//   - the self-use layer: the user's own accounts, while the marketplace and
//     self-use are on;
//   - a restricted subscription of the user: the accounts its plan's scope
//     names now (a seller's own accounts, or listed platform accounts the
//     user may use);
//   - any other layer: the platform accounts the user may use.
//
// Every layer drops accounts the quota guard holds back. Layers are returned
// in the given order.
func (s *Store) LayerEligibleAccounts(ctx context.Context, userID string, layers []UpstreamAllocationGroup) ([]UpstreamLayerEligibility, error) {
	if !s.validUpstreamProvider() {
		return nil, fmt.Errorf("%w: invalid upstream provider", ErrInvalid)
	}
	if len(layers) > MaxFundingLayers+1 {
		return nil, fmt.Errorf("%w: too many funding layers", ErrInvalid)
	}
	subscriptions, err := s.layerSubscriptions(ctx, userID, layers)
	if err != nil {
		return nil, err
	}
	result := make([]UpstreamLayerEligibility, 0, len(layers))
	for _, layer := range layers {
		row := UpstreamLayerEligibility{GroupID: layer.GroupID, Kind: LayerEligibilityPlatform, Accounts: []UpstreamAccountEligibility{}}
		subscription, isSubscription := subscriptions[layer.GroupID]
		switch {
		case layer.GroupID == SelfFundingLayerID:
			row.Kind = LayerEligibilitySelf
		case isSubscription && !subscription.active:
			row.Kind = LayerEligibilityClosed
		case isSubscription && (subscription.seller != "" || subscription.mode == "selected"):
			row.Kind = LayerEligibilitySubscription
		}
		if len(layer.AccountIDs) > 0 {
			switch row.Kind {
			case LayerEligibilitySelf:
				row.Accounts, err = s.tenantLayerEligibility(ctx, userID, userID, "", layer.AccountIDs)
			case LayerEligibilitySubscription:
				if subscription.seller != "" {
					row.Accounts, err = s.tenantLayerEligibility(ctx, userID, subscription.seller, subscription.planID, layer.AccountIDs)
				} else {
					row.Accounts, err = s.listedPlatformEligibility(ctx, userID, subscription.planID, layer.AccountIDs)
				}
			case LayerEligibilityPlatform:
				row.Accounts, err = s.EligibleUpstreamAccountLimits(ctx, userID, layer.AccountIDs)
			}
			if err != nil {
				return nil, err
			}
		}
		result = append(result, row)
	}
	return result, nil
}

// layerSubscriptions looks up which layer IDs name subscriptions of the user,
// and whether each can still pay. A seller plan pays only while the
// marketplace is on; a platform plan restricts its accounts only then.
func (s *Store) layerSubscriptions(ctx context.Context, userID string, layers []UpstreamAllocationGroup) (map[string]layerSubscription, error) {
	ids := make([]string, 0, len(layers))
	for _, layer := range layers {
		if layer.GroupID != SelfFundingLayerID && layer.GroupID != PersonalFundingLayerID && validUUIDText(layer.GroupID) {
			ids = append(ids, layer.GroupID)
		}
	}
	result := make(map[string]layerSubscription)
	if len(ids) == 0 {
		return result, nil
	}
	args := []any{userID}
	placeholders := make([]string, len(ids))
	for i, id := range ids {
		args = append(args, id)
		placeholders[i] = fmt.Sprintf("$%d::uuid", i+2)
	}
	// #nosec G202 -- the placeholder list is generated; every value is a bound parameter.
	rows, err := s.db.QueryContext(ctx, `SELECT s.id::text,
			s.enabled AND s.charge_enabled AND (s.expires_at IS NULL OR s.expires_at > now())
				AND (bp.seller_user_id IS NULL OR `+marketplaceTenantSQL+`),
			COALESCE(bp.id::text,''), COALESCE(bp.seller_user_id::text,''),
			CASE WHEN `+marketplaceTenantSQL+` THEN COALESCE(sc.mode,'all') ELSE 'all' END
		FROM billing_subscriptions s
		LEFT JOIN billing_plans bp ON bp.id=s.source_plan_id
		LEFT JOIN billing_plan_provider_scopes sc ON sc.plan_id=bp.id
		WHERE s.user_id=$1::uuid AND s.id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return nil, mapDBError("read layer subscriptions", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var value layerSubscription
		if err := rows.Scan(&id, &value.active, &value.planID, &value.seller, &value.mode); err != nil {
			return nil, fmt.Errorf("scan layer subscription: %w", err)
		}
		result[id] = value
	}
	return result, rows.Err()
}

// marketplaceTenantSQL is true while tenant features are on. Unlike the
// platform predicate, a missing settings row reads as off.
const marketplaceTenantSQL = `COALESCE((SELECT ms.enabled FROM marketplace_settings ms WHERE ms.singleton), false)`

// tenantLayerEligibility returns the candidates owned by owner that may serve
// user: the user is active, the marketplace is on, the account is neither
// suspended nor released, and the quota guard does not hold it. With planID
// empty this is the self-use layer (owner is the user, and self-use must be
// on); otherwise the plan's current scope must name the account and its
// seller must be active.
func (s *Store) tenantLayerEligibility(ctx context.Context, userID, owner, planID string, ids []string) ([]UpstreamAccountEligibility, error) {
	args, values, err := upstreamCandidateArguments(userID, ids)
	if err != nil {
		return nil, err
	}
	args = append(args, s.upstreamProviderName(), owner, valueOrNil(planID))
	providerArg := fmt.Sprintf("$%d", len(args)-2)
	ownerArg := fmt.Sprintf("$%d", len(args)-1)
	planArg := fmt.Sprintf("$%d", len(args))
	// #nosec G202 -- fragments are fixed SQL over generated placeholder names; every value is a bound parameter.
	query := `WITH candidates(id) AS (VALUES ` + strings.Join(values, ",") + `)
		SELECT c.id, a.concurrent_limit FROM candidates c
		JOIN upstream_accounts a ON a.id=c.id
		WHERE a.provider=` + providerArg + ` AND a.owner_user_id=` + ownerArg + `::uuid
		  AND NOT a.admin_suspended AND a.credential_removed_at IS NULL
		  AND EXISTS(SELECT 1 FROM users WHERE id=$1::uuid AND status='active')
		  AND EXISTS(SELECT 1 FROM users WHERE id=` + ownerArg + `::uuid AND status='active')
		  AND ` + marketplaceTenantSQL + `
		  AND CASE WHEN ` + planArg + `::uuid IS NULL
			THEN COALESCE((SELECT b.self_use_enabled FROM billing_accounts b WHERE b.user_id=$1::uuid), false)
			ELSE EXISTS(SELECT 1 FROM billing_plans p LEFT JOIN billing_plan_provider_scopes sc ON sc.plan_id=p.id
				WHERE p.id=` + planArg + `::uuid AND p.seller_user_id=` + ownerArg + `::uuid
				  AND (COALESCE(sc.mode,'all')='all' OR EXISTS(SELECT 1 FROM billing_plan_provider_accounts pa
					WHERE pa.plan_id=p.id AND pa.account_id=a.id)))
		  END
		  AND NOT ` + upstreamQuotaGuardHeld("c.id", "now()") + `
		ORDER BY c.id`
	return s.scanEligibility(ctx, "read tenant layer eligibility", query, args...)
}

// listedPlatformEligibility is the platform eligibility of the candidates,
// narrowed to the accounts a platform plan's scope lists.
func (s *Store) listedPlatformEligibility(ctx context.Context, userID, planID string, ids []string) ([]UpstreamAccountEligibility, error) {
	eligible, err := s.EligibleUpstreamAccountLimits(ctx, userID, ids)
	if err != nil || len(eligible) == 0 {
		return eligible, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT account_id FROM billing_plan_provider_accounts WHERE plan_id=$1::uuid`, planID)
	if err != nil {
		return nil, mapDBError("read plan scope accounts", err)
	}
	defer rows.Close()
	listed := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan plan scope account: %w", err)
		}
		listed[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]UpstreamAccountEligibility, 0, len(eligible))
	for _, item := range eligible {
		if listed[item.ID] {
			result = append(result, item)
		}
	}
	return result, nil
}

func (s *Store) scanEligibility(ctx context.Context, operation, query string, args ...any) ([]UpstreamAccountEligibility, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError(operation, err)
	}
	defer rows.Close()
	result := make([]UpstreamAccountEligibility, 0)
	for rows.Next() {
		var item UpstreamAccountEligibility
		if err := rows.Scan(&item.ID, &item.ConcurrentLimit); err != nil {
			return nil, fmt.Errorf("scan %s: %w", operation, err)
		}
		if item.ConcurrentLimit < 1 || item.ConcurrentLimit > MaxUpstreamConcurrentLimit {
			return nil, fmt.Errorf("%w: invalid upstream concurrent limit", ErrInvalid)
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// SelectLayerUpstreamAccount chooses one account of a funding layer for a new
// binding: the layer's eligible candidates, by allocation weight and recent
// spend. A layer with no eligible candidate returns ErrNoUpstreamAccount.
func (s *Store) SelectLayerUpstreamAccount(ctx context.Context, userID string, layer UpstreamAllocationGroup, at time.Time) (string, error) {
	eligible, err := s.LayerEligibleAccounts(ctx, userID, []UpstreamAllocationGroup{layer})
	if err != nil {
		return "", err
	}
	ids := make([]string, 0, len(eligible[0].Accounts))
	for _, item := range eligible[0].Accounts {
		ids = append(ids, item.ID)
	}
	if len(ids) == 0 {
		return "", ErrNoUpstreamAccount
	}
	if eligible[0].Kind == LayerEligibilityPlatform {
		return s.SelectUpstreamAccount(ctx, userID, ids, at)
	}
	return s.selectAuthorizedUpstreamAccount(ctx, ids, at)
}

// selectAuthorizedUpstreamAccount is the weighted choice among candidates
// whose authorization the caller has just checked.
func (s *Store) selectAuthorizedUpstreamAccount(ctx context.Context, candidateIDs []string, at time.Time) (string, error) {
	if at.IsZero() {
		at = s.now().UTC()
	} else {
		at = at.UTC()
	}
	args := []any{at.Add(-24 * time.Hour), at, s.upstreamProviderName()}
	values := make([]string, len(candidateIDs))
	for i, id := range candidateIDs {
		if !upstreamAccountIDPattern.MatchString(id) {
			return "", fmt.Errorf("%w: noncanonical upstream candidate", ErrInvalid)
		}
		args = append(args, id)
		values[i] = fmt.Sprintf("($%d::text)", i+4)
	}
	// #nosec G202 -- fragments are fixed SQL over generated placeholder names; every value is a bound parameter.
	rows, err := s.db.QueryContext(ctx, `WITH candidates(id) AS (VALUES `+strings.Join(values, ",")+`)
		SELECT c.id, a.allocation_weight, COALESCE(l.cost, '0')
		FROM candidates c
		JOIN upstream_accounts a ON a.id = c.id AND a.provider = $3
		LEFT JOIN LATERAL (
			SELECT COALESCE(sum(actual_cost_usd), 0::numeric)::text cost
			FROM billing_ledger_entries
			WHERE entry_type = 'usage_charge' AND upstream_account_id = a.id
			  AND COALESCE(usage_requested_at, created_at) >= $1
			  AND COALESCE(usage_requested_at, created_at) < $2
		) l ON true
		WHERE NOT `+upstreamQuotaGuardHeld("c.id", "$2"), args...)
	if err != nil {
		return "", mapDBError("read authorized upstream allocation snapshot", err)
	}
	defer rows.Close()
	candidates := make([]upstreamAllocationCandidate, 0, len(candidateIDs))
	for rows.Next() {
		var candidate upstreamAllocationCandidate
		if err := rows.Scan(&candidate.id, &candidate.weight, &candidate.cost); err != nil {
			return "", fmt.Errorf("scan authorized upstream allocation snapshot: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate authorized upstream allocation snapshot: %w", err)
	}
	return selectUpstreamAllocation(candidates, func(limit int64) (int64, error) {
		value, err := rand.Int(rand.Reader, big.NewInt(limit))
		if err != nil {
			return 0, err
		}
		return value.Int64(), nil
	})
}

// validUUIDText accepts the canonical lowercase form.
func validUUIDText(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	return true
}

// TenantReachableAccounts lists the member accounts of this store's provider
// that could serve userID right now through a member layer: the user's own
// accounts while self-use is on, and the accounts of each restricted
// subscription's plan. Funds are not considered; suspended and released
// accounts never appear.
func (s *Store) TenantReachableAccounts(ctx context.Context, userID string) ([]string, error) {
	if !s.validUpstreamProvider() || !validUUIDText(userID) {
		return nil, fmt.Errorf("%w: invalid tenant account lookup", ErrInvalid)
	}
	var result []string
	err := s.withTx(ctx, &sql.TxOptions{ReadOnly: true}, func(tx *sql.Tx) error {
		enabled, err := marketplaceEnabledTx(ctx, tx)
		if err != nil || !enabled {
			return err
		}
		seen := map[string]bool{}
		add := func(ids []string) {
			for _, id := range ids {
				if !seen[id] {
					seen[id] = true
					result = append(result, id)
				}
			}
		}
		own, err := selfUsePoolTx(ctx, tx, userID, s.upstreamProviderName())
		if err != nil {
			return err
		}
		add(own)
		plans, err := queryAccountIDsTx(ctx, tx, "read seller plans of subscriptions", `SELECT DISTINCT p.id::text
			FROM billing_subscriptions s JOIN billing_plans p ON p.id=s.source_plan_id
			WHERE s.user_id=$1::uuid AND s.enabled AND s.charge_enabled AND (s.expires_at IS NULL OR s.expires_at > now())
			  AND p.seller_user_id IS NOT NULL`, userID)
		if err != nil {
			return err
		}
		for _, plan := range plans {
			pool, err := resolvePlanPoolTx(ctx, tx, plan, s.upstreamProviderName())
			if err != nil {
				return err
			}
			add(pool)
		}
		return nil
	})
	return result, err
}

// RoutableModelSuppliers lists the platform's API key suppliers of this
// store's provider and, among memberIDs, the members' suppliers that still
// hold a credential. Callers check each supplier's eligibility per layer.
func (s *Store) RoutableModelSuppliers(ctx context.Context, memberIDs []string) ([]ModelSupplier, error) {
	args := []any{s.upstreamProviderName()}
	member := "false"
	if len(memberIDs) > 0 {
		placeholders := make([]string, 0, len(memberIDs))
		for _, id := range memberIDs {
			if !upstreamAccountIDPattern.MatchString(id) {
				return nil, fmt.Errorf("%w: invalid supplier id", ErrInvalid)
			}
			args = append(args, id)
			placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
		}
		member = "a.id IN (" + strings.Join(placeholders, ",") + ")"
	}
	// #nosec G202 -- the placeholder list is generated; every value is a bound parameter.
	rows, err := s.db.QueryContext(ctx, `SELECT a.id, a.provider, m.base_url, m.key_ciphertext, m.models, a.status='available', m.revision
		FROM model_suppliers m JOIN upstream_accounts a ON a.id=m.account_id
		WHERE a.provider=$1 AND a.credential_removed_at IS NULL
		  AND (a.owner_user_id IS NULL OR (`+member+`))
		ORDER BY a.id`, args...)
	if err != nil {
		return nil, mapDBError("list routable model suppliers", err)
	}
	defer rows.Close()
	var values []ModelSupplier
	for rows.Next() {
		var value ModelSupplier
		var models []byte
		if err := rows.Scan(&value.ID, &value.Provider, &value.BaseURL, &value.Ciphertext, &models, &value.Enabled, &value.Revision); err != nil {
			return nil, mapDBError("read routable model supplier", err)
		}
		if json.Unmarshal(models, &value.Models) != nil {
			return nil, fmt.Errorf("invalid stored supplier catalog")
		}
		values = append(values, value)
	}
	return values, rows.Err()
}
