package antigravity

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	// #nosec G101 -- This is the CLI's fixed credential filename, not a credential value.
	credentialFilename         = "antigravity-oauth-token"
	credentialLimit            = 1 << 20
	credentialPartSize         = 6000 // Bookworm secret-tool silently truncates stdin after 8192 bytes.
	credentialApp              = "codex-gateway-antigravity"
	credentialVersion          = "1"
	credentialBaselineFilename = "antigravity-bridge-baseline"
	loginCollection            = "/org/freedesktop/secrets/collection/login"
)

var ErrCredentialsMissing = errors.New("Antigravity credentials are missing")

// CredentialError deliberately excludes underlying errors, paths, command output,
// and secret values so that callers can safely report its fixed category.
type CredentialError struct{ Category string }

func (e *CredentialError) Error() string { return "Antigravity credentials: " + e.Category }
func (e *CredentialError) Is(target error) bool {
	return target == ErrCredentialsMissing && e.Category == "credential_missing"
}

func CredentialCategory(err error) string {
	var credentialErr *CredentialError
	if errors.As(err, &credentialErr) {
		return credentialErr.Category
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	return "io_failed"
}

func credentialError(category string) error { return &CredentialError{Category: category} }

type Credentials interface {
	Restore(context.Context, string) error
	Save(context.Context, string) error
}

// KeyringCredentials stores only the complete authentication file. Settings,
// history and caches never cross the isolated HOME boundary. Callers serialize
// all CLI calls and use an independent bounded context for Save after CLI exit.
type KeyringCredentials struct {
	// Account is empty only for the original default account. Distinct
	// application attributes prevent partial Secret Service searches from
	// mixing named credentials with the legacy account.
	Account string
	mu      sync.Mutex
	run     func(context.Context, string, []string, []byte, int) ([]byte, error)
}

type credentialManifest struct {
	Generation string `json:"generation"`
	Size       int    `json:"size"`
	Parts      int    `json:"parts"`
	SHA256     string `json:"sha256"`
}

func credentialAttributes(extra ...string) []string {
	return append([]string{"application", credentialApp, "version", credentialVersion}, extra...)
}

func (k *KeyringCredentials) attributes(extra ...string) []string {
	attrs := credentialAttributes(extra...)
	if k.Account != "" {
		attrs[1] += "-" + k.Account
	}
	return attrs
}

func (k *KeyringCredentials) command(ctx context.Context, binary string, args []string, stdin []byte, limit int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, credentialError(CredentialCategory(err))
	}
	if k.run != nil {
		return k.run(ctx, binary, args, stdin, limit)
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	// bytes.Reader always gives helpers a pipe or /dev/null, never the login TTY.
	cmd.Stdin = bytes.NewReader(stdin)
	stdout := &cappedBuffer{limit: limit}
	stderr := &cappedBuffer{limit: 0}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 100 * time.Millisecond
	err := cmd.Run()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if ctx.Err() != nil {
		return nil, credentialError(CredentialCategory(ctx.Err()))
	}
	if stdout.count > int64(limit) {
		return nil, credentialError("credential_too_large")
	}
	if err != nil {
		// libsecret distinguishes a missing lookup (exit 1, no output) from
		// backend errors (exit 1 and stderr). Never retain that stderr.
		var exit *exec.ExitError
		if binary == "secret-tool" && len(args) > 0 && args[0] == "lookup" && errors.As(err, &exit) && exit.ExitCode() == 1 && stdout.count == 0 && stderr.count == 0 {
			return nil, credentialError("credential_missing")
		}
		return nil, credentialError("keyring_failed")
	}
	if stderr.count != 0 {
		return nil, credentialError("keyring_failed")
	}
	return stdout.Bytes(), nil
}

var searchItemsPattern = regexp.MustCompile(`^\((?:@ao )?\[([^\]]*)\], (?:@ao )?\[([^\]]*)\]\)$`)
var itemPathPattern = regexp.MustCompile(`^(?:objectpath )?'(/org/freedesktop/secrets/collection/login/[A-Za-z0-9_]+)'$`)

func (k *KeyringCredentials) checkCollection(ctx context.Context) error {
	args := []string{"call", "--session", "--dest", "org.freedesktop.secrets", "--object-path", loginCollection, "--method", "org.freedesktop.DBus.Properties.Get", "org.freedesktop.Secret.Collection", "Locked"}
	locked, err := k.command(ctx, "gdbus", args, nil, 4096)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(locked)) != "(<false>,)" {
		return credentialError("keyring_failed")
	}
	// lookup cannot select a collection. Reject any matching shadow item in
	// another collection, or any locked matching item, before using lookup.
	_, err = k.searchItems(ctx)
	return err
}

