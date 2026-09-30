package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/config"
)

const flashModel = "gemini-3.8-flash-high"

func listedModels(t *testing.T, response *httptest.ResponseRecorder) []string {
	t.Helper()
	var listing struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &listing) != nil || listing.Object != "list" || listing.Data == nil {
		t.Fatalf("invalid models response: %d %s", response.Code, response.Body)
	}
	models := []string{}
	for _, row := range listing.Data {
		models = append(models, row.ID)
	}
	return models
}

func managedModels(server http.Handler, user string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer "+testBridgeToken)
	if user != "" {
		r.Header.Set("X-Codex-Gateway-User", user)
	}
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	return w
}

func TestBridgeForwardsEveryExactModelAcrossProtocols(t *testing.T) {
	for _, model := range config.LegacyAntigravityModels() {
		t.Run(model, func(t *testing.T) {
			runner, capturePath := fakeRunner(t, fakeCLIConfig{
				Models: model + " Gemini model\n",
				Stream: strings.Replace(initEvent, CLIModel, model, 1) + resultEvent,
			})
			server := NewServer(runner, testBridgeToken)
			server.Refresh(context.Background())
			if got := listedModels(t, bridgeRequest(server, http.MethodGet, "/v1/models", "")); !reflect.DeepEqual(got, []string{model}) {
				t.Fatalf("discovered models=%v", got)
			}
			for _, test := range []struct{ path, body, field string }{
				{"/v1/responses", `{"model":"` + model + `","input":"hi"}`, "model"},
				{"/v1/responses", `{"model":"` + model + `","input":"hi","stream":true}`, "model"},
				{"/v1beta/models/" + model + ":generateContent", nativeText, "modelVersion"},
				{"/v1beta/models/" + model + ":streamGenerateContent?alt=sse", nativeText, "modelVersion"},
			} {
				response := bridgeRequest(server, http.MethodPost, test.path, test.body)
				if response.Code != 200 || !strings.Contains(response.Body.String(), `"`+test.field+`":"`+model+`"`) {
					t.Fatalf("response lost model: %d %s", response.Code, response.Body)
				}
				capture := readCapture(t, capturePath)
				if !strings.Contains(strings.Join(capture.Args, " "), "--model "+model+" ") {
					t.Fatalf("CLI did not receive requested model: %v", capture.Args)
				}
			}
			assertWorkspacesClean(t, runner)
		})
	}
}

func TestBridgeRejectsAliasesAndUnavailableModels(t *testing.T) {
	executor := &accountExecutor{models: []string{flashModel}}
	server := NewServer(executor, testBridgeToken)
	server.Refresh(context.Background())
	for _, model := range []string{"gemini-3.1-pro-preview", "gemini-3.1-pro-preview-customtools", "Gemini-3.1-pro-high", flashModel + "-next"} {
		response := bridgeRequest(server, http.MethodPost, "/v1/responses", `{"model":"`+model+`","input":"hi"}`)
		if response.Code != 400 {
			t.Fatalf("accepted Responses alias %q: %d", model, response.Code)
		}
		if _, failure := DecodeGeminiRequest(model, []byte(nativeText)); failure == nil || failure.Code != "antigravity_model_unsupported" {
			t.Fatalf("accepted native model %q", model)
		}
		response = bridgeRequest(server, http.MethodPost, "/v1beta/models/"+model+":generateContent", nativeText)
		if response.Code != 404 {
			t.Fatalf("accepted native alias %q: %d", model, response.Code)
		}
	}
	response := bridgeRequest(server, http.MethodPost, "/v1/responses", `{"model":"`+PublicModel+`","input":"hi"}`)
	if response.Code != 503 || executor.runs.Load() != 0 {
		t.Fatalf("unavailable model ran: response=%d runs=%d", response.Code, executor.runs.Load())
	}
}

func TestRunnerDiscoveryReturnsOnlyExactModelsInCatalogOrder(t *testing.T) {
	models := "other-model " + flashModel + "\n" + PublicModel + "-next Unsupported\n"
	catalog := config.LegacyAntigravityModels()
	for index := len(catalog) - 1; index >= 0; index-- {
		models += catalog[index] + " Gemini\n" + catalog[index] + " Duplicate\n"
	}
	runner, _ := fakeRunner(t, fakeCLIConfig{Models: models})
	got, err := runner.Check(context.Background())
	if err != nil || !reflect.DeepEqual(got, catalog) {
		t.Fatalf("models=%v want=%v err=%v", got, catalog, err)
	}
}

