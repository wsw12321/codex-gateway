package gatewayplugin

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Provider keys as CPA reports them on credentials.
const (
	ProviderCodex       = "codex"
	ProviderAntigravity = "antigravity"
	ProviderClaude      = "claude"

	// AntigravityIdentityFile and AnthropicIdentityFile keep the patched image's
	// names and schema; see AccountStateFile.
	AntigravityIdentityFile = ".gateway-antigravity-identities"
	AnthropicIdentityFile   = ".gateway-anthropic-identities"

	antigravityIdentityDomain = "codex-gateway-antigravity-google-subject:\x00"
	anthropicIdentityDomain   = "codex-gateway-anthropic-account-organization:\x00"
)

// AuthEntry is the subset of a host credential listing this plugin relies on.
type AuthEntry struct {
	ID          string    `json:"id"`
	AuthIndex   string    `json:"auth_index"`
	Name        string    `json:"name"`
	Provider    string    `json:"provider"`
	Status      string    `json:"status"`
	Disabled    bool      `json:"disabled"`
	Unavailable bool      `json:"unavailable"`
	RuntimeOnly bool      `json:"runtime_only"`
	Source      string    `json:"source"`
	Size        int64     `json:"size"`
	ModTime     time.Time `json:"modtime"`
	UpdatedAt   time.Time `json:"updated_at"`
	CreatedAt   time.Time `json:"created_at"`
	LastRefresh time.Time `json:"last_refresh"`
	AccountType string    `json:"account_type"`
}

// Host is the part of the CPA host this plugin calls back into.
type Host interface {
	// AuthList returns every credential the host currently knows.
	AuthList() ([]AuthEntry, error)
	// AuthJSON returns the stored credential document for a host auth index.
	AuthJSON(authIndex string) (json.RawMessage, error)
	// ResetCooldown clears quota and cooldown routing state for one credential.
	ResetCooldown(authIndex string) error
}

// Account is one routable upstream account: a credential with a verified,
// stable Gateway identity.
type Account struct {
	// ID is the 16-character stable account index shared with the Gateway.
	ID       string
	Provider string
	// AuthID is the host credential id; scheduler candidates carry it.
	AuthID string
	// AuthIndex addresses the credential in host callbacks.
	AuthIndex   string
	Name        string
	Email       string
	Plan        string
	Status      string
	Disabled    bool
	Unavailable bool
	LastRefresh time.Time
	UpdatedAt   time.Time
	CreatedAt   time.Time
	// ProxyURL is the egress stored in the credential file. It is what CPA
	// uses for this credential, and is kept equal to the Gateway's binding.
	ProxyURL string
	// Duplicate marks a credential file that lost to another file of the same
	// real account. Duplicates are listed nowhere and never selected.
	Duplicate bool
}

// identityRegistry maps a verified provider subject to a stable account index.
// The mapping is independent of file names, refreshes and re-authorization.
type identityRegistry struct {
	mu       sync.RWMutex
	path     string
	domain   string
	valid    func(string) bool
	accounts map[string]string
	write    func(string, []byte) error
}

func loadIdentityRegistry(authDir, file, domain string, valid func(string) bool) (*identityRegistry, error) {
	registry := &identityRegistry{path: filepath.Join(authDir, file), domain: domain, valid: valid, accounts: map[string]string{}, write: writeControlFile}
	data, exists, err := readControlFile(registry.path, 0o600)
	if err != nil {
		return nil, ErrStateUnavailable
	}
	if !exists {
		return registry, nil
	}
	fields, err := decodeObject(data, "version", "accounts")
	if err != nil || string(fields["version"]) != "1" {
		return nil, ErrStateUnavailable
	}
	raw, err := decodeObject(fields["accounts"])
	if err != nil {
		return nil, ErrStateUnavailable
	}
	seen := map[string]bool{}
	for subject, value := range raw {
		var id string
		if !valid(subject) || json.Unmarshal(value, &id) != nil || !validAccountID(id) || seen[id] {
			return nil, ErrStateUnavailable
		}
		registry.accounts[subject] = id
		seen[id] = true
	}
	return registry, nil
}

func (r *identityRegistry) lookup(subject string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.accounts[subject]
	return id, ok
}

