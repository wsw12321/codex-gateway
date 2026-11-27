package gatewayplugin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func seatDocument(workspace, member, email string) map[string]any {
	claims := map[string]string{"chatgpt_account_id": workspace}
	if member != "" {
		claims["chatgpt_user_id"] = member
	}
	raw, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": claims})
	return map[string]any{"type": "codex", "email": email, "account_id": workspace,
		"id_token": "h." + base64.RawURLEncoding.EncodeToString(raw) + ".s"}
}

func TestCodexSeatsRegisterAndDeduplicatePerWorkspaceMember(t *testing.T) {
	f := newFixture(t, 0)
	register := func(file, workspace, member, email string) string {
		t.Helper()
		f.advance(time.Minute)
		f.host.addEntry(file, ProviderCodex, seatDocument(workspace, member, email), f.clock)
		id, err := f.plugin.completeCredential(context.Background(), ProviderCodex, file, testRegistration, nil)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	alice := register("a.json", "team", "user-a", "a@example.test")
	bob := register("b.json", "team", "user-b", "b@example.test")
	other := register("other.json", "another-team", "user-a", "a@example.test")
	if again := register("a2.json", "team", "user-a", "renamed@example.test"); again != alice {
		t.Fatalf("re-login changed seat %s to %s", alice, again)
	}
	if alice == bob || alice == other || alice == CodexAccountIndex("team") {
		t.Fatal("workspace and member must both contribute to the new account ID")
	}
	f.build()
	list, err := f.plugin.inventory.Accounts(ProviderCodex)
	if err != nil || len(list) != 3 {
		t.Fatalf("accounts: %+v %v", list, err)
	}
	for id, file := range map[string]string{alice: "a2.json", bob: "b.json", other: "other.json"} {
		account, found, err := f.plugin.inventory.Find(ProviderCodex, id)
		if err != nil || !found || account.Name != file {
			t.Fatalf("seat %s: %+v %v", id, account, err)
		}
	}
	f.plugin.observeCredential = func(_ context.Context, account Account) (credentialObservation, error) {
		if account.Name != "b.json" {
			t.Fatalf("quota used the wrong seat: %s", account.Name)
		}
		return credentialObservation{Quota: signalSnapshot{ObservedAt: f.clock, Signals: codexSignals()}}, nil
	}
	if _, status, reason := f.plugin.observeCodexQuota(url.Values{"id": {bob}}, []byte(rpcBody)); status != 0 {
		t.Fatalf("seat quota: %d %s", status, reason)
	}
	// Simulate replacement after inventory lookup without changing its
	// fingerprint. The second document read must reject the sibling seat.
	f.host.setDocField("idx-b.json", "id_token", seatDocument("team", "user-a", "")["id_token"])
	if _, _, reason := f.plugin.observeCodexQuota(url.Values{"id": {bob}}, []byte(rpcBody)); reason != "quota_account_identity_unavailable" {
		t.Fatalf("accepted a replaced credential: %s", reason)
	}
	// Losing the member claim cannot move a modern credential onto the old workspace ID.
	if _, err := f.plugin.inventory.Register(ProviderCodex, "fixture.json", mustJSON(seatDocument("team", "", ""))); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("accepted unidentified credential in a known seat-aware workspace: %v", err)
	}
}

func TestCodexSeatMigrationUsesOnlyUnambiguousStoredCredentials(t *testing.T) {
	dir := t.TempDir()
	host := &fakeHost{}
	add := func(file, workspace, member string) {
		t.Helper()
		doc := seatDocument(workspace, member, file+"@example.test")
		if err := os.WriteFile(filepath.Join(dir, file+".json"), mustJSON(doc), 0600); err != nil {
			t.Fatal(err)
		}
		host.addEntry(file, ProviderCodex, doc, time.Now())
	}
	add("one", "unique", "alice")
	add("copy", "unique", "alice")
	add("two", "shared", "alice")
	add("three", "shared", "bob")
	add("old", "legacy", "")
	inv, err := NewInventory(host, dir)
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(workspace, member string) string {
		return inv.identify(ProviderCodex, "fixture.json", mustJSON(seatDocument(workspace, member, "new@example.test"))).id
	}
	if lookup("unique", "alice") != CodexAccountIndex("unique") || lookup("legacy", "") != CodexAccountIndex("legacy") ||
		lookup("unique", "bob") == CodexAccountIndex("unique") || lookup("unique", "") != "" {
		t.Fatal("legacy workspace ID was lost or assigned to a different seat")
	}
	if lookup("shared", "alice") != "" || lookup("shared", "bob") != "" {
		t.Fatal("ambiguous old credentials became routable before confirmation")
	}
	raw := mustJSON(seatDocument("shared", "alice", ""))
	id, err := inv.Register(ProviderCodex, "fixture.json", raw)
	if err != nil || id == CodexAccountIndex("shared") || lookup("shared", "alice") != "" {
		t.Fatal("identification prematurely enabled a seat", err)
	}
	if _, err := inv.codexSeats.confirm("fixture.json", raw, id); err != nil {
		t.Fatal(err)
	}
	inv, err = NewInventory(host, dir)
	if err != nil || lookup("shared", "alice") != id || lookup("shared", "bob") != "" {
		t.Fatal("confirmation did not survive restart", err)
	}
	// A legacy file with no member claim keeps its ID alongside a new
	// confirmed seat; loss of that new seat's claim cannot adopt the old ID.
	newRaw := mustJSON(seatDocument("legacy", "new-member", ""))
	newID, err := inv.Register(ProviderCodex, "new.json", newRaw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inv.codexSeats.confirm("new.json", newRaw, newID); err != nil {
		t.Fatal(err)
	}
	oldRaw := mustJSON(seatDocument("legacy", "", ""))
	if inv.identify(ProviderCodex, "old.json", oldRaw).id != CodexAccountIndex("legacy") ||
		inv.identify(ProviderCodex, "new.json", oldRaw).id != "" {
		t.Fatal("memberless credentials crossed seats")
	}
	if err := os.WriteFile(filepath.Join(dir, codexSeatsFile), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewInventory(host, dir); !errors.Is(err, ErrStateUnavailable) {
		t.Fatal("corrupt migration state was accepted")
	}
}
