package gatewayplugin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Credential operations of the credential page. Each one is a call to CPA's
// own management API on its loopback listener: CPA runs the login, the token
// exchange, the refresh and the deletion, and builds every request a provider
// sees. This plugin only chooses the egress beforehand, identifies the
// resulting credential from the fields CPA stored, and binds it to its
// account. The Gateway's contract is the source-patched image's
// /internal/gateway-management/* one.
const (
	oauthSessionTTL           = 10 * time.Minute
	maxOAuthSessions          = 64
	maxCredentialRequestBytes = 128 << 10
	maxCredentialTokenBytes   = 32 << 10
	maxOAuthCodeBytes         = 8 << 10
	maxLoopbackResponseBytes  = 1 << 20
	credentialCallTimeout     = 60 * time.Second
	// importSettleTimeout bounds how long an import waits for CPA to list the
	// refreshed credential.
	importSettleTimeout = 15 * time.Second
	importSettlePoll    = 200 * time.Millisecond
)

var (
	errCredentialPending  = errors.New("credential not listed yet")
	errCredentialIdentity = errors.New("credential identity unavailable")
	errCredentialConflict = errors.New("credential identity conflict")
	errCredentialEgress   = errors.New("credential egress not committed")
)

// credentialRefusal is a login or import the Gateway refused to commit to its
// account (see RegistrationRefusedError). Its code reaches the person who
// started it.
type credentialRefusal struct{ code string }

func (e *credentialRefusal) Error() string { return "credential refused: " + e.code }

// cpaOAuthProviders names each provider the way CPA's login dispatcher does,
// with the only host the authorization URL may point to.
var cpaOAuthProviders = map[string]struct{ name, host string }{
	ProviderCodex:       {"codex", "auth.openai.com"},
	ProviderClaude:      {"claude", "claude.ai"},
	ProviderAntigravity: {"antigravity", "accounts.google.com"},
}

// oauthSession is one login the Owner started through this plugin.
type oauthSession struct {
	provider       string
	registrationID string
	// startedAt bounds the credential listing fallback used when CPA marks a
	// built-in OAuth session complete without returning auth_id. A credential
	// that predates this login must never be adopted by that login.
	startedAt time.Time
	// existing names the provider's credential files when the login began. A
	// refused login deletes only a file that is not among them: CPA may have
	// rewritten an existing account's own file in place.
	existing map[string]bool
	expires  time.Time
	// accountID is set once the login produced an identified account, so a
	// repeated status poll gets the same answer.
	accountID string
}

type oauthSessions struct {
	mu      sync.Mutex
	byState map[string]oauthSession
}

func newOAuthSessions() *oauthSessions {
	return &oauthSessions{byState: map[string]oauthSession{}}
}

func (s *oauthSessions) add(state string, session oauthSession, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, existing := range s.byState {
		if !existing.expires.After(now) {
			delete(s.byState, key)
		}
	}
	if _, exists := s.byState[state]; exists || len(s.byState) >= maxOAuthSessions {
		return false
	}
	s.byState[state] = session
	return true
}

func (s *oauthSessions) get(state, provider string, now time.Time) (oauthSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.byState[state]
	if !ok || session.provider != provider || !session.expires.After(now) {
		return oauthSession{}, false
	}
	return session, true
}

func (s *oauthSessions) complete(state, accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session, ok := s.byState[state]; ok {
		session.accountID = accountID
		s.byState[state] = session
	}
}

func (s *oauthSessions) drop(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byState, state)
}

// credentialError is the fixed answer to a failed credential operation.
func credentialError(status int, code string) []byte {
	return managementJSON(status, map[string]string{"status": "error", "error": code})
}

// decodeCredentialRequest parses one JSON object whose keys are all in
// allowed. Every value must be a string.
func decodeCredentialRequest(body []byte, allowed ...string) (map[string]string, bool) {
	if len(body) > maxCredentialRequestBytes {
		return nil, false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return map[string]string{}, true
	}
	fields, err := decodeObject(trimmed)
	if err != nil {
		return nil, false
	}
	out := make(map[string]string, len(fields))
	for key, raw := range fields {
		known := false
		for _, name := range allowed {
			known = known || key == name
		}
		var value string
		if !known || json.Unmarshal(raw, &value) != nil {
			return nil, false
		}
		out[key] = value
	}
	return out, true
}

