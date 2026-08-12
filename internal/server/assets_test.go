package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDashboardAssetsAreNotCachedAcrossDeployments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		handler func(*Server, *httptest.ResponseRecorder)
	}{
		{
			name: "page",
			handler: func(server *Server, recorder *httptest.ResponseRecorder) {
				server.page(recorder, httptest.NewRequest("GET", "/", nil))
			},
		},
		{
			name: "javascript",
			handler: func(server *Server, recorder *httptest.ResponseRecorder) {
				server.javascript(recorder, httptest.NewRequest("GET", "/static/app.js", nil))
			},
		},
		{
			name: "stylesheet",
			handler: func(server *Server, recorder *httptest.ResponseRecorder) {
				server.stylesheet(recorder, httptest.NewRequest("GET", "/static/style.css", nil))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			test.handler(&Server{}, recorder)
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", got)
			}
		})
	}
}

func TestDashboardAssetsRemainDependencyFreeAndCSPCompatible(t *testing.T) {
	t.Parallel()

	html := string(indexHTML)
	javascript := string(appJS)
	stylesheet := string(styleCSS)
	for _, forbidden := range []struct {
		name  string
		value string
	}{
		{"external HTML URL", "https://"},
		{"external HTML URL", "http://"},
		{"inline style", "style="},
		{"inline click handler", "onclick="},
		{"inline submit handler", "onsubmit="},
	} {
		if strings.Contains(html, forbidden.value) {
			t.Fatalf("dashboard HTML contains %s", forbidden.name)
		}
	}
	for _, forbidden := range []string{
		"localStorage", "sessionStorage", "innerHTML", "insertAdjacentHTML", "eval(", "new Function",
	} {
		if strings.Contains(javascript, forbidden) {
			t.Fatalf("dashboard JavaScript contains forbidden construct %q", forbidden)
		}
	}
	if strings.Contains(stylesheet, "url(") || strings.Contains(stylesheet, "@import") {
		t.Fatal("dashboard stylesheet contains an external asset hook")
	}
	for _, required := range []string{
		`href="#overview"`, `href="#resources"`, `href="#keys"`, `href="#billing"`, `href="#security"`, `href="#usage"`,
		`id="secret-dialog"`, `id="operation-status"`, `class="skip-link"`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("dashboard HTML is missing %s", required)
		}
	}
}

func TestBillingDashboardIncludesReadOnlyAndOwnerWorkflows(t *testing.T) {
	t.Parallel()

	html := string(indexHTML)
	javascript := string(appJS)
	for _, required := range []string{
		`data-section="billing"`, `id="billing-cash-balance"`, `id="billing-subscriptions"`,
		`id="billing-ledger-rows"`, `id="billing-user-select"`, `id="billing-rate-form"`,
		`id="billing-recharge-form"`, `id="billing-adjustment-form"`,
		`id="billing-subscription-day"`, `id="billing-subscription-week"`, `id="billing-subscription-month"`,
		`name="reason" required`, `class="owner-only hidden billing-owner-tools"`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("billing dashboard HTML is missing %s", required)
		}
	}
	for _, required := range []string{
		`/admin/billing/me`, `/admin/billing/settings`, `/admin/billing/users`,
		`/recharges`, `/adjustments`, `/subscriptions/`, `crypto.randomUUID()`,
		`sensitiveAction(() => api(path, {method, body}))`,
	} {
		if !strings.Contains(javascript, required) {
			t.Fatalf("billing dashboard JavaScript is missing %s", required)
		}
	}
	for _, forbidden := range []string{
		`Number(data.get("cny_amount"))`, `Number(data.get("usd_amount"))`, `Number(data.get("quota_usd"))`,
	} {
		if strings.Contains(javascript, forbidden) {
			t.Fatalf("billing dashboard converts an exact money string with %s", forbidden)
		}
	}
}
