package cpamigrate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/antigravity"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testProvider(t *testing.T, identity string) *provider {
	t.Helper()
	return &provider{oauthClient: googleOAuthClient{ClientID: "test-client-id", ClientSecret: "test-client-secret"}, now: func() time.Time { return time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC) }, client: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.String() {
		case googleOAuthURL:
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("client_id") != "test-client-id" || r.Form.Get("client_secret") != "test-client-secret" || r.Form.Get("refresh_token") == "" {
				t.Error("did not force CPA client refresh")
			}
			body = `{"access_token":"fresh-access-private","refresh_token":"rotated-refresh-private","token_type":"Bearer","expires_in":3600}`
		case googleIdentityURL:
			body = identity
		case googleProjectURL:
			body = `{"cloudaicompanionProject":{"id":"verified-project"}}`
		case googleModelsURL:
			body = `{"models":{"gemini-pro-agent":{"quotaInfo":{"remainingFraction":0.5}},"claude-out-of-scope":{"quotaInfo":{"remainingFraction":1}},"gemini-3.8-flash-high":{"quotaInfo":{"remainingFraction":0}}}}`
		default:
			t.Fatalf("unexpected destination %s", r.URL.String())
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})}}
}

type fakeKeyring struct {
	raw               []byte
	saves             int
	verifyErr         error
	latestAfterVerify bool
}

func (f *fakeKeyring) Restore(_ context.Context, _ string, home string) error {
	dir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, legacyFilename), f.raw, 0600)
}
func (f *fakeKeyring) Save(_ context.Context, _ string, home string) error {
	f.saves++
	raw, err := os.ReadFile(filepath.Join(home, ".gemini", "antigravity-cli", legacyFilename))
	f.raw = raw
	return err
}
func (f *fakeKeyring) Verify(_ context.Context, _ string, _ string) error {
	if f.latestAfterVerify {
		f.raw = []byte(strings.ReplaceAll(string(f.raw), "rotated-refresh-private", "legacy-newest-refresh-private"))
	}
	return f.verifyErr
}

const legacyFixture = `{"token":{"access_token":"old-still-valid-private","refresh_token":"original-refresh-private","token_type":"Bearer","expiry":"2099-01-01T00:00:00Z","unknown_token_field":true},"project_id":"old-project","unknown_metadata":{"retained":true}}`

