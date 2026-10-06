package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/store"
)

type fakeModelPriceRepository struct {
	pricing config.UsagePricing
	values  []store.ModelPrice
	params  []store.SetModelPriceParams
	err     error
	replay  *store.ModelPrice
}

func (f *fakeModelPriceRepository) ListModelPrices(_ context.Context, pricing config.UsagePricing) ([]store.ModelPrice, error) {
	f.pricing = pricing
	return f.values, f.err
}

func (f *fakeModelPriceRepository) SetModelPrice(_ context.Context, params store.SetModelPriceParams) (store.ModelPrice, error) {
	f.params = append(f.params, params)
	if f.replay != nil {
		return *f.replay, f.err
	}
	if !params.Pricing.IsManageableModel(params.Model) && f.err == nil {
		return store.ModelPrice{}, store.ErrNotFound
	}
	return store.ModelPrice{Model: params.Model, Version: params.Version + 1, Source: "override", EffectivePrice: params.Price, Editable: true}, f.err
}

func modelPriceTestCatalog(t *testing.T) config.UsagePricing {
	t.Helper()
	const tokenPrice = `{"input_usd_per_million":"1.2500","cached_input_usd_per_million":"0","cache_write_usd_per_million":"2.5000","output_usd_per_million":"8.0000"}`
	tier := `{"short":` + tokenPrice + `,"long":` + tokenPrice + `}`
	pricing, err := config.ParseUsagePricing(`{
		"schema_version":2,"catalog_as_of":"2026-10-06","fx_as_of":"2026-10-06","usd_cny_rate":"7",
		"fallback_policy":{"unknown_service_tier":"max_published","missing_price_combination":"max_published","missing_cache_write_tokens":"all_uncached_as_write"},
		"models":{"gpt-6-astra":{"cache_write_mode":"separate","max_input_tokens":1000000,"long_context_threshold_tokens":200000,
		"service_tiers":{"standard":` + tier + `,"flex":` + tier + `,"fast":` + tier + `}}}}`)
	if err != nil {
		t.Fatal(err)
	}
	return pricing
}

func newModelPriceTestServer(t *testing.T, role string, verified *time.Time) (*Server, *fakeModelPriceRepository) {
	t.Helper()
	s, _ := newBillingSourceTestServer(t, role, verified)
	s.config.UsagePricing = modelPriceTestCatalog(t)
	price := s.config.UsagePricing.Models["gpt-6-astra"]
	structureID, err := s.config.UsagePricing.ModelPriceStructureID("gpt-6-astra")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeModelPriceRepository{values: []store.ModelPrice{
		{Model: "gpt-6-astra", SchemaVersion: 2, ConfiguredPrice: price, EffectivePrice: &price, Multiplier: "0.25", StructureID: structureID, Source: "config", Editable: true},
		{Model: config.InternalGovernanceModel, SchemaVersion: 1, Multiplier: "1", Source: "internal"},
	}}
	s.modelPriceRepo = f
	return s, f
}

func modelPriceTestBody(t *testing.T, s *Server) map[string]any {
	t.Helper()
	structureID, err := s.config.UsagePricing.ModelPriceStructureID("gpt-6-astra")
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"operation_id": "c99f6d40-3fe3-4901-934c-0b41d835d6a0", "reason": "update base prices", "action": "save",
		"version": 0, "structure_id": structureID, "price": s.config.UsagePricing.Models["gpt-6-astra"],
	}
}

func modelPriceJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func modelPriceTestRequest(t *testing.T, method, model, body string) *http.Request {
	t.Helper()
	r := modelMultiplierTestRequest(t, method, model, body)
	r.URL.Path = strings.Replace(r.URL.Path, "model-multipliers", "model-prices", 1)
	return r
}

