package server

import (
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/store"
)

const (
	globalUsageMaximumRange = 90 * 24 * time.Hour
	moneyDecimalPlaces      = 6
)

type pricedUsage struct {
	Requests          int64  `json:"requests"`
	Tokens            int64  `json:"tokens"`
	InputTokens       int64  `json:"input_tokens"`
	CachedInputTokens int64  `json:"cached_input_tokens"`
	CacheWriteTokens  int64  `json:"cache_write_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	ReasoningTokens   int64  `json:"reasoning_tokens"`
	PricedTokens      int64  `json:"priced_tokens"`
	UnpricedTokens    int64  `json:"unpriced_tokens"`
	EstimatedUSD      string `json:"estimated_usd"`
	EstimatedCNY      string `json:"estimated_cny"`
	ActualCostUSD     string `json:"actual_cost_usd"`
	ChargedUSD        string `json:"charged_usd"`
	UncoveredUSD      string `json:"uncovered_usd"`
}

type globalUserUsage struct {
	ID              string      `json:"id"`
	Username        string      `json:"username"`
	DisplayName     string      `json:"display_name"`
	Usage           pricedUsage `json:"usage"`
	Share           string      `json:"share"`
	PricingCoverage string      `json:"pricing_coverage"`
	PricingStatus   string      `json:"pricing_status"`
}

type globalUsageSummary struct {
	Usage           pricedUsage `json:"usage"`
	ActiveUsers     int         `json:"active_users"`
	TotalUsers      int         `json:"total_users"`
	PricingCoverage string      `json:"pricing_coverage"`
}

type globalUsagePeriod struct {
	From  *time.Time `json:"from"`
	Until time.Time  `json:"until"`
	All   bool       `json:"all"`
	Model string     `json:"model"`
}

type globalUsageResponse struct {
	Period    globalUsagePeriod      `json:"period"`
	Summary   globalUsageSummary     `json:"summary"`
	Users     []globalUserUsage      `json:"users"`
	Pricing   globalPricingMeta      `json:"pricing"`
	Breakdown globalPricingBreakdown `json:"breakdown"`
}

type globalPricingDimension struct {
	Value            string `json:"value"`
	Requests         int64  `json:"requests"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	ActualCostUSD    string `json:"actual_cost_usd"`
}

type globalPricingBreakdown struct {
	ServiceTiers   []globalPricingDimension `json:"service_tiers"`
	ContextClasses []globalPricingDimension `json:"context_classes"`
	Fallbacks      []globalPricingDimension `json:"fallbacks"`
}

type globalPricingMeta struct {
	CatalogAsOf    string   `json:"catalog_as_of"`
	FXAsOf         string   `json:"fx_as_of"`
	USDCNYRate     string   `json:"usd_cny_rate"`
	UnpricedModels []string `json:"unpriced_models"`
	Disclaimer     string   `json:"disclaimer"`
}

type globalUsageQuery struct {
	From     time.Time
	Until    time.Time
	All      bool
	Model    string
	LiveFrom time.Time
}

type usageAccumulator struct {
	usage        pricedUsage
	actualUSD    *big.Rat
	chargedUSD   *big.Rat
	uncoveredUSD *big.Rat
}

type userUsageAccumulator struct {
	id          string
	username    string
	displayName string
	usage       usageAccumulator
}

func (s *Server) globalUsageJSON(w http.ResponseWriter, r *http.Request) {
	query, err := parseGlobalUsageQuery(time.Now(), r.URL.Query())
	if err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_global_usage_filter", "全员统计区间或模型筛选无效")
		return
	}

	rows, err := s.store.GlobalUsage(
		r.Context(), query.From, query.Until, query.Model, query.All, query.LiveFrom,
	)
	if err != nil {
		internalError(s, w, r, "global usage", err)
		return
	}
	breakdownRows, err := s.store.GlobalPricingBreakdown(
		r.Context(), query.From, query.Until, query.Model, query.All,
	)
	if err != nil {
		internalError(s, w, r, "global pricing breakdown", err)
		return
	}
	response, err := summarizeGlobalUsage(rows, s.config.UsagePricing)
	if err != nil {
		internalError(s, w, r, "price global usage", err)
		return
	}
	response.Breakdown = formatGlobalPricingBreakdown(breakdownRows)
	response.Period = globalUsagePeriod{
		Until: query.Until,
		All:   query.All,
		Model: query.Model,
	}
	if !query.All {
		from := query.From
		response.Period.From = &from
	}
	writeJSON(w, http.StatusOK, response)
}

