package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/wsw/codex-gateway/internal/cpaprotocol"
	"github.com/wsw/codex-gateway/internal/httpx"
	"github.com/wsw/codex-gateway/internal/store"
)

//go:embed assets/cpa/*
var cpaAssets embed.FS

const cpaOAuthTTL = 5 * time.Minute

type cpaOAuthAttempt struct {
	SessionID string
	Provider  string
	State     string
	ExpiresAt time.Time
	Submitted bool
}

type cpaAdminState struct {
	mu       sync.Mutex
	attempts map[string]cpaOAuthAttempt
	client   *http.Client
}

func (s *Server) cpaAdminRoutes() {
	s.cpaAdmin = &cpaAdminState{attempts: make(map[string]cpaOAuthAttempt), client: &http.Client{
		Timeout:       30 * time.Second,
		Transport:     &http.Transport{Proxy: nil, MaxIdleConns: 10, IdleConnTimeout: 30 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	read := func(h http.HandlerFunc) http.Handler { return s.requireSession(s.ownerOnly(httpx.NoStore(h))) }
	write := func(h http.HandlerFunc) http.Handler {
		return s.browserOrigin(s.requireRecentVerification(s.ownerOnly(s.cpaAudit(h))))
	}
	s.mux.Handle("GET /admin/cpa/{$}", read(s.cpaPage))
	s.mux.Handle("GET /admin/cpa/assets/{file}", read(s.cpaAsset))
	s.mux.Handle("GET /admin/cpa/api/{provider}/accounts", read(s.cpaAccounts))
	s.mux.Handle("GET /admin/cpa/api/{provider}/concurrency", read(s.cpaConcurrency))
	s.mux.Handle("PUT /admin/cpa/api/{provider}/accounts/{id}/status", write(s.cpaStatus))
	s.mux.Handle("POST /admin/cpa/api/{provider}/oauth", write(s.cpaOAuthBegin))
	s.mux.Handle("GET /admin/cpa/api/oauth/{id}/status", read(s.cpaOAuthStatus))
	s.mux.Handle("POST /admin/cpa/api/oauth/{id}/callback", write(s.cpaOAuthCallback))
	s.mux.Handle("POST /admin/cpa/api/{provider}/credentials", write(s.cpaImport))
	s.mux.Handle("POST /admin/cpa/api/{provider}/accounts/{id}/refresh", write(s.cpaRefresh))
	s.mux.Handle("POST /admin/cpa/api/{provider}/accounts/{id}/quota", write(s.cpaQuota))
	s.mux.Handle("DELETE /admin/cpa/api/{provider}/accounts/{id}", write(s.cpaDelete))
	// A catch-all prevents the dashboard fallback from presenting forbidden API
	// paths as successful HTML. This is an allowlist, never a reverse proxy.
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
		s.mux.Handle(method+" /admin/cpa/", read(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	}
}

func (s *Server) cpaAudit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		if s.store == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Second)
		defer cancel()
		// No URL query, body, authorization state/code or provider response is
		// written to the audit trail. Stable account IDs are the only subjects.
		subject := r.PathValue("id")
		if !validUpstreamAccountID(subject) {
			subject = ""
		}
		_, err := s.store.AppendAuditEvent(ctx, store.AppendAuditEventParams{
			ActorUserID: userFrom(r.Context()).ID, ActorSessionID: sessionFrom(r.Context()).ID,
			OccurredAt: time.Now().UTC(), EventType: "cpa.credentials.operation", Severity: "info",
			Success: recorder.status >= 200 && recorder.status < 300, SourceIP: safeIP(r.Context()),
			SubjectType: "upstream_account", SubjectID: subject, RequestID: httpx.RequestID(r.Context()),
			Metadata: map[string]any{"provider": r.PathValue("provider"), "method": r.Method, "route": r.Pattern, "http_status": recorder.status},
		})
		if err != nil && s.logger != nil {
			s.logger.Error("CPA audit unavailable", "code", "cpa_audit_failed")
		}
	})
}

