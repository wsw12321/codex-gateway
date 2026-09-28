package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestGeminiAdmissionRequiresBillingBeforeDatabaseWrites(t *testing.T) {
	for _, endpoint := range []string{"gemini.generateContent", "gemini.streamGenerateContent"} {
		t.Run(endpoint, func(t *testing.T) {
			at := time.Now().UTC()
			// A nil database makes an accidental transaction or write fail the test.
			repository := &Store{}
			_, err := repository.AdmitRequest(context.Background(), AdmitRequestParams{
				Quota: ReserveQuotaParams{RequestID: "request", UserID: "user", APIKeyID: "key", Now: at},
				Usage: BeginUsageRequestParams{RequestID: "request", UserID: "user", APIKeyID: "key",
					DeviceID: "device", Model: "gemini-3.1-pro-high", Endpoint: endpoint, RequestedAt: at},
			})
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "requires billing") {
				t.Fatalf("unbilled %s admission error = %v", endpoint, err)
			}
		})
	}
}
