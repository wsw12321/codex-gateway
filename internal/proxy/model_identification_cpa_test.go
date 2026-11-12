package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestModelIdentificationProbeThroughCPA(t *testing.T) {
	const (
		account = "0123456789abcdef"
		other   = "fedcba9876543210"
	)
	completed := func(text string) string {
		return `{"id":"resp_1","status":"completed","output":[{"type":"reasoning","summary":[]},` +
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":` + jsonQuote(text) + `}]}],"usage":{"input_tokens":5,"output_tokens":7}}`
	}
	for name, test := range map[string]struct {
		report   AllocationReport
		status   int
		body     string
		media    string
		want     string
		wantCode string
	}{
		"answer":            {report: AllocationReport{AccountID: account}, status: 200, body: completed("the answer")},
		"another account":   {report: AllocationReport{AccountID: other}, status: 200, body: completed("x"), wantCode: "probe_account_mismatch"},
		"concurrency":       {report: AllocationReport{Rejected: allocationRejectedConcurrency}, status: 429, body: `{}`, wantCode: "probe_rate_limited"},
		"allocation down":   {report: AllocationReport{Rejected: allocationRejectedUnavailable}, status: 500, body: `{}`, wantCode: "probe_unavailable"},
		"reauthentication":  {report: AllocationReport{AccountID: account}, status: 401, body: `{}`, wantCode: "probe_authentication_failed"},
		"upstream 429":      {report: AllocationReport{AccountID: account}, status: 429, body: `{}`, wantCode: "probe_rate_limited"},
		"upstream 502":      {report: AllocationReport{AccountID: account}, status: 502, body: `{}`, wantCode: "probe_upstream_unavailable"},
		"rejected request":  {report: AllocationReport{AccountID: account}, status: 400, body: `{}`, wantCode: "probe_upstream_rejected"},
		"tool call":         {report: AllocationReport{AccountID: account}, status: 200, body: `{"status":"completed","output":[{"type":"function_call","name":"x"}]}`, wantCode: "probe_unexpected_tool"},
		"incomplete":        {report: AllocationReport{AccountID: account}, status: 200, body: `{"status":"incomplete","output":[]}`, wantCode: "probe_response_incomplete"},
		"empty":             {report: AllocationReport{AccountID: account}, status: 200, body: completed("  "), wantCode: "probe_empty_output"},
		"too many tokens":   {report: AllocationReport{AccountID: account}, status: 200, body: `{"status":"completed","output":[],"usage":{"output_tokens":20000}}`, wantCode: "probe_output_tokens_exceeded"},
		"too long":          {report: AllocationReport{AccountID: account}, status: 200, body: completed(strings.Repeat("x", 17<<10)), wantCode: "probe_output_too_large"},
		"not json":          {report: AllocationReport{AccountID: account}, status: 200, body: `<html>`, media: "text/html", wantCode: "probe_invalid_response"},
		"duplicate key":     {report: AllocationReport{AccountID: account}, status: 200, body: `{"status":"completed","status":"completed"}`, wantCode: "probe_invalid_response"},
		"unexpected status": {report: AllocationReport{AccountID: account}, status: 200, body: `{"status":"in_progress","output":[]}`, wantCode: "probe_invalid_response"},
	} {
		t.Run(name, func(t *testing.T) {
			ledger := NewAllocationLedger()
			var client *Client
			var identity string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				identity = r.Header.Get(gatewayUserHeader)
				if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer data-plane" ||
					!affinityScopePattern.MatchString(r.Header.Get(affinityHeader)) || r.Header.Get(diagnosticRunHeader) != "" {
					t.Errorf("not an ordinary pinned request: %s %v", r.URL.Path, r.Header)
				}
				if pinned, ok := ledger.ProbeAccount(identity); !ok || pinned != account || identity == "6b4ec2a8-3d1f-4a9e-9c6f-2f1e0d9c8b7a" {
					t.Errorf("probe identity %q pinned to %q", identity, pinned)
				}
				raw, _ := io.ReadAll(r.Body)
				if string(raw) != `{"model":"gpt-6","input":"which model are you?","max_output_tokens":16384,"stream":false}` {
					t.Errorf("probe body %s", raw)
				}
				// The plugin reports its allocation before CPA answers.
				report := test.report
				report.Token, report.UserID = r.Header.Get(dispatchHeader), identity
				if err := ledger.Record(client.AccountProvider(), report); err != nil {
					t.Errorf("report: %v", err)
				}
				media := test.media
				if media == "" {
					media = "application/json"
				}
				w.Header().Set("Content-Type", media)
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			base, _ := url.Parse(server.URL)
			client = NewWithHTTPClient(base, "data-plane", server.Client())
			client.UseAllocationLedger(ledger)
			client.UsePluginManagement("management")
			text, err := client.ProbeModelIdentification(context.Background(), ModelIdentificationProbe{
				RunID: "11111111-1111-4111-8111-111111111111", UserID: "6b4ec2a8-3d1f-4a9e-9c6f-2f1e0d9c8b7a",
				AccountID: account, Model: "gpt-6", Prompt: "which model are you?", ProbeIndex: 1,
			})
			var failure *ModelIdentificationError
			switch {
			case test.wantCode == "" && (err != nil || text != "the answer"):
				t.Fatalf("got %q, %v", text, err)
			case test.wantCode != "" && (!errors.As(err, &failure) || failure.SafeCode() != test.wantCode):
				t.Fatalf("got %q, %v; want %s", text, err, test.wantCode)
			}
			if _, still := ledger.ProbeAccount(identity); still {
				t.Fatal("the probe identity outlived the probe")
			}
		})
	}
}

func TestModelIdentificationCapabilityFollowsTheIntegration(t *testing.T) {
	var protocol string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/management/gateway/model-identification/capabilities" || r.Header.Get("Authorization") != "Bearer management" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"protocol": protocol})
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	client := NewWithHTTPClient(base, "data-plane", server.Client())
	client.UseAllocationLedger(NewAllocationLedger())
	client.UsePluginManagement("management")
	protocol = ModelIdentificationCPAProtocol
	if err := client.RequireModelIdentificationCapability(context.Background()); err != nil {
		t.Fatalf("the plugin's protocol was refused: %v", err)
	}
	protocol = ModelIdentificationProtocol
	if err := client.RequireModelIdentificationCapability(context.Background()); err == nil {
		t.Fatal("the source-patched protocol was accepted in plugin mode")
	}
}

func jsonQuote(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