// register returns the index of a verified subject, adding and persisting a
// new mapping when needed. Callers must only pass subjects verified against
// the provider's fixed identity endpoint.
func (r *identityRegistry) register(subject string) (string, error) {
	if !r.valid(subject) {
		return "", ErrAccountNotFound
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if id := r.accounts[subject]; id != "" {
		return id, nil
	}
	sum := sha256.Sum256([]byte(r.domain + subject))
	id := hex.EncodeToString(sum[:8])
	next := make(map[string]string, len(r.accounts)+1)
	for existingSubject, existing := range r.accounts {
		if existing == id {
			return "", ErrStateUnavailable
		}
		next[existingSubject] = existing
	}
	next[subject] = id
	data, err := json.Marshal(struct {
		Version  int               `json:"version"`
		Accounts map[string]string `json:"accounts"`
	}{1, next})
	if err != nil || r.write(r.path, data) != nil {
		return "", ErrStateUnavailable
	}
	r.accounts = next
	return id, nil
}

func validGoogleSubject(subject string) bool {
	if subject == "" || len(subject) > 128 {
		return false
	}
	for i := 0; i < len(subject); i++ {
		c := subject[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func validAnthropicSubject(subject string) bool {
	parts := strings.Split(subject, ":")
	return len(parts) == 2 && validUUID(parts[0]) && validUUID(parts[1])
}

// AnthropicSubject joins a verified account and organization UUID.
func AnthropicSubject(account, organization string) string {
	subject := strings.ToLower(strings.TrimSpace(account)) + ":" + strings.ToLower(strings.TrimSpace(organization))
	if !validAnthropicSubject(subject) {
		return ""
	}
	return subject
}

// validCodexAccountID is the fixed ASCII contract for a ChatGPT account id.
func validCodexAccountID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == ':' {
			continue
		}
		return false
	}
	return true
}

// CodexAccountIndex derives the stable index from a ChatGPT account id. It is
// an irreversible digest, so neither the account id nor the e-mail address in
// the credential file name ever leaves CPA.
func CodexAccountIndex(accountID string) string {
	inner := sha256.Sum256([]byte(accountID))
	outer := sha256.Sum256([]byte("auth_index_seed:codex-account:" + hex.EncodeToString(inner[:])))
	return hex.EncodeToString(outer[:8])
}

// codexTokenAccountID reads chatgpt_account_id from an ID token. present is
// false when the claim is absent; valid is false when the token is malformed.
func codexTokenAccountID(token string) (accountID string, present, valid bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", false, true
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", false, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if payload, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return "", false, false
		}
	}
	var claims map[string]json.RawMessage
	if json.Unmarshal(payload, &claims) != nil {
		return "", false, false
	}
	rawAuth, ok := claims["https://api.openai.com/auth"]
	if !ok {
		return "", false, true
	}
	var authClaims map[string]json.RawMessage
	if json.Unmarshal(rawAuth, &authClaims) != nil {
		return "", false, false
	}
	rawID, ok := authClaims["chatgpt_account_id"]
	if !ok {
		return "", false, true
	}
	if json.Unmarshal(rawID, &accountID) != nil {
		return "", true, false
	}
	accountID = strings.TrimSpace(accountID)
	return accountID, true, validCodexAccountID(accountID)
}

// codexTokenPlan reads the subscription plan claim from an ID token. Only the
// plans the Gateway distinguishes are reported; anything else is unknown.
func codexTokenPlan(token string) string {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return "unknown"
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if payload, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return "unknown"
		}
	}
	var claims struct {
		Auth struct {
			Plan string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return "unknown"
	}
	switch plan := strings.ToLower(strings.TrimSpace(claims.Auth.Plan)); plan {
	case "plus", "pro":
		return plan
	}
	return "unknown"
}

// codexIdentity resolves a Codex credential's account id from the top-level
// field and the ID-token claim. Conflicting or malformed values quarantine the
// credential; so does a missing identity, since an unattributed account can be
// neither authorized nor billed.
func codexIdentity(document map[string]json.RawMessage) (string, bool) {
	var identities []string
	if raw, ok := document["account_id"]; ok {
		var top string
		if json.Unmarshal(raw, &top) != nil {
			return "", false
		}
		if top = strings.TrimSpace(top); top != "" {
			if !validCodexAccountID(top) {
				return "", false
			}
			identities = append(identities, top)
		}
	}
	if raw, ok := document["id_token"]; ok {
		var token string
		if json.Unmarshal(raw, &token) != nil {
			return "", false
		}
		claim, present, valid := codexTokenAccountID(token)
		if !valid {
			return "", false
		}
		if present {
			identities = append(identities, claim)
		}
	}
	if len(identities) == 0 {
		return "", false
	}
	for _, identity := range identities[1:] {
		if identity != identities[0] {
			return "", false
		}
	}
	return identities[0], true
}