// cleanToken accepts a token without whitespace or control characters.
func cleanToken(value string, limit int) bool {
	if value == "" || len(value) > limit {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] <= 0x20 || value[i] >= 0x7f {
			return false
		}
	}
	return true
}

// validOAuthState accepts the opaque state CPA generated.
func validOAuthState(state string) bool {
	if len(state) < 16 || len(state) > 256 {
		return false
	}
	for i := 0; i < len(state); i++ {
		c := state[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~') {
			return false
		}
	}
	return true
}

// validOAuthURL checks that CPA's authorization URL points at the provider's
// fixed login host and carries the session's state.
func validOAuthURL(provider, raw, state string) bool {
	expected, ok := cpaOAuthProviders[provider]
	if !ok || len(raw) > 16384 {
		return false
	}
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Host == expected.host && parsed.User == nil &&
		parsed.Fragment == "" && parsed.Query().Get("state") == state
}

// loginProxy is the egress a login or an import must use: the route the
// Gateway prepared for the registration, or the global egress written out.
// Either way the credential CPA saves carries it, and nothing reaches a
// provider before an egress is fixed.
func (p *Plugin) loginProxy(raw string) (string, error) {
	if raw == "" {
		return p.accountProxy(Egress{})
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" || parsed.Host != p.cfg.EgressProxyHost || parsed.User == nil ||
		parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return "", ErrUnavailable
	}
	return raw, nil
}

// loopbackManagement calls CPA's management API on its own listener with the
// management key. The answer is bounded and only parsed here: none of it is
// passed to the Gateway as is.
func (p *Plugin) loopbackManagement(ctx context.Context, method, path string, query url.Values, body []byte) (int, []byte, error) {
	if p.cfg.ManagementKey == "" {
		return 0, nil, ErrUnavailable
	}
	target := p.cfg.LoopbackURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return 0, nil, ErrUnavailable
	}
	request.Header.Set("Authorization", "Bearer "+p.cfg.ManagementKey)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := p.loopback.Do(request)
	if err != nil {
		return 0, nil, ErrUnavailable
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxLoopbackResponseBytes+1))
	if err != nil || len(data) > maxLoopbackResponseBytes {
		return 0, nil, ErrUnavailable
	}
	return response.StatusCode, data, nil
}

// ---- login -------------------------------------------------------------------

// oauthBegin starts a login in CPA with the egress fixed in advance.
func (p *Plugin) oauthBegin(provider string, body []byte) []byte {
	fields, ok := decodeCredentialRequest(body, "registration_id", "egress_proxy_url")
	if !ok {
		return credentialError(http.StatusBadRequest, "credential_request_invalid")
	}
	registrationID := fields["registration_id"]
	if registrationID != "" && !validUUID(registrationID) {
		return credentialError(http.StatusBadRequest, "credential_request_invalid")
	}
	proxyURL, err := p.loginProxy(fields["egress_proxy_url"])
	if err != nil {
		return credentialError(http.StatusServiceUnavailable, "credential_egress_unavailable")
	}
	entries, err := p.host.AuthList()
	if err != nil {
		return credentialError(http.StatusServiceUnavailable, "credential_login_unavailable")
	}
	existing := map[string]bool{}
	for _, entry := range entries {
		if strings.EqualFold(strings.TrimSpace(entry.Provider), provider) {
			for _, name := range []string{entry.ID, entry.Name} {
				if name != "" {
					existing[name] = true
				}
			}
		}
	}
	payload, _ := json.Marshal(map[string]string{"provider": cpaOAuthProviders[provider].name, "proxy_url": proxyURL})
	ctx, cancel := context.WithTimeout(context.Background(), credentialCallTimeout)
	defer cancel()
	status, data, err := p.cpaManagement(ctx, http.MethodPost, "/v8/management/oauth/auth-url", nil, payload)
	var answer struct {
		Status string `json:"status"`
		URL    string `json:"url"`
		State  string `json:"state"`
	}
	if err != nil || status != http.StatusOK || json.Unmarshal(data, &answer) != nil || answer.Status != "ok" ||
		!validOAuthState(answer.State) || !validOAuthURL(provider, answer.URL, answer.State) {
		return credentialError(http.StatusBadGateway, "credential_login_unavailable")
	}
	now := p.now()
	if !p.oauth.add(answer.State, oauthSession{provider: provider, registrationID: registrationID, startedAt: now, expires: now.Add(oauthSessionTTL), existing: existing}, now) {
		_, _, _ = p.cpaManagement(ctx, http.MethodDelete, "/v8/management/oauth/session", url.Values{"state": {answer.State}}, nil)
		return credentialError(http.StatusServiceUnavailable, "credential_login_busy")
	}
	return managementJSON(http.StatusOK, map[string]string{"url": answer.URL, "state": answer.State})
}

