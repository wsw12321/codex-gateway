package antigravity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

const DefaultAccountRegistryPath = "/var/lib/antigravity/keyrings/accounts.json"

var accountNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
var accountEmailPattern = regexp.MustCompile(`^[A-Za-z0-9]\*{3}@[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)

// AccountRecord contains only non-secret, bounded management metadata. OAuth
// files remain encrypted in separate Secret Service application namespaces.
type AccountRecord struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	MaskedEmail string `json:"masked_email"`
	Enabled     bool   `json:"enabled"`
}

type AccountRegistry struct {
	mu      sync.Mutex
	path    string
	records []AccountRecord
}

func ValidAccountName(name string) bool { return accountNamePattern.MatchString(name) }

func accountID(name string) string {
	sum := sha256.Sum256([]byte("codex-gateway-antigravity:" + name))
	return hex.EncodeToString(sum[:8])
}

func AccountCredentials(name string) *KeyringCredentials {
	if name == "default" {
		return &KeyringCredentials{}
	}
	return &KeyringCredentials{Account: accountID(name)}
}

// OpenAccountRegistry adopts an existing single-account installation as
// "default" without rewriting, exporting, or weakening its encrypted keyring.
func OpenAccountRegistry(ctx context.Context, path string, runner Runner) (*AccountRegistry, error) {
	return openAccountRegistry(ctx, path, runner, true)
}

// OpenExistingAccountRegistry is for migration reauthorization: missing
// metadata must fail, even if a legacy default credential could be adopted.
func OpenExistingAccountRegistry(ctx context.Context, path string) (*AccountRegistry, error) {
	return openAccountRegistry(ctx, path, Runner{}, false)
}

func openAccountRegistry(ctx context.Context, path string, runner Runner, adoptLegacy bool) (*AccountRegistry, error) {
	registry := &AccountRegistry{path: path, records: []AccountRecord{}}
	dir, err := registry.directory()
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	fd, err := syscall.Openat(int(dir.Fd()), filepath.Base(path), syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if errors.Is(err, os.ErrNotExist) {
		if !adoptLegacy {
			return nil, credentialError("credential_missing")
		}
		if runner.Credentials == nil {
			runner.Credentials = AccountCredentials("default")
		}
		email, err := runner.accountMaskedEmail(ctx)
		if errors.Is(err, ErrCredentialsMissing) {
			return registry, nil
		}
		if err != nil {
			return nil, err
		}
		if err := registry.Register("default", email); err != nil {
			return nil, err
		}
		return registry, nil
	}
	if err != nil {
		return nil, credentialFileError(err)
	}
	file := os.NewFile(uintptr(fd), "account-registry")
	defer file.Close()
	if err := checkCredentialFile(file, false); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, credentialLimit+1))
	if err != nil || len(data) > credentialLimit || uniqueJSON(data) != nil {
		return nil, credentialError("credential_invalid")
	}
	var document struct {
		Accounts []AccountRecord `json:"accounts"`
	}
	if err := json.Unmarshal(data, &document); err != nil || document.Accounts == nil || len(document.Accounts) > 100 {
		return nil, credentialError("credential_invalid")
	}
	seen := map[string]bool{}
	for _, account := range document.Accounts {
		if !ValidAccountName(account.Name) || account.ID != accountID(account.Name) || seen[account.ID] || (account.MaskedEmail != "" && !accountEmailPattern.MatchString(account.MaskedEmail)) {
			return nil, credentialError("credential_invalid")
		}
		seen[account.ID] = true
	}
	registry.records = document.Accounts
	return registry, nil
}

func (r *AccountRegistry) directory() (*os.File, error) {
	if !filepath.IsAbs(r.path) || filepath.Base(r.path) == "." {
		return nil, credentialError("unsafe_path")
	}
	// Walk from /, refusing symlinks in every component. The final directory
	// is private and owned by the bridge identity.
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, credentialFileError(err)
	}
	for _, component := range strings.Split(strings.TrimPrefix(filepath.Dir(filepath.Clean(r.path)), "/"), "/") {
		if component == "" {
			continue
		}
		next, err := syscall.Openat(fd, component, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		_ = syscall.Close(fd)
		if err != nil {
			return nil, credentialFileError(err)
		}
		fd = next
	}
	dir := os.NewFile(uintptr(fd), "account-registry-directory")
	if err := checkCredentialFile(dir, true); err != nil {
		_ = dir.Close()
		return nil, err
	}
	return dir, nil
}

func (r *AccountRegistry) Records() []AccountRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]AccountRecord{}, r.records...)
}

func (r *AccountRegistry) Register(name, email string) error {
	if !ValidAccountName(name) || (email != "" && !accountEmailPattern.MatchString(email)) {
		return credentialError("credential_invalid")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	next := append([]AccountRecord{}, r.records...)
	for index := range next {
		if next[index].Name == name {
			next[index].MaskedEmail = email
			return r.persist(next)
		}
	}
	if len(next) >= 100 {
		return credentialError("credential_too_large")
	}
	next = append(next, AccountRecord{ID: accountID(name), Name: name, MaskedEmail: email, Enabled: true})
	return r.persist(next)
}

func (r *AccountRegistry) SetEnabled(id string, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := append([]AccountRecord{}, r.records...)
	for index := range next {
		if next[index].ID == id {
			next[index].Enabled = enabled
			return r.persist(next)
		}
	}
	return credentialError("credential_missing")
}

func (r *AccountRegistry) persist(records []AccountRecord) error {
	dir, err := r.directory()
	if err != nil {
		return err
	}
	defer dir.Close()
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return credentialError("io_failed")
	}
	name := ".accounts-" + hex.EncodeToString(random[:])
	fd, err := syscall.Openat(int(dir.Fd()), name, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return credentialFileError(err)
	}
	defer syscall.Unlinkat(int(dir.Fd()), name)
	file := os.NewFile(uintptr(fd), "account-registry-temporary")
	writeErr := json.NewEncoder(file).Encode(map[string]any{"accounts": records})
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return credentialError("io_failed")
	}
	if err := syscall.Renameat(int(dir.Fd()), name, int(dir.Fd()), filepath.Base(r.path)); err != nil {
		return credentialFileError(err)
	}
	if err := dir.Sync(); err != nil {
		return credentialFileError(err)
	}
	r.records = records
	return nil
}

// RegisterLogin reads only after official login has committed its credential.
func (r *AccountRegistry) RegisterLogin(ctx context.Context, name string, runner Runner) error {
	email, err := runner.accountMaskedEmail(ctx)
	if err != nil {
		return err
	}
	return r.Register(name, email)
}

func (r Runner) accountMaskedEmail(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	root, _, err := r.workspace()
	if err != nil {
		return "", credentialError("io_failed")
	}
	defer os.RemoveAll(root)
	home := filepath.Join(root, "home")
	if err := r.credentials().Restore(ctx, home); err != nil {
		return "", err
	}
	dir, err := credentialDirectory(home, false)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	file, err := openCredential(dir)
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, credentialLimit+1))
	if err != nil || validateCredential(data) != nil {
		return "", credentialError("credential_invalid")
	}
	var stored struct {
		Email   string `json:"email"`
		IDToken string `json:"id_token"`
		Token   struct {
			IDToken string `json:"id_token"`
		} `json:"token"`
	}
	_ = json.Unmarshal(data, &stored)
	if stored.IDToken == "" {
		stored.IDToken = stored.Token.IDToken
	}
	// JWT claims are unverified display hints, never identity or authorization.
	if stored.Email == "" {
		parts := strings.Split(stored.IDToken, ".")
		if len(parts) == 3 {
			if claims, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
				var hint struct {
					Email string `json:"email"`
				}
				if json.Unmarshal(claims, &hint) == nil {
					stored.Email = hint.Email
				}
			}
		}
	}
	local, domain, found := strings.Cut(stored.Email, "@")
	if found && len(local) > 0 {
		masked := local[:1] + "***@" + strings.ToLower(domain)
		if len(masked) <= 254 && accountEmailPattern.MatchString(masked) {
			return masked, nil
		}
	}
	return "", nil
}
