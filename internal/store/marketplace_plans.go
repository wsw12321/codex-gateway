package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
	"unicode/utf8"

	decimal "github.com/wsw/codex-gateway/internal/billing"
)

// Aggregation gateway, stage 4 (migration 0054): seller plans, review, the
// plan marketplace and the sale split.

// Seller plan review states.
const (
	PlanReviewDraft    = "draft"
	PlanReviewPending  = "pending"
	PlanReviewApproved = "approved"
	PlanReviewRejected = "rejected"
)

const maxPlanDescriptionRunes = 1000
const maxPlanReviewNoteRunes = 500

var (
	// ErrMarketplaceDisabled refuses a seller-plan action while the switch is off.
	ErrMarketplaceDisabled = fmt.Errorf("marketplace is disabled: %w", ErrConflict)
	// ErrSellerPlanLimit means the seller already has max_plans_per_user plans.
	ErrSellerPlanLimit = fmt.Errorf("seller plan limit reached: %w", ErrConflict)
	// ErrPlanNotOnSale refuses buying or renewing a seller plan that is not
	// approved, is suspended, or whose seller is not active.
	ErrPlanNotOnSale = fmt.Errorf("plan is not on sale: %w", ErrConflict)
	// ErrSelfPurchase refuses buying one's own plan.
	ErrSelfPurchase = fmt.Errorf("a seller cannot buy their own plan: %w", ErrInvalid)
	// ErrThirdPartyAcknowledgement requires the buyer to accept that a
	// member-run API supplier sees the requests it serves.
	ErrThirdPartyAcknowledgement = fmt.Errorf("plan uses member API suppliers: %w", ErrConflict)
	// ErrSellerPlanSuspended refuses listing a plan the Owner took down.
	ErrSellerPlanSuspended = fmt.Errorf("plan is suspended by the Owner: %w", ErrConflict)
	// ErrPlanReviewState refuses a review action the plan's state does not allow.
	ErrPlanReviewState = fmt.Errorf("plan review state does not allow this: %w", ErrConflict)
)

func normalizePlanDescription(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > maxPlanDescriptionRunes || strings.ContainsRune(value, 0) {
		return "", fmt.Errorf("%w: plan description must be at most %d characters", ErrInvalid, maxPlanDescriptionRunes)
	}
	return value, nil
}

func normalizePlanReviewNote(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > maxPlanReviewNoteRunes || strings.ContainsRune(value, 0) {
		return "", fmt.Errorf("%w: review note must be at most %d characters", ErrInvalid, maxPlanReviewNoteRunes)
	}
	return value, nil
}

// marketplaceSchemaTx reports whether migration 0054 is applied. Upgrade
// tests replay older schemas, which have no seller plans.
func marketplaceSchemaTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	var present bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass('marketplace_sales') IS NOT NULL`).Scan(&present); err != nil {
		return false, mapDBError("check marketplace schema", err)
	}
	return present, nil
}

// requirePlatformPlanTx keeps the Owner's plan routes on platform plans. A
// replay of migrations before 0053 has no seller plans at all.
func requirePlatformPlanTx(ctx context.Context, tx *sql.Tx, planID string) error {
	sellers, err := planScopeSchemaTx(ctx, tx)
	if err != nil || !sellers {
		return err
	}
	var platform bool
	if err := tx.QueryRowContext(ctx, `SELECT seller_user_id IS NULL FROM billing_plans WHERE id=$1`, planID).Scan(&platform); err != nil {
		return mapDBError("read plan seller", err)
	}
	if !platform {
		return ErrNotFound
	}
	return nil
}

// requireSellerPlanTx keeps a seller on the seller's own plans: another
// seller's plan, or a platform plan, reads as not found.
func requireSellerPlanTx(ctx context.Context, tx *sql.Tx, planID, sellerUserID string) error {
	var owned bool
	if err := tx.QueryRowContext(ctx, `SELECT seller_user_id IS NOT DISTINCT FROM $2::uuid FROM billing_plans WHERE id=$1`, planID, sellerUserID).Scan(&owned); err != nil {
		return mapDBError("read plan seller", err)
	}
	if !owned {
		return ErrNotFound
	}
	return nil
}

// sellerPlanCreateAllowedTx checks the switch, that the seller is an active
// member and the per-seller plan limit. The caller holds the seller's billing
// account lock, which serializes the count.
func sellerPlanCreateAllowedTx(ctx context.Context, tx *sql.Tx, sellerUserID string) error {
	var enabled, member bool
	var limit, count int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(ms.enabled,false), COALESCE(ms.max_plans_per_user,0),
			EXISTS(SELECT 1 FROM users u WHERE u.id=$1 AND u.role='member' AND u.status='active'),
			(SELECT count(*) FROM billing_plans p WHERE p.seller_user_id=$1 AND p.deleted_at IS NULL)
		FROM (SELECT 1) one LEFT JOIN marketplace_settings ms ON ms.singleton`, sellerUserID).Scan(&enabled, &limit, &member, &count); err != nil {
		return mapDBError("check seller plan limit", err)
	}
	switch {
	case !enabled:
		return ErrMarketplaceDisabled
	case !member:
		return fmt.Errorf("%w: only active members publish seller plans", ErrInvalid)
	case count >= limit:
		return ErrSellerPlanLimit
	}
	return nil
}

