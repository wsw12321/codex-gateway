//go:build integration

package store

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestEgressAllowedDomainsPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s, err := Open(ctx, Config{DSN: dsn, MaxOpenConns: 4, MaxIdleConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	owner := usageSummaryUser(t, ctx, s, "allow-owner-"+suffix)
	alice := usageSummaryUser(t, ctx, s, "allow-alice-"+suffix)
	ownerActor := EgressActor{UserID: owner.ID, SourceIP: "192.0.2.10"}
	aliceActor := EgressActor{UserID: alice.ID}
	aliceStore := s.WithUpstreamOwner(alice.ID)

	settings, err := s.MarketplaceSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	previousDomains, err := s.EgressAllowedDomains(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var created []string
	t.Cleanup(func() {
		background := context.Background()
		for _, id := range created {
			if err := aliceStore.DeleteEgressServer(background, id, aliceActor); err != nil && !errors.Is(err, ErrNotFound) {
				t.Errorf("delete node %s: %v", id, err)
			}
		}
		// The allowlist is global: remove only this test's entries.
		if current, err := s.EgressAllowedDomains(background); err != nil {
			t.Errorf("read allowlist for restore: %v", err)
		} else {
			kept := []string{}
			for _, domain := range current {
				if !strings.Contains(domain, suffix) {
					kept = append(kept, domain)
				}
			}
			if _, err := s.ReplaceEgressAllowedDomains(background, kept, ownerActor); err != nil {
				t.Errorf("restore allowlist: %v", err)
			}
		}
		current, err := s.MarketplaceSettings(background)
		if err != nil {
			t.Errorf("read settings for restore: %v", err)
			return
		}
		if _, err := s.SetMarketplaceSettings(background, SetMarketplaceSettingsParams{
			Enabled: settings.Enabled, CommissionRate: settings.CommissionRate,
			MaxAccountsPerUser: settings.MaxAccountsPerUser, MaxPlansPerUser: settings.MaxPlansPerUser,
			AllowMemberSuppliers: settings.AllowMemberSuppliers, MaxEgressServersPerUser: settings.MaxEgressServersPerUser,
			ExpectedRevision: current.Revision, ActorUserID: owner.ID,
		}); err != nil {
			t.Errorf("restore settings: %v", err)
		}
	})
	if _, err := s.SetMarketplaceSettings(ctx, SetMarketplaceSettingsParams{
		Enabled: true, CommissionRate: "0", MaxAccountsPerUser: 2, MaxPlansPerUser: 1,
		MaxEgressServersPerUser: 4, ExpectedRevision: settings.Revision, ActorUserID: owner.ID,
	}); err != nil {
		t.Fatal(err)
	}

	wildcard := "*.nodes-" + suffix + ".example.com"
	exact := "exact-" + suffix + ".example.net"
	domains, err := s.ReplaceEgressAllowedDomains(ctx, append([]string{" EXACT-" + suffix + ".example.net. ", wildcard, exact}, previousDomains...), ownerActor)
	if err != nil {
		t.Fatal(err)
	}
	if listed, err := s.EgressAllowedDomains(ctx); err != nil || !reflect.DeepEqual(listed, domains) {
		t.Fatalf("listed=%v saved=%v err=%v", listed, domains, err)
	}
	for _, invalid := range [][]string{{"localhost"}, {"https://example.com"}, {"example.com:443"}, {"*.com"}} {
		if _, err := s.ReplaceEgressAllowedDomains(ctx, invalid, ownerActor); !errors.Is(err, ErrInvalid) {
			t.Fatalf("allowlist %v err=%v", invalid, err)
		}
	}

	password := make([]byte, 40)
	if _, err := rand.Read(password); err != nil {
		t.Fatal(err)
	}
	node := func(server string, port int) EgressServer {
		return EgressServer{ID: uuid.NewString(), Name: "node " + server, Protocol: "ss", Server: server, Port: port, Cipher: "aes-256-gcm", PasswordCiphertext: password}
	}
	save := func(server string, port int) (EgressServer, error) {
		saved, err := aliceStore.SaveEgressServer(ctx, node(server, port), true, aliceActor)
		if err == nil {
			created = append(created, saved.ID)
		}
		return saved, err
	}
	sub := "jp.nodes-" + suffix + ".example.com"
	if _, err := save(sub, 8388); err != nil {
		t.Fatalf("wildcard host: %v", err)
	}
	if _, err := save(exact, 65000); err != nil {
		t.Fatalf("exact host on another port: %v", err)
	}
	for _, refused := range []string{"nodes-" + suffix + ".example.com", "other-" + suffix + ".example.org"} {
		if _, err := save(refused, 8388); !errors.Is(err, ErrEgressDomainNotAllowed) || !errors.Is(err, ErrInvalid) {
			t.Fatalf("host %s err=%v", refused, err)
		}
	}

	// Removing a name still in use is refused and leaves the list unchanged.
	_, err = s.ReplaceEgressAllowedDomains(ctx, append([]string{exact}, previousDomains...), ownerActor)
	var inUse *EgressDomainInUseError
	if !errors.As(err, &inUse) || !errors.Is(err, ErrConflict) || !reflect.DeepEqual(inUse.Hosts, []string{sub}) {
		t.Fatalf("stranding replace err=%v", err)
	}
	if listed, err := s.EgressAllowedDomains(ctx); err != nil || !reflect.DeepEqual(listed, domains) {
		t.Fatalf("allowlist changed after refusal: %v %v", listed, err)
	}
}
