package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	decimal "github.com/wsw/codex-gateway/internal/billing"
	"github.com/wsw/codex-gateway/internal/config"
)

const (
	BillingTierDay   = "day"
	BillingTierWeek  = "week"
	BillingTierMonth = "month"

	BillingModeLegacy              = "legacy"
	BillingModeOpenAIAPIEquivalent = "openai_api_token_equivalent"
	BillingModeInternalZero        = "internal_zero"
)

// InsufficientFundsError is returned when a billed endpoint has no positive
// admission-time source. RetryAfter is the earliest enabled subscription
// renewal, when one exists.
type InsufficientFundsError struct {
	RetryAfter time.Duration
}

func (e *InsufficientFundsError) Error() string { return "billing: insufficient quota" }

type BillingSettings struct {
	USDPerCNY       string    `json:"usd_per_cny"`
	UpdatedAt       time.Time `json:"updated_at"`
	UpdatedByUserID *string   `json:"updated_by_user_id,omitempty"`
}

type BillingSubscriptionState struct {
	ID                  string     `json:"id,omitempty"`
	Tier                string     `json:"tier"`
	Enabled             bool       `json:"enabled"`
	AllowanceUSD        string     `json:"allowance_usd"`
	RemainingUSD        string     `json:"remaining_usd"`
	PeriodCount         int        `json:"period_count"`
	CurrentPeriodNumber int        `json:"current_period_number"`
	ExpiresAt           *time.Time `json:"expires_at"`
	PeriodID            *string    `json:"period_id,omitempty"`
	PeriodStartsAt      *time.Time `json:"period_starts_at,omitempty"`
	PeriodEndsAt        *time.Time `json:"period_ends_at,omitempty"`
	UpdatedAt           *time.Time `json:"updated_at,omitempty"`
}

type BillingLedgerEntry struct {
	ID                              int64      `json:"id"`
	UserID                          *string    `json:"user_id,omitempty"`
	OperationID                     *string    `json:"operation_id,omitempty"`
	EntryType                       string     `json:"entry_type"`
	AmountUSD                       string     `json:"amount_usd"`
	CashDeltaUSD                    string     `json:"cash_delta_usd"`
	BalanceAfterUSD                 *string    `json:"balance_after_usd,omitempty"`
	CNYAmount                       *string    `json:"cny_amount,omitempty"`
	USDPerCNYSnapshot               *string    `json:"usd_per_cny_snapshot,omitempty"`
	SubscriptionTier                *string    `json:"subscription_tier,omitempty"`
	SubscriptionPeriodID            *string    `json:"subscription_period_id,omitempty"`
	RequestID                       *string    `json:"request_id,omitempty"`
	Model                           *string    `json:"model,omitempty"`
	ActualModel                     *string    `json:"actual_model,omitempty"`
	InputTokens                     *int64     `json:"input_tokens,omitempty"`
	CachedInputTokens               *int64     `json:"cached_input_tokens,omitempty"`
	CacheWriteTokens                *int64     `json:"cache_write_tokens,omitempty"`
	OutputTokens                    *int64     `json:"output_tokens,omitempty"`
	CacheWriteMode                  *string    `json:"cache_write_mode,omitempty"`
	RequestedServiceTier            *string    `json:"requested_service_tier,omitempty"`
	ActualServiceTier               *string    `json:"actual_service_tier,omitempty"`
	PricingServiceTier              *string    `json:"pricing_service_tier,omitempty"`
	ContextClass                    *string    `json:"context_class,omitempty"`
	PricingRuleVersion              int        `json:"pricing_rule_version"`
	PricingCatalogAsOf              *string    `json:"pricing_catalog_as_of,omitempty"`
	AppliedInputUSDPerMillion       *string    `json:"applied_input_usd_per_million,omitempty"`
	AppliedCachedInputUSDPerMillion *string    `json:"applied_cached_input_usd_per_million,omitempty"`
	AppliedCacheWriteUSDPerMillion  *string    `json:"applied_cache_write_usd_per_million,omitempty"`
	AppliedOutputUSDPerMillion      *string    `json:"applied_output_usd_per_million,omitempty"`
	PricingFallbackReason           *string    `json:"pricing_fallback_reason,omitempty"`
	UsageRequestedAt                *time.Time `json:"usage_requested_at,omitempty"`
	ActualCostUSD                   *string    `json:"actual_cost_usd,omitempty"`
	ChargedUSD                      *string    `json:"charged_usd,omitempty"`
	UncoveredUSD                    *string    `json:"uncovered_usd,omitempty"`
	Reason                          string     `json:"reason"`
	ActorUserID                     *string    `json:"actor_user_id,omitempty"`
	CreatedAt                       time.Time  `json:"created_at"`
}

type BillingState struct {
	UserID        string                     `json:"user_id"`
	Username      string                     `json:"username,omitempty"`
	DisplayName   string                     `json:"display_name,omitempty"`
	BalanceUSD    string                     `json:"balance_usd"`
	Subscriptions []BillingSubscriptionState `json:"subscriptions"`
	Ledger        []BillingLedgerEntry       `json:"ledger"`
	LedgerTotal   int64                      `json:"ledger_total"`
}

type BillingUserSummary struct {
	UserID        string                     `json:"user_id"`
	Username      string                     `json:"username"`
	DisplayName   string                     `json:"display_name"`
	Role          string                     `json:"role"`
	Status        string                     `json:"status"`
	BalanceUSD    string                     `json:"balance_usd"`
	Subscriptions []BillingSubscriptionState `json:"subscriptions"`
}

type BillingReservationParams struct {
	RequestID                string
	UserID                   string
	APIKeyID                 string
	Model                    string
	InputUSDPerMillion       string
	CachedInputUSDPerMillion string
	OutputUSDPerMillion      string
	PricingRuleVersion       int
	BillingMode              string
	PricingCatalogAsOf       string
	PricingModel             string
	PricingSnapshot          []byte
	CacheWriteMode           string
	RequestedServiceTier     string
	Now                      time.Time
}

type BillingReservation struct {
	RequestID                       string          `json:"request_id"`
	UserID                          string          `json:"user_id"`
	APIKeyID                        string          `json:"api_key_id"`
	Model                           string          `json:"model"`
	InputUSDPerMillion              string          `json:"input_usd_per_million"`
	CachedInputUSDPerMillion        string          `json:"cached_input_usd_per_million"`
	OutputUSDPerMillion             string          `json:"output_usd_per_million"`
	PricingRuleVersion              int             `json:"pricing_rule_version"`
	BillingMode                     string          `json:"billing_mode"`
	PricingCatalogAsOf              *string         `json:"pricing_catalog_as_of,omitempty"`
	PricingModel                    *string         `json:"pricing_model,omitempty"`
	PricingSnapshot                 json.RawMessage `json:"pricing_snapshot,omitempty"`
	CacheWriteMode                  *string         `json:"cache_write_mode,omitempty"`
	RequestedServiceTier            *string         `json:"requested_service_tier,omitempty"`
	ActualServiceTier               *string         `json:"actual_service_tier,omitempty"`
	PricingServiceTier              *string         `json:"pricing_service_tier,omitempty"`
	ActualModel                     *string         `json:"actual_model,omitempty"`
	ContextClass                    *string         `json:"context_class,omitempty"`
	ActualCacheWriteTokens          *int64          `json:"actual_cache_write_tokens,omitempty"`
	AppliedInputUSDPerMillion       *string         `json:"applied_input_usd_per_million,omitempty"`
	AppliedCachedInputUSDPerMillion *string         `json:"applied_cached_input_usd_per_million,omitempty"`
	AppliedCacheWriteUSDPerMillion  *string         `json:"applied_cache_write_usd_per_million,omitempty"`
	AppliedOutputUSDPerMillion      *string         `json:"applied_output_usd_per_million,omitempty"`
	PricingFallbackReason           *string         `json:"pricing_fallback_reason,omitempty"`
	DayPeriodID                     *string         `json:"day_period_id,omitempty"`
	WeekPeriodID                    *string         `json:"week_period_id,omitempty"`
	MonthPeriodID                   *string         `json:"month_period_id,omitempty"`
	CashLotCutoff                   *int64          `json:"cash_lot_cutoff,omitempty"`
	State                           string          `json:"state"`
	ActualInputTokens               *int64          `json:"actual_input_tokens,omitempty"`
	ActualCachedInputTokens         *int64          `json:"actual_cached_input_tokens,omitempty"`
	ActualOutputTokens              *int64          `json:"actual_output_tokens,omitempty"`
	ActualCostUSD                   *string         `json:"actual_cost_usd,omitempty"`
	ChargedUSD                      *string         `json:"charged_usd,omitempty"`
	UncoveredUSD                    *string         `json:"uncovered_usd,omitempty"`
	CreatedAt                       time.Time       `json:"created_at"`
	SettledAt                       *time.Time      `json:"settled_at,omitempty"`
}

type BillingWriteParams struct {
	OperationID    string
	Reason         string
	ActorUserID    string
	ActorSessionID string
	RequestID      string
	SourceIP       string
	At             time.Time
}

type SetRechargeRateParams struct {
	BillingWriteParams
	USDPerCNY string
}

type RechargeUserParams struct {
	BillingWriteParams
	UserID    string
	CNYAmount string
}

type AdjustUserBalanceParams struct {
	BillingWriteParams
	UserID    string
	USDAmount string
}

type PutSubscriptionParams struct {
	BillingWriteParams
	UserID       string
	Tier         string
	AllowanceUSD string
	PeriodCount  int
}

type DeleteSubscriptionParams struct {
	BillingWriteParams
	UserID string
	Tier   string
}

func billingPeriodDuration(tier string) (time.Duration, error) {
	switch tier {
	case BillingTierDay:
		return 24 * time.Hour, nil
	case BillingTierWeek:
		return 7 * 24 * time.Hour, nil
	case BillingTierMonth:
		return 31 * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("%w: invalid subscription tier", ErrInvalid)
	}
}

func validateBillingPeriodCount(periodCount int) error {
	if periodCount < 0 || periodCount > 99 {
		return fmt.Errorf("%w: subscription period count must be between 0 and 99", ErrInvalid)
	}
	return nil
}

func billingSubscriptionExpiry(periodStart time.Time, duration time.Duration, periodNumber, periodCount int) *time.Time {
	if periodCount == 0 {
		return nil
	}
	remainingPeriods := periodCount - periodNumber + 1
	expiresAt := periodStart.Add(time.Duration(remainingPeriods) * duration)
	return &expiresAt
}

func normalizedBillingTime(value time.Time, now func() time.Time) time.Time {
	if value.IsZero() {
		return now().UTC()
	}
	return value.UTC()
}

func validateBillingWrite(params BillingWriteParams) error {
	if params.OperationID == "" || params.ActorUserID == "" ||
		strings.TrimSpace(params.Reason) == "" || len([]rune(strings.TrimSpace(params.Reason))) > 500 {
		return fmt.Errorf("%w: operation id, actor and reason are required", ErrInvalid)
	}
	return nil
}

func nullableString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func nullableInt64(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	return &value.Int64
}