func fixtureMigrator(t *testing.T, identity string) (*migrator, *fakeKeyring, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	oauth, err := openDirectory(dir, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(oauth.close)
	keys := &fakeKeyring{raw: []byte(legacyFixture)}
	m := &migrator{provider: testProvider(t, identity), keys: keys, oauth: oauth, identities: identityMap{1, map[string]string{}}, legacyUID: os.Geteuid(), temp: t.TempDir()}
	return m, keys, dir
}

func TestForwardForcesRefreshPreservesStableIdentityAndStagesDisabled(t *testing.T) {
	m, keys, dir := fixtureMigrator(t, `{"id":"123456789","email":"user@example.test","verified_email":true}`)
	record := antigravity.AccountRecord{ID: "0123456789abcdef", Name: "default", Enabled: false}
	report := Report{}
	if err := m.forward(context.Background(), record, t.TempDir(), &report); err != nil {
		t.Fatal(err)
	}
	if keys.saves != 1 || !report.Refreshed || !strings.Contains(string(keys.raw), "rotated-refresh-private") || !strings.Contains(string(keys.raw), "unknown_token_field") {
		t.Fatal("rotated tokens or unrelated metadata not saved")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "antigravity-"+record.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var cpa map[string]any
	if json.Unmarshal(raw, &cpa) != nil || cpa["google_subject"] != "123456789" || cpa["disabled"] != true || cpa["project_id"] != "verified-project" {
		t.Fatalf("incorrect CPA metadata")
	}
	if m.identities.Accounts["123456789"] != record.ID || len(report.Models) != 2 {
		t.Fatal("verified mapping/models missing")
	}
	serialized, _ := json.Marshal(report)
	if strings.Contains(string(serialized), "private") || strings.Contains(string(serialized), "example.test") {
		t.Fatal("secret identity leaked into report")
	}
}

func TestForwardFailureRetainsRotatedCredentialsAndRejectsConflict(t *testing.T) {
	for _, kind := range []string{"unverified", "conflict"} {
		t.Run(kind, func(t *testing.T) {
			identity := `{"id":"123456789","email":"user@example.test","verified_email":true}`
			if kind == "unverified" {
				identity = `{"id":"123456789","email":"user@example.test","verified_email":false}`
			}
			m, keys, dir := fixtureMigrator(t, identity)
			if kind == "conflict" {
				m.identities.Accounts["123456789"] = "fedcba9876543210"
			}
			err := m.forward(context.Background(), antigravity.AccountRecord{ID: "0123456789abcdef", Name: "default"}, t.TempDir(), &Report{})
			if err == nil || strings.Contains(err.Error(), "private") {
				t.Fatalf("failure was not redacted: %v", err)
			}
			if keys.saves != 1 || !strings.Contains(string(keys.raw), "rotated-refresh-private") {
				t.Fatal("failed validation lost refreshed token")
			}
			if _, err := os.Stat(filepath.Join(dir, "antigravity-0123456789abcdef.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed identity produced active credential")
			}
			if _, err := os.Stat(filepath.Join(dir, ".gateway-migration-recovery-0123456789abcdef")); err != nil {
				t.Fatal("rotated recovery token not durable")
			}
		})
	}
}

func TestReverseFindsRenamedCredentialAndKeepsLatestAfterFailure(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "verified", true: "failed-after-refresh"}[failed], func(t *testing.T) {
			m, keys, dir := fixtureMigrator(t, `{"id":"123456789","email":"user@example.test","verified_email":true}`)
			m.identities.Accounts["123456789"] = "0123456789abcdef"
			raw := []byte(`{"type":"antigravity","google_subject":"123456789","email":"user@example.test","refresh_token":"cpa-current-refresh-private","access_token":"cpa-current-access-private","project_id":"old-project","disabled":true}`)
			if err := m.oauth.write("renamed-auth.json", raw); err != nil {
				t.Fatal(err)
			}
			keys.latestAfterVerify = true
			if failed {
				keys.verifyErr = errors.New("failed after refresh")
			}
			err := m.reverse(context.Background(), antigravity.AccountRecord{ID: "0123456789abcdef", Name: "default"}, t.TempDir(), &Report{})
			if (err != nil) != failed {
				t.Fatalf("reverse result=%v", err)
			}
			for _, text := range []string{string(keys.raw), func() string { raw, _ := os.ReadFile(filepath.Join(dir, "renamed-auth.json")); return string(raw) }()} {
				if !strings.Contains(text, "legacy-newest-refresh-private") {
					t.Fatal("newest legacy refresh was not retained in both stores")
				}
			}
			if !strings.Contains(string(keys.raw), "unknown_metadata") || !strings.Contains(string(keys.raw), "verified-project") {
				t.Fatal("reverse lost legacy metadata or verified project")
			}
		})
	}
}

func TestPrivateFilesRejectLinksAndSerializeRefresher(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	d, err := openDirectory(dir, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	if err := os.WriteFile(filepath.Join(dir, "real"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.read("link"); err == nil {
		t.Fatal("accepted symlink")
	}
	if err := d.write("link", []byte("replacement")); err == nil {
		t.Fatal("replaced symlink")
	}
	if err := os.Link(filepath.Join(dir, "real"), filepath.Join(dir, "hard")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.read("real"); err == nil {
		t.Fatal("accepted hardlink")
	}
	lock, err := d.lock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if second, err := d.lock(); err == nil {
		second.Close()
		t.Fatal("live refresher lock bypassed")
	}
}

func TestProviderRefusesRedirectsAndProxyBypass(t *testing.T) {
	for _, proxy := range []string{"", "socks5://localhost:9999", "http://user:password@localhost:9999", "http://localhost:9999/path"} {
		if _, err := newProvider(proxy, ""); err == nil || err.Error() != "egress_proxy_required" {
			t.Fatalf("accepted invalid proxy %q", proxy)
		}
	}
	p, err := newProvider("http://127.0.0.1:9999", writeTestOAuthClient(t, `{"client_id":"test-client-id","client_secret":"test-client-secret"}`))
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost, googleOAuthURL, nil)
	proxy, err := p.client.Transport.(*http.Transport).Proxy(request)
	if err != nil || proxy.Host != "127.0.0.1:9999" {
		t.Fatal("explicit proxy missing")
	}
	if err := p.client.CheckRedirect(request, nil); err != http.ErrUseLastResponse {
		t.Fatal("redirects allowed")
	}
	if err := uniqueJSON([]byte(`{"token":{"refresh_token":"a","refresh_token":"b"}}`)); err == nil {
		t.Fatal("duplicate credential fields accepted")
	}
}