// oauthCallback hands the Owner's authorization code to CPA's own callback.
func (p *Plugin) oauthCallback(provider string, body []byte) []byte {
	fields, ok := decodeCredentialRequest(body, "state", "code")
	if !ok || len(fields) != 2 || !cleanToken(fields["code"], maxOAuthCodeBytes) {
		return credentialError(http.StatusBadRequest, "credential_request_invalid")
	}
	state := fields["state"]
	if _, ok = p.oauth.get(state, provider, p.now()); !ok {
		return credentialError(http.StatusNotFound, "credential_login_not_found")
	}
	payload, _ := json.Marshal(map[string]string{"provider": cpaOAuthProviders[provider].name, "code": fields["code"], "state": state})
	ctx, cancel := context.WithTimeout(context.Background(), credentialCallTimeout)
	defer cancel()
	status, _, err := p.cpaManagement(ctx, http.MethodPost, "/v0/management/oauth-callback", nil, payload)
	if err != nil || status != http.StatusOK {
		return credentialError(http.StatusBadGateway, "credential_login_rejected")
	}
	return managementJSON(http.StatusOK, map[string]string{"status": "submitted"})
}

// oauthStatus reports a login's progress. Once CPA has saved the credential,
// the account is identified, its egress registration is committed and its
// route is written before the account id is reported.
func (p *Plugin) oauthStatus(provider string, body []byte) []byte {
	fields, ok := decodeCredentialRequest(body, "state")
	if !ok || len(fields) != 1 {
		return credentialError(http.StatusBadRequest, "credential_request_invalid")
	}
	state := fields["state"]
	session, ok := p.oauth.get(state, provider, p.now())
	if !ok {
		return credentialError(http.StatusNotFound, "credential_login_not_found")
	}
	if session.accountID != "" {
		return managementJSON(http.StatusOK, map[string]string{"status": "ok", "account_id": session.accountID})
	}
	ctx, cancel := context.WithTimeout(context.Background(), credentialCallTimeout)
	defer cancel()
	status, data, err := p.cpaManagement(ctx, http.MethodGet, "/v0/management/get-auth-status", url.Values{"state": {state}}, nil)
	var answer struct {
		Status string `json:"status"`
		AuthID string `json:"auth_id"`
	}
	if err != nil || status != http.StatusOK || json.Unmarshal(data, &answer) != nil {
		return credentialError(http.StatusBadGateway, "credential_login_unavailable")
	}
	switch {
	case answer.Status == "wait":
		return managementJSON(http.StatusOK, map[string]string{"status": "wait"})
	case answer.Status != "ok":
		p.oauth.drop(state)
		return managementJSON(http.StatusOK, map[string]string{"status": "error"})
	}
	authID := strings.TrimSpace(answer.AuthID)
	if authID == "" {
		// Built-in CPA OAuth completes non-targeted Claude/Antigravity sessions
		// with status=ok but no auth_id. Wait for its auth listing to expose the
		// file written by this session, and never adopt an older credential.
		var errPending error
		authID, errPending = p.newOAuthCredential(provider, session.startedAt)
		if errors.Is(errPending, errCredentialPending) {
			return managementJSON(http.StatusOK, map[string]string{"status": "wait"})
		}
		if errPending != nil {
			return managementJSON(http.StatusOK, map[string]string{"status": "wait"})
		}
	}
	accountID, err := p.completeCredential(ctx, provider, authID, session.registrationID, session.existing)
	if errors.Is(err, errCredentialPending) {
		return managementJSON(http.StatusOK, map[string]string{"status": "wait"})
	}
	var refusal *credentialRefusal
	if errors.As(err, &refusal) {
		p.oauth.drop(state)
		return managementJSON(http.StatusOK, map[string]string{"status": "error", "error": refusal.code})
	}
	if err != nil {
		logf("login for a %s credential was not completed: %v", provider, err)
		p.oauth.drop(state)
		return managementJSON(http.StatusOK, map[string]string{"status": "error"})
	}
	p.oauth.complete(state, accountID)
	return managementJSON(http.StatusOK, map[string]string{"status": "ok", "account_id": accountID})
}

