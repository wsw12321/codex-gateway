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
		{"run detail", http.MethodGet, "/admin/model-identifications/runs/00000000-0000-0000-0000-000000000002", ""},
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
	item                   store.ModelIdentification
	completed              bool
	failed                 string
	progress               int
	stages                 []string
	detail                 store.ModelIdentificationFailure
	beginErr, errorAtStage error
	failStage              string
}

func (r *fakeIdentificationRepo) BeginModelIdentificationRun(_ context.Context, account, model string, actors ...string) (store.ModelIdentification, error) {
	r.item.AccountID, r.item.RequestedModel = account, model
	if len(actors) > 0 {
		r.item.RunActorID = actors[0]
	}
	return r.item, r.beginErr
}
func (r *fakeIdentificationRepo) UpdateModelIdentificationRun(_ context.Context, _ string, stage string, index, progress int) (store.ModelIdentification, error) {
	if stage == r.failStage {
		return r.item, r.errorAtStage
	}
	r.item.RunStage, r.item.RunProbeIndex, r.item.RunProgress = stage, index, progress
	r.progress = progress
	r.stages = append(r.stages, stage)
	return r.item, nil
}
func (r *fakeIdentificationRepo) GetModelIdentificationRun(_ context.Context, id string) (store.ModelIdentification, error) {
	if id != r.item.RunID {
		return store.ModelIdentification{}, store.ErrNotFound
	}
	return r.item, nil
}
func (r *fakeIdentificationRepo) CompleteModelIdentificationRun(_ context.Context, _ string, result store.ModelIdentificationResult) (store.ModelIdentification, error) {
	r.completed = true
	r.item.Conclusion, r.item.ClosestModel, r.item.MatchLevel, r.item.ReferenceVersion = result.Conclusion, result.ClosestModel, result.MatchLevel, result.ReferenceVersion
	r.item.RunStatus = store.ModelIdentificationSucceeded
	return r.item, nil
}
func (r *fakeIdentificationRepo) FailModelIdentificationRun(_ context.Context, _ string, code string, details ...store.ModelIdentificationFailure) (store.ModelIdentification, error) {
	r.failed = code
	r.item.RunStatus = store.ModelIdentificationFailed
	if len(details) > 0 {
		r.detail = details[0]
	}
	return r.item, nil
}
func (r *fakeIdentificationRepo) ListModelIdentifications(context.Context) ([]store.ModelIdentification, error) {
	return []store.ModelIdentification{r.item}, nil
}

