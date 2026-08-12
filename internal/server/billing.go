package server

import (
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
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
	case "day", "week", "month":
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