// newOAuthCredential returns the newest credential of provider that CPA
// reports as changed after this login began. CPA's built-in OAuth status does
// not include auth_id for non-targeted Claude/Antigravity sessions, so this
// listing is the safe fallback that lets the plugin finish its own identity
// and egress binding. No pre-existing file is eligible.
func (p *Plugin) newOAuthCredential(provider string, startedAt time.Time) (string, error) {
	if startedAt.IsZero() {
		return "", errCredentialPending
	}
	entries, err := p.host.AuthList()
	if err != nil {
		return "", errCredentialPending
	}
	var selected AuthEntry
	var selectedAt time.Time
	for _, entry := range entries {
		if !strings.EqualFold(strings.TrimSpace(entry.Provider), provider) || entry.RuntimeOnly || entry.Source != "file" {
			continue
		}
		updatedAt := entry.UpdatedAt
		if updatedAt.IsZero() {
			updatedAt = entry.ModTime
		}
		if updatedAt.IsZero() {
			updatedAt = entry.CreatedAt
		}
		if updatedAt.IsZero() || !updatedAt.After(startedAt) {
			continue
		}
		if (selected.ID != "" || selected.Name != "") && !updatedAt.After(selectedAt) {
			continue
		}
		selected = entry
		selectedAt = updatedAt
	}
	if selected.ID == "" && selected.Name == "" {
		return "", errCredentialPending
	}
	if selected.ID != "" {
		return selected.ID, nil
	}
	return selected.Name, nil
}

// completeCredential turns a credential CPA has just saved into a usable
// account: identify it from the fields CPA stored, register that identity,
// commit the egress choice (and the account's owner) and write its route. A
// credential without an identity, or one that collides with another
// credential of the same verified account, is deleted again. existing names
// the credential files that were there before the login began; nil means the
// credential is a new file (an import).
func (p *Plugin) completeCredential(ctx context.Context, provider, authID, registrationID string, existing map[string]bool) (string, error) {
	entries, err := p.host.AuthList()
	if err != nil {
		return "", ErrUnavailable
	}
	var entry *AuthEntry
	for i := range entries {
		candidate := &entries[i]
		if (candidate.ID == authID || candidate.Name == authID) && strings.EqualFold(strings.TrimSpace(candidate.Provider), provider) {
			entry = candidate
		}
	}
	if entry == nil {
		return "", errCredentialPending
	}
	raw, err := p.host.AuthJSON(entry.AuthIndex)
	if err != nil {
		return "", ErrUnavailable
	}
	id, err := p.inventory.Register(provider, raw)
	if errors.Is(err, ErrAccountNotFound) {
		p.removeCredentialFile(ctx, entry.Name)
		return "", errCredentialIdentity
	}
	if err != nil {
		return "", err
	}
	if provider != ProviderCodex {
		// Two credentials of one verified Claude or Antigravity account would
		// both be quarantined; keep the existing one. The inventory hides
		// both, so every other file of the provider is identified directly.
		for _, other := range entries {
			if other.ID == entry.ID || !strings.EqualFold(strings.TrimSpace(other.Provider), provider) {
				continue
			}
			otherRaw, errOther := p.host.AuthJSON(other.AuthIndex)
			if errOther != nil {
				return "", ErrUnavailable
			}
			if p.inventory.identify(provider, otherRaw).id == id {
				p.removeCredentialFile(ctx, entry.Name)
				return "", errCredentialConflict
			}
		}
	}
	if registrationID != "" {
		if err = p.alloc.RegisterEgress(ctx, provider, registrationID, id); err != nil {
			var refused *RegistrationRefusedError
			if errors.As(err, &refused) {
				p.refuseCredential(ctx, provider, *entry, id, existing)
				return "", &credentialRefusal{code: refused.Code}
			}
			// The egress choice is not bound to the account. Keep the
			// account out of service rather than let it run on whatever
			// binding it had before.
			if errDisable := p.control.SetEnabled(id, false); errDisable != nil {
				logf("account %s could not be disabled after a failed egress registration: %T", id, errDisable)
			}
			return "", errCredentialEgress
		}
	}
	account, found, err := p.inventory.Find(provider, id)
	if err != nil || !found {
		return "", errCredentialPending
	}
	if _, err = p.syncEgress(ctx, provider, account); err != nil {
		return "", errCredentialEgress
	}
	return id, nil
}

