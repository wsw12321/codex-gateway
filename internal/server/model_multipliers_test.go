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

type fakeModelMultiplierRepository struct {
	models []string
	values []store.ModelMultiplier
	params []store.SetModelMultiplierParams
	err    error
}

func (f *fakeModelMultiplierRepository) ListModelMultipliers(_ context.Context, models []string) ([]store.ModelMultiplier, error) {
	f.models = models
	if f.values != nil || f.err != nil {
		return f.values, f.err
	}
	values := make([]store.ModelMultiplier, 0, len(models))
	for _, model := range models {
		values = append(values, store.ModelMultiplier{Model: model, Multiplier: "1"})
	}
	return values, nil
}

func (f *fakeModelMultiplierRepository) SetModelMultiplier(_ context.Context, params store.SetModelMultiplierParams) (store.ModelMultiplier, error) {
	f.params = append(f.params, params)
	return store.ModelMultiplier{Model: params.Model, Multiplier: params.Multiplier, UpdatedAt: &params.At}, f.err
}

func newModelMultiplierTestServer(t *testing.T, role string, verified *time.Time) (*Server, *fakeModelMultiplierRepository) {
	t.Helper()
	s, _ := newBillingSourceTestServer(t, role, verified)
	s.config.UsagePricing.Models = map[string]config.ModelPricing{"gpt-6-astra": {}, config.AntigravityPublicModel: {}}
	f := &fakeModelMultiplierRepository{}
	s.modelMultiplierRepo = f
	return s, f
}

const validModelMultiplierBody = `{"multiplier":"0.500","operation_id":"c99f6d40-3fe3-4901-934c-0b41d835d6a0","reason":"discount"}`

func modelMultiplierTestRequest(t *testing.T, method, model, body string) *http.Request {
	t.Helper()
	path := "/admin/billing/model-multipliers"
	if model != "" {
		path += "/" + model
	}
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Origin", "https://gateway.example")
	r.Header.Set("Content-Type", "application/json")
	addBillingSourceTestSession(t, r)
	return r
}

func TestModelMultiplierRoutesRequireOwnerAndWriteVerification(t *testing.T) {
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
			s, f := newModelMultiplierTestServer(t, tt.role, verified)
			model := ""
			if tt.method == "PUT" {
				model = "gpt-6-astra"
			}
			r := modelMultiplierTestRequest(t, tt.method, model, validModelMultiplierBody)
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
			if tt.status != 200 && (len(f.params) != 0 || len(f.models) != 0) {
				t.Fatal("unauthorized request reached multiplier storage")
			}
		})
	}
}

func TestModelMultiplierCatalogAndWriteAttribution(t *testing.T) {
	now := time.Now()
	s, f := newModelMultiplierTestServer(t, "owner", &now)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, modelMultiplierTestRequest(t, "GET", "", ""))
	var document struct {
		Models []modelMultiplierResponse `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &document); err != nil || w.Code != 200 {
		t.Fatalf("status=%d body=%s error=%v", w.Code, w.Body, err)
	}
	if !reflect.DeepEqual(f.models, []string{config.InternalGovernanceModel, config.AntigravityPublicModel, "gpt-6-astra"}) || len(document.Models) != 3 {
		t.Fatalf("catalog=%+v read=%v", document.Models, f.models)
	}
	for _, row := range document.Models {
		if row.Multiplier != "1" || row.UpdatedAt != nil || row.Editable != (row.Model != config.InternalGovernanceModel) {
			t.Fatalf("default row=%+v", row)
		}
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("settings may be cached")
	}
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, modelMultiplierTestRequest(t, "PUT", config.AntigravityPublicModel, validModelMultiplierBody))
	if w.Code != 200 || len(f.params) != 1 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	p := f.params[0]
	if p.Model != config.AntigravityPublicModel || p.Multiplier != "0.5" || p.ActorUserID != "self-1" || p.ActorSessionID != "session-1" || p.OperationID != "c99f6d40-3fe3-4901-934c-0b41d835d6a0" || p.Reason != "discount" || p.At.IsZero() {
		t.Fatalf("incorrect billing attribution: %+v", p)
	}
	for _, model := range []string{"removed", config.InternalGovernanceModel, "gemini-3.1-pro-preview-customtools"} {
		w = httptest.NewRecorder()
		s.mux.ServeHTTP(w, modelMultiplierTestRequest(t, "PUT", model, validModelMultiplierBody))
		if w.Code != 404 || len(f.params) != 1 {
			t.Fatalf("modified absent/fixed/alias model %s: %d %s", model, w.Code, w.Body)
		}
	}
}

func TestModelMultiplierRejectsInvalidWritesAndDatabaseFailure(t *testing.T) {
	now := time.Now()
	s, f := newModelMultiplierTestServer(t, "owner", &now)
	for _, body := range []string{
		``, `{}`, `null`, `[]`, validModelMultiplierBody + `{}`,
		strings.Replace(validModelMultiplierBody, `"0.500"`, `0.5`, 1),
		strings.Replace(validModelMultiplierBody, `"0.500"`, `null`, 1),
		strings.Replace(validModelMultiplierBody, `"0.500"`, `"0"`, 1),
		strings.Replace(validModelMultiplierBody, `"0.500"`, `"-1"`, 1),
		strings.Replace(validModelMultiplierBody, `"0.500"`, `"1e2"`, 1),
		strings.Replace(validModelMultiplierBody, `"0.500"`, `"1000000000000000000"`, 1),
		strings.Replace(validModelMultiplierBody, `"0.500"`, `"0.0000000000001"`, 1),
		strings.Replace(validModelMultiplierBody, `"discount"`, `"  "`, 1),
		strings.Replace(validModelMultiplierBody, `"discount"`, `"`+strings.Repeat("x", 501)+`"`, 1),
		strings.Replace(validModelMultiplierBody, `c99f6d40-3fe3-4901-934c-0b41d835d6a0`, `invalid`, 1),
		strings.Replace(validModelMultiplierBody, `"reason"`, `"unexpected"`, 1),
	} {
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, modelMultiplierTestRequest(t, "PUT", "gpt-6-astra", body))
		if w.Code != 400 || len(f.params) != 0 {
			t.Fatalf("invalid body %s: status=%d response=%s", body, w.Code, w.Body)
		}
	}
	for _, err := range []error{errors.New("database unavailable"), store.ErrConflict, store.ErrBillingOperationCleaned} {
		f.err = err
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, modelMultiplierTestRequest(t, "PUT", "gpt-6-astra", validModelMultiplierBody))
		want := 409
		if err.Error() == "database unavailable" {
			want = 500
		}
		if w.Code != want {
			t.Fatalf("error %v status=%d body=%s", err, w.Code, w.Body)
		}
	}
	f.err = errors.New("database unavailable")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, modelMultiplierTestRequest(t, "GET", "", ""))
	if w.Code != 500 || strings.Contains(w.Body.String(), `"multiplier":"1"`) {
		t.Fatalf("GET failed open: %d %s", w.Code, w.Body)
	}
}