// updateSellerPlanTx saves a seller's edit. A change to what a buyer pays for
// or receives sends an approved or rejected plan back to review (pending,
// off sale); a draft stays a draft. Listing on or off is not such a change,
// but a plan the Owner suspended cannot be listed.
func updateSellerPlanTx(ctx context.Context, tx *sql.Tx, params BillingWriteParams, plan BillingPlan, seller sellerPlanWrite, modelsJSON []byte) (BillingPlan, error) {
	if enabled, err := marketplaceEnabledTx(ctx, tx); err != nil {
		return BillingPlan{}, err
	} else if !enabled {
		return BillingPlan{}, ErrMarketplaceDisabled
	}
	var current BillingPlan
	var owner sql.NullString
	var description, status string
	var suspended bool
	row := tx.QueryRowContext(ctx, `SELECT `+billingPlanColumns+`, seller_user_id, description, review_status, admin_suspended
		FROM billing_plans WHERE id=$1 FOR UPDATE`, plan.ID)
	var models []byte
	if err := row.Scan(&current.ID, &current.Name, &current.PriceUSD, &current.Tier, &current.AllowanceUSD, &current.MinPeriodCount,
		&current.Active, &current.Version, &current.CreatedAt, &current.UpdatedAt, &current.PeriodDays, &models, &current.DeletedAt,
		&owner, &description, &status, &suspended); err != nil {
		return BillingPlan{}, mapDBError("lock seller plan", err)
	}
	if !owner.Valid || owner.String != seller.SellerUserID {
		return BillingPlan{}, ErrNotFound
	}
	if current.DeletedAt != nil || current.Version != plan.Version {
		return BillingPlan{}, ErrBillingPlanChanged
	}
	if suspended && plan.Active && !current.Active {
		return BillingPlan{}, ErrSellerPlanSuspended
	}
	return scanBillingPlan(tx.QueryRowContext(ctx, `UPDATE billing_plans SET
		review_status=CASE WHEN review_status<>'draft' AND (name,price_usd,tier,allowance_usd,min_period_count,period_days,model_ids,description)
			IS DISTINCT FROM ($2::text,$3::numeric,$4::text,$5::numeric,$6::integer,$10::integer,$11::jsonb,$12::text) THEN 'pending' ELSE review_status END,
		submitted_at=CASE WHEN review_status<>'draft' AND (name,price_usd,tier,allowance_usd,min_period_count,period_days,model_ids,description)
			IS DISTINCT FROM ($2::text,$3::numeric,$4::text,$5::numeric,$6::integer,$10::integer,$11::jsonb,$12::text) THEN $8 ELSE submitted_at END,
		version=version+CASE WHEN (name,price_usd,tier,allowance_usd,min_period_count,active,period_days,model_ids,description)
			IS DISTINCT FROM ($2::text,$3::numeric,$4::text,$5::numeric,$6::integer,$7::boolean,$10::integer,$11::jsonb,$12::text) THEN 1 ELSE 0 END,
		name=$2,price_usd=$3::numeric,tier=$4,allowance_usd=$5::numeric,min_period_count=$6,active=$7,updated_at=$8,
		updated_by_user_id=$9,period_days=$10,model_ids=$11::jsonb,description=$12
		WHERE id=$1 RETURNING `+billingPlanColumns, plan.ID, plan.Name, plan.PriceUSD, plan.Tier, plan.AllowanceUSD,
		plan.MinPeriodCount, plan.Active, params.At, params.ActorUserID, plan.PeriodDays, modelsJSON, seller.Description))
}

// SellerPlan is a seller plan with its review state, as the seller and the
// Owner's review queue see it.
type SellerPlan struct {
	BillingPlan
	SellerUserID   string     `json:"seller_user_id"`
	SellerName     string     `json:"seller_name,omitempty"`
	Description    string     `json:"description"`
	ReviewStatus   string     `json:"review_status"`
	ReviewNote     string     `json:"review_note"`
	SubmittedAt    *time.Time `json:"submitted_at,omitempty"`
	ReviewedAt     *time.Time `json:"reviewed_at,omitempty"`
	AdminSuspended bool       `json:"admin_suspended"`
	// OnSale: buyers can buy it in the marketplace right now.
	OnSale bool `json:"on_sale"`
	// Subscribers counts subscriptions bought from the plan that are still
	// enabled and not expired.
	Subscribers int64                 `json:"subscribers"`
	Providers   []MarketPlanProviders `json:"providers"`
}

// MarketPlanProviders summarizes, per protocol, the accounts that serve a
// plan's subscriptions right now: official OAuth accounts and API key
// suppliers.
type MarketPlanProviders struct {
	Provider   string `json:"provider"`
	Official   int64  `json:"official"`
	ThirdParty int64  `json:"third_party"`
}

// MarketPlan is a plan on sale in the marketplace.
type MarketPlan struct {
	BillingPlan
	Platform    bool   `json:"platform"`
	SellerName  string `json:"seller_name"`
	Description string `json:"description"`
	// ThirdPartyAPI: a member-run API supplier may serve this plan's
	// subscriptions and sees those requests; buying needs an explicit
	// acknowledgement.
	ThirdPartyAPI bool                  `json:"third_party_api"`
	Providers     []MarketPlanProviders `json:"providers"`
	// Own: the viewer is the seller and cannot buy it.
	Own bool `json:"own"`
}

const sellerPlanSelect = `SELECT p.id,p.name,p.price_usd::text,p.tier,p.allowance_usd::text,p.min_period_count,p.active,p.version,
	p.created_at,p.updated_at,p.period_days,p.model_ids,p.deleted_at,
	p.seller_user_id,COALESCE(NULLIF(u.display_name,''),u.username),p.description,p.review_status,p.review_note,
	p.submitted_at,p.reviewed_at,p.admin_suspended,` + planOnSaleSQL + `,
	(SELECT count(*) FROM billing_subscriptions s WHERE s.source_plan_id=p.id AND s.enabled
		AND (s.expires_at IS NULL OR s.expires_at > now()))
	FROM billing_plans p JOIN users u ON u.id=p.seller_user_id`

// planOnSaleSQL is true while plan p can be bought: listed and not deleted,
// and for a seller plan also approved, not suspended, its seller active and
// the marketplace on.
const planOnSaleSQL = `(p.deleted_at IS NULL AND p.active AND (p.seller_user_id IS NULL OR (
	p.review_status='approved' AND NOT p.admin_suspended
	AND EXISTS (SELECT 1 FROM users su WHERE su.id=p.seller_user_id AND su.status='active')
	AND COALESCE((SELECT ms.enabled FROM marketplace_settings ms WHERE ms.singleton), false))))`