// refuseCredential undoes a login or import the Gateway refused because the
// account is not the caller's. The account and its control state belong to
// someone else and stay as they are. A new credential file is deleted. A file
// that existed before the login was rewritten in place by CPA: it is the
// account's only credential, so it stays, and its route is put back to the
// one the account's owner bound. Only if that route cannot be restored is the
// account taken out of service, rather than run through the refused login's
// egress.
func (p *Plugin) refuseCredential(ctx context.Context, provider string, entry AuthEntry, id string, existing map[string]bool) {
	if !existing[entry.ID] && !existing[entry.Name] {
		p.removeCredentialFile(ctx, entry.Name)
		return
	}
	account, found, err := p.inventory.Find(provider, id)
	if err == nil && found {
		if _, err = p.syncEgress(ctx, provider, account); err == nil {
			return
		}
	}
	logf("account %s keeps a refused login's credential and its route could not be restored: %T", id, err)
	if errDisable := p.control.SetEnabled(id, false); errDisable != nil {
		logf("account %s could not be disabled after a refused login: %T", id, errDisable)
	}
}

// removeCredentialFile deletes one credential file through CPA. It is used
// only for a credential this plugin has just created or a login has just
// saved, never for an existing account.
func (p *Plugin) removeCredentialFile(ctx context.Context, name string) {
	if name == "" {
		return
	}
	if status, _, err := p.cpaManagement(ctx, http.MethodDelete, "/v0/management/auth-files", url.Values{"name": {name}}, nil); err != nil || status != http.StatusOK {
		logf("an unusable credential could not be removed: %d %T", status, err)
	}
}

// ---- import ------------------------------------------------------------------

// importCredential stores the Owner's tokens as a CPA credential that already
// carries its egress, lets CPA refresh it through that egress, and completes
// it like a login. Antigravity cannot be imported: CPA learns an Antigravity
// account's e-mail address only during a login, and this plugin sends no
// request of its own to look it up.
func (p *Plugin) importCredential(provider string, body []byte) []byte {
	if provider == ProviderAntigravity {
		return credentialError(http.StatusBadRequest, "credential_import_unsupported")
	}
	fields, ok := decodeCredentialRequest(body, "refresh_token", "access_token", "id_token", "registration_id", "egress_proxy_url")
	if !ok || !cleanToken(fields["refresh_token"], maxCredentialTokenBytes) {
		// CPA can only verify, identify and keep a credential it can refresh.
		return credentialError(http.StatusBadRequest, "credential_request_invalid")
	}
	for _, name := range []string{"access_token", "id_token"} {
		if value, present := fields[name]; present && !cleanToken(value, maxCredentialTokenBytes) {
			return credentialError(http.StatusBadRequest, "credential_request_invalid")
		}
	}
	registrationID := fields["registration_id"]
	if registrationID != "" && !validUUID(registrationID) {
		return credentialError(http.StatusBadRequest, "credential_request_invalid")
	}
	proxyURL, err := p.loginProxy(fields["egress_proxy_url"])
	if err != nil {
		return credentialError(http.StatusServiceUnavailable, "credential_egress_unavailable")
	}
	var random [12]byte
	if _, err = rand.Read(random[:]); err != nil {
		return credentialError(http.StatusServiceUnavailable, "credential_import_unavailable")
	}
	cpaType := cpaOAuthProviders[provider].name
	name := cpaType + "-import-" + hex.EncodeToString(random[:]) + ".json"
	document := map[string]string{"type": cpaType, "refresh_token": fields["refresh_token"], "proxy_url": proxyURL}
	for _, field := range []string{"access_token", "id_token"} {
		if value := fields[field]; value != "" {
			document[field] = value
		}
	}
	payload, _ := json.Marshal(document)
	ctx, cancel := context.WithTimeout(context.Background(), credentialCallTimeout)
	defer cancel()
	status, _, err := p.cpaManagement(ctx, http.MethodPost, "/v0/management/auth-files", url.Values{"name": {name}}, payload)
	if err != nil || status != http.StatusOK {
		return credentialError(http.StatusBadGateway, "credential_import_rejected")
	}
	refresh, _ := json.Marshal(map[string]string{"name": name})
	status, data, err := p.cpaManagement(ctx, http.MethodPost, "/v0/management/auth-files/refresh", nil, refresh)
	var refreshed struct {
		OK bool `json:"ok"`
	}
	if err != nil || status != http.StatusOK || json.Unmarshal(data, &refreshed) != nil || !refreshed.OK {
		p.removeCredentialFile(ctx, name)
		return credentialError(http.StatusBadGateway, "credential_refresh_failed")
	}
	deadline := p.now().Add(importSettleTimeout)
	for {
		accountID, errComplete := p.completeCredential(ctx, provider, name, registrationID, nil)
		if errComplete == nil {
			return managementJSON(http.StatusOK, map[string]string{"status": "ok", "account_id": accountID})
		}
		var refusal *credentialRefusal
		if errors.As(errComplete, &refusal) {
			return credentialError(http.StatusConflict, refusal.code)
		}
		if !errors.Is(errComplete, errCredentialPending) || !p.now().Before(deadline) {
			logf("an imported %s credential was not completed: %v", provider, errComplete)
			if errors.Is(errComplete, errCredentialPending) {
				p.removeCredentialFile(ctx, name)
			}
			return credentialError(http.StatusBadGateway, "credential_identity_unavailable")
		}
		select {
		case <-ctx.Done():
			return credentialError(http.StatusGatewayTimeout, "credential_import_timeout")
		case <-time.After(importSettlePoll):
		}
	}
}

