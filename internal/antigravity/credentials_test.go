package antigravity

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const syntheticCredential = `{"token":{"access_token":"synthetic-private-access","refresh_token":"synthetic-private-refresh","token_type":"Bearer","expiry":"2099-01-01T00:00:00Z"},"auth_method":"oauth","id_token":"synthetic-id","project_id":"synthetic-project","region":"us-central1","user_tier":"synthetic-tier","tier_display_name":"Synthetic tier","unknown_metadata":{"preserve":true}}`

type credentialCommand struct {
	binary string
	args   []string
	stdin  []byte
}

type memoryKeyring struct {
	values      map[string][]byte
	calls       []credentialCommand
	property    string
	search      string
	failAction  string
	failPart    string
	failContext bool
}

func (m *memoryKeyring) manager() *KeyringCredentials {
	if m.values == nil {
		m.values = make(map[string][]byte)
	}
	return &KeyringCredentials{run: m.run}
}

func (m *memoryKeyring) run(ctx context.Context, binary string, args []string, stdin []byte, limit int) ([]byte, error) {
	m.calls = append(m.calls, credentialCommand{binary, slices.Clone(args), slices.Clone(stdin)})
	if m.failContext {
		<-ctx.Done()
		return nil, credentialError(CredentialCategory(ctx.Err()))
	}
	if binary == "gdbus" {
		if args[len(args)-1] == "Locked" {
			if m.property != "" {
				return []byte(m.property), nil
			}
			return []byte("(<false>,)\n"), nil
		}
		if m.search != "" {
			return []byte(m.search), nil
		}
		return []byte("([objectpath '/org/freedesktop/secrets/collection/login/1'], @ao [])\n"), nil
	}
	if binary != "secret-tool" {
		panic("unexpected credential binary")
	}
	attrs := args[1:]
	if args[0] == "store" {
		attrs = args[3:]
		if args[2] != "--collection="+loginCollection {
			panic("wrong collection")
		}
		if len(stdin) > 8000 {
			panic("would silently truncate in Bookworm secret-tool")
		}
	}
	key := strings.Join(attrs, " ")
	if len(attrs) < 4 || !reflect.DeepEqual(attrs[:4], credentialAttributes()) {
		panic("missing fixed application/version attributes")
	}
	if args[0] == m.failAction && (m.failPart == "" || strings.HasSuffix(key, "part "+m.failPart)) {
		return nil, credentialError("keyring_failed")
	}
	switch args[0] {
	case "lookup":
		if value, ok := m.values[key]; ok {
			return slices.Clone(value), nil
		}
		return nil, credentialError("credential_missing")
	case "store":
		m.values[key] = slices.Clone(stdin)
		return nil, nil
	case "clear":
		for item := range m.values {
			if strings.HasPrefix(item, key+" ") {
				delete(m.values, item)
			}
		}
		return nil, nil
	}
	panic("unexpected credential operation")
}

func credentialHome(t *testing.T, data string) string {
	t.Helper()
	home := privateCredentialHome(t)
	dir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, credentialFilename), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return home
}

func credentialPath(home string) string {
	return filepath.Join(home, ".gemini", "antigravity-cli", credentialFilename)
}

func assertCredentialCategory(t *testing.T, err error, category string) {
	t.Helper()
	if err == nil || CredentialCategory(err) != category {
		t.Fatalf("got %v, want category %s", err, category)
	}
	if strings.Contains(err.Error(), "synthetic-private") || strings.Contains(err.Error(), "/tmp/") {
		t.Fatalf("error leaked sensitive content: %v", err)
	}
}

