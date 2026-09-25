package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/config"
	"github.com/wsw/codex-gateway/internal/modelid"
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"github.com/wsw/codex-gateway/internal/store"
)

const identificationTestAccount = "0123456789abcdef"
const identificationTestUser = "00000000-0000-0000-0000-000000000001"

func TestModelIdentificationRoutesRequireOwnerOriginAndRecentVerification(t *testing.T) {
	tests := []struct {
		name, method, path, body string
	}{
		{"list", http.MethodGet, "/admin/model-identifications", ""},
		{"options", http.MethodGet, "/admin/model-identifications/options?account_id=" + identificationTestAccount, ""},
		{"run", http.MethodPost, "/admin/model-identifications/runs", `{"account_id":"` + identificationTestAccount + `","model":"gpt-6-sol"}`},
	}
	for _, endpoint := range tests {
		t.Run(endpoint.name, func(t *testing.T) {
			for _, test := range []struct {
				name, role, origin, site, code string
				session, verified              bool
				age                            time.Duration
			}{
				{"no session", store.UserRoleOwner, "https://gateway.example", "", "session_required", false, true, 0},
				{"member", store.UserRoleMember, "https://gateway.example", "", "owner_required", true, true, 0},
				{"foreign origin", store.UserRoleOwner, "https://other.example", "", "invalid_origin", true, true, 0},
				{"cross site", store.UserRoleOwner, "https://gateway.example", "cross-site", "cross_site_request", true, true, 0},
				{"unverified", store.UserRoleOwner, "https://gateway.example", "", "recent_identity_verification_required", true, false, 0},
				{"verification expired", store.UserRoleOwner, "https://gateway.example", "", "recent_identity_verification_required", true, true, 6 * time.Minute},
			} {
				if endpoint.method == http.MethodGet && test.name != "no session" && test.name != "member" {
					continue
				}
				t.Run(test.name, func(t *testing.T) {
					var verified *time.Time
					if test.verified {
						now := time.Now().UTC().Add(-test.age)
						verified = &now
					}
					s, _ := newBillingSourceTestServer(t, test.role, verified)
					r := httptest.NewRequest(endpoint.method, endpoint.path, strings.NewReader(endpoint.body))
					r.Header.Set("Origin", test.origin)
					r.Header.Set("Sec-Fetch-Site", test.site)
					if test.session {
						addBillingSourceTestSession(t, r)
					}
					w := httptest.NewRecorder()
					s.Handler().ServeHTTP(w, r)
					if !strings.Contains(w.Body.String(), test.code) || w.Code != http.StatusForbidden && w.Code != http.StatusUnauthorized {
						t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
					}
				})
			}
		})
	}
}

type identificationRoundTripper func(*http.Request) (*http.Response, error)

func (fn identificationRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func identificationResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)),
	}
}

type fakeIdentificationRepo struct {
	item      store.ModelIdentification
	completed bool
	failed    string
	progress  int
}

func (r *fakeIdentificationRepo) BeginModelIdentificationRun(context.Context, string, string) (store.ModelIdentification, error) {
	return r.item, nil
}
func (r *fakeIdentificationRepo) AdvanceModelIdentificationRun(_ context.Context, _ string, progress int) (store.ModelIdentification, error) {
	r.progress = progress
	return r.item, nil
}
func (r *fakeIdentificationRepo) CompleteModelIdentificationRun(_ context.Context, _ string, result store.ModelIdentificationResult) (store.ModelIdentification, error) {
	r.completed = true
	r.item.Conclusion = result.Conclusion
	r.item.ClosestModel = result.ClosestModel
	r.item.MatchLevel = result.MatchLevel
	r.item.ReferenceVersion = result.ReferenceVersion
	return r.item, nil
}
func (r *fakeIdentificationRepo) FailModelIdentificationRun(_ context.Context, _ string, code string) (store.ModelIdentification, error) {
	r.failed = code
	return r.item, nil
}
func (r *fakeIdentificationRepo) ListModelIdentifications(context.Context) ([]store.ModelIdentification, error) {
	return []store.ModelIdentification{r.item}, nil
}
func (r *fakeIdentificationRepo) RecoverInterruptedModelIdentificationRuns(context.Context) (int64, error) {
	return 0, nil
}
func (r *fakeIdentificationRepo) PurgeExpiredModelIdentifications(context.Context) (int64, error) {
	return 0, nil
}