func (s *Server) cpaPage(w http.ResponseWriter, r *http.Request) {
	data, err := cpaAssets.ReadFile("assets/cpa/index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) cpaAsset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	contentType := ""
	switch name {
	case "panel.js":
		contentType = "text/javascript; charset=utf-8"
	case "panel.css":
		contentType = "text/css; charset=utf-8"
	default:
		http.NotFound(w, r)
		return
	}
	data, err := cpaAssets.ReadFile("assets/cpa/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(data)
}

func cpaProvider(provider string) bool { return provider == "codex" || provider == "antigravity" }

func (s *Server) cpaAccountRequest(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	provider := r.PathValue("provider")
	if !cpaProvider(provider) || (provider == "antigravity" && s.antigravity == nil) {
		http.NotFound(w, r)
		return nil, false
	}
	if provider == "antigravity" && !s.config.UsesCPAAntigravity() {
		httpx.WriteError(w, r, http.StatusServiceUnavailable, "upstream_error", "cpa_antigravity_not_active", "当前使用旧 bridge 回滚链路，请在 Gateway 原账号页面管理状态；恢复 CPA 后再维护凭据")
		return nil, false
	}
	request := r.Clone(r.Context())
	request.URL = new(url.URL)
	*request.URL = *r.URL
	request.URL.Path = "/admin/upstream-accounts"
	if provider == "antigravity" {
		request.URL.Path = "/admin/antigravity-accounts"
	}
	return request, true
}

func (s *Server) cpaAccounts(w http.ResponseWriter, r *http.Request) {
	if rr, ok := s.cpaAccountRequest(w, r); ok {
		s.upstreamAccountsJSON(w, rr)
	}
}
func (s *Server) cpaConcurrency(w http.ResponseWriter, r *http.Request) {
	if rr, ok := s.cpaAccountRequest(w, r); ok {
		s.upstreamAccountConcurrency(w, rr)
	}
}
func (s *Server) cpaStatus(w http.ResponseWriter, r *http.Request) {
	if rr, ok := s.cpaAccountRequest(w, r); ok {
		s.setUpstreamAccountStatus(w, rr)
	}
}

// cpaCall constructs every request from a fixed server-side route. No browser
// URL, cookie, authorization header, or upstream response header is forwarded.
func (s *Server) cpaCall(ctx context.Context, method, path string, input, output any) error {
	if s.config.CPAManagementToken == "" || s.config.SidecarURL == nil || s.cpaAdmin == nil {
		return errors.New("cpa management unavailable")
	}
	// A rollback activates the bridge as the sole Google credential refresher.
	// Never mutate or refresh the stopped CPA credential set through this UI.
	if strings.HasPrefix(path, "antigravity/") && !s.config.UsesCPAAntigravity() {
		return errors.New("cpa antigravity inactive")
	}
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	target := *s.config.SidecarURL
	target.Path = strings.TrimRight(target.Path, "/") + "/internal/gateway-management/" + path
	target.RawPath, target.RawQuery, target.Fragment = "", "", ""
	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.config.CPAManagementToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.cpaAdmin.client.Do(req)
	if err != nil {
		return errors.New("cpa request unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errors.New("cpa request rejected")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return errors.New("cpa response invalid")
	}
	if output == nil {
		return nil
	}
	return json.Unmarshal(data, output)
}

func cpaError(w http.ResponseWriter, r *http.Request) {
	httpx.WriteError(w, r, http.StatusBadGateway, "upstream_error", "cpa_management_unavailable", "CPA 账号管理操作未完成，请检查服务状态后重试")
}

func cpaJSON(w http.ResponseWriter, r *http.Request, v any, limit int64, allowed ...string) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || r.URL.RawQuery != "" {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_cpa_request", "请求必须为 JSON，且不能包含查询参数")
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		badJSON(w, r, err)
		return false
	}
	fields, err := cpaprotocol.Object(body)
	if err != nil {
		badJSON(w, r, err)
		return false
	}
	for key := range fields {
		known := false
		for _, name := range allowed {
			if key == name {
				known = true
				break
			}
		}
		if !known {
			badJSON(w, r, errors.New("unknown field"))
			return false
		}
	}
	if err := json.Unmarshal(body, v); err != nil {
		badJSON(w, r, err)
		return false
	}
	return true
}

func (s *Server) cpaOAuthBegin(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	if !cpaProvider(provider) {
		http.NotFound(w, r)
		return
	}
	var input struct{}
	if !cpaJSON(w, r, &input, 128) {
		return
	}
	var result struct {
		URL   string `json:"url"`
		State string `json:"state"`
	}
	if err := s.cpaCall(r.Context(), http.MethodPost, provider+"/oauth", input, &result); err != nil {
		cpaError(w, r)
		return
	}
	if !validCPAOAuthURL(provider, result.URL, result.State) {
		cpaError(w, r)
		return
	}
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		cpaError(w, r)
		return
	}
	id := hex.EncodeToString(random[:])
	now := time.Now()
	s.cpaAdmin.mu.Lock()
	for key, attempt := range s.cpaAdmin.attempts {
		if !attempt.ExpiresAt.After(now) {
			delete(s.cpaAdmin.attempts, key)
		}
	}
	if len(s.cpaAdmin.attempts) >= 256 {
		s.cpaAdmin.mu.Unlock()
		cpaError(w, r)
		return
	}
	s.cpaAdmin.attempts[id] = cpaOAuthAttempt{SessionID: sessionFrom(r.Context()).ID, Provider: provider, State: result.State, ExpiresAt: now.Add(cpaOAuthTTL)}
	s.cpaAdmin.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "url": result.URL, "expires_in": int(cpaOAuthTTL.Seconds())})
}