func identificationTestRun() store.ModelIdentification {
	now := time.Now().UTC()
	return store.ModelIdentification{RunID: "00000000-0000-0000-0000-000000000002", AccountID: identificationTestAccount, RequestedModel: "gpt-6-sol", RunActorID: identificationTestUser, RunStartedAt: &now, RunStatus: store.ModelIdentificationRunning, RunStage: "preflight", Conclusion: "old conclusion", ClosestModel: "old-model"}
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
		name, failure, want string
		wantProbes          int
	}{
		{"success", "", "", 3},
		{"disabled remains testable", "disabled", "", 3},
		{"unconfigured native model remains testable", "unpriced", "", 3},
		{"account removed", "removed", "model_identification_account_not_found", 1},
		{"returned account mismatch", "mismatch", "model_identification_account_mismatch", 1},
		{"invalid first answer", "invalid", "model_identification_invalid_answer", 1},
		{"duplicate second answer", "duplicate", "model_identification_invalid_answer", 2},
		{"upstream limited", "limited", "model_identification_rate_limited", 1},
		{"protocol mismatch", "protocol", "model_identification_protocol_unsupported", 0},
		{"probe timeout", "timeout", "model_identification_timeout", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			probes := 0
			transport := identificationRoundTripper(func(request *http.Request) (*http.Response, error) {
				if request.Header.Get("Authorization") != "Bearer secret" {
					t.Error("missing internal bearer")
				}
				switch request.URL.Path {
				case "/internal/model-identification/capabilities":
					protocol := gatewayproxy.ModelIdentificationProtocol
					if test.failure == "protocol" {
						protocol = "old_v0"
					}
					return identificationResponse(200, `{"protocol":"`+protocol+`"}`), nil
				case "/internal/upstream-accounts":
					status, manual := "available", "enabled"
					if test.failure == "disabled" {
						status, manual = "unavailable", "manual_disabled"
					}
					return identificationResponse(200, `{"accounts":[{"id":"`+identificationTestAccount+`","masked_email":"u***@example.com","plan":"plus","status":"`+status+`","cliproxy_status":"active","gateway_manual_status":"`+manual+`","gateway_quota_status":"available","last_synced_at":"2026-09-25T00:00:00Z"}]}`), nil
				case "/internal/model-identification/accounts/" + identificationTestAccount + "/models":
					return identificationResponse(200, `{"account_id":"`+identificationTestAccount+`","models":["gpt-6-sol","native-unpriced"]}`), nil
				case "/internal/model-identification/accounts/" + identificationTestAccount + "/probe":
					if request.Header.Get("X-Codex-Gateway-User") != identificationTestUser || request.Header.Get("X-Codex-Diagnostic-Run") != identificationTestRun().RunID {
						t.Error("missing trusted actor or run")
					}
					var body struct {
						Model           string `json:"model"`
						Input           string `json:"input"`
						MaxOutputTokens int    `json:"max_output_tokens"`
					}
					if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if body.Input != modelid.Challenges()[probes].Prompt || body.MaxOutputTokens != 16384 {
						t.Error("unexpected challenge")
					}
					if test.failure == "unpriced" && body.Model != "native-unpriced" {
						t.Error("native selection lost")
					}
					reply := replies[probes]
					if test.failure == "duplicate" && probes == 1 {
						reply = replies[0]
					}
					probes++
					switch test.failure {
					case "timeout":
						return nil, context.DeadlineExceeded
					case "invalid":
						reply = "1,2,3"
					case "removed":
						return identificationResponse(404, `{"error":{"code":"probe_account_not_found","stage":"preflight"}}`), nil
					case "limited":
						return identificationResponse(502, `{"error":{"code":"probe_rate_limited","stage":"probing","upstream_status":429,"retry_after":12}}`), nil
					}
					account := identificationTestAccount
					if test.failure == "mismatch" {
						account = "ffffffffffffffff"
					}
					encoded, _ := json.Marshal(map[string]string{"account_id": account, "output_text": reply})
					return identificationResponse(200, string(encoded)), nil
				}
				return nil, errors.New("unexpected sidecar path")
			})
			base, _ := url.Parse("http://sidecar.test")
			repository := &fakeIdentificationRepo{item: identificationTestRun()}
			if test.failure == "unpriced" {
				repository.item.RequestedModel = "native-unpriced"
			}
			s := &Server{upstream: gatewayproxy.NewWithHTTPClient(base, "secret", &http.Client{Transport: transport}), modelIdentificationRepo: repository, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			if test.failure != "protocol" {
				options, err := s.resolveModelIdentificationOptions(context.Background(), identificationTestAccount)
				if err != nil || len(options.Models) != 2 {
					t.Fatalf("options=%+v err=%v", options, err)
				}
			}
			s.runModelIdentification(repository.item)
			if probes != test.wantProbes {
				t.Fatalf("probes=%d want=%d", probes, test.wantProbes)
			}
			if test.want == "" {
				if !repository.completed || repository.failed != "" || repository.progress != 3 || repository.item.ClosestModel != "claude-fable-5-1" {
					t.Fatalf("success=%+v", repository)
				}
				if strings.Join(repository.stages, ",") != "preflight,probing,validating,probing,validating,probing,validating,scoring,saving" {
					t.Fatalf("stages=%v", repository.stages)
				}
			} else if repository.completed || repository.failed != test.want || repository.item.Conclusion != "old conclusion" {
				t.Fatalf("failure=%+v", repository)
			}
			if test.failure == "limited" && (repository.detail.Source != "upstream" || repository.detail.UpstreamStatus != 429 || repository.detail.RetryAfter != 12) {
				t.Fatalf("detail=%+v", repository.detail)
			}
		})
	}
}

func TestModelIdentificationGracefulStopMarksInterrupted(t *testing.T) {
	probeStarted := make(chan struct{})
	transport := identificationRoundTripper(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/internal/model-identification/capabilities":
			return identificationResponse(200, `{"protocol":"`+gatewayproxy.ModelIdentificationProtocol+`"}`), nil
		case "/internal/model-identification/accounts/" + identificationTestAccount + "/models":
			return identificationResponse(200, `{"account_id":"`+identificationTestAccount+`","models":["gpt-6-sol"]}`), nil
		case "/internal/model-identification/accounts/" + identificationTestAccount + "/probe":
			close(probeStarted)
			<-request.Context().Done()
			return nil, request.Context().Err()
		}
		return nil, errors.New("unexpected path")
	})
	base, _ := url.Parse("http://sidecar.test")
	parent, cancel := context.WithCancel(context.Background())
	repository := &fakeIdentificationRepo{item: identificationTestRun()}
	s := &Server{upstream: gatewayproxy.NewWithHTTPClient(base, "secret", &http.Client{Transport: transport}), modelIdentificationRepo: repository, identificationContext: parent, identificationCancel: cancel, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.identificationWG.Add(1)
	go func() { defer s.identificationWG.Done(); s.runModelIdentification(repository.item) }()
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
	if repository.completed || repository.failed != "model_identification_interrupted" || repository.item.Conclusion != "old conclusion" {
		t.Fatalf("shutdown=%+v", repository)
	}
}

