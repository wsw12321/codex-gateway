package cpamigrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/wsw/codex-gateway/internal/antigravity"
	"golang.org/x/sys/unix"
)

const identityFilename = ".gateway-antigravity-identities"
const legacyFilename = "antigravity-oauth-token"

var subjectPattern = regexp.MustCompile(`^[0-9]{1,64}$`)
var accountIDPattern = regexp.MustCompile(`^[a-f0-9]{16}$`)
var projectPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)

type Options struct{ Direction, Registry, OAuthDir, TempDir, ProxyURL, Account, OAuthClientFile string }

// Report contains only stable Gateway identifiers and bounded diagnostics.
// A successful conversion is explicitly pending real Gateway acceptance.
type Report struct {
	AccountID     string   `json:"account_id"`
	Status        string   `json:"status"`
	LegacyEnabled bool     `json:"legacy_enabled"`
	Refreshed     bool     `json:"refreshed"`
	Models        []string `json:"models,omitempty"`
	Reason        string   `json:"reason,omitempty"`
}

type identityMap struct {
	Version  int               `json:"version"`
	Accounts map[string]string `json:"accounts"`
}

type keyring interface {
	Restore(context.Context, string, string) error
	Save(context.Context, string, string) error
	Verify(context.Context, string, string) error
}

// Run requires root only to bridge the old UID 10002 Keyring and the new UID
// 10001 CPA volume. Google credentials are never sent through command args.
func Run(ctx context.Context, o Options) ([]Report, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("root_maintenance_identity_required")
	}
	if o.Direction != "forward" && o.Direction != "reverse" {
		return nil, errors.New("direction_invalid")
	}
	if o.Account != "" && !antigravity.ValidAccountName(o.Account) {
		return nil, errors.New("account_invalid")
	}
	if !filepath.IsAbs(o.Registry) || filepath.Base(o.Registry) != "accounts.json" {
		return nil, errors.New("registry_path_invalid")
	}
	legacy, err := openDirectory(filepath.Dir(o.Registry), 10002)
	if err != nil {
		return nil, err
	}
	defer legacy.close()
	oauth, err := openDirectory(o.OAuthDir, 10001)
	if err != nil {
		return nil, err
	}
	defer oauth.close()
	stage, err := openDirectory(o.TempDir, 10002)
	if err != nil {
		return nil, err
	}
	defer stage.close()
	var fs unix.Statfs_t
	if unix.Fstatfs(int(stage.file.Fd()), &fs) != nil || fs.Type != unix.TMPFS_MAGIC {
		return nil, errors.New("staging_tmpfs_required")
	}
	legacyLock, err := legacy.lock()
	if err != nil {
		return nil, err
	}
	defer legacyLock.Close()
	cpaLock, err := oauth.lock()
	if err != nil {
		return nil, err
	}
	defer cpaLock.Close()
	raw, err := legacy.read("accounts.json")
	if err != nil {
		return nil, errors.New("legacy_registry_unavailable")
	}
	records, err := parseRegistry(raw)
	if err != nil {
		return nil, err
	}
	identities, err := loadIdentities(oauth)
	if err != nil {
		return nil, err
	}
	p, err := newProvider(o.ProxyURL, o.OAuthClientFile)
	if err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, errors.New("helper_unavailable")
	}
	m := migrator{provider: p, keys: processKeyring{executable, o.ProxyURL}, oauth: oauth, identities: identities, temp: o.TempDir, legacyUID: 10002}
	var reports []Report
	for _, record := range records {
		if o.Account != "" && record.Name != o.Account {
			continue
		}
		report := Report{AccountID: record.ID, LegacyEnabled: record.Enabled, Status: "quarantined"}
		home, err := os.MkdirTemp(o.TempDir, "cpa-credential-")
		if err != nil {
			return reports, errors.New("staging_unavailable")
		}
		if err := os.Chown(home, 10002, 10002); err != nil {
			os.RemoveAll(home)
			return reports, errors.New("staging_permissions_invalid")
		}
		if o.Direction == "forward" {
			err = m.forward(ctx, record, home, &report)
		} else {
			err = m.reverse(ctx, record, home, &report)
		}
		os.RemoveAll(home)
		if err != nil {
			report.Reason = err.Error()
		} else {
			report.Status = "awaiting_gateway_verification"
		}
		reports = append(reports, report)
	}
	if len(reports) == 0 {
		return nil, errors.New("no_matching_legacy_account")
	}
	return reports, nil
}

type migrator struct {
	provider   *provider
	keys       keyring
	oauth      *privateDir
	identities identityMap
	temp       string
	legacyUID  int
}

func parseRegistry(raw []byte) ([]antigravity.AccountRecord, error) {
	var v struct {
		Accounts []antigravity.AccountRecord `json:"accounts"`
	}
	if uniqueJSON(raw) != nil || json.Unmarshal(raw, &v) != nil || v.Accounts == nil || len(v.Accounts) > 100 {
		return nil, errors.New("legacy_registry_invalid")
	}
	seen := map[string]bool{}
	for _, r := range v.Accounts {
		sum := sha256.Sum256([]byte("codex-gateway-antigravity:" + r.Name))
		if !antigravity.ValidAccountName(r.Name) || r.ID != hex.EncodeToString(sum[:8]) || seen[r.ID] {
			return nil, errors.New("legacy_registry_invalid")
		}
		seen[r.ID] = true
	}
	return v.Accounts, nil
}

