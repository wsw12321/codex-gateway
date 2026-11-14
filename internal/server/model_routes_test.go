package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wsw/codex-gateway/internal/httpx"
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

// fakeModelRoutes resolves routes from a map for every user.
type fakeModelRoutes struct {
	routes  map[string]string
	err     error
	lookups atomic.Int64
}

func (f *fakeModelRoutes) ListUserModelRoutes(context.Context, string) ([]store.ModelRoute, error) {
	return nil, f.err
}

func (f *fakeModelRoutes) ResolveUserModelRoute(_ context.Context, _ string, model string) (string, bool, error) {
	f.lookups.Add(1)
	if f.err != nil {
		return "", false, f.err
	}
	target, ok := f.routes[model]
	return target, ok, nil
}

func (f *fakeModelRoutes) ReplaceUserModelRoutes(context.Context, store.ReplaceUserModelRoutesParams) ([]store.ModelRoute, error) {
	return nil, f.err
}

func TestRewriteTopLevelModel(t *testing.T) {
	large := strings.Repeat("x", 200<<10)
	for _, test := range []struct {
		name, body, want string
		chunked          bool
	}{
		{"first", `{"model":"gpt-test","input":"hi"}`, `{"model":"claude-target","input":"hi"}`, false},
		{"middle", `{"input":"hi","model":"gpt-test","stream":true}`, `{"input":"hi","model":"claude-target","stream":true}`, false},
		{"last with whitespace", "{ \"input\" : [ ] ,\n \"model\" :\t\"gpt-test\" }\n", "{ \"input\" : [ ] ,\n \"model\" :\t\"claude-target\" }\n", false},
		{"nested model first", `{"input":[{"model":"gpt-test"}],"model":"gpt-test"}`, `{"input":[{"model":"gpt-test"}],"model":"claude-target"}`, false},
		{"after large field", `{"input":"` + large + `","model":"gpt-test"}`, `{"input":"` + large + `","model":"claude-target"}`, true},
		{"before large field", `{"model":"gpt-test","input":"` + large + `"}`, `{"model":"claude-target","input":"` + large + `"}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := &Server{}
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(test.body))
			if test.chunked {
				request.ContentLength = -1
			}
			routing, body, err := s.prepareModelBody(httptest.NewRecorder(), request, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer body.Close()
			size, err := rewriteTopLevelModel(body, routing, "claude-target")
			if err != nil || size != int64(len(test.want)) {
				t.Fatalf("size=%d want=%d err=%v", size, len(test.want), err)
			}
			replayed, err := io.ReadAll(body)
			if err != nil || string(replayed) != test.want {
				t.Fatalf("rewritten body mismatch (len %d, want %d): %v", len(replayed), len(test.want), err)
			}
			if body.bytes != size {
				t.Fatalf("counted bytes=%d, want %d", body.bytes, size)
			}
		})
	}
}

func TestRewriteTopLevelModelRejectsUnsafeInput(t *testing.T) {
	s := &Server{}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test"}`))
	routing, body, err := s.prepareModelBody(httptest.NewRecorder(), request, 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if _, err := rewriteTopLevelModel(body, routing, `bad"target`); err == nil {
		t.Fatal("accepted a target that needs JSON escaping")
	}
	if _, err := body.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := rewriteTopLevelModel(body, routing, "claude-target"); err == nil {
		t.Fatal("rewrote a body that was already partly read")
	}
}

func TestModelRouteRejectionsHappenBeforeUpstreamOrAdmission(t *testing.T) {
	for _, test := range []struct {
		name, path, body, wantCode string
		routes                     map[string]string
		lookupErr                  error
		wantStatus                 int
	}{
		{"lookup failure fails closed", "/v1/responses", `{"model":"gpt-test","input":"hi"}`, "model_route_unavailable", nil, errors.New("database down"), 503},
		{"messages cannot route to codex", "/v1/messages", `{"model":"claude-alias","max_tokens":1,"messages":[]}`, "model_route_incompatible", map[string]string{"claude-alias": "gpt-test"}, nil, 400},
		{"count tokens cannot route to codex", "/v1/messages/count_tokens", `{"model":"claude-alias","messages":[]}`, "model_route_incompatible", map[string]string{"claude-alias": "gpt-test"}, nil, 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newResponsesWebSocketTestHarness(t)
			h.server.config.BodyLimit = 1 << 20
			routes := &fakeModelRoutes{routes: test.routes, err: test.lookupErr}
			h.server.modelRouteRepo = routes
			var anthropicCalls atomic.Int64
			cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				anthropicCalls.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer cpa.Close()
			base, _ := url.Parse(cpa.URL)
			h.server.anthropic = withManagement(gatewayproxy.NewCPAAnthropic(base, "internal"))
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			if strings.HasPrefix(test.path, "/v1/messages") {
				request.Header.Set("X-Api-Key", h.apiKey)
			} else {
				request.Header.Set("Authorization", "Bearer "+h.apiKey)
			}
			recorder := httptest.NewRecorder()
			h.handler.ServeHTTP(recorder, request)
			var response httpx.ErrorBody
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != test.wantStatus || response.Error.Code != test.wantCode || routes.lookups.Load() != 1 {
				t.Fatalf("response %d %s lookups=%d", recorder.Code, recorder.Body, routes.lookups.Load())
			}
			if test.wantCode == "model_route_incompatible" && (!strings.Contains(response.Error.Message, "claude-alias") || !strings.Contains(response.Error.Message, "gpt-test")) {
				t.Fatalf("message does not name the route: %s", response.Error.Message)
			}
			if calls := anthropicCalls.Load() + h.upstreamCalls.Load(); calls != 0 {
				t.Fatalf("upstream calls=%d", calls)
			}
			if queries := h.database.queriesSnapshot(); len(queries) != 1 || !strings.Contains(queries[0], "FROM api_keys k") {
				t.Fatalf("database queries=%#v, want only API key authentication", queries)
			}
			if slots := len(h.server.spoolSlots); slots != 0 {
				t.Fatalf("request spool slots=%d, want 0", slots)
			}
		})
	}
}
