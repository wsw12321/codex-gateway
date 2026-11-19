//go:build integration

package store

import (
	"bytes"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestModelSupplierPostgresLifecycle(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := t.Context()
	repo, err := Open(ctx, Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	user := usageSummaryUser(t, ctx, repo, "supplier-"+uuid.NewString()[:12])
	actor := EgressActor{UserID: user.ID}
	node, err := repo.SaveEgressServer(ctx, EgressServer{ID: uuid.NewString(), Name: "Supplier test", Server: "egress.example.com", Port: 8388, Cipher: "aes-256-gcm", PasswordCiphertext: bytes.Repeat([]byte{1}, 40)}, true, actor)
	if err != nil {
		t.Fatal(err)
	}
	pool := repo.WithUpstreamProvider(UpstreamProviderAnthropic)
	value := ModelSupplier{ID: upstreamIntegrationAccountID(user.ID), BaseURL: "https://supplier.example.com/v1", Ciphertext: []byte("encrypted-test-value"), Models: []string{"claude-test"}}
	if err := pool.CreateModelSupplier(ctx, value, node.ID, actor); err != nil {
		t.Fatal(err)
	}
	if err := pool.SyncUpstreamAccounts(ctx, nil, time.Now().UTC().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	values, err := pool.ModelSuppliers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var persisted ModelSupplier
	for _, item := range values {
		if item.ID == value.ID {
			persisted = item
		}
	}
	if !persisted.Enabled || persisted.Revision != 1 || !bytes.Equal(value.Ciphertext, persisted.Ciphertext) {
		t.Fatalf("supplier lost after OAuth inventory sync: %+v", persisted)
	}
	eligible, err := pool.EligibleUpstreamAccountLimits(ctx, user.ID, []string{value.ID})
	if err != nil || len(eligible) != 1 || eligible[0].ID != value.ID {
		t.Fatalf("supplier eligibility=%+v %v", eligible, err)
	}
	persisted.Ciphertext = []byte("replacement-encrypted-value")
	if err := pool.ReplaceModelSupplierKey(ctx, persisted); err != nil {
		t.Fatal(err)
	}
	if err := pool.ReplaceModelSupplierKey(ctx, persisted); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale key rotation=%v", err)
	}
	if err := pool.DeleteModelSupplier(ctx, value.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("enabled credential deletion=%v", err)
	}
	if err := pool.SetModelSupplierEnabled(ctx, value.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := pool.DeleteModelSupplier(ctx, value.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteEgressServer(ctx, node.ID, actor); err != nil {
		t.Fatalf("supplier deletion did not release its node: %v", err)
	}
	rows, err := pool.SummarizeUpstreamAccounts(ctx, UpstreamAccountSummaryFilter{All: true, LiveFrom: time.Now().UTC().Add(-time.Hour), Until: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.AccountID != nil && *row.AccountID == value.ID && row.CredentialKind == "supplier" && row.Status == UpstreamAccountStatusUnavailable {
			return
		}
	}
	t.Fatal("deleted supplier no longer appears in historical summaries")
}