func TestModelPriceRoutesRequireOwnerAndWriteVerification(t *testing.T) {
	for _, tt := range []struct {
		name, method, role, origin, site, code string
		cookie, verified                       bool
		age                                    time.Duration
		status                                 int
	}{
		{name: "get anonymous", method: "GET", status: 401, code: "session_required"},
		{name: "get member", method: "GET", cookie: true, role: "member", status: 403, code: "owner_required"},
		{name: "get owner", method: "GET", cookie: true, role: "owner", status: 200},
		{name: "put foreign origin", method: "PUT", cookie: true, role: "owner", verified: true, origin: "https://evil.example", status: 403, code: "invalid_origin"},
		{name: "put no origin", method: "PUT", cookie: true, role: "owner", verified: true, status: 403, code: "invalid_origin"},
		{name: "put cross site", method: "PUT", cookie: true, role: "owner", verified: true, origin: "https://gateway.example", site: "cross-site", status: 403, code: "cross_site_request"},
		{name: "put anonymous", method: "PUT", origin: "https://gateway.example", status: 401, code: "session_required"},
		{name: "put unverified", method: "PUT", cookie: true, role: "owner", origin: "https://gateway.example", status: 403, code: "recent_identity_verification_required"},
		{name: "put expired", method: "PUT", cookie: true, role: "owner", verified: true, age: 6 * time.Minute, origin: "https://gateway.example", status: 403, code: "recent_identity_verification_required"},
		{name: "put member", method: "PUT", cookie: true, role: "member", verified: true, origin: "https://gateway.example", status: 403, code: "owner_required"},
		{name: "put owner", method: "PUT", cookie: true, role: "owner", verified: true, origin: "https://gateway.example", status: 200},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var verified *time.Time
			if tt.verified {
				at := time.Now().Add(-tt.age)
				verified = &at
			}
			s, f := newModelPriceTestServer(t, tt.role, verified)
			model := ""
			if tt.method == "PUT" {
				model = "gpt-6-astra"
			}
			r := modelPriceTestRequest(t, tt.method, model, modelPriceJSON(t, modelPriceTestBody(t, s)))
			if !tt.cookie {
				r.Header.Del("Cookie")
			}
			r.Header.Set("Origin", tt.origin)
			r.Header.Set("Sec-Fetch-Site", tt.site)
			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, r)
			if w.Code != tt.status || !strings.Contains(w.Body.String(), tt.code) {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
			if tt.status != 200 && (len(f.params) != 0 || f.pricing.Models != nil) {
				t.Fatal("unauthorized request reached model-price storage")
			}
		})
	}
}