func loadIdentities(d *privateDir) (identityMap, error) {
	var v identityMap
	raw, err := d.read(identityFilename)
	if errors.Is(err, os.ErrNotExist) {
		return identityMap{1, map[string]string{}}, nil
	}
	if err != nil || uniqueJSON(raw) != nil || json.Unmarshal(raw, &v) != nil || v.Version != 1 || v.Accounts == nil {
		return v, errors.New("identity_mapping_invalid")
	}
	seen := map[string]bool{}
	for subject, id := range v.Accounts {
		if !subjectPattern.MatchString(subject) || !accountIDPattern.MatchString(id) || seen[id] {
			return v, errors.New("identity_mapping_conflict")
		}
		seen[id] = true
	}
	return v, nil
}

func (m *migrator) checkMapping(subject, id string) error {
	for knownSubject, knownID := range m.identities.Accounts {
		if (knownSubject == subject && knownID != id) || (knownSubject != subject && knownID == id) {
			return errors.New("identity_conflict_reauthorize")
		}
	}
	return nil
}

func legacyDirectory(home string, uid int) (*privateDir, error) {
	return openDirectory(filepath.Join(home, ".gemini", "antigravity-cli"), uid)
}

func readLegacy(home string, uid int) (map[string]json.RawMessage, token, error) {
	d, err := legacyDirectory(home, uid)
	if err != nil {
		return nil, token{}, err
	}
	defer d.close()
	raw, err := d.read(legacyFilename)
	if err != nil {
		return nil, token{}, errors.New("legacy_credential_unavailable")
	}
	var doc map[string]json.RawMessage
	if uniqueJSON(raw) != nil || json.Unmarshal(raw, &doc) != nil || doc == nil {
		return nil, token{}, errors.New("legacy_credential_invalid")
	}
	body := raw
	if wrapped, ok := doc["token"]; ok {
		body = wrapped
	}
	var t token
	if json.Unmarshal(body, &t) != nil || strings.TrimSpace(t.Refresh) == "" {
		return nil, token{}, errors.New("legacy_refresh_token_missing")
	}
	return doc, t, nil
}

func writeLegacy(home string, uid int, doc map[string]json.RawMessage, t token, project string, expire bool) error {
	body, wrapped := doc["token"]
	fields := map[string]json.RawMessage{}
	if wrapped {
		if json.Unmarshal(body, &fields) != nil || fields == nil {
			return errors.New("legacy_credential_invalid")
		}
	} else {
		fields = doc
	}
	if expire {
		t.Expiry = time.Unix(0, 0).UTC()
	}
	for k, v := range map[string]any{"access_token": t.Access, "refresh_token": t.Refresh, "token_type": "Bearer", "expiry": t.Expiry, "expires_in": t.ExpiresIn} {
		fields[k], _ = json.Marshal(v)
	}
	if wrapped {
		doc["token"], _ = json.Marshal(fields)
	}
	if project != "" {
		doc["project_id"], _ = json.Marshal(project)
	}
	raw, _ := json.Marshal(doc)
	d, err := legacyDirectory(home, uid)
	if err != nil {
		return err
	}
	defer d.close()
	return d.write(legacyFilename, raw)
}

// Save independently of canceled requests: a rotated refresh token must survive
// a failed identity/project/quota check, cancellation or a later failed account.
func (m *migrator) saveLegacy(name, home string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := m.keys.Save(ctx, name, home); err != nil {
		return errors.New("latest_keyring_save_failed")
	}
	return nil
}

func (m *migrator) forward(ctx context.Context, r antigravity.AccountRecord, home string, report *Report) error {
	if err := m.keys.Restore(ctx, r.Name, home); err != nil {
		return errors.New("legacy_restore_failed")
	}
	doc, old, err := readLegacy(home, m.legacyUID)
	if err != nil {
		return err
	}
	fresh, err := m.provider.refresh(ctx, old)
	if err != nil {
		return err
	}
	report.Refreshed = true
	// Persist a rotated token before any further operation can fail. Recovery
	// files are private, have no .json extension, and are not loaded by CPA.
	recoveryRaw, _ := json.Marshal(fresh)
	if err := m.oauth.write(".gateway-migration-recovery-"+r.ID, recoveryRaw); err != nil {
		return err
	}
	if err := writeLegacy(home, m.legacyUID, doc, fresh, "", false); err != nil {
		return err
	}
	if err := m.saveLegacy(r.Name, home); err != nil {
		return err
	}
	subject, email, err := m.provider.identity(ctx, fresh)
	if err != nil {
		return err
	}
	if err := m.checkMapping(subject, r.ID); err != nil {
		return err
	}
	project, err := m.provider.project(ctx, fresh)
	if err != nil {
		return err
	}
	models, err := m.provider.models(ctx, fresh, project)
	if err != nil {
		return err
	}
	// Existing CPA files are matched by verified Google subject, never filename.
	filename, existing, err := m.findCredential(subject)
	if err != nil {
		return err
	}
	if filename == "" {
		filename = "antigravity-" + r.ID + ".json"
		existing = map[string]any{}
	}
	// Never overwrite a colliding file belonging to a different provider/user.
	if _, err := m.oauth.read(filename); err == nil && len(existing) == 0 {
		return errors.New("credential_filename_conflict")
	}
	setCPAToken(existing, fresh)
	existing["type"], existing["google_subject"], existing["email"], existing["project_id"], existing["disabled"] = "antigravity", subject, email, project, true
	m.identities.Accounts[subject] = r.ID
	mappingRaw, _ := json.Marshal(m.identities)
	if err := m.oauth.write(identityFilename, mappingRaw); err != nil {
		return err
	}
	credentialRaw, _ := json.Marshal(existing)
	if err := m.oauth.write(filename, credentialRaw); err != nil {
		return err
	}
	report.Models = models
	return nil
}

