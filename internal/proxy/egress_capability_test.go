package proxy

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestAccountEgressCapabilityIsRecheckedAfterSidecarReplacement(t *testing.T) {
	base, _ := url.Parse("http://sidecar.invalid")
	marker := `,"account_egress":"account_egress_v1"`
	probes := 0
	client := NewWithHTTPClient(base, "internal", &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/internal/upstream-accounts/capabilities" || request.Header.Get("Authorization") != "Bearer internal" {
			t.Fatal("unsafe capability request")
		}
		probes++
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"protocol":"upstream_account_access_v1"` + marker + `}`))}, nil
	})})
	client.RequireAccountEgress(true)
	if err := client.requireAccountAccessCapability(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	marker = ""
	if err := client.requireAccountAccessCapability(context.Background(), false); err == nil {
		t.Fatal("old sidecar accepted account egress")
	}
	if _, err := client.AccountCapabilities(context.Background()); err == nil {
		t.Fatal("management accepted old sidecar")
	}
	if probes != 3 {
		t.Fatalf("capability cached across requests: %d probes", probes)
	}
	client.RequireAccountEgress(false)
	if _, err := client.AccountCapabilities(context.Background()); err != nil {
		t.Fatal("legacy installation without account exits rejected")
	}
}