func scanSellerPlan(row rowScanner) (SellerPlan, error) {
	var plan SellerPlan
	var models []byte
	err := row.Scan(&plan.ID, &plan.Name, &plan.PriceUSD, &plan.Tier, &plan.AllowanceUSD, &plan.MinPeriodCount, &plan.Active,
		&plan.Version, &plan.CreatedAt, &plan.UpdatedAt, &plan.PeriodDays, &models, &plan.DeletedAt,
		&plan.SellerUserID, &plan.SellerName, &plan.Description, &plan.ReviewStatus, &plan.ReviewNote,
		&plan.SubmittedAt, &plan.ReviewedAt, &plan.AdminSuspended, &plan.OnSale, &plan.Subscribers)
	if err == nil {
		err = json.Unmarshal(models, &plan.ModelIDs)
	}
	plan.Providers = []MarketPlanProviders{}
	return plan, err
}

func (s *Store) querySellerPlans(ctx context.Context, where string, args ...any) ([]SellerPlan, error) {
	// #nosec G202 -- callers pass fixed predicates over placeholder names; values are bound parameters.
	rows, err := s.db.QueryContext(ctx, sellerPlanSelect+` WHERE `+where, args...)
	if err != nil {
		return nil, mapDBError("list seller plans", err)
	}
	defer rows.Close()
	plans := make([]SellerPlan, 0)
	ids := make([]string, 0)
	for rows.Next() {
		plan, err := scanSellerPlan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan seller plan: %w", err)
		}
		plans = append(plans, plan)
		ids = append(ids, plan.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	providers, err := s.planProviderSummaries(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range plans {
		if summary, ok := providers[plans[i].ID]; ok {
			plans[i].Providers = summary
		}
	}
	return plans, nil
}

// SellerPlans lists a seller's plans that are not deleted.
func (s *Store) SellerPlans(ctx context.Context, sellerUserID string) ([]SellerPlan, error) {
	if !validUUIDText(sellerUserID) {
		return nil, fmt.Errorf("%w: invalid seller", ErrInvalid)
	}
	return s.querySellerPlans(ctx, `p.seller_user_id=$1 AND p.deleted_at IS NULL ORDER BY p.created_at, p.id`, sellerUserID)
}

// SellerPlan reads one of a seller's plans; another seller's reads as not found.
func (s *Store) SellerPlan(ctx context.Context, sellerUserID, planID string) (SellerPlan, error) {
	if !validUUIDText(sellerUserID) || !validUUIDText(planID) {
		return SellerPlan{}, ErrNotFound
	}
	plans, err := s.querySellerPlans(ctx, `p.seller_user_id=$1 AND p.id=$2 AND p.deleted_at IS NULL`, sellerUserID, planID)
	if err != nil {
		return SellerPlan{}, err
	}
	if len(plans) != 1 {
		return SellerPlan{}, ErrNotFound
	}
	return plans[0], nil
}

// PendingSellerPlans is the Owner's review queue, oldest submission first.
func (s *Store) PendingSellerPlans(ctx context.Context) ([]SellerPlan, error) {
	return s.querySellerPlans(ctx, `p.review_status='pending' AND p.deleted_at IS NULL ORDER BY p.submitted_at, p.id`)
}

// AllSellerPlans lists every seller plan that is not deleted, for the Owner.
func (s *Store) AllSellerPlans(ctx context.Context) ([]SellerPlan, error) {
	return s.querySellerPlans(ctx, `p.deleted_at IS NULL ORDER BY p.created_at, p.id`)
}

// planProviderSummaries counts, per plan and protocol, the accounts that can
// serve it now: accounts of the plan's seller (platform accounts for a
// platform plan) that are neither suspended nor released, within its scope.
func (s *Store) planProviderSummaries(ctx context.Context, planIDs []string) (map[string][]MarketPlanProviders, error) {
	result := make(map[string][]MarketPlanProviders, len(planIDs))
	if len(planIDs) == 0 {
		return result, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT p.id::text, a.provider,
			count(*) FILTER (WHERE a.credential_kind <> 'supplier'), count(*) FILTER (WHERE a.credential_kind = 'supplier')
		FROM billing_plans p
		LEFT JOIN billing_plan_provider_scopes sc ON sc.plan_id = p.id
		JOIN upstream_accounts a ON a.owner_user_id IS NOT DISTINCT FROM p.seller_user_id
			AND NOT a.admin_suspended AND a.credential_removed_at IS NULL
		WHERE p.id = ANY($1::uuid[])
		  AND (COALESCE(sc.mode, 'all') = 'all' OR EXISTS (SELECT 1 FROM billing_plan_provider_accounts pa
			WHERE pa.plan_id = p.id AND pa.account_id = a.id))
		GROUP BY p.id, a.provider ORDER BY p.id, a.provider`, planIDs)
	if err != nil {
		return nil, mapDBError("summarize plan providers", err)
	}
	defer rows.Close()
	for rows.Next() {
		var planID string
		var summary MarketPlanProviders
		if err := rows.Scan(&planID, &summary.Provider, &summary.Official, &summary.ThirdParty); err != nil {
			return nil, fmt.Errorf("scan plan provider summary: %w", err)
		}
		result[planID] = append(result[planID], summary)
	}
	return result, rows.Err()
}

// MarketPlans lists every plan on sale: platform plans and approved seller
// plans of active sellers. viewerUserID marks the viewer's own plans.
func (s *Store) MarketPlans(ctx context.Context, viewerUserID string) ([]MarketPlan, error) {
	// #nosec G202 -- planOnSaleSQL is a fixed SQL constant.
	rows, err := s.db.QueryContext(ctx, `SELECT p.id,p.name,p.price_usd::text,p.tier,p.allowance_usd::text,p.min_period_count,p.active,
			p.version,p.created_at,p.updated_at,p.period_days,p.model_ids,p.deleted_at,
			p.seller_user_id IS NULL, COALESCE(NULLIF(u.display_name,''),u.username,''), p.description,
			p.seller_user_id IS NOT DISTINCT FROM $1::uuid
		FROM billing_plans p LEFT JOIN users u ON u.id = p.seller_user_id
		WHERE `+planOnSaleSQL+` ORDER BY p.seller_user_id IS NOT NULL, p.created_at, p.id`, valueOrNil(viewerUserID))
	if err != nil {
		return nil, mapDBError("list market plans", err)
	}
	defer rows.Close()
	plans := make([]MarketPlan, 0)
	ids := make([]string, 0)
	for rows.Next() {
		var plan MarketPlan
		var models []byte
		if err := rows.Scan(&plan.ID, &plan.Name, &plan.PriceUSD, &plan.Tier, &plan.AllowanceUSD, &plan.MinPeriodCount, &plan.Active,
			&plan.Version, &plan.CreatedAt, &plan.UpdatedAt, &plan.PeriodDays, &models, &plan.DeletedAt,
			&plan.Platform, &plan.SellerName, &plan.Description, &plan.Own); err != nil {
			return nil, fmt.Errorf("scan market plan: %w", err)
		}
		if err := json.Unmarshal(models, &plan.ModelIDs); err != nil {
			return nil, fmt.Errorf("decode market plan models: %w", err)
		}
		plan.Own = plan.Own && !plan.Platform
		plan.Providers = []MarketPlanProviders{}
		plans = append(plans, plan)
		ids = append(ids, plan.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	providers, err := s.planProviderSummaries(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range plans {
		if summary, ok := providers[plans[i].ID]; ok {
			plans[i].Providers = summary
		}
		if !plans[i].Platform {
			for _, item := range plans[i].Providers {
				plans[i].ThirdPartyAPI = plans[i].ThirdPartyAPI || item.ThirdParty > 0
			}
		}
	}
	return plans, nil
}

// planUsesThirdPartyTx reports whether a member-run API supplier can serve the
// seller plan's subscriptions now.
func planUsesThirdPartyTx(ctx context.Context, tx *sql.Tx, planID string) (bool, error) {
	var uses bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM billing_plans p
		LEFT JOIN billing_plan_provider_scopes sc ON sc.plan_id = p.id
		JOIN upstream_accounts a ON a.owner_user_id = p.seller_user_id AND a.credential_kind = 'supplier'
			AND NOT a.admin_suspended AND a.credential_removed_at IS NULL
		WHERE p.id = $1 AND (COALESCE(sc.mode, 'all') = 'all' OR EXISTS (SELECT 1 FROM billing_plan_provider_accounts pa
			WHERE pa.plan_id = p.id AND pa.account_id = a.id)))`, planID).Scan(&uses)
	return uses, mapDBError("check plan third-party suppliers", err)
}

type SellerPlanParams struct {
	BillingWriteParams
	PlanID         string
	Version        int64
	Name           string
	Description    string
	PriceUSD       string
	Tier           string
	PeriodDays     int
	AllowanceUSD   string
	ModelIDs       []string
	MinPeriodCount int
	Active         bool
}

func (p SellerPlanParams) plan() BillingPlan {
	return BillingPlan{ID: p.PlanID, Version: p.Version, Name: p.Name, PriceUSD: p.PriceUSD, Tier: p.Tier,
		PeriodDays: p.PeriodDays, AllowanceUSD: p.AllowanceUSD, ModelIDs: p.ModelIDs, MinPeriodCount: p.MinPeriodCount, Active: p.Active}
}

// CreateSellerPlan publishes a new plan of the acting member as a draft. It is
// sold only after the seller submits it and the Owner approves it.
func (s *Store) CreateSellerPlan(ctx context.Context, params SellerPlanParams) (SellerPlan, error) {
	if params.MinPeriodCount == 0 {
		params.MinPeriodCount = 1
	}
	params.PlanID, params.Version = "", 0
	plan, err := s.writeBillingPlan(ctx, params.BillingWriteParams, params.plan(), true,
		&sellerPlanWrite{SellerUserID: params.ActorUserID, Description: params.Description})
	if err != nil {
		return SellerPlan{}, err
	}
	return s.SellerPlan(ctx, params.ActorUserID, plan.ID)
}

// UpdateSellerPlan edits one of the acting member's plans. See
// updateSellerPlanTx for when the edit returns the plan to review.
func (s *Store) UpdateSellerPlan(ctx context.Context, params SellerPlanParams) (SellerPlan, error) {
	if !validUUIDText(params.PlanID) || params.Version < 1 {
		return SellerPlan{}, fmt.Errorf("%w: plan and version are required", ErrInvalid)
	}
	plan, err := s.writeBillingPlan(ctx, params.BillingWriteParams, params.plan(), false,
		&sellerPlanWrite{SellerUserID: params.ActorUserID, Description: params.Description})
	if err != nil {
		return SellerPlan{}, err
	}
	return s.SellerPlan(ctx, params.ActorUserID, plan.ID)
}

type SetSellerPlanListingParams struct {
	BillingWriteParams
	PlanID  string
	Version int64
	Active  bool
}

// SetSellerPlanListing lists or delists one of the acting member's plans
// without changing anything else, so it never returns the plan to review.
func (s *Store) SetSellerPlanListing(ctx context.Context, params SetSellerPlanListingParams) (SellerPlan, error) {
	current, err := s.SellerPlan(ctx, params.ActorUserID, params.PlanID)
	if err != nil {
		return SellerPlan{}, err
	}
	if current.Version != params.Version {
		return SellerPlan{}, ErrBillingPlanChanged
	}
	return s.UpdateSellerPlan(ctx, SellerPlanParams{BillingWriteParams: params.BillingWriteParams, PlanID: current.ID,
		Version: current.Version, Name: current.Name, Description: current.Description, PriceUSD: current.PriceUSD,
		Tier: current.Tier, PeriodDays: current.PeriodDays, AllowanceUSD: current.AllowanceUSD, ModelIDs: current.ModelIDs,
		MinPeriodCount: current.MinPeriodCount, Active: params.Active})
}

type PlanReviewWriteParams struct {
	PlanID         string
	Version        int64
	ActorUserID    string
	ActorSessionID string
	RequestID      string
	SourceIP       string
	At             time.Time
}

func (p PlanReviewWriteParams) audit() BillingWriteParams {
	return BillingWriteParams{ActorUserID: p.ActorUserID, ActorSessionID: p.ActorSessionID, RequestID: p.RequestID, SourceIP: p.SourceIP, At: p.At}
}

// SubmitSellerPlan sends a draft or rejected plan to the Owner's review.
// Version must match the plan the seller is looking at.
func (s *Store) SubmitSellerPlan(ctx context.Context, params PlanReviewWriteParams) (SellerPlan, error) {
	if !validUUIDText(params.PlanID) || params.Version < 1 || !validUUIDText(params.ActorUserID) {
		return SellerPlan{}, fmt.Errorf("%w: invalid plan submission", ErrInvalid)
	}
	params.At = normalizedBillingTime(params.At, s.now)
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if enabled, err := marketplaceEnabledTx(ctx, tx); err != nil {
			return err
		} else if !enabled {
			return ErrMarketplaceDisabled
		}
		var status string
		var version int64
		if err := tx.QueryRowContext(ctx, `SELECT review_status, version FROM billing_plans
			WHERE id=$1 AND seller_user_id=$2 AND deleted_at IS NULL FOR UPDATE`, params.PlanID, params.ActorUserID).Scan(&status, &version); err != nil {
			return mapDBError("lock submitted plan", err)
		}
		if version != params.Version {
			return ErrBillingPlanChanged
		}
		if status != PlanReviewDraft && status != PlanReviewRejected {
			return ErrPlanReviewState
		}
		if _, err := tx.ExecContext(ctx, `UPDATE billing_plans SET review_status='pending', submitted_at=$2 WHERE id=$1`, params.PlanID, params.At); err != nil {
			return mapDBError("submit seller plan", err)
		}
		return appendBillingAuditTx(ctx, tx, params.audit(), "marketplace.plan_submitted", "billing_plan", params.PlanID,
			map[string]any{"version": params.Version, "previous_status": status})
	})
	if err != nil {
		return SellerPlan{}, err
	}
	return s.SellerPlan(ctx, params.ActorUserID, params.PlanID)
}