func setCPAToken(doc map[string]any, t token) {
	doc["access_token"], doc["refresh_token"], doc["token_type"] = t.Access, t.Refresh, "Bearer"
	doc["expired"], doc["expires_in"], doc["timestamp"] = t.Expiry.Format(time.RFC3339Nano), t.ExpiresIn, t.Expiry.Add(-time.Duration(t.ExpiresIn)*time.Second).UnixMilli()
}

func (m *migrator) findCredential(subject string) (string, map[string]any, error) {
	// Enumerate the already validated directory descriptor, preserving safety
	// if a parent is renamed. Reset iteration before each account scan.
	if _, err := m.oauth.file.Seek(0, 0); err != nil {
		return "", nil, errors.New("credential_directory_unavailable")
	}
	entries, err := m.oauth.file.ReadDir(-1)
	if err != nil {
		return "", nil, errors.New("credential_directory_unavailable")
	}
	var filename string
	var selected map[string]any
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := m.oauth.read(entry.Name())
		if err != nil {
			return "", nil, err
		}
		var doc map[string]any
		if uniqueJSON(raw) != nil || json.Unmarshal(raw, &doc) != nil || doc == nil {
			return "", nil, errors.New("credential_invalid")
		}
		if doc["type"] != "antigravity" || doc["google_subject"] != subject {
			continue
		}
		if filename != "" {
			return "", nil, errors.New("duplicate_identity_quarantined")
		}
		filename, selected = entry.Name(), doc
	}
	return filename, selected, nil
}

func (m *migrator) reverse(ctx context.Context, r antigravity.AccountRecord, home string, report *Report) error {
	var subject string
	for key, id := range m.identities.Accounts {
		if id == r.ID {
			subject = key
		}
	}
	if subject == "" {
		return errors.New("identity_mapping_missing")
	}
	filename, cpa, err := m.findCredential(subject)
	if err != nil {
		return err
	}
	if filename == "" {
		return errors.New("current_cpa_credential_missing")
	}
	raw, _ := json.Marshal(cpa)
	var old token
	if json.Unmarshal(raw, &old) != nil {
		return errors.New("cpa_credential_invalid")
	}
	fresh, err := m.provider.refresh(ctx, old)
	if err != nil {
		return err
	}
	setCPAToken(cpa, fresh)
	cpa["disabled"] = true
	raw, _ = json.Marshal(cpa)
	if err := m.oauth.write(filename, raw); err != nil {
		return err
	}
	report.Refreshed = true
	actual, _, err := m.provider.identity(ctx, fresh)
	if err != nil {
		return err
	}
	if actual != subject {
		return errors.New("identity_conflict_reauthorize")
	}
	project, err := m.provider.project(ctx, fresh)
	if err != nil {
		return err
	}
	if err := m.keys.Restore(ctx, r.Name, home); err != nil {
		return errors.New("legacy_restore_failed")
	}
	doc, _, err := readLegacy(home, m.legacyUID)
	if err != nil {
		return err
	}
	// Force the legacy CLI to prove it can refresh the converted credential.
	if err := writeLegacy(home, m.legacyUID, doc, fresh, project, true); err != nil {
		return err
	}
	if err := m.saveLegacy(r.Name, home); err != nil {
		return err
	}
	verifyErr := m.keys.Verify(ctx, r.Name, m.temp)
	// Verification may rotate tokens even on error. Always preserve the newest
	// Keyring generation in the CPA file before reporting its outcome.
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := m.keys.Restore(cleanupCtx, r.Name, home); err != nil {
		return errors.New("latest_legacy_restore_failed")
	}
	_, latest, err := readLegacy(home, m.legacyUID)
	if err != nil {
		return err
	}
	setCPAToken(cpa, latest)
	raw, _ = json.Marshal(cpa)
	if err := m.oauth.write(filename, raw); err != nil {
		return err
	}
	if verifyErr != nil {
		return errors.New("legacy_refresh_or_generation_failed_reauthorize")
	}
	return nil
}