func scanBillingReservation(row rowScanner) (BillingReservation, error) {
	var value BillingReservation
	var day, week, month sql.NullString
	var cutoff sql.NullInt64
	var inputPrice, cachedPrice, outputPrice sql.NullString
	var catalog, pricingModel, cacheWriteMode, requestedTier, actualTier sql.NullString
	var pricingTier, actualModel, contextClass, fallbackReason sql.NullString
	var inputTokens, cachedTokens, cacheWriteTokens, outputTokens sql.NullInt64
	var appliedInput, appliedCached, appliedCacheWrite, appliedOutput sql.NullString
	var actualCost, charged, uncovered sql.NullString
	var snapshot []byte
	err := row.Scan(&value.RequestID, &value.UserID, &value.APIKeyID, &value.Model,
		&inputPrice, &cachedPrice, &outputPrice, &value.PricingRuleVersion,
		&value.BillingMode, &catalog, &pricingModel, &snapshot, &cacheWriteMode,
		&requestedTier, &actualTier, &pricingTier, &actualModel, &contextClass,
		&cacheWriteTokens, &appliedInput, &appliedCached, &appliedCacheWrite,
		&appliedOutput, &fallbackReason, &day, &week, &month, &cutoff,
		&value.State, &inputTokens, &cachedTokens,
		&outputTokens, &actualCost, &charged, &uncovered, &value.CreatedAt, &value.SettledAt)
	value.InputUSDPerMillion, value.CachedInputUSDPerMillion = inputPrice.String, cachedPrice.String
	value.OutputUSDPerMillion = outputPrice.String
	value.PricingCatalogAsOf, value.PricingModel = nullableString(catalog), nullableString(pricingModel)
	if len(snapshot) > 0 {
		value.PricingSnapshot = append(json.RawMessage(nil), snapshot...)
	}
	value.CacheWriteMode = nullableString(cacheWriteMode)
	value.RequestedServiceTier, value.ActualServiceTier = nullableString(requestedTier), nullableString(actualTier)
	value.PricingServiceTier, value.ActualModel = nullableString(pricingTier), nullableString(actualModel)
	value.ContextClass, value.PricingFallbackReason = nullableString(contextClass), nullableString(fallbackReason)
	value.ActualCacheWriteTokens = nullableInt64(cacheWriteTokens)
	value.AppliedInputUSDPerMillion = nullableString(appliedInput)
	value.AppliedCachedInputUSDPerMillion = nullableString(appliedCached)
	value.AppliedCacheWriteUSDPerMillion = nullableString(appliedCacheWrite)
	value.AppliedOutputUSDPerMillion = nullableString(appliedOutput)
	value.DayPeriodID, value.WeekPeriodID, value.MonthPeriodID = nullableString(day), nullableString(week), nullableString(month)
	value.CashLotCutoff = nullableInt64(cutoff)
	value.ActualInputTokens, value.ActualCachedInputTokens = nullableInt64(inputTokens), nullableInt64(cachedTokens)
	value.ActualOutputTokens = nullableInt64(outputTokens)
	value.ActualCostUSD, value.ChargedUSD, value.UncoveredUSD = nullableString(actualCost), nullableString(charged), nullableString(uncovered)
	return value, err
}

const billingReservationColumns = `request_id, user_id, api_key_id, requested_model,
	input_usd_per_million::text, cached_input_usd_per_million::text,
	output_usd_per_million::text, pricing_rule_version, billing_mode,
	pricing_catalog_as_of::text, pricing_model, pricing_snapshot, cache_write_mode,
	requested_service_tier, actual_service_tier, pricing_service_tier, actual_model,
	context_class, actual_cache_write_tokens, applied_input_usd_per_million::text,
	applied_cached_input_usd_per_million::text, applied_cache_write_usd_per_million::text,
	applied_output_usd_per_million::text, pricing_fallback_reason,
	day_period_id, week_period_id, month_period_id, cash_lot_cutoff, state,
	actual_input_tokens, actual_cached_input_tokens,
	actual_output_tokens, actual_cost_usd::text, charged_usd::text,
	uncovered_usd::text, created_at, settled_at`

func (s *Store) ReserveBilling(ctx context.Context, params BillingReservationParams) (BillingReservation, error) {
	if params.Now.IsZero() {
		params.Now = s.now().UTC()
	}
	var reservation BillingReservation
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		var err error
		reservation, err = reserveBillingTx(ctx, tx, params)
		return err
	})
	var insufficient *InsufficientFundsError
	if errors.As(err, &insufficient) {
		if convergeErr := s.convergeBillingSubscriptions(ctx, params.UserID, params.Now); convergeErr != nil {
			return BillingReservation{}, convergeErr
		}
	}
	return reservation, err
}

// reserveBillingTx must run after the caller has taken the standard quota
// locks. It locks the user's billing account, rolls enabled subscriptions
// forward, then snapshots only the sources that exist at admission time.
func reserveBillingTx(ctx context.Context, tx *sql.Tx, params BillingReservationParams) (BillingReservation, error) {
	params.Model = strings.TrimSpace(params.Model)
	params.PricingModel = strings.TrimSpace(params.PricingModel)
	params.RequestedServiceTier = strings.TrimSpace(params.RequestedServiceTier)
	if params.PricingRuleVersion == 0 {
		params.PricingRuleVersion = config.PricingSchemaV1
	}
	if params.RequestID == "" || params.UserID == "" || params.APIKeyID == "" || params.Model == "" {
		return BillingReservation{}, fmt.Errorf("%w: invalid billing reservation", ErrInvalid)
	}
	switch params.PricingRuleVersion {
	case config.PricingSchemaV1:
		if params.BillingMode == "" {
			params.BillingMode = BillingModeLegacy
		}
		if params.BillingMode != BillingModeLegacy || params.PricingCatalogAsOf != "" ||
			params.PricingModel != "" || len(params.PricingSnapshot) != 0 || params.CacheWriteMode != "" {
			return BillingReservation{}, fmt.Errorf("%w: invalid v1 pricing snapshot", ErrInvalid)
		}
		prices := []*string{&params.InputUSDPerMillion, &params.CachedInputUSDPerMillion, &params.OutputUSDPerMillion}
		for _, price := range prices {
			value, err := decimal.ParsePrice(*price)
			if err != nil {
				return BillingReservation{}, fmt.Errorf("%w: invalid price snapshot", ErrInvalid)
			}
			*price = value
		}
	case config.PricingSchemaV2:
		if params.BillingMode != BillingModeOpenAIAPIEquivalent && params.BillingMode != BillingModeInternalZero {
			return BillingReservation{}, fmt.Errorf("%w: invalid v2 billing mode", ErrInvalid)
		}
		if params.InputUSDPerMillion != "" || params.CachedInputUSDPerMillion != "" || params.OutputUSDPerMillion != "" ||
			params.PricingCatalogAsOf == "" || params.PricingModel == "" || len(params.PricingSnapshot) == 0 {
			return BillingReservation{}, fmt.Errorf("%w: invalid v2 pricing snapshot", ErrInvalid)
		}
		if _, err := time.Parse("2006-01-02", params.PricingCatalogAsOf); err != nil {
			return BillingReservation{}, fmt.Errorf("%w: invalid pricing catalog date", ErrInvalid)
		}
		snapshot, err := config.ParsePricingSnapshot(params.PricingSnapshot)
		if err != nil || snapshot.Model != params.PricingModel || params.PricingModel != params.Model ||
			snapshot.Rule.CacheWriteMode != params.CacheWriteMode {
			return BillingReservation{}, fmt.Errorf("%w: inconsistent v2 pricing snapshot", ErrInvalid)
		}
		if params.BillingMode == BillingModeInternalZero && !pricingRuleIsZero(snapshot.Rule) {
			return BillingReservation{}, fmt.Errorf("%w: internal zero pricing must contain only zero prices", ErrInvalid)
		}
	default:
		return BillingReservation{}, fmt.Errorf("%w: unsupported pricing rule version", ErrInvalid)
	}
	if len(params.RequestedServiceTier) > 32 {
		return BillingReservation{}, fmt.Errorf("%w: invalid requested service tier", ErrInvalid)
	}
	if params.Now.IsZero() {
		params.Now = time.Now().UTC()
	} else {
		params.Now = params.Now.UTC()
	}
	var balance string
	var nextSequence int64
	if err := tx.QueryRowContext(ctx, `
		SELECT balance_usd::text, next_cash_lot_sequence
		FROM billing_accounts WHERE user_id = $1 FOR UPDATE`, params.UserID,
	).Scan(&balance, &nextSequence); err != nil {
		return BillingReservation{}, mapDBError("lock billing account", err)
	}
	if _, err := rollBillingSubscriptionsTx(ctx, tx, params.UserID, params.Now); err != nil {
		return BillingReservation{}, err
	}
	periods := map[string]*string{BillingTierDay: nil, BillingTierWeek: nil, BillingTierMonth: nil}
	var earliestEnd *time.Time
	rows, err := tx.QueryContext(ctx, `
		SELECT s.tier, p.id, p.ends_at
		FROM billing_subscriptions s
		JOIN billing_subscription_periods p ON p.id = s.current_period_id
		WHERE s.user_id = $1 AND s.enabled
		  AND p.closed_at IS NULL AND p.starts_at <= $2 AND p.ends_at > $2
		  AND p.remaining_usd > 0
		ORDER BY CASE s.tier WHEN 'day' THEN 1 WHEN 'week' THEN 2 ELSE 3 END
		FOR UPDATE OF s, p`, params.UserID, params.Now)
	if err != nil {
		return BillingReservation{}, mapDBError("read billing sources", err)
	}
	for rows.Next() {
		var tier, id string
		var ends time.Time
		if err := rows.Scan(&tier, &id, &ends); err != nil {
			_ = rows.Close()
			return BillingReservation{}, fmt.Errorf("scan billing source: %w", err)
		}
		periodID := id
		periods[tier] = &periodID
		if earliestEnd == nil || ends.Before(*earliestEnd) {
			copy := ends
			earliestEnd = &copy
		}
	}
	if err := rows.Close(); err != nil {
		return BillingReservation{}, fmt.Errorf("close billing sources: %w", err)
	}
	if err := rows.Err(); err != nil {
		return BillingReservation{}, fmt.Errorf("iterate billing sources: %w", err)
	}
	var cashCutoff *int64
	if billingPositive(balance) {
		cutoff := nextSequence - 1
		if cutoff > 0 {
			cashCutoff = &cutoff
		}
	}
	if params.BillingMode != BillingModeInternalZero &&
		periods[BillingTierDay] == nil && periods[BillingTierWeek] == nil &&
		periods[BillingTierMonth] == nil && cashCutoff == nil {
		retry := time.Duration(0)
		var renewal sql.NullTime
		err := tx.QueryRowContext(ctx, `
			SELECT min(p.ends_at)
			FROM billing_subscriptions s
			JOIN billing_subscription_periods p ON p.id = s.current_period_id
			WHERE s.user_id = $1 AND s.enabled AND p.ends_at > $2
			  AND (s.period_count = 0 OR s.current_period_number < s.period_count)`, params.UserID, params.Now,
		).Scan(&renewal)
		if err != nil {
			return BillingReservation{}, mapDBError("read billing retry time", err)
		}
		if renewal.Valid && renewal.Time.After(params.Now) {
			retry = renewal.Time.Sub(params.Now)
		} else if earliestEnd != nil && earliestEnd.After(params.Now) {
			retry = earliestEnd.Sub(params.Now)
		}
		return BillingReservation{}, &InsufficientFundsError{RetryAfter: retry}
	}
	reservation, err := scanBillingReservation(tx.QueryRowContext(ctx, `
		INSERT INTO billing_reservations
			(request_id, user_id, api_key_id, requested_model,
			 input_usd_per_million, cached_input_usd_per_million, output_usd_per_million,
			 pricing_rule_version, billing_mode, pricing_catalog_as_of, pricing_model,
			 pricing_snapshot, cache_write_mode, requested_service_tier,
			 day_period_id, week_period_id, month_period_id, cash_lot_cutoff, created_at)
		VALUES ($1,$2,$3,$4,$5::numeric,$6::numeric,$7::numeric,$8,$9,$10::date,$11,
			$12::jsonb,$13,$14,$15,$16,$17,$18,$19)
		RETURNING `+billingReservationColumns,
		params.RequestID, params.UserID, params.APIKeyID, params.Model,
		valueOrNil(params.InputUSDPerMillion), valueOrNil(params.CachedInputUSDPerMillion),
		valueOrNil(params.OutputUSDPerMillion), params.PricingRuleVersion, params.BillingMode,
		valueOrNil(params.PricingCatalogAsOf), valueOrNil(params.PricingModel),
		valueOrNil(string(params.PricingSnapshot)), valueOrNil(params.CacheWriteMode),
		valueOrNil(params.RequestedServiceTier), periods[BillingTierDay], periods[BillingTierWeek],
		periods[BillingTierMonth], cashCutoff, params.Now))
	return reservation, mapDBError("insert billing reservation", err)
}