func stringField(document map[string]json.RawMessage, name string) string {
	var value string
	if raw, ok := document[name]; ok && json.Unmarshal(raw, &value) == nil {
		return strings.TrimSpace(value)
	}
	return ""
}

func boolField(document map[string]json.RawMessage, name string) bool {
	var value bool
	raw, ok := document[name]
	return ok && json.Unmarshal(raw, &value) == nil && value
}

// Inventory resolves host credentials to stable Gateway accounts. Credential
// documents are read through the host only when a file changed, and only the
// derived identity is kept: no token is retained in this plugin's memory.
type Inventory struct {
	host        Host
	antigravity *identityRegistry
	anthropic   *identityRegistry
	antigravOn  bool
	now         func() time.Time
	maxAge      time.Duration

	mu          sync.Mutex
	loadedAt    time.Time
	missAt      time.Time
	byAuthID    map[string]*Account
	fingerprint map[string]string
	identity    map[string]resolvedIdentity
}

type resolvedIdentity struct {
	id    string
	email string
	plan  string
	proxy string
}

// NewInventory loads the identity registries from authDir.
func NewInventory(host Host, authDir string, antigravityEnabled bool) (*Inventory, error) {
	antigravity, err := loadIdentityRegistry(authDir, AntigravityIdentityFile, antigravityIdentityDomain, validGoogleSubject)
	if err != nil {
		return nil, err
	}
	anthropic, err := loadIdentityRegistry(authDir, AnthropicIdentityFile, anthropicIdentityDomain, validAnthropicSubject)
	if err != nil {
		return nil, err
	}
	return &Inventory{
		host: host, antigravity: antigravity, anthropic: anthropic, antigravOn: antigravityEnabled,
		now: time.Now, maxAge: 15 * time.Second,
		byAuthID: map[string]*Account{}, fingerprint: map[string]string{}, identity: map[string]resolvedIdentity{},
	}, nil
}

// Resolve maps scheduler candidate ids to accounts. Unknown ids trigger one
// throttled refresh, so a credential added a moment ago is usable at once
// without letting a stream of unknown ids hammer the host.
func (inv *Inventory) Resolve(authIDs []string) (map[string]*Account, error) {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	now := inv.now()
	stale := inv.loadedAt.IsZero() || now.Sub(inv.loadedAt) > inv.maxAge
	if !stale {
		for _, id := range authIDs {
			if _, known := inv.byAuthID[id]; !known && now.Sub(inv.missAt) > time.Second {
				stale = true
				inv.missAt = now
				break
			}
		}
	}
	if stale {
		if err := inv.refreshLocked(now); err != nil {
			return nil, err
		}
	}
	result := make(map[string]*Account, len(authIDs))
	for _, id := range authIDs {
		if account := inv.byAuthID[id]; account != nil {
			result[id] = account
		}
	}
	return result, nil
}

// NoteProxy records the egress just written to a credential, so the next
// lookup sees it without waiting for the host to report the file change.
func (inv *Inventory) NoteProxy(authID, proxyURL string) {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	// Replace rather than mutate: callers hold pointers to the old value and
	// read it without this lock.
	if account := inv.byAuthID[authID]; account != nil {
		updated := *account
		updated.ProxyURL = proxyURL
		inv.byAuthID[authID] = &updated
	}
	if identity, ok := inv.identity[authID]; ok {
		identity.proxy = proxyURL
		inv.identity[authID] = identity
	}
}

