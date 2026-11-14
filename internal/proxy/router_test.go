package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const antigravityPublicModel = "gemini-3.8-flash-high"

// testRouterClient builds a CPA client over a fake transport. The Antigravity
// client gets its own token here so a test can tell which client was used; in
// a deployment both use the one internal key.
func testRouterClient(antigravity bool, fn roundTripFunc) *Client {
	base, _ := url.Parse("http://internal.test")
	client := withManagement(NewWithHTTPClient(base, "codex-secret", &http.Client{Transport: fn}))
	if antigravity {
		client.antigravity = true
		client.token = "antigravity-secret"
	}
	return client
}

func routerTestResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestRouterUsesExactModelsAndTheMatchingClient(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		model       string
		antigravity bool
	}{
		{antigravityPublicModel, true},
		{"gemini-3.8-flash-high-other", false},
		{"gemini-3.8-flash", false},
		{"gemini-other", false},
		{"gpt-6-astra", false},
	} {
		t.Run(test.model, func(t *testing.T) {
			body := `{"model":"` + test.model + `","input":"private prompt"}`
			scope := strings.Repeat("a", 43)
			var primaryCalls, antigravityCalls atomic.Int64
			transport := func(antigravity bool) roundTripFunc {
				return func(request *http.Request) (*http.Response, error) {
					wantAuth := "Bearer codex-secret"
					if antigravity {
						antigravityCalls.Add(1)
						wantAuth = "Bearer antigravity-secret"
					} else {
						primaryCalls.Add(1)
					}
					gotBody, err := io.ReadAll(request.Body)
					if err != nil || string(gotBody) != body {
						t.Errorf("request body changed: %q, %v", gotBody, err)
					}
					// The caller scope is the Gateway's, never the client's.
					if request.Header.Get("Authorization") != wantAuth || request.Header.Get(affinityHeader) != scope || request.Header.Get("Cookie") != "" {
						t.Error("upstream received incorrect credentials, affinity, or client cookie")
					}
					return routerTestResponse(http.StatusOK, `{"model":"`+test.model+`","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`), nil
				}
			}
			routes := map[string]string{antigravityPublicModel: antigravityPublicModel}
			router := NewRouter(testRouterClient(false, transport(false)), testRouterClient(true, transport(true)), routes)
			// The router owns a snapshot of configured routing.
			delete(routes, antigravityPublicModel)
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer public-key")
			request.Header.Set("Cookie", "session=client-cookie")
			request.Header.Set(affinityHeader, "spoofed-affinity")
			result, failure := router.ForwardWithOptions(context.Background(), httptest.NewRecorder(), request, test.model, "/v1/responses", ForwardOptions{AffinityScope: scope})
			if failure != nil || result.Model != test.model || result.Usage.Total() != 5 {
				t.Fatalf("result = %+v, failure = %v", result, failure)
			}
			if got := antigravityCalls.Load(); (got == 1) != test.antigravity || primaryCalls.Load()+got != 1 {
				t.Fatalf("wrong client: primary=%d antigravity=%d", primaryCalls.Load(), got)
			}
		})
	}
}