func pricingRuleIsZero(rule config.ModelPricing) bool {
	for _, tier := range rule.ServiceTiers {
		for _, price := range []*config.TokenPricing{tier.Short, tier.Long} {
			if price == nil {
				continue
			}
			values := []string{price.InputUSDPerMillion, price.CachedInputUSDPerMillion, price.OutputUSDPerMillion}
			if price.CacheWriteUSDPerMillion != nil {
				values = append(values, *price.CacheWriteUSDPerMillion)
			}
			for _, value := range values {
				canonical, err := decimal.ParsePrice(value)
				if err != nil || canonical != "0" {
					return false
				}
			}
		}
	}
	return true
}

func rollBillingSubscriptionsTx(ctx context.Context, tx *sql.Tx, userID string, at time.Time) (int, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT s.id, s.tier, s.allowance_usd::text, s.period_count,
			s.current_period_number, s.expires_at, p.id, p.ends_at
		FROM billing_subscriptions s
		JOIN billing_subscription_periods p ON p.id = s.current_period_id
		WHERE s.user_id = $1 AND s.enabled
		ORDER BY CASE s.tier WHEN 'day' THEN 1 WHEN 'week' THEN 2 ELSE 3 END
		FOR UPDATE OF s, p`, userID)
	if err != nil {
		return 0, mapDBError("lock billing subscriptions", err)
	}
	type expired struct {
		subscriptionID, tier, allowance, periodID string
		periodCount, periodNumber                 int
		expiresAt                                 sql.NullTime
		end                                       time.Time
	}
	var values []expired
	for rows.Next() {
		var value expired
		var periodID sql.NullString
		var end sql.NullTime
		if err := rows.Scan(&value.subscriptionID, &value.tier, &value.allowance,
			&value.periodCount, &value.periodNumber, &value.expiresAt, &periodID, &end); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan billing subscription: %w", err)
		}
		if !periodID.Valid || !end.Valid || !end.Time.After(at) {
			value.periodID = periodID.String
			value.end = end.Time
			values = append(values, value)
		}
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close billing subscriptions: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate billing subscriptions: %w", err)
	}
	disabled := 0
	for _, value := range values {
		if value.periodCount > 0 && value.expiresAt.Valid && !value.expiresAt.Time.After(at) {
			if value.periodID != "" {
				closedAt := value.end
				if closedAt.IsZero() || closedAt.After(value.expiresAt.Time) {
					closedAt = value.expiresAt.Time
				}
				if _, err := tx.ExecContext(ctx, `
					UPDATE billing_subscription_periods
					SET closed_at = $2, close_reason = 'expired'
					WHERE id = $1 AND closed_at IS NULL`, value.periodID, closedAt); err != nil {
					return disabled, mapDBError("close expired finite billing period", err)
				}
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE billing_subscriptions
				SET enabled = false, disabled_at = expires_at, updated_at = $2
				WHERE id = $1 AND enabled`, value.subscriptionID, at); err != nil {
				return disabled, mapDBError("expire finite billing subscription", err)
			}
			disabled++
			continue
		}
		if value.periodID != "" {
			if _, err := tx.ExecContext(ctx, `
				UPDATE billing_subscription_periods
				SET closed_at = $2, close_reason = 'expired'
				WHERE id = $1 AND closed_at IS NULL`, value.periodID, value.end); err != nil {
				return disabled, mapDBError("close expired billing period", err)
			}
		}
		duration, _ := billingPeriodDuration(value.tier)
		advance := 1
		periodStart := value.end
		if periodStart.IsZero() {
			periodStart = at
		} else {
			advance += int(at.Sub(periodStart) / duration)
			periodStart = periodStart.Add(time.Duration(advance-1) * duration)
		}
		periodNumber := value.periodNumber + advance
		if value.periodCount > 0 && periodNumber > value.periodCount {
			return disabled, fmt.Errorf("subscription advanced past configured period count: %w", ErrConflict)
		}
		periodID, err := newUUID()
		if err != nil {
			return disabled, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO billing_subscription_periods
				(id, subscription_id, user_id, tier, starts_at, ends_at,
				 allowance_usd, remaining_usd, period_number, period_count, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7::numeric,$7::numeric,$8,$9,$10)`,
			periodID, value.subscriptionID, userID, value.tier,
			periodStart, periodStart.Add(duration), value.allowance,
			periodNumber, value.periodCount, at); err != nil {
			return disabled, mapDBError("create renewed billing period", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE billing_subscriptions
			SET current_period_id = $2, current_period_number = $3, updated_at = $4
			WHERE id = $1`, value.subscriptionID, periodID, periodNumber, at); err != nil {
			return disabled, mapDBError("activate renewed billing period", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO billing_ledger_entries
				(user_id, entry_type, amount_usd, subscription_tier,
				 subscription_period_id, reason, created_at)
			VALUES ($1,'subscription_renewal',$2::numeric,$3,$4,'automatic renewal',$5)`,
			userID, value.allowance, value.tier, periodID, at); err != nil {
			return disabled, mapDBError("record subscription renewal", err)
		}
	}
	return disabled, nil
}

func expireBillingSubscriptionsTx(ctx context.Context, tx *sql.Tx, userID string, at time.Time) (int, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT s.id, s.expires_at, p.id, p.ends_at
		FROM billing_subscriptions s
		JOIN billing_subscription_periods p ON p.id = s.current_period_id
		WHERE s.user_id = $1 AND s.enabled AND s.period_count > 0
		  AND s.expires_at <= $2
		ORDER BY CASE s.tier WHEN 'day' THEN 1 WHEN 'week' THEN 2 ELSE 3 END
		FOR UPDATE OF s, p`, userID, at)
	if err != nil {
		return 0, mapDBError("lock expired finite billing subscriptions", err)
	}
	type expiredSubscription struct {
		id, periodID            string
		expiresAt, periodEndsAt time.Time
	}
	var values []expiredSubscription
	for rows.Next() {
		var value expiredSubscription
		if err := rows.Scan(&value.id, &value.expiresAt,
			&value.periodID, &value.periodEndsAt); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan expired finite billing subscription: %w", err)
		}
		values = append(values, value)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close expired finite billing subscriptions: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate expired finite billing subscriptions: %w", err)
	}
	for _, value := range values {
		closedAt := value.periodEndsAt
		if closedAt.After(value.expiresAt) {
			closedAt = value.expiresAt
		}
		if _, err := tx.ExecContext(ctx, `UPDATE billing_subscription_periods
			SET closed_at = $2, close_reason = 'expired'
			WHERE id = $1 AND closed_at IS NULL`, value.periodID, closedAt); err != nil {
			return 0, mapDBError("close converged finite billing period", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE billing_subscriptions
			SET enabled = false, disabled_at = expires_at, updated_at = $2
			WHERE id = $1 AND enabled`, value.id, at); err != nil {
			return 0, mapDBError("converge finite billing subscription expiry", err)
		}
	}
	return len(values), nil
}

// ExpireBillingSubscriptions converges finite subscriptions that reached their
// scheduled final boundary without a concurrent admission or billing read.
// Each user is processed under the standard billing-account lock so this job
// serializes with subscription writes, admissions and settlements.
func (s *Store) ExpireBillingSubscriptions(ctx context.Context, at time.Time, limit int) (int, error) {
	if limit <= 0 || limit > 10_000 {
		limit = 1000
	}
	at = normalizedBillingTime(at, s.now)
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT user_id
		FROM billing_subscriptions
		WHERE enabled AND period_count > 0 AND expires_at <= $1
		ORDER BY user_id
		LIMIT $2`, at, limit)
	if err != nil {
		return 0, mapDBError("list expired billing subscriptions", err)
	}
	var userIDs []string
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan expired billing subscription user: %w", err)
		}
		userIDs = append(userIDs, userID)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close expired billing subscription users: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate expired billing subscription users: %w", err)
	}

	disabled := 0
	for _, userID := range userIDs {
		count, err := s.convergeBillingSubscriptionsCount(ctx, userID, at)
		if err != nil {
			return disabled, err
		}
		disabled += count
	}
	return disabled, nil
}

func (s *Store) convergeBillingSubscriptions(ctx context.Context, userID string, at time.Time) error {
	_, err := s.convergeBillingSubscriptionsCount(ctx, userID, at)
	return err
}

func (s *Store) convergeBillingSubscriptionsCount(ctx context.Context, userID string, at time.Time) (int, error) {
	var count int
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if err := lockBillingAccountTx(ctx, tx, userID); err != nil {
			return err
		}
		var err error
		count, err = expireBillingSubscriptionsTx(ctx, tx, userID, at)
		return err
	})
	return count, err
}

const billingLedgerColumns = `id, user_id, operation_id, entry_type,
	amount_usd::text, cash_delta_usd::text, balance_after_usd::text,
	cny_amount::text, usd_per_cny_snapshot::text, subscription_tier,
	subscription_period_id, request_id, model, input_tokens,
	cached_input_tokens, output_tokens, actual_cost_usd::text,
	charged_usd::text, uncovered_usd::text, reason, actor_user_id, created_at,
	usage_requested_at, actual_model, cache_write_tokens, cache_write_mode,
	requested_service_tier, actual_service_tier, pricing_service_tier,
	context_class, pricing_rule_version, pricing_catalog_as_of::text,
	applied_input_usd_per_million::text, applied_cached_input_usd_per_million::text,
	applied_cache_write_usd_per_million::text, applied_output_usd_per_million::text,
	pricing_fallback_reason`

