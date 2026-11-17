package server

import (
	"context"
	"encoding/csv"
	"encoding/json"
	gatewayproxy "github.com/wsw/codex-gateway/internal/proxy"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wsw/codex-gateway/internal/store"
)

func TestUsageDTOExplicitOwnerAttributionAndPersonalIsolation(t *testing.T) {
	id := "0123456789abcdef"
	for _, state := range []string{"completed", "degraded", "failed", "cancelled"} {
		rows := []store.UsageRequest{{RequestID: "request-1", State: state, UpstreamAccountID: &id}, {RequestID: "historical", State: "completed"}}
		accounts := map[string]usageAccountIdentity{id: {Alias: "主力 Plus", DisplayName: "Team", MaskedEmail: "a***@example.com"}}
		owner, err := json.Marshal(usageResponseDTO(rows, true, accounts))
		if err != nil {
			t.Fatal(err)
		}
		var decoded []map[string]any
		if err = json.Unmarshal(owner, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded[0]["upstream_account_id"] != id || decoded[0]["upstream_alias"] != "主力 Plus" || decoded[0]["upstream_display_name"] != "Team" ||
			decoded[0]["upstream_masked_email"] != "a***@example.com" || decoded[0]["State"] != state {
			t.Fatalf("owner DTO: %s", owner)
		}
		for _, field := range []string{"upstream_account_id", "upstream_alias", "upstream_display_name", "upstream_masked_email"} {
			if value, ok := decoded[1][field]; !ok || value != nil {
				t.Fatalf("unattributed %s: %s", field, owner)
			}
		}
		personal, err := json.Marshal(usageResponseDTO(rows, false, accounts))
		if err != nil {
			t.Fatal(err)
		}
		for _, leaked := range []string{"upstream_", id, "example.com", "主力 Plus", "Team"} {
			if strings.Contains(string(personal), leaked) {
				t.Fatalf("personal leak %q: %s", leaked, personal)
			}
		}
	}
}

func TestUsageDTOUnknownMetadataPreservesOnlyStableID(t *testing.T) {
	id := "0123456789abcdef"
	rows := []store.UsageRequest{{UpstreamAccountID: &id}}
	result := usageResponseDTO(rows, true, nil).([]ownerUsageRequestDTO)
	if result[0].UpstreamAccountID == nil || *result[0].UpstreamAccountID != id || result[0].UpstreamMaskedEmail != nil ||
		result[0].UpstreamAlias != nil || result[0].UpstreamDisplayName != nil {
		t.Fatalf("unavailable metadata: %+v", result)
	}
	// An alias without synchronized CPA metadata still names the account.
	result = usageResponseDTO(rows, true, map[string]usageAccountIdentity{id: {Alias: "备用"}}).([]ownerUsageRequestDTO)
	if result[0].UpstreamAlias == nil || *result[0].UpstreamAlias != "备用" || result[0].UpstreamDisplayName != nil || result[0].UpstreamMaskedEmail != nil {
		t.Fatalf("alias only: %+v", result)
	}
}

func TestUsageCSVIncludesAttributionOnlyForOwner(t *testing.T) {
	id := "0123456789abcdef"
	rows := []store.UsageRequest{{RequestID: "failure", State: "failed", UpstreamAccountID: &id}, {RequestID: "historic"}}
	emails := map[string]string{id: "a***@example.com"}
	accounts := map[string]usageAccountIdentity{id: {Alias: "主力 Plus", DisplayName: "Team", MaskedEmail: emails[id]}}
	for _, owner := range []bool{false, true} {
		response := httptest.NewRecorder()
		writeUsageCSV(response, rows, owner, accounts)
		records, err := csv.NewReader(response.Body).ReadAll()
		if err != nil {
			t.Fatal(err)
		}
		if len(records) != 3 {
			t.Fatalf("rows=%d", len(records))
		}
		if owner {
			index := len(records[0]) - 2
			if records[0][index] != "upstream_account_id" || records[0][index+1] != "upstream_masked_email" || records[1][index] != id || records[1][index+1] != emails[id] || records[2][index] != "" || records[2][index+1] != "" {
				t.Fatalf("owner CSV: %v", records)
			}
		} else if strings.Contains(response.Body.String(), "upstream_account_id") || strings.Contains(response.Body.String(), "upstream_masked_email") || strings.Contains(response.Body.String(), id) || strings.Contains(response.Body.String(), emails[id]) {
			t.Fatalf("personal CSV leaked attribution: %s", response.Body.String())
		}
	}
}

func TestUsageMetadataInitialReadFillsOnlyKnownAttribution(t *testing.T) {
	id := "0123456789abcdef"
	calls := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != pluginPath("upstream-accounts") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"accounts": []gatewayproxy.UpstreamAccount{{ID: id, DisplayName: "team-a", MaskedEmail: "a***@example.com", Plan: "pro", Status: "active", LastSyncedAt: time.Now().UTC()}}})
	}))
	defer remote.Close()
	base, _ := url.Parse(remote.URL)
	server := &Server{upstream: withManagement(gatewayproxy.NewWithHTTPClient(base, "secret", remote.Client()))}
	// A stored alias without synchronized metadata is kept; the sidecar only
	// fills the CPA name and masked email.
	accounts := map[string]usageAccountIdentity{id: {Alias: "主力 Plus"}}
	rows := []store.UsageRequest{{UpstreamAccountID: &id}, {RequestID: "unattributed"}}
	server.completeUsageAccountMetadata(context.Background(), rows, accounts)
	if calls != 1 || accounts[id] != (usageAccountIdentity{Alias: "主力 Plus", DisplayName: "team-a", MaskedEmail: "a***@example.com"}) || rows[1].UpstreamAccountID != nil {
		t.Fatalf("calls=%d accounts=%v rows=%v", calls, accounts, rows)
	}
	server.completeUsageAccountMetadata(context.Background(), rows, accounts)
	if calls != 1 {
		t.Fatal("available durable metadata unnecessarily queried")
	}
	server.completeUsageAccountMetadata(context.Background(), []store.UsageRequest{{RequestID: "unattributed"}}, map[string]usageAccountIdentity{})
	if calls != 1 {
		t.Fatal("unattributed request triggered account lookup")
	}
}
