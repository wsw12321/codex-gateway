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
	OutputTokens      int64  `json:"output_tokens"`
	ReasoningTokens   int64  `json:"reasoning_tokens"`
	PricedTokens      int64  `json:"priced_tokens"`
	UnpricedTokens    int64  `json:"unpriced_tokens"`
	EstimatedUSD      string `json:"estimated_usd"`
	EstimatedCNY      string `json:"estimated_cny"`
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
	Period  globalUsagePeriod  `json:"period"`
	Summary globalUsageSummary `json:"summary"`
	Users   []globalUserUsage  `json:"users"`
	Pricing globalPricingMeta  `json:"pricing"`
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
	usage pricedUsage
	usd   *big.Rat
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
	response, err := summarizeGlobalUsage(rows, s.config.UsagePricing)
	if err != nil {
		internalError(s, w, r, "price global usage", err)
		return
	}
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
		if err := addUsageRow(&user.usage, row, pricing.Models, unpricedModels); err != nil {
			return globalUsageResponse{}, err
		}
		if err := addUsageRow(&total, row, pricing.Models, nil); err != nil {
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
		userExactUSD[accumulator.id] = new(big.Rat).Set(accumulator.usage.usd)
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
			Disclaimer:     "API 等价费用估算，不代表实际账单；仅按配置的输入、缓存输入和输出 Token 比较价计算，不含 ChatGPT Pro 订阅费、税费、基础设施、工具、缓存写入、服务层级或区域附加项。历史用量按当前价格快照重新估算。",
		},
	}, nil
}

func newUsageAccumulator() usageAccumulator {
	return usageAccumulator{usd: new(big.Rat)}
}

func addUsageRow(
	destination *usageAccumulator,
	row store.GlobalUsageRow,
	prices map[string]config.ModelPricing,
	unpricedModels map[string]struct{},
) error {
	if row.RequestCount < 0 || row.InputTokens < 0 || row.CachedInputTokens < 0 ||
		row.OutputTokens < 0 || row.ReasoningTokens < 0 || row.CachedInputTokens > row.InputTokens {
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

	price, priced := prices[row.Model]
	if row.Model == "" || !priced {
		if err := addUsageMetric(&destination.usage.UnpricedTokens, tokens); err != nil {
			return err
		}
		if unpricedModels != nil && row.Model != "" && (row.RequestCount > 0 || tokens > 0) {
			unpricedModels[row.Model] = struct{}{}
		}
		return nil
	}
	usd, err := estimateUSD(row, price)
	if err != nil {
		return fmt.Errorf("price model %q: %w", row.Model, err)
	}
	if err := addUsageMetric(&destination.usage.PricedTokens, tokens); err != nil {
		return err
	}
	destination.usd.Add(destination.usd, usd)
	return nil
}

func addUsageMetric(destination *int64, value int64) error {
	const maximumInt64 = int64(^uint64(0) >> 1)
	if value < 0 || *destination < 0 || value > maximumInt64-*destination {
		return errors.New("global usage metrics overflow")
	}
	*destination += value
	return nil
}

func estimateUSD(row store.GlobalUsageRow, price config.ModelPricing) (*big.Rat, error) {
	if row.InputTokens < 0 || row.CachedInputTokens < 0 || row.OutputTokens < 0 ||
		row.CachedInputTokens > row.InputTokens {
		return nil, errors.New("invalid token metrics")
	}
	inputPrice, err := parseDecimal(price.InputUSDPerMillion)
	if err != nil {
		return nil, fmt.Errorf("invalid input price: %w", err)
	}
	cachedPrice, err := parseDecimal(price.CachedInputUSDPerMillion)
	if err != nil {
		return nil, fmt.Errorf("invalid cached input price: %w", err)
	}
	outputPrice, err := parseDecimal(price.OutputUSDPerMillion)
	if err != nil {
		return nil, fmt.Errorf("invalid output price: %w", err)
	}

	uncachedInput := row.InputTokens - row.CachedInputTokens
	value := new(big.Rat).Mul(big.NewRat(uncachedInput, 1), inputPrice)
	value.Add(value, new(big.Rat).Mul(big.NewRat(row.CachedInputTokens, 1), cachedPrice))
	value.Add(value, new(big.Rat).Mul(big.NewRat(row.OutputTokens, 1), outputPrice))
	return value.Quo(value, big.NewRat(1_000_000, 1)), nil
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
	usage.EstimatedUSD = accumulator.usd.FloatString(moneyDecimalPlaces)
	cny := new(big.Rat).Mul(accumulator.usd, rate)
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
	if usage.Tokens == 0 {
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
