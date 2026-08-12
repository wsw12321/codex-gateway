package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/store"
)

func TestDecodeBillingWritePreservesExactAmountStrings(t *testing.T) {
	t.Parallel()

	const body = `{"operation_id":"550e8400-e29b-41d4-a716-446655440000","reason":" invoice checked ","cny_amount":"123456789012345678.123456"}`
	request := httptest.NewRequest(http.MethodPost, "/admin/billing/users/user/recharges", strings.NewReader(body))
	response := httptest.NewRecorder()
	var input rechargeUserInput
	server := &Server{}
	if !server.decodeBillingWrite(response, request, &input) {
		t.Fatalf("decode failed: status=%d body=%s", response.Code, response.Body.String())
	}
	if input.CNYAmount != "123456789012345678.123456" {
		t.Fatalf("cny amount = %q", input.CNYAmount)
	}
	if input.Reason != " invoice checked " {
		t.Fatalf("reason was unexpectedly rewritten: %q", input.Reason)
	}
}

func TestDecodeBillingWriteRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	const body = `{"operation_id":"550e8400-e29b-41d4-a716-446655440000","reason":"required","usd_amount":"1","unexpected":true}`
	request := httptest.NewRequest(http.MethodPost, "/admin/billing/users/user/adjustments", strings.NewReader(body))
	response := httptest.NewRecorder()
	var input adjustmentInput
	server := &Server{}
	if server.decodeBillingWrite(response, request, &input) {
		t.Fatal("unknown field was accepted")
	}
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_json") {
		t.Fatalf("unexpected response: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestBillingStoreErrorMapsQuotaAndIdempotencyFailures(t *testing.T) {
	t.Parallel()

	server := &Server{}
	for _, test := range []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "negative adjustment", err: &store.InsufficientFundsError{}, wantStatus: http.StatusBadRequest, wantCode: "insufficient_balance"},
		{name: "operation replay mismatch", err: store.ErrConflict, wantStatus: http.StatusConflict, wantCode: "billing_operation_conflict"},
		{name: "invalid amount", err: store.ErrInvalid, wantStatus: http.StatusBadRequest, wantCode: "invalid_billing_operation"},
		{name: "missing user", err: store.ErrNotFound, wantStatus: http.StatusNotFound, wantCode: "billing_resource_not_found"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/admin/billing", nil)
			response := httptest.NewRecorder()
			server.billingStoreError(response, request, "test billing", test.err)
			if response.Code != test.wantStatus || !strings.Contains(response.Body.String(), `"code":"`+test.wantCode+`"`) {
				t.Fatalf("unexpected response: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestValidateBillingOperation(t *testing.T) {
	t.Parallel()

	const operationID = "550e8400-e29b-41d4-a716-446655440000"
	for _, test := range []struct {
		name        string
		operationID string
		reason      string
		wantError   bool
	}{
		{name: "valid", operationID: operationID, reason: "invoice 2026-08-12"},
		{name: "unicode reason", operationID: operationID, reason: "充值单据已核对"},
		{name: "maximum unicode reason", operationID: operationID, reason: strings.Repeat("界", 500)},
		{name: "missing operation id", reason: "required", wantError: true},
		{name: "malformed operation id", operationID: "not-a-uuid", reason: "required", wantError: true},
		{name: "operation id with surrounding whitespace", operationID: " " + operationID, reason: "required", wantError: true},
		{name: "uppercase operation id", operationID: strings.ToUpper(operationID), reason: "required", wantError: true},
		{name: "noncanonical operation id", operationID: strings.ReplaceAll(operationID, "-", ""), reason: "required", wantError: true},
		{name: "missing reason", operationID: operationID, wantError: true},
		{name: "blank reason", operationID: operationID, reason: " \t\n", wantError: true},
		{name: "reason too long", operationID: operationID, reason: strings.Repeat("界", 501), wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateBillingOperation(test.operationID, test.reason)
			if test.wantError && err == nil {
				t.Fatal("validation unexpectedly succeeded")
			}
			if !test.wantError && err != nil {
				t.Fatalf("validation failed: %v", err)
			}
		})
	}
}

func TestBillingStateResponseKeepsMoneyAsJSONStrings(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 12, 12, 0, 0, 0, time.UTC)
	state := store.BillingState{
		UserID: "user-id", BalanceUSD: "12.340000000000", LedgerTotal: 1,
		Subscriptions: []store.BillingSubscriptionState{{
			Tier: "day", Enabled: true, AllowanceUSD: "5.000000000000",
			RemainingUSD: "4.500000000000", PeriodStartsAt: &now, PeriodEndsAt: &now,
		}},
		Ledger: []store.BillingLedgerEntry{{
			ID: 1, EntryType: "recharge", AmountUSD: "12.340000000000",
			CashDeltaUSD: "12.340000000000", CreatedAt: now,
		}},
	}
	raw, err := json.Marshal(billingStateResponse(state, 50, 0))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, expected := range []string{
		`"cash_balance_usd":"12.340000000000"`,
		`"quota_usd":"5.000000000000"`,
		`"remaining_usd":"4.500000000000"`,
		`"amount_usd":"12.340000000000"`,
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("billing response lost exact string %s: %s", expected, text)
		}
	}
}

func TestParseBillingTier(t *testing.T) {
	t.Parallel()

	for _, tier := range []string{"day", "week", "month"} {
		tier := tier
		t.Run(tier, func(t *testing.T) {
			got, err := parseBillingTier(tier)
			if err != nil {
				t.Fatal(err)
			}
			if got != tier {
				t.Fatalf("tier = %q, want %q", got, tier)
			}
		})
	}

	for _, tier := range []string{"", "Day", "daily", " month ", "month/../day"} {
		tier := tier
		t.Run("reject_"+tier, func(t *testing.T) {
			if _, err := parseBillingTier(tier); err == nil {
				t.Fatalf("accepted invalid tier %q", tier)
			}
		})
	}
}

func TestParseBillingPagination(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		values     url.Values
		wantLimit  int
		wantOffset int
	}{
		{name: "defaults", values: url.Values{}, wantLimit: 50},
		{name: "explicit", values: url.Values{"limit": {"25"}, "offset": {"75"}}, wantLimit: 25, wantOffset: 75},
		{name: "bounds", values: url.Values{"limit": {"100"}, "offset": {"0"}}, wantLimit: 100},
	} {
		t.Run(test.name, func(t *testing.T) {
			limit, offset, err := parseBillingPagination(test.values)
			if err != nil {
				t.Fatal(err)
			}
			if limit != test.wantLimit || offset != test.wantOffset {
				t.Fatalf("pagination = (%d, %d), want (%d, %d)", limit, offset, test.wantLimit, test.wantOffset)
			}
		})
	}
}

func TestParseBillingPaginationRejectsInvalidValues(t *testing.T) {
	t.Parallel()

	for name, values := range map[string]url.Values{
		"zero limit":        {"limit": {"0"}},
		"limit over max":    {"limit": {"101"}},
		"negative offset":   {"offset": {"-1"}},
		"empty limit":       {"limit": {""}},
		"non decimal":       {"offset": {"1.5"}},
		"signed integer":    {"limit": {"+1"}},
		"surrounding space": {"offset": {" 1"}},
		"duplicate limit":   {"limit": {"10", "20"}},
		"duplicate offset":  {"offset": {"0", "1"}},
	} {
		name, values := name, values
		t.Run(name, func(t *testing.T) {
			if _, _, err := parseBillingPagination(values); err == nil {
				t.Fatalf("accepted invalid pagination: %v", values)
			}
		})
	}
}