func TestCredentialsPersistCompleteFileAndRefresh(t *testing.T) {
	for _, fixture := range []string{syntheticCredential, `{"access_token":"old-direct","refresh_token":"old-refresh","expiry":"2000-01-01T00:00:00Z"}`} {
		t.Run(fixture[:12], func(t *testing.T) {
			memory := &memoryKeyring{}
			manager := memory.manager()
			home := credentialHome(t, fixture)
			if err := manager.Save(context.Background(), home); err != nil {
				t.Fatal(err)
			}
			// Saving never captures settings, history or arbitrary workspace data.
			if err := os.WriteFile(filepath.Join(home, ".gemini", "antigravity-cli", "settings.json"), []byte("private-settings"), 0600); err != nil {
				t.Fatal(err)
			}
			nextHome := privateCredentialHome(t)
			if err := manager.Restore(context.Background(), nextHome); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(credentialPath(nextHome))
			if string(got) != fixture {
				t.Fatal("restore did not preserve the complete authentication file")
			}
			for _, path := range []string{nextHome, filepath.Join(nextHome, ".gemini"), filepath.Join(nextHome, ".gemini", "antigravity-cli"), credentialPath(nextHome)} {
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != map[bool]os.FileMode{true: 0700, false: 0600}[info.IsDir()] {
					t.Fatalf("unsafe restored mode: %v %v", info, err)
				}
			}
			if _, err := os.Stat(filepath.Join(nextHome, ".gemini", "antigravity-cli", "settings.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("settings were persisted")
			}
			// Change a value, preserving the schema for both supported forms.
			refreshed := strings.Replace(fixture, "old-direct", "new-direct", 1)
			if fixture == syntheticCredential {
				refreshed = strings.Replace(fixture, "synthetic-private-access", "synthetic-private-refreshed", 1)
			}
			if err := os.WriteFile(credentialPath(nextHome), []byte(refreshed), 0600); err != nil {
				t.Fatal(err)
			}
			if err := manager.Save(context.Background(), nextHome); err != nil {
				t.Fatal(err)
			}
			lastHome := privateCredentialHome(t)
			if err := manager.Restore(context.Background(), lastHome); err != nil {
				t.Fatal(err)
			}
			got, _ = os.ReadFile(credentialPath(lastHome))
			if string(got) != refreshed || len(memory.values) != 2 {
				t.Fatal("refresh did not replace the credential and remove superseded chunks")
			}
			for _, call := range memory.calls {
				if strings.Contains(strings.Join(call.args, " "), "synthetic-private") || strings.Contains(strings.Join(call.args, " "), "old-direct") {
					t.Fatal("credential value entered command arguments")
				}
			}
		})
	}
}

func TestCredentialsLargeFileAndAtomicManifest(t *testing.T) {
	// The exact 1 MiB boundary also exceeds secret-tool's 8 KiB stdin limit.
	base := `{"refresh_token":"large-synthetic","metadata":""}`
	fixture := strings.Replace(base, `"metadata":""`, `"metadata":"`+strings.Repeat("x", credentialLimit-len(base))+`"`, 1)
	if len(fixture) != credentialLimit {
		t.Fatal("incorrect boundary fixture")
	}
	memory := &memoryKeyring{}
	manager := memory.manager()
	home := credentialHome(t, fixture)
	if err := manager.Save(context.Background(), home); err != nil {
		t.Fatal(err)
	}
	nextHome := privateCredentialHome(t)
	if err := manager.Restore(context.Background(), nextHome); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(credentialPath(nextHome))
	if string(got) != fixture {
		t.Fatal("multipart file was truncated")
	}
	manifestKey := strings.Join(credentialAttributes("record", "manifest"), " ")
	before := slices.Clone(memory.values[manifestKey])
	memory.failAction, memory.failPart = "store", "1"
	assertCredentialCategory(t, manager.Save(context.Background(), home), "keyring_failed")
	if !bytes.Equal(before, memory.values[manifestKey]) {
		t.Fatal("incomplete generation replaced the committed manifest")
	}
	memory.failAction = ""
	if err := manager.Restore(context.Background(), privateCredentialHome(t)); err != nil {
		t.Fatalf("prior credential no longer restores after incomplete refresh: %v", err)
	}
}

func TestCredentialsRejectInvalidWithoutOverwritingKeyring(t *testing.T) {
	fixtures := []struct{ name, data, category string }{
		{"empty", "", "credential_invalid"},
		{"broken_json", `{"token":`, "credential_invalid"},
		{"null", "null", "credential_invalid"},
		{"array", "[]", "credential_invalid"},
		{"no_auth", `{"project_id":"project"}`, "credential_invalid"},
		{"null_token", `{"token":null}`, "credential_invalid"},
		{"string_token", `{"token":"secret"}`, "credential_invalid"},
		{"empty_token", `{"token":{}}`, "credential_invalid"},
		{"empty_wrapper_outer_token", `{"token":{},"access_token":"outer-ignored-by-agy"}`, "credential_invalid"},
		{"duplicate_wrapper", `{"token":{"access_token":"first"},"token":{"access_token":"second"}}`, "credential_invalid"},
		{"duplicate_nested", `{"token":{"access_token":"first","access_token":"second"}}`, "credential_invalid"},
		{"invalid_utf8", "{\"access_token\":\"\xff\"}", "credential_invalid"},
		{"wrong_access_type", `{"access_token":1}`, "credential_invalid"},
		{"null_refresh", `{"access_token":"ok","refresh_token":null}`, "credential_invalid"},
		{"bad_expiry", `{"refresh_token":"ok","expiry":"tomorrow"}`, "credential_invalid"},
		{"bad_expires_in", `{"refresh_token":"ok","expires_in":"3600"}`, "credential_invalid"},
		{"fractional_expires_in", `{"refresh_token":"ok","expires_in":0.5}`, "credential_invalid"},
		{"bad_token_type", `{"refresh_token":"ok","token_type":false}`, "credential_invalid"},
		{"bad_metadata_type", `{"token":{"refresh_token":"ok"},"user_tier":{}}`, "credential_invalid"},
		{"oversize", strings.Repeat("x", credentialLimit+1), "credential_too_large"},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			memory := &memoryKeyring{}
			manager := memory.manager()
			home := credentialHome(t, syntheticCredential)
			if err := manager.Save(context.Background(), home); err != nil {
				t.Fatal(err)
			}
			before := len(memory.calls)
			if err := os.WriteFile(credentialPath(home), []byte(fixture.data), 0600); err != nil {
				t.Fatal(err)
			}
			assertCredentialCategory(t, manager.Save(context.Background(), home), fixture.category)
			if len(memory.calls) != before {
				t.Fatal("invalid local file reached the keyring")
			}
			if err := manager.Restore(context.Background(), privateCredentialHome(t)); err != nil {
				t.Fatalf("original credential lost: %v", err)
			}
		})
	}
}

