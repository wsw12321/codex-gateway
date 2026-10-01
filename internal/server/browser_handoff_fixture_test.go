package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"
)

// Opt-in, loopback-only fixture using the production handoff, decryption and
// CORS handlers. User/session lookup and model inference are test doubles.
// GATEWAY_BROWSER_FIXTURE_LISTEN=127.0.0.1:4180 go test -run '^TestBrowserHandoffBrowserFixture$' -timeout 20m ./internal/server
func TestBrowserHandoffBrowserFixture(t *testing.T) {
	address := os.Getenv("GATEWAY_BROWSER_FIXTURE_LISTEN")
	if address == "" {
		t.Skip("opt-in browser integration fixture")
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		t.Fatal("fixture listener must be a loopback IP")
	}
	s, repo, apiKey := newBrowserHandoffTestServer(t)
	s.config.PublicURL, _ = url.Parse("http://" + address)
	origin := os.Getenv("GATEWAY_BROWSER_FIXTURE_ORIGIN")
	if origin == "" {
		origin = "http://127.0.0.1:4174"
	}
	s.config.BrowserClientURL, _ = url.Parse(origin)
	var mu sync.Mutex
	modelIDs := []string{"gpt-6.1-sol"}
	modelFailures := 0
	exchanges, models, responses := 0, 0, 0
	exchangeCookies, modelAuthorizations := []string{}, []string{}
	stop := make(chan struct{})
	var stopOnce sync.Once
	s.mux.HandleFunc("POST /test/setup", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Models        []string `json:"models"`
			ModelFailures int      `json:"model_failures"`
			RememberKey   bool     `json:"remember_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, "invalid setup", 400)
			return
		}
		mu.Lock()
		if input.Models != nil {
			modelIDs = input.Models
		} else {
			modelIDs = []string{"gpt-6.1-sol"}
		}
		modelFailures = input.ModelFailures
		exchanges = 0
		models = 0
		responses = 0
		exchangeCookies = []string{}
		modelAuthorizations = []string{}
		mu.Unlock()
		recorder := httptest.NewRecorder()
		s.createBrowserHandoff(recorder, newBrowserHandoffIssueRequest(repo.user, repo.session, repo.key.ID, input.RememberKey))
		var result map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
			http.Error(w, "invalid issue response", 500)
			return
		}
		result["api_key_id"] = repo.key.ID
		result["api_key"] = apiKey
		writeJSON(w, recorder.Code, result)
	})
	s.mux.HandleFunc("GET /test/stats", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		writeJSON(w, 200, map[string]any{"exchanges": exchanges, "models": models, "responses": responses, "exchange_cookies": exchangeCookies, "model_authorizations": modelAuthorizations})
	})
	s.mux.HandleFunc("POST /test/stop", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204); stopOnce.Do(func() { close(stop) }) })
	s.mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		models++
		modelAuthorizations = append(modelAuthorizations, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "Bearer "+apiKey {
			http.Error(w, `{"error":{"message":"invalid key"}}`, 401)
			return
		}
		if modelFailures > 0 {
			modelFailures--
			http.Error(w, `{"error":{"message":"temporary model list failure"}}`, 503)
			return
		}
		data := make([]map[string]string, 0, len(modelIDs))
		for _, id := range modelIDs {
			data = append(data, map[string]string{"id": id, "object": "model"})
		}
		writeJSON(w, 200, map[string]any{"object": "list", "data": data})
	})
	s.mux.HandleFunc("POST /v1/responses", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		responses++
		mu.Unlock()
		http.Error(w, "inference disabled in fixture", 503)
	})
	production := s.Handler()
	server := &http.Server{Addr: address, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/browser-handoffs/exchange" && r.Method == "POST" {
			mu.Lock()
			exchanges++
			exchangeCookies = append(exchangeCookies, r.Header.Get("Cookie"))
			mu.Unlock()
		}
		production.ServeHTTP(w, r)
	})}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Logf("browser fixture ready at http://%s, client origin %s", address, origin)
	select {
	case <-stop:
	case err := <-done:
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}
