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

func TestSOCKS5EgressPostgresLifecycle(t *testing.T) {
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
	actor := EgressActor{UserID: usageSummaryUser(t, ctx, repository, "socks5-egress-"+suffix).ID}
	secret := bytes.Repeat([]byte{2}, 40)

	open, err := repository.SaveEgressServer(ctx, EgressServer{ID: uuid.NewString(), Name: "open", Protocol: "socks5", Server: "203.0.113.5", Port: 1080}, true, actor)
	if err != nil || open.Protocol != "socks5" || open.Username != "" || open.PasswordCiphertext != nil || open.Cipher != "" {
		t.Fatalf("unauthenticated SOCKS5 node: %+v %v", open, err)
	}
	if _, err = repository.SaveEgressServer(ctx, EgressServer{ID: uuid.NewString(), Name: "no-pass", Protocol: "socks5", Server: "203.0.113.5", Port: 1080, Username: "user"}, true, actor); !errors.Is(err, ErrInvalid) {
		t.Fatalf("SOCKS5 username without password: %v", err)
	}
	if _, err = repository.SaveEgressServer(ctx, EgressServer{ID: uuid.NewString(), Name: "pass-only", Protocol: "socks5", Server: "203.0.113.5", Port: 1080, PasswordCiphertext: secret}, true, actor); !errors.Is(err, ErrInvalid) {
		t.Fatalf("SOCKS5 password without username: %v", err)
	}
	// Enabling authentication on an unauthenticated node needs a new password.
	enable := open
	enable.Username = "user"
	if _, err = repository.SaveEgressServer(ctx, enable, false, actor); !errors.Is(err, ErrInvalid) {
		t.Fatalf("enabled SOCKS5 auth without a password: %v", err)
	}
	enable.PasswordCiphertext = secret
	authed, err := repository.SaveEgressServer(ctx, enable, false, actor)
	if err != nil || authed.Username != "user" || !bytes.Equal(authed.PasswordCiphertext, secret) {
		t.Fatalf("enable SOCKS5 auth: %+v %v", authed, err)
	}
	// A blank password on the same protocol keeps the stored one.
	authed.PasswordCiphertext, authed.Name = nil, "renamed"
	if authed, err = repository.SaveEgressServer(ctx, authed, false, actor); err != nil || !bytes.Equal(authed.PasswordCiphertext, secret) {
		t.Fatalf("SOCKS5 password was not retained: %v", err)
	}
	// Removing the username clears the password.
	authed.Username, authed.PasswordCiphertext = "", nil
	cleared, err := repository.SaveEgressServer(ctx, authed, false, actor)
	if err != nil || cleared.PasswordCiphertext != nil || cleared.Username != "" {
		t.Fatalf("SOCKS5 auth was not cleared: %+v %v", cleared, err)
	}
	// Switching an unauthenticated SOCKS5 node to SS cannot retain a password.
	toSS := cleared
	toSS.Protocol, toSS.Cipher = "ss", "aes-256-gcm"
	if _, err = repository.SaveEgressServer(ctx, toSS, false, actor); !errors.Is(err, ErrInvalid) {
		t.Fatalf("SS node without a password: %v", err)
	}
	toSS.PasswordCiphertext = secret
	if toSS, err = repository.SaveEgressServer(ctx, toSS, false, actor); err != nil || toSS.Protocol != "ss" {
		t.Fatalf("switch to SS: %v", err)
	}

	snapshot, err := repository.EgressSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range snapshot.Nodes {
		if n.ID == open.ID {
			found = n.Protocol == "ss" && n.Cipher == "aes-256-gcm" && bytes.Equal(n.PasswordCiphertext, secret)
		}
	}
	if !found {
		t.Fatal("snapshot lacks the switched node")
	}
	// The database refuses combinations the Go validation would also refuse.
	for _, values := range [][]any{
		{"socks5", "aes-256-gcm", "", nil},
		{"socks5", "", "user", nil},
		{"socks5", "", "", secret},
		{"ss", "aes-256-gcm", "", nil},
		{"ss", "aes-256-gcm", "user", secret},
		{"http", "", "", nil},
	} {
		_, err := repository.db.ExecContext(ctx, `INSERT INTO egress_servers(id,name,protocol,server,port,cipher,username,password_ciphertext,udp,revision,created_at,updated_at) VALUES($1::uuid,'bad',$2,'203.0.113.5',1080,$3,$4,$5::bytea,false,1,now(),now())`, uuid.NewString(), values[0], values[1], values[2], values[3])
		if err == nil {
			t.Errorf("database accepted %v", values[:3])
		}
	}
	var metadata string
	if err = repository.db.QueryRowContext(ctx, `SELECT metadata::text FROM audit_events WHERE event_type='egress_server.saved' AND subject_id=$1 ORDER BY id DESC LIMIT 1`, open.ID).Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains([]byte(metadata), []byte("user")) || bytes.Contains([]byte(metadata), []byte("password")) {
		t.Fatalf("audit metadata leaked credentials: %s", metadata)
	}
	for _, id := range []string{open.ID} {
		if err = repository.DeleteEgressServer(ctx, id, actor); err != nil {
			t.Fatal(err)
		}
	}
}