func scanBillingLedgerEntry(row rowScanner) (BillingLedgerEntry, error) {
	var value BillingLedgerEntry
	var userID, operationID, balance, cny, rate, tier, periodID sql.NullString
	var requestID, model, cost, charged, uncovered, actorID sql.NullString
	var usageRequestedAt sql.NullTime
	var actualModel, cacheWriteMode, requestedTier, actualTier, pricingTier sql.NullString
	var contextClass, pricingCatalog, appliedInput, appliedCached sql.NullString
	var appliedCacheWrite, appliedOutput, fallbackReason sql.NullString
	var input, cached, cacheWrite, output sql.NullInt64
	err := row.Scan(&value.ID, &userID, &operationID, &value.EntryType,
		&value.AmountUSD, &value.CashDeltaUSD, &balance, &cny, &rate, &tier,
		&periodID, &requestID, &model, &input, &cached, &output, &cost,
		&charged, &uncovered, &value.Reason, &actorID, &value.CreatedAt,
		&usageRequestedAt, &actualModel, &cacheWrite, &cacheWriteMode,
		&requestedTier, &actualTier, &pricingTier, &contextClass,
		&value.PricingRuleVersion, &pricingCatalog, &appliedInput, &appliedCached,
		&appliedCacheWrite, &appliedOutput, &fallbackReason)
	value.UserID, value.OperationID = nullableString(userID), nullableString(operationID)
	value.BalanceAfterUSD, value.CNYAmount = nullableString(balance), nullableString(cny)
	value.USDPerCNYSnapshot, value.SubscriptionTier = nullableString(rate), nullableString(tier)
	value.SubscriptionPeriodID, value.RequestID = nullableString(periodID), nullableString(requestID)
	value.Model, value.ActorUserID = nullableString(model), nullableString(actorID)
	value.InputTokens, value.CachedInputTokens, value.OutputTokens = nullableInt64(input), nullableInt64(cached), nullableInt64(output)
	value.CacheWriteTokens = nullableInt64(cacheWrite)
	if usageRequestedAt.Valid {
		value.UsageRequestedAt = &usageRequestedAt.Time
	}
	value.ActualModel, value.CacheWriteMode = nullableString(actualModel), nullableString(cacheWriteMode)
	value.RequestedServiceTier, value.ActualServiceTier = nullableString(requestedTier), nullableString(actualTier)
	value.PricingServiceTier, value.ContextClass = nullableString(pricingTier), nullableString(contextClass)
	value.PricingCatalogAsOf = nullableString(pricingCatalog)
	value.AppliedInputUSDPerMillion, value.AppliedCachedInputUSDPerMillion = nullableString(appliedInput), nullableString(appliedCached)
	value.AppliedCacheWriteUSDPerMillion, value.AppliedOutputUSDPerMillion = nullableString(appliedCacheWrite), nullableString(appliedOutput)
	value.PricingFallbackReason = nullableString(fallbackReason)
	value.ActualCostUSD, value.ChargedUSD, value.UncoveredUSD = nullableString(cost), nullableString(charged), nullableString(uncovered)
	return value, err
}

func billingOperationFingerprint(values ...string) []byte {
	body, _ := json.Marshal(values)
	digest := sha256.Sum256(body)
	return digest[:]
}

func claimBillingOperationTx(ctx context.Context, tx *sql.Tx, params BillingWriteParams,
	operationType, targetUserID string, fingerprint []byte, compatibleFingerprints ...[]byte) (bool, *int64, error) {
	result, err := tx.ExecContext(ctx, `
		INSERT INTO billing_operations
			(operation_id, operation_type, actor_user_id, target_user_id, reason,
			 request_fingerprint, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`,
		params.OperationID, operationType, params.ActorUserID, valueOrNil(targetUserID),
		strings.TrimSpace(params.Reason), fingerprint, params.At)
	if err != nil {
		return false, nil, mapDBError("claim billing operation", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, nil, fmt.Errorf("claim billing operation rows affected: %w", err)
	}
	if n == 1 {
		return true, nil, nil
	}
	var storedType, actor string
	var storedTarget sql.NullString
	var storedFingerprint []byte
	var ledgerID sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
		SELECT operation_type, actor_user_id, target_user_id, request_fingerprint,
			result_ledger_entry_id
		FROM billing_operations WHERE operation_id = $1 FOR UPDATE`, params.OperationID,
	).Scan(&storedType, &actor, &storedTarget, &storedFingerprint, &ledgerID); err != nil {
		return false, nil, mapDBError("read billing operation replay", err)
	}
	fingerprintMatches := bytes.Equal(storedFingerprint, fingerprint)
	for _, compatible := range compatibleFingerprints {
		fingerprintMatches = fingerprintMatches || bytes.Equal(storedFingerprint, compatible)
	}
	if storedType != operationType || actor != params.ActorUserID ||
		storedTarget.String != targetUserID || !fingerprintMatches || !ledgerID.Valid {
		return false, nil, fmt.Errorf("billing operation replay mismatch: %w", ErrConflict)
	}
	return false, &ledgerID.Int64, nil
}

func finishBillingOperationTx(ctx context.Context, tx *sql.Tx, operationID string, ledgerID int64) error {
	result, err := tx.ExecContext(ctx, `UPDATE billing_operations
		SET result_ledger_entry_id = $2 WHERE operation_id = $1
		AND result_ledger_entry_id IS NULL`, operationID, ledgerID)
	if err != nil {
		return mapDBError("finish billing operation", err)
	}
	return requireAffected("finish billing operation", result)
}

func replayBillingLedgerTx(ctx context.Context, tx *sql.Tx, id int64) (BillingLedgerEntry, error) {
	entry, err := scanBillingLedgerEntry(tx.QueryRowContext(ctx,
		`SELECT `+billingLedgerColumns+` FROM billing_ledger_entries WHERE id = $1`, id))
	return entry, mapDBError("read billing operation result", err)
}

func lockBillingAccountTx(ctx context.Context, tx *sql.Tx, userID string) error {
	var locked int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM billing_accounts
		WHERE user_id = $1 FOR UPDATE`, userID).Scan(&locked); err != nil {
		return mapDBError("lock billing account", err)
	}
	return nil
}

func appendBillingAuditTx(ctx context.Context, tx *sql.Tx, params BillingWriteParams,
	eventType, subjectType, subjectID string, metadata map[string]any) error {
	encoded, err := marshalSafeMetadata(metadata)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO audit_events
			(occurred_at, actor_user_id, actor_session_id, event_type, severity,
			 success, source_ip, subject_type, subject_id, request_id, metadata)
		VALUES ($1,$2,$3,$4,'info',true,$5::inet,$6,$7,$8,$9::jsonb)`,
		params.At, params.ActorUserID, valueOrNil(params.ActorSessionID), eventType,
		valueOrNil(params.SourceIP), subjectType, subjectID, valueOrNil(params.RequestID), encoded)
	return mapDBError("append billing audit event", err)
}

func (s *Store) GetBillingSettings(ctx context.Context) (BillingSettings, error) {
	var value BillingSettings
	var actor sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT usd_per_cny::text, updated_at,
		updated_by_user_id FROM billing_settings WHERE singleton`).Scan(
		&value.USDPerCNY, &value.UpdatedAt, &actor)
	value.UpdatedByUserID = nullableString(actor)
	return value, mapDBError("get billing settings", err)
}

func (s *Store) SetRechargeRate(ctx context.Context, params SetRechargeRateParams) (BillingSettings, error) {
	var setting BillingSettings
	if err := validateBillingWrite(params.BillingWriteParams); err != nil {
		return setting, err
	}
	rate, err := decimal.ParseRate(params.USDPerCNY)
	if err != nil {
		return setting, fmt.Errorf("%w: invalid recharge rate", ErrInvalid)
	}
	params.At = normalizedBillingTime(params.At, s.now)
	params.Reason = strings.TrimSpace(params.Reason)
	fingerprint := billingOperationFingerprint("recharge_rate", params.ActorUserID, params.Reason, rate)
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		created, replayID, err := claimBillingOperationTx(ctx, tx, params.BillingWriteParams,
			"recharge_rate", "", fingerprint)
		if err != nil {
			return err
		}
		if !created {
			entry, err := replayBillingLedgerTx(ctx, tx, *replayID)
			if err != nil {
				return err
			}
			if entry.USDPerCNYSnapshot == nil {
				return fmt.Errorf("replayed rate operation has no snapshot: %w", ErrConflict)
			}
			setting = BillingSettings{USDPerCNY: *entry.USDPerCNYSnapshot,
				UpdatedAt: entry.CreatedAt, UpdatedByUserID: entry.ActorUserID}
			return nil
		}
		var updatedBy sql.NullString
		if err := tx.QueryRowContext(ctx, `
			UPDATE billing_settings SET usd_per_cny = $1::numeric,
				updated_at = $2, updated_by_user_id = $3 WHERE singleton
			RETURNING usd_per_cny::text, updated_at, updated_by_user_id`,
			rate, params.At, params.ActorUserID).Scan(
			&setting.USDPerCNY, &setting.UpdatedAt, &updatedBy); err != nil {
			return mapDBError("set recharge rate", err)
		}
		setting.UpdatedByUserID = nullableString(updatedBy)
		entry, err := scanBillingLedgerEntry(tx.QueryRowContext(ctx, `
			INSERT INTO billing_ledger_entries
				(user_id, operation_id, entry_type, usd_per_cny_snapshot, reason,
				 actor_user_id, created_at)
			VALUES ($1,$2,'recharge_rate',$3::numeric,$4,$1,$5)
			RETURNING `+billingLedgerColumns,
			params.ActorUserID, params.OperationID, rate, params.Reason, params.At))
		if err != nil {
			return mapDBError("record recharge rate ledger", err)
		}
		if err := finishBillingOperationTx(ctx, tx, params.OperationID, entry.ID); err != nil {
			return err
		}
		return appendBillingAuditTx(ctx, tx, params.BillingWriteParams,
			"billing.rate_updated", "billing_settings", "global", map[string]any{
				"operation_id": params.OperationID, "usd_per_cny": rate,
			})
	})
	return setting, err
}

