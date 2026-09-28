package httpx

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRecoverPreservesIntentionalStreamAbort(t *testing.T) {
	w := httptest.NewRecorder()
	handler := Recover(slog.New(slog.NewTextHandler(io.Discard, nil)), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "data: partial\n\n")
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		if got := recover(); got != http.ErrAbortHandler {
			t.Fatalf("stream abort swallowed: %v", got)
		}
		if w.Body.String() != "data: partial\n\n" {
			t.Fatalf("recovery appended an ordinary response: %s", w.Body)
		}
	}()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
}