func formatGlobalPricingBreakdown(rows []store.GlobalPricingBreakdownRow) globalPricingBreakdown {
	result := globalPricingBreakdown{
		ServiceTiers:   make([]globalPricingDimension, 0),
		ContextClasses: make([]globalPricingDimension, 0),
		Fallbacks:      make([]globalPricingDimension, 0),
	}
	for _, row := range rows {
		value := globalPricingDimension{
			Value: row.Value, Requests: row.RequestCount,
			CacheWriteTokens: row.CacheWriteTokens, ActualCostUSD: row.ActualCostUSD,
		}
		switch row.Dimension {
		case "service_tier":
			result.ServiceTiers = append(result.ServiceTiers, value)
		case "context_class":
			result.ContextClasses = append(result.ContextClasses, value)
		case "fallback":
			result.Fallbacks = append(result.Fallbacks, value)
		}
	}
	return result
}

func parseGlobalUsageQuery(now time.Time, values url.Values) (globalUsageQuery, error) {
	now = now.UTC()
	query := globalUsageQuery{
		From:     time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC),
		Until:    now,
		LiveFrom: time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, time.UTC),
	}

	if rawValues, ok := values["all"]; ok {
		if len(rawValues) != 1 || (rawValues[0] != "true" && rawValues[0] != "false") {
			return globalUsageQuery{}, errors.New("all must be true or false")
		}
		query.All = rawValues[0] == "true"
	}
	if rawValues, ok := values["model"]; ok {
		if len(rawValues) != 1 {
			return globalUsageQuery{}, errors.New("model must be supplied once")
		}
		query.Model = rawValues[0]
		if query.Model != "" && !validModel(query.Model) {
			return globalUsageQuery{}, errors.New("model must be an exact model name")
		}
	}
	if query.All {
		_, hasFrom := values["from"]
		_, hasUntil := values["until"]
		if hasFrom || hasUntil {
			return globalUsageQuery{}, errors.New("all cannot be combined with a bounded interval")
		}
		return query, nil
	}

	var err error
	fromValue := ""
	untilValue := ""
	if rawValues, ok := values["from"]; ok {
		if len(rawValues) != 1 {
			return globalUsageQuery{}, errors.New("from must be supplied once")
		}
		if rawValues[0] != "" {
			fromValue = rawValues[0]
			query.From, err = parseDashboardTime(fromValue, false)
		}
		if err != nil {
			return globalUsageQuery{}, fmt.Errorf("parse from: %w", err)
		}
	}
	if rawValues, ok := values["until"]; ok {
		if len(rawValues) != 1 {
			return globalUsageQuery{}, errors.New("until must be supplied once")
		}
		if rawValues[0] != "" {
			untilValue = rawValues[0]
			query.Until, err = parseDashboardTime(untilValue, true)
		}
		if err != nil {
			return globalUsageQuery{}, fmt.Errorf("parse until: %w", err)
		}
	}
	if !query.From.Before(query.Until) || !globalUsageRangeAllowed(query.From, query.Until, fromValue, untilValue) {
		return globalUsageQuery{}, errors.New("bounded interval must be positive and no longer than 90 days")
	}
	return query, nil
}