func (s *Store) RechargeUser(ctx context.Context, params RechargeUserParams) (BillingLedgerEntry, error) {
	var entry BillingLedgerEntry
	if err := validateBillingWrite(params.BillingWriteParams); err != nil || params.UserID == "" {
		if err != nil {
			return entry, err
		}
		return entry, fmt.Errorf("%w: recharge user is required", ErrInvalid)
	}
	cny, err := decimal.ParseInput(params.CNYAmount, false, false)
	if err != nil {
		return entry, fmt.Errorf("%w: invalid recharge amount", ErrInvalid)
	}
	params.At = normalizedBillingTime(params.At, s.now)
	params.Reason = strings.TrimSpace(params.Reason)
	fingerprint := billingOperationFingerprint("recharge", params.ActorUserID,
		params.UserID, params.Reason, cny)
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if err := lockBillingAccountTx(ctx, tx, params.UserID); err != nil {
			return err
		}
		created, replayID, err := claimBillingOperationTx(ctx, tx, params.BillingWriteParams,
			"recharge", params.UserID, fingerprint)
		if err != nil {
			return err
		}
		if !created {
			entry, err = replayBillingLedgerTx(ctx, tx, *replayID)
			return err
		}
		var rate string
		if err := tx.QueryRowContext(ctx, `SELECT usd_per_cny::text FROM billing_settings
			WHERE singleton FOR SHARE`).Scan(&rate); err != nil {
			return mapDBError("snapshot recharge rate", err)
		}
		usd, err := decimal.MultiplyToAmount(cny, rate)
		if err != nil {
			return fmt.Errorf("convert recharge amount: %w", err)
		}
		if !billingPositive(usd) {
			return fmt.Errorf("%w: recharge rounds to zero USD", ErrInvalid)
		}
		var balance string
		var sequence int64
		if err := tx.QueryRowContext(ctx, `
			UPDATE billing_accounts SET balance_usd = balance_usd + $2::numeric,
				next_cash_lot_sequence = next_cash_lot_sequence + 1, updated_at = $3
			WHERE user_id = $1
			RETURNING balance_usd::text, next_cash_lot_sequence - 1`,
			params.UserID, usd, params.At).Scan(&balance, &sequence); err != nil {
			return mapDBError("credit recharge balance", err)
		}
		entry, err = scanBillingLedgerEntry(tx.QueryRowContext(ctx, `
			INSERT INTO billing_ledger_entries
				(user_id, operation_id, entry_type, amount_usd, cash_delta_usd,
				 balance_after_usd, cny_amount, usd_per_cny_snapshot, reason,
				 actor_user_id, created_at)
			VALUES ($1,$2,'recharge',$3::numeric,$3::numeric,$4::numeric,
				$5::numeric,$6::numeric,$7,$8,$9)
			RETURNING `+billingLedgerColumns,
			params.UserID, params.OperationID, usd, balance, cny, rate,
			params.Reason, params.ActorUserID, params.At))
		if err != nil {
			return mapDBError("record recharge ledger", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO billing_cash_credit_lots
				(user_id, lot_sequence, source_ledger_entry_id, original_usd,
				 remaining_usd, created_at)
			VALUES ($1,$2,$3,$4::numeric,$4::numeric,$5)`,
			params.UserID, sequence, entry.ID, usd, params.At); err != nil {
			return mapDBError("create recharge credit lot", err)
		}
		if err := finishBillingOperationTx(ctx, tx, params.OperationID, entry.ID); err != nil {
			return err
		}
		return appendBillingAuditTx(ctx, tx, params.BillingWriteParams,
			"billing.recharged", "user", params.UserID, map[string]any{
				"operation_id": params.OperationID, "cny_amount": cny,
				"usd_amount": usd, "usd_per_cny": rate,
			})
	})
	return entry, err
}

func (s *Store) AdjustUserBalance(ctx context.Context, params AdjustUserBalanceParams) (BillingLedgerEntry, error) {
	var entry BillingLedgerEntry
	if err := validateBillingWrite(params.BillingWriteParams); err != nil || params.UserID == "" {
		if err != nil {
			return entry, err
		}
		return entry, fmt.Errorf("%w: adjustment user is required", ErrInvalid)
	}
	amount, err := decimal.ParseInput(params.USDAmount, true, false)
	if err != nil {
		return entry, fmt.Errorf("%w: invalid adjustment amount", ErrInvalid)
	}
	params.At = normalizedBillingTime(params.At, s.now)
	params.Reason = strings.TrimSpace(params.Reason)
	fingerprint := billingOperationFingerprint("adjustment", params.ActorUserID,
		params.UserID, params.Reason, amount)
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if err := lockBillingAccountTx(ctx, tx, params.UserID); err != nil {
			return err
		}
		created, replayID, err := claimBillingOperationTx(ctx, tx, params.BillingWriteParams,
			"adjustment", params.UserID, fingerprint)
		if err != nil {
			return err
		}
		if !created {
			entry, err = replayBillingLedgerTx(ctx, tx, *replayID)
			return err
		}
		var oldBalance string
		var nextSequence int64
		if err := tx.QueryRowContext(ctx, `SELECT balance_usd::text,
			next_cash_lot_sequence FROM billing_accounts WHERE user_id = $1`,
			params.UserID).Scan(&oldBalance, &nextSequence); err != nil {
			return mapDBError("lock adjustment account", err)
		}
		newBalance, err := billingAdd(oldBalance, amount)
		if err != nil {
			return err
		}
		newValue, err := billingRat(newBalance)
		if err != nil {
			return err
		}
		if newValue.Sign() < 0 {
			return &InsufficientFundsError{}
		}
		positive := billingPositive(amount)
		if !positive {
			needed, err := billingSubtract("0", amount)
			if err != nil {
				return err
			}
			rows, err := tx.QueryContext(ctx, `SELECT id, remaining_usd::text
				FROM billing_cash_credit_lots WHERE user_id = $1 AND remaining_usd > 0
				ORDER BY lot_sequence FOR UPDATE`, params.UserID)
			if err != nil {
				return mapDBError("lock adjustment credit lots", err)
			}
			type adjustmentLot struct {
				id        int64
				remaining string
			}
			var lots []adjustmentLot
			for rows.Next() {
				var lot adjustmentLot
				if err := rows.Scan(&lot.id, &lot.remaining); err != nil {
					_ = rows.Close()
					return fmt.Errorf("scan adjustment credit lot: %w", err)
				}
				lots = append(lots, lot)
			}
			if err := rows.Close(); err != nil {
				return fmt.Errorf("close adjustment credit lots: %w", err)
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf("iterate adjustment credit lots: %w", err)
			}
			for _, lot := range lots {
				if !billingPositive(needed) {
					break
				}
				deduction, err := billingMin(lot.remaining, needed)
				if err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE billing_cash_credit_lots
					SET remaining_usd = remaining_usd - $2::numeric WHERE id = $1`,
					lot.id, deduction); err != nil {
					return mapDBError("debit adjustment credit lot", err)
				}
				needed, err = billingSubtract(needed, deduction)
				if err != nil {
					return err
				}
			}
			if billingPositive(needed) {
				return fmt.Errorf("cash lots do not reconcile with account: %w", ErrConflict)
			}
		}
		sequence := nextSequence
		if positive {
			nextSequence++
		}
		if _, err := tx.ExecContext(ctx, `UPDATE billing_accounts SET
			balance_usd = $2::numeric, next_cash_lot_sequence = $3, updated_at = $4
			WHERE user_id = $1`, params.UserID, newBalance, nextSequence, params.At); err != nil {
			return mapDBError("update adjusted balance", err)
		}
		entry, err = scanBillingLedgerEntry(tx.QueryRowContext(ctx, `
			INSERT INTO billing_ledger_entries
				(user_id, operation_id, entry_type, amount_usd, cash_delta_usd,
				 balance_after_usd, reason, actor_user_id, created_at)
			VALUES ($1,$2,'adjustment',$3::numeric,$3::numeric,$4::numeric,$5,$6,$7)
			RETURNING `+billingLedgerColumns,
			params.UserID, params.OperationID, amount, newBalance, params.Reason,
			params.ActorUserID, params.At))
		if err != nil {
			return mapDBError("record adjustment ledger", err)
		}
		if positive {
			if _, err := tx.ExecContext(ctx, `INSERT INTO billing_cash_credit_lots
				(user_id, lot_sequence, source_ledger_entry_id, original_usd,
				 remaining_usd, created_at) VALUES ($1,$2,$3,$4::numeric,$4::numeric,$5)`,
				params.UserID, sequence, entry.ID, amount, params.At); err != nil {
				return mapDBError("create adjustment credit lot", err)
			}
		}
		if err := finishBillingOperationTx(ctx, tx, params.OperationID, entry.ID); err != nil {
			return err
		}
		return appendBillingAuditTx(ctx, tx, params.BillingWriteParams,
			"billing.adjusted", "user", params.UserID, map[string]any{
				"operation_id": params.OperationID, "usd_amount": amount,
			})
	})
	return entry, err
}

func subscriptionStateFromPeriodRow(row rowScanner) (BillingSubscriptionState, error) {
	var value BillingSubscriptionState
	var periodID sql.NullString
	var startsAt, endsAt, expiresAt sql.NullTime
	var remaining sql.NullString
	var updatedAt time.Time
	err := row.Scan(&value.ID, &value.Tier, &value.Enabled, &value.AllowanceUSD,
		&value.PeriodCount, &value.CurrentPeriodNumber, &expiresAt,
		&periodID, &startsAt, &endsAt, &remaining, &updatedAt)
	if expiresAt.Valid {
		value.ExpiresAt = &expiresAt.Time
	}
	value.PeriodID = nullableString(periodID)
	if startsAt.Valid {
		value.PeriodStartsAt = &startsAt.Time
	}
	if endsAt.Valid {
		value.PeriodEndsAt = &endsAt.Time
	}
	if remaining.Valid && value.Enabled {
		value.RemainingUSD = remaining.String
	} else {
		value.RemainingUSD = "0.000000000000"
	}
	value.UpdatedAt = &updatedAt
	return value, err
}

// subscriptionOperationStateTx reconstructs the immutable response of a
// subscription write. A replay must not expose a later remaining balance: the
// period may have been charged by an admission-bound request after the first
// response was committed. Enabled operations always opened a fresh period at
// the full allowance; disabled operations expose zero usable quota even though
// the closed period remains available to requests bound before the disable.
func subscriptionOperationStateTx(ctx context.Context, tx *sql.Tx, entry BillingLedgerEntry, enabled bool) (BillingSubscriptionState, error) {
	if entry.UserID == nil || entry.SubscriptionTier == nil {
		return BillingSubscriptionState{}, fmt.Errorf("subscription operation ledger is incomplete: %w", ErrConflict)
	}
	var subscriptionID string
	var startsAt, endsAt, expiresAt *time.Time
	var periodNumber, periodCount int
	if entry.SubscriptionPeriodID != nil {
		var startValue, endValue time.Time
		if err := tx.QueryRowContext(ctx, `
			SELECT subscription_id, starts_at, ends_at, period_number, period_count
			FROM billing_subscription_periods
			WHERE id = $1 AND user_id = $2 AND tier = $3`,
			*entry.SubscriptionPeriodID, *entry.UserID, *entry.SubscriptionTier,
		).Scan(&subscriptionID, &startValue, &endValue, &periodNumber, &periodCount); err != nil {
			return BillingSubscriptionState{}, mapDBError("read subscription operation period", err)
		}
		startsAt, endsAt = &startValue, &endValue
		duration, err := billingPeriodDuration(*entry.SubscriptionTier)
		if err != nil {
			return BillingSubscriptionState{}, err
		}
		expiresAt = billingSubscriptionExpiry(startValue, duration, periodNumber, periodCount)
	} else {
		var expiry sql.NullTime
		err := tx.QueryRowContext(ctx, `SELECT subscription_id, period_count,
			current_period_number, expires_at
			FROM billing_subscription_operation_snapshots
			WHERE ledger_entry_id = $1`, entry.ID,
		).Scan(&subscriptionID, &periodCount, &periodNumber, &expiry)
		if errors.Is(err, sql.ErrNoRows) {
			return BillingSubscriptionState{}, fmt.Errorf("subscription operation snapshot is incomplete: %w", ErrConflict)
		}
		if err != nil {
			return BillingSubscriptionState{}, mapDBError("read subscription operation snapshot", err)
		}
		if expiry.Valid {
			expiresAt = &expiry.Time
		}
	}
	remaining := "0.000000000000"
	if enabled {
		remaining = entry.AmountUSD
	}
	updatedAt := entry.CreatedAt
	return BillingSubscriptionState{
		ID:                  subscriptionID,
		Tier:                *entry.SubscriptionTier,
		Enabled:             enabled,
		AllowanceUSD:        entry.AmountUSD,
		RemainingUSD:        remaining,
		PeriodCount:         periodCount,
		CurrentPeriodNumber: periodNumber,
		ExpiresAt:           expiresAt,
		PeriodID:            entry.SubscriptionPeriodID,
		PeriodStartsAt:      startsAt,
		PeriodEndsAt:        endsAt,
		UpdatedAt:           &updatedAt,
	}, nil
}

