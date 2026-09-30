// Package cpamigrate implements an offline credential conversion. It reports
// bounded diagnostics and never includes credentials or provider bodies.
package cpamigrate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/wsw/codex-gateway/internal/config"
	"golang.org/x/sys/unix"
)

const (
	googleTokenURL    = "https://oauth2.googleapis.com/token"
	googleIdentityURL = "https://www.googleapis.com/oauth2/v2/userinfo?alt=json"
	googleProjectURL  = "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist"
	googleModelsURL   = "https://cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels"
)

type token struct {
	Access    string    `json:"access_token"`
	Refresh   string    `json:"refresh_token"`
	Type      string    `json:"token_type"`
	ExpiresIn int64     `json:"expires_in"`
	Expiry    time.Time `json:"expiry"`
}

type provider struct {
	client      *http.Client
	now         func() time.Time
	oauthClient googleOAuthClient
}

type googleOAuthClient struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// Read the OAuth client at runtime so credentials stay out of source and images.
// Return bounded diagnostics without exposing file contents or decoder errors.
func loadGoogleOAuthClient(path string) (googleOAuthClient, error) {
	const maxSize = 16 << 10
	var client googleOAuthClient
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return client, errors.New("google_oauth_client_file_unavailable")
	}
	f := os.NewFile(uintptr(fd), "google-oauth-client")
	defer f.Close()
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0077 != 0 || st.Size > maxSize {
		return client, errors.New("google_oauth_client_file_invalid")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil || len(raw) > maxSize || uniqueJSON(raw) != nil || json.Unmarshal(raw, &client) != nil {
		return googleOAuthClient{}, errors.New("google_oauth_client_file_invalid")
	}
	client.ClientID = strings.TrimSpace(client.ClientID)
	client.ClientSecret = strings.TrimSpace(client.ClientSecret)
	if client.ClientID == "" || client.ClientSecret == "" {
		return googleOAuthClient{}, errors.New("google_oauth_client_file_invalid")
	}
	return client, nil
}

func newProvider(proxyURL, oauthClientFile string) (*provider, error) {
	u, err := url.Parse(proxyURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("egress_proxy_required")
	}
	oauthClient, err := loadGoogleOAuthClient(oauthClientFile)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Explicit proxy always wins; NO_PROXY and environment values cannot bypass it.
	transport.Proxy = http.ProxyURL(u)
	return &provider{client: &http.Client{Transport: transport, Timeout: 45 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, now: time.Now, oauthClient: oauthClient}, nil
}

func (p *provider) request(ctx context.Context, method, endpoint, contentType, access string, body []byte, dst any) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("provider_request_invalid")
	}
	if access != "" {
		req.Header.Set("Authorization", "Bearer "+access)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("User-Agent", "antigravity/1.23.2 linux/amd64")
	client := *p.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("provider_unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("provider_rejected")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxFileSize+1))
	if err != nil || len(raw) > maxFileSize || uniqueJSON(raw) != nil || json.Unmarshal(raw, dst) != nil {
		return errors.New("provider_response_invalid")
	}
	return nil
}

func (p *provider) refresh(ctx context.Context, old token) (token, error) {
	if strings.TrimSpace(old.Refresh) == "" {
		return token{}, errors.New("refresh_token_missing")
	}
	form := url.Values{"client_id": {p.oauthClient.ClientID}, "client_secret": {p.oauthClient.ClientSecret}, "grant_type": {"refresh_token"}, "refresh_token": {old.Refresh}}
	var fresh token
	if err := p.request(ctx, http.MethodPost, googleTokenURL, "application/x-www-form-urlencoded", "", []byte(form.Encode()), &fresh); err != nil {
		return token{}, errors.New("refresh_failed_reauthorize")
	}
	if strings.TrimSpace(fresh.Access) == "" || fresh.ExpiresIn <= 0 || fresh.ExpiresIn > 86400 || (fresh.Type != "" && !strings.EqualFold(fresh.Type, "Bearer")) {
		return token{}, errors.New("refresh_response_invalid")
	}
	if fresh.Refresh == "" {
		fresh.Refresh = old.Refresh
	}
	fresh.Type = "Bearer"
	fresh.Expiry = p.now().UTC().Add(time.Duration(fresh.ExpiresIn) * time.Second)
	return fresh, nil
}

func (p *provider) identity(ctx context.Context, t token) (string, string, error) {
	var v struct {
		Subject  string `json:"id"`
		Email    string `json:"email"`
		Verified bool   `json:"verified_email"`
	}
	if err := p.request(ctx, http.MethodGet, googleIdentityURL, "", t.Access, nil, &v); err != nil {
		return "", "", errors.New("identity_verification_failed")
	}
	if !v.Verified || !subjectPattern.MatchString(v.Subject) || len(v.Email) > 254 || !strings.Contains(v.Email, "@") || strings.ContainsAny(v.Email, "\r\n\x00") {
		return "", "", errors.New("identity_unverified")
	}
	return v.Subject, v.Email, nil
}

func (p *provider) project(ctx context.Context, t token) (string, error) {
	var v map[string]json.RawMessage
	body := []byte(`{"metadata":{"ideType":"ANTIGRAVITY"}}`)
	if err := p.request(ctx, http.MethodPost, googleProjectURL, "application/json", t.Access, body, &v); err != nil {
		return "", errors.New("project_verification_failed")
	}
	for _, key := range []string{"cloudaicompanionProject", "projectId", "project"} {
		var id string
		if json.Unmarshal(v[key], &id) != nil {
			var obj struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(v[key], &obj) == nil {
				id = obj.ID
			}
		}
		if projectPattern.MatchString(id) {
			return id, nil
		}
	}
	return "", errors.New("project_unavailable")
}

func (p *provider) models(ctx context.Context, t token, project string) ([]string, error) {
	body, _ := json.Marshal(map[string]string{"project": project})
	var v struct {
		Models map[string]struct {
			Quota *struct {
				Remaining *float64 `json:"remainingFraction"`
			} `json:"quotaInfo"`
		} `json:"models"`
	}
	if err := p.request(ctx, http.MethodPost, googleModelsURL, "application/json", t.Access, body, &v); err != nil {
		return nil, errors.New("quota_verification_failed")
	}
	var models []string
	for model, value := range v.Models {
		if config.IsAntigravityModel(model) && value.Quota != nil && value.Quota.Remaining != nil && *value.Quota.Remaining >= 0 && *value.Quota.Remaining <= 1 {
			models = append(models, model)
		}
	}
	if len(models) == 0 {
		return nil, errors.New("reviewed_models_or_quota_unavailable")
	}
	sort.Strings(models)
	return models, nil
}