func (k *KeyringCredentials) searchItems(ctx context.Context, extra ...string) (int, error) {
	attrs := k.attributes(extra...)
	pairs := make([]string, 0, len(attrs)/2)
	for index := 0; index < len(attrs); index += 2 {
		pairs = append(pairs, fmt.Sprintf("%q: %q", attrs[index], attrs[index+1]))
	}
	args := []string{"call", "--session", "--dest", "org.freedesktop.secrets", "--object-path", "/org/freedesktop/secrets", "--method", "org.freedesktop.Secret.Service.SearchItems", "{" + strings.Join(pairs, ", ") + "}"}
	items, err := k.command(ctx, "gdbus", args, nil, 1<<20)
	if err != nil {
		return 0, err
	}
	matches := searchItemsPattern.FindStringSubmatch(strings.TrimSpace(string(items)))
	if matches == nil || strings.TrimSpace(matches[2]) != "" {
		return 0, credentialError("keyring_failed")
	}
	if strings.TrimSpace(matches[1]) != "" {
		paths := strings.Split(matches[1], ",")
		for _, path := range paths {
			if !itemPathPattern.MatchString(strings.TrimSpace(path)) {
				return 0, credentialError("keyring_failed")
			}
		}
		return len(paths), nil
	}
	return 0, nil
}

func (k *KeyringCredentials) lookup(ctx context.Context, extra ...string) ([]byte, error) {
	count, err := k.searchItems(ctx, extra...)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, credentialError("credential_missing")
	}
	if count != 1 {
		return nil, credentialError("keyring_failed")
	}
	args := append([]string{"lookup"}, k.attributes(extra...)...)
	encoded, err := k.command(ctx, "secret-tool", args, nil, 8192)
	if err != nil {
		return nil, err
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSuffix(string(encoded), "\n"))
	if err != nil || len(decoded) == 0 {
		return nil, credentialError("credential_invalid")
	}
	return decoded, nil
}

func (k *KeyringCredentials) store(ctx context.Context, data []byte, extra ...string) error {
	args := append([]string{"store", "--label=Antigravity Bridge credentials", "--collection=" + loginCollection}, k.attributes(extra...)...)
	_, err := k.command(ctx, "secret-tool", args, []byte(base64.StdEncoding.EncodeToString(data)), 4096)
	return err
}

func (k *KeyringCredentials) manifest(ctx context.Context) (credentialManifest, error) {
	var manifest credentialManifest
	data, err := k.lookup(ctx, "record", "manifest")
	if err != nil {
		return manifest, err
	}
	if uniqueJSON(data) != nil || json.Unmarshal(data, &manifest) != nil || len(manifest.Generation) != 32 || len(manifest.SHA256) != 64 || manifest.Size < 1 || manifest.Size > credentialLimit || manifest.Parts != (manifest.Size+credentialPartSize-1)/credentialPartSize {
		return manifest, credentialError("credential_invalid")
	}
	if _, err := hex.DecodeString(manifest.Generation); err != nil {
		return manifest, credentialError("credential_invalid")
	}
	if _, err := hex.DecodeString(manifest.SHA256); err != nil {
		return manifest, credentialError("credential_invalid")
	}
	return manifest, nil
}

