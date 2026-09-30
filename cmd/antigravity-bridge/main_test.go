package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/wsw/codex-gateway/internal/antigravity"
)

func TestReauthorizeRequiresExplicitExistingRegistrySlot(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("codex-gateway-antigravity:existing"))
	registry := struct {
		Accounts []antigravity.AccountRecord `json:"accounts"`
	}{Accounts: []antigravity.AccountRecord{{ID: hex.EncodeToString(sum[:8]), Name: "existing", Enabled: false}}}
	raw, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "accounts.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTIGRAVITY_ACCOUNT_REGISTRY_PATH", path)
	t.Setenv("AGY_BINARY", filepath.Join(dir, "must-never-run"))
	args := os.Args
	defer func() { os.Args = args }()
	for _, tc := range []struct {
		args     []string
		category string
	}{
		{[]string{"antigravity-bridge", "auth-reauthorize"}, "configuration"},
		{[]string{"antigravity-bridge", "auth-reauthorize", "typo-new-account"}, "credential_missing"},
	} {
		os.Args = tc.args
		err := run(slog.New(slog.NewTextHandler(io.Discard, nil)))
		var authErr *antigravity.AuthError
		if !errors.As(err, &authErr) || authErr.Stage != "authorization" || authErr.Category != tc.category {
			t.Fatalf("args %v: error %v, want authorization/%s", tc.args, err, tc.category)
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(raw) {
			t.Fatal("rejected reauthorization changed registry")
		}
	}
}
