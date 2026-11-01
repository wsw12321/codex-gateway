package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestGenerationRequiresAndForwardsGroupAccountScope(t *testing.T) {
	const userID = "00000000-0000-0000-0000-000000000001"
	const groupID = "00000000-0000-0000-0000-000000000002"
	for _, route := range []struct {
		name, provider, path string
	}{
		{"codex responses", "codex", "/v1/responses"},
		{"codex compact", "codex", "/v1/responses/compact"},
		{"antigravity responses", "antigravity", "/v1/responses"},
		{"antigravity native", "antigravity", "/v1beta/models/gemini-3.8-flash-high:generateContent"},
		{"antigravity native stream", "antigravity", "/v1beta/models/gemini-3.8-flash-high:streamGenerateContent"},
		{"legacy antigravity native", "legacy", "/v1beta/models/gemini-3.8-flash-high:generateContent"},
		{"anthropic messages", "anthropic", "/v1/messages"},
	} {
		for _, scope := range []struct {
			name, capability string
			groups           []AllocationGroup
			blocked          bool
		}{
			{"personal with old sidecar", "", nil, false},
			{"selected", GroupUpstreamAccountScopeV1, []AllocationGroup{{GroupID: groupID, AccountIDs: []string{"0123456789abcdef"}}}, false},
			{"selected empty", GroupUpstreamAccountScopeV1, []AllocationGroup{{GroupID: groupID, AccountIDs: []string{}}}, false},
			{"selected null", GroupUpstreamAccountScopeV1, []AllocationGroup{{GroupID: groupID}}, false},
			{"old sidecar", "", []AllocationGroup{{GroupID: groupID}}, true},
			{"wrong capability", "group_upstream_account_scope_v0", []AllocationGroup{{GroupID: groupID}}, true},
		} {
			t.Run(route.name+"/"+scope.name, func(t *testing.T) {
				base, _ := url.Parse("http://sidecar.invalid")
				client := New(base, "internal-test-key")
				switch route.provider {
				case "antigravity":
					client = NewCPAAntigravity(base, "internal-test-key")
				case "legacy":
					client = NewAntigravity(base, "internal-test-key")
				case "anthropic":
					client = NewCPAAnthropic(base, "internal-test-key")
				}
				dispatches, probes := 0, 0
				client.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.Header.Get("Authorization") != "Bearer internal-test-key" {
						t.Error("missing internal authentication")
					}
					if strings.HasSuffix(r.URL.Path, "/capabilities") {
						probes++
						capabilities := map[string]string{
							"protocol": AccountAccessProtocolV1, "anthropic_messages": AnthropicMessagesProtocolV1,
						}
						if scope.capability != "" {
							capabilities["group_upstream_account_scope"] = scope.capability
						}
						body, _ := json.Marshal(capabilities)
						return routerTestResponse(http.StatusOK, string(body)), nil
					}
					dispatches++
					if r.URL.Path != route.path || r.Header.Get(gatewayUserHeader) != userID {
						t.Errorf("wrong dispatch path or user: %s", r.URL.Path)
					}
					raw := r.Header.Get(gatewayGroupsHeader)
					if len(scope.groups) == 0 {
						if raw != "" {
							t.Error("client-supplied group scope was forwarded")
						}
					} else {
						var got []AllocationGroup
						if err := json.Unmarshal([]byte(raw), &got); err != nil || !reflect.DeepEqual(got, scope.groups) {
							t.Errorf("group scope = %q; want %+v", raw, scope.groups)
						}
					}
					// Stop before provider response parsing; this test covers the
					// authorization contract at the dispatch boundary.
					return routerTestResponse(http.StatusTooManyRequests, `{"type":"error","error":{"type":"rate_limit_error","message":"busy"}}`), nil
				})
				body := `{"contents":[],"messages":[]}`
				if route.provider == "antigravity" && route.path == "/v1/responses" {
					body = `{"model":"gemini-3.8-flash-high","input":"hello"}`
				}
				incoming := httptest.NewRequest(http.MethodPost, route.path, strings.NewReader(body))
				incoming.Header.Set(gatewayGroupsHeader, `[{"group_id":"forged","account_ids":["ffffffffffffffff"]}]`)
				options := ForwardOptions{UserID: userID, Groups: scope.groups}
				var result Result
				var failure *Failure
				switch {
				case route.provider == "anthropic":
					result, failure = client.ForwardMessages(context.Background(), httptest.NewRecorder(), incoming, "claude-test", route.path, options)
				case strings.HasPrefix(route.path, "/v1beta/"):
					result, failure = client.forwardGemini(context.Background(), httptest.NewRecorder(), incoming, "gemini-3.8-flash-high", route.path, options)
				default:
					result, failure = client.ForwardWithOptions(context.Background(), httptest.NewRecorder(), incoming, route.path, options)
				}
				if probes != 1 {
					t.Fatalf("capability probes = %d; want 1", probes)
				}
				if scope.blocked {
					if dispatches != 0 || failure == nil || failure.Code != "upstream_access_protocol_unavailable" {
						t.Fatalf("unsupported sidecar received generation: dispatches=%d failure=%v", dispatches, failure)
					}
				} else if dispatches != 1 || failure == nil || result.StatusCode != http.StatusTooManyRequests {
					t.Fatalf("dispatches=%d failure=%v", dispatches, failure)
				}
			})
		}
	}
}

func TestGenerationRejectsGroupAccountScopeWithoutIdentity(t *testing.T) {
	for _, provider := range []string{"codex", "antigravity", "legacy", "anthropic"} {
		for _, userID := range []string{"", "forged-user"} {
			t.Run(provider+"/"+userID, func(t *testing.T) {
				base, _ := url.Parse("http://sidecar.invalid")
				client := New(base, "internal-test-key")
				path := "/v1/responses"
				switch provider {
				case "antigravity":
					client = NewCPAAntigravity(base, "internal-test-key")
					path = "/v1beta/models/gemini-3.8-flash-high:generateContent"
				case "legacy":
					client = NewAntigravity(base, "internal-test-key")
					path = "/v1beta/models/gemini-3.8-flash-high:generateContent"
				case "anthropic":
					client = NewCPAAnthropic(base, "internal-test-key")
					path = "/v1/messages"
				}
				requests := 0
				client.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					requests++
					return routerTestResponse(http.StatusOK, `{}`), nil
				})
				incoming := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"contents":[]}`))
				options := ForwardOptions{UserID: userID, Groups: []AllocationGroup{{GroupID: "00000000-0000-0000-0000-000000000002"}}}
				var failure *Failure
				switch provider {
				case "anthropic":
					_, failure = client.ForwardMessages(context.Background(), httptest.NewRecorder(), incoming, "claude-test", path, options)
				case "antigravity", "legacy":
					_, failure = client.forwardGemini(context.Background(), httptest.NewRecorder(), incoming, "gemini-3.8-flash-high", path, options)
				default:
					_, failure = client.ForwardWithOptions(context.Background(), httptest.NewRecorder(), incoming, path, options)
				}
				if failure == nil || requests != 0 {
					t.Fatalf("unidentified group request dispatched: requests=%d failure=%v", requests, failure)
				}
			})
		}
	}
}
