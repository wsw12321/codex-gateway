package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/wsw/codex-gateway/internal/config"
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

func TestCPAV830CatalogIntersectsPricingPermissionsKeyAndUpstream(t *testing.T) {
	const haiku, argon = "claude-haiku-5-5", "gemini-4-argon-high"
	raw, err := os.ReadFile("../../deploy/pricing-v2.example.json")
	if err != nil {
		t.Fatal(err)
	}
	pricing, err := config.ParseUsagePricing(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                   string
		enabled, key, upstream []string
		noRoute                bool
		want                   []string
	}{
		{name: "both authorized", enabled: []string{haiku, argon}, want: []string{haiku, argon}},
		{name: "restricted key", enabled: []string{haiku, argon}, key: []string{haiku}, want: []string{haiku}},
		{name: "old key gains no new model", enabled: []string{haiku, argon}, key: []string{"gemini-pro-agent"}, want: []string{}},
		{name: "user permission still required", enabled: []string{argon}, key: []string{haiku, argon}, want: []string{argon}},
		{name: "route still required", enabled: []string{haiku, argon}, noRoute: true, want: []string{haiku}},
		{name: "account capability still required", enabled: []string{haiku, argon}, upstream: []string{argon}, want: []string{argon}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repository := &fakeModelAccessRepository{enabledModels: append([]string{"gemini-pro-agent", "gemini-4-argon", "muse-wonder-1.0"}, tc.enabled...)}
			for _, model := range pricing.ManageableModelNames() {
				repository.models = append(repository.models, store.ModelAccessModel{Model: model})
			}
			s := newModelAccessHandlerServer(repository)
			s.config.UsagePricing = pricing
			s.config.AntigravityTransport = "cpa"
			if !tc.noRoute {
				s.config.AntigravityModelRoutes = map[string]string{argon: argon}
			}
			allowed, err := s.allowedModelsForAPIKey(context.Background(), store.APIKey{UserID: "user", ModelAllowlist: tc.key})
			if err != nil {
				t.Fatal(err)
			}
			upstreamModels := tc.upstream
			if upstreamModels == nil {
				upstreamModels = []string{haiku, argon, "gemini-pro-agent", "gemini-4-argon", "muse-wonder-1.0"}
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer internal-test" {
					t.Error("unexpected catalog request")
				}
				entries := make([]map[string]string, 0, len(upstreamModels))
				for _, model := range upstreamModels {
					entries = append(entries, map[string]string{"id": model, "object": "model"})
				}
				writeJSON(w, 200, map[string]any{"object": "list", "data": entries})
			}))
			defer upstream.Close()
			base, _ := url.Parse(upstream.URL)
			client := gatewayproxy.NewWithHTTPClient(base, "internal-test", upstream.Client())
			w := httptest.NewRecorder()
			_, failure := client.ForwardModelsWithOptions(context.Background(), w,
				httptest.NewRequest(http.MethodGet, "/v1/models", nil), allowed, gatewayproxy.ForwardOptions{})
			if failure != nil {
				t.Fatal(failure)
			}
			var catalog struct {
				Data []struct{ ID string } `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &catalog); err != nil {
				t.Fatal(err)
			}
			models := make([]string, 0, len(catalog.Data))
			for _, model := range catalog.Data {
				models = append(models, model.ID)
			}
			sort.Strings(models)
			if !reflect.DeepEqual(models, tc.want) {
				t.Fatalf("public catalog = %v, want %v", models, tc.want)
			}
		})
	}
}

func TestCPAV830RemovedModelsRejectBeforeAdmission(t *testing.T) {
	for _, model := range []string{"gemini-pro-agent", "gemini-3.1-pro-low", "gemini-3.6-flash-high", "gemini-3.7-flash-high", "gemini-3.7-flash", "gemini-4-argon"} {
		for _, native := range []bool{false, true} {
			t.Run(model+map[bool]string{false: "/responses", true: "/native"}[native], func(t *testing.T) {
				h := newResponsesWebSocketTestHarness(t)
				h.server.config.AntigravityTransport = "cpa"
				h.server.config.BodyLimit = 64 << 20
				h.server.config.UsagePricing = nativeGeminiPricing(t)
				h.server.config.AntigravityModelRoutes = map[string]string{config.AntigravityPublicModel: config.AntigravityPublicModel}
				database := &geminiAdmissionTestConnector{auth: h.database, model: model}
				db := sql.OpenDB(database)
				t.Cleanup(func() { _ = db.Close() })
				h.server.store = store.New(db)
				path, body := "/v1/responses", `{"model":"`+model+`","input":"hello"}`
				if native {
					path, body = "/v1beta/models/"+model+":generateContent", nativeGeminiText
				}
				r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer "+h.apiKey)
				w := httptest.NewRecorder()
				h.handler.ServeHTTP(w, r)
				if w.Code != 404 || h.upstreamCalls.Load() != 0 || database.begins.Load() != 0 || database.writes.Load() != 0 {
					t.Fatalf("retired request crossed admission: status=%d upstream=%d begins=%d writes=%d", w.Code, h.upstreamCalls.Load(), database.begins.Load(), database.writes.Load())
				}
			})
		}
	}
}