func TestRunnerRejectsDifferentSupportedModel(t *testing.T) {
	runner, _ := fakeRunner(t, fakeCLIConfig{Models: flashModel + " Flash\n", Stream: initEvent + resultEvent})
	result, failure := runner.Run(context.Background(), flashModel, "hi")
	if failure == nil || failure.Code != "upstream_protocol_error" || result.Response != "" {
		t.Fatalf("accepted substituted model: %+v %+v", result, failure)
	}
	before := runner.Credentials.(*runnerCredentials).restores
	_, failure = runner.Run(context.Background(), "gemini-3.1-pro-preview", "hi")
	if failure == nil || failure.Code != "antigravity_model_unsupported" || runner.Credentials.(*runnerCredentials).restores != before {
		t.Fatalf("unsupported model invoked CLI: %+v", failure)
	}
	assertWorkspacesClean(t, runner)
}

func TestAuthVerifySupportsFlashOnlyAccount(t *testing.T) {
	runner, capturePath := fakeRunner(t, fakeCLIConfig{
		Models: flashModel + " Flash\n", Usage: "usage",
		Stream: strings.Replace(initEvent, CLIModel, flashModel, 1) + resultEvent,
	})
	if err := runner.AuthVerify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if capture := readCapture(t, capturePath); !strings.Contains(strings.Join(capture.Args, " "), "--model "+flashModel+" ") {
		t.Fatalf("verification used unavailable model: %v", capture.Args)
	}
}

func TestAccountModelsRespectUserAccessAndSelectionCapabilities(t *testing.T) {
	var selected []string
	var mu sync.Mutex
	selector := testSelector(t, map[string]int64{accountID("default"): 1}, &selected, &mu)
	flash, pro := &accountExecutor{models: []string{flashModel}}, &accountExecutor{models: []string{PublicModel}}
	server := accountServer(t, selector.URL, flash, pro)
	if got := listedModels(t, managedModels(server, accountUserID)); !reflect.DeepEqual(got, []string{flashModel}) {
		t.Fatalf("unauthorized model exposed: %v", got)
	}
	if got := listedModels(t, managedModels(server, "")); !reflect.DeepEqual(got, orderedModels(modelSet([]string{flashModel, PublicModel}))) {
		t.Fatalf("internal union=%v", got)
	}
	if len(selected) != 0 || flash.runs.Load() != 0 || pro.runs.Load() != 0 {
		t.Fatal("model discovery reserved or ran an account")
	}
	response := managedRequest(server, context.Background(), "/v1/responses", accountRequestBody, accountUserID)
	if response.Code != 429 || pro.runs.Load() != 0 {
		t.Fatalf("unauthorized Pro ran: %d %s", response.Code, response.Body)
	}
	response = managedRequest(server, context.Background(), "/v1/responses", `{"model":"`+flashModel+`","input":"hi"}`, accountUserID)
	if response.Code != 200 || flash.runs.Load() != 1 || pro.runs.Load() != 0 || response.Header().Get("X-Codex-Upstream-Account") != accountID("default") {
		t.Fatalf("wrong selected account: %d %s", response.Code, response.Body)
	}
}

func TestAccountRetriesOnlyAccountsSupportingRequestedModel(t *testing.T) {
	var selected []string
	var mu sync.Mutex
	selector := testSelector(t, map[string]int64{accountID("default"): 1, accountID("work"): 1, accountID("backup"): 1}, &selected, &mu)
	bad := &accountExecutor{models: []string{flashModel}, failure: &Failure{429, "upstream_rate_limited", "quota exhausted"}}
	pro := &accountExecutor{models: []string{PublicModel}}
	good := &accountExecutor{models: []string{flashModel}}
	server := accountServer(t, selector.URL, bad, pro, good)
	response := managedRequest(server, context.Background(), "/v1/responses", `{"model":"`+flashModel+`","input":"hi"}`, accountUserID)
	mu.Lock()
	defer mu.Unlock()
	if response.Code != 200 || bad.runs.Load() != 1 || good.runs.Load() != 1 || pro.runs.Load() != 0 || !reflect.DeepEqual(selected, []string{accountID("default"), accountID("backup")}) {
		t.Fatalf("model capability ignored: response=%d selected=%v", response.Code, selected)
	}
}