func (s *Store) PutSubscription(ctx context.Context, params PutSubscriptionParams) (BillingSubscriptionState, error) {
	var state BillingSubscriptionState
	if err := validateBillingWrite(params.BillingWriteParams); err != nil || params.UserID == "" {
		if err != nil {
			return state, err
		}
		return state, fmt.Errorf("%w: subscription user is required", ErrInvalid)
	}
	duration, err := billingPeriodDuration(params.Tier)
	if err != nil {
		return state, err
	}
	if err := validateBillingPeriodCount(params.PeriodCount); err != nil {
		return state, err
	}
	allowance, err := decimal.ParseInput(params.AllowanceUSD, false, false)
	if err != nil {
		return state, fmt.Errorf("%w: invalid subscription allowance", ErrInvalid)
	}
	params.At = normalizedBillingTime(params.At, s.now)
	params.Reason = strings.TrimSpace(params.Reason)
	fingerprint := billingOperationFingerprint("subscription_set", params.ActorUserID,
		params.UserID, params.Tier, params.Reason, allowance, fmt.Sprintf("%d", params.PeriodCount))
	var compatibleFingerprints [][]byte
	if params.PeriodCount == 1 {
		// Before finite-period subscriptions, subscription_set fingerprints did
		// not include a period count. Existing subscriptions migrate to 1/1, so
		// accept that exact legacy request only when the caller explicitly sends
		// the migrated one-period configuration.
		compatibleFingerprints = append(compatibleFingerprints, billingOperationFingerprint(
			"subscription_set", params.ActorUserID, params.UserID, params.Tier, params.Reason, allowance))
	}
	var expiresAt *time.Time
	if params.PeriodCount > 0 {
		value := params.At.Add(time.Duration(params.PeriodCount) * duration)
		expiresAt = &value
	}
	err = s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if err := lockBillingAccountTx(ctx, tx, params.UserID); err != nil {
			return err
		}
		created, replayID, err := claimBillingOperationTx(ctx, tx, params.BillingWriteParams,
			"subscription_set", params.UserID, fingerprint, compatibleFingerprints...)
		if err != nil {
			return err
		}
		if !created {
			entry, err := replayBillingLedgerTx(ctx, tx, *replayID)
			if err != nil {
				return err
			}
			state, err = subscriptionOperationStateTx(ctx, tx, entry, true)
			return err
		}
		var subscriptionID string
		var oldPeriodID sql.NullString
		var wasEnabled bool
		err = tx.QueryRowContext(ctx, `SELECT id, current_period_id
			, enabled FROM billing_subscriptions WHERE user_id = $1 AND tier = $2 FOR UPDATE`,
			params.UserID, params.Tier).Scan(&subscriptionID, &oldPeriodID, &wasEnabled)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			subscriptionID, err = newUUID()
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO billing_subscriptions
				(id,user_id,tier,enabled,allowance_usd,period_count,
				 current_period_number,expires_at,created_at,updated_at)
				VALUES ($1,$2,$3,true,$4::numeric,$5,1,$6,$7,$7)`, subscriptionID,
				params.UserID, params.Tier, allowance, params.PeriodCount,
				timeOrNil(expiresAt), params.At); err != nil {
				return mapDBError("create billing subscription", err)
			}
		case err != nil:
			return mapDBError("lock billing subscription", err)
		case oldPeriodID.Valid && wasEnabled:
			result, err := tx.ExecContext(ctx, `UPDATE billing_subscription_periods
				SET closed_at = $2, close_reason = 'modified'
				WHERE id = $1 AND closed_at IS NULL AND $2 >= starts_at`, oldPeriodID.String, params.At)
			if err != nil {
				return mapDBError("close modified subscription period", err)
			}
			if err := requireAffected("close modified subscription period", result); err != nil {
				return err
			}
		}
		periodID, err := newUUID()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO billing_subscription_periods
			(id,subscription_id,user_id,tier,starts_at,ends_at,allowance_usd,
			 remaining_usd,period_number,period_count,created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7::numeric,$7::numeric,1,$8,$5)`, periodID,
			subscriptionID, params.UserID, params.Tier, params.At,
			params.At.Add(duration), allowance, params.PeriodCount); err != nil {
			return mapDBError("create subscription period", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE billing_subscriptions SET enabled = true,
			allowance_usd = $2::numeric, period_count = $3, current_period_number = 1,
			expires_at = $4, current_period_id = $5, disabled_at = NULL,
			updated_at = $6 WHERE id = $1`, subscriptionID, allowance, params.PeriodCount,
			timeOrNil(expiresAt), periodID, params.At); err != nil {
			return mapDBError("activate billing subscription", err)
		}
		entry, err := scanBillingLedgerEntry(tx.QueryRowContext(ctx, `
			INSERT INTO billing_ledger_entries
				(user_id,operation_id,entry_type,amount_usd,subscription_tier,
				 subscription_period_id,reason,actor_user_id,created_at)
			VALUES ($1,$2,'subscription_set',$3::numeric,$4,$5,$6,$7,$8)
			RETURNING `+billingLedgerColumns, params.UserID, params.OperationID,
			allowance, params.Tier, periodID, params.Reason, params.ActorUserID, params.At))
		if err != nil {
			return mapDBError("record subscription ledger", err)
		}
		if err := finishBillingOperationTx(ctx, tx, params.OperationID, entry.ID); err != nil {
			return err
		}
		if err := appendBillingAuditTx(ctx, tx, params.BillingWriteParams,
			"billing.subscription_updated", "subscription", params.UserID+":"+params.Tier,
			map[string]any{"operation_id": params.OperationID, "tier": params.Tier,
				"allowance_usd": allowance, "period_count": params.PeriodCount}); err != nil {
			return err
		}
		state, err = subscriptionOperationStateTx(ctx, tx, entry, true)
		return err
	})
	return state, err
}

func (s *Store) DeleteSubscription(ctx context.Context, params DeleteSubscriptionParams) (BillingSubscriptionState, error) {
	var state BillingSubscriptionState
	if err := validateBillingWrite(params.BillingWriteParams); err != nil || params.UserID == "" {
		if err != nil {
			return state, err
		}
		return state, fmt.Errorf("%w: subscription user is required", ErrInvalid)
	}
	if _, err := billingPeriodDuration(params.Tier); err != nil {
		return state, err
	}
	params.At = normalizedBillingTime(params.At, s.now)
	params.Reason = strings.TrimSpace(params.Reason)
	fingerprint := billingOperationFingerprint("subscription_disable", params.ActorUserID,
		params.UserID, params.Tier, params.Reason)
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if err := lockBillingAccountTx(ctx, tx, params.UserID); err != nil {
			return err
		}
		created, replayID, err := claimBillingOperationTx(ctx, tx, params.BillingWriteParams,
			"subscription_disable", params.UserID, fingerprint)
		if err != nil {
			return err
		}
		if !created {
			entry, err := replayBillingLedgerTx(ctx, tx, *replayID)
			if err != nil {
				return err
			}
			state, err = subscriptionOperationStateTx(ctx, tx, entry, false)
			return err
		}
		var subscriptionID, allowance string
		var periodID sql.NullString
		var expiresAt sql.NullTime
		var periodCount, periodNumber int
		var enabled bool
		if err := tx.QueryRowContext(ctx, `SELECT id, allowance_usd::text,
			enabled, current_period_id, period_count, current_period_number, expires_at
			FROM billing_subscriptions
			WHERE user_id = $1 AND tier = $2 FOR UPDATE`, params.UserID, params.Tier,
		).Scan(&subscriptionID, &allowance, &enabled, &periodID, &periodCount,
			&periodNumber, &expiresAt); err != nil {
			return mapDBError("lock subscription for disable", err)
		}
		if enabled && periodID.Valid {
			result, err := tx.ExecContext(ctx, `UPDATE billing_subscription_periods
				SET closed_at = $2, close_reason = 'disabled'
				WHERE id = $1 AND closed_at IS NULL AND $2 >= starts_at`, periodID.String, params.At)
			if err != nil {
				return mapDBError("close disabled subscription period", err)
			}
			if err := requireAffected("close disabled subscription period", result); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE billing_subscriptions SET enabled = false,
			disabled_at = CASE WHEN enabled THEN $2 ELSE disabled_at END,
			updated_at = $2 WHERE id = $1`, subscriptionID, params.At); err != nil {
			return mapDBError("disable billing subscription", err)
		}
		entry, err := scanBillingLedgerEntry(tx.QueryRowContext(ctx, `
			INSERT INTO billing_ledger_entries
				(user_id,operation_id,entry_type,amount_usd,subscription_tier,
				 subscription_period_id,reason,actor_user_id,created_at)
			VALUES ($1,$2,'subscription_disable',$3::numeric,$4,$5,$6,$7,$8)
			RETURNING `+billingLedgerColumns, params.UserID, params.OperationID,
			allowance, params.Tier, periodID, params.Reason, params.ActorUserID, params.At))
		if err != nil {
			return mapDBError("record subscription disable ledger", err)
		}
		if !periodID.Valid {
			if _, err := tx.ExecContext(ctx, `INSERT INTO billing_subscription_operation_snapshots
				(ledger_entry_id, subscription_id, period_count, current_period_number, expires_at)
				VALUES ($1,$2,$3,$4,$5)`, entry.ID, subscriptionID, periodCount,
				periodNumber, expiresAt); err != nil {
				return mapDBError("record subscription operation snapshot", err)
			}
		}
		if err := finishBillingOperationTx(ctx, tx, params.OperationID, entry.ID); err != nil {
			return err
		}
		if err := appendBillingAuditTx(ctx, tx, params.BillingWriteParams,
			"billing.subscription_disabled", "subscription", params.UserID+":"+params.Tier,
			map[string]any{"operation_id": params.OperationID, "tier": params.Tier}); err != nil {
			return err
		}
		state, err = subscriptionOperationStateTx(ctx, tx, entry, false)
		return err
	})
	return state, err
}