func TestRouterUnavailableAntigravityNeverFallsBack(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	primary := testRouterClient(false, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return routerTestResponse(http.StatusOK, `{}`), nil
	})
	broken := testRouterClient(true, func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})
	for _, antigravity := range []*Client{nil, broken} {
		router := NewRouter(primary, antigravity, map[string]string{antigravityPublicModel: antigravityPublicModel})
		request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"`+antigravityPublicModel+`","input":"hello"}`))
		recorder := httptest.NewRecorder()
		_, failure := router.ForwardWithOptions(context.Background(), recorder, request, antigravityPublicModel, "/v1/responses", ForwardOptions{})
		if failure == nil || failure.Status != http.StatusServiceUnavailable || failure.Code != "upstream_unavailable" || recorder.Body.Len() != 0 {
			t.Fatalf("unavailable Antigravity failure = %+v", failure)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("Antigravity requests fell back to the primary client")
	}
}

func TestRouterRejectsAntigravityCompactBeforeDispatch(t *testing.T) {
	t.Parallel()
	unexpected := func(*http.Request) (*http.Response, error) {
		t.Error("compact request for an Antigravity model was dispatched")
		return nil, errors.New("unexpected request")
	}
	router := NewRouter(testRouterClient(false, unexpected), testRouterClient(true, unexpected), map[string]string{antigravityPublicModel: antigravityPublicModel})
	request := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(`{"model":"`+antigravityPublicModel+`","input":"hello"}`))
	_, failure := router.ForwardWithOptions(context.Background(), httptest.NewRecorder(), request, antigravityPublicModel, "/v1/responses/compact", ForwardOptions{})
	if failure == nil || failure.Status != http.StatusNotImplemented || failure.Code != "endpoint_not_supported" {
		t.Fatalf("failure = %+v", failure)
	}
}

// One CPA instance lists every provider's models, so the catalog is a single
// request through the primary client whatever routes are configured.
func TestRouterModelCatalogComesFromThePrimaryClientOnly(t *testing.T) {
	t.Parallel()
	for _, routes := range []map[string]string{nil, {antigravityPublicModel: antigravityPublicModel}} {
		var calls atomic.Int64
		primary := testRouterClient(false, func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			if request.URL.Path != "/v1/models" || request.Header.Get("Authorization") != "Bearer codex-secret" {
				t.Error("invalid catalog request")
			}
			return routerTestResponse(http.StatusOK, `{"object":"list","data":[{"id":"gpt-6-astra"},{"id":"`+antigravityPublicModel+`"},{"id":"gpt-hidden"}]}`), nil
		})
		antigravity := testRouterClient(true, func(*http.Request) (*http.Response, error) {
			t.Error("the Antigravity client fetched a catalog of its own")
			return nil, errors.New("unexpected request")
		})
		router := NewRouter(primary, antigravity, routes)
		recorder := httptest.NewRecorder()
		allowed := map[string]struct{}{"gpt-6-astra": {}, antigravityPublicModel: {}}
		result, failure := router.ForwardModelsWithOptions(context.Background(), recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil), allowed, ForwardOptions{})
		if failure != nil || result.StatusCode != http.StatusOK || calls.Load() != 1 {
			t.Fatalf("result=%+v failure=%v calls=%d", result, failure, calls.Load())
		}
		body := recorder.Body.String()
		if !strings.Contains(body, "gpt-6-astra") || !strings.Contains(body, antigravityPublicModel) || strings.Contains(body, "gpt-hidden") {
			t.Fatalf("catalog = %s", body)
		}
		if _, failure := router.ForwardModelsWithOptions(context.Background(), httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/models", nil), allowed, ForwardOptions{}); failure == nil || failure.Status != http.StatusNotFound {
			t.Fatalf("non-GET catalog failure = %+v", failure)
		}
	}
}

func TestCPAAntigravityClientTransport(t *testing.T) {
	t.Parallel()
	base, _ := url.Parse("http://codex-compat:8317")
	client := withManagement(NewCPAAntigravity(base, "internal-secret"))
	transport := client.http.Transport.(*http.Transport)
	if transport.Proxy != nil || transport.ResponseHeaderTimeout < 5*time.Minute+15*time.Second {
		t.Fatal("Antigravity needs direct internal traffic and a header timeout longer than a slow reply")
	}
	if client.AccountProvider() != "antigravity" {
		t.Fatalf("provider = %q", client.AccountProvider())
	}
}

func TestAntigravityForwardCancellationWhileWaitingForResult(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	antigravity := testRouterClient(true, func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	router := NewRouter(nil, antigravity, map[string]string{antigravityPublicModel: antigravityPublicModel})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan *Failure, 1)
	go func() {
		request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"`+antigravityPublicModel+`","input":"hello"}`))
		_, failure := router.ForwardWithOptions(ctx, httptest.NewRecorder(), request, antigravityPublicModel, "/v1/responses", ForwardOptions{})
		done <- failure
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Antigravity request did not start")
	}
	cancel()
	select {
	case failure := <-done:
		if failure == nil || failure.Code != "client_disconnected" || !errors.Is(failure, context.Canceled) {
			t.Fatalf("failure = %+v", failure)
		}
	case <-time.After(time.Second):
		t.Fatal("Antigravity request survived cancellation")
	}
}