func (k *KeyringCredentials) Restore(ctx context.Context, home string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.checkCollection(ctx); err != nil {
		return err
	}
	manifest, err := k.manifest(ctx)
	if err != nil {
		return err
	}
	data := make([]byte, 0, manifest.Size)
	for part := 0; part < manifest.Parts; part++ {
		chunk, err := k.lookup(ctx, "record", "part", "generation", manifest.Generation, "part", strconv.Itoa(part))
		if errors.Is(err, ErrCredentialsMissing) {
			return credentialError("credential_invalid")
		}
		if err != nil {
			return err
		}
		remaining := manifest.Size - len(data)
		if len(chunk) != min(credentialPartSize, remaining) {
			return credentialError("credential_invalid")
		}
		data = append(data, chunk...)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != manifest.SHA256 {
		return credentialError("credential_invalid")
	}
	if err := validateCredential(data); err != nil {
		return err
	}
	dir, err := credentialDirectory(home, true)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := writeCredential(dir, data); err != nil {
		return err
	}
	return writeCredentialBaseline(dir, manifest)
}

func (k *KeyringCredentials) Save(ctx context.Context, home string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	dir, err := credentialDirectory(home, false)
	if err != nil {
		return err
	}
	defer dir.Close()
	file, err := openCredential(dir)
	if err != nil {
		return err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, credentialLimit+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return credentialError("io_failed")
	}
	if err := validateCredential(data); err != nil {
		return err
	}
	baseline, err := readCredentialBaseline(dir)
	if err != nil {
		return err
	}
	if err := k.checkCollection(ctx); err != nil {
		return err
	}
	old, err := k.manifest(ctx)
	if err != nil && !errors.Is(err, ErrCredentialsMissing) {
		return err
	}
	sum := sha256.Sum256(data)
	if baseline.Generation != "" {
		if old.Generation == "" {
			return credentialError("keyring_failed")
		}
		// Every request starts from an isolated snapshot. An unchanged snapshot
		// or a refresh based on an obsolete generation must never replace a
		// newer credential committed by another in-flight request.
		if baseline.SHA256 == hex.EncodeToString(sum[:]) || baseline.Generation != old.Generation {
			return nil
		}
	}
	var generation [16]byte
	if _, err := rand.Read(generation[:]); err != nil {
		return credentialError("io_failed")
	}
	manifest := credentialManifest{Generation: hex.EncodeToString(generation[:]), Size: len(data), Parts: (len(data) + credentialPartSize - 1) / credentialPartSize, SHA256: hex.EncodeToString(sum[:])}
	commitAttempted := false
	defer func() {
		if !commitAttempted {
			// This generation is not referenced. Cleanup uses the same bounded
			// context; if it is exhausted encrypted orphan parts are harmless.
			_ = k.clearGeneration(ctx, manifest.Generation)
		}
	}()
	for part := 0; part < manifest.Parts; part++ {
		start := part * credentialPartSize
		if err := k.store(ctx, data[start:min(start+credentialPartSize, len(data))], "record", "part", "generation", manifest.Generation, "part", strconv.Itoa(part)); err != nil {
			return err
		}
	}
	index, _ := json.Marshal(manifest)
	// Commit the pointer only after every chunk has been stored successfully.
	// In particular, an incomplete refresh never replaces the prior credential.
	commitAttempted = true
	if err := k.store(ctx, index, "record", "manifest"); err != nil {
		return err
	}
	if old.Generation != "" {
		if err := k.clearGeneration(ctx, old.Generation); err != nil {
			return err
		}
	}
	return writeCredentialBaseline(dir, manifest)
}

type credentialBaseline struct {
	Generation string `json:"generation"`
	SHA256     string `json:"sha256"`
}

func writeCredentialBaseline(dir *os.File, manifest credentialManifest) error {
	data, _ := json.Marshal(credentialBaseline{Generation: manifest.Generation, SHA256: manifest.SHA256})
	return writePrivateCredentialFile(dir, credentialBaselineFilename, data)
}

func readCredentialBaseline(dir *os.File) (credentialBaseline, error) {
	var baseline credentialBaseline
	file, err := openPrivateCredentialFile(dir, credentialBaselineFilename)
	if errors.Is(err, ErrCredentialsMissing) {
		return baseline, nil
	}
	if err != nil {
		return baseline, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 257))
	if err != nil || len(data) > 256 || uniqueJSON(data) != nil || json.Unmarshal(data, &baseline) != nil || len(baseline.Generation) != 32 || len(baseline.SHA256) != 64 {
		return baseline, credentialError("credential_invalid")
	}
	if _, err := hex.DecodeString(baseline.Generation); err != nil {
		return baseline, credentialError("credential_invalid")
	}
	if _, err := hex.DecodeString(baseline.SHA256); err != nil {
		return baseline, credentialError("credential_invalid")
	}
	return baseline, nil
}

func (k *KeyringCredentials) clearGeneration(ctx context.Context, generation string) error {
	args := append([]string{"clear"}, k.attributes("record", "part", "generation", generation)...)
	_, err := k.command(ctx, "secret-tool", args, nil, 4096)
	return err
}

func validateCredential(data []byte) error {
	if len(data) > credentialLimit {
		return credentialError("credential_too_large")
	}
	var object map[string]json.RawMessage
	if len(data) == 0 || !utf8.Valid(data) || uniqueJSON(data) != nil || json.Unmarshal(data, &object) != nil || object == nil {
		return credentialError("credential_invalid")
	}
	token := object
	if wrapped, ok := object["token"]; ok {
		token = nil
		if json.Unmarshal(wrapped, &token) != nil || token == nil {
			return credentialError("credential_invalid")
		}
		// Fields and their types come from StoredToken in the checksum-pinned
		// AGY 1.2.4 binary. Unknown fields remain preserved byte for byte.
		for _, key := range []string{"auth_method", "id_token", "wif_provider", "saved_wif_provider", "project_id", "region", "user_tier", "tier_display_name"} {
			if raw, ok := object[key]; ok {
				var value string
				if bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &value) != nil {
					return credentialError("credential_invalid")
				}
			}
		}
	}
	var access, refresh string
	for _, field := range []struct {
		key string
		dst *string
	}{{"access_token", &access}, {"refresh_token", &refresh}} {
		if raw, ok := token[field.key]; ok && (bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, field.dst) != nil) {
			return credentialError("credential_invalid")
		}
	}
	if strings.TrimSpace(access) == "" && strings.TrimSpace(refresh) == "" {
		return credentialError("credential_invalid")
	}
	for _, key := range []string{"token_type", "id_token"} {
		if raw, ok := token[key]; ok {
			var value string
			if bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &value) != nil {
				return credentialError("credential_invalid")
			}
		}
	}
	if raw, ok := token["expiry"]; ok {
		var expiry time.Time
		if bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &expiry) != nil {
			return credentialError("credential_invalid")
		}
	}
	if raw, ok := token["expires_in"]; ok {
		var expiresIn int64
		if bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &expiresIn) != nil {
			return credentialError("credential_invalid")
		}
	}
	return nil
}

