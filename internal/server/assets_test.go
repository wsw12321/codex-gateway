package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDashboardAssetsAreNotCachedAcrossDeployments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		handler func(*Server, *httptest.ResponseRecorder)
	}{
		{
			name: "page",
			handler: func(server *Server, recorder *httptest.ResponseRecorder) {
				server.page(recorder, httptest.NewRequest("GET", "/", nil))
			},
		},
		{
			name: "javascript",
			handler: func(server *Server, recorder *httptest.ResponseRecorder) {
				server.javascript(recorder, httptest.NewRequest("GET", "/static/app.js", nil))
			},
		},
		{
			name: "stylesheet",
			handler: func(server *Server, recorder *httptest.ResponseRecorder) {
				server.stylesheet(recorder, httptest.NewRequest("GET", "/static/style.css", nil))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			test.handler(&Server{}, recorder)
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", got)
			}
		})
	}
}

func TestDashboardAssetsRemainDependencyFreeAndCSPCompatible(t *testing.T) {
	t.Parallel()

	html := string(indexHTML)
	javascript := string(appJS)
	stylesheet := string(styleCSS)
	for _, forbidden := range []struct {
		name  string
		value string
	}{
		{"external HTML URL", "https://"},
		{"external HTML URL", "http://"},
		{"inline style", "style="},
		{"inline click handler", "onclick="},
		{"inline submit handler", "onsubmit="},
	} {
		if strings.Contains(html, forbidden.value) {
			t.Fatalf("dashboard HTML contains %s", forbidden.name)
		}
	}
	for _, forbidden := range []string{
		"localStorage", "sessionStorage", "innerHTML", "insertAdjacentHTML", "eval(", "new Function",
	} {
		if strings.Contains(javascript, forbidden) {
			t.Fatalf("dashboard JavaScript contains forbidden construct %q", forbidden)
		}
	}
	if strings.Contains(stylesheet, "url(") || strings.Contains(stylesheet, "@import") {
		t.Fatal("dashboard stylesheet contains an external asset hook")
	}
	for _, required := range []string{
		`href="#overview"`, `href="#resources"`, `href="#keys"`, `href="#security"`, `href="#usage"`,
		`id="secret-dialog"`, `id="operation-status"`, `class="skip-link"`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("dashboard HTML is missing %s", required)
		}
	}
}