func TestModelIdentificationRunDeadlineStartsAtReservation(t *testing.T) {
	repository := &fakeIdentificationRepo{item: identificationTestRun()}
	started := time.Now().Add(-11 * time.Minute)
	repository.item.RunStartedAt = &started
	s := &Server{modelIdentificationRepo: repository}
	s.runModelIdentification(repository.item)
	if repository.failed != "model_identification_timeout" || len(repository.stages) != 0 {
		t.Fatalf("expired reservation executed: %+v", repository)
	}
}

func TestModelIdentificationRunDetailAndListAreReadOnly(t *testing.T) {
	repository := &fakeIdentificationRepo{item: identificationTestRun()}
	s := &Server{modelIdentificationRepo: repository}
	for _, id := range []string{repository.item.RunID, "replaced-run"} {
		r := httptest.NewRequest(http.MethodGet, "/admin/model-identifications/runs/"+id, nil)
		r.SetPathValue("run_id", id)
		w := httptest.NewRecorder()
		s.modelIdentificationRunJSON(w, r)
		if id == repository.item.RunID && (w.Code != 200 || !strings.Contains(w.Body.String(), `"run_stage":"preflight"`)) {
			t.Fatalf("detail=%d %s", w.Code, w.Body.String())
		}
		if id != repository.item.RunID && w.Code != 404 {
			t.Fatalf("unknown run status=%d", w.Code)
		}
	}
	w := httptest.NewRecorder()
	s.modelIdentificationsJSON(w, httptest.NewRequest(http.MethodGet, "/admin/model-identifications", nil))
	if w.Code != 200 || repository.failed != "" || len(repository.stages) != 0 {
		t.Fatal("reads mutated task")
	}
}

func TestModelIdentificationFailureLogsOnlySafeMetadata(t *testing.T) {
	var output strings.Builder
	repository := &fakeIdentificationRepo{item: identificationTestRun(), failStage: "preflight", errorAtStage: errors.New("secret-answer-sentinel")}
	s := &Server{modelIdentificationRepo: repository, logger: slog.New(slog.NewTextHandler(&output, nil))}
	s.runModelIdentification(repository.item)
	if repository.failed != "model_identification_storage_failed" || repository.detail.Source != "storage" || strings.Contains(output.String(), "secret-answer-sentinel") {
		t.Fatalf("unsafe failure: %s", output.String())
	}
}