func (s *Store) GetBillingState(ctx context.Context, userID string, limit, offset int) (BillingState, error) {
	var state BillingState
	if userID == "" || offset < 0 {
		return state, fmt.Errorf("%w: invalid billing state query", ErrInvalid)
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `
			SELECT u.id, u.username, u.display_name, a.balance_usd::text
			FROM users u JOIN billing_accounts a ON a.user_id = u.id
			WHERE u.id = $1 FOR UPDATE OF a`, userID).Scan(
			&state.UserID, &state.Username, &state.DisplayName, &state.BalanceUSD); err != nil {
			return mapDBError("lock billing state account", err)
		}
		if _, err := rollBillingSubscriptionsTx(ctx, tx, userID, s.now().UTC()); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT s.id, s.tier, s.enabled, s.allowance_usd::text,
				s.period_count, s.current_period_number, s.expires_at, p.id,
				p.starts_at, p.ends_at, p.remaining_usd::text, s.updated_at
			FROM billing_subscriptions s
			LEFT JOIN billing_subscription_periods p ON p.id = s.current_period_id
			WHERE s.user_id = $1
			ORDER BY CASE s.tier WHEN 'day' THEN 1 WHEN 'week' THEN 2 ELSE 3 END`, userID)
		if err != nil {
			return mapDBError("list billing subscriptions", err)
		}
		byTier := map[string]BillingSubscriptionState{
			BillingTierDay:   {Tier: BillingTierDay, AllowanceUSD: "0.000000000000", RemainingUSD: "0.000000000000"},
			BillingTierWeek:  {Tier: BillingTierWeek, AllowanceUSD: "0.000000000000", RemainingUSD: "0.000000000000"},
			BillingTierMonth: {Tier: BillingTierMonth, AllowanceUSD: "0.000000000000", RemainingUSD: "0.000000000000"},
		}
		for rows.Next() {
			value, err := subscriptionStateFromPeriodRow(rows)
			if err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan billing subscription state: %w", err)
			}
			byTier[value.Tier] = value
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close billing subscriptions: %w", err)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate billing subscriptions: %w", err)
		}
		state.Subscriptions = []BillingSubscriptionState{
			byTier[BillingTierDay], byTier[BillingTierWeek], byTier[BillingTierMonth],
		}
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM billing_ledger_entries
			WHERE user_id = $1`, userID).Scan(&state.LedgerTotal); err != nil {
			return mapDBError("count billing ledger", err)
		}
		ledgerRows, err := tx.QueryContext(ctx, `SELECT `+billingLedgerColumns+`
			FROM billing_ledger_entries WHERE user_id = $1
			ORDER BY created_at DESC, id DESC LIMIT $2 OFFSET $3`, userID, limit, offset)
		if err != nil {
			return mapDBError("list billing ledger", err)
		}
		state.Ledger = make([]BillingLedgerEntry, 0)
		for ledgerRows.Next() {
			entry, err := scanBillingLedgerEntry(ledgerRows)
			if err != nil {
				_ = ledgerRows.Close()
				return fmt.Errorf("scan billing ledger: %w", err)
			}
			state.Ledger = append(state.Ledger, entry)
		}
		if err := ledgerRows.Close(); err != nil {
			return fmt.Errorf("close billing ledger: %w", err)
		}
		if err := ledgerRows.Err(); err != nil {
			return fmt.Errorf("iterate billing ledger: %w", err)
		}
		return nil
	})
	return state, err
}