func TestCredentialsRejectUnsafePathsAndPermissions(t *testing.T) {
	for _, mode := range []string{"file_symlink", "dir_symlink", "home_symlink", "ancestor_symlink", "hardlink", "fifo", "directory_file", "file_mode", "dir_mode", "home_mode", "file_owner", "dir_owner", "home_owner", "missing"} {
		t.Run(mode, func(t *testing.T) {
			home := credentialHome(t, syntheticCredential)
			path := credentialPath(home)
			category := "unsafe_path"
			switch mode {
			case "file_symlink":
				_ = os.Remove(path)
				_ = os.Symlink("settings.json", path)
			case "dir_symlink":
				dir := filepath.Dir(path)
				_ = os.Rename(dir, dir+"-real")
				_ = os.Symlink(dir+"-real", dir)
			case "home_symlink":
				link := filepath.Join(privateCredentialHome(t), "home")
				_ = os.Symlink(home, link)
				home = link
			case "ancestor_symlink":
				link := filepath.Join(privateCredentialHome(t), "parent")
				_ = os.Symlink(filepath.Dir(home), link)
				home = filepath.Join(link, filepath.Base(home))
			case "hardlink":
				_ = os.Link(path, filepath.Join(home, "linked"))
			case "fifo":
				_ = os.Remove(path)
				_ = syscall.Mkfifo(path, 0600)
			case "directory_file":
				_ = os.Remove(path)
				_ = os.Mkdir(path, 0600)
			case "file_mode":
				_ = os.Chmod(path, 0644)
				category = "invalid_permissions"
			case "dir_mode":
				_ = os.Chmod(filepath.Dir(path), 0755)
				category = "invalid_permissions"
			case "home_mode":
				_ = os.Chmod(home, 0755)
				category = "invalid_permissions"
			case "file_owner", "dir_owner", "home_owner":
				if os.Geteuid() != 0 {
					t.Skip("changing credential ownership requires root")
				}
				target := path
				if mode == "dir_owner" {
					target = filepath.Dir(path)
				} else if mode == "home_owner" {
					target = home
				}
				if err := os.Chown(target, 1, -1); err != nil {
					t.Fatal(err)
				}
				category = "invalid_permissions"
			case "missing":
				_ = os.Remove(path)
				category = "credential_missing"
			}
			memory := &memoryKeyring{}
			assertCredentialCategory(t, memory.manager().Save(context.Background(), home), category)
			if len(memory.calls) != 0 {
				t.Fatal("unsafe local file reached keyring")
			}
		})
	}
}

func TestCredentialsLockedOrWrongCollectionFailsClosed(t *testing.T) {
	for _, fixture := range []struct{ property, search string }{
		{property: "(<true>,)"},
		{property: "untrusted-error synthetic-private-access"},
		{search: "([objectpath '/org/freedesktop/secrets/collection/session/1'], @ao [])"},
		{search: "([objectpath '/org/freedesktop/secrets/collection/login/1'], [objectpath '/org/freedesktop/secrets/collection/login/2'])"},
		{search: "invalid"},
	} {
		memory := &memoryKeyring{property: fixture.property, search: fixture.search}
		manager := memory.manager()
		assertCredentialCategory(t, manager.Save(context.Background(), credentialHome(t, syntheticCredential)), "keyring_failed")
		assertCredentialCategory(t, manager.Restore(context.Background(), privateCredentialHome(t)), "keyring_failed")
		for _, call := range memory.calls {
			if call.binary == "secret-tool" {
				t.Fatal("unsafe collection accessed secrets")
			}
		}
	}
}