func TestCreateModelIdentificationReturnsReservedRunWithoutPricingOrStatusGate(t *testing.T) {
	for _, test := range []struct {
		name     string
		accounts string
		beginErr error
		status   int
	}{
		{"disabled unpriced account", `[{"id":"` + identificationTestAccount + `","masked_email":"u***@example.com","plan":"plus","status":"unavailable","cliproxy_status":"disabled","gateway_manual_status":"manual_disabled","gateway_quota_status":"quota_exhausted","last_synced_at":"2026-09-25T00:00:00Z"}]`, nil, http.StatusAccepted},
		{"another active run", `[{"id":"` + identificationTestAccount + `","masked_email":"u***@example.com","plan":"plus","status":"available","cliproxy_status":"active","gateway_manual_status":"enabled","gateway_quota_status":"available","last_synced_at":"2026-09-25T00:00:00Z"}]`, store.ErrConflict, http.StatusConflict},
		{"stale account snapshot", `[]`, nil, http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			transport := identificationRoundTripper(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path != "/internal/upstream-accounts" {
					t.Errorf("unexpected request before canceled worker: %s", request.URL.Path)
				}
				return identificationResponse(200, `{"accounts":`+test.accounts+`}`), nil
			})
			base, _ := url.Parse("http://sidecar.test")
			parent, cancel := context.WithCancel(context.Background())
			cancel()
			repository := &fakeIdentificationRepo{item: identificationTestRun(), beginErr: test.beginErr}
			s := &Server{modelIdentificationRepo: repository, upstream: gatewayproxy.NewWithHTTPClient(base, "secret", &http.Client{Transport: transport}), identificationContext: parent}
			r := httptest.NewRequest(http.MethodPost, "/admin/model-identifications/runs", strings.NewReader(`{"account_id":"`+identificationTestAccount+`","model":"native/name"}`))
			r = r.WithContext(context.WithValue(r.Context(), userContextKey, store.User{ID: identificationTestUser, Role: store.UserRoleOwner}))
			w := httptest.NewRecorder()
			s.createModelIdentificationRun(w, r)
			s.identificationWG.Wait()
			if w.Code != test.status {
				t.Fatalf("create=%d %s", w.Code, w.Body.String())
			}
			if test.status == http.StatusAccepted {
				var response struct {
					RunID string                    `json:"run_id"`
					Run   store.ModelIdentification `json:"run"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if response.RunID != repository.item.RunID || response.Run.RunStatus != store.ModelIdentificationRunning || response.Run.RunStage != "preflight" || response.Run.RunActorID != identificationTestUser || response.Run.RequestedModel != "native/name" {
					t.Fatalf("reservation=%+v", response)
				}
			}
		})
	}
}

func TestModelIdentificationFailureClassification(t *testing.T) {
	for _, test := range []struct {
		wire     string
		upstream int
		want     string
	}{
		{"probe_credential_unavailable", 0, "credential_missing"},
		{"probe_authentication_failed", 401, "auth_failed"},
		{"probe_upstream_rejected", 403, "forbidden"},
		{"probe_upstream_rejected", 302, "redirect"},
		{"probe_upstream_unavailable", 503, "upstream_failed"},
		{"probe_network_failed", 0, "network_failed"},
		{"probe_response_incomplete", 0, "incomplete"},
		{"probe_unexpected_tool", 0, "unexpected_tool"},
		{"probe_output_tokens_exceeded", 0, "response_too_large"},
		{"arbitrary_secret_text", 0, "unavailable"},
	} {
		code, detail := modelIdentificationFailure(&gatewayproxy.ModelIdentificationError{Code: test.wire, UpstreamStatus: test.upstream, Cause: errors.New("secret-error-body")})
		if code != "model_identification_"+test.want || detail.UpstreamStatus != test.upstream {
			t.Fatalf("classification=%s %+v", code, detail)
		}
	}
}

func TestModelIdentificationProtocolMismatchIsServiceUnavailable(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	s.modelIdentificationOptionsError(w, httptest.NewRequest(http.MethodGet, "/admin/model-identifications/options", nil), &gatewayproxy.ModelIdentificationError{Code: "protocol_unsupported", Stage: "preflight"})
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "model_identification_protocol_unsupported") {
		t.Fatalf("protocol mismatch=%d %s", w.Code, w.Body.String())
	}
}

func TestModelIdentificationMetadataFailureClassification(t *testing.T) {
	for _, test := range []struct{ code, want string }{
		{"sidecar_timeout", "timeout"},
		{"upstream_quota_timeout", "timeout"},
		{"sidecar_unavailable", "network_failed"},
		{"sidecar_invalid_response", "invalid_response"},
		{"sidecar_auth_failed", "unavailable"},
		{"sidecar_auth_unavailable", "unavailable"},
		{"sidecar_account_registry_unavailable", "unavailable"},
		{"invalid_upstream_account", "account_not_found"},
		{"unrecognized_safe_code", "probe_failed"},
		{"raw upstream message", "probe_failed"},
	} {
		t.Run(test.code, func(t *testing.T) {
			err := &gatewayproxy.InternalAPIError{Code: test.code, StatusCode: http.StatusBadGateway, Cause: errors.New("private-sidecar-answer")}
			code, detail := modelIdentificationFailure(err)
			if code != "model_identification_"+test.want || detail.Source != "sidecar" || detail.UpstreamStatus != 0 || detail.RetryAfter != 0 {
				t.Fatalf("metadata classification=%s %+v", code, detail)
			}
			w := httptest.NewRecorder()
			(&Server{}).modelIdentificationOptionsError(w, httptest.NewRequest(http.MethodGet, "/admin/model-identifications/options", nil), err)
			if !strings.Contains(w.Body.String(), code) || strings.Contains(w.Body.String(), "private-sidecar-answer") {
				t.Fatalf("unsafe metadata failure=%s", w.Body.String())
			}
		})
	}
}
