package antigravity

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNamedCredentialsAreIsolatedFromLegacyAndOtherAccounts(t *testing.T) {
	memory := &memoryKeyring{}
	legacy := memory.manager()
	work := &KeyringCredentials{Account: accountID("work"), run: memory.run}
	other := &KeyringCredentials{Account: accountID("other"), run: memory.run}
	fixtures := []string{syntheticCredential, strings.ReplaceAll(syntheticCredential, "synthetic-private", "work-private"), strings.ReplaceAll(syntheticCredential, "synthetic-private", "other-private")}
	for index, keyring := range []*KeyringCredentials{legacy, work, other} {
		if err := keyring.Save(context.Background(), credentialHome(t, fixtures[index])); err != nil {
			t.Fatal(err)
		}
	}
	for index, keyring := range []*KeyringCredentials{legacy, work, other} {
		home := privateCredentialHome(t)
		if err := keyring.Restore(context.Background(), home); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(credentialPath(home))
		if err != nil || string(got) != fixtures[index] {
			t.Fatal("account restored another account's secret")
		}
	}
	// Refresh cleanup in the named namespace must preserve the other manifests.
	home := credentialHome(t, strings.ReplaceAll(fixtures[1], "work-private", "work-refreshed"))
	if err := work.Save(context.Background(), home); err != nil {
		t.Fatal(err)
	}
	for _, keyring := range []*KeyringCredentials{legacy, other} {
		if err := keyring.Restore(context.Background(), privateCredentialHome(t)); err != nil {
			t.Fatal("named refresh deleted other account", err)
		}
	}
}

func TestConcurrentCredentialSavesPreserveNewestCommittedGeneration(t *testing.T) {
	for _, staleRefresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged older request", true: "conflicting refresh"}[staleRefresh], func(t *testing.T) {
			memory := &memoryKeyring{}
			keyring := memory.manager()
			if err := keyring.Save(context.Background(), credentialHome(t, syntheticCredential)); err != nil {
				t.Fatal(err)
			}
			first, second := privateCredentialHome(t), privateCredentialHome(t)
			for _, home := range []string{first, second} {
				if err := keyring.Restore(context.Background(), home); err != nil {
					t.Fatal(err)
				}
			}
			newer := strings.ReplaceAll(syntheticCredential, "synthetic-private", "first-refreshed")
			if err := os.WriteFile(credentialPath(first), []byte(newer), 0600); err != nil {
				t.Fatal(err)
			}
			if err := keyring.Save(context.Background(), first); err != nil {
				t.Fatal(err)
			}
			if staleRefresh {
				if err := os.WriteFile(credentialPath(second), []byte(strings.ReplaceAll(syntheticCredential, "synthetic-private", "stale-refreshed")), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before := len(memory.calls)
			if err := keyring.Save(context.Background(), second); err != nil {
				t.Fatal(err)
			}
			for _, call := range memory.calls[before:] {
				if call.binary == "secret-tool" && (call.args[0] == "store" || call.args[0] == "clear") {
					t.Fatal("stale request mutated encrypted state")
				}
			}
			latest := privateCredentialHome(t)
			if err := keyring.Restore(context.Background(), latest); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(credentialPath(latest))
			if err != nil || string(data) != newer {
				t.Fatal("stale request overwrote refresh")
			}
			// A fresh request can continue to refresh after an earlier conflict.
			newest := strings.ReplaceAll(newer, "first-refreshed", "latest-refreshed")
			if err := os.WriteFile(credentialPath(latest), []byte(newest), 0600); err != nil {
				t.Fatal(err)
			}
			if err := keyring.Save(context.Background(), latest); err != nil {
				t.Fatal(err)
			}
			if err := keyring.Restore(context.Background(), first); err != nil {
				t.Fatal(err)
			}
			data, err = os.ReadFile(credentialPath(first))
			if err != nil || string(data) != newest {
				t.Fatal("later fresh refresh failed")
			}
		})
	}
}

func TestRestoreSerializesMultipartReadsWithRefreshCleanup(t *testing.T) {
	memory := &memoryKeyring{}
	keyring := memory.manager()
	if err := keyring.Save(context.Background(), credentialHome(t, syntheticCredential)); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var pause atomic.Bool
	keyring.run = func(ctx context.Context, binary string, args []string, stdin []byte, limit int) ([]byte, error) {
		data, err := memory.run(ctx, binary, args, stdin, limit)
		if binary == "secret-tool" && args[0] == "lookup" && strings.HasSuffix(strings.Join(args, " "), "record manifest") && pause.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return data, err
	}
	dest := privateCredentialHome(t)
	restored := make(chan error, 1)
	go func() { restored <- keyring.Restore(context.Background(), dest) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("restore did not read manifest")
	}
	newHome := credentialHome(t, strings.ReplaceAll(syntheticCredential, "synthetic-private", "changed-private"))
	saved := make(chan error, 1)
	go func() { saved <- keyring.Save(context.Background(), newHome) }()
	select {
	case err := <-saved:
		t.Fatalf("save crossed in-progress multipart restore: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-restored; err != nil {
		t.Fatal(err)
	}
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(credentialPath(dest))
	if err != nil || string(data) != syntheticCredential {
		t.Fatal("restore lost its manifest generation")
	}
}