type ReviewSellerPlanParams struct {
	PlanReviewWriteParams
	Approve bool
	Note    string
}

// ReviewSellerPlan approves or rejects a pending plan at the version the
// Owner reviewed. An approved plan that is listed is on sale at once.
func (s *Store) ReviewSellerPlan(ctx context.Context, params ReviewSellerPlanParams) (SellerPlan, error) {
	note, err := normalizePlanReviewNote(params.Note)
	if err != nil {
		return SellerPlan{}, err
	}
	if !validUUIDText(params.PlanID) || params.Version < 1 || !validUUIDText(params.ActorUserID) {
		return SellerPlan{}, fmt.Errorf("%w: invalid plan review", ErrInvalid)
	}
	if !params.Approve && note == "" {
		return SellerPlan{}, fmt.Errorf("%w: a rejection needs a note", ErrInvalid)
	}
	params.At = normalizedBillingTime(params.At, s.now)
	decision := PlanReviewRejected
	if params.Approve {
		decision = PlanReviewApproved
	}
	var seller string
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		var status string
		var version int64
		if err := tx.QueryRowContext(ctx, `SELECT review_status, version, seller_user_id FROM billing_plans
			WHERE id=$1 AND seller_user_id IS NOT NULL AND deleted_at IS NULL FOR UPDATE`, params.PlanID).Scan(&status, &version, &seller); err != nil {
			return mapDBError("lock reviewed plan", err)
		}
		if version != params.Version {
			return ErrBillingPlanChanged
		}
		if status != PlanReviewPending {
			return ErrPlanReviewState
		}
		if _, err := tx.ExecContext(ctx, `UPDATE billing_plans SET review_status=$2, review_note=$3,
			reviewed_by_user_id=$4, reviewed_at=$5 WHERE id=$1`, params.PlanID, decision, note, params.ActorUserID, params.At); err != nil {
			return mapDBError("review seller plan", err)
		}
		return appendBillingAuditTx(ctx, tx, params.audit(), "marketplace.plan_reviewed", "billing_plan", params.PlanID,
			map[string]any{"version": params.Version, "decision": decision, "note": note})
	})
	if err != nil {
		return SellerPlan{}, err
	}
	return s.SellerPlan(ctx, seller, params.PlanID)
}

