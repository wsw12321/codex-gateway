package server

import (
	"net/http/httptest"
	"testing"
)

func TestDashboardAssetsAreNotCachedAcrossDeployments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		handler func(*Server, *httptest.ResponseRecorder)
	}{
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