func TestModelPriceCatalogAndCompleteMatrixWriteAttribution(t *testing.T) {
	now := time.Now()
	s, f := newModelPriceTestServer(t, "owner", &now)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, modelPriceTestRequest(t, "GET", "", ""))
	var document struct {
		Models []store.ModelPrice `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &document); err != nil || w.Code != 200 {
		t.Fatalf("status=%d body=%s error=%v", w.Code, w.Body, err)
	}
	if !reflect.DeepEqual(document.Models, f.values) || !reflect.DeepEqual(f.pricing, s.config.UsagePricing) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("catalog not returned faithfully or cached: %s", w.Body)
	}
	body := modelPriceJSON(t, modelPriceTestBody(t, s))
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, modelPriceTestRequest(t, "PUT", "gpt-6-astra", body))
	if w.Code != 200 || len(f.params) != 1 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	p := f.params[0]
	if p.Model != "gpt-6-astra" || p.ActorUserID != "self-1" || p.ActorSessionID != "session-1" || p.OperationID != "c99f6d40-3fe3-4901-934c-0b41d835d6a0" || p.Reason != "update base prices" || p.At.IsZero() || p.Version != 0 || p.Action != "save" || p.StructureID == "" || !reflect.DeepEqual(p.Pricing, s.config.UsagePricing) {
		t.Fatalf("incorrect billing attribution: %+v", p)
	}
	for name, tier := range p.Price.ServiceTiers {
		for _, price := range []*config.TokenPricing{tier.Short, tier.Long} {
			if price.InputUSDPerMillion != "1.25" || price.CachedInputUSDPerMillion != "0" || *price.CacheWriteUSDPerMillion != "2.5" || price.OutputUSDPerMillion != "8" {
				t.Fatalf("matrix tier %s was dropped or not normalized: %+v", name, price)
			}
		}
	}
	restore := modelPriceTestBody(t, s)
	restore["action"], restore["version"] = "restore", 4
	delete(restore, "price")
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, modelPriceTestRequest(t, "PUT", "gpt-6-astra", modelPriceJSON(t, restore)))
	if w.Code != 200 || len(f.params) != 2 || f.params[1].Action != "restore" || f.params[1].Version != 4 || f.params[1].Price != nil {
		t.Fatalf("restore not forwarded: status=%d body=%s", w.Code, w.Body)
	}
	for _, model := range []string{"removed", config.InternalGovernanceModel, "gemini-3.1-pro-preview-customtools"} {
		wantCalls := len(f.params) + 1
		if model == config.InternalGovernanceModel {
			wantCalls--
		}
		w = httptest.NewRecorder()
		s.mux.ServeHTTP(w, modelPriceTestRequest(t, "PUT", model, body))
		if w.Code != 404 || len(f.params) != wantCalls || !strings.Contains(w.Body.String(), "model_price_not_editable") {
			t.Fatalf("modified absent/fixed/alias model %s: %d %s", model, w.Code, w.Body)
		}
	}
}

func TestModelPriceRemovedCatalogDelegatesReplayAndRejectsNewWrites(t *testing.T) {
	now := time.Now()
	s, f := newModelPriceTestServer(t, "owner", &now)
	body := modelPriceJSON(t, modelPriceTestBody(t, s))
	original := f.values[0]
	original.Version, original.Source = 7, "override"
	delete(s.config.UsagePricing.Models, "gpt-6-astra")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, modelPriceTestRequest(t, "PUT", "gpt-6-astra", body))
	if w.Code != 404 || len(f.params) != 1 || !strings.Contains(w.Body.String(), "model_price_not_editable") {
		t.Fatalf("new absent-model write not delegated and rejected: %d %s", w.Code, w.Body)
	}
	f.replay = &original
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, modelPriceTestRequest(t, "PUT", "gpt-6-astra", body))
	var result store.ModelPrice
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || len(f.params) != 2 || !reflect.DeepEqual(result, original) {
		t.Fatalf("removed-model replay did not return stored response: %d %s err=%v", w.Code, w.Body, err)
	}
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, modelPriceTestRequest(t, "PUT", config.InternalGovernanceModel, body))
	if w.Code != 404 || len(f.params) != 2 {
		t.Fatalf("internal model reached replay storage: %d %s", w.Code, w.Body)
	}
}

func TestModelPriceRejectsInvalidOperationsAndAmounts(t *testing.T) {
	now := time.Now()
	s, f := newModelPriceTestServer(t, "owner", &now)
	valid := modelPriceJSON(t, modelPriceTestBody(t, s))
	for _, body := range []string{
		``, `{}`, `null`, `[]`, valid + `{}`,
		strings.Replace(valid, `"1.2500"`, `1.25`, 1),
		strings.Replace(valid, `"1.2500"`, `null`, 1),
		strings.Replace(valid, `"1.2500"`, `"-1"`, 1),
		strings.Replace(valid, `"1.2500"`, `"1e2"`, 1),
		strings.Replace(valid, `"1.2500"`, `"1000000000000000000"`, 1),
		strings.Replace(valid, `"1.2500"`, `"0.0000000000001"`, 1),
		strings.Replace(valid, `"reason":"update base prices"`, `"reason":"  "`, 1),
		strings.Replace(valid, `"update base prices"`, `"`+strings.Repeat("x", 501)+`"`, 1),
		strings.Replace(valid, `c99f6d40-3fe3-4901-934c-0b41d835d6a0`, `invalid`, 1),
		strings.Replace(valid, `"version":0`, `"version":null`, 1),
		strings.Replace(valid, `"version":0`, `"version":-1`, 1),
		strings.Replace(valid, `"version":0`, `"version":0.5`, 1),
		strings.Replace(valid, `"action":"save"`, `"action":"delete"`, 1),
		strings.Replace(valid, `"action":"save"`, `"action":"restore"`, 1),
		strings.Replace(valid, `"reason"`, `"unexpected"`, 1),
		strings.Replace(valid, `"input_usd_per_million"`, `"unknown_price"`, 1),
	} {
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, modelPriceTestRequest(t, "PUT", "gpt-6-astra", body))
		if w.Code != 400 || len(f.params) != 0 {
			t.Fatalf("invalid body %s: status=%d response=%s", body, w.Code, w.Body)
		}
	}
	for _, field := range []string{"version", "price", "structure_id", "reason", "operation_id", "action"} {
		body := modelPriceTestBody(t, s)
		delete(body, field)
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, modelPriceTestRequest(t, "PUT", "gpt-6-astra", modelPriceJSON(t, body)))
		if w.Code != 400 || len(f.params) != 0 {
			t.Fatalf("accepted missing %s: status=%d body=%s", field, w.Code, w.Body)
		}
	}
}

func TestModelPriceRejectsReadonlyMatrixChanges(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*config.ModelPricing)
	}{
		{"threshold", func(p *config.ModelPricing) { p.LongContextThresholdTokens++ }},
		{"maximum", func(p *config.ModelPricing) { p.MaxInputTokens++ }},
		{"cache mode", func(p *config.ModelPricing) { p.CacheWriteMode = config.CacheWriteIncludedInInput }},
		{"missing tier", func(p *config.ModelPricing) { delete(p.ServiceTiers, config.PricingTierFast) }},
		{"new tier", func(p *config.ModelPricing) { p.ServiceTiers["other"] = p.ServiceTiers[config.PricingTierStandard] }},
		{"missing context", func(p *config.ModelPricing) {
			tier := p.ServiceTiers[config.PricingTierFast]
			tier.Long = nil
			p.ServiceTiers[config.PricingTierFast] = tier
		}},
		{"missing write", func(p *config.ModelPricing) {
			p.ServiceTiers[config.PricingTierFlex].Short.CacheWriteUSDPerMillion = nil
		}},
		{"legacy price", func(p *config.ModelPricing) { p.InputUSDPerMillion = "1" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now()
			s, f := newModelPriceTestServer(t, "owner", &now)
			body := modelPriceTestBody(t, s)
			price := modelPriceTestCatalog(t).Models["gpt-6-astra"]
			tt.change(&price)
			body["price"] = price
			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, modelPriceTestRequest(t, "PUT", "gpt-6-astra", modelPriceJSON(t, body)))
			if w.Code != 400 || len(f.params) != 0 {
				t.Fatalf("accepted read-only change: status=%d body=%s", w.Code, w.Body)
			}
		})
	}
}

func TestModelPriceLegacyAndIncludedCacheWrite(t *testing.T) {
	for _, schema := range []int{1, 2} {
		now := time.Now()
		s, f := newModelPriceTestServer(t, "owner", &now)
		price := config.ModelPricing{InputUSDPerMillion: "999999999999999999.999999999999", CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "0.000000000001"}
		if schema == 2 {
			price = s.config.UsagePricing.Models["gpt-6-astra"]
			price.CacheWriteMode = config.CacheWriteIncludedInInput
			for _, tier := range price.ServiceTiers {
				tier.Short.CacheWriteUSDPerMillion = nil
				tier.Long.CacheWriteUSDPerMillion = nil
			}
		}
		s.config.UsagePricing.SchemaVersion = schema
		s.config.UsagePricing.Models["gpt-6-astra"] = price
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, modelPriceTestRequest(t, "PUT", "gpt-6-astra", modelPriceJSON(t, modelPriceTestBody(t, s))))
		if w.Code != 200 || len(f.params) != 1 {
			t.Fatalf("schema %d rejected: status=%d body=%s", schema, w.Code, w.Body)
		}
		if schema == 2 {
			body := modelPriceTestBody(t, s)
			var draft config.ModelPricing
			if err := json.Unmarshal([]byte(modelPriceJSON(t, price)), &draft); err != nil {
				t.Fatal(err)
			}
			writePrice := "0"
			draft.ServiceTiers[config.PricingTierStandard].Short.CacheWriteUSDPerMillion = &writePrice
			body["price"] = draft
			w = httptest.NewRecorder()
			s.mux.ServeHTTP(w, modelPriceTestRequest(t, "PUT", "gpt-6-astra", modelPriceJSON(t, body)))
			if w.Code != 400 || len(f.params) != 1 {
				t.Fatalf("included cache write editable: status=%d body=%s", w.Code, w.Body)
			}
		}
	}
}

func TestModelPriceStorageFailuresAndStaleStructureReplay(t *testing.T) {
	now := time.Now()
	s, f := newModelPriceTestServer(t, "owner", &now)
	for _, tt := range []struct {
		err  error
		code int
	}{
		{errors.New("database unavailable"), 500},
		{store.ErrConflict, 409},
		{store.ErrBillingOperationCleaned, 409},
		{store.ErrInvalid, 400},
	} {
		f.err = tt.err
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, modelPriceTestRequest(t, "PUT", "gpt-6-astra", modelPriceJSON(t, modelPriceTestBody(t, s))))
		if w.Code != tt.code || (errors.Is(tt.err, store.ErrConflict) && !strings.Contains(w.Body.String(), "model_price_conflict")) {
			t.Fatalf("error %v status=%d body=%s", tt.err, w.Code, w.Body)
		}
	}
	f.err = errors.New("database unavailable")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, modelPriceTestRequest(t, "GET", "", ""))
	if w.Code != 500 || strings.Contains(w.Body.String(), "configured_price") {
		t.Fatalf("GET failed open: %d %s", w.Code, w.Body)
	}
	// Storage must receive completed-operation retries even after a deployment
	// changes the current catalog; it decides replay versus stale new write.
	f.err = nil
	body := modelPriceTestBody(t, s)
	body["structure_id"] = strings.Repeat("a", 64)
	body["price"] = config.ModelPricing{InputUSDPerMillion: "1", CachedInputUSDPerMillion: "0", OutputUSDPerMillion: "2"}
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, modelPriceTestRequest(t, "PUT", "gpt-6-astra", modelPriceJSON(t, body)))
	if w.Code != 200 || len(f.params) != 5 {
		t.Fatalf("old-structure retry did not reach operation lookup: %d %s", w.Code, w.Body)
	}
}