func globalUsageRangeAllowed(from, until time.Time, fromValue, untilValue string) bool {
	elapsed := until.Sub(from)
	if elapsed <= globalUsageMaximumRange {
		return true
	}
	// Browser-local midnight boundaries can span one extra clock hour when a
	// 90-natural-day interval crosses the end of daylight-saving time. This
	// exception is deliberately narrow so unrelated or adversarial offsets do
	// not expand the query window.
	if elapsed > globalUsageMaximumRange+2*time.Hour {
		return false
	}
	fromDay, fromOffset, fromOK := dashboardLocalMidnight(fromValue)
	untilDay, untilOffset, untilOK := dashboardLocalMidnight(untilValue)
	offsetDifference := fromOffset - untilOffset
	if offsetDifference < 0 {
		offsetDifference = -offsetDifference
	}
	return fromOK && untilOK && offsetDifference <= 2*60*60 &&
		fromDay.Before(untilDay) && untilDay.Sub(fromDay) <= globalUsageMaximumRange
}

func dashboardLocalMidnight(value string) (time.Time, int, bool) {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, 0, false
	}
	if parsed.Hour() != 0 || parsed.Minute() != 0 || parsed.Second() != 0 || parsed.Nanosecond() != 0 {
		return time.Time{}, 0, false
	}
	_, offset := parsed.Zone()
	day := time.Date(parsed.Year(), parsed.Month(), parsed.Day(), 0, 0, 0, 0, time.UTC)
	return day, offset, true
}

func summarizeGlobalUsage(rows []store.GlobalUsageRow, pricing config.UsagePricing) (globalUsageResponse, error) {
	rate, err := parseDecimal(pricing.USDCNYRate)
	if err != nil || rate.Sign() <= 0 {
		return globalUsageResponse{}, errors.New("invalid USD/CNY pricing rate")
	}

	usersByID := make(map[string]*userUsageAccumulator)
	unpricedModels := make(map[string]struct{})
	total := newUsageAccumulator()
	for _, row := range rows {
		user := usersByID[row.UserID]
		if user == nil {
			user = &userUsageAccumulator{
				id:          row.UserID,
				username:    row.Username,
				displayName: row.DisplayName,
				usage:       newUsageAccumulator(),
			}
			usersByID[row.UserID] = user
		}
		if user.username != row.Username || user.displayName != row.DisplayName {
			return globalUsageResponse{}, fmt.Errorf("inconsistent user metadata for %q", row.UserID)
		}
		if err := addUsageRow(&user.usage, row, unpricedModels); err != nil {
			return globalUsageResponse{}, err
		}
		if err := addUsageRow(&total, row, nil); err != nil {
			return globalUsageResponse{}, err
		}
	}

	users := make([]globalUserUsage, 0, len(usersByID))
	userExactUSD := make(map[string]*big.Rat, len(usersByID))
	for _, accumulator := range usersByID {
		usage := finalizeUsage(accumulator.usage, rate)
		users = append(users, globalUserUsage{
			ID:              accumulator.id,
			Username:        accumulator.username,
			DisplayName:     accumulator.displayName,
			Usage:           usage,
			Share:           ratioString(usage.Tokens, total.usage.Tokens),
			PricingCoverage: ratioString(usage.PricedTokens, usage.Tokens),
			PricingStatus:   pricingStatus(usage),
		})
		userExactUSD[accumulator.id] = new(big.Rat).Set(accumulator.usage.actualUSD)
	}
	sort.Slice(users, func(i, j int) bool {
		comparison := userExactUSD[users[i].ID].Cmp(userExactUSD[users[j].ID])
		if comparison != 0 {
			return comparison > 0
		}
		if users[i].Username != users[j].Username {
			return users[i].Username < users[j].Username
		}
		return users[i].ID < users[j].ID
	})

	models := make([]string, 0, len(unpricedModels))
	for model := range unpricedModels {
		models = append(models, model)
	}
	sort.Strings(models)
	totalUsage := finalizeUsage(total, rate)
	// The public contract uses fixed six-place decimal amounts. Summing the
	// already-rounded per-user amounts keeps the summary exactly reconcilable
	// with the rows, including half-micro rounding boundaries.
	totalUsage.EstimatedUSD, err = sumUserAmounts(users, func(usage pricedUsage) string {
		return usage.EstimatedUSD
	})
	if err != nil {
		return globalUsageResponse{}, err
	}
	totalUsage.EstimatedCNY, err = sumUserAmounts(users, func(usage pricedUsage) string {
		return usage.EstimatedCNY
	})
	if err != nil {
		return globalUsageResponse{}, err
	}
	totalUsage.ActualCostUSD = totalUsage.EstimatedUSD
	totalUsage.ChargedUSD, err = sumUserAmounts(users, func(usage pricedUsage) string {
		return usage.ChargedUSD
	})
	if err != nil {
		return globalUsageResponse{}, err
	}
	totalUsage.UncoveredUSD, err = sumUserAmounts(users, func(usage pricedUsage) string {
		return usage.UncoveredUSD
	})
	if err != nil {
		return globalUsageResponse{}, err
	}
	return globalUsageResponse{
		Summary: globalUsageSummary{
			Usage:           totalUsage,
			ActiveUsers:     activeUsers(users),
			TotalUsers:      len(users),
			PricingCoverage: ratioString(totalUsage.PricedTokens, totalUsage.Tokens),
		},
		Users: users,
		Pricing: globalPricingMeta{
			CatalogAsOf:    pricing.CatalogAsOf,
			FXAsOf:         pricing.FXAsOf,
			USDCNYRate:     pricing.USDCNYRate,
			UnpricedModels: models,
			Disclaimer:     "OpenAI API Token 等价成本，不代表 OpenAI 实际账单；USD 金额来自不可变用量 ledger，包含已应用的服务层、上下文档位、缓存读取与缓存写入规则。ChatGPT Pro 订阅、工具、区域、税费和基础设施不在本报表范围内。",
		},
	}, nil
}

