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
		name   string
		model  string
		bridge bool
	}{
		{name: "primary", model: "gpt-6-astra"},
		{name: "antigravity", model: antigravityPublicModel, bridge: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := testRouterClient(test.bridge, func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header: http.Header{
						"Content-Type":        {"application/json"},
						upstreamAccountHeader: {account},
					},
					Body: io.NopCloser(strings.NewReader(`{"model":"` + test.model + `"}`)),
				}, nil
			})
			var primary, bridge *Client
			if test.bridge {
				primary = testRouterClient(false, func(*http.Request) (*http.Response, error) {
					t.Fatal("primary upstream selected for bridge model")
					return nil, nil
				})
				bridge = client
			} else {
				primary = client
				bridge = nil
			}
			router := NewRouter(primary, bridge, map[string]string{antigravityPublicModel: "gemini-3.1-pro-high"})
			var got string
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
			result, failure := router.ForwardWithOptions(context.Background(), httptest.NewRecorder(), request, test.model, "/v1/responses", ForwardOptions{
				OnUpstreamAccount: func(accountID string) { got = accountID },
			})
			if failure != nil || result.UpstreamAccountID != account || got != account {
				t.Fatalf("result=%+v failure=%v callback=%q", result, failure, got)
			}
		})
	}
}