// Accounts returns a fresh snapshot of every identified, non-duplicate account
// of provider, sorted by index.
func (inv *Inventory) Accounts(provider string) ([]Account, error) {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	if err := inv.refreshLocked(inv.now()); err != nil {
		return nil, err
	}
	accounts := make([]Account, 0, len(inv.byAuthID))
	for _, account := range inv.byAuthID {
		if account.Provider == provider && !account.Duplicate {
			accounts = append(accounts, *account)
		}
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	return accounts, nil
}

// Find returns the canonical credential of one account.
func (inv *Inventory) Find(provider, id string) (Account, bool, error) {
	accounts, err := inv.Accounts(provider)
	if err != nil {
		return Account{}, false, err
	}
	for _, account := range accounts {
		if account.ID == id {
			return account, true, nil
		}
	}
	return Account{}, false, nil
}

func (inv *Inventory) refreshLocked(now time.Time) error {
	entries, err := inv.host.AuthList()
	if err != nil {
		return err
	}
	next := make(map[string]*Account, len(entries))
	fingerprints := make(map[string]string, len(entries))
	identities := make(map[string]resolvedIdentity, len(entries))
	for _, entry := range entries {
		provider := strings.ToLower(strings.TrimSpace(entry.Provider))
		if entry.ID == "" || entry.AuthIndex == "" || entry.RuntimeOnly || entry.Source != "file" {
			continue
		}
		if provider != ProviderCodex && provider != ProviderAntigravity && provider != ProviderClaude {
			continue
		}
		if provider == ProviderAntigravity && !inv.antigravOn {
			continue
		}
		fingerprint := entry.AuthIndex + "|" + entry.ModTime.UTC().Format(time.RFC3339Nano) + "|" + entry.UpdatedAt.UTC().Format(time.RFC3339Nano) + "|" + itoa(entry.Size)
		identity, cached := inv.identity[entry.ID]
		if !cached || inv.fingerprint[entry.ID] != fingerprint {
			raw, errGet := inv.host.AuthJSON(entry.AuthIndex)
			if errGet != nil {
				return errGet
			}
			identity = inv.identify(provider, raw)
		}
		fingerprints[entry.ID] = fingerprint
		identities[entry.ID] = identity
		if identity.id == "" {
			continue
		}
		next[entry.ID] = &Account{
			ID: identity.id, Provider: provider, AuthID: entry.ID, AuthIndex: entry.AuthIndex, Name: entry.Name,
			Email: identity.email, Plan: identity.plan, Status: entry.Status, Disabled: entry.Disabled,
			Unavailable: entry.Unavailable, LastRefresh: entry.LastRefresh, UpdatedAt: entry.UpdatedAt, CreatedAt: entry.CreatedAt,
			ProxyURL: identity.proxy,
		}
	}
	markDuplicates(next)
	inv.byAuthID, inv.fingerprint, inv.identity, inv.loadedAt = next, fingerprints, identities, now
	return nil
}

// identify derives the stable index of one credential document. An empty id
// means the credential is quarantined.
func (inv *Inventory) identify(provider string, raw json.RawMessage) resolvedIdentity {
	var document map[string]json.RawMessage
	if json.Unmarshal(raw, &document) != nil {
		return resolvedIdentity{}
	}
	identity := resolvedIdentity{email: stringField(document, "email"), proxy: stringField(document, "proxy_url")}
	switch provider {
	case ProviderCodex:
		accountID, ok := codexIdentity(document)
		if !ok {
			return resolvedIdentity{}
		}
		identity.id = CodexAccountIndex(accountID)
		identity.plan = codexTokenPlan(stringField(document, "id_token"))
	case ProviderAntigravity:
		subject := stringField(document, "google_subject")
		if !validGoogleSubject(subject) {
			return resolvedIdentity{}
		}
		identity.id, _ = inv.antigravity.lookup(subject)
	case ProviderClaude:
		subject := stringField(document, "gateway_anthropic_subject")
		if boolField(document, "gateway_identity_conflict") || !validAnthropicSubject(subject) ||
			subject != AnthropicSubject(stringField(document, "account_uuid"), stringField(document, "organization_uuid")) {
			return resolvedIdentity{}
		}
		identity.id, _ = inv.anthropic.lookup(subject)
	}
	return identity
}

// markDuplicates keeps one credential per real account. Codex prefers the most
// recent last_refresh with the credential id as a deterministic tie-breaker.
// Two Antigravity or Claude credentials for one verified subject are a
// conflict: all of them are quarantined until the Owner removes one.
func markDuplicates(accounts map[string]*Account) {
	groups := map[string][]*Account{}
	for _, account := range accounts {
		key := account.Provider + "\x00" + account.ID
		groups[key] = append(groups[key], account)
	}
	for _, group := range groups {
		if len(group) < 2 {
			continue
		}
		if group[0].Provider != ProviderCodex {
			for _, account := range group {
				delete(accounts, account.AuthID)
			}
			continue
		}
		sort.Slice(group, func(i, j int) bool {
			if !group[i].LastRefresh.Equal(group[j].LastRefresh) {
				return group[i].LastRefresh.After(group[j].LastRefresh)
			}
			return group[i].AuthID < group[j].AuthID
		})
		for _, account := range group[1:] {
			account.Duplicate = true
		}
	}
}

func itoa(value int64) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
