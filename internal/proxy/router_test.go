package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const antigravityPublicModel = "gemini-3.1-pro-preview"

func testRouterClient(bridge bool, fn roundTripFunc) *Client {
	base, _ := url.Parse("http://internal.test")
	client := NewWithHTTPClient(base, "codex-secret", &http.Client{Transport: fn})
	if bridge {
		client.antigravity = true
		client.token = "bridge-secret"
	}
	return client
}

func routerTestResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestRouterUsesExactModelsAndIndependentCredentials(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		model  string
		bridge bool
	}{
		{antigravityPublicModel, true},
		{"gemini-3.1-pro-preview-other", false},
		{"gemini-3.1-pro-high", false},
		{"gemini-other", false},
		{"gpt-6-astra", false},
	} {
		t.Run(test.model, func(t *testing.T) {
			body := `{"model":"` + test.model + `","input":"private prompt"}`
			var primaryCalls, bridgeCalls atomic.Int64
			transport := func(bridge bool) roundTripFunc {
				return func(request *http.Request) (*http.Response, error) {
					wantAuth := "Bearer codex-secret"
					wantAffinity := strings.Repeat("a", 43)
					if bridge {
						bridgeCalls.Add(1)
						wantAuth, wantAffinity = "Bearer bridge-secret", ""
					} else {
						primaryCalls.Add(1)
					}
					gotBody, err := io.ReadAll(request.Body)
					if err != nil || string(gotBody) != body {
						t.Errorf("request body changed: %q, %v", gotBody, err)
					}
					if request.Header.Get("Authorization") != wantAuth || request.Header.Get(affinityHeader) != wantAffinity || request.Header.Get("Cookie") != "" {
						t.Error("upstream received incorrect credentials, affinity, or client cookie")
					}
					return routerTestResponse(http.StatusOK, `{"model":"`+test.model+`","usage":{"input_tokens":2,"output_tokens":3}}`), nil
				}
			}
			routes := map[string]string{antigravityPublicModel: "gemini-3.1-pro-high"}
			router := NewRouter(testRouterClient(false, transport(false)), testRouterClient(true, transport(true)), routes)
			// The router owns a snapshot of configured routing.
			delete(routes, antigravityPublicModel)
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer public-key")
			request.Header.Set("Cookie", "session=client-cookie")
			request.Header.Set(affinityHeader, "spoofed-affinity")
			result, failure := router.ForwardWithOptions(context.Background(), httptest.NewRecorder(), request, test.model, "/v1/responses", ForwardOptions{AffinityScope: strings.Repeat("a", 43)})
			if failure != nil || result.Model != test.model || result.Usage.Total() != 5 {
				t.Fatalf("result = %+v, failure = %v", result, failure)
			}
			if got := bridgeCalls.Load(); (got == 1) != test.bridge || primaryCalls.Load()+got != 1 {
				t.Fatalf("wrong upstream: primary=%d bridge=%d", primaryCalls.Load(), got)
			}
		})
	}
}

func TestRouterUnavailableBridgeNeverFallsBack(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	primary := testRouterClient(false, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return routerTestResponse(http.StatusOK, `{}`), nil
	})
	broken := testRouterClient(true, func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})
	for _, bridge := range []*Client{nil, broken} {
		router := NewRouter(primary, bridge, map[string]string{antigravityPublicModel: "gemini-3.1-pro-high"})
		request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gemini-3.1-pro-preview","input":"hello"}`))
		recorder := httptest.NewRecorder()
		_, failure := router.ForwardWithOptions(context.Background(), recorder, request, antigravityPublicModel, "/v1/responses", ForwardOptions{})
		if failure == nil || failure.Status != http.StatusServiceUnavailable || failure.Code != "upstream_unavailable" || recorder.Body.Len() != 0 {
			t.Fatalf("unavailable bridge failure = %+v", failure)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("bridge requests fell back to the primary upstream")
	}
}