func validCPAOAuthURL(provider, raw, state string) bool {
	if len(state) < 16 || len(state) > 256 || len(raw) > 16384 {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Port() != "" || u.Query().Get("state") != state {
		return false
	}
	return (provider == "codex" && u.Host == "auth.openai.com") || (provider == "antigravity" && u.Host == "accounts.google.com")
}

func (s *cpaAdminState) oauthAttempt(id, sessionID string, consume bool, now time.Time) (cpaOAuthAttempt, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.attempts[id]
	if !ok || !a.ExpiresAt.After(now) || a.SessionID != sessionID || (consume && a.Submitted) {
		return cpaOAuthAttempt{}, false
	}
	if consume {
		a.Submitted = true
		s.attempts[id] = a
	}
	return a, true
}

func (s *Server) cpaOAuthCallback(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Code  string `json:"code"`
		State string `json:"state"`
	}
	if !cpaJSON(w, r, &input, 16384, "code", "state") {
		return
	}
	a, ok := s.cpaAdmin.oauthAttempt(r.PathValue("id"), sessionFrom(r.Context()).ID, false, time.Now())
	if !ok || len(input.Code) == 0 || len(input.Code) > 8192 || subtle.ConstantTimeCompare([]byte(input.State), []byte(a.State)) != 1 {
		httpx.WriteError(w, r, http.StatusConflict, "invalid_request_error", "cpa_oauth_invalid", "授权流程已过期、已提交或不属于当前会话，请重新授权")
		return
	}
	// Consume before sending: a timeout may happen after CPA accepted the code.
	if _, ok := s.cpaAdmin.oauthAttempt(r.PathValue("id"), sessionFrom(r.Context()).ID, true, time.Now()); !ok {
		http.Error(w, "authorization already submitted", http.StatusConflict)
		return
	}
	if err := s.cpaCall(r.Context(), http.MethodPost, a.Provider+"/oauth/callback", map[string]string{"state": a.State, "code": input.Code}, nil); err != nil {
		cpaError(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "submitted"})
}