func TestAccountSelectionRechecksModelAfterCallbacks(t *testing.T) {
	for _, stage := range []string{"eligible", "select"} {
		t.Run(stage, func(t *testing.T) {
			var server *Server
			ready := make(chan struct{})
			selector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				<-ready
				if strings.HasSuffix(r.URL.Path, "/"+stage) {
					server.manager.mu.Lock()
					server.manager.accounts[0].models = modelSet([]string{PublicModel})
					server.manager.mu.Unlock()
				}
				if strings.HasSuffix(r.URL.Path, "/eligible") {
					writeJSON(w, 200, map[string]any{"accounts": []map[string]any{{"id": accountID("default"), "concurrent_limit": 1}}})
				} else {
					writeJSON(w, 200, map[string]string{"account_id": accountID("default")})
				}
			}))
			defer selector.Close()
			executor := &accountExecutor{models: []string{flashModel}}
			server = accountServer(t, selector.URL, executor)
			close(ready)
			response := managedRequest(server, context.Background(), "/v1/responses", `{"model":"`+flashModel+`","input":"hi"}`, accountUserID)
			if response.Code != 429 || executor.runs.Load() != 0 || server.manager.accounts[0].active != 0 {
				t.Fatalf("lost model capability was used: %d %s", response.Code, response.Body)
			}
		})
	}
}

func TestAccountModelsRefreshRemovesLostCapabilities(t *testing.T) {
	changing, stable := &accountExecutor{models: []string{flashModel}}, &accountExecutor{}
	server := accountServer(t, "http://127.0.0.1:1", changing, stable)
	changing.models = []string{PublicModel}
	server.Refresh(context.Background())
	if got := listedModels(t, managedModels(server, "")); !reflect.DeepEqual(got, []string{PublicModel}) {
		t.Fatalf("removed capability retained: %v", got)
	}
	changing.models, changing.checkErr = []string{flashModel}, errors.New("synthetic check failure")
	server.Refresh(context.Background())
	if got := listedModels(t, managedModels(server, "")); !reflect.DeepEqual(got, []string{PublicModel}) {
		t.Fatalf("failed check retained capability: %v", got)
	}
	server.manager.mu.Lock()
	stableAccount := server.manager.accounts[1]
	stableAccount.cooldown = time.Now().Add(time.Minute)
	server.manager.mu.Unlock()
	if response := managedModels(server, ""); response.Code != 503 {
		t.Fatalf("cooldown model advertised: %d %s", response.Code, response.Body)
	}
}

func TestAccountModelDiscoveryFailsClosedOnIdentityAndCallback(t *testing.T) {
	server := accountServer(t, "http://127.0.0.1:1", &accountExecutor{})
	for _, user := range []string{accountUserID, "invalid", "11111111111141118111111111111111"} {
		if response := managedModels(server, user); response.Code != 503 {
			t.Fatalf("identity or callback failure exposed models: %d %s", response.Code, response.Body)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer "+testBridgeToken)
	r.Header.Add("X-Codex-Gateway-User", accountUserID)
	r.Header.Add("X-Codex-Gateway-User", accountUserID)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatalf("ambiguous identity exposed models: %d %s", w.Code, w.Body)
	}
	var selected []string
	var mu sync.Mutex
	selector := testSelector(t, map[string]int64{}, &selected, &mu)
	server = accountServer(t, selector.URL, &accountExecutor{})
	if got := listedModels(t, managedModels(server, accountUserID)); len(got) != 0 {
		t.Fatalf("model advertised without permitted accounts: %v", got)
	}
}

func TestAccountSmokeModelsAreScopedAndLoopbackOnly(t *testing.T) {
	server := accountServer(t, "http://127.0.0.1:1", &accountExecutor{models: []string{flashModel}}, &accountExecutor{})
	for _, test := range []struct {
		path, remote, token string
		status              int
	}{
		{"/internal/smoke/models/default", "127.0.0.1:1234", testBridgeToken, 200},
		{"/internal/smoke/models/default", "[::1]:1234", testBridgeToken, 200},
		{"/internal/smoke/models/default", "192.0.2.1:1234", testBridgeToken, 404},
		{"/internal/smoke/models/default", "127.0.0.1:1234", "invalid", 401},
		{"/internal/smoke/models/default?extra=1", "127.0.0.1:1234", testBridgeToken, 404},
		{"/internal/smoke/models/missing", "127.0.0.1:1234", testBridgeToken, 503},
	} {
		r := httptest.NewRequest(http.MethodGet, test.path, nil)
		r.RemoteAddr = test.remote
		r.Header.Set("Authorization", "Bearer "+test.token)
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != test.status {
			t.Fatalf("smoke models %s %s: %d %s", test.path, test.remote, w.Code, w.Body)
		}
		if test.status == 200 {
			if got := listedModels(t, w); !reflect.DeepEqual(got, []string{flashModel}) {
				t.Fatalf("another account's models leaked into smoke: %v", got)
			}
		}
	}
}