type SetSellerPlanSuspendedParams struct {
	PlanReviewWriteParams
	Suspended bool
	Reason    string
}

// SetSellerPlanSuspended takes a seller plan off sale (or lifts that). The
// seller cannot list it again while it is suspended. Subscriptions already
// sold keep their layer: suspension stops sales, not service.
func (s *Store) SetSellerPlanSuspended(ctx context.Context, params SetSellerPlanSuspendedParams) (SellerPlan, error) {
	reason, err := normalizePlanReviewNote(params.Reason)
	if err != nil || !validUUIDText(params.PlanID) || !validUUIDText(params.ActorUserID) || reason == "" {
		return SellerPlan{}, fmt.Errorf("%w: invalid plan suspension", ErrInvalid)
	}
	params.At = normalizedBillingTime(params.At, s.now)
	var seller string
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		var previous bool
		if err := tx.QueryRowContext(ctx, `SELECT admin_suspended, seller_user_id FROM billing_plans
			WHERE id=$1 AND seller_user_id IS NOT NULL AND deleted_at IS NULL FOR UPDATE`, params.PlanID).Scan(&previous, &seller); err != nil {
			return mapDBError("lock suspended plan", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE billing_plans SET admin_suspended=$2 WHERE id=$1`, params.PlanID, params.Suspended); err != nil {
			return mapDBError("suspend seller plan", err)
		}
		return appendBillingAuditTx(ctx, tx, params.audit(), "marketplace.plan_suspended", "billing_plan", params.PlanID,
			map[string]any{"suspended": params.Suspended, "previous_suspended": previous, "reason": reason})
	})
	if err != nil {
		return SellerPlan{}, err
	}
	return s.SellerPlan(ctx, seller, params.PlanID)
}

// commissionAmounts splits a sale: the commission is gross x rate rounded
// down to 12 decimals, and the seller's net is the rest.
func commissionAmounts(gross, rate string) (commission, net string, err error) {
	total, err := billingRat(gross)
	if err != nil {
		return "", "", err
	}
	share, ok := new(big.Rat).SetString(rate)
	if !ok || share.Sign() < 0 || share.Cmp(big.NewRat(1, 1)) > 0 || total.Sign() < 0 {
		return "", "", fmt.Errorf("%w: invalid commission rate", ErrInvalid)
	}
	scaled := new(big.Rat).Mul(total, share)
	scaled.Mul(scaled, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(decimal.AmountScale), nil)))
	units := new(big.Int).Quo(scaled.Num(), scaled.Denom())
	value := new(big.Rat).SetFrac(units, new(big.Int).Exp(big.NewInt(10), big.NewInt(decimal.AmountScale), nil))
	commission = value.FloatString(decimal.AmountScale)
	net, err = billingSubtract(gross, commission)
	return commission, net, err
}

// MarketplaceSale is one seller plan sale. BuyerUserID is never shown to the
// seller, who sees BuyerNumber instead.
type MarketplaceSale struct {
	ID             int64     `json:"id"`
	Kind           string    `json:"kind"`
	PlanID         string    `json:"plan_id"`
	PlanName       string    `json:"plan_name"`
	PlanVersion    int64     `json:"plan_version"`
	SellerUserID   string    `json:"seller_user_id,omitempty"`
	SellerName     string    `json:"seller_name,omitempty"`
	BuyerUserID    string    `json:"buyer_user_id,omitempty"`
	BuyerName      string    `json:"buyer_name,omitempty"`
	BuyerNumber    int       `json:"buyer_number"`
	PeriodCount    int       `json:"period_count"`
	GrossUSD       string    `json:"gross_usd"`
	CommissionRate string    `json:"commission_rate"`
	CommissionUSD  string    `json:"commission_usd"`
	NetUSD         string    `json:"net_usd"`
	CreatedAt      time.Time `json:"created_at"`
}

// MarketplaceSalesSummary totals sales.
type MarketplaceSalesSummary struct {
	Count         int64  `json:"count"`
	GrossUSD      string `json:"gross_usd"`
	CommissionUSD string `json:"commission_usd"`
	NetUSD        string `json:"net_usd"`
	Buyers        int64  `json:"buyers"`
}

type MarketplaceSales struct {
	Sales   []MarketplaceSale       `json:"sales"`
	Summary MarketplaceSalesSummary `json:"summary"`
	Total   int64                   `json:"total"`
}

// recordPlanSaleTx splits a seller plan sale inside the purchase or renewal
// transaction, which already holds both billing account locks: the seller's
// net becomes a cash lot of its own (plan_sale_income), and the sale is
// recorded once per operation.
func recordPlanSaleTx(ctx context.Context, tx *sql.Tx, params BillingWriteParams, plan BillingPlan, seller, buyer, subscriptionID, kind string, count int, gross string, buyerEntry int64) (MarketplaceSale, error) {
	var sale MarketplaceSale
	var rate string
	if err := tx.QueryRowContext(ctx, `SELECT trim_scale(commission_rate)::text FROM marketplace_settings WHERE singleton FOR SHARE`).Scan(&rate); err != nil {
		return sale, mapDBError("read commission rate", err)
	}
	commission, net, err := commissionAmounts(gross, rate)
	if err != nil {
		return sale, err
	}
	var buyerNumber int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(
			(SELECT buyer_number FROM marketplace_sales WHERE seller_user_id=$1 AND buyer_user_id=$2 LIMIT 1),
			(SELECT COALESCE(max(buyer_number),0)+1 FROM marketplace_sales WHERE seller_user_id=$1))`, seller, buyer).Scan(&buyerNumber); err != nil {
		return sale, mapDBError("number plan buyer", err)
	}
	sale = MarketplaceSale{Kind: kind, PlanID: plan.ID, PlanName: plan.Name, PlanVersion: plan.Version, BuyerNumber: buyerNumber,
		PeriodCount: count, GrossUSD: gross, CommissionRate: rate, CommissionUSD: commission, NetUSD: net, CreatedAt: params.At}
	var sellerEntry any
	if billingPositive(net) {
		var balance string
		var sequence int64
		if err := tx.QueryRowContext(ctx, `UPDATE billing_accounts SET balance_usd = balance_usd + $2::numeric,
				next_cash_lot_sequence = next_cash_lot_sequence + 1, updated_at = $3
			WHERE user_id = $1 RETURNING balance_usd::text, next_cash_lot_sequence - 1`, seller, net, params.At).Scan(&balance, &sequence); err != nil {
			return sale, mapDBError("credit seller balance", err)
		}
		snapshot, err := json.Marshal(map[string]any{"sale": sale})
		if err != nil {
			return sale, err
		}
		// The entry names neither the buyer nor the buyer as actor: the seller
		// reads it in their own ledger.
		var entryID int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO billing_ledger_entries
				(user_id, entry_type, amount_usd, cash_delta_usd, balance_after_usd, reason, created_at, transaction_snapshot)
			VALUES ($1,'plan_sale_income',$2::numeric,$2::numeric,$3::numeric,$4,$5,$6) RETURNING id`,
			seller, net, balance, "套餐销售收入："+plan.Name, params.At, snapshot).Scan(&entryID); err != nil {
			return sale, mapDBError("record plan sale income", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO billing_cash_credit_lots
				(user_id, lot_sequence, source_ledger_entry_id, original_usd, remaining_usd, created_at)
			VALUES ($1,$2,$3,$4::numeric,$4::numeric,$5)`, seller, sequence, entryID, net, params.At); err != nil {
			return sale, mapDBError("create plan sale credit lot", err)
		}
		sellerEntry = entryID
	}
	if err := tx.QueryRowContext(ctx, `INSERT INTO marketplace_sales
			(operation_id, kind, plan_id, plan_version, seller_user_id, buyer_user_id, buyer_number, subscription_id,
			 period_count, gross_usd, commission_rate, commission_usd, net_usd, buyer_ledger_entry_id, seller_ledger_entry_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::numeric,$11::numeric,$12::numeric,$13::numeric,$14,$15,$16) RETURNING id`,
		params.OperationID, kind, plan.ID, plan.Version, seller, buyer, buyerNumber, subscriptionID, count, gross, rate,
		commission, net, buyerEntry, sellerEntry, params.At).Scan(&sale.ID); err != nil {
		return sale, mapDBError("record plan sale", err)
	}
	return sale, appendBillingAuditTx(ctx, tx, params, "marketplace.plan_sold", "billing_plan", plan.ID, map[string]any{
		"operation_id": params.OperationID, "kind": kind, "plan_version": plan.Version, "seller_user_id": seller,
		"buyer_user_id": buyer, "gross_usd": gross, "commission_rate": rate, "commission_usd": commission, "net_usd": net,
	})
}