func newUsageAccumulator() usageAccumulator {
	return usageAccumulator{
		actualUSD: new(big.Rat), chargedUSD: new(big.Rat), uncoveredUSD: new(big.Rat),
	}
}

func addUsageRow(
	destination *usageAccumulator,
	row store.GlobalUsageRow,
	unpricedModels map[string]struct{},
) error {
	if row.RequestCount < 0 || row.InputTokens < 0 || row.CachedInputTokens < 0 ||
		row.CacheWriteTokens < 0 || row.OutputTokens < 0 || row.ReasoningTokens < 0 ||
		row.CachedInputTokens > row.InputTokens ||
		row.CacheWriteTokens > row.InputTokens-row.CachedInputTokens || row.LedgerTokens < 0 {
		return errors.New("invalid global usage metrics")
	}
	if err := addUsageMetric(&destination.usage.Requests, row.RequestCount); err != nil {
		return err
	}
	if err := addUsageMetric(&destination.usage.InputTokens, row.InputTokens); err != nil {
		return err
	}
	if err := addUsageMetric(&destination.usage.CachedInputTokens, row.CachedInputTokens); err != nil {
		return err
	}
	if err := addUsageMetric(&destination.usage.CacheWriteTokens, row.CacheWriteTokens); err != nil {
		return err
	}
	if err := addUsageMetric(&destination.usage.OutputTokens, row.OutputTokens); err != nil {
		return err
	}
	if err := addUsageMetric(&destination.usage.ReasoningTokens, row.ReasoningTokens); err != nil {
		return err
	}
	tokens := row.InputTokens
	if err := addUsageMetric(&tokens, row.OutputTokens); err != nil {
		return err
	}
	if err := addUsageMetric(&destination.usage.Tokens, tokens); err != nil {
		return err
	}

	pricedTokens := min(row.LedgerTokens, tokens)
	if err := addUsageMetric(&destination.usage.PricedTokens, pricedTokens); err != nil {
		return err
	}
	unpricedTokens := tokens - pricedTokens
	if err := addUsageMetric(&destination.usage.UnpricedTokens, unpricedTokens); err != nil {
		return err
	}
	if unpricedModels != nil && row.Model != "" && unpricedTokens > 0 {
		unpricedModels[row.Model] = struct{}{}
	}
	actual, err := parseLedgerDecimal(row.ActualCostUSD)
	if err != nil {
		return fmt.Errorf("parse ledger actual cost for model %q: %w", row.Model, err)
	}
	charged, err := parseLedgerDecimal(row.ChargedUSD)
	if err != nil {
		return fmt.Errorf("parse ledger charged cost for model %q: %w", row.Model, err)
	}
	uncovered, err := parseLedgerDecimal(row.UncoveredUSD)
	if err != nil {
		return fmt.Errorf("parse ledger uncovered cost for model %q: %w", row.Model, err)
	}
	if new(big.Rat).Add(new(big.Rat).Set(charged), uncovered).Cmp(actual) != 0 {
		return fmt.Errorf("ledger cost does not reconcile for model %q", row.Model)
	}
	destination.actualUSD.Add(destination.actualUSD, actual)
	destination.chargedUSD.Add(destination.chargedUSD, charged)
	destination.uncoveredUSD.Add(destination.uncoveredUSD, uncovered)
	return nil
}