func (s *Store) ListBillingUsers(ctx context.Context) ([]BillingUserSummary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, username, display_name, role, status
		FROM users ORDER BY lower(username), id`)
	if err != nil {
		return nil, mapDBError("list billing users", err)
	}
	type listedUser struct{ id, username, displayName, role, status string }
	var users []listedUser
	for rows.Next() {
		var user listedUser
		if err := rows.Scan(&user.id, &user.username, &user.displayName, &user.role, &user.status); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan billing user: %w", err)
		}
		users = append(users, user)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close billing users: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate billing users: %w", err)
	}
	result := make([]BillingUserSummary, 0, len(users))
	for _, user := range users {
		state, err := s.GetBillingState(ctx, user.id, 1, 0)
		if err != nil {
			return nil, err
		}
		result = append(result, BillingUserSummary{UserID: user.id, Username: user.username,
			DisplayName: user.displayName, Role: user.role, Status: user.status,
			BalanceUSD: state.BalanceUSD, Subscriptions: state.Subscriptions})
	}
	return result, nil
}

func billingRat(value string) (*big.Rat, error) {
	result, ok := new(big.Rat).SetString(value)
	if !ok {
		return nil, fmt.Errorf("%w: invalid stored billing amount", ErrInvalid)
	}
	return result, nil
}

func billingMin(left, right string) (string, error) {
	l, err := billingRat(left)
	if err != nil {
		return "", err
	}
	r, err := billingRat(right)
	if err != nil {
		return "", err
	}
	if l.Cmp(r) > 0 {
		l = r
	}
	return l.FloatString(decimal.AmountScale), nil
}

func billingSubtract(left, right string) (string, error) {
	l, err := billingRat(left)
	if err != nil {
		return "", err
	}
	r, err := billingRat(right)
	if err != nil {
		return "", err
	}
	return l.Sub(l, r).FloatString(decimal.AmountScale), nil
}

func billingAdd(left, right string) (string, error) {
	l, err := billingRat(left)
	if err != nil {
		return "", err
	}
	r, err := billingRat(right)
	if err != nil {
		return "", err
	}
	return l.Add(l, r).FloatString(decimal.AmountScale), nil
}

func billingPositive(value string) bool {
	r, ok := new(big.Rat).SetString(value)
	return ok && r.Sign() > 0
}

func (s *Store) SettleBilling(ctx context.Context, requestID string, at time.Time) (BillingReservation, error) {
	if at.IsZero() {
		at = s.now().UTC()
	}
	var reservation BillingReservation
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		var err error
		reservation, err = settleBillingTx(ctx, tx, requestID, at)
		return err
	})
	return reservation, err
}

// settleBillingTx prices terminal usage from the admission snapshot and
// atomically drains only admission-bound sources in day/week/month/cash order.
func settleBillingTx(ctx context.Context, tx *sql.Tx, requestID string, at time.Time) (BillingReservation, error) {
	if requestID == "" {
		return BillingReservation{}, fmt.Errorf("%w: empty billing request id", ErrInvalid)
	}
	if at.IsZero() {
		at = time.Now().UTC()
	} else {
		at = at.UTC()
	}
	pre, err := scanBillingReservation(tx.QueryRowContext(ctx,
		`SELECT `+billingReservationColumns+` FROM billing_reservations WHERE request_id = $1`, requestID))
	if err != nil {
		return BillingReservation{}, mapDBError("read billing reservation", err)
	}
	// Keep the same account-before-source order used by admission and all
	// administrative balance/subscription changes.
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM billing_accounts WHERE user_id = $1 FOR UPDATE`, pre.UserID); err != nil {
		return BillingReservation{}, mapDBError("lock billing account", err)
	}
	reservation, err := scanBillingReservation(tx.QueryRowContext(ctx,
		`SELECT `+billingReservationColumns+` FROM billing_reservations WHERE request_id = $1 FOR UPDATE`, requestID))
	if err != nil {
		return BillingReservation{}, mapDBError("lock billing reservation", err)
	}
	if reservation.State == "settled" {
		return reservation, nil
	}
	if reservation.State == "released" {
		return BillingReservation{}, fmt.Errorf("settle released billing reservation: %w", ErrConflict)
	}
	var usageRequestedAt time.Time
	var actualModel string
	var actualServiceTier sql.NullString
	var inputTokens, cachedTokens, cacheWriteTokens, outputTokens int64
	var cacheWriteTokensPresent bool
	if err := tx.QueryRowContext(ctx, `
		SELECT requested_at, model, actual_service_tier, input_tokens,
			cached_input_tokens, cache_write_tokens, cache_write_tokens_present,
			output_tokens
		FROM usage_requests WHERE request_id = $1 AND state <> 'in_progress'
		FOR UPDATE`, requestID).Scan(&usageRequestedAt, &actualModel, &actualServiceTier,
		&inputTokens, &cachedTokens, &cacheWriteTokens, &cacheWriteTokensPresent,
		&outputTokens); err != nil {
		return BillingReservation{}, mapDBError("read terminal usage for billing", err)
	}
	var decision config.PricingDecision
	pricingFallbackReason := ""
	var cost string
	if reservation.PricingRuleVersion == config.PricingSchemaV1 {
		cost, err = decimal.CalculateCost(inputTokens, cachedTokens, outputTokens,
			reservation.InputUSDPerMillion, reservation.CachedInputUSDPerMillion,
			reservation.OutputUSDPerMillion)
	} else {
		snapshot, snapshotErr := config.ParsePricingSnapshot(reservation.PricingSnapshot)
		if snapshotErr != nil {
			return BillingReservation{}, fmt.Errorf("parse request pricing snapshot: %w", snapshotErr)
		}
		selectionTier := actualServiceTier.String
		if reservation.BillingMode == BillingModeInternalZero {
			selectionTier = "default"
		}
		decision, err = snapshot.Select(selectionTier, inputTokens)
		if err == nil && snapshot.Rule.CacheWriteMode == config.CacheWriteSeparate && !cacheWriteTokensPresent {
			cacheWriteTokens = inputTokens - cachedTokens
			decision.FallbackReason = config.AppendFallbackReason(
				decision.FallbackReason, config.FallbackMissingCacheWriteTokens,
			)
		}
		if err == nil {
			cost, err = decimal.CalculateCostV2(
				inputTokens, cachedTokens, cacheWriteTokens, outputTokens,
				snapshot.Rule.CacheWriteMode, decision.InputUSDPerMillion,
				decision.CachedInputUSDPerMillion, decision.CacheWriteUSDPerMillion,
				decision.OutputUSDPerMillion,
			)
			pricingFallbackReason = decision.FallbackReason
		}
	}
	if err != nil {
		return BillingReservation{}, fmt.Errorf("calculate request cost: %w", err)
	}
	if reservation.PricingRuleVersion == config.PricingSchemaV2 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE usage_requests SET cache_write_tokens = $2,
				pricing_service_tier = $3, context_class = $4,
				pricing_fallback_reason = $5
			WHERE request_id = $1`, requestID, cacheWriteTokens,
			decision.PricingServiceTier, decision.ContextClass,
			valueOrNil(pricingFallbackReason)); err != nil {
			return BillingReservation{}, mapDBError("record usage pricing decision", err)
		}
	}
	remaining, charged := cost, "0.000000000000"
	allocationOrder := 0
	type periodSource struct {
		tier string
		id   *string
	}
	for _, source := range []periodSource{
		{BillingTierDay, reservation.DayPeriodID},
		{BillingTierWeek, reservation.WeekPeriodID},
		{BillingTierMonth, reservation.MonthPeriodID},
	} {
		if source.id == nil || !billingPositive(remaining) {
			continue
		}
		var available string
		err := tx.QueryRowContext(ctx, `
			SELECT remaining_usd::text FROM billing_subscription_periods
			WHERE id = $1 AND user_id = $2 AND tier = $3 FOR UPDATE`, *source.id, reservation.UserID, source.tier,
		).Scan(&available)
		if err != nil {
			return BillingReservation{}, mapDBError("lock bound subscription period", err)
		}
		amount, err := billingMin(available, remaining)
		if err != nil {
			return BillingReservation{}, err
		}
		if !billingPositive(amount) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE billing_subscription_periods
			SET remaining_usd = remaining_usd - $2::numeric WHERE id = $1`, *source.id, amount); err != nil {
			return BillingReservation{}, mapDBError("charge bound subscription period", err)
		}
		allocationOrder++
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO billing_charge_allocations
				(request_id, allocation_order, source_type, subscription_period_id, amount_usd, created_at)
			VALUES ($1,$2,$3,$4,$5::numeric,$6)`, requestID, allocationOrder, source.tier, *source.id, amount, at); err != nil {
			return BillingReservation{}, mapDBError("record subscription charge allocation", err)
		}
		remaining, err = billingSubtract(remaining, amount)
		if err != nil {
			return BillingReservation{}, err
		}
		charged, err = billingAdd(charged, amount)
		if err != nil {
			return BillingReservation{}, err
		}
	}
	cashCharged := "0.000000000000"
	if reservation.CashLotCutoff != nil && billingPositive(remaining) {
		rows, err := tx.QueryContext(ctx, `
			SELECT id, remaining_usd::text FROM billing_cash_credit_lots
			WHERE user_id = $1 AND lot_sequence <= $2 AND remaining_usd > 0
			ORDER BY lot_sequence FOR UPDATE`, reservation.UserID, *reservation.CashLotCutoff)
		if err != nil {
			return BillingReservation{}, mapDBError("lock bound cash lots", err)
		}
		type boundCashLot struct {
			id        int64
			available string
		}
		var lots []boundCashLot
		for rows.Next() {
			var lot boundCashLot
			if err := rows.Scan(&lot.id, &lot.available); err != nil {
				_ = rows.Close()
				return BillingReservation{}, fmt.Errorf("scan bound cash lot: %w", err)
			}
			lots = append(lots, lot)
		}
		if err := rows.Close(); err != nil {
			return BillingReservation{}, fmt.Errorf("close bound cash lots: %w", err)
		}
		if err := rows.Err(); err != nil {
			return BillingReservation{}, fmt.Errorf("iterate bound cash lots: %w", err)
		}
		for _, lot := range lots {
			if !billingPositive(remaining) {
				break
			}
			amount, err := billingMin(lot.available, remaining)
			if err != nil {
				return BillingReservation{}, err
			}
			if !billingPositive(amount) {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE billing_cash_credit_lots
				SET remaining_usd = remaining_usd - $2::numeric WHERE id = $1`, lot.id, amount); err != nil {
				return BillingReservation{}, mapDBError("charge bound cash lot", err)
			}
			allocationOrder++
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO billing_charge_allocations
					(request_id, allocation_order, source_type, cash_credit_lot_id, amount_usd, created_at)
				VALUES ($1,$2,'cash',$3,$4::numeric,$5)`, requestID, allocationOrder, lot.id, amount, at); err != nil {
				return BillingReservation{}, mapDBError("record cash charge allocation", err)
			}
			remaining, err = billingSubtract(remaining, amount)
			if err != nil {
				return BillingReservation{}, err
			}
			charged, err = billingAdd(charged, amount)
			if err != nil {
				return BillingReservation{}, err
			}
			cashCharged, err = billingAdd(cashCharged, amount)
			if err != nil {
				return BillingReservation{}, err
			}
		}
	}
	var balanceAfter string
	if err := tx.QueryRowContext(ctx, `
		UPDATE billing_accounts SET balance_usd = balance_usd - $2::numeric, updated_at = $3
		WHERE user_id = $1 AND balance_usd >= $2::numeric
		RETURNING balance_usd::text`, reservation.UserID, cashCharged, at).Scan(&balanceAfter); err != nil {
		return BillingReservation{}, mapDBError("debit billing account", err)
	}
	var ledgerRequestedAt, ledgerActualModel, ledgerCacheWriteTokens any
	var ledgerCacheWriteMode, ledgerRequestedTier, ledgerActualTier any
	var ledgerPricingTier, ledgerContextClass, ledgerCatalog any
	var ledgerAppliedInput, ledgerAppliedCached, ledgerAppliedCacheWrite, ledgerAppliedOutput any
	var ledgerFallback any
	if reservation.PricingRuleVersion == config.PricingSchemaV2 {
		ledgerRequestedAt = usageRequestedAt
		ledgerActualModel = actualModel
		ledgerCacheWriteTokens = cacheWriteTokens
		ledgerCacheWriteMode = pointerDatabaseValue(reservation.CacheWriteMode)
		ledgerRequestedTier = pointerDatabaseValue(reservation.RequestedServiceTier)
		ledgerActualTier = valueOrNil(actualServiceTier.String)
		ledgerPricingTier = decision.PricingServiceTier
		ledgerContextClass = decision.ContextClass
		ledgerCatalog = pointerDatabaseValue(reservation.PricingCatalogAsOf)
		ledgerAppliedInput = decision.InputUSDPerMillion
		ledgerAppliedCached = decision.CachedInputUSDPerMillion
		ledgerAppliedCacheWrite = decision.CacheWriteUSDPerMillion
		ledgerAppliedOutput = decision.OutputUSDPerMillion
		ledgerFallback = valueOrNil(pricingFallbackReason)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO billing_ledger_entries
			(user_id, entry_type, amount_usd, cash_delta_usd, balance_after_usd,
			 request_id, model, input_tokens, cached_input_tokens, output_tokens,
			 actual_cost_usd, charged_usd, uncovered_usd, reason, created_at,
			 usage_requested_at, actual_model, cache_write_tokens, cache_write_mode,
			 requested_service_tier, actual_service_tier, pricing_service_tier,
			 context_class, pricing_rule_version, pricing_catalog_as_of,
			 applied_input_usd_per_million, applied_cached_input_usd_per_million,
			 applied_cache_write_usd_per_million, applied_output_usd_per_million,
			 pricing_fallback_reason)
		VALUES ($1,'usage_charge',$2::numeric,-$3::numeric,$4::numeric,$5,$6,$7,$8,$9,
			$2::numeric,$10::numeric,$11::numeric,'request usage charge',$12,
			$13,$14,$15,$16,$17,$18,$19,$20,$21,$22::date,$23::numeric,$24::numeric,
			$25::numeric,$26::numeric,$27)`,
		reservation.UserID, cost, cashCharged, balanceAfter, requestID, reservation.Model,
		inputTokens, cachedTokens, outputTokens, charged, remaining, at,
		ledgerRequestedAt, ledgerActualModel, ledgerCacheWriteTokens, ledgerCacheWriteMode,
		ledgerRequestedTier, ledgerActualTier, ledgerPricingTier, ledgerContextClass,
		reservation.PricingRuleVersion, ledgerCatalog, ledgerAppliedInput, ledgerAppliedCached,
		ledgerAppliedCacheWrite, ledgerAppliedOutput, ledgerFallback); err != nil {
		return BillingReservation{}, mapDBError("record usage billing ledger", err)
	}
	var settledCacheWrite, settledActualTier, settledPricingTier, settledActualModel any
	var settledContext, settledAppliedInput, settledAppliedCached, settledAppliedCacheWrite any
	var settledAppliedOutput, settledFallback any
	if reservation.PricingRuleVersion == config.PricingSchemaV2 {
		settledCacheWrite = cacheWriteTokens
		settledActualTier = valueOrNil(actualServiceTier.String)
		settledPricingTier = decision.PricingServiceTier
		settledActualModel = actualModel
		settledContext = decision.ContextClass
		settledAppliedInput = decision.InputUSDPerMillion
		settledAppliedCached = decision.CachedInputUSDPerMillion
		settledAppliedCacheWrite = decision.CacheWriteUSDPerMillion
		settledAppliedOutput = decision.OutputUSDPerMillion
		settledFallback = valueOrNil(pricingFallbackReason)
	}
	reservation, err = scanBillingReservation(tx.QueryRowContext(ctx, `
		UPDATE billing_reservations SET state = 'settled', actual_input_tokens = $2,
			actual_cached_input_tokens = $3, actual_output_tokens = $4,
			actual_cost_usd = $5::numeric, charged_usd = $6::numeric,
			uncovered_usd = $7::numeric, settled_at = $8,
			actual_cache_write_tokens = $9, actual_service_tier = $10,
			pricing_service_tier = $11, actual_model = $12, context_class = $13,
			applied_input_usd_per_million = $14::numeric,
			applied_cached_input_usd_per_million = $15::numeric,
			applied_cache_write_usd_per_million = $16::numeric,
			applied_output_usd_per_million = $17::numeric,
			pricing_fallback_reason = $18
		WHERE request_id = $1 RETURNING `+billingReservationColumns,
		requestID, inputTokens, cachedTokens, outputTokens, cost, charged, remaining, at,
		settledCacheWrite, settledActualTier, settledPricingTier, settledActualModel,
		settledContext, settledAppliedInput, settledAppliedCached, settledAppliedCacheWrite,
		settledAppliedOutput, settledFallback))
	return reservation, mapDBError("settle billing reservation", err)
}

func pointerDatabaseValue(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func (s *Store) ReleaseBilling(ctx context.Context, requestID string, at time.Time) error {
	if at.IsZero() {
		at = s.now().UTC()
	}
	return s.withTx(ctx, nil, func(tx *sql.Tx) error {
		return releaseBillingTx(ctx, tx, requestID, at)
	})
}

func releaseBillingTx(ctx context.Context, tx *sql.Tx, requestID string, at time.Time) error {
	if requestID == "" {
		return fmt.Errorf("%w: empty billing request id", ErrInvalid)
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM billing_reservations
		WHERE request_id = $1 FOR UPDATE`, requestID).Scan(&state); err != nil {
		return mapDBError("lock billing reservation for release", err)
	}
	switch state {
	case "released":
		return nil
	case "settled":
		return fmt.Errorf("release settled billing reservation: %w", ErrConflict)
	}
	result, err := tx.ExecContext(ctx, `UPDATE billing_reservations
		SET state = 'released', settled_at = $2 WHERE request_id = $1 AND state = 'reserved'`, requestID, at)
	if err != nil {
		return mapDBError("release billing reservation", err)
	}
	return requireAffected("release billing reservation", result)
}

// RetryUnsettledBilling first settles terminal usage metadata and deliberately
// leaves non-terminal reservations untouched for the stale-release pass.
func (s *Store) RetryUnsettledBilling(ctx context.Context, limit int) (int, error) {
	if limit <= 0 || limit > 10_000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT b.request_id FROM billing_reservations b
		JOIN usage_requests u ON u.request_id = b.request_id
		WHERE b.state = 'reserved' AND u.state <> 'in_progress'
		ORDER BY u.completed_at, b.created_at LIMIT $1`, limit)
	if err != nil {
		return 0, mapDBError("list unsettled terminal billing", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan unsettled terminal billing: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close unsettled terminal billing: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate unsettled terminal billing: %w", err)
	}
	settled := 0
	for _, id := range ids {
		_, err := s.SettleBilling(ctx, id, time.Time{})
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return settled, err
		}
		settled++
	}
	return settled, nil
}