const marketplaceSaleColumns = `m.id, m.kind, m.plan_id::text, p.name, m.plan_version, m.seller_user_id::text,
	COALESCE(NULLIF(su.display_name,''),su.username), m.buyer_user_id::text, COALESCE(NULLIF(bu.display_name,''),bu.username),
	m.buyer_number, m.period_count, m.gross_usd::text, trim_scale(m.commission_rate)::text, m.commission_usd::text,
	m.net_usd::text, m.created_at`

const marketplaceSaleJoins = ` FROM marketplace_sales m JOIN billing_plans p ON p.id = m.plan_id
	JOIN users su ON su.id = m.seller_user_id JOIN users bu ON bu.id = m.buyer_user_id`

// SellerSales lists a seller's sales, newest first, with totals. The buyer is
// only the per-seller buyer number.
func (s *Store) SellerSales(ctx context.Context, sellerUserID string, limit, offset int) (MarketplaceSales, error) {
	if !validUUIDText(sellerUserID) {
		return MarketplaceSales{}, fmt.Errorf("%w: invalid seller", ErrInvalid)
	}
	sales, err := s.marketplaceSales(ctx, `m.seller_user_id=$1`, []any{sellerUserID}, limit, offset)
	for i := range sales.Sales {
		sales.Sales[i].SellerUserID, sales.Sales[i].SellerName = "", ""
		sales.Sales[i].BuyerUserID, sales.Sales[i].BuyerName = "", ""
	}
	return sales, err
}

