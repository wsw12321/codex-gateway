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
	for _, native := range []bool{false, true} {
		for _, capable := range []bool{false, true} {
			calls := 0
			bridge := testRouterClient(true, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/internal/upstream-accounts/capabilities" {
					if capable {
						return routerTestResponse(200, `{"protocol":"upstream_account_access_v1"}`), nil
					}
					return routerTestResponse(404, `{}`), nil
				}
				calls++
				if r.Header.Get(gatewayUserHeader) != userID || r.Header.Get(affinityHeader) != "" || r.Header.Get("Authorization") != "Bearer bridge-secret" {
					t.Fatalf("incorrect bridge headers: %v", r.Header)
				}
				response := routerTestResponse(http.StatusTooManyRequests, `{"error":{"code":"upstream_concurrency_exceeded"}}`)
				response.Header.Set(upstreamAccountHeader, "0123456789abcdef")
				return response, nil
			})
			router := NewRouter(nil, bridge, map[string]string{antigravityPublicModel: "gemini-3.1-pro-high"})
			path, body := "/v1/responses", `{"model":"gemini-3.1-pro-preview","input":"hello"}`
			if native {
				path, body = "/v1beta/models/"+antigravityPublicModel+":generateContent", nativeGeminiRequest
			}
			r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			r.Header.Set(gatewayUserHeader, "spoofed")
			r.Header.Set(affinityHeader, "spoofed")
			options := ForwardOptions{UserID: userID, AffinityScope: strings.Repeat("a", 43)}
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
				t.Fatalf("old bridge did not fail closed: calls=%d failure=%v", calls, failure)
			}
		}
	}
}