func modelIdentificationSampleReplies(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile("../modelid/testdata/example.json")
	if err != nil {
		t.Fatal(err)
	}
	var sample struct {
		Sets map[string][]struct {
			Text string `json:"text"`
		} `json:"sets"`
	}
	if err := json.Unmarshal(data, &sample); err != nil {
		t.Fatal(err)
	}
	rows := sample.Sets["environment-05"]
	return []string{rows[0].Text, rows[1].Text, rows[2].Text}
}

func TestModelIdentificationWorkerPinsAccountAndPreservesOldResultOnFailure(t *testing.T) {
	replies := modelIdentificationSampleReplies(t)
	for _, test := range []struct {
		name, failure, wantFailure string
	}{
		{"success", "", ""},
		{"disabled between probes", "disabled", "model_identification_account_unavailable"},
		{"model removed between probes", "removed", "model_identification_model_unavailable"},
		{"returned account mismatch", "mismatch", "model_identification_account_mismatch"},
		{"invalid answer", "invalid", "model_identification_invalid_answer"},
		{"timeout", "timeout", "model_identification_timeout"},
	} {
		t.Run(test.name, func(t *testing.T) {
			probes := 0
			transport := identificationRoundTripper(func(request *http.Request) (*http.Response, error) {
				if request.Header.Get("Authorization") != "Bearer secret" {
					t.Errorf("missing internal bearer")
				}
				switch request.URL.Path {
				case "/internal/upstream-accounts":
					status, manual := "available", "enabled"
					if test.failure == "disabled" && probes > 0 {
						status, manual = "unavailable", "manual_disabled"
					}
					return identificationResponse(200, `{"accounts":[{"id":"`+identificationTestAccount+`","masked_email":"u***@example.com","plan":"plus","status":"`+status+`","cliproxy_status":"active","gateway_manual_status":"`+manual+`","gateway_quota_status":"available","last_synced_at":"2026-09-25T00:00:00Z"}]}`), nil
				case "/internal/upstream-accounts/" + identificationTestAccount + "/models":
					models := `["gpt-6-sol","gemini-bridge","not-configured"]`
					if test.failure == "removed" && probes > 0 {
						models = `[]`
					}
					return identificationResponse(200, `{"account_id":"`+identificationTestAccount+`","models":`+models+`}`), nil
				case "/internal/upstream-accounts/" + identificationTestAccount + "/probe":
					if got := request.Header.Get("X-Codex-Gateway-User"); got != identificationTestUser {
						t.Errorf("probe user identity = %q, want %q", got, identificationTestUser)
					}
					var body struct {
						Model           string `json:"model"`
						Input           string `json:"input"`
						MaxOutputTokens int    `json:"max_output_tokens"`
					}
					if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if body.Model != "gpt-6-sol" || body.Input != modelid.Challenges()[probes].Prompt || body.MaxOutputTokens != 16384 {
						t.Errorf("probe %d had unexpected request", probes)
					}
					if test.failure == "timeout" {
						return nil, context.DeadlineExceeded
					}
					account := identificationTestAccount
					if test.failure == "mismatch" {
						account = "ffffffffffffffff"
					}
					reply := replies[probes]
					if test.failure == "invalid" {
						reply = "1, 2, 3"
					}
					probes++
					encoded, _ := json.Marshal(map[string]string{"account_id": account, "output_text": reply})
					return identificationResponse(200, string(encoded)), nil
				}
				return nil, errors.New("unexpected sidecar path")
			})
			base, _ := url.Parse("http://sidecar.test")
			repository := &fakeIdentificationRepo{item: store.ModelIdentification{RunID: "run-1", Conclusion: "old conclusion", ClosestModel: "old-model"}}
			s := &Server{
				upstream:                gatewayproxy.NewWithHTTPClient(base, "secret", &http.Client{Transport: transport}),
				modelIdentificationRepo: repository,
				logger:                  slog.New(slog.NewTextHandler(io.Discard, nil)),
				config: config.Config{UsagePricing: config.UsagePricing{Models: map[string]config.ModelPricing{
					"gpt-6-sol": {}, "gemini-bridge": {},
				}}, AntigravityModelRoutes: map[string]string{"gemini-bridge": "bridge"}},
			}
			options, err := s.resolveModelIdentificationOptions(context.Background(), identificationTestAccount)
			if err != nil || len(options.Models) != 1 || options.Models[0] != "gpt-6-sol" {
				t.Fatalf("options=%+v err=%v", options, err)
			}
			s.runModelIdentification("run-1", identificationTestAccount, "gpt-6-sol", identificationTestUser)
			if test.wantFailure == "" {
				if !repository.completed || repository.failed != "" || repository.progress != 3 ||
					repository.item.ClosestModel != "claude-fable-5-1" || repository.item.ReferenceVersion != modelid.ReferenceVersion {
					t.Fatalf("unexpected success: %+v", repository)
				}
			} else {
				if repository.completed || repository.failed != test.wantFailure || repository.item.Conclusion != "old conclusion" || repository.item.ClosestModel != "old-model" {
					t.Fatalf("old result replaced on failure: %+v", repository)
				}
			}
		})
	}
}