// AllMarketplaceSales lists every sale for the Owner, with buyers and sellers.
func (s *Store) AllMarketplaceSales(ctx context.Context, limit, offset int) (MarketplaceSales, error) {
	return s.marketplaceSales(ctx, `true`, nil, limit, offset)
}

func (s *Store) marketplaceSales(ctx context.Context, where string, args []any, limit, offset int) (MarketplaceSales, error) {
	if limit < 1 || limit > 200 || offset < 0 {
		return MarketplaceSales{}, fmt.Errorf("%w: invalid sales page", ErrInvalid)
	}
	result := MarketplaceSales{Sales: []MarketplaceSale{}}
	// #nosec G202 -- callers pass fixed predicates over placeholder names; values are bound parameters.
	if err := s.db.QueryRowContext(ctx, `SELECT count(*), COALESCE(sum(m.gross_usd),0)::numeric(30,12)::text,
			COALESCE(sum(m.commission_usd),0)::numeric(30,12)::text, COALESCE(sum(m.net_usd),0)::numeric(30,12)::text,
			count(DISTINCT m.buyer_user_id) FROM marketplace_sales m WHERE `+where, args...).Scan(
		&result.Summary.Count, &result.Summary.GrossUSD, &result.Summary.CommissionUSD, &result.Summary.NetUSD, &result.Summary.Buyers); err != nil {
		return result, mapDBError("summarize marketplace sales", err)
	}
	result.Total = result.Summary.Count
	page := append(append([]any(nil), args...), limit, offset)
	// #nosec G202 -- callers pass fixed predicates over placeholder names; values are bound parameters.
	rows, err := s.db.QueryContext(ctx, `SELECT `+marketplaceSaleColumns+marketplaceSaleJoins+` WHERE `+where+
		fmt.Sprintf(` ORDER BY m.created_at DESC, m.id DESC LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2), page...)
	if err != nil {
		return result, mapDBError("list marketplace sales", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sale MarketplaceSale
		if err := rows.Scan(&sale.ID, &sale.Kind, &sale.PlanID, &sale.PlanName, &sale.PlanVersion, &sale.SellerUserID,
			&sale.SellerName, &sale.BuyerUserID, &sale.BuyerName, &sale.BuyerNumber, &sale.PeriodCount, &sale.GrossUSD,
			&sale.CommissionRate, &sale.CommissionUSD, &sale.NetUSD, &sale.CreatedAt); err != nil {
			return result, fmt.Errorf("scan marketplace sale: %w", err)
		}
		result.Sales = append(result.Sales, sale)
	}
	return result, rows.Err()
}

// SellerPlanAccountUsage is one account's usage paid by a plan's
// subscriptions. Buyers are not identified.
type SellerPlanAccountUsage struct {
	AccountID    string `json:"account_id"`
	Label        string `json:"label"`
	Provider     string `json:"provider"`
	Requests     int64  `json:"requests"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	ChargedUSD   string `json:"charged_usd"`
}

// SellerPlanUsage summarizes a seller plan: active subscribers, how much of
// its allowance was consumed and, per account, the requests it paid for.
type SellerPlanUsage struct {
	PlanID      string                   `json:"plan_id"`
	Subscribers int64                    `json:"subscribers"`
	ConsumedUSD string                   `json:"consumed_usd"`
	Accounts    []SellerPlanAccountUsage `json:"accounts"`
}

// SellerPlanUsage aggregates usage charged to subscriptions bought from one of
// the seller's plans, by account.
func (s *Store) SellerPlanUsage(ctx context.Context, sellerUserID, planID string) (SellerPlanUsage, error) {
	plan, err := s.SellerPlan(ctx, sellerUserID, planID)
	if err != nil {
		return SellerPlanUsage{}, err
	}
	usage := SellerPlanUsage{PlanID: plan.ID, Subscribers: plan.Subscribers, Accounts: []SellerPlanAccountUsage{}}
	rows, err := s.db.QueryContext(ctx, `WITH charged AS (
			SELECT a.request_id, sum(a.amount_usd) amount
			FROM billing_charge_allocations a
			JOIN billing_subscription_periods sp ON sp.id = a.subscription_period_id
			JOIN billing_subscriptions s ON s.id = sp.subscription_id
			WHERE a.source_type = 'subscription' AND s.source_plan_id = $1
			GROUP BY a.request_id)
		SELECT COALESCE(l.upstream_account_id,''), COALESCE(NULLIF(u.alias,''),NULLIF(u.display_name,''),NULLIF(u.masked_email,''),l.upstream_account_id,''),
			COALESCE(u.provider,''), count(*), COALESCE(sum(l.input_tokens),0), COALESCE(sum(l.output_tokens),0),
			sum(c.amount)::numeric(30,12)::text
		FROM charged c JOIN billing_ledger_entries l ON l.request_id = c.request_id AND l.entry_type = 'usage_charge'
		LEFT JOIN upstream_accounts u ON u.id = l.upstream_account_id
		GROUP BY 1, 2, 3 ORDER BY 7 DESC, 1`, planID)
	if err != nil {
		return usage, mapDBError("aggregate seller plan usage", err)
	}
	defer rows.Close()
	consumed := "0.000000000000"
	for rows.Next() {
		var item SellerPlanAccountUsage
		if err := rows.Scan(&item.AccountID, &item.Label, &item.Provider, &item.Requests, &item.InputTokens, &item.OutputTokens, &item.ChargedUSD); err != nil {
			return usage, fmt.Errorf("scan seller plan usage: %w", err)
		}
		if consumed, err = billingAdd(consumed, item.ChargedUSD); err != nil {
			return usage, err
		}
		usage.Accounts = append(usage.Accounts, item)
	}
	usage.ConsumedUSD = consumed
	return usage, rows.Err()
}

// planSaleState is what a purchase or renewal checks about a seller plan
// under the buyer's and seller's locks.
type planSaleState struct {
	seller      string
	status      string
	suspended   bool
	sellerOK    bool
	marketplace bool
}

func readPlanSaleStateTx(ctx context.Context, tx *sql.Tx, planID string) (planSaleState, error) {
	var state planSaleState
	var seller sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT p.seller_user_id, p.review_status, p.admin_suspended,
			COALESCE((SELECT u.status='active' FROM users u WHERE u.id=p.seller_user_id), false),
			COALESCE((SELECT ms.enabled FROM marketplace_settings ms WHERE ms.singleton), false)
		FROM billing_plans p WHERE p.id=$1`, planID).Scan(&seller, &state.status, &state.suspended, &state.sellerOK, &state.marketplace)
	state.seller = seller.String
	if errors.Is(err, sql.ErrNoRows) {
		return state, ErrBillingPlanChanged
	}
	return state, mapDBError("read plan sale state", err)
}

// planSellerForTransactionTx reads, without locks, the seller of the plan a
// purchase or renewal is about to pay for. seller_user_id never changes, and
// the transaction re-checks it once the account locks are held.
func planSellerForTransactionTx(ctx context.Context, tx *sql.Tx, userID, planID, subscriptionID string, renew bool) (string, error) {
	var seller sql.NullString
	var err error
	if renew {
		err = tx.QueryRowContext(ctx, `SELECT p.seller_user_id FROM billing_subscriptions s
			JOIN billing_plans p ON p.id = s.plan_id WHERE s.user_id=$1 AND s.id=$2`, userID, subscriptionID).Scan(&seller)
	} else {
		err = tx.QueryRowContext(ctx, `SELECT seller_user_id FROM billing_plans WHERE id=$1`, planID).Scan(&seller)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", mapDBError("read plan seller", err)
	}
	return seller.String, nil
}

// lockBillingAccountsTx locks the given billing accounts in UUID order, so two
// members buying each other's plans at once cannot deadlock.
func lockBillingAccountsTx(ctx context.Context, tx *sql.Tx, userIDs ...string) error {
	ordered := make([]string, 0, len(userIDs))
	for _, id := range userIDs {
		if id == "" {
			continue
		}
		duplicate := false
		for _, existing := range ordered {
			duplicate = duplicate || existing == id
		}
		if !duplicate {
			ordered = append(ordered, id)
		}
	}
	for i := 1; i < len(ordered); i++ {
		for j := i; j > 0 && ordered[j] < ordered[j-1]; j-- {
			ordered[j], ordered[j-1] = ordered[j-1], ordered[j]
		}
	}
	for _, id := range ordered {
		if err := lockBillingAccountTx(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}
