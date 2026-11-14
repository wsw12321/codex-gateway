package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRouterForwardsAttributionCallbackToSelectedUpstream(t *testing.T) {
	const account = "0123456789abcdef"
	for _, test := range []struct {
		name        string
		model       string
		antigravity bool
	}{
		{name: "primary", model: "gpt-6-astra"},
		{name: "antigravity", model: antigravityPublicModel, antigravity: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := "codex"
			if test.antigravity {
				provider = "antigravity"
			}
			ledger := NewAllocationLedger()
			client := testRouterClient(test.antigravity, func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/capabilities") {
					return routerTestResponse(http.StatusOK, `{"protocol":"`+AccountAccessProtocolV1+`"}`), nil
				}
				reportAllocation(t, ledger, provider, r, account, "")
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"model":"` + test.model + `","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)),
				}, nil
			})
			client.UseAllocationLedger(ledger)
			var primary, antigravity *Client
			if test.antigravity {
				primary = testRouterClient(false, func(*http.Request) (*http.Response, error) {
					t.Fatal("primary client selected for an Antigravity model")
					return nil, nil
				})
				antigravity = client
			} else {
				primary = client
			}
			router := NewRouter(primary, antigravity, map[string]string{antigravityPublicModel: antigravityPublicModel})
			var got string
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"`+test.model+`","input":"hello"}`))
			result, failure := router.ForwardWithOptions(context.Background(), httptest.NewRecorder(), request, test.model, "/v1/responses", ForwardOptions{
				UserID:            ledgerUser,
				OnUpstreamAccount: func(accountID string) { got = accountID },
			})
			if failure != nil || result.UpstreamAccountID != account || got != account {
				t.Fatalf("result=%+v failure=%v callback=%q", result, failure, got)
			}
		})
	}
}