func TestRouterModelCatalogMergeAndOutage(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name         string
		primary      string
		bridge       string
		bridgeStatus int
		allowed      map[string]struct{}
		want         []string
		wantFailure  bool
	}{
		{name: "merge", primary: `{"object":"list","data":[{"id":"gpt-6-astra","owned_by":"codex"}]}`, bridge: `{"object":"list","data":[{"id":"gemini-3.1-pro-preview","owned_by":"antigravity"}]}`, allowed: map[string]struct{}{"gpt-6-astra": {}, antigravityPublicModel: {}}, want: []string{"gpt-6-astra", antigravityPublicModel}},
		{name: "same effective permission filter", primary: `{"data":[{"id":"gpt-6-astra"}]}`, bridge: `{"data":[{"id":"gemini-3.1-pro-preview"}]}`, allowed: map[string]struct{}{"gpt-6-astra": {}}, want: []string{"gpt-6-astra"}},
		{name: "bridge unavailable", primary: `{"data":[{"id":"gpt-6-astra"}]}`, bridgeStatus: http.StatusServiceUnavailable, allowed: map[string]struct{}{"gpt-6-astra": {}, antigravityPublicModel: {}}, want: []string{"gpt-6-astra"}},
		{name: "routed models hidden even if primary advertises during outage", primary: `{"data":[{"id":"gpt-6-astra"},{"id":"gemini-3.1-pro-preview"}]}`, bridgeStatus: http.StatusServiceUnavailable, allowed: map[string]struct{}{"gpt-6-astra": {}, antigravityPublicModel: {}}, want: []string{"gpt-6-astra"}},
		{name: "unrouted bridge models excluded", primary: `{"data":[{"id":"gpt-6-astra"}]}`, bridge: `{"data":[{"id":"gemini-unconfigured"},{"id":"gemini-3.1-pro-preview"}]}`, allowed: map[string]struct{}{"gpt-6-astra": {}, antigravityPublicModel: {}, "gemini-unconfigured": {}}, want: []string{"gpt-6-astra", antigravityPublicModel}},
		{name: "duplicate across upstreams", primary: `{"data":[{"id":"gemini-3.1-pro-preview"}]}`, bridge: `{"data":[{"id":"gemini-3.1-pro-preview"}]}`, wantFailure: true},
		{name: "duplicate hidden models across upstreams", primary: `{"data":[{"id":"gpt-hidden"}]}`, bridge: `{"data":[{"id":"gpt-hidden"}]}`, allowed: map[string]struct{}{antigravityPublicModel: {}}, wantFailure: true},
		{name: "duplicate in primary", primary: `{"data":[{"id":"gpt-6-astra"},{"id":"gpt-6-astra"}]}`, bridge: `{"data":[]}`, wantFailure: true},
		{name: "duplicate in bridge", primary: `{"data":[]}`, bridge: `{"data":[{"id":"gemini-3.1-pro-preview"},{"id":"gemini-3.1-pro-preview"}]}`, wantFailure: true},
		{name: "malformed bridge", primary: `{"data":[]}`, bridge: `{"data":[{"id":42}]}`, wantFailure: true},
		{name: "malformed primary", primary: `{"data":[`, bridge: `{"data":[]}`, wantFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			primary := testRouterClient(false, func(request *http.Request) (*http.Response, error) {
				if request.URL.Path != "/v1/models" || request.Header.Get("Authorization") != "Bearer codex-secret" {
					t.Error("invalid primary catalog request")
				}
				return routerTestResponse(http.StatusOK, test.primary), nil
			})
			bridge := testRouterClient(true, func(request *http.Request) (*http.Response, error) {
				if request.URL.Path != "/v1/models" || request.Header.Get("Authorization") != "Bearer bridge-secret" || request.Header.Get(affinityHeader) != "" {
					t.Error("invalid bridge catalog request")
				}
				status := test.bridgeStatus
				if status == 0 {
					status = http.StatusOK
				}
				return routerTestResponse(status, test.bridge), nil
			})
			router := NewRouter(primary, bridge, map[string]string{antigravityPublicModel: "gemini-3.1-pro-high"})
			recorder := httptest.NewRecorder()
			result, failure := router.ForwardModelsWithOptions(context.Background(), recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil), test.allowed, ForwardOptions{AffinityScope: strings.Repeat("a", 43)})
			if test.wantFailure {
				if failure == nil || failure.Status != http.StatusBadGateway || failure.Code != "upstream_invalid_model_catalog" || recorder.Body.Len() != 0 {
					t.Fatalf("invalid catalog result=%+v failure=%v body=%s", result, failure, recorder.Body)
				}
				return
			}
			if failure != nil || result.StatusCode != http.StatusOK || result.BytesOut != int64(recorder.Body.Len()) {
				t.Fatalf("result=%+v failure=%v", result, failure)
			}
			var catalog struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &catalog); err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(catalog.Data))
			for _, model := range catalog.Data {
				got = append(got, model.ID)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("models = %v, want %v", got, test.want)
			}
		})
	}
}