func credentialFileError(err error) error {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return credentialError("credential_missing")
	case errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.ENOTDIR):
		return credentialError("unsafe_path")
	case errors.Is(err, os.ErrPermission):
		return credentialError("invalid_permissions")
	default:
		return credentialError("io_failed")
	}
}

func checkCredentialFile(file *os.File, directory bool) error {
	info, err := file.Stat()
	if err != nil {
		return credentialFileError(err)
	}
	mode := os.FileMode(0600)
	if directory {
		mode = 0700
	}
	if info.IsDir() != directory || (!directory && !info.Mode().IsRegular()) {
		return credentialError("unsafe_path")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int64(stat.Uid) != int64(os.Geteuid()) || info.Mode().Perm() != mode || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return credentialError("invalid_permissions")
	}
	if !directory && stat.Nlink != 1 {
		return credentialError("unsafe_path")
	}
	if !directory && info.Size() > credentialLimit {
		return credentialError("credential_too_large")
	}
	return nil
}

// Walk each path component with openat and O_NOFOLLOW. Keeping directory file
// descriptors prevents a symlink substitution between validation and file I/O.
func credentialDirectory(home string, create bool) (*os.File, error) {
	if !filepath.IsAbs(home) || filepath.Clean(home) == "/" {
		return nil, credentialError("unsafe_path")
	}
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, credentialFileError(err)
	}
	components := strings.Split(strings.TrimPrefix(filepath.Clean(home), "/"), "/")
	homeIndex := len(components) - 1
	components = append(components, ".gemini", "antigravity-cli")
	for index, component := range components {
		next, err := syscall.Openat(fd, component, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if errors.Is(err, os.ErrNotExist) && create && index > homeIndex {
			if mkdirErr := syscall.Mkdirat(fd, component, 0700); mkdirErr != nil && !errors.Is(mkdirErr, os.ErrExist) {
				_ = syscall.Close(fd)
				return nil, credentialFileError(mkdirErr)
			}
			next, err = syscall.Openat(fd, component, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		}
		_ = syscall.Close(fd)
		if err != nil {
			return nil, credentialFileError(err)
		}
		fd = next
		if index >= homeIndex {
			file := os.NewFile(uintptr(fd), "credential-directory")
			if err := checkCredentialFile(file, true); err != nil {
				_ = file.Close()
				return nil, err
			}
			if index == len(components)-1 {
				return file, nil
			}
			// Keep ownership of fd here; File must not finalize a descriptor
			// which is reused by the next iteration.
			next, err := syscall.Dup(fd)
			_ = file.Close()
			if err != nil {
				return nil, credentialFileError(err)
			}
			syscall.CloseOnExec(next)
			fd = next
		}
	}
	panic("unreachable credential directory")
}

func openCredential(dir *os.File) (*os.File, error) {
	return openPrivateCredentialFile(dir, credentialFilename)
}

func openPrivateCredentialFile(dir *os.File, name string) (*os.File, error) {
	fd, err := syscall.Openat(int(dir.Fd()), name, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, credentialFileError(err)
	}
	file := os.NewFile(uintptr(fd), "credential-file")
	if err := checkCredentialFile(file, false); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func writeCredential(dir *os.File, data []byte) error {
	return writePrivateCredentialFile(dir, credentialFilename, data)
}

func writePrivateCredentialFile(dir *os.File, destination string, data []byte) error {
	if existing, err := openPrivateCredentialFile(dir, destination); err == nil {
		_ = existing.Close()
	} else if !errors.Is(err, ErrCredentialsMissing) {
		return err
	}
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return credentialError("io_failed")
	}
	name := ".credential-" + hex.EncodeToString(suffix[:])
	fd, err := syscall.Openat(int(dir.Fd()), name, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return credentialFileError(err)
	}
	defer syscall.Unlinkat(int(dir.Fd()), name)
	file := os.NewFile(uintptr(fd), "credential-temporary")
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return credentialError("io_failed")
	}
	if err := syscall.Renameat(int(dir.Fd()), name, int(dir.Fd()), destination); err != nil {
		return credentialFileError(err)
	}
	if err := dir.Sync(); err != nil {
		return credentialFileError(err)
	}
	return nil
}
