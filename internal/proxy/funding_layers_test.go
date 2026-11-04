package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestFundingLayersNegotiateWithSidecar(t *testing.T) {
	const userID = "00000000-0000-0000-0000-000000000001"
	first := AllocationGroup{GroupID: "00000000-0000-0000-0000-000000000002", AccountIDs: []string{"0123456789abcdef"}}
	second := AllocationGroup{GroupID: "00000000-0000-0000-0000-000000000003", AccountIDs: []string{"fedcba9876543210"}}
	personal := AllocationGroup{GroupID: PersonalLayerID, AccountIDs: []string{}, AllAccounts: true}
	for _, test := range []struct {
		name    string
		layers  []AllocationGroup
		status  int // funding-layers route status; 0 means the route is not probed
		header  string
		blocked bool
	}{
		{name: "personal only sends no scope", layers: []AllocationGroup{personal}},
		{name: "single group keeps the v1 contract", layers: []AllocationGroup{first}, header: `[{"group_id":"00000000-0000-0000-0000-000000000002","account_ids":["0123456789abcdef"]}]`},
		{name: "layered sidecar receives every layer", layers: []AllocationGroup{first, second, personal}, status: http.StatusOK,
			header: `[{"group_id":"00000000-0000-0000-0000-000000000002","account_ids":["0123456789abcdef"]},{"group_id":"00000000-0000-0000-0000-000000000003","account_ids":["fedcba9876543210"]},{"group_id":"00000000-0000-0000-0000-000000000000","all_accounts":true}]`},
		{name: "old sidecar receives the first group", layers: []AllocationGroup{first, personal}, status: http.StatusNotFound,
			header: `[{"group_id":"00000000-0000-0000-0000-000000000002","account_ids":["0123456789abcdef"]}]`},
		{name: "unavailable capability fails closed", layers: []AllocationGroup{first, personal}, status: http.StatusServiceUnavailable, blocked: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			base, _ := url.Parse("http://sidecar.invalid")
			client := New(base, "internal-test-key")
			layerProbes, dispatches := 0, 0
			client.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/funding-layers"):
					layerProbes++
					if test.status == http.StatusOK {
						return routerTestResponse(http.StatusOK, `{"protocol":"`+GroupUpstreamAccountLayersV1+`"}`), nil
					}
					return routerTestResponse(test.status, `{"error":"not_found"}`), nil
				case strings.HasSuffix(r.URL.Path, "/capabilities"):
					body, _ := json.Marshal(map[string]string{"protocol": AccountAccessProtocolV1, "group_upstream_account_scope": GroupUpstreamAccountScopeV1})
					return routerTestResponse(http.StatusOK, string(body)), nil
				}
				dispatches++
				if got := r.Header.Get(gatewayGroupsHeader); got != test.header {
					t.Errorf("groups header = %s\nwant %s", got, test.header)
				}
				return routerTestResponse(http.StatusTooManyRequests, `{"error":{"code":"usage_limit_reached"}}`), nil
			})
			incoming := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test"}`))
			_, failure := client.ForwardWithOptions(context.Background(), httptest.NewRecorder(), incoming, "/v1/responses", ForwardOptions{UserID: userID, Groups: test.layers})
			if wantProbe := test.status != 0; (layerProbes == 1) != wantProbe {
				t.Fatalf("funding-layer probes = %d", layerProbes)
			}
			if test.blocked {
				if dispatches != 0 || failure == nil || failure.Code != "upstream_access_protocol_unavailable" {
					t.Fatalf("dispatches=%d failure=%+v", dispatches, failure)
				}
				return
			}
			if dispatches != 1 {
				t.Fatalf("dispatches = %d", dispatches)
			}
		})
	}
}

func TestFundingLayersRejectMalformedPersonalLayer(t *testing.T) {
	personal := AllocationGroup{GroupID: PersonalLayerID, AllAccounts: true}
	group := AllocationGroup{GroupID: "00000000-0000-0000-0000-000000000002", AccountIDs: []string{"0123456789abcdef"}}
	for _, layers := range [][]AllocationGroup{
		{personal, group},
		{{GroupID: PersonalLayerID, AccountIDs: []string{"0123456789abcdef"}, AllAccounts: true}},
		{{GroupID: PersonalLayerID}},
		{{GroupID: group.GroupID, AllAccounts: true}},
	} {
		if err := (TrustedRequestMetadata{UserID: "00000000-0000-0000-0000-000000000001", Groups: layers}).Validate(); err == nil {
			t.Fatalf("malformed layers accepted: %+v", layers)
		}
	}
	if err := (TrustedRequestMetadata{UserID: "00000000-0000-0000-0000-000000000001", Groups: []AllocationGroup{group, personal}}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSidecarAllocationFailuresKeepTheirMeaning(t *testing.T) {
	for _, test := range []struct {
		status       int
		body         string
		wantStatus   int
		wantCode     string
		wantRetryMin int
	}{
		{http.StatusTooManyRequests, `{"error":{"message":"upstream_concurrency_exceeded: upstream account concurrency limit reached","type":"rate_limit_error","code":"upstream_concurrency_exceeded"}}`, http.StatusTooManyRequests, "upstream_concurrency_exceeded", 5},
		{http.StatusTooManyRequests, `{"error":{"message":"limit","type":"rate_limit_error","code":"rate_limit_exceeded"}}`, http.StatusTooManyRequests, "upstream_rate_limited", 60},
		{http.StatusTooManyRequests, `not json`, http.StatusTooManyRequests, "upstream_rate_limited", 60},
		{http.StatusServiceUnavailable, `{"error":{"message":"gateway_allocation_unavailable: upstream account allocation unavailable","type":"server_error","code":"gateway_allocation_unavailable"}}`, http.StatusServiceUnavailable, "upstream_allocation_unavailable", 0},
	} {
		failure := sanitizeUpstreamFailure(&http.Response{StatusCode: test.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(test.body))})
		if failure.Status != test.wantStatus || failure.Code != test.wantCode || failure.RetryAfter < test.wantRetryMin {
			t.Fatalf("%s => %+v", test.body, failure)
		}
		if strings.Contains(failure.Message, "upstream_concurrency_exceeded") || strings.Contains(failure.Message, "gateway_allocation") {
			t.Fatalf("raw sidecar text leaked into the client message: %q", failure.Message)
		}
	}
}

func TestAllocationGroupWireFormat(t *testing.T) {
	encoded, err := json.Marshal([]AllocationGroup{
		{GroupID: "00000000-0000-0000-0000-000000000002", AccountIDs: []string{"0123456789abcdef"}},
		{GroupID: PersonalLayerID, AllAccounts: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{
		{"group_id": "00000000-0000-0000-0000-000000000002", "account_ids": []any{"0123456789abcdef"}},
		{"group_id": PersonalLayerID, "all_accounts": true},
	}
	if !reflect.DeepEqual(decoded, want) {
		t.Fatalf("wire = %s", encoded)
	}
}
