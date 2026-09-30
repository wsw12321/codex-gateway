package cpamigrate

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestOAuthClient(t *testing.T, raw string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oauth-client.json")
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProviderUsesRuntimeOAuthClient(t *testing.T) {
	path := writeTestOAuthClient(t, `{"client_id":" configured-client ","client_secret":" configured-secret "}`)
	p, err := newProvider("http://127.0.0.1:9999", path)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	p.client.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		if r.Method != http.MethodPost || r.URL.String() != googleOAuthURL {
			t.Fatal("unexpected refresh request")
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("client_id") != "configured-client" || r.Form.Get("client_secret") != "configured-secret" || r.Form.Get("refresh_token") != "test-refresh" {
			t.Fatal("refresh did not use the configured OAuth client")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"access_token":"test-access","expires_in":3600}`))}, nil
	})
	fresh, err := p.refresh(context.Background(), token{Refresh: "test-refresh"})
	if err != nil || !called || fresh.Access != "test-access" || fresh.Refresh != "test-refresh" {
		t.Fatal("configured refresh failed")
	}
}

func TestProviderRejectsInvalidOAuthClient(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"malformed", `{"client_secret":"private-value"`},
		{"missing-id", `{"client_secret":"private-value"}`},
		{"missing-secret", `{"client_id":"private-value"}`},
		{"blank-id", `{"client_id":"  ","client_secret":"private-value"}`},
		{"blank-secret", `{"client_id":"private-value","client_secret":"\t"}`},
		{"wrong-type", `{"client_id":123,"client_secret":"private-value"}`},
		{"duplicate", `{"client_id":"private-value","client_id":"other","client_secret":"private-value"}`},
		{"trailing-json", `{"client_id":"private-value","client_secret":"private-value"}{}`},
		{"oversized", `{"client_id":"private-value","client_secret":"` + strings.Repeat("x", 16<<10) + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTestOAuthClient(t, tc.raw)
			p, err := newProvider("http://127.0.0.1:9999", path)
			if p != nil || err == nil || err.Error() != "google_oauth_client_file_invalid" {
				t.Fatal("invalid OAuth client must fail with a fixed diagnostic")
			}
		})
	}
}

func TestProviderRejectsMissingOrUnsafeOAuthClientFile(t *testing.T) {
	valid := writeTestOAuthClient(t, `{"client_id":"test-client-id","client_secret":"test-client-secret"}`)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(valid, link); err != nil {
		t.Fatal(err)
	}
	public := writeTestOAuthClient(t, `{"client_id":"test-client-id","client_secret":"test-client-secret"}`)
	if err := os.Chmod(public, 0644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path, reason string
	}{
		{"unspecified", "", "google_oauth_client_file_unavailable"},
		{"missing", filepath.Join(t.TempDir(), "missing"), "google_oauth_client_file_unavailable"},
		{"symlink", link, "google_oauth_client_file_unavailable"},
		{"directory", t.TempDir(), "google_oauth_client_file_invalid"},
		{"public", public, "google_oauth_client_file_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := newProvider("http://127.0.0.1:9999", tc.path)
			if p != nil || err == nil || err.Error() != tc.reason {
				t.Fatal("unsafe OAuth client file must fail with a fixed diagnostic")
			}
		})
	}
}
