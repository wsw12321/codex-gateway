package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/store"
)

func TestWriteModelPricingNotFound(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	writeModelPricingNotFound(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	var body httpx.ErrorBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if body.Error.Type != "invalid_request_error" || body.Error.Code != "model_pricing_not_found" ||
		body.Error.Message != "模型未记录计费价格，请联系管理员" {
		t.Fatalf("unexpected error response: %#v", body.Error)
	}
}

func TestWriteInsufficientQuota(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		retryAfter time.Duration
		wantHeader string
	}{
		{name: "without renewal", retryAfter: 0},
		{name: "round renewal up", retryAfter: 1500 * time.Millisecond, wantHeader: "2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			writeInsufficientQuota(recorder, request, test.retryAfter)

			if recorder.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
			}
			if got := recorder.Header().Get("Retry-After"); got != test.wantHeader {
				t.Fatalf("Retry-After = %q, want %q", got, test.wantHeader)
			}
			var body httpx.ErrorBody
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if body.Error.Type != "insufficient_quota" || body.Error.Code != "insufficient_quota" ||
				body.Error.Message != "可用额度不足" {
				t.Fatalf("unexpected error response: %#v", body.Error)
			}
		})
	}
}

func TestRetryUsageCompletionRetriesTransientFailure(t *testing.T) {
	t.Parallel()

	transient := errors.New("temporary database failure")
	attempts := 0
	err := retryUsageCompletion(context.Background(), 3, 0, func(context.Context) error {
		attempts++
		if attempts < 3 {
			return transient
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retryUsageCompletion: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestRetryUsageCompletionStopsOnDurableErrors(t *testing.T) {
	t.Parallel()

	for _, durable := range []error{store.ErrInvalid, store.ErrConflict, store.ErrNotFound} {
		attempts := 0
		err := retryUsageCompletion(context.Background(), 3, 0, func(context.Context) error {
			attempts++
			return durable
		})
		if !errors.Is(err, durable) {
			t.Fatalf("error = %v, want %v", err, durable)
		}
		if attempts != 1 {
			t.Fatalf("attempts = %d for %v, want 1", attempts, durable)
		}
	}
}