func TestRouterWithoutRoutesOnlyFetchesPrimaryCatalog(t *testing.T) {
	t.Parallel()
	primary := testRouterClient(false, func(*http.Request) (*http.Response, error) {
		return routerTestResponse(http.StatusOK, `{"data":[]}`), nil
	})
	bridge := testRouterClient(true, func(*http.Request) (*http.Response, error) {
		t.Error("unconfigured bridge catalog requested")
		return nil, errors.New("unconfigured")
	})
	router := NewRouter(primary, bridge, nil)
	_, failure := router.ForwardModelsWithOptions(context.Background(), httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil), nil, ForwardOptions{})
	if failure != nil {
		t.Fatal(failure)
	}
}

func TestAntigravityClientTimeoutAndErrorMapping(t *testing.T) {
	t.Parallel()
	base, _ := url.Parse("http://antigravity-bridge:8318")
	client := NewAntigravity(base, "bridge-secret")
	transport := client.http.Transport.(*http.Transport)
	if transport.Proxy != nil || transport.ResponseHeaderTimeout < 5*time.Minute+15*time.Second {
		t.Fatal("bridge needs direct internal traffic and a timeout longer than the CLI timeout")
	}
	for _, test := range []struct {
		status     int
		code       string
		wantStatus int
		wantCode   string
	}{
		{400, "antigravity_tools_unsupported", 400, "antigravity_tools_unsupported"},
		{400, "antigravity_invalid_request", 400, "antigravity_invalid_request"},
		{400, "secret_token", 502, "upstream_protocol_error"},
		{401, "anything", 503, "upstream_reauthentication_required"},
		{403, "anything", 503, "upstream_reauthentication_required"},
		{429, "upstream_concurrency_exceeded", 429, "upstream_concurrency_exceeded"},
		{429, "upstream_rate_limited", 429, "upstream_rate_limited"},
		{503, "upstream_unavailable", 503, "upstream_unavailable"},
		{503, "upstream_reauthentication_required", 503, "upstream_reauthentication_required"},
		{504, "upstream_timeout", 504, "upstream_timeout"},
		{502, "upstream_protocol_error", 502, "upstream_protocol_error"},
		{502, "upstream_process_error", 502, "upstream_process_error"},
		{500, "anything", 502, "upstream_protocol_error"},
	} {
		response := routerTestResponse(test.status, `{"error":{"code":"`+test.code+`","message":"private prompt or credentials"}}`)
		response.Header.Set("Retry-After", "2")
		failure := client.sanitizeFailure(response)
		if failure.Status != test.wantStatus || failure.Code != test.wantCode || strings.Contains(failure.Message, "private prompt") || strings.Contains(failure.Message, "credentials") {
			t.Fatalf("status=%d code=%s: failure=%+v", test.status, test.code, failure)
		}
	}
}

func TestAntigravityForwardCancellationWhileWaitingForResult(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	bridge := testRouterClient(true, func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	router := NewRouter(nil, bridge, map[string]string{antigravityPublicModel: "gemini-3.1-pro-high"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan *Failure, 1)
	go func() {
		_, failure := router.ForwardWithOptions(ctx, httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", nil), antigravityPublicModel, "/v1/responses", ForwardOptions{})
		done <- failure
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("bridge request did not start")
	}
	cancel()
	select {
	case failure := <-done:
		if failure == nil || failure.Code != "client_disconnected" || !errors.Is(failure, context.Canceled) {
			t.Fatalf("failure = %+v", failure)
		}
	case <-time.After(time.Second):
		t.Fatal("bridge request survived cancellation")
	}
}
