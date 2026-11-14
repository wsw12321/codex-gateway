package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAntigravityDispatchRequiresAccountAccessAndTrustedUser(t *testing.T) {
	const userID = "00000000-0000-0000-0000-000000000001"
	scope := strings.Repeat("a", 43)
	for _, native := range []bool{false, true} {
		for _, capable := range []bool{false, true} {
			calls := 0
			ledger := NewAllocationLedger()
			antigravity := testRouterClient(true, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == pluginPath("antigravity-accounts/capabilities") {
					if capable {
						return routerTestResponse(200, `{"protocol":"upstream_account_access_v1"}`), nil
					}
					return routerTestResponse(404, `{}`), nil
				}
				calls++
				if r.Header.Get(gatewayUserHeader) != userID || r.Header.Get(affinityHeader) != scope || r.Header.Get("Authorization") != "Bearer antigravity-secret" {
					t.Fatalf("incorrect dispatch headers: %v", r.Header)
				}
				reportAllocation(t, ledger, "antigravity", r, "0123456789abcdef", "")
				return routerTestResponse(http.StatusTooManyRequests, `{"error":{"code":"upstream_concurrency_exceeded"}}`), nil
			})
			antigravity.UseAllocationLedger(ledger)
			router := NewRouter(nil, antigravity, map[string]string{antigravityPublicModel: antigravityPublicModel})
			path, body := "/v1/responses", `{"model":"`+antigravityPublicModel+`","input":"hello"}`
			if native {
				path, body = "/v1beta/models/"+antigravityPublicModel+":generateContent", nativeGeminiRequest
			}
			r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			r.Header.Set(gatewayUserHeader, "spoofed")
			r.Header.Set(affinityHeader, "spoofed")
			options := ForwardOptions{UserID: userID, AffinityScope: scope}
			var result Result
			var failure *Failure
			if native {
				result, failure = router.ForwardGemini(context.Background(), httptest.NewRecorder(), r, antigravityPublicModel, path, options)
			} else {
				result, failure = router.ForwardWithOptions(context.Background(), httptest.NewRecorder(), r, antigravityPublicModel, path, options)
			}
			if capable {
				if calls != 1 || failure == nil || failure.Code != "upstream_concurrency_exceeded" || result.UpstreamAccountID != "0123456789abcdef" {
					t.Fatalf("dispatch calls=%d result=%+v failure=%v", calls, result, failure)
				}
			} else if calls != 0 || failure == nil || failure.Code != "upstream_access_protocol_unavailable" {
				t.Fatalf("CPA without the access protocol did not fail closed: calls=%d failure=%v", calls, failure)
			}
		}
	}
}