func (s *Server) cpaOAuthStatus(w http.ResponseWriter, r *http.Request) {
	a, ok := s.cpaAdmin.oauthAttempt(r.PathValue("id"), sessionFrom(r.Context()).ID, false, time.Now())
	if !ok || r.URL.RawQuery != "" {
		http.NotFound(w, r)
		return
	}
	var result struct {
		Status string `json:"status"`
	}
	if err := s.cpaCall(r.Context(), http.MethodPost, a.Provider+"/oauth/status", map[string]string{"state": a.State}, &result); err != nil {
		cpaError(w, r)
		return
	}
	if result.Status != "ok" && result.Status != "wait" && result.Status != "error" {
		cpaError(w, r)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// Tokens are accepted only for import; no read operation returns credentials.
// Explicit fields reject CPA per-file proxy, priority, prefix and API-key
// metadata that could bypass the Gateway's allocation and routing policy.
type cpaCredentialImport struct {
	RefreshToken string `json:"refresh_token"`
	AccessToken  string `json:"access_token,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
}

func (s *Server) cpaImport(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	if !cpaProvider(provider) {
		http.NotFound(w, r)
		return
	}
	var input cpaCredentialImport
	if !cpaJSON(w, r, &input, 128<<10, "refresh_token", "access_token", "id_token") {
		return
	}
	if strings.TrimSpace(input.RefreshToken) == "" || len(input.RefreshToken) > 32768 || len(input.AccessToken) > 32768 || len(input.IDToken) > 32768 {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_cpa_credential", "必须提供可刷新的 OAuth 凭据")
		return
	}
	if err := s.cpaCall(r.Context(), http.MethodPost, provider+"/credentials", input, nil); err != nil {
		cpaError(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) cpaRefresh(w http.ResponseWriter, r *http.Request) {
	s.cpaAccountOperation(w, r, "refresh")
}
func (s *Server) cpaQuota(w http.ResponseWriter, r *http.Request) {
	s.cpaAccountOperation(w, r, "quota")
}

func (s *Server) cpaAccountOperation(w http.ResponseWriter, r *http.Request, operation string) {
	provider, id := r.PathValue("provider"), r.PathValue("id")
	if !cpaProvider(provider) || !validUpstreamAccountID(id) {
		http.NotFound(w, r)
		return
	}
	var input struct{}
	if !cpaJSON(w, r, &input, 128) {
		return
	}
	// The facade returns only a fixed normalized schema, never raw provider
	// responses or the auth record returned by CPA's stock refresh handler.
	var result struct {
		Status string           `json:"status"`
		Quota  []cpaQuotaWindow `json:"quota,omitempty"`
	}
	if err := s.cpaCall(r.Context(), http.MethodPost, provider+"/accounts/"+id+"/"+operation, input, &result); err != nil {
		cpaError(w, r)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type cpaQuotaWindow struct {
	Model             string  `json:"model"`
	RemainingFraction float64 `json:"remaining_fraction"`
	ResetTime         string  `json:"reset_time,omitempty"`
}

func (s *Server) cpaDelete(w http.ResponseWriter, r *http.Request) {
	rr, ok := s.cpaAccountRequest(w, r)
	if !ok || !validUpstreamAccountID(r.PathValue("id")) {
		if ok {
			http.NotFound(w, r)
		}
		return
	}
	var input struct{}
	if !cpaJSON(w, r, &input, 128) {
		return
	}
	// Persist the same business control state used by the main dashboard. The
	// CPA facade rechecks disabled + zero active requests immediately before
	// deletion; an unavailable concurrency snapshot always prevents removal.
	client := s.accountClient(rr)
	id := r.PathValue("id")
	s.upstreamAccountSyncMu.Lock()
	defer s.upstreamAccountSyncMu.Unlock()
	if _, err := client.SetUpstreamAccountStatus(r.Context(), id, false); err != nil {
		writeUpstreamManagementError(w, r, err, "无法停用账号")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	for {
		snapshot, err := client.UpstreamAccountConcurrency(ctx)
		if err != nil {
			writeUpstreamManagementError(w, r, err, "无法确认账号请求已结束")
			return
		}
		found, active := false, int64(0)
		for _, account := range snapshot.Accounts {
			if account.ID == id {
				found = true
				active = account.ActiveRequests
			}
		}
		if !found {
			cpaError(w, r)
			return
		}
		if active == 0 {
			break
		}
		select {
		case <-ctx.Done():
			httpx.WriteError(w, r, http.StatusConflict, "invalid_request_error", "cpa_account_draining", "账号已停用，仍有请求运行，请稍后重试删除")
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
	if err := s.cpaCall(ctx, http.MethodDelete, r.PathValue("provider")+"/accounts/"+id, input, nil); err != nil {
		cpaError(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
