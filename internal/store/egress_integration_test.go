//go:build integration

package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestEgressPostgresLifecycle(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	repository, err := Open(ctx, Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if err = repository.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	user := usageSummaryUser(t, ctx, repository, "egress-"+suffix)
	actor := EgressActor{UserID: user.ID}
	makeNode := func(name string) EgressServer {
		n, err := repository.SaveEgressServer(ctx, EgressServer{ID: uuid.NewString(), Name: name, Server: "ss.example.com", Port: 8388, Cipher: "aes-256-gcm", PasswordCiphertext: bytes.Repeat([]byte{1}, 40)}, true, actor)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	first, second := makeNode("first"), makeNode("second")
	account := upstreamIntegrationAccountID("egress-codex-" + suffix)
	other := upstreamIntegrationAccountID("egress-other-" + suffix)
	agy := upstreamIntegrationAccountID("egress-agy-" + suffix)
	for _, id := range []string{account, other} {
		if err = repository.EnsureUpstreamAccount(ctx, id, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if err = repository.WithUpstreamProvider("antigravity").EnsureUpstreamAccount(ctx, agy, time.Now()); err != nil {
		t.Fatal(err)
	}
	bind := func(id, nodeID string) EgressBinding {
		b, err := repository.SetEgressBinding(ctx, id, nodeID, actor)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	bind(account, first.ID)
	bind(other, first.ID)
	findBinding := func(id string) EgressBinding {
		snapshot, err := repository.EgressSnapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range snapshot.Bindings {
			if b.AccountID == id {
				return b
			}
		}
		return EgressBinding{}
	}
	if err = repository.SyncUpstreamAccounts(ctx, []UpstreamAccountSnapshot{{ID: account, MaskedEmail: "a***@example.com", Plan: "plus", Status: "available"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if findBinding(account).NodeID != first.ID {
		t.Fatal("metadata sync removed binding")
	}
	retained := append([]byte(nil), first.PasswordCiphertext...)
	first.PasswordCiphertext = nil
	first.Name = "edited"
	first, err = repository.SaveEgressServer(ctx, first, false, actor)
	if err != nil || !bytes.Equal(first.PasswordCiphertext, retained) {
		t.Fatal("password was not retained")
	}
	if err = repository.DeleteEgressServer(ctx, first.ID, actor); !errors.Is(err, ErrConflict) {
		t.Fatalf("bound delete: %v", err)
	}
	if _, err = repository.WithUpstreamProvider("antigravity").SetEgressBinding(ctx, account, second.ID, actor); !errors.Is(err, ErrConflict) {
		t.Fatalf("provider isolation: %v", err)
	}
	selected := second.ID
	registration, err := repository.CreateEgressRegistration(ctx, "codex", &selected, actor)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := repository.PrepareEgressRegistration(ctx, registration.ID, "codex", account)
	if err != nil || prepared.State != "prepared" || findBinding(account).NodeID != first.ID {
		t.Fatalf("prepare changed live binding: %v", err)
	}
	if _, err = repository.CommitEgressRegistration(ctx, registration.ID, "codex", account, second.Revision+1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale node preflight accepted: %v", err)
	}
	committed, err := repository.CommitEgressRegistration(ctx, registration.ID, "codex", account, second.Revision)
	if err != nil || committed.State != "committed" || findBinding(account).NodeID != second.ID {
		t.Fatalf("commit failed: %v", err)
	}
	bind(account, first.ID)
	before, _ := repository.EgressSnapshot(ctx)
	if _, err = repository.CommitEgressRegistration(ctx, registration.ID, "codex", account); err != nil {
		t.Fatal(err)
	}
	after, _ := repository.EgressSnapshot(ctx)
	if findBinding(account).NodeID != first.ID || before.Version != after.Version {
		t.Fatal("replay reapplied old binding or mutated config")
	}
	if _, err = repository.CommitEgressRegistration(ctx, registration.ID, "codex", other); !errors.Is(err, ErrConflict) {
		t.Fatal("registration accepted another identity")
	}
	preserve, err := repository.CreateEgressRegistration(ctx, "codex", nil, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repository.PrepareEgressRegistration(ctx, preserve.ID, "codex", account); err != nil {
		t.Fatal(err)
	}
	if _, err = repository.CommitEgressRegistration(ctx, preserve.ID, "codex", account); err != nil {
		t.Fatal(err)
	}
	if findBinding(account).NodeID != first.ID {
		t.Fatal("reauth erased binding")
	}
	stale, err := repository.CreateEgressRegistration(ctx, "codex", &selected, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repository.PrepareEgressRegistration(ctx, stale.ID, "codex", account); err != nil {
		t.Fatal(err)
	}
	bind(account, second.ID)
	if _, err = repository.CommitEgressRegistration(ctx, stale.ID, "codex", account); !errors.Is(err, ErrConflict) {
		t.Fatalf("prepared recovery overwrote later edit: %v", err)
	}
	delayed, err := repository.CreateEgressRegistration(ctx, "codex", &selected, actor)
	if err != nil {
		t.Fatal(err)
	}
	bind(account, first.ID)
	if _, err = repository.PrepareEgressRegistration(ctx, delayed.ID, "codex", account); !errors.Is(err, ErrConflict) {
		t.Fatalf("delayed recovery overwrote later edit: %v", err)
	}
	abandoned := makeNode("abandoned")
	selected = abandoned.ID
	intent, err := repository.CreateEgressRegistration(ctx, "codex", &selected, actor)
	if err != nil {
		t.Fatal(err)
	}
	if err = repository.DeleteEgressServer(ctx, abandoned.ID, actor); !errors.Is(err, ErrConflict) {
		t.Fatal("recent registration did not protect node")
	}
	if _, err = repository.DB().ExecContext(ctx, `UPDATE egress_registrations SET created_at=now()-interval '25 hours' WHERE id=$1::uuid`, intent.ID); err != nil {
		t.Fatal(err)
	}
	if err = repository.DeleteEgressServer(ctx, abandoned.ID, actor); err != nil {
		t.Fatalf("abandoned node could not delete: %v", err)
	}
	if _, err = repository.PrepareEgressRegistration(ctx, intent.ID, "codex", agy); !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong provider preparation: %v", err)
	}
	bind(account, "")
	bind(other, "")
	if err = repository.DeleteEgressServer(ctx, first.ID, actor); err != nil {
		t.Fatalf("unbound committed reference blocked delete: %v", err)
	}
	var leaked bool
	if err = repository.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM audit_events WHERE subject_id=$1 AND (metadata::text LIKE '%password%' OR metadata::text LIKE '%ciphertext%'))`, account).Scan(&leaked); err != nil || leaked {
		t.Fatal("audit contains credential fields")
	}
}
