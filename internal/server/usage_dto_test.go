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
		emails := map[string]string{id: "a***@example.com"}
		owner, err := json.Marshal(usageResponseDTO(rows, true, emails))
		if err != nil {
			t.Fatal(err)
		}
		var decoded []map[string]any
		if err = json.Unmarshal(owner, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded[0]["upstream_account_id"] != id || decoded[0]["upstream_masked_email"] != emails[id] || decoded[0]["State"] != state || decoded[1]["upstream_account_id"] != nil || decoded[1]["upstream_masked_email"] != nil {
			t.Fatalf("owner DTO: %s", owner)
		}
		personal, err := json.Marshal(usageResponseDTO(rows, false, emails))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(personal), "upstream_account") || strings.Contains(string(personal), "upstream_masked") || strings.Contains(string(personal), id) || strings.Contains(string(personal), "example.com") {
			t.Fatalf("personal leak: %s", personal)
		}
	}
}

func TestUsageDTOUnknownMetadataPreservesOnlyStableID(t *testing.T) {
	id := "0123456789abcdef"
	rows := []store.UsageRequest{{UpstreamAccountID: &id}}
	result := usageResponseDTO(rows, true, nil).([]ownerUsageRequestDTO)
	if result[0].UpstreamAccountID == nil || *result[0].UpstreamAccountID != id || result[0].UpstreamMaskedEmail != nil {
		t.Fatalf("unavailable metadata: %+v", result)
	}
}

func TestUsageCSVIncludesAttributionOnlyForOwner(t *testing.T) {
	id := "0123456789abcdef"
	rows := []store.UsageRequest{{RequestID: "failure", State: "failed", UpstreamAccountID: &id}, {RequestID: "historic"}}
	emails := map[string]string{id: "a***@example.com"}
	for _, owner := range []bool{false, true} {
		response := httptest.NewRecorder()
		writeUsageCSV(response, rows, owner, emails)
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
		if r.URL.Path != "/internal/upstream-accounts" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"accounts": []gatewayproxy.UpstreamAccount{{ID: id, MaskedEmail: "a***@example.com", Plan: "pro", Status: "active", LastSyncedAt: time.Now().UTC()}}})
	}))
	defer remote.Close()
	base, _ := url.Parse(remote.URL)
	server := &Server{upstream: gatewayproxy.NewWithHTTPClient(base, "secret", remote.Client())}
	emails := make(map[string]string)
	rows := []store.UsageRequest{{UpstreamAccountID: &id}, {RequestID: "unattributed"}}
	server.completeUsageAccountEmails(context.Background(), rows, emails)
	if calls != 1 || emails[id] != "a***@example.com" || rows[1].UpstreamAccountID != nil {
		t.Fatalf("calls=%d emails=%v rows=%v", calls, emails, rows)
	}
	server.completeUsageAccountEmails(context.Background(), rows, emails)
	if calls != 1 {
		t.Fatal("available durable metadata unnecessarily queried")
	}
	server.completeUsageAccountEmails(context.Background(), []store.UsageRequest{{RequestID: "unattributed"}}, map[string]string{})
	if calls != 1 {
		t.Fatal("unattributed request triggered account lookup")
	}
}