func TestModelIdentificationGracefulStopMarksInterrupted(t *testing.T) {
	probeStarted := make(chan struct{})
	transport := identificationRoundTripper(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/internal/upstream-accounts":
			return identificationResponse(200, `{"accounts":[{"id":"`+identificationTestAccount+`","masked_email":"u***@example.com","plan":"plus","status":"available","cliproxy_status":"active","gateway_manual_status":"enabled","gateway_quota_status":"available","last_synced_at":"2026-09-25T00:00:00Z"}]}`), nil
		case "/internal/upstream-accounts/" + identificationTestAccount + "/models":
			return identificationResponse(200, `{"account_id":"`+identificationTestAccount+`","models":["gpt-6-sol"]}`), nil
		case "/internal/upstream-accounts/" + identificationTestAccount + "/probe":
			close(probeStarted)
			<-request.Context().Done()
			return nil, request.Context().Err()
		}
		return nil, errors.New("unexpected path")
	})
	base, _ := url.Parse("http://sidecar.test")
	parent, cancel := context.WithCancel(context.Background())
	repository := &fakeIdentificationRepo{item: store.ModelIdentification{RunID: "run-1", Conclusion: "old", ClosestModel: "old-model"}}
	s := &Server{
		upstream:                gatewayproxy.NewWithHTTPClient(base, "secret", &http.Client{Transport: transport}),
		modelIdentificationRepo: repository, identificationContext: parent, identificationCancel: cancel,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		config: config.Config{UsagePricing: config.UsagePricing{Models: map[string]config.ModelPricing{"gpt-6-sol": {}}}},
	}
	s.identificationWG.Add(1)
	go func() {
		defer s.identificationWG.Done()
		s.runModelIdentification("run-1", identificationTestAccount, "gpt-6-sol")
	}()
	select {
	case <-probeStarted:
	case <-time.After(time.Second):
		t.Fatal("probe did not start")
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := s.StopModelIdentifications(stopCtx); err != nil {
		t.Fatal(err)
	}
	if repository.completed || repository.failed != "model_identification_interrupted" || repository.item.Conclusion != "old" {
		t.Fatalf("shutdown outcome=%+v", repository)
	}
}