// ---- refresh and delete ------------------------------------------------------

// refreshCredential asks CPA to refresh one account's credential, through the
// route the Gateway binds the account to right now.
func (p *Plugin) refreshCredential(provider string, query url.Values, body []byte) []byte {
	if fields, ok := decodeCredentialRequest(body); !ok || len(fields) != 0 {
		return credentialError(http.StatusBadRequest, "credential_request_invalid")
	}
	account, code, status := p.credentialAccount(provider, query)
	if code != "" {
		return credentialError(status, code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), credentialCallTimeout)
	defer cancel()
	if _, err := p.syncEgress(ctx, provider, account); err != nil {
		return credentialError(http.StatusServiceUnavailable, "credential_egress_unavailable")
	}
	payload, _ := json.Marshal(map[string]string{"name": account.Name})
	status, data, err := p.cpaManagement(ctx, http.MethodPost, "/v0/management/auth-files/refresh", nil, payload)
	var refreshed struct {
		OK bool `json:"ok"`
	}
	if err != nil || status != http.StatusOK || json.Unmarshal(data, &refreshed) != nil || !refreshed.OK {
		return credentialError(http.StatusBadGateway, "credential_refresh_failed")
	}
	return managementJSON(http.StatusOK, map[string]string{"status": "ok"})
}

// deleteCredential removes one account's credential through CPA. The Gateway
// disables the account and waits for its requests first; both are checked
// again here immediately before the file goes. The identity registration and
// the control state stay, so a later login gets the same account id.
func (p *Plugin) deleteCredential(provider string, query url.Values, body []byte) []byte {
	if fields, ok := decodeCredentialRequest(body); !ok || len(fields) != 0 {
		return credentialError(http.StatusBadRequest, "credential_request_invalid")
	}
	account, code, status := p.credentialAccount(provider, query)
	if code != "" {
		return credentialError(status, code)
	}
	if manual, _ := p.control.Statuses(account.ID); manual != "manual_disabled" {
		return credentialError(http.StatusConflict, "upstream_account_not_disabled")
	}
	for _, row := range p.concurrency.Snapshot([]string{account.ID}) {
		if row.ID == account.ID && row.ActiveRequests > 0 {
			return credentialError(http.StatusConflict, "upstream_account_draining")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), credentialCallTimeout)
	defer cancel()
	status, _, err := p.cpaManagement(ctx, http.MethodDelete, "/v0/management/auth-files", url.Values{"name": {account.Name}}, nil)
	if err != nil || status != http.StatusOK {
		return credentialError(http.StatusBadGateway, "credential_delete_failed")
	}
	return managementJSON(http.StatusOK, map[string]string{"status": "deleted"})
}

// credentialAccount resolves the id query parameter to one account.
func (p *Plugin) credentialAccount(provider string, query url.Values) (Account, string, int) {
	id, ok := quotaAccountID(query)
	if !ok {
		return Account{}, "upstream_account_not_found", http.StatusNotFound
	}
	account, found, err := p.inventory.Find(provider, id)
	if err != nil {
		return Account{}, "account_registry_unavailable", http.StatusServiceUnavailable
	}
	if !found || account.Name == "" {
		return Account{}, "upstream_account_not_found", http.StatusNotFound
	}
	return account, "", 0
}
