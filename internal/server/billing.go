package server

import (
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/wsw/codex-gateway/internal/config"
)

var (
	errInvalidBillingOperation = errors.New("invalid billing operation")
	unsignedIntegerPattern     = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
)

func validateBillingOperation(operationID, reason string) error {
	parsed, err := uuid.Parse(operationID)
	if err != nil || parsed.String() != operationID {
		return errInvalidBillingOperation
	}
	reason = strings.TrimSpace(reason)
	if reason == "" || utf8.RuneCountInString(reason) > 500 {
		return errInvalidBillingOperation
	}
	return nil
}

func parseBillingTier(value string) (string, error) {
	switch value {
	case "day", "week", "month", "custom":
		return value, nil
	default:
		return "", errInvalidBillingOperation
	}
}

func parseBillingPagination(values url.Values) (int, int, error) {
	limit, offset := 50, 0
	var err error
	if raw, exists := values["limit"]; exists {
		if len(raw) != 1 {
			return 0, 0, errInvalidBillingOperation
		}
		limit, err = parseBillingPageInteger(raw[0])
		if err != nil || limit < 1 || limit > 100 {
			return 0, 0, errInvalidBillingOperation
		}
	}
	if raw, exists := values["offset"]; exists {
		if len(raw) != 1 {
			return 0, 0, errInvalidBillingOperation
		}
		offset, err = parseBillingPageInteger(raw[0])
		if err != nil || offset < 0 {
			return 0, 0, errInvalidBillingOperation
		}
	}
	return limit, offset, nil
}

func parseBillingPageInteger(value string) (int, error) {
	if !unsignedIntegerPattern.MatchString(value) {
		return 0, errInvalidBillingOperation
	}
	parsed, err := strconv.ParseInt(value, 10, 31)
	if err != nil {
		return 0, errInvalidBillingOperation
	}
	return int(parsed), nil
}

// Path IDs never accept retired tier names, so old clients cannot mutate an
// arbitrary subscription after the user gains a second subscription.
func validBillingResourceID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == value
}

func validBillingPeriod(tier string, days int) bool {
	switch tier {
	case "day":
		return days == 0 || days == 1
	case "week":
		return days == 0 || days == 7
	case "month":
		return days == 0 || days == 31
	case "custom":
		return days >= 1 && days <= 3650
	default:
		return false
	}
}

// Validate only the immutable request shape here. The store validates current
// catalog membership after checking operation replay, so a later catalog edit
// cannot invalidate an already committed financial response.
func (s *Server) validBillingModels(models []string) bool {
	if len(models) == 0 || len(models) > 1000 {
		return false
	}
	seen := make(map[string]bool, len(models))
	for _, model := range models {
		if seen[model] || !validModel(model) || model == config.InternalGovernanceModel {
			return false
		}
		seen[model] = true
	}
	return true
}

// validManageableBillingModels applies the current configured catalog to
// model-scope edits. Subscription writes retain the looser shape check above
// so the store can preserve an already committed operation replay after a
// catalog change; new cash and group scopes must always name currently
// manageable models.
func (s *Server) validManageableBillingModels(models []string) bool {
	if !s.validBillingModels(models) {
		return false
	}
	for _, model := range models {
		if !s.config.UsagePricing.IsManageableModel(model) {
			return false
		}
	}
	return true
}