func parseLedgerDecimal(value string) (*big.Rat, error) {
	if value == "" {
		value = "0"
	}
	return parseDecimal(value)
}

func addUsageMetric(destination *int64, value int64) error {
	const maximumInt64 = int64(^uint64(0) >> 1)
	if value < 0 || *destination < 0 || value > maximumInt64-*destination {
		return errors.New("global usage metrics overflow")
	}
	*destination += value
	return nil
}

func parseDecimal(value string) (*big.Rat, error) {
	if value == "" {
		return nil, errors.New("invalid non-negative decimal")
	}
	dot := -1
	for index := 0; index < len(value); index++ {
		switch character := value[index]; {
		case character >= '0' && character <= '9':
		case character == '.' && dot == -1:
			dot = index
		default:
			return nil, errors.New("invalid non-negative decimal")
		}
	}
	if dot == 0 || dot == len(value)-1 {
		return nil, errors.New("invalid non-negative decimal")
	}
	result, ok := new(big.Rat).SetString(value)
	if !ok || result.Sign() < 0 {
		return nil, errors.New("invalid non-negative decimal")
	}
	return result, nil
}

func finalizeUsage(accumulator usageAccumulator, rate *big.Rat) pricedUsage {
	usage := accumulator.usage
	usage.ActualCostUSD = accumulator.actualUSD.FloatString(moneyDecimalPlaces)
	usage.EstimatedUSD = usage.ActualCostUSD
	usage.ChargedUSD = accumulator.chargedUSD.FloatString(moneyDecimalPlaces)
	usage.UncoveredUSD = accumulator.uncoveredUSD.FloatString(moneyDecimalPlaces)
	cny := new(big.Rat).Mul(accumulator.actualUSD, rate)
	usage.EstimatedCNY = cny.FloatString(moneyDecimalPlaces)
	return usage
}

func sumUserAmounts(users []globalUserUsage, amount func(pricedUsage) string) (string, error) {
	total := new(big.Rat)
	for _, user := range users {
		value, err := parseDecimal(amount(user.Usage))
		if err != nil {
			return "", fmt.Errorf("sum user estimates: %w", err)
		}
		total.Add(total, value)
	}
	return total.FloatString(moneyDecimalPlaces), nil
}

func ratioString(value, total int64) string {
	if total == 0 {
		return "0.0000"
	}
	return new(big.Rat).Quo(big.NewRat(value, 1), big.NewRat(total, 1)).FloatString(4)
}

func pricingStatus(usage pricedUsage) string {
	if usage.Tokens == 0 && usage.ActualCostUSD == "0.000000" {
		return "no_usage"
	}
	if usage.UnpricedTokens == 0 {
		return "complete"
	}
	if usage.PricedTokens == 0 {
		return "unpriced"
	}
	return "partial"
}

func activeUsers(users []globalUserUsage) int {
	count := 0
	for _, user := range users {
		if user.Usage.Requests > 0 {
			count++
		}
	}
	return count
}