func TestCredentialsRejectDuplicateKeyringItems(t *testing.T) {
	memory := &memoryKeyring{search: "([objectpath '/org/freedesktop/secrets/collection/login/1', '/org/freedesktop/secrets/collection/login/2'], @ao [])"}
	manager := memory.manager()
	assertCredentialCategory(t, manager.Restore(context.Background(), privateCredentialHome(t)), "keyring_failed")
	assertCredentialCategory(t, manager.Save(context.Background(), credentialHome(t, syntheticCredential)), "keyring_failed")
	for _, call := range memory.calls {
		if call.binary == "secret-tool" {
			t.Fatal("duplicate matching keyring items reached lookup or save")
		}
	}
}

func TestCredentialsKeyringCorruptionAndFailures(t *testing.T) {
	for _, failure := range []string{"missing", "lookup", "store", "bad_manifest", "bad_part", "missing_part", "oversize_manifest"} {
		t.Run(failure, func(t *testing.T) {
			memory := &memoryKeyring{}
			manager := memory.manager()
			home := credentialHome(t, syntheticCredential)
			if err := manager.Save(context.Background(), home); err != nil {
				t.Fatal(err)
			}
			category := "credential_invalid"
			switch failure {
			case "missing":
				clear(memory.values)
				category = "credential_missing"
			case "lookup", "store":
				memory.failAction = failure
				category = "keyring_failed"
			case "bad_manifest":
				memory.values[strings.Join(credentialAttributes("record", "manifest"), " ")] = []byte("bad base64!")
			case "oversize_manifest":
				memory.values[strings.Join(credentialAttributes("record", "manifest"), " ")] = []byte(base64.StdEncoding.EncodeToString([]byte(`{"size":1048577}`)))
			case "bad_part", "missing_part":
				for key := range memory.values {
					if strings.Contains(key, "record part") {
						if failure == "missing_part" {
							delete(memory.values, key)
						} else {
							memory.values[key] = []byte(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", len(syntheticCredential)))))
						}
					}
				}
			}
			var err error
			dest := privateCredentialHome(t)
			if failure == "store" {
				err = manager.Save(context.Background(), home)
			} else {
				err = manager.Restore(context.Background(), dest)
			}
			assertCredentialCategory(t, err, category)
			if failure == "missing" && !errors.Is(err, ErrCredentialsMissing) {
				t.Fatal("missing credential cannot be identified for first login")
			}
			if _, err := os.Stat(credentialPath(dest)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed restore created credential")
			}
		})
	}
}

func TestCredentialsRestoreRejectsDestinationSymlink(t *testing.T) {
	memory := &memoryKeyring{}
	manager := memory.manager()
	if err := manager.Save(context.Background(), credentialHome(t, syntheticCredential)); err != nil {
		t.Fatal(err)
	}
	home := credentialHome(t, "old")
	target := filepath.Join(privateCredentialHome(t), "must-not-overwrite")
	_ = os.WriteFile(target, []byte("unchanged"), 0600)
	_ = os.Remove(credentialPath(home))
	_ = os.Symlink(target, credentialPath(home))
	assertCredentialCategory(t, manager.Restore(context.Background(), home), "unsafe_path")
	got, _ := os.ReadFile(target)
	if string(got) != "unchanged" {
		t.Fatal("restore followed credential symlink")
	}
}

func TestCredentialHelpersHaveBoundedCancellationAndSafeErrors(t *testing.T) {
	dir := privateCredentialHome(t)
	script := filepath.Join(dir, "secret-tool")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nset -eu\nprintf 'synthetic-private-token https://auth.invalid/private' >&2\nsleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	manager := &KeyringCredentials{}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := manager.command(ctx, "secret-tool", []string{"lookup", "application", credentialApp}, nil, 1024)
	assertCredentialCategory(t, err, "timeout")
	if time.Since(start) > 2*time.Second {
		t.Fatal("helper timeout failed to clean up descendant process")
	}
	for _, exit := range []int{1, 2} {
		if err := os.WriteFile(script, []byte("#!/bin/sh\nset -eu\nprintf 'synthetic-private-token' >&2\nexit "+strconv.Itoa(exit)+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
		_, err := manager.command(context.Background(), "secret-tool", []string{"lookup"}, nil, 1024)
		assertCredentialCategory(t, err, "keyring_failed")
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\nset -eu\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	_, err = manager.command(context.Background(), "secret-tool", []string{"lookup"}, nil, 1024)
	if !errors.Is(err, ErrCredentialsMissing) {
		t.Fatalf("empty lookup should indicate missing, got %v", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	_, err = manager.command(ctx, "secret-tool", []string{"lookup"}, nil, 1024)
	assertCredentialCategory(t, err, "canceled")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nset -eu\nprintf '0123456789'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	_, err = manager.command(context.Background(), "secret-tool", []string{"lookup"}, nil, 5)
	assertCredentialCategory(t, err, "credential_too_large")
}

func privateCredentialHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	return home
}
